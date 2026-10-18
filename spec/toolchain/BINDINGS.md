# Language Bindings: Toolchains, CI, and C API Contract

**Status**: Milestone M7 (complete).

The official HQQ language bindings live under [`bindings/`](../../bindings): C++17 in [`bindings/cpp`](../../bindings/cpp) and Rust in [`bindings/rust`](../../bindings/rust). Both bindings wrap the C11 reference library (`libhqq` in [`c/`](../../c)) and interoperate with the Go and C implementations through the two-process test contract defined in [`conformance/INTEROP_CLI.md`](../../conformance/INTEROP_CLI.md). Continuous integration is configured in [`.github/workflows/bindings.yml`](../../.github/workflows/bindings.yml).

## 1. Design Rules Across All Bindings

Every official binding in `bindings/` adheres to five invariants:

- **Zero network access at build time**: Neither CMake `FetchContent` nor external `crates.io` dependencies are permitted. Every build compiles `libhqq` directly from the local [`c/`](../../c) directory: CMake adds `c/` via `add_subdirectory`, and Cargo's `hqq-sys/build.rs` parses and compiles the `HQQ_SOURCES` list directly from `c/CMakeLists.txt`.
- **Single source of truth for C sources**: Because `hqq-sys/build.rs` extracts the source file list from `c/CMakeLists.txt`, the CMake and Cargo builds cannot drift out of sync when C source files are added or renamed.
- **RAII resource ownership and thread safety**: Each binding manages its `libhqq` pointers through move-only RAII handles. Every blocking operation accepts an explicit deadline (`hqq_wait`). Thread-safety guarantees mirror `libhqq`: link and acceptor handles may be shared across threads (`Send + Sync` in Rust), whereas reservation handles, incoming message handles, and server request contexts are single-owner types.
- **Exact error hierarchy mapping**: Every `hqq_status` code maps 1-to-1 to a typed error (`hqq::Status` with `std::error_code` support in C++, and `hqq::Error` / `hqq::ErrorKind` in Rust). Category matching delegates directly to `hqq_error_is()` so the Go `errors.Is` hierarchy (`SPEC.md` section 12) is preserved without duplicating matching tables.
- **No unwinding across the C FFI boundary**: In Rust, the RPC request handler wrapper catches panics via `std::panic::catch_unwind` and replies with an application error frame rather than unwinding across C stack frames. In C++, `hqq_adv_serve` callbacks must not allow exceptions to escape.

## 2. Toolchain Matrix

The C++17 and Rust bindings are verified against the following compiler and platform combinations:

| Binding | Toolchain Requirements | CI Runners | Local Verification |
| --- | --- | --- | --- |
| **C++17** | C++17 compiler, CMake 3.21+, Ninja, and a C11 compiler providing always-lock-free 32/64-bit atomics (GCC 13/14, Clang 18/20, Apple Clang, `clang-cl`, or MSVC 17.5+ with `/experimental:c11atomics`; see [`C_TOOLCHAIN.md`](C_TOOLCHAIN.md)). | `ubuntu-24.04` (GCC, Clang 18), `macos-latest` (Apple Clang), `windows-latest` (`clang-cl`) | Apple Clang 21, CMake 4.3, Ninja |
| **Rust** | Stable Rust (`rust-version = "1.75"`, zero external crate dependencies); C compiler and archiver via `$HQQ_CC`/`$CC` and `$HQQ_AR`/`$AR`. On `*-msvc` targets, `hqq-sys/build.rs` invokes `clang-cl` (`HQQ_CC=clang-cl` in CI) or `cl.exe` (adding `/std:c11 /experimental:c11atomics /DHQQ_FORCE_C11_ATOMICS=1`) and archives with `lib.exe`. | `ubuntu-24.04`, `macos-latest`, `windows-latest` (`ilammy/msvc-dev-cmd` + `clang-cl`) | `cargo` / `rustc` 1.96, Apple Clang 21 |

Platform-specific notes:

