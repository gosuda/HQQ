package rt

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"sync"
	"testing"
	"time"

	hqq "gosuda.org/hqq"
)

func TestSyncCall(t *testing.T) {
	p := newPair(t, 16, 4096)
	startFakeServer(t, p, microHandler)
	c := newTestConn(t, p, connOpts{})
	cl := &stubClient{c}
	for i := range int32(50) {
		got, err := cl.Add(i, 2*i)
		if err != nil || got != 3*i {
			t.Fatalf("Add(%d, %d) = %d, %v", i, 2*i, got, err)
		}
	}
	if c.unansN != 0 || c.unansRF != 0 || len(c.live) != 0 {
		t.Errorf("state left over: unans=%d R=%d live=%d", c.unansN, c.unansRF, len(c.live))
	}
}

func TestPipelinedOutOfOrderWait(t *testing.T) {
	p := newPair(t, 16, 4096)
	startFakeServer(t, p, microHandler)
	c := newTestConn(t, p, connOpts{})
	cl := &stubClient{c}
	// 16 calls fill the window exactly; the 17th has to read a response
	// first.
	var futs []addFuture
	for i := range int32(40) {
		f, err := cl.AddAsync(i, 1000)
		if err != nil {
			t.Fatalf("AddAsync %d: %v", i, err)
		}
		futs = append(futs, f)
	}
	for i := len(futs) - 1; i >= 0; i-- {
		got, err := futs[i].Wait()
		if err != nil || got != int32(i)+1000 {
			t.Fatalf("Wait %d = %d, %v", i, got, err)
		}
	}
	if len(c.live) != 0 || c.unansN != 0 {
		t.Errorf("live=%d unans=%d", len(c.live), c.unansN)
	}
}

func TestOutOfOrderWithChunkedResponses(t *testing.T) {
	p := newPair(t, 64, 256)
	startFakeServer(t, p, microHandler)
	c := newTestConn(t, p, connOpts{})
	cl := &stubClient{c}
	srcs := [][]byte{bytes.Repeat([]byte{1}, 10), bytes.Repeat([]byte{2}, 700), bytes.Repeat([]byte{3}, 239), bytes.Repeat([]byte{4}, 240)}
	var futs []xorFuture
	for i, s := range srcs {
		f, err := cl.XorAsync(s, byte(i), make([]byte, len(s)))
		if err != nil {
			t.Fatal(err)
		}
		futs = append(futs, f)
	}
	for _, i := range []int{2, 0, 3, 1} {
		out, err := futs[i].Wait()
		if err != nil || len(out) != len(srcs[i]) {
			t.Fatalf("Wait %d: %d bytes, %v", i, len(out), err)
		}
		for _, b := range out {
			if b != srcs[i][0]^byte(i) {
				t.Fatalf("Wait %d: wrong byte %#x", i, b)
			}
		}
	}
}

// TestFlowControlNoDeadlock is the deadlock of 4.12: with B = 2 and a slow
// server, a client that kept issuing requests would block on the request ring
// while the server blocks on the response ring.
func TestFlowControlNoDeadlock(t *testing.T) {
	for _, count := range []int{2, 4} {
		p := newPair(t, count, 256)
		startFakeServer(t, p, func(req fakeReq) fakeReply {
			time.Sleep(100 * time.Microsecond)
			return microHandler(req)
		})
		c := newTestConn(t, p, connOpts{})
		cl := &stubClient{c}
		done := make(chan error, 1)
		go func() {
			futs := make([]addFuture, 0, 100)
			for i := range int32(100) {
				f, err := cl.AddAsync(i, 7)
				if err != nil {
					done <- err
					return
				}
				futs = append(futs, f)
			}
			for i, f := range futs {
				if got, err := f.Wait(); err != nil || got != int32(i)+7 {
					done <- errors.Join(err, errors.New("wrong result"))
					return
				}
			}
			done <- nil
		}()
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("B=%d: %v", count, err)
			}
		case <-time.After(20 * time.Second):
			t.Fatalf("B=%d: deadlock", count)
		}
	}
}

// TestMaxInFlight holds every response and checks that the client commits no
// more requests than the granted window.
func TestMaxInFlight(t *testing.T) {
	p := newPair(t, 64, 4096)
	gate := make(chan struct{})
	s := startFakeServer(t, p, func(req fakeReq) fakeReply {
		r := microHandler(req)
		r.wait = gate
		return r
	})
	c := newTestConn(t, p, connOpts{maxInFlight: 3})
	cl := &stubClient{c}
	var wg sync.WaitGroup
	results := make(chan error, 10)
	wg.Add(1)
	go func() {
		defer wg.Done()
		var futs []addFuture
		for i := range int32(10) {
			f, err := cl.AddAsync(i, 1)
			if err != nil {
				results <- err
				return
			}
			futs = append(futs, f)
		}
		for i, f := range futs {
			if got, err := f.Wait(); err != nil || got != int32(i)+1 {
				results <- errors.Join(err, errors.New("wrong result"))
				return
			}
		}
	}()
	time.Sleep(150 * time.Millisecond)
	if n := s.requestCount(); n != 3 {
		t.Fatalf("server saw %d requests while all responses are held, want 3", n)
	}
	close(gate)
	wg.Wait()
	close(results)
	for err := range results {
		t.Fatal(err)
	}
}

