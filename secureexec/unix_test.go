//go:build unix

package secureexec

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestSymlinkFinalComponentRefused(t *testing.T) {
	src, want := victimCopy(t, "VICTIM-LINK", t.TempDir())
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(src, link); err != nil {
		t.Fatal(err)
	}
	for _, opts := range [][]Option{
		nil,
		{WithMechanism(MechanismVault), WithVaultDir(t.TempDir())},
		{WithPolicy(Policy{TrustOwnedReadOnly: true})},
	} {
		v, err := Verify(link, want, opts...)
		if err == nil {
			v.Close()
			t.Fatalf("symlink source accepted with opts %d", len(opts))
		}
		if !errors.Is(err, syscall.ELOOP) {
			t.Fatalf("err = %v, want ELOOP", err)
		}
	}
}

func TestVaultLayoutAndPermissions(t *testing.T) {
	src, want := victimCopy(t, "VICTIM-VAULT", t.TempDir())
	base := filepath.Join(t.TempDir(), "vaults")
	v, err := Verify(src, want, WithMechanism(MechanismVault), WithVaultDir(base))
	if err != nil {
		t.Fatal(err)
	}
	if v.Mechanism() != MechanismVault {
		t.Fatalf("mechanism = %s", v.Mechanism())
	}
	exe := v.p.execPath
	dir := filepath.Dir(exe)
	if filepath.Dir(dir) != base {
		t.Fatalf("vault %s is not directly under %s", dir, base)
	}
	di, err := os.Lstat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !di.IsDir() || di.Mode().Perm() != 0o700 {
		t.Fatalf("vault dir mode = %v, want directory 0700", di.Mode())
	}
	fi, err := os.Lstat(exe)
	if err != nil {
		t.Fatal(err)
	}
	if !fi.Mode().IsRegular() || fi.Mode().Perm() != 0o500 {
		t.Fatalf("vault file mode = %v, want regular 0500", fi.Mode())
	}
	if os.Geteuid() != 0 {
		if f, err := os.OpenFile(exe, os.O_WRONLY, 0); err == nil {
			f.Close()
			t.Fatal("owner could open the vault copy for writing")
		}
	}
	if got := runVerified(t, v, "v"); len(got) == 0 || got[0] != "VICTIM-VAULT" {
		t.Fatalf("vault child output = %q", got)
	}
	if err := v.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(dir); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("vault dir still present after Close: %v", err)
	}
	if entries, _ := os.ReadDir(base); len(entries) != 0 {
		t.Fatalf("base not empty after Close: %d entries", len(entries))
	}
}

func TestVaultRemovedOnMismatch(t *testing.T) {
	src, _ := victimCopy(t, "VICTIM-VAULT-MISMATCH", t.TempDir())
	base := filepath.Join(t.TempDir(), "vaults")
	if _, err := Verify(src, Hash{1}, WithMechanism(MechanismVault), WithVaultDir(base)); !errors.Is(err, ErrHashMismatch) {
		t.Fatalf("err = %v, want ErrHashMismatch", err)
	}
	if entries, _ := os.ReadDir(base); len(entries) != 0 {
		t.Fatalf("mismatch left %d vault entries", len(entries))
	}
}

func TestVaultStartCleansUpAfterExit(t *testing.T) {
	src, want := victimCopy(t, "VICTIM-VAULT-START", t.TempDir())
	base := filepath.Join(t.TempDir(), "vaults")
	p, err := Start(context.Background(), src, want, nil, WithMechanism(MechanismVault), WithVaultDir(base))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Wait(); err != nil {
		t.Fatal(err)
	}
	if entries, _ := os.ReadDir(base); len(entries) != 0 {
		t.Fatalf("vault left after child exit: %d entries", len(entries))
	}
}

func TestVaultBaseMissingIsCreatedPrivate(t *testing.T) {
	src, want := victimCopy(t, "VICTIM-VAULT-BASE", t.TempDir())
	base := filepath.Join(t.TempDir(), "new", "vaults")
	v, err := Verify(src, want, WithMechanism(MechanismVault), WithVaultDir(base))
	if err != nil {
		t.Fatal(err)
	}
	defer v.Close()
	fi, err := os.Lstat(base)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o700 {
		t.Fatalf("created base mode = %v, want 0700", fi.Mode().Perm())
	}
}

