//go:build unix

package secureexec

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

const (
	vaultDirName    = "hqq-vault"
	vaultRunPrefix  = "run-"
	vaultExecMode   = 0o500
	vaultPrivateDir = 0o700
)

// protect chooses the POSIX mechanism: trusted path (only with the policy),
// then memfd (Linux only), then the vault. A mechanism returns (nil, nil) when
// it is not available here; the next one is tried.
func protect(path string, o *Options) (*protected, error) {
	if o.Policy.TrustOwnedReadOnly {
		p, err := protectTrusted(path)
		if err != nil || p != nil {
			return p, err
		}
	}
	if o.Mechanism != MechanismVault {
		p, err := protectMemfd(path)
		if err != nil || p != nil {
			return p, err
		}
	}
	return protectVault(path, o)
}

// openSource opens path read-only without following a symlink in the final
// component, and requires a regular file. O_NONBLOCK keeps a FIFO from blocking
// the open; the fstat then rejects it.
func openSource(path string) (*os.File, os.FileInfo, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, nil, err
	}
	fi, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, nil, err
	}
	if !fi.Mode().IsRegular() {
		_ = f.Close()
		return nil, nil, fmt.Errorf("%s: %w", path, ErrNotRegular)
	}
	return f, fi, nil
}

// ownedBy reports whether fi is owned by the effective user.
func ownedBy(fi os.FileInfo) bool {
	st, ok := fi.Sys().(*syscall.Stat_t)
	return ok && int(st.Uid) == os.Geteuid()
}

// trustedNode reports whether fi is owned by root or the effective user and is
// not writable by group or other.
func trustedNode(fi os.FileInfo) bool {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok || (st.Uid != 0 && int(st.Uid) != os.Geteuid()) {
		return false
	}
	return fi.Mode().Perm()&0o022 == 0
}