// TestFlowControlWindowByFrames checks R + rf(q) <= B with rf > 1: with S =
// 128 and a bound of 300 bytes rf is 3, so B = 8 allows two calls in flight.
func TestFlowControlWindowByFrames(t *testing.T) {
	p := newPair(t, 8, 128)
	gate := make(chan struct{})
	s := startFakeServer(t, p, func(req fakeReq) fakeReply {
		r := microHandler(req)
		r.wait = gate
		return r
	})
	c := newTestConn(t, p, connOpts{})
	cl := &stubClient{c}
	if got := c.rfOf(opXor, 300); got != 3 {
		t.Fatalf("rf(300) = %d, want 3", got)
	}
	errc := make(chan error, 1)
	go func() {
		var futs []xorFuture
		for range 5 {
			f, err := cl.XorAsync([]byte{1}, 0, make([]byte, 300-8-VarSlack))
			if err != nil {
				errc <- err
				return
			}
			futs = append(futs, f)
		}
		for _, f := range futs {
			if _, err := f.Wait(); err != nil {
				errc <- err
				return
			}
		}
		errc <- nil
	}()
	time.Sleep(150 * time.Millisecond)
	if n := s.requestCount(); n != 2 {
		t.Fatalf("server saw %d requests, want 2 (R + rf <= B)", n)
	}
	close(gate)
	if err := <-errc; err != nil {
		t.Fatal(err)
	}
}

// TestResponseLargerThanRing: rf > B, so the call is synchronous: the client
// has read the whole response before it commits the next request.
func TestResponseLargerThanRing(t *testing.T) {
	p := newPair(t, 4, 128)
	var s *fakeServer
	sentAtSecond := make(chan int, 1)
	s = startFakeServer(t, p, func(req fakeReq) fakeReply {
		if req.id == 2 {
			s.mu.Lock()
			sentAtSecond <- s.sent
			s.mu.Unlock()
		}
		return microHandler(req)
	})
	c := newTestConn(t, p, connOpts{})
	cl := &stubClient{c}
	const n = 900
	src := bytes.Repeat([]byte{9}, n)
	capacity := n + 100
	if rf := c.rfOf(opXor, int64(8+VarSlack+capacity)); rf <= p.count {
		t.Fatalf("rf = %d, test needs rf > B = %d", rf, p.count)
	}
	f1, err := cl.XorAsync(src, 1, make([]byte, capacity))
	if err != nil {
		t.Fatal(err)
	}
	f2, err := cl.XorAsync(src, 2, make([]byte, capacity))
	if err != nil {
		t.Fatal(err)
	}
	frames := int64(len(splitMessage(kindResponse, 112, opXor, 1, 0, 0, make([]byte, 8+4+n))))
	var got int
	select {
	case got = <-sentAtSecond:
	case <-time.After(5 * time.Second):
		t.Fatal("request 2 never arrived")
	}
	if int64(got) < frames {
		t.Fatalf("server had written %d response frames when request 2 arrived, want %d: the client did not wait for the response of a call with rf > B", got, frames)
	}
	for i, f := range []xorFuture{f1, f2} {
		out, err := f.Wait()
		if err != nil || len(out) != n || out[0] != 9^byte(i+1) {
			t.Fatalf("future %d: %d bytes, %v", i, len(out), err)
		}
	}
}

func TestRfCache(t *testing.T) {
	c := bareConn(8, 128, 1<<20)
	chunk := int64(112)
	for _, tc := range []struct {
		bound int64
		want  int
	}{{0, 1}, {1, 1}, {chunk, 1}, {chunk + 1, 2}, {2 * chunk, 2}, {2*chunk + 1, 3}, {1 << 20, 9363}} {
		if got := c.rfOf(opXor, tc.bound); got != tc.want {
			t.Errorf("rf(%d) = %d, want %d", tc.bound, got, tc.want)
		}
		if e := c.rfs[opXor]; e.bound != tc.bound || e.rf != tc.want {
			t.Errorf("cache after rf(%d) = %+v", tc.bound, e)
		}
	}
	// The cache is per op: another op keeps its own entry.
	c.rfOf(opAdd, 4)
	if e := c.rfs[opXor]; e.bound != 1<<20 {
		t.Errorf("rf of one op evicted the entry of another: %+v", e)
	}
}

func TestOversizeRequestAndResponse(t *testing.T) {
	p := newPair(t, 64, 256)
	startFakeServer(t, p, microHandler)
	c := newTestConn(t, p, connOpts{})
	cl := &stubClient{c}
	for _, n := range []int{0, 1, 239 - 12, 240 - 12 + 1, 5000, 65536} {
		src := make([]byte, n)
		for i := range src {
			src[i] = byte(i * 7)
		}
		out, err := cl.Xor(src, 0x5a, make([]byte, n))
		if err != nil || len(out) != n {
			t.Fatalf("n=%d: %d bytes, %v", n, len(out), err)
		}
		for i := range out {
			if out[i] != src[i]^0x5a {
				t.Fatalf("n=%d: byte %d = %#x", n, i, out[i])
			}
		}
	}
	// Pipelined oversize calls whose responses fit the ring.
	var futs []xorFuture
	src := bytes.Repeat([]byte{3}, 1500)
	for range 6 {
		f, err := cl.XorAsync(src, 1, make([]byte, len(src)))
		if err != nil {
			t.Fatal(err)
		}
		futs = append(futs, f)
	}
	for _, f := range futs {
		if out, err := f.Wait(); err != nil || len(out) != 1500 || out[0] != 2 {
			t.Fatalf("pipelined oversize: %d bytes, %v", len(out), err)
		}
	}
}

