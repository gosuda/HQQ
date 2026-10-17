//go:build unix

package microlib_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	hqq "gosuda.org/hqq"
	"gosuda.org/hqq/bindgen/e2e/microlib"
	"gosuda.org/hqq/bindgen/rt"
	"gosuda.org/hqq/internal/shm"
	"gosuda.org/hqq/launch"
	"gosuda.org/hqq/secureexec"
)

// Environment of the suite.
//
//	HQQ_BINDGEN_MICROLIB_SERVER    path of a prebuilt microlib_server; the tests do not build one
//	HQQ_BINDGEN_MICROLIB_SANITIZE  value of -DHQQ_SANITIZE for the build (for example
//	                               address+undefined): the whole suite then runs the sanitized server
//
// Without HQQ_BINDGEN_MICROLIB_SERVER the suite builds server/ with CMake and Ninja into a temporary
// directory once per test binary run, and skips with the reason when cmake, ninja or a C compiler is
// missing, on Windows (the C server is POSIX only, BINDGEN.md 1.2) and with -short.
const (
	envServer   = "HQQ_BINDGEN_MICROLIB_SERVER"
	envSanitize = "HQQ_BINDGEN_MICROLIB_SANITIZE"
)

// Liveness timing of the tests that wait for a dead peer: the server flags match the client options.
// The other tests keep the defaults (2 s), so that a loaded machine cannot fake a peer death.
var (
	fastLink       = hqq.LinkOptions{HeartbeatInterval: 50 * time.Millisecond, PeerTimeout: 500 * time.Millisecond}
	fastServerArgs = []string{"--heartbeat-ms", "50", "--peer-timeout-ms", "500"}
)

// failBound bounds every wait of the suite for an event that must happen: it is a diagnostic limit
// for a failing run, never a delay of a passing one.
const failBound = 20 * time.Second

var (
	buildOnce sync.Once
	buildBin  string
	buildSkip string
	buildErr  error
	buildDir  string
)

func TestMain(m *testing.M) {
	if mode := os.Getenv(envHelper); mode != "" {
		os.Exit(runHelper(mode))
	}
	code := m.Run()
	if buildDir != "" {
		if err := os.RemoveAll(buildDir); err != nil {
			fmt.Fprintln(os.Stderr, "e2e: removing the build directory:", err)
		}
	}
	os.Exit(code)
}

// serverBinary returns the path of the server, building it on the first call, or skips the test.
func serverBinary(t testing.TB) string {
	t.Helper()
	buildOnce.Do(buildServer)
	if buildSkip != "" {
		t.Skip(buildSkip)
	}
	if buildErr != nil {
		t.Fatalf("building the microlib server: %v", buildErr)
	}
	return buildBin
}

func buildServer() {
	if p := os.Getenv(envServer); p != "" {
		if _, err := os.Stat(p); err != nil {
			buildErr = fmt.Errorf("%s: %w", envServer, err)
			return
		}
		buildBin = p
		return
	}
	switch {
	case testing.Short():
		buildSkip = "the server is not built with -short; set " + envServer + " to a built microlib_server"
		return
	case runtime.GOOS == "windows":
		buildSkip = "the microlib server is POSIX only"
		return
	}
	for _, tool := range []string{"cmake", "ninja"} {
		if _, err := exec.LookPath(tool); err != nil {
			buildSkip = tool + " is not installed: install it or set " + envServer + " to a built microlib_server"
			return
		}
	}
	if !haveCompiler() {
		buildSkip = "no C compiler found (set CC, or install cc, gcc or clang) or set " + envServer + " to a built microlib_server"
		return
	}
	dir, err := os.MkdirTemp("", "microlib-build-")
	if err != nil {
		buildErr = err
		return
	}
	buildDir = dir
	src, err := filepath.Abs("server")
	if err != nil {
		buildErr = err
		return
	}
	cfg := []string{"-S", src, "-B", dir, "-G", "Ninja", "-DCMAKE_BUILD_TYPE=Release"}
	if san := sanitizer(); san != "" {
		cfg = append(cfg, "-DHQQ_SANITIZE="+san, "-DCMAKE_BUILD_TYPE=Debug")
	}
	for _, args := range [][]string{cfg, {"--build", dir, "--target", "microlib_server"}} {
		cmd := exec.Command("cmake", args...)
		if out, err := cmd.CombinedOutput(); err != nil {
			buildErr = fmt.Errorf("cmake %s: %w\n%s", strings.Join(args, " "), err, out)
			return
		}
	}
	buildBin = filepath.Join(dir, "microlib_server")
}

