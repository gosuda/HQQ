package rt

import (
	"errors"
	"testing"
	"time"

	hqq "gosuda.org/hqq"
)

// fuzzBounds are the RESP_BOUND values the fuzz targets give their calls.
var fuzzBounds = [...]int64{0, 4, 16, 200, 300, 1000, 4096}

const (
	fuzzSize       = 128 // buffer size of the fuzz links: CHUNK is 112
	fuzzCount      = 16
	fuzzMaxMessage = 4096
)

// fuzzInput builds a corpus entry: header bytes (one per call), then frames
// prefixed by a length byte.
func fuzzInput(calls []byte, frames ...[]byte) []byte {
	out := append([]byte{byte(len(calls) - 1)}, calls...)
	for _, f := range frames {
		out = append(out, byte(len(f)))
		out = append(out, f...)
	}
	return out
}

// callSpec encodes a call as one input byte: the high nibble selects the op
// (1 + n mod 11), the low nibble the bound from fuzzBounds.
func callSpec(op uint16, boundIdx int) byte { return byte(op-1)<<4 | byte(boundIdx) }

func seedFrames(f *testing.F) {
	vecs, _ := loadVectors(f)
	resp := func(op uint16, id uint32, flags uint8, aux uint32, body []byte) []byte {
		return frameOf(header{kind: kindResponse, op: op, id: id, flags: flags, aux: aux, total: uint32(len(body))}, body)
	}
	f.Add(fuzzInput([]byte{callSpec(1, 1)}, vecs["R3_RESP"]))
	f.Add(fuzzInput([]byte{callSpec(1, 1), callSpec(1, 1)}, resp(1, 1, 0, 0, i32Body(1)), resp(1, 2, 0, 0, i32Body(2))))
	f.Add(fuzzInput([]byte{callSpec(1, 1)}, resp(1, 1, 0, statusDenied, errBody("denied"))))
	f.Add(fuzzInput([]byte{callSpec(1, 1)}, resp(0, 0, 0, statusProtocol, errBody("protocol violation"))))
	f.Add(fuzzInput([]byte{callSpec(8, 4)}, vecs["C_RESP_1"], vecs["C_RESP_2"]))
	f.Add(fuzzInput([]byte{callSpec(8, 3)}, vecs["R4_RESP"][:0], vecs["R4_RESP"]))
	f.Add(fuzzInput([]byte{callSpec(3, 4)}, vecs["R2_RESP"]))
	f.Add(fuzzInput([]byte{callSpec(7, 1)}, vecs["R10_RESP"]))
	f.Add(fuzzInput([]byte{callSpec(1, 1)}, resp(1, 1, 0xff, 0, i32Body(1))))
	f.Add(fuzzInput([]byte{callSpec(1, 1)}, frameOf(header{kind: kindCont, id: 1, total: 4}, i32Body(1))))
}

func acceptable(err error) bool {
	var (
		pe *ProtocolError
		se *StatusError
		te *TransportError
	)
	return errors.As(err, &pe) || errors.As(err, &se) || errors.As(err, &te) || errors.Is(err, ErrFutureUsed)
}

