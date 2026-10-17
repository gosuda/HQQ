# sqlite/launcher

`gosuda.org/hqq/sqlite/launcher` starts the HQQ SQLite server
(`examples/sqlite/server`, the `hqq-sqlite-server` binary) as a child process,
verifies the binary before it runs, ties the server's life to the process that
started it, and returns a `database/sql` handle for its listener.

```go
m, err := launcher.OpenManaged(ctx, cfg,
    launcher.WithDatabase("main"),
    launcher.WithSQLiteConfig(sqlite.Config{Timeout: 5 * time.Second}))
if err != nil {
    return err
}
defer m.Close() // closes the DB, then stops the server
rows, err := m.DB.QueryContext(ctx, "SELECT 1")
```

Use `Start` when you need the server without a handle, and `Open` when you
want the `*sql.DB` and the `*Server` separately (close the DB first, then the
server).

## Usage

Pin the digest of the binary you built or downloaded:

```go
want, err := secureexec.ParseHash("9f86d08...") // 64 hex digits
cfg := launcher.Config{
    Binary:   "/opt/hqq/bin/hqq-sqlite-server",
    SHA256:   want,
    AllowDir: "/var/lib/app/db",           // raw paths allowed inside this directory
    Aliases:  map[string]string{"main": "/var/lib/app/db/app.sqlite"},
    MaxSessions: 64,
}
srv, err := launcher.Start(ctx, cfg)
```

Or verify a signed release manifest (the usual way for releases):

```go
data, _ := os.ReadFile("/opt/hqq/dist/manifest.json")
sig, _ := os.ReadFile("/opt/hqq/dist/manifest.json.sig")
srv, err := launcher.Start(ctx, launcher.Config{
    Manifest: &launcher.ManifestSource{
        Data:      data,
        Signature: sig,
        PublicKey: pub, // your release key; nil selects the embedded key (below)
        Dir:       "/opt/hqq/dist",
    },
    AllowDir: "/var/lib/app/db",
})
```

`Start` checks the signature before it parses the manifest, then selects the
entry for `runtime.GOOS` and `runtime.GOARCH`, and verifies the file named by that
entry in `Dir`. A manifest without an entry for this platform fails with
`ErrNoArtifact`.

### Example: a signed manifest with an embedded public key

Your program embeds the public key of the release, so the caller passes nothing
but the files:

```go
package myapp

import (
    _ "embed"
    "os"
    "path/filepath"

    "gosuda.org/hqq/sqlite/launcher"
    "gosuda.org/hqq/secureexec/manifest"
)

// releasePub is the hex text from `hqq-manifest keygen -pub release.pub`.
//
//go:embed release.pub
var releasePub []byte

func startDB(ctx context.Context, dist string) (*launcher.Managed, error) {
    pub, err := manifest.ParsePublicKey(releasePub)
    if err != nil {
        return nil, err
    }
    data, err := os.ReadFile(filepath.Join(dist, "manifest.json"))
    if err != nil {
        return nil, err
    }
    sig, err := os.ReadFile(filepath.Join(dist, "manifest.json.sig"))
    if err != nil {
        return nil, err
    }
    return launcher.OpenManaged(ctx, launcher.Config{
        Manifest: &launcher.ManifestSource{Data: data, Signature: sig, PublicKey: pub, Dir: dist},
        AllowDir: "/var/lib/app/db",
    }, launcher.WithDatabase("main"))
}
```

`manifest.Verify` rejects any manifest whose signature does not match `pub`,
including one that was edited after signing. Rotating the key means shipping a
new public key with the verifier; old manifests keep verifying with the old key.

## Embedded release key and the placeholder

The package embeds `release_public_key.hex`. The repository ships it as a
placeholder:

```
PLACEHOLDER-run-hqq-manifest-keygen-and-commit-the-public-key
```

**The maintainer must replace this line with the real 32-byte ed25519 public key
(64 hex digits) before the first signed release.** Generate it with
`hqq-manifest keygen -priv release.key -pub release.pub` and commit only
`release.pub`'s content (the hex text), never the private key. Until then:

- `launcher.ReleasePublicKey()` returns `ErrNoReleaseKey`, and never a zero key;
- a manifest-based `Start` with `Manifest.PublicKey == nil` fails with
  `ErrNoReleaseKey` and starts nothing;
- a caller that passes its own `PublicKey` is not affected.

The release workflow fails closed when the file is not a valid key.

## Getting the binary and the manifest from a release

1. Build the server: `cmake -S c -B build -DHQQ_BUILD_SQLITE_SERVER=ON -DHQQ_WERROR=ON`
   and `cmake --build build --target hqq-sqlite-server`. The binary is
   `build/sqlite-server/hqq-sqlite-server`. See `examples/sqlite/server/README.md`.
