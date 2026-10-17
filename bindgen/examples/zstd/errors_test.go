package zstd_test

import (
	"bytes"
	"errors"
	"testing"

	"gosuda.org/hqq/bindgen/examples/zstd"
	"gosuda.org/hqq/bindgen/rt"
)

// wantLibError checks that err is a library failure with this ZSTD_ErrorCode and this message from
// ZSTD_getErrorName, which the server sends as the error detail.
func wantLibError(t testing.TB, err error, code zstd.ErrorCode, detail string) {
	t.Helper()
	var le *rt.LibError
	if !errors.As(err, &le) {
		t.Fatalf("error = %v (%T), want a *rt.LibError with detail %q", err, err, detail)
	}
	if got, ok := zstd.ErrorCodeOf(err); !ok || got != code {
		t.Errorf("ZSTD_ErrorCode = %d (%v), want %d; error: %v", got, ok, code, err)
	}
	if le.Detail != detail {
		t.Errorf("detail = %q, want %q", le.Detail, detail)
	}
}

// stillWorks proves that a failed call left the session usable.
func stillWorks(t testing.TB, c *zstd.Client) {
	t.Helper()
	src := textData(1000, 99)
	frame := compressAll(t, c, src, 3)
	out, err := c.Decompress(make([]byte, len(src)), frame)
	if err != nil || !bytes.Equal(out, src) {
		t.Fatalf("session unusable after a library failure: %v", err)
	}
}

// checksummedFrame compresses src with the frame checksum on, which needs the streaming call:
// ZSTD_compress and ZSTD_compressCCtx ignore advanced parameters.
func checksummedFrame(t testing.TB, c *zstd.Client, src []byte) []byte {
	t.Helper()
	cctx, err := c.CreateCCtx()
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := c.FreeCCtx(cctx); err != nil {
			t.Error(err)
		}
	}()
	if _, err := c.CCtxSetParameter(cctx, zstd.ZSTD_c_checksumFlag, 1); err != nil {
		t.Fatal(err)
	}
	dst := make([]byte, zstd.CompressBound(uint64(len(src)))+8)
	hint, frame, used, err := c.CompressStream2Flat(cctx, dst, src, zstd.ZSTD_e_end)
	if err != nil || hint != 0 || used != uint64(len(src)) {
		t.Fatalf("CompressStream2Flat: hint %d, consumed %d of %d, %v", hint, used, len(src), err)
	}
	return frame
}

func TestDecompressRejectsInputThatIsNotAFrame(t *testing.T) {
	c := newClient(t)
	frame := compressAll(t, c, textData(4<<10, 1), 3)
	bad := bytes.Clone(frame)
	bad[0] ^= 0xff
	_, err := c.Decompress(make([]byte, 4<<10), bad)
	wantLibError(t, err, zstd.ErrPrefixUnknown, "Unknown frame descriptor")
	stillWorks(t, c)
}

func TestDecompressRejectsTruncatedFrame(t *testing.T) {
	c := newClient(t)
	frame := compressAll(t, c, textData(4<<10, 1), 3)
	for _, cut := range []int{1, 3, 5, 10, len(frame) / 2, len(frame) - 1} {
		_, err := c.Decompress(make([]byte, 4<<10), frame[:cut])
		wantLibError(t, err, zstd.ErrSrcSizeWrong, "Src size is incorrect")
	}
	// No input is no frame, and the library takes that for an empty result, not for an error.
	out, err := c.Decompress(make([]byte, 16), nil)
	if err != nil || len(out) != 0 {
		t.Errorf("Decompress of empty input = %d bytes, %v; want 0 bytes and no error", len(out), err)
	}
	stillWorks(t, c)
}

func TestDecompressDetectsChecksumMismatch(t *testing.T) {
	c := newClient(t)
	src := textData(10<<10, 2)
	frame := checksummedFrame(t, c, src)
	out, err := c.Decompress(make([]byte, len(src)), frame)
	if err != nil || !bytes.Equal(out, src) {
		t.Fatalf("intact checksummed frame: %d bytes, %v", len(out), err)
	}
	bad := bytes.Clone(frame)
	bad[len(bad)-1] ^= 0x01
	_, err = c.Decompress(make([]byte, len(src)), bad)
	wantLibError(t, err, zstd.ErrChecksumWrong, "Restored data doesn't match checksum")
	stillWorks(t, c)
}

func TestDecompressDetectsCorruptBlockHeader(t *testing.T) {
	c := newClient(t)
	src := randomData(1000, 5)
	frame := compressAll(t, c, src, 3)
	// A single-segment frame of 1000 bytes has a 4 byte magic, a descriptor byte and a 2 byte
	// content size, so the first block header starts at offset 7; block type 3 is reserved.
	bad := bytes.Clone(frame)
	bad[7] |= 0x06
	_, err := c.Decompress(make([]byte, len(src)), bad)
	wantLibError(t, err, zstd.ErrCorruptionDetected, "Data corruption detected")
	stillWorks(t, c)
}

