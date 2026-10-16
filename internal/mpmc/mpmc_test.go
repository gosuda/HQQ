package mpmc_test

import (
	"context"
	"sync"
	"testing"
	"time"
	"unsafe"

	"gosuda.org/hqq/internal/mpmc"
	"gosuda.org/hqq/internal/protocol"
)

func TestMPMC(t *testing.T) {
	const size = 128
	buffer := make([]byte, mpmc.SizeMPMCRing[uintptr](size))
	b := uintptr(unsafe.Pointer(&buffer[0]))
	if !mpmc.MPMCInit[uintptr](b, size) {
		panic("failed to initialize offheap mpmc ring")
	}
	r := mpmc.MPMCAttach[uintptr](b, 0)
	for i := uintptr(0); i < size; i++ {
		r.Enqueue(i)
	}
	for i := uintptr(0); i < size; i++ {
		n := r.Dequeue()
		if n != i {
			panic("queue sequence violation")
		}
	}
}

func TestMPMCUint8(t *testing.T) {
	const size = 128
	buffer := make([]byte, mpmc.SizeMPMCRing[uint8](size))
	b := uintptr(unsafe.Pointer(&buffer[0]))
	if !mpmc.MPMCInit[uint8](b, size) {
		panic("failed to initialize offheap mpmc ring")
	}
	r := mpmc.MPMCAttach[uint8](b, 0)
	for i := uint8(0); i < size; i++ {
		r.Enqueue(i)
	}
	for i := uint8(0); i < size; i++ {
		n := r.Dequeue()
		if n != i {
			panic("queue sequence violation")
		}
	}
}

func TestMPMCComplex128(t *testing.T) {
	const size = 128
	buffer := make([]byte, mpmc.SizeMPMCRing[complex128](size))
	b := uintptr(unsafe.Pointer(&buffer[0]))
	if !mpmc.MPMCInit[complex128](b, size) {
		panic("failed to initialize offheap mpmc ring")
	}
	r := mpmc.MPMCAttach[complex128](b, 0)
	for i := 0; i < 10; i++ {
		for ii := uint8(0); ii < size; ii++ {
			r.Enqueue(complex(0, 0))
		}
		for ii := uint8(0); ii < size; ii++ {
			n := r.Dequeue()
			_ = n
		}
	}
}

type _chunk struct {
	_pointer uintptr // relative pointer to the start of the buffer
	_size    uintptr // size of the chunk
}

func TestMPMCChunk(t *testing.T) {
	const size = 128
	buffer := make([]byte, mpmc.SizeMPMCRing[_chunk](size))
	b := uintptr(unsafe.Pointer(&buffer[0]))
	if !mpmc.MPMCInit[_chunk](b, size) {
		panic("failed to initialize offheap mpmc ring")
	}
	r := mpmc.MPMCAttach[_chunk](b, 0)

	for i := 0; i < 10; i++ {
		for ii := uint8(0); ii < size; ii++ {
			r.Enqueue(_chunk{})
		}
		for ii := uint8(0); ii < size; ii++ {
			_ = r.Dequeue()
		}
	}
}

func TestMPMCFunc(t *testing.T) {
	const size = 128
	buffer := make([]byte, mpmc.SizeMPMCRing[uintptr](size))
	b := uintptr(unsafe.Pointer(&buffer[0]))
	if !mpmc.MPMCInit[uintptr](b, size) {
		panic("failed to initialize offheap mpmc ring")
	}
	r := mpmc.MPMCAttach[uintptr](b, 0)
	for i := uintptr(0); i < size; i++ {
		r.EnqueueFunc(func(v *uintptr) {
			*v = i
		})
	}

	for i := uintptr(0); i < size; i++ {
		r.DequeueFunc(func(t *uintptr) {
			if *t != i {
				panic("queue sequence violation")
			}
		})
	}
}

