# `bindgen/bench`: Generated IPC Client vs In-Process `cgo`

This package implements the benchmark suite specified in [`spec/toolchain/BINDGEN.md`](../../spec/toolchain/BINDGEN.md) §10.5. It calls the exact same C functions from [`bindgen/testdata/microlib/microlib.c`](../testdata/microlib/microlib.c) across two execution paths:

- **In-process `cgo` (`cgobench/`)**: includes `microlib.c` directly in its cgo preamble (`-O2`) and calls the C functions in-process. Because Go prohibits `import "C"` inside `_test.go` files, the cgo wrappers live in a subpackage guarded by the `cgo` build tag so `CGO_ENABLED=0 go build ./...` and `go vet ./...` continue to succeed without a C toolchain.
- **Out-of-process HQQ (`microbench/` + `server/`)**: uses the pure-Go client generated from [`bindgen/testdata/microlib/microbench.idl`](../testdata/microlib/microbench.idl) to call the C server (`server/`, compiled with CMake `Release` flags) over HQQ shared memory. `TestMain` starts the C server once per benchmark run via `launch.Start` (verifying its SHA-256 digest and attaching a parent lifeline) and stops it when the binary exits.

| Path | Role |
| :--- | :--- |
| `microbench/` | Generated pure-Go client (`microbench_client.go`) and `go:generate` directive. |
| `server/` | Generated C dispatch glue (`microbench_server.{h,c}`), hand-written `@intercept` hook (`microbench_hooks.c`, implementing §9.6 `ml_counter_add` policy), `main.c`, and `CMakeLists.txt`. |
| `cgobench/` | In-process `cgo` baseline wrappers. |
| `bench_test.go` | Comparative benchmark definitions (`BenchmarkAdd`, `BenchmarkWork`, `BenchmarkCounterAdd`, `BenchmarkXor`, `BenchmarkRoundTrip`, `BenchmarkWaitCosts`). |
| `smoke_test.go`, `allocs_test.go` | Differential correctness and zero-allocation tests across both paths. |
| `harness_test.go` | Environment parsing, server lifecycle, and `TestMain`. |
| `verify_test.go` | Pure-Go reference oracles for `ml_work`, `ml_xor`, and wrapping `Add` sums checked inside every benchmark loop. |
| `benchtable/` | `go run ./bindgen/bench/benchtable`: formats `go test -bench` output into crossover tables. |

Regenerate and verify the generated bindings:

```sh
go generate ./bindgen/bench/microbench
go run ./cmd/hqq-bindgen -check -go-out bindgen/bench/microbench -go-strip ml_ -c-out bindgen/bench/server bindgen/testdata/microlib/microbench.idl
```

## Build

Compile the benchmark C server and the `hqq_echo` baseline helper with CMake and Ninja:

```sh
cmake -S bindgen/bench/server -B build/bench -G Ninja -DCMAKE_BUILD_TYPE=Release
cmake --build build/bench                      # produces build/bench/microbench_server
cmake -S c -B build/c -G Ninja -DCMAKE_BUILD_TYPE=Release -DHQQ_BUILD_CXX_CHECK=OFF
cmake --build build/c --target hqq_echo        # produces build/c/tools/hqq_echo for BenchmarkRoundTrip/echo
```

`bindgen/bench/server/CMakeLists.txt` compiles `libhqq` directly from `c/` when built standalone. The C server builds with CMake `Release` optimization (`-O3`), while `cgobench` uses `-O2` per §10.5.

## Environment variables

Configure the benchmark suite through the following environment variables:

| Variable | Effect |
| :--- | :--- |
| `HQQ_BENCH_SERVER` | Path to the compiled `microbench_server` binary. When unset, server-dependent tests and benchmark leaves skip with a diagnostic while `cgo`-only tests and leaves still run. |
| `HQQ_BENCH_WAIT` | `spin` (default) or `adaptive`. Configures both the client's `Link.WaitPolicy` and the server's `--spin` flag. Section 10.5 pins pipelining crossover measurements to `spin`; run `adaptive` as a separate labeled pass. |
| `HQQ_BENCH_ECHO` | Path to `hqq_echo` (`c/tools/hqq_echo.c`). When unset, `BenchmarkRoundTrip/echo` and `TestEchoRoundTrip` skip. The test runner launches it with `--create NAME --server --count 64 --size 16384 --wait <mode>`. |
| `HQQ_BENCH_SERVER_CPU` | Linux only: pins the server process to the specified CPU core via `taskset -a -p -c`. Because macOS provides no CPU affinity API, setting this variable on Darwin fails fast; pin the client on Linux by wrapping `go test` in `taskset -c N`. |

