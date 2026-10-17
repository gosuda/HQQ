//go:build cgo

package bench

import (
	"bytes"
	"fmt"
)

// Independent oracles. The benchmarks compare every result with these, not with the other
// implementation, so a bug shared by the generated client and cgo (both call microlib.c) still fails.

// workStart and the two multipliers of ml_work (microlib.h).
const (
	workStart = 0x9e3779b97f4a7c15
	workMul   = 0xbf58476d1ce4e5b9
)

// refWork is ml_work in Go: x = (x ^ (x >> 29)) * workMul + 1, iters times, from workStart.
func refWork(iters uint32) uint64 {
	x := uint64(workStart)
	for range iters {
		x = (x^(x>>29))*workMul + 1
	}
	return x
}

// addA, addB and addWant give the operands and the result of call i of the Add benchmarks. The
// operands differ in every call and the sum wraps, so a stale, duplicated or misrouted response is
// a wrong value.
func addA(i int) int32 { return int32(i) }
func addB(i int) int32 { return int32(i)*3 + 1 }
func addWant(i int) int32 {
	return addA(i) + addB(i)
}

// counterDelta is the delta of call i of the CounterAdd benchmarks: 1, 2, 3, 1, ...
func counterDelta(i int) int64 { return int64(i%3) + 1 }

// xorSrc returns n deterministic bytes that no short period describes.
func xorSrc(n int) []byte {
	b := make([]byte, n)
	x := uint32(0x2545F491)
	for i := range b {
		x ^= x << 13
		x ^= x >> 17
		x ^= x << 5
		b[i] = byte(x >> 11)
	}
	return b
}

// xorKey is the key of call i of the Xor benchmarks. Consecutive keys differ and a pipeline of up to
// 64 calls never reuses a key inside one window, so an output buffer that keeps the result of an
// earlier call is caught by the next verification.
func xorKey(i int) uint8 { return uint8(i*37 + 11) }

// xorSamples is how many bytes checkXor compares per call besides the length.
const xorSamples = 8

// checkXor verifies dst against src ^ key. Every call compares the length and eight bytes spread
// over the buffer, the first and the last among them; full compares every byte. The benchmarks ask
// for full on the first and the last call of a leaf and on every 1024th.
func checkXor(src, dst []byte, key uint8, full bool) error {
	if len(dst) != len(src) {
		return fmt.Errorf("xor result has %d bytes, want %d", len(dst), len(src))
	}
	n := len(src)
	if n == 0 {
		return nil
	}
	if full {
		for i := range src {
			if dst[i] != src[i]^key {
				return fmt.Errorf("xor result byte %d of %d is %#x, want %#x (key %#x)", i, n, dst[i], src[i]^key, key)
			}
		}
		return nil
	}
	for k := range xorSamples {
		i := (n - 1) * k / (xorSamples - 1)
		if dst[i] != src[i]^key {
			return fmt.Errorf("xor result byte %d of %d is %#x, want %#x (key %#x)", i, n, dst[i], src[i]^key, key)
		}
	}
	return nil
}

// refXor is ml_xor in Go.
func refXor(src []byte, key uint8) []byte {
	out := bytes.Clone(src)
	for i := range out {
		out[i] ^= key
	}
	return out
}
