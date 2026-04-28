//go:build windows
// +build windows

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

func nsCreateFileMapping(hFile syscall.Handle, lpFileMappingAttributes uintptr, flProtect uint32, MaximumSize uint64, lpName string) (syscall.Handle, error) {
	b, err := syscall.UTF16PtrFromString(lpName)
	if err != nil {
		return syscall.InvalidHandle, err
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
		return syscall.InvalidHandle, err
	}

	return syscall.Handle(ret), err
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

func OpenSharedMemory(name string, size int, flags int, mode os.FileMode) (*SharedMemory, error) {
	s := &SharedMemory{
		name: name,
		size: size,
	}

	_ = mode // Ignore FileMode
	name = windowsSharedMemoryName(name)

	if flags&os.O_CREATE != 0 {
		prot := 0
		if flags&os.O_RDWR != 0 {
			prot = syscall.PAGE_READWRITE
		} else if flags == os.O_RDONLY {
			prot = syscall.PAGE_READONLY
		}

		fd, err := nsCreateFileMapping(
			syscall.InvalidHandle,
			0,
			uint32(prot),
			uint64(size),
			name,
		)
		if err != nil && err != ERROR_ALREADY_EXISTS {
			return nil, err
		}
		if err == ERROR_ALREADY_EXISTS {
			if flags&os.O_EXCL != 0 {
				_ = syscall.CloseHandle(fd)
				return nil, os.ErrExist
			}
			s.fd = uintptr(fd)
			return s, nil
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
		name,
	)
	if err != nil {
		return nil, err
	}
	s.fd = uintptr(fd)

	return s, nil
}

func (s *SharedMemory) Delete() error {
	return nil // Do nothing
}

func (s *SharedMemory) Close() error {
	return syscall.CloseHandle(syscall.Handle(s.fd))
}

func windowsSharedMemoryName(name string) string {
	name = strings.TrimLeft(name, `/\`)
	return `Global\` + name
}
