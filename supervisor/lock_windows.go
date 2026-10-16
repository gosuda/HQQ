//go:build windows

package supervisor

import (
	"os"
	"syscall"
	"unsafe"
)

var (
	kernel32         = syscall.NewLazyDLL("Kernel32.dll")
	procLockFileEx   = kernel32.NewProc("LockFileEx")
	procUnlockFileEx = kernel32.NewProc("UnlockFileEx")
)

const (
	lockfileFailImmediately = 0x00000001
	lockfileExclusiveLock   = 0x00000002
	errorLockViolation      = syscall.Errno(33)
)

// fileLock is an exclusive byte-range lock on the first byte of a file, taken
// with LockFileEx. The lock is released when the handle is closed.
type fileLock struct {
	path string
	f    *os.File
}

// acquireLock takes the exclusive lock on path without blocking. It returns
// ErrLockHeld when another handle holds it.
func acquireLock(path string) (*fileLock, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	var ol syscall.Overlapped
	r1, _, e1 := procLockFileEx.Call(f.Fd(), lockfileExclusiveLock|lockfileFailImmediately, 0, 1, 0, uintptr(unsafe.Pointer(&ol)))
	if r1 == 0 {
		_ = f.Close()
		if e1 == errorLockViolation {
			return nil, ErrLockHeld
		}
		return nil, e1
	}
	return &fileLock{path: path, f: f}, nil
}

// release unlocks and closes the file.
func (l *fileLock) release() {
	var ol syscall.Overlapped
	_, _, _ = procUnlockFileEx.Call(l.f.Fd(), 0, 1, 0, uintptr(unsafe.Pointer(&ol)))
	_ = l.f.Close()
}
