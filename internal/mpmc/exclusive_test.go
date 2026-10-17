package mpmc

import (
	"bytes"
	"context"
	"encoding/binary"
	"runtime"
	"sync"
	"testing"
	"time"
	"unsafe"

	"gosuda.org/hqq/internal/protocol"
)

type packetRing = MPMCRing[protocol.Packet]

func seqPacket(i uint64) protocol.Packet {
	return protocol.NewPacket(protocol.OpStandardLinkCopy, i, ^i)
}

func checkSeqPacket(t testing.TB, p protocol.Packet, want uint64) {
	t.Helper()
	if got, inv := p.Operand(0), p.Operand(1); got != want || inv != ^want {
		t.Fatalf("packet carries (%d, %d), want (%d, %d): FIFO or payload broken", got, inv, want, ^want)
	}
}

// attachMode attaches another handle to the memory of r in the given mode.
func attachMode(t testing.TB, r *specRing, exclusive bool) *packetRing {
	t.Helper()
	h := Attach[protocol.Packet](unsafe.Pointer(&r.mem[0]), time.Second)
	if h == nil {
		t.Fatal("Attach failed")
	}
	h.SetExclusive(exclusive)
	return h
}

// startAt moves an empty ring to position start, as if start packets had
// been exchanged: both cursors and every slot sequence number are rewritten.
func (r *specRing) startAt(c *specRingConsts, start uint64) {
	r.put64(r.field(c, "r"), start)
	r.put64(r.field(c, "w"), start)
	for k := uint64(0); k < r.size; k++ {
		pos := start + k
		r.putSeq(c, pos&(r.size-1), pos)
	}
}

// TestExclusiveMixedModesFIFO runs one producer goroutine and one consumer
// goroutine over the same ring memory in every combination of CAS and
// exclusive handles, through all entry points, over many laps of a small ring
// and across the wrap of the 64-bit positions. Every packet must arrive once,
// in order, intact, and the canonical cursors and sequence numbers must end
// where a CAS run would leave them.
func TestExclusiveMixedModesFIFO(t *testing.T) {
	c := loadSpecRing(t)
	n := uint64(1 << 17)
	if raceEnabled {
		n = 1 << 14
	}
	for _, tc := range []struct {
		name       string
		prod, cons bool
		start      uint64
		slow       bool // slow consumer: the ring is full most of the time
	}{
		{"cas-producer/cas-consumer", false, false, 0, false},
		{"exclusive-producer/cas-consumer", true, false, 0, false},
		{"cas-producer/exclusive-consumer", false, true, 0, false},
		{"exclusive-producer/exclusive-consumer", true, true, 0, false},
		{"exclusive/exclusive across the 2^64 wrap", true, true, ^uint64(0) - 1000, false},
		{"exclusive/exclusive slow consumer", true, true, 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newSpecRing(t, 8)
			r.startAt(c, tc.start)
			p, k := attachMode(t, r, tc.prod), attachMode(t, r, tc.cons)
			count := n
			if tc.slow {
				count = 512
			}

			var wg sync.WaitGroup
			wg.Add(1)
			go func() {
				defer wg.Done()
				for i := range count {
					switch i % 3 {
					case 0:
						p.Enqueue(seqPacket(i))
					case 1:
						p.EnqueueZeroCopy(func(_ uint64, e *protocol.Packet) { *e = seqPacket(i) })
					default:
						s := p.ReserveProducer()
						*s.Value() = seqPacket(i)
						if !s.Commit() {
							t.Errorf("commit %d failed", i)
							return
						}
					}
				}
			}()
			for i := range count {
				if tc.slow && i%64 == 0 {
					time.Sleep(time.Millisecond)
				}
				switch i % 3 {
				case 0:
					checkSeqPacket(t, k.Dequeue(), i)
				case 1:
					k.DequeueZeroCopy(func(_ uint64, e *protocol.Packet) { checkSeqPacket(t, *e, i) })
				default:
					s := k.ReserveConsumer()
					checkSeqPacket(t, *s.Value(), i)
					if !s.Release() {
						t.Fatalf("release %d failed", i)
					}
				}
			}
			wg.Wait()

			end := tc.start + count
			specEq(t, "w after the run", r.u64(r.field(c, "w")), end)
			specEq(t, "r after the run", r.u64(r.field(c, "r")), end)
			for slot := uint64(0); slot < r.size; slot++ {
				// Every slot is released: it is free for the producer of the
				// first position at or after end that maps to it, plus one lap.
				specEq(t, "final seq", r.seq(c, slot), end+((slot-end)&(r.size-1)))
			}
		})
	}
}

