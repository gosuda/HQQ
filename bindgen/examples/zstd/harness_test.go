package zstd_test

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"math/rand/v2"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"gosuda.org/hqq/bindgen/examples/zstd"
	"gosuda.org/hqq/bindgen/rt"
	"gosuda.org/hqq/launch"
	"gosuda.org/hqq/secureexec"
)

// serverEnv names the zstd_server binary that build.sh prints. The tests that need a server skip
// when it is unset, like the other end-to-end tests of this repository.
const serverEnv = "HQQ_ZSTD_SERVER"

var shared *launch.Server

func TestMain(m *testing.M) {
	code := 1
	defer func() { os.Exit(code) }()
	if os.Getenv(serverEnv) != "" {
		srv, err := startServer()
		if err != nil {
			fmt.Fprintln(os.Stderr, "zstd tests: start shared server:", err)
			return
		}
		shared = srv
		defer func() {
			if err := srv.Close(); err != nil {
				fmt.Fprintln(os.Stderr, "zstd tests: stop shared server:", err)
				code = 1
			}
		}()
	}
	code = m.Run()
}

// logBuffer collects the stderr of a server and lets a test wait for its lines.
type logBuffer struct {
	mu   sync.Mutex
	text strings.Builder
}

func (l *logBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.text.Write(p)
}

func (l *logBuffer) count(substr string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Count(l.text.String(), substr)
}

// waitCount waits until the log holds at least n occurrences of substr.
func (l *logBuffer) waitCount(substr string, n int, d time.Duration) bool {
	return eventually(d, func() bool { return l.count(substr) >= n })
}

// startServer runs the server binary of serverEnv with extra arguments after the listener name.
func startServer(args ...string) (*launch.Server, error) {
	return startServerTo(os.Stderr, nil, args...)
}

// startServerTo is startServer with the stderr of the server and extra environment variables, which
// are added to the environment of this process.
func startServerTo(stderr io.Writer, env []string, args ...string) (*launch.Server, error) {
	bin := os.Getenv(serverEnv)
	sum, _, err := secureexec.HashFile(bin)
	if err != nil {
		return nil, fmt.Errorf("hash %s: %w", bin, err)
	}
	name, err := launch.NewListenerName("zst")
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cfg := launch.Config{
		Binary:   bin,
		SHA256:   sum,
		Listener: name,
		Args:     []string{name},
		Stderr:   stderr,
	}
	if len(env) > 0 {
		cfg.Env = append(os.Environ(), env...)
	}
	return launch.Start(ctx, cfg, args...)
}

// requireServer skips the test when no server binary is configured.
func requireServer(t testing.TB) {
	t.Helper()
	if shared == nil {
		t.Skipf("%s is not set: run `eval \"$(bindgen/examples/zstd/build.sh)\"` first", serverEnv)
	}
}

// ownServer starts a server with extra arguments and stops it when the test ends.
func ownServer(t testing.TB, args ...string) *launch.Server {
	t.Helper()
	srv, _ := ownLoggedServer(t, args...)
	return srv
}

// ownLoggedServer is ownServer with the server's stderr captured and forwarded to the test's stderr.
func ownLoggedServer(t testing.TB, args ...string) (*launch.Server, *logBuffer) {
	t.Helper()
	return ownServerTo(t, os.Stderr, nil, args...)
}

// reusableFree makes an AddressSanitizer server hand freed memory back to the allocator at once,
// instead of holding up to 256 MiB in quarantine. Memory-size tests need it: the quarantine looks
// like growth and hides reuse. Double frees are still reported. A server built without
// AddressSanitizer ignores the variable.
var reusableFree = []string{"ASAN_OPTIONS=quarantine_size_mb=0:thread_local_quarantine_size_kb=0:detect_leaks=0"}

