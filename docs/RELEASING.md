# Releasing `hqq-sqlite-server`

This guide covers how release binaries of the C SQLite server (`hqq-sqlite-server`) are built, smoke-tested, signed, and published by `.github/workflows/release.yml`, how ed25519 signing keys are managed, and how Go applications verify downloaded artifacts before executing them.

## What a release contains

For every `vX.Y.Z` tag, the GitHub Release publishes the following assets:

| Asset | Content |
| :--- | :--- |
| `hqq-sqlite-server_<os>_<arch>[.exe]` | Stripped native server binary, one per target platform (listed below). |
| `manifest.json` | Version 1 release manifest recording filenames, byte sizes, SHA-256 digests, and SPDX license IDs. |
| `manifest.json.sig` | Detached ed25519 signature over the exact bytes of `manifest.json`. |
| `licenses.tar.gz` | Archive of the `licenses/` tree: `LICENSE`, `SQLITE-PUBLIC-DOMAIN.txt`, `NOTICE.md`, and `linkage/*.txt`. |
| `SHA256SUMS` | `sha256sum`-compatible lines covering every release asset except `SHA256SUMS` itself. |

Because GitHub Release assets occupy a flat namespace, the `licenses/` directory is packaged as `licenses.tar.gz`. `SHA256SUMS` is provided for human inspection and CLI checks; automated verifiers authenticate binaries through the signed `manifest.json` + `manifest.json.sig` pair.

### Platforms

| Asset | GitHub Actions Runner (Native Build) | Status |
| :--- | :--- | :--- |
| `hqq-sqlite-server_linux_amd64` | `ubuntu-24.04` | published |
| `hqq-sqlite-server_linux_arm64` | `ubuntu-24.04-arm` | published |
| `hqq-sqlite-server_darwin_arm64` | `macos-latest` | published |
| `hqq-sqlite-server_darwin_amd64` | `macos-15-intel` | published |
| `hqq-sqlite-server_windows_amd64.exe` | `windows-latest`, `clang-cl` | published |
| `hqq-sqlite-server_windows_arm64.exe` | `windows-11-arm`, `clang-cl` | published |

Every binary is compiled natively on a runner of its target architecture and smoke-tested on that same machine by running the `sqlite/e2e` Go test suite against the stripped binary. Cross-compiled binaries are never published without native execution.

Windows binaries (`windows/amd64` and `windows/arm64`, built with `clang-cl` and static CRT linkage) are required release targets: a failure on any platform blocks the `publish` job. Windows build details live in [`examples/sqlite/server/README.md`](../examples/sqlite/server/README.md).

## SQLite Amalgamation

The repository does not vendor the SQLite C sources in Git. Instead, `third_party/sqlite/` pins the exact amalgamation release:

- `VERSION`: version number, zip archive name, and canonical download URL.
- `SHA256` and `SHA3-256`: expected cryptographic digests of the archive (`SHA3-256` matches the digest published on `sqlite.org`).

`scripts/release/fetch-sqlite.sh` (POSIX) and `scripts/release/fetch-sqlite.ps1` (Windows) download the archive, verify both digests (`SHA3-256` when a local tool supports it), reject archive members with path-traversal names, and unpack the sources into a directory outside the working tree passed to CMake via `-DHQQ_SQLITE_AMALGAMATION_DIR=...`:

```sh
dir=$(scripts/release/fetch-sqlite.sh -d /tmp/hqq-sqlite)     # or -a ARCHIVE for a local copy
cmake -S c -B build -DHQQ_BUILD_SQLITE_SERVER=ON -DHQQ_SQLITE_AMALGAMATION_DIR="$dir" \
  -DCMAKE_BUILD_TYPE=Release -DCMAKE_C_FLAGS="-DSQLITE_DQS=0 -DSQLITE_OMIT_LOAD_EXTENSION -DSQLITE_DEFAULT_WAL_SYNCHRONOUS=1 -DSQLITE_USE_URI=0"
cmake --build build --target hqq-sqlite-server
```

Release builds compile `sqlite3.c` with the following hardening flags in `CMAKE_C_FLAGS`:

