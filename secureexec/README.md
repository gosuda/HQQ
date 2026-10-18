# `secureexec`

Package `gosuda.org/hqq/secureexec` verifies the SHA-256 digest of a native executable and launches it without a time-of-check to time-of-use (TOCTOU) window. Built entirely on the Go standard library, `secureexec` works alongside the signed release manifest package (`gosuda.org/hqq/secureexec/manifest`) and the `cmd/hqq-manifest` CLI tool to ensure that child server binaries cannot be swapped or modified between verification and execution.

Package-level API reference and threat-model documentation are also available via `go doc gosuda.org/hqq/secureexec`.

## Threat Model

The verification mechanisms in `secureexec` defend against local unprivileged tampering under the following assumptions:

- **Execution context**: The verifier (launcher) runs as an ordinary unprivileged user process.
- **Adversary**: Another local process or user has write access to the directory or file from which the executable is loaded.
- **Attack 1 (Path swap)**: Between hashing the file and calling `execve` / `CreateProcessW`, the attacker replaces the directory entry (via symlink, `rename`, or `unlink` + create) so that a different binary is executed.
- **Attack 2 (In-place overwrite)**: After the file is hashed, another process modifies the underlying inode in place using `write()` or `mmap(..., MAP_SHARED)` before the kernel maps the image for execution.
- **Defense**: Each platform mechanism isolates or locks the byte stream before computing the SHA-256 digest and executes directly from that sealed snapshot or locked handle, neutralizing both path swaps and in-place writes.

The following threats are outside the scope of `secureexec`:

- **Root or Administrator attackers and kernel compromise**: A privileged attacker who can ptrace the parent process, modify kernel memory, or override mandatory file locks can bypass user-space verification.
- **Authenticity of the expected digest**: `secureexec` checks that the binary matches the caller-supplied SHA-256 hash. Callers must obtain that hash from a trusted source or verify a detached Ed25519 signature first using `gosuda.org/hqq/secureexec/manifest`.
- **Shebang (`#!`) scripts**: Only native binaries (`ELF`, `Mach-O`, `PE`) are supported. When executing a script, the OS kernel re-opens the script path by name in the interpreter process, re-introducing a TOCTOU window that neither file descriptors nor share locks can close.
- **Dynamic libraries loaded by path**: Shared libraries (`LD_LIBRARY_PATH`, `@rpath`, DLL search order) opened by the dynamic linker after `exec` are not hashed by `secureexec`. Production HQQ server binaries link their dependencies statically.

## Platform Mechanisms

`Verify` selects the strongest mechanism supported by the host operating system, and `Verified.Mechanism()` reports the active mechanism name:

