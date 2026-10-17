package gen

import (
	"bytes"
	"math"
	"path/filepath"
	"strings"
	"testing"

	"gosuda.org/hqq/bindgen/idl"
)

// ---- request bodies -----------------------------------------------------------------------------------

// enc builds a request body with the alignment rules of 4.5.
type enc struct{ b []byte }

func (e *enc) align(n int) *enc {
	for len(e.b)%n != 0 {
		e.b = append(e.b, 0)
	}
	return e
}
func (e *enc) raw(b ...byte) *enc { e.b = append(e.b, b...); return e }
func (e *enc) u8(v uint8) *enc    { return e.raw(v) }
func (e *enc) u16(v uint16) *enc  { e.align(2); return e.raw(byte(v), byte(v>>8)) }
func (e *enc) u32(v uint32) *enc {
	e.align(4)
	return e.raw(byte(v), byte(v>>8), byte(v>>16), byte(v>>24))
}
func (e *enc) i32(v int32) *enc { return e.u32(uint32(v)) }
func (e *enc) u64(v uint64) *enc {
	e.align(8)
	return e.raw(byte(v), byte(v>>8), byte(v>>16), byte(v>>24), byte(v>>32), byte(v>>40), byte(v>>48), byte(v>>56))
}
func (e *enc) i64(v int64) *enc   { return e.u64(uint64(v)) }
func (e *enc) f32(v float32) *enc { return e.u32(math.Float32bits(v)) }
func (e *enc) str(s string) *enc  { e.u32(uint32(len(s))); return e.raw([]byte(s)...).raw(0) }
func (e *enc) bytes(b []byte) *enc {
	e.u32(uint32(len(b)))
	return e.raw(b...)
}
func (e *enc) nullStr() *enc { return e.u32(0xFFFFFFFF) }
func (e *enc) padTo(n int) *enc {
	for len(e.b) < n {
		e.b = append(e.b, 0)
	}
	return e
}
func (e *enc) build() []byte { return e.b }

func le(vals ...uint32) []byte {
	var b []byte
	for _, v := range vals {
		b = append(b, byte(v), byte(v>>8), byte(v>>16), byte(v>>24))
	}
	return b
}

func le64(v uint64) []byte { return append(le(uint32(v)), le(uint32(v>>32))...) }

func cat(parts ...[]byte) []byte { return bytes.Join(parts, nil) }

// ---- a driver run -------------------------------------------------------------------------------------

type edgeRun struct {
	t       *testing.T
	exe     string
	spec    *idl.Spec
	frames  [][]byte
	id      uint32
	res     []exchangeResult
	trailer string
}

func newEdgeRun(t *testing.T, exe string, spec *idl.Spec) *edgeRun {
	r := &edgeRun{t: t, exe: exe, spec: spec}
	r.frames = append(r.frames, helloFrame(spec.Digest(), 16384, 64))
	return r
}

// call queues a request for fn and returns its index.
func (r *edgeRun) call(fn string, body []byte) int {
	f := r.spec.Func(fn)
	if f == nil || f.Opcode == 0 {
		r.t.Fatalf("no wire function %s", fn)
	}
	r.id++
	r.frames = append(r.frames, requestFrame(uint16(f.Opcode), r.id, body))
	return len(r.frames) - 1
}

func (r *edgeRun) run() {
	r.t.Helper()
	r.res, r.trailer = (&session{t: r.t, exe: r.exe}).run(r.frames...)
	if r.res[0].status != 0 || len(r.res[0].out) < 20 || r.res[0].out[0] != 5 || le32(r.res[0].out[12:]) != 0 {
		r.t.Fatalf("handshake failed: %x", r.res[0].out)
	}
}

// resp is the single response frame of call i.
func (r *edgeRun) resp(i int) frame {
	r.t.Helper()
	if r.res[i].status != 0 {
		r.t.Fatalf("call %d: exchange status %d", i, r.res[i].status)
	}
	fs := parseFrames(r.t, r.res[i].out)
	if len(fs) != 1 {
		r.t.Fatalf("call %d: %d frames, want 1: %x", i, len(fs), r.res[i].out)
	}
	return fs[0]
}

