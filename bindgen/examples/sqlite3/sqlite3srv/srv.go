//go:build unix

// Package sqlite3srv starts the sqlite3 example server (bindgen/examples/sqlite3/server) for tests: it
// finds or builds the binary, starts it with launch.Start, and hands back the listener, the allow
// directory and the log of the server. It is test support: every function takes a testing.TB and
// registers its own cleanup. The tests of the example and the tests of the database/sql driver on top of
// it share it.
//
// The binary comes from HQQ_SQLITE3GEN_SERVER, the path of a built sqlite3_server (see build.sh).
// Without it, Binary builds the server once per process with build.sh into a temporary directory, which
// needs cmake, ninja, a C compiler, unzip and either network access (the pinned SQLite is downloaded and
// verified by scripts/release/fetch-sqlite.sh) or HQQ_SQLITE_AMALGAMATION_DIR, and it builds with the
// test hooks of the server (build.sh -T), which a prebuilt server may lack. It skips the test with
// the reason when that is not possible, and with -short. HQQ_SQLITE3GEN_SANITIZE=address+undefined
// builds and runs the server under ASan and UBSan. A test binary that builds should call Cleanup from
// TestMain after m.Run, which removes the build directory.
package sqlite3srv

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"gosuda.org/hqq/bindgen/examples/sqlite3"
	"gosuda.org/hqq/bindgen/rt"
	"gosuda.org/hqq/launch"
	"gosuda.org/hqq/secureexec"
)

const (
	// EnvServer names a built sqlite3_server.
	EnvServer = "HQQ_SQLITE3GEN_SERVER"
	// EnvSanitize is the HQQ_SANITIZE value to build the server with (for example address+undefined).
	EnvSanitize = "HQQ_SQLITE3GEN_SANITIZE"
	// EnvAmalgamation is the unpacked SQLite amalgamation that build.sh should use instead of fetching.
	EnvAmalgamation = "HQQ_SQLITE_AMALGAMATION_DIR"
)

// FailBound bounds every wait of the tests for an event that must happen. It is a diagnostic limit for a
// failing run, never a delay of a passing one.
const FailBound = 30 * time.Second

var (
	buildOnce sync.Once
	buildBin  string
	buildSkip string
	buildErr  error
	buildDir  string
)

// Sanitized reports whether the server of this run is built with sanitizers.
func Sanitized() bool { return sanitizer() != "" }

func sanitizer() string {
	if san := os.Getenv(EnvSanitize); san != "none" {
		return san
	}
	return ""
}

// Binary returns the path of the server, building it on the first call, or skips the test.
func Binary(t testing.TB) string {
	t.Helper()
	buildOnce.Do(build)
	if buildSkip != "" {
		t.Skip(buildSkip)
	}
	if buildErr != nil {
		t.Fatalf("building the sqlite3 server: %v", buildErr)
	}
	return buildBin
}

// Cleanup removes the build directory that Binary made. Call it from TestMain after m.Run.
func Cleanup() {
	if buildDir != "" {
		if err := os.RemoveAll(buildDir); err != nil {
			fmt.Fprintln(os.Stderr, "sqlite3srv: removing the build directory:", err)
		}
	}
}

