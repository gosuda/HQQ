package conformance

import (
	"encoding/binary"
	"fmt"

	"gosuda.org/hqq/internal/protocol"
)

// Fixed values that replace the live words of a Go run (random owner tokens,
// the random creator token and the wall-clock heartbeats and init markers).
// Model images use them directly, so model images and normalized Go images
// are byte-identical. See Manifest.Normalized.
const (
	normPrimaryToken   uint64 = 0x1111111111111111
	normSecondaryToken uint64 = 0x2222222222222222
	normInitToken      uint64 = 0x3333333333333333
	normCreatorToken   uint64 = 0x4444444444444444
)

// segImage is a segment image of one page family, count and size, laid out
// by ComputeRegions (SPEC 2.1). Every word is written through the offsets of
// layout.go, so the bytes are the spec's bytes on any host.
type segImage struct {
	page, count, size int
	reg               Regions
	b                 []byte
}

// newSegImage returns a zero-filled segment image for the geometry.
func newSegImage(page, count, size int) (*segImage, error) {
	reg, ok := ComputeRegions(page, count, size)
	if !ok {
		return nil, fmt.Errorf("conformance: no layout for page %d, count %d, size %d", page, count, size)
	}
	return &segImage{page: page, count: count, size: size, reg: reg, b: make([]byte, reg.Total)}, nil
}

func (s *segImage) setWord(off int, v uint64) {
	binary.LittleEndian.PutUint64(s.b[off:off+8], v)
}

// sbState is the superblock page content (SPEC 3.1). Zero values are the
// zero word of the spec.
type sbState struct {
	magic, state, major, minor, pageSize, bufferCount, bufferSize, layoutSize uint64
	epoch, initToken, initHeartbeat, requiredFeatures, creatorToken           uint64
}

// endpointState is one endpoint record (SPEC 3.2).
type endpointState struct {
	token, pid, heartbeat, interval uint64
}

// setSuperblock writes the superblock words and both endpoint records.
func (s *segImage) setSuperblock(sb sbState, ep [2]endpointState) {
	words := [...]uint64{
		sb.magic, sb.state, sb.major, sb.minor, sb.pageSize, sb.bufferCount,
		sb.bufferSize, sb.layoutSize, sb.epoch, sb.initToken, sb.initHeartbeat,
		sb.requiredFeatures, sb.creatorToken,
	}
	for i, w := range words {
		s.setWord(i*8, w)
	}
	for i, e := range ep {
		base := SBEndpointPrimary + i*RecordSize
		s.setWord(base+RecToken, e.token)
		s.setWord(base+RecPID, e.pid)
		s.setWord(base+RecHeartbeat, e.heartbeat)
		s.setWord(base+RecInterval, e.interval)
	}
}

// ringBase returns the offsets of the ring and of the buffer pool of
// direction d (0: Primary to Secondary, 1: Secondary to Primary).
func (s *segImage) ringBase(d int) (ring, buf int) {
	if d == 0 {
		return s.reg.Ring0, s.reg.Buf0
	}
	return s.reg.Ring1, s.reg.Buf1
}

// slotOff returns the offset of slot slot of the ring of direction d.
func (s *segImage) slotOff(d, slot int) int {
	ring, _ := s.ringBase(d)
	return ring + RingHeaderSize + slot*RingSlotStride
}

// formatRing writes the image of Format (SPEC 7.2) for direction d: header
// words, slot sequences seq[i] = i, empty packets, cursors and waiter words
// at zero.
func (s *segImage) formatRing(d int) {
	ring, _ := s.ringBase(d)
	s.setWord(ring+RingMagic, MagicRing)
	s.setWord(ring+RingSize, uint64(s.count))
	s.setWord(ring+RingFlag, RingFlagInit)
	s.setWord(ring+RingStride, RingSlotStride)
	s.setWord(ring+RingVersionOff, RingVersion)
	for i := range s.count {
		s.setWord(s.slotOff(d, i)+RingSlotSeq, uint64(i))
	}
}

