package protocol_test

import (
	"encoding/binary"
	"testing"
	"unsafe"

	"gosuda.org/hqq/internal/protocol"
)

func TestPacketLittleEndianLayout(t *testing.T) {
	p := protocol.NewPacket(
		protocol.OpStandardLinkCopy,
		0x0102030405060708,
		0x1112131415161718,
		0xFFEEDDCCBBAA9988,
	)

	if got := unsafe.Sizeof(p); got != protocol.PacketSize {
		t.Fatalf("Packet size = %d, want %d", got, protocol.PacketSize)
	}
	if got := binary.LittleEndian.Uint64(p[0:8]); got != uint64(protocol.OpStandardLinkCopy) {
		t.Fatalf("opcode word = %#x, want %#x", got, protocol.OpStandardLinkCopy)
	}
	if got := p.Op(); got != protocol.OpStandardLinkCopy {
		t.Fatalf("Op() = %v, want %v", got, protocol.OpStandardLinkCopy)
	}
	if got := p.Operand(0); got != 0x0102030405060708 {
		t.Fatalf("operand 0 = %#x", got)
	}
	if got := p.Operand(1); got != 0x1112131415161718 {
		t.Fatalf("operand 1 = %#x", got)
	}
	if got := p.Operand(2); got != 0xFFEEDDCCBBAA9988 {
		t.Fatalf("operand 2 = %#x", got)
	}
	if got := p.Operand(protocol.PacketOperandCount); got != 0 {
		t.Fatalf("out-of-range operand = %#x, want 0", got)
	}
}

func TestOpCodeStringIncludesStandardCopy(t *testing.T) {
	if got := protocol.OpStandardLinkCopy.String(); got != "OpStandardLinkCopy" {
		t.Fatalf("OpStandardLinkCopy.String() = %q", got)
	}
}