The test runner strips `ML_TRACE` from the environment of both the test process and the child server so file-tracing hooks inside `microlib.c` cannot perturb measurements. Before running benchmarks, `TestMain` emits `benchfmt` configuration headers (`hqq-wait: spin|adaptive`, `hqq-server-sha256: ...`, and optional `hqq-server-cpu: ...`) so `benchstat` preserves run metadata.

## Running the measurements

On an idle machine from the repository root, after building both C binaries:

```sh
export HQQ_BENCH_SERVER=$PWD/build/bench/microbench_server
export HQQ_BENCH_ECHO=$PWD/build/c/tools/hqq_echo

# Section 10.5 pinned-spin run
go test -run '^$' -bench . -benchtime 2s -count 5 ./bindgen/bench | tee spin.txt

# Separate labeled run with the adaptive wait policy
HQQ_BENCH_WAIT=adaptive go test -run '^$' -bench . -benchtime 2s -count 5 ./bindgen/bench | tee adaptive.txt

# Linux: pin server and client to distinct physical cores
HQQ_BENCH_SERVER_CPU=3 taskset -c 2 go test -run '^$' -bench . -benchtime 2s -count 5 ./bindgen/bench | tee spin.txt
```

Running all 102 benchmark leaves at `-benchtime 2s -count 5` takes roughly 20 minutes. Filter individual groups with `-bench 'Work/'` or `-bench 'Xor/64KiB/'`.

To run a fast functional verification of every benchmark loop and test oracle without waiting for full timing:

```sh
go test -run '^$' -bench . -benchtime 1x ./bindgen/bench
go test -count=1 ./bindgen/bench          # add -race to run under the race detector
```

## Formatting the output

Each benchmark leaf reports `ns/op`, the custom `calls/s` metric, `B/op`, `allocs/op`, and (for `BenchmarkXor`) `MB/s`. Feed the raw output directly to `benchstat` (`benchstat spin.txt adaptive.txt`) or format it into a Markdown/ASCII crossover table with `benchtable` (which computes the median across `-count` runs):

```sh
go run ./bindgen/bench/benchtable spin.txt                       # median ns/op across all leaves
go run ./bindgen/bench/benchtable -match '^Work/' spin.txt       # ml_work crossover table
go run ./bindgen/bench/benchtable -metric calls/s -match '^Add/' spin.txt
go test -run '^$' -bench Xor -benchtime 2s ./bindgen/bench | go run ./bindgen/bench/benchtable -match '^Xor/'
```

## What each benchmark measures

Every benchmark iteration executes one call and validates the returned value against the Go reference oracle (`verify_test.go`) inside the timed loop on both `cgo` and `hqq`, ensuring a broken fast path fails immediately. Pipelined leaves drain all in-flight futures before stopping the benchmark timer.