func TestRequestLimits(t *testing.T) {
	p := newPair(t, 8, 256)
	startFakeServer(t, p, microHandler)
	c := newTestConn(t, p, connOpts{maxMessage: 4096})
	for _, tc := range []struct {
		name    string
		op      uint16
		bodyLen int
		respMax int64
	}{
		{"op zero", 0, 8, 4},
		{"op above the table", 12, 8, 4},
		{"negative body", opAdd, -1, 4},
		{"body above max_message", opAdd, 4097, 4},
		{"bound above max_message", opAdd, 8, 4097},
		{"negative bound", opAdd, 8, -1},
	} {
		if e, err := c.BeginRequest(tc.op, tc.bodyLen, tc.respMax); !errors.Is(err, ErrInvalidArg) || e != nil {
			t.Errorf("%s: %v", tc.name, err)
		}
	}
	// Nothing was reserved or committed by the refused calls.
	cl := &stubClient{c}
	if v, err := cl.Add(1, 2); err != nil || v != 3 {
		t.Fatalf("Add after refused calls = %d, %v", v, err)
	}
	if e, err := c.BeginRequest(opAdd, 4096, 4); err != nil {
		t.Fatalf("body of exactly max_message: %v", err)
	} else {
		c.Abort(e)
	}
}

func TestLibraryFailureWithDetail(t *testing.T) {
	p := newPair(t, 8, 4096)
	startFakeServer(t, p, microHandler)
	c := newTestConn(t, p, connOpts{})
	cl := &stubClient{c}
	err := cl.CounterCheck(counterH{c.NewHandle(0x00010001)}, 5)
	var le *LibError
	if !errors.As(err, &le) || le.Code != -1 || le.Detail != "over limit" || le.Func != "ml_counter_check" {
		t.Fatalf("CounterCheck = %v", err)
	}
	if errors.Is(err, ErrOutcomeUnknown) {
		t.Error("a library failure is a known outcome")
	}
	// A failed call leaves the session usable.
	if v, e := cl.Add(1, 1); e != nil || v != 2 {
		t.Fatalf("Add = %d, %v", v, e)
	}
}

func TestDetailBodyChecks(t *testing.T) {
	mk := func(flags uint8, body []byte) fakeReply { return fakeReply{flags: flags, body: body} }
	good := make([]byte, 8+StrWire(3))
	le.PutUint32(good, ^uint32(0))
	PutStr(good, 8, "abc")
	long := make([]byte, 8+StrWire(DetailMax+1))
	PutStr(long, 8, string(bytes.Repeat([]byte{'x'}, DetailMax+1)))
	cases := []struct {
		name  string
		reply fakeReply
		ok    bool
	}{
		{"detail", mk(flagFail|flagDetail, good), true},
		{"detail missing", mk(flagFail|flagDetail, good[:4]), false},
		{"detail truncated", mk(flagFail|flagDetail, good[:len(good)-1]), false},
		{"detail with trailing byte", mk(flagFail|flagDetail, append(bytes.Clone(good), 0)), false},
		{"detail too long", mk(flagFail|flagDetail, long), false},
		{"detail with NUL inside", mk(flagFail|flagDetail, func() []byte { b := bytes.Clone(good); b[13] = 0; return b }()), false},
		{"fail without detail but with a string", mk(flagFail, good), false},
		{"fail body of the wrong size", mk(flagFail, good[:5]), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := newPair(t, 8, 4096)
			startFakeServer(t, p, func(fakeReq) fakeReply { return tc.reply })
			c := newTestConn(t, p, connOpts{})
			err := (&stubClient{c}).CounterCheck(counterH{c.NewHandle(0x00010001)}, 5)
			var le *LibError
			var pe *ProtocolError
			switch {
			case tc.ok && (!errors.As(err, &le) || le.Detail != "abc"):
				t.Fatalf("got %v", err)
			case !tc.ok && !errors.As(err, &pe):
				t.Fatalf("got %v, want *ProtocolError", err)
			}
			if !tc.ok && c.Err() == nil {
				t.Error("a malformed body must break the session")
			}
		})
	}
}

func TestCallStatuses(t *testing.T) {
	statuses := []struct {
		status  uint32
		want    error
		unknown bool
	}{
		{statusUnknownOp, ErrUnknownOp, false}, {statusBadRequest, ErrBadRequest, false},
		{statusBadHandle, ErrBadHandle, false}, {statusLimit, ErrLimit, false},
		{statusDenied, ErrDenied, false}, {statusInternal, ErrInternal, true},
		{0x100, nil, false}, {0xFFFF, nil, false},
	}
	p := newPair(t, 8, 4096)
	i := 0
	startFakeServer(t, p, func(req fakeReq) fakeReply {
		if req.op == opAdd {
			return microHandler(req)
		}
		st := statuses[i%len(statuses)]
		i++
		return fakeReply{status: st.status, body: errBody("m")}
	})
	c := newTestConn(t, p, connOpts{})
	cl := &stubClient{c}
	for _, st := range statuses {
		err := cl.CounterFree(counterH{c.NewHandle(0x00010001)})
		var se *StatusError
		if !errors.As(err, &se) || se.Status != st.status || se.Msg != "m" || se.Func != "ml_counter_free" {
			t.Fatalf("status %d: %v", st.status, err)
		}
		if st.want != nil && !errors.Is(err, st.want) {
			t.Errorf("status %d does not match %v", st.status, st.want)
		}
		if errors.Is(err, ErrOutcomeUnknown) != st.unknown {
			t.Errorf("status %d: outcome unknown = %v, want %v", st.status, !st.unknown, st.unknown)
		}
		if errors.Is(err, hqq.ErrProtocol) {
			t.Errorf("status %d matches hqq.ErrProtocol", st.status)
		}
		if v, err := cl.Add(1, 2); err != nil || v != 3 {
			t.Fatalf("session unusable after status %d: %d, %v", st.status, v, err)
		}
	}
}

