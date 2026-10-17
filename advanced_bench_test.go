package hqq

import (
	"context"
	"testing"
	"time"
)

func BenchmarkAdvancedLinkLargeCopy(b *testing.B) {
	benchmarkAdvancedSend(b, 4096)
}

func BenchmarkAdvancedLinkSmallMessage(b *testing.B) {
	benchmarkAdvancedSend(b, 64)
}

// benchmarkAdvancedSend measures Send on one endpoint followed by Recv and
// Release on the other, over 256-byte chunks.
func benchmarkAdvancedSend(b *testing.B, payloadSize int) {
	const (
		bufferCount = 1024
		bufferSize  = 256
	)
	primary, secondary := newNegotiatedPair(b, bufferCount, bufferSize, AdvancedOptions{}, AdvancedOptions{})

	payload := make([]byte, payloadSize)
	ctx := context.Background()
	b.SetBytes(int64(payloadSize))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := primary.Send(ctx, payload); err != nil {
			b.Fatalf("Send failed: %v", err)
		}
		message, err := secondary.Recv(ctx)
		if err != nil {
			b.Fatalf("Recv failed: %v", err)
		}
		if message.Len() != payloadSize {
			b.Fatalf("payload size = %d, want %d", message.Len(), payloadSize)
		}
		message.Release()
	}
}

// BenchmarkAdvancedLinkRequestResponse measures a sequential Call against a
// Serve echoing a payload-sized response over 256-byte buffers (the 8 B and
// 64 B payloads fit one chunk, 1 KB takes four). The deadline variants carry
// a time budget to the server: "deadline" creates and cancels a
// context.WithTimeout per call, "deadline_reused" reuses one context with a
// far deadline (HQQ's own cost without creating the context), and
// "deadline_context_only" is the context creation and cancel loop alone, to
// subtract from "deadline".
func BenchmarkAdvancedLinkRequestResponse(b *testing.B) {
	payloads := []struct {
		name string
		size int
	}{
		{"8B", 8},
		{"64B", 64},
		{"1KB", 1024},
	}
	for _, p := range payloads {
		b.Run(p.name, func(b *testing.B) {
			benchmarkRequestResponse(b, p.size, func(parent context.Context) (context.Context, context.CancelFunc) {
				return parent, func() {}
			})
		})
	}
	b.Run("deadline/64B", func(b *testing.B) {
		benchmarkRequestResponse(b, 64, func(parent context.Context) (context.Context, context.CancelFunc) {
			return context.WithTimeout(parent, time.Minute)
		})
	})
	b.Run("deadline_reused/64B", func(b *testing.B) {
		reused, cancel := context.WithTimeout(context.Background(), time.Hour)
		defer cancel()
		benchmarkRequestResponse(b, 64, func(context.Context) (context.Context, context.CancelFunc) {
			return reused, func() {}
		})
	})
	b.Run("deadline_context_only", func(b *testing.B) {
		b.ReportAllocs()
		for range b.N {
			_, cancel := context.WithTimeout(context.Background(), time.Minute)
			cancel()
		}
	})
}

// benchmarkRequestResponse runs b.N sequential Calls with a payloadSize
// request, each under the context callCtx returns (and cancels afterwards).
func benchmarkRequestResponse(b *testing.B, payloadSize int, callCtx func(parent context.Context) (context.Context, context.CancelFunc)) {
	const (
		bufferCount = 1024
		bufferSize  = 256
	)
	primary, secondary := newNegotiatedPair(b, bufferCount, bufferSize, AdvancedOptions{}, AdvancedOptions{})

	request := make([]byte, payloadSize)
	response := make([]byte, payloadSize)
	serveCtx, stop := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- secondary.Serve(serveCtx, HandlerFunc(func(_ context.Context, w *ResponseWriter, _ *Request) {
			_, _ = w.Write(response)
		}))
	}()

	parent := context.Background()
	b.SetBytes(int64(payloadSize) * 2)
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		ctx, cancel := callCtx(parent)
		reply, err := primary.Call(ctx, 7, request)
		cancel()
		if err != nil {
			b.Fatalf("Call failed: %v", err)
		}
		if reply.Len() != payloadSize {
			b.Fatalf("reply size = %d, want %d", reply.Len(), payloadSize)
		}
		reply.Release()
	}
	b.StopTimer()
	stop()
	if err := <-done; err != context.Canceled {
		b.Fatalf("Serve = %v, want context.Canceled", err)
	}
}
