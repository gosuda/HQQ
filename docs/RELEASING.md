# Releasing hqq-sqlite-server

This document covers how a release of the SQLite server binary
(`hqq-sqlite-server`) is built, signed and published, how the signing keys are
managed, and how a Go program verifies a downloaded binary. The workflow is
`.github/workflows/release.yml`.

## What a release contains

For a tag `vX.Y.Z` the GitHub Release contains:

| Asset | Content |
|---|---|
| `hqq-sqlite-server_<os>_<arch>[.exe]` | the stripped server, one per platform (table below) |
| `manifest.json` | version 1 manifest: file names, sizes and SHA-256 digests, licenses |
| `manifest.json.sig` | detached ed25519 signature over the exact bytes of `manifest.json` |
| `licenses.tar.gz` | the `licenses/` tree: `LICENSE`, `SQLITE-PUBLIC-DOMAIN.txt`, `NOTICE.md`, `linkage/*.txt` |
| `SHA256SUMS` | `sha256sum` lines for every asset above except itself |

GitHub Releases are flat, so the `licenses/` directory is shipped as
`licenses.tar.gz`. `SHA256SUMS` is not signed; the binaries are authenticated
through the signed manifest, and the license files are covered only by
`SHA256SUMS`.

### Platforms

| Asset | Runner (native build) | Status |
|---|---|---|
| `hqq-sqlite-server_linux_amd64` | `ubuntu-24.04` | published |
| `hqq-sqlite-server_linux_arm64` | `ubuntu-24.04-arm` | published |
| `hqq-sqlite-server_darwin_arm64` | `macos-latest` | published |
| `hqq-sqlite-server_darwin_amd64` | `macos-15-intel` | published |
| `hqq-sqlite-server_windows_amd64.exe` | `windows-latest`, clang-cl | gated, see below |
| `hqq-sqlite-server_windows_arm64.exe` | `windows-11-arm`, clang-cl | gated, see below |

Each binary is built on a runner of its own platform and smoke-tested there
(the Go end-to-end tests of `sqlite/e2e` run against the stripped binary).
Cross-compiled binaries are not used, because they could not be run before
publishing.

Windows is gated behind the repository variable `HQQ_RELEASE_WINDOWS`. The
server's CMake file (`examples/sqlite/server/CMakeLists.txt`) stops with a
fatal error on `WIN32`, so the Windows jobs would fail. Set the variable to
`true` only after the server builds and passes its tests on Windows. Until
then the Windows jobs are skipped and the release has the four POSIX binaries.

## SQLite

The release does not vendor SQLite. The pinned amalgamation is recorded in
`third_party/sqlite/`:

- `VERSION`: the version, archive name and download URL;
- `SHA256` and `SHA3-256`: the digests of the archive. SHA3-256 is the value
  sqlite.org publishes on its download page.

`scripts/release/fetch-sqlite.sh` (POSIX) and `fetch-sqlite.ps1` (Windows)
download the archive, check both digests (SHA3-256 when the host can compute
it), refuse unsafe member names, and unpack into a directory outside the
repository. The directory is passed to CMake as
`-DHQQ_SQLITE_AMALGAMATION_DIR=...`. Local use:

```sh
dir=$(scripts/release/fetch-sqlite.sh -d /tmp/hqq-sqlite)     # or -a ARCHIVE for a local copy
cmake -S c -B build -DHQQ_BUILD_SQLITE_SERVER=ON -DHQQ_SQLITE_AMALGAMATION_DIR="$dir" \
  -DCMAKE_BUILD_TYPE=Release -DCMAKE_C_FLAGS="-DSQLITE_DQS=0 -DSQLITE_OMIT_LOAD_EXTENSION -DSQLITE_DEFAULT_WAL_SYNCHRONOUS=1 -DSQLITE_USE_URI=0"
cmake --build build --target hqq-sqlite-server
```

The release build compiles the amalgamation with these definitions, passed
through `CMAKE_C_FLAGS`:

| Definition | Why |
|---|---|
| `SQLITE_DQS=0` | double-quoted strings are identifiers only, not string literals |
| `SQLITE_OMIT_LOAD_EXTENSION` | the server cannot load extensions; the server checks the option at run time |
| `SQLITE_DEFAULT_WAL_SYNCHRONOUS=1` | WAL default is `NORMAL`. A power loss can drop the last commits (the database stays consistent). Remove it to keep the stock default `FULL` if durability matters more than write speed |
| `SQLITE_USE_URI=0` | file names are never interpreted as URIs |

