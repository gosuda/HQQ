# libhqq bindings: C++, Rust, Zig

Language bindings for the C reference library [`c/`](../c) (`libhqq`). Each binding
wraps the three layers of the C API (Standard link, Advanced layer, acceptor and
connect) and ships an `hqq-interop` helper, so its programs interoperate with the Go
implementation through the contract in [`conformance/INTEROP_CLI.md`](../conformance/INTEROP_CLI.md).
Toolchain requirements, CI pins and the C API requests are in
[`spec/toolchain/BINDINGS.md`](../spec/toolchain/BINDINGS.md).

| Binding | Path | Interface | Status on the authoring host (macOS arm64) |
| --- | --- | --- | --- |
| C++17 | [`cpp/`](cpp) | header-only `include/hqq/hqq.hpp`, CMake target `hqq::cpp` | tests, examples and interop verified (Apple clang, CMake 4.3) |
| Rust | [`rust/`](rust) | Cargo workspace: `hqq-sys` (raw FFI), `hqq` (safe API), `hqq-interop` | tests, examples and interop verified (cargo 1.96) |
| Zig | [`zig/`](zig) | `build.zig`, module `hqq`, examples, `hqq-interop` | **unverified**: Zig is not installed on the host; written for Zig 0.15.2 |

None of the bindings needs network access to build: the C library is compiled from
`c/` in each build (CMake `add_subdirectory`, Cargo `build.rs`, `build.zig`).

## Layout

```text
bindings/
  README.md                 this file
  cpp/
    CMakeLists.txt          target hqq::cpp, tests, examples, hqq-interop (sanitizers, -Werror)
    include/hqq/hqq.hpp     the wrapper (C++17, exceptions off by default)
    tests/test_hqq.cpp      unit and integration tests (Standard, Advanced, acceptor)
    examples/echo.cpp       echo over a Standard link (--create / --open / --selftest)
    examples/rpc.cpp        request/response over an Advanced link
    interop/hqq_interop.cpp hqq-interop helper (contract)
  rust/
    Cargo.toml              workspace (no crates.io dependencies)
    hqq-sys/                raw FFI, build.rs compiles c/ with $CC and $AR
    hqq/                    safe API: Link, AdvLink, Stream, Message, Acceptor, Error
    hqq/tests/              integration tests; hqq/examples/ echo.rs and rpc.rs
    hqq-interop/            hqq-interop helper (contract)
  zig/
    build.zig               builds libhqq from c/, module hqq, examples, hqq-interop
    src/hqq.zig             wrapper and unit tests
    examples/               echo.zig, rpc.zig
    interop/hqq_interop.zig hqq-interop helper (contract, unverified)
```

## Build and test

C++ (any C++17 compiler; the C library needs always-lock-free C11 atomics):

```sh
cmake -S bindings/cpp -B build-cpp -G Ninja -DCMAKE_BUILD_TYPE=Release -DHQQ_CPP_WERROR=ON
cmake --build build-cpp
ctest --test-dir build-cpp --output-on-failure
# ASan + UBSan: add -DCMAKE_BUILD_TYPE=Debug -DHQQ_CPP_SANITIZE=address+undefined
# Exceptions in the tests: add -DHQQ_CPP_EXCEPTIONS=ON
```

Rust (stable; `hqq-sys` compiles the C sources with `$HQQ_CC`, `$CC` or `cc`, and archives
with `$HQQ_AR`, `$AR` or `ar`; `*-msvc` targets use `cl` and `lib`):

```sh
cd bindings/rust
cargo test --workspace
cargo run -p hqq --example echo -- --selftest
cargo run -p hqq --example rpc -- --selftest
cargo build --release -p hqq-interop      # target/release/hqq-interop
```

Zig 0.15.2 (unverified here):

```sh
cd bindings/zig
zig build
zig build test
zig build run-echo -- --create NAME       # or --open NAME, see examples/echo.zig
```

Interop with the Go implementation (run from the repository root, after building the
helpers; the paths are absolute or relative to the module root):

```sh
go build -o /tmp/hqq-interop-go ./cmd/hqq-interop
HQQ_INTEROP_IMPLS="go=/tmp/hqq-interop-go,cpp=$PWD/build-cpp/hqq-interop,rust=$PWD/bindings/rust/target/release/hqq-interop" \
  go test ./interop -run '^TestInteropMatrix$' -count=1 -timeout 30m
```

The matrix runs every scenario that both helpers list, in both role directions.

## API at a glance

The three bindings use the same concepts. Names follow each language.

| Concept | C | C++ (`hqq::`) | Rust (`hqq::`) |
| --- | --- | --- | --- |
| Standard link | `hqq_standard_create/open` | `Link::create/open` | `Link::create/open` |
| write / read one message | `hqq_write`, `hqq_read` | `write`, `read`, `receive` | `write`, `read`, `receive` |
| zero-copy reservations | `hqq_write_reserve`, ... | `reserve_write`, `reserve_read` | `reserve_write`, `reserve_read` |
| Advanced link | `hqq_adv_create/open` | `AdvLink::create/open` | `AdvLink::create/open` |
| negotiate | `hqq_adv_negotiate` | `negotiate(features, wait)` | `negotiate(features, wait)`, `negotiate_all` |
| streams | `hqq_adv_open_stream` ... | `open_stream`, `accept_stream`, `Stream` | `open_stream`, `accept_stream`, `Stream<'_>` |
| call | `hqq_stream_call` | `Stream::call` (`Message`, `Status`) | `Stream::call` (`Result<Message, CallError>`) |
| serve | `hqq_adv_serve` | `serve(handler, wait)` | `serve(handler, wait)` (`Fn + Sync`) |
| acceptor / connect | `hqq_acceptor_*`, `hqq_connect` | `Acceptor`, `connect` | `Acceptor`, `connect` |
| wait and cancel | `hqq_wait`, `hqq_cancel` | `Wait`, `CancelToken` | `Wait<'_>`, `CancelToken` |
| errors | `hqq_status` | `Status` (`std::error_code`), `Result<T>` | `Error` (one variant per status), `Result<T>` |

