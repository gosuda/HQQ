package conformance

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	"gosuda.org/hqq/internal/protocol"
)

// Advanced frame flag bits (PROTOCOL_1_1_DRAFT section 3.1, SPEC 10.4).
const (
	flagBegin    = 1 << 0
	flagEnd      = 1 << 1
	flagAbort    = 1 << 2
	flagRequest  = 1 << 3
	flagResponse = 1 << 4
	flagError    = 1 << 5
	flagMeta     = 1 << 6
)

// Protocol 1.1 negotiation words (PROTOCOL_1_1_DRAFT section 1).
const (
	protoVersion11 = 0x0101
	featuresAll11  = 0x0f
)

// packetVectors returns the packet byte vectors: the nine vectors of
// constants.json packet_vectors, then the extra frames of protocol 1.1.
func packetVectors() []PacketVector {
	type spec struct {
		name, source string
		op           protocol.OpCode
		operands     []uint64
	}
	specs := []spec{
		{"standard_copy", "constants.json", protocol.OpStandardLinkCopy, []uint64{1, 2, 3}},
		{"standard_tombstone", "constants.json", protocol.OpStandardLinkTombstone, []uint64{1, 2, 6}},
		{"error", "constants.json", protocol.OpError, []uint64{9, 5}},
		{"conn_create", "constants.json", protocol.OpConnCreate, []uint64{2}},
		{"conn_accept", "constants.json", protocol.OpConnAccept, []uint64{2}},
		{"conn_close", "constants.json", protocol.OpConnClose, []uint64{2, 0}},
		{"conn_copy", "constants.json", protocol.OpConnCopy, []uint64{1, 2, 3, 4, 5, 3, 7}},
		{"proto_negotiate", "constants.json", protocol.OpProtoNegotiate, []uint64{256, 3, 0}},
		{"proto_ack", "constants.json", protocol.OpProtoAck, []uint64{256, 3, 0}},
		// Extra frames.
		{"conn_copy_begin_end_request_meta", "extra", protocol.OpConnCopy,
			[]uint64{0, 1, 45, 0, 45, flagBegin | flagEnd | flagRequest | flagMeta, 9}},
		{"conn_copy_begin_end", "extra", protocol.OpConnCopy, []uint64{0, 1, 5, 0, 5, flagBegin | flagEnd, 0}},
		{"conn_copy_abort", "extra", protocol.OpConnCopy, []uint64{0, 1, 0, 0, 0, flagAbort, 0}},
		{"conn_copy_response_error", "extra", protocol.OpConnCopy,
			[]uint64{0, 1, 4, 0, 4, flagBegin | flagEnd | flagResponse | flagError, 42}},
		{"proto_negotiate_1_1_all", "extra", protocol.OpProtoNegotiate, []uint64{protoVersion11, featuresAll11, 0}},
		{"proto_ack_1_1_all", "extra", protocol.OpProtoAck, []uint64{protoVersion11, featuresAll11, 0}},
		{"cancel_request_1", "extra", protocol.OpCancel, []uint64{0, 1}},
	}
	out := make([]PacketVector, 0, len(specs))
	for _, s := range specs {
		p := protocol.NewPacket(s.op, s.operands...)
		out = append(out, PacketVector{
			Name:     s.name,
			Source:   s.source,
			Opcode:   uint8(s.op),
			Operands: append([]uint64(nil), s.operands...),
			Hex:      hex.EncodeToString(p[:]),
		})
	}
	return out
}

// layoutVectors returns the region-offset vectors: the twelve geometries of
// constants.json segment.layout_vectors and extra geometries.
func layoutVectors() []LayoutVector {
	type geo struct {
		source          string
		page, cnt, size int
	}
	var geos []geo
	for _, page := range []int{4096, 16384, 65536} {
		for _, g := range [][2]int{{2, 8}, {256, 4096}, {1024, 65536}, {16, 800}} {
			geos = append(geos, geo{"constants.json", page, g[0], g[1]})
		}
	}
	geos = append(geos,
		geo{"extra", 4096, 4, 64},
		geo{"extra", 16384, 4, 64},
		geo{"extra", 65536, 4, 64},
		geo{"extra", 16384, 2, 4096},
	)
	out := make([]LayoutVector, 0, len(geos))
	for _, g := range geos {
		r, ok := ComputeRegions(g.page, g.cnt, g.size)
		if !ok {
			panic(fmt.Sprintf("conformance: layout vector %+v is outside the limits", g))
		}
		out = append(out, LayoutVector{
			Source:      g.source,
			PageSize:    g.page,
			BufferCount: g.cnt,
			BufferSize:  g.size,
			Ring0:       r.Ring0,
			Buffers0:    r.Buf0,
			Ring1:       r.Ring1,
			Buffers1:    r.Buf1,
			Total:       r.Total,
		})
	}
	return out
}