| Platform | Mechanism (`Mechanism()`) | How the Bytes Are Protected | Guaranteed | Not Guaranteed |
| --- | --- | --- | --- | --- |
| Linux (`amd64`, `arm64`, `386`, `arm`, `riscv64`, `ppc64`/`le`, `s390x`, `loong64`, `mips*`) | `memfd` | Opens the source with `O_RDONLY\|O_CLOEXEC\|O_NOFOLLOW\|O_NONBLOCK`, checks `fstat` for a regular file, and copies the contents once into an anonymous file created via `memfd_create(MFD_CLOEXEC\|MFD_ALLOW_SEALING[\|MFD_EXEC])`. Applies `F_SEAL_WRITE\|F_SEAL_SHRINK\|F_SEAL_GROW\|F_SEAL_SEAL` (plus `F_SEAL_EXEC` when supported), hashes the sealed `memfd`, and executes the child via `/proc/self/fd/3` (`ExtraFiles[0]`). | The executed bytes equal the hashed bytes against any non-root user, including processes running under the **same UID**, because kernel seals cannot be removed and the `memfd` has no filesystem path. Automatically falls back to `vault` if `/proc/self/fd` is unavailable, `memfd_create` returns `ENOSYS`/`EPERM`/`EACCES`/`EINVAL`, or `vm.memfd_noexec=2`. | Consumes RAM equal to the binary size while the snapshot or child image is active. If a strict kernel policy blocks `execve` on `memfd`, callers can pass `WithMechanism(MechanismVault)`. `fs-verity` checks are not yet implemented. |
| macOS, FreeBSD, and other POSIX systems (`unix` build tag, non-Linux) | `vault` | Creates a per-run directory via `os.MkdirTemp(base, "run-")` with mode `0700` (verified via `Lstat` to be a real directory owned by the current UID with mode `0700`). Copies the binary into a new file (`O_EXCL\|O_NOFOLLOW`), calls `fsync` and `chmod 0500`, and sets `UF_IMMUTABLE` best-effort on Darwin and FreeBSD. Reopens the copy, verifies ownership and absence of group/other write bits, checks the SHA-256 digest, and executes the copy by path. | Protects against all **other local users**: no other UID can traverse, rename, or write inside the `0700` vault directory, so the hashed copy is the executed copy. | Verifies the vault base directory itself (not a symlink, owned by current UID, not group/other writable), but does not walk ancestor directories. Another process running under the **same UID** can clear `UF_IMMUTABLE` / `chmod` and modify the copy. A `SIGKILL` of the parent during verification can leave a stale directory behind (clean up with `CleanVaults`). Requires temporary disk space equal to the binary size. |
| Windows | `windows-share-lock` | Opens the binary via `CreateFileW(GENERIC_READ, FILE_SHARE_READ)` without `FILE_SHARE_WRITE` or `FILE_SHARE_DELETE`. Retains the open handle, hashes the file through the handle, rejects reparse points after resolution, queries the canonical path via `GetFinalPathNameByHandleW` (stripping the `\\?\` prefix), and calls `CreateProcessW` on that canonical path while the read-only share lock remains held. Releases the handle when `Close` is called or when the child exits (`Start`). | While `Verified` remains open, the Windows kernel rejects any concurrent open for write, delete, or rename with `ERROR_SHARING_VIOLATION`, and the mapped image section blocks modifications while the child process runs. | Symbolic links are resolved to their target before locking rather than rejected. The share lock lasts only while `Verified` is open; calling `Close` before `cmd.Start()` is a caller bug. |
| Other platforms (`plan9`, `js`, `wasip1`, etc.) | none | `Verify` returns `ErrUnsupported`. | None | None |

For environments where binaries are installed into root-owned, read-only system directories and copying the binary is undesirable, callers can opt into `Policy{TrustOwnedReadOnly: true}` via `WithPolicy`. Under this `trusted-path` policy, `Verify` resolves symlinks via `EvalSymlinks` and checks that the target file and every ancestor directory up to `/` are owned by `root` or the current UID and have neither group-write nor world-write permissions. If that check succeeds, `Verify` hashes the open file descriptor and executes the binary in place without copying; if any directory fails the check, `Verify` falls back to the platform default (`memfd` or `vault`). Note that `trusted-path` trusts processes running under the same UID not to modify files they own between hashing and execution.

### Symbolic Link Handling

Symbolic links are handled according to platform filesystem semantics:

- **POSIX (`linux`, `darwin`, `freebsd`, etc.)**: The final path component passed to `Verify` must not be a symbolic link. If the leaf is a symlink, `Verify` fails with `syscall.ELOOP` (`errors.Is(err, syscall.ELOOP)`) across all mechanisms, including `trusted-path`. Symbolic links in parent directories above the final path component are permitted.
- **Windows**: Symbolic links are resolved by `CreateFileW`, the resulting handle is checked to ensure no unresolved reparse point remains, and the canonical path returned by `GetFinalPathNameByHandleW` is executed.

## Error Sentinels

Verification failures return typed or sentinel errors inspectable with `errors.Is` and `errors.As`:

- `ErrHashMismatch`: Matched via `errors.Is(err, secureexec.ErrHashMismatch)`. Unwraps to `*HashMismatchError{Path, Got, Want}` containing the expected and actual 32-byte SHA-256 digests.
- `ErrNotRegular`: Returned when the target path refers to a directory, device node, FIFO, or socket rather than a regular file.
- `ErrUnsupported`: Returned when the host OS or an explicitly requested `Mechanism` is not supported on the current platform.

## Usage

To verify an executable, construct an `*exec.Cmd` bound to the verified snapshot, and release the snapshot once the child process has started:

```go
want, err := secureexec.ParseHash(expectedHex) // 64 hex digits from a signed manifest
if err != nil {
    return err
}
v, err := secureexec.Verify("/opt/app/bin/hqq-sqlite-server", want)
if err != nil {
    return err // *HashMismatchError when the digest does not match
}
defer v.Close()

cmd := v.Command(ctx, "--listen", "127.0.0.1:0")
cmd.Stdout = os.Stdout
cmd.Stderr = os.Stderr
cmd.Env = []string{"HOME=" + home}
if err := cmd.Start(); err != nil {
    return err
}
// Deferred v.Close() runs after cmd.Start(): the memfd, share lock, or vault directory
// can be released safely once the kernel has mapped the running child image.
```

For simpler workflows where `secureexec` manages both `Start` and snapshot cleanup on child exit:

```go
p, err := secureexec.Start(ctx, path, want, []string{"--once"},
    secureexec.WithStdio(nil, os.Stdout, os.Stderr),
    secureexec.WithEnv(env),
)
if err != nil {
    return err
}
state, err := p.Wait() // snapshot is released when the child exits, even if Wait is not called
```

Additional configuration options include `WithVaultDir`, `WithMechanism(MechanismVault)`, `WithPolicy`, `WithStdio`, `WithEnv`, and `WithDir`.

For build and release tooling that needs to compute a file's SHA-256 digest without creating an execution snapshot:

```go
sum, size, err := secureexec.HashFile(path) // follows symlinks; not an execution boundary
```

On POSIX platforms using the `vault` mechanism, if a supervisor process was previously killed with `SIGKILL` before `Close` could remove its temporary directory, stale vaults older than a retention threshold can be swept at startup:

```go
n, err := secureexec.CleanVaults("", 24*time.Hour) // "" uses the default vault base directory
```

Call `CleanVaults` only with an age threshold guaranteed to exceed the startup window of any concurrent launcher.

## Release Manifests and Signing

Package `gosuda.org/hqq/secureexec/manifest` defines a strict version-1 JSON release manifest (`manifest.json`) paired with a detached Ed25519 signature (`manifest.json.sig`):

```json
{
  "version": 1,
  "name": "hqq-sqlite-server",
  "release": "v0.6.0",
  "files": [
    {
      "os": "linux",
      "arch": "amd64",
      "name": "hqq-sqlite-server_linux_amd64",
      "sha256": "<64 hex digits>",
      "size": 1234567
    }
  ],
  "licenses": ["MIT"]
}
```

Key properties of the manifest format and parser:

- **Detached signature (`manifest.json.sig`)**: Hex-encoded 64-byte Ed25519 signature over the exact raw bytes of `manifest.json`, terminated by a newline.
- **Plain-hex keys**: Public keys are 32 bytes (64 hex characters); private keys are the 32-byte Ed25519 seed (64 hex characters). Neither format uses PEM or ASN.1 wrappers.
- **Strict parsing (`manifest.Parse`)**: Rejects unknown JSON fields, trailing bytes after the root object, unsupported `version` numbers, duplicate `os`/`arch` entries, filenames containing path separators (`/` or `\`), and negative file sizes.
- **Verify-before-parse (`manifest.Verify(data, sig, pub)`)**: Verifies the Ed25519 signature over `data` before passing a single byte to the JSON parser.

Typical verification flow in a launcher:

```go
pub, err := manifest.ParsePublicKey(pubText)
if err != nil {
    return err
}
m, err := manifest.Verify(manifestBytes, sigText, pub)
if err != nil {
    return err
}
f, ok := m.Find(runtime.GOOS, runtime.GOARCH)
if !ok {
    return errors.New("no artifact for this platform")
}
v, err := secureexec.Verify(artifactPath, f.SHA256)
```

### The `hqq-manifest` CLI

Command `cmd/hqq-manifest` generates keypairs, builds manifests from a directory of cross-compiled binaries, signs manifests, and verifies release bundles:

```bash
hqq-manifest keygen -priv release.key -pub release.pub
hqq-manifest gen    -dir dist -name hqq-sqlite-server -release v0.6.0 -license MIT -out dist/manifest.json
hqq-manifest sign   -key release.key -in dist/manifest.json            # writes dist/manifest.json.sig
hqq-manifest verify -pub release.pub -in dist/manifest.json -sig dist/manifest.json.sig -dir dist
hqq-manifest hash   dist/hqq-sqlite-server_linux_amd64
```

Operational rules for `hqq-manifest`:

- `gen` scans `-dir` for regular files matching `NAME_<os>_<arch>` (with an optional `.exe` suffix on Windows) and ignores unrelated files. Any file starting with `NAME_` that fails `<os>_<arch>` parsing, any symbolic link, or any duplicate platform entry causes `gen` to exit with an error.
- `sign -key-env VAR` reads the hex private key from an environment variable (such as `HQQ_RELEASE_SIGNING_KEY` in CI) so private keys never touch disk in automated pipelines. `keygen` refuses to overwrite existing files and writes private keys with mode `0600`.
- Exit codes: `0` on success, `1` on verification or runtime error, `2` on command-line usage error.

## Testing and Platform Coverage

Run the local test suite and cross-compilation checks with:

```bash
go vet ./secureexec/... ./cmd/hqq-manifest/
go test -race -count=1 ./secureexec/... ./cmd/hqq-manifest/
GOOS=linux   go test -c -o /dev/null ./secureexec/
GOOS=windows go test -c -o /dev/null ./secureexec/
GOOS=freebsd go test -c -o /dev/null ./secureexec/
```

The test suite covers:

- **All POSIX hosts (including macOS)**: Digest parsing and hashing; matching, mismatched, and non-regular file errors; leaf symlink refusal (`ELOOP`); `Start`/`Wait` lifecycle and snapshot cleanup; `vault` directory (`0700`) and file (`0500`) permissions, symlinked or group-writable base refusal, `CleanVaults`, and `UF_IMMUTABLE`; `trusted-path` policy acceptance and fallback; simulated TOCTOU attacks (in-place overwrite, `rename` over the binary, and `truncate` after `Verify`) plus a 20-round concurrent attacker race during `exec`; manifest parsing, Ed25519 signing, and tampering rejection; and the `hqq-manifest` CLI end to end.
- **Linux CI**: `TestMemfdSealedAndRuns` verifies kernel file seals (`F_SEAL_WRITE|SHRINK|GROW|SEAL`), confirms that `write` and `ftruncate` on the sealed descriptor fail with `EPERM`, and runs the `/proc/self/fd/3` execution path and TOCTOU swap tests against `memfd`.
- **Windows CI**: `windows_test.go` verifies `FILE_SHARE_READ` lock enforcement (confirming that concurrent write, delete, and rename attempts fail with sharing violations while `Verified` is open and succeed after `Close`) and tests `CreateProcessW` execution via the `GetFinalPathNameByHandleW` canonical path.

## Future Work

Potential extensions not implemented in the current release:

- **`fs-verity` integration (`FS_IOC_MEASURE_VERITY`)**: Zero-copy kernel-enforced Merkle tree verification on `ext4`, `f2fs`, and `btrfs` filesystems without copying the binary into a `memfd`.
- **Interpreter / script policy**: Optional policy for executing verified scripts through a pinned, verified interpreter binary with file-descriptor passing.
