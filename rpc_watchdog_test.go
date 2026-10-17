package hqq

import (
	"context"
	"errors"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const (
	methodFast uint64 = 1
	methodSlow uint64 = 2
)

// watchdogHarness runs Serve with a chosen inline limit on one end of a
// negotiated pair. The slow method blocks until release is closed and
// reports when it started; the fast method answers at once.
type watchdogHarness struct {
	client  *AdvancedLink
	link    *AdvancedLink
	s       *server
	started chan time.Time
	release chan struct{}

	cancel    context.CancelFunc
	serveDone chan error
	stopOnce  sync.Once
	stopErr   error
}

func newWatchdogHarness(t *testing.T, limit time.Duration) *watchdogHarness {
	t.Helper()
	h := &watchdogHarness{
		started:   make(chan time.Time, 8),
		release:   make(chan struct{}),
		serveDone: make(chan error, 1),
	}
	h.client, h.link = newNegotiatedPair(t, 16, 64, AdvancedOptions{}, AdvancedOptions{})
	mux := NewMux()
	mux.HandleFunc(methodFast, func(_ context.Context, w *ResponseWriter, _ *Request) {
		_, _ = w.Write([]byte("fast"))
	})
	mux.HandleFunc(methodSlow, func(_ context.Context, w *ResponseWriter, _ *Request) {
		h.started <- time.Now()
		<-h.release
		_, _ = w.Write([]byte("slow"))
	})
	var ctx context.Context
	ctx, h.cancel = context.WithCancel(context.Background())
	h.s = h.link.newServer(ctx, mux, limit)
	go func() { h.serveDone <- h.s.serve() }()
	t.Cleanup(func() {
		select {
		case <-h.release:
		default:
			close(h.release)
		}
		h.stop(t)
	})
	return h
}

// stop ends Serve and returns its result; later calls return the same one.
func (h *watchdogHarness) stop(t *testing.T) error {
	t.Helper()
	h.stopOnce.Do(func() {
		h.cancel()
		h.stopErr = waitErr(t, h.serveDone, 5*time.Second)
	})
	return h.stopErr
}

func (h *watchdogHarness) call(t *testing.T, method uint64, want string) {
	t.Helper()
	m, err := h.client.Call(context.Background(), method, nil)
	if err != nil || string(m.Bytes()) != want {
		t.Fatalf("Call(%d) = %v, %v; want %q", method, m, err, want)
	}
	m.Release()
}

// startSlowCall issues the slow call in the background and returns when its
// handler started, with the time it started and a channel for the call's
// outcome.
func (h *watchdogHarness) startSlowCall(t *testing.T) (time.Time, <-chan error) {
	t.Helper()
	res := make(chan error, 1)
	go func() {
		m, err := h.client.Call(context.Background(), methodSlow, nil)
		if err == nil {
			if string(m.Bytes()) != "slow" {
				err = errors.New("slow call answered " + string(m.Bytes()))
			}
			m.Release()
		}
		res <- err
	}()
	select {
	case at := <-h.started:
		return at, res
	case <-time.After(5 * time.Second):
		t.Fatal("slow handler did not start")
		return time.Time{}, nil
	}
}

// awaitEscalation waits until the receiver role moved on after `before`
// escalations, and returns when it noticed. Polling can only notice late, so
// the time it returns is never earlier than the escalation.
func (h *watchdogHarness) awaitEscalation(t *testing.T, before uint64) time.Time {
	t.Helper()
	eventually(t, "an escalation", func() bool {
		return h.link.stats.serveEscalations.Load() > before
	})
	return time.Now()
}

// A handler is escalated no earlier than inlineLimit after it started even
// though the timer that watches it was armed for an earlier handler, which
// returned long before. The first handler arms the timer for its own
// deadline; the slow handler starts 70% of the way to it.
func TestServeTimerArmedByEarlierHandlerDoesNotEscalateYoungerHandlerEarly(t *testing.T) {
	const limit = 100 * time.Millisecond
	h := newWatchdogHarness(t, limit)

	h.call(t, methodFast, "fast")
	// The scenario needs real time to pass: the timer armed above must
	// still be pending, and the next handler must be younger than the
	// timer's deadline by a known margin.
	time.Sleep(limit * 7 / 10)

	before := h.link.stats.serveEscalations.Load()
	startedAt, result := h.startSlowCall(t)
	noticed := h.awaitEscalation(t, before)

	// The timer fires about 0.3*limit after the handler started. Escalating
	// then would be early by 0.7*limit; the margin below only absorbs the
	// delay between the receiver's clock read and the handler's.
	if age := noticed.Sub(startedAt); age < limit*9/10 {
		t.Fatalf("handler escalated after %v, want at least the inline limit %v", age, limit)
	}

	close(h.release)
	if err := <-result; err != nil {
		t.Fatalf("slow call: %v", err)
	}
	if got := h.link.stats.serveEscalations.Load() - before; got != 1 {
		t.Fatalf("escalations during the episode = %d, want 1", got)
	}
}

// After the watchdog found nothing to watch it stays off, and a slow handler
// that comes later is still escalated by the timer the next request arms.
func TestServeEscalatesSlowHandlerAfterIdleGap(t *testing.T) {
	const limit = 40 * time.Millisecond
	h := newWatchdogHarness(t, limit)

	h.call(t, methodFast, "fast")
	eventually(t, "the watchdog to go idle", func() bool { return !h.s.armed.Load() })
	if got := h.s.resets.Load(); got != 1 {
		t.Fatalf("timer set %d times for one request and an idle gap, want 1", got)
	}

	before := h.link.stats.serveEscalations.Load()
	startedAt, result := h.startSlowCall(t)
	noticed := h.awaitEscalation(t, before)
	if age := noticed.Sub(startedAt); age < limit*3/4 {
		t.Fatalf("handler escalated after %v, want at least the inline limit %v", age, limit)
	}

	// The receiver role moved: later requests are served while the slow
	// handler still runs.
	h.call(t, methodFast, "fast")

	close(h.release)
	if err := <-result; err != nil {
		t.Fatalf("slow call: %v", err)
	}
}

// The request that follows an idle period arms the timer again.
func TestServeRequestAfterIdleGapArmsWatchdog(t *testing.T) {
	h := newWatchdogHarness(t, 20*time.Millisecond)

	h.call(t, methodFast, "fast")
	eventually(t, "the watchdog to go idle", func() bool { return !h.s.armed.Load() })
	h.call(t, methodFast, "fast")

	if got := h.s.resets.Load(); got != 2 {
		t.Fatalf("timer set %d times for two requests around an idle gap, want 2", got)
	}
	eventually(t, "the watchdog to go idle again", func() bool { return !h.s.armed.Load() })
	if got := h.s.resets.Load(); got != 2 {
		t.Fatalf("an idle watchdog set the timer again: %d settings, want 2", got)
	}
}

// Handlers that return in time share the one timer arming: the request path
// does not set the timer.
func TestServeFastHandlersDoNotSetTimerPerRequest(t *testing.T) {
	h := newWatchdogHarness(t, time.Hour)

	for range 2000 {
		h.call(t, methodFast, "fast")
	}

	if got := h.s.resets.Load(); got != 1 {
		t.Fatalf("timer set %d times for 2000 requests, want 1", got)
	}
	if got := h.link.stats.serveEscalations.Load(); got != 0 {
		t.Fatalf("%d escalations with a one hour limit", got)
	}
}

// When Serve returns, a timer left armed by the last handler is stopped.
func TestServeReturnStopsArmedTimer(t *testing.T) {
	h := newWatchdogHarness(t, time.Hour)

	h.call(t, methodFast, "fast")
	if !h.s.armed.Load() {
		t.Fatal("the watchdog is not armed after a request")
	}
	if err := h.stop(t); !errors.Is(err, context.Canceled) {
		t.Fatalf("Serve = %v, want context.Canceled", err)
	}

	if h.s.timer.Stop() {
		t.Fatal("the timer was still pending after Serve returned")
	}
}

// With a limit so short that the timer callback runs almost all the time,
// ending Serve leaves no callback behind: none runs and none is pending
// afterwards. Run with -race: a callback that outlived Serve would touch the
// finished server.
func TestServeReturnLeavesNoWatchdogCallbackRunning(t *testing.T) {
	const (
		rounds   = 25
		minCalls = 20
	)
	for round := range rounds {
		client, link := newNegotiatedPair(t, 16, 64, AdvancedOptions{}, AdvancedOptions{})
		spin := HandlerFunc(func(_ context.Context, w *ResponseWriter, _ *Request) {
			for start := time.Now(); time.Since(start) < 10*time.Microsecond; {
			}
			_, _ = w.Write([]byte("ok"))
		})
		serveCtx, stopServe := context.WithCancel(context.Background())
		s := link.newServer(serveCtx, spin, 2*time.Microsecond)
		serveDone := make(chan error, 1)
		go func() { serveDone <- s.serve() }()

		callCtx, stopCalls := context.WithCancel(context.Background())
		var calls atomic.Int64
		var callers sync.WaitGroup
		callers.Add(1)
		go func() {
			defer callers.Done()
			for {
				m, err := client.Call(callCtx, methodFast, nil)
				if err != nil {
					return
				}
				m.Release()
				calls.Add(1)
			}
		}()
		eventually(t, "requests to be served", func() bool { return calls.Load() >= minCalls })

		stopServe()
		err := waitErr(t, serveDone, 5*time.Second)
		stopCalls()
		callers.Wait()
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("round %d: Serve = %v, want context.Canceled", round, err)
		}

		if s.timer.Stop() {
			t.Fatalf("round %d: the timer was pending after Serve returned", round)
		}
		buf := make([]byte, 1<<20)
		stacks := string(buf[:runtime.Stack(buf, true)])
		if strings.Contains(stacks, "(*server).watch") {
			t.Fatalf("round %d: a watchdog callback is running after Serve returned:\n%s", round, stacks)
		}
	}
}
