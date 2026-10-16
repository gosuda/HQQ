package hqq

// Executable cross-check between spec/constants.json (the machine-readable
// ABI of the specification, see spec/SPEC.md) and the code. A failure here
// means the specification and the implementation drifted apart: either update
// both together (and bump the layout/protocol version if the ABI changed) or
// revert the change.

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unsafe"

	"gosuda.org/hqq/internal/mpmc"
	"gosuda.org/hqq/internal/protocol"
	"gosuda.org/hqq/internal/shm"
)

type specField struct {
	Name   string  `json:"name"`
	Offset uintptr `json:"offset"`
	Size   uintptr `json:"size"`
}

type specLayoutVector struct {
	PageSize    uintptr `json:"page_size"`
	BufferCount int     `json:"buffer_count"`
	BufferSize  int     `json:"buffer_size"`
	Ring0       uintptr `json:"ring0"`
	Buffers0    uintptr `json:"buffers0"`
	Ring1       uintptr `json:"ring1"`
	Buffers1    uintptr `json:"buffers1"`
	Total       uintptr `json:"total"`
}

type specConsts struct {
	Versions struct {
		SegmentLayoutMajor uint64 `json:"segment_layout_major"`
		SegmentLayoutMinor uint64 `json:"segment_layout_minor"`
		RingLayout         uint64 `json:"ring_layout"`
		ProtocolMajor      uint64 `json:"protocol_major"`
		ProtocolMinor      uint64 `json:"protocol_minor"`
		ProtocolEncoded    uint64 `json:"protocol_encoded"`
	} `json:"versions"`
	Segment struct {
		Magic                 string            `json:"magic"`
		MagicASCII            string            `json:"magic_ascii"`
		MinPageSize           uintptr           `json:"min_page_size"`
		KnownRequiredFeatures uint64            `json:"known_required_features"`
		SuperblockSize        uintptr           `json:"superblock_size"`
		SuperblockFields      []specField       `json:"superblock_fields"`
		States                map[string]uint64 `json:"states"`
		EndpointRecordSize    uintptr           `json:"endpoint_record_size"`
		EndpointRecordFields  []specField       `json:"endpoint_record_fields"`
		Endpoints             struct {
			PrimaryIndex    int     `json:"primary_index"`
			SecondaryIndex  int     `json:"secondary_index"`
			PrimaryOffset   uintptr `json:"primary_offset"`
			SecondaryOffset uintptr `json:"secondary_offset"`
		} `json:"endpoints"`
		Tokens struct {
			Unused   uint64 `json:"unused"`
			Detached uint64 `json:"detached"`
		} `json:"tokens"`
		LayoutVectors []specLayoutVector `json:"layout_vectors"`
	} `json:"segment"`
	Geometry struct {
		MinBufferCount      int `json:"min_buffer_count"`
		MaxBufferCount      int `json:"max_buffer_count"`
		MinBufferSize       int `json:"min_buffer_size"`
		MaxBufferSize       int `json:"max_buffer_size"`
		BufferSizeAlignment int `json:"buffer_size_alignment"`
	} `json:"geometry"`
	Ring struct {
		HeaderSize   uintptr     `json:"header_size"`
		Magic        string      `json:"magic"`
		HeaderFields []specField `json:"header_fields"`
		Slot         struct {
			SeqOffset     uintptr `json:"seq_offset"`
			PayloadOffset uintptr `json:"payload_offset"`
			Stride        uintptr `json:"stride"`
		} `json:"slot"`
		Flags struct {
			Init uint64 `json:"init"`
		} `json:"flags"`
		LayoutVersion uint64 `json:"layout_version"`
	} `json:"ring"`
	PacketVectors map[string]struct {
		Opcode   uint64   `json:"opcode"`
		Operands []uint64 `json:"operands"`
		Hex      string   `json:"hex"`
	} `json:"packet_vectors"`
	ErrorCodes map[string]uint64 `json:"error_codes"`
	Advanced   struct {
		Features struct {
			Messages        uint64 `json:"messages"`
			RequestResponse uint64 `json:"request_response"`
			SupportedMask   uint64 `json:"supported_mask"`
		} `json:"features"`
		FrameFlags            map[string]uint64 `json:"frame_flags"`
		KnownFlagsMask        uint64            `json:"known_flags_mask"`
		DefaultMaxMessageSize uint64            `json:"default_max_message_size"`
		ReassemblyFactor      uint64            `json:"reassembly_factor"`
		AssemblyOverhead      uint64            `json:"assembly_overhead"`
		MaxAbandonedCalls     int               `json:"max_abandoned_calls"`
		MaxRejectedMessages   int               `json:"max_rejected_messages"`
		MinControlQueue       int               `json:"min_control_queue"`
		PrimaryFirstStreamID  uint64            `json:"primary_first_stream_id"`
		SecondaryFirstStream  uint64            `json:"secondary_first_stream_id"`
		StreamIDStep          uint64            `json:"stream_id_step"`
		FirstMessageID        uint64            `json:"first_message_id"`
		ProtocolStates        map[string]int    `json:"protocol_states"`
		OverflowPolicies      map[string]int    `json:"overflow_policies"`
	} `json:"advanced"`
	LocalEnums struct {
		LinkMode   map[string]uint64 `json:"link_mode"`
		LinkType   map[string]uint64 `json:"link_type"`
		PeerState  map[string]uint32 `json:"peer_state"`
		WaitPolicy map[string]uint8  `json:"wait_policy"`
	} `json:"local_enums"`
	TimingNS struct {
		HeartbeatInterval   int64  `json:"default_heartbeat_interval"`
		PeerTimeout         int64  `json:"default_peer_timeout"`
		AttachTimeout       int64  `json:"default_attach_timeout"`
		InitStaleFloor      int64  `json:"init_stale_floor"`
		ClaimPollInterval   int64  `json:"claim_poll_interval"`
		RingAttachSlice     int64  `json:"ring_attach_slice"`
		PeerTimeoutMultiple int64  `json:"peer_timeout_min_heartbeat_multiple"`
		ZeroChunkBytes      uint64 `json:"zero_chunk_bytes"`
	} `json:"timing_ns"`
	Platform struct {
		NameValidation struct {
			MaxBaseBytes   int      `json:"max_base_bytes"`
			InvalidVectors []string `json:"invalid_vectors"`
		} `json:"name_validation"`
		Darwin struct {
			MaxName int `json:"max_name_bytes_including_slash"`
		} `json:"darwin"`
		Windows struct {
			SemaphoreCount int `json:"semaphore_count"`
		} `json:"windows"`
	} `json:"platform"`
}

func loadSpecConsts(t *testing.T) *specConsts {
	t.Helper()
	raw, err := os.ReadFile("spec/constants.json")
	if err != nil {
		t.Fatalf("read spec/constants.json: %v", err)
	}
	var c specConsts
	if err := json.Unmarshal(raw, &c); err != nil {
		t.Fatalf("parse spec/constants.json: %v", err)
	}
	return &c
}

func specHex64(t *testing.T, s string) uint64 {
	t.Helper()
	v, err := strconv.ParseUint(strings.TrimPrefix(s, "0x"), 16, 64)
	if err != nil {
		t.Fatalf("bad hex constant %q: %v", s, err)
	}
	return v
}