`SQLITE_THREADSAFE=1` is set by the CMake file. `SQLITE_ENABLE_COLUMN_METADATA`
is not set: the server uses only `sqlite3_column_decltype` and `sqlite3_column_name`.

SQLite is in the public domain. `licenses/SQLITE-PUBLIC-DOMAIN.txt` quotes the
dedication and records the pinned version and digests.

### Updating SQLite

1. Take the new version, archive name, URL and SHA3-256 from
   <https://www.sqlite.org/download.html> (the `sqlite-amalgamation-*.zip` row).
2. Download the archive with curl over HTTPS and compute SHA256 with the host
   tools; check that the SHA3-256 computed locally equals the sqlite.org value.
3. Update `third_party/sqlite/VERSION`, `SHA256` and `SHA3-256`.
4. Run `scripts/release/fetch-sqlite.sh -d /tmp/x` and the server build and tests.

## Build and test steps (per platform)

1. Fetch and verify the amalgamation.
2. `cmake -S c -B build -G Ninja -DCMAKE_BUILD_TYPE=Release -DHQQ_WERROR=ON -DHQQ_BUILD_TESTS=ON -DHQQ_BUILD_STRESS=OFF -DHQQ_BUILD_SQLITE_SERVER=ON -DHQQ_SQLITE_AMALGAMATION_DIR=...`
   (`-static-libgcc` on Linux; `-DCMAKE_MSVC_RUNTIME_LIBRARY=MultiThreaded` on Windows).
3. Build `hqq-sqlite-server` and the C unit tests; run `ctest -R hqqsqlite_server`.
4. Copy to `dist/hqq-sqlite-server_<goos>_<goarch>[.exe]`, strip (`strip --strip-all`
   on Linux, `strip -x` plus ad-hoc `codesign` on macOS).
5. Smoke test: `HQQ_SQLITE_SERVER=<binary> go test ./sqlite/...`, and
   `./sqlite/e2e/...` alone; any skip there other than the `TestHelper*`
   re-exec helpers fails the job.
6. Linkage report (`scripts/release/linkage.sh`): `readelf`/`ldd`, `otool -L`
   or `dumpbin /DEPENDENTS`.

## Cutting a release

1. Make sure `main` is green (`c.yml`, `sqlite.yml`, `ci.yml`), and that
   `CHANGELOG.md` has the release notes.
2. Dry run first: Actions, **Release**, **Run workflow**, keep `dry_run` as
   `true`. This builds, tests and uploads the artifacts. It does not sign or
   publish.
3. Tag the commit and push the tag:

   ```sh
   git tag -a vX.Y.Z -m "hqq vX.Y.Z"
   git push origin vX.Y.Z
   ```

4. The tag run builds the platforms, then the `publish` job: it writes the
   license files, generates `manifest.json`, signs it, verifies the signature
   with the committed public key, checks the tampered-manifest case, writes
   `SHA256SUMS` and creates the GitHub Release (or uploads with `--clobber` when
   the release already exists).
5. Check the release page, then download one binary and run the verification
   below.

Manual publish of an existing tag: **Run workflow** on that tag with
`dry_run` set to `false`. The workflow refuses to publish from a branch.

Without the signing secret (forks, dry runs) nothing is signed or published.
The unsigned `manifest.json`, `licenses.tar.gz` and `SHA256SUMS` are uploaded
as the `release-unsigned` workflow artifact, and the build artifacts are kept
in the run.

## Signing keys

The manifest is signed with ed25519 by `cmd/hqq-manifest`.

- **Public key**: `sqlite/launcher/release_public_key.hex` (one line, 64 hex
  digits, committed). The release job verifies with this file, and the
  verifier of consumers embeds a copy of it.
- **Private key**: the GitHub Actions secret `HQQ_RELEASE_SIGNING_KEY`
  (repository or environment secret, visible to the `publish` job only). It is
  never committed. Keep an offline backup, encrypted.

First-time setup (or key rotation), on a trusted machine:

```sh
go build -o /tmp/hqq-manifest ./cmd/hqq-manifest
/tmp/hqq-manifest keygen -priv /tmp/release.key -pub /tmp/release.pub
# commit /tmp/release.pub as sqlite/launcher/release_public_key.hex (public key only)
gh secret set HQQ_RELEASE_SIGNING_KEY < /tmp/release.key
# keep /tmp/release.key in an encrypted backup, then delete the plain copy
rm /tmp/release.key
```