// rawReply answers every request with frames built from the request.
func rawReply(f func(req fakeReq) [][]byte) func(fakeReq) fakeReply {
	return func(req fakeReq) fakeReply { return fakeReply{raw: f(req)} }
}

func TestProtocolViolationsBreakTheSession(t *testing.T) {
	resp := func(req fakeReq, mod func(h *header)) [][]byte {
		h := header{kind: kindResponse, op: req.op, id: req.id, total: 4}
		mod(&h)
		return [][]byte{frameOf(h, i32Body(1))}
	}
	cases := []struct {
		name string
		f    func(req fakeReq) [][]byte
	}{
		{"wrong id", func(r fakeReq) [][]byte { return resp(r, func(h *header) { h.id++ }) }},
		{"wrong op", func(r fakeReq) [][]byte { return resp(r, func(h *header) { h.op++ }) }},
		{"reserved flag", func(r fakeReq) [][]byte { return resp(r, func(h *header) { h.flags = 0x80 }) }},
		{"F_FAIL on a function without @error", func(r fakeReq) [][]byte { return resp(r, func(h *header) { h.flags = flagFail }) }},
		{"kind", func(r fakeReq) [][]byte { return resp(r, func(h *header) { h.kind = kindCloseAck }) }},
		{"total above the bound", func(r fakeReq) [][]byte {
			return [][]byte{frameOf(header{kind: kindResponse, op: r.op, id: r.id, total: 8}, make([]byte, 8))}
		}},
		{"truncated", func(r fakeReq) [][]byte {
			return [][]byte{frameOf(header{kind: kindResponse, op: r.op, id: r.id}, nil)[:9]}
		}},
		{"reserved status", func(r fakeReq) [][]byte { return resp(r, func(h *header) { h.aux = 12; h.total = 0 }) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := newPair(t, 8, 4096)
			startFakeServer(t, p, rawReply(tc.f))
			c := newTestConn(t, p, connOpts{})
			cl := &stubClient{c}
			f1, err := cl.AddAsync(1, 2)
			if err != nil {
				t.Fatal(err)
			}
			f2, err := cl.AddAsync(3, 4)
			if err != nil {
				t.Fatal(err)
			}
			_, err = f1.Wait()
			var pe *ProtocolError
			if !errors.As(err, &pe) {
				t.Fatalf("Wait = %v, want *ProtocolError", err)
			}
			if !errors.Is(err, hqq.ErrProtocol) || !errors.Is(err, ErrOutcomeUnknown) || pe.Func != "ml_add" {
				t.Errorf("error identities: %v", err)
			}
			// The call that was in flight fails the same way.
			if _, err := f2.Wait(); !errors.As(err, &pe) || !errors.Is(err, ErrOutcomeUnknown) {
				t.Errorf("second Wait = %v", err)
			}
			if c.Err() == nil {
				t.Fatal("session not broken")
			}
			// Later calls are refused without being committed.
			_, err = cl.Add(1, 1)
			var te *TransportError
			if !errors.As(err, &te) || !errors.Is(err, ErrSessionBroken) || te.Unknown || errors.Is(err, ErrOutcomeUnknown) {
				t.Errorf("call on a broken session = %v", err)
			}
			// The runtime closed the link, so the server sees the detach.
			deadline := time.Now().Add(2 * time.Second)
			for p.server.PeerState() != hqq.PeerClosed && time.Now().Before(deadline) {
				time.Sleep(time.Millisecond)
			}
			if st := p.server.PeerState(); st != hqq.PeerClosed {
				t.Errorf("server sees the client as %v, want closed", st)
			}
		})
	}
}

func TestFinalFrames(t *testing.T) {
	for _, status := range []uint32{statusProtocol, statusShuttingDown} {
		p := newPair(t, 8, 4096)
		startFakeServer(t, p, func(req fakeReq) fakeReply { return fakeReply{none: true, final: status} })
		c := newTestConn(t, p, connOpts{})
		cl := &stubClient{c}
		f1, _ := cl.AddAsync(1, 2)
		f2, _ := cl.AddAsync(1, 2)
		_, err := f1.Wait()
		var se *StatusError
		if !errors.As(err, &se) || se.Status != status || !errors.Is(err, ErrOutcomeUnknown) {
			t.Fatalf("status %d: Wait = %v", status, err)
		}
		if _, err := f2.Wait(); !errors.As(err, &se) || se.Status != status || se.Func != "ml_add" {
			t.Fatalf("status %d: second Wait = %v", status, err)
		}
		if _, err := cl.Add(1, 1); !errors.Is(err, ErrSessionBroken) {
			t.Fatalf("status %d: later call = %v", status, err)
		}
	}
}

func TestPeerClosedDeliversPublishedResponses(t *testing.T) {
	p := newPair(t, 8, 4096)
	gate := make(chan struct{})
	startFakeServer(t, p, func(req fakeReq) fakeReply {
		r := microHandler(req)
		r.wait = gate
		if req.id == 3 {
			r.hangup = true
		}
		return r
	})
	c := newTestConn(t, p, connOpts{})
	cl := &stubClient{c}
	var futs []addFuture
	for i := range int32(4) {
		f, err := cl.AddAsync(i, 10)
		if err != nil {
			t.Fatal(err)
		}
		futs = append(futs, f)
	}
	close(gate)
	for i := range 3 {
		if got, err := futs[i].Wait(); err != nil || got != int32(i)+10 {
			t.Fatalf("answered call %d: %d, %v", i, got, err)
		}
	}
	_, err := futs[3].Wait()
	var te *TransportError
	if !errors.As(err, &te) || !errors.Is(err, hqq.ErrPeerClosed) || !errors.Is(err, io.EOF) || !te.Unknown || !errors.Is(err, ErrOutcomeUnknown) {
		t.Fatalf("unanswered call = %v, want TransportError wrapping ErrPeerClosed, unknown outcome", err)
	}
	if !errors.Is(c.Err(), hqq.ErrPeerClosed) {
		t.Errorf("Err = %v", c.Err())
	}
	if _, err := cl.Add(1, 1); !errors.Is(err, ErrSessionBroken) {
		t.Errorf("call after peer closed = %v", err)
	}
}

