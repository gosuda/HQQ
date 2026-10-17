# bindgen/bench: the generated client against cgo

Benchmarks of `spec/toolchain/BINDGEN.md` 10.5. The same functions of the MIT test library `bindgen/testdata/microlib/microlib.c` are called two ways:

- **cgo**: `cgobench/cgobench.go` includes `microlib.c` in its cgo preamble (`-O2`) and calls it in process. cgo cannot live in a `_test.go` file, so the wrappers are a regular package with the `cgo` build tag; without cgo the package is empty and `CGO_ENABLED=0 go build ./...` and `go vet ./...` still work.
- **hqq**: the client generated from `bindgen/testdata/microlib/microbench.idl` (`microbench/`) talks to the generated C server (`server/`, built with CMake, Release flags) over HQQ shared memory. The server runs as a separate process, started once per benchmark binary with `launch.Start` (the binary is verified against its SHA-256) and stopped when the binary exits.

| Path | Content |
| --- | --- |
| `microbench/` | generated Go client (`microbench_client.go`) and the `go:generate` line |
| `server/` | generated `microbench_server.{h,c}`; hand-written `microbench_hooks.c` (the `ml_counter_add` interceptor of 9.6), `main.c`, `CMakeLists.txt` |
| `cgobench/` | the cgo baseline |
| `bench_test.go` | the benchmarks |
| `smoke_test.go`, `allocs_test.go` | tests of every benchmarked operation through both paths (below) |
| `harness_test.go` | environment, server and session startup, `TestMain` |
| `verify_test.go` | the oracles: `ml_work`, `ml_xor`, the operands of `Add` and the byte checks, written in Go |
| `benchtable/` | `go run ./bindgen/bench/benchtable`: turns benchmark output into a table |

Regenerate and check the generated files:

```sh
go generate ./bindgen/bench/microbench
go run ./cmd/hqq-bindgen -check -go-out bindgen/bench/microbench -go-strip ml_ -c-out bindgen/bench/server bindgen/testdata/microlib/microbench.idl
```

## Build

```sh
cmake -S bindgen/bench/server -B build/bench -G Ninja -DCMAKE_BUILD_TYPE=Release
cmake --build build/bench                      # build/bench/microbench_server
cmake -S c -B build/c -G Ninja -DCMAKE_BUILD_TYPE=Release -DHQQ_BUILD_CXX_CHECK=OFF
cmake --build build/c --target hqq_echo        # build/c/tools/hqq_echo, for BenchmarkRoundTrip/echo
```

`bindgen/bench/server/CMakeLists.txt` builds libhqq from `c/` itself when no `hqq` target exists (without its tests), so it needs no option in `c/CMakeLists.txt`. The server is compiled with the CMake Release flags (`-O3`); cgo uses `-O2`, as 10.5 says. `ml_add` and `ml_work` are small enough that this does not matter, but it is the one flag difference between the two sides.

## Environment

| Variable | Meaning |
| --- | --- |
| `HQQ_BENCH_SERVER` | path of `microbench_server`. Unset: every test and benchmark that needs the server skips with a message; the cgo-only tests and the cgo leaves still run. |
| `HQQ_BENCH_WAIT` | `spin` (default) or `adaptive`. It sets `Link.WaitPolicy` of the client and the `--spin` flag of the server together. 10.5 pins the pipelining numbers to `spin`; an `adaptive` run is a separate, labeled run. |
| `HQQ_BENCH_ECHO` | path of `hqq_echo` (`c/tools/hqq_echo.c`). Unset: `BenchmarkRoundTrip/echo` and `TestEchoRoundTrip` skip. The harness starts it with `--create NAME --server --count 64 --size 16384 --wait <mode>` and opens the link as `rt` does (`Exclusive`, same wait policy). |
| `HQQ_BENCH_SERVER_CPU` | Linux only: pin the server to this CPU with `taskset -a -p -c`. macOS has no affinity API, so on Apple silicon the "server on another core" of 10.5 is up to the scheduler; setting the variable there is an error. Pin the client by running the whole command under `taskset -c N`. |

`ML_TRACE` is removed from the environment of the benchmark binary and of the server: with it set the library appends a line to a file on every call.

