package rt

import (
	"bytes"
	"errors"
	"testing"
	"time"

	hqq "gosuda.org/hqq"
)

// TestVectorHeaders checks the header codec against every golden frame: the
// header parses to the fields the document gives and encodes back to the same
// 16 bytes, and total agrees with the frame length.
func TestVectorHeaders(t *testing.T) {
	vecs, file := loadVectors(t)
	for _, set := range [][]vectorFrame{file.Microlib.Frames, file.Chunking.Frames, file.Wrap.Frames} {
		for _, f := range set {
			b := vecs.get(t, f.Name)
			h := loadHeader(b)
			var re [HeaderSize]byte
			putHeader(re[:], h)
			if !bytes.Equal(re[:], b[:HeaderSize]) {
				t.Errorf("%s: header re-encodes to %x, want %x", f.Name, re[:], b[:HeaderSize])
			}
			if h.id != f.ID {
				t.Errorf("%s: id %d, want %d", f.Name, h.id, f.ID)
			}
			if h.kind == kindRequest || h.kind == kindResponse || h.kind == kindHello || h.kind == kindHelloReply {
				chunk := file.Microlib.Geometry.BufferSize - HeaderSize
				if set[0].Name == file.Chunking.Frames[0].Name {
					chunk = file.Chunking.BufferSize - HeaderSize
				}
				if got, want := len(b)-HeaderSize, min(int(h.total), chunk); got != want {
					t.Errorf("%s: body %d bytes, want min(total %d, chunk) = %d", f.Name, got, h.total, want)
				}
			}
		}
	}
}

type sessionStep struct {
	req  string
	resp []string
}

