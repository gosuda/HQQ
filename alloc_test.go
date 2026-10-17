package hqq

import (
	"bytes"
	"context"
	"errors"
	"runtime"
	"testing"
	"time"
)

// zeroAllocCase is one steady-state allocation measurement of the Advanced
// message paths.
type zeroAllocCase struct {
	name       string
	size       int
	dispatcher bool
}

func zeroAllocCases() []zeroAllocCase {
	var cases []zeroAllocCase
	for _, size := range []struct {
		name string
		n    int
	}{{"small64B", 64}, {"multiChunk4KiB", 4096}} {
		for _, dispatcher := range []bool{false, true} {
			name := size.name + "/sync"
			if dispatcher {
				name = size.name + "/dispatcher"
			}
			cases = append(cases, zeroAllocCase{name: name, size: size.n, dispatcher: dispatcher})
		}
	}
	return cases
}

// measureAllocs warms the pools up and returns the allocations of fn per
// run: the larger of testing.AllocsPerRun (GOMAXPROCS=1, which rounds down)
// and the exact average over a long run on all Ps, which also covers the
// cross-P hand-offs of the pools and tolerates the odd refill.
func measureAllocs(t *testing.T, fn func()) float64 {
	t.Helper()
	if raceEnabled {
		t.Skip("sync.Pool drops items at random under the race detector")
	}
	for i := 0; i < 500; i++ {
		fn()
	}
	const runs = 20000
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	for i := 0; i < runs; i++ {
		fn()
	}
	runtime.ReadMemStats(&after)
	// A rate below this is the noise of background goroutines (heartbeats),
	// not an allocation per operation.
	const noise = 0.05
	if rate := float64(after.Mallocs-before.Mallocs) / runs; rate > noise {
		return rate
	}
	return testing.AllocsPerRun(2000, fn)
}

func TestStreamSendRecvReleaseZeroAlloc(t *testing.T) {
	for _, tc := range zeroAllocCases() {
		t.Run(tc.name, func(t *testing.T) {
			sender, receiver := newNegotiatedPair(t, 1024, 256, AdvancedOptions{}, AdvancedOptions{})
			if tc.dispatcher {
				if err := receiver.startDispatcher(); err != nil {
					t.Fatal(err)
				}
			}
			ctx := context.Background()
			payload := patternedBytes(tc.size, 3)
			var failure error
			allocs := measureAllocs(t, func() {
				if err := sender.Send(ctx, payload); err != nil {
					failure = err
					return
				}
				m, err := receiver.Recv(ctx)
				if err != nil {
					failure = err
					return
				}
				if m.Len() != tc.size {
					failure = ErrInvalidSize
				}
				m.Release()
			})
			if failure != nil {
				t.Fatal(failure)
			}
			if allocs != 0 {
				t.Fatalf("Send+Recv+Release allocates %v per operation, want 0", allocs)
			}
		})
	}
}

func TestStreamSendRecvReleaseZeroAllocOnStream(t *testing.T) {
	sender, receiver := newNegotiatedPair(t, 1024, 256, AdvancedOptions{}, AdvancedOptions{})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := receiver.ListenStreams(ctx); err != nil {
		t.Fatal(err)
	}
	opened := make(chan *Stream, 1)
	go func() {
		s, err := receiver.AcceptStream(ctx)
		if err != nil {
			t.Error(err)
		}
		opened <- s
	}()
	out, err := sender.OpenStream(ctx)
	if err != nil {
		t.Fatal(err)
	}
	in := <-opened
	if in == nil {
		t.FailNow()
	}
	payload := patternedBytes(4096, 5)
	var failure error
	allocs := measureAllocs(t, func() {
		if err := out.Send(context.Background(), payload); err != nil {
			failure = err
			return
		}
		m, err := in.Recv(context.Background())
		if err != nil {
			failure = err
			return
		}
		m.Release()
	})
	if failure != nil {
		t.Fatal(failure)
	}
	if allocs != 0 {
		t.Fatalf("stream Send+Recv+Release allocates %v per operation, want 0", allocs)
	}
}

func TestStreamCallServeZeroAlloc(t *testing.T) {
	for _, tc := range zeroAllocCases() {
		t.Run(tc.name, func(t *testing.T) {
			client, server := newNegotiatedPair(t, 1024, 256, AdvancedOptions{}, AdvancedOptions{})
			if tc.dispatcher {
				if err := client.startDispatcher(); err != nil {
					t.Fatal(err)
				}
				if err := server.startDispatcher(); err != nil {
					t.Fatal(err)
				}
			}
			serveDone := make(chan error, 1)
			response := patternedBytes(tc.size, 9)
			go func() {
				// A context-free Serve: parking arms no context wake-up.
				serveDone <- server.Serve(context.Background(), HandlerFunc(func(_ context.Context, w *ResponseWriter, req *Request) {
					_, _ = w.Write(response)
				}))
			}()
			defer func() {
				_ = server.Close()
				if err := <-serveDone; !errors.Is(err, ErrClosed) {
					t.Errorf("Serve = %v, want ErrClosed", err)
				}
			}()
			request := patternedBytes(tc.size, 4)
			var failure error
			allocs := measureAllocs(t, func() {
				m, err := client.Call(context.Background(), 7, request)
				if err != nil {
					failure = err
					return
				}
				if !bytes.Equal(m.Bytes(), response) {
					failure = ErrProtocol
				}
				m.Release()
			})
			if failure != nil {
				t.Fatal(failure)
			}
			if allocs != 0 {
				t.Fatalf("Call+Release with Serve allocates %v per operation (both endpoints), want 0", allocs)
			}
		})
	}
}

