package rt

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

func TestAlign(t *testing.T) {
	for _, tc := range []struct{ off, n, want int }{
		{0, 4, 0}, {1, 4, 4}, {3, 4, 4}, {4, 4, 4}, {5, 8, 8}, {8, 8, 8}, {9, 8, 16}, {7, 1, 7}, {3, 2, 4}, {17, 16, 32},
	} {
		if got := Align(tc.off, tc.n); got != tc.want {
			t.Errorf("Align(%d, %d) = %d, want %d", tc.off, tc.n, got, tc.want)
		}
	}
	if StrWire(0) != 5 || StrWire(3) != 8 || BytesWire(0) != 4 || BytesWire(3) != 7 {
		t.Error("wire sizes")
	}
}

func TestCheckStr(t *testing.T) {
	for _, tc := range []struct {
		s     string
		limit int
		ok    bool
	}{
		{"", 0, true}, {"abc", 0, true}, {"abc", 3, true}, {"abc", 2, false}, {"a\x00b", 0, false}, {"\x00", 5, false},
		{strings.Repeat("x", 1000), 0, true}, {"é", 2, true}, {"é", 1, false},
	} {
		err := CheckStr(tc.s, tc.limit)
		if (err == nil) != tc.ok || (err != nil && !errors.Is(err, ErrInvalidArg)) {
			t.Errorf("CheckStr(%q, %d) = %v", tc.s, tc.limit, err)
		}
	}
}

func TestPutGetStr(t *testing.T) {
	// The detail string of vector R2: 08000000 "overflow" 00.
	want := []byte{8, 0, 0, 0, 'o', 'v', 'e', 'r', 'f', 'l', 'o', 'w', 0}
	b := make([]byte, 16+StrWire(8))
	end := PutStr(b, 16, "overflow")
	if end != 16+len(want) || !bytes.Equal(b[16:end], want) {
		t.Fatalf("PutStr = %x end %d", b[16:end], end)
	}
	s, e, err := GetStr(b[:end], 16, DetailMax)
	if err != nil || s != "overflow" || e != end {
		t.Fatalf("GetStr = %q, %d, %v", s, e, err)
	}
	// An unaligned offset is aligned up first.
	s, e, err = GetStr(b[:end], 13, 0)
	if err != nil || s != "overflow" || e != end {
		t.Fatalf("GetStr(13) = %q, %d, %v", s, e, err)
	}
	// The empty string.
	empty := []byte{0, 0, 0, 0, 0}
	if s, e, err := GetStr(empty, 0, 0); err != nil || s != "" || e != 5 {
		t.Fatalf("empty = %q, %d, %v", s, e, err)
	}
}

func TestGetStrRejects(t *testing.T) {
	good := []byte{3, 0, 0, 0, 'a', 'b', 'c', 0}
	cases := []struct {
		name  string
		b     []byte
		off   int
		limit int
	}{
		{"empty body", nil, 0, 0},
		{"length truncated", good[:3], 0, 0},
		{"data truncated", good[:6], 0, 0},
		{"NUL missing", good[:7], 0, 0},
		{"NUL replaced", []byte{3, 0, 0, 0, 'a', 'b', 'c', 'd'}, 0, 0},
		{"NUL inside", []byte{3, 0, 0, 0, 'a', 0, 'c', 0}, 0, 0},
		{"NULL_LEN", []byte{0xff, 0xff, 0xff, 0xff, 0}, 0, 0},
		{"length above limit", good, 0, 2},
		{"huge length", []byte{0xfe, 0xff, 0xff, 0xff, 'a', 0}, 0, 0},
		{"offset beyond body", good, 9, 0},
		{"negative offset", good, -1, 0},
		{"offset at the end", good, 8, 0},
	}
	for _, tc := range cases {
		if s, e, err := GetStr(tc.b, tc.off, tc.limit); err == nil {
			t.Errorf("%s: accepted %q end %d", tc.name, s, e)
		}
	}
	if _, _, err := GetStr(good, 0, 3); err != nil {
		t.Errorf("length exactly at the limit: %v", err)
	}
}