// TestVectorSession replays the vector session of BINDGEN.md 4.14 and 10.1:
// the request frames of the stubs must equal R1..R13 byte for byte, and the
// scripted responses must decode to the documented results.
func TestVectorSession(t *testing.T) {
	vecs, file := loadVectors(t)
	geo := file.Microlib.Geometry
	p := newPair(t, geo.BufferCount, geo.BufferSize)
	c := newTestConn(t, p, connOpts{maxInFlight: 64, maxMessage: 16 << 20})
	cl := &stubClient{c}

	steps := []sessionStep{
		{"R1_REQ", []string{"R1_RESP"}}, {"R2_REQ", []string{"R2_RESP"}}, {"R3_REQ", []string{"R3_RESP"}},
		{"R4_REQ", []string{"R4_RESP"}}, {"R5_REQ", []string{"R5_RESP"}}, {"R6_REQ", []string{"R6_RESP"}},
		{"R7_REQ", []string{"R7_RESP"}}, {"R8_REQ", []string{"R8_RESP"}}, {"R9_REQ", []string{"R9_RESP"}},
		{"R10_REQ", []string{"R10_RESP"}}, {"R11_REQ", []string{"R11_RESP"}}, {"R12_REQ", []string{"R12_RESP"}},
		{"R13_REQ", []string{"R13_RESP"}},
	}
	mismatch := make(chan string, 64)
	scriptDone := make(chan struct{})
	go func() {
		defer close(scriptDone)
		read := func() []byte {
			r, err := p.server.ReserveRead()
			if err != nil {
				mismatch <- "server read: " + err.Error()
				return nil
			}
			defer r.Release()
			return bytes.Clone(r.Buffer())
		}
		write := func(b []byte) {
			w, err := p.server.ReserveWrite()
			if err != nil {
				mismatch <- "server write: " + err.Error()
				return
			}
			copy(w.Buffer(), b)
			if err := w.Commit(len(b)); err != nil {
				mismatch <- "server commit: " + err.Error()
			}
		}
		for _, s := range steps {
			got := read()
			if want := vecs.get(t, s.req); !bytes.Equal(got, want) {
				mismatch <- s.req + ": client sent " + hexOf(got) + ", want " + hexOf(want)
			}
			for _, r := range s.resp {
				write(vecs.get(t, r))
			}
		}
		if got, want := read(), vecs.get(t, "CLOSE"); !bytes.Equal(got, want) {
			mismatch <- "CLOSE: client sent " + hexOf(got)
		}
		write(vecs.get(t, "CLOSE_ACK"))
	}()

	counter, err := cl.CounterNew(9223372036854775800)
	if err != nil || counter.ID() != 0x00010001 {
		t.Fatalf("R1 CounterNew = %#x, %v; want handle 0x00010001", counter.ID(), err)
	}

	var le *LibError
	if _, err := cl.CounterAdd(counter, 100); !errors.As(err, &le) || le.Code != -1 || le.Detail != "overflow" || le.Func != "ml_counter_add" {
		t.Fatalf("R2 CounterAdd = %v; want LibError{-1, overflow}", err)
	}
	if got := le.Error(); got != "microlib: ml_counter_add: library error -1: overflow" {
		t.Errorf("LibError text = %q", got)
	}

	if v, err := cl.Add(7, 35); err != nil || v != 42 {
		t.Fatalf("R3 Add = %d, %v; want 42", v, err)
	}

	dst := make([]byte, 8)
	if out, err := cl.Xor([]byte{1, 2, 3}, 0xff, dst); err != nil || !bytes.Equal(out, []byte{0xfe, 0xfd, 0xfc}) {
		t.Fatalf("R4 Xor = %x, %v; want fefdfc", out, err)
	}
	if _, err := cl.Xor([]byte{1, 2, 3}, 0xff, make([]byte, 2)); !errors.As(err, &le) || le.Code != -1 || le.Detail != "" {
		t.Fatalf("R5 Xor = %v; want LibError{SIZE_MAX}", err)
	}

	var se *StatusError
	if _, err := cl.CounterAdd(counter, -1); !errors.As(err, &se) || se.Status != 5 || se.Msg != "denied" || !errors.Is(err, ErrDenied) {
		t.Fatalf("R6 CounterAdd = %v; want StatusError{5, denied}", err)
	}
	if errors.Is(se, ErrOutcomeUnknown) {
		t.Error("E_DENIED must be a known outcome")
	}
	if _, err := cl.CounterAdd(counterH{c.NewHandle(0x00010009)}, 1); !errors.As(err, &se) || se.Status != 3 || !errors.Is(err, ErrBadHandle) {
		t.Fatalf("R7 CounterAdd = %v; want StatusError{3}", err)
	}

	cells, err := cl.Cells(counter, 3, nil)
	want := []cell{{Tag: cellInt, Int: -2}, {Tag: cellText, Text: "hi"}, {Tag: cellNull}}
	if err != nil || len(cells) != 3 || cells[0] != want[0] || cells[1] != want[1] || cells[2] != want[2] {
		t.Fatalf("R8 Cells = %+v, %v; want %+v", cells, err, want)
	}

	cursor, err := cl.CursorOpen(counter, 3)
	if err != nil || cursor.ID() != 0x00010002 {
		t.Fatalf("R9 CursorOpen = %#x, %v", cursor.ID(), err)
	}
	if err := cl.CursorFree(cursor); !errors.As(err, &le) || le.Code != 2 || le.Detail != "" {
		t.Fatalf("R10 CursorFree = %v; want LibError{2}", err)
	}
	if err := cl.CursorFree(cursor); !errors.As(err, &se) || se.Status != 3 {
		t.Fatalf("R11 CursorFree = %v; want StatusError{3}", err)
	}
	if err := cl.CounterCheck(counter, 5); !errors.As(err, &le) || le.Code != -1 || le.Detail != "over limit" {
		t.Fatalf("R12 CounterCheck = %v; want LibError{-1, over limit}", err)
	}
	if err := cl.CounterFree(counter); err != nil {
		t.Fatalf("R13 CounterFree = %v", err)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("Close = %v", err)
	}
	<-scriptDone
	close(mismatch)
	for m := range mismatch {
		t.Error(m)
	}
	if err := c.Err(); err != nil {
		t.Errorf("session broken after a clean run: %v", err)
	}
}

