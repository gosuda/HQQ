//go:build linux

package secureexec

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"syscall"
	"unsafe"
)

// Constants missing from the std syscall package on most GOARCH values
// (values from linux/include/uapi/linux/{memfd,fcntl}.h).
const (
	mfdCloexec      = 0x1
	mfdAllowSealing = 0x2
	mfdExec         = 0x10 // Linux 6.3+; older kernels return EINVAL

	fGetSeals   = 0x40a
	fAddSeals   = 0x409
	fSealSeal   = 0x1
	fSealShrink = 0x2
	fSealGrow   = 0x4
	fSealWrite  = 0x8
	fSealExec   = 0x20 // Linux 6.3+; older kernels return EINVAL

	memfdName = "hqq-secureexec"

	// memfdExecPath is what the child execs. Command puts the memfd as the
	// only ExtraFiles entry, so the child sees it as fd 3. The path is resolved
	// in the child after dup2, so it names the child's own descriptor.
	memfdExecPath = "/proc/self/fd/3"

	// procNoexecSysctl is vm.memfd_noexec. Value 2 means memfds may not be
	// executed, so this mechanism cannot work.
	procNoexecSysctl = "/proc/sys/vm/memfd_noexec"
)

// memfdCreate calls memfd_create(2) and returns the descriptor.
func memfdCreate(name string, flags uintptr) (int, error) {
	p, err := syscall.BytePtrFromString(name)
	if err != nil {
		return -1, err
	}
	r, _, errno := syscall.Syscall(sysMemfdCreate, uintptr(unsafe.Pointer(p)), flags, 0)
	if errno != 0 {
		return -1, errno
	}
	return int(r), nil
}

// fcntlArg calls fcntl(2) with an integer argument.
func fcntlArg(fd int, cmd, arg uintptr) error {
	_, _, errno := syscall.Syscall(syscall.SYS_FCNTL, uintptr(fd), cmd, arg)
	if errno != 0 {
		return errno
	}
	return nil
}

// memfdUnavailable reports errors that mean "this mechanism is not usable here"
// (old kernel, seccomp filter, LSM denial), as opposed to real failures.
func memfdUnavailable(err error) bool {
	return errors.Is(err, syscall.ENOSYS) ||
		errors.Is(err, syscall.EPERM) ||
		errors.Is(err, syscall.EACCES) ||
		errors.Is(err, syscall.EINVAL)
}

// protectMemfd copies path into a sealed memfd and hashes the sealed bytes.
// It returns (nil, nil) when memfd or /proc is unavailable, so that the vault
// is used instead.
func protectMemfd(path string) (*protected, error) {
	if !memfdArchSupported {
		return nil, nil
	}
	if _, err := os.Stat("/proc/self/fd"); err != nil {
		return nil, nil
	}
	src, _, err := openSource(path)
	if err != nil {
		return nil, err
	}
	defer src.Close()

	fd, err := memfdCreate(memfdName, mfdCloexec|mfdAllowSealing|mfdExec)
	if errors.Is(err, syscall.EINVAL) {
		// Kernels before 6.3 reject MFD_EXEC.
		fd, err = memfdCreate(memfdName, mfdCloexec|mfdAllowSealing)
	}
	if err != nil {
		if memfdUnavailable(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("secureexec: memfd_create: %w", err)
	}
	mem := os.NewFile(uintptr(fd), memfdName)
	fail := func(err error) (*protected, error) {
		_ = mem.Close()
		return nil, err
	}
	if policy, err := os.ReadFile(procNoexecSysctl); err == nil && strings.TrimSpace(string(policy)) == "2" {
		// memfds are not executable on this system; use the vault.
		_ = mem.Close()
		return nil, nil
	}
	if _, err := io.Copy(mem, src); err != nil {
		return fail(fmt.Errorf("secureexec: copy to memfd: %w", err))
	}
	// Seal before hashing: after this the bytes cannot change, so the digest
	// is the digest of what exec will read.
	err = fcntlArg(fd, fAddSeals, fSealWrite|fSealShrink|fSealGrow|fSealExec|fSealSeal)
	if errors.Is(err, syscall.EINVAL) {
		// Kernels before 6.3 do not know F_SEAL_EXEC.
		err = fcntlArg(fd, fAddSeals, fSealWrite|fSealShrink|fSealGrow|fSealSeal)
	}
	if err != nil {
		return fail(fmt.Errorf("secureexec: seal memfd: %w", err))
	}
	h, err := sumFile(mem)
	if err != nil {
		return fail(err)
	}
	return &protected{
		mechanism: MechanismMemfd,
		execPath:  memfdExecPath,
		extra:     []*os.File{mem},
		hash:      h,
		release:   mem.Close,
	}, nil
}
