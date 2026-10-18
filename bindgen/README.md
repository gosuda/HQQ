# `hqq-bindgen`: Cgo-Free Go-to-C IPC Code Generator

`hqq-bindgen` compiles a declarative interface description (`.idl`) into a pure-Go client package and a C server dispatcher. The target C library executes inside its own OS process behind the generated C server, while the Go application links zero C code and communicates with the server over HQQ shared memory.

The normative specification is [`spec/toolchain/BINDGEN.md`](../spec/toolchain/BINDGEN.md), covering the IDL grammar and validation rules V1 through V41 (§2), the wire protocol with worked hexadecimal byte vectors (§4), the session and generation-checked handle model (§5), error propagation (§6), threat model and memory safety (§7), threading (§8), runtime APIs (§9), test plan (§10), and architectural trade-offs (§12). This document summarizes the toolchain layout, operational limits, and measured performance against cgo.

```sh
go run gosuda.org/hqq/cmd/hqq-bindgen -go-out client -go-strip ml_ -c-out server mylib.idl
go run gosuda.org/hqq/cmd/hqq-bindgen -check -go-out client -go-strip ml_ -c-out server mylib.idl   # CI: fail on stale output
go run gosuda.org/hqq/cmd/hqq-bindgen -print-digest mylib.idl                                       # print 256-bit interface digest
```

## What is generated

For a service declared in `<service>.idl`, `hqq-bindgen` emits three files:

| Generated File | Role |
| :--- | :--- |
| `<service>_client.go` | Pure-Go client methods, pipelined futures (`...Async`), opaque handle wrappers, and error types layered on `bindgen/rt`. |
| `<service>_server.h`, `<service>_server.c` | C dispatch table and per-function glue that validates incoming frames, decodes arguments in place, checks handle generations, calls the C library, and encodes responses into reserved ring slots via `hqq_rpc.h`. |

You supply the `.idl` file, any optional `@intercept` pre-call policy hooks, C `shim` functions for composite operations (such as batching SQLite rows into a single frame), and a short `main.c` calling `hqqrpc_main`. By construction, only functions declared in the IDL are reachable (default deny), C pointers never cross the process boundary (clients hold generation-checked 64-bit handle IDs), and client and server verify a SHA-256 digest of the canonical IDL during the `HELLO` handshake.

## Layout

| Path | Description |
| :--- | :--- |
| `cmd/hqq-bindgen` | Command-line code generator binary. |
| `bindgen/idl` | Lexer, parser, validation rules V1–V41, canonical printer, interface digest, and wire layout planner. |
| `bindgen/gen` | Deterministic Go and C code emitters. |
| `bindgen/rt` | Pure-Go client runtime: frame codecs, `Conn`, pipelined futures, credit flow control, and `Pool`. |
| `c/include/hqq/hqq_rpc.h`, `c/src/hqq_rpc*.c` | C server runtime: frame validation, handle table, in-place readers/writers, session threads, and `hqqrpc_main`. |
| `bindgen/testdata` | Reference C library `microlib`, test IDLs, and golden wire vectors extracted from `spec/toolchain/BINDGEN.md`. |
| `bindgen/e2e/microlib` | Generated client and server for `microlib` plus the end-to-end integration and differential test suite. |
| `bindgen/bench` | Microbenchmarks comparing generated IPC calls directly against in-process cgo ([`bindgen/bench/README.md`](bench/README.md)). |
| `bindgen/examples/zstd` | Pinned zstd 1.5.7 wrapped via `hqq-bindgen`, with correctness tests and cgo crossover benchmarks ([`bindgen/examples/zstd/README.md`](examples/zstd/README.md)). |
| `bindgen/examples/sqlite3`, `sqlite` | Low-level SQLite IDL service, the `hqq-sqlite-server` C server, and the `hqq-sqlite` `database/sql` driver ([`bindgen/examples/sqlite3/README.md`](examples/sqlite3/README.md)). |
| `launch` | Verified child-process starter (`secureexec` + `lifeline` + `ready <listener>` handshake). |

## Operational limits

- **Target languages and OS**: generates Go clients and C11 servers. The C server entry point (`hqqrpc_main`) runs on Linux, macOS, FreeBSD, and Windows.
- **Handle-based call cancellation (`@cancel`)**: handle types can declare `@cancel(fn)` (for example `@cancel(sqlite3_interrupt)` on `handle Db`). Calling `Conn.Cancel()` sends a `BG_CANCEL` request on the control link (`HQQRPC_CANCEL_METHOD`), sets the session's canceled watermark (`hqqrpc_session_canceled`), and invokes the `@cancel` hook on every live handle of that type in the session. For handles without `@cancel`, an abandoned client wait leaves the server-side call running to completion (`spec/toolchain/BINDGEN.md` §6.3).
- **IDL type surface**: no C-to-Go callbacks, no variadic functions, and no struct/union value parameters. Composite inputs or batched outputs are expressed as byte buffers or packed records via a C `shim` function.
- **Concurrency and serialization**: each `rt.Conn` is used by one goroutine at a time (`rt.Pool` manages concurrent goroutine access), and each server session runs on a dedicated OS thread. `@serialize(session)` acquires a per-session or server lock around every C call, while `@serialize(none)` omits the lock for thread-safe functions.
- **Fault containment**: a crash (`SIGSEGV`, `SIGABRT`) inside the C library terminates the C server process without corrupting the Go client process, which observes `ErrPeerGone` and can launch a fresh server.