// sanitizer returns the sanitizer the suite runs the server under, or "".
func sanitizer() string {
	if san := os.Getenv(envSanitize); san != "none" {
		return san
	}
	return ""
}

func haveCompiler() bool {
	if cc := os.Getenv("CC"); cc != "" {
		_, err := exec.LookPath(strings.Fields(cc)[0])
		return err == nil
	}
	for _, cc := range []string{"cc", "gcc", "clang"} {
		if _, err := exec.LookPath(cc); err == nil {
			return true
		}
	}
	return false
}

// textBuf is a goroutine-safe buffer that lets a test wait for a line.
type textBuf struct {
	mu   sync.Mutex
	b    bytes.Buffer
	wake chan struct{} // closed and replaced at every write
}

func newTextBuf() *textBuf { return &textBuf{wake: make(chan struct{})} }

func (w *textBuf) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.b.Write(p)
	close(w.wake)
	w.wake = make(chan struct{})
	return len(p), nil
}

func (w *textBuf) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.String()
}

// waitFor blocks until the text contains sub, or fails the test after failBound.
func (w *textBuf) waitFor(t testing.TB, sub string) {
	t.Helper()
	deadline := time.NewTimer(failBound)
	defer deadline.Stop()
	for {
		w.mu.Lock()
		found := strings.Contains(w.b.String(), sub)
		wake := w.wake
		w.mu.Unlock()
		if found {
			return
		}
		select {
		case <-wake:
		case <-deadline.C:
			t.Fatalf("server output has no %q after %v:\n%s", sub, failBound, w.String())
		}
	}
}

// server is one running microlib_server process.
type server struct {
	t      testing.TB
	name   string // listener name, 8 characters: it is the prefix of every segment of the server
	trace  string // ML_TRACE file
	proc   *launch.Server
	stderr *textBuf

	// expectDeath is set by the tests that end the server themselves (kill, stall): the cleanup then
	// does not require a clean exit status.
	expectDeath bool
	// clientsMayFail is set by the tests that stop the server while clients are connected: their
	// Close then reports the gone server, which is not a finding.
	clientsMayFail bool
}

// serverOpts configures startServer.
type serverOpts struct {
	args []string // flags of the server after the listener name
	env  []string // extra environment, "K=V"
	// noTrace leaves ML_TRACE unset: tests that make many calls and do not read the trace.
	noTrace bool
}

// uniqueName returns an 8-character listener name. The server derives every segment name from its
// first 8 bytes (BINDGEN.md 4.11), so the name is also the prefix that the leak check lists.
func uniqueName(t testing.TB) string {
	t.Helper()
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatalf("random listener name: %v", err)
	}
	return hex.EncodeToString(b[:])
}

// sanitizerEnv returns the options of the sanitizer runtimes for a sanitized server.
func sanitizerEnv() []string {
	if sanitizer() == "" {
		return nil
	}
	asan := "halt_on_error=1:abort_on_error=0"
	if runtime.GOOS == "linux" {
		asan += ":detect_leaks=1" // LeakSanitizer is not available on macOS
	}
	env := []string{"ASAN_OPTIONS=" + asan, "UBSAN_OPTIONS=halt_on_error=1:print_stacktrace=1"}
	// A variable set by the caller wins.
	var out []string
	for _, kv := range env {
		if os.Getenv(kv[:strings.IndexByte(kv, '=')]) == "" {
			out = append(out, kv)
		}
	}
	return out
}

// startServer starts the server through launch.Start with the SHA-256 of the binary computed here
// (D17), on a fresh listener, and registers the cleanup: stop it, require a clean exit unless the
// test ended it, and check that no segment of the server is left (Linux).
func startServer(t *testing.T, o serverOpts) *server {
	t.Helper()
	bin := serverBinary(t)
	hash, _, err := secureexec.HashFile(bin)
	if err != nil {
		t.Fatalf("hashing %s: %v", bin, err)
	}
	s := &server{t: t, name: uniqueName(t), stderr: newTextBuf()}
	env := append(os.Environ(), sanitizerEnv()...)
	if !o.noTrace {
		s.trace = filepath.Join(t.TempDir(), "trace")
		env = append(env, "ML_TRACE="+s.trace)
	}
	env = append(env, o.env...)
	ctx, cancel := context.WithTimeout(context.Background(), failBound)
	defer cancel()
	s.proc, err = launch.Start(ctx, launch.Config{
		Binary:      bin,
		SHA256:      hash,
		Listener:    s.name,
		Args:        append([]string{s.name}, o.args...),
		Env:         env,
		Stderr:      s.stderr,
		StopTimeout: failBound,
	})
	if err != nil {
		t.Fatalf("launch.Start: %v", err)
	}
	t.Cleanup(s.cleanup)
	return s
}

