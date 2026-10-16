package conformance

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"gosuda.org/hqq/internal/protocol"
)

// TestPacketVectors (G-02) encodes every packet vector with internal/protocol
// and decodes the bytes back. The nine constants.json vectors are also
// compared with the document's hex, opcode and operands.
func TestPacketVectors(t *testing.T) {
	m := loadManifest(t)
	raw, err := os.ReadFile("../spec/constants.json")
	if err != nil {
		t.Fatalf("read spec/constants.json: %v", err)
	}
	var doc struct {
		Packets map[string]struct {
			Opcode   uint8    `json:"opcode"`
			Operands []uint64 `json:"operands"`
			Hex      string   `json:"hex"`
		} `json:"packet_vectors"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("decode spec/constants.json: %v", err)
	}
	fromDoc := 0
	for _, v := range m.Packets {
		if v.Source == "constants.json" {
			want, ok := doc.Packets[v.Name]
			if !ok {
				t.Errorf("packet vector %s is not in constants.json", v.Name)
				continue
			}
			fromDoc++
			if v.Opcode != want.Opcode || v.Hex != want.Hex || !equalU64(v.Operands, want.Operands) {
				t.Errorf("packet %s: manifest %+v, constants.json %+v", v.Name, v, want)
			}
		}
		t.Run(v.Name, func(t *testing.T) {
			want := mustDecodeHex(t, v.Hex)
			if len(want) != protocol.PacketSize {
				t.Fatalf("hex has %d bytes, want %d", len(want), protocol.PacketSize)
			}
			ops := make([]uint64, len(v.Operands))
			copy(ops, v.Operands)
			p := protocol.NewPacket(protocol.OpCode(v.Opcode), ops...)
			if !bytes.Equal(p[:], want) {
				t.Fatalf("encode:\n got %x\nwant %x", p[:], want)
			}
			var d protocol.Packet
			copy(d[:], want)
			if d.Op() != protocol.OpCode(v.Opcode) {
				t.Fatalf("decoded opcode %#x, want %#x", d.Op(), v.Opcode)
			}
			for i, op := range v.Operands {
				if got := d.Operand(i); got != op {
					t.Errorf("operand %d = %#x, want %#x", i, got, op)
				}
			}
			for i := len(v.Operands); i < protocol.PacketOperandCount; i++ {
				if got := d.Operand(i); got != 0 {
					t.Errorf("unused operand %d = %#x, want 0", i, got)
				}
			}
		})
	}
	if fromDoc != 9 {
		t.Errorf("manifest carries %d constants.json packet vectors, want 9", fromDoc)
	}
}

// equalU64 reports whether two operand lists are equal.
func equalU64(a, b []uint64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestPayloadVectors checks the payload algorithm of INTEROP_CLI section 5
// against the manifest, the golden values quoted in the document, and the
// verifier.
func TestPayloadVectors(t *testing.T) {
	m := loadManifest(t)
	for _, v := range m.Payloads {
		got := Payload(v.Seed, v.Seq, v.Size)
		if len(got) != v.Size {
			t.Fatalf("Payload(%#x, %d, %d) has %d bytes", v.Seed, v.Seq, v.Size, len(got))
		}
		if v.Hex != "" && hex.EncodeToString(got) != v.Hex {
			t.Errorf("Payload(%#x, %d, %d) = %x, want %s", v.Seed, v.Seq, v.Size, got, v.Hex)
		}
		if v.SHA256 != "" {
			sum := sha256.Sum256(got)
			if hex.EncodeToString(sum[:]) != v.SHA256 || v.Length != v.Size {
				t.Errorf("Payload(%#x, %d, %d): sha256 %x, want %s", v.Seed, v.Seq, v.Size, sum, v.SHA256)
			}
		}
		if err := VerifyPayload(v.Seed, v.Seq, got); err != nil {
			t.Errorf("VerifyPayload(%#x, %d, %d): %v", v.Seed, v.Seq, v.Size, err)
		}
	}

	// Golden values quoted in INTEROP_CLI.md section 5.
	golden := []struct {
		seed, seq uint64
		size      int
		want      string
	}{
		{1, 0, 1, "c1"},
		{1, 0, 8, "c15c0289ec2d0a91"},
		{1, 0, 16, "0000000000000000" + "add8412c" + "c15c0289"},
		{0xdeadbeef, 7, 40, strings.Join([]string{"0700000000000000", "47a0b112", "7536cc3826b6b2d2c8375c13ebdfc0ab9ca7d35f20744cb4782227b3"}, "")},
	}
	for _, g := range golden {
		if got := hex.EncodeToString(Payload(g.seed, g.seq, g.size)); got != g.want {
			t.Errorf("Payload(%#x, %d, %d) = %s, want %s", g.seed, g.seq, g.size, got, g.want)
		}
	}

	// The verifier rejects a changed byte, a wrong sequence number and a bad
	// header checksum.
	p := Payload(5, 9, 32)
	if err := VerifyPayload(5, 9, p); err != nil {
		t.Fatalf("verify good payload: %v", err)
	}
	bad := append([]byte(nil), p...)
	bad[20] ^= 1
	if err := VerifyPayload(5, 9, bad); err == nil {
		t.Error("verify accepted a corrupted body")
	}
	if err := VerifyPayload(5, 10, p); err == nil {
		t.Error("verify accepted the wrong sequence number")
	}
	bad = append([]byte(nil), p...)
	bad[8] ^= 0xff
	if err := VerifyPayload(5, 9, bad); err == nil {
		t.Error("verify accepted a bad crc")
	}
}