// ok checks a success response (status 0, no flags) and returns its body.
func (r *edgeRun) ok(i int) []byte {
	r.t.Helper()
	f := r.resp(i)
	if f.aux != 0 || f.flags != 0 {
		r.t.Fatalf("call %d: status %d flags %#x body %x, want a plain success", i, f.aux, f.flags, f.body)
	}
	return f.body
}

// fail checks a library failure (status 0, F_FAIL, with F_DETAIL iff detail) and returns its body.
func (r *edgeRun) fail(i int, detail bool) []byte {
	r.t.Helper()
	f := r.resp(i)
	want := byte(2)
	if detail {
		want = 3
	}
	if f.aux != 0 || f.flags != want {
		r.t.Fatalf("call %d: status %d flags %#x body %x, want a failure with flags %#x", i, f.aux, f.flags, f.body, want)
	}
	return f.body
}

// status checks a call-level status and returns it.
func (r *edgeRun) status(i int, want uint32) {
	r.t.Helper()
	f := r.resp(i)
	if f.aux != want || f.flags != 0 {
		r.t.Fatalf("call %d: status %d flags %#x body %x, want status %d", i, f.aux, f.flags, f.body, want)
	}
}

func eq(t *testing.T, what string, got, want []byte) {
	t.Helper()
	if !bytes.Equal(got, want) {
		t.Errorf("%s:\n  got  %x\n  want %x", what, got, want)
	}
}

func eqU32(t *testing.T, what string, body []byte, off int, want uint32) {
	t.Helper()
	if len(body) < off+4 {
		t.Errorf("%s: body %x too short for offset %d", what, body, off)
		return
	}
	if got := le32(body[off:]); got != want {
		t.Errorf("%s: got %#x, want %#x (body %x)", what, got, want, body)
	}
}

// strBody is the encoding of a str value at offset 0.
func strBody(s string) []byte { return (&enc{}).str(s).build() }

// edgeCase is one scenario of TestCEdge. It queues calls, runs them, and checks the responses.
type edgeCase struct {
	name string
	run  func(t *testing.T, r *edgeRun)
	end  string // the trailer of the session
}

const (
	stBadRequest = 2
	stBadHandle  = 3
	stLimit      = 4
	stDenied     = 5
	stInternal   = 6
)

func TestCEdge(t *testing.T) {
	spec := parseSpec(t, "bindgen/gen/testdata/c/edge.idl")
	files := genC(t, spec, "edge.idl")
	dir := t.TempDir()
	writeFiles(t, dir, files)
	root := repoRoot(t)
	exe := buildProgram(t, dir,
		[]string{cTestdata(t), filepath.Join(root, "c", "src")},
		[]string{"IFACE=edge_iface"},
		[]string{filepath.Join(dir, "edge_server.c"), cTestdata(t, "edge_hooks.c"), cTestdata(t, "edge.c")},
		[]string{cTestdata(t, "exchange_driver.c")})

	for _, tc := range edgeCases {
		t.Run(tc.name, func(t *testing.T) {
			r := newEdgeRun(t, exe, spec)
			tc.run(t, r)
			want := tc.end
			if want == "" {
				want = "end live=0 failures=0 tombstones=0"
			}
			if r.trailer != want {
				t.Errorf("session end %q, want %q", r.trailer, want)
			}
		})
	}
}

