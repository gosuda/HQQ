package zstd_test

import (
	"bytes"
	"fmt"
	"testing"

	"gosuda.org/hqq/bindgen/examples/zstd"
	"gosuda.org/hqq/bindgen/rt"
)

// reference is the library called in process through cgo. It is nil unless the tests are built with
// -tags zstdcgo (cgo_test.go sets it); tests that compare against it then run their comparison.
type reference struct {
	VersionNumber func() uint32
	Compress      func(dst, src []byte, level int) ([]byte, error)
	Decompress    func(dst, src []byte) ([]byte, error)
	CompressBound func(n uint64) (uint64, error)
	FrameSize     func(frame []byte) uint64
}

var ref *reference

// compressAll compresses src into a buffer of the documented bound.
func compressAll(t testing.TB, c *zstd.Client, src []byte, level int32) []byte {
	t.Helper()
	dst := make([]byte, zstd.CompressBound(uint64(len(src))))
	frame, err := c.Compress(dst, src, level)
	if err != nil {
		t.Fatalf("Compress(%d bytes, level %d): %v", len(src), level, err)
	}
	return frame
}

// frameHeader is the prefix of a frame that ZSTD_getFrameContentSize reads.
func frameHeader(frame []byte) []byte {
	return frame[:min(len(frame), int(zstd.ZSTD_FRAMEHEADER_MAX))]
}

// roundTrip compresses src at level through the server, checks the frame and decompresses it again.
func roundTrip(t *testing.T, c *zstd.Client, src []byte, level int32) []byte {
	t.Helper()
	frame := compressAll(t, c, src, level)
	if bound := zstd.CompressBound(uint64(len(src))); uint64(len(frame)) > bound {
		t.Fatalf("frame is %d bytes, above the bound %d", len(frame), bound)
	}
	size, err := c.GetFrameContentSize(frameHeader(frame))
	if err != nil || size != uint64(len(src)) {
		t.Fatalf("GetFrameContentSize = %d, %v; want %d", size, err, len(src))
	}
	out, err := c.Decompress(make([]byte, len(src)), frame)
	if err != nil {
		t.Fatalf("Decompress(%d byte frame): %v", len(frame), err)
	}
	if !bytes.Equal(out, src) {
		t.Fatalf("decompressed %d bytes differ from the %d byte source", len(out), len(src))
	}
	return frame
}

// compareReference checks that the in-process library produces the same frame and decodes the
// server's frame to the same bytes.
func compareReference(t *testing.T, src, frame []byte, level int) {
	t.Helper()
	if ref == nil {
		return
	}
	want, err := ref.Compress(make([]byte, zstd.CompressBound(uint64(len(src)))), src, level)
	if err != nil {
		t.Fatalf("reference Compress: %v", err)
	}
	if !bytes.Equal(frame, want) {
		t.Fatalf("server frame (%d bytes) differs from the in-process frame (%d bytes)", len(frame), len(want))
	}
	got, err := ref.Decompress(make([]byte, len(src)), frame)
	if err != nil || !bytes.Equal(got, src) {
		t.Fatalf("reference Decompress of the server frame: %v, equal %v", err, bytes.Equal(got, src))
	}
}

// TestRoundTripSizesAndLevels covers the boundaries of the size classes: empty, below and at the 64
// byte cache line, a single-frame request (4 KiB), a multi-frame one (1 MiB) and a CONT-frame
// message of 64 MiB in both directions. Level 19 is skipped at 64 MiB, where it takes minutes.
func TestRoundTripSizesAndLevels(t *testing.T) {
	c := newClient(t)
	for _, size := range []int{0, 1, 63, 64, 4 << 10, 1 << 20, 64 << 20} {
		src := textData(size, uint64(size))
		for _, level := range []int32{1, 3, 19} {
			if size == 64<<20 && level == 19 {
				continue
			}
			t.Run(fmt.Sprintf("text/size=%d/level=%d", size, level), func(t *testing.T) {
				frame := roundTrip(t, c, src, level)
				compareReference(t, src, frame, int(level))
				if size >= 4<<10 && len(frame) >= size {
					t.Errorf("text of %d bytes did not compress: %d bytes", size, len(frame))
				}
			})
		}
	}
}

// TestRoundTripIncompressible sends data that zstd stores in raw blocks, so the response is as
// large as the bound allows: 64 MiB of it crosses the ring as about 1,000 CONT frames.
func TestRoundTripIncompressible(t *testing.T) {
	c := newClient(t)
	for _, tc := range []struct {
		size  int
		level int32
	}{{63, 3}, {4 << 10, 3}, {1 << 20, 3}, {64 << 20, 1}} {
		src := randomData(tc.size, uint64(tc.size))
		t.Run(fmt.Sprintf("size=%d/level=%d", tc.size, tc.level), func(t *testing.T) {
			frame := roundTrip(t, c, src, tc.level)
			if len(frame) < tc.size {
				t.Errorf("random data shrank from %d to %d bytes", tc.size, len(frame))
			}
			compareReference(t, src, frame, int(tc.level))
		})
	}
}

// TestDecompressExactAndOversizedDestination checks that the result is the prefix that was
// produced, not the capacity: a destination larger than the content comes back trimmed.
func TestDecompressExactAndOversizedDestination(t *testing.T) {
	c := newClient(t)
	src := textData(10_000, 3)
	frame := compressAll(t, c, src, 3)
	for _, extra := range []int{0, 1, 5000, 3 << 20} {
		out, err := c.Decompress(make([]byte, len(src)+extra), frame)
		if err != nil || !bytes.Equal(out, src) {
			t.Fatalf("destination %d bytes larger than the content: %d bytes, %v", extra, len(out), err)
		}
	}
}

