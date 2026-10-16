package conformance

import (
	"encoding/json"
	"errors"
	"os"
	"strconv"
	"strings"
	"testing"

	"gosuda.org/hqq"
	"gosuda.org/hqq/internal/mpmc"
	"gosuda.org/hqq/internal/protocol"
	"gosuda.org/hqq/internal/shm"
)

// field is an offset/size pair of the document.
type field struct {
	Name   string `json:"name"`
	Offset int    `json:"offset"`
	Size   int    `json:"size"`
}

// specConstants is the part of spec/constants.json this package checks. Keys
// are the names of the document.
type specConstants struct {
	Versions struct {
		SegmentLayoutMajor int `json:"segment_layout_major"`
		SegmentLayoutMinor int `json:"segment_layout_minor"`
		RingLayout         int `json:"ring_layout"`
		ProtocolMajor      int `json:"protocol_major"`
		ProtocolMinor      int `json:"protocol_minor"`
		ProtocolEncoded    int `json:"protocol_encoded"`
	} `json:"versions"`
	Segment struct {
		Magic          string         `json:"magic"`
		MagicASCII     string         `json:"magic_ascii"`
		MinPageSize    int            `json:"min_page_size"`
		SuperblockSize int            `json:"superblock_size"`
		Fields         []field        `json:"superblock_fields"`
		States         map[string]int `json:"states"`
		RecordSize     int            `json:"endpoint_record_size"`
		RecordFields   []field        `json:"endpoint_record_fields"`
		Endpoints      struct {
			PrimaryOffset   int `json:"primary_offset"`
			SecondaryOffset int `json:"secondary_offset"`
		} `json:"endpoints"`
		Tokens struct {
			Unused   int `json:"unused"`
			Detached int `json:"detached"`
		} `json:"tokens"`
		LayoutVectors []struct {
			PageSize    int `json:"page_size"`
			BufferCount int `json:"buffer_count"`
			BufferSize  int `json:"buffer_size"`
			Ring0       int `json:"ring0"`
			Buffers0    int `json:"buffers0"`
			Ring1       int `json:"ring1"`
			Buffers1    int `json:"buffers1"`
			Total       int `json:"total"`
		} `json:"layout_vectors"`
	} `json:"segment"`
	Geometry struct {
		MinBufferCount int `json:"min_buffer_count"`
		MaxBufferCount int `json:"max_buffer_count"`
		MinBufferSize  int `json:"min_buffer_size"`
		MaxBufferSize  int `json:"max_buffer_size"`
	} `json:"geometry"`
	Ring struct {
		HeaderSize int    `json:"header_size"`
		Magic      string `json:"magic"`
		Layout     int    `json:"layout_version"`
		Flags      struct {
			Init int `json:"init"`
		} `json:"flags"`
		HeaderFields []field `json:"header_fields"`
		Slot         struct {
			SeqOffset     int `json:"seq_offset"`
			PayloadOffset int `json:"payload_offset"`
			PayloadSize   int `json:"payload_size"`
			Stride        int `json:"stride"`
			FirstSlot     int `json:"first_slot_offset"`
		} `json:"slot"`
	} `json:"ring"`
	Packet struct {
		Size int `json:"size"`
	} `json:"packet"`
	Opcodes  map[string]int `json:"opcodes"`
	Advanced struct {
		Features struct {
			Messages        int `json:"messages"`
			RequestResponse int `json:"request_response"`
		} `json:"features"`
		FrameFlags map[string]int `json:"frame_flags"`
	} `json:"advanced"`
	ErrorCodes map[string]int `json:"error_codes"`
	Platform   struct {
		Windows struct {
			SemaphoreCount int `json:"semaphore_count"`
		} `json:"windows"`
	} `json:"platform"`
}

// loadSpecConstants parses spec/constants.json (relative to this package).
func loadSpecConstants(t *testing.T) specConstants {
	t.Helper()
	raw, err := os.ReadFile("../spec/constants.json")
	if err != nil {
		t.Fatalf("read spec/constants.json: %v", err)
	}
	var c specConstants
	if err := json.Unmarshal(raw, &c); err != nil {
		t.Fatalf("decode spec/constants.json: %v", err)
	}
	return c
}

