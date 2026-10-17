//go:build unix

package microlib_test

import (
	"errors"
	"fmt"
	"math"
	"math/rand/v2"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"

	"gosuda.org/hqq/bindgen/e2e/microlib"
	"gosuda.org/hqq/bindgen/rt"
)

// The differential test: random call sequences against the real server, compared call by call with the
// Go model of microlib (model_test.go). A sequence is a mix of pipelined calls whose futures are waited
// for in random order, library failures, interceptor vetoes, stale, mistyped, foreign and nil handles,
// out-of-bounds arguments, malformed bodies and unknown functions. The model computes every expected
// result at the moment the call is issued, which is the order the server executes them in (FIFO,
// BINDGEN.md 4.12). Each sequence is a function of the fixed seed and its index alone: set
// HQQ_E2E_DIFF_SEQ=N to replay sequence N only, with its operation log.

const (
	diffSeed      = 0x68717162696e6467 // "hqqbindg"
	diffSequences = 10_000
	envDiffSeq    = "HQQ_E2E_DIFF_SEQ"
)

// geometries are the session shapes the sequences run on: (B, S). S = 128 and B = 2 are the minimum
// the protocol allows; they put the framing and the flow control under the load of every call.
var geometries = [][2]int{{64, 16384}, {64, 4096}, {16, 1024}, {8, 256}, {4, 128}, {2, 16384}, {2, 128}, {32, 128}}

// diffCounter and diffCursor pair a model object with the client handle, which exists once the
// future that created it was waited for.
type diffCounter struct {
	m     *mlCounter
	h     microlib.Counter
	ready bool
}

type diffCursor struct {
	m      *mlCursor
	h      microlib.Cursor
	parent *diffCounter
	ready  bool
}

// future is one issued call whose result is compared with the model when it is waited for. wait(true)
// is the call after Conn.Sync discarded it: the future must then report rt.ErrFutureUsed.
type future struct {
	desc string
	wait func(synced bool) error
}

// diffRun is the state of one sequence.
type diffRun struct {
	c       *microlib.Client
	foreign microlib.Counter // a handle of another session
	rng     *rand.Rand

	counters []*diffCounter
	cursors  []*diffCursor
	staleCtr []microlib.Counter
	staleCur []microlib.Cursor
	pending  []future
	log      []string
	// leaked is set when Sync discarded a future that created an object: its handle is lost and the
	// object lives until the session ends.
	leaked bool
	// failure is the first mismatch found by a Wait.
	failure error
}

func (r *diffRun) note(format string, args ...any) {
	r.log = append(r.log, fmt.Sprintf(format, args...))
}

func (r *diffRun) liveCounters() []*diffCounter {
	var out []*diffCounter
	for _, c := range r.counters {
		if c.ready && !c.m.freed {
			out = append(out, c)
		}
	}
	return out
}

func (r *diffRun) liveCursors() []*diffCursor {
	var out []*diffCursor
	for _, k := range r.cursors {
		if k.ready && !k.m.freed {
			out = append(out, k)
		}
	}
	return out
}

func pick[T any](rng *rand.Rand, xs []T) T { return xs[rng.IntN(len(xs))] }

// errCheck helpers return a description of a mismatch instead of failing the test, so that the
// sequence can print its log.
func checkStatus(err error, fn string, status uint32) error {
	var se *rt.StatusError
	if !errors.As(err, &se) || se.Func != fn || se.Status != status {
		return fmt.Errorf("error %v (%T), want StatusError{%s, %d}", err, err, fn, status)
	}
	return nil
}

func checkLib(err error, fn string, code int64, detail string) error {
	var le *rt.LibError
	if !errors.As(err, &le) || le.Func != fn || le.Code != code || le.Detail != detail {
		return fmt.Errorf("error %v (%T), want LibError{%s, %d, %q}", err, err, fn, code, detail)
	}
	return nil
}

func checkIs(err, target error) error {
	if !errors.Is(err, target) {
		return fmt.Errorf("error %v, want %v", err, target)
	}
	return nil
}

func checkNil(err error) error {
	if err != nil {
		return fmt.Errorf("unexpected error %v", err)
	}
	return nil
}