// TestCompressBoundMatchesLibrary checks the local formula against the server and, when built with
// cgo, against the in-process library, around the 128 KiB step of the macro.
func TestCompressBoundMatchesLibrary(t *testing.T) {
	c := newClient(t)
	for _, n := range []uint64{0, 1, 255, 256, 1 << 10, 128<<10 - 1, 128 << 10, 128<<10 + 1, 1 << 20, 64 << 20, 1 << 40} {
		got, err := c.CompressBound(n)
		if err != nil {
			t.Fatalf("CompressBound(%d): %v", n, err)
		}
		if want := zstd.CompressBound(n); got != want {
			t.Errorf("server ZSTD_compressBound(%d) = %d, local formula = %d", n, got, want)
		}
		if ref != nil {
			if want, err := ref.CompressBound(n); err != nil || got != want {
				t.Errorf("in-process ZSTD_compressBound(%d) = %d, %v; server = %d", n, want, err, got)
			}
		}
	}
}

// TestVersionMatchesPin checks that the server runs the release that third_party/zstd pins: the
// byte-for-byte comparisons of these tests are only meaningful against one release.
func TestVersionMatchesPin(t *testing.T) {
	c := newClient(t)
	num, err := c.VersionNumber()
	if err != nil {
		t.Fatal(err)
	}
	str, err := c.VersionString()
	if err != nil {
		t.Fatal(err)
	}
	pinned := pinnedVersion(t)
	var major, minor, patch int
	if _, err := fmt.Sscanf(pinned, "%d.%d.%d", &major, &minor, &patch); err != nil {
		t.Fatalf("pinned version %q: %v", pinned, err)
	}
	if want := uint32(major*10000 + minor*100 + patch); num != want || str != pinned {
		t.Errorf("server reports %d (%q), pin is %d (%q)", num, str, want, pinned)
	}
	if ref != nil && ref.VersionNumber() != num {
		t.Errorf("in-process library is %d, server is %d", ref.VersionNumber(), num)
	}
	lo, err := c.MinCLevel()
	if err != nil || lo >= 0 {
		t.Errorf("MinCLevel = %d, %v; want a negative level", lo, err)
	}
	hi, err := c.MaxCLevel()
	if err != nil || hi != 22 {
		t.Errorf("MaxCLevel = %d, %v; want 22", hi, err)
	}
}

// TestCopyRequestServerProducesTheSameFrames runs the server with --copy-request, which copies a
// single-frame request body into private memory before the library reads it (BINDGEN.md 7.2). The
// frames must equal the ones of the default server, which hands the library a pointer into the ring
// slot.
func TestCopyRequestServerProducesTheSameFrames(t *testing.T) {
	copying := ownServer(t, "--copy-request")
	cc := dialTo(t, copying.Listener())
	c := newClient(t)
	for _, size := range []int{0, 1, 4 << 10, 60_000, 65_000, 1 << 20} {
		src := textData(size, uint64(70+size))
		inPlace := compressAll(t, c, src, 3)
		copied := compressAll(t, cc, src, 3)
		if !bytes.Equal(inPlace, copied) {
			t.Errorf("%d bytes: --copy-request frame (%d bytes) differs from the in-place frame (%d bytes)", size, len(copied), len(inPlace))
		}
		out, err := cc.Decompress(make([]byte, size), inPlace)
		if err != nil || !bytes.Equal(out, src) {
			t.Errorf("%d bytes: --copy-request server did not decode the in-place frame: %v", size, err)
		}
	}
}

// TestSmallFramesAndFewBuffers runs a session with 4 KiB frames and 8 buffers, where a 20 KB response
// needs six frames and flow control lets only one such call be in flight, and a 1 MiB response needs
// more frames than the ring has buffers. Every call must still complete with the right bytes when
// six of them are issued before the first Wait.
func TestSmallFramesAndFewBuffers(t *testing.T) {
	srv := ownServer(t, "--buffer-size", "4096", "--buffer-count", "8")
	c := dialTo(t, srv.Listener(), func(o *rt.Options) { o.BufferSize, o.BufferCount = 4096, 8 })
	if lim := c.Conn().Limits(); lim.BufferSize != 4096 || lim.BufferCount != 8 {
		t.Fatalf("granted geometry %+v, want 4096 x 8", lim)
	}
	for _, size := range []int{20_000, 1 << 20} {
		const n = 6
		srcs := make([][]byte, n)
		futs := make([]zstd.CompressFuture, n)
		for i := range n {
			srcs[i] = textData(size, uint64(80+i))
			var err error
			if futs[i], err = c.CompressAsync(make([]byte, zstd.CompressBound(uint64(size))), srcs[i], 1); err != nil {
				t.Fatalf("%d bytes: issue %d: %v", size, i, err)
			}
		}
		for i := range n {
			frame, err := futs[i].Wait()
			if err != nil {
				t.Fatalf("%d bytes: future %d: %v", size, i, err)
			}
			out, err := c.Decompress(make([]byte, size), frame)
			if err != nil || !bytes.Equal(out, srcs[i]) {
				t.Fatalf("%d bytes: future %d decoded to %d bytes, %v", size, i, len(out), err)
			}
		}
	}
}
