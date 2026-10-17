package zstd_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"sync"
	"testing"
	"time"

	hqq "gosuda.org/hqq"
	"gosuda.org/hqq/bindgen/examples/zstd"
	"gosuda.org/hqq/bindgen/rt"
)

// The benchmarks compare the generated client with the same library called through cgo
// (bench_cgo_test.go, built with -tags zstdcgo), over payloads from 64 B to 64 MiB. SetBytes is the
// uncompressed size, so the MB/s column of compress and decompress is comparable.
//
//	go test -tags zstdcgo -run '^$' -bench . -benchtime 1s ./bindgen/examples/zstd/
//
// Modes of every size:
//
//	server_sync                one call at a time on one session
//	server_pipelined           eight calls in flight on one session (up to 1 MiB: above that the
//	                           response needs more than B/2 frames and flow control serializes the calls)
//	server_sync_2sessions      two goroutines, one session each, one call at a time
//	server_*_copyreq           the first two against a server started with --copy-request (up to 256 KiB)
//	cgo_sync, cgo_2threads     the library in process on one goroutine and on two goroutines
//
// HQQ_ZSTD_BENCH_SPIN=1 runs both ends with the spin wait policy (BINDGEN.md 1.3, item 5).

const (
	benchLevel = 1
	benchDepth = 8
)

var benchSizes = []int{64, 256, 1 << 10, 4 << 10, 16 << 10, 64 << 10, 256 << 10, 1 << 20, 4 << 20, 16 << 20, 64 << 20}

type benchOp int

const (
	opCompress benchOp = iota
	opDecompress
	opVersion
)

func (o benchOp) String() string { return [...]string{"compress", "decompress", "version"}[o] }

// benchCase is the data of one size: the source, its frame at benchLevel, and the output buffers.
type benchCase struct {
	op    benchOp
	size  int
	src   []byte
	frame []byte
}

// run is the whole body of a mode: it may use b.N and the timer.
type benchMode struct {
	name    string
	maxSize int // modes are skipped above this size; 0 means no limit
	run     func(b *testing.B, cs *benchCase)
}

// cgoModes is filled by bench_cgo_test.go.
var cgoModes []benchMode

func sizeLabel(n int) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%dMiB", n>>20)
	case n >= 1<<10:
		return fmt.Sprintf("%dKiB", n>>10)
	}
	return fmt.Sprintf("%dB", n)
}

// benchServers are the servers of one benchmark function: the default one and one that copies request
// bodies before the library reads them.
type benchServers struct {
	plain, copying string
	opts           rt.Options
}

func newBenchServers(b *testing.B) *benchServers {
	b.Helper()
	requireServer(b)
	var args []string
	opts := zstd.DefaultOptions()
	if os.Getenv("HQQ_ZSTD_BENCH_SPIN") == "1" {
		args = append(args, "--spin")
		opts.Link.WaitPolicy = hqq.WaitSpin
	}
	// The session lines of the servers would interleave with the benchmark output.
	plain, _ := ownServerTo(b, io.Discard, nil, args...)
	copying, _ := ownServerTo(b, io.Discard, nil, append([]string{"--copy-request"}, args...)...)
	return &benchServers{plain: plain.Listener(), copying: copying.Listener(), opts: opts}
}

func (s *benchServers) dial(b *testing.B, listener string) *zstd.Client {
	b.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, err := zstd.Dial(ctx, listener, s.opts)
	if err != nil {
		b.Fatalf("dial %s: %v", listener, err)
	}
	b.Cleanup(func() {
		if err := c.Close(); err != nil {
			b.Errorf("close: %v", err)
		}
	})
	return c
}

// serverModes returns the modes that talk to the servers of s.
func (s *benchServers) serverModes() []benchMode {
	return []benchMode{
		{"server_sync", 0, func(b *testing.B, cs *benchCase) { s.syncLoop(b, cs, s.plain) }},
		{"server_pipelined", 1 << 20, func(b *testing.B, cs *benchCase) { s.pipelinedLoop(b, cs, s.plain) }},
		{"server_sync_2sessions", 0, s.twoSessions},
		{"server_sync_copyreq", 256 << 10, func(b *testing.B, cs *benchCase) { s.syncLoop(b, cs, s.copying) }},
		{"server_pipelined_copyreq", 256 << 10, func(b *testing.B, cs *benchCase) { s.pipelinedLoop(b, cs, s.copying) }},
	}
}

// call issues one call of cs.op, with dst as the output buffer where the call has one.
type caller interface {
	// do runs one call and returns the number of result bytes.
	do(c *zstd.Client, dst []byte) (int, error)
	// async starts a call; the returned function waits for it.
	async(c *zstd.Client, dst []byte) (func() (int, error), error)
}

type compressCall struct{ src []byte }

func (k compressCall) do(c *zstd.Client, dst []byte) (int, error) {
	out, err := c.Compress(dst, k.src, benchLevel)
	return len(out), err
}

func (k compressCall) async(c *zstd.Client, dst []byte) (func() (int, error), error) {
	f, err := c.CompressAsync(dst, k.src, benchLevel)
	if err != nil {
		return nil, err
	}
	return func() (int, error) { out, err := f.Wait(); return len(out), err }, nil
}

type decompressCall struct{ frame []byte }

func (k decompressCall) do(c *zstd.Client, dst []byte) (int, error) {
	out, err := c.Decompress(dst, k.frame)
	return len(out), err
}

func (k decompressCall) async(c *zstd.Client, dst []byte) (func() (int, error), error) {
	f, err := c.DecompressAsync(dst, k.frame)
	if err != nil {
		return nil, err
	}
	return func() (int, error) { out, err := f.Wait(); return len(out), err }, nil
}

type versionCall struct{}

