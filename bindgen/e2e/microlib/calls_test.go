//go:build unix

package microlib_test

import (
	"bytes"
	"errors"
	"math"
	"slices"
	"strings"
	"testing"

	"gosuda.org/hqq/bindgen/e2e/microlib"
	"gosuda.org/hqq/bindgen/rt"
)

// TestEveryFunctionOnce calls every wire function of microlib.idl once over real shared memory and
// compares the result with microlib.c, and the ML_TRACE file with the calls the server must have made.
func TestEveryFunctionOnce(t *testing.T) {
	s := startServer(t, serverOpts{})
	c := dial(t, s, rt.Options{})

	sum, err := c.Add(40, 2)
	wantNoErr(t, "Add", err)
	if sum != 42 {
		t.Fatalf("Add(40, 2) = %d, want 42", sum)
	}

	ctr, err := c.CounterNew(100)
	wantNoErr(t, "CounterNew", err)
	if ctr.IsNil() {
		t.Fatal("CounterNew returned the nil handle")
	}
	total, err := c.CounterAdd(ctr, 5)
	wantNoErr(t, "CounterAdd", err)
	if total != 105 {
		t.Fatalf("CounterAdd(5) total = %d, want 105", total)
	}
	wantNoErr(t, "CounterCheck within the limit", c.CounterCheck(ctr, 105))

	cur, err := c.CursorOpen(ctr, 10)
	wantNoErr(t, "CursorOpen", err)
	for i, want := range []int64{105, 115, 125} {
		ret, v, err := c.CursorNext(cur)
		wantNoErr(t, "CursorNext", err)
		if ret != microlib.ML_OK || v != want {
			t.Fatalf("CursorNext #%d = (%d, %d), want (ML_OK, %d)", i, ret, v, want)
		}
	}
	ret, v, err := c.CursorNext(cur)
	wantNoErr(t, "CursorNext at the end", err)
	if ret != microlib.ML_EMPTY || v != 0 {
		t.Fatalf("CursorNext at the end = (%d, %d), want (ML_EMPTY, 0): the out value must not leak stack contents", ret, v)
	}
	wantNoErr(t, "CursorFree of an exhausted cursor", c.CursorFree(cur))

	src := []byte("hqq-bindgen")
	dst := make([]byte, 64)
	got, err := c.Xor(src, 0x5a, dst)
	wantNoErr(t, "Xor", err)
	if !bytes.Equal(got, xorModel(src, 0x5a)) {
		t.Fatalf("Xor = %x, want %x", got, xorModel(src, 0x5a))
	}

	h, err := c.Hash("foobar")
	wantNoErr(t, "Hash", err)
	if h != 0x85944171f73967e8 {
		t.Fatalf("Hash(foobar) = %#x, want the FNV-1a vector 0x85944171f73967e8", h)
	}

	cells, err := c.Cells(ctr, 5, nil)
	wantNoErr(t, "Cells", err)
	if want := cellsModel(5); !slices.Equal(cells, want) {
		t.Fatalf("Cells(5) = %+v, want %+v", cells, want)
	}

	wantNoErr(t, "CounterFree", c.CounterFree(ctr))
	wantNoErr(t, "Close", c.Close())

	want := []string{
		"ml_add a=40 b=2 ret=42",
		"ml_counter_new start=100 counter=1",
		"ml_counter_add counter=1 delta=5 ret=0 total=105",
		"ml_counter_check counter=1 limit=105 ret=0",
		"ml_cursor_open counter=1 step=10 cursor=1",
		"ml_cursor_next cursor=1 ret=0 value=105",
		"ml_cursor_next cursor=1 ret=0 value=115",
		"ml_cursor_next cursor=1 ret=0 value=125",
		"ml_cursor_next cursor=1 ret=1",
		"ml_cursor_free cursor=1 ret=0",
		"ml_xor n=11 key=90 cap=64 ret=11",
		"ml_xor_failed result=11 ret=0",
		"ml_hash len=6 ret=9625390261332436968",
		"ml_counter_free counter=1",
	}
	if gotTrace := s.traceLines(); !slices.Equal(gotTrace, want) {
		t.Fatalf("trace:\n%s\nwant:\n%s", strings.Join(gotTrace, "\n"), strings.Join(want, "\n"))
	}
}