// FuzzDecodeResponse feeds an arbitrary sequence of server frames to a client
// Conn over an in-process link and waits for the calls it issued. A frame
// sequence may end a call with a result, a status or a violation; it must
// never panic, leave a reservation open, or make the client allocate more
// than the bounds of 4.6.3 and max_message allow.
func FuzzDecodeResponse(f *testing.F) {
	seedFrames(f)
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) < 2 {
			return
		}
		k := 1 + int(data[0])%4
		pos := 1
		type spec struct {
			op    uint16
			bound int64
		}
		specs := make([]spec, 0, k)
		for range k {
			var b byte
			if pos < len(data) {
				b = data[pos]
				pos++
			}
			specs = append(specs, spec{op: 1 + uint16(b>>4)%11, bound: fuzzBounds[int(b&15)%len(fuzzBounds)]})
		}
		for i := range specs {
			if specs[i].bound > 300 {
				specs[i].bound = 300 // keep sum(rf) within the ring of 16 for 4 calls
			}
		}
		var frames [][]byte
		for pos < len(data) && len(frames) < fuzzCount {
			n := int(data[pos]) % (fuzzSize + 1)
			pos++
			end := min(pos+n, len(data))
			if end > pos {
				frames = append(frames, data[pos:end])
			}
			pos = end
		}

		mem := hqq.AlignedBuffer(hqq.SizeStandardLink(fuzzCount, fuzzSize))
		quiet := hqq.LinkOptions{HeartbeatInterval: -1, PeerTimeout: time.Second}
		server, err := hqq.NewStandardLink(mem, fuzzCount, fuzzSize, quiet)
		if err != nil {
			t.Fatal(err)
		}
		client, err := hqq.NewStandardLink(mem, fuzzCount, fuzzSize, quiet)
		if err != nil {
			t.Fatal(err)
		}
		lim := Limits{BufferCount: fuzzCount, BufferSize: fuzzSize, MaxInFlight: fuzzCount, MaxMessage: fuzzMaxMessage, MaxHandles: 4096}
		c := newConn(client, client.Close, microIface, config{closeTimeout: 50 * time.Millisecond}, lim)

		calls := make([]*Call, 0, k)
		for _, s := range specs {
			e, err := c.BeginRequest(s.op, 0, s.bound)
			if err != nil {
				t.Fatalf("BeginRequest: %v", err)
			}
			call, err := c.Submit(e)
			if err != nil {
				t.Fatalf("Submit: %v", err)
			}
			calls = append(calls, call)
		}
		for _, fr := range frames {
			w, err := server.ReserveWrite()
			if err != nil {
				t.Fatalf("server write: %v", err)
			}
			copy(w.Buffer(), fr)
			if err := w.Commit(len(fr)); err != nil {
				t.Fatalf("server commit: %v", err)
			}
		}
		if err := server.Close(); err != nil {
			t.Fatal(err)
		}

		order := make([]int, k)
		for i := range order {
			order[i] = i
			if data[0]&0x80 != 0 {
				order[i] = k - 1 - i
			}
		}
		for _, i := range order {
			d, err := c.Wait(calls[i], calls[i].id)
			if err != nil {
				if !acceptable(err) {
					t.Fatalf("Wait: unexpected error type %T: %v", err, err)
				}
				continue
			}
			if int64(len(d.Body)) > specs[i].bound {
				t.Fatalf("body of %d bytes above its bound %d", len(d.Body), specs[i].bound)
			}
			if d.Detail && !d.Fail {
				t.Fatal("F_DETAIL without F_FAIL")
			}
			d.Release()
			if c.rxHeld || c.rxOpen {
				t.Fatal("reservation left open after Release")
			}
		}
		if c.rxHeld || c.rxOpen || c.txOpen {
			t.Fatal("reservation left open")
		}
		for _, call := range append(append([]*Call(nil), c.free...), c.live...) {
			if cap(call.buf) > 512 {
				t.Fatalf("buffer of %d bytes: allocation above the bounds", cap(call.buf))
			}
		}
		if c.unansN < 0 || c.unansRF < 0 || c.unansN > fuzzCount {
			t.Fatalf("flow-control state: n=%d R=%d", c.unansN, c.unansRF)
		}
		if err := c.Close(); err != nil && !acceptable(err) {
			t.Fatalf("Close: unexpected error type %T: %v", err, err)
		}
	})
}

// FuzzFrameReceiver feeds arbitrary frames straight to the receiver rules of
// 4.4 and 4.8, in every body mode, and checks the invariants of its state.
func FuzzFrameReceiver(f *testing.F) {
	seedFrames(f)
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) < 2 {
			return
		}
		c := bareConn(8, fuzzSize, fuzzMaxMessage)
		k := 1 + int(data[0])%8
		pos := 1
		var calls []*Call
		for range k {
			var b byte
			if pos < len(data) {
				b = data[pos]
				pos++
			}
			calls = append(calls, c.pushCall(1+uint16(b>>4)%11, fuzzBounds[int(b&15)%len(fuzzBounds)]))
		}
		mode := rxMode(data[0]>>4) % 3
		for pos < len(data) {
			n := int(data[pos])
			pos++
			end := min(pos+n, len(data))
			frame := append([]byte(nil), data[pos:end]...)
			pos = end

			head := c.unansHead()
			held, complete, err := c.acceptFrame(frame, mode)
			if err != nil {
				var pe *ProtocolError
				var se *StatusError
				if !errors.As(err, &pe) && !errors.As(err, &se) {
					t.Fatalf("error %T: %v", err, err)
				}
				if complete || held {
					t.Fatal("a failed frame reported progress")
				}
				return
			}
			sum, count := 0, 0
			for i := range c.unansN {
				sum += c.unans[(c.unansHd+i)%len(c.unans)].rf
				count++
			}
			if sum != c.unansRF || count != c.unansN {
				t.Fatalf("R = %d, ring holds %d", c.unansRF, sum)
			}
			if complete {
				if head == nil || !head.done || c.asm.active {
					t.Fatal("complete without a finished call")
				}
				if head.status == 0 && mode == rxBuffer && int64(len(head.body)) > head.respMax {
					t.Fatalf("body of %d bytes above bound %d", len(head.body), head.respMax)
				}
				if held && (mode != rxHold || !head.inPlace) {
					t.Fatal("held outside hold mode")
				}
				continue
			}
			if held {
				t.Fatal("held an incomplete response")
			}
			if head != nil && c.asm.active {
				if c.asm.got <= 0 || c.asm.got >= int(c.asm.total) || int64(c.asm.total) > head.respMax || int(c.asm.total) > fuzzMaxMessage {
					t.Fatalf("assembly state %+v for bound %d", c.asm, head.respMax)
				}
				if mode != rxDiscard && len(head.buf) < int(c.asm.total) {
					t.Fatal("assembly buffer smaller than total")
				}
			}
		}
		for _, call := range calls {
			if cap(call.buf) > max(int(call.respMax), fuzzSize) {
				t.Fatalf("buffer of %d bytes for bound %d", cap(call.buf), call.respMax)
			}
		}
	})
}