// TestExclusiveImageMatchesCAS runs one operation script, including
// out-of-order completion, stale copies, full and empty rings, several laps
// and an abandoned zero-copy write, against a CAS ring and an exclusive ring
// and compares the complete memory images (cursors, sequence numbers,
// packets, wait words) after every step: the store-based protocol must leave
// exactly the image the CAS protocol leaves (SPEC 7.6, constraint 1).
func TestExclusiveImageMatchesCAS(t *testing.T) {
	c := loadSpecRing(t)
	for _, start := range []uint64{0, 5, ^uint64(0) - 3} {
		var images [2][][]byte
		var names [2][]string
		for mode, exclusive := range []bool{false, true} {
			r := newSpecRing(t, 4)
			r.startAt(c, start)
			r.ring.SetExclusive(exclusive)
			runImageScript(t, r.ring, func(step string) {
				images[mode] = append(images[mode], bytes.Clone(r.bytes()))
				names[mode] = append(names[mode], step)
			})
		}
		if len(images[0]) != len(images[1]) {
			t.Fatalf("start %d: the script took %d steps in CAS mode and %d in exclusive mode", start, len(images[0]), len(images[1]))
		}
		for i := range images[0] {
			if !bytes.Equal(images[0][i], images[1][i]) {
				t.Fatalf("start %d: memory images differ after step %q (first difference at byte %d)",
					start, names[0][i], firstDifference(images[0][i], images[1][i]))
			}
		}
	}
}

func firstDifference(a, b []byte) int {
	for i := range min(len(a), len(b)) {
		if a[i] != b[i] {
			return i
		}
	}
	return min(len(a), len(b))
}

func runImageScript(t *testing.T, ring *packetRing, snap func(step string)) {
	t.Helper()
	noWait := &Cancel{NoWait: true}
	nop := func(uint64, *protocol.Packet) {}

	ring.Enqueue(seqPacket(0))
	snap("enqueue 0")
	ring.Enqueue(seqPacket(1))
	snap("enqueue 1")
	checkSeqPacket(t, ring.Dequeue(), 0)
	snap("dequeue 0")

	// Two outstanding producer reservations, committed out of order.
	x := ring.ReserveProducer()
	y := ring.ReserveProducer()
	*x.Value() = seqPacket(2)
	*y.Value() = seqPacket(3)
	snap("reserve x, y")
	if !y.Commit() {
		t.Fatal("commit y failed")
	}
	snap("commit y")
	xStale := x
	if !x.Commit() {
		t.Fatal("commit x failed")
	}
	snap("commit x")
	if xStale.Commit() {
		t.Fatal("a stale copy committed")
	}
	snap("stale commit refused")

	// Full ring (1, 2, 3, 4): a non-waiting producer changes nothing.
	ring.Enqueue(seqPacket(4))
	snap("enqueue 4")
	if err := ring.EnqueueZeroCopyCancel(noWait, nop); err != ErrTimeout {
		t.Fatalf("enqueue on a full ring: %v", err)
	}
	snap("enqueue on full ring")

	// Two outstanding consumer reservations, released out of order.
	c1 := ring.ReserveConsumer()
	c2 := ring.ReserveConsumer()
	checkSeqPacket(t, *c1.Value(), 1)
	checkSeqPacket(t, *c2.Value(), 2)
	snap("reserve c1, c2")
	c1Stale := c1
	if !c2.Release() {
		t.Fatal("release c2 failed")
	}
	snap("release c2")
	if !c1.Release() {
		t.Fatal("release c1 failed")
	}
	snap("release c1")
	if c1Stale.Release() {
		t.Fatal("a stale copy released")
	}
	snap("stale release refused")

	checkSeqPacket(t, ring.Dequeue(), 3)
	snap("dequeue 3")
	checkSeqPacket(t, ring.Dequeue(), 4)
	snap("dequeue 4")
	if err := ring.DequeueZeroCopyCancel(noWait, nop); err != ErrTimeout {
		t.Fatalf("dequeue on an empty ring: %v", err)
	}
	snap("dequeue on empty ring")

	for i := uint64(5); i < 5+3*4+1; i++ { // three laps and a bit
		ring.Enqueue(seqPacket(i))
		snap("lap enqueue")
		checkSeqPacket(t, ring.Dequeue(), i)
		snap("lap dequeue")
	}

	// A zero-copy callback that panics still publishes a zeroed packet.
	func() {
		defer func() { _ = recover() }()
		_ = ring.EnqueueZeroCopyCancel(nil, func(_ uint64, e *protocol.Packet) {
			*e = seqPacket(99)
			panic("abandoned write")
		})
	}()
	snap("panicking enqueue")
	if got := ring.Dequeue(); got != (protocol.Packet{}) {
		t.Fatalf("abandoned write delivered %x, want a zero packet", got[:])
	}
	snap("dequeue of the zero packet")
}

