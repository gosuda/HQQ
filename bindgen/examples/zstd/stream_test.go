package zstd_test

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"os"
	"runtime"
	"strconv"
	"testing"
	"time"

	"gosuda.org/hqq/bindgen/examples/zstd"
)

// pipelinedCompressor streams chunks through ZSTD_compressStream2 on one context with up to depth
// calls in flight. Each call gets a destination of the one-shot bound of the largest chunk, which
// lets zstd consume a whole chunk whatever the output, so the calls do not depend on each other's
// results and the server works on one chunk while the client copies the next. The output of the
// oldest call goes to emit when a slot is needed, in stream order; emit's slice is reused after it
// returns.
type pipelinedCompressor struct {
	c      *zstd.Client
	cctx   zstd.CCtx
	emit   func([]byte) error
	slots  []compressSlot
	head   int
	flying int
}

type compressSlot struct {
	fut zstd.CompressStream2FlatFuture
	dst []byte
	n   int
	end bool
}

func newPipelinedCompressor(c *zstd.Client, cctx zstd.CCtx, depth, maxChunk int, emit func([]byte) error) *pipelinedCompressor {
	p := &pipelinedCompressor{c: c, cctx: cctx, emit: emit, slots: make([]compressSlot, depth)}
	for i := range p.slots {
		p.slots[i].dst = make([]byte, zstd.CompressBound(uint64(maxChunk))+64)
	}
	return p
}

// push issues one call; chunk may be reused as soon as it returns. op is ZSTD_e_continue or ZSTD_e_end.
func (p *pipelinedCompressor) push(chunk []byte, op uint32) error {
	if p.flying == len(p.slots) {
		if err := p.waitOldest(); err != nil {
			return err
		}
	}
	s := &p.slots[(p.head+p.flying)%len(p.slots)]
	fut, err := p.c.CompressStream2FlatAsync(p.cctx, s.dst, chunk, op)
	if err != nil {
		return err
	}
	s.fut, s.n, s.end = fut, len(chunk), op == zstd.ZSTD_e_end
	p.flying++
	return nil
}

func (p *pipelinedCompressor) waitOldest() error {
	s := &p.slots[p.head]
	p.head, p.flying = (p.head+1)%len(p.slots), p.flying-1
	hint, out, used, err := s.fut.Wait()
	if err != nil {
		return err
	}
	if used != uint64(s.n) {
		return fmt.Errorf("ZSTD_compressStream2 consumed %d of %d bytes with a destination of %d", used, s.n, len(s.dst))
	}
	if s.end && hint != 0 {
		return fmt.Errorf("end of stream left %d bytes to flush", hint)
	}
	return p.emit(out)
}

// drain waits for every call in flight.
func (p *pipelinedCompressor) drain() error {
	for p.flying > 0 {
		if err := p.waitOldest(); err != nil {
			return err
		}
	}
	return nil
}

// compressChunks compresses chunks as one frame: all but the last with ZSTD_e_continue, the last with
// ZSTD_e_end.
func compressChunks(c *zstd.Client, cctx zstd.CCtx, chunks [][]byte, depth int, emit func([]byte) error) error {
	p := newPipelinedCompressor(c, cctx, depth, maxLen(chunks), emit)
	for i, chunk := range chunks {
		op := zstd.ZSTD_e_continue
		if i == len(chunks)-1 {
			op = zstd.ZSTD_e_end
		}
		if err := p.push(chunk, uint32(op)); err != nil {
			return err
		}
	}
	return p.drain()
}

func maxLen(chunks [][]byte) int {
	m := 0
	for _, c := range chunks {
		m = max(m, len(c))
	}
	return m
}

// decompressChunk feeds one piece of compressed input to ZSTD_decompressStream on dctx and passes
// the output to emit. dst is the output buffer of each call. It returns the library's hint after the
// last call, which is 0 exactly at the end of a frame. The loop is the documented one: it continues
// until the input is consumed and the output buffer was not filled.
func decompressChunk(c *zstd.Client, dctx zstd.DCtx, dst, in []byte, emit func([]byte) error) (hint uint64, err error) {
	for {
		h, out, used, err := c.DecompressStreamFlat(dctx, dst, in)
		if err != nil {
			return 0, err
		}
		if err := emit(out); err != nil {
			return 0, err
		}
		in = in[used:]
		if used > 0 || len(out) > 0 {
			hint = h
		}
		if len(in) == 0 && len(out) < len(dst) {
			return hint, nil
		}
	}
}

