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
	return len(l.connections)
}

func TestNegotiationFailedIsTerminal(t *testing.T) {
	link := newTestAdvancedLink(t, 8, 32, AdvancedOptions{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := link.NegotiateProtocol(ctx, currentProtocolVersion, FeatureLargeCopy); err == nil {
		t.Fatal("NegotiateProtocol with a canceled context succeeded")
	}
	if got := link.ProtocolState(); got != ProtocolStateFailed {
		t.Fatalf("state = %v, want ProtocolStateFailed", got)
	}

	live := context.Background()
	if _, err := link.NegotiateProtocol(live, currentProtocolVersion, FeatureLargeCopy); !errors.Is(err, ErrNegotiationFailed) || errors.Is(err, ErrAlreadyNegotiated) {
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
		_, err := link.NegotiateProtocol(ctx, currentProtocolVersion, FeatureLargeCopy)
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
	mux.HandleFunc(1, func(_ context.Context, req AdvancedMessage) ([]byte, error) { return req.Payload, nil })
	for _, l := range []*AdvancedLink{primary, secondary} {
		go l.Serve(ctx, mux)
	}

	// settle makes sure every packet sent by from before the call was
	// processed by the other side's receive path: a response is sent after,
	// and received after, everything that preceded the request.
	settle := func(from *AdvancedLink) {
		t.Helper()
		if _, err := from.Call(ctx, 1, []byte("sync")); err != nil {
			t.Fatalf("sync call: %v", err)
		}
	}
	checkClosed := func(l *AdvancedLink, id uint64) {
		t.Helper()
		if _, err := l.CallOn(ctx, id, 1, nil); !errors.Is(err, ErrConnNotFound) {
			t.Fatalf("CallOn closed stream = %v, want ErrConnNotFound", err)
		}
		if _, err := l.SendLarge(ctx, id, []byte("x")); !errors.Is(err, ErrConnNotFound) {
			t.Fatalf("SendLarge closed stream = %v, want ErrConnNotFound", err)
		}
		if err := l.CloseStream(ctx, id); !errors.Is(err, ErrConnNotFound) {
			t.Fatalf("CloseStream closed stream = %v, want ErrConnNotFound", err)
		}
	}

	// The opener closes.
	id, err := primary.OpenStream(ctx)
	if err != nil {
		t.Fatal(err)
	}
	accepted, err := secondary.AcceptStream(ctx)
	if err != nil || accepted != id {
		t.Fatalf("AcceptStream = %d, %v; want %d", accepted, err, id)
	}
	if streamCount(primary) != 1 || streamCount(secondary) != 1 {
		t.Fatalf("open stream registered %d/%d times, want 1/1", streamCount(primary), streamCount(secondary))
	}
	if resp, err := primary.CallOn(ctx, id, 1, []byte("hi")); err != nil || string(resp.Payload) != "hi" {
		t.Fatalf("CallOn open stream = %q, %v", resp.Payload, err)
	}
	if err := primary.CloseStream(ctx, id); err != nil {
		t.Fatal(err)
	}
	if n := streamCount(primary); n != 0 {
		t.Fatalf("opener registry after CloseStream = %d, want 0", n)
	}
	settle(primary)
	if n := streamCount(secondary); n != 0 {
		t.Fatalf("peer registry after OpConnClose = %d, want 0", n)
	}
	checkClosed(primary, id)
	checkClosed(secondary, id)

	// The acceptor closes.
	id2, err := primary.OpenStream(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if id2 == id {
		t.Fatalf("stream ID %d was reused", id)
	}
	if accepted, err := secondary.AcceptStream(ctx); err != nil || accepted != id2 {
		t.Fatalf("AcceptStream = %d, %v; want %d", accepted, err, id2)
	}
	if err := secondary.CloseStream(ctx, id2); err != nil {
		t.Fatal(err)
	}
	if n := streamCount(secondary); n != 0 {
		t.Fatalf("acceptor registry after CloseStream = %d, want 0", n)
	}
	settle(secondary)
	if n := streamCount(primary); n != 0 {
		t.Fatalf("opener registry after the peer's OpConnClose = %d, want 0", n)
	}
	checkClosed(primary, id2)
	checkClosed(secondary, id2)

	// IDs that were never streams.
	for _, bad := range []uint64{0, 12345} {
		if err := primary.CloseStream(ctx, bad); !errors.Is(err, ErrConnNotFound) {
			t.Fatalf("CloseStream(%d) = %v, want ErrConnNotFound", bad, err)
		}
	}
	if _, err := primary.CallOn(ctx, 12345, 1, nil); !errors.Is(err, ErrConnNotFound) {
		t.Fatalf("CallOn unknown stream = %v, want ErrConnNotFound", err)
	}
	// Link-level traffic on ID 0 is unaffected.
	settle(primary)
}

func TestCloseStreamBeforeNegotiation(t *testing.T) {
	link := newTestAdvancedLink(t, 8, 32, AdvancedOptions{})
	if err := link.CloseStream(context.Background(), 2); !errors.Is(err, ErrNotNegotiated) {
		t.Fatalf("CloseStream before negotiation = %v, want ErrNotNegotiated", err)
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

	opened := make(chan uint64, 1)
	go func() {
		id, err := primary.OpenStream(ctx)
		if err != nil {
			t.Error(err)
		}
		opened <- id
	}()
	id, err := secondary.AcceptStream(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := secondary.SendLarge(ctx, id, []byte("first")); err != nil {
		t.Fatal(err)
	}
	message, err := primary.Receive(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// The dispatcher processed the accept before the data frame it
	// delivered: no OpenStream is pending any more.
	primary.controlMu.Lock()
	pending := len(primary.pendingConnections)
	primary.controlMu.Unlock()
	if message.ConnectionID != id || pending != 0 {
		t.Fatalf("data on stream %d arrived with %d pending OpenStream calls (message on %d)", id, pending, message.ConnectionID)
	}
	if got := <-opened; got != id {
		t.Fatalf("OpenStream = %d, want %d", got, id)
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
	mux.HandleFunc(1, func(_ context.Context, req AdvancedMessage) ([]byte, error) { return req.Payload, nil })
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
					id, err := l.OpenStream(ctx)
					if err != nil {
						t.Errorf("%v worker %d round %d: OpenStream: %v", l.Mode(), w, r, err)
						return
					}
					resp, err := l.CallOn(ctx, id, 1, payload)
					if err != nil || !bytes.Equal(resp.Payload, payload) {
						t.Errorf("%v worker %d round %d: CallOn: %d bytes, %v", l.Mode(), w, r, len(resp.Payload), err)
						return
					}
					if _, err := l.Call(ctx, 1, payload); err != nil {
						t.Errorf("%v worker %d round %d: Call: %v", l.Mode(), w, r, err)
						return
					}
					if err := l.CloseStream(ctx, id); err != nil {
						t.Errorf("%v worker %d round %d: CloseStream: %v", l.Mode(), w, r, err)
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
