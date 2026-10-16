package supervisor

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"gosuda.org/hqq"
)

// The fake service is this test binary, re-executed with envFake set. TestMain
// runs it instead of the tests, so every test has a real HQQ service to
// supervise.
const (
	envFake     = "HQQD_FAKE_SERVICE" // mode; see runFake
	envListener = "HQQD_FAKE_LISTENER"
	envAfter    = "HQQD_FAKE_AFTER"   // duration after which the mode misbehaves
	envCounter  = "HQQD_FAKE_COUNTER" // file counting the incarnations
	envCrashes  = "HQQD_FAKE_CRASHES" // incarnations that crash (mode crash)
	envTag      = "HQQD_FAKE_TAG"     // ignored; changing it changes the spec
)

const (
	fakePing = 1
	fakeStop = 7
)

// exitFake ends the fake service with syscall.Exit: unlike os.Exit it skips
// the race runtime's exit hook, which takes about a second per process.
// fakeIgnoreTerm is set in the mode that ignores SIGTERM.
var fakeIgnoreTerm bool

func TestMain(m *testing.M) {
	if mode := os.Getenv(envFake); mode != "" {
		runFake(mode)
	}
	os.Exit(m.Run())
}

// incarnation increments the counter file and returns the new value.
func incarnation() int {
	path := os.Getenv(envCounter)
	if path == "" {
		return 1
	}
	n := 0
	if b, err := os.ReadFile(path); err == nil {
		n, _ = strconv.Atoi(string(b))
	}
	n++
	_ = os.WriteFile(path, []byte(strconv.Itoa(n)), 0o600)
	return n
}

func runFake(mode string) {
	fmt.Println("fake", mode, "started")
	fmt.Fprintln(os.Stderr, "fake stderr")
	after, _ := time.ParseDuration(os.Getenv(envAfter))
	listener := os.Getenv(envListener)
	n := incarnation()

	switch mode {
	case "exit-clean":
		syscall.Exit(0)
	case "silent":
		// Never serves: readiness times out, or the process just runs.
		waitTerm(nil)
	case "ignore-term":
		fakeIgnoreTerm = true
		signal.Ignore(syscall.SIGTERM)
		serveFake(listener, nil, 0)
	case "serve":
		serveFake(listener, nil, 0)
	case "crash":
		crashes, _ := strconv.Atoi(os.Getenv(envCrashes))
		if listener == "" {
			// Without a listener the crash is just a sleep and an exit.
			if n <= crashes {
				time.Sleep(after)
				syscall.Exit(3)
			}
			waitTerm(nil)
			syscall.Exit(0)
		}
		if n <= crashes {
			// A crash leaves the listener name behind, like a real one.
			serveFake(listener, nil, after)
		}
		serveFake(listener, nil, 0)
	case "close":
		var closeAt <-chan time.Time
		if n == 1 {
			// Close the listener after a while but keep the process alive:
			// health checks fail and hqqd must restart it.
			closeAt = time.After(after)
		}
		serveFake(listener, closeAt, 0)
	default:
		fmt.Fprintln(os.Stderr, "unknown fake mode", mode)
		syscall.Exit(2)
	}
	syscall.Exit(0)
}

// serveFake serves the HQQ listener until a termination signal, a stop request
// or (when crashAfter > 0) a crash after crashAfter. closeAt, when set, closes
// the acceptor and keeps running.
func serveFake(listener string, closeAt <-chan time.Time, crashAfter time.Duration) {
	acc, err := hqq.NewAcceptor(listener, hqq.AcceptorConfig{})
	if err != nil {
		fmt.Fprintln(os.Stderr, "fake: listen:", err)
		syscall.Exit(2)
	}
	mux := hqq.NewMux()
	mux.HandleFunc(fakePing, func(_ context.Context, w *hqq.ResponseWriter, _ *hqq.Request) {
		_, _ = w.Write([]byte("pong"))
	})
	stop := make(chan struct{}, 1)
	mux.HandleFunc(fakeStop, func(_ context.Context, w *hqq.ResponseWriter, _ *hqq.Request) {
		_, _ = w.Write([]byte("bye"))
		select {
		case stop <- struct{}{}:
		default:
		}
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		for {
			link, err := acc.Accept(ctx)
			if err != nil {
				return
			}
			go func() {
				defer link.Close()
				_ = link.Serve(ctx, mux)
			}()
		}
	}()
	if crashAfter > 0 {
		time.Sleep(crashAfter)
		syscall.Exit(3)
	}
	if closeAt != nil {
		<-closeAt
		closeBounded(acc)
	}
	waitTerm(stop)
	cancel()
	closeBounded(acc)
	time.Sleep(20 * time.Millisecond)
}

// closeBounded closes the acceptor, waiting at most 100ms: Acceptor.Close also
// waits for the handshakes in progress, which a probe may keep busy.
func closeBounded(acc *hqq.Acceptor) {
	done := make(chan struct{})
	go func() {
		_ = acc.Close()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(100 * time.Millisecond):
	}
}

// waitTerm blocks until SIGTERM/SIGINT or a stop request (stop may be nil).
func waitTerm(stop chan struct{}) {
	sig := make(chan os.Signal, 1)
	if fakeIgnoreTerm {
		signal.Notify(sig, syscall.SIGINT) // Notify would undo signal.Ignore(SIGTERM)
	} else {
		signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)
	}
	if stop == nil {
		stop = make(chan struct{})
	}
	select {
	case <-sig:
	case <-stop:
	}
}

