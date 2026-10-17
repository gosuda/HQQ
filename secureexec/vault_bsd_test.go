//go:build darwin || freebsd

package secureexec

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// TestVaultCopyIsImmutable checks the best-effort UF_IMMUTABLE flag on the
// vault copy, and that Close can still remove it.
func TestVaultCopyIsImmutable(t *testing.T) {
	src, want := victimCopy(t, "VICTIM-IMMUTABLE", t.TempDir())
	v, err := Verify(src, want, WithMechanism(MechanismVault), WithVaultDir(filepath.Join(t.TempDir(), "vaults")))
	if err != nil {
		t.Fatal(err)
	}
	exe := v.p.execPath
	fi, err := os.Stat(exe)
	if err != nil {
		t.Fatal(err)
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatal("no Stat_t")
	}
	if st.Flags&ufImmutable == 0 {
		t.Logf("UF_IMMUTABLE not applied on this file system (best effort); flags=%#x", st.Flags)
	}
	if got := runVerified(t, v, "i"); len(got) == 0 || got[0] != "VICTIM-IMMUTABLE" {
		t.Fatalf("child output = %q", got)
	}
	if err := v.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(exe); !os.IsNotExist(err) {
		t.Fatalf("immutable vault copy still present after Close: %v", err)
	}
}
