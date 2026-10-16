//go:build darwin || freebsd

package shm

import (
	"os"
	"runtime"
	"syscall"
	"unsafe"
)

// NamesPersist reports whether names outlive their handles on this platform,
// so that a name left behind needs Unlink (everywhere except Windows).
const NamesPersist = true

// maxNameLen is the platform limit for shm_open names. macOS rejects names
// longer than PSHMNAMLEN (31) bytes; FreeBSD allows up to PATH_MAX.
var maxNameLen = func() int {
	if runtime.GOOS == "darwin" {
		return 31
	}
	return 1023
}()

// posixName validates name and returns it with the leading slash that
// shm_open expects for portable names.
func posixName(name string) (string, error) {
	base, err := baseName(name, maxNameLen)
	if err != nil {
		return "", err
	}
	full := "/" + base
	if len(full) > maxNameLen {
		return "", &os.PathError{Op: "shm_open", Path: name, Err: ErrInvalidName}
	}
	return full, nil
}

func shmOpen(name string, oflag int, mode os.FileMode) (fd int, err error) {
	b, err := syscall.BytePtrFromString(name)
	if err != nil {
		return 0, err
	}

	f, _, errno := syscall.Syscall(
		syscall.SYS_SHM_OPEN,
		uintptr(unsafe.Pointer(b)),
		uintptr(oflag),
		uintptr(mode.Perm()),
	)
	runtime.KeepAlive(b)

	if errno != 0 {
		return 0, errno
	}
	return int(f), nil
}

func shmUnlink(name string) error {
	b, err := syscall.BytePtrFromString(name)
	if err != nil {
		return err
	}

	_, _, errno := syscall.Syscall(
		syscall.SYS_SHM_UNLINK,
		uintptr(unsafe.Pointer(b)),
		0, 0,
	)
	runtime.KeepAlive(b)

	if errno != 0 {
		return errno
	}
	return nil
}

// OpenSharedMemory opens or creates the shared memory object name. flags are
// os.O_* flags; when os.O_CREATE is set the object is sized to size bytes.
//
// A created object is sized with ftruncate, which does not reserve storage
// (unlike the Linux fallocate path), and the name is opened without
// O_NOFOLLOW or a file-type check: shm_open names are not file system paths
// and cannot be symbolic links.
// The descriptor is close-on-exec.
func OpenSharedMemory(name string, size int, flags int, mode os.FileMode) (*SharedMemory, error) {
	full, err := posixName(name)
	if err != nil {
		return nil, err
	}
	fd, err := shmOpen(full, flags, mode)
	if err != nil {
		return nil, &os.PathError{Op: "shm_open", Path: name, Err: err}
	}
	syscall.CloseOnExec(fd)
	if flags&os.O_CREATE != 0 {
		if err := syscall.Ftruncate(fd, int64(size)); err != nil {
			_ = syscall.Close(fd)
			if flags&os.O_EXCL != 0 {
				// We created the object; do not leave it behind.
				_ = shmUnlink(full)
			}
			return nil, &os.PathError{Op: "ftruncate", Path: name, Err: err}
		}
	}
	return &SharedMemory{name: name, size: size, fd: uintptr(fd)}, nil
}

// Unlink removes the shared memory object name. Existing mappings stay valid.
func Unlink(name string) error {
	full, err := posixName(name)
	if err != nil {
		return err
	}
	if err := shmUnlink(full); err != nil {
		return &os.PathError{Op: "shm_unlink", Path: name, Err: err}
	}
	return nil
}

// NameRefersToSelf reports that macOS and FreeBSD offer no way to compare the
// object behind a shm name with an open descriptor (shm_open names have no
// inode identity visible to user space): known is always false. Callers
// must identify their object some other way, for example by a token stored
// in the object itself.
//
// Allocation is unchanged from POSIX: a created object is sized with
// ftruncate, which does not reserve storage (macOS does not support
// fallocate or posix_fallocate on shm objects).
func (s *SharedMemory) NameRefersToSelf() (known, same bool, err error) {
	return false, false, nil
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