`keygen` never overwrites an existing file and writes the private key with mode
0600. The release job fails before building anything when the public key file
is missing or is not a valid key (the failure message repeats these steps).

Rotation: publish a new public key in the repository and set the new private
key as the secret. Old releases stay verifiable with the old key, so keep the
old public key in the repository history and in consumers' builds.

If the private key leaks: remove the secret, rotate, and publish a new release
that supersedes the affected tags. Mark the affected releases in their notes.

## Verifying a download (Go)

Embed the public key in your program, then verify the manifest and the binary
before starting it. `secureexec` checks the digest of the bytes it executes,
so the file cannot be replaced between the check and the start.

```go
package main

import (
	"context"
	_ "embed"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"runtime"

	"gosuda.org/hqq/secureexec"
	"gosuda.org/hqq/secureexec/manifest"
)

// release_public_key.hex is a copy of sqlite/launcher/release_public_key.hex.
//
//go:embed release_public_key.hex
var releasePublicKey []byte

func main() {
	dir := "hqq-sqlite-release" // the downloaded assets

	pub, err := manifest.ParsePublicKey(releasePublicKey)
	if err != nil {
		log.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		log.Fatal(err)
	}
	sig, err := os.ReadFile(filepath.Join(dir, "manifest.json.sig"))
	if err != nil {
		log.Fatal(err)
	}
	m, err := manifest.Verify(data, sig, pub) // signature first, then parse
	if err != nil {
		log.Fatal(err)
	}

	f, ok := m.Find(runtime.GOOS, runtime.GOARCH)
	if !ok {
		log.Fatalf("no %s/%s build in release %s", runtime.GOOS, runtime.GOARCH, m.Release)
	}
	v, err := secureexec.Verify(filepath.Join(dir, f.Name), f.SHA256)
	if err != nil {
		log.Fatal(err) // *secureexec.HashMismatchError when the bytes differ
	}
	defer v.Close()

	cmd := v.Command(context.Background(), "--help")
	out, err := cmd.CombinedOutput()
	fmt.Printf("%s %s verified, %d bytes\n%s", f.Name, m.Release, f.Size, out)
	if err != nil {
		log.Fatal(err)
	}
}
```

Check the manifest against your own copy of the public key, never against a
key taken from the same download.

Command-line checks with the same tool (from a clone of this repository):

```sh
go run ./cmd/hqq-manifest verify -pub release_public_key.hex \
  -in manifest.json -sig manifest.json.sig -dir hqq-sqlite-release
sha256sum -c SHA256SUMS
```

## Reproducibility and limits

- Builds are not bit-for-bit reproducible across runners. The inputs are pinned
  (SQLite archive digest, Go version in `go.mod`, CMake options), and the
  compiler is whatever the runner image provides. The build passes
  `-ffile-prefix-map` to remove the workspace path. Compare a rebuild with the
  same runner image before you rely on equality of digests.
- The runner images are not pinned by digest. `ubuntu-24.04`, `macos-latest` and
  the other labels move over time. The linkage reports record what each build
  loaded.
- The manifest is only as trustworthy as the signing key and the committed
  public key. A compromised secret allows signing of new releases; see
  "If the private key leaks".
- `SHA256SUMS` is not signed; the license files are only covered by it.
- Smoke tests exercise the server through the Go client on the build machine.
  They do not cover every platform feature (for example Windows, which is
  gated).
- Gatekeeper and other platform trust checks are not part of this process.
  macOS binaries carry an ad-hoc signature only, and they are not notarized.
- The Windows path is untested until the server builds on Windows; the
  CMake and PowerShell scripts for it are unverified.

## Files

- `.github/workflows/release.yml` — the workflow.
- `scripts/release/fetch-sqlite.sh`, `fetch-sqlite.ps1` — fetch and verify SQLite.
- `scripts/release/write-licenses.sh` — `LICENSE`, `SQLITE-PUBLIC-DOMAIN.txt`, `NOTICE.md`.
- `scripts/release/linkage.sh` — linkage report of one binary.
- `third_party/sqlite/` — pinned version and digests.
- `cmd/hqq-manifest` and `secureexec/manifest` — manifest, signing, verification.
