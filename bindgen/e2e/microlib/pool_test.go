//go:build unix

package microlib_test

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	hqq "gosuda.org/hqq"
	"gosuda.org/hqq/bindgen/e2e/microlib"
	"gosuda.org/hqq/bindgen/rt"
)

// newPool returns a pool of sessions of s and a counter of the sessions it dialed.
func newPool(s *server, max int) (*rt.Pool[*microlib.Client], *atomic.Int64) {
	var dialed atomic.Int64
	p := rt.NewPool(max, func(ctx context.Context) (*microlib.Client, error) {
		dialed.Add(1)
		return microlib.Dial(ctx, s.name, rt.Options{})
	}, func(c *microlib.Client) error { return c.Close() })
	return p, &dialed
}

// TestPoolManyGoroutines: 8 goroutines share a pool of 8 sessions and do real work on the session they
// are lent (handles, out-params, buffers); a pool of 3 shared by 16 goroutines never holds more than 3
// sessions. Run under -race, this is also the check that a Conn is only ever used by one goroutine at
// a time (BINDGEN.md 8.2).
func TestPoolManyGoroutines(t *testing.T) {
	for _, tc := range []struct {
		name         string
		max, workers int
	}{{"8x8", 8, 8}, {"3x16", 3, 16}} {
		t.Run(tc.name, func(t *testing.T) {
			s := startServer(t, serverOpts{args: []string{"--max-sessions", "8"}})
			pool, dialed := newPool(s, tc.max)
			var wg sync.WaitGroup
			errs := make(chan error, tc.workers)
			for w := range tc.workers {
				wg.Add(1)
				go func() {
					defer wg.Done()
					errs <- poolWorker(pool, w)
				}()
			}
			wg.Wait()
			close(errs)
			for err := range errs {
				if err != nil {
					t.Error(err)
				}
			}
			if got := dialed.Load(); got > int64(tc.max) {
				t.Errorf("the pool dialed %d sessions, want at most %d", got, tc.max)
			}
			wantNoErr(t, "pool Close", pool.Close())
			s.requireNoSessionSegments()
			if _, err := pool.Get(context.Background()); !errors.Is(err, net.ErrClosed) {
				t.Errorf("Get on a closed pool = %v, want an error matching net.ErrClosed", err)
			}
			// Every object the workers created was freed by them or by the session teardown.
			counts := map[string]int{}
			for _, l := range s.traceFuncs("ml_counter_new", "ml_counter_free", "ml_cursor_open", "ml_cursor_free") {
				name, _, _ := strings.Cut(l, " ")
				counts[name]++
			}
			if counts["ml_counter_new"] != counts["ml_counter_free"] || counts["ml_cursor_open"] != counts["ml_cursor_free"] {
				t.Errorf("created and destroyed objects differ: %v", counts)
			}
		})
	}
}

// poolWorker borrows a session 50 times; each time it creates a counter with a worker-specific start,
// works on it and checks the results, and returns the session. A session of the pool is used by one
// goroutine at a time, and a handle never leaves the borrow that made it.
func poolWorker(pool *rt.Pool[*microlib.Client], w int) error {
	ctx := context.Background()
	for i := range 50 {
		c, err := pool.Get(ctx)
		if err != nil {
			return fmt.Errorf("worker %d: Get: %w", w, err)
		}
		start := int64(w*1000 + i)
		err = func() error {
			ctr, err := c.CounterNew(start)
			if err != nil {
				return err
			}
			total, err := c.CounterAdd(ctr, 5)
			if err != nil || total != start+5 {
				return fmt.Errorf("CounterAdd = (%d, %v), want %d", total, err, start+5)
			}
			cur, err := c.CursorOpen(ctr, 2)
			if err != nil {
				return err
			}
			if _, v, err := c.CursorNext(cur); err != nil || v != start+5 {
				return fmt.Errorf("CursorNext = (%d, %v), want %d", v, err, start+5)
			}
			src := []byte(fmt.Sprintf("worker %d round %d", w, i))
			got, err := c.Xor(src, byte(w), make([]byte, len(src)))
			if err != nil || string(xorModel(got, byte(w))) != string(src) {
				return fmt.Errorf("Xor round trip = (%q, %v)", got, err)
			}
			if i%2 == 0 { // half of the borrows leave their objects to the session's end
				if err := c.CounterFree(ctr); err != nil {
					return err
				}
			}
			return nil
		}()
		if err != nil {
			pool.Discard(c)
			return fmt.Errorf("worker %d round %d: %w", w, i, err)
		}
		pool.Put(c)
	}
	return nil
}