// TestAddWraps: ml_add is addition modulo 2^32, so the result is not saturated or widened.
func TestAddWraps(t *testing.T) {
	s := startServer(t, serverOpts{noTrace: true})
	c := dial(t, s, rt.Options{})
	for _, tc := range []struct{ a, b, want int32 }{
		{math.MaxInt32, 1, math.MinInt32},
		{math.MinInt32, -1, math.MaxInt32},
		{math.MinInt32, math.MinInt32, 0},
		{-7, 7, 0},
	} {
		got, err := c.Add(tc.a, tc.b)
		wantNoErr(t, "Add", err)
		if got != tc.want {
			t.Errorf("Add(%d, %d) = %d, want %d", tc.a, tc.b, got, tc.want)
		}
	}
}

// TestInterceptor: the hook of ml_counter_add refuses a negative delta without calling the library
// and clamps a large one before the library sees it.
func TestInterceptor(t *testing.T) {
	s := startServer(t, serverOpts{})
	c := dial(t, s, rt.Options{})
	ctr, err := c.CounterNew(0)
	wantNoErr(t, "CounterNew", err)

	_, err = c.CounterAdd(ctr, -1)
	wantStatus(t, "CounterAdd(-1)", err, "ml_counter_add", 5)
	if !errors.Is(err, rt.ErrDenied) {
		t.Errorf("CounterAdd(-1) error %v does not match rt.ErrDenied", err)
	}
	if errors.Is(err, rt.ErrOutcomeUnknown) {
		t.Errorf("CounterAdd(-1): a veto is a known outcome, but the error matches rt.ErrOutcomeUnknown")
	}

	total, err := c.CounterAdd(ctr, 5_000_000)
	wantNoErr(t, "CounterAdd(5000000)", err)
	if total != hookClamp {
		t.Fatalf("CounterAdd(5000000) total = %d, want the clamp %d", total, hookClamp)
	}
	total, err = c.CounterAdd(ctr, math.MaxInt64)
	wantNoErr(t, "CounterAdd(MaxInt64)", err)
	if total != 2*hookClamp {
		t.Fatalf("CounterAdd(MaxInt64) total = %d, want %d", total, 2*hookClamp)
	}
	// The session survives a veto.
	if _, err := c.Add(1, 1); err != nil {
		t.Fatalf("Add after a veto: %v", err)
	}
	wantNoErr(t, "Close", c.Close())

	got := s.traceFuncs("ml_counter_add")
	want := []string{
		"ml_counter_add counter=1 delta=1000000 ret=0 total=1000000",
		"ml_counter_add counter=1 delta=1000000 ret=0 total=2000000",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("library saw %q, want %q: the vetoed call must not reach the library, a large one is clamped", got, want)
	}
}

