# hqq-bindgen

`hqq-bindgen` generates a Go client and a C server from an interface description (IDL), so that Go can call a C library that runs in its own process, over HQQ shared memory, without cgo. The Go client never links C code. The C server links the real library and a small runtime (`libhqq`, `c/include/hqq/hqq_rpc.h`).

The normative design is [`spec/toolchain/BINDGEN.md`](../spec/toolchain/BINDGEN.md): the IDL (section 2), the wire format with worked byte vectors (4), the session and handle model (5), errors (6), security (7), threading (8), the runtime APIs (9), the test plan (10) and the decisions (12). This file is the map and the measured status.

```sh
go run gosuda.org/hqq/cmd/hqq-bindgen -go-out client -go-strip ml_ -c-out server mylib.idl
go run gosuda.org/hqq/cmd/hqq-bindgen -check -go-out client -go-strip ml_ -c-out server mylib.idl   # CI: fail on stale output
go run gosuda.org/hqq/cmd/hqq-bindgen -print-digest mylib.idl                                       # interface digest
```

## What is generated

| Output | Content |
| --- | --- |
| `<service>_client.go` | typed Go methods, futures (`...Async`), handle types, errors; calls `bindgen/rt` |
| `<service>_server.h`, `<service>_server.c` | the op table and the glue that decodes a frame, validates it, calls the C function and encodes the response; calls `hqq_rpc.h` |

You write the IDL, the interceptors (pre-call hooks that can refuse or rewrite a call), the `shim` functions (C functions for composite operations such as batching rows) and a `main.c` that calls `hqqrpc_main`. Only functions declared in the IDL can be called; everything else is refused (default deny). A client and a server compare the interface digest when a session opens, and a mismatch fails the connect.

## Layout

| Path | Content |
| --- | --- |
| `cmd/hqq-bindgen` | the command line |
| `bindgen/idl` | lexer, parser, validation rules V1 to V41, canonical form and digest, wire plan |
| `bindgen/gen` | the Go and C emitters |
| `bindgen/rt` | the Go runtime: frames, `Conn`, futures, flow control, `Pool` |
| `c/include/hqq/hqq_rpc.h`, `c/src/hqq_rpc*.c` | the C runtime: frame validation, handle table, writers, session threads, `hqqrpc_main` |
| `bindgen/testdata` | the MIT test library `microlib`, the IDL files, the golden vectors extracted from the design document |
| `bindgen/e2e/microlib` | generated client and server for `microlib` and the end-to-end suite |
| `bindgen/bench` | cgo comparison benchmarks (see its README for the exact commands) |
| `bindgen/examples/zstd` | zstd 1.5.7 (pinned source, BSD-3-Clause option) through the generator, with tests and benchmarks |
| `bindgen/examples/sqlite3`, `sqlite/gendriver` | a SQLite service built from a low-level IDL and a `database/sql` driver (`hqq-sqlite-gen`) on top of the generated client |
| `launch` | starts a generated server: verifies the binary, ties its life to the parent, waits for the ready line |

## Limits

- Output languages are Go (client) and C (server). The C server runs on POSIX systems; on Windows `hqqrpc_main` returns `HQQ_ERR_UNSUPPORTED`. The runtime sources compile on Windows and are part of the Rust build.
- No call cancellation: a call that times out is abandoned, its outcome is unknown, and the session stays usable only through the rules of 6.3. A running `sqlite3_step` cannot be interrupted through the generated service.
- No callbacks, no varargs, no composite in-parameters (a struct, union or array of structs as an argument); composite results and composite arguments go through `shim` functions.
- A `Conn` is for one goroutine at a time (`rt.Pool` hands out connections). A server session is served by one thread; `@serialize(session)` also holds a lock around every library entry, `@serialize(none)` removes it for libraries that tolerate concurrent use.
- Same-host, trusted-listener model: access control is the permission of the listener segment. A client that writes its own request slots concurrently can change arguments of functions that have no `@intercept` after the server validated them; the server stays memory safe (design document 7.2).
- A crash of the C library ends the server process. The process boundary is the containment; there is no in-process recovery.

## Measured status

Apple M1, macOS, Go 1.27, quiet machine, spin wait policy, median of 3 runs of `-benchtime 1s` (cgo benchmarks: `bindgen/bench`, `bindgen/examples/zstd`, `sqlite/gendriver`). The server runs as a separate process and the scheduler places it; macOS has no affinity API. Other machines will differ; run the benchmarks yourself with the commands in `bindgen/bench/README.md`.

**A call with no work.** `ml_add`, 25 ns through cgo.

| Path | ns/call |
| --- | ---: |
| cgo | 25 |
| generated client, synchronous | 310 |
| generated client, 8 / 32 / 64 calls in flight | 173 / 160 / 175 |

A synchronous call costs about twelve times a cgo call, and a pipelined call about six times. The floor of the link itself is one Standard-link round trip of about 190 ns (`BenchmarkCompareRoundTrip`); the generated path adds frame encoding, handle and bound checks, the response reservation and the execution lock on top. The design does not aim at tiny synchronous calls.

**A call with work.** `ml_work(iters)`, a dependency chain of known cost (cgo ns/call in the first column).

