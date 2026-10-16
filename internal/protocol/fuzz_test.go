package protocol_test

import (
	"encoding/binary"
	"testing"

	"gosuda.org/hqq/internal/protocol"
)

// FuzzPacketRoundTrip checks the fixed little-endian packet ABI: every
// constructor must agree with the generic accessors and with a manual
// little-endian decode of the 64 bytes.
func FuzzPacketRoundTrip(f *testing.F) {
	f.Add(uint8(protocol.OpStandardLinkCopy), uint64(1), uint64(2), uint64(3), uint64(4), uint64(5), uint64(6), uint64(7))
	f.Add(uint8(protocol.OpConnCopy), ^uint64(0), uint64(0), ^uint64(0), uint64(1)<<63, uint64(0), uint64(0x3f), uint64(42))
	f.Fuzz(func(t *testing.T, op uint8, a, b, c, d, e, g, h uint64) {
		operands := []uint64{a, b, c, d, e, g, h}
		p := protocol.NewPacket(protocol.OpCode(op), operands...)
		if p.Op() != protocol.OpCode(op) {
			t.Fatalf("Op = %v, want %v", p.Op(), op)
		}
		if w0 := binary.LittleEndian.Uint64(p[0:8]); w0 != uint64(op) {
			t.Fatalf("word 0 = %#x, want opcode with zero reserved bits", w0)
		}
		for i, v := range operands {
			if got := p.Operand(i); got != v {
				t.Fatalf("Operand(%d) = %#x, want %#x", i, got, v)
			}
			if got := binary.LittleEndian.Uint64(p[(i+1)*8:]); got != v {
				t.Fatalf("word %d = %#x, want %#x", i+1, got, v)
			}
		}
		if p.Operand(-1) != 0 || p.Operand(protocol.PacketOperandCount) != 0 {
			t.Fatal("out-of-range operands must read as zero")
		}

		cc := protocol.NewConnCopyPacket(a, b, c, d, e, g, h)
		want := protocol.NewPacket(protocol.OpConnCopy, operands...)
		if cc != want {
			t.Fatal("NewConnCopyPacket disagrees with NewPacket")
		}
		if protocol.NewStandardLinkCopyPacket(a, b, c) != protocol.NewPacket(protocol.OpStandardLinkCopy, a, b, c) {
			t.Fatal("NewStandardLinkCopyPacket disagrees with NewPacket")
		}
		if protocol.NewStandardLinkTombstonePacket(a, b, c) != protocol.NewPacket(protocol.OpStandardLinkTombstone, a, b, c) {
			t.Fatal("NewStandardLinkTombstonePacket disagrees with NewPacket")
		}
	})
}
