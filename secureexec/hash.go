package secureexec

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
)

// HashSize is the size in bytes of a SHA-256 digest.
const HashSize = sha256.Size

// Hash is a SHA-256 digest. Its text form is 64 lowercase hex digits.
type Hash [HashSize]byte

// ParseHash parses a 64-digit hexadecimal SHA-256 digest.
func ParseHash(s string) (Hash, error) {
	var h Hash
	if len(s) != 2*HashSize {
		return Hash{}, fmt.Errorf("secureexec: sha256 %q: want %d hex digits", s, 2*HashSize)
	}
	if _, err := hex.Decode(h[:], []byte(s)); err != nil {
		return Hash{}, fmt.Errorf("secureexec: sha256 %q: %w", s, err)
	}
	return h, nil
}

// String returns the digest as 64 lowercase hex digits.
func (h Hash) String() string {
	return hex.EncodeToString(h[:])
}

// MarshalText implements encoding.TextMarshaler.
func (h Hash) MarshalText() ([]byte, error) {
	return []byte(h.String()), nil
}

// UnmarshalText implements encoding.TextUnmarshaler.
func (h *Hash) UnmarshalText(text []byte) error {
	v, err := ParseHash(string(text))
	if err != nil {
		return err
	}
	*h = v
	return nil
}

// HashFile streams the regular file at path through SHA-256 and returns its
// digest and size. It is a tooling helper: it follows symbolic links and is
// not a security boundary; use Verify to run an executable.
func HashFile(path string) (Hash, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return Hash{}, 0, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return Hash{}, 0, err
	}
	if !fi.Mode().IsRegular() {
		return Hash{}, 0, fmt.Errorf("%s: %w", path, ErrNotRegular)
	}
	h, n, err := hashReader(f)
	if err != nil {
		return Hash{}, 0, fmt.Errorf("%s: %w", path, err)
	}
	return h, n, nil
}

// hashReader hashes r to EOF.
func hashReader(r io.Reader) (Hash, int64, error) {
	hs := sha256.New()
	n, err := io.Copy(hs, r)
	if err != nil {
		return Hash{}, n, err
	}
	var h Hash
	copy(h[:], hs.Sum(nil))
	return h, n, nil
}

// sumFile rewinds f and hashes it to EOF.
func sumFile(f *os.File) (Hash, error) {
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return Hash{}, err
	}
	h, _, err := hashReader(f)
	return h, err
}