Ownership: C++ and Rust release every handle in the destructor (`Message` and
reservations included). Zig uses explicit `deinit`/`close`, with `defer` at the call site.
Streams and messages borrow the Advanced link they come from (Rust lifetimes; C++
documents it).

Errors are never swallowed: `Result`/`Status` carry the `hqq_status`, and `hqq_error_is`
provides the Go "wraps" relation (`Error::matches` in Rust, `Status::matches` in C++).

Example (Rust, from [`hqq/examples/echo.rs`](rust/hqq/examples/echo.rs)):

```rust
use hqq::{Link, Options, Wait};

let link = Link::open("my-link", 8, 4096, &Options::new(), None)?;
link.write(b"ping", Wait::timeout(std::time::Duration::from_secs(5)))?;
let reply = link.receive(Wait::timeout(std::time::Duration::from_secs(5)))?;
```

Example (C++, from [`cpp/examples/rpc.cpp`](cpp/examples/rpc.cpp)):

```cpp
auto link = hqq::AdvLink::open("my-service", 8, 4096);   // Result<AdvLink>
if (!link.ok()) return 2;
link.value().negotiate(HQQ_FEATURES_ALL, hqq::Wait::for_duration(std::chrono::seconds(5)));
hqq::Message resp;
hqq::Status st = link.value().stream0().call(1, std::string("hello"), resp, hqq::Wait{});
```

## Interop coverage

Each helper lists the scenarios it implements; the driver runs a scenario only when both
helpers list it, with the same variants and steps as the Go helper. The three-helper matrix
(`TestInteropMatrix` with `go`, `cpp` and `rust` labels) ran on this host: every subtest of
every pair passed, 0 failed, 0 skipped, in both role directions.

| Scenario | C++ | Rust | Zig (unverified) |
| --- | --- | --- | --- |
| X-01 create, open, exchange, close (2 variants) | yes | yes | yes |
| X-02 geometry mismatch (4 variants) | yes | yes | no |
| X-03 third endpoint, re-join | yes | yes | no |
| X-05 ring full, deadline | yes | yes | no |
| X-06 bidirectional stream, sequence and CRC (ring2, ring256) | yes | yes | yes |
| X-07 kill -9 of the peer (3 variants) | yes | yes | no |
| X-12 parked consumer woken (ring2) | yes | yes | yes |
| X-14 simple-mode peer (adaptive, spin-peer) | yes | yes | no |
| X-A04 request/response, concurrent calls, errors | yes | yes | yes |
| X-A08 request metadata (metadata, without-metadata) | yes | yes | no |
| X-A09 graceful shutdown | yes | yes | no |
| X-A10 acceptor, connect, busy listener, acceptor close | yes | yes | no |

Listed scenarios: C++ and Rust list 12 scenarios and 21 variants each; Zig lists 4
scenarios (X-01, X-06, X-12, X-A04), and its helper has never been compiled or run here.

"Run only" means implemented and checked by hand in both directions, but not listed: the
driver requires the Go variant and step sets for a listed scenario. C++ and Rust run X-A01
(six variants: `all-features`, `opener-initiates`, `simultaneous`, `messages-only`,
`request-response-only`, `none`), X-A02 (`sizes`), X-A05 (`basic`) and X-04 (`before-creator`,
`dead`). Zig runs X-A01 (`all-features`, `none`) and X-A05 (`basic`).

Not implemented in C++ and Rust: X-04 `initializing`, X-A01 `bad-major` and `op-error`,
X-A02 `sender-abort-after-chunk-1`, X-A05 `bad-parity` and `duplicate`, X-A07 (cancel,
nocancel), X-A03, X-A06, X-08, X-09, X-10, X-11, X-13, X-21. These need raw segment or frame
writes (only the C helper has them) or a platform the helper does not run on.


## Limits

- Zero-copy callbacks (`hqq_read_zc`, `hqq_write_zc`) are not wrapped; reservations are.
- The C++ wrapper's serve handler is a `F&&` invoked from worker threads; it must be
  thread-safe and must not throw.
- The Rust serve handler catches panics and answers with an error response
  (`HQQ_ERRCODE_INVALID_OP`, payload `handler panicked`).
- Windows is covered by CI only (C++ with clang-cl, Rust with MSVC, Zig not in CI on Windows).
- `Acceptor::close` (Rust `close_shared`) and `AdvLink::close` (C++) may run while another
  thread is inside `accept`/`call`/`serve`; the C library waits for those calls.

## Status of the verification

- C++: `hqq_cpp_test` passes with 105 checks in the default build and 106 with
  `HQQ_CPP_EXCEPTIONS=ON` (the exception path is checked only there); the default and
  ASan+UBSan CMake builds pass, plus the echo and rpc self-tests. macOS arm64 only.
- Rust: `cargo test --workspace --locked` passes (3 unit, 9 integration, 1 doc test);
  `cargo clippy --workspace --all-targets` is clean; echo and rpc self-tests pass.
- Interop: the three-helper matrix (go, cpp, rust) passes in both directions.
- Not run here: Zig (not installed), Windows, Linux.