// ownServerTo starts a server whose stderr goes to w and to the returned buffer, with extra
// environment variables.
func ownServerTo(t testing.TB, w io.Writer, env []string, args ...string) (*launch.Server, *logBuffer) {
	t.Helper()
	requireServer(t)
	logs := new(logBuffer)
	srv, err := startServerTo(io.MultiWriter(w, logs), env, args...)
	if err != nil {
		t.Fatalf("start server %v: %v", args, err)
	}
	t.Cleanup(func() {
		if err := srv.Close(); err != nil {
			if _, dead := killedServers.Load(srv); !dead {
				t.Errorf("stop server %v: %v", args, err)
			}
		}
	})
	return srv, logs
}

// dialTo opens a session with the documented geometry and closes it when the test ends. A session
// that the test broke on purpose is closed by Kill.
func dialTo(t testing.TB, listener string, opts ...func(*rt.Options)) *zstd.Client {
	t.Helper()
	o := zstd.DefaultOptions()
	for _, f := range opts {
		f(&o)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, err := zstd.Dial(ctx, listener, o)
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

// killed holds the sessions that a test ended with Kill, which the cleanup of dialTo must not Close.
var killed sync.Map

// killClient ends a session without the CLOSE exchange, as a crashed client would.
func killClient(c *zstd.Client) {
	killed.Store(c, true)
	c.Kill()
}

// newClient opens a session on the shared server.
func newClient(t testing.TB, opts ...func(*rt.Options)) *zstd.Client {
	t.Helper()
	requireServer(t)
	return dialTo(t, shared.Listener(), opts...)
}

// ---- deterministic data ----------------------------------------------------------------------

var (
	textOnce sync.Once
	textBase []byte
)

// textBlock is 4 MiB of text-like data from a 256-word vocabulary: about 2.5 to 3 times
// compressible at level 3, and the same on every run.
func textBlock() []byte {
	textOnce.Do(func() {
		r := rand.New(rand.NewPCG(0x7a737464, 1))
		vocab := make([]string, 256)
		for i := range vocab {
			w := make([]byte, 2+r.IntN(9))
			for j := range w {
				w[j] = byte('a' + r.IntN(26))
			}
			vocab[i] = string(w)
		}
		buf := make([]byte, 0, 4<<20+16)
		for len(buf) < 4<<20 {
			buf = append(buf, vocab[r.IntN(len(vocab))]...)
			if r.IntN(12) == 0 {
				buf = append(buf, ".\n"...)
			} else {
				buf = append(buf, ' ')
			}
		}
		textBase = buf[:4<<20]
	})
	return textBase
}

// textData returns n bytes of compressible data. Different salts start at different offsets of the
// block and stamp the salt into the data, so two salts never produce identical buffers.
func textData(n int, salt uint64) []byte {
	base := textBlock()
	out := make([]byte, n)
	pos := int(salt*7919*64) % len(base)
	for off := 0; off < n; {
		c := copy(out[off:], base[pos:])
		off += c
		pos = 0
	}
	if n >= 8 {
		binary.LittleEndian.PutUint64(out, salt)
	}
	return out
}

// randomData returns n incompressible bytes derived from seed.
func randomData(n int, seed uint64) []byte {
	out := make([]byte, n)
	var s [32]byte
	for i := range s {
		s[i] = byte(seed >> (i % 8 * 8))
	}
	rand.NewChaCha8(s).Read(out)
	return out
}

// ---- process memory --------------------------------------------------------------------------

// rssOf returns the resident set size of a process in KiB through ps, which macOS and Linux share.
func rssOf(pid int) (int64, error) {
	out, err := exec.Command("ps", "-o", "rss=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return 0, fmt.Errorf("ps -p %d: %w", pid, err)
	}
	n, err := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parse rss %q: %w", out, err)
	}
	return n, nil
}

// rssKiB is rssOf for the test goroutine.
func rssKiB(t testing.TB, pid int) int64 {
	t.Helper()
	n, err := rssOf(pid)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// eventually polls cond every 20 ms until it holds or d passes, and reports whether it held.
func eventually(d time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(d)
	for {
		if cond() {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(20 * time.Millisecond)
	}
}