// setCursors writes the consumer and producer positions of direction d.
func (s *segImage) setCursors(d int, r, w uint64) {
	ring, _ := s.ringBase(d)
	s.setWord(ring+RingR, r)
	s.setWord(ring+RingW, w)
}

// setSlotSeq writes the sequence word of slot slot of direction d.
func (s *segImage) setSlotSeq(d, slot int, seq uint64) {
	s.setWord(s.slotOff(d, slot)+RingSlotSeq, seq)
}

// publish stores a published packet at position pos of direction d: the
// packet and the payload go to slot pos&mask and the slot sequence becomes
// pos+1 (SPEC 7.4, publish). Cursors are not moved; callers set them.
func (s *segImage) publish(d int, pos uint64, pkt protocol.Packet, payload []byte) {
	_, buf := s.ringBase(d)
	slot := int(pos & uint64(s.count-1))
	so := s.slotOff(d, slot)
	s.setWord(so+RingSlotSeq, pos+1)
	copy(s.b[so+RingSlotPacket:so+RingSlotPacket+PacketSize], pkt[:])
	if len(payload) > s.size {
		panic("conformance: payload larger than the buffer size")
	}
	copy(s.b[buf+slot*s.size:], payload)
}

// normalizeSegment rewrites, in place, the words of a segment image that a
// live run fills with random or wall-clock values, and returns the names of
// the words that held such a value. Owner tokens become the fixed owner
// tokens (0 and 1 are kept); the creator token and the init token become
// fixed values when nonzero; pid, heartbeat and initHeartbeat become 0.
// Everything else (state, cursors, sequences, buffers) is left alone.
func normalizeSegment(b []byte) []string {
	var changed []string
	rewrite := func(off int, name string, want uint64) {
		if binary.LittleEndian.Uint64(b[off:off+8]) == 0 {
			return
		}
		binary.LittleEndian.PutUint64(b[off:off+8], want)
		changed = append(changed, name)
	}
	rewrite(SBInitToken, "initToken", normInitToken)
	rewrite(SBInitHeartbeat, "initHeartbeat", 0)
	rewrite(SBCreatorToken, "creatorToken", normCreatorToken)
	owner := [2]uint64{normPrimaryToken, normSecondaryToken}
	for i := range 2 {
		base := SBEndpointPrimary + i*RecordSize
		name := fmt.Sprintf("endpoints[%d]", i)
		if tok := binary.LittleEndian.Uint64(b[base+RecToken:]); tok != 0 && tok != TokenDetached {
			rewrite(base+RecToken, name+".token", owner[i])
		}
		rewrite(base+RecPID, name+".pid", 0)
		rewrite(base+RecHeartbeat, name+".heartbeat", 0)
	}
	return changed
}

// normalizedWords returns the normalized-word names that carry live values in
// a segment image, applying the rule of normalizeSegment to a copy of b.
func normalizedWords(b []byte) []string {
	return normalizeSegment(append([]byte(nil), b...))
}

// modelPrimaryReady returns the normalized image of a Primary that published
// a Ready segment on caller memory with heartbeats disabled (SPEC 5.3, 6.1).
// It is the model of golden sequence step 1 and is cross-checked against the
// Go implementation at generation time.
func modelPrimaryReady(page, count, size int) ([]byte, error) {
	s, err := newSegImage(page, count, size)
	if err != nil {
		return nil, err
	}
	s.setSuperblock(sbState{
		magic:       MagicSuperblock,
		state:       StateReady,
		major:       LayoutMajor,
		minor:       LayoutMinor,
		pageSize:    uint64(page),
		bufferCount: uint64(count),
		bufferSize:  uint64(size),
		layoutSize:  uint64(s.reg.Total),
		epoch:       1,
	}, [2]endpointState{{token: normPrimaryToken}, {}})
	s.formatRing(0)
	s.formatRing(1)
	return s.b, nil
}
