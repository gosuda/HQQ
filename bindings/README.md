# `libhqq` Language Bindings: C++17 and Rust

This directory contains the C++17 and Rust bindings for the [`libhqq`](../c/README.md) C11 reference library. Both bindings wrap all three layers of the protocol stack (Standard links, Advanced streams and RPC, and the multi-client Acceptor / Connect protocol) and include an `hqq-interop` binary implementing the cross-language test contract defined in [`conformance/INTEROP_CLI.md`](../conformance/INTEROP_CLI.md). Toolchain versions and FFI design rules are documented in [`spec/toolchain/BINDINGS.md`](../spec/toolchain/BINDINGS.md).

| Binding | Directory | Interface | Verification Status |
| :--- | :--- | :--- | :--- |
| **C++17** | [`cpp/`](cpp) | Header-only `include/hqq/hqq.hpp`, CMake target `hqq::cpp` | Unit tests, examples, and Go/C++/Rust interop verified on macOS arm64 and in CI (`bindings.yml`: Linux GCC/Clang with ASan+UBSan, macOS, Windows `clang-cl`). |
| **Rust** | [`rust/`](rust) | Cargo workspace: `hqq-sys` (FFI), `hqq` (safe API), `hqq-interop` | Unit, integration, and doc tests, examples, and interop verified on macOS arm64 and in CI (`bindings.yml`: Linux, macOS, Windows MSVC). |

Neither binding requires network access at build time: both compile `libhqq` directly from [`../c`](../c) (`add_subdirectory` in CMake, `build.rs` in Cargo) with zero external crate or package dependencies.

## Layout

```text
bindings/
  README.md                 Overview, build instructions, and API comparison
  cpp/
    CMakeLists.txt          CMake target hqq::cpp, tests, examples, hqq-interop
    include/hqq/hqq.hpp     C++17 RAII wrapper (exceptions disabled by default)
    tests/test_hqq.cpp      Unit and integration tests (Standard, Advanced, Acceptor)
    examples/echo.cpp       Standard link echo (--create / --open / --selftest)
    examples/rpc.cpp        Advanced link RPC example
    support/process_id.hpp  Cross-platform PID helper for unique test names
    interop/hqq_interop.cpp Cross-language interop CLI helper
  rust/
    Cargo.toml              Offline Cargo workspace (zero external dependencies)
    hqq-sys/                Raw C FFI bindings; build.rs compiles ../c
    hqq/                    Safe Rust API: Link, AdvLink, Stream, Message, Acceptor, Error
    hqq/tests/              Integration tests
    hqq/examples/           echo.rs and rpc.rs examples
    hqq-interop/            Cross-language interop CLI helper
```

## Build and test

Build and test the C++17 binding with CMake and Ninja (requires a C++17 compiler and a C11 compiler with lock-free atomics):

```sh
cmake -S bindings/cpp -B build-cpp -G Ninja -DCMAKE_BUILD_TYPE=Release -DHQQ_CPP_WERROR=ON
cmake --build build-cpp
ctest --test-dir build-cpp --output-on-failure
# With ASan + UBSan: add -DCMAKE_BUILD_TYPE=Debug -DHQQ_CPP_SANITIZE=address+undefined
# With C++ exceptions enabled in tests: add -DHQQ_CPP_EXCEPTIONS=ON
```

Build and test the Rust workspace with Cargo (`hqq-sys` compiles the C sources using `$HQQ_CC`, `$CC`, or `cc`, and archives with `$HQQ_AR`, `$AR`, or `ar`; `*-msvc` targets use `cl.exe` with `/std:c11 /experimental:c11atomics` or `clang-cl`):

```sh
cd bindings/rust
cargo test --workspace
cargo run -p hqq --example echo -- --selftest
cargo run -p hqq --example rpc -- --selftest
cargo build --release -p hqq-interop      # produces target/release/hqq-interop
```

To run the two-process cross-language interoperability matrix against the Go implementation from the repository root:

```sh
go build -o /tmp/hqq-interop-go ./cmd/hqq-interop
HQQ_INTEROP_IMPLS="go=/tmp/hqq-interop-go,cpp=$PWD/build-cpp/hqq-interop,rust=$PWD/bindings/rust/target/release/hqq-interop" \
  go test ./interop -run '^TestInteropMatrix$' -count=1 -timeout 30m
```

`TestInteropMatrix` executes every scenario supported by both helpers in a pair across both primary/secondary role assignments.

## API at a glance

Both bindings expose idiomatic RAII types that mirror the underlying `libhqq` concepts:

