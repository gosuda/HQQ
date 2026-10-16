package conformance

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
)

// HeaderSize is the size of the self-describing header of a payload of at
// least PayloadMinHeaded bytes: seq (u64 LE) followed by crc32-IEEE of the
// body (u32 LE). Shorter payloads carry only the body.
const (
	PayloadHeaderSize  = 12
	PayloadMinHeaded   = 16
	payloadGolden      = 0x9E3779B97F4A7C15
	payloadSeqMultiply = 0xD1B54A32D192ED03
)

// splitmix is the SplitMix64 generator (INTEROP_CLI.md, section 5).
type splitmix struct{ s uint64 }

func (g *splitmix) next() uint64 {
	g.s += payloadGolden
	z := g.s
	z = (z ^ (z >> 30)) * 0xBF58476D1CE4E5B9
	z = (z ^ (z >> 27)) * 0x94D049BB133111EB
	return z ^ (z >> 31)
}

// bodyStream returns n bytes of the body stream of (seed, seq): the words
// of SplitMix64 started at seed ^ (seq * 0xD1B54A32D192ED03), written
// little-endian, the last word truncated.
func bodyStream(seed, seq uint64, n int) []byte {
	g := splitmix{s: seed ^ (seq * payloadSeqMultiply)}
	out := make([]byte, n)
	var w [8]byte
	for i := 0; i < n; i += 8 {
		binary.LittleEndian.PutUint64(w[:], g.next())
		copy(out[i:], w[:])
	}
	return out
}

// Payload returns the deterministic payload of message seq of a session
// with the given seed. Its length is size.
//
//   - size == 0: empty.
//   - 1 <= size < 16: the body stream, nothing else.
//   - size >= 16: seq (u64 LE) | crc32-IEEE(body) (u32 LE) | body, where body
//     is the body stream of size-12 bytes.
func Payload(seed, seq uint64, size int) []byte {
	if size <= 0 {
		return []byte{}
	}
	if size < PayloadMinHeaded {
		return bodyStream(seed, seq, size)
	}
	body := bodyStream(seed, seq, size-PayloadHeaderSize)
	out := make([]byte, size)
	binary.LittleEndian.PutUint64(out[0:8], seq)
	binary.LittleEndian.PutUint32(out[8:12], crc32.ChecksumIEEE(body))
	copy(out[PayloadHeaderSize:], body)
	return out
}

// ErrPayloadMismatch is returned by VerifyPayload when a payload is not the
// one Payload(seed, seq, len(got)) produces.
var ErrPayloadMismatch = errors.New("conformance: payload mismatch")

// VerifyPayload checks got against Payload(seed, seq, len(got)) and returns
// a descriptive error (wrapping ErrPayloadMismatch) on the first difference.
func VerifyPayload(seed, seq uint64, got []byte) error {
	want := Payload(seed, seq, len(got))
	if bytes.Equal(got, want) {
		return nil
	}
	if len(got) >= PayloadMinHeaded {
		if s := binary.LittleEndian.Uint64(got[0:8]); s != seq {
			return fmt.Errorf("%w: header seq %d, want %d", ErrPayloadMismatch, s, seq)
		}
		if c := binary.LittleEndian.Uint32(got[8:12]); c != crc32.ChecksumIEEE(got[PayloadHeaderSize:]) {
			return fmt.Errorf("%w: body crc does not match the header", ErrPayloadMismatch)
		}
	}
	for i := range got {
		if got[i] != want[i] {
			return fmt.Errorf("%w: byte %d is %#02x, want %#02x", ErrPayloadMismatch, i, got[i], want[i])
		}
	}
	return ErrPayloadMismatch
}
