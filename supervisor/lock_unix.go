//go:build unix

package supervisor

import (
	"os"
	"syscall"
)

// fileLock is an advisory exclusive lock on a file, held with flock(2).
type fileLock struct {
	path string
	f    *os.File
}

// acquireLock takes the exclusive lock on path without blocking. It returns
// ErrLockHeld when another open file description holds it.
func acquireLock(path string) (*fileLock, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		if err == syscall.EWOULDBLOCK || err == syscall.EAGAIN {
			return nil, ErrLockHeld
		}
		return nil, err
	}
	return &fileLock{path: path, f: f}, nil
}

// release drops the lock and closes the file.
func (l *fileLock) release() {
	_ = syscall.Flock(int(l.f.Fd()), syscall.LOCK_UN)
	_ = l.f.Close()
}
