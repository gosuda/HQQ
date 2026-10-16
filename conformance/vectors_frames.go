package conformance

import (
	"encoding/hex"
	"fmt"

	"gosuda.org/hqq/internal/protocol"
)

// frameSpec is one ring-0 packet of a frame row before encoding.
type frameSpec struct {
	op      uint8
	ops     []uint64
	payload []byte
}

// packet encodes the frame (SPEC 9.1: unused operand words are zero).
func (f frameSpec) packet() protocol.Packet {
	return protocol.NewPacket(protocol.OpCode(f.op), f.ops...)
}

// opcodeNames maps the opcodes of this protocol to their names (SPEC 9.2,
// PROTOCOL_1_1_DRAFT section 2).
var opcodeNames = map[uint8]string{
	0x00: "OpError", 0x01: "OpConnCreate", 0x02: "OpConnAccept", 0x03: "OpConnClose",
	0x04: "OpConnCopy", 0x05: "OpProtoNegotiate", 0x06: "OpProtoAck", 0x07: "OpCancel",
	0xfe: "OpStandardLinkTombstone", 0xff: "OpStandardLinkCopy",
}

// opName returns the name of an opcode, or its hex value when unknown.
func opName(op uint8) string {
	if n, ok := opcodeNames[op]; ok {
		return n
	}
	return fmt.Sprintf("0x%02x", op)
}

// copyFrame is an OpConnCopy chunk (SPEC 10.4 operands).
func copyFrame(conn, msg, total, off, size, flags, aux uint64, payload []byte) frameSpec {
	return frameSpec{op: uint8(protocol.OpConnCopy), ops: []uint64{conn, msg, total, off, size, flags, aux}, payload: payload}
}

// negotiateFrame is OpProtoNegotiate with version 1.1 (PROTOCOL_1_1_DRAFT section 1).
func negotiateFrame(features uint64) frameSpec {
	return frameSpec{op: uint8(protocol.OpProtoNegotiate), ops: []uint64{protoVersion11, features, 0}}
}

// cancelFrame is OpCancel for request msg on stream conn (PROTOCOL_1_1_DRAFT section 2).
func cancelFrame(conn, msg uint64) frameSpec {
	return frameSpec{op: uint8(protocol.OpCancel), ops: []uint64{conn, msg}}
}

// mustHex decodes a hex literal of a row (rows are constants: a bad literal
// is a generator bug).
func mustHex(s string) []byte {
	b, err := hex.DecodeString(s)
	if err != nil {
		panic(fmt.Sprintf("conformance: bad hex literal %q: %v", s, err))
	}
	return b
}

// metaBlockExample is the request metadata block of PROTOCOL_1_1_DRAFT
// section 3.3: budget 5 s, caller 0x1122334455667788, trace 01 00 .. 00
// (42 bytes).
const metaBlockExample = "26000000" + "0108" + "00f2052a01000000" + "0208" + "8877665544332211" +
	"0310" + "01000000000000000000000000000000"

// Canonical error kinds used by fail rows (INTEROP_CLI section 4.1).
const (
	errProtocol     = "ErrProtocol"
	errInvalidSize  = "ErrInvalidSize"
	errBufOverflow  = "ErrBufferOverflow"
	errUnexpected   = "ErrUnexpectedPacket"
	errAborted      = "ErrMessageAborted"
	errReassembly   = "ErrReassemblyLimit"
	errResponse     = "ResponseError"
	errConnNotFound = "ErrConnNotFound"
)

// Flag combinations used by the rows (PROTOCOL_1_1_DRAFT section 3.1).
const (
	fBE   = flagBegin | flagEnd
	fBEq  = flagBegin | flagEnd | flagRequest
	fBEs  = flagBegin | flagEnd | flagResponse
	fBEqm = flagBegin | flagEnd | flagRequest | flagMeta
)

// frameRowSpec is a row of the matrix before the negotiate frame is added.
type frameRowSpec struct {
	id, title, target string
	features          uint64 // features offered in OpProtoNegotiate (0: 0x0f)
	maxReassembly     uint64
	frames            []frameSpec
	late              []frameSpec
	effect, failScope string
	deliver           []Delivery
	errKind           string
	sentinel          bool
}

// data is an expected Recv delivery.
func data(payload string) Delivery { return Delivery{Kind: "data", Payload: hexOf([]byte(payload))} }

