//go:build cgo && zstdcgo

package zstd_test

import (
	"bytes"
	"errors"
	"testing"

	"gosuda.org/hqq/bindgen/examples/zstd"
	"gosuda.org/hqq/bindgen/examples/zstd/internal/cgozstd"
	"gosuda.org/hqq/bindgen/rt"
)

func init() {
	ref = &reference{
		VersionNumber: cgozstd.VersionNumber,
		Compress: func(dst, src []byte, level int) ([]byte, error) {
			return cgozstd.Compress(dst, src, level)
		},
		Decompress:    cgozstd.Decompress,
		CompressBound: cgozstd.CompressBound,
		FrameSize:     cgozstd.GetFrameContentSize,
	}
}

// sameFailure checks that the server's failure carries the code and the message that the in-process
// library produced for the same call.
func sameFailure(t *testing.T, name string, served, inproc error) {
	t.Helper()
	var le *rt.LibError
	var ce *cgozstd.Error
	if !errors.As(served, &le) || !errors.As(inproc, &ce) {
		t.Errorf("%s: server error %v (%T), in-process error %v (%T); want a failure from both", name, served, served, inproc, inproc)
		return
	}
	if code, _ := zstd.ErrorCodeOf(served); int(code) != ce.Code || le.Detail != ce.Text {
		t.Errorf("%s: server reports code %d %q, in-process library code %d %q", name, code, le.Detail, ce.Code, ce.Text)
	}
}

func TestErrorTextMatchesInProcessLibrary(t *testing.T) {
	c := newClient(t)
	src := textData(4<<10, 41)
	frame := compressAll(t, c, src, 3)

	bad := bytes.Clone(frame)
	bad[0] ^= 0xff
	_, served := c.Decompress(make([]byte, len(src)), bad)
	_, inproc := cgozstd.Decompress(make([]byte, len(src)), bad)
	sameFailure(t, "wrong magic", served, inproc)

	_, served = c.Decompress(make([]byte, len(src)), frame[:len(frame)/2])
	_, inproc = cgozstd.Decompress(make([]byte, len(src)), frame[:len(frame)/2])
	sameFailure(t, "truncated frame", served, inproc)

	_, served = c.Decompress(make([]byte, len(src)-1), frame)
	_, inproc = cgozstd.Decompress(make([]byte, len(src)-1), frame)
	sameFailure(t, "destination too small (decompress)", served, inproc)

	_, served = c.Compress(make([]byte, 10), src, 3)
	_, inproc = cgozstd.Compress(make([]byte, 10), src, 3)
	sameFailure(t, "destination too small (compress)", served, inproc)

	_, served = c.CompressBound(0xFF00FF00FF00FF00)
	_, inproc = cgozstd.CompressBound(0xFF00FF00FF00FF00)
	sameFailure(t, "compress bound of an impossible size", served, inproc)

	cctx := mustCCtx(t, c)
	cc, err := cgozstd.NewCCtx()
	if err != nil {
		t.Fatal(err)
	}
	_, served = c.CCtxSetParameter(cctx, zstd.ZSTD_c_windowLog, 9)
	inproc = cc.SetParameter(int(zstd.ZSTD_c_windowLog), 9)
	sameFailure(t, "window log out of bounds", served, inproc)
	if err := cc.Close(); err != nil {
		t.Fatal(err)
	}
	if err := c.FreeCCtx(cctx); err != nil {
		t.Fatal(err)
	}
}