| Concept | C (`libhqq`) | C++17 (`hqq::`) | Rust (`hqq::`) |
| :--- | :--- | :--- | :--- |
| Standard link | `hqq_standard_create` / `open` | `Link::create` / `open` | `Link::create` / `open` |
| Whole-message I/O | `hqq_write`, `hqq_read` | `write`, `read`, `receive` | `write`, `read`, `receive` |
| Zero-copy reservations | `hqq_write_reserve`, ... | `reserve_write`, `reserve_read` | `reserve_write`, `reserve_read` |
| Advanced link | `hqq_adv_create` / `open` | `AdvLink::create` / `open` | `AdvLink::create` / `open` |
| Protocol negotiation | `hqq_adv_negotiate` | `negotiate(features, wait)` | `negotiate(features, wait)`, `negotiate_all` |
| Multiplexed streams | `hqq_adv_open_stream`, ... | `open_stream`, `accept_stream`, `Stream` | `open_stream`, `accept_stream`, `Stream<'_>` |
| RPC call | `hqq_stream_call` | `Stream::call` (`Message`, `Status`) | `Stream::call` (`Result<Message, CallError>`) |
| RPC server (`serve`) | `hqq_adv_serve` | `serve(handler, wait)` | `serve(handler, wait)` (`Fn + Sync`) |
| Multi-client acceptor | `hqq_acceptor_*`, `hqq_connect` | `Acceptor`, `connect` | `Acceptor`, `connect` |
| Deadlines & cancellation | `hqq_wait`, `hqq_cancel` | `Wait`, `CancelToken` | `Wait<'_>`, `CancelToken` |
| Error handling | `hqq_status`, `hqq_error_is` | `Status` (`std::error_code`), `Result<T>`, `Status::matches` | `Error`, `Result<T>`, `Error::matches` |

**Resource ownership**: both bindings release links, pooled `Message` buffers, and slot reservations automatically in their destructors (`Drop` in Rust, `~Message` / `~Link` in C++). Streams and messages borrow their parent `AdvLink` (enforced at compile time via lifetimes in Rust and documented in C++).

Rust example ([`rust/hqq/examples/echo.rs`](rust/hqq/examples/echo.rs)):

```rust
use hqq::{Link, Options, Wait};

let link = Link::open("my-link", 8, 4096, &Options::new(), None)?;
link.write(b"ping", Wait::timeout(std::time::Duration::from_secs(5)))?;
let reply = link.receive(Wait::timeout(std::time::Duration::from_secs(5)))?;
```

C++17 example ([`cpp/examples/rpc.cpp`](cpp/examples/rpc.cpp)):

```cpp
auto link = hqq::AdvLink::open("my-service", 8, 4096);   // Result<AdvLink>
if (!link.ok()) return 2;
link.value().negotiate(HQQ_FEATURES_ALL, hqq::Wait::for_duration(std::chrono::seconds(5)));
hqq::Message resp;
hqq::Status st = link.value().stream0().call(1, std::string("hello"), resp, hqq::Wait{});
```

## Interop coverage

Each language's `hqq-interop` binary advertises its supported scenarios via `--list`. Both the C++17 and Rust helpers implement 12 full scenarios (21 variants) verified against Go and against each other in both role directions:

| Scenario | C++17 | Rust |
| :--- | :--- | :--- |
| `X-01` create, open, exchange, graceful close (2 variants) | yes | yes |
| `X-02` geometry mismatch detection (4 variants) | yes | yes |
| `X-03` third-endpoint rejection and re-join after death | yes | yes |
| `X-05` ring-full backpressure and deadline expiry | yes | yes |
| `X-06` bidirectional stream with sequence and CRC checks (`ring2`, `ring256`) | yes | yes |
| `X-07` `SIGKILL` crash detection (3 variants) | yes | yes |
| `X-12` parked consumer wake-up across processes (`ring2`) | yes | yes |
| `X-14` `HQQ_WAIT_SIMPLE` peer interop (`adaptive`, `spin-peer`) | yes | yes |
| `X-A04` concurrent RPC request/response and application errors | yes | yes |
| `X-A08` Protocol 1.1 request metadata (`metadata`, `without-metadata`) | yes | yes |
| `X-A09` graceful `Shutdown` and request draining | yes | yes |
| `X-A10` multi-client acceptor, connect, busy listener, acceptor close | yes | yes |

Additional scenarios implemented as standalone subcommands (`X-04` `before-creator` and `dead`, `X-A01` six negotiation variants, `X-A02` `sizes`, and `X-A05` `basic`) can be invoked directly; scenarios that require injecting intentionally corrupted raw ring frames remain in the Go and C helpers.

## Design notes and limits

- **Reservations over callbacks**: C++17 and Rust wrap RAII slot reservations (`reserve_write`, `reserve_read`) rather than the C callback-based `hqq_read_zc` / `hqq_write_zc` functions.
- **Handler exception and panic safety**: C++ `serve` handlers are invoked from C worker threads and must be thread-safe and `noexcept`. Rust `serve` handlers catch panics at the FFI boundary with `catch_unwind` and reply with an `HQQ_ERRCODE_INVALID_OP` error frame (`"handler panicked"`).
- **Concurrent close**: `Acceptor::close` (Rust `close_shared`) and `AdvLink::close` safely wake and wait for threads blocked inside `accept`, `call`, or `serve`.
