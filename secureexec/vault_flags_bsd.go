//go:build darwin || freebsd

package secureexec

import (
	"os"
	"syscall"
)

// ufImmutable is UF_IMMUTABLE (sys/stat.h) on darwin and freebsd. The std
// syscall package does not export it.
const ufImmutable = 0x2

// setVaultFlags marks the vault copy immutable (best effort: an error such as
// EPERM leaves the vault protected only by its 0700 directory).
func setVaultFlags(f *os.File) {
	_ = syscall.Fchflags(int(f.Fd()), ufImmutable)
}

// clearVaultFlags removes UF_IMMUTABLE so that the file can be deleted.
// Errors are ignored; RemoveAll reports any failure that matters.
func clearVaultFlags(path string) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return
	}
	_ = syscall.Fchflags(int(f.Fd()), 0)
	_ = f.Close()
}
