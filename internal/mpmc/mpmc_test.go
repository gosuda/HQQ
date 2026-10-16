package mpmc_test

import (
	"context"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unsafe"

	"gosuda.org/hqq/internal/mpmc"
	"gosuda.org/hqq/internal/protocol"
)

func TestMPMC(t *testing.T) {
	const size = 128
	buffer, r := newRingOf[uintptr](t, size)
	defer runtime.KeepAlive(buffer)
	for i := uintptr(0); i < size; i++ {
		r.Enqueue(i)
	}
	for i := uintptr(0); i < size; i++ {
		if n := r.Dequeue(); n != i {
			t.Fatalf("Dequeue = %d, want %d", n, i)
		}
	}
}

func TestMPMCUint8(t *testing.T) {
	const size = 128
	buffer, r := newRingOf[uint8](t, size)
	defer runtime.KeepAlive(buffer)
	for i := uint8(0); i < size; i++ {
		r.Enqueue(i)
	}
	for i := uint8(0); i < size; i++ {
		if n := r.Dequeue(); n != i {
			t.Fatalf("Dequeue = %d, want %d", n, i)
		}
	}
}

func TestMPMCComplex128(t *testing.T) {
	const size = 128
	buffer, r := newRingOf[complex128](t, size)
	defer runtime.KeepAlive(buffer)
	for lap := range 10 {
		for i := range size {
			r.Enqueue(complex(float64(lap), float64(-i)))
		}
		for i := range size {
			if n, want := r.Dequeue(), complex(float64(lap), float64(-i)); n != want {
				t.Fatalf("lap %d: Dequeue = %v, want %v", lap, n, want)
			}
		}
	}
}

type _chunk struct {
	_pointer uintptr // relative pointer to the start of the buffer
	_size    uintptr // size of the chunk
}

func TestMPMCChunk(t *testing.T) {
	const size = 128
	buffer, r := newRingOf[_chunk](t, size)
	defer runtime.KeepAlive(buffer)
	for lap := range 10 {
		for i := range size {
			r.Enqueue(_chunk{_pointer: uintptr(lap), _size: uintptr(i) + 1})
		}
		for i := range size {
			if c, want := r.Dequeue(), (_chunk{_pointer: uintptr(lap), _size: uintptr(i) + 1}); c != want {
				t.Fatalf("lap %d: Dequeue = %+v, want %+v", lap, c, want)
			}
		}
	}
}

func TestMPMCFunc(t *testing.T) {
	const size = 128
	buffer, r := newRingOf[uintptr](t, size)
	defer runtime.KeepAlive(buffer)
	for i := uintptr(0); i < size; i++ {
		r.EnqueueFunc(func(v *uintptr) {
			*v = i
		})
	}

	for i := uintptr(0); i < size; i++ {
		r.DequeueFunc(func(v *uintptr) {
			if *v != i {
				t.Errorf("DequeueFunc saw %d, want %d", *v, i)
			}
		})
	}
}

func TestMPMCZeroCopySlot(t *testing.T) {
	const size = 8
	buffer, r := newRingOf[uintptr](t, size)
	defer runtime.KeepAlive(buffer)

	for i := uintptr(0); i < size; i++ {
		r.EnqueueZeroCopy(func(slot uint64, v *uintptr) {
			if slot != uint64(i) {
				t.Errorf("enqueue slot = %d, want %d", slot, i)
			}
			*v = i + 100
		})
	}

	for i := uintptr(0); i < size; i++ {
		r.DequeueZeroCopy(func(slot uint64, v *uintptr) {
			if slot != uint64(i) {
				t.Errorf("dequeue slot = %d, want %d", slot, i)
			}
			if *v != i+100 {
				t.Errorf("value = %d, want %d", *v, i+100)
			}
		})
	}
}