// parseHex parses a "0x..." string of the document.
func parseHex(t *testing.T, s string) uint64 {
	t.Helper()
	v, err := strconv.ParseUint(strings.TrimPrefix(s, "0x"), 16, 64)
	if err != nil {
		t.Fatalf("parse %q: %v", s, err)
	}
	return v
}

// TestConstantsMatchSpec (G-01) checks layout.go and the internal packages
// against spec/constants.json.
func TestConstantsMatchSpec(t *testing.T) {
	c := loadSpecConstants(t)

	check := func(name string, got, want int) {
		t.Helper()
		if got != want {
			t.Errorf("%s = %d, constants.json says %d", name, got, want)
		}
	}

	check("LayoutMajor", LayoutMajor, c.Versions.SegmentLayoutMajor)
	check("LayoutMinor", LayoutMinor, c.Versions.SegmentLayoutMinor)
	check("RingVersion", RingVersion, c.Versions.RingLayout)
	check("ProtocolMajor", ProtocolMajor, c.Versions.ProtocolMajor)
	// constants.json still describes the 1.0 baseline (protocol_minor 0,
	// encoded 256). layout.go and the Go code carry 1.1
	// (spec/ext/PROTOCOL_1_1_DRAFT.md). The difference is pinned here and
	// reported; the document is the one to update.
	check("ProtocolMinor (1.1 draft; constants.json baseline is 1.0)", ProtocolMinor, 1)
	check("protocol_encoded (1.0 baseline)", c.Versions.ProtocolEncoded, 256)

	if got := parseHex(t, c.Segment.Magic); got != MagicSuperblock {
		t.Errorf("MagicSuperblock = %#x, constants.json %#x", MagicSuperblock, got)
	}
	if c.Segment.MagicASCII != "HQQ_STL3" {
		t.Errorf("constants.json magic_ascii = %q", c.Segment.MagicASCII)
	}
	if got := parseHex(t, c.Ring.Magic); got != MagicRing {
		t.Errorf("MagicRing = %#x, constants.json %#x", uint64(MagicRing), got)
	}
	check("SuperblockSize", SuperblockSize, c.Segment.SuperblockSize)
	check("RecordSize", RecordSize, c.Segment.RecordSize)
	check("SBEndpointPrimary", SBEndpointPrimary, c.Segment.Endpoints.PrimaryOffset)
	check("SBEndpointSecond", SBEndpointSecond, c.Segment.Endpoints.SecondaryOffset)
	check("StateEmpty", StateEmpty, c.Segment.States["empty"])
	check("StateInitializing", StateInitializing, c.Segment.States["initializing"])
	check("StateReady", StateReady, c.Segment.States["ready"])
	check("TokenUnused", TokenUnused, c.Segment.Tokens.Unused)
	check("TokenDetached", TokenDetached, c.Segment.Tokens.Detached)

	superblockOff := map[string]int{
		"magic": SBMagic, "state": SBState, "major": SBMajor, "minor": SBMinor,
		"pageSize": SBPageSize, "bufferCount": SBBufferCount, "bufferSize": SBBufferSize,
		"layoutSize": SBLayoutSize, "epoch": SBEpoch, "initToken": SBInitToken,
		"initHeartbeat": SBInitHeartbeat, "requiredFeatures": SBRequiredFeatures,
		"creatorToken": SBCreatorToken, "reserved": SBReserved, "endpoints": SBEndpointPrimary,
	}
	if len(c.Segment.Fields) != len(superblockOff) {
		t.Errorf("constants.json lists %d superblock fields, the test knows %d", len(c.Segment.Fields), len(superblockOff))
	}
	for _, f := range c.Segment.Fields {
		off, ok := superblockOff[f.Name]
		if !ok {
			t.Errorf("superblock field %q has no constant", f.Name)
			continue
		}
		check("superblock "+f.Name, off, f.Offset)
	}
	recordOff := map[string]int{"token": RecToken, "pid": RecPID, "heartbeat": RecHeartbeat, "interval": RecInterval}
	for _, f := range c.Segment.RecordFields {
		if off, ok := recordOff[f.Name]; ok {
			check("record "+f.Name, off, f.Offset)
		}
	}

	ringOff := map[string]int{
		"magic": RingMagic, "size": RingSize, "flag": RingFlag, "stride": RingStride,
		"version": RingVersionOff, "r": RingR, "w": RingW, "notEmpty": RingNotEmpty,
		"emptyWaiters": RingEmptyWaiters, "notFull": RingNotFull, "fullWaiters": RingFullWaiters,
	}
	for _, f := range c.Ring.HeaderFields {
		off, ok := ringOff[f.Name]
		if !ok {
			t.Errorf("ring header field %q has no constant", f.Name)
			continue
		}
		check("ring "+f.Name, off, f.Offset)
	}
	check("RingHeaderSize", RingHeaderSize, c.Ring.HeaderSize)
	check("RingFlagInit", RingFlagInit, c.Ring.Flags.Init)
	check("RingSlotStride", RingSlotStride, c.Ring.Slot.Stride)
	check("RingSlotSeq", RingSlotSeq, c.Ring.Slot.SeqOffset)
	check("RingSlotPacket", RingSlotPacket, c.Ring.Slot.PayloadOffset)
	check("PacketSize (ring payload)", PacketSize, c.Ring.Slot.PayloadSize)
	check("ring first slot offset", RingHeaderSize, c.Ring.Slot.FirstSlot)

	check("MinBufferCount", MinBufferCount, c.Geometry.MinBufferCount)
	check("MaxBufferCount", MaxBufferCount, c.Geometry.MaxBufferCount)
	check("MinBufferSize", MinBufferSize, c.Geometry.MinBufferSize)
	check("MaxBufferSize", MaxBufferSize, c.Geometry.MaxBufferSize)
	check("minimum page size", 4096, c.Segment.MinPageSize)

	// Internal packages.
	check("protocol.PacketSize", protocol.PacketSize, c.Packet.Size)
	check("protocol.PacketSize vs layout", protocol.PacketSize, PacketSize)
	check("protocol major", protocol.HQQProtocolMajorVersion, ProtocolMajor)
	check("protocol minor", protocol.HQQProtocolMinorVersion, ProtocolMinor)
	check("mpmc.HeaderSize", mpmc.HeaderSize, RingHeaderSize)
	check("shm.SemaphoreCount", shm.SemaphoreCount, c.Platform.Windows.SemaphoreCount)

	opcodes := map[string]protocol.OpCode{
		"OpError": protocol.OpError, "OpConnCreate": protocol.OpConnCreate,
		"OpConnAccept": protocol.OpConnAccept, "OpConnClose": protocol.OpConnClose,
		"OpConnCopy": protocol.OpConnCopy, "OpProtoNegotiate": protocol.OpProtoNegotiate,
		"OpProtoAck": protocol.OpProtoAck, "OpStandardLinkTombstone": protocol.OpStandardLinkTombstone,
		"OpStandardLinkCopy": protocol.OpStandardLinkCopy,
	}
	for name, op := range opcodes {
		check(name, int(op), c.Opcodes[name])
	}
	check("OpCancel (PROTOCOL_1_1_DRAFT section 2)", int(protocol.OpCancel), 0x07)

	check("FeatureMessages", int(hqq.FeatureMessages), c.Advanced.Features.Messages)
	check("FeatureRequestResponse", int(hqq.FeatureRequestResponse), c.Advanced.Features.RequestResponse)
	flags := map[string]uint64{
		"Begin": hqq.AdvFlagBegin, "End": hqq.AdvFlagEnd, "Abort": hqq.AdvFlagAbort,
		"Request": hqq.AdvFlagRequest, "Response": hqq.AdvFlagResponse, "Error": hqq.AdvFlagError,
	}
	for name, v := range flags {
		check("AdvFlag"+name, int(v), c.Advanced.FrameFlags[name])
	}
	check("AdvFlagMeta (1.1 draft: 1 << 6)", int(hqq.AdvFlagMeta), 0x40)

	codes := map[string]hqq.ErrorCode{
		"MemoryAlign": hqq.ErrCodeMemoryAlign, "InvalidSize": hqq.ErrCodeInvalidSize,
		"MemorySmall": hqq.ErrCodeMemorySmall, "FailedInit": hqq.ErrCodeFailedInit,
		"ConnNotFound": hqq.ErrCodeConnNotFound, "InvalidOp": hqq.ErrCodeInvalidOp,
		"BufferOverflow": hqq.ErrCodeBufferOverflow,
	}
	for name, code := range codes {
		check("ErrCode"+name, int(code), c.ErrorCodes[name])
	}
	// Not in constants.json: ErrCodeShuttingDown is defined by the 1.1 draft.
	check("ErrCodeShuttingDown (1.1 draft)", int(hqq.ErrCodeShuttingDown), 8)
}

