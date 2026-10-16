package mpmc_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unsafe"

	"gosuda.org/hqq/internal/mpmc"
)

// newRing allocates heap memory for a ring of size uintptr slots and attaches
// to it. The returned slice keeps the memory alive.
func newRing(t testing.TB, size uint64) ([]byte, *mpmc.MPMCRing[uintptr]) {
	t.Helper()
	buffer := make([]byte, mpmc.SizeMPMCRing[uintptr](uintptr(size)))
	b := uintptr(unsafe.Pointer(&buffer[0]))
	if !mpmc.MPMCInit[uintptr](b, size) {
		t.Fatal("failed to initialize ring")
	}
	r := mpmc.MPMCAttach[uintptr](b, time.Second)
	if r == nil {
		t.Fatal("failed to attach ring")
	}
	return buffer, r
}

func TestMPMCQueueFullBlockAndCancel(t *testing.T) {
	buffer, r := newRing(t, 2)
	defer func() { _ = buffer }()

	// The first two enqueues fill the ring.
	if !r.EnqueueWithContext(context.Background(), 10) {
		t.Fatal("Enqueue 1 failed")
	}
	if !r.EnqueueWithContext(context.Background(), 20) {
		t.Fatal("Enqueue 2 failed")
	}

	// The third enqueue must block until its context expires. Take the start
	// time before creating the context so the timer cannot start earlier
	// than the measurement.
	const timeout = 100 * time.Millisecond
	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	ok := r.EnqueueWithContext(ctx, 30)
	elapsed := time.Since(start)

	if ok {
		t.Fatal("Enqueue 3 should have failed because the queue is full")
	}
	if elapsed < timeout {
		t.Fatalf("Enqueue 3 returned too early: %v", elapsed)
	}
}

func TestMPMCQueueEmptyBlockAndCancel(t *testing.T) {
	buffer, r := newRing(t, 2)
	defer func() { _ = buffer }()

	// Dequeue on an empty ring must block until its context expires.
	const timeout = 100 * time.Millisecond
	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	_, ok := r.DequeueWithContext(ctx)
	elapsed := time.Since(start)

	if ok {
		t.Fatal("Dequeue should have failed because the queue is empty")
	}
	if elapsed < timeout {
		t.Fatalf("Dequeue returned too early: %v", elapsed)
	}
}

