package hqq

import (
	"context"
	"testing"
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

func BenchmarkAdvancedLinkRequestResponse(b *testing.B) {
	const (
		bufferCount = 1024
		bufferSize  = 256
		payloadSize = 1024
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

	ctx := context.Background()
	b.SetBytes(payloadSize * 2)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		reply, err := primary.Call(ctx, 7, request)
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