func TestCallTimeout(t *testing.T) {
	p := newPair(t, 8, 4096)
	gate := make(chan struct{})
	startFakeServer(t, p, func(req fakeReq) fakeReply {
		r := microHandler(req)
		if req.id == 1 {
			r.wait = gate
		}
		return r
	})
	c := newTestConn(t, p, connOpts{callTimeout: 80 * time.Millisecond})
	cl := &stubClient{c}
	f1, err := cl.AddAsync(1, 2)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	_, err = f1.Wait()
	var te *TransportError
	if !errors.As(err, &te) || !errors.Is(err, hqq.ErrTimeout) || !errors.Is(err, os.ErrDeadlineExceeded) || !te.Unknown || !errors.Is(err, ErrOutcomeUnknown) {
		t.Fatalf("Wait = %v", err)
	}
	if d := time.Since(start); d < 60*time.Millisecond || d > 2*time.Second {
		t.Errorf("timeout after %v", d)
	}
	if c.Err() != nil {
		t.Fatalf("a timeout broke the session: %v", c.Err())
	}
	// The call stays pending: waiting again works once the server answers.
	close(gate)
	if got, err := f1.Wait(); err != nil || got != 3 {
		t.Fatalf("second Wait = %d, %v", got, err)
	}
	// An abandoned call (the caller never waits again) does not disturb the
	// pairing of later responses.
	gate2 := make(chan struct{})
	p2 := newPair(t, 8, 4096)
	startFakeServer(t, p2, func(req fakeReq) fakeReply {
		r := microHandler(req)
		if req.id == 1 {
			r.wait = gate2
		}
		return r
	})
	c2 := newTestConn(t, p2, connOpts{callTimeout: 50 * time.Millisecond})
	cl2 := &stubClient{c2}
	if _, err := cl2.Add(1, 1); !errors.Is(err, hqq.ErrTimeout) {
		t.Fatalf("sync call = %v", err)
	}
	close(gate2)
	c2.callTimeout = 5 * time.Second
	if got, err := cl2.Add(5, 6); err != nil || got != 11 {
		t.Fatalf("call after an abandoned one = %d, %v", got, err)
	}
	if err := c2.Sync(); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if len(c2.live) != 0 {
		t.Errorf("Sync left %d calls", len(c2.live))
	}
}

func TestCallTimeoutBetweenFramesResumes(t *testing.T) {
	p := newPair(t, 16, 128)
	startFakeServer(t, p, func(req fakeReq) fakeReply {
		body := make([]byte, 8+4+300)
		le.PutUint64(body, 300)
		le.PutUint32(body[8:], 300)
		copy(body[12:], bytes.Repeat([]byte{7}, 300))
		frames := splitMessage(kindResponse, 112, req.op, req.id, 0, 0, body)
		return fakeReply{raw: frames[:1]}
	})
	c := newTestConn(t, p, connOpts{callTimeout: 60 * time.Millisecond})
	cl := &stubClient{c}
	f, err := cl.XorAsync(bytes.Repeat([]byte{7}, 300), 0, make([]byte, 300))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Wait(); !errors.Is(err, hqq.ErrTimeout) {
		t.Fatalf("Wait = %v, want a timeout after the first frame", err)
	}
	if !c.asm.active || c.asm.got != 112 {
		t.Fatalf("assembly not kept: %+v", c.asm)
	}
	// Send the rest.
	body := make([]byte, 8+4+300)
	le.PutUint64(body, 300)
	le.PutUint32(body[8:], 300)
	copy(body[12:], bytes.Repeat([]byte{7}, 300))
	for _, fr := range splitMessage(kindResponse, 112, opXor, 1, 0, 0, body)[1:] {
		w, err := p.server.ReserveWrite()
		if err != nil {
			t.Fatal(err)
		}
		copy(w.Buffer(), fr)
		if err := w.Commit(len(fr)); err != nil {
			t.Fatal(err)
		}
	}
	out, err := f.Wait()
	if err != nil || len(out) != 300 {
		t.Fatalf("resumed Wait = %d bytes, %v", len(out), err)
	}
}

func TestCloseDrainsUnreadResponses(t *testing.T) {
	p := newPair(t, 16, 4096)
	s := startFakeServer(t, p, microHandler)
	s.closeAux = 2
	c := newTestConn(t, p, connOpts{})
	cl := &stubClient{c}
	var futs []addFuture
	for i := range int32(10) {
		f, err := cl.AddAsync(i, 1)
		if err != nil {
			t.Fatal(err)
		}
		futs = append(futs, f)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("Close = %v", err)
	}
	if n := s.closeAckCount(); n != 1 {
		t.Errorf("CLOSE_ACK count = %d", n)
	}
	for i, f := range futs {
		if _, err := f.Wait(); !errors.Is(err, ErrFutureUsed) {
			t.Errorf("future %d after Close = %v", i, err)
		}
	}
	if err := c.Close(); err != nil {
		t.Errorf("second Close = %v", err)
	}
	if _, err := cl.Add(1, 1); !errors.Is(err, hqq.ErrClosed) {
		t.Errorf("call after Close = %v", err)
	}
}

