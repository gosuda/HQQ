package rt

import (
	"bytes"
	"errors"
	"strings"
)

// errValue is returned by the Get helpers for a value that is not encoded as
// BINDGEN.md 4.5 says. Callers turn it into Dec.Malformed.
var errValue = errors.New("hqqbg: malformed value")

// Align rounds off up to a multiple of n, which must be a power of two.
func Align(off, n int) int { return (off + n - 1) &^ (n - 1) }

// BytesWire is the encoded size of a bytes value of n bytes without padding.
func BytesWire(n int) int { return 4 + n }

// StrWire is the encoded size of a str value of n bytes without padding.
func StrWire(n int) int { return 4 + n + 1 }

// CheckStr reports ErrInvalidArg if s contains a NUL byte or is longer than
// limit; a limit of 0 means no bound.
func CheckStr(s string, limit int) error {
	if limit > 0 && len(s) > limit {
		return ErrInvalidArg
	}
	if strings.IndexByte(s, 0) >= 0 {
		return ErrInvalidArg
	}
	return nil
}

// PutStr writes the length, the bytes and the terminating NUL of s at off,
// which is already aligned to 4, and returns the end offset. b must be long
// enough; CheckStr validates s.
func PutStr(b []byte, off int, s string) int {
	le.PutUint32(b[off:], uint32(len(s)))
	end := off + 4 + len(s)
	copy(b[off+4:end], s)
	b[end] = 0
	return end + 1
}

// PutBytes writes the length and the bytes of p at off, which is already
// aligned to 4, and returns the end offset.
func PutBytes(b []byte, off int, p []byte) int {
	le.PutUint32(b[off:], uint32(len(p)))
	end := off + 4 + len(p)
	copy(b[off+4:end], p)
	return end
}

// getLen aligns off to 4 and loads the u32 that starts there. It returns the
// offset after the u32.
func getLen(b []byte, off int) (n uint32, next int, ok bool) {
	if off < 0 || off > len(b) {
		return 0, 0, false
	}
	off = Align(off, 4)
	if off > len(b) || len(b)-off < 4 {
		return 0, 0, false
	}
	return le.Uint32(b[off:]), off + 4, true
}

// lengthOK reports whether a decoded length or count is not NULL_LEN and not
// above limit (0: no bound).
func lengthOK(n uint32, limit int) bool {
	if n == NullLen {
		return false
	}
	return limit <= 0 || uint64(n) <= uint64(limit)
}

// GetStr decodes a non-nullable str at off (aligned up to 4 first). The text
// must not contain NUL, must be followed by the NUL byte, and must not be
// longer than limit (0: no bound).
func GetStr(b []byte, off int, limit int) (s string, end int, err error) {
	n, p, ok := getLen(b, off)
	if !ok || !lengthOK(n, limit) {
		return "", 0, errValue
	}
	if uint64(n)+1 > uint64(len(b)-p) {
		return "", 0, errValue
	}
	end = p + int(n)
	if b[end] != 0 || bytes.IndexByte(b[p:end], 0) >= 0 {
		return "", 0, errValue
	}
	return string(b[p:end]), end + 1, nil
}

// GetBytesInto decodes a non-nullable bytes value at off into dst[:n]. The
// length must not exceed len(dst), the capacity the request sent.
func GetBytesInto(dst, b []byte, off int) (n, end int, err error) {
	l, p, ok := getLen(b, off)
	if !ok || l == NullLen || uint64(l) > uint64(len(dst)) || uint64(l) > uint64(len(b)-p) {
		return 0, 0, errValue
	}
	n = int(l)
	copy(dst[:n], b[p:p+n])
	return n, p + n, nil
}

// GetCount decodes the element count of a non-nullable array at off. The
// count must not exceed limit (0: no bound) and count * minsize must fit in the
// bytes that remain, so a hostile count cannot make the caller allocate more
// than the message itself.
func GetCount(b []byte, off int, limit, minsize int) (count uint32, end int, err error) {
	c, p, ok := getLen(b, off)
	if !ok || !lengthOK(c, limit) {
		return 0, 0, errValue
	}
	if uint64(c)*uint64(max(minsize, 1)) > uint64(len(b)-p) {
		return 0, 0, errValue
	}
	return c, p, nil
}