// TestLibraryFailureWithDetail: a library failure carries the return value and the text of
// ml_counter_errmsg, read inside the same call.
func TestLibraryFailureWithDetail(t *testing.T) {
	s := startServer(t, serverOpts{noTrace: true})
	c := dial(t, s, rt.Options{})

	ctr, err := c.CounterNew(math.MaxInt64 - 1)
	wantNoErr(t, "CounterNew", err)
	total, err := c.CounterAdd(ctr, 1)
	wantNoErr(t, "CounterAdd(1)", err)
	if total != math.MaxInt64 {
		t.Fatalf("total = %d, want MaxInt64", total)
	}
	total, err = c.CounterAdd(ctr, 1)
	wantLib(t, "overflowing CounterAdd", err, "ml_counter_add", -1, "overflow")
	if total != 0 {
		t.Errorf("total after a failed add = %d, want 0: out values of a failure are zero", total)
	}
	var le *rt.LibError
	if !errors.As(err, &le) || errors.Is(err, rt.ErrOutcomeUnknown) {
		t.Errorf("a library failure is a known outcome, got %v", err)
	}

	// A different function with a different message on the same counter.
	wantLib(t, "CounterCheck over the limit", c.CounterCheck(ctr, 10), "ml_counter_check", -1, "over limit")

	// The message is the one of the latest call: a success clears it, so a later failure of the same
	// function reports its own text and not a stale one.
	small, err := c.CounterNew(5)
	wantNoErr(t, "CounterNew", err)
	wantNoErr(t, "CounterCheck ok", c.CounterCheck(small, 5))
	wantLib(t, "CounterCheck failing after a success", c.CounterCheck(small, 4), "ml_counter_check", -1, "over limit")

	// NULL from ml_counter_new cannot be provoked (out of memory), but ML_EMPTY is an ok status.
	cur, err := c.CursorOpen(small, 1)
	wantNoErr(t, "CursorOpen", err)
	for range cursorLen {
		_, _, err = c.CursorNext(cur)
		wantNoErr(t, "CursorNext", err)
	}
	ret, _, err := c.CursorNext(cur)
	wantNoErr(t, "CursorNext at the end", err)
	if ret != microlib.ML_EMPTY {
		t.Fatalf("CursorNext at the end returned %d, want ML_EMPTY (an ok value, not an error)", ret)
	}
}

// TestFailedDestroyConsumesTheHandle: ml_cursor_free reports ML_PARTIAL for an unread cursor and frees
// it anyway, so the client sees a library failure and the handle is gone (BINDGEN.md 5.1, vectors R10
// and R11). microlib has no destroy function with @retain_on_fail, so the retained-entry arm of 5.1 is
// covered by the C runtime tests (c/tests/test_rpc_handles.c), not here.
func TestFailedDestroyConsumesTheHandle(t *testing.T) {
	s := startServer(t, serverOpts{})
	c := dial(t, s, rt.Options{})
	ctr, err := c.CounterNew(1)
	wantNoErr(t, "CounterNew", err)
	cur, err := c.CursorOpen(ctr, 1)
	wantNoErr(t, "CursorOpen", err)
	_, _, err = c.CursorNext(cur)
	wantNoErr(t, "CursorNext", err)

	wantLib(t, "CursorFree of an unread cursor", c.CursorFree(cur), "ml_cursor_free", int64(microlib.ML_PARTIAL), "")
	wantStatus(t, "second CursorFree", c.CursorFree(cur), "ml_cursor_free", 3)
	_, _, err = c.CursorNext(cur)
	wantStatus(t, "CursorNext on the consumed handle", err, "ml_cursor_next", 3)
	if !errors.Is(err, rt.ErrBadHandle) {
		t.Errorf("error %v does not match rt.ErrBadHandle", err)
	}
	// The counter is a separate object and untouched.
	total, err := c.CounterAdd(ctr, 1)
	wantNoErr(t, "CounterAdd on the parent", err)
	if total != 2 {
		t.Fatalf("total = %d, want 2", total)
	}
	wantNoErr(t, "Close", c.Close())
	if got := s.traceFuncs("ml_cursor_free"); !slices.Equal(got, []string{"ml_cursor_free cursor=1 ret=2"}) {
		t.Fatalf("cursor destroy calls %q: the consumed handle must not be destroyed again by teardown", got)
	}
}

// TestHash covers the FNV-1a vectors, the longest allowed string and the client-side refusals.
func TestHash(t *testing.T) {
	s := startServer(t, serverOpts{noTrace: true})
	c := dial(t, s, rt.Options{})
	long := strings.Repeat("x", 255)
	for _, in := range []string{"", "a", "foobar", "caf\u00e9", long} {
		got, err := c.Hash(in)
		wantNoErr(t, "Hash", err)
		if want := fnv1a(in); got != want {
			t.Errorf("Hash(%.20q, len %d) = %#x, want %#x", in, len(in), got, want)
		}
	}
	for name, in := range map[string]string{"256 bytes": long + "x", "embedded NUL": "a\x00b"} {
		_, err := c.Hash(in)
		if !errors.Is(err, rt.ErrInvalidArg) {
			t.Errorf("Hash(%s) error = %v, want rt.ErrInvalidArg (refused before anything is sent)", name, err)
		}
	}
	// A refusal commits nothing, so the session is intact.
	if got, err := c.Hash("a"); err != nil || got != fnv1a("a") {
		t.Fatalf("Hash after the refusals = (%#x, %v)", got, err)
	}
}