func TestCloseTimeout(t *testing.T) {
	p := newPair(t, 8, 4096)
	// A server that reads everything and never acknowledges.
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			r, err := p.server.ReserveRead()
			if err != nil {
				return
			}
			_ = r.Release()
		}
	}()
	c := newTestConn(t, p, connOpts{closeTO: 100 * time.Millisecond})
	start := time.Now()
	err := c.Close()
	if !errors.Is(err, hqq.ErrTimeout) {
		t.Fatalf("Close = %v, want a timeout", err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("Close took %v", d)
	}
	_ = p.server.Close()
	<-done
}

func TestCloseAfterPeerLeft(t *testing.T) {
	p := newPair(t, 8, 4096)
	c := newTestConn(t, p, connOpts{})
	_ = p.server.Close()
	if err := c.Close(); !errors.Is(err, hqq.ErrPeerClosed) && !errors.Is(err, hqq.ErrPeerGone) {
		t.Fatalf("Close = %v", err)
	}
}

func TestKillUnblocksWait(t *testing.T) {
	p := newPair(t, 8, 4096)
	gate := make(chan struct{})
	startFakeServer(t, p, func(req fakeReq) fakeReply {
		r := microHandler(req)
		r.wait = gate
		return r
	})
	c := newTestConn(t, p, connOpts{})
	cl := &stubClient{c}
	f, err := cl.AddAsync(1, 2)
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		time.Sleep(60 * time.Millisecond)
		c.Kill()
	}()
	_, err = f.Wait()
	var te *TransportError
	if !errors.As(err, &te) || !errors.Is(err, hqq.ErrClosed) || !te.Unknown {
		t.Fatalf("Wait after Kill = %v", err)
	}
	close(gate)
	if _, err := cl.Add(1, 1); !errors.Is(err, ErrSessionBroken) {
		t.Errorf("call after Kill = %v", err)
	}
	c.Kill() // idempotent
	if err := c.Close(); err != nil {
		t.Errorf("Close after Kill = %v", err)
	}
}

func TestHandleStamps(t *testing.T) {
	p1 := newPair(t, 8, 4096)
	p2 := newPair(t, 8, 4096)
	c1 := newTestConn(t, p1, connOpts{closeTO: 20 * time.Millisecond})
	c2 := newTestConn(t, p2, connOpts{closeTO: 20 * time.Millisecond})
	h := c1.NewHandle(0x00010001)
	if h.ID() != 0x00010001 || h.IsNil() {
		t.Fatalf("handle = %+v", h)
	}
	if err := c1.CheckHandle(h, false); err != nil {
		t.Errorf("own handle: %v", err)
	}
	if err := c2.CheckHandle(h, false); !errors.Is(err, ErrForeignHandle) {
		t.Errorf("foreign handle: %v", err)
	}
	if err := c2.CheckHandle(h, true); !errors.Is(err, ErrForeignHandle) {
		t.Errorf("foreign handle, nullable: %v", err)
	}
	nilH := c1.NewHandle(0)
	if !nilH.IsNil() || nilH != (Handle{}) {
		t.Errorf("NewHandle(0) = %+v", nilH)
	}
	if err := c1.CheckHandle(Handle{}, false); !errors.Is(err, ErrInvalidArg) {
		t.Errorf("nil handle, not nullable: %v", err)
	}
	if err := c1.CheckHandle(Handle{}, true); err != nil {
		t.Errorf("nil handle, nullable: %v", err)
	}
	// A stale stamp: the session that issued the handle was closed and a
	// new Conn took its place.
	_ = c1.Close()
	p3 := newPair(t, 8, 4096)
	c3 := newTestConn(t, p3, connOpts{closeTO: 20 * time.Millisecond})
	if err := c3.CheckHandle(h, false); !errors.Is(err, ErrForeignHandle) {
		t.Errorf("handle of a closed session: %v", err)
	}
	// The stub refuses before it writes anything.
	cl := &stubClient{c3}
	if _, err := cl.CounterAdd(counterH{h}, 1); !errors.Is(err, ErrForeignHandle) {
		t.Errorf("CounterAdd with a foreign handle: %v", err)
	}
	if c3.unansN != 0 || c3.nextID != 0 {
		t.Error("a foreign handle reached the wire")
	}
}

