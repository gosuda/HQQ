package hqq

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"testing"
	"time"

	"gosuda.org/hqq/internal/protocol"
)

// serveAsync runs l.Serve(ctx, h) on a goroutine and returns its result.
func serveAsync(l *AdvancedLink, ctx context.Context, h Handler) <-chan error {
	done := make(chan error, 1)
	go func() { done <- l.Serve(ctx, h) }()
	return done
}

// callAsync runs s.CallWith in the background and returns its error (the
// response is released).
func callAsync(ctx context.Context, s *Stream, method uint64, req []byte, opts CallOptions) <-chan error {
	done := make(chan error, 1)
	go func() {
		m, err := s.CallWith(ctx, method, req, opts)
		if err == nil {
			m.Release()
		}
		done <- err
	}()
	return done
}

type reqCtxKey struct{}

// stream0Cases runs fn on the link-level stream and on an opened stream.
func forEachStream(t *testing.T, bufferCount, bufferSize int, primaryFeatures, secondaryFeatures uint64, fn func(t *testing.T, client *Stream, server, clientLink *AdvancedLink)) {
	t.Helper()
	for _, name := range []string{"stream0", "stream"} {
		t.Run(name, func(t *testing.T) {
			client, server := newNegotiatedPairFeatures(t, bufferCount, bufferSize, AdvancedOptions{}, AdvancedOptions{}, primaryFeatures, secondaryFeatures)
			out := client.Stream()
			if name == "stream" {
				out, _ = openStreamPair(t, client, server)
			}
			fn(t, out, server, client)
		})
	}
}

func TestMetaBlockWireFormat(t *testing.T) {
	var block [metaMaxBlock]byte
	opts := CallOptions{
		Caller: 0x1122334455667788,
		Trace:  [16]byte{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15},
	}
	got := putMeta(&block, 1_000_000_000, opts)
	const want = "26000000" + // tlvLen = 38, reserved = 0
		"0108" + "00ca9a3b00000000" + // tag 1: budget 1e9 ns
		"0208" + "8877665544332211" + // tag 2: caller
		"0310" + "000102030405060708090a0b0c0d0e0f" // tag 3: trace
	if hex.EncodeToString(got) != want {
		t.Fatalf("block = %x\nwant    %s", got, want)
	}
	if len(got) != metaMaxBlock {
		t.Fatalf("len = %d, want metaMaxBlock %d", len(got), metaMaxBlock)
	}

	meta, n, err := parseMeta(append(got, "payload"...))
	if err != nil || n != len(got) {
		t.Fatalf("parseMeta = %d, %v", n, err)
	}
	if !meta.hasBudget || meta.budget != 1_000_000_000 || meta.caller != opts.Caller || meta.trace != opts.Trace {
		t.Fatalf("parseMeta = %+v", meta)
	}

	// Only the deadline: header + one TLV.
	block = [metaMaxBlock]byte{}
	if got := hex.EncodeToString(putMeta(&block, 5, CallOptions{})); got != "0a000000"+"0108"+"0500000000000000" {
		t.Fatalf("deadline-only block = %s", got)
	}
	// Nothing to send.
	block = [metaMaxBlock]byte{}
	if putMeta(&block, 0, CallOptions{}) != nil {
		t.Fatal("empty options must not produce a block")
	}
}

func TestParseMetaRejectsMalformedBlocks(t *testing.T) {
	good := func() []byte {
		var block [metaMaxBlock]byte
		return append([]byte(nil), putMeta(&block, 7, CallOptions{Caller: 9})...)
	}
	cases := map[string][]byte{
		"shorter than the header": {1, 0},
		"length beyond message":   {0xff, 0, 0, 0, 1, 8},
		"tlv header cut":          {1, 0, 0, 0, 1},
		"tlv value cut":           {3, 0, 0, 0, 1, 8, 0},
		"budget length":           {6, 0, 0, 0, 1, 4, 0, 0, 0, 0},
		"caller length":           {6, 0, 0, 0, 2, 4, 0, 0, 0, 0},
		"trace length":            {6, 0, 0, 0, 3, 4, 0, 0, 0, 0},
	}
	for name, block := range cases {
		if _, _, err := parseMeta(block); !errors.Is(err, ErrProtocol) {
			t.Errorf("%s: parseMeta = %v, want ErrProtocol", name, err)
		}
	}
	// Unknown tags and a reserved field that is not zero are ignored.
	block := []byte{0x0f, 0, 0xaa, 0xbb, 0x63, 3, 1, 2, 3, 0x02, 0x08, 5, 0, 0, 0, 0, 0, 0, 0}
	meta, n, err := parseMeta(append(block, "x"...))
	if err != nil || n != len(block) || meta.caller != 5 || meta.hasBudget {
		t.Fatalf("parseMeta with unknown tag = %+v, %d, %v", meta, n, err)
	}
	if _, _, err := parseMeta(good()); err != nil {
		t.Fatal(err)
	}
}