Each binary prints `key: value` lines before the results, which benchfmt (and so benchstat) takes as file-level configuration: `hqq-wait: spin|adaptive`, `hqq-server-sha256: ...` and, if set, `hqq-server-cpu: ...`. A spin file and an adaptive file therefore carry their label.

## Commands for the measurement run

On a quiet machine, from the repository root, after the build above:

```sh
export HQQ_BENCH_SERVER=$PWD/build/bench/microbench_server
export HQQ_BENCH_ECHO=$PWD/build/c/tools/hqq_echo

# 10.5: the pinned-spin run
go test -run '^$' -bench . -benchtime 2s -count 5 ./bindgen/bench | tee spin.txt
# the separate, labeled run with the adaptive wait policy
HQQ_BENCH_WAIT=adaptive go test -run '^$' -bench . -benchtime 2s -count 5 ./bindgen/bench | tee adaptive.txt

# Linux, server and client on different cores
HQQ_BENCH_SERVER_CPU=3 taskset -c 2 go test -run '^$' -bench . -benchtime 2s -count 5 ./bindgen/bench | tee spin.txt
```

`-bench .` is 102 leaves; at `-benchtime 2s -count 5` that is about 20 minutes. A benchmark can be run alone, for example `-bench 'Work/'` or `-bench 'Xor/64KiB/'`.

Smoke runs, which prove that every loop works and verifies, and say nothing about speed:

```sh
go test -run '^$' -bench . -benchtime 1x ./bindgen/bench
go test -count=1 ./bindgen/bench          # the tests below; add -race for the race detector
```

## Output

`go test -bench` prints one line per leaf with `ns/op` and the custom metric `calls/s`; `B/op`, `allocs/op` (every leaf calls `ReportAllocs`) and `MB/s` (Xor) are standard. The files are benchstat input as they are (`benchstat spin.txt adaptive.txt`).

For the layout of 10.5, rows by size or loop count and columns by implementation, use the table printer. It shows the median over `-count` runs of one metric:

```sh
go run ./bindgen/bench/benchtable spin.txt                       # ns/op of everything
go run ./bindgen/bench/benchtable -match '^Work/' spin.txt       # the crossover table
go run ./bindgen/bench/benchtable -metric calls/s -match '^Add/' spin.txt
go test -run '^$' -bench Xor -benchtime 2s ./bindgen/bench | go run ./bindgen/bench/benchtable -match '^Xor/'
```

## What each benchmark measures

One iteration is one call. Every loop compares every result with an oracle written in Go (`verify_test.go`), in the timed region and for both implementations, so a wrong fast path fails instead of looking fast. A pipelined leaf waits for the calls still in flight after the loop with the timer running, so its elapsed time covers all calls.

- **`BenchmarkAdd/{cgo,hqq-sync,hqq-pipe-8,hqq-pipe-32,hqq-pipe-64}`**: a trivial call. `hqq-pipe-N` keeps N calls in flight. Operands differ in every call and the sum wraps, so a stale or misrouted response is a wrong value.
- **`BenchmarkWork/iters=I/{cgo,hqq-sync,hqq-pipe-1,hqq-pipe-4,hqq-pipe-16,hqq-pipe-64}`**: `ml_work(I)`, a serial dependency chain of known cost, for I in 0, 8, 16, 32, 64, 128, 256, 512, 1024 and 2048. This is the crossover sweep: the row where a `hqq-pipe` cell drops below the `cgo` cell is the break-even. The leaves are named by `I`, not by nanoseconds, so names stay the same between machines and benchstat can match them. Read `w` for a row as the `cgo` cell of that row minus the `cgo` cell of `iters=0`. 10.5 lists `w` of 50 to 1000 ns; pick the rows nearest to those values on the measured machine. All calls of a leaf have the same `I`, so the oracle value is constant; pipeline order is verified by `Add` and `CounterAdd`, and `smoke_test.go` runs `Work` with distinct `I` pipelined.
- **`BenchmarkCounterAdd/{cgo,hqq-sync,hqq-pipe-8,hqq-pipe-64}`**: handle, out parameter and interceptor. The interceptor (refuse negative deltas, clamp above 1,000,000) is `server/microbench_hooks.c`; `cgobench.Counter.Add` applies the same rule in Go so both sides compute the same totals. The running total of the library is the oracle, so a lost, repeated or reordered call is detected.
- **`BenchmarkXor/{64B,1KiB,16KiB,64KiB}/...`**: `ml_xor` with a source and a destination of that size. The key changes with every call, so a destination that keeps an earlier result is wrong. Each call checks the length and eight bytes spread over the result; the first, the last and every 1024th call compare every byte. cgo passes the Go slices to C in place. The hqq columns:
  - `hqq-sync`, `hqq-pipe-8`: the default session, `S = 16384`, server without `--copy-request`. 64 B and 1 KiB are one frame; 16 KiB and 64 KiB are chunked (4.8).
  - `hqq-sync-bigbuf`: a session with `S = 256 KiB` and `B = 8`, so every size is one frame in place (the zero-copy path against the chunked one).
  - `...-copyreq`: the same with a second server started with `--copy-request` (7.2), which copies the request body before the library call. This is the data for the open item of 12.2 ("`copy_request` default"): compare `hqq-sync` with `hqq-sync-copyreq` and `hqq-sync-bigbuf` with `hqq-sync-bigbuf-copyreq`. The client copies the request into the ring and the response out of it in every column; `copy_request` only adds the server-side copy.
