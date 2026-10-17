package rt

import (
	"cmp"
	"encoding/binary"
)

// Wire constants (BINDGEN.md 4.2).
const (
	HeaderSize    = 16
	DetailMax     = 1024
	DetailReserve = 1032
	MsgMax        = 32
	ErrBodyMax    = 37
	VarSlack      = 11
	NullLen       = 0xFFFFFFFF
	Align16       = 16 // ALIGN of 4.2
)

const (
	protoMajor     = 1
	protoMinor     = 0
	minBufferSize  = 128
	maxBufferSize  = 1 << 20
	minBufferCount = 2
	maxBufferCount = 4096
	hardMaxMessage = 1 << 30
	minMaxMessage  = 4096
	maxHandleCount = 65535

	helloBody = 64
	openReq   = 16
	openResp  = 12
	openName  = 64
	openMeth  = 1 // control-link method BG_OPEN

	// openStatusBase is added to the OPEN error code to form the Advanced
	// response status (4.11).
	openStatusBase = 0x50000
)

var (
	magic     = [4]byte{'H', 'Q', 'B', 'G'}
	openMagic = [4]byte{'H', 'Q', 'B', 'O'}
)

// Frame kinds (4.4). The batch kinds are reserved by 4.13 and never sent or
// accepted: a receiver treats them like any other kind it cannot use.
const (
	kindRequest       = 0x01
	kindResponse      = 0x02
	kindCont          = 0x03
	kindHello         = 0x04
	kindHelloReply    = 0x05
	kindBatchRequest  = 0x06
	kindBatchResponse = 0x07
	kindClose         = 0x08
	kindCloseAck      = 0x09
)

// Frame flags (4.3) and the reserved HELLO flag of 4.13.
const (
	flagDetail = 0x01
	flagFail   = 0x02
	flagsKnown = flagDetail | flagFail

	helloFlagBatch = 0x1
)

// Status values (4.10).
const (
	statusOK           = 0
	statusUnknownOp    = 1
	statusBadRequest   = 2
	statusBadHandle    = 3
	statusLimit        = 4
	statusDenied       = 5
	statusInternal     = 6
	statusProtocol     = 7
	statusShuttingDown = 8
	statusReservedMax  = 0xFF
	statusAppMin       = 0x0100
	statusAppMax       = 0xFFFF
)

// HELLO_REPLY status values (4.11).
const (
	helloOK             = 0
	helloDigestMismatch = 1
	helloVersion        = 2
	helloGeometry       = 3
	helloLimit          = 4
	helloBad            = 5
)

// Bootstrap error codes (4.11).
const (
	openCodeVersion  = 1
	openCodeGeometry = 2
	openCodeBusy     = 3
	openCodeInternal = 4
)

var le = binary.LittleEndian

// header is the 16-byte frame header (4.3), copied once out of shared memory
// (4.4 rule 8).
type header struct {
	kind  uint8
	flags uint8
	op    uint16
	id    uint32
	total uint32
	aux   uint32
}

// loadHeader reads the header of b with one load per field. b must hold at
// least HeaderSize bytes.
func loadHeader(b []byte) header {
	_ = b[HeaderSize-1]
	return header{
		kind:  b[0],
		flags: b[1],
		op:    le.Uint16(b[2:]),
		id:    le.Uint32(b[4:]),
		total: le.Uint32(b[8:]),
		aux:   le.Uint32(b[12:]),
	}
}

// putHeader writes h at the start of b.
func putHeader(b []byte, h header) {
	_ = b[HeaderSize-1]
	le.PutUint64(b[0:], uint64(h.kind)|uint64(h.flags)<<8|uint64(h.op)<<16|uint64(h.id)<<32)
	le.PutUint32(b[8:], h.total)
	le.PutUint32(b[12:], h.aux)
}

// nextID is the successor of id under 4.12: 0 is never used.
func nextID(id uint32) uint32 {
	return cmp.Or(id+1, 1)
}

// statusName names a status for error text.
func statusName(s uint32) string {
	switch s {
	case statusUnknownOp:
		return "unknown op"
	case statusBadRequest:
		return "bad request"
	case statusBadHandle:
		return "bad handle"
	case statusLimit:
		return "limit exceeded"
	case statusDenied:
		return "denied"
	case statusInternal:
		return "internal error"
	case statusProtocol:
		return "protocol violation"
	case statusShuttingDown:
		return "shutting down"
	}
	if s >= statusAppMin && s <= statusAppMax {
		return "application status"
	}
	return "reserved status"
}