func TestFutureReuse(t *testing.T) {
	p := newPair(t, 8, 4096)
	s := startFakeServer(t, p, microHandler)
	c := newTestConn(t, p, connOpts{})
	cl := &stubClient{c}

	f, err := cl.AddAsync(1, 2)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := f.Wait(); err != nil || got != 3 {
		t.Fatalf("Wait = %d, %v", got, err)
	}
	if _, err := f.Wait(); !errors.Is(err, ErrFutureUsed) {
		t.Errorf("second Wait = %v", err)
	}
	var zero addFuture
	if _, err := (addFuture{c: cl, call: zero.call, id: 0}).Wait(); !errors.Is(err, ErrFutureUsed) {
		t.Errorf("zero future = %v", err)
	}

	// A recycled Call gets a new id; the old future does not match it.
	f1, _ := cl.AddAsync(1, 1)
	old := f1
	if _, err := f1.Wait(); err != nil {
		t.Fatal(err)
	}
	f2, _ := cl.AddAsync(2, 2)
	if f2.call != old.call {
		t.Fatal("test needs the Call to be recycled")
	}
	if f2.id == old.id {
		t.Fatal("recycled Call kept its id")
	}
	if _, err := old.Wait(); !errors.Is(err, ErrFutureUsed) {
		t.Errorf("stale future on a recycled Call = %v", err)
	}
	if got, err := f2.Wait(); err != nil || got != 4 {
		t.Errorf("new future = %d, %v", got, err)
	}

	// Sync discards every unclaimed result.
	fs := make([]addFuture, 5)
	for i := range fs {
		fs[i], _ = cl.AddAsync(int32(i), 0)
	}
	if err := c.Sync(); err != nil {
		t.Fatalf("Sync = %v", err)
	}
	for i, f := range fs {
		if _, err := f.Wait(); !errors.Is(err, ErrFutureUsed) {
			t.Errorf("future %d after Sync = %v", i, err)
		}
	}
	if c.unansN != 0 || len(c.live) != 0 {
		t.Errorf("Sync left unans=%d live=%d", c.unansN, len(c.live))
	}
	// None of the refused Waits read from the link: the session is still in
	// step.
	if got, err := cl.Add(40, 2); err != nil || got != 42 {
		t.Fatalf("Add after refused Waits = %d, %v", got, err)
	}
	if n := s.requestCount(); n != 9 {
		t.Errorf("server saw %d requests", n)
	}
	// A Call of another Conn.
	p2 := newPair(t, 8, 4096)
	startFakeServer(t, p2, microHandler)
	c2 := newTestConn(t, p2, connOpts{})
	g, _ := (&stubClient{c2}).AddAsync(1, 1)
	if _, err := c.Wait(g.call, g.id); !errors.Is(err, ErrFutureUsed) {
		t.Errorf("Wait of a foreign Call = %v", err)
	}
	if _, err := c.Wait(nil, 0); !errors.Is(err, ErrFutureUsed) {
		t.Errorf("Wait(nil) = %v", err)
	}
}

func TestReentrancy(t *testing.T) {
	p := newPair(t, 8, 4096)
	startFakeServer(t, p, microHandler)
	c := newTestConn(t, p, connOpts{})
	cl := &stubClient{c}

	e, err := c.BeginRequest(opAdd, 8, 4)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.BeginRequest(opAdd, 8, 4); !errors.Is(err, ErrConcurrentUse) {
		t.Errorf("second BeginRequest = %v", err)
	}
	if err := c.Close(); !errors.Is(err, ErrConcurrentUse) {
		t.Errorf("Close with an open Enc = %v", err)
	}
	if err := c.Sync(); !errors.Is(err, ErrConcurrentUse) {
		t.Errorf("Sync with an open Enc = %v", err)
	}
	// Abort consumes no id: the server checks that ids stay consecutive.
	c.Abort(e)
	if got, err := cl.Add(1, 2); err != nil || got != 3 {
		t.Fatalf("Add after Abort = %d, %v", got, err)
	}

	f, _ := cl.AddAsync(5, 5)
	g, _ := cl.AddAsync(6, 6)
	d, err := c.Wait(f.call, f.id)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Wait(g.call, g.id); !errors.Is(err, ErrConcurrentUse) {
		t.Errorf("Wait with an unreleased Dec = %v", err)
	}
	if _, err := c.BeginRequest(opAdd, 8, 4); !errors.Is(err, ErrConcurrentUse) {
		t.Errorf("BeginRequest with an unreleased Dec = %v", err)
	}
	if err := c.Sync(); !errors.Is(err, ErrConcurrentUse) {
		t.Errorf("Sync with an unreleased Dec = %v", err)
	}
	if err := c.Close(); !errors.Is(err, ErrConcurrentUse) {
		t.Errorf("Close with an unreleased Dec = %v", err)
	}
	if len(d.Body) != 4 || hqqLE.Uint32(d.Body) != 10 {
		t.Errorf("Dec body = %x", d.Body)
	}
	d.Release()
	d.Release() // idempotent
	if got, err := g.Wait(); err != nil || got != 12 {
		t.Errorf("Wait after Release = %d, %v", got, err)
	}
	// Submit of something that is not the open Enc.
	if _, err := c.Submit(e); !errors.Is(err, ErrInvalidArg) {
		t.Errorf("Submit without BeginRequest = %v", err)
	}
}

// TestDecCopyReleaseIsHarmless: a copy of a Dec released after a later Wait
// must not release the newer response.
func TestDecCopyReleaseIsHarmless(t *testing.T) {
	p := newPair(t, 8, 4096)
	startFakeServer(t, p, microHandler)
	c := newTestConn(t, p, connOpts{})
	cl := &stubClient{c}
	f, _ := cl.AddAsync(1, 1)
	d1, err := c.Wait(f.call, f.id)
	if err != nil {
		t.Fatal(err)
	}
	d1copy := d1
	d1.Release()
	g, _ := cl.AddAsync(2, 2)
	d2, err := c.Wait(g.call, g.id)
	if err != nil {
		t.Fatal(err)
	}
	d1copy.Release()
	if !c.rxOpen || !c.rxHeld {
		t.Fatal("the stale copy released the live response")
	}
	if hqqLE.Uint32(d2.Body) != 4 {
		t.Fatal("body changed")
	}
	d2.Release()
	if c.rxOpen || c.rxHeld {
		t.Fatal("not released")
	}
}

func TestMalformedBreaksAndCloses(t *testing.T) {
	p := newPair(t, 8, 4096)
	startFakeServer(t, p, func(req fakeReq) fakeReply { return fakeReply{body: []byte{1, 2, 3}} })
	c := newTestConn(t, p, connOpts{})
	cl := &stubClient{c}
	_, err := cl.Add(1, 2)
	var pe *ProtocolError
	if !errors.As(err, &pe) || pe.Func != "ml_add" || !pe.Unknown {
		t.Fatalf("Add = %v", err)
	}
	if c.Err() == nil {
		t.Fatal("not broken")
	}
	// The Dec was released by the stub's defer, so the link is closed now.
	if st := p.server.PeerState(); st != hqq.PeerClosed {
		deadline := time.Now().Add(time.Second)
		for p.server.PeerState() != hqq.PeerClosed && time.Now().Before(deadline) {
			time.Sleep(time.Millisecond)
		}
		if st = p.server.PeerState(); st != hqq.PeerClosed {
			t.Errorf("peer state %v", st)
		}
	}
}

