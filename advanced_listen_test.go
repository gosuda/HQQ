package hqq

import (
	"context"
	"errors"
	"testing"
	"time"
)

// heldRequests returns how many stream requests l holds.
func heldRequests(l *AdvancedLink) int {
	l.controlMu.Lock()
	defer l.controlMu.Unlock()
	return len(l.held)
}

// waitHeld waits until l holds n stream requests. It polls the link state; no
// test depends on a fixed delay.
func waitHeld(t *testing.T, l *AdvancedLink, n int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for heldRequests(l) != n {
		if time.Now().After(deadline) {
			t.Fatalf("held stream requests = %d, want %d", heldRequests(l), n)
		}
		time.Sleep(time.Millisecond)
	}
}

// newListenPair is newNegotiatedPair with the dispatcher of the secondary
// running, as it runs once the application calls AcceptStream: requests that
// reach the secondary are then processed while it does not listen yet.
func newListenPair(t *testing.T, bufferCount int, secondaryOpts AdvancedOptions) (*AdvancedLink, *AdvancedLink) {
	t.Helper()
	primary, secondary := newNegotiatedPair(t, bufferCount, 32, AdvancedOptions{}, secondaryOpts)
	if err := secondary.startDispatcher(); err != nil {
		t.Fatalf("startDispatcher: %v", err)
	}
	return primary, secondary
}

type openResult struct {
	st  *Stream
	err error
}

// openAsync runs OpenStream on its own goroutine and reports the result on
// the returned channel.
func openAsync(ctx context.Context, l *AdvancedLink) <-chan openResult {
	ch := make(chan openResult, 1)
	go func() {
		st, err := l.OpenStream(ctx)
		ch <- openResult{st, err}
	}()
	return ch
}

// awaitOpen returns the result of an openAsync call, failing the test if it
// does not finish in time.
func awaitOpen(t *testing.T, ch <-chan openResult) openResult {
	t.Helper()
	select {
	case r := <-ch:
		return r
	case <-time.After(5 * time.Second):
		t.Fatal("OpenStream did not return")
		return openResult{}
	}
}

// TestStreamRequestHeldUntilListenStreams: a request that arrives before the
// peer listens waits, and is accepted when ListenStreams runs.
func TestStreamRequestHeldUntilListenStreams(t *testing.T) {
	primary, secondary := newListenPair(t, 8, AdvancedOptions{StreamListenGrace: time.Minute})
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	ch := openAsync(ctx, primary)
	waitHeld(t, secondary, 1)
	select {
	case r := <-ch:
		t.Fatalf("OpenStream returned before ListenStreams: %v", r.err)
	default:
	}
	if err := secondary.ListenStreams(ctx); err != nil {
		t.Fatalf("ListenStreams: %v", err)
	}
	r := awaitOpen(t, ch)
	if r.err != nil {
		t.Fatalf("OpenStream after ListenStreams: %v", r.err)
	}
	if r.st.ID() == 0 || r.st.ID()%2 != 0 {
		t.Fatalf("stream ID = %d, want a non-zero even ID", r.st.ID())
	}
	accepted, err := secondary.AcceptStream(ctx)
	if err != nil {
		t.Fatalf("AcceptStream: %v", err)
	}
	if accepted.ID() != r.st.ID() {
		t.Fatalf("accepted ID %d, want %d", accepted.ID(), r.st.ID())
	}
	if err := r.st.Send(ctx, []byte("held")); err != nil {
		t.Fatalf("Send: %v", err)
	}
	m, err := accepted.Recv(ctx)
	if err != nil {
		t.Fatalf("Recv: %v", err)
	}
	if string(m.Bytes()) != "held" {
		t.Fatalf("message = %q, want %q", m.Bytes(), "held")
	}
	m.Release()
	if n := heldRequests(secondary); n != 0 {
		t.Fatalf("held stream requests after ListenStreams = %d, want 0", n)
	}
}