func specEq[T comparable](t *testing.T, what string, got, want T) {
	t.Helper()
	if got != want {
		t.Errorf("%s = %v, spec/constants.json says %v", what, got, want)
	}
}

func TestSpecSuperblockLayout(t *testing.T) {
	c := loadSpecConsts(t)

	specEq(t, "superblockMagic", superblockMagic, specHex64(t, c.Segment.Magic))
	var be [8]byte
	binary.BigEndian.PutUint64(be[:], superblockMagic)
	specEq(t, "magic ASCII", string(be[:]), c.Segment.MagicASCII)
	specEq(t, "superblockMajor", superblockMajor, c.Versions.SegmentLayoutMajor)
	specEq(t, "superblockMinor", superblockMinor, c.Versions.SegmentLayoutMinor)
	specEq(t, "knownRequiredFeatures", knownRequiredFeatures, c.Segment.KnownRequiredFeatures)
	specEq(t, "superblock states: empty", superblockStateEmpty, c.Segment.States["empty"])
	specEq(t, "superblock states: initializing", superblockStateInitializing, c.Segment.States["initializing"])
	specEq(t, "superblock states: ready", superblockStateReady, c.Segment.States["ready"])
	specEq(t, "detachedToken", detachedToken, c.Segment.Tokens.Detached)
	specEq(t, "endpointPrimary", endpointPrimary, c.Segment.Endpoints.PrimaryIndex)
	specEq(t, "endpointSecondary", endpointSecondary, c.Segment.Endpoints.SecondaryIndex)
	specEq(t, "min page size fits superblock", unsafe.Sizeof(standardSuperblock{}) <= c.Segment.MinPageSize, true)

	var sb standardSuperblock
	offsets := map[string]uintptr{
		"magic":            unsafe.Offsetof(sb.magic),
		"state":            unsafe.Offsetof(sb.state),
		"major":            unsafe.Offsetof(sb.major),
		"minor":            unsafe.Offsetof(sb.minor),
		"pageSize":         unsafe.Offsetof(sb.pageSize),
		"bufferCount":      unsafe.Offsetof(sb.bufferCount),
		"bufferSize":       unsafe.Offsetof(sb.bufferSize),
		"layoutSize":       unsafe.Offsetof(sb.layoutSize),
		"epoch":            unsafe.Offsetof(sb.epoch),
		"initToken":        unsafe.Offsetof(sb.initToken),
		"initHeartbeat":    unsafe.Offsetof(sb.initHeartbeat),
		"requiredFeatures": unsafe.Offsetof(sb.requiredFeatures),
		"creatorToken":     unsafe.Offsetof(sb.creatorToken),
		"endpoints":        unsafe.Offsetof(sb.endpoints),
	}
	specEq(t, "sizeof superblock", unsafe.Sizeof(sb), c.Segment.SuperblockSize)
	seen := 0
	var prevEnd uintptr
	for _, f := range c.Segment.SuperblockFields {
		if f.Offset != prevEnd {
			t.Errorf("superblock field %s at %d leaves a gap or overlap (previous field ends at %d)", f.Name, f.Offset, prevEnd)
		}
		prevEnd = f.Offset + f.Size
		if f.Name == "reserved" {
			specEq(t, "reserved start", offsets["creatorToken"]+8, f.Offset)
			specEq(t, "reserved end", offsets["endpoints"], f.Offset+f.Size)
			continue
		}
		got, ok := offsets[f.Name]
		if !ok {
			t.Errorf("spec superblock field %q is unknown to the test", f.Name)
			continue
		}
		seen++
		specEq(t, "offset of superblock."+f.Name, got, f.Offset)
		if f.Name != "endpoints" {
			specEq(t, "size of superblock."+f.Name, f.Size, 8)
		}
	}
	specEq(t, "superblock field count", seen, len(offsets))
	specEq(t, "superblock fields cover the struct", prevEnd, c.Segment.SuperblockSize)

	var rec endpointRecord
	recOffsets := map[string]uintptr{
		"token":     unsafe.Offsetof(rec.token),
		"pid":       unsafe.Offsetof(rec.pid),
		"heartbeat": unsafe.Offsetof(rec.heartbeat),
		"interval":  unsafe.Offsetof(rec.interval),
	}
	specEq(t, "sizeof endpointRecord", unsafe.Sizeof(rec), c.Segment.EndpointRecordSize)
	prevEnd = 0
	for _, f := range c.Segment.EndpointRecordFields {
		if f.Offset != prevEnd {
			t.Errorf("endpoint field %s at %d leaves a gap or overlap", f.Name, f.Offset)
		}
		prevEnd = f.Offset + f.Size
		if f.Name == "reserved" {
			specEq(t, "endpoint reserved start", recOffsets["interval"]+8, f.Offset)
			continue
		}
		specEq(t, "offset of endpointRecord."+f.Name, recOffsets[f.Name], f.Offset)
		specEq(t, "size of endpointRecord."+f.Name, f.Size, 8)
	}
	specEq(t, "endpoint fields cover the record", prevEnd, c.Segment.EndpointRecordSize)
	specEq(t, "primary record offset", unsafe.Offsetof(sb.endpoints), c.Segment.Endpoints.PrimaryOffset)
	specEq(t, "secondary record offset", unsafe.Offsetof(sb.endpoints)+unsafe.Sizeof(sb.endpoints[0]), c.Segment.Endpoints.SecondaryOffset)
}

