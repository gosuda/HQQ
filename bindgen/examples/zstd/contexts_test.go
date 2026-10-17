package zstd_test

import (
	"bytes"
	"errors"
	"os"
	"testing"
	"time"

	"gosuda.org/hqq/bindgen/examples/zstd"
	"gosuda.org/hqq/bindgen/rt"
	"gosuda.org/hqq/launch"
)

func mustCCtx(t testing.TB, c *zstd.Client) zstd.CCtx {
	t.Helper()
	h, err := c.CreateCCtx()
	if err != nil {
		t.Fatalf("CreateCCtx: %v", err)
	}
	return h
}

func mustDCtx(t testing.TB, c *zstd.Client) zstd.DCtx {
	t.Helper()
	h, err := c.CreateDCtx()
	if err != nil {
		t.Fatalf("CreateDCtx: %v", err)
	}
	return h
}

// TestContextCallsMatchOneShot reuses one compression and one decompression context for inputs of
// different sizes and checks that ZSTD_compressCCtx produces the bytes of ZSTD_compress at the same
// level: reuse must leave no state behind.
func TestContextCallsMatchOneShot(t *testing.T) {
	c := newClient(t)
	cctx, dctx := mustCCtx(t, c), mustDCtx(t, c)
	for i, size := range []int{5000, 0, 70_000, 1, 300_000, 5000} {
		level := int32(1 + 2*i)
		src := textData(size, uint64(100+i))
		bound := make([]byte, zstd.CompressBound(uint64(size)))
		got, err := c.CompressCCtx(cctx, bound, src, level)
		if err != nil {
			t.Fatalf("CompressCCtx(%d bytes, level %d): %v", size, level, err)
		}
		if want := compressAll(t, c, src, level); !bytes.Equal(got, want) {
			t.Errorf("CompressCCtx(%d bytes, level %d) differs from one-shot Compress", size, level)
		}
		out, err := c.DecompressDCtx(dctx, make([]byte, size), got)
		if err != nil || !bytes.Equal(out, src) {
			t.Fatalf("DecompressDCtx(%d bytes): %d bytes, %v", size, len(out), err)
		}
	}
	if err := c.FreeCCtx(cctx); err != nil {
		t.Fatal(err)
	}
	if err := c.FreeDCtx(dctx); err != nil {
		t.Fatal(err)
	}
}

// TestParametersApplyToStreamsNotToCompressCCtx pins the library behavior that the IDL comments rely
// on: ZSTD_compressCCtx takes only the level, while the streaming call honors the parameters set on
// the context. The checksum adds four bytes to a frame.
func TestParametersApplyToStreamsNotToCompressCCtx(t *testing.T) {
	c := newClient(t)
	src := textData(20_000, 11)
	plain := compressAll(t, c, src, 3)

	cctx := mustCCtx(t, c)
	if _, err := c.CCtxSetParameter(cctx, zstd.ZSTD_c_checksumFlag, 1); err != nil {
		t.Fatal(err)
	}
	ignored, err := c.CompressCCtx(cctx, make([]byte, zstd.CompressBound(uint64(len(src)))), src, 3)
	if err != nil || !bytes.Equal(ignored, plain) {
		t.Errorf("ZSTD_compressCCtx honored a context parameter: %d bytes against %d, %v", len(ignored), len(plain), err)
	}
	if _, err := c.CCtxSetParameter(cctx, zstd.ZSTD_c_compressionLevel, 3); err != nil {
		t.Fatal(err)
	}
	hint, withSum, used, err := c.CompressStream2Flat(cctx, make([]byte, len(plain)+64), src, zstd.ZSTD_e_end)
	if err != nil || hint != 0 || used != uint64(len(src)) {
		t.Fatalf("stream with checksum: hint %d, consumed %d, %v", hint, used, err)
	}
	if len(withSum) != len(plain)+4 {
		t.Errorf("checksummed stream frame is %d bytes, want %d plus the 4 byte checksum", len(withSum), len(plain))
	}
	if err := c.FreeCCtx(cctx); err != nil {
		t.Fatal(err)
	}
}

