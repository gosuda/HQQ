//go:build unix || linux || darwin || freebsd || openbsd

package mmap

import "syscall"

const (
	PROT_READ  = syscall.PROT_READ
	PROT_WRITE = syscall.PROT_WRITE

	MAP_SHARED  = syscall.MAP_SHARED
	MAP_PRIVATE = syscall.MAP_PRIVATE
)

// Map maps len bytes of the object referenced by fd starting at offset.
func Map(fd uintptr, offset int, len int, prot int, flags int) ([]byte, error) {
	return syscall.Mmap(int(fd), int64(offset), len, prot, flags)
}

// UnMap unmaps a region previously returned by Map.
func UnMap(b []byte) error {
	return syscall.Munmap(b)
}