func TestMPMCReserveCommitRelease(t *testing.T) {
	const size = 8
	buffer, r := newRingOf[uintptr](t, size)
	defer runtime.KeepAlive(buffer)

	for i := uintptr(0); i < size; i++ {
		slot := r.ReserveProducer()
		if !slot.Valid() {
			t.Fatalf("producer slot %d is invalid", i)
		}
		if slot.Slot() != uint64(i) {
			t.Fatalf("producer slot = %d, want %d", slot.Slot(), i)
		}
		*slot.Value() = i + 1000
		if !slot.Commit() {
			t.Fatalf("producer slot %d did not commit", i)
		}
		if slot.Commit() {
			t.Fatalf("producer slot %d committed twice", i)
		}
	}

	for i := uintptr(0); i < size; i++ {
		slot := r.ReserveConsumer()
		if !slot.Valid() {
			t.Fatalf("consumer slot %d is invalid", i)
		}
		if slot.Slot() != uint64(i) {
			t.Fatalf("consumer slot = %d, want %d", slot.Slot(), i)
		}
		if got := *slot.Value(); got != i+1000 {
			t.Fatalf("value = %d, want %d", got, i+1000)
		}
		if !slot.Release() {
			t.Fatalf("consumer slot %d did not release", i)
		}
		if slot.Release() {
			t.Fatalf("consumer slot %d released twice", i)
		}
	}
}

// TestMPMCStaleProducerCopyCannotCorruptRing guards the compare-and-swap
// publish: a copy of a ProducerSlot that was already committed must not store
// into the slot's sequence number again. A plain store would overwrite the
// sequence the slot reached in a later lap and wedge the ring.
func TestMPMCStaleProducerCopyCannotCorruptRing(t *testing.T) {
	buffer, r := newRingOf[uintptr](t, 2)
	defer runtime.KeepAlive(buffer)

	slot := r.ReserveProducer()
	stale := slot
	if !stale.Owned() {
		t.Fatal("a fresh reservation must be owned")
	}
	*slot.Value() = 1
	if !slot.Commit() {
		t.Fatal("commit failed")
	}
	if stale.Owned() {
		t.Fatal("a copy still reports ownership after the original committed")
	}
	if got := r.Dequeue(); got != 1 {
		t.Fatalf("Dequeue = %d, want 1", got)
	}

	// The slot is now free for its next lap. The stale copy must be refused.
	if stale.Commit() {
		t.Fatal("stale copy committed an already completed slot")
	}
	if stale.Valid() {
		t.Fatal("a failed commit must invalidate the copy")
	}

	// Two more full laps must still work (the stale commit would have left the
	// slot's sequence number behind its lap and blocked the third enqueue).
	for i := uintptr(10); i < 14; i++ {
		if err := r.EnqueueZeroCopyCancel(&mpmc.Cancel{NoWait: true}, func(_ uint64, e *uintptr) { *e = i }); err != nil {
			t.Fatalf("enqueue %d after stale commit: %v", i, err)
		}
		if got := r.Dequeue(); got != i {
			t.Fatalf("Dequeue = %d, want %d", got, i)
		}
	}
}

// TestMPMCStaleConsumerCopyCannotCorruptRing is the consumer-side
// counterpart: releasing a copy must neither free a slot a producer already
// refilled nor report success.
func TestMPMCStaleConsumerCopyCannotCorruptRing(t *testing.T) {
	buffer, r := newRingOf[uintptr](t, 2)
	defer runtime.KeepAlive(buffer)

	r.Enqueue(1)
	slot := r.ReserveConsumer()
	stale := slot
	if !stale.Owned() {
		t.Fatal("a fresh reservation must be owned")
	}
	if !slot.Release() {
		t.Fatal("release failed")
	}
	if stale.Owned() {
		t.Fatal("a copy still reports ownership after the original released")
	}

	// A producer refills the slot's next lap and a consumer holds it.
	r.Enqueue(2)
	r.Enqueue(3)
	if got := r.Dequeue(); got != 2 {
		t.Fatalf("Dequeue = %d, want 2", got)
	}
	held := r.ReserveConsumer()
	if held.Slot() != slot.Slot() {
		t.Fatalf("held slot %d, want the stale copy's slot %d", held.Slot(), slot.Slot())
	}
	if got := *held.Value(); got != 3 {
		t.Fatalf("held value = %d, want 3", got)
	}
	if stale.Release() {
		t.Fatal("stale copy released a slot that was already completed")
	}
	if !held.Owned() {
		t.Fatal("stale release stole the slot from its new owner")
	}
	if !held.Release() {
		t.Fatal("release of the current owner failed")
	}
	// The ring is empty again and fully usable.
	if err := r.DequeueZeroCopyCancel(&mpmc.Cancel{NoWait: true}, func(uint64, *uintptr) {}); err == nil {
		t.Fatal("ring should be empty")
	}
	r.Enqueue(4)
	if got := r.Dequeue(); got != 4 {
		t.Fatalf("Dequeue = %d, want 4", got)
	}
}

