package mpmc_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gosuda.org/hqq/internal/mpmc"
)

// gatedParker is a Parker whose Wake blocks until the test opens its gate. It
// lets a test hold the context wake-up callback in the middle of running.
type gatedParker struct {
	parks    atomic.Int64
	entered  chan struct{} // closed when Wake was entered
	gate     chan struct{}
	wakeDone atomic.Bool
	once     sync.Once
}

func (p *gatedParker) Park(_ mpmc.Side, timeout time.Duration) {
	p.parks.Add(1)
	time.Sleep(min(timeout, 5*time.Millisecond))
}

func (p *gatedParker) Wake(mpmc.Side, uint32) {
	p.once.Do(func() { close(p.entered) })
	<-p.gate
	p.wakeDone.Store(true)
}

// TestMPMCWaitFinishWaitsForContextCallback is the regression test for a
// use-after-unmap: context.AfterFunc's stop function does not wait for a
// callback that already started, so a blocking operation could return (and
// its owner unmap the ring) while the callback was still about to touch the
// ring's wait word or Parker.
func TestMPMCWaitFinishWaitsForContextCallback(t *testing.T) {
	buffer, r := newRing(t, 2)
	defer func() { _ = buffer }()
	p := &gatedParker{entered: make(chan struct{}), gate: make(chan struct{})}
	r.SetParker(p)
	var gateOnce sync.Once
	openGate := func() { gateOnce.Do(func() { close(p.gate) }) }
	defer openGate()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	type result struct {
		err          error
		callbackDone bool
	}
	resCh := make(chan result, 1)
	go func() {
		// The ring is empty, so the wait parks until the context is done.
		err := r.DequeueZeroCopyCancel(&mpmc.Cancel{Ctx: ctx}, func(uint64, *uintptr) {})
		resCh <- result{err: err, callbackDone: p.wakeDone.Load()}
	}()

	waitUntil(t, "waiter to park", func() bool { return p.parks.Load() > 0 })
	cancel()
	select {
	case <-p.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("context callback never started")
	}
	select {
	case res := <-resCh:
		t.Fatalf("operation returned (err=%v) while its context callback was still running", res.err)
	case <-time.After(100 * time.Millisecond):
		// The waiter has long observed the cancellation, yet it still waits
		// for the callback.
	}
	openGate()
	select {
	case res := <-resCh:
		if !errors.Is(res.err, mpmc.ErrCanceled) {
			t.Fatalf("err = %v, want ErrCanceled", res.err)
		}
		if !res.callbackDone {
			t.Fatal("operation returned before the context callback finished")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("operation never returned after the callback finished")
	}
}

func mustPanic(t *testing.T, want any, f func()) {
	t.Helper()
	defer func() {
		t.Helper()
		if got := recover(); got != want {
			t.Fatalf("recovered %v, want %v", got, want)
		}
	}()
	f()
	t.Fatal("expected a panic")
}

// TestMPMCEnqueueZeroCopyPanicKeepsRingMoving checks that a panicking
// producer callback still publishes its slot (zeroed) and that the panic
// propagates, so the ring is not wedged for every later operation.
func TestMPMCEnqueueZeroCopyPanicKeepsRingMoving(t *testing.T) {
	buffer, r := newRing(t, 2)
	defer func() { _ = buffer }()

	// More panics than slots: each one must free its slot again.
	for i := 0; i < 5; i++ {
		mustPanic(t, "boom", func() {
			_ = r.EnqueueZeroCopyCancel(&mpmc.Cancel{NoWait: true}, func(_ uint64, e *uintptr) {
				*e = 0xdead
				panic("boom")
			})
		})
		// The abandoned write is published as a zero element.
		var got uintptr = 1
		if err := r.DequeueZeroCopyCancel(&mpmc.Cancel{NoWait: true}, func(_ uint64, e *uintptr) { got = *e }); err != nil {
			t.Fatalf("dequeue after panic %d: %v", i, err)
		}
		if got != 0 {
			t.Fatalf("panicked write left element %#x, want zero", got)
		}
	}

	// Normal traffic is unaffected.
	r.Enqueue(7)
	r.Enqueue(8)
	if a, b := r.Dequeue(), r.Dequeue(); a != 7 || b != 8 {
		t.Fatalf("dequeued %d,%d, want 7,8", a, b)
	}
}

// enqueueWithin enqueues v and fails the test instead of hanging if the ring
// stays full.
func enqueueWithin(t *testing.T, r *mpmc.MPMCRing[uintptr], v uintptr) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if !r.EnqueueWithContext(ctx, v) {
		t.Fatalf("enqueue of %d timed out: the ring is wedged", v)
	}
}

// TestMPMCDequeueZeroCopyPanicReleasesSlot checks that a panicking consumer
// callback releases its slot instead of leaving the ring full forever.
func TestMPMCDequeueZeroCopyPanicReleasesSlot(t *testing.T) {
	buffer, r := newRing(t, 2)
	defer func() { _ = buffer }()

	for i := uintptr(1); i <= 5; i++ {
		enqueueWithin(t, r, i)
		mustPanic(t, "boom", func() {
			_ = r.DequeueZeroCopyCancel(&mpmc.Cancel{NoWait: true}, func(_ uint64, e *uintptr) {
				if *e != i {
					t.Errorf("callback saw %d, want %d", *e, i)
				}
				panic("boom")
			})
		})
	}
	// Every slot was released: the ring accepts a full batch again and keeps
	// FIFO order.
	enqueueWithin(t, r, 100)
	enqueueWithin(t, r, 200)
	if err := r.EnqueueZeroCopyCancel(&mpmc.Cancel{NoWait: true}, func(uint64, *uintptr) {}); !errors.Is(err, mpmc.ErrTimeout) {
		t.Fatalf("third enqueue into a full ring = %v, want ErrTimeout", err)
	}
	if a, b := r.Dequeue(), r.Dequeue(); a != 100 || b != 200 {
		t.Fatalf("dequeued %d,%d, want 100,200", a, b)
	}
}