- **`BenchmarkRoundTrip/{echo,hqq-sync}`**: a 64-byte round trip to `hqq_echo` through the Standard link (`ReserveWrite`, `Commit`, `ReserveRead`, `Release`, the reply compared with the message), next to the generated synchronous `Add`. The difference is the per-call path of the generated code and `rt`, without the link.
- **`BenchmarkWaitCosts/{in-order,out-of-order,callTimeout}`**: batches of 8 `Add` calls issued together and then waited for. `in-order` is the baseline; `out-of-order` waits in reverse, so the first `Wait` buffers the responses of the earlier calls (the copy of 8.3); `callTimeout` is the baseline on a session with `CallTimeout` set, which adds a `SetReadDeadline` to every `Wait` (6.3).

## Tests

`go test ./bindgen/bench` needs `HQQ_BENCH_SERVER`; without it the tests that use the server skip.

| Test | Bug it catches |
| --- | --- |
| `TestRefWorkMatchesCgo`, `TestCgoXorMatchesReference` | an oracle that disagrees with the library, which would make a benchmark fail for the wrong reason (no server needed) |
| `TestCheckXorRejectsWrongResults` | a result check that accepts a stale buffer, a short result or a wrong first or last byte |
| `TestAddAgreesOnBothPaths`, `TestWorkAgreesOnBothPaths` | a client that encodes or decodes a scalar differently from cgo, including the wrapping sum |
| `TestCounterAddAgreesOnBothPaths` | handle, out parameter, interceptor denial (`StatusError` 5), clamp, and the overflow failure (`LibError` -1 with its detail) differing between the paths; the counter must be unchanged after a refused call |
| `TestXorAgreesOnBothPaths` | in-place, chunked, `copy_request` and big-buffer sessions disagreeing with each other or with the reference, for sizes 0 to 64 KiB and a destination one byte short |
| `TestPipelinedCallsKeepTheirResults` | responses attached to the wrong call at depths 1, 8 and 64, out-of-order waits, running-total order, pipelined chunked `Xor` |
| `TestCallTimeoutSessionAgrees` | the `callTimeout` session answering differently |
| `TestEchoRoundTrip` | the echo harness failing to start or to compare (needs `HQQ_BENCH_ECHO`) |
| `TestHotPathAllocs` | an allocation on the in-order hot path (`Add`, `Work`, `CounterAdd`, 64 B `Xor`, 8 pipelined `Add`) with `CallTimeout == 0`; excluded from `-race` builds, where the detector allocates |

cgo and `-race` work together on macOS arm64 with the default toolchain; the race run covers the tests (a benchmark under `-race` measures the detector, not the code).

Fault injection, run once by hand: a server built from a copy of `microlib.c` with `ml_add` off by one at `a == 5000`, with `ml_xor` not writing its last byte, and with `ml_work` dropping an iteration made `BenchmarkAdd/hqq-sync`, `hqq-pipe-64` and `WaitCosts/out-of-order`, `BenchmarkXor/64B/hqq-sync` and `1KiB/hqq-pipe-8`, and `BenchmarkWork/iters=64/hqq-sync` fail with the first wrong value.
