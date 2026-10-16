package protocol_test

// Executable cross-check between spec/constants.json and the packet ABI.
// Additions (new opcodes) are allowed; every baseline value must still hold.

import (
	"encoding/hex"
	"encoding/json"
	"os"
	"testing"

	"gosuda.org/hqq/internal/protocol"
)

type specProtocolConsts struct {
	Versions struct {
		ProtocolMajor   uint64 `json:"protocol_major"`
		ProtocolMinor   uint64 `json:"protocol_minor"`
		ProtocolEncoded uint64 `json:"protocol_encoded"`
	} `json:"versions"`
	Packet struct {
		Size           int    `json:"size"`
		Words          int    `json:"words"`
		OperandCount   int    `json:"operand_count"`
		OpcodeMask     uint64 `json:"opcode_mask"`
		OperandOffsets []int  `json:"operand_word_offset"`
	} `json:"packet"`
	Opcodes       map[string]uint8    `json:"opcodes"`
	Operands      map[string][]string `json:"operands"`
	PacketVectors map[string]struct {
		Opcode   uint8    `json:"opcode"`
		Operands []uint64 `json:"operands"`
		Hex      string   `json:"hex"`
	} `json:"packet_vectors"`
}

func loadSpecProtocol(t *testing.T) *specProtocolConsts {
	t.Helper()
	raw, err := os.ReadFile("../../spec/constants.json")
	if err != nil {
		t.Fatalf("read spec/constants.json: %v", err)
	}
	var c specProtocolConsts
	if err := json.Unmarshal(raw, &c); err != nil {
		t.Fatalf("parse spec/constants.json: %v", err)
	}
	return &c
}

func TestSpecPacketABI(t *testing.T) {
	c := loadSpecProtocol(t)
	if protocol.PacketSize != c.Packet.Size || protocol.PacketWordCount != c.Packet.Words || protocol.PacketOperandCount != c.Packet.OperandCount {
		t.Errorf("packet geometry = %d bytes, %d words, %d operands; spec says %d, %d, %d",
			protocol.PacketSize, protocol.PacketWordCount, protocol.PacketOperandCount, c.Packet.Size, c.Packet.Words, c.Packet.OperandCount)
	}
	if len(c.Packet.OperandOffsets) != c.Packet.OperandCount {
		t.Fatalf("spec lists %d operand offsets for %d operands", len(c.Packet.OperandOffsets), c.Packet.OperandCount)
	}
	// Operand i lives in bytes [8*(i+1), 8*(i+2)) as a little-endian word.
	var p protocol.Packet
	for i, off := range c.Packet.OperandOffsets {
		p = protocol.Packet{}
		p.SetOperand(i, 0x0807060504030201)
		want := []byte{1, 2, 3, 4, 5, 6, 7, 8}
		if string(p[off:off+8]) != string(want) {
			t.Errorf("operand %d is not the little-endian word at byte offset %d: %x", i, off, p[:])
		}
	}
	// The opcode is the low byte of word 0; setting it clears the reserved bits.
	copy(p[0:8], []byte{0xAA, 0xBB, 0xCC, 0xDD, 0xEE, 0xFF, 0x11, 0x22})
	if uint64(p.Op()) != uint64(0xAA)&c.Packet.OpcodeMask {
		t.Errorf("Op() of word 0xAA.. = %#x", p.Op())
	}
	p.SetOp(protocol.OpConnCopy)
	for _, b := range p[1:8] {
		if b != 0 {
			t.Errorf("SetOp must clear reserved bits of word 0: %x", p[0:8])
			break
		}
	}
}