// TestStreamRequestRefusedAfterGrace: a request that the peer does not admit
// within StreamListenGrace is refused with ErrConnNotFound, and its ID is not
// reused.
func TestStreamRequestRefusedAfterGrace(t *testing.T) {
	primary, secondary := newListenPair(t, 8, AdvancedOptions{StreamListenGrace: 50 * time.Millisecond})
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	refused, err := primary.OpenStream(ctx)
	if !errors.Is(err, ErrConnNotFound) {
		t.Fatalf("OpenStream before ListenStreams = %v (stream %v), want ErrConnNotFound", err, refused)
	}
	if n := heldRequests(secondary); n != 0 {
		t.Fatalf("held stream requests after the grace period = %d, want 0", n)
	}
	if err := secondary.ListenStreams(ctx); err != nil {
		t.Fatalf("ListenStreams: %v", err)
	}
	st, err := primary.OpenStream(ctx)
	if err != nil {
		t.Fatalf("OpenStream after ListenStreams: %v", err)
	}
	if st.ID() != 4 {
		t.Fatalf("stream ID after a refused request = %d, want 4 (IDs are not reused)", st.ID())
	}
	if _, err := secondary.AcceptStream(ctx); err != nil {
		t.Fatalf("AcceptStream: %v", err)
	}
}

// TestStreamRequestBeyondBoundRefusedAtOnce: with IncomingQueueSize 2, the
// third request finds the hold bound reached and is refused at once, while the
// first two are still held; the held two are then accepted in arrival order.
func TestStreamRequestBeyondBoundRefusedAtOnce(t *testing.T) {
	primary, secondary := newListenPair(t, 8, AdvancedOptions{IncomingQueueSize: 2, StreamListenGrace: time.Minute})
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	ch1 := openAsync(ctx, primary)
	waitHeld(t, secondary, 1)
	ch2 := openAsync(ctx, primary)
	waitHeld(t, secondary, 2)

	ch3 := openAsync(ctx, primary)
	if r := awaitOpen(t, ch3); !errors.Is(r.err, ErrConnNotFound) {
		t.Fatalf("third OpenStream = %v, want ErrConnNotFound", r.err)
	}
	if n := heldRequests(secondary); n != 2 {
		t.Fatalf("held stream requests = %d, want 2", n)
	}

	if err := secondary.ListenStreams(ctx); err != nil {
		t.Fatalf("ListenStreams: %v", err)
	}
	r1 := awaitOpen(t, ch1)
	r2 := awaitOpen(t, ch2)
	if r1.err != nil || r2.err != nil {
		t.Fatalf("held OpenStream results: %v, %v", r1.err, r2.err)
	}
	if r1.st.ID() != 2 || r2.st.ID() != 4 {
		t.Fatalf("held stream IDs = %d, %d; want 2, 4 (arrival order)", r1.st.ID(), r2.st.ID())
	}
	for _, want := range []uint64{2, 4} {
		st, err := secondary.AcceptStream(ctx)
		if err != nil {
			t.Fatalf("AcceptStream: %v", err)
		}
		if st.ID() != want {
			t.Fatalf("accepted ID %d, want %d", st.ID(), want)
		}
	}
}

