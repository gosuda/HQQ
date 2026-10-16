package mpmc_test

import (
	"context"
	"errors"
	"runtime"
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
	return newRingOf[uintptr](t, size)
}

// newRingOf is newRing for any element type. Callers must keep the returned
// slice alive (runtime.KeepAlive) for as long as they use the ring through
// anything but the returned handle.
func newRingOf[T any](t testing.TB, size uint64) ([]byte, *mpmc.MPMCRing[T]) {
	t.Helper()
	buffer := make([]byte, mpmc.SizeMPMCRing[T](uintptr(size)))
	p := unsafe.Pointer(&buffer[0])
	mpmc.Format[T](p, size)
	r := mpmc.Attach[T](p, time.Second)
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
	r.Enqueue(42)
	select {
	case v := <-got:
		if v != 42 {
			t.Fatalf("Dequeue = %d, want 42", v)
		}
		// Promptness of the wake-up is verified without timing by
		// TestMPMCParkerWakesConsumerAndProducer, whose semaphore ignores the
		// park timeout and would hang on a lost wake-up.
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
	cancel()
	select {
	case err := <-errCh:
		if !errors.Is(err, mpmc.ErrCanceled) {
			t.Fatalf("err = %v, want ErrCanceled", err)
		}
		// Promptness of the context wake-up is verified without timing by
		// TestMPMCParkerContextCancelAndWakeAll, whose semaphore ignores the
		// park timeout.
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
	if d := time.Since(start); d < 30*time.Millisecond || d > 10*time.Second {
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

func TestMPMCFormatAndAttachValidation(t *testing.T) {
	buffer, r := newRing(t, 8)
	defer runtime.KeepAlive(buffer)
	p := unsafe.Pointer(&buffer[0])

	// A ring formatted for 8-byte elements has a 64-byte stride; attaching
	// with a type whose stride differs must be rejected.
	type wide struct{ a [9]uint64 }
	if r := mpmc.Attach[wide](p, time.Millisecond); r != nil {
		t.Fatal("Attach accepted a ring with a different slot layout")
	}

	empty := make([]byte, mpmc.SizeMPMCRing[uintptr](8))
	if r := mpmc.Attach[uintptr](unsafe.Pointer(&empty[0]), 5*time.Millisecond); r != nil {
		t.Fatal("Attach attached to uninitialized memory")
	}

	// Format resets a used ring.
	r.Enqueue(1)
	mpmc.Format[uintptr](p, 8)
	r = mpmc.Attach[uintptr](p, time.Second)
	if err := r.DequeueZeroCopyCancel(&mpmc.Cancel{NoWait: true}, func(uint64, *uintptr) {}); !errors.Is(err, mpmc.ErrTimeout) {
		t.Fatalf("Dequeue after Format = %v, want an empty ring", err)
	}
}

func TestMPMCSizeIncludesHeaderOnce(t *testing.T) {
	if got, want := mpmc.SizeMPMCRing[uint64](16), uintptr(mpmc.HeaderSize+16*64); got != want {
		t.Fatalf("SizeMPMCRing[uint64](16) = %d, want %d", got, want)
	}
}

// TestMPMCUnsupportedSizes checks that sizes that cannot form a correct ring
// are rejected consistently: SizeMPMCRing reports 0 and Format panics. A
// one-slot ring is among them because its "published" and "free" sequence
// values coincide, which would let a producer overwrite an unread element.
func TestMPMCUnsupportedSizes(t *testing.T) {
	for _, n := range []uintptr{0, 1, 3, 6, 12, 100} {
		if got := mpmc.SizeMPMCRing[uint64](n); got != 0 {
			t.Errorf("SizeMPMCRing[uint64](%d) = %d, want 0", n, got)
		}
	}
	if got := mpmc.SizeMPMCRing[uint64](1 << (unsafe.Sizeof(uintptr(0))*8 - 1)); got != 0 {
		t.Errorf("SizeMPMCRing of an overflowing size = %d, want 0", got)
	}
	if got := mpmc.SizeMPMCRing[uint64](2); got == 0 {
		t.Error("SizeMPMCRing[uint64](2) = 0, want a valid size")
	}

	buffer := make([]byte, mpmc.SizeMPMCRing[uintptr](8))
	defer runtime.KeepAlive(buffer)
	for _, n := range []uint64{0, 1, 3, 6, 100} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("Format with size %d did not panic", n)
				}
			}()
			mpmc.Format[uintptr](unsafe.Pointer(&buffer[0]), n)
		}()
	}
	// The rejected attempts left the memory unformatted.
	if r := mpmc.Attach[uintptr](unsafe.Pointer(&buffer[0]), time.Millisecond); r != nil {
		t.Error("a rejected Format initialized the ring")
	}
}

// TestMPMCNonBlockingPathDoesNotAllocate guards the hot path: neither
// operation may allocate when it does not have to wait.
func TestMPMCNonBlockingPathDoesNotAllocate(t *testing.T) {
	buffer, r := newRing(t, 4)
	defer runtime.KeepAlive(buffer)
	if allocs := testing.AllocsPerRun(100, func() {
		r.Enqueue(1)
		_ = r.Dequeue()
	}); allocs != 0 {
		t.Fatalf("Enqueue+Dequeue allocated %v times per run, want 0", allocs)
	}
}