// TestCells: the shim writes a union array through the response writer.
func TestCells(t *testing.T) {
	s := startServer(t, serverOpts{noTrace: true})
	c := dial(t, s, rt.Options{})
	ctr, err := c.CounterNew(0)
	wantNoErr(t, "CounterNew", err)
	var dst []microlib.Cell
	for _, max := range []int32{0, 1, 2, 3, 4, 7, 100, 255, 256, 257, 1000, math.MaxInt32, -1, math.MinInt32} {
		got, err := c.Cells(ctr, max, dst)
		wantNoErr(t, "Cells", err)
		if want := cellsModel(max); !slices.Equal(got, want) {
			t.Fatalf("Cells(%d): %d cells, want %d (first difference at %d)", max, len(got), len(want), firstDiff(got, want))
		}
		dst = got
	}
	wantStatus(t, "Cells on a freed counter", func() error {
		wantNoErr(t, "CounterFree", c.CounterFree(ctr))
		_, err := c.Cells(ctr, 1, nil)
		return err
	}(), "ml_cells", 3)
}

func firstDiff[T comparable](a, b []T) int {
	for i := range min(len(a), len(b)) {
		if a[i] != b[i] {
			return i
		}
	}
	return min(len(a), len(b))
}

// TestHandleTypeAndSessionIsolation: a handle id is an index into the table of one session, so another
// session's id reaches only the caller's own objects, and a handle of another type is refused.
func TestHandleTypeAndSessionIsolation(t *testing.T) {
	s := startServer(t, serverOpts{noTrace: true})
	c1 := dial(t, s, rt.Options{})
	c2 := dial(t, s, rt.Options{})

	a, err := c1.CounterNew(100)
	wantNoErr(t, "c1.CounterNew", err)
	b, err := c2.CounterNew(7)
	wantNoErr(t, "c2.CounterNew", err)
	if a.ID() != b.ID() {
		t.Fatalf("the first handle of two sessions differs (%#x, %#x): ids are table-local", a.ID(), b.ID())
	}

	// The client refuses a handle of another session before it sends anything.
	_, err = c2.CounterAdd(a, 1)
	if !errors.Is(err, rt.ErrForeignHandle) {
		t.Fatalf("c2.CounterAdd(handle of c1) error = %v, want rt.ErrForeignHandle", err)
	}
	// A forged stamp reaches c2's own table: the object is c2's counter, not c1's.
	forged := microlib.Counter{Handle: c2.Conn().NewHandle(a.ID())}
	total, err := c2.CounterAdd(forged, 1)
	wantNoErr(t, "c2.CounterAdd(forged)", err)
	if total != 8 {
		t.Fatalf("total through the forged handle = %d, want 8 (c2's counter), not 101", total)
	}
	total, err = c1.CounterAdd(a, 1)
	wantNoErr(t, "c1.CounterAdd", err)
	if total != 101 {
		t.Fatalf("c1's counter total = %d, want 101: c2's call must not touch it", total)
	}

	// A handle id of the wrong type is E_BAD_HANDLE.
	cur, err := c1.CursorOpen(a, 1)
	wantNoErr(t, "CursorOpen", err)
	_, err = c1.CounterAdd(microlib.Counter{Handle: cur.Handle}, 1)
	wantStatus(t, "CounterAdd(cursor handle)", err, "ml_counter_add", 3)

	// The nil handle is refused by the client.
	_, err = c1.CounterAdd(microlib.Counter{}, 1)
	if !errors.Is(err, rt.ErrInvalidArg) {
		t.Fatalf("CounterAdd(nil handle) error = %v, want rt.ErrInvalidArg", err)
	}
	// An id that was never issued is E_BAD_HANDLE at the server.
	_, err = c1.CounterAdd(microlib.Counter{Handle: c1.Conn().NewHandle(0x7fff_0001)}, 1)
	wantStatus(t, "CounterAdd(never issued)", err, "ml_counter_add", 3)
}