func splitChunks(data []byte, sizes ...int) [][]byte {
	var chunks [][]byte
	for i := 0; len(data) > 0; i++ {
		n := min(len(data), sizes[i%len(sizes)])
		chunks = append(chunks, data[:n])
		data = data[n:]
	}
	return chunks
}

func TestStreamRoundTripWithIrregularChunks(t *testing.T) {
	c := newClient(t)
	src := textData(3<<20, 21)
	chunks := splitChunks(src, 1, 100_000, 7, 4096, 700_000, 65_536, 65_537)
	for _, level := range []int32{1, 3} {
		for _, depth := range []int{1, 4} {
			t.Run(fmt.Sprintf("level=%d/depth=%d", level, depth), func(t *testing.T) {
				cctx, dctx := mustCCtx(t, c), mustDCtx(t, c)
				if _, err := c.CCtxSetParameter(cctx, zstd.ZSTD_c_compressionLevel, level); err != nil {
					t.Fatal(err)
				}
				var frame bytes.Buffer
				if err := compressChunks(c, cctx, chunks, depth, func(p []byte) error { frame.Write(p); return nil }); err != nil {
					t.Fatal(err)
				}
				if frame.Len() >= len(src) {
					t.Errorf("stream of text grew from %d to %d bytes", len(src), frame.Len())
				}
				var out bytes.Buffer
				hint, err := decompressChunk(c, dctx, make([]byte, 128<<10), frame.Bytes(), func(p []byte) error { out.Write(p); return nil })
				if err != nil || hint != 0 {
					t.Fatalf("decompress: hint %d, %v", hint, err)
				}
				if !bytes.Equal(out.Bytes(), src) {
					t.Fatalf("stream decoded to %d bytes that differ from the %d byte source", out.Len(), len(src))
				}
				// One-shot decompression reads the stream frame too: the stream has no content size,
				// so the destination is the caller's choice.
				one, err := c.Decompress(make([]byte, len(src)), frame.Bytes())
				if err != nil || !bytes.Equal(one, src) {
					t.Errorf("one-shot Decompress of the stream frame: %d bytes, %v", len(one), err)
				}
				if err := c.FreeCCtx(cctx); err != nil {
					t.Fatal(err)
				}
				if err := c.FreeDCtx(dctx); err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}

// TestStreamFlushMakesOutputDecodable checks ZSTD_e_flush: everything sent so far can be decoded
// from the output so far, while the frame stays open (hint above 0).
func TestStreamFlushMakesOutputDecodable(t *testing.T) {
	c := newClient(t)
	cctx, dctx := mustCCtx(t, c), mustDCtx(t, c)
	part1, part2 := textData(50_000, 22), textData(30_000, 23)
	dst := make([]byte, 1<<20)

	_, out1, used, err := c.CompressStream2Flat(cctx, dst, part1, zstd.ZSTD_e_flush)
	if err != nil || used != uint64(len(part1)) {
		t.Fatalf("flush: consumed %d, %v", used, err)
	}
	var got bytes.Buffer
	hint, err := decompressChunk(c, dctx, make([]byte, 128<<10), out1, func(p []byte) error { got.Write(p); return nil })
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got.Bytes(), part1) {
		t.Fatalf("after a flush the decoder produced %d of %d bytes", got.Len(), len(part1))
	}
	if hint == 0 {
		t.Error("the decoder reports the frame complete after a flush")
	}

	end := make([]byte, 1<<20)
	h, out2, _, err := c.CompressStream2Flat(cctx, end, part2, zstd.ZSTD_e_end)
	if err != nil || h != 0 {
		t.Fatalf("end: hint %d, %v", h, err)
	}
	got.Reset()
	hint, err = decompressChunk(c, dctx, make([]byte, 128<<10), out2, func(p []byte) error { got.Write(p); return nil })
	if err != nil || hint != 0 || !bytes.Equal(got.Bytes(), part2) {
		t.Fatalf("after the end: hint %d, %d of %d bytes, %v", hint, got.Len(), len(part2), err)
	}
}

// TestStreamDecompressSmallOutputBuffer makes the decoder return before it consumed its input: with a
// 64 byte output buffer the consumed count is below the input size and the caller resends the rest.
func TestStreamDecompressSmallOutputBuffer(t *testing.T) {
	c := newClient(t)
	dctx := mustDCtx(t, c)
	src := textData(20_000, 24)
	frame := compressAll(t, c, src, 3)
	_, out, used, err := c.DecompressStreamFlat(dctx, make([]byte, 64), frame)
	if err != nil || len(out) != 64 || used >= uint64(len(frame)) {
		t.Fatalf("first call: %d bytes out, %d of %d consumed, %v", len(out), used, len(frame), err)
	}
	var got bytes.Buffer
	got.Write(out)
	hint, err := decompressChunk(c, dctx, make([]byte, 64), frame[used:], func(p []byte) error { got.Write(p); return nil })
	if err != nil || hint != 0 || !bytes.Equal(got.Bytes(), src) {
		t.Fatalf("after resending: hint %d, %d of %d bytes, %v", hint, got.Len(), len(src), err)
	}
}

// ---- the long stream ------------------------------------------------------------------------

// streamMiB is the size of the uncompressed stream in the long test: 4096 MiB by default, 64 MiB with
// -short, or the value of HQQ_ZSTD_STREAM_MIB.
func streamMiB(t *testing.T) int {
	t.Helper()
	if v := os.Getenv("HQQ_ZSTD_STREAM_MIB"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 16 {
			t.Fatalf("HQQ_ZSTD_STREAM_MIB=%q: want an integer of at least 16", v)
		}
		return n
	}
	if testing.Short() {
		return 64
	}
	return 4096
}

const streamChunk = 256 << 10

// streamChunkData fills dst with chunk k of the synthetic stream: a 256 KiB window of the text block
// at an offset that depends on k, so that no two neighbors repeat, and the index stamped at the start.
func streamChunkData(dst []byte, k int) {
	base := textBlock()
	off := (k * 7919 * 4096) % (len(base) - len(dst))
	copy(dst, base[off:])
	for i := range 8 {
		dst[i] = byte(k >> (8 * i))
	}
}

// TestStreamLongSyntheticStreamRunsInConstantMemory pushes a synthetic stream of several GiB through
// one session that compresses and another that decompresses, 256 KiB at a time with four compress
// calls in flight, and checks that
//   - the decompressed stream has the length and SHA-256 of the generated one,
//   - the stream compressed (the ratio is above 1.5),
//   - the resident size of the server and the heap of this process stop growing after the first
//     64 MiB: the largest value seen afterwards is within 48 MiB (server) and 64 MiB (client) of the
//     value after the warm-up.
//
// Only a few buffers exist at a time: the chunk producer, the four compress destinations, the bounded
// channel between the two sessions and the decompress buffer.
func TestStreamLongSyntheticStreamRunsInConstantMemory(t *testing.T) {
	srv, _ := ownServerTo(t, os.Stderr, reusableFree)
	runtime.GC() // the earlier tests leave 64 MiB buffers behind that would blur the heap baseline
	comp := dialTo(t, srv.Listener())
	decomp := dialTo(t, srv.Listener())
	cctx, dctx := mustCCtx(t, comp), mustDCtx(t, decomp)
	if _, err := comp.CCtxSetParameter(cctx, zstd.ZSTD_c_compressionLevel, 1); err != nil {
		t.Fatal(err)
	}

	total := streamMiB(t) << 20
	chunks := total / streamChunk
	wantSum, gotSum := sha256.New(), sha256.New()

	pipe := make(chan []byte, 8)
	free := make(chan []byte, cap(pipe)+2)
	for range cap(free) {
		free <- make([]byte, 0, zstd.CompressBound(streamChunk)+64)
	}
	stop := make(chan struct{}) // closed by the decompressor on failure
	var (
		compressed int64 // written by the compressor goroutine, read after it finished
		decoded    int64
		hint       uint64
		sampler    = newSamples(srv.PID())
	)
	errc := make(chan error, 2)

	go func() { // producer and compressor: this goroutine owns the compress session
		defer close(pipe)
		emit := func(p []byte) error {
			if len(p) == 0 {
				return nil
			}
			select {
			case b := <-free:
				b = append(b[:0], p...)
				compressed += int64(len(b))
				pipe <- b
				return nil
			case <-stop:
				return fmt.Errorf("the decompressor stopped")
			}
		}
		pc := newPipelinedCompressor(comp, cctx, 4, streamChunk, emit)
		buf := make([]byte, streamChunk) // the call copies the chunk at issue, so one buffer serves all
		for k := range chunks {
			streamChunkData(buf, k)
			wantSum.Write(buf)
			op := zstd.ZSTD_e_continue
			if k == chunks-1 {
				op = zstd.ZSTD_e_end
			}
			if err := pc.push(buf, uint32(op)); err != nil {
				errc <- err
				return
			}
		}
		errc <- pc.drain()
	}()

	go func() { // decompressor: this goroutine owns the decompress session
		var failure error
		dst := make([]byte, 1<<20)
		for b := range pipe {
			if failure == nil {
				hint, failure = decompressChunk(decomp, dctx, dst, b, func(p []byte) error {
					decoded += int64(len(p))
					gotSum.Write(p)
					sampler.at(decoded)
					return nil
				})
				if failure != nil {
					close(stop)
				}
			}
			free <- b[:0]
		}
		errc <- failure
	}()

	deadline := time.After(15 * time.Minute)
	for range 2 {
		select {
		case err := <-errc:
			if err != nil {
				t.Fatal(err)
			}
		case <-deadline:
			t.Fatal("the stream did not finish within 15 minutes")
		}
	}
	if decoded != int64(total) {
		t.Fatalf("decoded %d bytes of %d", decoded, total)
	}
	if hint != 0 {
		t.Errorf("the decoder still expects input after the last frame (hint %d)", hint)
	}
	if !bytes.Equal(wantSum.Sum(nil), gotSum.Sum(nil)) {
		t.Fatalf("the SHA-256 of the decoded stream differs from the generated one")
	}
	if sampler.err != nil {
		t.Fatalf("sampling the server: %v", sampler.err)
	}
	if !sampler.warmed {
		t.Fatalf("the stream of %d MiB ended before the 64 MiB warm-up sample", total>>20)
	}
	if ratio := float64(total) / float64(compressed); ratio < 1.5 {
		t.Errorf("compression ratio %.2f of a text stream, want above 1.5", ratio)
	}
	t.Logf("%d MiB through the stream, %.1fx; server RSS %d KiB after warm-up, at most %d KiB later; client heap %d KiB, at most %d KiB later",
		total>>20, float64(total)/float64(compressed), sampler.rssWarm, sampler.rssMax, sampler.heapWarm, sampler.heapMax)
	if growth := sampler.rssMax - sampler.rssWarm; growth > 48<<10 {
		t.Errorf("server resident size grew by %d KiB after the warm-up", growth)
	}
	if growth := sampler.heapMax - sampler.heapWarm; growth > 64<<10 {
		t.Errorf("client heap grew by %d KiB after the warm-up", growth)
	}
	if err := comp.FreeCCtx(cctx); err != nil {
		t.Error(err)
	}
	if err := decomp.FreeDCtx(dctx); err != nil {
		t.Error(err)
	}
}

// samples records the resident size of the server and the heap of this process every 64 MiB of
// decoded output. The first sample at or after 64 MiB is the warm-up value.
type samples struct {
	err               error // the first failure to read a size, checked by the test goroutine
	pid               int
	next              int64
	rssWarm, rssMax   int64
	heapWarm, heapMax int64
	warmed            bool
}

func cmpErr(first, next error) error {
	if first != nil {
		return first
	}
	return next
}

func newSamples(pid int) *samples { return &samples{pid: pid, next: 64 << 20} }

func (s *samples) at(decoded int64) {
	if decoded < s.next {
		return
	}
	s.next = decoded + 64<<20
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	rss, err := rssOf(s.pid)
	if err != nil {
		s.err = cmpErr(s.err, err)
		return
	}
	heap := int64(ms.HeapInuse >> 10)
	if !s.warmed {
		s.warmed, s.rssWarm, s.rssMax, s.heapWarm, s.heapMax = true, rss, rss, heap, heap
		return
	}
	s.rssMax, s.heapMax = max(s.rssMax, rss), max(s.heapMax, heap)
}