// TestStreamCallServeProtocol11ZeroAlloc proves that the protocol 1.1
// features cost no allocation per call once negotiated: the deadline budget,
// CallOptions metadata, the pooled per-request context (also when the handler
// polls it), OpCancel registration and the deadline timer.
func TestStreamCallServeProtocol11ZeroAlloc(t *testing.T) {
	type variant struct {
		name     string
		deadline bool
		opts     CallOptions
		onStream bool
	}
	variants := []variant{
		{name: "negotiated"},
		{name: "deadline", deadline: true},
		{name: "options", opts: CallOptions{Caller: 7, Trace: [16]byte{1, 2, 3}}},
		{name: "deadline+options", deadline: true, opts: CallOptions{Caller: 7, Trace: [16]byte{1, 2, 3}}},
		{name: "stream/deadline+options", deadline: true, opts: CallOptions{Caller: 7}, onStream: true},
	}
	for _, v := range variants {
		for _, tc := range zeroAllocCases() {
			t.Run(v.name+"/"+tc.name, func(t *testing.T) {
				client, server := newNegotiatedPair(t, 1024, 256, AdvancedOptions{}, AdvancedOptions{})
				if tc.dispatcher {
					if err := client.startDispatcher(); err != nil {
						t.Fatal(err)
					}
					if err := server.startDispatcher(); err != nil {
						t.Fatal(err)
					}
				}
				stream := client.Stream()
				if v.onStream {
					stream, _ = openStreamPair(t, client, server)
				}
				serveDone := make(chan error, 1)
				response := patternedBytes(tc.size, 9)
				go func() {
					serveDone <- server.Serve(context.Background(), HandlerFunc(func(ctx context.Context, w *ResponseWriter, req *Request) {
						// The handler polls the per-request context and
						// reads the metadata: neither may allocate.
						if ctx.Err() != nil {
							w.WriteError(0, nil)
							return
						}
						_, _ = ctx.Deadline()
						_ = req.Caller()
						_ = req.Trace()
						_, _ = w.Write(response)
					}))
				}()
				defer func() {
					_ = server.Close()
					if err := <-serveDone; !errors.Is(err, ErrClosed) {
						t.Errorf("Serve = %v, want ErrClosed", err)
					}
				}()
				callCtx := context.Background()
				if v.deadline {
					var cancel context.CancelFunc
					callCtx, cancel = context.WithTimeout(context.Background(), time.Hour)
					defer cancel()
				}
				request := patternedBytes(tc.size, 4)
				var failure error
				allocs := measureAllocs(t, func() {
					m, err := stream.CallWith(callCtx, 7, request, v.opts)
					if err != nil {
						failure = err
						return
					}
					if !bytes.Equal(m.Bytes(), response) {
						failure = ErrProtocol
					}
					m.Release()
				})
				if failure != nil {
					t.Fatal(failure)
				}
				// A caller context with a deadline has a Done channel, so a
				// synchronous ring wait that has to park arms a context
				// wake-up, which allocates (as in protocol 1.0, for every
				// cancelable context). How often the wait parks depends on
				// scheduling (on a loaded Linux runner up to ~0.4 per call was
				// seen), so this case tolerates a fraction of calls parking; a
				// per-call allocation would be 1.0 or more.
				limit := 0.0
				if v.deadline && !tc.dispatcher {
					limit = 0.6
				}
				if allocs > limit {
					t.Fatalf("CallWith+Release with Serve allocates %v per operation (both endpoints), want at most %v", allocs, limit)
				}
			})
		}
	}
}

// TestCanceledCallAllocations measures the cancel path: a Call whose context
// ends while the handler runs, with OpCancel reaching the handler. It is not
// allocation-free and is documented as such: the canceled call registers an
// entry in the abandoned set, the caller's cancelable context arms wake-ups,
// and the handler's Done channel is created. The test only bounds it.
func TestCanceledCallAllocations(t *testing.T) {
	client, server := newNegotiatedPair(t, 64, 256, AdvancedOptions{}, AdvancedOptions{})
	serveDone := make(chan error, 1)
	go func() {
		serveDone <- server.Serve(context.Background(), HandlerFunc(func(ctx context.Context, _ *ResponseWriter, _ *Request) {
			<-ctx.Done()
		}))
	}()
	defer func() {
		_ = server.Close()
		<-serveDone
	}()
	request := patternedBytes(64, 4)
	allocs := measureAllocs(t, func() {
		ctx, cancel := context.WithCancel(context.Background())
		time.AfterFunc(50*time.Microsecond, cancel)
		if _, err := client.Call(ctx, 7, request); !errors.Is(err, context.Canceled) {
			t.Errorf("Call = %v, want Canceled", err)
		}
	})
	t.Logf("canceled Call allocates %.1f objects per operation (both endpoints)", allocs)
	if allocs > 40 {
		t.Fatalf("canceled Call allocates %v per operation, want a small bounded number", allocs)
	}
}