- **`BenchmarkAdd/{cgo,hqq-sync,hqq-pipe-8,hqq-pipe-32,hqq-pipe-64}`**: scalar `ml_add(a, b)` with zero library work. `hqq-pipe-N` maintains `N` concurrent futures in flight. Operands vary on every iteration with wrapping arithmetic so stale or misrouted responses fail the oracle check.
- **`BenchmarkWork/iters=I/{cgo,hqq-sync,hqq-pipe-1,hqq-pipe-4,hqq-pipe-16,hqq-pipe-64}`**: `ml_work(I)`, executing a serial dependency chain of $I \in \{0, 8, 16, 32, 64, 128, 256, 512, 1024, 2048\}$ steps. This sweep identifies the crossover point where pipelined IPC (`hqq-pipe-N`) matches or beats sequential in-process `cgo`. Leaf names are keyed by iteration count $I$ rather than nanoseconds so `benchstat` can compare runs across machines; pure work time $w$ for row $I$ equals `cgo(iters=I) - cgo(iters=0)`.
- **`BenchmarkCounterAdd/{cgo,hqq-sync,hqq-pipe-8,hqq-pipe-64}`**: exercises a session handle, an output parameter, and a server-side `@intercept` hook (`server/microbench_hooks.c` rejects negative deltas and clamps deltas above `1,000,000`; `cgobench.Counter.Add` applies the identical rule in Go). Validates the running total on every call to catch dropped, duplicated, or reordered executions.
- **`BenchmarkXor/{64B,1KiB,16KiB,64KiB}/...`**: runs `ml_xor` from a source slice into an equal-sized destination slice with a per-call varying key, verifying length and eight sampled byte offsets on every call plus a full byte-by-byte check on the first, last, and every 1,024th call:
  - `hqq-sync`, `hqq-pipe-8`: default geometry (`S = 16384`, no `--copy-request`). `64 B` and `1 KiB` fit in a single frame; `16 KiB` and `64 KiB` use multi-frame `CONT` chunking (§4.8).
  - `hqq-sync-bigbuf`: geometry with `S = 256 KiB` and `B = 8`, allowing even `64 KiB` payloads to travel in a single in-place frame.
  - `...-copyreq`: pairs against a second server started with `--copy-request` (§7.2), measuring the exact cost of copying single-frame request buffers into private server memory prior to calling C (§12.2).
- **`BenchmarkRoundTrip/{echo,hqq-sync}`**: compares a raw 64-byte `StandardLink` round trip against `hqq_echo` (`ReserveWrite` / `Commit` / `ReserveRead` / `Release`) next to a synchronous generated `Add` call, isolating the framing and runtime overhead of `hqq-bindgen` above the raw ring.
- **`BenchmarkWaitCosts/{in-order,out-of-order,callTimeout}`**: issues batches of 8 `AddAsync` calls and awaits them in FIFO order (`in-order`), reverse order (`out-of-order`, triggering out-of-order response buffering per §8.3), or on a session configured with `CallTimeout` (adding `SetReadDeadline` per wait per §6.3).

## Verification tests

Running `go test ./bindgen/bench` verifies the benchmark oracles, wire equivalence, and zero-allocation invariants:

| Test | What it verifies |
| :--- | :--- |
| `TestRefWorkMatchesCgo`, `TestCgoXorMatchesReference` | Confirms the pure-Go oracle functions match `microlib.c` bit-for-bit (runs even without `HQQ_BENCH_SERVER`). |
| `TestCheckXorRejectsWrongResults` | Confirms the sampled XOR verifier catches stale buffers, short lengths, and corrupted boundary bytes. |
| `TestAddAgreesOnBothPaths`, `TestWorkAgreesOnBothPaths` | Verifies scalar encoding/decoding and wrapping arithmetic between `cgo` and `hqq-bindgen`. |
| `TestCounterAddAgreesOnBothPaths` | Verifies handle passing, out-parameters, `@intercept` denial (`StatusError` code `5`), clamping, and library overflow errors (`LibError` `-1`), ensuring denied calls leave counter state untouched. |
| `TestXorAgreesOnBothPaths` | Compares in-place, chunked, `--copy-request`, and big-buffer sessions against `cgo` across payload sizes `0..64 KiB` plus short destination buffers. |
| `TestPipelinedCallsKeepTheirResults` | Verifies future correlation at pipeline depths `1`, `8`, and `64`, reverse-order waiting, stateful counter ordering, and pipelined chunked `Xor`. |
| `TestCallTimeoutSessionAgrees` | Confirms sessions with `CallTimeout` enabled produce identical results. |
| `TestEchoRoundTrip` | Verifies the `hqq_echo` raw-ring benchmark driver (requires `HQQ_BENCH_ECHO`). |
| `TestHotPathAllocs` | Asserts zero heap allocations (`0 allocs/op`) on the steady-state hot path for `Add`, `Work`, `CounterAdd`, `64 B Xor`, and 8-deep pipelined `Add` with `CallTimeout == 0` (skipped under `-race`). |