func (versionCall) do(c *zstd.Client, _ []byte) (int, error) {
	v, err := c.VersionNumber()
	return int(v), err
}

func (versionCall) async(c *zstd.Client, _ []byte) (func() (int, error), error) {
	f, err := c.VersionNumberAsync()
	if err != nil {
		return nil, err
	}
	return func() (int, error) { v, err := f.Wait(); return int(v), err }, nil
}

// plan returns the call, the buffer size it needs and the result length it must produce.
func (cs *benchCase) plan() (call caller, dstLen, want int) {
	switch cs.op {
	case opCompress:
		return compressCall{cs.src}, int(zstd.CompressBound(uint64(cs.size))), len(cs.frame)
	case opDecompress:
		return decompressCall{cs.frame}, cs.size, cs.size
	}
	return versionCall{}, 0, 0
}

func (s *benchServers) syncLoop(b *testing.B, cs *benchCase, listener string) {
	c := s.dial(b, listener)
	call, dstLen, want := cs.plan()
	dst := make([]byte, dstLen)
	if cs.op != opVersion {
		b.SetBytes(int64(cs.size))
	}
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		n, err := call.do(c, dst)
		if err != nil {
			b.Fatal(err)
		}
		if cs.op != opVersion && n != want {
			b.Fatalf("result of %d bytes, want %d", n, want)
		}
	}
}

func (s *benchServers) pipelinedLoop(b *testing.B, cs *benchCase, listener string) {
	depth := benchDepth
	if cs.op == opVersion {
		depth = 32
	}
	c := s.dial(b, listener)
	call, dstLen, want := cs.plan()
	dsts := make([][]byte, depth)
	waits := make([]func() (int, error), depth)
	for i := range dsts {
		dsts[i] = make([]byte, dstLen)
	}
	wait := func(i int) {
		n, err := waits[i]()
		if err != nil {
			b.Fatal(err)
		}
		if cs.op != opVersion && n != want {
			b.Fatalf("result of %d bytes, want %d", n, want)
		}
	}
	if cs.op != opVersion {
		b.SetBytes(int64(cs.size))
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := range b.N {
		slot := i % depth
		if i >= depth {
			wait(slot)
		}
		w, err := call.async(c, dsts[slot])
		if err != nil {
			b.Fatal(err)
		}
		waits[slot] = w
	}
	for i := max(0, b.N-depth); i < b.N; i++ {
		wait(i % depth)
	}
}

// split runs b.N operations over workers goroutines and waits for all of them. Each worker gets its
// index and its share of the operations; a worker that fails reports through errs.
func split(b *testing.B, workers int, work func(w, n int) error) {
	b.Helper()
	var wg sync.WaitGroup
	errs := make(chan error, workers)
	for w := range workers {
		n := b.N / workers
		if w < b.N%workers {
			n++
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := work(w, n); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		b.Error(err)
	}
}

func (s *benchServers) twoSessions(b *testing.B, cs *benchCase) {
	clients := []*zstd.Client{s.dial(b, s.plain), s.dial(b, s.plain)}
	call, dstLen, want := cs.plan()
	dsts := [][]byte{make([]byte, dstLen), make([]byte, dstLen)}
	if cs.op != opVersion {
		b.SetBytes(int64(cs.size))
	}
	b.ReportAllocs()
	b.ResetTimer()
	split(b, 2, func(w, n int) error {
		for range n {
			got, err := call.do(clients[w], dsts[w])
			if err != nil {
				return err
			}
			if cs.op != opVersion && got != want {
				return fmt.Errorf("result of %d bytes, want %d", got, want)
			}
		}
		return nil
	})
}

// benchCases returns the data of one size for op; the frame is compressed by the server and checked
// by a round trip first.
func benchCaseFor(b *testing.B, s *benchServers, op benchOp, size int) *benchCase {
	b.Helper()
	cs := &benchCase{op: op, size: size, src: textData(size, uint64(size))}
	c := s.dial(b, s.plain)
	cs.frame = compressAll(b, c, cs.src, benchLevel)
	out, err := c.Decompress(make([]byte, size), cs.frame)
	if err != nil || !bytes.Equal(out, cs.src) {
		b.Fatalf("round trip of %d bytes failed: %v", size, err)
	}
	return cs
}

func benchmarkOp(b *testing.B, op benchOp) {
	s := newBenchServers(b)
	modes := append(s.serverModes(), cgoModes...)
	for _, size := range benchSizes {
		cs := benchCaseFor(b, s, op, size)
		for _, m := range modes {
			if m.maxSize != 0 && size > m.maxSize {
				continue
			}
			b.Run(fmt.Sprintf("size=%s/%s", sizeLabel(size), m.name), func(b *testing.B) { m.run(b, cs) })
		}
	}
}

// BenchmarkCompress is ZSTD_compress at level 1 of text data.
func BenchmarkCompress(b *testing.B) { benchmarkOp(b, opCompress) }

// BenchmarkDecompress is ZSTD_decompress of the level 1 frame of the same data.
func BenchmarkDecompress(b *testing.B) { benchmarkOp(b, opDecompress) }

// BenchmarkTrivialCall is ZSTD_versionNumber: no payload and no library work, so it measures the cost
// of a call alone. The pipelined mode keeps 32 calls in flight.
func BenchmarkTrivialCall(b *testing.B) {
	s := newBenchServers(b)
	cs := &benchCase{op: opVersion}
	for _, m := range append(s.serverModes(), cgoModes...) {
		if m.maxSize != 0 && m.maxSize < 64 {
			continue
		}
		if m.name == "server_sync_copyreq" || m.name == "server_pipelined_copyreq" {
			continue
		}
		b.Run(m.name, func(b *testing.B) { m.run(b, cs) })
	}
}