// TestUnexpectedPacketBreaksTheSession drives a real ErrUnexpectedPacket: an
// Advanced endpoint on the other side writes its negotiation frames into the
// ring that the client reads as Standard frames (5.4).
func TestUnexpectedPacketBreaksTheSession(t *testing.T) {
	mem := hqq.AlignedBuffer(hqq.SizeStandardLink(8, 4096))
	adv, err := hqq.NewAdvancedLink(mem, 8, 4096, hqq.AdvancedOptions{Link: testLinkOpts(false)})
	if err != nil {
		t.Fatal(err)
	}
	client, err := hqq.NewStandardLink(mem, 8, 4096, testLinkOpts(true))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = client.Close()
		_ = adv.Close()
	})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = adv.Negotiate(ctx, hqq.FeatureRequestResponse) }()

	lim := Limits{BufferCount: 8, BufferSize: 4096, MaxInFlight: 8, MaxMessage: 1 << 20, MaxHandles: 1}
	c := newConn(client, client.Close, microIface, config{closeTimeout: time.Second}, lim)
	cl := &stubClient{c}
	f, err := cl.AddAsync(1, 2)
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.Wait()
	var te *TransportError
	if !errors.As(err, &te) || !errors.Is(err, hqq.ErrUnexpectedPacket) || !te.Unknown {
		t.Fatalf("Wait = %v, want TransportError wrapping ErrUnexpectedPacket", err)
	}
	if _, err := cl.Add(1, 1); !errors.Is(err, ErrSessionBroken) {
		t.Errorf("call after a dropped frame = %v", err)
	}
}

// TestLinkErrorsBreakTheSession: every ReserveRead error other than a timeout
// marks the session broken (9.3), in particular the two that leave the link
// usable (5.4).
func TestLinkErrorsBreakTheSession(t *testing.T) {
	for _, linkErr := range []error{hqq.ErrInvalidSize, hqq.ErrUnexpectedPacket, hqq.ErrPeerGone, hqq.ErrPeerClosed, hqq.ErrClosed} {
		c := bareConn(8, 4096, 1<<20)
		c.pushCall(opAdd, 4)
		err := c.readError(linkErr)
		if !c.rxDead || c.broken == nil || err != c.broken {
			t.Errorf("%v: rxDead=%v broken=%v", linkErr, c.rxDead, c.broken)
		}
		if !errors.Is(c.broken, linkErr) {
			t.Errorf("%v: session error %v does not wrap it", linkErr, c.broken)
		}
	}
	c := bareConn(8, 4096, 1<<20)
	if err := c.readError(hqq.ErrTimeout); c.rxDead || err != hqq.ErrTimeout {
		t.Errorf("timeout broke the session: %v", c.rxDead)
	}
}

func TestAbortPublishesTombstoneOnly(t *testing.T) {
	p := newPair(t, 4, 256)
	s := startFakeServer(t, p, microHandler)
	c := newTestConn(t, p, connOpts{})
	cl := &stubClient{c}
	for range 10 {
		e, err := c.BeginRequest(opAdd, 8, 4)
		if err != nil {
			t.Fatal(err)
		}
		c.Abort(e)
		if _, err := cl.Add(1, 1); err != nil {
			t.Fatal(err)
		}
	}
	if n := s.requestCount(); n != 10 {
		t.Errorf("%d requests", n)
	}
}

func TestErrorTypes(t *testing.T) {
	var _ error = (*TransportError)(nil)
	te := &TransportError{Func: "f", Err: hqq.ErrPeerGone, Unknown: true, svc: "svc"}
	if !errors.Is(te, hqq.ErrPeerGone) || !errors.Is(te, ErrOutcomeUnknown) || errors.Is(te, ErrSessionBroken) {
		t.Error("TransportError identities")
	}
	if got := te.Error(); got != "svc: f: transport error: hqq: peer is gone (no heartbeat) (outcome unknown)" {
		t.Errorf("text %q", got)
	}
	if errors.Is(&TransportError{Err: hqq.ErrPeerGone}, ErrOutcomeUnknown) {
		t.Error("known outcome matches ErrOutcomeUnknown")
	}
	pe := &ProtocolError{Func: "f", Reason: "r"}
	if !errors.Is(pe, hqq.ErrProtocol) || errors.Is(pe, ErrOutcomeUnknown) {
		t.Error("ProtocolError identities")
	}
	oe := &OpenError{Code: 3, Msg: "busy"}
	if !errors.Is(oe, ErrLimit) || !errors.Is(&OpenError{Code: 2}, ErrLimit) || errors.Is(&OpenError{Code: 1}, ErrLimit) || errors.Is(&OpenError{Code: 4}, ErrLimit) {
		t.Error("OpenError identities")
	}
	if !errors.Is(&MismatchError{}, ErrInterfaceMismatch) {
		t.Error("MismatchError identity")
	}
	le := &LibError{Func: "f", Code: 3}
	if errors.Is(le, ErrInternal) || errors.Is(le, ErrOutcomeUnknown) {
		t.Error("LibError matches nothing by default")
	}
	if got := le.Error(); got != "f: library error 3" {
		t.Errorf("LibError text %q", got)
	}
}
