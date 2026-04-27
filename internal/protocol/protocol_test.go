package protocol_test

import (
	"encoding/binary"
	"testing"
	"unsafe"

	"gosuda.org/hqq/internal/protocol"
)

var (
	sinkPacket  protocol.Packet
	sinkOp      protocol.OpCode
	sinkOperand uint64
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
	if got := protocol.OpStandardLinkTombstone.String(); got != "OpStandardLinkTombstone" {
		t.Fatalf("OpStandardLinkTombstone.String() = %q", got)
	}
	if got := protocol.OpStandardLinkCopy.String(); got != "OpStandardLinkCopy" {
		t.Fatalf("OpStandardLinkCopy.String() = %q", got)
	}
}

func TestTombstonePacketLayout(t *testing.T) {
	p := protocol.NewStandardLinkTombstonePacket(11, 22, 33)
	if got := p.Op(); got != protocol.OpStandardLinkTombstone {
		t.Fatalf("Op() = %v, want %v", got, protocol.OpStandardLinkTombstone)
	}
	if got := p.Operand(0); got != 11 {
		t.Fatalf("operand 0 = %d, want 11", got)
	}
	if got := p.Operand(1); got != 22 {
		t.Fatalf("operand 1 = %d, want 22", got)
	}
	if got := p.Operand(2); got != 33 {
		t.Fatalf("operand 2 = %d, want 33", got)
	}
}

func BenchmarkNewPacket(b *testing.B) {
	var p protocol.Packet
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		p = protocol.NewPacket(protocol.OpStandardLinkCopy, uint64(i), 2, 3)
	}
	sinkPacket = p
}

func BenchmarkNewStandardLinkCopyPacket(b *testing.B) {
	var p protocol.Packet
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		p = protocol.NewStandardLinkCopyPacket(uint64(i), 2, 3)
	}
	sinkPacket = p
}

func BenchmarkPacketDecode(b *testing.B) {
	p := protocol.NewStandardLinkCopyPacket(1, 2, 3)
	var op protocol.OpCode
	var operand uint64
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		op = p.Op()
		operand = p.Operand(1)
	}
	sinkOp = op
	sinkOperand = operand
}
