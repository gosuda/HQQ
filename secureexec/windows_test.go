//go:build windows

package secureexec

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"syscall"
	"testing"
)

// errorSharingViolation is ERROR_SHARING_VIOLATION (32), missing from package syscall.
const errorSharingViolation syscall.Errno = 32

// TestWindowsShareLockWhileOpen checks that while a Verified is open no other
// handle can write, delete or rename the file. CI-only: the host is macOS.

func TestWindowsShareLockWhileOpen(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("windows only")
	}
	dir := t.TempDir()
	src, want := victimCopy(t, "VICTIM-WIN", dir)
	v, err := Verify(src, want)
	if err != nil {
		t.Fatal(err)
	}
	defer v.Close()
	if v.Mechanism() != MechanismWindowsShareLock {
		t.Fatalf("mechanism = %s", v.Mechanism())
	}

	f, err := os.OpenFile(src, os.O_WRONLY, 0)
	if err == nil {
		f.Close()
		t.Fatal("write open succeeded while the verified handle is open")
	}
	if !errors.Is(err, errorSharingViolation) {
		t.Errorf("write open error = %v, want a sharing violation", err)
	}

	if err := os.Remove(src); err == nil {
		t.Fatal("delete succeeded while the verified handle is open")
	}

	other := filepath.Join(dir, "other")
	copyExecutable(t, helperBinary(t, "VICTIM-WIN-OTHER"), other)
	if err := os.Rename(other, src); err == nil {
		t.Fatal("rename over the verified file succeeded")
	}

	if got := runVerified(t, v, "w"); len(got) == 0 || got[0] != "VICTIM-WIN" {
		t.Fatalf("child output = %q", got)
	}
	if err := v.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(src); err != nil {
		t.Fatalf("delete after Close: %v", err)
	}
}