// TestLayoutVectors (G-03) checks the region-offset vectors: the twelve
// constants.json vectors against the document, every manifest vector against
// ComputeRegions and, on the host page family, against the total the Go
// implementation reports.
func TestLayoutVectors(t *testing.T) {
	m := loadManifest(t)
	c := loadSpecConstants(t)

	type key struct{ page, cnt, size int }
	fromJSON := map[key]Regions{}
	for _, v := range c.Segment.LayoutVectors {
		fromJSON[key{v.PageSize, v.BufferCount, v.BufferSize}] = Regions{
			Ring0: v.Ring0, Buf0: v.Buffers0, Ring1: v.Ring1, Buf1: v.Buffers1, Total: v.Total,
		}
	}
	if len(fromJSON) != 12 {
		t.Fatalf("constants.json has %d layout vectors, want 12", len(fromJSON))
	}
	seen := map[key]bool{}
	for _, lv := range m.Layouts {
		k := key{lv.PageSize, lv.BufferCount, lv.BufferSize}
		seen[k] = true
		r, ok := ComputeRegions(lv.PageSize, lv.BufferCount, lv.BufferSize)
		if !ok {
			t.Errorf("%+v: outside the geometry limits", lv)
			continue
		}
		got := Regions{Ring0: lv.Ring0, Buf0: lv.Buffers0, Ring1: lv.Ring1, Buf1: lv.Buffers1, Total: lv.Total}
		if got != r {
			t.Errorf("%+v: manifest %+v, ComputeRegions %+v", lv, got, r)
		}
		if want, ok := fromJSON[k]; ok && lv.Source == "constants.json" && want != got {
			t.Errorf("%+v: constants.json says %+v", lv, want)
		}
		if lv.PageSize == hostPage() {
			if total := hqq.SizeStandardLink(lv.BufferCount, lv.BufferSize); int(total) != lv.Total {
				t.Errorf("%+v: SizeStandardLink = %d on the host page family", lv, total)
			}
		}
	}
	for k := range fromJSON {
		if !seen[k] {
			t.Errorf("constants.json layout vector %+v is missing from the manifest", k)
		}
	}
}

// TestGeometryLimits (G-04) checks SizeStandardLink and NewStandardLink on
// invalid geometry.
func TestGeometryLimits(t *testing.T) {
	m := loadManifest(t)
	mem := hqq.AlignedBuffer(4096)
	for _, g := range m.Geometry {
		size := hqq.SizeStandardLink(g.BufferCount, g.BufferSize)
		if !g.Valid {
			if size != 0 {
				t.Errorf("count %d size %d (%s): SizeStandardLink = %d, want 0", g.BufferCount, g.BufferSize, g.Reason, size)
			}
			link, err := hqq.NewStandardLink(mem, g.BufferCount, g.BufferSize, testOpts())
			if link != nil {
				_ = link.Close()
				t.Errorf("count %d size %d: NewStandardLink created a link", g.BufferCount, g.BufferSize)
			}
			if !errors.Is(err, hqq.ErrInvalidSize) {
				t.Errorf("count %d size %d (%s): NewStandardLink error %v, want ErrInvalidSize", g.BufferCount, g.BufferSize, g.Reason, err)
			}
			continue
		}
		if size == 0 {
			t.Errorf("count %d size %d: valid geometry has size 0", g.BufferCount, g.BufferSize)
		}
	}
}
