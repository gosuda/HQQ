//go:build cgo && zstdcgo

package zstd_test

import (
	"fmt"
	"testing"

	"gosuda.org/hqq/bindgen/examples/zstd"
	"gosuda.org/hqq/bindgen/examples/zstd/internal/cgozstd"
)

func init() {
	cgoModes = []benchMode{
		{"cgo_sync", 0, func(b *testing.B, cs *benchCase) { cgoRun(b, cs, 1) }},
		{"cgo_2threads", 0, func(b *testing.B, cs *benchCase) { cgoRun(b, cs, 2) }},
	}
}

// cgoRun runs b.N calls of the in-process library split over workers goroutines, each with its own
// output buffer.
func cgoRun(b *testing.B, cs *benchCase, workers int) {
	_, dstLen, want := cs.plan()
	dsts := make([][]byte, workers)
	for i := range dsts {
		dsts[i] = make([]byte, dstLen)
	}
	one := func(dst []byte) error {
		switch cs.op {
		case opCompress:
			out, err := cgozstd.Compress(dst, cs.src, benchLevel)
			if err != nil || len(out) != want {
				return fmt.Errorf("compress: %d bytes, want %d, %v", len(out), want, err)
			}
		case opDecompress:
			out, err := cgozstd.Decompress(dst, cs.frame)
			if err != nil || len(out) != want {
				return fmt.Errorf("decompress: %d bytes, want %d, %v", len(out), want, err)
			}
		default:
			if cgozstd.VersionNumber() == 0 {
				return fmt.Errorf("version number 0")
			}
		}
		return nil
	}
	if cs.op != opVersion {
		b.SetBytes(int64(cs.size))
	}
	b.ReportAllocs()
	b.ResetTimer()
	split(b, workers, func(w, n int) error {
		for range n {
			if err := one(dsts[w]); err != nil {
				return err
			}
		}
		return nil
	})
}

// TestCgoBenchmarkCallsAgreeWithServer keeps the cgo side of the benchmarks honest: for every size
// its Compress produces the frame that the server produces, which the benchmark loops then check by
// length on every call.
func TestCgoBenchmarkCallsAgreeWithServer(t *testing.T) {
	c := newClient(t)
	for _, size := range []int{64, 4 << 10, 1 << 20} {
		src := textData(size, uint64(size))
		got, err := cgozstd.Compress(make([]byte, zstd.CompressBound(uint64(size))), src, benchLevel)
		if err != nil {
			t.Fatal(err)
		}
		if want := compressAll(t, c, src, benchLevel); string(got) != string(want) {
			t.Errorf("size %d: in-process frame differs from the server frame", size)
		}
	}
}
