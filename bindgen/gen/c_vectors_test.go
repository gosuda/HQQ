package gen

import (
	"bytes"
	"encoding/hex"
	"path/filepath"
	"strings"
	"testing"

	"gosuda.org/hqq/bindgen/idl"
)

// buildMicrolibDriver generates the glue of idlPath (microlib.idl or a variant), compiles it with the real
// microlib, the test hooks and the exchange driver, links the runtime, and returns the program.
func buildMicrolibDriver(t testing.TB, idlPath, source string) (string, []byte) {
	t.Helper()
	spec := parseSpec(t, idlPath)
	exe := buildMicrolibSpec(t, spec, source, genC(t, spec, source))
	d := spec.Digest()
	return exe, d[:]
}

// buildMicrolibSpec compiles the generated files of a microlib variant with the real microlib and the driver.
func buildMicrolibSpec(t testing.TB, spec *idl.Spec, source string, files map[string][]byte) string {
	t.Helper()
	dir := t.TempDir()
	writeFiles(t, dir, files)
	micro := filepath.Join(repoRoot(t), "bindgen", "testdata", "microlib")
	root := repoRoot(t)
	return buildProgram(t, dir,
		[]string{micro, cTestdata(t), filepath.Join(root, "c", "src")},
		[]string{"IFACE=" + spec.Service + "_iface"},
		[]string{filepath.Join(dir, spec.Service+"_server.c"), cTestdata(t, "microlib_hooks.c"), filepath.Join(micro, "microlib.c")},
		[]string{cTestdata(t, "exchange_driver.c")})
}

// TestCVectorSession replays the session of 4.14 (HELLO, requests 1 to 13, CLOSE) through the generated microlib glue
// and the runtime's hqqrpc_test_exchange, under ASan and UBSan where the compiler has them: every response must equal
// the vector byte for byte, and the session must end without a live handle, a failed destroy or a tombstone.
func TestCVectorSession(t *testing.T) {
	exe, _ := buildMicrolibDriver(t, "bindgen/testdata/microlib/microlib.idl", "microlib.idl")
	vec := loadVectors(t)["microlib"]

	var reqs [][]byte
	var want [][]byte
	var names []string
	var pending string
	for _, f := range vec.Frames {
		switch f.Dir {
		case "c2s":
			if f.Name == "HELLO_REPLY_DIGEST" {
				continue
			}
			reqs = append(reqs, f.bytes(t))
			names = append(names, f.Name)
			pending = f.Name
		case "s2c":
			if f.Name == "HELLO_REPLY_DIGEST" {
				continue
			}
			if len(want) != len(reqs)-1 {
				t.Fatalf("vector %s answers %s out of order", f.Name, pending)
			}
			want = append(want, f.bytes(t))
		}
	}
	if len(reqs) != 15 || len(want) != 15 {
		t.Fatalf("vector session has %d requests and %d responses, want 15 each", len(reqs), len(want))
	}
	s := &session{t: t, exe: exe}
	res, trailer := s.run(reqs...)
	for i := range res {
		if res[i].status != 0 {
			t.Errorf("%s: exchange status %d", names[i], res[i].status)
		}
		if !bytes.Equal(res[i].out, want[i]) {
			t.Errorf("%s: response\n  got  %x\n  want %x", names[i], res[i].out, want[i])
		}
	}
	if want := "end live=0 failures=0 tombstones=0"; trailer != want {
		t.Errorf("session end: %q, want %q", trailer, want)
	}
}

// TestCVectorDigestMismatch sends the HELLO of another interface: the reply carries the digest of the generated table.
func TestCVectorDigestMismatch(t *testing.T) {
	exe, _ := buildMicrolibDriver(t, "bindgen/testdata/microlib/microlib.idl", "microlib.idl")
	vec := loadVectors(t)["microlib"]
	var hello, want []byte
	for _, f := range vec.Frames {
		switch f.Name {
		case "HELLO":
			hello = f.bytes(t)
		case "HELLO_REPLY_DIGEST":
			want = f.bytes(t)
		}
	}
	hello[16+32] ^= 0xFF
	res, _ := (&session{t: t, exe: exe}).run(hello)
	if !bytes.Equal(res[0].out, want) {
		t.Errorf("reply\n  got  %x\n  want %x", res[0].out, want)
	}
}

// TestCVectorChunking sends the two-frame request of 4.14 at S = 128 and expects the two response frames, which the
// generated glue builds in scratch because RESP_BOUND is above one chunk.
func TestCVectorChunking(t *testing.T) {
	exe, digest := buildMicrolibDriver(t, "bindgen/testdata/microlib/microlib.idl", "microlib.idl")
	vec := loadVectors(t)
	var d [32]byte
	copy(d[:], digest)
	var reqs [][]byte
	var want []byte
	for _, f := range vec["chunking"].Frames {
		if f.Dir == "c2s" {
			reqs = append(reqs, f.bytes(t))
		} else {
			want = append(want, f.bytes(t)...)
		}
	}
	frames := append([][]byte{helloFrame(d, 128, 64)}, reqs...)
	res, trailer := (&session{t: t, exe: exe, args: []string{"128"}}).run(frames...)
	if res[0].status != 0 || len(res[0].out) != 16+64 || res[0].out[16+0] != 'H' {
		t.Fatalf("HELLO at S=128 failed: status %d out %x", res[0].status, res[0].out)
	}
	if len(res[1].out) != 0 {
		t.Errorf("first request frame answered: %x", res[1].out)
	}
	if !bytes.Equal(res[2].out, want) {
		t.Errorf("chunked response\n  got  %x\n  want %x", res[2].out, want)
	}
	if !strings.HasPrefix(trailer, "end live=0") {
		t.Errorf("trailer %q", trailer)
	}
}

// TestCVectorWrap checks the id wrap of 4.12 against the generated glue.
func TestCVectorWrap(t *testing.T) {
	exe, digest := buildMicrolibDriver(t, "bindgen/testdata/microlib/microlib.idl", "microlib.idl")
	var d [32]byte
	copy(d[:], digest)
	vec := loadVectors(t)["wrap"]
	var a, b []byte
	for _, f := range vec.Frames {
		switch f.Name {
		case "WRAP_A_REQ":
			a = f.bytes(t)
		case "WRAP_B_REQ":
			b = f.bytes(t)
		}
	}
	hello := helloFrame(d, 16384, 64)
	last := func(n uint32) []byte { return []byte("#last " + itoa(int(n))) }
	res, _ := (&session{t: t, exe: exe}).run(hello, last(0xFFFFFFFE), a, b)
	if got := hex.EncodeToString(res[2].out); !strings.HasSuffix(got, "03000000") || le32(res[2].out[4:]) != 0xFFFFFFFF {
		t.Errorf("WRAP_A response %x", res[2].out)
	}
	if got := hex.EncodeToString(res[3].out); !strings.HasSuffix(got, "07000000") || le32(res[3].out[4:]) != 1 {
		t.Errorf("WRAP_B response %x", res[3].out)
	}
}
