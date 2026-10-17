# secureexec

`gosuda.org/hqq/secureexec` verifies the SHA-256 digest of a native executable
and starts it without a time-of-check/time-of-use (TOCTOU) gap. The package is
standard library only. It is used together with the signed release manifest in
`gosuda.org/hqq/secureexec/manifest` and the `cmd/hqq-manifest` tool.

Package documentation (threat model, API summary): `doc.go`, or `go doc gosuda.org/hqq/secureexec`.

## Threat model

- The verifier (launcher) runs as an ordinary user.
- The attacker is another local process or user who can write in the
  directories the executable is read from.
- Attack 1, **path swap**: the directory entry is replaced (symlink, rename over,
  unlink) between the hash and the exec, so a different file runs.
- Attack 2, **in-place overwrite**: another process `write()`s or
  `mmap(MAP_SHARED)`-writes into the inode that was hashed before it is executed.
- Each mechanism makes the bytes that are hashed the bytes that are executed,
  and stops other users from changing them, so both attacks fail.

Out of scope:

- root or Administrator attackers, and kernel compromise;
- the integrity of the expected digest. It comes from the caller or from a
  signed manifest (`manifest.Verify`). The package does not authenticate it;
- **scripts with a shebang line**: only native executables are supported. The
  kernel re-resolves the interpreter path, and the interpreter opens files by
  name, so neither the copy nor the handle protects them;
- dynamic loaders and shared libraries that the executable loads by path.

## Mechanisms

`Verify` picks the mechanism for the platform. `Verified.Mechanism()` reports
which one was used.