func TestSpecOpcodes(t *testing.T) {
	c := loadSpecProtocol(t)
	code := map[string]protocol.OpCode{
		"OpError": protocol.OpError, "OpConnCreate": protocol.OpConnCreate, "OpConnAccept": protocol.OpConnAccept,
		"OpConnClose": protocol.OpConnClose, "OpConnCopy": protocol.OpConnCopy, "OpProtoNegotiate": protocol.OpProtoNegotiate,
		"OpProtoAck": protocol.OpProtoAck, "OpStandardLinkTombstone": protocol.OpStandardLinkTombstone,
		"OpStandardLinkCopy": protocol.OpStandardLinkCopy,
	}
	for name, want := range c.Opcodes {
		got, ok := code[name]
		if !ok {
			t.Errorf("spec opcode %q unknown to the test", name)
			continue
		}
		if uint8(got) != want {
			t.Errorf("%s = %#x, spec says %#x", name, uint8(got), want)
		}
		if got.String() != name {
			t.Errorf("OpCode(%#x).String() = %q, want %q", want, got.String(), name)
		}
		if _, ok := c.Operands[name]; !ok {
			t.Errorf("spec has no operand table for %s", name)
		}
	}
	if len(c.Opcodes) != len(code) {
		t.Errorf("spec lists %d opcodes, the test knows %d baseline opcodes", len(c.Opcodes), len(code))
	}
}

func TestSpecVersions(t *testing.T) {
	c := loadSpecProtocol(t)
	if uint64(protocol.HQQProtocolMajorVersion) != c.Versions.ProtocolMajor {
		t.Errorf("protocol major = %d, spec says %d", protocol.HQQProtocolMajorVersion, c.Versions.ProtocolMajor)
	}
	if uint64(protocol.HQQProtocolMinorVersion) < c.Versions.ProtocolMinor {
		t.Errorf("protocol minor = %d, below the baseline %d", protocol.HQQProtocolMinorVersion, c.Versions.ProtocolMinor)
	}
	if enc := c.Versions.ProtocolMajor<<8 | c.Versions.ProtocolMinor; enc != c.Versions.ProtocolEncoded {
		t.Errorf("encoded baseline version = %#x, spec says %#x", enc, c.Versions.ProtocolEncoded)
	}
}

func TestSpecPacketVectors(t *testing.T) {
	c := loadSpecProtocol(t)
	build := map[string]protocol.Packet{
		"standard_copy":      protocol.NewStandardLinkCopyPacket(1, 2, 3),
		"standard_tombstone": protocol.NewStandardLinkTombstonePacket(1, 2, 6),
		"error":              protocol.NewPacket(protocol.OpError, 9, 5),
		"conn_create":        protocol.NewPacket(protocol.OpConnCreate, 2),
		"conn_accept":        protocol.NewPacket(protocol.OpConnAccept, 2),
		"conn_close":         protocol.NewPacket(protocol.OpConnClose, 2, 0),
		"conn_copy":          protocol.NewConnCopyPacket(1, 2, 3, 4, 5, 3, 7),
		"proto_negotiate":    protocol.NewPacket(protocol.OpProtoNegotiate, c.Versions.ProtocolEncoded, 3, 0),
		"proto_ack":          protocol.NewPacket(protocol.OpProtoAck, c.Versions.ProtocolEncoded, 3, 0),
	}
	if len(c.PacketVectors) != len(build) {
		t.Errorf("spec has %d packet vectors, the test builds %d", len(c.PacketVectors), len(build))
	}
	for name, v := range c.PacketVectors {
		p, ok := build[name]
		if !ok {
			t.Errorf("packet vector %q unknown to the test", name)
			continue
		}
		if got := hex.EncodeToString(p[:]); got != v.Hex {
			t.Errorf("%s = %s, spec vector %s", name, got, v.Hex)
		}
		if uint8(p.Op()) != v.Opcode {
			t.Errorf("%s opcode = %#x, want %#x", name, uint8(p.Op()), v.Opcode)
		}
		for i, want := range v.Operands {
			if got := p.Operand(i); got != want {
				t.Errorf("%s operand %d = %d, want %d", name, i, got, want)
			}
		}
		raw, _ := hex.DecodeString(v.Hex)
		if len(raw) != c.Packet.Size {
			t.Errorf("%s vector has %d bytes", name, len(raw))
		}
	}
}
