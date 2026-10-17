package rt

import (
	"bytes"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	hqq "gosuda.org/hqq"
)

// namedPair is a session link in named shared memory. Closing the client end
// unmaps its segment, so a use after Close faults, which an in-process
// hqq.AlignedBuffer link would hide.
type namedPair struct {
	server *hqq.NamedStandardLink
	client *hqq.NamedStandardLink
	count  int
	size   int
}

func newNamedPair(tb testing.TB, count, size int) *namedPair {
	tb.Helper()
	opts := hqq.LinkOptions{HeartbeatInterval: 10 * time.Millisecond, PeerTimeout: 300 * time.Millisecond, AttachTimeout: 3 * time.Second}
	name := "bgk" + randHex(5)
	server, err := hqq.CreateStandardLink(name, count, size, 0o600, opts)
	if err != nil {
		tb.Fatalf("CreateStandardLink: %v", err)
	}
	client, err := hqq.OpenStandardLink(name, count, size, opts)
	if err != nil {
		_ = server.Close()
		tb.Fatalf("OpenStandardLink: %v", err)
	}
	p := &namedPair{server: server, client: client, count: count, size: size}
	tb.Cleanup(func() {
		_ = client.Close()
		_ = server.Close()
	})
	return p
}

// killConn builds a Conn over the client end whose link close is counted.
func (p *namedPair) conn(closes *atomic.Int32) *Conn {
	lim := Limits{BufferCount: p.count, BufferSize: p.size, MaxInFlight: p.count, MaxMessage: 1 << 20, MaxHandles: 1}
	closeFn := func() error {
		closes.Add(1)
		return p.client.Close()
	}
	return newConn(p.client.StandardLink, closeFn, microIface, config{closeTimeout: 100 * time.Millisecond}, lim)
}

func (p *namedPair) serve(tb testing.TB, handle func(fakeReq) fakeReply) *fakeServer {
	tb.Helper()
	s := startFakeServerOn(tb, p.server.StandardLink, p.size, handle)
	tb.Cleanup(s.stop)
	return s
}

