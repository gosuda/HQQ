package protocol

//go:generate go tool stringer -type=OpCode
type OpCode uintptr

const (
	// Error: 0:ContextID, 1:ErrNo
	OpError OpCode = 0x00

	// ConnCreate: 0:ConnectionID
	OpConnCreate OpCode = 0x01

	// ConnAccept: 0:ConnectionID
	OpConnAccept OpCode = 0x02

	// ConnClose: 0:ConnectionID
	OpConnClose OpCode = 0x03

	// ConnCopy: 0:ConnectionID, 1:CopyID, 2:CopySize, 3:TransferSize, 4:TransferOffset, 5:BufferSize, 6:BufferOffset
	OpConnCopy OpCode = 0x04

	// 0x05-0x0F: Reserved
)

type Packet struct {
	Op       OpCode
	Operand0 uintptr
	Operand1 uintptr
	Operand2 uintptr
	Operand3 uintptr
	Operand4 uintptr
	Operand5 uintptr
	Operand6 uintptr
}