// TestExclusiveHandoverKeepsCursors: cursors are read from shared memory at
// every claim, so a handle of either mode can take over a side that another
// handle used (a re-attach after a process restart that kept the memory).
func TestExclusiveHandoverKeepsCursors(t *testing.T) {
	c := loadSpecRing(t)
	r := newSpecRing(t, 4)
	cas := r.ring
	for i := uint64(0); i < 3; i++ {
		cas.Enqueue(seqPacket(i))
	}
	checkSeqPacket(t, cas.Dequeue(), 0)

	ex := attachMode(t, r, true)
	checkSeqPacket(t, ex.Dequeue(), 1) // the exclusive consumer continues at r = 1
	ex.Enqueue(seqPacket(3))           // and the exclusive producer at w = 3
	ex.Enqueue(seqPacket(4))
	specEq(t, "w", r.u64(r.field(c, "w")), 5)
	specEq(t, "r", r.u64(r.field(c, "r")), 2)

	cas2 := attachMode(t, r, false)
	for i := uint64(2); i < 5; i++ {
		checkSeqPacket(t, cas2.Dequeue(), i)
	}
	cas2.Enqueue(seqPacket(5))
	checkSeqPacket(t, ex.Dequeue(), 5)
	specEq(t, "w at the end", r.u64(r.field(c, "w")), 6)
	specEq(t, "r at the end", r.u64(r.field(c, "r")), 6)
}

// TestExclusiveSlotCompletionNotifies: Commit and Release on an exclusive
// ring wake the other side like the CAS versions, and a refused stale copy
// wakes nobody.
func TestExclusiveSlotCompletionNotifies(t *testing.T) {
	c := loadSpecRing(t)
	r := newSpecRing(t, 4)
	r.ring.SetExclusive(true)
	ne, ew, nf, fw := r.field(c, "notEmpty"), r.field(c, "emptyWaiters"), r.field(c, "notFull"), r.field(c, "fullWaiters")
	binary.LittleEndian.PutUint32(r.bytes()[ew:], 1) // a consumer process is parked
	binary.LittleEndian.PutUint32(r.bytes()[fw:], 1) // a producer process is parked

	p := r.ring.ReserveProducer()
	stale := p
	if !p.Commit() {
		t.Fatal("commit failed")
	}
	specEq(t, "notEmpty bumped by Commit", r.u32(ne), 1)
	if stale.Commit() {
		t.Fatal("stale commit succeeded")
	}
	specEq(t, "notEmpty untouched by a stale Commit", r.u32(ne), 1)

	k := r.ring.ReserveConsumer()
	staleK := k
	if !k.Release() {
		t.Fatal("release failed")
	}
	specEq(t, "notFull bumped by Release", r.u32(nf), 1)
	if staleK.Release() {
		t.Fatal("stale release succeeded")
	}
	specEq(t, "notFull untouched by a stale Release", r.u32(nf), 1)
}