func TestDestinationTooSmall(t *testing.T) {
	c := newClient(t)
	src := textData(4<<10, 4)
	_, err := c.Compress(make([]byte, 10), src, 3)
	wantLibError(t, err, zstd.ErrDstSizeTooSmall, "Destination buffer is too small")
	_, err = c.Compress(nil, src, 3)
	wantLibError(t, err, zstd.ErrDstSizeTooSmall, "Destination buffer is too small")
	stillWorks(t, c)

	frame := compressAll(t, c, src, 3)
	_, err = c.Decompress(make([]byte, len(src)-1), frame)
	wantLibError(t, err, zstd.ErrDstSizeTooSmall, "Destination buffer is too small")
	stillWorks(t, c)
}

func TestLevelsOutsideTheLibraryRangeAreClamped(t *testing.T) {
	c := newClient(t)
	lo, err := c.MinCLevel()
	if err != nil {
		t.Fatal(err)
	}
	hi, err := c.MaxCLevel()
	if err != nil {
		t.Fatal(err)
	}
	src := textData(4<<10, 6)
	for _, tc := range []struct {
		name        string
		level, same int32
	}{
		{"above the maximum", 1 << 30, hi},
		{"below the minimum", -1 << 30, lo},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := compressAll(t, c, src, tc.level)
			want := compressAll(t, c, src, tc.same)
			if !bytes.Equal(got, want) {
				t.Errorf("level %d produced a different frame than the clamped level %d", tc.level, tc.same)
			}
			if ref != nil {
				inproc, err := ref.Compress(make([]byte, zstd.CompressBound(uint64(len(src)))), src, int(tc.level))
				if err != nil || !bytes.Equal(got, inproc) {
					t.Errorf("in-process library at level %d: %v, equal %v", tc.level, err, bytes.Equal(got, inproc))
				}
			}
		})
	}
}

func TestSetParameterPolicyAndLibraryBounds(t *testing.T) {
	c := newClient(t)
	cctx, err := c.CreateCCtx()
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name         string
		param, value int32
	}{
		{"level", zstd.ZSTD_c_compressionLevel, 5},
		{"window log", zstd.ZSTD_c_windowLog, 20},
		{"window log at the policy limit", zstd.ZSTD_c_windowLog, 27},
		{"strategy", zstd.ZSTD_c_strategy, 3},
		{"content size flag", zstd.ZSTD_c_contentSizeFlag, 0},
		{"checksum flag", zstd.ZSTD_c_checksumFlag, 1},
		{"dictionary id flag", zstd.ZSTD_c_dictIDFlag, 0},
	} {
		if _, err := c.CCtxSetParameter(cctx, tc.param, tc.value); err != nil {
			t.Errorf("%s: %v", tc.name, err)
		}
	}
	for _, tc := range []struct {
		name         string
		param, value int32
	}{
		{"window log above the policy", zstd.ZSTD_c_windowLog, 28},
		{"worker threads", zstd.ZSTD_c_nbWorkers, 2},
		{"hash log, not exposed", 102, 30},
		{"unknown parameter", 9999, 1},
	} {
		_, err := c.CCtxSetParameter(cctx, tc.param, tc.value)
		if !errors.Is(err, rt.ErrDenied) {
			t.Errorf("%s: error = %v, want rt.ErrDenied", tc.name, err)
		}
	}
	for _, tc := range []struct {
		name         string
		param, value int32
	}{
		{"window log below the library minimum", zstd.ZSTD_c_windowLog, 9},
		{"strategy that does not exist", zstd.ZSTD_c_strategy, 99},
		{"negative strategy", zstd.ZSTD_c_strategy, -1},
	} {
		_, err := c.CCtxSetParameter(cctx, tc.param, tc.value)
		wantLibError(t, err, zstd.ErrParameterOutOfBound, "Parameter is out of bound")
	}
	if err := c.FreeCCtx(cctx); err != nil {
		t.Fatal(err)
	}
	stillWorks(t, c)
}

func TestParameterAfterCompressionStartedIsRefusedByTheLibrary(t *testing.T) {
	c := newClient(t)
	cctx, err := c.CreateCCtx()
	if err != nil {
		t.Fatal(err)
	}
	src := textData(100<<10, 7)
	dst := make([]byte, 1<<20)
	if _, _, _, err := c.CompressStream2Flat(cctx, dst, src[:1000], zstd.ZSTD_e_continue); err != nil {
		t.Fatal(err)
	}
	// The level can change mid-stream; the frame parameters cannot.
	_, err = c.CCtxSetParameter(cctx, zstd.ZSTD_c_checksumFlag, 1)
	wantLibError(t, err, zstd.ErrStageWrong, "Operation not authorized at current processing stage")
	if _, err := c.CCtxReset(cctx, zstd.ZSTD_reset_session_only); err != nil {
		t.Fatal(err)
	}
	if _, err := c.CCtxSetParameter(cctx, zstd.ZSTD_c_checksumFlag, 1); err != nil {
		t.Errorf("after a session reset: %v", err)
	}
	if err := c.FreeCCtx(cctx); err != nil {
		t.Fatal(err)
	}
}

