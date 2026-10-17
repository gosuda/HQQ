//go:build unix

package microlib_test

import (
	"errors"
	"math"
	"testing"

	"gosuda.org/hqq/bindgen/e2e/microlib"
	"gosuda.org/hqq/bindgen/rt"
)

// The Go model of microlib: what bindgen/testdata/microlib/microlib.c and the hooks of
// server/microlib_hooks.c compute, written from their contracts and not from the generated code. The
// differential test compares the real server with it; the unit tests use its helpers for expected
// values.

const (
	// hookClamp is the largest delta the interceptor of ml_counter_add lets through.
	hookClamp = 1000000
	// cursorLen is the number of values a cursor yields (microlib.h).
	cursorLen = 3
	// cellsMax is the bound of Cell[]<=256.
	cellsMax = 256
	// xorMax is the bound of ml_xor's src, bytes<=65536.
	xorMax = 65536
)

// fnv1a is ml_hash: FNV-1a over the bytes of s.
func fnv1a(s string) uint64 {
	h := uint64(0xcbf29ce484222325)
	for i := range len(s) {
		h ^= uint64(s[i])
		h *= 0x00000100000001b3
	}
	return h
}

// xorModel is ml_xor: src[i] ^ key.
func xorModel(src []byte, key byte) []byte {
	out := make([]byte, len(src))
	for i, b := range src {
		out[i] = b ^ key
	}
	return out
}

// cellsModel is the ml_cells shim: max cells (negative is 0, at most 256), cycling Int(-2 + i/3),
// Text "hi", Null.
func cellsModel(max int32) []microlib.Cell {
	n := int(max)
	if n < 0 {
		n = 0
	}
	n = min(n, cellsMax)
	out := make([]microlib.Cell, 0, n)
	for i := range n {
		switch i % 3 {
		case 0:
			out = append(out, microlib.Cell{Tag: microlib.CellInt, Int: int64(-2 + i/3)})
		case 1:
			out = append(out, microlib.Cell{Tag: microlib.CellText, Text: "hi"})
		default:
			out = append(out, microlib.Cell{Tag: microlib.CellNull})
		}
	}
	return out
}

// mlCounter and mlCursor are the library objects of the model.
type mlCounter struct {
	total int64
	// cursors are the live children, in creation order.
	cursors []*mlCursor
	freed   bool
}

type mlCursor struct {
	next, step int64
	left       int
	parent     *mlCounter
	freed      bool
}

func newCounterModel(start int64) *mlCounter { return &mlCounter{total: start} }

// add is the interceptor and ml_counter_add: it returns the new total, a library error code and
// detail (libErr true), or the status the interceptor answered (status nonzero).
type addResult struct {
	total  int64
	status uint32 // 5 when the interceptor refused
	libErr bool
	detail string
}

func (c *mlCounter) add(delta int64) addResult {
	if delta < 0 {
		return addResult{status: 5}
	}
	delta = min(delta, hookClamp)
	if c.total > math.MaxInt64-delta {
		return addResult{libErr: true, detail: "overflow"}
	}
	c.total += delta
	return addResult{total: c.total}
}

// check is ml_counter_check: ok, or a library failure with the detail "over limit".
func (c *mlCounter) check(limit int32) (ok bool) { return c.total <= int64(limit) }

func (c *mlCounter) open(step int32) *mlCursor {
	cur := &mlCursor{next: c.total, step: int64(step), left: cursorLen, parent: c}
	c.cursors = append(c.cursors, cur)
	return cur
}

// next is ml_cursor_next: the value and ML_OK, or 0 and ML_EMPTY.
func (cur *mlCursor) nextValue() (int32, int64) {
	if cur.left == 0 {
		return microlib.ML_EMPTY, 0
	}
	v := cur.next
	cur.next = int64(uint64(cur.next) + uint64(cur.step))
	cur.left--
	return microlib.ML_OK, v
}

// free is ml_cursor_free: ML_PARTIAL when values were unread.
func (cur *mlCursor) free() int32 {
	cur.freed = true
	if cur.left > 0 {
		return microlib.ML_PARTIAL
	}
	return microlib.ML_OK
}

// free destroys the counter after its live cursors, newest first, as a cascade does.
func (c *mlCounter) free() {
	for i := len(c.cursors) - 1; i >= 0; i-- {
		if !c.cursors[i].freed {
			c.cursors[i].free()
		}
	}
	c.freed = true
}

// Assertions on the error layers of BINDGEN.md 6.1.

func wantStatus(t testing.TB, what string, err error, fn string, status uint32) {
	t.Helper()
	var se *rt.StatusError
	if !errors.As(err, &se) {
		t.Fatalf("%s: error = %v (%T), want *rt.StatusError with status %d", what, err, err, status)
	}
	if se.Status != status || se.Func != fn {
		t.Fatalf("%s: StatusError{Func: %q, Status: %d}, want {%q, %d}", what, se.Func, se.Status, fn, status)
	}
}

func wantLib(t testing.TB, what string, err error, fn string, code int64, detail string) {
	t.Helper()
	var le *rt.LibError
	if !errors.As(err, &le) {
		t.Fatalf("%s: error = %v (%T), want *rt.LibError", what, err, err)
	}
	if le.Func != fn || le.Code != code || le.Detail != detail {
		t.Fatalf("%s: LibError{Func: %q, Code: %d, Detail: %q}, want {%q, %d, %q}",
			what, le.Func, le.Code, le.Detail, fn, code, detail)
	}
}

func wantNoErr(t testing.TB, what string, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("%s: %v", what, err)
	}
}