func TestMPMCReserveContextTimeout(t *testing.T) {
	const size = 2
	buffer, r := newRingOf[uintptr](t, size)
	defer runtime.KeepAlive(buffer)

	for i := uintptr(0); i < size; i++ {
		slot := r.ReserveProducer()
		*slot.Value() = i
		slot.Commit()
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if _, ok := r.ReserveProducerWithContext(ctx); ok {
		t.Fatal("ReserveProducerWithContext succeeded on a full ring")
	}

	for i := uintptr(0); i < size; i++ {
		slot := r.ReserveConsumer()
		if got := *slot.Value(); got != i {
			t.Fatalf("value = %d, want %d", got, i)
		}
		slot.Release()
	}

	ctx, cancel = context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if _, ok := r.ReserveConsumerWithContext(ctx); ok {
		t.Fatal("ReserveConsumerWithContext succeeded on an empty ring")
	}
}

// TestMPMCParallel pushes many more items than slots through a small ring
// with several producers and consumers, so every slot is reused many times
// under contention, and checks with a bitset that every item is delivered
// exactly once.
func TestMPMCParallel(t *testing.T) {
	const (
		size      = 64
		items     = 1 << 16
		producers = 4
		consumers = 4
	)
	buffer, _ := newRingOf[uintptr](t, size)
	defer runtime.KeepAlive(buffer)
	base := unsafe.Pointer(&buffer[0])

	var seen [items / 64]atomic.Uint64
	var delivered atomic.Int64
	var wg sync.WaitGroup

	for p := range producers {
		wg.Add(1)
		go func(first uintptr) {
			defer wg.Done()
			r := mpmc.Attach[uintptr](base, 0)
			for i := first; i < items; i += producers {
				r.Enqueue(i)
			}
		}(uintptr(p))
	}
	for range consumers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := mpmc.Attach[uintptr](base, 0)
			for delivered.Add(1) <= items {
				v := r.Dequeue()
				if v >= items {
					t.Errorf("dequeued out-of-range item %d", v)
					continue
				}
				bit := uint64(1) << (v % 64)
				if old := seen[v/64].Or(bit); old&bit != 0 {
					t.Errorf("item %d delivered twice", v)
				}
			}
		}()
	}
	wg.Wait()

	for i := range seen {
		if got := seen[i].Load(); got != ^uint64(0) {
			t.Errorf("items %d..%d: delivery bitset %#x, want all ones", i*64, i*64+63, got)
		}
	}
}

func BenchmarkMPMC(b *testing.B) {
	const size = 128
	buffer, _ := newRingOf[uintptr](b, size)
	defer runtime.KeepAlive(buffer)
	bb := unsafe.Pointer(&buffer[0])
	b.RunParallel(func(p *testing.PB) {
		r := mpmc.Attach[uintptr](bb, 0)
		for p.Next() {
			r.Enqueue(0)
			_ = r.Dequeue()
		}
	})
}

func BenchmarkMPMCPayloadTypes(b *testing.B) {
	const size = 128

	b.Run("uint64", func(b *testing.B) {
		buffer, _ := newRingOf[uint64](b, size)
		defer runtime.KeepAlive(buffer)
		bb := unsafe.Pointer(&buffer[0])

		b.ReportAllocs()
		b.RunParallel(func(p *testing.PB) {
			r := mpmc.Attach[uint64](bb, 0)
			for p.Next() {
				r.Enqueue(0)
				_ = r.Dequeue()
			}
		})
	})

	b.Run("chunk16", func(b *testing.B) {
		buffer, _ := newRingOf[_chunk](b, size)
		defer runtime.KeepAlive(buffer)
		bb := unsafe.Pointer(&buffer[0])

		b.ReportAllocs()
		b.RunParallel(func(p *testing.PB) {
			r := mpmc.Attach[_chunk](bb, 0)
			for p.Next() {
				r.Enqueue(_chunk{})
				_ = r.Dequeue()
			}
		})
	})

	b.Run("packet64", func(b *testing.B) {
		buffer, _ := newRingOf[protocol.Packet](b, size)
		defer runtime.KeepAlive(buffer)
		bb := unsafe.Pointer(&buffer[0])
		packet := protocol.NewPacket(protocol.OpStandardLinkCopy, 1, 2, 3)

		b.ReportAllocs()
		b.RunParallel(func(p *testing.PB) {
			r := mpmc.Attach[protocol.Packet](bb, 0)
			for p.Next() {
				r.Enqueue(packet)
				_ = r.Dequeue()
			}
		})
	})
}

