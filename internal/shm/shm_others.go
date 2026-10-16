//go:build !linux && !freebsd && !darwin && !windows

package shm

import (
	"os"
	"syscall"
)

// OpenSharedMemory is not supported on this platform.
func OpenSharedMemory(name string, size int, flags int, mode os.FileMode) (*SharedMemory, error) {
	if _, err := baseName(name, 0); err != nil {
		return nil, err
	}
	return nil, syscall.ENOSYS
}

// Unlink is not supported on this platform.
func Unlink(name string) error {
	return syscall.ENOSYS
}

// Delete is a no-op on this platform.
func (s *SharedMemory) Delete() error {
	return nil
}

// CurrentSize is not supported on this platform.
func (s *SharedMemory) CurrentSize() (int64, error) {
	return 0, syscall.ENOSYS
}

// Close is a no-op on this platform.
func (s *SharedMemory) Close() error {
	return nil
}