func eventually(tb testing.TB, what string, cond func() bool) {
	tb.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			tb.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

// TestKillWaitsForHeldResponse: Kill while the owner decodes a response in
// place must not unmap the slot. With the old Kill (close the link at once)
// the reads of d.Body below fault.
func TestKillWaitsForHeldResponse(t *testing.T) {
	p := newNamedPair(t, 8, 4096)
	p.serve(t, microHandler)
	var closes atomic.Int32
	c := p.conn(&closes)
	cl := &stubClient{c}

	src := bytes.Repeat([]byte{0x11}, 3000)
	f, err := cl.XorAsync(src, 0x22, make([]byte, len(src)))
	if err != nil {
		t.Fatal(err)
	}
	d, err := c.Wait(f.call, f.id)
	if err != nil {
		t.Fatal(err)
	}
	if !c.rxHeld {
		t.Fatal("test needs the response held in place")
	}
	killed := make(chan struct{})
	go func() {
		c.Kill()
		close(killed)
	}()
	select {
	case <-killed:
	case <-time.After(5 * time.Second):
		t.Fatal("Kill blocked on the owner's reservation")
	}
	time.Sleep(20 * time.Millisecond)
	if n := closes.Load(); n != 0 {
		t.Fatalf("link closed %d times while a response was held", n)
	}
	// The slot is still mapped and intact.
	if len(d.Body) != 12+len(src) {
		t.Fatalf("body of %d bytes", len(d.Body))
	}
	for i, b := range d.Body[12:] {
		if b != 0x11^0x22 {
			t.Fatalf("body byte %d = %#x", i, b)
		}
	}
	d.Release()
	if n := closes.Load(); n != 1 {
		t.Fatalf("link closed %d times after Release, want 1", n)
	}
	eventually(t, "the server to see the close", func() bool { return p.server.PeerState() == hqq.PeerClosed })

	// The session is dead for every later call, with a known outcome.
	_, err = cl.Add(1, 2)
	if !errors.Is(err, ErrSessionBroken) || errors.Is(err, ErrOutcomeUnknown) {
		t.Errorf("call after Kill = %v", err)
	}
	if !errors.Is(c.Err(), hqq.ErrClosed) {
		t.Errorf("Err = %v", c.Err())
	}
	c.Kill()
	if err := c.Close(); err != nil && !errors.Is(err, hqq.ErrClosed) {
		t.Errorf("Close after Kill = %v", err)
	}
	if n := closes.Load(); n != 1 {
		t.Errorf("link closed %d times in total", n)
	}
}

// TestKillWaitsForOpenRequest: the same for the request slot between
// BeginRequest and Submit. The request is committed, and the call fails with
// an unknown outcome.
func TestKillWaitsForOpenRequest(t *testing.T) {
	p := newNamedPair(t, 8, 4096)
	p.serve(t, microHandler)
	var closes atomic.Int32
	c := p.conn(&closes)

	e, err := c.BeginRequest(opAdd, 8, 4)
	if err != nil {
		t.Fatal(err)
	}
	c.Kill()
	if n := closes.Load(); n != 0 {
		t.Fatalf("link closed %d times while a request was open", n)
	}
	body := e.Body()
	hqqLE.PutUint32(body[0:], 40)
	hqqLE.PutUint32(body[4:], 2)
	call, err := c.Submit(e)
	if err != nil {
		t.Fatalf("Submit = %v", err)
	}
	if n := closes.Load(); n != 1 {
		t.Fatalf("link closed %d times after Submit, want 1", n)
	}
	_, err = c.Wait(call, call.ID())
	var te *TransportError
	if !errors.As(err, &te) || !errors.Is(err, hqq.ErrClosed) || !te.Unknown || !errors.Is(err, ErrOutcomeUnknown) {
		t.Fatalf("Wait = %v, want unknown ErrClosed", err)
	}
	if _, err := c.BeginRequest(opAdd, 8, 4); !errors.Is(err, ErrSessionBroken) {
		t.Errorf("BeginRequest after Kill = %v", err)
	}
}

// TestKillBlockedOwner: Kill wakes an owner that is blocked in a ring wait,
// both in Wait and in BeginRequest (full request ring), and the link is closed
// afterwards.
func TestKillBlockedOwner(t *testing.T) {
	t.Run("Wait", func(t *testing.T) {
		p := newNamedPair(t, 8, 4096)
		gate := make(chan struct{})
		defer close(gate)
		p.serve(t, func(req fakeReq) fakeReply {
			r := microHandler(req)
			r.wait = gate
			return r
		})
		var closes atomic.Int32
		c := p.conn(&closes)
		f, err := (&stubClient{c}).AddAsync(1, 2)
		if err != nil {
			t.Fatal(err)
		}
		go func() {
			time.Sleep(50 * time.Millisecond)
			c.Kill()
		}()
		start := time.Now()
		_, err = f.Wait()
		var te *TransportError
		if !errors.As(err, &te) || !errors.Is(err, hqq.ErrClosed) || !te.Unknown {
			t.Fatalf("Wait = %v", err)
		}
		if d := time.Since(start); d > 3*time.Second {
			t.Errorf("Wait returned after %v", d)
		}
		eventually(t, "the link to close", func() bool { return closes.Load() == 1 })
	})
	t.Run("BeginRequest on a full ring", func(t *testing.T) {
		p := newNamedPair(t, 2, 4096)
		// Nobody reads the request ring: the third reservation blocks.
		var closes atomic.Int32
		c := p.conn(&closes)
		c.lim.MaxInFlight, c.lim.BufferCount = 64, 64 // flow control must not stop the third call
		c.unans = make([]*Call, 64)
		cl := &stubClient{c}
		for range 2 {
			if _, err := cl.AddAsync(1, 1); err != nil {
				t.Fatal(err)
			}
		}
		go func() {
			time.Sleep(50 * time.Millisecond)
			c.Kill()
		}()
		_, err := cl.AddAsync(1, 1)
		if !errors.Is(err, hqq.ErrClosed) {
			t.Fatalf("AddAsync = %v", err)
		}
		eventually(t, "the link to close", func() bool { return closes.Load() == 1 })
	})
}

// TestKillStress runs pipelined calls with large in-place responses while
// another goroutine kills the session at a random moment. Every call must
// either return its exact result or a session error, and nothing may fault.
func TestKillStress(t *testing.T) {
	iterations := 1500
	if testing.Short() {
		iterations = 100
	}
	handler := func(req fakeReq) fakeReply { return microHandler(req) }
	var results, failures atomic.Int64
	for i := range iterations {
		stressOnce(t, uint64(i)*2654435761+1, handler, &results, &failures)
		if t.Failed() {
			return
		}
	}
	t.Logf("%d calls returned results, %d failed with a session error", results.Load(), failures.Load())
	if results.Load() == 0 || failures.Load() == 0 {
		t.Errorf("the schedule never mixed results and failures: results=%d failures=%d", results.Load(), failures.Load())
	}
}

func stressOnce(t *testing.T, seed uint64, handler func(fakeReq) fakeReply, results, failures *atomic.Int64) {
	t.Helper()
	p := newNamedPair(t, 16, 4096)
	srv := startFakeServerOn(t, p.server.StandardLink, p.size, handler)
	var closes atomic.Int32
	c := p.conn(&closes)
	cl := &stubClient{c}

	next := func() uint64 {
		seed ^= seed << 13
		seed ^= seed >> 7
		seed ^= seed << 17
		return seed
	}
	delay := time.Duration(next()%400) * time.Microsecond
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		time.Sleep(delay)
		c.Kill()
	}()

	src := make([]byte, 3500)
	for i := range src {
		src[i] = byte(i)
	}
	okErr := func(err error) bool {
		return errors.Is(err, ErrSessionBroken) || errors.Is(err, hqq.ErrClosed) || errors.Is(err, ErrOutcomeUnknown)
	}
loop:
	for range 40 {
		var futs [4]xorFuture
		n := 0
		for ; n < len(futs); n++ {
			f, err := cl.XorAsync(src, byte(n), make([]byte, len(src)))
			if err != nil {
				if !okErr(err) {
					t.Errorf("XorAsync: unexpected error %v", err)
				}
				failures.Add(1)
				break
			}
			futs[n] = f
		}
		for i := range n {
			out, err := futs[i].Wait()
			if err != nil {
				if !okErr(err) {
					t.Errorf("Wait: unexpected error %v", err)
				}
				failures.Add(1)
				continue
			}
			if len(out) != len(src) {
				t.Errorf("result of %d bytes", len(out))
			}
			for j := range out {
				if out[j] != src[j]^byte(i) {
					t.Errorf("byte %d = %#x", j, out[j])
					break
				}
			}
			results.Add(1)
		}
		if n < len(futs) || c.Err() != nil {
			break loop
		}
	}
	wg.Wait()
	if err := c.Close(); err != nil && !errors.Is(err, hqq.ErrClosed) && !errors.Is(err, hqq.ErrPeerClosed) {
		t.Errorf("Close = %v", err)
	}
	if n := closes.Load(); n != 1 {
		t.Errorf("link closed %d times", n)
	}
	srv.stop()
}