func TestVaultRefusesSymlinkBase(t *testing.T) {
	src, want := victimCopy(t, "VICTIM-VAULT-SYMLINK", t.TempDir())
	target := t.TempDir()
	link := filepath.Join(t.TempDir(), "vaults")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if v, err := Verify(src, want, WithMechanism(MechanismVault), WithVaultDir(link)); err == nil {
		v.Close()
		t.Fatal("symlinked vault base accepted")
	}
	if entries, _ := os.ReadDir(target); len(entries) != 0 {
		t.Fatalf("vault created through a symlink: %d entries", len(entries))
	}
}

func TestVaultRefusesGroupWritableBase(t *testing.T) {
	src, want := victimCopy(t, "VICTIM-VAULT-GW", t.TempDir())
	base := filepath.Join(t.TempDir(), "vaults")
	if err := os.Mkdir(base, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(base, 0o775); err != nil {
		t.Fatal(err)
	}
	if v, err := Verify(src, want, WithMechanism(MechanismVault), WithVaultDir(base)); err == nil {
		v.Close()
		t.Fatal("group-writable vault base accepted")
	}
	if entries, _ := os.ReadDir(base); len(entries) != 0 {
		t.Fatalf("entries created in refused base: %d", len(entries))
	}
}

func TestCleanVaults(t *testing.T) {
	src, want := victimCopy(t, "VICTIM-CLEAN", t.TempDir())
	base := filepath.Join(t.TempDir(), "vaults")
	// Two vaults that are never closed, as after a crashed supervisor.
	stale, err := Verify(src, want, WithMechanism(MechanismVault), WithVaultDir(base))
	if err != nil {
		t.Fatal(err)
	}
	fresh, err := Verify(src, want, WithMechanism(MechanismVault), WithVaultDir(base))
	if err != nil {
		t.Fatal(err)
	}
	staleDir := filepath.Dir(stale.p.execPath)
	freshDir := filepath.Dir(fresh.p.execPath)
	old := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(staleDir, old, old); err != nil {
		t.Fatal(err)
	}
	// A foreign directory must never be touched.
	foreign := filepath.Join(base, "keep-me")
	if err := os.Mkdir(foreign, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(foreign, old, old); err != nil {
		t.Fatal(err)
	}

	n, err := CleanVaults(base, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("removed %d vaults, want 1", n)
	}
	if _, err := os.Lstat(staleDir); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("stale vault still present: %v", err)
	}
	if _, err := os.Lstat(freshDir); err != nil {
		t.Fatalf("fresh vault removed: %v", err)
	}
	if _, err := os.Lstat(foreign); err != nil {
		t.Fatalf("foreign directory removed: %v", err)
	}
	if _, err := CleanVaults(base, 0); err == nil {
		t.Fatal("CleanVaults accepted olderThan 0")
	}
	if n, err := CleanVaults(filepath.Join(t.TempDir(), "absent"), time.Hour); err != nil || n != 0 {
		t.Fatalf("CleanVaults on absent base = %d, %v", n, err)
	}
	if err := fresh.Close(); err != nil {
		t.Fatal(err)
	}
	if err := stale.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestTrustedPathRefusesGroupWritableDirectory(t *testing.T) {
	src, want := victimCopy(t, "VICTIM-TRUST-NO", t.TempDir())
	dir := filepath.Join(t.TempDir(), "open")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o777); err != nil {
		t.Fatal(err)
	}
	trusted := filepath.Join(dir, "tool")
	copyExecutable(t, src, trusted)
	v, err := Verify(trusted, want, WithPolicy(Policy{TrustOwnedReadOnly: true}))
	if err != nil {
		t.Fatal(err)
	}
	defer v.Close()
	if v.Mechanism() == MechanismTrustedPath {
		t.Fatal("trusted path used although the directory is world-writable")
	}
}

func TestTrustedPathPolicy(t *testing.T) {
	src, want := victimCopy(t, "VICTIM-TRUST", t.TempDir())
	v, err := Verify(src, want, WithPolicy(Policy{TrustOwnedReadOnly: true}))
	if err != nil {
		t.Fatal(err)
	}
	defer v.Close()
	if v.Mechanism() != MechanismTrustedPath {
		// Ancestors of t.TempDir may not be trusted on every host; the fallback is
		// still correct, so only report it.
		t.Logf("trusted path not applicable here; mechanism = %s", v.Mechanism())
		return
	}
	if got := runVerified(t, v, "t"); len(got) == 0 || got[0] != "VICTIM-TRUST" {
		t.Fatalf("trusted child output = %q", got)
	}
}