func (s *server) cleanup() {
	t := s.t
	t.Helper()
	err := s.proc.Close()
	switch {
	case s.expectDeath:
		// The server was ended by the test; remove the listener name a dead server could not.
		if uerr := hqq.Unlink(s.name); uerr != nil && !errors.Is(uerr, os.ErrNotExist) {
			t.Errorf("unlinking the listener of the dead server: %v", uerr)
		}
	case err != nil:
		t.Errorf("server did not stop cleanly: %v\nstderr:\n%s", err, s.stderr.String())
	}
	if left := segmentsOf(t, s.name); len(left) != 0 {
		t.Errorf("segments left in the shared memory namespace after the server ended: %v", left)
	}
}

// segmentsOf lists the shared memory names that begin with prefix, where the platform lists them
// (Linux: /dev/shm). Elsewhere (macOS has no listing of POSIX shared memory) it probes the name
// itself, which covers the listener segment.
func segmentsOf(t testing.TB, prefix string) []string {
	t.Helper()
	if runtime.GOOS == "linux" {
		entries, err := os.ReadDir("/dev/shm")
		if err != nil {
			t.Fatalf("listing /dev/shm: %v", err)
		}
		var out []string
		for _, e := range entries {
			if strings.HasPrefix(e.Name(), prefix) {
				out = append(out, e.Name())
			}
		}
		return out
	}
	if nameExists(t, prefix) {
		return []string{prefix}
	}
	return nil
}

// nameExists reports whether a shared memory object of that exact name exists, by opening it read
// only without attaching an endpoint.
func nameExists(t testing.TB, name string) bool {
	t.Helper()
	smem, err := shm.OpenSharedMemory(name, 4096, os.O_RDONLY, 0)
	if errors.Is(err, os.ErrNotExist) {
		return false
	}
	if err != nil {
		t.Fatalf("probing the segment %q: %v", name, err)
	}
	_ = smem.Close()
	return true
}

// onlyListenerLeft reports whether the listener segment is the only one of the server (on a platform
// that lists the namespace; elsewhere whether the listener segment exists).
func (s *server) onlyListenerLeft() bool {
	left := segmentsOf(s.t, s.name)
	return len(left) == 1 && left[0] == s.name
}

// requireNoSessionSegments waits until nothing but the listener segment is left of the server in the
// shared memory namespace: the sessions and control links that ended have removed their names. Where
// the platform cannot list the namespace (macOS) it only checks that the listener segment exists, and
// the per-test cleanup checks the listener's removal; the session names are covered by the tests that
// know a name (TestSessionSegmentNameIsUnlinkedAtHello, TestHelloTimeoutRemovesTheSegment).
func (s *server) requireNoSessionSegments() {
	s.t.Helper()
	eventually(s.t, "only the listener segment to be left", s.onlyListenerLeft)
}

// pid returns the process id of the server.
func (s *server) pid() int { return s.proc.PID() }

// kill ends the server with SIGKILL and waits until it is reaped.
func (s *server) kill() {
	s.t.Helper()
	s.expectDeath = true
	if err := syscall.Kill(s.pid(), syscall.SIGKILL); err != nil {
		s.t.Fatalf("kill -9 %d: %v", s.pid(), err)
	}
	s.waitExit()
}

// signal sends sig to the server.
func (s *server) signal(sig syscall.Signal) {
	s.t.Helper()
	if err := syscall.Kill(s.pid(), sig); err != nil {
		s.t.Fatalf("kill -%d %d: %v", sig, s.pid(), err)
	}
}