| Platform | Mechanism (`Mechanism()`) | How the bytes are protected | Guaranteed | Not guaranteed |
|---|---|---|---|---|
| Linux (amd64, arm64, 386, arm, riscv64, ppc64/le, s390x, loong64, mips*) | `memfd` | Source opened `O_RDONLY\|O_CLOEXEC\|O_NOFOLLOW\|O_NONBLOCK`, fstat must be a regular file, copied once into `memfd_create(MFD_CLOEXEC\|MFD_ALLOW_SEALING[\|MFD_EXEC])`. Sealed with `F_SEAL_WRITE\|SHRINK\|GROW\|SEAL` and `F_SEAL_EXEC` when the kernel accepts it. Then the sealed memfd is hashed and the child is started as `/proc/self/fd/3` (memfd is `ExtraFiles[0]`). | The executed bytes are the hashed bytes, for any user, including a same-user process: seals cannot be lifted and the memfd has no path. Falls back to `vault` if `/proc/self/fd` is missing, `memfd_create` returns ENOSYS/EPERM/EACCES/EINVAL, or `vm.memfd_noexec=2`. | Uses RAM equal to the executable size. A kernel policy that still forbids exec of memfds makes the start fail; use `WithMechanism(MechanismVault)`. The `fs-verity` check is not implemented (future work). |
| macOS, FreeBSD, other POSIX (`unix` build tag, not Linux) | `vault` | Per-run directory `os.MkdirTemp(base, "run-")` (mode 0700, checked with Lstat: real directory, owned by us, mode exactly 0700). The source is copied into it as a new file (`O_EXCL\|O_NOFOLLOW`), `fsync`ed and `chmod 0500`. On darwin and freebsd `UF_IMMUTABLE` is set (best effort). The copy is reopened and must be owned by us with no group/other write bits; its digest is checked. The child is started by the copy's path. | Attacks from other users: nobody else can create, rename or write entries in the 0700 directory, so the hashed copy is the executed copy. | The vault base is checked itself (not a symlink, owned by us, not group/other writable), not its ancestors. Processes running as the same user can `chmod`/clear the flag and rewrite the copy. A crashed supervisor leaves the directory behind (see `CleanVaults`). Copy takes time and disk. |
| Windows | `windows-share-lock` | `CreateFileW(GENERIC_READ, FILE_SHARE_READ)` with no `FILE_SHARE_WRITE`/`FILE_SHARE_DELETE`. The handle stays open, the digest is read through it, the reparse-point attribute is rejected after resolution, and the path is the canonical path from `GetFinalPathNameByHandleW` (`\\?\` drive prefix removed). The child is started by that path while the handle is open; the handle is released when `Close` is called or the child exits (`Start`). | While the `Verified` is open, other handles cannot write, delete, or rename the file (sharing violation). The image section keeps a running executable from being overwritten. | Symbolic links are resolved (not refused) by design. The handle is only as long-lived as `Verified`; `Close` before `Start` is a caller error. Behaviour of CreateProcess with the canonical path is CI-only (see below). |
| Other platforms (plan9, js, wasip1, ...) | none | `Verify` returns `ErrUnsupported`. | — | — |

Opt-in: `Policy{TrustOwnedReadOnly: true}` (`WithPolicy`) implements
`trusted-path`. The file and every directory above its real path (after
`EvalSymlinks`) must be owned by root or the current user and must not be
writable by group or other. Then the file is hashed through an fd and started
by path, with no copy. If the check fails, the default mechanism is used. This
trades the copy for trust in the owner: a same-user process can change the
file between the hash and the exec.

### Symlinks

- POSIX: the final path component must not be a symlink. `Verify` fails with
  `ELOOP` (`errors.Is(err, syscall.ELOOP)`), in every mechanism, including
  `trusted-path`. Symlinks in the directories above it are followed.
- Windows: symlinks are resolved and the canonical path of the opened file is used.

## Errors

- `ErrHashMismatch` (`errors.Is`) — `*HashMismatchError{Path, Got, Want}`.
- `ErrNotRegular` — directories, devices, FIFOs, sockets.
- `ErrUnsupported` — platform or forced mechanism not available.

## Usage

Verify, build the command, start it while the snapshot is open:

```go
want, err := secureexec.ParseHash(expectedHex) // from a signed manifest
if err != nil {
    return err
}
v, err := secureexec.Verify("/opt/app/bin/hqq-sqlite-server", want)
if err != nil {
    return err // *HashMismatchError when the bytes differ
}
defer v.Close()

cmd := v.Command(ctx, "--listen", "127.0.0.1:0")
cmd.Stdout = os.Stdout
cmd.Stderr = os.Stderr
cmd.Env = []string{"HOME=" + home}
if err := cmd.Start(); err != nil {
    return err
}
// Close (deferred) runs after Start: memfd/handle/vault can be released once the
// child is running. On POSIX the vault directory is removed; the running image is unaffected.
```

Start, wait, and clean up automatically:

```go
p, err := secureexec.Start(ctx, path, want, []string{"--once"},
    secureexec.WithStdio(nil, os.Stdout, os.Stderr),
    secureexec.WithEnv(env),
)
if err != nil {
    return err
}
state, err := p.Wait() // the snapshot is released when the child exits, even if Wait is never called
```

Options: `WithVaultDir`, `WithMechanism(MechanismVault)`, `WithPolicy`,
`WithStdio`, `WithEnv`, `WithDir`. Hashing for tools:

```go
sum, size, err := secureexec.HashFile(path) // follows symlinks; not a security boundary
```

Vault maintenance (POSIX): after a crash, remove old vaults:

```go
n, err := secureexec.CleanVaults("", 24*time.Hour) // "" = default base
```

Call it only for vaults that belong to finished or long-dead runs.

## Manifest and signing

`secureexec/manifest` defines a version 1 manifest, `manifest.json`:

```json
{
  "version": 1,
  "name": "hqq-sqlite-server",
  "release": "v0.6.0",
  "files": [
    {"os": "linux", "arch": "amd64", "name": "hqq-sqlite-server_linux_amd64",
     "sha256": "<64 hex digits>", "size": 1234567}
  ],
  "licenses": ["MIT"]
}
```

- The detached signature `manifest.json.sig` is the hex of the 64-byte ed25519
  signature over the exact bytes of `manifest.json`, plus a newline.
- Public keys are 32 bytes in hex. Private keys are the 32-byte ed25519 seed in
  hex. Both are plain text files; there is no PEM.
- `manifest.Parse` rejects unknown fields, trailing data, unknown versions,
  duplicate `os`/`arch` pairs, names containing separators, and negative sizes.
- `manifest.Verify(data, sig, pub)` checks the signature before parsing.

Typical flow:

```go
pub, _ := manifest.ParsePublicKey(pubText)
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

### hqq-manifest

```
hqq-manifest keygen  -priv release.key -pub release.pub
hqq-manifest gen     -dir dist -name hqq-sqlite-server -release v0.6.0 -license MIT -out dist/manifest.json
hqq-manifest sign    -key release.key -in dist/manifest.json            # writes dist/manifest.json.sig
hqq-manifest verify  -pub release.pub -in dist/manifest.json -sig dist/manifest.json.sig -dir dist
hqq-manifest hash    dist/hqq-sqlite-server_linux_amd64
```

- `gen` scans `-dir` for regular files named `NAME_<os>_<arch>` (optionally
  `.exe`). Other names are ignored. A name that starts with `NAME_` but does not
  parse, a symlink, or two files for the same platform is an error.
- `sign -key-env VAR` reads the private key from an environment variable, which
  is the way to supply it from a CI secret. Do not commit private keys.
  `keygen` never overwrites an existing file. Private keys are written with mode 0600.
- Exit status: 0 success, 1 failure, 2 usage error.

Key handling for CI: store the private key as an encrypted secret and expose it
only to the release job, as `HQQ_RELEASE_SIGNING_KEY`. Ship the public key with the
verifier, or fetch it from a pinned location. Rotate by publishing a new public
key; old manifests remain verifiable with the old key.

## Tests and what only CI can confirm

Run on the host (macOS):

```
go vet ./secureexec/... ./cmd/hqq-manifest/
go test -race -count=1 ./secureexec/... ./cmd/hqq-manifest/
GOOS=linux   go test -c -o /dev/null ./secureexec/
GOOS=windows go test -c -o /dev/null ./secureexec/
GOOS=freebsd go test -c -o /dev/null ./secureexec/
```

Covered on macOS: hash helpers; match, mismatch and non-regular errors;
symlink refusal; `Start`/`Wait` and cleanup; vault layout (0700 dir, 0500 file),
base refusals (symlink, group-writable), `CleanVaults`, immutable flag;
`trusted-path` policy; in-place overwrite, rename-over and truncate after
`Verify` (both default and vault); a 20-round race with an attacker running
during exec; manifest parsing, signing and tampering; the CLI end to end.

CI-only (cannot run on this host; compile-checked only):

- Linux memfd: `TestMemfdSealedAndRuns` (seals, `EPERM` on write/truncate) and the default-mechanism swap tests. The `/proc/self/fd/3` exec path, and the syscall numbers for every listed GOARCH (cross-checked against `golang.org/x/sys` tables, not run).
- Linux `trusted-path` and vault permission behaviour when run as root (tests skip the write-refusal check as root).
- Windows: `windows_test.go` (sharing violations on write/delete/rename, rename-over refusal, release after `Close`), the canonical-path exec, and `GetFinalPathNameByHandleW` output.
- The `go test ./...` and `go test -race ./...` steps in `.github/workflows/ci.yml` run these packages on ubuntu, macos and windows (the race run is skipped on windows). The swap tests are `unix`-only and so do not run on windows.

Unverified claims, marked for CI confirmation:

- [INFERENCE] On Windows, `CreateProcess` accepts the drive-letter canonical
  path and the image load succeeds while the verified handle holds
  `FILE_SHARE_READ` only.
- [INFERENCE] On Linux with `vm.memfd_noexec=1` the `MFD_EXEC` flag is honoured
  and the memfd is executable.

## Future work

- fs-verity (`FS_IOC_MEASURE_VERITY`) for files on verity-enabled file systems;
  not implemented.
- A shebang-script or interpreter policy; not supported.