// request is an expected Serve delivery.
func request(method uint64, caller uint64, trace string, payload string) Delivery {
	d := Delivery{Kind: "request", Payload: hexOf([]byte(payload)), Method: method, Caller: caller}
	if trace != "" {
		d.Trace = trace
	}
	return d
}

// response is an expected Call result.
func response(payload string) Delivery {
	return Delivery{Kind: "response", Payload: hexOf([]byte(payload))}
}

// frameRows returns the advanced frame matrix (G-10). Message IDs are chosen
// per row; the Call of target "call" is message 1 (the first message ID of
// the link), so response rows use msg 1.
func frameRows() []frameRowSpec {
	const (
		big      = 1<<26 + 1 // larger than MaxMessageSize (64 MiB)
		noFeat   = 0x0b      // messages, request/response, metadata; no cancel
		noMeta   = 0x07      // messages, request/response, cancel; no metadata
		reassCap = 100       // MaxReassemblyBytes that refuses a 8-byte message
	)
	fill := func(n int, c byte) []byte {
		b := make([]byte, n)
		for i := range b {
			b[i] = c
		}
		return b
	}
	block := mustHex(metaBlockExample)
	withBlock := func(user string) []byte { return append(append([]byte(nil), block...), user...) }
	serveSentinel := true
	return []frameRowSpec{
		{id: "G10-01", title: "single chunk data message", target: "recv",
			frames: []frameSpec{copyFrame(0, 1, 5, 0, 5, fBE, 0, []byte("hello"))},
			effect: "deliver", deliver: []Delivery{data("hello")}, sentinel: true},
		{id: "G10-02", title: "zero-length message (TotalSize 0, Begin|End only)", target: "recv",
			frames: []frameSpec{copyFrame(0, 1, 0, 0, 0, fBE, 0, nil)},
			effect: "deliver", deliver: []Delivery{data("")}, sentinel: true},
		{id: "G10-03", title: "two chunks (SPEC 10.4 chunking)", target: "recv",
			frames: []frameSpec{
				copyFrame(0, 2, 12, 0, 8, flagBegin, 0, []byte("abcdefgh")),
				copyFrame(0, 2, 12, 8, 4, flagEnd, 0, []byte("ijkl")),
			},
			effect: "deliver", deliver: []Delivery{data("abcdefghijkl")}, sentinel: true},
		{id: "G10-04", title: "three chunks, middle chunk without flags", target: "recv",
			frames: []frameSpec{
				copyFrame(0, 3, 10, 0, 4, flagBegin, 0, []byte("0123")),
				copyFrame(0, 3, 10, 4, 4, 0, 0, []byte("4567")),
				copyFrame(0, 3, 10, 8, 2, flagEnd, 0, []byte("89")),
			},
			effect: "deliver", deliver: []Delivery{data("0123456789")}, sentinel: true},
		{id: "G10-05", title: "abort after the first chunk: partial data dropped silently", target: "recv",
			frames: []frameSpec{
				copyFrame(0, 4, 8, 0, 4, flagBegin, 0, []byte("abcd")),
				copyFrame(0, 4, 0, 0, 0, flagAbort, 0, nil),
			},
			effect: "drop", sentinel: true},
		{id: "G10-06", title: "abort frame accepted whatever the other operands are (SPEC 10.5 step 2)", target: "recv",
			frames: []frameSpec{copyFrame(0, 5, 99, 7, 3, flagAbort|fBE, 3, nil)},
			effect: "drop", sentinel: true},
		{id: "G10-07", title: "abort of a response: the waiting call fails, the path survives", target: "call",
			frames: []frameSpec{
				copyFrame(0, 1, 8, 0, 4, flagBegin|flagResponse, 0, []byte("abcd")),
				copyFrame(0, 1, 0, 0, 0, flagAbort|flagResponse, 0, nil),
			},
			effect: "fail", failScope: "call", errKind: errAborted, sentinel: true},
		{id: "G10-08", title: "request with metadata block in one chunk (budget, caller, trace; SPEC PROTOCOL_1_1 3.3)", target: "serve",
			frames:   []frameSpec{copyFrame(0, 1, 45, 0, 45, fBEqm, 9, withBlock("abc"))},
			effect:   "deliver",
			deliver:  []Delivery{request(9, 0x1122334455667788, "01000000000000000000000000000000", "abc")},
			sentinel: serveSentinel},
		{id: "G10-09", title: "request with metadata block split over two chunks", target: "serve",
			frames: []frameSpec{
				copyFrame(0, 1, 45, 0, 32, flagBegin|flagRequest|flagMeta, 9, withBlock("abc")[:32]),
				copyFrame(0, 1, 45, 32, 13, flagEnd|flagRequest|flagMeta, 0, withBlock("abc")[32:]),
			},
			effect:   "deliver",
			deliver:  []Delivery{request(9, 0x1122334455667788, "01000000000000000000000000000000", "abc")},
			sentinel: serveSentinel},
		{id: "G10-10", title: "metadata flag on a data message (only requests may carry it)", target: "recv",
			frames: []frameSpec{copyFrame(0, 1, 5, 0, 5, fBE|flagMeta, 0, []byte("hello"))},
			effect: "fail", failScope: "path", errKind: errProtocol},
		{id: "G10-11", title: "metadata on a request while FeatureMetadata is not negotiated", target: "serve", features: noMeta,
			frames: []frameSpec{copyFrame(0, 1, 45, 0, 45, fBEqm, 9, withBlock("abc"))},
			effect: "fail", failScope: "path", errKind: errProtocol},
		{id: "G10-12", title: "metadata block longer than the message (tlvLen 32, 14 bytes)", target: "serve",
			frames: []frameSpec{copyFrame(0, 1, 14, 0, 14, fBEqm, 9, mustHex("20000000"+"010800f2052a01000000"))},
			effect: "fail", failScope: "path", errKind: errProtocol},
		{id: "G10-13", title: "known metadata tag with the wrong length (budget of 4 bytes)", target: "serve",
			frames: []frameSpec{copyFrame(0, 1, 10, 0, 10, fBEqm, 9, mustHex("06000000"+"0104"+"01020304"))},
			effect: "fail", failScope: "path", errKind: errProtocol},
		{id: "G10-14", title: "metadata TLV overruns tlvLen (budget tag of 8 bytes in 3 bytes)", target: "serve",
			frames: []frameSpec{copyFrame(0, 1, 7, 0, 7, fBEqm, 9, mustHex("03000000"+"010800"))},
			effect: "fail", failScope: "path", errKind: errProtocol},
		{id: "G10-15", title: "unknown metadata tag is skipped (caller ID after it, user payload ok)", target: "serve",
			frames: []frameSpec{copyFrame(0, 1, 20, 0, 20, fBEqm, 9,
				mustHex("0e000000"+"7f02aabb"+"0208"+"8877665544332211"+hexOf([]byte("ok"))))},
			effect: "deliver", deliver: []Delivery{request(9, 0x1122334455667788, "", "ok")},
			sentinel: serveSentinel},
		{id: "G10-16", title: "metadata block shorter than its 4-byte header", target: "serve",
			frames: []frameSpec{copyFrame(0, 1, 2, 0, 2, fBEqm, 9, []byte("ab"))},
			effect: "fail", failScope: "path", errKind: errProtocol},
		{id: "G10-17", title: "flag bit above the known set (0x80)", target: "recv",
			frames: []frameSpec{copyFrame(0, 1, 2, 0, 2, fBE|0x80, 0, []byte("hi"))},
			effect: "fail", failScope: "path", errKind: errProtocol},
		{id: "G10-18", title: "Request and Response together (SPEC 10.5 step 1)", target: "recv",
			frames: []frameSpec{copyFrame(0, 1, 2, 0, 2, fBE|flagRequest|flagResponse, 0, []byte("hi"))},
			effect: "fail", failScope: "path", errKind: errProtocol},
		{id: "G10-19", title: "Error without Response (SPEC 10.5 step 1)", target: "recv",
			frames: []frameSpec{copyFrame(0, 1, 2, 0, 2, fBE|flagError, 0, []byte("hi"))},
			effect: "fail", failScope: "path", errKind: errProtocol},
		{id: "G10-20", title: "response success: delivered to the waiting call", target: "call",
			frames: []frameSpec{copyFrame(0, 1, 4, 0, 4, fBEs, 0, []byte("pong"))},
			effect: "deliver", deliver: []Delivery{response("pong")}, sentinel: true},
		{id: "G10-21", title: "error response: the call fails with ResponseError{Status 42, Payload}", target: "call",
			frames: []frameSpec{copyFrame(0, 1, 4, 0, 4, fBEs|flagError, 42, []byte("boom"))},
			effect: "fail", failScope: "call", errKind: errResponse, sentinel: true},
		{id: "G10-22", title: "TotalSize 0 with ChunkOffset != 0 (SPEC 10.5 step 3)", target: "recv",
			frames: []frameSpec{copyFrame(0, 6, 0, 1, 0, fBE, 0, nil)},
			effect: "fail", failScope: "path", errKind: errInvalidSize},
		{id: "G10-23", title: "TotalSize 0 without Begin|End", target: "recv",
			frames: []frameSpec{copyFrame(0, 6, 0, 0, 0, flagBegin, 0, nil)},
			effect: "fail", failScope: "path", errKind: errInvalidSize},
		{id: "G10-24", title: "TotalSize > 0 with ChunkSize 0", target: "recv",
			frames: []frameSpec{copyFrame(0, 6, 4, 0, 0, flagBegin, 0, nil)},
			effect: "fail", failScope: "path", errKind: errInvalidSize},
		{id: "G10-25", title: "ChunkSize larger than the buffer (64 bytes)", target: "recv",
			frames: []frameSpec{copyFrame(0, 6, 65, 0, 65, fBE, 0, fill(64, 'x'))},
			effect: "fail", failScope: "path", errKind: errInvalidSize},
		{id: "G10-26", title: "ChunkOffset beyond TotalSize", target: "recv",
			frames: []frameSpec{copyFrame(0, 6, 4, 6, 1, flagBegin, 0, []byte("x"))},
			effect: "fail", failScope: "path", errKind: errInvalidSize},
		{id: "G10-27", title: "TotalSize above MaxMessageSize (64 MiB)", target: "recv",
			frames: []frameSpec{copyFrame(0, 6, big, 0, 8, flagBegin, 0, []byte("xxxxxxxx"))},
			effect: "fail", failScope: "path", errKind: errBufOverflow},
		{id: "G10-28", title: "duplicate Begin of an assembling message", target: "recv",
			frames: []frameSpec{
				copyFrame(0, 7, 12, 0, 4, flagBegin, 0, []byte("abcd")),
				copyFrame(0, 7, 12, 0, 4, flagBegin, 0, []byte("abcd")),
			},
			effect: "fail", failScope: "path", errKind: errInvalidSize},
		{id: "G10-29", title: "continuation chunk without an assembly (SPEC 10.5 step 5)", target: "recv",
			frames: []frameSpec{copyFrame(0, 8, 8, 4, 4, flagEnd, 0, []byte("efgh"))},
			effect: "fail", failScope: "path", errKind: errProtocol},
		{id: "G10-30", title: "continuation offset differs from the bytes received", target: "recv",
			frames: []frameSpec{
				copyFrame(0, 9, 12, 0, 4, flagBegin, 0, []byte("abcd")),
				copyFrame(0, 9, 12, 8, 4, flagEnd, 0, []byte("ijkl")),
			},
			effect: "fail", failScope: "path", errKind: errInvalidSize},
		{id: "G10-31", title: "Begin with ChunkOffset != 0", target: "recv",
			frames: []frameSpec{copyFrame(0, 9, 8, 4, 4, fBE, 0, []byte("abcd"))},
			effect: "fail", failScope: "path", errKind: errInvalidSize},
		{id: "G10-32", title: "non-zero aux on a continuation chunk", target: "recv",
			frames: []frameSpec{
				copyFrame(0, 10, 8, 0, 4, flagBegin, 0, []byte("abcd")),
				copyFrame(0, 10, 8, 4, 4, flagEnd, 7, []byte("efgh")),
			},
			effect: "fail", failScope: "path", errKind: errInvalidSize},
		{id: "G10-33", title: "single chunk with End but ChunkSize != TotalSize", target: "recv",
			frames: []frameSpec{copyFrame(0, 11, 8, 0, 4, fBE, 0, []byte("abcd"))},
			effect: "fail", failScope: "path", errKind: errInvalidSize},
		{id: "G10-34", title: "End chunk before TotalSize is reached", target: "recv",
			frames: []frameSpec{
				copyFrame(0, 12, 8, 0, 4, flagBegin, 0, []byte("abcd")),
				copyFrame(0, 12, 8, 4, 2, flagEnd, 0, []byte("ef")),
			},
			effect: "fail", failScope: "path", errKind: errInvalidSize},
		{id: "G10-35", title: "reassembly refused (MaxReassemblyBytes 100): remaining chunks drained", target: "recv", maxReassembly: reassCap,
			frames: []frameSpec{
				copyFrame(0, 13, 8, 0, 4, flagBegin, 0, []byte("abcd")),
				copyFrame(0, 13, 8, 4, 4, flagEnd, 0, []byte("efgh")),
			},
			effect: "drain", sentinel: true},
		{id: "G10-36", title: "refused response fails its call with ErrReassemblyLimit; the rest is drained", target: "call", maxReassembly: reassCap,
			frames: []frameSpec{
				copyFrame(0, 1, 8, 0, 4, flagBegin|flagResponse, 0, []byte("abcd")),
				copyFrame(0, 1, 8, 4, 4, flagEnd|flagResponse, 0, []byte("efgh")),
			},
			effect: "fail", failScope: "call", errKind: errReassembly, sentinel: true},
		{id: "G10-37", title: "refused request is drained and never handed to the handler", target: "serve", maxReassembly: reassCap,
			frames: []frameSpec{
				copyFrame(0, 14, 8, 0, 4, flagBegin|flagRequest, 9, []byte("abcd")),
				copyFrame(0, 14, 8, 4, 4, flagEnd|flagRequest, 0, []byte("efgh")),
			},
			effect: "drain", sentinel: serveSentinel},
		{id: "G10-38", title: "cancel of a running request: the handler context is canceled and no response is sent", target: "serve",
			frames: []frameSpec{copyFrame(0, 15, 4, 0, 4, fBEq, 1, []byte("slow"))},
			late:   []frameSpec{cancelFrame(0, 15)},
			effect: "deliver", deliver: []Delivery{request(1, 0, "", "slow")}},
		{id: "G10-39", title: "OpCancel without FeatureCancel is an unexpected packet", target: "recv", features: noFeat,
			frames: []frameSpec{cancelFrame(0, 1)},
			effect: "fail", failScope: "path", errKind: errUnexpected},
		{id: "G10-40", title: "OpCancel for an unknown request is ignored", target: "recv",
			frames: []frameSpec{cancelFrame(0, 99), copyFrame(0, 16, 5, 0, 5, fBE, 0, []byte("after"))},
			effect: "deliver", deliver: []Delivery{data("after")}, sentinel: true},
		{id: "G10-41", title: "data for an unknown stream is dropped (stream 5)", target: "recv",
			frames: []frameSpec{copyFrame(5, 17, 5, 0, 5, fBE, 0, []byte("other"))},
			effect: "drop", sentinel: true},
		{id: "G10-42", title: "unknown opcode on the Advanced receive path", target: "recv",
			frames: []frameSpec{{op: 0x10, ops: []uint64{1, 2}}},
			effect: "fail", failScope: "path", errKind: errUnexpected},
		{id: "G10-43", title: "OpError with an unknown code surfaces as ErrProtocol", target: "recv",
			frames: []frameSpec{{op: uint8(protocol.OpError), ops: []uint64{0, 9}}},
			effect: "fail", failScope: "path", errKind: errProtocol},
		{id: "G10-44", title: "OpError with code 5 surfaces as ErrConnNotFound", target: "recv",
			frames: []frameSpec{{op: uint8(protocol.OpError), ops: []uint64{0, 5}}},
			effect: "fail", failScope: "path", errKind: errConnNotFound},
	}
}

// frameVectors builds the frame matrix rows with their encoded packets.
func frameVectors() ([]FrameRow, error) {
	encode := func(specs []frameSpec) []Frame {
		out := make([]Frame, 0, len(specs))
		for _, f := range specs {
			pkt := f.packet()
			out = append(out, Frame{
				Op:       opName(f.op),
				Operands: append([]uint64(nil), f.ops...),
				Payload:  hexOf(f.payload),
				Packet:   hexOf(pkt[:]),
			})
		}
		return out
	}
	specs := frameRows()
	out := make([]FrameRow, 0, len(specs))
	for _, sp := range specs {
		features := sp.features
		if features == 0 {
			features = featuresAll11
		}
		frames := append([]frameSpec{negotiateFrame(features)}, sp.frames...)
		out = append(out, FrameRow{
			ID:                 sp.id,
			Title:              sp.title,
			Target:             sp.target,
			MaxReassemblyBytes: sp.maxReassembly,
			Frames:             encode(frames),
			LateFrames:         encode(sp.late),
			Effect:             sp.effect,
			FailScope:          sp.failScope,
			Deliver:            sp.deliver,
			Error:              sp.errKind,
			Sentinel:           sp.sentinel,
			AckFeatures:        features,
		})
	}
	return out, nil
}
