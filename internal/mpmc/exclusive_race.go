//go:build race

package mpmc

import "sync/atomic"

// exclusiveGuard detects concurrent use of one side of an exclusive ring: it
// counts the goroutines inside a claim (including its wait for room or data)
// of each side and panics in the one that enters second. Only claims are
// guarded; they are the only operations whose load-check-store sequence two
// goroutines cannot interleave without corrupting the ring. A claim that
// panics for another reason (a callback in Cancel) leaves its side marked
// busy, which is acceptable for a debugging aid.
type exclusiveGuard struct {
	producers atomic.Int32
	consumers atomic.Int32
}

func (g *exclusiveGuard) enterProducer() {
	if g.producers.Add(1) != 1 {
		g.producers.Add(-1)
		panic("mpmc: concurrent producer-side use of an exclusive ring: at most one goroutine at a time may use each side")
	}
}

func (g *exclusiveGuard) exitProducer() { g.producers.Add(-1) }

func (g *exclusiveGuard) enterConsumer() {
	if g.consumers.Add(1) != 1 {
		g.consumers.Add(-1)
		panic("mpmc: concurrent consumer-side use of an exclusive ring: at most one goroutine at a time may use each side")
	}
}

func (g *exclusiveGuard) exitConsumer() { g.consumers.Add(-1) }
