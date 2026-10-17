package hqq

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unsafe"
)

// openStreamPair opens one stream from opener to acceptor and returns both
// ends.
func openStreamPair(t testing.TB, opener, acceptor *AdvancedLink) (out, in *Stream) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := acceptor.ListenStreams(context.Background()); err != nil && !errors.Is(err, ErrAlreadyListening) {
		t.Fatalf("ListenStreams: %v", err)
	}
	type result struct {
		s   *Stream
		err error
	}
	accepted := make(chan result, 1)
	go func() {
		s, err := acceptor.AcceptStream(ctx)
		accepted <- result{s, err}
	}()
	out, err := opener.OpenStream(ctx)
	if err != nil {
		t.Fatalf("OpenStream: %v", err)
	}
	r := <-accepted
	if r.err != nil {
		t.Fatalf("AcceptStream: %v", r.err)
	}
	if r.s.ID() != out.ID() {
		t.Fatalf("accepted stream ID %d, opened %d", r.s.ID(), out.ID())
	}
	return out, r.s
}

func recvString(t testing.TB, s *Stream) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	m, err := s.Recv(ctx)
	if err != nil {
		t.Fatalf("Recv on stream %d: %v", s.ID(), err)
	}
	got := string(m.Bytes())
	if m.StreamID() != s.ID() {
		t.Fatalf("message of stream %d returned by stream %d", m.StreamID(), s.ID())
	}
	m.Release()
	return got
}

func expectRecvTimeout(t testing.TB, s *Stream, d time.Duration) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	m, err := s.Recv(ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		got := ""
		if m != nil {
			got = string(m.Bytes())
		}
		t.Fatalf("Recv on stream %d = %q, %v; want context.DeadlineExceeded", s.ID(), got, err)
	}
}