func TestPledgedSizeIsCheckedByTheLibrary(t *testing.T) {
	c := newClient(t)
	cctx := mustCCtx(t, c)
	if _, err := c.CCtxSetPledgedSrcSize(cctx, 1000); err != nil {
		t.Fatal(err)
	}
	dst := make([]byte, 4096)
	// A lone ZSTD_e_end call replaces the pledge with its own input size, so the mismatch shows
	// only when the stream is closed after a ZSTD_e_continue call.
	if _, _, _, err := c.CompressStream2Flat(cctx, dst, textData(500, 12), zstd.ZSTD_e_continue); err != nil {
		t.Fatal(err)
	}
	_, _, _, err := c.CompressStream2Flat(cctx, dst, nil, zstd.ZSTD_e_end)
	wantLibError(t, err, zstd.ErrSrcSizeWrong, "Src size is incorrect")

	if _, err := c.CCtxReset(cctx, zstd.ZSTD_reset_session_and_parameters); err != nil {
		t.Fatal(err)
	}
	src := textData(1000, 13)
	if _, err := c.CCtxSetPledgedSrcSize(cctx, uint64(len(src))); err != nil {
		t.Fatal(err)
	}
	_, frame, _, err := c.CompressStream2Flat(cctx, dst, src, zstd.ZSTD_e_end)
	if err != nil {
		t.Fatal(err)
	}
	if n, err := c.GetFrameContentSize(frameHeader(frame)); err != nil || n != 1000 {
		t.Errorf("content size of the pledged frame = %d, %v; want 1000", n, err)
	}
	if err := c.FreeCCtx(cctx); err != nil {
		t.Fatal(err)
	}
}