func TestNegotiatesProtocol11Features(t *testing.T) {
	client, server := newNegotiatedPair(t, 8, 64, AdvancedOptions{}, AdvancedOptions{})
	want := supportedFeatures
	for _, l := range []*AdvancedLink{client, server} {
		if got := l.NegotiatedFeatures(); got != want {
			t.Fatalf("%v: features = %#x, want %#x", l.Mode(), got, want)
		}
		if v := l.NegotiatedVersion(); v != (ProtocolVersion{Major: 1, Minor: 1}) {
			t.Fatalf("%v: version = %+v, want 1.1", l.Mode(), v)
		}
	}
	if FeatureCancel != 1<<2 || FeatureMetadata != 1<<3 || AdvFlagMeta != 1<<6 || protocol.OpCancel != 0x07 {
		t.Fatal("protocol 1.1 constants changed")
	}
}

func TestCancelReachesHandler(t *testing.T) {
	forEachStream(t, 16, 64, 0, 0, func(t *testing.T, out *Stream, server, client *AdvancedLink) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		entered := make(chan struct{}, 4)
		handlerErr := make(chan error, 4)
		serveDone := serveAsync(server, ctx, HandlerFunc(func(ctx context.Context, w *ResponseWriter, req *Request) {
			if req.Method() == 2 {
				_, _ = w.Write([]byte("pong"))
				return
			}
			entered <- struct{}{}
			select {
			case <-ctx.Done():
				handlerErr <- ctx.Err()
			case <-time.After(5 * time.Second):
				handlerErr <- errors.New("handler context was not canceled")
			}
			_, _ = w.Write([]byte("late"))
		}))

		callCtx, cancelCall := context.WithCancel(ctx)
		call := callAsync(callCtx, out, 1, []byte("slow"), CallOptions{})
		<-entered
		cancelCall()
		if err := waitErr(t, call, 5*time.Second); !errors.Is(err, context.Canceled) {
			t.Fatalf("Call = %v, want context.Canceled", err)
		}
		if err := waitErr(t, handlerErr, 5*time.Second); !errors.Is(err, context.Canceled) {
			t.Fatalf("handler ctx.Err() = %v, want context.Canceled", err)
		}
		if got := server.Stats().CanceledRequests; got != 1 {
			t.Fatalf("CanceledRequests = %d, want 1", got)
		}

		// The handler returned after the cancel: no response was sent for
		// it, and the link keeps working.
		m, err := out.Call(ctx, 2, nil)
		if err != nil || string(m.Bytes()) != "pong" {
			t.Fatalf("second Call = %v, %v", m, err)
		}
		m.Release()
		if got := server.Stats().Responses; got != 1 {
			t.Fatalf("Responses = %d, want 1 (the canceled request is not answered)", got)
		}
		if got := client.Stats().AbandonedResponses; got != 0 {
			t.Fatalf("AbandonedResponses = %d, want 0", got)
		}
		cancel()
		if err := waitErr(t, serveDone, 5*time.Second); !errors.Is(err, context.Canceled) {
			t.Fatalf("Serve = %v, want context.Canceled", err)
		}
	})
}