func TestSpecGeometryAndRegions(t *testing.T) {
	c := loadSpecConsts(t)
	specEq(t, "MinBufferCount", MinBufferCount, c.Geometry.MinBufferCount)
	specEq(t, "maxBufferCount", maxBufferCount, c.Geometry.MaxBufferCount)
	specEq(t, "MinBufferSize", MinBufferSize, c.Geometry.MinBufferSize)
	specEq(t, "maxBufferSize", maxBufferSize, c.Geometry.MaxBufferSize)
	specEq(t, "BufferSizeAlignment", BufferSizeAlignment, c.Geometry.BufferSizeAlignment)
	specEq(t, "mpmc.HeaderSize", uintptr(mpmc.HeaderSize), c.Ring.HeaderSize)

	// Invalid configurations yield size 0; the bounds themselves are valid.
	for _, bad := range [][2]int{{0, 8}, {1, 8}, {3, 8}, {2, 0}, {2, 4}, {2, 12}, {c.Geometry.MaxBufferCount * 2, 8}, {2, c.Geometry.MaxBufferSize + 8}} {
		if got := SizeStandardLink(bad[0], bad[1]); got != 0 {
			t.Errorf("SizeStandardLink(%d, %d) = %d, want 0 (invalid geometry)", bad[0], bad[1], got)
		}
	}
	if strconv.IntSize == 64 {
		for _, ok := range [][2]int{{c.Geometry.MinBufferCount, c.Geometry.MinBufferSize}, {c.Geometry.MaxBufferCount, c.Geometry.MinBufferSize}, {2, c.Geometry.MaxBufferSize}} {
			if SizeStandardLink(ok[0], ok[1]) == 0 {
				t.Errorf("SizeStandardLink(%d, %d) = 0, want a valid size", ok[0], ok[1])
			}
		}
	}

	// The region formula of SPEC.md, evaluated independently of the code.
	alignUp := func(x, p uintptr) uintptr { return (x + p - 1) / p * p }
	formula := func(page uintptr, count, size int) (l specLayoutVector) {
		ring := c.Ring.HeaderSize + c.Ring.Slot.Stride*uintptr(count)
		buf := uintptr(count) * uintptr(size)
		l.PageSize, l.BufferCount, l.BufferSize = page, count, size
		l.Ring0 = alignUp(c.Segment.SuperblockSize, page)
		l.Buffers0 = alignUp(l.Ring0+ring, page)
		l.Ring1 = alignUp(l.Buffers0+buf, page)
		l.Buffers1 = alignUp(l.Ring1+ring, page)
		l.Total = alignUp(l.Buffers1+buf, page)
		return l
	}
	for _, v := range c.Segment.LayoutVectors {
		if v.PageSize < c.Segment.MinPageSize || v.PageSize&(v.PageSize-1) != 0 {
			t.Errorf("vector page size %d is not a power of two >= %d", v.PageSize, c.Segment.MinPageSize)
		}
		if got := formula(v.PageSize, v.BufferCount, v.BufferSize); got != v {
			t.Errorf("region formula for %+v gives %+v", v, got)
		}
		if v.PageSize != pagesize {
			continue
		}
		l, ok := computeLayout(v.BufferCount, v.BufferSize)
		if !ok {
			t.Errorf("computeLayout(%d, %d) failed", v.BufferCount, v.BufferSize)
			continue
		}
		got := specLayoutVector{PageSize: pagesize, BufferCount: v.BufferCount, BufferSize: v.BufferSize,
			Ring0: l.ring0, Buffers0: l.buf0, Ring1: l.ring1, Buffers1: l.buf1, Total: l.total}
		if got != v {
			t.Errorf("computeLayout(%d, %d) = %+v, spec vector %+v", v.BufferCount, v.BufferSize, got, v)
		}
		specEq(t, "SizeStandardLink", SizeStandardLink(v.BufferCount, v.BufferSize), v.Total)
	}
	// The host page size follows the same formula for further geometries.
	for _, g := range [][2]int{{2, 8}, {4, 64}, {64, 1024}, {512, 4096}} {
		want := formula(pagesize, g[0], g[1])
		l, ok := computeLayout(g[0], g[1])
		if !ok || l.ring0 != want.Ring0 || l.buf0 != want.Buffers0 || l.ring1 != want.Ring1 || l.buf1 != want.Buffers1 || l.total != want.Total {
			t.Errorf("computeLayout(%d, %d) = %+v ok=%v, formula %+v", g[0], g[1], l, ok, want)
		}
	}
}

func TestSpecVersionsAndEnums(t *testing.T) {
	c := loadSpecConsts(t)
	specEq(t, "protocol major", uint64(protocol.HQQProtocolMajorVersion), c.Versions.ProtocolMajor)
	if uint64(protocol.HQQProtocolMinorVersion) < c.Versions.ProtocolMinor {
		t.Errorf("protocol minor %d is lower than the baseline %d", protocol.HQQProtocolMinorVersion, c.Versions.ProtocolMinor)
	}
	v := CurrentProtocolVersion()
	if uint64(v.Major) != c.Versions.ProtocolMajor {
		t.Errorf("CurrentProtocolVersion().Major = %d, want %d", v.Major, c.Versions.ProtocolMajor)
	}
	baseline := ProtocolVersion{Major: uint8(c.Versions.ProtocolMajor), Minor: uint8(c.Versions.ProtocolMinor)}
	specEq(t, "encoded baseline version", baseline.encode(), c.Versions.ProtocolEncoded)
	specEq(t, "decode(encode(v))", decodeVersion(baseline.encode()), baseline)

	specEq(t, "FeatureMessages", FeatureMessages, c.Advanced.Features.Messages)
	specEq(t, "FeatureRequestResponse", FeatureRequestResponse, c.Advanced.Features.RequestResponse)
	if supportedFeatures&c.Advanced.Features.SupportedMask != c.Advanced.Features.SupportedMask {
		t.Errorf("supportedFeatures = %#x lacks baseline mask %#x", supportedFeatures, c.Advanced.Features.SupportedMask)
	}

	flags := map[string]uint64{"Begin": AdvFlagBegin, "End": AdvFlagEnd, "Abort": AdvFlagAbort, "Request": AdvFlagRequest, "Response": AdvFlagResponse, "Error": AdvFlagError}
	for name, want := range c.Advanced.FrameFlags {
		got, ok := flags[name]
		if !ok {
			t.Errorf("spec frame flag %q unknown to the test", name)
			continue
		}
		specEq(t, "AdvFlag"+name, got, want)
	}
	if advancedKnownFlags&c.Advanced.KnownFlagsMask != c.Advanced.KnownFlagsMask {
		t.Errorf("advancedKnownFlags = %#x lacks baseline mask %#x", advancedKnownFlags, c.Advanced.KnownFlagsMask)
	}

	codes := map[string]ErrorCode{"MemoryAlign": ErrCodeMemoryAlign, "InvalidSize": ErrCodeInvalidSize, "MemorySmall": ErrCodeMemorySmall,
		"FailedInit": ErrCodeFailedInit, "ConnNotFound": ErrCodeConnNotFound, "InvalidOp": ErrCodeInvalidOp, "BufferOverflow": ErrCodeBufferOverflow}
	sentinels := map[string]error{"MemoryAlign": ErrMemoryAlign, "InvalidSize": ErrInvalidSize, "MemorySmall": ErrMemorySmall,
		"FailedInit": ErrFailedInit, "ConnNotFound": ErrConnNotFound, "InvalidOp": ErrInvalidOp, "BufferOverflow": ErrBufferOverflow}
	for name, want := range c.ErrorCodes {
		got, ok := codes[name]
		if !ok {
			t.Errorf("spec error code %q unknown to the test", name)
			continue
		}
		specEq(t, "ErrCode"+name, uint64(got), want)
		if !errors.Is(errorFromCode(got), sentinels[name]) {
			t.Errorf("errorFromCode(%d) does not match Err%s", got, name)
		}
	}
	if err := errorFromCode(ErrorCode(0x99)); !errors.Is(err, ErrProtocol) {
		t.Errorf("errorFromCode(unknown) = %v, want a match for ErrProtocol", err)
	}

	specEq(t, "DefaultMaxAdvancedMessageSize", DefaultMaxAdvancedMessageSize, c.Advanced.DefaultMaxMessageSize)
	specEq(t, "defaultReassemblyFactor", uint64(defaultReassemblyFactor), c.Advanced.ReassemblyFactor)
	specEq(t, "advancedAssemblyOverhead", uint64(advancedAssemblyOverhead), c.Advanced.AssemblyOverhead)
	specEq(t, "maxAbandonedCalls", maxAbandonedCalls, c.Advanced.MaxAbandonedCalls)
	specEq(t, "maxRejectedMessages", maxRejectedMessages, c.Advanced.MaxRejectedMessages)
	specEq(t, "minControlQueue", minControlQueue, c.Advanced.MinControlQueue)
	specEq(t, "ProtocolStateNone", int(ProtocolStateNone), c.Advanced.ProtocolStates["None"])
	specEq(t, "ProtocolStateNegotiating", int(ProtocolStateNegotiating), c.Advanced.ProtocolStates["Negotiating"])
	specEq(t, "ProtocolStateNegotiated", int(ProtocolStateNegotiated), c.Advanced.ProtocolStates["Negotiated"])
	specEq(t, "ProtocolStateFailed", int(ProtocolStateFailed), c.Advanced.ProtocolStates["Failed"])
	specEq(t, "OverflowBlock", int(OverflowBlock), c.Advanced.OverflowPolicies["Block"])
	specEq(t, "OverflowDrop", int(OverflowDrop), c.Advanced.OverflowPolicies["Drop"])
	specEq(t, "OverflowFail", int(OverflowFail), c.Advanced.OverflowPolicies["Fail"])

	specEq(t, "LinkModePrimary", uint64(LinkModePrimary), c.LocalEnums.LinkMode["Primary"])
	specEq(t, "LinkModeSecondary", uint64(LinkModeSecondary), c.LocalEnums.LinkMode["Secondary"])
	specEq(t, "LinkTypeStandard", uint64(LinkTypeStandard), c.LocalEnums.LinkType["Standard"])
	specEq(t, "LinkTypeAdvanced", uint64(LinkTypeAdvanced), c.LocalEnums.LinkType["Advanced"])
	specEq(t, "PeerNotConnected", uint32(PeerNotConnected), c.LocalEnums.PeerState["NotConnected"])
	specEq(t, "PeerConnected", uint32(PeerConnected), c.LocalEnums.PeerState["Connected"])
	specEq(t, "PeerClosed", uint32(PeerClosed), c.LocalEnums.PeerState["Closed"])
	specEq(t, "PeerGone", uint32(PeerGone), c.LocalEnums.PeerState["Gone"])
	specEq(t, "WaitAdaptive", uint8(WaitAdaptive), c.LocalEnums.WaitPolicy["Adaptive"])
	specEq(t, "WaitSpin", uint8(WaitSpin), c.LocalEnums.WaitPolicy["Spin"])
}