- **Windows C compilation in Cargo**: The Windows CI job sets `HQQ_CC=clang-cl`, matching the primary Windows compiler in `.github/workflows/c.yml`. When `cl.exe` is used instead, `hqq-sys/build.rs` automatically passes `/std:c11`, `/experimental:c11atomics`, and `/DHQQ_FORCE_C11_ATOMICS=1`.
- **Windows shell environment**: On Windows runners, Rust example binaries are executed from PowerShell (`shell: pwsh`) rather than Git Bash so that GNU coreutils `/usr/bin/link.exe` does not shadow the MSVC `link.exe` linker on `PATH`.
- **Windows shared-memory lifetime**: Because Windows kernel file mappings live only while at least one handle remains open, the C++ and Rust `hqq-interop` helpers follow the Go reference helper: scenario `X-01` (`creator-closes-first`) accepts `ErrLinkBusy` when probing the name while the opener still holds its handle, and POSIX-only persistence scenarios (`X-08` and `X-09`) are omitted on Windows.

## 3. CI Workflow (`bindings.yml`)

Each job in [`.github/workflows/bindings.yml`](../../.github/workflows/bindings.yml) compiles `libhqq` and the target binding from source, runs the binding's native unit tests and self-testing examples, builds the Go reference helper (`go build ./cmd/hqq-interop`), and executes `TestInteropMatrix` across all shared scenarios in both role directions (`creator=go,opener=<binding>` and `creator=<binding>,opener=go`):

| Job | Runners | Native Build and Test Steps | Interop Matrix Target |
| --- | --- | --- | --- |
| `cpp` | `ubuntu-24.04` (GCC; Clang with ASan+UBSan), `macos-latest` (Clang), `windows-latest` (`clang-cl`) | Configure with `-DHQQ_CPP_WERROR=ON`, build, and run `ctest`. | `cpp` (`build-cpp/hqq-interop`) |
| `rust` | `ubuntu-24.04`, `macos-latest`, `windows-latest` (`HQQ_CC=clang-cl`, `lib.exe`) | `cargo build --workspace --locked`, `cargo test --workspace --locked`, and run both example binaries with `--selftest`. | `rust` (`bindings/rust/target/release/hqq-interop`) |

## 4. Scope Boundaries

The following low-level C APIs and raw test hooks are intentionally scoped as follows:

- **Reservation RAII over callbacks**: Both bindings expose zero-copy slot access through RAII reservation types (`hqq_write_reserve` / `hqq_read_reserve`) rather than wrapping the C function-pointer callbacks (`hqq_read_zc` / `hqq_write_zc`).
- **Caller-owned memory**: `hqq_standard_open_memory` and `hqq_adv_open_memory` are exposed as `unsafe` functions in Rust (`StandardLink::open_memory`, `AdvancedLink::open_memory`) and as static factory methods in C++ (`StandardLink::open_memory`, `AdvancedLink::open_memory`).
- **Mid-initialization crash injection**: Interop crash points that require mutating raw superblock and ring header words mid-initialization (`claim`, `format`, `publish`, `zeroing`, and crafted wire violations in `X-21`) are implemented in the Go and C helpers (`cmd/hqq-interop` and `c/tools/hqq_interop.c`). The C++ and Rust helpers test the safe public wrapper APIs and return exit code `4` (`skipped`) for raw-memory corruption steps.

## 5. C API Behavioral Notes

Two subtleties in `c/include/hqq/hqq.h` are handled explicitly by the C++ and Rust bindings:

1. **Negotiating zero features (`features == 0`)**: In `libhqq`, `hqq_adv_negotiate(link, 0, wait)` treats `0` as a shorthand for `HQQ_FEAT_ALL_1_1`, whereas `hqq_adv_negotiate_protocol(link, version, 0, wait)` passes `0` literally on the wire (requesting no features). Both bindings call `hqq_adv_negotiate_protocol(link, version, 0, wait)` when the caller explicitly requests an empty feature set.
2. **Peer close immediately after graceful shutdown**: When a client closes its link immediately after receiving the final response before a server shutdown, `hqq_adv_serve` may observe `HQQ_ERR_PEER_CLOSED` on the receive ring before exiting its serve loop. The C++ and Rust example servers treat both `HQQ_OK` and `HQQ_ERR_PEER_CLOSED` after `shutdown()` as a clean service stop.