// geometryVectors returns valid and invalid configurations (SPEC 2.2).
func geometryVectors() []GeometryVector {
	type geo struct {
		cnt, size int
		reason    string
	}
	invalid := []geo{
		{0, 8, "bufferCount < 2"},
		{1, 8, "bufferCount < 2"},
		{3, 8, "bufferCount not a power of two"},
		{1 << 25, 8, "bufferCount > 2^24"},
		{2, 0, "bufferSize < 8"},
		{2, 4, "bufferSize < 8"},
		{2, 12, "bufferSize not a multiple of 8"},
		{2, 1<<30 + 8, "bufferSize > 2^30"},
		{2, 1<<30 + 1, "bufferSize not a multiple of 8"},
	}
	valid := []geo{
		{2, 8, ""},
		{1 << 24, 8, ""},
		{2, 1 << 30, ""},
	}
	out := make([]GeometryVector, 0, len(invalid)+len(valid))
	for _, g := range invalid {
		out = append(out, GeometryVector{BufferCount: g.cnt, BufferSize: g.size, Valid: false, Reason: g.reason})
	}
	for _, g := range valid {
		out = append(out, GeometryVector{BufferCount: g.cnt, BufferSize: g.size, Valid: true})
	}
	return out
}

// Name used by the darwin length vectors: 30 bytes (valid) and 31 bytes
// (invalid) without the leading slash, so 31 and 32 bytes with it.
const (
	darwinNameValid   = "hqq-conf-nnnnnnnnnnnnnnnnnnnnn"
	darwinNameInvalid = darwinNameValid + "n"
)

// nameVectors returns the name vectors of SPEC 11.1 and the platform tables.
func nameVectors() []NameVector {
	var out []NameVector
	for _, n := range []string{"", "/", ".", "/.", "..", "a/b", "a\\b", "a\x00b", "//"} {
		out = append(out, NameVector{Name: n, Platform: "any", Valid: false, Source: "constants.json"})
	}
	out = append(out,
		NameVector{Name: "abc", Platform: "any", Valid: true, Source: "extra"},
		NameVector{Name: "/abc", Platform: "any", Valid: true, Source: "extra"},
		NameVector{Name: darwinNameValid, Platform: "darwin", Valid: true, Source: "extra"},
		NameVector{Name: darwinNameInvalid, Platform: "darwin", Valid: false, Source: "extra"},
	)
	for _, v := range []struct{ name, object string }{
		{"abc", `Local\abc`},
		{`Global\abc`, `Global\abc`},
		{`global\abc`, `Global\abc`},
		{`LOCAL\abc`, `Local\abc`},
		{"/abc", `Local\abc`},
		{`Global\/abc`, `Global\abc`},
	} {
		out = append(out, NameVector{Name: v.name, Platform: "windows", Valid: true, Object: v.object, Source: "constants.json"})
	}
	for _, n := range []string{"", `Global\`, `Local\`, `a\b`, `Global\a\b`} {
		out = append(out, NameVector{Name: n, Platform: "windows", Valid: false, Source: "constants.json"})
	}
	return out
}

// errorCodeVectors returns the wire error codes and their canonical kinds
// (SPEC 12, PROTOCOL_1_1_DRAFT section 7, INTEROP_CLI section 4.1).
func errorCodeVectors() []ErrorCodeVector {
	return []ErrorCodeVector{
		{"MemoryAlign", 1, "ErrMemoryAlign", "constants.json"},
		{"InvalidSize", 2, "ErrInvalidSize", "constants.json"},
		{"MemorySmall", 3, "ErrMemorySmall", "constants.json"},
		{"FailedInit", 4, "ErrFailedInit", "constants.json"},
		{"ConnNotFound", 5, "ErrConnNotFound", "constants.json"},
		{"InvalidOp", 6, "ErrInvalidOp", "constants.json"},
		{"BufferOverflow", 7, "ErrBufferOverflow", "constants.json"},
		{"ShuttingDown", 8, "ErrShuttingDown", "protocol_1_1"},
		{"unknown", 0, "ErrProtocol", "extra"},
		{"unknown", 9, "ErrProtocol", "extra"},
		{"unknown", 0xffff, "ErrProtocol", "extra"},
	}
}

// payloadVectors returns the payload vectors of INTEROP_CLI section 5. The
// bytes come from conformance.Payload; the test checks them against the
// golden values of the document as well.
func payloadVectors() []PayloadVector {
	small := []struct {
		seed, seq uint64
		size      int
	}{
		{1, 0, 0}, {1, 0, 1}, {1, 0, 8}, {1, 0, 15}, {1, 0, 16},
		{0xdeadbeef, 7, 40}, {1, 3, 24},
	}
	out := make([]PayloadVector, 0, len(small)+2)
	for _, v := range small {
		out = append(out, PayloadVector{
			Seed: v.seed, Seq: v.seq, Size: v.size,
			Hex: hex.EncodeToString(Payload(v.seed, v.seq, v.size)),
		})
	}
	for _, size := range []int{1024, 65536} {
		sum := sha256.Sum256(Payload(1, 0, size))
		out = append(out, PayloadVector{
			Seed: 1, Seq: 0, Size: size,
			SHA256: hex.EncodeToString(sum[:]), Length: size,
		})
	}
	return out
}

// hexOf returns the lowercase hex of b.
func hexOf(b []byte) string { return hex.EncodeToString(b) }
