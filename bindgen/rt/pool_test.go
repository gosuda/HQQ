package rt

import (
	"context"
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

var (
	errDialFailed  = errors.New("dial failed")
	errClosedTwice = errors.New("closed twice")
)

type poolItem struct {
	id     int
	closed atomic.Bool
}

type poolHarness struct {
	dialed atomic.Int32
	closed atomic.Int32
	pool   *Pool[*poolItem]
	failN  atomic.Int32
}

func newPoolHarness(max int) *poolHarness {
	h := &poolHarness{}
	h.pool = NewPool(max, func(ctx context.Context) (*poolItem, error) {
		if h.failN.Load() > 0 {
			h.failN.Add(-1)
			return nil, errDialFailed
		}
		return &poolItem{id: int(h.dialed.Add(1))}, nil
	}, func(it *poolItem) error {
		if !it.closed.CompareAndSwap(false, true) {
			return errClosedTwice
		}
		h.closed.Add(1)
		return nil
	})
	return h
}

func TestPoolReusesAndBounds(t *testing.T) {
	h := newPoolHarness(2)
	ctx := context.Background()
	a, err := h.pool.Get(ctx)
	if err != nil {
		t.Fatal(err)
	}
	b, err := h.pool.Get(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if a == b || h.dialed.Load() != 2 {
		t.Fatal("two Gets must dial two values")
	}
	// The third Get waits until a value comes back.
	got := make(chan *poolItem)
	go func() {
		v, err := h.pool.Get(ctx)
		if err != nil {
			t.Error(err)
		}
		got <- v
	}()
	select {
	case <-got:
		t.Fatal("Get exceeded the limit")
	case <-time.After(50 * time.Millisecond):
	}
	h.pool.Put(a)
	if v := <-got; v != a {
		t.Fatal("did not reuse the returned value")
	}
	if h.dialed.Load() != 2 {
		t.Fatal("dialed again")
	}
	h.pool.Put(a)
	h.pool.Put(b)
	if err := h.pool.Close(); err != nil {
		t.Fatal(err)
	}
	if h.closed.Load() != 2 {
		t.Fatalf("Close closed %d values", h.closed.Load())
	}
	if _, err := h.pool.Get(ctx); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("Get after Close = %v", err)
	}
	if err := h.pool.Close(); err != nil {
		t.Fatalf("second Close = %v", err)
	}
}

func TestPoolDiscardFreesSlot(t *testing.T) {
	h := newPoolHarness(1)
	ctx := context.Background()
	a, _ := h.pool.Get(ctx)
	h.pool.Discard(a)
	if !a.closed.Load() {
		t.Fatal("Discard did not close")
	}
	b, err := h.pool.Get(ctx)
	if err != nil || b == a || h.dialed.Load() != 2 {
		t.Fatalf("Get after Discard: %v", err)
	}
	h.pool.Put(b)
	_ = h.pool.Close()
}

func TestPoolDialErrorFreesSlot(t *testing.T) {
	h := newPoolHarness(1)
	ctx := context.Background()
	h.failN.Store(1)
	if _, err := h.pool.Get(ctx); err == nil {
		t.Fatal("expected the dial error")
	}
	v, err := h.pool.Get(ctx)
	if err != nil {
		t.Fatalf("slot leaked after a failed dial: %v", err)
	}
	h.pool.Put(v)
	_ = h.pool.Close()
}

func TestPoolGetHonorsContext(t *testing.T) {
	h := newPoolHarness(1)
	a, _ := h.pool.Get(context.Background())
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := h.pool.Get(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Get = %v", err)
	}
	// A canceled waiter must not swallow the wake-up of the next one.
	done := make(chan struct{})
	go func() {
		v, err := h.pool.Get(context.Background())
		if err != nil {
			t.Error(err)
		}
		h.pool.Put(v)
		close(done)
	}()
	time.Sleep(20 * time.Millisecond)
	h.pool.Put(a)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("waiter starved")
	}
	_ = h.pool.Close()
}

func TestPoolCloseWakesWaitersAndClosesLentValues(t *testing.T) {
	h := newPoolHarness(1)
	a, _ := h.pool.Get(context.Background())
	errc := make(chan error, 1)
	go func() {
		_, err := h.pool.Get(context.Background())
		errc <- err
	}()
	time.Sleep(30 * time.Millisecond)
	if err := h.pool.Close(); err != nil {
		t.Fatal(err)
	}
	if err := <-errc; !errors.Is(err, net.ErrClosed) {
		t.Fatalf("waiter = %v", err)
	}
	h.pool.Put(a)
	if !a.closed.Load() {
		t.Fatal("a value returned after Close was not closed")
	}
}

func TestPoolConcurrent(t *testing.T) {
	h := newPoolHarness(4)
	var wg sync.WaitGroup
	var inUse, maxUse atomic.Int32
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 50 {
				v, err := h.pool.Get(context.Background())
				if err != nil {
					t.Error(err)
					return
				}
				n := inUse.Add(1)
				for {
					m := maxUse.Load()
					if n <= m || maxUse.CompareAndSwap(m, n) {
						break
					}
				}
				inUse.Add(-1)
				if v.id%5 == 0 {
					h.pool.Discard(v)
				} else {
					h.pool.Put(v)
				}
			}
		}()
	}
	wg.Wait()
	if maxUse.Load() > 4 {
		t.Fatalf("%d values in use at once, limit 4", maxUse.Load())
	}
	if err := h.pool.Close(); err != nil {
		t.Fatal(err)
	}
	if h.dialed.Load() != h.closed.Load() {
		t.Fatalf("dialed %d, closed %d", h.dialed.Load(), h.closed.Load())
	}
}