func hexOf(b []byte) string {
	const digits = "0123456789abcdef"
	out := make([]byte, 0, 2*len(b))
	for _, x := range b {
		out = append(out, digits[x>>4], digits[x&15])
	}
	return string(out)
}

// TestVectorChunking replays the oversize pair with S = 128: the request is
// two frames and the response, which does not fit one frame, is assembled.
func TestVectorChunking(t *testing.T) {
	vecs, file := loadVectors(t)
	size := file.Chunking.BufferSize
	p := newPair(t, 8, size)
	c := newTestConn(t, p, connOpts{closeTO: 20 * time.Millisecond})
	cl := &stubClient{c}
	got := make(chan []byte, 2)
	go func() {
		for range 2 {
			r, err := p.server.ReserveRead()
			if err != nil {
				return
			}
			got <- bytes.Clone(r.Buffer())
			_ = r.Release()
		}
		for _, name := range []string{"C_RESP_1", "C_RESP_2"} {
			w, err := p.server.ReserveWrite()
			if err != nil {
				return
			}
			b := vecs.get(t, name)
			copy(w.Buffer(), b)
			_ = w.Commit(len(b))
		}
	}()
	src := make([]byte, 110)
	for i := range src {
		src[i] = byte(i)
	}
	dst := make([]byte, 110)
	out, err := cl.Xor(src, 0x5a, dst)
	if err != nil {
		t.Fatalf("Xor: %v", err)
	}
	for i, v := range out {
		if v != src[i]^0x5a {
			t.Fatalf("out[%d] = %#x", i, v)
		}
	}
	if len(out) != 110 {
		t.Fatalf("len = %d", len(out))
	}
	for _, name := range []string{"C_REQ_1", "C_REQ_2"} {
		if b := <-got; !bytes.Equal(b, vecs.get(t, name)) {
			t.Errorf("%s: client sent %s, want %s", name, hexOf(b), hexOf(vecs.get(t, name)))
		}
	}
}

// TestVectorWrap checks that the id after 0xFFFFFFFF is 1.
func TestVectorWrap(t *testing.T) {
	vecs, file := loadVectors(t)
	p := newPair(t, 8, file.Microlib.Geometry.BufferSize)
	c := newTestConn(t, p, connOpts{closeTO: 20 * time.Millisecond})
	c.nextID = 0xFFFFFFFE
	cl := &stubClient{c}
	got := make(chan []byte, 2)
	go func() {
		for range 2 {
			r, err := p.server.ReserveRead()
			if err != nil {
				return
			}
			b := bytes.Clone(r.Buffer())
			_ = r.Release()
			got <- b
			h := loadHeader(b)
			resp := frameOf(header{kind: kindResponse, op: h.op, id: h.id, total: 4}, i32Body(0))
			w, err := p.server.ReserveWrite()
			if err != nil {
				return
			}
			copy(w.Buffer(), resp)
			_ = w.Commit(len(resp))
		}
	}()
	// WRAP_A is the call with id 0xFFFFFFFF, WRAP_B the call with id 1.
	if _, err := cl.Add(1, 2); err != nil {
		t.Fatal(err)
	}
	if b := <-got; !bytes.Equal(b, vecs.get(t, "WRAP_A_REQ")) {
		t.Errorf("WRAP_A: client sent %s", hexOf(b))
	}
	if _, err := cl.Add(3, 4); err != nil {
		t.Fatal(err)
	}
	if b := <-got; !bytes.Equal(b, vecs.get(t, "WRAP_B_REQ")) {
		t.Errorf("WRAP_B: client sent %s", hexOf(b))
	}
}