// issue appends a future. When the call itself failed at issue time (the client refused it), the
// outcome is checked at once with immediate and nothing is pending.
func (r *diffRun) issue(desc string, issueErr error, f future) {
	r.note("%s", desc)
	if issueErr != nil {
		r.fail(desc, fmt.Errorf("issue failed: %w", issueErr))
		return
	}
	r.pending = append(r.pending, f)
}

func (r *diffRun) fail(desc string, err error) {
	if err != nil && r.failure == nil {
		r.failure = fmt.Errorf("%s: %w", desc, err)
	}
}

// immediate records the outcome of a call that the client must refuse before it commits anything.
func (r *diffRun) immediate(desc string, err error, want error) {
	r.note("%s -> %v", desc, want)
	if !errors.Is(err, want) {
		r.fail(desc, fmt.Errorf("error %v, want %v at issue time", err, want))
	}
}

// waitOne waits for the pending future at index i and compares it with the model.
func (r *diffRun) waitOne(i int, synced bool) {
	f := r.pending[i]
	r.pending = slices.Delete(r.pending, i, i+1)
	r.fail(f.desc, f.wait(synced))
}

func (r *diffRun) drain() {
	for len(r.pending) > 0 {
		r.waitOne(r.rng.IntN(len(r.pending)), false)
	}
}

var edgeI64 = []int64{0, 1, -1, 7, 1000, math.MaxInt64, math.MaxInt64 - 1, math.MaxInt64 - 2, math.MinInt64, math.MinInt64 + 1, hookClamp, hookClamp + 1, 424243}

func (r *diffRun) randI64() int64 {
	switch r.rng.IntN(4) {
	case 0:
		return pick(r.rng, edgeI64)
	case 1:
		return int64(r.rng.IntN(2000)) - 1000
	case 2:
		return int64(r.rng.Uint64())
	}
	return int64(r.rng.IntN(5_000_000))
}

func (r *diffRun) randI32() int32 {
	switch r.rng.IntN(3) {
	case 0:
		return pick(r.rng, []int32{0, 1, -1, math.MaxInt32, math.MinInt32})
	case 1:
		return int32(r.rng.IntN(200)) - 100
	}
	return int32(r.rng.Uint32())
}

// step performs one random operation. Operations on an object need its handle, which exists after the
// future that created it was waited for; when there is none, the step waits for a pending creation (or
// creates one).
func (r *diffRun) step() {
	cs, ks := r.liveCounters(), r.liveCursors()
	n := r.rng.IntN(100)
	needCounter := func() bool {
		if len(cs) > 0 {
			return false
		}
		if len(r.pending) > 0 && r.rng.IntN(3) != 0 {
			r.waitOne(r.rng.IntN(len(r.pending)), false)
		} else {
			r.opCounterNew()
		}
		return true
	}
	needCursor := func() bool {
		if len(ks) > 0 {
			return false
		}
		switch {
		case len(cs) > 0 && r.rng.IntN(2) == 0:
			r.opCursorOpen(pick(r.rng, cs))
		case len(r.pending) > 0:
			r.waitOne(r.rng.IntN(len(r.pending)), false)
		default:
			r.opCounterNew()
		}
		return true
	}
	switch {
	case n < 6:
		r.opAdd()
	case n < 12:
		r.opCounterNew()
	case n < 20:
		if !needCounter() {
			r.opCounterAdd(pick(r.rng, cs))
		}
	case n < 23:
		if !needCounter() {
			r.opCounterCheck(pick(r.rng, cs))
		}
	case n < 26:
		if !needCounter() {
			r.opCounterFree(pick(r.rng, cs))
		}
	case n < 33:
		if !needCounter() {
			r.opCursorOpen(pick(r.rng, cs))
		}
	case n < 45:
		if !needCursor() {
			r.opCursorNext(pick(r.rng, ks))
		}
	case n < 50:
		if !needCursor() {
			r.opCursorFree(pick(r.rng, ks))
		}
	case n < 58:
		r.opXor()
	case n < 62:
		r.opHash()
	case n < 66:
		if !needCounter() {
			r.opCells(pick(r.rng, cs))
		}
	case n < 78:
		r.opBadHandle(cs, ks)
	case n < 82:
		r.opMalformed()
	case n < 84:
		r.opSync()
	default:
		// wait: out of order most of the time, oldest first otherwise
		if len(r.pending) > 0 {
			i := 0
			if r.rng.IntN(4) != 0 {
				i = r.rng.IntN(len(r.pending))
			}
			r.waitOne(i, false)
		}
	}
}

