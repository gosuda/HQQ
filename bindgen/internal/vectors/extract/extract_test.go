package extract

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func repoFile(t *testing.T, rel string) string {
	t.Helper()
	return filepath.Join("..", "..", "..", "..", filepath.FromSlash(rel))
}

func readRepo(t *testing.T, rel string) []byte {
	t.Helper()
	data, err := os.ReadFile(repoFile(t, rel))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// TestDocVectors is the check of 10.1: the committed files equal what the document yields today.
func TestDocVectors(t *testing.T) {
	if err := VerifyCommitted(filepath.Join("..", "..", "..", "..")); err != nil {
		t.Error(err)
	}
}

func TestExtractShape(t *testing.T) {
	f, err := Extract(readRepo(t, DocRel))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, fr := range f.Microlib.Frames {
		names = append(names, fr.Name)
	}
	want := "HELLO HELLO_REPLY HELLO_REPLY_DIGEST R1_REQ R1_RESP R2_REQ R2_RESP R3_REQ R3_RESP R4_REQ R4_RESP " +
		"R5_REQ R5_RESP R6_REQ R6_RESP R7_REQ R7_RESP R8_REQ R8_RESP R9_REQ R9_RESP R10_REQ R10_RESP " +
		"R11_REQ R11_RESP R12_REQ R12_RESP R13_REQ R13_RESP CLOSE CLOSE_ACK"
	if got := strings.Join(names, " "); got != want {
		t.Errorf("microlib frames:\n got %s\nwant %s", got, want)
	}
	if len(f.Chunking.Frames) != 4 || f.Chunking.BufferSize != 128 {
		t.Errorf("chunking group: %d frames, buffer size %d", len(f.Chunking.Frames), f.Chunking.BufferSize)
	}
	if len(f.Wrap.Frames) != 2 {
		t.Errorf("wrap group has %d frames", len(f.Wrap.Frames))
	}
	byName := map[string]Frame{}
	for _, g := range [][]Frame{f.Microlib.Frames, f.Chunking.Frames, f.Wrap.Frames} {
		for _, fr := range g {
			byName[fr.Name] = fr
		}
	}
	for name, c := range map[string]struct {
		id  uint32
		dir string
	}{
		"R3_REQ":      {3, dirC2S},
		"R3_RESP":     {3, dirS2C},
		"R13_RESP":    {13, dirS2C},
		"HELLO":       {0, dirC2S},
		"CLOSE_ACK":   {0, dirS2C},
		"C_REQ_2":     {1, dirC2S},
		"WRAP_A_REQ":  {0xFFFFFFFF, dirC2S},
		"WRAP_B_REQ":  {1, dirC2S},
		"C_RESP_1":    {1, dirS2C},
		"R8_RESP":     {8, dirS2C},
		"R7_REQ":      {7, dirC2S},
		"HELLO_REPLY": {0, dirS2C},
	} {
		fr, ok := byName[name]
		if !ok {
			t.Errorf("vector %s missing", name)
			continue
		}
		if fr.ID != c.id || fr.Dir != c.dir {
			t.Errorf("%s: id %d dir %s, want id %d dir %s", name, fr.ID, fr.Dir, c.id, c.dir)
		}
	}
	if fr := byName["R8_RESP"]; len(fr.Hex) != 2*(16+32) {
		t.Errorf("R8_RESP spans two hex lines and must be 48 bytes, got %d hex digits", len(fr.Hex))
	}
	sum := sha256.Sum256([]byte(f.Microlib.Canonical))
	if hex.EncodeToString(sum[:]) != f.Microlib.Digest {
		t.Error("microlib digest is not the hash of its canonical text")
	}
}

// TestExtractRejects mutates the document and expects Extract to name the defect.
func TestExtractRejects(t *testing.T) {
	doc := string(readRepo(t, DocRel))
	digestLine := "06a738744a1ea9f11fb08cc41afbe48e1891c76e60830a276e98cf8c15944e5c"
	cases := []struct {
		name   string
		mutate func(string) string
		want   string
	}{
		{"digest differs from the canonical text", func(s string) string {
			return strings.Replace(s, "```text digest=microlib\n"+digestLine, "```text digest=microlib\n"+strings.Repeat("0", 64), 1)
		}, "digest=microlib is"},
		{"canonical text edited", func(s string) string {
			return strings.Replace(s, "fn 1 ml_add", "fn 1 ml_addx", 1)
		}, "digest=microlib is"},
		{"frame body shortened", func(s string) string {
			return strings.Replace(s, "```hex vector=R3_RESP\n020001000300000004000000000000002a000000", "```hex vector=R3_RESP\n020001000300000004000000000000002a0000", 1)
		}, "R3_RESP"},
		{"odd hex digit count", func(s string) string {
			return strings.Replace(s, "```hex vector=CLOSE\n08000000000000000000000000000000", "```hex vector=CLOSE\n0800000000000000000000000000000", 1)
		}, "CLOSE"},
		{"duplicate vector", func(s string) string {
			return strings.Replace(s, "```hex vector=CLOSE_ACK", "```hex vector=CLOSE", 1)
		}, "duplicate block"},
		{"unterminated fence", func(s string) string {
			return s + "\n```hex vector=EXTRA\n00\n"
		}, "unterminated"},
		{"HELLO carries another digest", func(s string) string {
			return strings.Replace(s, "06a738744a1ea9f11fb08cc41afbe48e\n1891c76e60830a276e98cf8c15944e5c\n```\n\nHeader `04", "06a738744a1ea9f11fb08cc41afbe48f\n1891c76e60830a276e98cf8c15944e5c\n```\n\nHeader `04", 1)
		}, "HELLO"},
		{"unknown vector name", func(s string) string {
			return strings.Replace(s, "vector=WRAP_B_REQ", "vector=ODD", 1)
		}, "ODD"},
		{"missing canonical block", func(s string) string {
			return strings.Replace(s, "```text canon=sqlite_lowlevel", "```text note=sqlite_lowlevel", 1)
		}, "canon=sqlite_lowlevel"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			mutated := c.mutate(doc)
			if mutated == doc {
				t.Fatal("mutation did not change the document")
			}
			_, err := Extract([]byte(mutated))
			if err == nil {
				t.Fatal("Extract accepted the mutated document")
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("error %q does not mention %q", err, c.want)
			}
		})
	}
}

func TestExtractIsDeterministic(t *testing.T) {
	doc := readRepo(t, DocRel)
	a, err := Extract(doc)
	if err != nil {
		t.Fatal(err)
	}
	b, err := Extract(doc)
	if err != nil {
		t.Fatal(err)
	}
	ja, _ := a.Marshal()
	jb, _ := b.Marshal()
	if !bytes.Equal(ja, jb) || !bytes.Equal(a.CHeader(), b.CHeader()) {
		t.Error("two extractions of the same document differ")
	}
	back, err := Load(ja)
	if err != nil {
		t.Fatal(err)
	}
	jc, _ := back.Marshal()
	if !bytes.Equal(ja, jc) {
		t.Error("vectors.json does not survive Load and Marshal")
	}
}