func TestVectorHandshake(t *testing.T) {
	vecs, file := loadVectors(t)
	geo := file.Microlib.Geometry
	cfg := config{bufferCount: geo.BufferCount, bufferSize: geo.BufferSize, maxInFlight: 64, maxMessage: 16 << 20}
	g := grant{count: geo.BufferCount, size: geo.BufferSize}

	buf := make([]byte, geo.BufferSize)
	putHello(buf, microIface.Digest, g, cfg)
	if want := vecs.get(t, "HELLO"); !bytes.Equal(buf[:len(want)], want) {
		t.Errorf("HELLO = %s, want %s", hexOf(buf[:len(want)]), hexOf(want))
	}

	reply := vecs.get(t, "HELLO_REPLY")
	lim, err := parseHelloReply(reply, len(reply), microIface, cfg, g)
	want := Limits{BufferCount: 64, BufferSize: 16384, MaxInFlight: 64, MaxMessage: 16 << 20, MaxHandles: 4096}
	if err != nil || lim != want {
		t.Errorf("HELLO_REPLY = %+v, %v; want %+v", lim, err, want)
	}

	other := &Iface{Service: "microlib"}
	other.Digest[0] = 0xAA
	reply = vecs.get(t, "HELLO_REPLY_DIGEST")
	_, err = parseHelloReply(reply, len(reply), other, cfg, g)
	var me *MismatchError
	if !errors.As(err, &me) || me.Reason != helloDigestMismatch || me.Local != other.Digest || me.Remote != microIface.Digest {
		t.Fatalf("HELLO_REPLY_DIGEST = %v; want MismatchError carrying both digests", err)
	}
	if !errors.Is(err, ErrInterfaceMismatch) {
		t.Error("MismatchError must match ErrInterfaceMismatch")
	}
}

func TestHelloReplyNegatives(t *testing.T) {
	vecs, file := loadVectors(t)
	geo := file.Microlib.Geometry
	cfg := config{bufferCount: geo.BufferCount, bufferSize: geo.BufferSize, maxInFlight: 64, maxMessage: 16 << 20}
	g := grant{count: geo.BufferCount, size: geo.BufferSize}
	cases := []struct {
		name string
		edit func(b []byte) []byte
	}{
		{"kind", func(b []byte) []byte { b[0] = kindResponse; return b }},
		{"flags", func(b []byte) []byte { b[1] = 1; return b }},
		{"op", func(b []byte) []byte { b[2] = 1; return b }},
		{"id", func(b []byte) []byte { b[4] = 1; return b }},
		{"total", func(b []byte) []byte { b[8] = 63; return b }},
		{"short", func(b []byte) []byte { return b[:79] }},
		{"long", func(b []byte) []byte { return append(b, 0) }},
		{"magic", func(b []byte) []byte { b[16] = 'X'; return b }},
		{"major", func(b []byte) []byte { b[20] = 2; return b }},
		{"minor", func(b []byte) []byte { b[22] = 1; return b }},
		{"flags granted", func(b []byte) []byte { b[24] = 1; return b }},
		{"in flight zero", func(b []byte) []byte { b[28], b[29], b[30], b[31] = 0, 0, 0, 0; return b }},
		{"in flight above request", func(b []byte) []byte { b[28] = 65; return b }},
		{"message too small", func(b []byte) []byte { b[32], b[33], b[34], b[35] = 0, 0x0f, 0, 0; return b }},
		{"message above request", func(b []byte) []byte { b[35] = 2; return b }},
		{"handles zero", func(b []byte) []byte { b[36], b[37], b[38], b[39] = 0, 0, 0, 0; return b }},
		{"handles too many", func(b []byte) []byte { b[36], b[37], b[38] = 0, 0, 1; return b }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := tc.edit(vecs.get(t, "HELLO_REPLY"))
			_, err := parseHelloReply(b, len(b), microIface, cfg, g)
			var pe *ProtocolError
			if !errors.As(err, &pe) {
				t.Fatalf("got %v, want *ProtocolError", err)
			}
		})
	}
	// A success reply that carries another digest is a mismatch.
	b := vecs.get(t, "HELLO_REPLY")
	b[48] ^= 1
	if _, err := parseHelloReply(b, len(b), microIface, cfg, g); !errors.Is(err, ErrInterfaceMismatch) {
		t.Errorf("success reply with another digest = %v", err)
	}
}