func TestSpecTimingDefaults(t *testing.T) {
	c := loadSpecConsts(t)
	tm := c.TimingNS
	specEq(t, "DefaultHeartbeatInterval", int64(DefaultHeartbeatInterval), tm.HeartbeatInterval)
	specEq(t, "DefaultPeerTimeout", int64(DefaultPeerTimeout), tm.PeerTimeout)
	specEq(t, "DefaultAttachTimeout", int64(DefaultAttachTimeout), tm.AttachTimeout)
	specEq(t, "claimPollInterval", int64(claimPollInterval), tm.ClaimPollInterval)
	specEq(t, "ringAttachSlice", int64(ringAttachSlice), tm.RingAttachSlice)
	specEq(t, "zeroChunk", uint64(zeroChunk), tm.ZeroChunkBytes)

	n := LinkOptions{}.normalized()
	specEq(t, "normalized HeartbeatInterval", int64(n.HeartbeatInterval), tm.HeartbeatInterval)
	specEq(t, "normalized PeerTimeout", int64(n.PeerTimeout), tm.PeerTimeout)
	specEq(t, "normalized AttachTimeout", int64(n.AttachTimeout), tm.AttachTimeout)
	specEq(t, "negative heartbeat disables", LinkOptions{HeartbeatInterval: -1}.normalized().HeartbeatInterval, 0)

	// Init stale window: max(PeerTimeout, floor) of the claiming endpoint.
	specEq(t, "initStaleWindow(default)", int64(n.initStaleWindow()), max(tm.PeerTimeout, tm.InitStaleFloor))
	short := LinkOptions{PeerTimeout: time.Second}.normalized()
	specEq(t, "initStaleWindow(short peer timeout)", int64(short.initStaleWindow()), tm.InitStaleFloor)
	long := LinkOptions{PeerTimeout: 30 * time.Second}.normalized()
	specEq(t, "initStaleWindow(long peer timeout)", int64(long.initStaleWindow()), int64(30*time.Second))

	// Effective peer timeout: never below N heartbeat intervals of the peer.
	o := LinkOptions{PeerTimeout: time.Second}
	iv := 1 * time.Second
	specEq(t, "effectivePeerTimeout(interval == timeout)", int64(effectivePeerTimeout(o, iv)), tm.PeerTimeoutMultiple*int64(iv))
	specEq(t, "effectivePeerTimeout(interval << timeout)", effectivePeerTimeout(o, time.Millisecond), time.Second)
}

// TestSpecNameValidation checks the platform-independent part of the name
// rules (SPEC.md section "Platform naming"). Only invalid names are probed, so
// no shared-memory object is touched.
func TestSpecNameValidation(t *testing.T) {
	c := loadSpecConsts(t)
	specEq(t, "shm.SemaphoreCount", shm.SemaphoreCount, c.Platform.Windows.SemaphoreCount)
	switch runtime.GOOS {
	case "linux", "darwin", "freebsd", "windows":
	default:
		t.Skip("no shared-memory backend on " + runtime.GOOS)
	}
	for _, name := range c.Platform.NameValidation.InvalidVectors {
		if err := shm.Unlink(name); !errors.Is(err, shm.ErrInvalidName) {
			t.Errorf("Unlink(%q) = %v, want ErrInvalidName", name, err)
		}
	}
	if err := shm.Unlink(strings.Repeat("x", c.Platform.NameValidation.MaxBaseBytes+1)); !errors.Is(err, shm.ErrInvalidName) {
		t.Errorf("a %d-byte name must be invalid, got %v", c.Platform.NameValidation.MaxBaseBytes+1, err)
	}
	if runtime.GOOS == "darwin" {
		// The leading '/' counts against the limit: 30 bytes of name are the maximum.
		limit := c.Platform.Darwin.MaxName
		if err := shm.Unlink(strings.Repeat("x", limit)); !errors.Is(err, shm.ErrInvalidName) {
			t.Errorf("a %d-byte name (+ '/') must be invalid on darwin, got %v", limit, err)
		}
		if err := shm.Unlink("/" + strings.Repeat("x", limit)); !errors.Is(err, shm.ErrInvalidName) {
			t.Errorf("'/' + %d bytes must be invalid on darwin, got %v", limit, err)
		}
	}
}

func TestSpecTokens(t *testing.T) {
	c := loadSpecConsts(t)
	if isOwnerToken(c.Segment.Tokens.Unused) || isOwnerToken(c.Segment.Tokens.Detached) {
		t.Error("unused/detached tokens must not be owner tokens")
	}
	for range 1000 {
		if tok := newToken(); !isOwnerToken(tok) {
			t.Fatalf("newToken() returned the reserved value %d", tok)
		}
	}
}

// specImage reads little-endian words straight from a segment image.
type specImage []byte

func (m specImage) u64(off uintptr) uint64 { return binary.LittleEndian.Uint64(m[off:]) }
func (m specImage) u32(off uintptr) uint32 { return binary.LittleEndian.Uint32(m[off:]) }

