//go:build linux

package secureexec

import (
	"errors"
	"syscall"
	"testing"
)

// TestMemfdSealedAndRuns checks that the default Linux mechanism is a sealed
// memfd and that the child runs from it. CI-only: the host is macOS.
func TestMemfdSealedAndRuns(t *testing.T) {
	src, want := victimCopy(t, "VICTIM-MEMFD", t.TempDir())
	v, err := Verify(src, want)
	if err != nil {
		t.Fatal(err)
	}
	defer v.Close()
	if v.Mechanism() != MechanismMemfd {
		t.Logf("memfd not used on this kernel or policy (mechanism %s); seal checks skipped", v.Mechanism())
		if got := runVerified(t, v, "m"); len(got) == 0 || got[0] != "VICTIM-MEMFD" {
			t.Fatalf("child output = %q", got)
		}
		return
	}
	mem := v.p.extra[0]
	if _, err := mem.Write([]byte{0}); !errors.Is(err, syscall.EPERM) {
		t.Fatalf("write to sealed memfd: err = %v, want EPERM", err)
	}
	if err := mem.Truncate(0); !errors.Is(err, syscall.EPERM) {
		t.Fatalf("truncate of sealed memfd: err = %v, want EPERM", err)
	}
	seals, _, errno := syscall.Syscall(syscall.SYS_FCNTL, mem.Fd(), fGetSeals, 0)
	if errno != 0 {
		t.Fatalf("F_GET_SEALS: %v", errno)
	}
	const need = fSealWrite | fSealShrink | fSealGrow | fSealSeal
	if int(seals)&need != need {
		t.Fatalf("seals = %#x, want at least %#x", seals, need)
	}
	if got := runVerified(t, v, "m"); len(got) == 0 || got[0] != "VICTIM-MEMFD" {
		t.Fatalf("child output = %q", got)
	}
}