// chanSemaphore is an in-process stand-in for a named kernel semaphore. Wait
// deliberately ignores the caller's timeout (up to a generous cap), so a lost
// wake-up shows up as a test that hangs instead of being hidden by the ring's
// periodic re-check.
type chanSemaphore struct {
	tokens   chan struct{}
	waits    atomic.Int64
	releases atomic.Int64
}

func newChanSemaphore() *chanSemaphore {
	return &chanSemaphore{tokens: make(chan struct{}, 1<<16)}
}

func (s *chanSemaphore) Wait(time.Duration) {
	s.waits.Add(1)
	select {
	case <-s.tokens:
	case <-time.After(10 * time.Second):
	}
}

func (s *chanSemaphore) Release(n uint32) {
	s.releases.Add(1)
	for ; n > 0; n-- {
		select {
		case s.tokens <- struct{}{}:
		default:
			return
		}
	}
}

func newParkedRing(t testing.TB, size uint64) (*mpmc.MPMCRing[uintptr], *chanSemaphore, *chanSemaphore) {
	t.Helper()
	buffer, r := newRing(t, size)
	t.Cleanup(func() { _ = buffer })
	notEmpty, notFull := newChanSemaphore(), newChanSemaphore()
	r.SetParker(mpmc.NewSemaphoreParker(notEmpty, notFull))
	return r, notEmpty, notFull
}

func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestMPMCParkerWakesConsumerAndProducer(t *testing.T) {
	r, notEmpty, notFull := newParkedRing(t, 2)

	got := make(chan uintptr, 1)
	go func() { got <- r.Dequeue() }()
	waitUntil(t, "consumer to park", func() bool { return notEmpty.waits.Load() > 0 })
	r.Enqueue(42)
	select {
	case v := <-got:
		if v != 42 {
			t.Fatalf("Dequeue = %d, want 42", v)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("parked consumer was never woken through the Parker")
	}
	if notEmpty.releases.Load() == 0 {
		t.Fatal("producer never called Wake(NotEmpty)")
	}

	r.Enqueue(1)
	r.Enqueue(2)
	done := make(chan struct{})
	go func() {
		r.Enqueue(3)
		close(done)
	}()
	waitUntil(t, "producer to park", func() bool { return notFull.waits.Load() > 0 })
	if v := r.Dequeue(); v != 1 {
		t.Fatalf("Dequeue = %d, want 1", v)
	}
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("parked producer was never woken through the Parker")
	}
	if notFull.releases.Load() == 0 {
		t.Fatal("consumer never called Wake(NotFull)")
	}
}

func TestMPMCParkerContextCancelAndWakeAll(t *testing.T) {
	r, notEmpty, _ := newParkedRing(t, 2)

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		_, err := r.ReserveConsumerCancel(&mpmc.Cancel{Ctx: ctx})
		errCh <- err
	}()
	waitUntil(t, "consumer to park", func() bool { return notEmpty.waits.Load() > 0 })
	cancel()
	select {
	case err := <-errCh:
		if !errors.Is(err, mpmc.ErrCanceled) {
			t.Fatalf("err = %v, want ErrCanceled", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("context cancellation did not wake the parked waiter")
	}

	closed := make(chan struct{})
	waitsBefore := notEmpty.waits.Load()
	go func() {
		errCh <- r.DequeueZeroCopyCancel(&mpmc.Cancel{Closed: closed}, func(uint64, *uintptr) {})
	}()
	waitUntil(t, "second consumer to park", func() bool { return notEmpty.waits.Load() > waitsBefore })
	close(closed)
	r.WakeAll()
	select {
	case err := <-errCh:
		if !errors.Is(err, mpmc.ErrClosed) {
			t.Fatalf("err = %v, want ErrClosed", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("WakeAll did not wake the parked waiter")
	}
}

// TestMPMCParkerNoLostWakeups pushes many items through a tiny ring with
// several producers and consumers so that waiters park constantly.
func TestMPMCParkerNoLostWakeups(t *testing.T) {
	r, _, _ := newParkedRing(t, 2)

	const producers, consumers, perProducer = 3, 3, 3000
	var wg sync.WaitGroup
	var sum atomic.Uint64
	var consumed atomic.Int64
	for p := 0; p < producers; p++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 1; i <= perProducer; i++ {
				r.Enqueue(uintptr(i))
			}
		}()
	}
	for c := 0; c < consumers; c++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for consumed.Add(1) <= producers*perProducer {
				sum.Add(uint64(r.Dequeue()))
			}
		}()
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(60 * time.Second):
		t.Fatal("producers/consumers hung: lost wake-up")
	}
	want := uint64(producers) * perProducer * (perProducer + 1) / 2
	if got := sum.Load(); got != want {
		t.Fatalf("sum = %d, want %d", got, want)
	}
}
