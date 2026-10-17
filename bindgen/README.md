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

Apple M1, macOS, Go 1.27, quiet machine, spin wait policy, median of 3 runs of `-benchtime 1s` (`bindgen/bench`, `sqlite/gendriver`; `-benchtime 300ms` for `bindgen/examples/zstd`). The server runs as a separate process and the scheduler places it; macOS has no affinity API. Other machines will differ; run the benchmarks yourself with the commands in `bindgen/bench/README.md`.

**A call with no work.** `ml_add`, 25 ns through cgo.

| Path | ns/call |
| --- | ---: |
| cgo | 25 |
| generated, synchronous | 335 |
| generated, 8 in flight | 175 |
| generated, 32 in flight | 164 |
| generated, 64 in flight | 178 |

A synchronous call costs about 13 times a cgo call, and a pipelined call about 7 times. The floor of the link itself is one Standard-link round trip of about 193 ns (`BenchmarkCompareRoundTrip`); the generated path adds frame encoding, handle and bound checks, the response reservation and the execution lock on top. The design does not aim at tiny synchronous calls.

**A call with work.** `ml_work(iters)`, a dependency chain of known cost (cgo ns/call in the first column).

| `iters` | cgo | sync | 4 in flight | 64 in flight |
| ---: | ---: | ---: | ---: | ---: |
| 0 | 25 | 331 | 178 | 181 |
| 64 | 83 | 400 | 246 | 243 |
| 256 | 396 | 1,795 | 568 | 565 |
| 1,024 | 1,650 | 3,083 | 1,866 | 1,868 |

With one calling goroutine the generated path does not cross cgo: pipelined, it trails cgo by about 155–220 ns per call at every size measured. On macOS the synchronous column grows faster than the work (about 1,400 ns of overhead at `iters=256`, against about 305 ns at `iters=0`; the run-to-run spread is large, and an earlier run of the same configuration measured 1,338 ns, even though this benchmark uses the spin policy and the C server runs with `--spin`). In a `linux/arm64` container on the same M1 with client and server pinned to separate cores (`HQQ_BENCH_SERVER_CPU=1 taskset -c 0`), the synchronous overhead stays flat across the sweep (`iters=0`: 403 ns against 26 ns, overhead 376 ns; `iters=64`: 501 ns against 98 ns, overhead 403 ns; `iters=256`: 962 ns against 486 ns, overhead 476 ns; `iters=1024`: 2,435 ns against 2,077 ns, overhead 358 ns), indicating that the `iters=256` jump on macOS is a scheduler/core-placement effect rather than a server-path defect.

**Buffers.** `ml_xor` over a source and a destination of the same size; cgo passes the Go slices in place.

| Size | cgo | sync | 8 in flight | cgo GB/s | hqq in flight GB/s |
| ---: | ---: | ---: | ---: | ---: | ---: |
| 64 B | 65 ns | 375 ns | 227 ns | 0.99 | 0.28 |
| 1 KiB | 89 ns | 567 ns | 281 ns | 11.5 | 3.64 |
| 64 KiB | 1,338 ns | 15,788 ns | 9,068 ns | 49 | 7.2 |

Two copies (request into the ring, response out of it) bound the large sizes. `copy_request` (design 7.2) costs 10% for synchronous 64 B calls (414 against 375 ns), 4% for synchronous 1 KiB calls (590 against 567 ns), 2% for pipelined 64 B calls (232 against 227 ns), 0% for pipelined 1 KiB calls (282 against 281 ns), and 1% at 64 KiB (15,996 against 15,788 ns synchronous), so its default stays off. With a buffer size that holds the whole message in one frame (`hqq-sync-bigbuf`), 64 KiB synchronous takes 13,001 ns instead of 15,788 ns.

**zstd 1.5.7, level 1, text.** Same library called through cgo and through the generated client (`server_sync`, `server_pipelined`); `cgo_sync` is the baseline. Ratios are server time over cgo time.

| Payload | compress sync | compress pipelined | decompress sync | decompress pipelined |
| ---: | ---: | ---: | ---: | ---: |
| 64 B | 1.39× | 1.15× | 2.2× | 1.4× |
| 1 KiB | 1.20× | 1.09× | 1.77× | 1.14× |
| 64 KiB | 1.18× | 1.01× | 1.56× | 1.07× |
| 1 MiB | 1.04× | 1.00× | 1.20× | 1.05× |
| 64 MiB | 1.04× | not run | 1.29× | not run |

