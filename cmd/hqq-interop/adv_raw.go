package main

import (
	"context"
	"os"
	"time"
	"unsafe"

	"gosuda.org/hqq"
	"gosuda.org/hqq/conformance"
	"gosuda.org/hqq/internal/mmap"
	"gosuda.org/hqq/internal/mpmc"
	"gosuda.org/hqq/internal/protocol"
	"gosuda.org/hqq/internal/shm"
)

// advRawWriter is a raw producer on the transmit ring of this endpoint. It
// writes the frames the Go API cannot produce (a negotiation answer with a
// wrong version, OpError during negotiation, an aborted chunk, a crafted
// OpCancel or OpConnCreate). Every frame goes through the producer side of
// the ring protocol (SPEC 7.4): claim a slot, store the packet and the
// payload in the slot's buffer, publish. The ring is the one the Go endpoint
// of this role sends on: the Primary sends on ring 0, the Secondary on ring 1
// (standard.go).
type advRawWriter struct {
	smem *shm.SharedMemory
	mem  []byte
	ring *mpmc.MPMCRing[protocol.Packet]
	bufs []byte // payload buffers of the transmit direction
	size int    // payload bytes per slot
}

// advRawOpen maps the segment of the scenario and attaches the transmit ring
// of this role.
func advRawOpen(e *env) (*advRawWriter, error) {
	c := e.cfg
	r, ok := conformance.ComputeRegions(os.Getpagesize(), c.BufferCount, c.BufferSize)
	if !ok {
		return nil, failf("raw: geometry %d x %d is outside the layout limits", c.BufferCount, c.BufferSize)
	}
	smem, err := shm.OpenSharedMemory(c.Name, r.Total, os.O_RDWR, 0)
	if err != nil {
		return nil, err
	}
	mem, err := mmap.Map(smem.FD(), 0, r.Total, mmap.PROT_READ|mmap.PROT_WRITE, mmap.MAP_SHARED)
	if err != nil {
		_ = smem.Close()
		return nil, err
	}
	ringOff, bufOff, idx := r.Ring0, r.Buf0, 0
	if c.Role == "opener" {
		ringOff, bufOff, idx = r.Ring1, r.Buf1, 1
	}
	ring := mpmc.Attach[protocol.Packet](unsafe.Pointer(unsafe.SliceData(mem[ringOff:])), 5*time.Second)
	if ring == nil {
		_ = mmap.UnMap(mem)
		_ = smem.Close()
		return nil, hqq.ErrIncompatibleLayout
	}
	// Same wake semaphores as the link itself (Windows only; nil elsewhere).
	notEmpty, notFull := smem.Semaphore(2*idx), smem.Semaphore(2*idx+1)
	if notEmpty != nil && notFull != nil {
		ring.SetParker(mpmc.NewSemaphoreParker(notEmpty, notFull))
	}
	return &advRawWriter{
		smem: smem,
		mem:  mem,
		ring: ring,
		bufs: mem[bufOff : bufOff+c.BufferCount*c.BufferSize],
		size: c.BufferSize,
	}, nil
}

// send publishes one frame: p, with payload stored in the slot's buffer. The
// caller sets the chunk size of p to len(payload).
func (w *advRawWriter) send(ctx context.Context, p protocol.Packet, payload []byte) error {
	if len(payload) > w.size {
		return hqq.ErrBufferOverflow
	}
	c := mpmc.Cancel{Ctx: ctx}
	err := w.ring.EnqueueZeroCopyCancel(&c, func(slot uint64, elem *protocol.Packet) {
		off := int(slot) * w.size
		copy(w.bufs[off:off+w.size], payload)
		*elem = p
	})
	if err != nil && ctx.Err() != nil {
		return ctx.Err()
	}
	return err
}

// close unmaps the segment. It does not remove the name.
func (w *advRawWriter) close() {
	_ = mmap.UnMap(w.mem)
	_ = w.smem.Close()
}