## Measured status

All measurements below were collected on an Apple M1 (`darwin/arm64`, Go 1.27, quiet machine, spin wait policy, median of 3 runs at `-benchtime 1s` for `bindgen/bench` and `sqlite`, and `-benchtime 300ms` for `bindgen/examples/zstd`). The C server runs as a separate child process placed by the macOS kernel scheduler (macOS does not expose thread-affinity APIs). Reproduction commands are in [`bindgen/bench/README.md`](bench/README.md).

### 1. Call with no work (`ml_add`)

Baseline integer addition (`25 ns` via in-process cgo):

| Path | Latency (`ns/call`) |
| :--- | ---: |
| `cgo` | `25` |
| `hqq-bindgen`, synchronous | `335` |
| `hqq-bindgen`, pipelined (`8` in flight) | `175` |
| `hqq-bindgen`, pipelined (`32` in flight) | `164` |
| `hqq-bindgen`, pipelined (`64` in flight) | `178` |

A synchronous no-op call costs ~13× an in-process cgo transition (~335 ns vs ~25 ns), and a pipelined call costs ~6.5× (~164 ns). The underlying `StandardLink` round trip accounts for ~193 ns (`BenchmarkCompareRoundTrip`); the remaining ~140 ns covers request framing, handle/bounds validation, response slot reservation, and the session lock. `hqq-bindgen` is not intended for nanosecond-scale getters executed in a tight synchronous loop.

### 2. Call with controlled CPU work (`ml_work(iters)`)

A dependency chain of `iters` steps (`ns/call`):

| `iters` | `cgo` | `hqq` sync | `hqq` 4 in flight | `hqq` 64 in flight |
| ---: | ---: | ---: | ---: | ---: |
| `0` | `25` | `331` | `178` | `181` |
| `64` | `83` | `400` | `246` | `243` |
| `256` | `396` | `1,795` | `568` | `565` |
| `1,024` | `1,650` | `3,083` | `1,866` | `1,868` |

When pipelined from a single goroutine, `hqq-bindgen` trails `cgo` by a flat 155–220 ns per call across the entire sweep. On macOS, the synchronous column exhibits a step increase around `iters=256` (~1,400 ns overhead vs ~305 ns at `iters=0`). Running the exact same benchmark inside a `linux/arm64` container on the same M1 with client and server pinned to distinct CPU cores (`HQQ_BENCH_SERVER_CPU=1 taskset -c 0`) keeps synchronous overhead flat across every work size (`iters=0`: 403 ns vs 26 ns, overhead 376 ns; `iters=64`: 501 ns vs 98 ns, overhead 403 ns; `iters=256`: 962 ns vs 486 ns, overhead 476 ns; `iters=1,024`: 2,435 ns vs 2,077 ns, overhead 358 ns), confirming that the macOS `iters=256` jump comes from Darwin core placement rather than the C server runtime.

### 3. Buffer transfer (`ml_xor`)

XOR over an input buffer into an equal-sized output buffer (`cgo` passes Go slice pointers directly):

| Payload Size | `cgo` | `hqq` sync | `hqq` 8 in flight | `cgo` (`GB/s`) | `hqq` 8 in flight (`GB/s`) |
| ---: | ---: | ---: | ---: | ---: | ---: |
| `64 B` | `65 ns` | `375 ns` | `227 ns` | `0.99` | `0.28` |
| `1 KiB` | `89 ns` | `567 ns` | `281 ns` | `11.5` | `3.64` |
| `64 KiB` | `1,338 ns` | `15,788 ns` | `9,068 ns` | `49` | `7.2` |

At 64 KiB, performance is dominated by copying the request into shared memory and copying the response out, whereas `cgo` reads and writes the Go heap buffer in place. Enabling defensive request copying (`copy_request`, design §7.2) adds 10% on synchronous 64 B calls (414 ns vs 375 ns), 4% at 1 KiB (590 ns vs 567 ns), 0–2% when pipelined, and 1% at 64 KiB (15,996 ns vs 15,788 ns), so it remains opt-in. Increasing the ring slot size so 64 KiB fits in a single frame (`hqq-sync-bigbuf`) lowers synchronous 64 KiB latency from 15,788 ns to 13,001 ns.

### 4. Real-world C library: zstd 1.5.7 (level 1, text corpus)

Calling the same zstd 1.5.7 C implementation via in-process `cgo` (`cgo_sync`) versus the `hqq-bindgen` server (`server_sync` and `server_pipelined`). Ratios report `server_time / cgo_time`:

| Payload | Compress Sync | Compress Pipelined | Decompress Sync | Decompress Pipelined |
| ---: | ---: | ---: | ---: | ---: |
| `64 B` | `1.39×` | `1.15×` | `2.2×` | `1.4×` |
| `1 KiB` | `1.20×` | `1.09×` | `1.77×` | `1.14×` |
| `64 KiB` | `1.18×` | `1.01×` | `1.56×` | `1.07×` |
| `1 MiB` | `1.04×` | `1.00×` | `1.20×` | `1.05×` |
| `64 MiB` | `1.04×` | not run | `1.29×` | not run |

Absolute latencies behind the ratios above:

| Operation | `cgo_sync` | `server_sync` | `server_pipelined` |
| :--- | ---: | ---: | ---: |
| Compress `64 B` | `911.5 ns` | `1,271 ns` | `1,044 ns` |
| Compress `1 KiB` | `4,618 ns` | `5,559 ns` | `5,020 ns` |
| Compress `64 KiB` | `160,754 ns` | `189,990 ns` | `162,757 ns` |
| Compress `1 MiB` | `2.784 ms` | `2.887 ms` | `2.778 ms` |
| Compress `64 MiB` | `180.5 ms` | `187.8 ms` | not run |
| Decompress `64 B` | `232.5 ns` | `517.8 ns` | `329.0 ns` |
| Decompress `1 KiB` | `2,024 ns` | `3,586 ns` | `2,301 ns` |
| Decompress `64 KiB` | `49,823 ns` | `77,627 ns` | `53,322 ns` |
| Decompress `1 MiB` | `697,856 ns` | `835,988 ns` | `731,675 ns` |
| Decompress `64 MiB` | `44.59 ms` | `57.67 ms` | not run |

For coarse compression workloads (`64 KiB` to `1 MiB`, including `256 KiB` at `705,377 ns` vs `695,542 ns`, or `1.4%`), pipelined `hqq-bindgen` stays within **2% of in-process cgo** on one goroutine, and two concurrent sessions against two cgo threads stay within **2% at 1 MiB** (`1.515 ms` vs `1.490 ms`, `1.02×`), **11% at 64 MiB** (`138.6 ms` vs `124.3 ms`), and **11% at 64 KiB** (`96.7 µs` vs `87.1 µs`). A no-op `ZSTD_versionNumber()` call takes `26.7 ns` via cgo, `339.8 ns` synchronous, and `155.8 ns` pipelined. Full benchmark details live in [`bindgen/examples/zstd/README.md`](examples/zstd/README.md).

### 5. SQLite service (`hqq-sqlite` on `hqq-bindgen`)

`gosuda.org/hqq/sqlite` (`"hqq-sqlite"`) implements `database/sql/driver` directly over the `hqq-bindgen` SQLite client (`bindgen/examples/sqlite3`) and C server (`hqq-sqlite-server`):

| Workload | `hqq-sqlite` (`hqq-bindgen`) |
| :--- | ---: |
| `SELECT 1` | `3.5 µs` |
| Point `SELECT`, prepared | `3.1 µs` |
| `INSERT` 1,000 rows in one transaction | `1.00 ms` |
| `INSERT`, autocommit | `9.8 µs` |
| Scan 100,000 rows (`fetch=256`) | `19.5 ms` |
| Scan 100,000 rows (`fetch=4096`) | `20.8 ms` |
| `1 MiB` BLOB round trip | `2.02 ms` |
| Point `SELECT`, 8 parallel connections | `4.0 µs` |

Each session uses a dedicated `StandardLink` ring and a dedicated server thread, calling `sqlite3_*` directly on the receive thread with zero intermediate queueing and supporting mid-query interruption via `@cancel(sqlite3_interrupt)`. `hqq-sqlite` passes all 107 tests across 11 groups in `sqlite/internal/contract`.

## Tests

Run the `hqq-bindgen` test suites from the repository root:

| Command | Coverage |
| :--- | :--- |
| `go test ./bindgen/... ./cmd/hqq-bindgen` | IDL parser (validation rules V1–V41, digests, wire plans), Go/C code emitters, Go client runtime (`bindgen/rt`), and end-to-end tests against the C `microlib` server. |
| `ctest` in a CMake build of `c/` | C RPC runtime (`test_rpc`, `test_rpc_calls`, `test_rpc_handles`, `test_rpc_fuzz`, `test_rpc_live`, `test_rpc_main`), run with ASan, UBSan, and TSan in `.github/workflows/bindgen.yml`. |
| `go test ./sqlite/...` | The `hqq-sqlite` `database/sql` driver, parameter/DSN packages, and the 107-case `database/sql` contract suite. |
| `go test -tags zstdcgo ./bindgen/examples/zstd/...` | End-to-end zstd correctness and differential tests against cgo (after `eval "$(bindgen/examples/zstd/build.sh)"`). |