var nameSeq atomic.Int64

// shortName returns an HQQ listener name short enough for every platform.
func shortName() string {
	return fmt.Sprintf("hqdt%x%x", os.Getpid()&0xfff, nameSeq.Add(1))
}

// fakeSpec returns a service that runs the fake service in mode.
func fakeSpec(name, listener, mode string, env map[string]string) ServiceSpec {
	e := map[string]string{envFake: mode}
	if listener != "" {
		e[envListener] = listener
	}
	for k, v := range env {
		e[k] = v
	}
	exe, err := os.Executable()
	if err != nil {
		panic(err)
	}
	return ServiceSpec{
		Name:     name,
		Command:  exe,
		Env:      e,
		Listener: listener,
		Restart: RestartSpec{
			Policy:      "on-failure",
			Backoff:     BackoffSpec{Initial: Duration(10 * time.Millisecond), Max: Duration(50 * time.Millisecond)},
			MaxRestarts: 5,
			Window:      Duration(time.Minute),
		},
		StopTimeout: Duration(3 * time.Second),
		Health: HealthSpec{
			Interval:   Duration(50 * time.Millisecond),
			Timeout:    Duration(300 * time.Millisecond),
			Failures:   2,
			StartGrace: Duration(10 * time.Second),
			PingMethod: fakePing,
		},
	}
}

// bareSpec returns a service without listener or health checks: it is ready as
// soon as its process started.
func bareSpec(name, mode string) ServiceSpec {
	spec := fakeSpec(name, "", mode, nil)
	spec.Listener = ""
	spec.Health = HealthSpec{}
	return spec
}

// recorder collects the events of a Supervisor.
type recorder struct {
	mu     sync.Mutex
	events []Event
}

func (r *recorder) add(ev Event) {
	r.mu.Lock()
	r.events = append(r.events, ev)
	r.mu.Unlock()
}

func (r *recorder) count(svc string, st State) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, ev := range r.events {
		if ev.Service == svc && ev.State == st {
			n++
		}
	}
	return n
}

func (r *recorder) has(svc string, st State, substr string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, ev := range r.events {
		if ev.Service == svc && ev.State == st && strings.Contains(ev.Message, substr) {
			return true
		}
	}
	return false
}

// newTestSupervisor creates a Supervisor with an event recorder and shuts it
// down when the test ends.
func newTestSupervisor(t *testing.T, cfg Config) (*Supervisor, *recorder) {
	t.Helper()
	rec := &recorder{}
	sv, err := New(cfg, WithEvents(rec.add))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		_ = sv.Shutdown(ctx)
	})
	return sv, rec
}

// waitFor polls cond until it holds or the test fails.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// waitState waits until the service reaches st.
func waitState(t *testing.T, sv *Supervisor, name string, st State) Status {
	t.Helper()
	var out Status
	waitFor(t, fmt.Sprintf("%s to be %s", name, st), func() bool {
		s, err := sv.Status(name)
		out = s
		return err == nil && s.State == st
	})
	return out
}

func ctxT(t *testing.T, d time.Duration) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), d)
	t.Cleanup(cancel)
	return ctx
}

func tempDir(t *testing.T) string {
	t.Helper()
	return filepath.Clean(t.TempDir())
}
