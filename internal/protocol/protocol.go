package protocol

// HQQ Protocol Version Constants
// These constants define the current protocol version for compatibility checking
const (
	HQQ_PROTOCOL_MAJOR_VERSION = 1 // Major version for incompatible changes
	HHQ_PROTOCOL_MINOR_VERSION = 1 // Minor version for backward-compatible additions
)

//go:generate go tool stringer -type=OpCode
// This generate directive creates string representations of OpCode constants
// Run "go generate" in this directory to update the stringer file

// OpCode represents the operation code for HQQ protocol packets
// Each operation code defines a specific type of packet or action
type OpCode uintptr

const (
	// OpError (0x00): Error reporting packet
	// Used to report errors between processes
	// Operands:
	//   Operand0: ContextID (typically connection ID or request ID)
	//   Operand1: Error Number (ErrorCode)
	// Direction: Bidirectional
	OpError OpCode = 0x00

	// OpConnCreate (0x01): Connection creation request
	// Sent to establish a new logical connection between processes
	// Operands:
	//   Operand0: ConnectionID (unique identifier for the connection)
	// Response: OpConnAccept with the same ConnectionID or OpError on failure
	// Direction: Primary → Secondary
	OpConnCreate OpCode = 0x01

	// OpConnAccept (0x02): Connection acceptance
	// Sent in response to OpConnCreate to acknowledge and accept a connection
	// Operands:
	//   Operand0: ConnectionID (matches the OpConnCreate request)
	// Direction: Secondary → Primary
	OpConnAccept OpCode = 0x02

	// OpConnClose (0x03): Connection closure
	// Sent by either party to terminate an existing connection
	// Operands:
	//   Operand0: ConnectionID (identifier of the connection to close)
	// Direction: Bidirectional
	OpConnClose OpCode = 0x03

	// OpConnCopy (0x04): Connection-oriented data transfer
	// Used for transferring data over an established connection
	// Operands:
	//   Operand0: ConnectionID (identifier of the connection)
	//   Operand1: CopyID (unique identifier for this transfer)
	//   Operand2: CopySize (total size of the data to transfer)
	//   Operand3: TransferSize (size of this specific transfer)
	//   Operand4: TransferOffset (offset within the total data)
	//   Operand5: BufferSize (size of the buffer containing data)
	//   Operand6: BufferOffset (offset within the buffer)
	// Direction: Bidirectional
	OpConnCopy OpCode = 0x04

	// OpProtoNegotiate (0x05): Protocol negotiation request
	// Sent to initiate protocol negotiation and feature discovery
	// Operands:
	//   Operand0: Protocol Version (encoded as major.minor)
	//   Operand1: Feature Flags (bitmask of requested features)
	//   Operand2: Capability Flags (bitmask of supported capabilities)
	// Version Encoding: (Major << 8) | Minor
	// Response: OpProtoAck with negotiated parameters
	// Direction: Primary → Secondary
	OpProtoNegotiate OpCode = 0x05

	// OpProtoAck (0x06): Protocol negotiation acknowledgment
	// Sent in response to OpProtoNegotiate to complete negotiation
	// Operands:
	//   Operand0: Accepted Protocol Version (encoded as major.minor)
	//   Operand1: Accepted Feature Flags (bitmask of negotiated features)
	//   Operand2: Accepted Capability Flags (bitmask of negotiated capabilities)
	// Version Encoding: (Major << 8) | Minor
	// Direction: Secondary → Primary
	OpProtoAck OpCode = 0x06

	// OpStandardLinkCopy (0xFF): Direct data copy without connection
	// Used for simple, connection-less data transfer with minimal overhead
	// Operands:
	//   Operand0: CopyID (unique identifier for this transfer)
	//   Operand1: BufferIndex (index of the buffer containing data)
	//   Operand2: DataSize (actual size of data in the buffer)
	// Note: This is the lowest overhead data transfer method
	// Direction: Bidirectional
	OpStandardLinkCopy OpCode = 0xFF

	// Note: Operation codes 0x07-0xFE are reserved for future use
)

// Packet represents the standard HQQ protocol packet structure
// All communication between processes uses this fixed-size packet format
type Packet struct {
	Op       OpCode  // Operation type (1 byte, padded to 8 bytes)
	Operand0 uintptr // First operand (typically ID or offset)
	Operand1 uintptr // Second operand (typically size or flags)
	Operand2 uintptr // Third operand (typically offset or count)
	Operand3 uintptr // Fourth operand (reserved for future use)
	Operand4 uintptr // Fifth operand (reserved for future use)
	Operand5 uintptr // Sixth operand (reserved for future use)
	Operand6 uintptr // Seventh operand (reserved for future use)
}

// Packet Structure Notes:
// - Total size: 56 bytes (7 operands × 8 bytes + 1 byte opcode + padding)
// - All operands are uintptr for platform independence (4 bytes on 32-bit, 8 bytes on 64-bit)
// - Fixed size enables efficient memory allocation and cache-friendly access
// - Reserved operands allow for future protocol extensions without breaking compatibility