func TestCompressBoundRefusesImpossibleSize(t *testing.T) {
	c := newClient(t)
	_, err := c.CompressBound(0xFF00FF00FF00FF00)
	wantLibError(t, err, zstd.ErrSrcSizeWrong, "Src size is incorrect")
	stillWorks(t, c)
}

func TestFrameContentSizeResultsAreNotFailures(t *testing.T) {
	c := newClient(t)
	src := textData(5000, 8)

	known := compressAll(t, c, src, 3)
	if n, err := c.GetFrameContentSize(frameHeader(known)); err != nil || n != uint64(len(src)) {
		t.Errorf("one-shot frame: %d, %v; want %d", n, err, len(src))
	}

	// A first ZSTD_e_continue call leaves the content size unknown.
	cctx, err := c.CreateCCtx()
	if err != nil {
		t.Fatal(err)
	}
	dst := make([]byte, zstd.CompressBound(uint64(len(src)))+64)
	_, part1, _, err := c.CompressStream2Flat(cctx, dst, src[:100], zstd.ZSTD_e_continue)
	if err != nil {
		t.Fatal(err)
	}
	_, part2, _, err := c.CompressStream2Flat(cctx, dst[len(part1):], src[100:], zstd.ZSTD_e_end)
	if err != nil {
		t.Fatal(err)
	}
	stream := dst[:len(part1)+len(part2)]
	if n, err := c.GetFrameContentSize(frameHeader(stream)); err != nil || n != zstd.ZSTD_CONTENTSIZE_UNKNOWN {
		t.Errorf("stream frame: %d, %v; want ZSTD_CONTENTSIZE_UNKNOWN", n, err)
	}
	if err := c.FreeCCtx(cctx); err != nil {
		t.Fatal(err)
	}

	if n, err := c.GetFrameContentSize(nil); err != nil || n != zstd.ZSTD_CONTENTSIZE_ERROR {
		t.Errorf("empty input: %d, %v; want ZSTD_CONTENTSIZE_ERROR", n, err)
	}
	if n, err := c.GetFrameContentSize([]byte("not a frame at all")); err != nil || n != zstd.ZSTD_CONTENTSIZE_ERROR {
		t.Errorf("garbage input: %d, %v; want ZSTD_CONTENTSIZE_ERROR", n, err)
	}
	if _, err := c.GetFrameContentSize(make([]byte, zstd.ZSTD_FRAMEHEADER_MAX+1)); !errors.Is(err, rt.ErrInvalidArg) {
		t.Errorf("19 byte input: error = %v, want rt.ErrInvalidArg (the declared bound is 18)", err)
	}
	stillWorks(t, c)
}

func TestStreamCallsRejectBadDirective(t *testing.T) {
	c := newClient(t)
	cctx, err := c.CreateCCtx()
	if err != nil {
		t.Fatal(err)
	}
	_, _, _, err = c.CompressStream2Flat(cctx, make([]byte, 1000), []byte("x"), 3)
	wantLibError(t, err, zstd.ErrParameterOutOfBound, "Parameter is out of bound")
	if err := c.FreeCCtx(cctx); err != nil {
		t.Fatal(err)
	}
	stillWorks(t, c)
}

// TestServerMessageLimitIsGrantedAndEnforced starts a server with a 1 MiB message limit and offers
// the client's 128 MiB: the session gets the server's value, and requests above it are refused by the
// client before they are committed (an argument error, known outcome).
func TestServerMessageLimitIsGrantedAndEnforced(t *testing.T) {
	srv := ownServer(t, "--max-message", "1048576")
	c := dialTo(t, srv.Listener())
	if got := c.Conn().Limits().MaxMessage; got != 1<<20 {
		t.Fatalf("granted max message = %d, want 1 MiB", got)
	}
	big := textData(2<<20, 9)
	_, err := c.Compress(make([]byte, zstd.CompressBound(uint64(len(big)))), big, 1)
	if !errors.Is(err, rt.ErrInvalidArg) || errors.Is(err, rt.ErrOutcomeUnknown) {
		t.Fatalf("2 MiB request against a 1 MiB limit: error = %v, want rt.ErrInvalidArg with a known outcome", err)
	}
	stillWorks(t, c)
	_, err = c.Compress(make([]byte, 2<<20), []byte("hello"), 1)
	if !errors.Is(err, rt.ErrInvalidArg) {
		t.Fatalf("2 MiB destination against a 1 MiB limit: error = %v, want rt.ErrInvalidArg", err)
	}
	stillWorks(t, c)
}