// bareConn is a Conn without a link, for tests of the receiver rules.
func bareConn(maxInFlight, chunkSize, maxMessage int) *Conn {
	lim := Limits{BufferCount: 64, BufferSize: chunkSize, MaxInFlight: maxInFlight, MaxMessage: maxMessage, MaxHandles: 4096}
	return newConn(nil, func() error { return nil }, microIface, config{closeTimeout: time.Second}, lim)
}

// pushCall registers an unanswered call as Submit does.
func (c *Conn) pushCall(op uint16, respMax int64) *Call {
	c.nextID = nextID(c.nextID)
	call := c.newCall()
	call.id, call.op, call.respMax = c.nextID, op, respMax
	call.rf = c.rfOf(op, respMax)
	call.pending = true
	c.unans[(c.unansHd+c.unansN)%len(c.unans)] = call
	c.unansN++
	c.unansRF += call.rf
	call.idx = len(c.live)
	c.live = append(c.live, call)
	return call
}

// TestResponseNegatives feeds mutated response vectors to the receiver: each
// must be a protocol violation and none may complete a call.
func TestResponseNegatives(t *testing.T) {
	vecs, _ := loadVectors(t)
	r3 := func() []byte { return vecs.get(t, "R3_RESP") }
	r7 := func() []byte { return vecs.get(t, "R7_RESP") }
	cases := []struct {
		name string
		op   uint16
		bnd  int64
		resp func() []byte
	}{
		{"reserved flag", opAdd, 4, func() []byte { b := r3(); b[1] = 0x04; return b }},
		{"detail without fail", opAdd, 4, func() []byte { b := r3(); b[1] = flagDetail; return b }},
		{"flags on error response", opCounterAdd, 1048, func() []byte { b := r7(); b[1] = flagFail; return b }},
		{"body shorter than total", opAdd, 4, func() []byte { b := r3(); return b[:len(b)-1] }},
		{"body longer than total", opAdd, 4, func() []byte { return append(r3(), 0) }},
		{"header only", opAdd, 4, func() []byte { return r3()[:HeaderSize] }},
		{"shorter than header", opAdd, 4, func() []byte { return r3()[:15] }},
		{"empty frame", opAdd, 4, func() []byte { return nil }},
		{"wrong id", opAdd, 4, func() []byte { b := r3(); b[4] = 9; return b }},
		{"wrong op", opAdd, 4, func() []byte { b := r3(); b[2] = 2; return b }},
		{"id zero is a final frame needing status 7 or 8", opAdd, 4, func() []byte { b := r3(); b[4] = 0; return b }},
		{"kind REQUEST", opAdd, 4, func() []byte { b := r3(); b[0] = kindRequest; return b }},
		{"kind CONT without a message", opAdd, 4, func() []byte { b := r3(); b[0] = kindCont; return b }},
		{"kind HELLO_REPLY", opAdd, 4, func() []byte { b := r3(); b[0] = kindHelloReply; return b }},
		{"kind CLOSE_ACK", opAdd, 4, func() []byte { b := r3(); b[0] = kindCloseAck; return b }},
		{"kind BATCH_RESPONSE", opAdd, 4, func() []byte { b := r3(); b[0] = kindBatchResponse; return b }},
		{"kind 0x0a", opAdd, 4, func() []byte { b := r3(); b[0] = 0x0a; return b }},
		{"total above the bound", opAdd, 3, r3},
		{"total above max_message", opAdd, 4, func() []byte {
			b := r3()
			b[8], b[9], b[10], b[11] = 0xff, 0xff, 0xff, 0x7f
			return b
		}},
		{"error response above ERRBODY_MAX", opCounterAdd, 1048, func() []byte {
			return frameOf(header{kind: kindResponse, op: opCounterAdd, id: 1, total: ErrBodyMax + 1, aux: statusDenied}, make([]byte, ErrBodyMax+1))
		}},
		{"reserved status 9", opCounterAdd, 1048, func() []byte { b := r7(); b[12] = 9; return b }},
		{"reserved status 0xff", opCounterAdd, 1048, func() []byte { b := r7(); b[12] = 0xff; return b }},
		{"status 0x10000", opCounterAdd, 1048, func() []byte { b := r7(); b[12], b[14] = 0, 1; return b }},
		{"status 7 with an id", opCounterAdd, 1048, func() []byte { b := r7(); b[12] = 7; return b }},
		{"status 8 with an id", opCounterAdd, 1048, func() []byte { b := r7(); b[12] = 8; return b }},
		{"status message without NUL", opCounterAdd, 1048, func() []byte { b := r7(); b[len(b)-1] = 'x'; return b }},
		{"status message with trailing byte", opCounterAdd, 1048, func() []byte {
			b := append(r7(), 0)
			b[8]++
			return b
		}},
		{"status message too long", opCounterAdd, 1048, func() []byte {
			return frameOf(header{kind: kindResponse, op: opCounterAdd, id: 1, total: uint32(StrWire(MsgMax + 1)), aux: statusDenied}, errBody(string(bytes.Repeat([]byte{'a'}, MsgMax+1))))
		}},
		{"final frame with status 5", opAdd, 4, func() []byte {
			return frameOf(header{kind: kindResponse, aux: statusDenied, total: uint32(StrWire(1))}, errBody("x"))
		}},
		{"final frame with op", opAdd, 4, func() []byte {
			return frameOf(header{kind: kindResponse, op: 1, aux: statusProtocol, total: uint32(StrWire(1))}, errBody("x"))
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := bareConn(8, 16384, 16<<20)
			call := c.pushCall(tc.op, tc.bnd)
			_, complete, err := c.acceptFrame(tc.resp(), rxBuffer)
			var pe *ProtocolError
			var se *StatusError
			switch {
			case errors.As(err, &pe), errors.As(err, &se) && se.Status == statusProtocol:
			default:
				t.Fatalf("acceptFrame = %v, want *ProtocolError", err)
			}
			if complete || call.done {
				t.Fatal("a rejected frame completed the call")
			}
		})
	}
}