func TestMPMCZeroCopySlot(t *testing.T) {
	const size = 8
	buffer := make([]byte, mpmc.SizeMPMCRing[uintptr](size))
	b := uintptr(unsafe.Pointer(&buffer[0]))
	if !mpmc.MPMCInit[uintptr](b, size) {
		panic("failed to initialize offheap mpmc ring")
	}
	r := mpmc.MPMCAttach[uintptr](b, 0)

	for i := uintptr(0); i < size; i++ {
		r.EnqueueZeroCopy(func(slot uint64, v *uintptr) {
			if slot != uint64(i) {
				t.Fatalf("enqueue slot = %d, want %d", slot, i)
			}
			*v = i + 100
		})
	}

	for i := uintptr(0); i < size; i++ {
		r.DequeueZeroCopy(func(slot uint64, v *uintptr) {
			if slot != uint64(i) {
				t.Fatalf("dequeue slot = %d, want %d", slot, i)
			}
			if *v != i+100 {
				t.Fatalf("value = %d, want %d", *v, i+100)
			}
		})
	}
}

func TestMPMCReserveCommitRelease(t *testing.T) {
	const size = 8
	buffer := make([]byte, mpmc.SizeMPMCRing[uintptr](size))
	b := uintptr(unsafe.Pointer(&buffer[0]))
	if !mpmc.MPMCInit[uintptr](b, size) {
		panic("failed to initialize offheap mpmc ring")
	}
	r := mpmc.MPMCAttach[uintptr](b, 0)

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

func TestMPMCReserveContextTimeout(t *testing.T) {
	const size = 2
	buffer := make([]byte, mpmc.SizeMPMCRing[uintptr](size))
	b := uintptr(unsafe.Pointer(&buffer[0]))
	if !mpmc.MPMCInit[uintptr](b, size) {
		panic("failed to initialize offheap mpmc ring")
	}
	r := mpmc.MPMCAttach[uintptr](b, 0)

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

func TestMPMCParallel(t *testing.T) {
	const size = 1 << 10
	buffer := make([]byte, mpmc.SizeMPMCRing[uintptr](size))
	b := uintptr(unsafe.Pointer(&buffer[0]))
	if !mpmc.MPMCInit[uintptr](b, size) {
		panic("failed to initialize offheap mpmc ring")
	}

	var mue, mud sync.Mutex
	var EnqueueMap, DequeueMap [(size + 63) / 64]uint64
	var wg sync.WaitGroup
	wg.Add(size * 2)

	for i := uintptr(0); i < size; i++ {
		// Spawn Enqueue goroutine.
		go func(i uintptr) {
			defer wg.Done()
			r := mpmc.MPMCAttach[uintptr](b, 0)
			r.Enqueue(i)

			mue.Lock()
			EnqueueMap[i/64] |= 1 << (i % 64)
			mue.Unlock()
		}(i)

		// Spawn Dequeue goroutine.
		go func() {
			defer wg.Done()
			r := mpmc.MPMCAttach[uintptr](b, 0)
			v := r.Dequeue()

			mud.Lock()
			DequeueMap[v/64] |= 1 << (v % 64)
			mud.Unlock()
		}()
	}

	// Wait for all goroutines to finish.
	wg.Wait()

	for i := uintptr(0); i < size; i++ {
		if EnqueueMap[i/64]&(1<<(i%64)) == 0 {
			t.Errorf("Enqueue Failed at index: %d", i)
			t.Fail()
		}
		if DequeueMap[i/64]&(1<<(i%64)) == 0 {
			t.Errorf("Dequeue Failed at index: %d", i)
			t.Fail()
		}
	}
}

func BenchmarkMPMC(b *testing.B) {
	const size = 128
	buffer := make([]byte, mpmc.SizeMPMCRing[uintptr](size))
	bb := uintptr(unsafe.Pointer(&buffer[0]))
	if !mpmc.MPMCInit[uintptr](bb, size) {
		panic("failed to initialize offheap mpmc ring")
	}
	b.RunParallel(func(p *testing.PB) {
		r := mpmc.MPMCAttach[uintptr](bb, 0)
		for p.Next() {
			r.Enqueue(0)
			_ = r.Dequeue()
		}
	})
}

func BenchmarkMPMCPayloadTypes(b *testing.B) {
	const size = 128

	b.Run("uint64", func(b *testing.B) {
		buffer := make([]byte, mpmc.SizeMPMCRing[uint64](size))
		bb := uintptr(unsafe.Pointer(&buffer[0]))
		if !mpmc.MPMCInit[uint64](bb, size) {
			b.Fatal("failed to initialize offheap mpmc ring")
		}

		b.ReportAllocs()
		b.RunParallel(func(p *testing.PB) {
			r := mpmc.MPMCAttach[uint64](bb, 0)
			for p.Next() {
				r.Enqueue(0)
				_ = r.Dequeue()
			}
		})
	})

	b.Run("chunk16", func(b *testing.B) {
		buffer := make([]byte, mpmc.SizeMPMCRing[_chunk](size))
		bb := uintptr(unsafe.Pointer(&buffer[0]))
		if !mpmc.MPMCInit[_chunk](bb, size) {
			b.Fatal("failed to initialize offheap mpmc ring")
		}

		b.ReportAllocs()
		b.RunParallel(func(p *testing.PB) {
			r := mpmc.MPMCAttach[_chunk](bb, 0)
			for p.Next() {
				r.Enqueue(_chunk{})
				_ = r.Dequeue()
			}
		})
	})

	b.Run("packet64", func(b *testing.B) {
		buffer := make([]byte, mpmc.SizeMPMCRing[protocol.Packet](size))
		bb := uintptr(unsafe.Pointer(&buffer[0]))
		if !mpmc.MPMCInit[protocol.Packet](bb, size) {
			b.Fatal("failed to initialize offheap mpmc ring")
		}
		packet := protocol.NewPacket(protocol.OpStandardLinkCopy, 1, 2, 3)

		b.ReportAllocs()
		b.RunParallel(func(p *testing.PB) {
			r := mpmc.MPMCAttach[protocol.Packet](bb, 0)
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
		buffer := make([]byte, mpmc.SizeMPMCRing[_chunk](size))
		bb := uintptr(unsafe.Pointer(&buffer[0]))
		if !mpmc.MPMCInit[_chunk](bb, size) {
			b.Fatal("failed to initialize offheap mpmc ring")
		}

		b.ReportAllocs()
		b.RunParallel(func(p *testing.PB) {
			r := mpmc.MPMCAttach[_chunk](bb, 0)
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
		buffer := make([]byte, mpmc.SizeMPMCRing[protocol.Packet](size))
		bb := uintptr(unsafe.Pointer(&buffer[0]))
		if !mpmc.MPMCInit[protocol.Packet](bb, size) {
			b.Fatal("failed to initialize offheap mpmc ring")
		}

		b.ReportAllocs()
		b.RunParallel(func(p *testing.PB) {
			r := mpmc.MPMCAttach[protocol.Packet](bb, 0)
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
		buffer := make([]byte, mpmc.SizeMPMCRing[_chunk](size))
		bb := uintptr(unsafe.Pointer(&buffer[0]))
		if !mpmc.MPMCInit[_chunk](bb, size) {
			b.Fatal("failed to initialize offheap mpmc ring")
		}

		b.ReportAllocs()
		b.RunParallel(func(p *testing.PB) {
			r := mpmc.MPMCAttach[_chunk](bb, 0)
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
		buffer := make([]byte, mpmc.SizeMPMCRing[protocol.Packet](size))
		bb := uintptr(unsafe.Pointer(&buffer[0]))
		if !mpmc.MPMCInit[protocol.Packet](bb, size) {
			b.Fatal("failed to initialize offheap mpmc ring")
		}

		b.ReportAllocs()
		b.RunParallel(func(p *testing.PB) {
			r := mpmc.MPMCAttach[protocol.Packet](bb, 0)
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
		buffer := make([]byte, mpmc.SizeMPMCRing[_chunk](size))
		bb := uintptr(unsafe.Pointer(&buffer[0]))
		if !mpmc.MPMCInit[_chunk](bb, size) {
			b.Fatal("failed to initialize offheap mpmc ring")
		}

		b.ReportAllocs()
		b.RunParallel(func(p *testing.PB) {
			r := mpmc.MPMCAttach[_chunk](bb, 0)
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
		buffer := make([]byte, mpmc.SizeMPMCRing[protocol.Packet](size))
		bb := uintptr(unsafe.Pointer(&buffer[0]))
		if !mpmc.MPMCInit[protocol.Packet](bb, size) {
			b.Fatal("failed to initialize offheap mpmc ring")
		}

		b.ReportAllocs()
		b.RunParallel(func(p *testing.PB) {
			r := mpmc.MPMCAttach[protocol.Packet](bb, 0)
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
