package secureexec

import (
	"errors"
	"fmt"
)

var (
	// ErrHashMismatch matches (errors.Is) any *HashMismatchError.
	ErrHashMismatch = errors.New("secureexec: sha256 mismatch")

	// ErrNotRegular is returned for sources that are not regular files
	// (directories, devices, FIFOs, sockets).
	ErrNotRegular = errors.New("secureexec: not a regular file")

	// ErrUnsupported is returned when the platform or the requested
	// mechanism cannot provide the guarantees of this package.
	ErrUnsupported = errors.New("secureexec: unsupported platform or mechanism")
)

// HashMismatchError reports that the bytes that would run do not have the
// expected SHA-256 digest. Got is the digest of the protected copy.
type HashMismatchError struct {
	Path string
	Got  Hash
	Want Hash
}

func (e *HashMismatchError) Error() string {
	return fmt.Sprintf("secureexec: %s: sha256 mismatch: got %s, want %s", e.Path, e.Got, e.Want)
}

// Is makes errors.Is(err, ErrHashMismatch) report true.
func (e *HashMismatchError) Is(target error) bool {
	return target == ErrHashMismatch
}