var edgeCases = []edgeCase{
	{"no parameters and void returns", func(t *testing.T, r *edgeRun) {
		r.call("edge_reset", nil)
		ping := []int{r.call("edge_ping", nil), r.call("edge_ping", nil), r.call("edge_ping", nil)}
		pings := r.call("edge_pings", nil)
		r.run()
		for _, i := range ping {
			if body := r.ok(i); len(body) != 0 {
				t.Errorf("ping answered %x, want an empty body", body)
			}
		}
		if f := r.resp(ping[0]); f.total != 0 || f.op != uint16(r.spec.Func("edge_ping").Opcode) {
			t.Errorf("ping response header %+v", f)
		}
		eq(t, "pings", r.ok(pings), le64(3))
	}, ""},

	{"scalars of every width", func(t *testing.T, r *edgeRun) {
		// a i8 @0, b u16 @2, c f32 @4, d bool @8, e u64 @16: 24 bytes
		body := (&enc{}).u8(0xFD).u16(500).f32(1.5).u8(1).u64(7).build()
		i := r.call("edge_mix", body)
		bad := r.call("edge_mix", (&enc{}).u8(0).u16(0).f32(0).u8(2).u64(0).build())
		flip := r.call("edge_flip", []byte{1})
		flip0 := r.call("edge_flip", []byte{0})
		big := r.call("edge_big", le64(1<<40))
		huge := r.call("edge_big", le64(math.MaxUint64))
		key := r.call("edge_key", []byte{1, 2, 3, 4})
		r.run()
		got := r.ok(i)
		if math.Float64frombits(uint64(le32(got))|uint64(le32(got[4:]))<<32) != -3+500+1.5+1000+7 {
			t.Errorf("edge_mix = %x", got)
		}
		r.status(bad, stBadRequest)
		eq(t, "flip(true)", r.ok(flip), []byte{0})
		eq(t, "flip(false)", r.ok(flip0), []byte{1})
		eq(t, "big", r.ok(big), le64(1<<40+1))
		eq(t, "big wraps in the library", r.ok(huge), le64(0))
		eq(t, "key", r.ok(key), le(0x04030201))
	}, ""},

	{"arrays with 8-byte elements, empty arrays and bool arrays", func(t *testing.T, r *edgeRun) {
		// i64[]: count, 4 bytes of padding, elements at 8
		three := r.call("edge_sum", (&enc{}).u32(3).i64(10).i64(20).i64(-5).build())
		empty := r.call("edge_sum", le(0)) // no padding after a count of 0 (4.5)
		trueOK := r.call("edge_count_true", (&enc{}).u32(4).raw(1, 0, 1, 1).build())
		trueBad := r.call("edge_count_true", (&enc{}).u32(2).raw(1, 2).build())
		r.run()
		eq(t, "sum of three", r.ok(three), le64(25))
		eq(t, "sum of none", r.ok(empty), le64(0))
		eq(t, "count_true", r.ok(trueOK), le(3))
		r.status(trueBad, stBadRequest)
	}, ""},

	{"nullable str is copied and NULL is passed through", func(t *testing.T, r *edgeRun) {
		null := r.call("edge_note", (&enc{}).nullStr().build())
		five := r.call("edge_note", strBody("hello"))
		empty := r.call("edge_note", strBody(""))
		long := r.call("edge_note", strBody(strings.Repeat("x", 33)))
		embedded := r.call("edge_note", append(append(le(3), 'a', 0, 'b'), 0))
		r.run()
		eq(t, "NULL", r.ok(null), le(0xFFFFFFFF))
		eq(t, "hello", r.ok(five), le(5))
		eq(t, "empty", r.ok(empty), le(0))
		r.status(long, stBadRequest)
		r.status(embedded, stBadRequest)
	}, ""},

	{"handles, parents, cascade and ids", func(t *testing.T, r *edgeRun) {
		open := r.call("edge_dir_open", le(1))
		r.run()
		d := le32(r.ok(open))
		if d != 0x00010001 {
			t.Fatalf("first handle %#x, want 0x00010001", d)
		}
	}, "end live=1 failures=0 tombstones=0"},

	{"cascade destroys children before the parent and consumes every entry", func(t *testing.T, r *edgeRun) {
		r.call("edge_reset", nil)
		open := r.call("edge_dir_open", le(1))
		file := r.call("edge_file_open", (&enc{}).u32(0x00010001).padTo(8).str("f1").build())
		file2 := r.call("edge_file_open", (&enc{}).u32(0x00010001).padTo(8).str("f2").build())
		tag := r.call("edge_tag_new", nil)
		closeDir := r.call("edge_dir_close", le(0x00010001))
		log := r.call("edge_log", nil)
		staleFile := r.call("edge_file_close", le(0x00010002))
		staleDir := r.call("edge_dir_close", le(0x00010001))
		tagFree := r.call("edge_tag_free", le(0x00010004))
		log2 := r.call("edge_log", nil)
		r.run()
		eq(t, "dir", r.ok(open), le(0x00010001))
		eq(t, "file 1", r.ok(file), le(0x00010002))
		eq(t, "file 2", r.ok(file2), le(0x00010003))
		eq(t, "tag", r.ok(tag), le(0x00010004))
		eq(t, "close", r.ok(closeDir), le(0))
		// newest child first: F F, then D
		eq(t, "log after close", r.ok(log), strBody("FFD"))
		r.status(staleFile, stBadHandle)
		r.status(staleDir, stBadHandle)
		if body := r.ok(tagFree); len(body) != 0 {
			t.Errorf("void destroy answered %x", body)
		}
		eq(t, "log after the tag", r.ok(log2), strBody("FFDT"))
	}, ""},

	{"retain_on_fail keeps the entry, its children are gone", func(t *testing.T, r *edgeRun) {
		r.call("edge_reset", nil)
		r.call("edge_dir_open", le(1))
		r.call("edge_file_open", (&enc{}).u32(0x00010001).padTo(8).str("f1").build())
		r.call("edge_dir_lock", (&enc{}).u32(0x00010001).u8(1).build())
		busy := r.call("edge_dir_close", le(0x00010001))
		peek := r.call("edge_dir_peek", le(0x00010001))
		file := r.call("edge_file_close", le(0x00010002))
		r.call("edge_dir_lock", (&enc{}).u32(0x00010001).u8(0).build())
		again := r.call("edge_dir_close", le(0x00010001))
		stale := r.call("edge_dir_peek", le(0x00010001))
		log := r.call("edge_log", nil)
		r.run()
		eq(t, "busy close", r.fail(busy, false), le(5))
		eq(t, "the directory is still live", r.ok(peek), le(1))
		r.status(file, stBadHandle) // the cascade ran before the failed destroy and is not rolled back
		eq(t, "second close", r.ok(again), le(0))
		r.status(stale, stBadHandle)
		eq(t, "log", r.ok(log), strBody("FdD"))
	}, ""},

	{"handle validation", func(t *testing.T, r *edgeRun) {
		r.call("edge_dir_open", le(4)) // 0x00010001
		r.call("edge_tag_new", nil)    // 0x00010002
		nullable := r.call("edge_dir_peek", le(0))
		nonNullable := r.call("edge_dir_lock", (&enc{}).u32(0).u8(1).build())
		wrongType := r.call("edge_dir_lock", (&enc{}).u32(0x00010002).u8(1).build())
		unknown := r.call("edge_dir_peek", le(0x00010009))
		stale := r.call("edge_dir_peek", le(0x00020001))
		r.run()
		eq(t, "peek of the null handle", r.ok(nullable), le(0xFFFFFFFF))
		r.status(nonNullable, stBadHandle)
		r.status(wrongType, stBadHandle)
		r.status(unknown, stBadHandle)
		r.status(stale, stBadHandle)
	}, "end live=2 failures=0 tombstones=0"},

	{"null convention: failed call sends id 0", func(t *testing.T, r *edgeRun) {
		neg := r.call("edge_dir_open", le(0xFFFFFFFF)) // id -1: NULL
		open := r.call("edge_dir_open", le(1))
		noName := r.call("edge_file_open", (&enc{}).u32(0x00010001).padTo(8).str("").build())
		r.run()
		eq(t, "failed open", r.fail(neg, false), le(0))
		eq(t, "open", r.ok(open), le(0x00010001))
		eq(t, "failed file open", r.fail(noName, false), le(0))
	}, "end live=1 failures=0 tombstones=0"},

	{"null convention with a str return and detail-free failure", func(t *testing.T, r *edgeRun) {
		r.call("edge_dir_open", le(5))
		r.call("edge_dir_open", le(7))
		r.call("edge_dir_open", le(1000000))
		name := r.call("edge_dir_name", le(0x00010001))
		null := r.call("edge_dir_name", le(0x00010002))
		long := r.call("edge_dir_name", le(0x00010003))
		r.run()
		eq(t, "name", r.ok(name), strBody("dir5"))
		if body := r.fail(null, false); len(body) != 0 {
			t.Errorf("NULL name failure body %x, want empty (G2 is empty)", body)
		}
		r.status(long, stInternal) // "dir1000000" is longer than str<=8: the library contradicts the IDL
	}, "end live=3 failures=0 tombstones=0"},

	{"predicate convention keeps ret on failure", func(t *testing.T, r *edgeRun) {
		ok := r.call("edge_div", cat(le(10), le(2)))
		neg := r.call("edge_div", cat(le(0xFFFFFFF6), le(3))) // -10 / 3
		bad := r.call("edge_div", cat(le(1), le(0)))
		r.run()
		eq(t, "10 / 2", r.ok(ok), le(5))
		eq(t, "-10 / 3", r.ok(neg), le(0xFFFFFFFD))
		eq(t, "1 / 0", r.fail(bad, false), le(0x80000000))
	}, ""},

	{"two out handles: success registers both, failure destroys both", func(t *testing.T, r *edgeRun) {
		r.call("edge_reset", nil)
		r.call("edge_dir_open", le(1)) // 0x00010001
		r.call("edge_dir_open", le(2)) // 0x00010002
		okPair := r.call("edge_open_pair", le(0x00010001))
		badPair := r.call("edge_open_pair", le(0x00010002))
		log := r.call("edge_log", nil)
		r.run()
		body := r.ok(okPair)
		eq(t, "ret", body[:4], le(0))
		eqU32(t, "file id", body, 4, 0x00010003)
		eqU32(t, "tag id", body, 8, 0x00010004)
		eq(t, "failed pair: ret 3, both ids 0", r.fail(badPair, false), le(3, 0, 0))
		eq(t, "the failed objects were destroyed, file first", r.ok(log), strBody("FT"))
	}, "end live=4 failures=0 tombstones=0"},

	{"out scalars, a @c struct, inout and a fixed array out", func(t *testing.T, r *edgeRun) {
		r.call("edge_dir_open", le(5))
		r.call("edge_dir_open", le(3))
		good := r.call("edge_stat", cat(le(0x00010001), le(41)))
		bad := r.call("edge_stat", cat(le(0x00010002), le(41)))
		r.run()
		// G2: ret i32 @0, n i32 @4, p Pt @8 (x @8, ok @12, y @16), k i32 @24, tag @28
		want := cat(le(0), le(10), le(5), []byte{1, 0, 0, 0}, le64(uint64(0xFFFFFFFFFFFFFFFB)), le(42), []byte("abcd"))
		eq(t, "stat", r.ok(good), want)
		// failure: G2 is zero except ret, then D at G2pad = 32
		body := r.fail(bad, true)
		eq(t, "failed stat G2", body[:32], cat(le(9), make([]byte, 28)))
		eq(t, "failed stat detail", body[32:], strBody("bad stat"))
	}, "end live=2 failures=0 tombstones=0"},

	{"borrowed results with @free", func(t *testing.T, r *edgeRun) {
		r.call("edge_reset", nil)
		for _, id := range []uint32{1, 4, 5} {
			r.call("edge_dir_open", le(id))
		}
		blob := r.call("edge_blob", le(0x00010001))
		blobFail := r.call("edge_blob", le(0x00010002))
		blobLong := r.call("edge_blob", le(0x00010003))
		label := r.call("edge_label", le(0x00010001))
		labelFail := r.call("edge_label", le(0x00010002))
		frees := r.call("edge_frees", nil)
		r.run()
		// G2 ret @0, pad to 8, bytes at 8
		eq(t, "blob", r.ok(blob), cat(le(0, 0), le(6), []byte("blob-x")))
		eq(t, "failed blob", r.fail(blobFail, false), le(1))
		r.status(blobLong, stInternal)
		eq(t, "label", r.ok(label), cat(le(0, 0), strBody("label-1")))
		eq(t, "failed label", r.fail(labelFail, false), le(1))
		// every pointer was freed: blob, failed blob, long blob, label, failed label
		eq(t, "frees", r.ok(frees), le(5))
	}, "end live=3 failures=0 tombstones=0"},

	{"caller-supplied buffer written in place after G2 and padding", func(t *testing.T, r *edgeRun) {
		for _, id := range []uint32{1, 6, 7} {
			r.call("edge_dir_open", le(id))
		}
		read := r.call("edge_read", cat(le(0x00010001), le(64), nil))
		small := r.call("edge_read", cat(le(0x00010001), le(2)))
		fail := r.call("edge_read", cat(le(0x00010002), le(64)))
		over := r.call("edge_read", cat(le(0x00010003), le(8)))
		limit := r.call("edge_read", cat(le(0x00010001), le(65)))
		r.run()
		// G2 = ret i32 (4), pad to 8, then count and data
		eq(t, "read", r.ok(read), cat(le(0, 0), le(4), []byte{0x10, 0x11, 0x12, 0x13}))
		eq(t, "short read", r.ok(small), cat(le(0, 0), le(2), []byte{0x10, 0x11}))
		// failure: G2 (4 bytes), pad to 8 because there is a detail, D
		eq(t, "failed read", r.fail(fail, true), cat(le(6, 0), strBody("bad read")))
		r.status(over, stInternal) // produced length above the capacity
		r.status(limit, stLimit)   // cap above its <=64 bound
	}, "end live=3 failures=0 tombstones=0"},

	{"in-place array of 8-byte elements starts at 16", func(t *testing.T, r *edgeRun) {
		four := r.call("edge_iota", cat(le64(100), le(4)))
		none := r.call("edge_iota", cat(le64(100), le(0)))
		many := r.call("edge_iota", cat(le64(0), le(8)))
		limit := r.call("edge_iota", cat(le64(0), le(9)))
		r.run()
		// ret u8 @0, pad to 8, count @8, 4 bytes of padding, elements @16
		eq(t, "iota 4", r.ok(four), cat([]byte{4, 0, 0, 0, 0, 0, 0, 0}, le(4, 0), le64(100), le64(101), le64(102), le64(103)))
		// a count of 0 has no element padding
		eq(t, "iota 0", r.ok(none), cat([]byte{0, 0, 0, 0, 0, 0, 0, 0}, le(0)))
		got := r.ok(many)
		if len(got) != 16+5*8 || got[0] != 5 || le32(got[8:]) != 5 {
			t.Errorf("iota 8: %x", got)
		}
		r.status(limit, stLimit)
	}, ""},

	{"two out buffers: the first in place, the second through scratch", func(t *testing.T, r *edgeRun) {
		two := r.call("edge_two", cat(le(5), le(5)))
		short := r.call("edge_two", cat(le(1), le(0)))
		r.run()
		// a: count 3 + 3 bytes + 1 byte of padding; b: count 2 + 2 bytes
		eq(t, "two", r.ok(two), cat(le(3), []byte{0xA0, 0xA1, 0xA2, 0}, le(2), []byte{0xB0, 0xB1}))
		eq(t, "short", r.ok(short), cat(le(1), []byte{0xA0, 0, 0, 0}, le(0)))
	}, ""},

	{"interceptor: veto, application status, reserved status, substitution and clamping", func(t *testing.T, r *edgeRun) {
		r.call("edge_reset", nil)
		r.call("edge_dir_open", le(1))
		call := func(name string, mode int32) int {
			return r.call("edge_guard", cat(le(0x00010001), le(uint32(mode)), strBody(name)))
		}
		good := call("good", 7)
		evil := call("evil", 7)
		clamp := call("good", 60)
		veto := call("good", -1)
		app := call("good", 100)
		reserved := call("good", 200)
		log := r.call("edge_log", nil)
		r.run()
		const want = 100000 + 4*1000 + 7*10 + 1
		eq(t, "good", r.ok(good), le(want))
		eq(t, "evil is replaced by good", r.ok(evil), le(want))
		eq(t, "mode is clamped", r.ok(clamp), le(100000+4*1000+50*10+1))
		r.status(veto, stDenied)
		r.status(app, 0x0123)
		r.status(reserved, stInternal)
		// the library ran for three calls only
		eq(t, "log", r.ok(log), strBody("ggg"))
	}, "end live=1 failures=0 tombstones=0"},

	{"shim with a writer: success and the failures of 4.6.6", func(t *testing.T, r *edgeRun) {
		two := r.call("edge_cells", le(2))
		none := r.call("edge_cells", le(0))
		app := r.call("edge_cells", le(0xFFFFFFFF)) // -1
		reserved := r.call("edge_cells", le(0xFFFFFFFE))
		over := r.call("edge_cells", le(4))      // one cell above the bound: array_end fails
		ignored := r.call("edge_cells", le(5))   // the shim ignores the writer failure and returns OK
		overflow := r.call("edge_cells", le(60)) // more bytes than max_out
		after := r.call("edge_ping", nil)
		r.run()
		// count 2; Int(0): tag @4, i64 @8; Text("ab"): tag @16, len @20, "ab" NUL (27 bytes)
		eq(t, "two cells", r.ok(two), cat(le(2), le(1), le64(0), le(2), le(2), []byte{'a', 'b', 0}))
		eq(t, "no cells", r.ok(none), le(0))
		r.status(app, 0x0123)
		r.status(reserved, stInternal)
		r.status(over, stInternal)
		r.status(ignored, stInternal)
		r.status(overflow, stInternal)
		r.ok(after)
	}, ""},

	{"shim returning a fixed struct with dirty padding", func(t *testing.T, r *edgeRun) {
		pt := r.call("edge_pt_shim", le(5))
		neg := r.call("edge_pt_shim", le(0xFFFFFFFE)) // -2
		app := r.call("edge_pt_shim", le(0xFFFFFFFF)) // -1: status 0x0100
		r.run()
		// x @0, ok @4, three bytes of padding that the library filled with 0xAA, y @8
		eq(t, "pt", r.ok(pt), cat(le(5), []byte{1, 0, 0, 0}, le64(5000)))
		eq(t, "negative id", r.ok(neg), cat(le(0xFFFFFFFE), []byte{0, 0, 0, 0}, le64(math.MaxUint64-1999)))
		r.status(app, 0x0100)
	}, ""},

	{"every union case writer", func(t *testing.T, r *edgeRun) {
		var idx [16]int
		for sel := 1; sel <= 15; sel++ {
			idx[sel] = r.call("edge_cell", le(uint32(sel)))
		}
		r.run()
		f2 := func(v float64) []byte { return le64(math.Float64bits(v)) }
		want := map[int][]byte{
			1:  cat(le(1, 0), le64(uint64(0xFFFFFFFFFFFFFFF7))),                                        // Int: tag, pad, i64 @8
			2:  cat(le(2, 3), []byte("abc\x00")),                                                       // Text: tag, len, bytes, NUL
			3:  le(3),                                                                                  // Nul: the tag only
			4:  cat(le(4, 0), le(1), []byte{1, 0, 0, 0}, le64(2)),                                      // At: Pt @8, padding zero
			5:  cat(le(5, 3), le(1, 2, 3)),                                                             // Arr: tag, count, i32 elements @8
			6:  cat(le(6, 2), []byte("xy")),                                                            // Blob
			7:  cat(le(7, 0), f2(2.5)),                                                                 // Real: f64 @8
			8:  cat(le(8), []byte{9}),                                                                  // Byte: u8 @4
			9:  cat(le(9), []byte{1}),                                                                  // Flag
			10: cat(le(10, 2), le(1), []byte{1, 0, 0, 0}, le64(2), le(3), []byte{0, 0, 0, 0}, le64(4)), // Words: Pt elements @8
			11: cat(le(11), []byte("wxyz")),                                                            // Quad: u8[4] @4
			12: cat(le(12), le(0xFFFFFFFE)),                                                            // Small
			13: cat(le(13, 0), le64(77)),                                                               // Wide: u64 @8
		}
		for sel := 1; sel <= 13; sel++ {
			eq(t, "case "+itoa(sel), r.ok(idx[sel]), want[sel])
		}
		r.status(idx[14], stInternal) // str<=4 given 5 bytes
		r.status(idx[15], stInternal) // Pt[]<=2 given 3 elements
	}, ""},

	{"unknown opcode", func(t *testing.T, r *edgeRun) {
		r.id++
		r.frames = append(r.frames, requestFrame(60000, r.id, nil))
		i := len(r.frames) - 1
		r.run()
		r.status(i, 1)
	}, ""},

	{"trailing bytes and short bodies", func(t *testing.T, r *edgeRun) {
		long := r.call("edge_flip", []byte{1, 0})
		short := r.call("edge_big", le(1))
		r.run()
		r.status(long, stBadRequest)
		r.status(short, stBadRequest)
	}, ""},
}