func TestSpecSegmentImage(t *testing.T) {
	c := loadSpecConsts(t)
	const count, size = 4, 64
	mem := AlignedBuffer(SizeStandardLink(count, size))
	img := specImage(mem)
	opts := LinkOptions{HeartbeatInterval: -1} // no background writers: the image is stable
	layout, _ := computeLayout(count, size)
	field := func(name string) uintptr {
		for _, f := range c.Segment.SuperblockFields {
			if f.Name == name {
				return f.Offset
			}
		}
		t.Fatalf("no superblock field %q in spec", name)
		return 0
	}
	rec := func(index int, name string) uintptr {
		base := c.Segment.Endpoints.PrimaryOffset
		if index == c.Segment.Endpoints.SecondaryIndex {
			base = c.Segment.Endpoints.SecondaryOffset
		}
		for _, f := range c.Segment.EndpointRecordFields {
			if f.Name == name {
				return base + f.Offset
			}
		}
		t.Fatalf("no endpoint field %q in spec", name)
		return 0
	}

	p, err := NewStandardLink(mem, count, size, opts)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	specEq(t, "image magic", img.u64(field("magic")), specHex64(t, c.Segment.Magic))
	specEq(t, "image state", img.u64(field("state")), c.Segment.States["ready"])
	specEq(t, "image major", img.u64(field("major")), c.Versions.SegmentLayoutMajor)
	specEq(t, "image minor", img.u64(field("minor")), c.Versions.SegmentLayoutMinor)
	specEq(t, "image pageSize", img.u64(field("pageSize")), uint64(pagesize))
	specEq(t, "image bufferCount", img.u64(field("bufferCount")), uint64(count))
	specEq(t, "image bufferSize", img.u64(field("bufferSize")), uint64(size))
	specEq(t, "image layoutSize", img.u64(field("layoutSize")), uint64(layout.total))
	specEq(t, "image epoch", img.u64(field("epoch")), 1)
	specEq(t, "image initToken", img.u64(field("initToken")), 0)
	specEq(t, "image requiredFeatures", img.u64(field("requiredFeatures")), 0)
	specEq(t, "image creatorToken (caller-owned memory)", img.u64(field("creatorToken")), 0)
	for off := uintptr(104); off < 128; off += 8 {
		specEq(t, "image reserved word", img.u64(off), 0)
	}
	if tok := img.u64(rec(0, "token")); !isOwnerToken(tok) || tok != p.token {
		t.Errorf("primary token = %#x, want the owner token %#x", tok, p.token)
	}
	specEq(t, "primary pid", img.u64(rec(0, "pid")), uint64(os.Getpid()))
	specEq(t, "primary interval (heartbeat disabled)", img.u64(rec(0, "interval")), 0)
	if img.u64(rec(0, "heartbeat")) == 0 {
		t.Error("primary heartbeat must be written at publish")
	}
	for _, name := range []string{"token", "pid", "heartbeat", "interval"} {
		specEq(t, "secondary "+name+" before attach", img.u64(rec(1, name)), 0)
	}
	// Every other word of the superblock page is zero.
	for off := c.Segment.SuperblockSize; off < pagesize; off += 8 {
		if img.u64(off) != 0 {
			t.Fatalf("word at superblock offset %d is not zero", off)
		}
	}

	// Ring headers.
	ringField := func(name string) uintptr {
		for _, f := range c.Ring.HeaderFields {
			if f.Name == name {
				return f.Offset
			}
		}
		t.Fatalf("no ring field %q in spec", name)
		return 0
	}
	for i, base := range []uintptr{layout.ring0, layout.ring1} {
		specEq(t, "ring magic", img.u64(base+ringField("magic")), specHex64(t, c.Ring.Magic))
		specEq(t, "ring size", img.u64(base+ringField("size")), uint64(count))
		specEq(t, "ring flag", img.u64(base+ringField("flag")), c.Ring.Flags.Init)
		specEq(t, "ring stride", img.u64(base+ringField("stride")), uint64(c.Ring.Slot.Stride))
		specEq(t, "ring version", img.u64(base+ringField("version")), c.Versions.RingLayout)
		specEq(t, "ring r", img.u64(base+ringField("r")), 0)
		specEq(t, "ring w", img.u64(base+ringField("w")), 0)
		for _, w := range []string{"notEmpty", "emptyWaiters", "notFull", "fullWaiters"} {
			specEq(t, "ring "+w, img.u32(base+ringField(w)), 0)
		}
		for s := range uintptr(count) {
			slot := base + c.Ring.HeaderSize + s*c.Ring.Slot.Stride
			specEq(t, "initial slot seq of ring "+strconv.Itoa(i), img.u64(slot+c.Ring.Slot.SeqOffset), uint64(s))
		}
	}

	// Secondary attaches: record filled, epoch unchanged.
	s, err := NewStandardLink(mem, count, size, opts)
	if err != nil {
		t.Fatal(err)
	}
	if tok := img.u64(rec(1, "token")); !isOwnerToken(tok) || tok != s.token {
		t.Errorf("secondary token = %#x, want %#x", tok, s.token)
	}
	specEq(t, "secondary pid", img.u64(rec(1, "pid")), uint64(os.Getpid()))
	specEq(t, "epoch after secondary attach", img.u64(field("epoch")), 1)

	// One message: slot 0 of ring 0 (primary -> secondary) goes through
	// seq 0 -> 1 (published) -> count (released: pos + mask + 1).
	if n, err := p.Write([]byte("hello")); n != 5 || err != nil {
		t.Fatalf("Write = %d, %v", n, err)
	}
	slot0 := layout.ring0 + c.Ring.HeaderSize
	specEq(t, "published seq", img.u64(slot0+c.Ring.Slot.SeqOffset), 1)
	specEq(t, "ring0.w after one write", img.u64(layout.ring0+ringField("w")), 1)
	want := protocol.NewStandardLinkCopyPacket(1, 0, 5)
	if got := mem[slot0+c.Ring.Slot.PayloadOffset : slot0+c.Ring.Slot.PayloadOffset+protocol.PacketSize]; string(got) != string(want[:]) {
		t.Errorf("published packet = %x, want %x", got, want[:])
	}
	if got := string(mem[layout.buf0 : layout.buf0+5]); got != "hello" {
		t.Errorf("payload in buffers0[0] = %q", got)
	}
	buf := make([]byte, 16)
	if n, err := s.Read(buf); n != 5 || err != nil || string(buf[:n]) != "hello" {
		t.Fatalf("Read = %d, %v, %q", n, err, buf[:n])
	}
	specEq(t, "released seq (pos+mask+1)", img.u64(slot0+c.Ring.Slot.SeqOffset), uint64(count))
	specEq(t, "ring0.r after one read", img.u64(layout.ring0+ringField("r")), 1)

	// Graceful close writes the detached token; the segment is then reusable.
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	specEq(t, "secondary token after Close", img.u64(rec(1, "token")), c.Segment.Tokens.Detached)
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	specEq(t, "primary token after Close", img.u64(rec(0, "token")), c.Segment.Tokens.Detached)
	p2, err := NewStandardLink(mem, count, size, opts)
	if err != nil {
		t.Fatal(err)
	}
	defer p2.Close()
	specEq(t, "epoch after re-initialization", img.u64(field("epoch")), 2)
	specEq(t, "reused ring0.w", img.u64(layout.ring0+ringField("w")), 0)
	specEq(t, "reused slot seq", img.u64(slot0+c.Ring.Slot.SeqOffset), 0)
	specEq(t, "reused secondary token", img.u64(rec(1, "token")), 0)
}