// TestMPMCParkedConsumerIsWoken verifies that a consumer that has gone past
// the spin and yield phases (and is parked) is woken by a producer.
func TestMPMCParkedConsumerIsWoken(t *testing.T) {
	buffer, r := newRing(t, 4)
	defer func() { _ = buffer }()

	got := make(chan uintptr, 1)
	go func() {
		got <- r.Dequeue()
	}()

	// Give the consumer time to reach the parking phase.
	time.Sleep(50 * time.Millisecond)
	start := time.Now()
	r.Enqueue(42)
	select {
	case v := <-got:
		if v != 42 {
			t.Fatalf("Dequeue = %d, want 42", v)
		}
		// maxPark is 100ms; a prompt wake-up must beat it comfortably.
		if d := time.Since(start); d > 50*time.Millisecond {
			t.Fatalf("parked consumer woke after %v, want a prompt wake-up", d)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("parked consumer was never woken")
	}
}

// TestMPMCParkedProducerIsWoken is the producer-side counterpart.
func TestMPMCParkedProducerIsWoken(t *testing.T) {
	buffer, r := newRing(t, 2)
	defer func() { _ = buffer }()
	r.Enqueue(1)
	r.Enqueue(2)

	done := make(chan struct{})
	go func() {
		r.Enqueue(3)
		close(done)
	}()
	time.Sleep(50 * time.Millisecond)
	if v := r.Dequeue(); v != 1 {
		t.Fatalf("Dequeue = %d, want 1", v)
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("parked producer was never woken")
	}
	if a, b := r.Dequeue(), r.Dequeue(); a != 2 || b != 3 {
		t.Fatalf("remaining = %d,%d, want 2,3", a, b)
	}
}

func TestMPMCCancelContextWakesParkedWaiter(t *testing.T) {
	buffer, r := newRing(t, 2)
	defer func() { _ = buffer }()

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		_, err := r.ReserveConsumerCancel(&mpmc.Cancel{Ctx: ctx})
		errCh <- err
	}()
	time.Sleep(50 * time.Millisecond)
	start := time.Now()
	cancel()
	select {
	case err := <-errCh:
		if !errors.Is(err, mpmc.ErrCanceled) {
			t.Fatalf("err = %v, want ErrCanceled", err)
		}
		if d := time.Since(start); d > 50*time.Millisecond {
			t.Fatalf("cancellation observed after %v", d)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("canceled waiter never returned")
	}
}

func TestMPMCCancelClosedAndWakeAll(t *testing.T) {
	buffer, r := newRing(t, 2)
	defer func() { _ = buffer }()

	closed := make(chan struct{})
	errCh := make(chan error, 1)
	go func() {
		errCh <- r.DequeueZeroCopyCancel(&mpmc.Cancel{Closed: closed}, func(uint64, *uintptr) {
			t.Error("callback must not run on an empty ring")
		})
	}()
	time.Sleep(50 * time.Millisecond)
	close(closed)
	r.WakeAll()
	select {
	case err := <-errCh:
		if !errors.Is(err, mpmc.ErrClosed) {
			t.Fatalf("err = %v, want ErrClosed", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("closed waiter never returned")
	}
}

func TestMPMCCancelDeadline(t *testing.T) {
	buffer, r := newRing(t, 2)
	defer func() { _ = buffer }()

	start := time.Now()
	_, err := r.ReserveConsumerCancel(&mpmc.Cancel{Deadline: start.Add(30 * time.Millisecond)})
	if !errors.Is(err, mpmc.ErrTimeout) {
		t.Fatalf("err = %v, want ErrTimeout", err)
	}
	if d := time.Since(start); d < 30*time.Millisecond || d > time.Second {
		t.Fatalf("deadline wait took %v", d)
	}

	// An already-expired deadline still succeeds when an item is available.
	r.Enqueue(7)
	slot, err := r.ReserveConsumerCancel(&mpmc.Cancel{Deadline: time.Now().Add(-time.Second)})
	if err != nil || *slot.Value() != 7 {
		t.Fatalf("ReserveConsumerCancel with data = %v, %v", slot.Value(), err)
	}
	slot.Release()
}

func TestMPMCCancelCheckAbortsWait(t *testing.T) {
	buffer, r := newRing(t, 2)
	defer func() { _ = buffer }()

	errPeer := errors.New("peer gone")
	var calls atomic.Int32
	_, err := r.ReserveConsumerCancel(&mpmc.Cancel{Check: func() error {
		if calls.Add(1) >= 2 {
			return errPeer
		}
		return nil
	}})
	if !errors.Is(err, errPeer) {
		t.Fatalf("err = %v, want check error", err)
	}
}

func TestMPMCWaitSpinPolicy(t *testing.T) {
	buffer, r := newRing(t, 2)
	defer func() { _ = buffer }()
	r.SetWaitPolicy(mpmc.WaitSpin)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, ok := r.DequeueWithContext(ctx); ok {
		t.Fatal("DequeueWithContext succeeded on an empty ring")
	}

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := uintptr(0); i < 1000; i++ {
			r.Enqueue(i)
		}
	}()
	for i := uintptr(0); i < 1000; i++ {
		if v := r.Dequeue(); v != i {
			t.Fatalf("Dequeue = %d, want %d", v, i)
		}
	}
	wg.Wait()
}

func TestMPMCInitAndAttachValidation(t *testing.T) {
	buffer, _ := newRing(t, 8)
	b := uintptr(unsafe.Pointer(&buffer[0]))

	if mpmc.MPMCInit[uintptr](b, 8) {
		t.Fatal("MPMCInit re-initialized an initialized ring")
	}
	// A ring formatted for 8-byte elements has a 64-byte stride; attaching
	// with a type whose stride differs must be rejected.
	type wide struct{ a [9]uint64 }
	if r := mpmc.MPMCAttach[wide](b, time.Millisecond); r != nil {
		t.Fatal("MPMCAttach accepted a ring with a different slot layout")
	}

	empty := make([]byte, mpmc.SizeMPMCRing[uintptr](8))
	if r := mpmc.MPMCAttach[uintptr](uintptr(unsafe.Pointer(&empty[0])), 5*time.Millisecond); r != nil {
		t.Fatal("MPMCAttach attached to uninitialized memory")
	}

	// Format resets a used ring.
	r := mpmc.MPMCAttach[uintptr](b, time.Second)
	r.Enqueue(1)
	mpmc.Format[uintptr](unsafe.Pointer(&buffer[0]), 8)
	r = mpmc.MPMCAttach[uintptr](b, time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if _, ok := r.DequeueWithContext(ctx); ok {
		t.Fatal("Format did not reset the ring")
	}
}

func TestMPMCSizeIncludesHeaderOnce(t *testing.T) {
	if got, want := mpmc.SizeMPMCRing[uint64](16), uintptr(mpmc.HeaderSize+16*64); got != want {
		t.Fatalf("SizeMPMCRing[uint64](16) = %d, want %d", got, want)
	}
}
