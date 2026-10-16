package main

import (
	"context"
	"encoding/binary"
	"time"

	"gosuda.org/hqq/conformance"
	"gosuda.org/hqq/internal/protocol"
)

// rawRing addresses one ring of a raw segment and its payload pool (SPEC 7).
type rawRing struct {
	seg  *rawSegment
	base int // ring header offset
	buf  int // payload pool offset (slot i owns buffer i)
	n    int // slots, a power of two
	size int // bytes per buffer
}

// ring returns ring i (0: primary to secondary, 1: secondary to primary).
func (s *rawSegment) ring(i int) rawRing {
	r := rawRing{seg: s, base: s.reg.Ring0, buf: s.reg.Buf0, n: s.count, size: s.size}
	if i == 1 {
		r.base, r.buf = s.reg.Ring1, s.reg.Buf1
	}
	return r
}

// slotOff returns the offset of slot idx (SPEC 7.1).
func (r rawRing) slotOff(idx int) int {
	return r.base + conformance.RingHeaderSize + conformance.RingSlotStride*idx
}

// format writes the ring image of SPEC 7.2 in the order the spec requires.
// It runs only while the caller owns the initialization.
func (r rawRing) format() {
	s := r.seg
	s.store(r.base+conformance.RingFlag, 0)
	s.store(r.base+conformance.RingMagic, 0)
	for i := range r.n {
		sl := r.slotOff(i)
		clear(s.mem[sl+conformance.RingSlotPacket : sl+conformance.RingSlotPacket+conformance.PacketSize])
		s.store(sl+conformance.RingSlotSeq, uint64(i))
	}
	s.store(r.base+conformance.RingR, 0)
	s.store(r.base+conformance.RingW, 0)
	s.store32(r.base+conformance.RingNotEmpty, 0)
	s.store32(r.base+conformance.RingEmptyWaiters, 0)
	s.store32(r.base+conformance.RingNotFull, 0)
	s.store32(r.base+conformance.RingFullWaiters, 0)
	s.store(r.base+conformance.RingSize, uint64(r.n))
	s.store(r.base+conformance.RingStride, conformance.RingSlotStride)
	s.store(r.base+conformance.RingVersionOff, conformance.RingVersion)
	s.store(r.base+conformance.RingMagic, conformance.MagicRing)
	s.store(r.base+conformance.RingFlag, conformance.RingFlagInit)
}

// rawPacket is one 64-byte packet: an opcode and seven operands (SPEC 9.1).
type rawPacket struct {
	op  protocol.OpCode
	ops [7]uint64
}

// bytes encodes the packet as eight little-endian words.
func (p rawPacket) bytes() [conformance.PacketSize]byte {
	var b [conformance.PacketSize]byte
	binary.LittleEndian.PutUint64(b[0:], uint64(p.op))
	for i, v := range p.ops {
		binary.LittleEndian.PutUint64(b[8*(i+1):], v)
	}
	return b
}

// put is the producer side of SPEC 7.4 for one packet: claim the position by a
// CAS on w, write the payload into the slot's buffer and the packet into the
// slot (plain stores), then publish with a store of seq = pos+1. It waits
// while the ring is full and gives up when ctx ends. A nil payload leaves the
// buffer alone.
func (r rawRing) put(ctx context.Context, p rawPacket, payload []byte) error {
	s := r.seg
	mask := uint64(r.n - 1)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		pos := s.load(r.base + conformance.RingW)
		idx := int(pos & mask)
		sl := r.slotOff(idx)
		d := int64(s.load(sl+conformance.RingSlotSeq) - pos)
		switch {
		case d == 0 && s.cas(r.base+conformance.RingW, pos, pos+1):
			if payload != nil {
				copy(s.mem[r.buf+idx*r.size:], payload)
			}
			b := p.bytes()
			copy(s.mem[sl+conformance.RingSlotPacket:], b[:])
			s.store(sl+conformance.RingSlotSeq, pos+1)
			return nil
		case d < 0:
			time.Sleep(200 * time.Microsecond) // full: the consumer has not released pos-N
		}
	}
}