func TestPutGetBytes(t *testing.T) {
	// The src of vector R4: 03000000 010203.
	b := make([]byte, 8+BytesWire(3))
	end := PutBytes(b, 8, []byte{1, 2, 3})
	if end != 15 || !bytes.Equal(b[8:], []byte{3, 0, 0, 0, 1, 2, 3}) {
		t.Fatalf("PutBytes = %x", b)
	}
	dst := make([]byte, 8)
	n, e, err := GetBytesInto(dst, b, 8)
	if err != nil || n != 3 || e != 15 || !bytes.Equal(dst[:3], []byte{1, 2, 3}) {
		t.Fatalf("GetBytesInto = %d, %d, %v", n, e, err)
	}
	if _, _, err := GetBytesInto(make([]byte, 3), b, 8); err != nil {
		t.Errorf("length equal to len(dst): %v", err)
	}
	for name, tc := range map[string]struct {
		dst []byte
		b   []byte
		off int
	}{
		"above len(dst)":   {make([]byte, 2), b, 8},
		"truncated data":   {dst, b[:14], 8},
		"truncated length": {dst, b[:10], 8},
		"NULL_LEN":         {dst, []byte{0xff, 0xff, 0xff, 0xff}, 0},
		"huge":             {dst, []byte{0xfe, 0xff, 0xff, 0x7f, 1}, 0},
		"offset past end":  {dst, b, 16},
	} {
		if _, _, err := GetBytesInto(tc.dst, tc.b, tc.off); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// Nothing is copied on failure.
	d := []byte{9, 9}
	_, _, _ = GetBytesInto(d, b, 8)
	if d[0] != 9 || d[1] != 9 {
		t.Error("dst written although the length is above len(dst)")
	}
}

func TestGetCount(t *testing.T) {
	body := func(count uint32, extra int) []byte {
		b := make([]byte, 4+extra)
		le.PutUint32(b, count)
		return b
	}
	if c, e, err := GetCount(body(3, 12), 0, 256, 4); err != nil || c != 3 || e != 4 {
		t.Fatalf("GetCount = %d, %d, %v", c, e, err)
	}
	for name, tc := range map[string]struct {
		b       []byte
		limit   int
		minsize int
	}{
		"count times minsize above remaining": {body(4, 12), 256, 4},
		"count above limit":                   {body(257, 2000), 256, 4},
		"NULL_LEN":                            {body(0xFFFFFFFF, 100), 0, 4},
		"overflowing product":                 {body(0xFFFFFFFE, 100), 0, 1 << 30},
		"count truncated":                     {[]byte{1, 0, 0}, 0, 1},
	} {
		if _, _, err := GetCount(tc.b, 0, tc.limit, tc.minsize); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, _, err := GetCount(body(256, 1024), 0, 256, 4); err != nil {
		t.Errorf("count exactly at the limit: %v", err)
	}
	// Alignment: an array that starts after a 5-byte prefix.
	b := append(make([]byte, 5), 0, 0, 0)
	b = append(b, 2, 0, 0, 0, 1, 1)
	if c, e, err := GetCount(b, 5, 0, 1); err != nil || c != 2 || e != 12 {
		t.Errorf("aligned GetCount = %d, %d, %v", c, e, err)
	}
	// A zero minsize cannot disable the plausibility check.
	if _, _, err := GetCount(body(1000, 4), 0, 0, 0); err == nil {
		t.Error("minsize 0 accepted an implausible count")
	}
}

// TestCellsDecoder runs the union decoder of the stubs over the body of vector
// R8 and over mutations of it.
func TestCellsDecoder(t *testing.T) {
	vecs, _ := loadVectors(t)
	resp := vecs.get(t, "R8_RESP")
	body := resp[HeaderSize:]
	c := bareConn(8, 16384, 1<<20)
	decode := func(b []byte) ([]cell, error) {
		d := Dec{Body: b, c: c}
		return decodeCells(&d, nil)
	}
	cells, err := decode(body)
	if err != nil || len(cells) != 3 || cells[0].Int != -2 || cells[1].Text != "hi" || cells[2].Tag != cellNull {
		t.Fatalf("decode = %+v, %v", cells, err)
	}
	mutations := map[string]func(b []byte) []byte{
		"trailing byte":       func(b []byte) []byte { return append(b, 0) },
		"truncated":           func(b []byte) []byte { return b[:len(b)-1] },
		"count too large":     func(b []byte) []byte { b[0] = 0xff; return b },
		"count above bound":   func(b []byte) []byte { b[0], b[1] = 0, 2; return b },
		"unknown tag":         func(b []byte) []byte { b[4] = 9; return b },
		"tag zero":            func(b []byte) []byte { b[4] = 0; return b },
		"text without NUL":    func(b []byte) []byte { b[26] = 'x'; return b },
		"text longer than 16": func(b []byte) []byte { b[20] = 17; return b },
		"text with NUL":       func(b []byte) []byte { b[24] = 0; return b },
		"empty":               func(b []byte) []byte { return nil },
	}
	for name, mut := range mutations {
		if cells, err := decode(mut(bytes.Clone(body))); err == nil {
			t.Errorf("%s: accepted %+v", name, cells)
		}
	}
}

func FuzzDecodeValues(f *testing.F) {
	vecs, _ := loadVectors(f)
	f.Add(byte(0), byte(16), []byte{3, 0, 0, 0, 'a', 'b', 'c', 0})
	f.Add(byte(1), byte(8), []byte{3, 0, 0, 0, 1, 2, 3})
	f.Add(byte(2), byte(0), []byte{0, 0, 0, 0})
	if b, ok := vecs["R8_RESP"]; ok {
		f.Add(byte(2), byte(0), b[HeaderSize:])
	}
	if b, ok := vecs["R2_RESP"]; ok {
		f.Add(byte(0), byte(0), b[HeaderSize+16:])
	}
	f.Fuzz(func(t *testing.T, which, param byte, body []byte) {
		off := int(param) % 32
		switch which % 4 {
		case 0:
			limit := int(param) % 20
			s, end, err := GetStr(body, off, limit)
			if err != nil {
				return
			}
			start := Align(off, 4)
			if end > len(body) || end != start+StrWire(len(s)) {
				t.Fatalf("GetStr end %d for %q", end, s)
			}
			if CheckStr(s, limit) != nil || body[end-1] != 0 {
				t.Fatalf("GetStr accepted %q", s)
			}
			re := make([]byte, StrWire(len(s)))
			PutStr(re, 0, s)
			if !bytes.Equal(re, body[start:end]) {
				t.Fatal("re-encoding differs")
			}
		case 1:
			dst := make([]byte, int(param)%40)
			n, end, err := GetBytesInto(dst, body, off)
			if err != nil {
				return
			}
			start := Align(off, 4)
			if n > len(dst) || end != start+BytesWire(n) || end > len(body) || !bytes.Equal(dst[:n], body[start+4:end]) {
				t.Fatalf("GetBytesInto = %d, %d", n, end)
			}
		case 2:
			c := bareConn(8, 16384, 1<<20)
			d := Dec{Body: body, c: c}
			cells, err := decodeCells(&d, nil)
			if err != nil {
				var pe *ProtocolError
				if !errors.As(err, &pe) {
					t.Fatalf("error %v", err)
				}
				return
			}
			if len(cells) > 256 {
				t.Fatal("more cells than the bound")
			}
			for _, cl := range cells {
				if cl.Tag < cellInt || cl.Tag > cellNull || len(cl.Text) > 16 || strings.IndexByte(cl.Text, 0) >= 0 {
					t.Fatalf("bad cell %+v", cl)
				}
			}
		case 3:
			count, end, err := GetCount(body, off, int(param)%64, 1+int(param)%8)
			if err != nil {
				return
			}
			if end > len(body) || uint64(count)*uint64(1+int(param)%8) > uint64(len(body)-end) {
				t.Fatalf("GetCount = %d, %d", count, end)
			}
		}
	})
}