func build() {
	for _, env := range []string{EnvServer, "HQQ_SQLITE_SERVER"} {
		if p := os.Getenv(env); p != "" {
			if _, err := os.Stat(p); err != nil {
				buildErr = fmt.Errorf("%s: %w", env, err)
				return
			}
			buildBin = p
			return
		}
	}
	switch {
	case testing.Short():
		buildSkip = "the server is not built with -short; set " + EnvServer + " to a built sqlite3_server (bindgen/examples/sqlite3/build.sh)"
		return
	case runtime.GOOS == "windows":
		buildSkip = "the sqlite3 server is POSIX only"
		return
	}
	for _, tool := range []string{"cmake", "ninja", "unzip"} {
		if _, err := exec.LookPath(tool); err != nil {
			buildSkip = tool + " is not installed: install it or set " + EnvServer + " to a built sqlite3_server"
			return
		}
	}
	root, err := repoRoot()
	if err != nil {
		buildErr = err
		return
	}
	dir, err := os.MkdirTemp("", "sqlite3gen-build-")
	if err != nil {
		buildErr = err
		return
	}
	buildDir = dir
	// -T: the test hooks of the server, which tests of the retain-on-fail path need.
	args := []string{filepath.Join(root, "bindgen", "examples", "sqlite3", "build.sh"), "-T", "-b", filepath.Join(dir, "b")}
	if a := os.Getenv(EnvAmalgamation); a != "" {
		args = append(args, "-d", a)
	}
	if san := sanitizer(); san != "" {
		args = append(args, "-s", san)
	}
	var out, errOut bytes.Buffer
	cmd := exec.Command("sh", args...)
	cmd.Stdout, cmd.Stderr = &out, &errOut
	if err := cmd.Run(); err != nil {
		if strings.Contains(errOut.String(), "downloading") && strings.Contains(errOut.String(), "curl") {
			buildSkip = "the pinned SQLite could not be downloaded (set " + EnvAmalgamation + " or " + EnvServer + "): " + lastLine(errOut.String())
			return
		}
		buildErr = fmt.Errorf("build.sh: %w\n%s", err, errOut.String())
		return
	}
	for _, line := range strings.Split(out.String(), "\n") {
		if v, ok := strings.CutPrefix(line, "export "+EnvServer+"="); ok {
			buildBin = strings.Trim(v, "'")
		}
	}
	if buildBin == "" {
		buildErr = fmt.Errorf("build.sh printed no %s:\n%s", EnvServer, out.String())
	}
}

func lastLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.LastIndexByte(s, '\n'); i >= 0 {
		return s[i+1:]
	}
	return s
}

// repoRoot finds the module root from the working directory of the test.
func repoRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", errors.New("go.mod not found above the working directory")
		}
		dir = parent
	}
}

// Log is the stderr of a server. It lets a test wait for a line.
type Log struct {
	mu   sync.Mutex
	text strings.Builder
	w    io.Writer
}

// Write implements io.Writer.
func (l *Log) Write(p []byte) (int, error) {
	l.mu.Lock()
	l.text.Write(p)
	w := l.w
	l.mu.Unlock()
	if w != nil {
		_, _ = w.Write(p)
	}
	return len(p), nil
}

// String returns everything the server wrote.
func (l *Log) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.text.String()
}

// Count returns how often sub occurs.
func (l *Log) Count(sub string) int { return strings.Count(l.String(), sub) }