func (r *diffRun) opAdd() {
	a, b := r.randI32(), r.randI32()
	want := int32(uint32(a) + uint32(b))
	f, err := r.c.AddAsync(a, b)
	r.issue(fmt.Sprintf("Add(%d, %d)", a, b), err, future{fmt.Sprintf("Add(%d, %d)", a, b), func(synced bool) error {
		got, err := f.Wait()
		if synced {
			return checkIs(err, rt.ErrFutureUsed)
		}
		if err != nil || got != want {
			return fmt.Errorf("= (%d, %v), want %d", got, err, want)
		}
		return nil
	}})
}

func (r *diffRun) opCounterNew() {
	start := r.randI64()
	dc := &diffCounter{m: newCounterModel(start)}
	r.counters = append(r.counters, dc)
	f, err := r.c.CounterNewAsync(start)
	desc := fmt.Sprintf("CounterNew(%d) #%d", start, len(r.counters))
	r.issue(desc, err, future{desc, func(synced bool) error {
		h, err := f.Wait()
		if synced {
			r.leaked = true // the handle is lost, the object stays until the session ends
			return checkIs(err, rt.ErrFutureUsed)
		}
		if err != nil || h.IsNil() {
			return fmt.Errorf("= (%v, %v)", h, err)
		}
		dc.h, dc.ready = h, true
		return nil
	}})
}

func (r *diffRun) opCounterAdd(dc *diffCounter) {
	delta := r.randI64()
	want := dc.m.add(delta)
	f, err := r.c.CounterAddAsync(dc.h, delta)
	desc := fmt.Sprintf("CounterAdd(#%x, %d)", dc.h.ID(), delta)
	r.issue(desc, err, future{desc, func(synced bool) error {
		total, err := f.Wait()
		switch {
		case synced:
			return checkIs(err, rt.ErrFutureUsed)
		case want.status != 0:
			return checkStatus(err, "ml_counter_add", want.status)
		case want.libErr:
			return checkLib(err, "ml_counter_add", -1, want.detail)
		case err != nil || total != want.total:
			return fmt.Errorf("= (%d, %v), want %d", total, err, want.total)
		}
		return nil
	}})
}

func (r *diffRun) opCounterCheck(dc *diffCounter) {
	limit := r.randI32()
	ok := dc.m.check(limit)
	f, err := r.c.CounterCheckAsync(dc.h, limit)
	desc := fmt.Sprintf("CounterCheck(#%x, %d)", dc.h.ID(), limit)
	r.issue(desc, err, future{desc, func(synced bool) error {
		err := f.Wait()
		switch {
		case synced:
			return checkIs(err, rt.ErrFutureUsed)
		case ok:
			return checkNil(err)
		}
		return checkLib(err, "ml_counter_check", -1, "over limit")
	}})
}

func (r *diffRun) opCounterFree(dc *diffCounter) {
	for _, k := range r.cursors {
		if k.parent == dc && k.ready && !k.m.freed {
			r.staleCur = append(r.staleCur, k.h) // the cascade destroys it
		}
	}
	dc.m.free()
	r.staleCtr = append(r.staleCtr, dc.h)
	f, err := r.c.CounterFreeAsync(dc.h)
	desc := fmt.Sprintf("CounterFree(#%x)", dc.h.ID())
	r.issue(desc, err, future{desc, func(synced bool) error {
		err := f.Wait()
		if synced {
			return checkIs(err, rt.ErrFutureUsed)
		}
		return checkNil(err)
	}})
}

func (r *diffRun) opCursorOpen(dc *diffCounter) {
	step := r.randI32()
	k := &diffCursor{m: dc.m.open(step), parent: dc}
	r.cursors = append(r.cursors, k)
	f, err := r.c.CursorOpenAsync(dc.h, step)
	desc := fmt.Sprintf("CursorOpen(#%x, %d) #%d", dc.h.ID(), step, len(r.cursors))
	r.issue(desc, err, future{desc, func(synced bool) error {
		h, err := f.Wait()
		if synced {
			r.leaked = true
			return checkIs(err, rt.ErrFutureUsed)
		}
		if err != nil || h.IsNil() {
			return fmt.Errorf("= (%v, %v)", h, err)
		}
		k.h, k.ready = h, true
		return nil
	}})
}

