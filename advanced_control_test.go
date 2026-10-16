package hqq

import (
	"bytes"
	"context"
	"errors"
	"runtime"
	"sync"
	"testing"
	"time"

	"gosuda.org/hqq/internal/protocol"
)

func streamCount(l *AdvancedLink) int {
	l.controlMu.Lock()
	defer l.controlMu.Unlock()
	return len(l.streams)
}

func TestNegotiationFailedIsTerminal(t *testing.T) {
	link := newTestAdvancedLink(t, 8, 32, AdvancedOptions{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := link.NegotiateProtocol(ctx, currentProtocolVersion, FeatureMessages); err == nil {
		t.Fatal("NegotiateProtocol with a canceled context succeeded")
	}
	if got := link.ProtocolState(); got != ProtocolStateFailed {
		t.Fatalf("state = %v, want ProtocolStateFailed", got)
	}

	live := context.Background()
	if _, err := link.NegotiateProtocol(live, currentProtocolVersion, FeatureMessages); !errors.Is(err, ErrNegotiationFailed) || errors.Is(err, ErrAlreadyNegotiated) {
		t.Fatalf("NegotiateProtocol after failure = %v, want ErrNegotiationFailed", err)
	}
	if _, err := link.WaitForNegotiation(live); !errors.Is(err, ErrNegotiationFailed) || errors.Is(err, ErrAlreadyNegotiated) {
		t.Fatalf("WaitForNegotiation after failure = %v, want ErrNegotiationFailed", err)
	}
	if err := link.Negotiate(live, 0); !errors.Is(err, ErrNegotiationFailed) || errors.Is(err, ErrAlreadyNegotiated) {
		t.Fatalf("Negotiate after failure = %v, want ErrNegotiationFailed", err)
	}
	if got := link.ProtocolState(); got != ProtocolStateFailed {
		t.Fatalf("state after retries = %v, want it to stay ProtocolStateFailed", got)
	}
}

func TestNegotiationRepeatIsAlreadyNegotiated(t *testing.T) {
	// In progress: a second attempt is refused with ErrAlreadyNegotiated.
	link := newTestAdvancedLink(t, 8, 32, AdvancedOptions{})
	ctx, cancel := context.WithCancel(context.Background())
	first := make(chan error, 1)
	go func() {
		_, err := link.NegotiateProtocol(ctx, currentProtocolVersion, FeatureMessages)
		first <- err
	}()
	for link.ProtocolState() != ProtocolStateNegotiating {
		runtime.Gosched()
	}
	if _, err := link.NegotiateProtocol(context.Background(), currentProtocolVersion, 0); !errors.Is(err, ErrAlreadyNegotiated) {
		t.Fatalf("NegotiateProtocol while in progress = %v, want ErrAlreadyNegotiated", err)
	}
	if _, err := link.WaitForNegotiation(context.Background()); !errors.Is(err, ErrAlreadyNegotiated) {
		t.Fatalf("WaitForNegotiation while in progress = %v, want ErrAlreadyNegotiated", err)
	}
	if err := link.Negotiate(context.Background(), 0); !errors.Is(err, ErrAlreadyNegotiated) {
		t.Fatalf("Negotiate while in progress = %v, want ErrAlreadyNegotiated", err)
	}
	cancel()
	if err := waitErr(t, first, 2*time.Second); err == nil {
		t.Fatal("canceled NegotiateProtocol succeeded")
	}

	// After success.
	primary, secondary := newNegotiatedPair(t, 8, 32, AdvancedOptions{}, AdvancedOptions{})
	for _, l := range []*AdvancedLink{primary, secondary} {
		if _, err := l.NegotiateProtocol(context.Background(), currentProtocolVersion, 0); !errors.Is(err, ErrAlreadyNegotiated) {
			t.Fatalf("NegotiateProtocol after success = %v, want ErrAlreadyNegotiated", err)
		}
		if _, err := l.WaitForNegotiation(context.Background()); !errors.Is(err, ErrAlreadyNegotiated) {
			t.Fatalf("WaitForNegotiation after success = %v, want ErrAlreadyNegotiated", err)
		}
		if err := l.Negotiate(context.Background(), 0); !errors.Is(err, ErrAlreadyNegotiated) {
			t.Fatalf("Negotiate after success = %v, want ErrAlreadyNegotiated", err)
		}
	}
}

func TestCloseStream(t *testing.T) {
	primary, secondary := newNegotiatedPair(t, 8, 32, AdvancedOptions{}, AdvancedOptions{})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := secondary.ListenStreams(ctx); err != nil {
		t.Fatal(err)
	}
	if err := primary.ListenStreams(ctx); err != nil {
		t.Fatal(err)
	}
	mux := NewMux()
	mux.HandleFunc(1, func(_ context.Context, w *ResponseWriter, req *Request) { w.Write(req.Payload()) })
	for _, l := range []*AdvancedLink{primary, secondary} {
		go l.Serve(ctx, mux)
	}

	// settle makes sure every packet sent by from before the call was
	// processed by the other side's receive path: a response is sent after,
	// and received after, everything that preceded the request.
	settle := func(from *AdvancedLink) {
		t.Helper()
		resp, err := from.Call(ctx, 1, []byte("sync"))
		if err != nil {
			t.Fatalf("sync call: %v", err)
		}
		resp.Release()
	}
	// checkUnusable verifies that st fails every operation with want.
	checkUnusable := func(st *Stream, want error) {
		t.Helper()
		if _, err := st.Call(ctx, 1, nil); !errors.Is(err, want) {
			t.Fatalf("Call on stream %d = %v, want %v", st.ID(), err, want)
		}
		if err := st.Send(ctx, []byte("x")); !errors.Is(err, want) {
			t.Fatalf("Send on stream %d = %v, want %v", st.ID(), err, want)
		}
		if m, err := st.Recv(ctx); !errors.Is(err, want) {
			t.Fatalf("Recv on stream %d = %v, want %v", st.ID(), err, want)
		} else if m != nil {
			t.Fatalf("Recv on stream %d returned a message with error %v", st.ID(), err)
		}
	}

	// The opener closes.
	opened, err := primary.OpenStream(ctx)
	if err != nil {
		t.Fatal(err)
	}
	accepted, err := secondary.AcceptStream(ctx)
	if err != nil || accepted.ID() != opened.ID() {
		t.Fatalf("AcceptStream = %v, %v; want stream %d", accepted, err, opened.ID())
	}
	if streamCount(primary) != 1 || streamCount(secondary) != 1 {
		t.Fatalf("open stream registered %d/%d times, want 1/1", streamCount(primary), streamCount(secondary))
	}
	resp, err := opened.Call(ctx, 1, []byte("hi"))
	if err != nil || string(resp.Bytes()) != "hi" {
		t.Fatalf("Call on open stream = %v, %v", resp, err)
	}
	resp.Release()
	if err := opened.Close(); err != nil {
		t.Fatal(err)
	}
	if n := streamCount(primary); n != 0 {
		t.Fatalf("opener registry after Close = %d, want 0", n)
	}
	settle(primary)
	if n := streamCount(secondary); n != 0 {
		t.Fatalf("peer registry after OpConnClose = %d, want 0", n)
	}
	if err := opened.Close(); !errors.Is(err, ErrClosed) {
		t.Fatalf("second Close = %v, want ErrClosed", err)
	}
	checkUnusable(opened, ErrClosed)
	checkUnusable(accepted, ErrPeerClosed)
	if err := accepted.Close(); err != nil {
		t.Fatalf("Close of a stream the peer closed = %v, want nil", err)
	}
	if err := accepted.Close(); !errors.Is(err, ErrClosed) {
		t.Fatalf("second Close of a stream the peer closed = %v, want ErrClosed", err)
	}
	checkUnusable(accepted, ErrClosed)

	// The acceptor closes.
	opened2, err := primary.OpenStream(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if opened2.ID() == opened.ID() {
		t.Fatalf("stream ID %d was reused", opened.ID())
	}
	accepted2, err := secondary.AcceptStream(ctx)
	if err != nil || accepted2.ID() != opened2.ID() {
		t.Fatalf("AcceptStream = %v, %v; want stream %d", accepted2, err, opened2.ID())
	}
	if err := accepted2.Close(); err != nil {
		t.Fatal(err)
	}
	if n := streamCount(secondary); n != 0 {
		t.Fatalf("acceptor registry after Close = %d, want 0", n)
	}
	settle(secondary)
	if n := streamCount(primary); n != 0 {
		t.Fatalf("opener registry after the peer's OpConnClose = %d, want 0", n)
	}
	checkUnusable(accepted2, ErrClosed)
	checkUnusable(opened2, ErrPeerClosed)

	// The link-level stream is always open and cannot be closed.
	for _, l := range []*AdvancedLink{primary, secondary} {
		for range 2 {
			if err := l.Stream().Close(); !errors.Is(err, ErrInvalidOp) {
				t.Fatalf("Close of stream 0 = %v, want ErrInvalidOp", err)
			}
		}
	}
	// Link-level traffic on ID 0 is unaffected.
	settle(primary)
	settle(secondary)
}

func TestCloseStreamBeforeNegotiation(t *testing.T) {
	link := newTestAdvancedLink(t, 8, 32, AdvancedOptions{})
	if err := newStream(link, 2).Close(); !errors.Is(err, ErrNotNegotiated) {
		t.Fatalf("Close before negotiation = %v, want ErrNotNegotiated", err)
	}
}

// TestAcceptedStreamIsVisibleAfterAccept: data the acceptor sends right after
// AcceptStream must not overtake the OpConnAccept on the wire.
func TestAcceptedStreamIsVisibleAfterAccept(t *testing.T) {
	primary, secondary := newNegotiatedPair(t, 8, 32, AdvancedOptions{}, AdvancedOptions{})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := secondary.ListenStreams(ctx); err != nil {
		t.Fatal(err)
	}

	type openResult struct {
		st  *Stream
		err error
	}
	opened := make(chan openResult, 1)
	go func() {
		st, err := primary.OpenStream(ctx)
		opened <- openResult{st, err}
	}()
	accepted, err := secondary.AcceptStream(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := accepted.Send(ctx, []byte("first")); err != nil {
		t.Fatal(err)
	}
	res := <-opened
	if res.err != nil {
		t.Fatal(res.err)
	}
	message, err := res.st.Recv(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer message.Release()
	// The dispatcher processed the accept before the data frame it
	// delivered: no OpenStream is pending any more.
	primary.controlMu.Lock()
	pending := len(primary.pendingConnections)
	primary.controlMu.Unlock()
	if message.StreamID() != accepted.ID() || pending != 0 || string(message.Bytes()) != "first" {
		t.Fatalf("data on stream %d arrived with %d pending OpenStream calls (message %q on %d)", accepted.ID(), pending, message.Bytes(), message.StreamID())
	}
	if res.st.ID() != accepted.ID() {
		t.Fatalf("OpenStream = %d, want %d", res.st.ID(), accepted.ID())
	}
}

// TestFullTxRingDoesNotStopReceiveDraining: the peer never drains our replies,
// so the tx ring of the flooded endpoint fills up; its receive path must keep
// draining the flood regardless.
func TestFullTxRingDoesNotStopReceiveDraining(t *testing.T) {
	const bufferCount = 4
	flooder, flooded := newNegotiatedPair(t, bufferCount, 32, AdvancedOptions{}, AdvancedOptions{})
	// flooded is not listening: it refuses every OpConnCreate.
	if err := flooded.startDispatcher(); err != nil {
		t.Fatal(err)
	}

	const creates = 64
	ids := make(map[uint64]bool, creates)
	for range creates {
		id := flooder.nextConnectionID()
		ids[id] = true
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		err := flooder.sendPacket(ctx, protocol.NewPacket(protocol.OpConnCreate, id))
		cancel()
		if err != nil {
			t.Fatalf("flooding stalled: the flooded endpoint stopped draining its rx ring: %v", err)
		}
	}

	// The refusals the flooded endpoint could not queue were dropped and
	// counted: its queue and tx ring hold fewer than creates replies.
	const maxReplies = bufferCount + 1 + minControlQueue
	deadline := time.Now().Add(5 * time.Second)
	for flooded.Stats().DroppedMessages < creates-maxReplies {
		if time.Now().After(deadline) {
			t.Fatalf("DroppedMessages = %d, want at least %d", flooded.Stats().DroppedMessages, creates-maxReplies)
		}
		runtime.Gosched()
	}

	// Everything that was queued is delivered once the flooder drains.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for range bufferCount {
		p, err := flooder.recvPacket(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if p.Op() != protocol.OpError || ErrorCode(p.Operand(1)) != ErrCodeConnNotFound || !ids[p.Operand(0)] {
			t.Fatalf("reply = op %v operands %d,%d; want a ConnNotFound refusal of a flooded ID", p.Op(), p.Operand(0), p.Operand(1))
		}
	}
}

// TestStreamAndCallFloodBothDirections: both endpoints flood each other with
// OpenStream and multi-chunk Call traffic over tiny rings. Before the control
// sender existed, the two receive paths blocked on each other's full tx rings.
func TestStreamAndCallFloodBothDirections(t *testing.T) {
	const (
		bufferCount = 4
		bufferSize  = 32
		workers     = 12 // per endpoint; outstanding creates stay below the control queue (minControlQueue)
		rounds      = 40
	)
	// The incoming queue holds every request that can be outstanding, so
	// OverflowBlock backpressure (an application-level wait that is
	// independent of the control replies under test) never engages.
	opts := AdvancedOptions{IncomingQueueSize: 4 * workers}
	a, b := newNegotiatedPair(t, bufferCount, bufferSize, opts, opts)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	mux := NewMux()
	mux.HandleFunc(1, func(_ context.Context, w *ResponseWriter, req *Request) { w.Write(req.Payload()) })
	var serving sync.WaitGroup
	for _, l := range []*AdvancedLink{a, b} {
		if err := l.ListenStreams(ctx); err != nil {
			t.Fatal(err)
		}
		serving.Add(2)
		go func() {
			defer serving.Done()
			l.Serve(ctx, mux)
		}()
		go func() {
			defer serving.Done()
			for {
				if _, err := l.AcceptStream(ctx); err != nil {
					return
				}
			}
		}()
	}

	payload := bytes.Repeat([]byte("p"), 100) // four chunks
	var wg sync.WaitGroup
	for _, l := range []*AdvancedLink{a, b} {
		for w := range workers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for r := range rounds {
					st, err := l.OpenStream(ctx)
					if err != nil {
						t.Errorf("%v worker %d round %d: OpenStream: %v", l.Mode(), w, r, err)
						return
					}
					resp, err := st.Call(ctx, 1, payload)
					if err != nil {
						t.Errorf("%v worker %d round %d: stream Call: %v", l.Mode(), w, r, err)
						return
					}
					ok := bytes.Equal(resp.Bytes(), payload)
					resp.Release()
					if !ok {
						t.Errorf("%v worker %d round %d: stream Call: wrong echo", l.Mode(), w, r)
						return
					}
					resp, err = l.Call(ctx, 1, payload)
					if err != nil {
						t.Errorf("%v worker %d round %d: Call: %v", l.Mode(), w, r, err)
						return
					}
					resp.Release()
					if err := st.Close(); err != nil {
						t.Errorf("%v worker %d round %d: Close: %v", l.Mode(), w, r, err)
						return
					}
				}
			}()
		}
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatalf("flood made no progress: %v", ctx.Err())
	}
	cancel()
	serving.Wait()
}