// TestExclusiveWakeups: a parked exclusive consumer is woken by a publish and
// a parked exclusive producer by a release, in combination with CAS handles
// on the other side. A park is bounded by maxPark, so a lost wake-up shows as
// a latency of about maxPark instead of microseconds; the best of several
// attempts must stay well below it.
func TestExclusiveWakeups(t *testing.T) {
	if !parkingSupported {
		t.Skip("no cross-process word wait on this platform")
	}
	const attempts = 5
	for _, tc := range []struct {
		name       string
		prod, cons bool
	}{
		{"exclusive/exclusive", true, true},
		{"exclusive-producer/cas-consumer", true, false},
		{"cas-producer/exclusive-consumer", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newSpecRing(t, 2)
			p, k := attachMode(t, r, tc.prod), attachMode(t, r, tc.cons)
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()

			bestConsumer, bestProducer := time.Hour, time.Hour
			for range attempts {
				// A consumer waits on an empty ring until the producer publishes.
				got := make(chan time.Time, 1)
				go func() {
					if _, ok := k.DequeueWithContext(ctx); !ok {
						t.Error("dequeue gave up")
					}
					got <- time.Now()
				}()
				time.Sleep(30 * time.Millisecond) // let the consumer park
				t0 := time.Now()
				p.Enqueue(seqPacket(1))
				bestConsumer = min(bestConsumer, (<-got).Sub(t0))

				// A producer waits on a full ring until the consumer releases.
				p.Enqueue(seqPacket(2))
				p.Enqueue(seqPacket(3))
				done := make(chan time.Time, 1)
				go func() {
					if !p.EnqueueWithContext(ctx, seqPacket(4)) {
						t.Error("enqueue gave up")
					}
					done <- time.Now()
				}()
				time.Sleep(30 * time.Millisecond) // let the producer park
				t0 = time.Now()
				checkSeqPacket(t, k.Dequeue(), 2)
				bestProducer = min(bestProducer, (<-done).Sub(t0))
				checkSeqPacket(t, k.Dequeue(), 3)
				checkSeqPacket(t, k.Dequeue(), 4)
			}
			if bestConsumer > maxPark/2 {
				t.Errorf("a parked consumer woke %v after the publish at best; the wake-up is lost", bestConsumer)
			}
			if bestProducer > maxPark/2 {
				t.Errorf("a parked producer woke %v after the release at best; the wake-up is lost", bestProducer)
			}
		})
	}
}

// TestExclusiveCancelLeavesCursorsUntouched: a producer or consumer that
// gives up while waiting must not have stored its cursor.
func TestExclusiveCancelLeavesCursorsUntouched(t *testing.T) {
	c := loadSpecRing(t)
	r := newSpecRing(t, 2)
	r.ring.SetExclusive(true)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, ok := r.ring.DequeueWithContext(ctx); ok {
		t.Fatal("dequeued from an empty ring")
	}
	specEq(t, "r after a canceled dequeue", r.u64(r.field(c, "r")), 0)
	r.ring.Enqueue(seqPacket(1))
	r.ring.Enqueue(seqPacket(2))
	ctx2, cancel2 := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel2()
	if r.ring.EnqueueWithContext(ctx2, seqPacket(3)) {
		t.Fatal("enqueued into a full ring")
	}
	specEq(t, "w after a canceled enqueue", r.u64(r.field(c, "w")), 2)
	checkSeqPacket(t, r.ring.Dequeue(), 1)
	checkSeqPacket(t, r.ring.Dequeue(), 2)
}

// TestExclusiveZeroAllocs: the exclusive claim path must not allocate.
func TestExclusiveZeroAllocs(t *testing.T) {
	if raceEnabled {
		t.Skip("allocation counts are not meaningful under the race detector")
	}
	r := newSpecRing(t, 8)
	r.ring.SetExclusive(true)
	pk := seqPacket(1)
	nop := func(uint64, *protocol.Packet) {}
	allocs := testing.AllocsPerRun(1000, func() {
		r.ring.Enqueue(pk)
		_ = r.ring.Dequeue()
		_ = r.ring.EnqueueZeroCopyCancel(nil, nop)
		_ = r.ring.DequeueZeroCopyCancel(nil, nop)
		s := r.ring.ReserveProducer()
		s.Commit()
		cs := r.ring.ReserveConsumer()
		cs.Release()
	})
	if allocs != 0 {
		t.Fatalf("exclusive hot path allocates %v times per iteration", allocs)
	}
	runtime.KeepAlive(r)
}