func (r *diffRun) opCursorNext(k *diffCursor) {
	wantRet, wantVal := k.m.nextValue()
	f, err := r.c.CursorNextAsync(k.h)
	desc := fmt.Sprintf("CursorNext(#%x)", k.h.ID())
	r.issue(desc, err, future{desc, func(synced bool) error {
		ret, v, err := f.Wait()
		if synced {
			return checkIs(err, rt.ErrFutureUsed)
		}
		if err != nil || ret != wantRet || v != wantVal {
			return fmt.Errorf("= (%d, %d, %v), want (%d, %d)", ret, v, err, wantRet, wantVal)
		}
		return nil
	}})
}

func (r *diffRun) opCursorFree(k *diffCursor) {
	ret := k.m.free()
	r.staleCur = append(r.staleCur, k.h)
	f, err := r.c.CursorFreeAsync(k.h)
	desc := fmt.Sprintf("CursorFree(#%x)", k.h.ID())
	r.issue(desc, err, future{desc, func(synced bool) error {
		err := f.Wait()
		switch {
		case synced:
			return checkIs(err, rt.ErrFutureUsed)
		case ret == microlib.ML_OK:
			return checkNil(err)
		}
		return checkLib(err, "ml_cursor_free", int64(ret), "")
	}})
}

// randBytes returns n pseudo-random bytes.
func (r *diffRun) randBytes(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(r.rng.Uint32())
	}
	return b
}

func (r *diffRun) opXor() {
	var n int
	switch x := r.rng.IntN(100); {
	case x < 70:
		n = r.rng.IntN(100)
	case x < 95:
		n = r.rng.IntN(5000)
	case x < 98:
		n = r.rng.IntN(xorMax + 1)
	default:
		n = xorMax + 1 + r.rng.IntN(100) // above the bound of the IDL: refused by the client
	}
	src := r.randBytes(n)
	key := byte(r.rng.Uint32())
	var capacity int
	switch r.rng.IntN(5) {
	case 0:
		capacity = n
	case 1:
		capacity = n + r.rng.IntN(300)
	case 2:
		capacity = max(0, n-1-r.rng.IntN(3)) // below the length: the library fails
	case 3:
		capacity = 0
	default:
		capacity = r.rng.IntN(70000)
	}
	desc := fmt.Sprintf("Xor(n=%d, key=%#x, cap=%d)", n, key, capacity)
	dst := make([]byte, capacity)
	f, err := r.c.XorAsync(src, key, dst)
	if n > xorMax {
		r.immediate(desc, err, rt.ErrInvalidArg)
		return
	}
	want := xorModel(src, key)
	r.issue(desc, err, future{desc, func(synced bool) error {
		got, err := f.Wait()
		switch {
		case synced:
			return checkIs(err, rt.ErrFutureUsed)
		case capacity < n:
			return checkLib(err, "ml_xor", -1, "")
		case err != nil || !slices.Equal(got, want):
			return fmt.Errorf("= (%d bytes, %v), want %d bytes", len(got), err, len(want))
		}
		return nil
	}})
}

func (r *diffRun) opHash() {
	switch r.rng.IntN(10) {
	case 0: // longer than the bound
		s := strings.Repeat("a", 256+r.rng.IntN(10))
		_, err := r.c.HashAsync(s)
		r.immediate(fmt.Sprintf("Hash(%d bytes)", len(s)), err, rt.ErrInvalidArg)
		return
	case 1: // embedded NUL
		b := r.randBytes(1 + r.rng.IntN(20))
		b[r.rng.IntN(len(b))] = 0
		_, err := r.c.HashAsync(string(b))
		r.immediate("Hash(with NUL)", err, rt.ErrInvalidArg)
		return
	}
	b := r.randBytes(r.rng.IntN(256))
	for i := range b {
		if b[i] == 0 {
			b[i] = 1
		}
	}
	s := string(b)
	want := fnv1a(s)
	f, err := r.c.HashAsync(s)
	desc := fmt.Sprintf("Hash(%d bytes)", len(s))
	r.issue(desc, err, future{desc, func(synced bool) error {
		got, err := f.Wait()
		if synced {
			return checkIs(err, rt.ErrFutureUsed)
		}
		if err != nil || got != want {
			return fmt.Errorf("= (%#x, %v), want %#x", got, err, want)
		}
		return nil
	}})
}