// waitExit waits until the server has exited, within failBound, and returns its exit status error.
func (s *server) waitExit() error {
	s.t.Helper()
	select {
	case <-s.proc.Done():
		return s.proc.Err()
	case <-time.After(failBound):
		s.t.Fatalf("server still running %v after the signal\nstderr:\n%s", failBound, s.stderr.String())
		return nil
	}
}

// traceLines returns the lines of the trace file.
func (s *server) traceLines() []string {
	s.t.Helper()
	b, err := os.ReadFile(s.trace)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		s.t.Fatalf("reading the trace: %v", err)
	}
	return strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")
}

// traceFuncs returns the trace lines whose function name is one of fns, in order.
func (s *server) traceFuncs(fns ...string) []string {
	s.t.Helper()
	var out []string
	for _, l := range s.traceLines() {
		name, _, _ := strings.Cut(l, " ")
		for _, fn := range fns {
			if name == fn {
				out = append(out, l)
			}
		}
	}
	return out
}

// waitTrace waits until the trace has at least n lines of the functions fns and returns them.
func (s *server) waitTrace(n int, fns ...string) []string {
	s.t.Helper()
	deadline := time.Now().Add(failBound)
	for {
		lines := s.traceFuncs(fns...)
		if len(lines) >= n {
			return lines
		}
		if time.Now().After(deadline) {
			s.t.Fatalf("trace has %d of the %d expected %v lines after %v:\n%s",
				len(lines), n, fns, failBound, strings.Join(s.traceLines(), "\n"))
		}
		time.Sleep(5 * time.Millisecond) // poll interval of a file that another process appends to
	}
}

// dial connects a client to the server and registers its cleanup.
func dial(t testing.TB, s *server, opts rt.Options) *microlib.Client {
	t.Helper()
	c, err := dialErr(s, opts)
	if err != nil {
		t.Fatalf("Dial: %v\nserver stderr:\n%s", err, s.stderr.String())
	}
	t.Cleanup(func() {
		// A client ended by Kill reports the closed link on Close; any other failure is a finding.
		if err := c.Close(); err != nil && !s.expectDeath && !s.clientsMayFail && !errors.Is(err, hqq.ErrClosed) {
			t.Errorf("client Close: %v", err)
		}
	})
	return c
}

// contextWithFail returns a context that ends after failBound.
func contextWithFail() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), failBound)
}

// dialUntil dials until it succeeds, retrying while retry reports true for the error, within
// failBound. It is for the moments when the server frees a resource a little after the client was
// told (a session slot after CLOSE_ACK, a control link after open_timeout).
func dialUntil(t testing.TB, s *server, opts rt.Options, retry func(error) bool) *microlib.Client {
	t.Helper()
	deadline := time.Now().Add(failBound)
	for {
		c, err := dialErr(s, opts)
		if err == nil {
			t.Cleanup(func() {
				if err := c.Close(); err != nil && !s.expectDeath && !s.clientsMayFail && !errors.Is(err, hqq.ErrClosed) {
					t.Errorf("client Close: %v", err)
				}
			})
			return c
		}
		if !retry(err) || time.Now().After(deadline) {
			t.Fatalf("Dial: %v\nserver stderr:\n%s", err, s.stderr.String())
		}
		time.Sleep(2 * time.Millisecond) // poll interval: the condition is in another process
	}
}

// isBusy reports a refused OPEN because max_sessions was reached.
func isBusy(err error) bool {
	var oe *rt.OpenError
	return errors.As(err, &oe) && oe.Code == openCodeBusy
}

func dialErr(s *server, opts rt.Options) (*microlib.Client, error) {
	ctx, cancel := context.WithTimeout(context.Background(), failBound)
	defer cancel()
	return dialWith(ctx, s.name, opts)
}

func dialWith(ctx context.Context, listener string, opts rt.Options) (*microlib.Client, error) {
	return microlib.Dial(ctx, listener, opts)
}

// fastDial is dial with the fast liveness timing of the death tests; the server must run with
// fastServerArgs.
func fastDial(t testing.TB, s *server, opts rt.Options) *microlib.Client {
	t.Helper()
	opts.Link = fastLink
	return dial(t, s, opts)
}

// eventually polls cond until it is true or failBound passes. It is for observing another process
// (a segment disappearing, a trace line appearing), where no event can be awaited.
func eventually(t testing.TB, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(failBound)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %v waiting for %s", failBound, what)
		}
		time.Sleep(5 * time.Millisecond) // poll interval of a condition in another process
	}
}