// TestStaleHandleAfterFree: a freed handle is E_BAD_HANDLE even after its table index was reused, because
// the id carries a generation (BINDGEN.md 4.9).
func TestStaleHandleAfterFree(t *testing.T) {
	s := startServer(t, serverOpts{noTrace: true})
	c := dial(t, s, rt.Options{})
	old, err := c.CounterNew(1)
	wantNoErr(t, "CounterNew", err)
	wantNoErr(t, "CounterFree", c.CounterFree(old))
	_, err = c.CounterAdd(old, 1)
	wantStatus(t, "CounterAdd(freed)", err, "ml_counter_add", 3)
	wantStatus(t, "CounterFree(freed)", c.CounterFree(old), "ml_counter_free", 3)

	fresh, err := c.CounterNew(50)
	wantNoErr(t, "CounterNew", err)
	if fresh.ID()&0xffff != old.ID()&0xffff {
		t.Fatalf("the freed table index was not reused: %#x then %#x", old.ID(), fresh.ID())
	}
	if fresh.ID() == old.ID() {
		t.Fatalf("the reused index kept its generation: %#x", fresh.ID())
	}
	_, err = c.CounterAdd(old, 1)
	wantStatus(t, "CounterAdd(stale generation)", err, "ml_counter_add", 3)
	total, err := c.CounterAdd(fresh, 1)
	wantNoErr(t, "CounterAdd(fresh)", err)
	if total != 51 {
		t.Fatalf("total = %d, want 51: the stale handle must not have reached the new object", total)
	}
}

// TestMaxHandles: the handle table of a session is bounded, a call that would exceed it is refused
// before the library runs, and a freed slot is usable again.
func TestMaxHandles(t *testing.T) {
	s := startServer(t, serverOpts{args: []string{"--max-handles", "4"}})
	c := dial(t, s, rt.Options{})
	if got := c.Conn().Limits().MaxHandles; got != 4 {
		t.Fatalf("Limits().MaxHandles = %d, want the server's 4", got)
	}
	var ctrs []microlib.Counter
	for i := range 3 {
		ctr, err := c.CounterNew(int64(i))
		wantNoErr(t, "CounterNew", err)
		ctrs = append(ctrs, ctr)
	}
	cur, err := c.CursorOpen(ctrs[0], 1) // cursors share the table: this is the fourth entry
	wantNoErr(t, "CursorOpen", err)

	_, err = c.CounterNew(99)
	wantStatus(t, "CounterNew with a full table", err, "ml_counter_new", 4)
	if !errors.Is(err, rt.ErrLimit) {
		t.Errorf("error %v does not match rt.ErrLimit", err)
	}
	_, err = c.CursorOpen(ctrs[1], 1)
	wantStatus(t, "CursorOpen with a full table", err, "ml_cursor_open", 4)

	// Freeing one entry makes room; the refused calls never reached the library.
	wantLib(t, "CursorFree", c.CursorFree(cur), "ml_cursor_free", int64(microlib.ML_PARTIAL), "")
	again, err := c.CounterNew(99)
	wantNoErr(t, "CounterNew after a free", err)
	total, err := c.CounterAdd(again, 1)
	wantNoErr(t, "CounterAdd", err)
	if total != 100 {
		t.Fatalf("total = %d, want 100", total)
	}
	wantNoErr(t, "Close", c.Close())
	if got := s.traceFuncs("ml_counter_new"); len(got) != 4 {
		t.Fatalf("library saw %d ml_counter_new calls, want 4: the refused ones must not run:\n%s", len(got), strings.Join(got, "\n"))
	}
	if got := s.traceFuncs("ml_cursor_open"); len(got) != 1 {
		t.Fatalf("library saw %d ml_cursor_open calls, want 1", len(got))
	}
}