func TestSpecPacketVectors(t *testing.T) {
	c := loadSpecConsts(t)
	build := map[string]protocol.Packet{
		"standard_copy":      protocol.NewStandardLinkCopyPacket(1, 2, 3),
		"standard_tombstone": protocol.NewStandardLinkTombstonePacket(1, 2, 6),
		"error":              protocol.NewPacket(protocol.OpError, 9, 5),
		"conn_create":        protocol.NewPacket(protocol.OpConnCreate, 2),
		"conn_accept":        protocol.NewPacket(protocol.OpConnAccept, 2),
		"conn_close":         protocol.NewPacket(protocol.OpConnClose, 2, 0),
		"conn_copy":          protocol.NewConnCopyPacket(1, 2, 3, 4, 5, AdvFlagBegin|AdvFlagEnd, 7),
		"proto_negotiate":    protocol.NewPacket(protocol.OpProtoNegotiate, baselineVersionWord(c), 3, 0),
		"proto_ack":          protocol.NewPacket(protocol.OpProtoAck, baselineVersionWord(c), 3, 0),
	}
	for name, v := range c.PacketVectors {
		p, ok := build[name]
		if !ok {
			t.Errorf("packet vector %q unknown to the test", name)
			continue
		}
		if got := specHexString(p[:]); got != v.Hex {
			t.Errorf("%s = %s, spec vector %s", name, got, v.Hex)
		}
		specEq(t, name+" opcode", uint64(p.Op()), v.Opcode)
		for i, want := range v.Operands {
			specEq(t, name+" operand", p.Operand(i), want)
		}
	}
}

func baselineVersionWord(c *specConsts) uint64 { return c.Versions.ProtocolEncoded }

func specHexString(b []byte) string {
	const digits = "0123456789abcdef"
	out := make([]byte, 0, 2*len(b))
	for _, x := range b {
		out = append(out, digits[x>>4], digits[x&15])
	}
	return string(out)
}

func TestSpecAdvancedIdentifiers(t *testing.T) {
	c := loadSpecConsts(t)
	const count, size = 4, 64
	mem := AlignedBuffer(SizeStandardLink(count, size))
	o := AdvancedOptions{Link: LinkOptions{HeartbeatInterval: -1}}
	p, err := NewAdvancedLink(mem, count, size, o)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	s, err := NewAdvancedLink(mem, count, size, o)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	specEq(t, "primary mode", p.Mode(), LinkModePrimary)
	specEq(t, "secondary mode", s.Mode(), LinkModeSecondary)

	step := c.Advanced.StreamIDStep
	for i := range uint64(4) {
		specEq(t, "primary stream id", p.nextConnectionID(), c.Advanced.PrimaryFirstStreamID+i*step)
		specEq(t, "secondary stream id", s.nextConnectionID(), c.Advanced.SecondaryFirstStream+i*step)
	}
	for _, id := range []uint64{1, 3, 5, 999} {
		specEq(t, "primary: peer owns odd id", p.peerOwnsConnectionID(id), true)
		specEq(t, "secondary: peer owns odd id", s.peerOwnsConnectionID(id), false)
	}
	for _, id := range []uint64{2, 4, 1000} {
		specEq(t, "primary: peer owns even id", p.peerOwnsConnectionID(id), false)
		specEq(t, "secondary: peer owns even id", s.peerOwnsConnectionID(id), true)
	}
	specEq(t, "primary: stream 0 is nobody's", p.peerOwnsConnectionID(0), false)
	specEq(t, "secondary: stream 0 is nobody's", s.peerOwnsConnectionID(0), false)
	specEq(t, "first message id", p.nextMessageID(), c.Advanced.FirstMessageID)
	specEq(t, "second message id", p.nextMessageID(), c.Advanced.FirstMessageID+1)
	specEq(t, "default maxMessageSize", p.maxMessageSize, c.Advanced.DefaultMaxMessageSize)
	specEq(t, "default maxReassemblyBytes", p.maxReassemblyBytes, c.Advanced.DefaultMaxMessageSize*c.Advanced.ReassemblyFactor)
	specEq(t, "default queue size", p.queueSize, count)
}

// specSuperblock returns a zeroed superblock page for table-driven claim tests.
func specSuperblock() (*standardSuperblock, []byte) {
	mem := AlignedBuffer(pagesize)
	return (*standardSuperblock)(unsafe.Pointer(&mem[0])), mem
}

func specSetReady(sb *standardSuperblock, primaryToken, secondaryToken uint64, count, size int) {
	st := func(p *uint64, v uint64) { atomic.StoreUint64(p, v) }
	st(&sb.magic, superblockMagic)
	st(&sb.state, superblockStateReady)
	st(&sb.major, superblockMajor)
	st(&sb.minor, superblockMinor)
	st(&sb.pageSize, uint64(pagesize))
	st(&sb.bufferCount, uint64(count))
	st(&sb.bufferSize, uint64(size))
	st(&sb.layoutSize, uint64(SizeStandardLink(count, size)))
	st(&sb.epoch, 1)
	st(&sb.endpoints[endpointPrimary].token, primaryToken)
	st(&sb.endpoints[endpointSecondary].token, secondaryToken)
}