| `iters` | cgo | hqq synchronous | hqq, 4 in flight | hqq, 64 in flight |
| ---: | ---: | ---: | ---: | ---: |
| 0 | 25 | 310 | 176 | 175 |
| 64 | 82 | 377 | 243 | 242 |
| 256 | 395 | 1,338 | 583 | 561 |
| 1,024 | 1,649 | 2,852 | 1,883 | 1,866 |

With one calling goroutine the generated path does not cross cgo: pipelined, it trails cgo by 150 to 220 ns per call at every size measured. The synchronous column grows faster than the work (943 ns of overhead at `iters=256` against 285 ns at `iters=0`); I did not find the cause. A plausible one is the placement of the spinning server thread on Apple silicon, which I did not verify; a pinned Linux run (`HQQ_BENCH_SERVER_CPU`, `taskset`) is the way to check.

**Buffers.** `ml_xor` over a source and a destination of the same size; cgo passes the Go slices in place.

| Size | cgo | hqq synchronous | hqq, 8 in flight | cgo GB/s | hqq in flight GB/s |
| ---: | ---: | ---: | ---: | ---: | ---: |
| 64 B | 58 ns | 343 ns | 196 ns | 1.1 | 0.33 |
| 1 KiB | 80 ns | 499 ns | 246 ns | 12.8 | 4.2 |
| 64 KiB | 1.26 µs | 15.3 µs | 8.7 µs | 52 | 7.5 |

Two copies (request into the ring, response out of it) bound the large sizes. `copy_request` (design 7.2) costs 3 to 5% for synchronous 64 B and 1 KiB calls, up to 8% for pipelined 1 KiB calls, and nothing measurable at 64 KiB, so its default stays off. With a buffer size that holds the whole message in one frame (`hqq-sync-bigbuf`), 64 KiB synchronous takes 12.5 µs instead of 15.3 µs.

**zstd 1.5.7, level 1, text.** Same library called through cgo and through the generated client (`server_sync`, `server_pipelined`); `cgo_sync` is the baseline. Ratios are server time over cgo time.

| Payload | compress sync | compress pipelined | decompress sync | decompress pipelined |
| ---: | ---: | ---: | ---: | ---: |
| 64 B | 3.1× | 1.18× | 2.2× | 1.4× |
| 1 KiB | 1.8× | 1.08× | 2.3× | 1.1× |
| 64 KiB | 1.19× | 1.01× | 1.7× | 1.10× |
| 1 MiB | 1.05× | 1.01× | 1.22× | 1.06× |
| 64 MiB | 1.06× | not run | 1.29× | not run |

For coarse calls (compression of 64 KiB to 1 MiB) the pipelined generated client is within 2% of cgo with one goroutine; two sessions against two cgo threads are within 6% at 1 MiB (1.50 ms against 1.42 ms) and 14% at 64 MiB. Decompression moves more bytes per unit of work and pays for the response copy. The source of the crossover table is `bindgen/examples/zstd` (`go test -tags zstdcgo -bench 'Compress|Decompress'`).

**SQLite, hand-written driver against the generated one.** Same workloads, each against its own server (`sqlite/gendriver` benchmarks).

| Workload | `hqq-sqlite` | `hqq-sqlite-gen` |
| --- | ---: | ---: |
| `SELECT 1` | 118 µs | 18.0 µs |
| point select, prepared | 116 µs | 6.4 µs |
| insert 1,000 rows in one transaction | 59 ms | 5.3 ms |
| insert, autocommit | 123 µs | 27.9 µs |
| scan 100,000 rows, fetch 256 | 43.4 ms | 18.2 ms |
| scan 100,000 rows, fetch 4096 | 13.6 ms | 18.6 ms |
| 1 MiB blob round trip | 2.35 ms | 1.96 ms |
| point select, 8 parallel | 99 µs | 11.3 µs |

Most of the gap is not the design of the generated path. The hand-written server is built on `hqq_adv_serve`, and a call from a Go client through that C path takes 31 to 35 µs on this machine regardless of the wait policy (measured with an echo handler: `hqq_adv_serve` hands every request to a worker thread), against about 0.6 µs for the Go `Serve`. A statement needs several such calls. A fix of `hqq_adv_serve` would narrow the table; the generated server does not use it. The fetch-4096 scan is the one workload where the generated driver is slower (1.37×).

The generated driver passes the whole `database/sql` contract suite of `sqlite/internal/contract` (107 cases in 11 groups). One case needs a flag, `Capabilities.NoInterrupt`: a deadline cannot interrupt a running statement, because the generator has no call cancellation. The hand-written driver and server remain the supported SQLite service; nothing was replaced.

## Tests

| Command | Covers |
| --- | --- |
| `go test ./bindgen/... ./cmd/hqq-bindgen` | parser (rules, digests, plan), emitters (goldens, the examples of the design document, generated code compiled and run against the real runtimes), Go runtime (vectors, flow control, fuzz targets), end-to-end suite against the real C server |
| `ctest` in a build of `c/` | C runtime: every vector of the design document, handle table, frame fuzzing, live sessions, peer death, `hqqrpc_main`, under ASan, UBSan and TSan in CI |
| `go test ./sqlite/...` | the SQLite drivers, the shared `convert` and `dsn` packages, the contract suite against the generated server |
| `go test -tags zstdcgo ./bindgen/examples/zstd/...` | the zstd example (needs `eval "$(bindgen/examples/zstd/build.sh)"`) |

CI is `.github/workflows/bindgen.yml`.