// WaitCount waits until sub occurs at least n times, and reports whether it did within d.
func (l *Log) WaitCount(sub string, n int, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for {
		if l.Count(sub) >= n {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// The lines the server prints for a session (server/main.c).
const (
	LineSessionStarted = "sqlite3_server: session started"
	LineSessionEnded   = "sqlite3_server: session ended"
)

// Config configures Start.
type Config struct {
	// AllowDir is the --allow-dir of the server. Empty: a new temporary directory. "-" passes no
	// --allow-dir (aliases only).
	AllowDir string
	// Aliases are the --alias NAME=PATH options.
	Aliases map[string]string
	// Args are further flags of the server, after the listener name.
	Args []string
	// Env is extra environment, "K=V".
	Env []string
	// Stderr also receives the log of the server (default: only the Log).
	Stderr io.Writer
}

// Server is one running sqlite3_server.
type Server struct {
	*launch.Server
	// AllowDir is the allow directory ("" when aliases only). It is the canonical directory: the
	// policy of the server compares canonical paths.
	AllowDir string
	Log      *Log
	t        testing.TB
	killed   bool
}

// Start starts a server on a fresh listener and registers its cleanup: stop it, and require a clean
// exit unless the test ended it with Kill.
func Start(t testing.TB, cfg Config) *Server {
	t.Helper()
	bin := Binary(t)
	hash, _, err := secureexec.HashFile(bin)
	if err != nil {
		t.Fatalf("hashing %s: %v", bin, err)
	}
	name, err := launch.NewListenerName("s3g")
	if err != nil {
		t.Fatalf("listener name: %v", err)
	}
	s := &Server{Log: &Log{w: cfg.Stderr}, t: t}
	args := []string{name}
	switch cfg.AllowDir {
	case "-":
	case "":
		s.AllowDir = canonical(t, t.TempDir())
		args = append(args, "--allow-dir", s.AllowDir)
	default:
		s.AllowDir = cfg.AllowDir
		args = append(args, "--allow-dir", cfg.AllowDir)
	}
	names := make([]string, 0, len(cfg.Aliases))
	for n := range cfg.Aliases {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		args = append(args, "--alias", n+"="+cfg.Aliases[n])
	}
	args = append(args, cfg.Args...)
	env := append(os.Environ(), sanitizerEnv()...)
	env = append(env, cfg.Env...)
	ctx, cancel := context.WithTimeout(context.Background(), FailBound)
	defer cancel()
	s.Server, err = launch.Start(ctx, launch.Config{
		Binary:      bin,
		SHA256:      hash,
		Listener:    name,
		Args:        args,
		Env:         env,
		Stderr:      s.Log,
		StopTimeout: FailBound,
	})
	if err != nil {
		t.Fatalf("launch.Start: %v", err)
	}
	t.Cleanup(func() {
		err := s.Server.Close()
		if s.killed {
			return
		}
		if err != nil {
			t.Errorf("sqlite3 server did not stop cleanly: %v\nstderr:\n%s", err, s.Log.String())
		}
	})
	return s
}

// KillServer ends the server with SIGKILL, as a crash would, and waits until it is gone.
func (s *Server) KillServer() {
	s.t.Helper()
	s.killed = true
	if err := syscall.Kill(s.PID(), syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		s.t.Fatalf("kill server: %v", err)
	}
	select {
	case <-s.Done():
	case <-time.After(FailBound):
		s.t.Fatalf("the killed server did not exit")
	}
}

func canonical(t testing.TB, dir string) string {
	t.Helper()
	real, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatalf("canonical path of %s: %v", dir, err)
	}
	return real
}

func sanitizerEnv() []string {
	if sanitizer() == "" {
		return nil
	}
	asan := "halt_on_error=1:abort_on_error=0"
	if runtime.GOOS == "linux" {
		asan += ":detect_leaks=1" // LeakSanitizer is not available on macOS
	}
	env := []string{"ASAN_OPTIONS=" + asan, "UBSAN_OPTIONS=halt_on_error=1:print_stacktrace=1"}
	var out []string
	for _, kv := range env {
		if os.Getenv(kv[:strings.IndexByte(kv, '=')]) == "" {
			out = append(out, kv)
		}
	}
	return out
}

// Dial opens a session on the server and closes it when the test ends. A session that the test broke on
// purpose (its connection failed) is ended with Kill.
func (s *Server) Dial(opts ...func(*rt.Options)) *sqlite3.Client {
	s.t.Helper()
	return DialTo(s.t, s.Listener(), opts...)
}

// killed holds the sessions that a test ended with KillClient, which the cleanup of Dial must not Close.
var killed sync.Map

// KillClient ends a session without the CLOSE exchange, as a crashed client would.
func KillClient(c *sqlite3.Client) {
	killed.Store(c, true)
	c.Kill()
}

// DialTo is Dial for a listener name.
func DialTo(t testing.TB, listener string, opts ...func(*rt.Options)) *sqlite3.Client {
	t.Helper()
	var o rt.Options
	for _, f := range opts {
		f(&o)
	}
	ctx, cancel := context.WithTimeout(context.Background(), FailBound)
	defer cancel()
	c, err := sqlite3.Dial(ctx, listener, o)
	if err != nil {
		t.Fatalf("dial %s: %v", listener, err)
	}
	t.Cleanup(func() {
		if _, dead := killed.Load(c); dead || c.Conn().Err() != nil {
			c.Kill()
			return
		}
		if err := c.Close(); err != nil {
			t.Errorf("close session: %v", err)
		}
	})
	return c
}
