package conformance

// Spec-derived constants shared by the vector generator, the conformance
// tests, the interop helper (cmd/hqq-interop) and any other tool that reads
// or writes raw segment bytes. Values come from spec/constants.json and
// spec/SPEC.md; TestConstantsMatchSpec (conformance_test.go) asserts that
// this file and the JSON agree.

// Magics and versions.
const (
	MagicSuperblock = 0x4851515F53544C33 // "HQQ_STL3" when read as big-endian ASCII
	MagicRing       = 0xC9D8C1D43F096702
	LayoutMajor     = 3
	LayoutMinor     = 0
	RingVersion     = 2
	ProtocolMajor   = 1
	ProtocolMinor   = 1 // 1.1 (spec/ext/PROTOCOL_1_1_DRAFT.md); 1.0 is 0
)

// Superblock states (SPEC 3.1).
const (
	StateEmpty        = 0
	StateInitializing = 1
	StateReady        = 2
)

// Superblock field offsets (SPEC 3.1). Every field is a little-endian u64.
const (
	SBMagic            = 0
	SBState            = 8
	SBMajor            = 16
	SBMinor            = 24
	SBPageSize         = 32
	SBBufferCount      = 40
	SBBufferSize       = 48
	SBLayoutSize       = 56
	SBEpoch            = 64
	SBInitToken        = 72
	SBInitHeartbeat    = 80
	SBRequiredFeatures = 88
	SBCreatorToken     = 96
	SBReserved         = 104 // three words, zero
	SBEndpointPrimary  = 128
	SBEndpointSecond   = 256
	SuperblockSize     = 384
)

// Endpoint record field offsets (SPEC 3.2); records are 128 bytes.
const (
	RecToken     = 0
	RecPID       = 8
	RecHeartbeat = 16
	RecInterval  = 24
	RecordSize   = 128
)

// Token values (SPEC 3.2). Owner tokens are every other value.
const (
	TokenUnused   = 0
	TokenDetached = 1
)

// Ring layout (SPEC 7.1).
const (
	RingMagic        = 0
	RingSize         = 8
	RingFlag         = 16
	RingStride       = 24
	RingVersionOff   = 32
	RingR            = 128
	RingW            = 256
	RingNotEmpty     = 384 // u32
	RingEmptyWaiters = 388 // u32
	RingNotFull      = 392 // u32
	RingFullWaiters  = 396 // u32
	RingHeaderSize   = 512
	RingSlotStride   = 128
	RingSlotSeq      = 0  // slot-relative
	RingSlotPacket   = 8  // slot-relative, 64 bytes
	RingFlagInit     = 2  // flag bit1 = initialized; bit0 reserved
	PacketSize       = 64 // bytes
)

// Geometry limits (SPEC 2.2).
const (
	MinBufferCount = 2
	MaxBufferCount = 1 << 24
	MinBufferSize  = 8
	MaxBufferSize  = 1 << 30
)

// Region computation (SPEC 2.1).
type Regions struct {
	Ring0, Buf0, Ring1, Buf1, Total int
}

// ComputeRegions returns the region offsets of a segment with the given page
// size and geometry. ok is false when the geometry is outside the limits of
// SPEC 2.2 (the Go size computation yields 0 in that case).
func ComputeRegions(page, count, size int) (r Regions, ok bool) {
	if page < 4096 || page&(page-1) != 0 {
		return Regions{}, false
	}
	if count < MinBufferCount || count > MaxBufferCount || count&(count-1) != 0 {
		return Regions{}, false
	}
	if size < MinBufferSize || size > MaxBufferSize || size%8 != 0 {
		return Regions{}, false
	}
	roundUp := func(x int) int { return (x + page - 1) / page * page }
	ringBytes := RingHeaderSize + RingSlotStride*count
	bufBytes := count * size
	r.Ring0 = roundUp(SuperblockSize)
	r.Buf0 = roundUp(r.Ring0 + ringBytes)
	r.Ring1 = roundUp(r.Buf0 + bufBytes)
	r.Buf1 = roundUp(r.Ring1 + ringBytes)
	r.Total = roundUp(r.Buf1 + bufBytes)
	return r, true
}
