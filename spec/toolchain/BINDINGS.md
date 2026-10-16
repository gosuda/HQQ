# Language bindings: toolchains, CI and C API requests

Status: milestone M7. The bindings live in [`bindings/`](../../bindings): C++17
([`bindings/cpp`](../../bindings/cpp)), Rust ([`bindings/rust`](../../bindings/rust))
and Zig ([`bindings/zig`](../../bindings/zig)). They wrap the C reference library
(`libhqq`, [`c/`](../../c)) and must interoperate with the Go implementation through
the helper contract in [`conformance/INTEROP_CLI.md`](../../conformance/INTEROP_CLI.md).
The CI workflow is [`.github/workflows/bindings.yml`](../../.github/workflows/bindings.yml).

## 1. Rules that apply to every binding

- **No network at build time.** No crates.io, Zig package or CMake FetchContent
  dependency. The C library is compiled from `c/` in every build:
  CMake adds `c/` as a subdirectory, Cargo's `hqq-sys` build script compiles the
  `HQQ_SOURCES` list of `c/CMakeLists.txt`, and `build.zig` compiles the same list.
- **One C source list.** The Rust build reads `c/CMakeLists.txt` (the `HQQ_SOURCES`
  block) so the sources cannot drift. The Zig build keeps a copy of that list and
  must be updated when `HQQ_SOURCES` changes.
- **Ownership.** A binding owns its handles (RAII in C++ and Rust, `defer` in
  Zig). Every blocking call takes a deadline (`hqq_wait`); the bindings keep the
  C threading rules: a link may be used from any thread, a message or request
  context by one owner at a time.
- **Error mapping.** Each `hqq_status` maps to one error value (C++ `hqq::Status`
  with `std::error_code`, Rust `hqq::Error`, Zig error set). `hqq_error_is()`
  provides the Go "wraps" relation and is used, not re-implemented.
- **Callbacks never unwind into C.** The Rust serve handler catches panics and
  answers with an error response. The C++ serve handler must not throw.

## 2. Toolchains

| Binding | Requirement | Pinned in CI | Verified here |
| --- | --- | --- | --- |
| C++ | C++17 compiler, CMake 3.21+, Ninja; the C library needs always-lock-free C11 atomics (CI tests GCC 13/14, Clang 18/20 and clang-cl; MSVC 17.5+ is best effort, see `C_TOOLCHAIN.md` 3.1) | ubuntu-24.04 (gcc, clang-18), macos-latest (clang), windows-latest (clang-cl) | Apple clang 21, CMake 4.3, Ninja |
| Rust | stable Rust; the workspace declares `rust-version = 1.75`, which is not verified (only 1.96 is installed here); C compiler and archiver: `$HQQ_CC`/`$CC` (default `cc`), `$HQQ_AR`/`$AR` (default `ar`); MSVC targets use `cl.exe` and `lib.exe` | the runner's preinstalled rustup stable; Windows through `ilammy/msvc-dev-cmd@v1.13.0` | cargo/rustc 1.96, Apple clang 21 |
| Zig | Zig 0.15.2 (the build API of `build.zig`: `addLibrary`/`addExecutable` with `root_module`, `addCSourceFiles`) | `mlugg/setup-zig@v2.2.1` with `version: 0.15.2` on Linux and macOS | not installed: unverified |

Notes:

- Zig 0.15.2 (2025-10-11) is the last 0.15 release listed on ziglang.org. The download
  page now lists newer releases; porting `bindings/zig` to a newer Zig is a separate task,
  and the pin stays at 0.15.2 until then.
- Windows is covered by CI only (the host of this work is macOS). The MSVC
  path of `hqq-sys/build.rs` (`cl`, `lib`, `/experimental:c11atomics`) is unverified.

## 3. CI jobs (`bindings.yml`)

Each job builds the binding and the C library from source, runs the binding's
tests and examples, builds the Go helper (`go build ./cmd/hqq-interop`) and runs
`TestInteropMatrix` with `HQQ_INTEROP_IMPLS="go=<go helper>,<label>=<binding helper>"`.
The matrix runs every scenario that both helpers list, in both role directions.

| Job | Runners | Test steps | Interop label |
| --- | --- | --- | --- |
| `cpp` | ubuntu-24.04 (gcc; clang with ASan+UBSan), macOS (clang), Windows (clang-cl) | configure with `-DHQQ_CPP_WERROR=ON`, `ctest` | `cpp` (`build-cpp/hqq-interop`) |
| `rust` | ubuntu-24.04, macOS, Windows (MSVC) | `cargo build/test --workspace --locked`, both examples with `--selftest` | `rust` (`bindings/rust/target/release/hqq-interop`) |
| `zig` | ubuntu-24.04, macOS | `zig fmt --check`, `zig build`, `zig build test` | `zig` (`bindings/zig/zig-out/bin/hqq-interop`) |

The workflow runs on pushes to `main`, on pull requests, and on changes to
`bindings/**`, `c/**`, `conformance/**`, `interop/**`, `cmd/hqq-interop/**` and the
workflow itself. It uses the same pinned actions as `ci.yml` (`actions/checkout@v6.0.2`,
`actions/setup-go@v6.4.0`).

## 4. Not covered

- Zero-copy callbacks (`hqq_read_zc`, `hqq_write_zc`) are not wrapped by the C++
  and Rust bindings; reservations (`hqq_write_reserve` and friends) are wrapped in both.
- Caller-owned memory (`hqq_standard_open_memory`, `hqq_adv_open_memory`) is wrapped
  as `unsafe` functions in Rust and as plain functions in C++.
- Crash points and raw protocol states of the interop contract (`claim`, `format`,
  `publish`, `zeroing`, protocol violations) need raw writes into the segment and are
  implemented only by the C helper (`c/tools/hqq_interop.c`). The other helpers report
  those steps as skipped (exit 4) and do not list them.
- Only the scenarios the binding's helper lists are run. Scenarios outside a binding's
  scope (see the helper's `list` output) are skipped by the driver, not failed.

## 5. C API requests

The C owner's work is finished, so requests are recorded here for a future C change.
Each request has a workaround in the bindings; none blocks a binding.

1. **Document `features == 0` in `hqq_adv_negotiate_protocol`.** `hqq_adv_negotiate()`
   maps 0 to "every feature of protocol 1.1", but `hqq_adv_negotiate_protocol()` passes 0
   through literally (no features). Both behaviours are observed, and the header says
   only the first. Workaround: the bindings call `negotiate_protocol(version, 0)` for the
   "no features" case and never call `negotiate(0)` for it.
2. **Document the peer-close outcome of `hqq_adv_serve` after a shutdown.** When the
   client closes its link right after receiving its response to a stop request, the
   service's `hqq_adv_serve` can return `HQQ_ERR_PEER_CLOSED` rather than `HQQ_OK`, because
   the receive path fails first. The header says "then HQQ_OK after the in-flight requests
   completed", which the observed behaviour does not always match. Workaround: the
   examples accept `PEER_CLOSED` as the normal end of service after a stop.

## 6. Open items

- The Zig binding, and the Windows paths of the C++ and Rust bindings, are not run
  on this host. The main agent's verification must run `zig build test` and the
  interop matrix on Linux and macOS, and the Windows job in CI.
- `bindings/zig` may not pass `zig fmt --check` until it is formatted with Zig 0.15.2.
