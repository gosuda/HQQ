package zstd

import (
	"errors"

	"gosuda.org/hqq/bindgen/rt"
)

// ErrorCode is a ZSTD_ErrorCode of zstd_errors.h, the stable ones that callers usually branch on.
type ErrorCode int

const (
	ErrPrefixUnknown        ErrorCode = 10 // not a zstd frame
	ErrCorruptionDetected   ErrorCode = 20
	ErrChecksumWrong        ErrorCode = 22
	ErrParameterOutOfBound  ErrorCode = 42
	ErrParameterUnsupported ErrorCode = 40
	ErrStageWrong           ErrorCode = 60
	ErrDstSizeTooSmall      ErrorCode = 70
	ErrSrcSizeWrong         ErrorCode = 72
)

// ErrorCodeOf returns the ZSTD_ErrorCode of a library failure reported by a call that returns size_t,
// and false when err is not such a failure. The server sends the size_t result of the failed call,
// which is minus the code; the text of the library's own message is in the *rt.LibError.
func ErrorCodeOf(err error) (ErrorCode, bool) {
	var le *rt.LibError
	if !errors.As(err, &le) || le.Code >= 0 {
		return 0, false
	}
	return ErrorCode(-le.Code), true
}

// maxInputSize is ZSTD_MAX_INPUT_SIZE for a 64-bit size_t.
const maxInputSize = 0xFF00FF00FF00FF00

// CompressBound is the ZSTD_COMPRESSBOUND macro: the largest size ZSTD_compress can produce for srcSize
// input bytes, computed locally so that sizing a destination costs no round trip. It returns 0 when
// srcSize is too large for zstd; the library function ZSTD_compressBound reports that as an error.
func CompressBound(srcSize uint64) uint64 {
	if srcSize >= maxInputSize {
		return 0
	}
	n := srcSize + srcSize>>8
	if srcSize < 128<<10 {
		n += (128<<10 - srcSize) >> 11
	}
	return n
}

// DefaultOptions is the session geometry that README.md documents and that the defaults of the
// server (server/main.c) match: 64 KiB frames, 64 frames per direction, and messages up to 128 MiB.
// A call with a destination of at most 65,501 bytes has a response bound of one frame (cap plus 19
// bytes of framing) and is built in place in the ring slot.
// rt.Options{} would select 16 KiB frames and 16 MiB messages.
func DefaultOptions() rt.Options {
	return rt.Options{BufferSize: 64 << 10, BufferCount: 64, MaxMessage: 128 << 20}
}