// protectTrusted implements Policy.TrustOwnedReadOnly. It returns (nil, nil)
// when the file or one of its ancestors is not trusted.
func protectTrusted(path string) (*protected, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	// Same rule as openSource: the final component must not be a symlink. Links
	// in the directories above it are resolved below.
	if li, err := os.Lstat(abs); err != nil {
		return nil, err
	} else if li.Mode()&os.ModeSymlink != 0 {
		return nil, &os.PathError{Op: "open", Path: path, Err: syscall.ELOOP}
	}
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return nil, err
	}
	f, fi, err := openSource(real)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if !trustedNode(fi) {
		return nil, nil
	}
	for dir := filepath.Dir(real); ; {
		di, err := os.Lstat(dir)
		if err != nil {
			return nil, err
		}
		if !di.IsDir() || !trustedNode(di) {
			return nil, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	// The path must still name the inode we hashed.
	if now, err := os.Lstat(real); err != nil || !os.SameFile(now, fi) {
		return nil, nil
	}
	h, err := sumFile(f)
	if err != nil {
		return nil, err
	}
	return &protected{mechanism: MechanismTrustedPath, execPath: real, hash: h, release: noRelease}, nil
}

// protectVault copies path into a fresh private directory, checks the copy,
// and returns it as the executable.
func protectVault(path string, o *Options) (*protected, error) {
	src, _, err := openSource(path)
	if err != nil {
		return nil, err
	}
	defer src.Close()

	base, err := vaultBase(o.VaultDir, true)
	if err != nil {
		return nil, err
	}
	dir, err := os.MkdirTemp(base, vaultRunPrefix)
	if err != nil {
		return nil, fmt.Errorf("secureexec: create vault: %w", err)
	}
	fail := func(err error) (*protected, error) {
		_ = removeVault(dir)
		return nil, err
	}
	if err := checkPrivateDir(dir); err != nil {
		return fail(err)
	}

	exe := filepath.Join(dir, executableName(path))
	dst, err := os.OpenFile(exe, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, vaultPrivateDir)
	if err != nil {
		return fail(err)
	}
	if _, err := io.Copy(dst, src); err != nil {
		_ = dst.Close()
		return fail(fmt.Errorf("secureexec: copy to vault: %w", err))
	}
	if err := dst.Sync(); err != nil {
		_ = dst.Close()
		return fail(err)
	}
	if err := dst.Chmod(vaultExecMode); err != nil {
		_ = dst.Close()
		return fail(err)
	}
	setVaultFlags(dst) // best effort
	if err := dst.Close(); err != nil {
		return fail(err)
	}

	// Hash the copy that will run: reopen it without following links and check
	// who owns it and who can write it.
	c, fi, err := openSource(exe)
	if err != nil {
		return fail(err)
	}
	defer c.Close()
	if !ownedBy(fi) || fi.Mode().Perm()&0o022 != 0 {
		return fail(fmt.Errorf("secureexec: vault copy %s: unexpected owner or mode %v", exe, fi.Mode()))
	}
	h, err := sumFile(c)
	if err != nil {
		return fail(err)
	}
	return &protected{
		mechanism: MechanismVault,
		execPath:  exe,
		hash:      h,
		release:   func() error { return removeVault(dir) },
	}, nil
}

// executableName is the base name of the vault copy. The name is cosmetic:
// the path is always built inside the private vault directory.
func executableName(path string) string {
	base := filepath.Base(path)
	if base == "." || base == ".." || base == string(filepath.Separator) {
		return "image"
	}
	return base
}

// checkPrivateDir requires a real directory, owned by us, with mode 0700.
func checkPrivateDir(dir string) error {
	fi, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if !fi.IsDir() || !ownedBy(fi) || fi.Mode().Perm() != vaultPrivateDir {
		return fmt.Errorf("secureexec: vault %s: not a private directory owned by the current user", dir)
	}
	return nil
}

// vaultBase resolves the vault base directory. The base must be a real
// directory (not a symlink) owned by the current user and not writable by
// group or other. When create is set and the base is missing, it is created
// with mode 0700.
func vaultBase(dir string, create bool) (string, error) {
	if dir == "" {
		cache, err := os.UserCacheDir()
		if err != nil {
			cache = os.TempDir()
		}
		dir = filepath.Join(cache, vaultDirName)
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	fi, err := os.Lstat(abs)
	if errors.Is(err, fs.ErrNotExist) && create {
		if err := os.MkdirAll(abs, vaultPrivateDir); err != nil {
			return "", err
		}
		fi, err = os.Lstat(abs)
	}
	if err != nil {
		return "", err
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("secureexec: vault base %s: symlinks are refused", abs)
	}
	if !fi.IsDir() {
		return "", fmt.Errorf("secureexec: vault base %s: not a directory", abs)
	}
	if !ownedBy(fi) || fi.Mode().Perm()&0o022 != 0 {
		return "", fmt.Errorf("secureexec: vault base %s: must be owned by the current user and not writable by group or other", abs)
	}
	return abs, nil
}

// removeVault deletes a vault directory and everything in it, clearing
// file flags first (best effort) so that immutable copies can be removed.
func removeVault(dir string) error {
	if entries, err := os.ReadDir(dir); err == nil {
		for _, e := range entries {
			clearVaultFlags(filepath.Join(dir, e.Name()))
		}
	}
	return os.RemoveAll(dir)
}

// CleanVaults removes vault directories under base (empty selects the default
// base) whose modification time is older than olderThan, and returns how many
// it removed. Only real directories owned by the current user and named
// run-* are touched. Running children keep their images on POSIX; do not clean
// vaults that belong to Commands not yet started.
func CleanVaults(base string, olderThan time.Duration) (int, error) {
	if olderThan <= 0 {
		return 0, errors.New("secureexec: CleanVaults: olderThan must be positive")
	}
	dir, err := vaultBase(base, false)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0, err
	}
	cutoff := time.Now().Add(-olderThan)
	removed := 0
	var firstErr error
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), vaultRunPrefix) {
			continue
		}
		full := filepath.Join(dir, e.Name())
		fi, err := os.Lstat(full)
		if err != nil || !fi.IsDir() || !ownedBy(fi) || fi.ModTime().After(cutoff) {
			continue
		}
		if err := removeVault(full); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		removed++
	}
	return removed, firstErr
}
