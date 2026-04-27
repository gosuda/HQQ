package protocol

import "encoding/binary"

// HQQ Protocol Version Constants.
//
// The Standard Protocol is versioned separately from optional AdvancedLink
// features. Packets are encoded as fixed-width little-endian words so the
// shared-memory ABI does not depend on uintptr width, Go struct padding, or the
// local machine's native byte order.
const (
	HQQProtocolMajorVersion = 1
	HQQProtocolMinorVersion = 1
)

const (
	// PacketWordCount is the number of 64-bit words in every packet:
	// word 0 stores the opcode and words 1-7 store operands.
	PacketWordCount = 8

	// PacketOperandCount is the number of operand words available after the
	// opcode word.
	PacketOperandCount = PacketWordCount - 1

	// PacketSize is the fixed wire/shared-memory size of every HQQ packet.
	PacketSize = PacketWordCount * 8
)

//go:generate go tool golang.org/x/tools/cmd/stringer -type=OpCode

// OpCode represents the operation code for HQQ protocol packets.
//
// OpCode is encoded in the low byte of little-endian packet word 0. The
// remaining bytes of word 0 are reserved and must be zero when creating new
// packets.
type OpCode uint8

const (
	// OpError (0x00): Error reporting packet.
	//
	// Operands:
	//   Operand0: ContextID (connection ID, copy ID, or request ID)
	//   Operand1: ErrorCode
	OpError OpCode = 0x00

	// OpConnCreate (0x01): Connection creation request.
	//
	// Operands:
	//   Operand0: ConnectionID
	OpConnCreate OpCode = 0x01

	// OpConnAccept (0x02): Connection acceptance.
	//
	// Operands:
	//   Operand0: ConnectionID
	OpConnAccept OpCode = 0x02

	// OpConnClose (0x03): Connection closure.
	//
	// Operands:
	//   Operand0: ConnectionID
	OpConnClose OpCode = 0x03

	// OpConnCopy (0x04): Connection-oriented data transfer.
	//
	// Connection-oriented transfer is currently reserved for AdvancedLink. The
	// Standard Protocol uses OpStandardLinkCopy directly.
	OpConnCopy OpCode = 0x04

	// OpProtoNegotiate (0x05): Protocol negotiation request.
	//
	// Operands:
	//   Operand0: Protocol version encoded as (major << 8) | minor
	//   Operand1: Requested feature flags
	//   Operand2: Supported capability flags
	OpProtoNegotiate OpCode = 0x05

	// OpProtoAck (0x06): Protocol negotiation acknowledgment.
	//
	// Operands:
	//   Operand0: Accepted protocol version encoded as (major << 8) | minor
	//   Operand1: Negotiated feature flags
	//   Operand2: Negotiated capability flags
	OpProtoAck OpCode = 0x06

	// OpStandardLinkCopy (0xFF): Standard Protocol data packet.
	//
	// Operands:
	//   Operand0: CopyID, monotonically increasing per endpoint
	//   Operand1: BufferIndex into the direction-local payload buffer pool
	//   Operand2: DataSize in bytes, 0 <= DataSize <= BufferSize
	//
	// Buffer ownership is not encoded in-band. Each direction has a separate
	// lock-free free-buffer ring. The receiver returns Operand1 to that free
	// ring after copying the payload out of shared memory.
	OpStandardLinkCopy OpCode = 0xFF
)

// Packet is the fixed-size Standard Protocol packet.
//
// Layout:
//
//	word 0: opcode in the low byte; remaining bits reserved
//	word 1: operand 0
//	word 2: operand 1
//	word 3: operand 2
//	word 4: operand 3
//	word 5: operand 4
//	word 6: operand 5
//	word 7: operand 6
//
// Every word is encoded with binary.LittleEndian.
type Packet [PacketSize]byte

// NewPacket returns a packet with op and up to seven operands encoded in
// little-endian form.
func NewPacket(op OpCode, operands ...uint64) Packet {
	var p Packet
	p.SetOp(op)
	for i, operand := range operands {
		if i >= PacketOperandCount {
			break
		}
		p.SetOperand(i, operand)
	}
	return p
}

// Op returns the packet opcode.
func (p Packet) Op() OpCode {
	return OpCode(binary.LittleEndian.Uint64(p[0:8]) & 0xFF)
}

// SetOp stores the packet opcode and clears reserved bits in word 0.
func (p *Packet) SetOp(op OpCode) {
	binary.LittleEndian.PutUint64(p[0:8], uint64(op))
}

// Operand returns operand i. It returns zero for out-of-range operand indexes.
func (p Packet) Operand(i int) uint64 {
	if i < 0 || i >= PacketOperandCount {
		return 0
	}
	offset := (i + 1) * 8
	return binary.LittleEndian.Uint64(p[offset : offset+8])
}

// SetOperand stores operand i. Out-of-range operand indexes are ignored.
func (p *Packet) SetOperand(i int, value uint64) {
	if i < 0 || i >= PacketOperandCount {
		return
	}
	offset := (i + 1) * 8
	binary.LittleEndian.PutUint64(p[offset:offset+8], value)
}
