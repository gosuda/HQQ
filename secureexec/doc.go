// Package secureexec verifies the SHA-256 digest of a native executable and
// starts it without a time-of-check/time-of-use (TOCTOU) gap. It uses only
// the standard library.
//
// # Usage
//
//	v, err := secureexec.Verify(path, want)   // hash what will run
//	if err != nil { ... }
//	cmd := v.Command(ctx, args...)             // *exec.Cmd; Path is the protected object
//	cmd.Stdout = os.Stdout
//	err = cmd.Start()                          // start while v is still open
//	v.Close()                                  // release the snapshot after Start
//
// Start combines these steps and releases the snapshot when the child exits.
// [HashFile] and [ParseHash] are helpers for tools and configuration.
//
// # Threat model
//
// The verifier runs as an ordinary user: a supervisor or launcher. The
// attacker is another local process or user that can write in the directories
// the binary is read from. Two attacks are stopped:
//
//  1. PATH SWAP. The attacker replaces the directory entry (symlink, rename over,
//     unlink) between the hash and the exec, so that a different file runs.
//  2. IN-PLACE OVERWRITE. The attacker write()s or mmap(MAP_SHARED)-writes into
//     the inode that was hashed before it is executed.
//
// Each mechanism makes the object that is hashed the same object that is
// executed, and prevents other users from changing it. See the per-platform
// table in README.md for what each one guarantees and what it does not.
//
// Out of scope:
//   - root or Administrator attackers, and kernel compromise;
//   - the integrity of the expected digest itself. It must come from a signed
//     manifest (see package gosuda.org/hqq/secureexec/manifest) or a trusted
//     configuration;
//   - scripts with a shebang line. Only native executables are supported. The
//     kernel re-resolves the interpreter path and the interpreter loads files by
//     name, so a script is not protected by the copy or the handle;
//   - dynamic loaders and shared libraries that the executable loads by path
//     after start.
//
// # Scope of the link check
//
// On POSIX, the final path component must not be a symbolic link; Verify
// fails with ELOOP (errors.Is(err, syscall.ELOOP)). Directories along the path
// may be symbolic links. On Windows, symbolic links are resolved and the
// canonical path of the opened file is used.
//
// # Mechanisms
//
// Linux uses a sealed memfd; the file is executed through /proc/self/fd/3
// (MechanismMemfd). The memfd falls back to the vault when memfd or /proc is
// unavailable. Windows opens the file with FILE_SHARE_READ only and keeps the
// handle open (MechanismWindowsShareLock). Other POSIX systems (macOS, FreeBSD,
// ...) use a private vault: a per-run directory with mode 0700 holding a 0500
// copy that is verified after it is written (MechanismVault). Policy
// TrustOwnedReadOnly skips the copy when the file and all its ancestors are
// trusted (MechanismTrustedPath). Platforms without any of these fail with
// ErrUnsupported.
//
// Errors: ErrHashMismatch (with *HashMismatchError carrying Got and Want),
// ErrNotRegular, ErrUnsupported.
package secureexec
