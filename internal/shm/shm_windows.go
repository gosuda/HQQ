//go:build windows

package shm

import (
	"os"
	"runtime"
	"strings"
	"syscall"
	"unsafe"
)

var kernel32 = syscall.NewLazyDLL("Kernel32.dll")
var _CreateFileMappingW = kernel32.NewProc("CreateFileMappingW")
var _OpenFileMappingW = kernel32.NewProc("OpenFileMappingW")

const ERROR_ALREADY_EXISTS syscall.Errno = 183

const (
	fileMapWrite = 0x0002
	fileMapRead  = 0x0004
)

func nsCreateFileMapping(hFile syscall.Handle, lpFileMappingAttributes uintptr, flProtect uint32, MaximumSize uint64, lpName string) (syscall.Handle, bool, error) {
	b, err := syscall.UTF16PtrFromString(lpName)
	if err != nil {
		return syscall.InvalidHandle, false, err
	}

	high := uint32(MaximumSize >> 32)
	low := uint32(MaximumSize & 0xffffffff)

	ret, _, err := _CreateFileMappingW.Call(
		uintptr(hFile),
		lpFileMappingAttributes,
		uintptr(flProtect),
		uintptr(high),
		uintptr(low),
		uintptr(unsafe.Pointer(b)),
	)
	runtime.KeepAlive(b)

	if ret == 0 {
		if err == syscall.Errno(0) {
			err = syscall.EINVAL
		}
		return syscall.InvalidHandle, false, err
	}

	return syscall.Handle(ret), err == ERROR_ALREADY_EXISTS, nil
}

func nsOpenFileMapping(dwDesiredAccess uint32, bInheritHandle bool, lpName string) (syscall.Handle, error) {
	b, err := syscall.UTF16PtrFromString(lpName)
	if err != nil {
		return syscall.InvalidHandle, err
	}

	inherit := 0
	if bInheritHandle {
		inherit = 1
	}

	ret, _, err := _OpenFileMappingW.Call(
		uintptr(dwDesiredAccess),
		uintptr(inherit),
		uintptr(unsafe.Pointer(b)),
	)
	runtime.KeepAlive(b)

	if ret == 0 {
		if err == syscall.Errno(0) {
			err = syscall.EINVAL
		}
		return syscall.InvalidHandle, err
	}

	return syscall.Handle(ret), nil
}

// OpenSharedMemory opens or creates the named file mapping name. Names
// without a namespace prefix are created in the session-local "Local\"
// namespace, which does not require SeCreateGlobalPrivilege. Pass an explicit
// "Global\name" to share a mapping across sessions (for example between a
// service and a desktop application). mode is ignored on Windows.
func OpenSharedMemory(name string, size int, flags int, mode os.FileMode) (*SharedMemory, error) {
	s := &SharedMemory{
		name: name,
		size: size,
	}

	_ = mode // Windows file mappings use security descriptors, not modes.
	objectName, err := windowsSharedMemoryName(name)
	if err != nil {
		return nil, err
	}

	if flags&os.O_CREATE != 0 {
		prot := 0
		if flags&os.O_RDWR != 0 {
			prot = syscall.PAGE_READWRITE
		} else {
			prot = syscall.PAGE_READONLY
		}

		fd, alreadyExists, err := nsCreateFileMapping(
			syscall.InvalidHandle,
			0,
			uint32(prot),
			uint64(size),
			objectName,
		)
		if err != nil {
			return nil, &os.PathError{Op: "CreateFileMapping", Path: name, Err: err}
		}
		if alreadyExists && flags&os.O_EXCL != 0 {
			_ = syscall.CloseHandle(fd)
			return nil, &os.PathError{Op: "CreateFileMapping", Path: name, Err: os.ErrExist}
		}
		s.fd = uintptr(fd)
		return s, nil
	}

	access := uint32(fileMapRead)
	if flags&os.O_RDWR != 0 {
		access |= fileMapWrite
	}
	fd, err := nsOpenFileMapping(
		access,
		false,
		objectName,
	)
	if err != nil {
		return nil, &os.PathError{Op: "OpenFileMapping", Path: name, Err: err}
	}
	s.fd = uintptr(fd)

	return s, nil
}

// Unlink is a no-op on Windows: a file mapping object is destroyed when its
// last handle is closed.
func Unlink(name string) error {
	_, err := windowsSharedMemoryName(name)
	return err
}

// Delete is a no-op on Windows; see Unlink.
func (s *SharedMemory) Delete() error {
	return nil
}

// CurrentSize returns -1: a Windows file mapping is created with its final
// size atomically, so there is no partially sized state to detect.
func (s *SharedMemory) CurrentSize() (int64, error) {
	return -1, nil
}

// Close closes the mapping handle. Views stay valid until unmapped.
func (s *SharedMemory) Close() error {
	return syscall.CloseHandle(syscall.Handle(s.fd))
}

// windowsSharedMemoryName maps a portable name to a kernel object name.
// "Global\x" and "Local\x" are kept; other names get the "Local\" prefix.
func windowsSharedMemoryName(name string) (string, error) {
	for _, prefix := range []string{`Global\`, `Local\`} {
		if len(name) > len(prefix) && strings.EqualFold(name[:len(prefix)], prefix) {
			base, err := baseName(name[len(prefix):], 0)
			if err != nil {
				return "", err
			}
			return prefix + base, nil
		}
	}
	base, err := baseName(name, 0)
	if err != nil {
		return "", err
	}
	return `Local\` + base, nil
}
