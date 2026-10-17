//go:build cgo && zstdcgo

package cgozstd

/*
#include <stddef.h>
#include <stdint.h>
#include <zstd.h>

// The stream functions take pointers to ZSTD_inBuffer and ZSTD_outBuffer, which hold a data pointer.
// These wrappers build the structs on the C stack from slices, so no Go memory holds a Go pointer.
static size_t cz_compress_stream(ZSTD_CCtx *c, void *dst, size_t dstCap, const void *src, size_t srcSize,
                                 unsigned endOp, size_t *dstPos, size_t *srcPos) {
	ZSTD_outBuffer out = {dst, dstCap, 0};
	ZSTD_inBuffer in = {src, srcSize, 0};
	size_t r = ZSTD_compressStream2(c, &out, &in, (ZSTD_EndDirective)endOp);
	*dstPos = out.pos;
	*srcPos = in.pos;
	return r;
}

static size_t cz_decompress_stream(ZSTD_DCtx *d, void *dst, size_t dstCap, const void *src, size_t srcSize,
                                   size_t *dstPos, size_t *srcPos) {
	ZSTD_outBuffer out = {dst, dstCap, 0};
	ZSTD_inBuffer in = {src, srcSize, 0};
	size_t r = ZSTD_decompressStream(d, &out, &in);
	*dstPos = out.pos;
	*srcPos = in.pos;
	return r;
}
*/
import "C"

import (
	"errors"
	"fmt"
	"unsafe"
)

// Error is a zstd failure: Code is the ZSTD_ErrorCode and Text the library's message.
type Error struct {
	Func string
	Code int
	Text string
}

func (e *Error) Error() string { return fmt.Sprintf("%s: %s (code %d)", e.Func, e.Text, e.Code) }

// errOf converts a size_t result to an error when ZSTD_isError says it is one.
func errOf(fn string, r C.size_t) error {
	if C.ZSTD_isError(r) == 0 {
		return nil
	}
	return &Error{Func: fn, Code: -int(int64(uint64(r))), Text: C.GoString(C.ZSTD_getErrorName(r))}
}

// ptr returns the address of the first byte of b, or nil for an empty slice.
func ptr(b []byte) unsafe.Pointer {
	if len(b) == 0 {
		return nil
	}
	return unsafe.Pointer(&b[0])
}

func VersionNumber() uint32 { return uint32(C.ZSTD_versionNumber()) }

func VersionString() string { return C.GoString(C.ZSTD_versionString()) }

func MinCLevel() int { return int(C.ZSTD_minCLevel()) }

func MaxCLevel() int { return int(C.ZSTD_maxCLevel()) }

// CompressBound is ZSTD_compressBound; it fails when srcSize is too large for zstd.
func CompressBound(srcSize uint64) (uint64, error) {
	r := C.ZSTD_compressBound(C.size_t(srcSize))
	if err := errOf("ZSTD_compressBound", r); err != nil {
		return 0, err
	}
	return uint64(r), nil
}

// GetFrameContentSize is ZSTD_getFrameContentSize over frame.
func GetFrameContentSize(frame []byte) uint64 {
	return uint64(C.ZSTD_getFrameContentSize(ptr(frame), C.size_t(len(frame))))
}

// Compress is ZSTD_compress into dst; it returns dst[:n].
func Compress(dst, src []byte, level int) ([]byte, error) {
	r := C.ZSTD_compress(ptr(dst), C.size_t(len(dst)), ptr(src), C.size_t(len(src)), C.int(level))
	if err := errOf("ZSTD_compress", r); err != nil {
		return nil, err
	}
	return dst[:int(r)], nil
}

// Decompress is ZSTD_decompress into dst; it returns dst[:n].
func Decompress(dst, src []byte) ([]byte, error) {
	r := C.ZSTD_decompress(ptr(dst), C.size_t(len(dst)), ptr(src), C.size_t(len(src)))
	if err := errOf("ZSTD_decompress", r); err != nil {
		return nil, err
	}
	return dst[:int(r)], nil
}

// CCtx is a ZSTD_CCtx. Not safe for concurrent use.
type CCtx struct{ p *C.ZSTD_CCtx }

var errAlloc = errors.New("cgozstd: out of memory")

func NewCCtx() (*CCtx, error) {
	p := C.ZSTD_createCCtx()
	if p == nil {
		return nil, errAlloc
	}
	return &CCtx{p}, nil
}

func (c *CCtx) Close() error {
	r := C.ZSTD_freeCCtx(c.p)
	c.p = nil
	return errOf("ZSTD_freeCCtx", r)
}

func (c *CCtx) SetParameter(param, value int) error {
	return errOf("ZSTD_CCtx_setParameter", C.ZSTD_CCtx_setParameter(c.p, C.ZSTD_cParameter(param), C.int(value)))
}

func (c *CCtx) SetPledgedSrcSize(n uint64) error {
	return errOf("ZSTD_CCtx_setPledgedSrcSize", C.ZSTD_CCtx_setPledgedSrcSize(c.p, C.ulonglong(n)))
}

func (c *CCtx) Compress(dst, src []byte, level int) ([]byte, error) {
	r := C.ZSTD_compressCCtx(c.p, ptr(dst), C.size_t(len(dst)), ptr(src), C.size_t(len(src)), C.int(level))
	if err := errOf("ZSTD_compressCCtx", r); err != nil {
		return nil, err
	}
	return dst[:int(r)], nil
}

// CompressStream is ZSTD_compressStream2 with the buffers unpacked: it returns the hint of the
// library (bytes left to flush, 0 when an end frame is complete), dst[:produced] and the number of
// src bytes consumed.
func (c *CCtx) CompressStream(dst, src []byte, endOp uint32) (hint uint64, out []byte, consumed uint64, err error) {
	var dstPos, srcPos C.size_t
	r := C.cz_compress_stream(c.p, ptr(dst), C.size_t(len(dst)), ptr(src), C.size_t(len(src)), C.uint(endOp), &dstPos, &srcPos)
	if err := errOf("ZSTD_compressStream2", r); err != nil {
		return 0, nil, 0, err
	}
	return uint64(r), dst[:int(dstPos)], uint64(srcPos), nil
}

// DCtx is a ZSTD_DCtx. Not safe for concurrent use.
type DCtx struct{ p *C.ZSTD_DCtx }

func NewDCtx() (*DCtx, error) {
	p := C.ZSTD_createDCtx()
	if p == nil {
		return nil, errAlloc
	}
	return &DCtx{p}, nil
}

func (d *DCtx) Close() error {
	r := C.ZSTD_freeDCtx(d.p)
	d.p = nil
	return errOf("ZSTD_freeDCtx", r)
}

func (d *DCtx) Decompress(dst, src []byte) ([]byte, error) {
	r := C.ZSTD_decompressDCtx(d.p, ptr(dst), C.size_t(len(dst)), ptr(src), C.size_t(len(src)))
	if err := errOf("ZSTD_decompressDCtx", r); err != nil {
		return nil, err
	}
	return dst[:int(r)], nil
}

// DecompressStream is ZSTD_decompressStream with the buffers unpacked, like CompressStream.
func (d *DCtx) DecompressStream(dst, src []byte) (hint uint64, out []byte, consumed uint64, err error) {
	var dstPos, srcPos C.size_t
	r := C.cz_decompress_stream(d.p, ptr(dst), C.size_t(len(dst)), ptr(src), C.size_t(len(src)), &dstPos, &srcPos)
	if err := errOf("ZSTD_decompressStream", r); err != nil {
		return 0, nil, 0, err
	}
	return uint64(r), dst[:int(dstPos)], uint64(srcPos), nil
}
