# `sqlite/launcher`: Verified Child-Process Starter for `hqq-sqlite-server`

`gosuda.org/hqq/sqlite/launcher` starts the C SQLite server (`hqq-sqlite-server` from [`examples/sqlite/server`](../../examples/sqlite/server/README.md)) as a child process, verifies its SHA-256 digest or ed25519-signed release manifest without a TOCTOU window, attaches a parent lifeline so the server exits if the Go parent dies, and returns a connected `*sql.DB` handle.

The service-agnostic process mechanics (binary verification via `secureexec`, `--parent-stdin` lifeline attachment, waiting for the `ready <listener>` line, and graceful/forced shutdown) live in [`gosuda.org/hqq/launch`](../../launch/README.md). This package layers `hqq-sqlite-server`-specific CLI flag construction (`--allow-dir`, `--alias`, `--max-sessions`, `--perm`), the embedded release public key, and `gosuda.org/hqq/sqlite` `database/sql` handle creation on top of `launch`. `Server`, `StartError`, and `ReadyTimeoutError` are type aliases of the corresponding `launch` types, and `ErrConfig`, `ErrManifest`, `ErrNoArtifact`, `ErrReadyTimeout`, and `ErrStopTimeout` alias the `launch` sentinel errors so `errors.Is` matches either package.

```go
m, err := launcher.OpenManaged(ctx, cfg,
	launcher.WithDatabase("main"),
	launcher.WithSQLiteConfig(sqlite.Config{Timeout: 5 * time.Second}))
if err != nil {
	return err
}
defer m.Close() // closes m.DB first, then stops the child server
rows, err := m.DB.QueryContext(ctx, "SELECT 1")
```

Call `Start` when you only need the running `*Server` process, `Open` when you want `*sql.DB` and `*Server` returned as separate values (close the DB before stopping the server), or `OpenManaged` to manage both through a single `Close()` call.

## Usage

The launcher supports two verification modes: pinning a 32-byte SHA-256 digest directly or verifying an ed25519-signed release manifest.

### 1. Pinning a binary SHA-256 digest directly

When launching a locally built or pinned server binary, supply its 32-byte SHA-256 digest in `Config.SHA256`:

```go
want, err := secureexec.ParseHash("9f86d08...") // 64 hex characters
cfg := launcher.Config{
	Binary:      "/opt/hqq/bin/hqq-sqlite-server",
	SHA256:      want,
	AllowDir:    "/var/lib/app/db", // allow relative database paths inside this directory
	Aliases:     map[string]string{"main": "/var/lib/app/db/app.sqlite"},
	MaxSessions: 64,
}
srv, err := launcher.Start(ctx, cfg)
```

### 2. Verifying a signed release manifest

When deploying release artifacts (`manifest.json` + `manifest.json.sig` + platform binaries), pass `Config.Manifest`:

```go
data, _ := os.ReadFile("/opt/hqq/dist/manifest.json")
sig, _ := os.ReadFile("/opt/hqq/dist/manifest.json.sig")
srv, err := launcher.Start(ctx, launcher.Config{
	Manifest: &launcher.ManifestSource{
		Data:      data,
		Signature: sig,
		PublicKey: pub, // explicit ed25519 key; nil uses the embedded release key
		Dir:       "/opt/hqq/dist",
	},
	AllowDir: "/var/lib/app/db",
})
```

`Start` verifies the ed25519 signature over `Data` before parsing any JSON, looks up the artifact entry matching `runtime.GOOS` and `runtime.GOARCH` (`ErrNoArtifact` if absent), and verifies and executes that binary from `Dir` using `secureexec`.

### 3. Complete example with an embedded public key

Embed your release public key in your Go binary so callers only need to point at the distribution directory:

```go
package myapp

import (
	"context"
	_ "embed"
	"os"
	"path/filepath"

	"gosuda.org/hqq/secureexec/manifest"
	"gosuda.org/hqq/sqlite/launcher"
)

// releasePub contains the 64-character hex public key from `hqq-manifest keygen -pub release.pub`.
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

## Embedded release key (`release_public_key.hex`)

The package embeds `release_public_key.hex`, which currently ships with a placeholder value until the first signed release key is generated:

```text
PLACEHOLDER-run-hqq-manifest-keygen-and-commit-the-public-key
```

Before cutting the first signed release, generate a production key pair with `hqq-manifest keygen -priv release.key -pub release.pub` and commit the 64-hex-digit public key into `sqlite/launcher/release_public_key.hex` (never commit `release.key`). While the placeholder remains in place:

- `launcher.ReleasePublicKey()` fails closed with `ErrNoReleaseKey`.
- Calling `Start` with `Manifest.PublicKey == nil` fails with `ErrNoReleaseKey` before starting any process.
- Callers that pass an explicit `Manifest.PublicKey` or `Config.SHA256` work normally.

## Building and signing a release directory locally

1. Build `hqq-sqlite-server`:
   ```sh
   cmake -S c -B build -DHQQ_BUILD_SQLITE_SERVER=ON -DHQQ_WERROR=ON
   cmake --build build --target hqq-sqlite-server
   ```
2. Copy the binary into `dist/hqq-sqlite-server_<os>_<arch>[.exe]`.
3. Generate `manifest.json`:
   ```sh
   hqq-manifest gen -dir dist -name hqq-sqlite-server -release vX.Y.Z -license MIT -out dist/manifest.json
   ```
4. Sign the manifest:
   ```sh
   hqq-manifest sign -key release.key -in dist/manifest.json
   ```
5. Verify the signature and binaries:
   ```sh
   hqq-manifest verify -pub release.pub -in dist/manifest.json -sig dist/manifest.json.sig -dir dist
   ```

## Threat model

The verifier runs as an unprivileged user against an adversary (another local process or user) with write access to the directory holding the binary. `Start` executes the binary exclusively through `secureexec.Verify` (sealed `memfd` on Linux, `0700` private vault copy on macOS/POSIX, `FILE_SHARE_READ` locked handle on Windows; see [`secureexec/README.md`](../../secureexec/README.md)), eliminating the TOCTOU window between hashing the file and `execve` / `CreateProcess`. Calling `Start` with neither `SHA256` nor `Manifest` fails immediately with `ErrConfig`.

## Process lifecycle and limits

- **Ready handshake**: `Start` blocks until the child prints `ready <listener>` on stdout, consuming that line and forwarding any subsequent stdout/stderr bytes to `Config.Stdout` and `Config.Stderr` (keeping the last 2 KiB for error diagnostics). If the server does not become ready within `ReadyTimeout` (default `10s`), `Start` kills the child and returns `*ReadyTimeoutError` (`ErrReadyTimeout`).
- **Startup context**: `ctx` passed to `Start` bounds only startup and verification; the child server continues running after `Start` returns until `Stop` / `Close` is called or the parent process exits.
- **Parent lifeline**: unless `DisableLifeline` is set, the server is started with `--parent-stdin` connected to a pipe held by the parent. If the parent exits for any reason (including `SIGKILL`), the kernel closes the pipe write descriptor and the server rolls back open transactions and exits with status `0`.
- **Graceful shutdown (`Stop` / `Close`)**: `Stop(ctx)` closes the lifeline pipe (or sends `SIGTERM` / `CTRL_BREAK_EVENT` when `DisableLifeline` is set) and waits up to `StopTimeout` (default `10s`) before sending `SIGKILL` and returning `ErrStopTimeout`. `Close()` calls `Stop(context.Background())`, and both are idempotent.
- **Scope**: `sqlite/launcher` starts a single child instance and does not auto-restart crashed servers.

## Tests

Run the unit and end-to-end launcher suites from the repository root:

```sh
go test -race -count=1 ./sqlite/launcher/...
HQQ_SQLITE_SERVER=/path/to/hqq-sqlite-server go test -count=1 ./sqlite/launcher/...
```

Without `HQQ_SQLITE_SERVER`, the unit suite tests CLI flag generation, config validation, manifest verification, placeholder key rejection, and `Open` / `OpenManaged` lifecycle against a re-executed stub binary. Setting `HQQ_SQLITE_SERVER` runs the full `database/sql` contract suite through `launcher`, verifies hash-mismatch rejection, and tests `SIGKILL` of a parent helper process to confirm the child server exits automatically.
