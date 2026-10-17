//go:build windows

package secureexec

import (
	"fmt"
	"os"
	"strings"
	"syscall"
	"time"
	"unsafe"
)

var (
	modKernel32                   = syscall.NewLazyDLL("kernel32.dll")
	procGetFinalPathNameByHandleW = modKernel32.NewProc("GetFinalPathNameByHandleW")
)

const (
	// fileNameVolumeDOS asks for a path such as \\?\C:\dir\file.exe.
	fileNameVolumeDOS = 0x0
)

// protect opens path with FILE_SHARE_READ only, so that no other handle can
// write, rename or delete the file while the Verified is open, and hashes
// through that handle. The executable is started by its canonical path.
func protect(path string, o *Options) (*protected, error) {
	if o.Mechanism == MechanismVault {
		return nil, fmt.Errorf("secureexec: vault mechanism on windows: %w", ErrUnsupported)
	}
	if fi, err := os.Stat(path); err != nil {
		return nil, err
	} else if fi.IsDir() {
		return nil, fmt.Errorf("%s: %w", path, ErrNotRegular)
	}
	p16, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	h, err := syscall.CreateFile(p16, syscall.GENERIC_READ, syscall.FILE_SHARE_READ, nil,
		syscall.OPEN_EXISTING, syscall.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	f := os.NewFile(uintptr(h), path) // f owns the handle from here on
	fail := func(err error) (*protected, error) {
		_ = f.Close()
		return nil, err
	}

	var info syscall.ByHandleFileInformation
	if err := syscall.GetFileInformationByHandle(h, &info); err != nil {
		return fail(err)
	}
	if info.FileAttributes&syscall.FILE_ATTRIBUTE_DIRECTORY != 0 {
		return fail(fmt.Errorf("%s: %w", path, ErrNotRegular))
	}
	if info.FileAttributes&syscall.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		return fail(fmt.Errorf("%s: reparse point after resolution: %w", path, ErrUnsupported))
	}
	final, err := finalPath(h)
	if err != nil {
		return fail(fmt.Errorf("%s: %w", path, err))
	}
	sum, err := sumFile(f)
	if err != nil {
		return fail(err)
	}
	return &protected{
		mechanism: MechanismWindowsShareLock,
		execPath:  final,
		hash:      sum,
		release:   f.Close,
	}, nil
}

// finalPath returns the canonical path of the file behind handle h, with the
// drive-letter form of the \\?\ prefix removed.
func finalPath(h syscall.Handle) (string, error) {
	buf := make([]uint16, syscall.MAX_PATH)
	for {
		n, _, err := procGetFinalPathNameByHandleW.Call(uintptr(h),
			uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)),
			fileNameVolumeDOS)
		if n == 0 {
			return "", fmt.Errorf("GetFinalPathNameByHandleW: %w", err)
		}
		if int(n) < len(buf) {
			s := syscall.UTF16ToString(buf[:n])
			if strings.HasPrefix(s, `\\?\`) && len(s) > 6 && s[5] == ':' {
				s = s[4:]
			}
			return s, nil
		}
		buf = make([]uint16, n+1)
	}
}

// CleanVaults is not supported on Windows, which has no vault mechanism.
func CleanVaults(string, time.Duration) (int, error) {
	return 0, fmt.Errorf("secureexec: CleanVaults: %w", ErrUnsupported)
}