2. Name each artifact `hqq-sqlite-server_<os>_<arch>` (optionally `.exe`).
3. `hqq-manifest gen -dir dist -name hqq-sqlite-server -release vX.Y.Z -license MIT -out dist/manifest.json`
4. `hqq-manifest sign -key release.key -in dist/manifest.json` (writes `manifest.json.sig`).
5. Check it: `hqq-manifest verify -pub release.pub -in dist/manifest.json -sig dist/manifest.json.sig -dir dist`.
6. For a single file without a manifest: `hqq-manifest hash dist/hqq-sqlite-server_linux_amd64`.

Ship `dist/` (binaries, `manifest.json`, `manifest.json.sig`). Verify the
manifest with the release public key before you use any digest from it.

## Threat model

The verifier runs as an ordinary user. The attacker is another local process or
user that can write where the binary is read from. `Start` runs the server only
after `secureexec.Verify` has hashed the bytes that will be executed: a path swap
or an in-place overwrite between the check and the exec does not change what
runs. The mechanism per platform (sealed memfd on Linux, a private 0700 vault
copy on macOS and other POSIX systems, a share-locked handle on Windows) and its
limits are in `secureexec/README.md`. Root attackers, kernel compromise and a
compromised expected digest are out of scope.

`Start` never runs a binary without an expected digest: `Config` with neither
`SHA256` nor `Manifest` returns `ErrConfig`.

## Lifecycle

- `Start` returns when the server prints `ready <listener>` on stdout. The ready
  line is consumed. Everything else on stdout and stderr goes to `Config.Stdout`
  and `Config.Stderr`, which are optional; their errors are ignored. The last 2
  KiB of output appear in error messages.
- `ctx` bounds only the start. The server keeps running after `Start` returns.
- `ReadyTimeout` (default 10 s) kills the server and returns `*ReadyTimeoutError`
  (`errors.Is(err, ErrReadyTimeout)`). A server that exits before it is ready
  returns `*StartError`.
- A verification failure returns `*StartError` whose `Err` wraps
  `secureexec.ErrHashMismatch` (or `ErrNotRegular`, `ErrUnsupported`). No process
  is started.
- Unless `DisableLifeline` is set, the server gets `--parent-stdin` and the
  parent keeps the write end of a pipe on the server's stdin. The server shuts
  down gracefully (status 0, open transactions roll back) when the parent exits
  for any reason, including SIGKILL, because the kernel closes the parent's
  descriptors. `DisableLifeline` omits the flag; `Stop` then sends SIGTERM on Unix (CTRL_BREAK on Windows).
- `Stop(ctx)` closes the pipe and waits up to `StopTimeout` (default 10 s) for a
  graceful exit. It returns the exit status error (nil for status 0). After
  `StopTimeout`, or when `ctx` ends, it kills the server and returns
  `ErrStopTimeout` (or the context error). `Close` is `Stop` with no caller deadline,
  and both are idempotent.
- `Done` closes when the process has exited. `Err` returns the exit status error
  after that, and nil while the server runs.
- The verified snapshot is released when the process exits (the same rule as
  `secureexec.Start`).

### Limits

- A server that is stopped (SIGSTOP, a debugger) rather than dead keeps the pipe
  open and keeps running. The same holds for any other process that inherits
  the write end; `os.Pipe` is close-on-exec, so only processes that
  deliberately inherit it can hold it.
- The server must not fork and keep running: the lifeline thread does not exist
  in a fork child (see `examples/sqlite/server/README.md`, "Zombie-free guarantee and its limits").
- `Stop` and the kill signal target the server process only, not its children.
- The launcher does not restart the server; the caller does. `Config.SHA256`,
  `Config.Manifest`, and the lifeline use `secureexec` and `lifeline`, which any parent can use.
- A `Config.Perm` of zero means the server default (0600), so mode 0000 cannot be
  requested.

## Tests

- `go test ./sqlite/launcher/...` runs the unit tests against a stub server (the
  test binary, re-executed): hash ok and mismatch, manifest accepted, bad
  signature, tampered manifest, missing platform, placeholder key, ready
  timeout, exit before ready, graceful and forced stop, crash status, output
  capture, arguments, `DisableLifeline`, `Open` and `OpenManaged`.
- The real server: set `HQQ_SQLITE_SERVER` to the built binary. The contract
  suite runs through the launcher, plus start/query/stop, the wrong-hash
  refusal, and the parent-kill check (a helper process holds the server and is
  SIGKILLed; the server must exit). Without the variable these tests skip.