func eventually(t testing.TB, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestStreamAccessors(t *testing.T) {
	primary, secondary := newNegotiatedPair(t, 16, 64, AdvancedOptions{}, AdvancedOptions{})
	if primary.Stream() != primary.Stream() || primary.Stream().ID() != 0 {
		t.Fatalf("Stream() must return the same stream 0 every time, got ID %d", primary.Stream().ID())
	}
	if primary.Stream() == secondary.Stream() {
		t.Fatal("two links share one link-level stream")
	}
	a, _ := openStreamPair(t, primary, secondary)
	b, _ := openStreamPair(t, secondary, primary)
	if a.ID() == 0 || a.ID()%2 != 0 {
		t.Fatalf("Primary stream ID = %d, want even and non-zero", a.ID())
	}
	if b.ID()%2 != 1 {
		t.Fatalf("Secondary stream ID = %d, want odd", b.ID())
	}
	if err := primary.Stream().Close(); !errors.Is(err, ErrInvalidOp) {
		t.Fatalf("Close of stream 0 = %v, want ErrInvalidOp", err)
	}
	// The link-level shortcuts are l.Stream().X.
	ctx := context.Background()
	if err := primary.Send(ctx, []byte("via link")); err != nil {
		t.Fatal(err)
	}
	if got := recvString(t, secondary.Stream()); got != "via link" {
		t.Fatalf("Stream().Recv = %q", got)
	}
	if err := primary.Stream().Send(ctx, []byte("via stream")); err != nil {
		t.Fatal(err)
	}
	m, err := secondary.Recv(ctx)
	if err != nil || string(m.Bytes()) != "via stream" || m.StreamID() != 0 || m.Len() != len("via stream") {
		t.Fatalf("Recv = %v, %v", m, err)
	}
	m.Release()
}

func TestStreamIsolation(t *testing.T) {
	primary, secondary := newNegotiatedPair(t, 16, 16, AdvancedOptions{}, AdvancedOptions{})
	outA, inA := openStreamPair(t, primary, secondary)
	outB, inB := openStreamPair(t, primary, secondary)
	ctx := context.Background()
	long := string(patternedBytes(100, 1)) // multi-chunk
	for _, step := range []struct {
		s *Stream
		p string
	}{
		{outA, "A1"}, {outB, "B1"}, {primary.Stream(), "L1"}, {outA, long}, {outB, "B2"}, {outA, "A3"},
	} {
		if err := step.s.Send(ctx, []byte(step.p)); err != nil {
			t.Fatal(err)
		}
	}
	// Each stream returns its own messages, in order, whatever the order in
	// which the streams are read.
	if got := recvString(t, inB); got != "B1" {
		t.Fatalf("B first = %q", got)
	}
	if got := recvString(t, secondary.Stream()); got != "L1" {
		t.Fatalf("link-level first = %q", got)
	}
	if got := recvString(t, inA); got != "A1" {
		t.Fatalf("A first = %q", got)
	}
	if got := recvString(t, inA); got != long {
		t.Fatalf("A second = %q", got)
	}
	if got := recvString(t, inB); got != "B2" {
		t.Fatalf("B second = %q", got)
	}
	if got := recvString(t, inA); got != "A3" {
		t.Fatalf("A third = %q", got)
	}
	for _, s := range []*Stream{inA, inB, secondary.Stream()} {
		expectRecvTimeout(t, s, 20*time.Millisecond)
	}
}

func TestRecvNeverReturnsRequestsOrResponses(t *testing.T) {
	client, server := newNegotiatedPair(t, 16, 64, AdvancedOptions{}, AdvancedOptions{})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Both endpoints Recv while a call is in flight: neither sees the
	// request or the response.
	type recvResult struct {
		m   *Message
		err error
	}
	clientRecv := make(chan recvResult, 1)
	serverRecv := make(chan recvResult, 1)
	recvCtx, recvCancel := context.WithTimeout(ctx, 400*time.Millisecond)
	defer recvCancel()
	go func() {
		m, err := client.Recv(recvCtx)
		clientRecv <- recvResult{m, err}
	}()
	go func() {
		m, err := server.Recv(recvCtx)
		serverRecv <- recvResult{m, err}
	}()

	callDone := make(chan error, 1)
	go func() {
		m, err := client.Call(ctx, 5, []byte("ping"))
		if err == nil {
			if string(m.Bytes()) != "pong" {
				err = fmt.Errorf("response %q", m.Bytes())
			}
			m.Release()
		}
		callDone <- err
	}()

	// No Serve yet: the request waits in the request queue.
	select {
	case err := <-callDone:
		t.Fatalf("Call finished without a server: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	serveCtx, stopServe := context.WithCancel(ctx)
	serveDone := make(chan error, 1)
	go func() {
		serveDone <- server.Serve(serveCtx, HandlerFunc(func(_ context.Context, w *ResponseWriter, req *Request) {
			if string(req.Payload()) == "ping" {
				_, _ = w.Write([]byte("pong"))
			}
		}))
	}()
	if err := <-callDone; err != nil {
		t.Fatalf("Call: %v", err)
	}
	for name, ch := range map[string]chan recvResult{"client": clientRecv, "server": serverRecv} {
		r := <-ch
		if !errors.Is(r.err, context.DeadlineExceeded) {
			t.Fatalf("%s Recv = %v, %v; want context.DeadlineExceeded", name, r.m, r.err)
		}
	}
	stopServe()
	<-serveDone
}

func TestUnmatchedResponseIsDroppedAndCounted(t *testing.T) {
	client, server := newNegotiatedPair(t, 16, 16, AdvancedOptions{}, AdvancedOptions{})
	ctx := context.Background()
	const resp = AdvFlagResponse
	// A single-chunk and a multi-chunk response to calls nobody made.
	injectAdvancedFrame(t, server, 0, 900, 3, 0, AdvFlagBegin|AdvFlagEnd|resp, 0, []byte("abc"))
	injectAdvancedFrame(t, server, 0, 901, 20, 0, AdvFlagBegin|resp, 0, bytes.Repeat([]byte{'x'}, 16))
	injectAdvancedFrame(t, server, 0, 901, 20, 16, AdvFlagEnd|resp, 0, bytes.Repeat([]byte{'x'}, 4))
	injectAdvancedFrame(t, server, 7, 902, 0, 0, AdvFlagBegin|AdvFlagEnd|resp|AdvFlagError, 5, nil)
	if err := server.Send(ctx, []byte("data")); err != nil {
		t.Fatal(err)
	}
	if got := recvString(t, client.Stream()); got != "data" {
		t.Fatalf("Recv = %q, want the data message after the dropped responses", got)
	}
	if got := client.Stats().DroppedMessages; got != 3 {
		t.Fatalf("DroppedMessages = %d, want 3", got)
	}
	if got := client.Stats().AbandonedResponses; got != 0 {
		t.Fatalf("AbandonedResponses = %d, want 0", got)
	}
}

func TestDataForUnknownStreamIsDroppedAndCounted(t *testing.T) {
	client, server := newNegotiatedPair(t, 16, 16, AdvancedOptions{}, AdvancedOptions{})
	ctx := context.Background()
	injectAdvancedFrame(t, server, 4242, 1, 3, 0, AdvFlagBegin|AdvFlagEnd, 0, []byte("abc"))
	injectAdvancedFrame(t, server, 4242, 2, 20, 0, AdvFlagBegin, 0, bytes.Repeat([]byte{'y'}, 16))
	injectAdvancedFrame(t, server, 4242, 2, 20, 16, AdvFlagEnd, 0, bytes.Repeat([]byte{'y'}, 4))
	if err := server.Send(ctx, []byte("data")); err != nil {
		t.Fatal(err)
	}
	if got := recvString(t, client.Stream()); got != "data" {
		t.Fatalf("Recv = %q", got)
	}
	if got := client.Stats().DroppedMessages; got != 2 {
		t.Fatalf("DroppedMessages = %d, want 2", got)
	}

	// Data that arrives after the stream was closed is dropped as well.
	out, in := openStreamPair(t, client, server)
	if err := in.Close(); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the peer's close", func() bool { return out.closedErr() != nil })
	injectAdvancedFrame(t, server, out.ID(), 77, 2, 0, AdvFlagBegin|AdvFlagEnd, 0, []byte("zz"))
	if err := server.Send(ctx, []byte("data2")); err != nil {
		t.Fatal(err)
	}
	if got := recvString(t, client.Stream()); got != "data2" {
		t.Fatalf("Recv = %q", got)
	}
	if got := client.Stats().DroppedMessages; got != 3 {
		t.Fatalf("DroppedMessages = %d, want 3", got)
	}
}

func TestStreamCloseErrors(t *testing.T) {
	primary, secondary := newNegotiatedPair(t, 16, 64, AdvancedOptions{}, AdvancedOptions{})
	out, in := openStreamPair(t, primary, secondary)
	ctx := context.Background()
	if err := out.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := out.Close(); !errors.Is(err, ErrClosed) {
		t.Fatalf("second Close = %v, want ErrClosed", err)
	}
	if err := out.Send(ctx, []byte("x")); !errors.Is(err, ErrClosed) {
		t.Fatalf("Send after Close = %v, want ErrClosed", err)
	}
	if m, err := out.Recv(ctx); !errors.Is(err, ErrClosed) {
		t.Fatalf("Recv after Close = %v, %v, want ErrClosed", m, err)
	}
	if m, err := out.Call(ctx, 1, nil); !errors.Is(err, ErrClosed) {
		t.Fatalf("Call after Close = %v, %v, want ErrClosed", m, err)
	}

	// The peer's stream reports ErrPeerClosed (which matches io.EOF).
	eventually(t, "the peer's close", func() bool { return in.closedErr() != nil })
	if err := in.Send(ctx, []byte("x")); !errors.Is(err, ErrPeerClosed) {
		t.Fatalf("peer Send = %v, want ErrPeerClosed", err)
	}
	if m, err := in.Recv(ctx); !errors.Is(err, ErrPeerClosed) {
		t.Fatalf("peer Recv = %v, %v, want ErrPeerClosed", m, err)
	}
	if m, err := in.Call(ctx, 1, nil); !errors.Is(err, ErrPeerClosed) {
		t.Fatalf("peer Call = %v, %v, want ErrPeerClosed", m, err)
	}
	// Closing a stream the peer already closed succeeds once.
	if err := in.Close(); err != nil {
		t.Fatalf("Close after peer close = %v", err)
	}
	if err := in.Close(); !errors.Is(err, ErrClosed) {
		t.Fatalf("second Close = %v, want ErrClosed", err)
	}
	if m, err := in.Recv(ctx); !errors.Is(err, ErrClosed) {
		t.Fatalf("Recv after local Close = %v, %v, want ErrClosed", m, err)
	}
	// The link and its other streams are unaffected.
	if err := primary.Send(ctx, []byte("alive")); err != nil {
		t.Fatal(err)
	}
	if got := recvString(t, secondary.Stream()); got != "alive" {
		t.Fatalf("Recv = %q", got)
	}
}

func TestStreamPeerCloseDrainsQueuedMessages(t *testing.T) {
	primary, secondary := newNegotiatedPair(t, 16, 16, AdvancedOptions{}, AdvancedOptions{})
	out, in := openStreamPair(t, primary, secondary)
	ctx := context.Background()
	long := string(patternedBytes(60, 3))
	for _, p := range []string{"one", long, "three"} {
		if err := out.Send(ctx, []byte(p)); err != nil {
			t.Fatal(err)
		}
	}
	if err := out.Close(); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the peer's close", func() bool { return in.closedErr() != nil })
	for _, want := range []string{"one", long, "three"} {
		if got := recvString(t, in); got != want {
			t.Fatalf("queued message = %q, want %q", got, want)
		}
	}
	if m, err := in.Recv(ctx); !errors.Is(err, ErrPeerClosed) {
		t.Fatalf("Recv after the queue drained = %v, %v, want ErrPeerClosed", m, err)
	}
}

func TestStreamCloseWakesBlockedRecv(t *testing.T) {
	primary, secondary := newNegotiatedPair(t, 16, 64, AdvancedOptions{}, AdvancedOptions{})
	out, in := openStreamPair(t, primary, secondary)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	recv := func(s *Stream) chan error {
		ch := make(chan error, 1)
		go func() {
			m, err := s.Recv(ctx)
			if err == nil {
				m.Release()
				err = errors.New("Recv returned a message")
			}
			ch <- err
		}()
		return ch
	}
	// Local Close wakes a blocked Recv with ErrClosed.
	blockedOut := recv(out)
	time.Sleep(20 * time.Millisecond)
	if err := out.Close(); err != nil {
		t.Fatal(err)
	}
	if err := waitErr(t, blockedOut, 5*time.Second); !errors.Is(err, ErrClosed) {
		t.Fatalf("blocked Recv after local Close = %v, want ErrClosed", err)
	}
	// The peer's Close wakes the other side with ErrPeerClosed.
	if err := waitErr(t, recv(in), 5*time.Second); !errors.Is(err, ErrPeerClosed) {
		t.Fatalf("blocked Recv after peer Close = %v, want ErrPeerClosed", err)
	}

	// Several blocked receivers all wake.
	out2, in2 := openStreamPair(t, primary, secondary)
	chans := []chan error{recv(in2), recv(in2), recv(in2)}
	time.Sleep(20 * time.Millisecond)
	if err := out2.Close(); err != nil {
		t.Fatal(err)
	}
	for i, ch := range chans {
		if err := waitErr(t, ch, 5*time.Second); !errors.Is(err, ErrPeerClosed) {
			t.Fatalf("receiver %d = %v, want ErrPeerClosed", i, err)
		}
	}
}

func TestStreamCloseWakesBlockedCall(t *testing.T) {
	client, server := newNegotiatedPair(t, 16, 64, AdvancedOptions{}, AdvancedOptions{})
	out, in := openStreamPair(t, client, server)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	entered := make(chan struct{}, 4)
	release := make(chan struct{})
	serveDone := make(chan error, 1)
	go func() {
		serveDone <- server.Serve(ctx, HandlerFunc(func(_ context.Context, w *ResponseWriter, _ *Request) {
			entered <- struct{}{}
			<-release
			_, _ = w.Write([]byte("late"))
		}))
	}()

	call := func(s *Stream) chan error {
		ch := make(chan error, 1)
		go func() {
			m, err := s.Call(ctx, 1, []byte("q"))
			if err == nil {
				m.Release()
				err = errors.New("Call succeeded")
			}
			ch <- err
		}()
		return ch
	}

	// The peer closes the stream under a waiting call.
	waiting := call(out)
	<-entered
	if err := in.Close(); err != nil {
		t.Fatal(err)
	}
	if err := waitErr(t, waiting, 5*time.Second); !errors.Is(err, ErrPeerClosed) {
		t.Fatalf("Call after peer Close = %v, want ErrPeerClosed", err)
	}

	// A local Close wakes a waiting call with ErrClosed.
	out2, _ := openStreamPair(t, client, server)
	waiting = call(out2)
	<-entered
	if err := out2.Close(); err != nil {
		t.Fatal(err)
	}
	if err := waitErr(t, waiting, 5*time.Second); !errors.Is(err, ErrClosed) {
		t.Fatalf("Call after local Close = %v, want ErrClosed", err)
	}

	// The late responses are discarded as responses of canceled calls.
	close(release)
	eventually(t, "late responses to be discarded", func() bool { return client.Stats().AbandonedResponses == 2 })
	if got := client.Stats().DroppedMessages; got != 0 {
		t.Fatalf("DroppedMessages = %d, want 0", got)
	}
}

func TestMessageReleaseReusesBuffer(t *testing.T) {
	sender, receiver := newNegotiatedPair(t, 64, 64, AdvancedOptions{}, AdvancedOptions{})
	ctx := context.Background()
	payload := patternedBytes(200, 7)
	data := func(m *Message) uintptr { return uintptr(unsafe.Pointer(unsafe.SliceData(m.data))) }

	if err := sender.Send(ctx, payload); err != nil {
		t.Fatal(err)
	}
	first, err := receiver.Recv(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first.Bytes(), payload) || cap(first.Bytes()) != first.Len() {
		t.Fatalf("message %d bytes, cap %d", first.Len(), cap(first.Bytes()))
	}
	released := map[uintptr]bool{data(first): true}
	first.Release()

	// A message of the same size class reuses the released buffer. sync.Pool
	// may drop an item at any time (and does at random under the race
	// detector), so look at a number of messages.
	reused := false
	for i := 0; i < 200 && !reused; i++ {
		if err := sender.Send(ctx, payload[:150]); err != nil {
			t.Fatal(err)
		}
		m, err := receiver.Recv(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(m.Bytes(), payload[:150]) {
			t.Fatalf("message %d corrupted", i)
		}
		reused = released[data(m)]
		released[data(m)] = true
		m.Release()
	}
	if !reused {
		t.Fatal("a released buffer was never reused by a later message of its size class")
	}
}

func TestUnreleasedMessageStaysIntact(t *testing.T) {
	client, server := newNegotiatedPair(t, 64, 64, AdvancedOptions{}, AdvancedOptions{})
	ctx, cancel := context.WithCancel(context.Background())
	serveDone := make(chan error, 1)
	reply := patternedBytes(300, 11)
	go func() {
		serveDone <- server.Serve(ctx, HandlerFunc(func(_ context.Context, w *ResponseWriter, _ *Request) {
			_, _ = w.Write(reply)
		}))
	}()
	defer func() {
		cancel()
		<-serveDone
	}()

	// Keep one received message and one call response while many others
	// of the same classes pass through and are released.
	heldData := patternedBytes(300, 21)
	if err := client.Send(ctx, heldData); err != nil {
		t.Fatal(err)
	}
	held, err := server.Recv(ctx)
	if err != nil {
		t.Fatal(err)
	}
	heldReply, err := client.Call(ctx, 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 300; i++ {
		p := patternedBytes(200+i%100, byte(i))
		if err := client.Send(ctx, p); err != nil {
			t.Fatal(err)
		}
		m, err := server.Recv(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(m.Bytes(), p) {
			t.Fatalf("message %d corrupted", i)
		}
		m.Release()
		r, err := client.Call(ctx, 1, p)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(r.Bytes(), reply) {
			t.Fatalf("response %d corrupted", i)
		}
		r.Release()
	}
	if !bytes.Equal(held.Bytes(), heldData) {
		t.Fatal("an unreleased message was overwritten while others arrived")
	}
	if !bytes.Equal(heldReply.Bytes(), reply) {
		t.Fatal("an unreleased response was overwritten while others arrived")
	}
	held.Release()
	heldReply.Release()
}

func TestResponseWriterSemantics(t *testing.T) {
	const maxSize = 1000
	opts := AdvancedOptions{MaxMessageSize: maxSize}
	client, server := newNegotiatedPair(t, 16, 64, opts, opts)
	ctx, cancel := context.WithCancel(context.Background())
	serveDone := make(chan error, 1)

	var writeErrs [3]error
	var writeN [3]int
	mux := NewMux()
	mux.HandleFunc(1, func(_ context.Context, w *ResponseWriter, _ *Request) {
		_, _ = w.Write([]byte("a"))
		_, _ = w.Write([]byte("bc"))
	})
	mux.HandleFunc(2, func(_ context.Context, w *ResponseWriter, _ *Request) {
		_, _ = w.Write([]byte("discarded"))
		w.WriteError(7, []byte("err"))
	})
	mux.HandleFunc(3, func(_ context.Context, w *ResponseWriter, _ *Request) {
		w.WriteError(8, []byte("err"))
		_, _ = w.Write([]byte("-more"))
	})
	mux.HandleFunc(4, func(_ context.Context, w *ResponseWriter, _ *Request) {})
	mux.HandleFunc(5, func(_ context.Context, w *ResponseWriter, _ *Request) {
		w.WriteError(0, []byte("zero"))
	})
	mux.HandleFunc(6, func(_ context.Context, w *ResponseWriter, _ *Request) {
		writeN[0], writeErrs[0] = w.Write(make([]byte, maxSize+1))
		writeN[1], writeErrs[1] = w.Write(bytes.Repeat([]byte{'p'}, 600))
		writeN[2], writeErrs[2] = w.Write(bytes.Repeat([]byte{'q'}, 600))
		_, _ = w.Write(bytes.Repeat([]byte{'r'}, 400)) // exactly maxSize in total
	})
	mux.HandleFunc(7, func(_ context.Context, w *ResponseWriter, _ *Request) {
		w.WriteError(5, make([]byte, maxSize+1))
	})
	mux.HandleFunc(8, func(_ context.Context, w *ResponseWriter, _ *Request) {
		for i := 0; i < 30; i++ { // grows the buffer across size classes
			_, _ = w.Write(bytes.Repeat([]byte{byte('a' + i)}, 33))
		}
	})
	mux.HandleFunc(9, func(_ context.Context, w *ResponseWriter, req *Request) {
		fmt.Fprintf(w, "%d/%d/%s", req.Method(), req.StreamID(), req.Payload())
	})
	go func() { serveDone <- server.Serve(ctx, mux) }()
	defer func() {
		cancel()
		<-serveDone
	}()

	call := func(s *Stream, method uint64, req []byte) ([]byte, error) {
		m, err := s.Call(ctx, method, req)
		if err != nil {
			return nil, err
		}
		defer m.Release()
		return append([]byte{}, m.Bytes()...), nil
	}
	wantResponseError := func(err error, status uint64, payload string) {
		t.Helper()
		var re ResponseError
		if !errors.As(err, &re) || re.Status != status || string(re.Payload) != payload {
			t.Fatalf("error = %#v, want ResponseError{%d, %q}", err, status, payload)
		}
	}

	if got, err := call(client.Stream(), 1, nil); err != nil || string(got) != "abc" {
		t.Fatalf("append Writes = %q, %v", got, err)
	}
	_, err := call(client.Stream(), 2, nil)
	wantResponseError(err, 7, "err") // WriteError replaces what was written
	_, err = call(client.Stream(), 3, nil)
	wantResponseError(err, 8, "err-more") // a Write after WriteError appends to its payload
	if got, err := call(client.Stream(), 4, []byte("ignored")); err != nil || len(got) != 0 {
		t.Fatalf("silent handler = %q, %v, want an empty success", got, err)
	}
	_, err = call(client.Stream(), 5, nil)
	wantResponseError(err, uint64(ErrCodeInvalidOp), "zero")

	got, err := call(client.Stream(), 6, nil)
	if err != nil || len(got) != maxSize || got[0] != 'p' || got[599] != 'p' || got[600] != 'r' {
		t.Fatalf("overflowing Writes: %d bytes, %v", len(got), err)
	}
	if writeN[0] != 0 || !errors.Is(writeErrs[0], ErrBufferOverflow) ||
		writeN[1] != 600 || writeErrs[1] != nil ||
		writeN[2] != 0 || !errors.Is(writeErrs[2], ErrBufferOverflow) {
		t.Fatalf("Write results = %v %v, want (0, ErrBufferOverflow), (600, nil), (0, ErrBufferOverflow)", writeN, writeErrs)
	}
	_, err = call(client.Stream(), 7, nil)
	wantResponseError(err, uint64(ErrCodeBufferOverflow), "")

	var want []byte
	for i := 0; i < 30; i++ {
		want = append(want, bytes.Repeat([]byte{byte('a' + i)}, 33)...)
	}
	if got, err := call(client.Stream(), 8, nil); err != nil || !bytes.Equal(got, want) {
		t.Fatalf("grown response: %d bytes, %v", len(got), err)
	}

	// Request accessors, on the link-level stream and on a stream.
	if got, err := call(client.Stream(), 9, []byte("p")); err != nil || string(got) != "9/0/p" {
		t.Fatalf("request accessors = %q, %v", got, err)
	}
	out, _ := openStreamPair(t, client, server)
	if got, err := call(out, 9, []byte("q")); err != nil || string(got) != fmt.Sprintf("9/%d/q", out.ID()) {
		t.Fatalf("request accessors on stream = %q, %v", got, err)
	}
	// An unregistered method is an error response.
	_, err = call(client.Stream(), 99, nil)
	wantResponseError(err, uint64(ErrCodeInvalidOp), "hqq: unknown method")
}

func TestMuxNotFound(t *testing.T) {
	client, server := newNegotiatedPair(t, 16, 64, AdvancedOptions{}, AdvancedOptions{})
	ctx, cancel := context.WithCancel(context.Background())
	serveDone := make(chan error, 1)
	mux := NewMux()
	mux.NotFound = HandlerFunc(func(_ context.Context, w *ResponseWriter, req *Request) {
		fmt.Fprintf(w, "nf %d", req.Method())
	})
	go func() { serveDone <- server.Serve(ctx, mux) }()
	defer func() {
		cancel()
		<-serveDone
	}()
	m, err := client.Call(ctx, 42, nil)
	if err != nil || string(m.Bytes()) != "nf 42" {
		t.Fatalf("NotFound = %v, %v", m, err)
	}
	m.Release()
}

func TestOverflowAppliesPerDestinationQueue(t *testing.T) {
	t.Run("drop", func(t *testing.T) {
		sender, receiver := newNegotiatedPair(t, 32, 64, AdvancedOptions{},
			AdvancedOptions{IncomingQueueSize: 2, IncomingOverflow: OverflowDrop})
		outA, inA := openStreamPair(t, sender, receiver)
		outB, inB := openStreamPair(t, sender, receiver)
		ctx := context.Background()
		for i := 0; i < 5; i++ {
			if err := outA.Send(ctx, []byte(fmt.Sprintf("a%d", i))); err != nil {
				t.Fatal(err)
			}
		}
		if err := outB.Send(ctx, []byte("b0")); err != nil {
			t.Fatal(err)
		}
		// B is not affected by the full queue of A.
		if got := recvString(t, inB); got != "b0" {
			t.Fatalf("B = %q", got)
		}
		if got := receiver.Stats().DroppedMessages; got != 3 {
			t.Fatalf("DroppedMessages = %d, want 3", got)
		}
		for _, want := range []string{"a0", "a1"} {
			if got := recvString(t, inA); got != want {
				t.Fatalf("A = %q, want %q", got, want)
			}
		}
		expectRecvTimeout(t, inA, 20*time.Millisecond)
	})

	t.Run("block", func(t *testing.T) {
		sender, receiver := newNegotiatedPair(t, 32, 64, AdvancedOptions{},
			AdvancedOptions{IncomingQueueSize: 2, IncomingOverflow: OverflowBlock})
		outA, inA := openStreamPair(t, sender, receiver)
		outB, inB := openStreamPair(t, sender, receiver)
		ctx := context.Background()
		for i := 0; i < 5; i++ {
			if err := outA.Send(ctx, []byte(fmt.Sprintf("a%d", i))); err != nil {
				t.Fatal(err)
			}
		}
		if err := outB.Send(ctx, []byte("b0")); err != nil {
			t.Fatal(err)
		}
		// The receive path waits for the consumer of A's queue, so B's
		// message behind it is not routed yet.
		expectRecvTimeout(t, inB, 50*time.Millisecond)
		for i := 0; i < 5; i++ {
			if got := recvString(t, inA); got != fmt.Sprintf("a%d", i) {
				t.Fatalf("A message %d = %q", i, got)
			}
		}
		if got := recvString(t, inB); got != "b0" {
			t.Fatalf("B = %q", got)
		}
		if got := receiver.Stats().DroppedMessages; got != 0 {
			t.Fatalf("DroppedMessages = %d, want 0", got)
		}
	})

	t.Run("fail", func(t *testing.T) {
		sender, receiver := newNegotiatedPair(t, 32, 64, AdvancedOptions{},
			AdvancedOptions{IncomingQueueSize: 2, IncomingOverflow: OverflowFail})
		outA, inA := openStreamPair(t, sender, receiver)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		for i := 0; i < 3; i++ {
			if err := outA.Send(ctx, []byte("a")); err != nil {
				t.Fatal(err)
			}
		}
		eventually(t, "the receive path to fail", func() bool {
			_, err := receiver.AcceptStream(ctx)
			return errors.Is(err, ErrIncomingOverflow)
		})
		// The queued messages are still delivered, then the failure.
		for i := 0; i < 2; i++ {
			if got := recvString(t, inA); got != "a" {
				t.Fatalf("queued = %q", got)
			}
		}
		if m, err := inA.Recv(ctx); !errors.Is(err, ErrIncomingOverflow) {
			t.Fatalf("Recv after the failure = %v, %v, want ErrIncomingOverflow", m, err)
		}
	})
}

// A goroutine that receives synchronously (no dispatcher) must not wait for
// the consumer of a full queue: the delivery is handed over and the order of
// the queue is kept.
func TestSynchronousReceiverHandsOverStalledDelivery(t *testing.T) {
	client, server := newNegotiatedPair(t, 32, 64, AdvancedOptions{IncomingQueueSize: 2}, AdvancedOptions{})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	const flood = 6
	serveDone := make(chan error, 1)
	go func() {
		serveDone <- server.Serve(ctx, HandlerFunc(func(ctx context.Context, w *ResponseWriter, _ *Request) {
			for i := 0; i < flood; i++ {
				if err := server.Send(ctx, []byte(fmt.Sprintf("m%d", i))); err != nil {
					w.WriteError(1, []byte(err.Error()))
					return
				}
			}
			_, _ = w.Write([]byte("done"))
		}))
	}()
	defer func() {
		cancel()
		<-serveDone
	}()

	if client.dispatchStarted.Load() {
		t.Fatal("the client unexpectedly runs a dispatcher")
	}
	callDone := make(chan error, 1)
	go func() {
		m, err := client.Call(ctx, 1, nil)
		if err == nil {
			if string(m.Bytes()) != "done" {
				err = fmt.Errorf("response %q", m.Bytes())
			}
			m.Release()
		}
		callDone <- err
	}()
	// The flood overflows the client's queue of two while the client waits
	// for its response: the response is stuck behind the full queue until
	// the application consumes the messages.
	select {
	case err := <-callDone:
		t.Fatalf("Call finished before the flood was consumed: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	for i := 0; i < flood; i++ {
		if got := recvString(t, client.Stream()); got != fmt.Sprintf("m%d", i) {
			t.Fatalf("message %d = %q", i, got)
		}
	}
	if err := <-callDone; err != nil {
		t.Fatalf("Call: %v", err)
	}
}

func TestStreamOrderingAcrossModes(t *testing.T) {
	for _, dispatcher := range []bool{false, true} {
		name := "sync"
		if dispatcher {
			name = "dispatcher"
		}
		t.Run(name, func(t *testing.T) {
			sender, receiver := newNegotiatedPair(t, 16, 32, AdvancedOptions{}, AdvancedOptions{})
			if dispatcher {
				if err := receiver.startDispatcher(); err != nil {
					t.Fatal(err)
				}
			}
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			const n = 400
			errCh := make(chan error, 1)
			go func() {
				for i := 0; i < n; i++ {
					// Sizes alternate between a single chunk and many chunks.
					size := 8 + (i%7)*40
					p := patternedBytes(size, byte(i))
					p[0], p[1] = byte(i), byte(i>>8)
					if err := sender.Send(ctx, p); err != nil {
						errCh <- err
						return
					}
				}
				errCh <- nil
			}()
			for i := 0; i < n; i++ {
				m, err := receiver.Recv(ctx)
				if err != nil {
					t.Fatalf("Recv %d: %v", i, err)
				}
				if got := int(m.Bytes()[0]) | int(m.Bytes()[1])<<8; got != i {
					t.Fatalf("message %d arrived as message %d", i, got)
				}
				m.Release()
			}
			if err := <-errCh; err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestStatsCountMessagesAndBytes(t *testing.T) {
	primary, secondary := newNegotiatedPair(t, 16, 16, AdvancedOptions{}, AdvancedOptions{})
	out, in := openStreamPair(t, primary, secondary)
	ctx := context.Background()
	for _, p := range [][]byte{nil, []byte("abc"), make([]byte, 50)} {
		if err := out.Send(ctx, p); err != nil {
			t.Fatal(err)
		}
		m, err := in.Recv(ctx)
		if err != nil {
			t.Fatal(err)
		}
		m.Release()
	}
	if err := primary.Send(ctx, []byte("link")); err != nil {
		t.Fatal(err)
	}
	if got := primary.Stats(); got.Messages != 4 || got.MessageBytes != 3+50+4 {
		t.Fatalf("Stats = %+v, want 4 messages and 57 bytes", got)
	}
	if got := secondary.Stats(); got.Messages != 0 {
		t.Fatalf("receiver Stats = %+v", got)
	}
}

func TestServeBoundsConcurrentHandlers(t *testing.T) {
	const workers = 4
	client, server := newNegotiatedPair(t, workers, 64, AdvancedOptions{}, AdvancedOptions{})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	var running, peak atomic.Int32
	release := make(chan struct{})
	serveDone := make(chan error, 1)
	go func() {
		serveDone <- server.Serve(ctx, HandlerFunc(func(_ context.Context, w *ResponseWriter, req *Request) {
			n := running.Add(1)
			for {
				p := peak.Load()
				if n <= p || peak.CompareAndSwap(p, n) {
					break
				}
			}
			<-release
			running.Add(-1)
			_, _ = w.Write(req.Payload())
		}))
	}()

	const calls = 3 * workers
	var wg sync.WaitGroup
	errs := make(chan error, calls)
	for i := 0; i < calls; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			m, err := client.Call(ctx, 1, []byte{byte(i)})
			if err == nil {
				if m.Len() != 1 || m.Bytes()[0] != byte(i) {
					err = fmt.Errorf("call %d got %v", i, m.Bytes())
				}
				m.Release()
			}
			errs <- err
		}(i)
	}
	eventually(t, "all workers busy", func() bool { return running.Load() == workers })
	time.Sleep(50 * time.Millisecond)
	if got := peak.Load(); got != workers {
		t.Fatalf("peak concurrent handlers = %d, want BufferCount = %d", got, workers)
	}
	close(release)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	cancel()
	if err := <-serveDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("Serve = %v", err)
	}
}

func TestServeWaitsForRunningHandlers(t *testing.T) {
	client, server := newNegotiatedPair(t, 16, 64, AdvancedOptions{}, AdvancedOptions{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	entered := make(chan struct{})
	release := make(chan struct{})
	serveDone := make(chan error, 1)
	go func() {
		serveDone <- server.Serve(ctx, HandlerFunc(func(_ context.Context, w *ResponseWriter, _ *Request) {
			close(entered)
			<-release
		}))
	}()
	callCtx, callCancel := context.WithTimeout(ctx, 200*time.Millisecond)
	defer callCancel()
	go func() { _, _ = client.Call(callCtx, 1, nil) }()
	<-entered
	cancel()
	select {
	case err := <-serveDone:
		t.Fatalf("Serve returned (%v) while a handler was still running", err)
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	if err := waitErr(t, serveDone, 5*time.Second); !errors.Is(err, context.Canceled) {
		t.Fatalf("Serve = %v, want context.Canceled", err)
	}
}

// A handler that waits for a later request must not deadlock Serve: the
// handlers start inline and escalate to worker goroutines when one is slow.
func TestServeSlowHandlerDoesNotBlockLaterRequests(t *testing.T) {
	for _, end := range []string{"cancel", "peerClose", "linkClose"} {
		t.Run(end, func(t *testing.T) {
			client, server := newNegotiatedPair(t, 16, 64, AdvancedOptions{}, AdvancedOptions{})
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			gate := make(chan struct{})
			var openGate sync.Once
			mux := NewMux()
			mux.HandleFunc(1, func(ctx context.Context, w *ResponseWriter, _ *Request) {
				select {
				case <-gate:
					_, _ = w.Write([]byte("slow"))
				case <-ctx.Done():
				}
			})
			mux.HandleFunc(2, func(_ context.Context, w *ResponseWriter, _ *Request) {
				openGate.Do(func() { close(gate) })
				_, _ = w.Write([]byte("fast"))
			})
			serveCtx, stop := context.WithCancel(ctx)
			defer stop()
			serveDone := make(chan error, 1)
			go func() { serveDone <- server.Serve(serveCtx, mux) }()

			results := make(chan string, 2)
			call := func(method uint64) {
				m, err := client.Call(ctx, method, nil)
				if err != nil {
					results <- err.Error()
					return
				}
				results <- string(m.Bytes())
				m.Release()
			}
			go call(1)
			time.Sleep(10 * time.Millisecond) // the slow handler is running inline
			go call(2)
			got := []string{<-results, <-results}
			if !(got[0] == "fast" && got[1] == "slow" || got[0] == "slow" && got[1] == "fast") {
				t.Fatalf("results = %v", got)
			}

			// Serve still serves, and ends with the right result.
			m, err := client.Call(ctx, 2, nil)
			if err != nil || string(m.Bytes()) != "fast" {
				t.Fatalf("Call after the escalation = %v, %v", m, err)
			}
			m.Release()
			var want error
			switch end {
			case "cancel":
				stop()
				want = context.Canceled
			case "peerClose":
				if err := client.Close(); err != nil {
					t.Fatal(err)
				}
				want = nil
			case "linkClose":
				if err := server.Close(); err != nil {
					t.Fatal(err)
				}
				want = ErrClosed
			}
			err = waitErr(t, serveDone, 5*time.Second)
			if want == nil && err != nil || want != nil && !errors.Is(err, want) {
				t.Fatalf("Serve = %v, want %v", err, want)
			}
		})
	}
}

// An escalation (an inline handler outliving inlineLimit) is an episode, not
// a permanent mode switch: once the slow handler is done, Serve runs requests
// inline again, and allocates nothing.
func TestServeReturnsToInlineAfterEscalation(t *testing.T) {
	client, server := newNegotiatedPair(t, 16, 64, AdvancedOptions{}, AdvancedOptions{})
	ctx, cancel := context.WithCancel(context.Background())
	serveDone := make(chan error, 1)

	// The slow handler returns once an escalation happened after it started.
	// Its own escalation is what it waits for; other escalations (a fast
	// handler outliving inlineLimit on a loaded machine) do not count.
	mux := NewMux()
	mux.HandleFunc(1, func(_ context.Context, w *ResponseWriter, _ *Request) {
		start := server.stats.serveEscalations.Load()
		deadline := time.Now().Add(10 * time.Second)
		for server.stats.serveEscalations.Load() == start && time.Now().Before(deadline) {
			time.Sleep(50 * time.Microsecond)
		}
		_, _ = w.Write([]byte("slow"))
	})
	mux.HandleFunc(2, func(_ context.Context, w *ResponseWriter, _ *Request) {
		_, _ = w.Write([]byte("fast"))
	})
	go func() { serveDone <- server.Serve(ctx, mux) }()
	defer func() {
		cancel()
		if err := waitErr(t, serveDone, 5*time.Second); !errors.Is(err, context.Canceled) {
			t.Errorf("Serve = %v, want context.Canceled", err)
		}
	}()

	call := func(method uint64, want string) {
		t.Helper()
		m, err := client.Call(context.Background(), method, nil)
		if err != nil || string(m.Bytes()) != want {
			t.Fatalf("Call(%d) = %v, %v; want %q", method, m, err, want)
		}
		m.Release()
	}

	for episode := 1; episode <= 3; episode++ {
		before := server.stats.serveEscalations.Load()
		call(1, "slow")
		if got := server.stats.serveEscalations.Load(); got <= before {
			t.Fatalf("episode %d: the slow handler did not escalate", episode)
		}
		// Serve returns to inline execution: within a short run of
		// sequential requests one finds nothing in flight and runs inline
		// (the slow handler's slot is released before its response is sent,
		// so normally it is the very first one).
		inline := false
		for i := 0; i < 50 && !inline; i++ {
			pooled := server.stats.servePooled.Load()
			call(2, "fast")
			inline = server.stats.servePooled.Load() == pooled
		}
		if !inline {
			t.Fatalf("episode %d: requests never returned to inline execution", episode)
		}
		// And it stays inline: a fast handler may outlive inlineLimit on a
		// loaded machine and escalate by itself, which sends a few requests
		// to workers, but never a lasting share of them.
		pooled := server.stats.servePooled.Load()
		for range 500 {
			call(2, "fast")
		}
		if got := server.stats.servePooled.Load() - pooled; got > 50 {
			t.Fatalf("episode %d: %d of 500 requests went to workers after the episode", episode, got)
		}
	}

	// Serve allocates nothing once it is back in inline mode. A fast handler
	// may still outlive inlineLimit on a loaded machine and escalate during a
	// measurement (a goroutine start allocates), so only a window in which no
	// escalation happened counts; a few disturbed windows in a row are noise.
	var failure error
	measureCall := func() {
		m, err := client.Call(context.Background(), 2, nil)
		if err != nil {
			failure = err
			return
		}
		m.Release()
	}
	measured := false
	for attempt := 0; attempt < 8 && !measured; attempt++ {
		before := server.stats.serveEscalations.Load()
		allocs := measureAllocs(t, measureCall)
		if failure != nil {
			t.Fatal(failure)
		}
		if server.stats.serveEscalations.Load() != before {
			continue
		}
		measured = true
		if allocs != 0 {
			t.Fatalf("Call after escalation episodes allocates %v per operation, want 0", allocs)
		}
	}
	if !measured {
		t.Log("every allocation window was disturbed by an escalation on this loaded machine; the exact check is covered by the ZeroAlloc tests")
	}
}

// Escalated slow handlers count against BufferCount: with many slow handlers
// in flight, no more than BufferCount run at once, whatever mix of inline
// and pooled execution produced them.
func TestServeConcurrencyBoundHoldsAcrossEscalations(t *testing.T) {
	const workers = 4
	client, server := newNegotiatedPair(t, workers, 64, AdvancedOptions{}, AdvancedOptions{})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	var running, peak atomic.Int32
	release := make(chan struct{})
	serveDone := make(chan error, 1)
	go func() {
		serveDone <- server.Serve(ctx, HandlerFunc(func(_ context.Context, w *ResponseWriter, _ *Request) {
			n := running.Add(1)
			for p := peak.Load(); n > p && !peak.CompareAndSwap(p, n); p = peak.Load() {
			}
			<-release
			running.Add(-1)
		}))
	}()
	var wg sync.WaitGroup
	for i := 0; i < 4*workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if m, err := client.Call(ctx, 1, nil); err == nil {
				m.Release()
			}
		}()
	}
	eventually(t, "all slots busy", func() bool { return running.Load() == workers })
	time.Sleep(50 * time.Millisecond)
	if got := peak.Load(); got != workers {
		t.Fatalf("peak concurrent handlers = %d, want %d", got, workers)
	}
	close(release)
	wg.Wait()
	cancel()
	if err := waitErr(t, serveDone, 5*time.Second); !errors.Is(err, context.Canceled) {
		t.Fatalf("Serve = %v", err)
	}
}