| Preprocessor Flag | Rationale |
| :--- | :--- |
| `SQLITE_DQS=0` | Disables double-quoted string literals so double quotes only quote SQL identifiers. |
| `SQLITE_OMIT_LOAD_EXTENSION` | Compiles out dynamic extension loading (`sqlite3_load_extension`); the server also verifies this option at startup. |
| `SQLITE_DEFAULT_WAL_SYNCHRONOUS=1` | Defaults WAL mode to `PRAGMA synchronous = NORMAL` for throughput (remove if power-loss durability of the final commit outweighs write latency). |
| `SQLITE_USE_URI=0` | Prevents database filenames from being interpreted as SQLite URI strings. |

`SQLITE_THREADSAFE=1` is set by `c/CMakeLists.txt`. `SQLITE_ENABLE_COLUMN_METADATA` is not needed because the server relies only on `sqlite3_column_decltype` and `sqlite3_column_name`.

SQLite is in the public domain; `licenses/SQLITE-PUBLIC-DOMAIN.txt` includes the public-domain dedication alongside the pinned archive version and digests.

### Updating SQLite

1. Copy the new version string, archive filename, download URL, and `SHA3-256` digest from <https://www.sqlite.org/download.html> (`sqlite-amalgamation-*.zip`).
2. Download the archive over HTTPS, compute its `SHA256` and `SHA3-256` locally, and confirm the local `SHA3-256` matches `sqlite.org`.
3. Update `third_party/sqlite/VERSION`, `SHA256`, and `SHA3-256`.
4. Run `scripts/release/fetch-sqlite.sh -d /tmp/x` followed by the C and Go SQLite test suites.

## Build and test steps (per platform)

Each matrix runner in `.github/workflows/release.yml` executes six steps:

1. Fetch and verify the pinned SQLite amalgamation.
2. Configure CMake:
   `cmake -S c -B build -G Ninja -DCMAKE_BUILD_TYPE=Release -DHQQ_WERROR=ON -DHQQ_BUILD_TESTS=ON -DHQQ_BUILD_STRESS=OFF -DHQQ_BUILD_SQLITE_SERVER=ON -DHQQ_SQLITE_AMALGAMATION_DIR=...`
   (adding `-static-libgcc` on Linux and `-DCMAKE_MSVC_RUNTIME_LIBRARY=MultiThreaded` on Windows).
3. Build `hqq-sqlite-server` and the C unit tests, then run `ctest -R hqqsqlite_server`.
4. Copy the binary to `dist/hqq-sqlite-server_<goos>_<goarch>[.exe]` and strip debug symbols (`strip --strip-all` on Linux, `strip -x` plus ad-hoc `codesign` on macOS).
5. Smoke-test the stripped binary with `HQQ_SQLITE_SERVER=<binary> go test ./sqlite/...` and `./sqlite/e2e/...` (failing the job if any test other than `TestHelper*` subprocess helpers is skipped).
6. Record shared-library linkage via `scripts/release/linkage.sh` (`readelf`/`ldd` on Linux, `otool -L` on macOS, `dumpbin /DEPENDENTS` on Windows).

## Cutting a release

1. Verify that `main` is green across `ci.yml`, `c.yml`, and `sqlite.yml`, and update `CHANGELOG.md` with the release notes.
2. Trigger a dry run first: in GitHub Actions, select **Release** -> **Run workflow** with `dry_run` set to `true`. Confirm all six platform binaries build, pass smoke tests, and upload unsigned workflow artifacts.
3. Create and push an annotated release tag:

   ```sh
   git tag -a vX.Y.Z -m "hqq vX.Y.Z"
   git push origin vX.Y.Z
   ```

4. Pushing the tag triggers the full release workflow: once all platform builds succeed, the `publish` job generates `licenses.tar.gz` and `manifest.json`, signs `manifest.json` with `HQQ_RELEASE_SIGNING_KEY`, verifies the signature against the committed `sqlite/launcher/release_public_key.hex` (including a negative tamper check), writes `SHA256SUMS`, and publishes the GitHub Release.
5. Inspect the published release page, download one platform binary along with `manifest.json` and `manifest.json.sig`, and run the verification check below.

To publish an existing tag manually, run the workflow on that tag with `dry_run` set to `false` (the workflow refuses to publish from a branch ref). When `HQQ_RELEASE_SIGNING_KEY` is absent (such as on forks or dry runs), the workflow uploads the unsigned bundle as the `release-unsigned` workflow artifact and skips publishing.

## Signing keys

Release manifests are signed with ed25519 via `cmd/hqq-manifest`:

- **Public key**: committed at `sqlite/launcher/release_public_key.hex` (a single 64-character hex line). Both the CI `publish` job and downstream consumers (`sqlite/launcher`) verify signatures against this key.
- **Private key**: stored exclusively in the GitHub Actions secret `HQQ_RELEASE_SIGNING_KEY` (scoped to the `publish` job) and backed up in encrypted offline storage. Never commit the private key to the repository.

To generate the initial signing key pair or rotate keys on a trusted offline machine:

```sh
go build -o /tmp/hqq-manifest ./cmd/hqq-manifest
/tmp/hqq-manifest keygen -priv /tmp/release.key -pub /tmp/release.pub
# commit /tmp/release.pub as sqlite/launcher/release_public_key.hex (public key only)
gh secret set HQQ_RELEASE_SIGNING_KEY < /tmp/release.key
# store /tmp/release.key in encrypted offline backup, then shred/remove the plaintext file
rm /tmp/release.key
```

`keygen` writes the private key with mode `0600` and refuses to overwrite existing files. If `sqlite/launcher/release_public_key.hex` is missing or still contains a placeholder, the `publish` job aborts before signing.

**Key rotation and compromise recovery**:

- **Routine rotation**: commit the new public key to `sqlite/launcher/release_public_key.hex` and update `HQQ_RELEASE_SIGNING_KEY`. Keep previous public keys documented in commit history so applications pinned to older releases can continue verifying them.
- **Compromise recovery**: if the private signing key ever leaks, delete `HQQ_RELEASE_SIGNING_KEY` immediately, generate and commit a replacement key pair, publish a new patch release signed with the new key, and mark affected release tags as revoked in their release notes.

## Verifying a download (Go)

Embed your trusted copy of `release_public_key.hex` in your Go binary, verify `manifest.json.sig` with `manifest.Verify` (which checks the ed25519 signature before parsing JSON), and launch the binary with `secureexec.Verify`:

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
	dir := "hqq-sqlite-release" // directory containing downloaded release assets

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
	m, err := manifest.Verify(data, sig, pub) // signature checked before JSON parse
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

Always supply the public key from your own compiled binary or configuration, never from an untrusted file downloaded alongside the release archive.

From a local checkout, you can run the equivalent verification on the command line:

```sh
go run ./cmd/hqq-manifest verify -pub release_public_key.hex \
  -in manifest.json -sig manifest.json.sig -dir hqq-sqlite-release
sha256sum -c SHA256SUMS
```

## Reproducibility and limits

- **Runner toolchain drift**: while source inputs are pinned (SQLite archive digests, Go version in `go.mod`, CMake flags, and `-ffile-prefix-map` to strip build paths), GitHub-hosted runner images (`ubuntu-24.04`, `macos-latest`, `windows-latest`) update their system compilers over time. Builds are therefore not bit-for-bit reproducible across different runner image revisions; each build records its exact dynamic dependencies in `licenses/linkage/*.txt`.
- **Trust root**: manifest verification relies on the secrecy of `HQQ_RELEASE_SIGNING_KEY` and the integrity of the public key embedded in the verifier.
- **Unsigned `SHA256SUMS`**: `SHA256SUMS` and `licenses.tar.gz` are not covered by `manifest.json.sig`; binary authenticity comes from `manifest.json` + `manifest.json.sig`.
- **OS code-signing**: macOS binaries carry only an ad-hoc `codesign` signature (required by arm64 Darwin kernels) and are not Apple-notarized; Windows binaries are not Authenticode-signed. Use `secureexec` + `manifest.Verify` to verify integrity directly.

## Files

Key files participating in the release pipeline:

- `.github/workflows/release.yml`: GitHub Actions build, smoke-test, signing, and release workflow.
- `scripts/release/fetch-sqlite.sh` and `scripts/release/fetch-sqlite.ps1`: download and digest verification for the SQLite amalgamation.
- `scripts/release/write-licenses.sh`: assembles `LICENSE`, `SQLITE-PUBLIC-DOMAIN.txt`, and `NOTICE.md`.
- `scripts/release/linkage.sh`: generates per-binary shared-library linkage reports.
- `third_party/sqlite/`: pinned SQLite amalgamation version and `SHA256` / `SHA3-256` digests.
- `cmd/hqq-manifest` and `secureexec/manifest`: ed25519 manifest generation, signing, and verification.
