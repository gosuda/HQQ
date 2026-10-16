//go:build linux

package shm

import (
	"fmt"
	"os"
	"syscall"
)

// shmDir is where glibc's shm_open places POSIX shared memory objects.
const shmDir = "/dev/shm/"

// NamesPersist reports whether names outlive their handles on this platform,
// so that a name left behind needs Unlink (everywhere except Windows).
const NamesPersist = true

// errNotRegular reports a name that refers to something other than a regular
// file (a symlink, a device, a FIFO, a directory). It matches ErrInvalidName.
// Such objects are never opened, mapped or removed: /dev/shm is typically
// world-writable, so a name there can be planted by another user.
var errNotRegular = fmt.Errorf("%w: not a regular shared memory object", ErrInvalidName)

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
//
// The name is opened with O_NOFOLLOW and the opened object must be a regular
// file; otherwise the error matches ErrInvalidName and nothing is modified.
// A created object is allocated with fallocate, so a full tmpfs fails with
// ENOSPC here instead of raising SIGBUS on first touch of a mapped page. The
// allocation falls back to ftruncate only where the file system does not
// support fallocate (EOPNOTSUPP, ENOSYS). A failed creation removes the
// object again.
func OpenSharedMemory(name string, size int, flags int, mode os.FileMode) (*SharedMemory, error) {
	path, err := shmPath(name)
	if err != nil {
		return nil, err
	}
	fd, err := syscall.Open(path, flags|syscall.O_CLOEXEC|syscall.O_NOFOLLOW, uint32(mode.Perm()))
	if err != nil {
		if err == syscall.ELOOP {
			err = errNotRegular
		}
		return nil, &os.PathError{Op: "shm_open", Path: name, Err: err}
	}
	created := flags&(os.O_CREATE|os.O_EXCL) == os.O_CREATE|os.O_EXCL
	var st syscall.Stat_t
	if err := syscall.Fstat(fd, &st); err != nil {
		_ = syscall.Close(fd)
		if created {
			_ = syscall.Unlink(path)
		}
		return nil, &os.PathError{Op: "fstat", Path: name, Err: err}
	}
	if st.Mode&syscall.S_IFMT != syscall.S_IFREG {
		_ = syscall.Close(fd)
		return nil, &os.PathError{Op: "shm_open", Path: name, Err: errNotRegular}
	}
	if flags&os.O_CREATE != 0 {
		if err := allocate(fd, int64(size)); err != nil {
			_ = syscall.Close(fd)
			if created {
				// We created the object; do not leave it behind.
				_ = syscall.Unlink(path)
			}
			return nil, &os.PathError{Op: "fallocate", Path: name, Err: err}
		}
	}
	return &SharedMemory{name: name, size: size, fd: uintptr(fd)}, nil
}

// allocate makes the first size bytes of fd backed by storage and sets the
// file size to at least size.
func allocate(fd int, size int64) error {
	for {
		err := syscall.Fallocate(fd, 0, 0, size)
		switch err {
		case syscall.EINTR:
			continue
		case syscall.EOPNOTSUPP, syscall.ENOSYS:
			return syscall.Ftruncate(fd, size)
		}
		return err
	}
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

// NameRefersToSelf reports whether the name still refers to the very object s
// is open on (same device and inode). known is always true on Linux. A name
// that no longer exists, or that was replaced by another object, reports
// same == false.
func (s *SharedMemory) NameRefersToSelf() (known, same bool, err error) {
	path, err := shmPath(s.name)
	if err != nil {
		return true, false, err
	}
	var ours, current syscall.Stat_t
	if err := syscall.Fstat(int(s.fd), &ours); err != nil {
		return true, false, &os.PathError{Op: "fstat", Path: s.name, Err: err}
	}
	if err := syscall.Lstat(path, &current); err != nil {
		if err == syscall.ENOENT {
			return true, false, nil
		}
		return true, false, &os.PathError{Op: "lstat", Path: s.name, Err: err}
	}
	return true, uint64(ours.Dev) == uint64(current.Dev) && uint64(ours.Ino) == uint64(current.Ino), nil
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
