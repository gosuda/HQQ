package mpmc

import (
	"runtime"
	"slices"
	"sync/atomic"
	"testing"
	"time"
	"unsafe"
)

// countingParker records the token count of every Wake call.
type countingParker struct {
	wakes []uint32
}

func (p *countingParker) Park(Side, time.Duration) {}

func (p *countingParker) Wake(_ Side, n uint32) { p.wakes = append(p.wakes, n) }

// A consumer killed while parked leaves its registration raised. Every
// later publish must grant one token, not the stale count, or the tokens
// pile up in the semaphore (F-02); WakeAll still grants the whole count.
func TestStaleWaiterCountGrantsOneTokenPerSlot(t *testing.T) {
	buf := make([]byte, SizeMPMCRing[uintptr](4))
	defer runtime.KeepAlive(buf)
	p := unsafe.Pointer(&buf[0])
	Format[uintptr](p, 4)
	r := Attach[uintptr](p, time.Second)
	if r == nil {
		t.Fatal("failed to attach ring")
	}
	cp := &countingParker{}
	r.SetParker(cp)

	atomic.StoreUint32(&r.head.emptyWaiters, 5) // stale: the waiters were killed
	r.Enqueue(1)
	r.Enqueue(2)
	if want := []uint32{1, 1}; !slices.Equal(cp.wakes, want) {
		t.Fatalf("Wake counts after two publishes = %v, want %v", cp.wakes, want)
	}

	r.WakeAll()
	if want := []uint32{1, 1, 5}; !slices.Equal(cp.wakes, want) {
		t.Fatalf("Wake counts after WakeAll = %v, want %v", cp.wakes, want)
	}
}