func (r *diffRun) opCells(dc *diffCounter) {
	max := int32(r.rng.IntN(310)) - 20
	want := cellsModel(max)
	f, err := r.c.CellsAsync(dc.h, max, nil)
	desc := fmt.Sprintf("Cells(#%x, %d)", dc.h.ID(), max)
	r.issue(desc, err, future{desc, func(synced bool) error {
		got, err := f.Wait()
		if synced {
			return checkIs(err, rt.ErrFutureUsed)
		}
		if err != nil || !slices.Equal(got, want) {
			return fmt.Errorf("= (%d cells, %v), want %d cells", len(got), err, len(want))
		}
		return nil
	}})
}

// opBadHandle uses a handle that must be refused: freed, of the wrong type, of another session, nil.
func (r *diffRun) opBadHandle(cs []*diffCounter, ks []*diffCursor) {
	type target struct {
		desc string
		call func() (wait func() error, issueErr error)
		want error // set when the client refuses at issue
		// server status expected when the call is sent
		status uint32
		fn     string
	}
	var ts []target
	counterCall := func(h microlib.Counter, which int) func() (func() error, error) {
		return func() (func() error, error) {
			switch which {
			case 0:
				f, err := r.c.CounterAddAsync(h, 1)
				return func() error { _, err := f.Wait(); return err }, err
			case 1:
				f, err := r.c.CounterCheckAsync(h, 1)
				return func() error { return f.Wait() }, err
			case 2:
				f, err := r.c.CursorOpenAsync(h, 1)
				return func() error { _, err := f.Wait(); return err }, err
			case 3:
				f, err := r.c.CellsAsync(h, 3, nil)
				return func() error { _, err := f.Wait(); return err }, err
			}
			f, err := r.c.CounterFreeAsync(h)
			return func() error { return f.Wait() }, err
		}
	}
	fns := []string{"ml_counter_add", "ml_counter_check", "ml_cursor_open", "ml_cells", "ml_counter_free"}
	cursorCall := func(h microlib.Cursor, which int) func() (func() error, error) {
		return func() (func() error, error) {
			if which == 0 {
				f, err := r.c.CursorNextAsync(h)
				return func() error { _, _, err := f.Wait(); return err }, err
			}
			f, err := r.c.CursorFreeAsync(h)
			return func() error { return f.Wait() }, err
		}
	}
	cursorFns := []string{"ml_cursor_next", "ml_cursor_free"}

	if len(r.staleCtr) > 0 {
		w := r.rng.IntN(len(fns))
		ts = append(ts, target{"stale counter", counterCall(pick(r.rng, r.staleCtr), w), nil, 3, fns[w]})
	}
	if len(r.staleCur) > 0 {
		w := r.rng.IntN(len(cursorFns))
		ts = append(ts, target{"stale cursor", cursorCall(pick(r.rng, r.staleCur), w), nil, 3, cursorFns[w]})
	}
	if len(ks) > 0 { // a cursor handle where a counter is expected
		k := pick(r.rng, ks)
		w := r.rng.IntN(len(fns))
		ts = append(ts, target{"cursor handle as counter", counterCall(microlib.Counter{Handle: k.h.Handle}, w), nil, 3, fns[w]})
	}
	if len(cs) > 0 { // a counter handle where a cursor is expected
		c := pick(r.rng, cs)
		w := r.rng.IntN(len(cursorFns))
		ts = append(ts, target{"counter handle as cursor", cursorCall(microlib.Cursor{Handle: c.h.Handle}, w), nil, 3, cursorFns[w]})
	}
	w := r.rng.IntN(len(fns))
	ts = append(ts, target{"nil handle", counterCall(microlib.Counter{}, w), rt.ErrInvalidArg, 0, fns[w]})
	if !r.foreign.IsNil() {
		w := r.rng.IntN(len(fns))
		ts = append(ts, target{"handle of another session", counterCall(r.foreign, w), rt.ErrForeignHandle, 0, fns[w]})
	}
	w = r.rng.IntN(len(fns))
	bogus := microlib.Counter{Handle: r.c.Conn().NewHandle(0x4000_0000 | uint32(r.rng.IntN(0xff00)+0x100))} // index beyond the table
	ts = append(ts, target{"never issued", counterCall(bogus, w), nil, 3, fns[w]})

	t := pick(r.rng, ts)
	wait, err := t.call()
	desc := t.desc + " in " + t.fn
	if t.want != nil {
		r.immediate(desc, err, t.want)
		return
	}
	r.issue(desc, err, future{desc, func(synced bool) error {
		if synced {
			return checkIs(wait(), rt.ErrFutureUsed)
		}
		return checkStatus(wait(), t.fn, t.status)
	}})
}