func BenchmarkMPMCFuncPayloadTypes(b *testing.B) {
	const size = 128

	b.Run("chunk16", func(b *testing.B) {
		buffer, _ := newRingOf[_chunk](b, size)
		defer runtime.KeepAlive(buffer)
		bb := unsafe.Pointer(&buffer[0])

		b.ReportAllocs()
		b.RunParallel(func(p *testing.PB) {
			r := mpmc.Attach[_chunk](bb, 0)
			for p.Next() {
				r.EnqueueFunc(func(c *_chunk) {
					c._pointer = 1
					c._size = 2
				})
				r.DequeueFunc(func(c *_chunk) {
					_ = c._pointer
				})
			}
		})
	})

	b.Run("packet64", func(b *testing.B) {
		buffer, _ := newRingOf[protocol.Packet](b, size)
		defer runtime.KeepAlive(buffer)
		bb := unsafe.Pointer(&buffer[0])

		b.ReportAllocs()
		b.RunParallel(func(p *testing.PB) {
			r := mpmc.Attach[protocol.Packet](bb, 0)
			for p.Next() {
				r.EnqueueFunc(func(packet *protocol.Packet) {
					*packet = protocol.NewStandardLinkCopyPacket(1, 2, 3)
				})
				r.DequeueFunc(func(packet *protocol.Packet) {
					_ = packet.Operand(1)
				})
			}
		})
	})
}

func BenchmarkMPMCZeroCopyPayloadTypes(b *testing.B) {
	const size = 128

	b.Run("chunk16", func(b *testing.B) {
		buffer, _ := newRingOf[_chunk](b, size)
		defer runtime.KeepAlive(buffer)
		bb := unsafe.Pointer(&buffer[0])

		b.ReportAllocs()
		b.RunParallel(func(p *testing.PB) {
			r := mpmc.Attach[_chunk](bb, 0)
			for p.Next() {
				r.EnqueueZeroCopy(func(slot uint64, c *_chunk) {
					c._pointer = uintptr(slot)
					c._size = 2
				})
				r.DequeueZeroCopy(func(slot uint64, c *_chunk) {
					_ = c._pointer + uintptr(slot)
				})
			}
		})
	})

	b.Run("packet64", func(b *testing.B) {
		buffer, _ := newRingOf[protocol.Packet](b, size)
		defer runtime.KeepAlive(buffer)
		bb := unsafe.Pointer(&buffer[0])

		b.ReportAllocs()
		b.RunParallel(func(p *testing.PB) {
			r := mpmc.Attach[protocol.Packet](bb, 0)
			for p.Next() {
				r.EnqueueZeroCopy(func(slot uint64, packet *protocol.Packet) {
					*packet = protocol.NewStandardLinkCopyPacket(1, slot, 3)
				})
				r.DequeueZeroCopy(func(slot uint64, packet *protocol.Packet) {
					_ = packet.Operand(1) + slot
				})
			}
		})
	})
}

func BenchmarkMPMCReservePayloadTypes(b *testing.B) {
	const size = 128

	b.Run("chunk16", func(b *testing.B) {
		buffer, _ := newRingOf[_chunk](b, size)
		defer runtime.KeepAlive(buffer)
		bb := unsafe.Pointer(&buffer[0])

		b.ReportAllocs()
		b.RunParallel(func(p *testing.PB) {
			r := mpmc.Attach[_chunk](bb, 0)
			for p.Next() {
				producer := r.ReserveProducer()
				c := producer.Value()
				c._pointer = uintptr(producer.Slot())
				c._size = 2
				producer.Commit()

				consumer := r.ReserveConsumer()
				_ = consumer.Value()._pointer + uintptr(consumer.Slot())
				consumer.Release()
			}
		})
	})

	b.Run("packet64", func(b *testing.B) {
		buffer, _ := newRingOf[protocol.Packet](b, size)
		defer runtime.KeepAlive(buffer)
		bb := unsafe.Pointer(&buffer[0])

		b.ReportAllocs()
		b.RunParallel(func(p *testing.PB) {
			r := mpmc.Attach[protocol.Packet](bb, 0)
			for p.Next() {
				producer := r.ReserveProducer()
				*producer.Value() = protocol.NewStandardLinkCopyPacket(1, producer.Slot(), 3)
				producer.Commit()

				consumer := r.ReserveConsumer()
				_ = consumer.Value().Operand(1) + consumer.Slot()
				consumer.Release()
			}
		})
	})
}