// TestSpecClaimTable pins the decisions of the claim loop (SPEC.md, "Claim
// loop"): every row is one row of the specification's table.
func TestSpecClaimTable(t *testing.T) {
	const count, size = 4, 64
	layoutSize := SizeStandardLink(count, size)
	quick := func(role attachRole) LinkOptions {
		o := LinkOptions{AttachTimeout: 20 * time.Millisecond, role: role, creatorToken: 77, initStale: 30 * time.Millisecond}
		return o.normalized()
	}
	claim := func(sb *standardSuperblock, role attachRole) (LinkMode, bool, error) {
		return claimSuperblock(sb, count, size, layoutSize, 0xabc, quick(role))
	}

	t.Run("empty page that is not zero is foreign for every role", func(t *testing.T) {
		for _, role := range []attachRole{roleAny, roleCreator, roleOpener} {
			sb, mem := specSuperblock()
			atomic.StoreUint64(&sb.endpoints[1].pid, 5) // any non-zero word of the page
			if _, _, err := claim(sb, role); !errors.Is(err, ErrIncompatibleLayout) {
				t.Errorf("role %d: err = %v, want ErrIncompatibleLayout", role, err)
			}
			if atomic.LoadUint64(&sb.state) != superblockStateEmpty || atomic.LoadUint64(&sb.initToken) != 0 {
				t.Errorf("role %d: a foreign page was modified", role)
			}
			_ = mem
		}
	})
	t.Run("non-zero word beyond the superblock struct still counts", func(t *testing.T) {
		sb, mem := specSuperblock()
		mem[pagesize-1] = 1
		if _, _, err := claim(sb, roleCreator); !errors.Is(err, ErrIncompatibleLayout) {
			t.Errorf("err = %v, want ErrIncompatibleLayout", err)
		}
	})
	t.Run("unknown state", func(t *testing.T) {
		sb, _ := specSuperblock()
		atomic.StoreUint64(&sb.state, 7)
		if _, _, err := claim(sb, roleCreator); !errors.Is(err, ErrIncompatibleLayout) {
			t.Errorf("err = %v, want ErrIncompatibleLayout", err)
		}
	})
	t.Run("empty zero page: creator and caller-owned claim, opener waits", func(t *testing.T) {
		for _, role := range []attachRole{roleAny, roleCreator} {
			sb, _ := specSuperblock()
			mode, mustInit, err := claim(sb, role)
			if err != nil || mode != LinkModePrimary || !mustInit {
				t.Fatalf("role %d: %v %v %v", role, mode, mustInit, err)
			}
			specEq(t, "state after claim", atomic.LoadUint64(&sb.state), superblockStateInitializing)
			specEq(t, "initToken after claim", atomic.LoadUint64(&sb.initToken), 0xabc)
			specEq(t, "major after claim", atomic.LoadUint64(&sb.major), superblockMajor)
			specEq(t, "creatorToken after claim", atomic.LoadUint64(&sb.creatorToken), 77)
			if atomic.LoadUint64(&sb.initHeartbeat) == 0 {
				t.Error("initHeartbeat must be set by the claim")
			}
			specEq(t, "magic is only written at publish", atomic.LoadUint64(&sb.magic), 0)
		}
		sb, _ := specSuperblock()
		if _, _, err := claim(sb, roleOpener); !errors.Is(err, errSegmentNotReady) {
			t.Errorf("opener on empty: err = %v, want errSegmentNotReady", err)
		}
		specEq(t, "opener leaves an empty segment untouched", atomic.LoadUint64(&sb.state), superblockStateEmpty)
	})
	t.Run("initializing: conflicting layouts are rejected", func(t *testing.T) {
		for name, mod := range map[string]func(sb *standardSuperblock){
			"foreign magic":          func(sb *standardSuperblock) { atomic.StoreUint64(&sb.magic, 0x1234) },
			"other major":            func(sb *standardSuperblock) { atomic.StoreUint64(&sb.major, 2) },
			"major 0 with initToken": func(sb *standardSuperblock) { atomic.StoreUint64(&sb.initToken, 9) },
			"unknown feature":        func(sb *standardSuperblock) { atomic.StoreUint64(&sb.requiredFeatures, 1) },
		} {
			sb, _ := specSuperblock()
			atomic.StoreUint64(&sb.state, superblockStateInitializing)
			mod(sb)
			if _, _, err := claim(sb, roleCreator); !errors.Is(err, ErrIncompatibleLayout) {
				t.Errorf("%s: err = %v, want ErrIncompatibleLayout", name, err)
			}
		}
	})
	t.Run("initializing: opener never takes over, creator takes over a stalled initializer", func(t *testing.T) {
		mk := func() *standardSuperblock {
			sb, _ := specSuperblock()
			atomic.StoreUint64(&sb.state, superblockStateInitializing)
			atomic.StoreUint64(&sb.major, superblockMajor)
			atomic.StoreUint64(&sb.initToken, 0x5555)
			atomic.StoreUint64(&sb.initHeartbeat, uint64(time.Now().UnixNano()))
			return sb
		}
		sb := mk()
		if _, _, err := claim(sb, roleOpener); !errors.Is(err, errInitContended) {
			t.Errorf("opener: err = %v, want errInitContended", err)
		}
		specEq(t, "opener leaves initToken", atomic.LoadUint64(&sb.initToken), 0x5555)
		sb = mk()
		mode, mustInit, err := claim(sb, roleCreator)
		if err != nil || mode != LinkModePrimary || !mustInit {
			t.Fatalf("creator takeover: %v %v %v", mode, mustInit, err)
		}
		specEq(t, "initToken after takeover", atomic.LoadUint64(&sb.initToken), 0xabc)
		specEq(t, "creatorToken after takeover", atomic.LoadUint64(&sb.creatorToken), 77)
	})
	t.Run("ready: incompatible layouts", func(t *testing.T) {
		for name, mod := range map[string]func(sb *standardSuperblock){
			"foreign magic":   func(sb *standardSuperblock) { atomic.StoreUint64(&sb.magic, 0x4851515f53544c32) }, // "HQQ_STL2"
			"other major":     func(sb *standardSuperblock) { atomic.StoreUint64(&sb.major, 4) },
			"unknown feature": func(sb *standardSuperblock) { atomic.StoreUint64(&sb.requiredFeatures, 1<<5) },
		} {
			sb, _ := specSuperblock()
			specSetReady(sb, 0x11, 0, count, size)
			mod(sb)
			if _, _, err := claim(sb, roleAny); !errors.Is(err, ErrIncompatibleLayout) {
				t.Errorf("%s: err = %v, want ErrIncompatibleLayout", name, err)
			}
		}
	})
	t.Run("ready: live primary", func(t *testing.T) {
		sb, _ := specSuperblock()
		specSetReady(sb, 0x11, 0, count, size) // interval 0: always alive
		if _, _, err := claim(sb, roleCreator); !errors.Is(err, ErrLinkBusy) {
			t.Errorf("creator: err = %v, want ErrLinkBusy", err)
		}
		specEq(t, "creator leaves the secondary record", atomic.LoadUint64(&sb.endpoints[1].token), 0)
		sb2, _ := specSuperblock()
		specSetReady(sb2, 0x11, 0, count, size)
		atomic.StoreUint64(&sb2.bufferSize, size*2)
		if _, _, err := claim(sb2, roleAny); !errors.As(err, new(*GeometryError)) || !errors.Is(err, ErrInvalidSize) {
			t.Errorf("geometry mismatch: err = %v, want *GeometryError", err)
		}
		mode, mustInit, err := claim(sb, roleAny)
		if err != nil || mode != LinkModeSecondary || mustInit {
			t.Fatalf("join: %v %v %v", mode, mustInit, err)
		}
		specEq(t, "secondary token", atomic.LoadUint64(&sb.endpoints[1].token), 0xabc)
		specEq(t, "secondary pid", atomic.LoadUint64(&sb.endpoints[1].pid), uint64(os.Getpid()))
		if _, _, err := claim(sb, roleAny); !errors.Is(err, ErrLinkBusy) {
			t.Errorf("third endpoint: err = %v, want ErrLinkBusy", err)
		}
		// The live primary's earlier peer already left (detached): no second peer.
		sb3, _ := specSuperblock()
		specSetReady(sb3, 0x11, detachedToken, count, size)
		if _, _, err := claim(sb3, roleAny); !errors.Is(err, ErrLinkBusy) {
			t.Errorf("after the peer closed: err = %v, want ErrLinkBusy", err)
		}
	})
	t.Run("ready: no live endpoint", func(t *testing.T) {
		sb, _ := specSuperblock()
		specSetReady(sb, detachedToken, 0, count, size)
		if _, _, err := claim(sb, roleOpener); !errors.Is(err, errNoLiveEndpoint) {
			t.Errorf("opener: err = %v, want errNoLiveEndpoint", err)
		}
		specEq(t, "opener leaves state", atomic.LoadUint64(&sb.state), superblockStateReady)
		mode, mustInit, err := claim(sb, roleCreator)
		if err != nil || mode != LinkModePrimary || !mustInit {
			t.Fatalf("reuse: %v %v %v", mode, mustInit, err)
		}
		specEq(t, "state during reuse", atomic.LoadUint64(&sb.state), superblockStateInitializing)
		specEq(t, "epoch is bumped only at publish", atomic.LoadUint64(&sb.epoch), 1)
		specEq(t, "creatorToken replaced", atomic.LoadUint64(&sb.creatorToken), 77)
	})
	t.Run("publish", func(t *testing.T) {
		sb, _ := specSuperblock()
		if _, _, err := claim(sb, roleCreator); err != nil {
			t.Fatal(err)
		}
		opts := quick(roleCreator)
		epoch, err := publishSuperblock(sb, count, size, layoutSize, 0xabc, opts)
		if err != nil || epoch != 1 {
			t.Fatalf("publish = %d, %v", epoch, err)
		}
		specEq(t, "magic", atomic.LoadUint64(&sb.magic), superblockMagic)
		specEq(t, "state", atomic.LoadUint64(&sb.state), superblockStateReady)
		specEq(t, "initToken released", atomic.LoadUint64(&sb.initToken), 0)
		specEq(t, "primary token", atomic.LoadUint64(&sb.endpoints[0].token), 0xabc)
		specEq(t, "primary interval", atomic.LoadUint64(&sb.endpoints[0].interval), uint64(opts.HeartbeatInterval))
		// A lost initialization is not published.
		sb2, _ := specSuperblock()
		if _, _, err := claim(sb2, roleCreator); err != nil {
			t.Fatal(err)
		}
		atomic.StoreUint64(&sb2.initToken, 0xdead)
		if _, err := publishSuperblock(sb2, count, size, layoutSize, 0xabc, opts); !errors.Is(err, errInitContended) {
			t.Errorf("publish after losing initToken: err = %v, want errInitContended", err)
		}
		specEq(t, "state stays Initializing", atomic.LoadUint64(&sb2.state), superblockStateInitializing)
		specEq(t, "magic stays unset", atomic.LoadUint64(&sb2.magic), 0)
	})
}