func TestContextCompressMatchesInProcessLibrary(t *testing.T) {
	c := newClient(t)
	cctx, dctx := mustCCtx(t, c), mustDCtx(t, c)
	cc, err := cgozstd.NewCCtx()
	if err != nil {
		t.Fatal(err)
	}
	dc, err := cgozstd.NewDCtx()
	if err != nil {
		t.Fatal(err)
	}
	for i, size := range []int{0, 63, 5000, 200_000, 3 << 20} {
		src := textData(size, uint64(50+i))
		bound := zstd.CompressBound(uint64(size))
		got, err := c.CompressCCtx(cctx, make([]byte, bound), src, 5)
		if err != nil {
			t.Fatal(err)
		}
		want, err := cc.Compress(make([]byte, bound), src, 5)
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("%d bytes: server frame %d bytes, in-process %d bytes, %v", size, len(got), len(want), err)
		}
		out, err := dc.Decompress(make([]byte, size), got)
		if err != nil || !bytes.Equal(out, src) {
			t.Fatalf("%d bytes: in-process DCtx cannot decode the server frame: %v", size, err)
		}
		if size > 0 {
			served, err := c.DecompressDCtx(dctx, make([]byte, size), want)
			if err != nil || !bytes.Equal(served, src) {
				t.Fatalf("%d bytes: server DCtx cannot decode the in-process frame: %v", size, err)
			}
		}
	}
	if err := cc.Close(); err != nil {
		t.Fatal(err)
	}
	if err := dc.Close(); err != nil {
		t.Fatal(err)
	}
}

// TestStreamFramesMatchInProcessLibrary compresses the same chunks, with the same destination
// capacity, through the server (four calls in flight) and through cgo, and requires equal bytes. Then
// each side decodes the other's frame with the streaming call and the same output buffer.
func TestStreamFramesMatchInProcessLibrary(t *testing.T) {
	c := newClient(t)
	src := textData(3<<20, 61)
	chunks := splitChunks(src, 1, 100_000, 7, 4096, 700_000, 65_536, 65_537)
	capacity := int(zstd.CompressBound(uint64(maxLen(chunks)))) + 64

	cctx, dctx := mustCCtx(t, c), mustDCtx(t, c)
	if _, err := c.CCtxSetParameter(cctx, zstd.ZSTD_c_compressionLevel, 3); err != nil {
		t.Fatal(err)
	}
	var served bytes.Buffer
	if err := compressChunks(c, cctx, chunks, 4, func(p []byte) error { served.Write(p); return nil }); err != nil {
		t.Fatal(err)
	}

	cc, err := cgozstd.NewCCtx()
	if err != nil {
		t.Fatal(err)
	}
	if err := cc.SetParameter(int(zstd.ZSTD_c_compressionLevel), 3); err != nil {
		t.Fatal(err)
	}
	var inproc bytes.Buffer
	dst := make([]byte, capacity)
	for i, chunk := range chunks {
		op := zstd.ZSTD_e_continue
		if i == len(chunks)-1 {
			op = zstd.ZSTD_e_end
		}
		_, out, used, err := cc.CompressStream(dst, chunk, op)
		if err != nil || used != uint64(len(chunk)) {
			t.Fatalf("in-process chunk %d: consumed %d of %d, %v", i, used, len(chunk), err)
		}
		inproc.Write(out)
	}
	if !bytes.Equal(served.Bytes(), inproc.Bytes()) {
		t.Fatalf("server stream frame (%d bytes) differs from the in-process one (%d bytes)", served.Len(), inproc.Len())
	}

	dc, err := cgozstd.NewDCtx()
	if err != nil {
		t.Fatal(err)
	}
	var decoded bytes.Buffer
	in := served.Bytes()
	buf := make([]byte, 128<<10)
	for len(in) > 0 {
		_, out, used, err := dc.DecompressStream(buf, in)
		if err != nil {
			t.Fatal(err)
		}
		decoded.Write(out)
		in = in[used:]
	}
	if !bytes.Equal(decoded.Bytes(), src) {
		t.Fatal("the in-process streaming decoder did not reproduce the source from the server frame")
	}
	var back bytes.Buffer
	hint, err := decompressChunk(c, dctx, buf, inproc.Bytes(), func(p []byte) error { back.Write(p); return nil })
	if err != nil || hint != 0 || !bytes.Equal(back.Bytes(), src) {
		t.Fatalf("the server decoder on the in-process frame: hint %d, %d bytes, %v", hint, back.Len(), err)
	}
	for _, closer := range []func() error{cc.Close, dc.Close} {
		if err := closer(); err != nil {
			t.Fatal(err)
		}
	}
}