// TestNegativeStreamListenGraceRefusesAtOnce: with a negative grace nothing is
// held and the request is refused at once (the behavior before holding).
func TestNegativeStreamListenGraceRefusesAtOnce(t *testing.T) {
	primary, secondary := newListenPair(t, 8, AdvancedOptions{StreamListenGrace: -1})
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	start := time.Now()
	_, err := primary.OpenStream(ctx)
	if !errors.Is(err, ErrConnNotFound) {
		t.Fatalf("OpenStream = %v, want ErrConnNotFound", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("refusal took %v; a negative grace must refuse at once", elapsed)
	}
	if n := heldRequests(secondary); n != 0 {
		t.Fatalf("held stream requests = %d, want 0", n)
	}
}

// TestHeldStreamRequestsAcceptedInArrivalOrder: four requests held one after
// the other are accepted in that order by ListenStreams.
func TestHeldStreamRequestsAcceptedInArrivalOrder(t *testing.T) {
	const n = 4
	primary, secondary := newListenPair(t, 8, AdvancedOptions{StreamListenGrace: time.Minute})
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	chs := make([]<-chan openResult, 0, n)
	for i := range n {
		chs = append(chs, openAsync(ctx, primary))
		waitHeld(t, secondary, i+1)
	}
	if err := secondary.ListenStreams(ctx); err != nil {
		t.Fatalf("ListenStreams: %v", err)
	}
	ids := make([]uint64, 0, n)
	for _, ch := range chs {
		r := awaitOpen(t, ch)
		if r.err != nil {
			t.Fatalf("held OpenStream: %v", r.err)
		}
		ids = append(ids, r.st.ID())
	}
	for i, want := range ids {
		st, err := secondary.AcceptStream(ctx)
		if err != nil {
			t.Fatalf("AcceptStream %d: %v", i, err)
		}
		if st.ID() != want {
			t.Fatalf("accept %d = stream %d, want %d", i, st.ID(), want)
		}
	}
}

// TestCloseReleasesHeldStreamRequests: Close discards the held requests, stops
// their timers and counts them as dropped.
func TestCloseReleasesHeldStreamRequests(t *testing.T) {
	primary, secondary := newListenPair(t, 8, AdvancedOptions{StreamListenGrace: time.Minute})
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	ch := openAsync(ctx, primary)
	waitHeld(t, secondary, 1)
	if err := secondary.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if n := heldRequests(secondary); n != 0 {
		t.Fatalf("held stream requests after Close = %d, want 0", n)
	}
	if d := secondary.Stats().DroppedMessages; d != 1 {
		t.Fatalf("DroppedMessages after Close = %d, want 1", d)
	}
	// The discarded request gets no reply; the primary's own Close ends the call.
	if err := primary.Close(); err != nil {
		t.Fatalf("primary Close: %v", err)
	}
	if r := awaitOpen(t, ch); r.err == nil {
		t.Fatal("OpenStream of a discarded request succeeded")
	}
}

// TestShutdownRefusesHeldStreamRequests: Shutdown refuses the requests it
// holds at once.
func TestShutdownRefusesHeldStreamRequests(t *testing.T) {
	primary, secondary := newListenPair(t, 8, AdvancedOptions{StreamListenGrace: time.Minute})
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	ch := openAsync(ctx, primary)
	waitHeld(t, secondary, 1)
	if err := secondary.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if r := awaitOpen(t, ch); !errors.Is(r.err, ErrConnNotFound) {
		t.Fatalf("OpenStream held at Shutdown = %v, want ErrConnNotFound", r.err)
	}
	if n := heldRequests(secondary); n != 0 {
		t.Fatalf("held stream requests after Shutdown = %d, want 0", n)
	}
}

// TestLinkFailureReleasesHeldStreamRequests: when the dispatcher of a link
// stops (here: the peer closed), its held requests are released well before
// their grace period, which is one minute.
func TestLinkFailureReleasesHeldStreamRequests(t *testing.T) {
	primary, secondary := newListenPair(t, 8, AdvancedOptions{StreamListenGrace: time.Minute})
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	_ = openAsync(ctx, primary)
	waitHeld(t, secondary, 1)
	if err := primary.Close(); err != nil {
		t.Fatalf("primary Close: %v", err)
	}
	waitHeld(t, secondary, 0)
	select {
	case <-secondary.dispatchDone:
	case <-time.After(5 * time.Second):
		t.Fatal("dispatcher did not stop after the peer closed")
	}
}

// TestDefaultStreamListenGrace: a zero StreamListenGrace selects the default.
func TestDefaultStreamListenGrace(t *testing.T) {
	link := newTestAdvancedLink(t, 8, 32, AdvancedOptions{})
	if link.streamListenGrace != 2*time.Second {
		t.Fatalf("streamListenGrace = %v, want 2s", link.streamListenGrace)
	}
}