func TestCancelAfterResponseIsIgnored(t *testing.T) {
	client, server := newNegotiatedPair(t, 16, 64, AdvancedOptions{}, AdvancedOptions{})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	serveDone := serveAsync(server, ctx, HandlerFunc(func(_ context.Context, w *ResponseWriter, _ *Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	callCtx, cancelCall := context.WithCancel(ctx)
	m, err := client.Call(callCtx, 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	m.Release()
	cancelCall() // the call is over: nothing is sent
	// A forged cancel for a request that is not in flight is ignored too.
	if err := client.sendPacket(ctx, protocol.NewPacket(protocol.OpCancel, 0, 12345)); err != nil {
		t.Fatal(err)
	}
	m, err = client.Call(ctx, 1, nil)
	if err != nil {
		t.Fatalf("Call after a stray cancel = %v", err)
	}
	m.Release()
	if got := server.Stats().CanceledRequests; got != 0 {
		t.Fatalf("CanceledRequests = %d, want 0", got)
	}
	cancel()
	waitErr(t, serveDone, 5*time.Second)
}

// TestCanceledQueuedRequestIsNotHandled cancels a request that waits in the
// request queue: its handler never runs.
func TestCanceledQueuedRequestIsNotHandled(t *testing.T) {
	client, server := newNegotiatedPair(t, 16, 64, AdvancedOptions{}, AdvancedOptions{})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	// The dispatcher routes requests into the queue although nobody serves.
	if err := server.startDispatcher(); err != nil {
		t.Fatal(err)
	}
	callCtx, cancelCall := context.WithCancel(ctx)
	call := callAsync(callCtx, client.Stream(), 1, []byte("never"), CallOptions{})
	eventually(t, "the request to be queued", func() bool { return len(server.requests) == 1 })
	cancelCall()
	if err := waitErr(t, call, 5*time.Second); !errors.Is(err, context.Canceled) {
		t.Fatalf("Call = %v", err)
	}
	eventually(t, "the cancel to arrive", func() bool { return server.Stats().CanceledRequests == 1 })

	handled := make(chan uint64, 4)
	serveDone := serveAsync(server, ctx, HandlerFunc(func(_ context.Context, w *ResponseWriter, req *Request) {
		handled <- req.Method()
		_, _ = w.Write([]byte("ok"))
	}))
	m, err := client.Call(ctx, 2, nil)
	if err != nil {
		t.Fatal(err)
	}
	m.Release()
	if len(handled) != 1 || <-handled != 2 {
		t.Fatal("the canceled request must be skipped: only the request of method 2 is handled")
	}
	cancel()
	waitErr(t, serveDone, 5*time.Second)
}

func TestDeadlineExpiryCancelsHandlerContext(t *testing.T) {
	// Only the deadline travels (no FeatureCancel): the handler context ends
	// by its own timer, with DeadlineExceeded.
	forEachStream(t, 16, 64, legacyFeatures|FeatureMetadata, 0, func(t *testing.T, out *Stream, server, _ *AdvancedLink) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		type observed struct {
			err         error
			hasDeadline bool
			remaining   time.Duration
		}
		seen := make(chan observed, 1)
		serveDone := serveAsync(server, ctx, HandlerFunc(func(ctx context.Context, w *ResponseWriter, _ *Request) {
			var o observed
			var d time.Time
			d, o.hasDeadline = ctx.Deadline()
			o.remaining = time.Until(d)
			select {
			case <-ctx.Done():
			case <-time.After(5 * time.Second):
			}
			o.err = ctx.Err()
			seen <- o
		}))
		callCtx, cancelCall := context.WithTimeout(ctx, 300*time.Millisecond)
		defer cancelCall()
		start := time.Now()
		_, err := out.Call(callCtx, 1, []byte("x"))
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Call = %v, want DeadlineExceeded", err)
		}
		o := <-seen
		if !errors.Is(o.err, context.DeadlineExceeded) {
			t.Fatalf("handler ctx.Err() = %v, want DeadlineExceeded", o.err)
		}
		if !o.hasDeadline || o.remaining <= 0 || o.remaining > 300*time.Millisecond {
			t.Fatalf("handler deadline: ok=%v remaining=%v, want within (0, 300ms]", o.hasDeadline, o.remaining)
		}
		if elapsed := time.Since(start); elapsed > 3*time.Second {
			t.Fatalf("took %v", elapsed)
		}
		cancel()
		waitErr(t, serveDone, 5*time.Second)
	})
}

func TestDeadlineAlsoEndsHandlerWithCancelNegotiated(t *testing.T) {
	client, server := newNegotiatedPair(t, 16, 64, AdvancedOptions{}, AdvancedOptions{})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	result := make(chan error, 1)
	serveDone := serveAsync(server, ctx, HandlerFunc(func(ctx context.Context, _ *ResponseWriter, _ *Request) {
		<-ctx.Done()
		result <- ctx.Err()
	}))
	callCtx, cancelCall := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancelCall()
	if _, err := client.Call(callCtx, 1, nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Call = %v", err)
	}
	// Either the handler's timer or the caller's OpCancel ends it first.
	if err := waitErr(t, result, 5*time.Second); !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, context.Canceled) {
		t.Fatalf("handler ctx.Err() = %v", err)
	}
	cancel()
	waitErr(t, serveDone, 5*time.Second)
}

func TestMetadataSurvivesMultiChunkRequests(t *testing.T) {
	// 32-byte buffers: the 42-byte metadata block itself spans two chunks.
	forEachStream(t, 64, 32, 0, 0, func(t *testing.T, out *Stream, server, _ *AdvancedLink) {
		type observed struct {
			payload     []byte
			caller      uint64
			trace       [16]byte
			hasDeadline bool
			remaining   time.Duration
			err         error
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		seen := make(chan observed, 4)
		serveDone := serveAsync(server, ctx, HandlerFunc(func(ctx context.Context, w *ResponseWriter, req *Request) {
			o := observed{payload: bytes.Clone(req.Payload()), caller: req.Caller(), trace: req.Trace()}
			var d time.Time
			d, o.hasDeadline = ctx.Deadline()
			o.remaining = time.Until(d)
			o.err = ctx.Err()
			seen <- o
			_, _ = w.Write(req.Payload())
		}))

		opts := CallOptions{Caller: 0xCAFE, Trace: [16]byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16}}
		for _, size := range []int{0, 1, 31, 32, 33, 1000} {
			payload := patternedBytes(size, 3)
			callCtx, cancelCall := context.WithTimeout(ctx, 5*time.Second)
			m, err := out.CallWith(callCtx, 1, payload, opts)
			cancelCall()
			if err != nil {
				t.Fatalf("size %d: %v", size, err)
			}
			if !bytes.Equal(m.Bytes(), payload) {
				t.Fatalf("size %d: response differs", size)
			}
			m.Release()
			o := <-seen
			if !bytes.Equal(o.payload, payload) {
				t.Fatalf("size %d: handler saw a different payload (%d bytes)", size, len(o.payload))
			}
			if o.caller != opts.Caller || o.trace != opts.Trace {
				t.Fatalf("size %d: caller=%#x trace=%x", size, o.caller, o.trace)
			}
			if !o.hasDeadline || o.remaining < 2*time.Second || o.remaining > 5*time.Second || o.err != nil {
				t.Fatalf("size %d: deadline ok=%v remaining=%v err=%v, want about 5s", size, o.hasDeadline, o.remaining, o.err)
			}
		}

		// A plain Call sends no caller or trace, and Background no deadline.
		if m, err := out.Call(ctx, 1, []byte("plain")); err != nil {
			t.Fatal(err)
		} else {
			m.Release()
		}
		o := <-seen
		if o.caller != 0 || o.trace != ([16]byte{}) || string(o.payload) != "plain" {
			t.Fatalf("plain call: caller=%#x trace=%x payload=%q", o.caller, o.trace, o.payload)
		}
		// The serve context has a deadline of its own: it is the handler's.
		if !o.hasDeadline {
			t.Fatal("handler must inherit the deadline of the Serve context")
		}
		cancel()
		waitErr(t, serveDone, 5*time.Second)
	})
}

func TestCallerAndTraceOnlyHaveNoDeadline(t *testing.T) {
	client, server := newNegotiatedPair(t, 16, 64, AdvancedOptions{}, AdvancedOptions{})
	got := make(chan [3]any, 1)
	serveDone := serveAsync(server, context.Background(), HandlerFunc(func(ctx context.Context, _ *ResponseWriter, req *Request) {
		_, ok := ctx.Deadline()
		got <- [3]any{req.Caller(), req.Trace(), ok}
	}))
	trace := [16]byte{0xAA}
	m, err := client.CallWith(context.Background(), 1, []byte("x"), CallOptions{Caller: 7, Trace: trace})
	if err != nil {
		t.Fatal(err)
	}
	m.Release()
	v := <-got
	if v[0] != uint64(7) || v[1] != trace || v[2] != false {
		t.Fatalf("handler saw caller=%v trace=%v deadline=%v", v[0], v[1], v[2])
	}
	_ = server.Close()
	waitErr(t, serveDone, 5*time.Second)
}

func TestHandlerContextValueDelegatesToServeContext(t *testing.T) {
	client, server := newNegotiatedPair(t, 16, 64, AdvancedOptions{}, AdvancedOptions{})
	serveCtx, cancel := context.WithCancel(context.WithValue(context.Background(), reqCtxKey{}, "serve"))
	defer cancel()
	got := make(chan any, 1)
	serveDone := serveAsync(server, serveCtx, HandlerFunc(func(ctx context.Context, _ *ResponseWriter, _ *Request) {
		got <- ctx.Value(reqCtxKey{})
	}))
	m, err := client.Call(context.Background(), 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	m.Release()
	if v := <-got; v != "serve" {
		t.Fatalf("ctx.Value = %v, want the Serve context's value", v)
	}
	cancel()
	waitErr(t, serveDone, 5*time.Second)
}

func TestHandlerContextEndsWithServe(t *testing.T) {
	for _, how := range []string{"serve ctx", "link close"} {
		t.Run(how, func(t *testing.T) {
			client, server := newNegotiatedPair(t, 16, 64, AdvancedOptions{}, AdvancedOptions{})
			serveCtx, cancel := context.WithCancel(context.Background())
			defer cancel()
			entered := make(chan struct{})
			result := make(chan error, 1)
			serveDone := serveAsync(server, serveCtx, HandlerFunc(func(ctx context.Context, _ *ResponseWriter, _ *Request) {
				close(entered)
				<-ctx.Done()
				result <- ctx.Err()
			}))
			callCtx, cancelCall := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancelCall()
			call := callAsync(callCtx, client.Stream(), 1, nil, CallOptions{})
			<-entered
			if how == "serve ctx" {
				cancel()
			} else {
				_ = server.Close()
			}
			if err := waitErr(t, result, 5*time.Second); !errors.Is(err, context.Canceled) {
				t.Fatalf("handler ctx.Err() = %v, want Canceled", err)
			}
			err := waitErr(t, serveDone, 5*time.Second)
			if how == "serve ctx" && !errors.Is(err, context.Canceled) || how == "link close" && !errors.Is(err, ErrClosed) {
				t.Fatalf("Serve = %v", err)
			}
			cancelCall()
			<-call
		})
	}
}

func TestHandlerContextCanceledOnStreamClose(t *testing.T) {
	for _, closer := range []string{"peer closes", "local closes"} {
		t.Run(closer, func(t *testing.T) {
			client, server := newNegotiatedPair(t, 16, 64, AdvancedOptions{}, AdvancedOptions{})
			out, in := openStreamPair(t, client, server)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			entered := make(chan struct{}, 1)
			result := make(chan error, 1)
			serveDone := serveAsync(server, ctx, HandlerFunc(func(ctx context.Context, _ *ResponseWriter, _ *Request) {
				entered <- struct{}{}
				select {
				case <-ctx.Done():
					result <- ctx.Err()
				case <-time.After(5 * time.Second):
					result <- errors.New("context not canceled")
				}
			}))
			// A request of an unrelated stream is not affected.
			other, _ := openStreamPair(t, client, server)
			otherCall := callAsync(ctx, other, 1, nil, CallOptions{})
			<-entered
			call := callAsync(ctx, out, 1, nil, CallOptions{})
			<-entered
			if closer == "peer closes" {
				if err := out.Close(); err != nil {
					t.Fatal(err)
				}
			} else if err := in.Close(); err != nil {
				t.Fatal(err)
			}
			if err := waitErr(t, result, 5*time.Second); !errors.Is(err, context.Canceled) {
				t.Fatalf("handler ctx.Err() = %v, want Canceled", err)
			}
			select {
			case err := <-result:
				t.Fatalf("the request of another stream was canceled too: %v", err)
			case <-time.After(100 * time.Millisecond):
			}
			waitErr(t, call, 5*time.Second)
			cancel()
			<-otherCall
			waitErr(t, serveDone, 5*time.Second)
		})
	}
}

// TestRetainedContextIsCanceled checks that a handler context that escaped
// (Done was called) observes the cancellation made when the handler returns,
// and that its memory is not reused for a later request.
func TestRetainedContextIsCanceled(t *testing.T) {
	client, server := newNegotiatedPair(t, 16, 64, AdvancedOptions{}, AdvancedOptions{})
	retained := make(chan context.Context, 1)
	serveDone := serveAsync(server, context.Background(), HandlerFunc(func(ctx context.Context, _ *ResponseWriter, _ *Request) {
		_ = ctx.Done()
		retained <- ctx
	}))
	for range 3 {
		m, err := client.Call(context.Background(), 1, nil)
		if err != nil {
			t.Fatal(err)
		}
		m.Release()
		ctx := <-retained
		select {
		case <-ctx.Done():
		case <-time.After(5 * time.Second):
			t.Fatal("retained context was not canceled after the handler returned")
		}
		if !errors.Is(ctx.Err(), context.Canceled) {
			t.Fatalf("retained ctx.Err() = %v", ctx.Err())
		}
	}
	_ = server.Close()
	waitErr(t, serveDone, 5*time.Second)
}

func TestShutdownDrainsAndRejects(t *testing.T) {
	for _, dispatcher := range []bool{false, true} {
		name := "sync"
		if dispatcher {
			name = "dispatcher"
		}
		t.Run(name, func(t *testing.T) {
			client, server := newNegotiatedPair(t, 16, 64, AdvancedOptions{}, AdvancedOptions{})
			if dispatcher {
				if err := server.startDispatcher(); err != nil {
					t.Fatal(err)
				}
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			entered := make(chan struct{}, 1)
			release := make(chan struct{})
			serveDone := serveAsync(server, context.Background(), HandlerFunc(func(_ context.Context, w *ResponseWriter, req *Request) {
				if req.Method() == 1 {
					entered <- struct{}{}
					<-release
				}
				_, _ = w.Write([]byte("done"))
			}))
			inflight := callAsync(ctx, client.Stream(), 1, nil, CallOptions{})
			<-entered

			shutdownDone := make(chan error, 1)
			go func() { shutdownDone <- server.Shutdown(ctx) }()
			eventually(t, "Shutdown to start", func() bool { return server.shuttingDown.Load() })

			// New requests are refused with a dedicated status.
			_, err := client.Call(ctx, 2, nil)
			var re ResponseError
			if !errors.Is(err, ErrShuttingDown) || !errors.As(err, &re) || re.Status != uint64(ErrCodeShuttingDown) {
				t.Fatalf("Call during Shutdown = %v, want a ResponseError matching ErrShuttingDown", err)
			}
			if got := server.Stats().RefusedRequests; got != 1 {
				t.Fatalf("RefusedRequests = %d, want 1", got)
			}
			select {
			case err := <-shutdownDone:
				t.Fatalf("Shutdown returned %v with a handler still running", err)
			case err := <-serveDone:
				t.Fatalf("Serve returned %v before Shutdown completed", err)
			case <-time.After(50 * time.Millisecond):
			}

			close(release)
			if err := waitErr(t, inflight, 5*time.Second); err != nil {
				t.Fatalf("in-flight Call = %v, want its response", err)
			}
			if err := waitErr(t, shutdownDone, 5*time.Second); err != nil {
				t.Fatalf("Shutdown = %v", err)
			}
			if err := waitErr(t, serveDone, 5*time.Second); err != nil {
				t.Fatalf("Serve = %v, want nil after Shutdown", err)
			}
			// Idempotent; a later Serve returns at once; the link still sends.
			if err := server.Shutdown(ctx); err != nil {
				t.Fatalf("second Shutdown = %v", err)
			}
			if err := server.Serve(ctx, HandlerFunc(func(context.Context, *ResponseWriter, *Request) {})); err != nil {
				t.Fatalf("Serve after Shutdown = %v", err)
			}
			if err := server.Send(ctx, []byte("still open")); err != nil {
				t.Fatalf("Send after Shutdown = %v", err)
			}
		})
	}
}

func TestShutdownWakesIdleServe(t *testing.T) {
	for _, dispatcher := range []bool{false, true} {
		name := "sync"
		if dispatcher {
			name = "dispatcher"
		}
		t.Run(name, func(t *testing.T) {
			_, server := newNegotiatedPair(t, 16, 64, AdvancedOptions{}, AdvancedOptions{})
			if dispatcher {
				if err := server.startDispatcher(); err != nil {
					t.Fatal(err)
				}
			}
			serveDone := serveAsync(server, context.Background(), HandlerFunc(func(context.Context, *ResponseWriter, *Request) {}))
			time.Sleep(100 * time.Millisecond) // let Serve park
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			start := time.Now()
			if err := server.Shutdown(ctx); err != nil {
				t.Fatalf("Shutdown = %v", err)
			}
			if err := waitErr(t, serveDone, 2*time.Second); err != nil {
				t.Fatalf("Serve = %v, want nil", err)
			}
			if d := time.Since(start); d > time.Second {
				t.Fatalf("Shutdown+Serve return took %v", d)
			}
		})
	}
}

func TestShutdownContextEndsAndCloseCancelsHandlers(t *testing.T) {
	client, server := newNegotiatedPair(t, 16, 64, AdvancedOptions{}, AdvancedOptions{})
	entered := make(chan struct{}, 1)
	result := make(chan error, 1)
	serveDone := serveAsync(server, context.Background(), HandlerFunc(func(ctx context.Context, _ *ResponseWriter, _ *Request) {
		entered <- struct{}{}
		<-ctx.Done()
		result <- ctx.Err()
	}))
	callCtx, cancelCall := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelCall()
	call := callAsync(callCtx, client.Stream(), 1, nil, CallOptions{})
	<-entered

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err := server.Shutdown(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Shutdown = %v, want DeadlineExceeded while a handler runs", err)
	}
	// Shutdown waiting on a closing link returns ErrClosed.
	late := make(chan error, 1)
	go func() { late <- server.Shutdown(context.Background()) }()
	time.Sleep(20 * time.Millisecond)
	_ = server.Close()
	if err := waitErr(t, result, 5*time.Second); !errors.Is(err, context.Canceled) {
		t.Fatalf("handler ctx.Err() after Close = %v", err)
	}
	if err := waitErr(t, late, 5*time.Second); !errors.Is(err, ErrClosed) {
		t.Fatalf("Shutdown during Close = %v, want ErrClosed", err)
	}
	if err := waitErr(t, serveDone, 5*time.Second); !errors.Is(err, ErrClosed) {
		t.Fatalf("Serve = %v, want ErrClosed", err)
	}
	cancelCall()
	<-call
}

func TestShutdownRequiresNegotiation(t *testing.T) {
	l, _ := newAdvancedPair(t, 8, 64)
	if err := l.Shutdown(context.Background()); !errors.Is(err, ErrNotNegotiated) {
		t.Fatalf("Shutdown before negotiation = %v", err)
	}
}

// TestFeatureFallbacks checks that a peer which does not negotiate the
// protocol 1.1 features sees plain protocol 1.0 requests, in both roles.
func TestFeatureFallbacks(t *testing.T) {
	type expect struct{ cancel, meta bool }
	cases := []struct {
		name               string
		primary, secondary uint64
		want               expect
	}{
		{"all/all", 0, 0, expect{true, true}},
		{"client legacy", legacyFeatures, 0, expect{false, false}},
		{"server legacy", 0, legacyFeatures, expect{false, false}},
		{"no cancel", 0, legacyFeatures | FeatureMetadata, expect{false, true}},
		{"no metadata", legacyFeatures | FeatureCancel, 0, expect{true, false}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client, server := newNegotiatedPairFeatures(t, 16, 64, AdvancedOptions{}, AdvancedOptions{}, tc.primary, tc.secondary)
			if got := client.NegotiatedFeatures()&FeatureCancel != 0; got != tc.want.cancel {
				t.Fatalf("cancel negotiated = %v", got)
			}
			if got := client.NegotiatedFeatures()&FeatureMetadata != 0; got != tc.want.meta {
				t.Fatalf("metadata negotiated = %v", got)
			}
			type observed struct {
				caller      uint64
				trace       [16]byte
				hasDeadline bool
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			serveCtx, stopServe := context.WithCancel(context.Background()) // no deadline of its own
			defer stopServe()
			seen := make(chan observed, 1)
			stop := make(chan struct{})
			ctxErr := make(chan error, 1)
			serveDone := serveAsync(server, serveCtx, HandlerFunc(func(hctx context.Context, _ *ResponseWriter, req *Request) {
				var o observed
				_, o.hasDeadline = hctx.Deadline()
				o.caller, o.trace = req.Caller(), req.Trace()
				seen <- o
				<-stop
				ctxErr <- hctx.Err()
			}))
			callCtx, cancelCall := context.WithTimeout(ctx, 400*time.Millisecond)
			defer cancelCall()
			opts := CallOptions{Caller: 42, Trace: [16]byte{9}}
			call := callAsync(callCtx, client.Stream(), 1, []byte("hello"), opts)
			o := <-seen
			if tc.want.meta {
				if o.caller != 42 || o.trace != opts.Trace || !o.hasDeadline {
					t.Fatalf("metadata missing: %+v", o)
				}
			} else if o.caller != 0 || o.trace != ([16]byte{}) {
				t.Fatalf("metadata leaked to a peer that did not negotiate it: %+v", o)
			}
			// The Serve context has no deadline, so only the caller's sends one.
			if o.hasDeadline != tc.want.meta {
				t.Fatalf("handler has deadline = %v, want %v", o.hasDeadline, tc.want.meta)
			}
			if err := waitErr(t, call, 5*time.Second); !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("Call = %v, want DeadlineExceeded", err)
			}
			// Give a cancel (or an expiring deadline) time to take effect.
			time.Sleep(100 * time.Millisecond)
			close(stop)
			err := waitErr(t, ctxErr, 5*time.Second)
			ended := err != nil
			if want := tc.want.cancel || tc.want.meta; ended != want {
				t.Fatalf("handler context ended = %v (%v), want %v", ended, err, want)
			}
			// With both features the deadline timer and the caller's cancel
			// race to end the context, so the cancel count is not exact.
			if canceled := server.Stats().CanceledRequests; !tc.want.meta && (canceled == 1) != tc.want.cancel {
				t.Fatalf("CanceledRequests = %d, want cancel=%v", canceled, tc.want.cancel)
			}
			stopServe()
			waitErr(t, serveDone, 5*time.Second)
		})
	}
}

// TestCancelFrameOnTheWire checks the exact frames of a call that is
// canceled: the request chunk with its metadata block, then OpCancel.
func TestCancelFrameOnTheWire(t *testing.T) {
	client, server := newNegotiatedPair(t, 16, 128, AdvancedOptions{}, AdvancedOptions{})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	callCtx, cancelCall := context.WithTimeout(ctx, 5*time.Second)
	defer cancelCall()
	opts := CallOptions{Caller: 0x1122334455667788, Trace: [16]byte{1}}
	call := callAsync(callCtx, client.Stream(), 9, []byte("abc"), opts)

	request, err := server.recvPacket(ctx)
	if err != nil {
		t.Fatal(err)
	}
	const wantFlags = AdvFlagBegin | AdvFlagEnd | AdvFlagRequest | AdvFlagMeta
	total := uint64(metaMaxBlock + len("abc"))
	if request.Op() != protocol.OpConnCopy || request.Operand(0) != 0 || request.Operand(2) != total ||
		request.Operand(3) != 0 || request.Operand(4) != total || request.Operand(5) != wantFlags || request.Operand(6) != 9 {
		t.Fatalf("request packet = %x", request[:])
	}
	cancelCall()
	frame, err := server.recvPacket(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := protocol.NewPacket(protocol.OpCancel, 0, request.Operand(1))
	if frame != want {
		t.Fatalf("cancel packet = %x, want %x", frame[:], want[:])
	}
	if err := waitErr(t, call, 5*time.Second); !errors.Is(err, context.Canceled) {
		t.Fatalf("Call = %v", err)
	}
}

// TestProtocol11FramesAreProtocolErrorsWhenNotNegotiated: a peer must not
// send what was not negotiated.
func TestProtocol11FramesAreProtocolErrorsWhenNotNegotiated(t *testing.T) {
	var block [metaMaxBlock]byte
	meta := putMeta(&block, 1000, CallOptions{})
	cases := []struct {
		name string
		send func(ctx context.Context, l *AdvancedLink) error
		want error
	}{
		{"meta flag", func(ctx context.Context, l *AdvancedLink) error {
			return l.sendAdvancedPayload(ctx, 0, 1, AdvFlagRequest|AdvFlagMeta, 1, meta, nil)
		}, ErrProtocol},
		{"cancel", func(ctx context.Context, l *AdvancedLink) error {
			return l.sendPacket(ctx, protocol.NewPacket(protocol.OpCancel, 0, 1))
		}, ErrUnexpectedPacket},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client, server := newNegotiatedPairFeatures(t, 16, 64, AdvancedOptions{}, AdvancedOptions{}, legacyFeatures, 0)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			serveDone := serveAsync(server, ctx, HandlerFunc(func(context.Context, *ResponseWriter, *Request) {
				t.Error("handler must not run")
			}))
			if err := tc.send(ctx, client); err != nil {
				t.Fatal(err)
			}
			if err := waitErr(t, serveDone, 5*time.Second); !errors.Is(err, tc.want) {
				t.Fatalf("Serve = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestMalformedMetadataIsProtocolError(t *testing.T) {
	cases := map[string]struct {
		flags   uint64
		prefix  []byte
		payload []byte
	}{
		"block overruns message":   {AdvFlagRequest | AdvFlagMeta, []byte{0xff, 0, 0, 0, 1, 8}, nil},
		"message shorter than 4":   {AdvFlagRequest | AdvFlagMeta, []byte{1, 0}, nil},
		"budget with wrong length": {AdvFlagRequest | AdvFlagMeta, []byte{6, 0, 0, 0, 1, 4, 0, 0, 0, 0}, nil},
		"meta on a data message":   {AdvFlagMeta, []byte{0, 0, 0, 0}, nil},
		"meta on a response":       {AdvFlagResponse | AdvFlagMeta, []byte{0, 0, 0, 0}, nil},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			client, server := newNegotiatedPair(t, 16, 64, AdvancedOptions{}, AdvancedOptions{})
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if err := client.sendAdvancedPayload(ctx, 0, 1, tc.flags, 1, tc.prefix, tc.payload); err != nil {
				t.Fatal(err)
			}
			_, err := server.Recv(ctx)
			if !errors.Is(err, ErrProtocol) {
				t.Fatalf("receive = %v, want ErrProtocol", err)
			}
		})
	}
}

func TestUnknownMetadataTagsAreSkipped(t *testing.T) {
	client, server := newNegotiatedPair(t, 16, 32, AdvancedOptions{}, AdvancedOptions{})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	got := make(chan [2]any, 1)
	serveDone := serveAsync(server, ctx, HandlerFunc(func(_ context.Context, w *ResponseWriter, req *Request) {
		got <- [2]any{req.Caller(), string(req.Payload())}
	}))
	// A future tag 0x63 with a 3-byte value, then the caller ID.
	block := []byte{0x0f, 0, 0, 0, 0x63, 3, 1, 2, 3, 0x02, 0x08, 5, 0, 0, 0, 0, 0, 0, 0}
	callCtx, cancelCall := context.WithTimeout(ctx, time.Second)
	defer cancelCall()
	if err := client.sendAdvancedPayload(callCtx, 0, client.nextMessageID(), AdvFlagRequest|AdvFlagMeta, 1, block, []byte("payload crossing chunks")); err != nil {
		t.Fatal(err)
	}
	v := <-got
	if v[0] != uint64(5) || v[1] != "payload crossing chunks" {
		t.Fatalf("handler saw %v", v)
	}
	cancel()
	waitErr(t, serveDone, 5*time.Second)
}