// TestSpecLiveness pins the endpoint liveness rules used when opening a
// segment (SPEC.md, "Liveness") and the peer-state derivation.
func TestSpecLiveness(t *testing.T) {
	opts := LinkOptions{PeerTimeout: 50 * time.Millisecond}.normalized()
	ctx := t.Context()
	alive := func(rec endpointRecord) bool {
		got, err := endpointAliveAtOpen(ctx, &rec, opts)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	now := uint64(time.Now().UnixNano())
	specEq(t, "unused record", alive(endpointRecord{}), false)
	specEq(t, "detached record", alive(endpointRecord{token: detachedToken, interval: 1, heartbeat: now}), false)
	specEq(t, "heartbeats disabled", alive(endpointRecord{token: 9}), true)
	specEq(t, "no heartbeat yet", alive(endpointRecord{token: 9, interval: uint64(time.Millisecond)}), true)
	specEq(t, "fresh heartbeat", alive(endpointRecord{token: 9, interval: uint64(time.Millisecond), heartbeat: now}), true)
	specEq(t, "stale heartbeat without progress", alive(endpointRecord{token: 9, interval: uint64(time.Millisecond), heartbeat: 1}), false)
	// A heartbeat younger than max(PeerTimeout, 4*interval) is alive even when
	// the owner's own interval is larger than the opener's timeout.
	slow := endpointRecord{token: 9, interval: uint64(time.Second), heartbeat: uint64(time.Now().Add(-500 * time.Millisecond).UnixNano())}
	specEq(t, "age below 4*interval", alive(slow), true)
}

func TestSpecPeerStateDerivation(t *testing.T) {
	const count, size = 4, 64
	mem := AlignedBuffer(SizeStandardLink(count, size))
	sb := (*standardSuperblock)(unsafe.Pointer(&mem[0]))
	opts := LinkOptions{HeartbeatInterval: -1, PeerTimeout: 10 * time.Millisecond}
	p, err := NewStandardLink(mem, count, size, opts)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	if err := p.checkPeer(); err != nil || p.PeerState() != PeerNotConnected {
		t.Fatalf("lonely primary: %v, %v", err, p.PeerState())
	}
	s, err := NewStandardLink(mem, count, size, opts)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if s.PeerState() != PeerConnected {
		t.Errorf("the secondary monitors the primary from the start: %v", s.PeerState())
	}
	if err := p.checkPeer(); err != nil || p.PeerState() != PeerConnected {
		t.Fatalf("after attach: %v, %v", err, p.PeerState())
	}
	// A heartbeat that stops advancing for more than effectivePeerTimeout is "gone".
	rec := &sb.endpoints[endpointSecondary]
	atomic.StoreUint64(&rec.interval, uint64(time.Millisecond))
	atomic.StoreUint64(&rec.heartbeat, 5)
	if err := p.checkPeer(); err != nil {
		t.Fatalf("first look at the new heartbeat: %v", err)
	}
	time.Sleep(25 * time.Millisecond)
	if err := p.checkPeer(); !errors.Is(err, ErrPeerGone) || p.PeerState() != PeerGone {
		t.Fatalf("stalled heartbeat: %v, %v", err, p.PeerState())
	}
	// Closed, replaced token and re-initialized epoch.
	mem2 := AlignedBuffer(SizeStandardLink(count, size))
	sb2 := (*standardSuperblock)(unsafe.Pointer(&mem2[0]))
	p2, _ := NewStandardLink(mem2, count, size, opts)
	s2, _ := NewStandardLink(mem2, count, size, opts)
	defer p2.Close()
	_ = p2.checkPeer()
	_ = s2.Close()
	if err := p2.checkPeer(); !errors.Is(err, ErrPeerClosed) || p2.PeerState() != PeerClosed {
		t.Fatalf("closed peer: %v, %v", err, p2.PeerState())
	}
	mem3 := AlignedBuffer(SizeStandardLink(count, size))
	sb3 := (*standardSuperblock)(unsafe.Pointer(&mem3[0]))
	p3, _ := NewStandardLink(mem3, count, size, opts)
	s3, _ := NewStandardLink(mem3, count, size, opts)
	defer p3.Close()
	defer s3.Close()
	_ = p3.checkPeer()
	atomic.StoreUint64(&sb3.endpoints[endpointSecondary].token, 0x4242)
	if err := p3.checkPeer(); !errors.Is(err, ErrPeerGone) {
		t.Fatalf("replaced peer token: %v", err)
	}
	mem4 := AlignedBuffer(SizeStandardLink(count, size))
	sb4 := (*standardSuperblock)(unsafe.Pointer(&mem4[0]))
	p4, _ := NewStandardLink(mem4, count, size, opts)
	defer p4.Close()
	atomic.AddUint64(&sb4.epoch, 1)
	if err := p4.checkPeer(); !errors.Is(err, ErrPeerGone) {
		t.Fatalf("epoch change: %v", err)
	}
	_, _ = sb2, sb4
}