// opMalformed sends a request the stubs cannot produce, through the Conn the stubs use: a body of the
// wrong length, a handle id that is garbage. (BeginRequest refuses an opcode the interface does not have,
// so an unknown function is tested with the raw client.) The server answers a status and executes
// nothing; the session goes on.
func (r *diffRun) opMalformed() {
	conn := r.c.Conn()
	var (
		op     uint16
		body   []byte
		status uint32
		desc   string
	)
	switch r.rng.IntN(2) {
	case 0: // ml_add needs 8 bytes
		op, body, status, desc = 1, r.randBytes(r.rng.IntN(20)), 2, "raw ml_add with a wrong body length"
		for len(body) == 8 {
			body = r.randBytes(r.rng.IntN(20))
		}
	default: // ml_counter_add with a garbage handle id: 16 bytes, id at 0
		body = make([]byte, 16)
		body[0], body[1], body[2], body[3] = 0xEF, 0xBE, 0xAD, 0xDE
		op, status, desc = 3, 3, "raw ml_counter_add with a garbage handle"
	}
	e, err := conn.BeginRequest(op, len(body), 64)
	if err != nil {
		r.fail(desc, fmt.Errorf("BeginRequest: %w", err))
		return
	}
	copy(e.Body(), body)
	call, err := conn.Submit(e)
	r.note("%s op %d, %d bytes", desc, op, len(body))
	if err != nil {
		r.fail(desc, fmt.Errorf("Submit: %w", err))
		return
	}
	id := call.ID()
	r.pending = append(r.pending, future{desc, func(synced bool) error {
		d, err := conn.Wait(call, id)
		if err == nil {
			d.Release()
			return errors.New("a malformed request was answered with success")
		}
		if synced {
			return checkIs(err, rt.ErrFutureUsed)
		}
		var se *rt.StatusError
		if !errors.As(err, &se) || se.Status != status {
			return fmt.Errorf("error %v, want server status %d", err, status)
		}
		return nil
	}})
}

// opSync discards every unanswered call (BINDGEN.md 8.3 item 6): the server still executes them, so the
// model keeps their effects, and the futures must report rt.ErrFutureUsed.
func (r *diffRun) opSync() {
	r.note("Sync with %d pending", len(r.pending))
	if err := r.c.Conn().Sync(); err != nil {
		r.fail("Sync", err)
		return
	}
	for len(r.pending) > 0 {
		r.waitOne(0, true)
	}
}

// finish waits for everything and frees what is left, or leaves it to the session teardown when the
// sequence was told to (teardown) or lost a handle.
func (r *diffRun) finish(teardown bool) {
	r.drain()
	if teardown || r.leaked {
		return
	}
	for _, dc := range r.liveCounters() {
		r.opCounterFree(dc)
	}
	r.drain()
}

// runSequence runs sequence index on c. It returns the log and the first failure.
func runSequence(c *microlib.Client, foreign microlib.Counter, index int, teardown bool) (log []string, leaked bool, err error) {
	r := &diffRun{c: c, foreign: foreign, rng: rand.New(rand.NewPCG(diffSeed, uint64(index)))}
	steps := 8 + r.rng.IntN(40)
	for range steps {
		r.step()
		if r.failure != nil {
			break
		}
	}
	if r.failure == nil {
		r.finish(teardown)
	}
	return r.log, r.leaked, r.failure
}