// ---- serialize(none) ----------------------------------------------------------------------------------

// TestCSerializeNone generates the microlib glue for @serialize(none): it must not mention the execution lock, the
// interface must say none, and the session of 4.14 must still answer every vector.
func TestCSerializeNone(t *testing.T) {
	src, err := readFile(t, "bindgen/testdata/microlib/microlib.idl")
	if err != nil {
		t.Fatal(err)
	}
	text := strings.Replace(src, "@serialize(session)", "@serialize(none)", 1)
	if text == src {
		t.Fatal("microlib.idl has no @serialize(session)")
	}
	spec := parseText(t, text)
	if spec.Serialize != idl.SerializeNone {
		t.Fatal("the variant is not serialize(none)")
	}
	files := genC(t, spec, "microlib.idl")
	c := string(files["microlib_server.c"])
	if strings.Contains(c, "hqqrpc_exec_") {
		t.Error("serialize(none) glue calls the execution lock")
	}
	if !strings.Contains(c, "    HQQRPC_SERIALIZE_NONE,\n};") {
		t.Error("the interface does not say HQQRPC_SERIALIZE_NONE")
	}
	if strings.Contains(string(genC(t, parseSpec(t, "bindgen/testdata/microlib/microlib.idl"), "microlib.idl")["microlib_server.c"]), "HQQRPC_SERIALIZE_NONE") {
		t.Error("serialize(session) glue says none")
	}

	// the digest does not cover @serialize, so the vectors hold unchanged
	exe := buildMicrolibSpec(t, spec, "microlib.idl", files)
	vec := loadVectors(t)["microlib"]
	var reqs, want [][]byte
	for _, f := range vec.Frames {
		if f.Name == "HELLO_REPLY_DIGEST" {
			continue
		}
		if f.Dir == "c2s" {
			reqs = append(reqs, f.bytes(t))
		} else {
			want = append(want, f.bytes(t))
		}
	}
	res, trailer := (&session{t: t, exe: exe}).run(reqs...)
	for i := range res {
		if !bytes.Equal(res[i].out, want[i]) {
			t.Errorf("request %d: got %x, want %x", i, res[i].out, want[i])
		}
	}
	if trailer != "end live=0 failures=0 tombstones=0" {
		t.Errorf("trailer %q", trailer)
	}
}
