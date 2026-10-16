// Package shm provides a minimal cross-platform wrapper around named shared
// memory objects: POSIX shm_open on Linux (/dev/shm), macOS and FreeBSD, and
// named file mappings on Windows.
package shm

import (
	"errors"
	"fmt"
	"strings"
)

// ErrInvalidName reports a shared-memory name that is empty, contains path
// separators or NUL bytes, refers to "." or "..", or exceeds the platform
// limit.
var ErrInvalidName = errors.New("shm: invalid shared memory name")

// SharedMemory represents a shared memory region for inter-process communication
// This structure provides a cross-platform interface for creating and managing
// shared memory segments that can be accessed by multiple processes.
//
// Shared memory is the foundation of HQQ's high-performance IPC, allowing
// processes to exchange data without kernel-mediated copying.
type SharedMemory struct {
	name string  // Name/identifier of the shared memory region
	size int     // Size of the shared memory region in bytes
	fd   uintptr // File descriptor or handle for the shared memory region
}

// Name returns the name/identifier of the shared memory region
// This name is used to identify the shared memory segment across processes
func (s *SharedMemory) Name() string {
	return s.name
}

// Size returns the size of the shared memory region in bytes
// This is the total size allocated for the shared memory segment
func (s *SharedMemory) Size() int {
	return s.size
}

// FD returns the file descriptor or handle for the shared memory region
// The exact type and meaning of this value depends on the platform:
// - On Unix-like systems: file descriptor from shm_open()
// - On Windows: handle from CreateFileMapping()
//
// The handle is passed to mmap.Map to map the region into the process.
func (s *SharedMemory) FD() uintptr {
	return s.fd
}

// baseName validates name and returns it without its optional leading slash.
// maxLen limits the length of the full name as passed by the caller
// (0 means no extra limit).
func baseName(name string, maxLen int) (string, error) {
	if maxLen > 0 && len(name) > maxLen {
		return "", fmt.Errorf("%w: %q is longer than %d bytes", ErrInvalidName, name, maxLen)
	}
	base := strings.TrimPrefix(name, "/")
	switch {
	case base == "":
		return "", fmt.Errorf("%w: empty name", ErrInvalidName)
	case base == "." || base == "..":
		return "", fmt.Errorf("%w: %q", ErrInvalidName, name)
	case strings.ContainsAny(base, "/\\\x00"):
		return "", fmt.Errorf("%w: %q must not contain path separators or NUL", ErrInvalidName, name)
	case len(base) > 255:
		return "", fmt.Errorf("%w: %q is longer than 255 bytes", ErrInvalidName, name)
	}
	return base, nil
}