Raw times behind the ratios above:

| Operation | `cgo_sync` | `server_sync` | `server_pipelined` |
| --- | ---: | ---: | ---: |
| compress 64 B | 911.5 ns | 1,271 ns | 1,044 ns |
| compress 1 KiB | 4,618 ns | 5,559 ns | 5,020 ns |
| compress 64 KiB | 160,754 ns | 189,990 ns | 162,757 ns |
| compress 1 MiB | 2.784 ms | 2.887 ms | 2.778 ms |
| compress 64 MiB | 180.5 ms | 187.8 ms | not run |
| decompress 64 B | 232.5 ns | 517.8 ns | 329.0 ns |
| decompress 1 KiB | 2,024 ns | 3,586 ns | 2,301 ns |
| decompress 64 KiB | 49,823 ns | 77,627 ns | 53,322 ns |
| decompress 1 MiB | 697,856 ns | 835,988 ns | 731,675 ns |
| decompress 64 MiB | 44.59 ms | 57.67 ms | not run |

For coarse calls (compression of 64 KiB to 1 MiB, including 256 KiB at 705,377 ns against 695,542 ns, or 1.4%) the pipelined generated client is within 2% of cgo with one goroutine; two sessions against two cgo threads are within 2% at 1 MiB (1.515 ms against 1.490 ms, 1.02×), 11% at 64 MiB (138.6 ms against 124.3 ms, 1.11×), and 11% at 64 KiB (96.7 µs against 87.1 µs). A trivial call (`ZSTD_versionNumber`) takes 26.7 ns through cgo, 339.8 ns synchronous, and 155.8 ns pipelined. Decompression moves more bytes per unit of work and pays for the response copy. The source of the crossover table is `bindgen/examples/zstd` (`go test -tags zstdcgo -bench 'Compress|Decompress|TrivialCall'`).

**SQLite, hand-written driver against the generated one.** Same workloads, each against its own server (`sqlite/gendriver` benchmarks).

| Workload | `hqq-sqlite` | `hqq-sqlite-gen` |
| --- | ---: | ---: |
| `SELECT 1` | 36.0 µs | 3.5 µs |
| point select, prepared | 39.6 µs | 3.1 µs |
| insert 1,000 rows in one transaction | 19.0 ms | 1.00 ms |
| insert, autocommit | 55.6 µs | 9.8 µs |
| scan 100,000 rows, fetch 256 | 36.6 ms | 19.5 ms |
| scan 100,000 rows, fetch 4096 | 15.2 ms | 20.8 ms |
| 1 MiB blob round trip | 2.28 ms | 2.02 ms |
| point select, 8 parallel | 30.1 µs | 4.0 µs |

Inline request execution in `hqq_adv_serve` (about 35 µs to about 0.6 µs per call) and the wider adaptive spin window explain the shifts from the initial runs. The remaining difference on short statements comes from the transport and server threading models: each `hqq-sqlite` statement goes through an Advanced-link `Call`, stream routing, and a hand-off from the `hqq_adv_serve` handler to the session's SQLite worker thread, whereas `hqq-sqlite-gen` uses a dedicated Standard link per connection whose session thread calls SQLite directly. The fetch-4096 scan is the one workload where the generated driver is slower (1.36×, 20.8 ms against 15.2 ms).

The generated driver passes the whole `database/sql` contract suite of `sqlite/internal/contract` (107 cases in 11 groups). One case needs a flag, `Capabilities.NoInterrupt`: a deadline cannot interrupt a running statement, because the generator has no call cancellation. The hand-written driver and server remain the supported SQLite service; nothing was replaced.

## Tests

| Command | Covers |
| --- | --- |
| `go test ./bindgen/... ./cmd/hqq-bindgen` | parser (rules, digests, plan), emitters (goldens, the examples of the design document, generated code compiled and run against the real runtimes), Go runtime (vectors, flow control, fuzz targets), end-to-end suite against the real C server |
| `ctest` in a build of `c/` | C runtime: every vector of the design document, handle table, frame fuzzing, live sessions, peer death, `hqqrpc_main`, under ASan, UBSan and TSan in CI |
| `go test ./sqlite/...` | the SQLite drivers, the shared `convert` and `dsn` packages, the contract suite against the generated server |
| `go test -tags zstdcgo ./bindgen/examples/zstd/...` | the zstd example (needs `eval "$(bindgen/examples/zstd/build.sh)"`) |

CI is `.github/workflows/bindgen.yml`.
