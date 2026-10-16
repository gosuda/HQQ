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
