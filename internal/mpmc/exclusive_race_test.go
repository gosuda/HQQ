//go:build race

package mpmc

import (
	"strings"
	"testing"
	"time"

	"gosuda.org/hqq/internal/protocol"
)

// mustPanicWith runs fn and fails unless it panics with a message that
// contains want.
func mustPanicWith(t *testing.T, want string, fn func()) {
	t.Helper()
	defer func() {
		t.Helper()
		v := recover()
		if v == nil {
			t.Fatalf("no panic, want one containing %q", want)
		}
		if msg, _ := v.(string); !strings.Contains(msg, want) {
			t.Fatalf("panic %v, want one containing %q", v, want)
		}
	}()
	fn()
}

// TestExclusiveGuardProducer: while one goroutine waits for room on an
// exclusive ring, a second producer-side claim is reported instead of
// corrupting the ring, and the ring still works afterwards.
func TestExclusiveGuardProducer(t *testing.T) {
	r := newSpecRing(t, 2)
	r.ring.SetExclusive(true)
	r.ring.Enqueue(seqPacket(0))
	r.ring.Enqueue(seqPacket(1)) // full

	done := make(chan struct{})
	go func() {
		defer close(done)
		r.ring.Enqueue(seqPacket(2)) // waits inside the claim
	}()
	for r.ring.guard.producers.Load() != 1 {
		time.Sleep(time.Millisecond)
	}
	mustPanicWith(t, "concurrent producer-side use", func() { r.ring.Enqueue(seqPacket(3)) })
	// The consumer side is independent of the producer side.
	checkSeqPacket(t, r.ring.Dequeue(), 0)
	<-done
	checkSeqPacket(t, r.ring.Dequeue(), 1)
	checkSeqPacket(t, r.ring.Dequeue(), 2)
	r.ring.Enqueue(seqPacket(4)) // the refused claim left the guard consistent
	checkSeqPacket(t, r.ring.Dequeue(), 4)
}

// TestExclusiveGuardConsumer is TestExclusiveGuardProducer for the consumer
// side.
func TestExclusiveGuardConsumer(t *testing.T) {
	r := newSpecRing(t, 2)
	r.ring.SetExclusive(true)

	done := make(chan protocol.Packet, 1)
	go func() { done <- r.ring.Dequeue() }() // waits inside the claim
	for r.ring.guard.consumers.Load() != 1 {
		time.Sleep(time.Millisecond)
	}
	mustPanicWith(t, "concurrent consumer-side use", func() { _ = r.ring.Dequeue() })
	r.ring.Enqueue(seqPacket(1))
	checkSeqPacket(t, <-done, 1)
	r.ring.Enqueue(seqPacket(2))
	checkSeqPacket(t, r.ring.Dequeue(), 2)
}

// TestExclusiveGuardIgnoresCASRings: a CAS handle allows many goroutines per
// side, so the guard must not touch it.
func TestExclusiveGuardIgnoresCASRings(t *testing.T) {
	r := newSpecRing(t, 2)
	r.ring.Enqueue(seqPacket(0))
	r.ring.Enqueue(seqPacket(1)) // full

	done := make(chan struct{})
	go func() {
		defer close(done)
		r.ring.Enqueue(seqPacket(2))
	}()
	time.Sleep(20 * time.Millisecond)
	if n := r.ring.guard.producers.Load(); n != 0 {
		t.Fatalf("a CAS handle entered the exclusive guard (%d)", n)
	}
	checkSeqPacket(t, r.ring.Dequeue(), 0)
	<-done
	checkSeqPacket(t, r.ring.Dequeue(), 1)
	checkSeqPacket(t, r.ring.Dequeue(), 2)
}