// TestPoolDiscardFreesTheSlot: a session that broke is discarded, its slot is given back, and the next
// Get dials a new one.
func TestPoolDiscardFreesTheSlot(t *testing.T) {
	s := startServer(t, serverOpts{noTrace: true})
	pool, dialed := newPool(s, 1)
	defer pool.Close()
	ctx := context.Background()

	c, err := pool.Get(ctx)
	wantNoErr(t, "Get", err)
	c.Kill() // the session is unusable now
	if _, err := c.Add(1, 1); err == nil {
		t.Fatal("a call on a killed session succeeded")
	}
	pool.Discard(c)

	c2, err := pool.Get(ctx) // would block forever if Discard had kept the slot
	wantNoErr(t, "Get after Discard", err)
	if c2 == c {
		t.Fatal("Get returned the discarded session")
	}
	if got, err := c2.Add(2, 3); err != nil || got != 5 {
		t.Fatalf("Add on the new session = (%d, %v)", got, err)
	}
	pool.Put(c2)
	if got := dialed.Load(); got != 2 {
		t.Fatalf("dialed %d sessions, want 2", got)
	}
}

// TestServerModes: the interop results do not depend on the server's request copy mode (--copy-request,
// BINDGEN.md 7.2) or its wait policy (--spin); the client uses WaitSpin together with --spin.
func TestServerModes(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		spin bool
	}{
		{"copy-request", []string{"--copy-request"}, false},
		{"spin", []string{"--spin"}, true},
		{"copy-request and spin", []string{"--copy-request", "--spin"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := startServer(t, serverOpts{args: tc.args, noTrace: true})
			var opts rt.Options
			if tc.spin {
				opts.Link = hqq.LinkOptions{WaitPolicy: hqq.WaitSpin}
			}
			runDifferential(t, s, 300, opts)
		})
	}
}

// TestHandleGenerationWrap: one session creates and destroys far more objects than the 16 bits of a
// handle generation count, reusing the same table slot every time; ids stay nonzero and valid, and a
// handle from before the wrap is refused.
func TestHandleGenerationWrap(t *testing.T) {
	s := startServer(t, serverOpts{noTrace: true})
	c := dial(t, s, rt.Options{})
	first, err := c.CounterNew(0)
	wantNoErr(t, "CounterNew", err)
	wantNoErr(t, "CounterFree", c.CounterFree(first))
	var last microlib.Counter
	for i := range 70_000 { // more than 2^16 reuses of the one free slot
		ctr, err := c.CounterNew(int64(i))
		if err != nil {
			t.Fatalf("CounterNew %d: %v", i, err)
		}
		if ctr.ID() == 0 {
			t.Fatalf("CounterNew %d returned the id 0", i)
		}
		if err := c.CounterFree(ctr); err != nil {
			t.Fatalf("CounterFree %d: %v", i, err)
		}
		last = ctr
	}
	_, err = c.CounterAdd(last, 1)
	wantStatus(t, "CounterAdd on the last freed handle", err, "ml_counter_add", 3)
	_, err = c.CounterAdd(first, 1)
	wantStatus(t, "CounterAdd on the first freed handle", err, "ml_counter_add", 3)
	live, err := c.CounterNew(10)
	wantNoErr(t, "CounterNew", err)
	if total, err := c.CounterAdd(live, 1); err != nil || total != 11 {
		t.Fatalf("CounterAdd = (%d, %v)", total, err)
	}
}
