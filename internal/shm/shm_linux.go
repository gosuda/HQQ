//go:build linux

package shm

import (
	"os"
	"syscall"
)

// shmDir is where glibc's shm_open places POSIX shared memory objects.
const shmDir = "/dev/shm/"

func shmPath(name string) (string, error) {
	base, err := baseName(name, 0)
	if err != nil {
		return "", err
	}
	return shmDir + base, nil
}

// OpenSharedMemory opens or creates the shared memory object name. flags are
// os.O_* flags; when os.O_CREATE is set the object is sized to size bytes.
// The descriptor is close-on-exec.
func OpenSharedMemory(name string, size int, flags int, mode os.FileMode) (*SharedMemory, error) {
	path, err := shmPath(name)
	if err != nil {
		return nil, err
	}
	fd, err := syscall.Open(path, flags|syscall.O_CLOEXEC, uint32(mode.Perm()))
	if err != nil {
		return nil, &os.PathError{Op: "shm_open", Path: name, Err: err}
	}
	if flags&os.O_CREATE != 0 {
		if err := syscall.Ftruncate(fd, int64(size)); err != nil {
			_ = syscall.Close(fd)
			if flags&os.O_EXCL != 0 {
				// We created the object; do not leave it behind.
				_ = syscall.Unlink(path)
			}
			return nil, &os.PathError{Op: "ftruncate", Path: name, Err: err}
		}
	}
	return &SharedMemory{name: name, size: size, fd: uintptr(fd)}, nil
}

// Unlink removes the shared memory object name. Existing mappings stay valid.
func Unlink(name string) error {
	path, err := shmPath(name)
	if err != nil {
		return err
	}
	if err := syscall.Unlink(path); err != nil {
		return &os.PathError{Op: "shm_unlink", Path: name, Err: err}
	}
	return nil
}

// Delete removes the shared memory object's name.
func (s *SharedMemory) Delete() error {
	return Unlink(s.name)
}

// CurrentSize returns the current size of the object (0 while its creator
// has not sized it yet).
func (s *SharedMemory) CurrentSize() (int64, error) {
	var st syscall.Stat_t
	if err := syscall.Fstat(int(s.fd), &st); err != nil {
		return 0, err
	}
	return st.Size, nil
}

// Close closes the descriptor. Mappings stay valid until unmapped.
func (s *SharedMemory) Close() error {
	return syscall.Close(int(s.fd))
}