// runDifferential runs sequences [0, n) (or only the one named by HQQ_E2E_DIFF_SEQ) against the server,
// on sessions of rotating geometry, reopening the session every few sequences and sometimes leaving
// objects to its teardown.
func runDifferential(t *testing.T, s *server, n int, link rt.Options) map[string]int {
	t.Helper()
	only := -1
	if v := os.Getenv(envDiffSeq); v != "" {
		i, err := strconv.Atoi(v)
		if err != nil {
			t.Fatalf("%s=%q: %v", envDiffSeq, v, err)
		}
		only = i
	}
	other := dialUntil(t, s, rt.Options{}, isBusy)
	foreign, err := other.CounterNew(0)
	wantNoErr(t, "CounterNew on the other session", err)

	var c *microlib.Client
	closeClient := func() {
		if c != nil {
			wantNoErr(t, "Close", c.Close())
			c = nil
		}
	}
	defer closeClient()
	stats := map[string]int{}
	for i := range n {
		if only >= 0 && i != only {
			continue
		}
		if c == nil || i%25 == 0 {
			closeClient()
			opts := link
			g := geometries[(i/25)%len(geometries)]
			opts.BufferCount, opts.BufferSize = g[0], g[1]
			c = dialUntil(t, s, opts, isBusy)
		}
		teardown := i%7 == 3 // leave this sequence's objects to the next teardown
		log, leaked, err := runSequence(c, foreign, i, teardown)
		if err != nil {
			t.Fatalf("sequence %d (seed %#x; replay with %s=%d) at geometry %v failed: %v\nlog:\n  %s",
				i, uint64(diffSeed), envDiffSeq, i, geometries[(i/25)%len(geometries)], err, strings.Join(log, "\n  "))
		}
		for _, l := range log {
			for _, k := range opKinds {
				if strings.HasPrefix(l, k) {
					stats[k]++
				}
			}
		}
		if leaked || teardown {
			closeClient() // the teardown of the session destroys what the sequence left
		}
	}
	// The other session's handle is still good.
	if total, err := other.CounterAdd(foreign, 1); err != nil || total != 1 {
		t.Fatalf("the bystander session's counter = (%d, %v), want (1, nil)", total, err)
	}
	return stats
}

// opKinds are the prefixes of the log lines of the operation classes; TestDifferential requires every
// class to have been exercised.
var opKinds = []string{
	"Add(", "CounterNew(", "CounterAdd(", "CounterCheck(", "CounterFree(", "CursorOpen(", "CursorNext(",
	"CursorFree(", "Xor(", "Hash(", "Cells(", "stale counter", "stale cursor", "cursor handle as counter",
	"counter handle as cursor", "nil handle", "handle of another session", "never issued",
	"raw ml_add", "raw ml_counter_add", "Sync",
}

// TestDifferential: 10,000 random sequences against the real server, compared with the model.
func TestDifferential(t *testing.T) {
	s := startServer(t, serverOpts{noTrace: true})
	stats := runDifferential(t, s, diffSequences, rt.Options{})
	if os.Getenv(envDiffSeq) != "" {
		return
	}
	t.Logf("operations: %v", stats)
	s.requireNoSessionSegments()
	for _, k := range opKinds {
		if stats[k] < 100 {
			t.Errorf("operation class %q ran %d times in %d sequences, want at least 100: the generator no longer covers it", k, stats[k], diffSequences)
		}
	}
}

// TestDifferentialTeardownBalance: the same generator with the library trace on. Everything the model
// created and the sequences freed or left to teardown must have been destroyed exactly once when the
// server stops: the trace has as many destroy lines as create lines for each type.
func TestDifferentialTeardownBalance(t *testing.T) {
	s := startServer(t, serverOpts{})
	s.clientsMayFail = true // the server is stopped below, with the bystander session still open
	runDifferential(t, s, 400, rt.Options{})
	closeAll := s.proc.Stop
	ctx, cancel := contextWithFail()
	defer cancel()
	wantNoErr(t, "Stop", closeAll(ctx))
	counts := map[string]int{}
	for _, l := range s.traceLines() {
		name, _, _ := strings.Cut(l, " ")
		counts[name]++
	}
	if counts["ml_counter_new"] == 0 || counts["ml_cursor_open"] == 0 {
		t.Fatalf("the sequences created nothing: %v", counts)
	}
	if counts["ml_counter_new"] != counts["ml_counter_free"] || counts["ml_cursor_open"] != counts["ml_cursor_free"] {
		t.Fatalf("library objects created and destroyed differ: counter new %d free %d, cursor open %d free %d",
			counts["ml_counter_new"], counts["ml_counter_free"], counts["ml_cursor_open"], counts["ml_cursor_free"])
	}
}