func TestFinalFrameIsStatusError(t *testing.T) {
	for _, status := range []uint32{statusProtocol, statusShuttingDown} {
		c := bareConn(8, 16384, 16<<20)
		c.pushCall(opAdd, 4)
		f := frameOf(header{kind: kindResponse, aux: status, total: uint32(StrWire(2))}, errBody("xy"))
		_, _, err := c.acceptFrame(f, rxBuffer)
		var se *StatusError
		if !errors.As(err, &se) || se.Status != status || se.Msg != "xy" {
			t.Fatalf("status %d: %v", status, err)
		}
		if !errors.Is(err, ErrOutcomeUnknown) {
			t.Errorf("status %d: final frames reach calls in flight and are an unknown outcome", status)
		}
		want := hqq.ErrProtocol
		if status == statusShuttingDown {
			want = nil
		}
		if want != nil && !errors.Is(err, want) {
			t.Errorf("status %d does not match hqq.ErrProtocol", status)
		}
		if status == statusShuttingDown && !errors.Is(err, ErrShuttingDown) {
			t.Error("status 8 does not match ErrShuttingDown")
		}
	}
}

// TestContinuationRules covers the receiver of 4.8: every CONT rule, and the
// memory bound that comes from the call's own bound.
func TestContinuationRules(t *testing.T) {
	const size = 128
	chunk := size - HeaderSize
	body := make([]byte, 3*chunk+5)
	for i := range body {
		body[i] = byte(i)
	}
	frames := splitMessage(kindResponse, chunk, opXor, 1, 0, 0, body)
	if len(frames) != 4 {
		t.Fatalf("%d frames", len(frames))
	}
	start := func() (*Conn, *Call) {
		c := bareConn(8, size, 1<<20)
		return c, c.pushCall(opXor, int64(len(body)))
	}
	t.Run("complete", func(t *testing.T) {
		c, call := start()
		for i, f := range frames {
			_, complete, err := c.acceptFrame(bytes.Clone(f), rxBuffer)
			if err != nil || complete != (i == len(frames)-1) {
				t.Fatalf("frame %d: complete=%v err=%v", i, complete, err)
			}
		}
		if !call.done || !bytes.Equal(call.body, body) {
			t.Fatal("assembled body differs")
		}
		if c.unansN != 0 || c.unansRF != 0 || c.asm.active {
			t.Fatal("state not reset")
		}
	})
	t.Run("discard", func(t *testing.T) {
		c, call := start()
		for _, f := range frames {
			if _, _, err := c.acceptFrame(bytes.Clone(f), rxDiscard); err != nil {
				t.Fatal(err)
			}
		}
		if !call.done || call.body != nil || cap(call.buf) != 0 {
			t.Fatal("discard mode kept the body")
		}
	})
	mutations := []struct {
		name string
		edit func(f []byte) []byte
	}{
		{"wrong id", func(f []byte) []byte { f[4] = 9; return f }},
		{"wrong total", func(f []byte) []byte { f[8]++; return f }},
		{"aux too small", func(f []byte) []byte { f[12]--; return f }},
		{"aux too large", func(f []byte) []byte { f[12]++; return f }},
		{"flags", func(f []byte) []byte { f[1] = flagFail; return f }},
		{"op", func(f []byte) []byte { f[2] = 1; return f }},
		{"kind RESPONSE", func(f []byte) []byte { f[0] = kindResponse; return f }},
		{"kind REQUEST", func(f []byte) []byte { f[0] = kindRequest; return f }},
		{"short", func(f []byte) []byte { return f[:len(f)-1] }},
		{"long", func(f []byte) []byte { return append(f, 0) }},
	}
	for _, m := range mutations {
		t.Run(m.name, func(t *testing.T) {
			c, call := start()
			if _, _, err := c.acceptFrame(bytes.Clone(frames[0]), rxBuffer); err != nil {
				t.Fatal(err)
			}
			_, _, err := c.acceptFrame(m.edit(bytes.Clone(frames[1])), rxBuffer)
			var pe *ProtocolError
			if !errors.As(err, &pe) {
				t.Fatalf("got %v, want *ProtocolError", err)
			}
			if call.done {
				t.Fatal("call completed")
			}
		})
	}
	t.Run("first frame short", func(t *testing.T) {
		c, _ := start()
		f := bytes.Clone(frames[0])
		_, _, err := c.acceptFrame(f[:len(f)-1], rxBuffer)
		var pe *ProtocolError
		if !errors.As(err, &pe) {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("total above the call's bound", func(t *testing.T) {
		c := bareConn(8, size, 1<<20)
		call := c.pushCall(opXor, int64(len(body))-1)
		_, _, err := c.acceptFrame(bytes.Clone(frames[0]), rxBuffer)
		var pe *ProtocolError
		if !errors.As(err, &pe) || cap(call.buf) != 0 {
			t.Fatalf("got %v, buf cap %d: an unbounded total must be refused before any allocation", err, cap(call.buf))
		}
	})
	t.Run("total above max_message", func(t *testing.T) {
		c := bareConn(8, size, 4096)
		call := c.pushCall(opXor, 1<<20)
		f := splitMessage(kindResponse, chunk, opXor, 1, 0, 0, make([]byte, 4097))
		_, _, err := c.acceptFrame(f[0], rxBuffer)
		var pe *ProtocolError
		if !errors.As(err, &pe) || cap(call.buf) != 0 {
			t.Fatalf("got %v, buf cap %d", err, cap(call.buf))
		}
	})
	t.Run("a message may not be interrupted by a response", func(t *testing.T) {
		c, _ := start()
		if _, _, err := c.acceptFrame(bytes.Clone(frames[0]), rxBuffer); err != nil {
			t.Fatal(err)
		}
		_, _, err := c.acceptFrame(frameOf(header{kind: kindResponse, op: opXor, id: 2}, nil), rxBuffer)
		var pe *ProtocolError
		if !errors.As(err, &pe) {
			t.Fatalf("got %v", err)
		}
	})
}

func TestBoundaryTotals(t *testing.T) {
	// A response of exactly CHUNK bytes is one frame; one byte more is two.
	const size = 128
	chunk := size - HeaderSize
	for _, n := range []int{0, 1, chunk - 1, chunk, chunk + 1, 2 * chunk, 2*chunk + 1} {
		c := bareConn(8, size, 1<<20)
		call := c.pushCall(opXor, int64(n))
		body := bytes.Repeat([]byte{7}, n)
		frames := splitMessage(kindResponse, chunk, opXor, 1, 0, 0, body)
		if want := max(1, (n+chunk-1)/chunk); len(frames) != want {
			t.Fatalf("n=%d: %d frames, want %d", n, len(frames), want)
		}
		for _, f := range frames {
			if _, _, err := c.acceptFrame(f, rxBuffer); err != nil {
				t.Fatalf("n=%d: %v", n, err)
			}
		}
		if !call.done || !bytes.Equal(call.body, body) {
			t.Fatalf("n=%d: body differs", n)
		}
	}
}

// TestResponseVectorsAccepted feeds every response vector to the receiver in
// each body mode: all are accepted and complete the call they answer.
func TestResponseVectorsAccepted(t *testing.T) {
	vecs, file := loadVectors(t)
	type vec struct {
		name string
		size int
	}
	var all []vec
	for _, f := range file.Microlib.Frames {
		if f.Dir == "s2c" && len(f.Name) > 5 && f.Name[len(f.Name)-5:] == "_RESP" {
			all = append(all, vec{f.Name, file.Microlib.Geometry.BufferSize})
		}
	}
	all = append(all, vec{"C_RESP_1", file.Chunking.BufferSize})
	for _, v := range all {
		for _, mode := range []rxMode{rxBuffer, rxHold, rxDiscard} {
			frames := [][]byte{vecs.get(t, v.name)}
			if v.name == "C_RESP_1" {
				frames = append(frames, vecs.get(t, "C_RESP_2"))
			}
			h := loadHeader(frames[0])
			c := bareConn(8, v.size, 1<<24)
			c.nextID = h.id - 1
			call := c.pushCall(h.op, 1<<14)
			var held, complete bool
			for i, f := range frames {
				var err error
				held, complete, err = c.acceptFrame(f, mode)
				if err != nil {
					t.Fatalf("%s mode %d: %v", v.name, mode, err)
				}
				if complete != (i == len(frames)-1) {
					t.Fatalf("%s mode %d: complete=%v after frame %d", v.name, mode, complete, i)
				}
			}
			if !call.done {
				t.Fatalf("%s mode %d: call not answered", v.name, mode)
			}
			wantHeld := mode == rxHold && h.aux == 0 && len(frames) == 1
			if held != wantHeld || call.inPlace != wantHeld {
				t.Errorf("%s mode %d: held=%v inPlace=%v, want %v", v.name, mode, held, call.inPlace, wantHeld)
			}
		}
	}
}
