//go:build cgo

package cgobench

/*
#cgo CFLAGS: -O2 -I${SRCDIR}/../../testdata/microlib
#include "microlib.c"
*/
import "C"

import (
	"errors"
	"unsafe"
)

// Errors returned by the wrappers. They stand for the failures the generated client reports as
// *rt.LibError and *rt.StatusError, so the benchmarks can compare outcomes without importing rt.
var (
	// ErrDenied: the counter policy refused a negative delta (E_DENIED on the server).
	ErrDenied = errors.New("cgobench: negative delta")
	// ErrOverflow: ml_counter_add returned -1.
	ErrOverflow = errors.New("cgobench: counter overflow")
	// ErrCap: ml_xor returned SIZE_MAX because the destination is too small.
	ErrCap = errors.New("cgobench: destination too small")
	// ErrNoMemory: ml_counter_new returned NULL.
	ErrNoMemory = errors.New("cgobench: out of memory")
)

// MaxDelta is the largest delta Counter.Add passes on; the server hook of the benchmark server clamps
// to the same value.
const MaxDelta = 1000000

// Add is ml_add.
func Add(a, b int32) int32 {
	return int32(C.ml_add(C.int32_t(a), C.int32_t(b)))
}

// Work is ml_work.
func Work(iters uint32) uint64 {
	return uint64(C.ml_work(C.uint32_t(iters)))
}

// Counter is an ml_counter. It is not safe for concurrent use, like the library. It owns the
// storage of the out parameter of ml_counter_add: a variable local to Add would escape to the heap
// on every call, because its address goes to C, a cost the cgo baseline should not pay.
type Counter struct {
	p     *C.ml_counter
	total C.int64_t
}

// NewCounter is ml_counter_new.
func NewCounter(start int64) (*Counter, error) {
	p := C.ml_counter_new(C.int64_t(start))
	if p == nil {
		return nil, ErrNoMemory
	}
	return &Counter{p: p}, nil
}

// Add is ml_counter_add with the policy of the benchmark server's interceptor
// (bindgen/bench/server/microbench_hooks.c): a negative delta is refused, a delta above MaxDelta is
// clamped. It returns the new total.
func (c *Counter) Add(delta int64) (int64, error) {
	if delta < 0 {
		return 0, ErrDenied
	}
	if delta > MaxDelta {
		delta = MaxDelta
	}
	if C.ml_counter_add(c.p, C.int64_t(delta), &c.total) != 0 {
		return 0, ErrOverflow
	}
	return int64(c.total), nil
}

// Free is ml_counter_free. The Counter must not be used afterwards.
func (c *Counter) Free() {
	C.ml_counter_free(c.p)
	c.p = nil
}

// Xor is ml_xor: dst[i] = src[i] ^ key. It returns the number of bytes written, or ErrCap if
// len(dst) < len(src). The slices are passed to C in place: this is the copy that cgo avoids by
// sharing the address space.
func Xor(src []byte, key byte, dst []byte) (int, error) {
	var s, d *C.uint8_t
	if len(src) > 0 {
		s = (*C.uint8_t)(unsafe.Pointer(&src[0]))
	}
	if len(dst) > 0 {
		d = (*C.uint8_t)(unsafe.Pointer(&dst[0]))
	}
	r := C.ml_xor(s, C.size_t(len(src)), C.uint8_t(key), d, C.size_t(len(dst)))
	if C.ml_xor_failed(r) != 0 {
		return 0, ErrCap
	}
	return int(r), nil
}