func TestHandleLifecycle(t *testing.T) {
	c := newClient(t)
	src := textData(2000, 14)
	dst := make([]byte, zstd.CompressBound(uint64(len(src))))

	cctx := mustCCtx(t, c)
	if _, err := c.CompressCCtx(cctx, dst, src, 3); err != nil {
		t.Fatal(err)
	}
	if err := c.FreeCCtx(cctx); err != nil {
		t.Fatal(err)
	}

	t.Run("use after free", func(t *testing.T) {
		_, err := c.CompressCCtx(cctx, dst, src, 3)
		if !errors.Is(err, rt.ErrBadHandle) {
			t.Errorf("error = %v, want rt.ErrBadHandle", err)
		}
		stillWorks(t, c)
	})
	t.Run("double free", func(t *testing.T) {
		if err := c.FreeCCtx(cctx); !errors.Is(err, rt.ErrBadHandle) {
			t.Errorf("error = %v, want rt.ErrBadHandle", err)
		}
		stillWorks(t, c)
	})
	t.Run("recycled index gets a new generation", func(t *testing.T) {
		again := mustCCtx(t, c)
		if again.ID() == cctx.ID() {
			t.Errorf("a new context has the id %#x of a freed one", again.ID())
		}
		if _, err := c.CompressCCtx(cctx, dst, src, 3); !errors.Is(err, rt.ErrBadHandle) {
			t.Errorf("stale handle after index reuse: error = %v, want rt.ErrBadHandle", err)
		}
		if err := c.FreeCCtx(again); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("nil handle", func(t *testing.T) {
		if _, err := c.CompressCCtx(zstd.CCtx{}, dst, src, 3); err == nil {
			t.Error("the nil handle was accepted")
		}
		stillWorks(t, c)
	})
	t.Run("handle of another session", func(t *testing.T) {
		other := newClient(t)
		theirs := mustCCtx(t, other)
		if _, err := c.CompressCCtx(theirs, dst, src, 3); !errors.Is(err, rt.ErrForeignHandle) {
			t.Errorf("error = %v, want rt.ErrForeignHandle", err)
		}
		if err := other.FreeCCtx(theirs); err != nil {
			t.Fatal(err)
		}
	})
}

func TestHandleTableLimit(t *testing.T) {
	srv := ownServer(t, "--max-handles", "4")
	c := dialTo(t, srv.Listener())
	var held []zstd.DCtx
	for range 4 {
		held = append(held, mustDCtx(t, c))
	}
	_, err := c.CreateDCtx()
	if !errors.Is(err, rt.ErrLimit) || errors.Is(err, rt.ErrOutcomeUnknown) {
		t.Fatalf("fifth context: error = %v, want rt.ErrLimit with a known outcome", err)
	}
	stillWorks(t, c)
	if err := c.FreeDCtx(held[0]); err != nil {
		t.Fatal(err)
	}
	held[0] = mustDCtx(t, c)
	for _, h := range held {
		if err := c.FreeDCtx(h); err != nil {
			t.Fatal(err)
		}
	}
}

// growBy runs the same work in a fresh session and returns how much the resident size of the server
// grew: six contexts, each of which holds a level 19 workspace of several MiB until it is freed. It
// returns the session and its contexts so that the caller decides how the session ends.
func growBy(t *testing.T, srv *launch.Server) (*zstd.Client, []zstd.CCtx, int64) {
	t.Helper()
	c := dialTo(t, srv.Listener())
	stillWorks(t, c)
	before := rssKiB(t, srv.PID())
	src := textData(1<<20, 15)
	dst := make([]byte, zstd.CompressBound(uint64(len(src))))
	var held []zstd.CCtx
	for range 6 {
		h := mustCCtx(t, c)
		if _, err := c.CompressCCtx(h, dst, src, 19); err != nil {
			t.Fatal(err)
		}
		held = append(held, h)
	}
	return c, held, rssKiB(t, srv.PID()) - before
}

// TestContextsAreReleasedWhenTheirOwnerGoes shows that the server frees the library objects of a
// session that ends without freeing them: after an orderly Close, and after the client vanished (the
// Kill of the client stands in for a crash: no CLOSE frame is sent).
//
// The resident size of the server is the observable, and allocators differ in how freed memory
// shows in it: glibc and the macOS allocator keep freed blocks and reuse them, AddressSanitizer
// unmaps large ones. Steps: session A grows the server by G; session B, with A still alive, grows it
// by about G again (the control: live contexts neither shrink nor feed the allocator); A ends in the
// way under test and the server logs "session ended" (printed after the server destroyed the handles
// of A). The contexts count as freed if the resident size fell by at least G/2 at that point, or if
// session C then grows the server by at most G/3 (reuse). An explicit free of the contexts is the
// third scenario and shows that one of the two effects is visible on the platform; if neither is,
// the other scenarios are skipped.
func TestContextsAreReleasedWhenTheirOwnerGoes(t *testing.T) {
	scenarios := []struct {
		name        string
		endsSession bool
		end         func(c *zstd.Client, held []zstd.CCtx)
	}{
		{"explicit free", false, func(c *zstd.Client, held []zstd.CCtx) {
			for _, h := range held {
				if err := c.FreeCCtx(h); err != nil {
					t.Error(err)
				}
			}
		}},
		{"orderly close", true, func(c *zstd.Client, _ []zstd.CCtx) {
			if err := c.Close(); err != nil {
				t.Error(err)
			}
		}},
		{"killed client", true, func(c *zstd.Client, _ []zstd.CCtx) { killClient(c) }},
	}
	measurable := true
	for _, sc := range scenarios {
		t.Run(sc.name, func(t *testing.T) {
			if !measurable {
				t.Skip("freed memory neither leaves the process nor is reused by the next session on this platform, so the resident size cannot show a leak")
			}
			srv, logs := ownServerTo(t, os.Stderr, reusableFree)
			a, held, gA := growBy(t, srv)
			if gA < 20<<10 {
				t.Fatalf("six level 19 contexts grew the server by %d KiB; the measurement needs at least 20 MiB", gA)
			}
			_, _, gB := growBy(t, srv)
			if gB < gA/2 {
				t.Fatalf("control failed: a second session grew the server by %d KiB against %d KiB for the first, although the first still holds its contexts", gB, gA)
			}
			ended := logs.count("session ended")
			rssBefore := rssKiB(t, srv.PID())
			sc.end(a, held)
			if sc.endsSession && !logs.waitCount("session ended", ended+1, 10*time.Second) {
				t.Fatalf("the server did not log the end of the session after %s", sc.name)
			}
			if fell := rssBefore - rssKiB(t, srv.PID()); fell >= gA/2 {
				return
			}
			_, _, gC := growBy(t, srv)
			if gC > gA/3 {
				if sc.name == "explicit free" {
					measurable = false
					t.Skipf("after explicit frees the resident size did not fall by %d KiB and a new session grew the server by %d KiB", gA/2, gC)
				}
				t.Fatalf("after the end of a session that held %d KiB the resident size did not fall by half, and a new session grew the server by %d KiB: its contexts were not freed", gA, gC)
			}
		})
	}
}
