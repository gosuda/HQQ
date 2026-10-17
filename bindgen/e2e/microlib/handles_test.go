//go:build unix

package microlib_test

import (
	"slices"
	"strings"
	"testing"

	"gosuda.org/hqq/bindgen/rt"
)

// destroyFuncs are the trace lines that record the destruction of a library object.
var destroyFuncs = []string{"ml_cursor_free", "ml_counter_free"}

// TestCascadeDestroyOrder: freeing a Counter destroys its live Cursors first, newest first, then the
// counter (BINDGEN.md 5.1). The cascade ignores the status of the cursors (an unread cursor reports
// ML_PARTIAL) and the response of the explicit destroy is the counter's own.
func TestCascadeDestroyOrder(t *testing.T) {
	s := startServer(t, serverOpts{})
	c := dial(t, s, rt.Options{})

	parent, err := c.CounterNew(10)
	wantNoErr(t, "CounterNew", err)
	other, err := c.CounterNew(20)
	wantNoErr(t, "CounterNew", err)
	k1, err := c.CursorOpen(parent, 1) // cursor 1
	wantNoErr(t, "CursorOpen", err)
	k2, err := c.CursorOpen(parent, 1) // cursor 2
	wantNoErr(t, "CursorOpen", err)
	k3, err := c.CursorOpen(other, 1) // cursor 3, child of the other counter
	wantNoErr(t, "CursorOpen", err)
	for range cursorLen { // k1 is exhausted, so its destroy reports ML_OK
		_, _, err := c.CursorNext(k1)
		wantNoErr(t, "CursorNext", err)
	}

	wantNoErr(t, "CounterFree(parent)", c.CounterFree(parent))
	want := []string{
		"ml_cursor_free cursor=2 ret=2", // newest child first, unread: ML_PARTIAL, ignored by the cascade
		"ml_cursor_free cursor=1 ret=0",
		"ml_counter_free counter=1",
	}
	if got := s.traceFuncs(destroyFuncs...); !slices.Equal(got, want) {
		t.Fatalf("destroy order:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}

	for _, tc := range []struct {
		name, fn string
		err      error
	}{
		{"CursorFree(k1)", "ml_cursor_free", c.CursorFree(k1)},
		{"CursorFree(k2)", "ml_cursor_free", c.CursorFree(k2)},
		{"CounterFree(parent)", "ml_counter_free", c.CounterFree(parent)},
	} {
		wantStatus(t, tc.name+" after the cascade", tc.err, tc.fn, 3)
	}

	// The sibling subtree is untouched.
	ret, v, err := c.CursorNext(k3)
	wantNoErr(t, "CursorNext on the other counter's cursor", err)
	if ret != 0 || v != 20 {
		t.Fatalf("CursorNext(k3) = (%d, %d), want (0, 20)", ret, v)
	}
	if total, err := c.CounterAdd(other, 1); err != nil || total != 21 {
		t.Fatalf("CounterAdd(other) = (%d, %v)", total, err)
	}
}

// TestTeardownOnClose: Close destroys what the client left, in descending creation sequence, so every
// cursor goes before its counter. Creation order: counter 1, counter 2, cursor 1 (of counter 1),
// cursor 2 (of counter 2), so the destroy order is cursor 2, cursor 1, counter 2, counter 1.
func TestTeardownOnClose(t *testing.T) {
	s := startServer(t, serverOpts{})
	c := dial(t, s, rt.Options{})
	c1, err := c.CounterNew(1)
	wantNoErr(t, "CounterNew", err)
	c2, err := c.CounterNew(2)
	wantNoErr(t, "CounterNew", err)
	_, err = c.CursorOpen(c1, 1)
	wantNoErr(t, "CursorOpen", err)
	_, err = c.CursorOpen(c2, 1)
	wantNoErr(t, "CursorOpen", err)

	wantNoErr(t, "Close", c.Close())
	// CLOSE_ACK is sent after the teardown, so the trace is complete when Close returns.
	if got := s.traceFuncs(destroyFuncs...); !slices.Equal(got, wantTeardown) {
		t.Fatalf("teardown order:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(wantTeardown, "\n"))
	}
}

var wantTeardown = []string{
	"ml_cursor_free cursor=2 ret=2",
	"ml_cursor_free cursor=1 ret=2",
	"ml_counter_free counter=2",
	"ml_counter_free counter=1",
}

// TestTeardownWhenTheClientDetaches: the link is closed without the CLOSE exchange (Conn.Kill). The
// server sees the detach, drains, and runs the same teardown.
func TestTeardownWhenTheClientDetaches(t *testing.T) {
	s := startServer(t, serverOpts{})
	c := dial(t, s, rt.Options{})
	c1, err := c.CounterNew(1)
	wantNoErr(t, "CounterNew", err)
	c2, err := c.CounterNew(2)
	wantNoErr(t, "CounterNew", err)
	_, err = c.CursorOpen(c1, 1)
	wantNoErr(t, "CursorOpen", err)
	_, err = c.CursorOpen(c2, 1)
	wantNoErr(t, "CursorOpen", err)

	c.Kill()
	got := s.waitTrace(len(wantTeardown), destroyFuncs...)
	if !slices.Equal(got, wantTeardown) {
		t.Fatalf("teardown order:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(wantTeardown, "\n"))
	}
}

// TestTeardownIsPerSession: closing one session destroys only its own objects.
func TestTeardownIsPerSession(t *testing.T) {
	s := startServer(t, serverOpts{})
	a := dial(t, s, rt.Options{})
	b := dial(t, s, rt.Options{})
	ca, err := a.CounterNew(1) // counter 1
	wantNoErr(t, "a.CounterNew", err)
	cb, err := b.CounterNew(2) // counter 2
	wantNoErr(t, "b.CounterNew", err)
	_, err = b.CursorOpen(cb, 1) // cursor 1
	wantNoErr(t, "b.CursorOpen", err)

	wantNoErr(t, "b.Close", b.Close())
	want := []string{"ml_cursor_free cursor=1 ret=2", "ml_counter_free counter=2"}
	if got := s.traceFuncs(destroyFuncs...); !slices.Equal(got, want) {
		t.Fatalf("destroy calls after closing b:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if total, err := a.CounterAdd(ca, 1); err != nil || total != 2 {
		t.Fatalf("a's counter after b closed = (%d, %v), want (2, nil)", total, err)
	}
}

// TestHandleTableDoesNotLeak: create and destroy more objects than the table holds, on one session,
// and again across many sessions: slots and generations are recycled and the library sees every
// object freed exactly once.
func TestHandleTableDoesNotLeak(t *testing.T) {
	s := startServer(t, serverOpts{args: []string{"--max-handles", "8"}})
	c := dial(t, s, rt.Options{})
	const rounds = 300
	for i := range rounds {
		ctr, err := c.CounterNew(int64(i))
		wantNoErr(t, "CounterNew", err)
		cur, err := c.CursorOpen(ctr, 1)
		wantNoErr(t, "CursorOpen", err)
		if i%2 == 0 {
			wantLib(t, "CursorFree", c.CursorFree(cur), "ml_cursor_free", 2, "")
			wantNoErr(t, "CounterFree", c.CounterFree(ctr))
		} else {
			wantNoErr(t, "CounterFree with a live cursor (cascade)", c.CounterFree(ctr))
		}
	}
	wantNoErr(t, "Close", c.Close())
	counts := map[string]int{}
	for _, l := range s.traceLines() {
		name, _, _ := strings.Cut(l, " ")
		counts[name]++
	}
	if counts["ml_counter_new"] != rounds || counts["ml_counter_free"] != rounds ||
		counts["ml_cursor_open"] != rounds || counts["ml_cursor_free"] != rounds {
		t.Fatalf("object counts: %v, want %d of each of new/free for both types", counts, rounds)
	}
}

// TestClientKilled: kill -9 of the client process. The server notices the dead peer within the peer
// timeout, tears the session down in the order of 5.2 and keeps serving: a new client works and the
// dead one left no shared memory name behind.
func TestClientKilled(t *testing.T) {
	s := startServer(t, serverOpts{args: fastServerArgs})
	h := startHelper(t, "client", envListener+"="+s.name)
	// The helper made counter 1, counter 2, cursor 1 and cursor 2 and printed "ready".
	h.kill(t)

	got := s.waitTrace(len(wantTeardown), destroyFuncs...)
	if !slices.Equal(got, wantTeardown) {
		t.Fatalf("teardown after kill -9 of the client:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(wantTeardown, "\n"))
	}

	// The server stays up for a new client, and the library state of the dead session is gone.
	c := fastDial(t, s, rt.Options{})
	ctr, err := c.CounterNew(5)
	wantNoErr(t, "CounterNew on the new session", err)
	if total, err := c.CounterAdd(ctr, 1); err != nil || total != 6 {
		t.Fatalf("CounterAdd = (%d, %v)", total, err)
	}
	wantNoErr(t, "Close", c.Close())

	eventually(t, "the segments of the dead client to be gone", s.onlyListenerLeft)
}
