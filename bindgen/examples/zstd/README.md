# `bindgen/examples/zstd`: Zstandard 1.5.7 via `hqq-bindgen`

This directory wraps Zstandard 1.5.7 using `hqq-bindgen`: a declarative interface in [`zstd.idl`](zstd.idl), a generated pure-Go client ([`zstd_client.go`](zstd_client.go)), a C server built from the generated dispatch glue and the pinned Zstandard 1.5.7 source ([`server/`](server)), and an end-to-end test and benchmark suite comparing out-of-process shared-memory IPC against in-process `cgo` ([`internal/cgozstd`](internal/cgozstd), guarded by `-tags zstdcgo`). The Go client package never uses `cgo`. See [`spec/toolchain/BINDGEN.md`](../../../spec/toolchain/BINDGEN.md) for the full toolchain specification.

## What this example demonstrates

| `hqq-bindgen` Feature | How `zstd.idl` Uses It |
| :--- | :--- |
| Predicate error convention: `@error(predicate(ZSTD_isError), detail(ZSTD_getErrorName, ret))` | Applied to every `size_t`-returning function; Go callers receive `*rt.LibError` with `ZSTD_getErrorName` text and extract the `ZSTD_ErrorCode` via `zstd.ErrorCodeOf(err)`. |
| In-place caller-sized output buffers (`out dst: bytes`) | `ZSTD_compress`, `ZSTD_decompress`, context variants, and streaming shims write compressed/decompressed bytes directly into the reserved response ring slot. |
| Multi-frame `CONT` transfers (`bytes<=N` and `max_message`) | Supports compressing and decompressing 64 MiB payloads in both directions. |
| Session-scoped handles with automatic teardown | `CCtx` (`ZSTD_createCCtx` / `ZSTD_freeCCtx`) and `DCtx` (`ZSTD_createDCtx` / `ZSTD_freeDCtx`) are tracked per session and freed automatically on disconnect or crash. |
| Pre-call `@intercept` policy hook | `ZSTD_CCtx_setParameter` enforces a parameter allowlist and caps `windowLog <= 27`, rejecting worker-thread creation (`ZSTD_c_nbWorkers`) with `rt.ErrDenied`. |
| Pointer-struct flattening via `shim` functions | `ZSTD_compressStream2_flat` and `ZSTD_decompressStream_flat` replace `ZSTD_inBuffer*` and `ZSTD_outBuffer*` pointer structs with flat byte slices and scalar counts. |
| Asynchronous pipelining (`*Async` futures) | Streaming compression pipelines four chunks in flight concurrently on a single `CCtx`. |
| Default-deny opcode table (`@allow(server)`) | Helper functions `ZSTD_isError` and `ZSTD_getErrorName` are marked `@allow(server)` so they are used by server error formatting without exposing client opcodes. |
| Lock-free multi-session execution (`serialize(none)`) | Because Zstandard functions and separate `CCtx`/`DCtx` handles are re-entrant, concurrent sessions execute in parallel without a server lock. |

## Files

| Path | Description |
| :--- | :--- |
| `zstd.idl` | Zstandard interface definition. |
| `zstd_client.go` | Generated pure-Go client (`package zstd`). |
| `zstd.go` | Hand-written helpers: `DefaultOptions`, `CompressBound`, `ErrorCode`, and `ErrorCodeOf`. |
| `server/zstd_server.{c,h}` | Generated C server glue. |
| `server/zstd_hooks.c`, `server/main.c` | Hand-written parameter interceptor, streaming shims, and `hqqrpc_main` entry point. |
| `server/CMakeLists.txt` | CMake configuration building `zstd_server` and `libzstd` from the verified source tree. |
| `build.sh` | Fetches pinned Zstandard 1.5.7, compiles `zstd_server`, and outputs environment exports for `eval`. |
| `internal/cgozstd` | In-process `cgo` reference wrapper (compiled only with `-tags zstdcgo`). |
| `*_test.go` | Integration, fault-injection, streaming, and cgo-comparison tests and benchmarks. |
| `../../../scripts/release/fetch-zstd.sh` | Downloads and verifies the pinned Zstandard release tarball. |
| `../../../third_party/zstd` | Pinned `VERSION`, `SHA256`, and `LICENSE` files for Zstandard 1.5.7. |

Regenerate bindings with `go generate ./bindgen/examples/zstd/`, or check freshness in CI with:

```sh
go run ./cmd/hqq-bindgen -check -go-out bindgen/examples/zstd -go-strip ZSTD_ -c-out bindgen/examples/zstd/server bindgen/examples/zstd/zstd.idl
```

## Fetch, build, and run

Zstandard's C source tree is not vendor-committed into the repository. Instead, `scripts/release/fetch-zstd.sh` downloads the official release archive specified in `third_party/zstd/VERSION`, verifies its SHA-256 digest against `third_party/zstd/SHA256` before unpacking, verifies that the archive's `LICENSE` file matches `third_party/zstd/LICENSE` byte-for-byte, and extracts it outside the repository (`${TMPDIR:-/tmp}/hqq-zstd-1.5.7`).

Run `build.sh` to fetch the source, build the server with CMake and Ninja, and export the test environment variables:

```sh
eval "$(bindgen/examples/zstd/build.sh)"
go test -race -tags zstdcgo ./bindgen/examples/zstd/...
```

`build.sh` exports `HQQ_ZSTD_SERVER` (used by the test suite via `launch.Start` with SHA-256 verification) along with `CGO_CFLAGS` and `CGO_LDFLAGS` pointing to the static `libzstd.a` built from the exact same source. Passing `-s address+undefined` builds `zstd_server` with ASan and UBSan enabled. Without `-tags zstdcgo`, the package builds and runs against the HQQ server without requiring any C headers on the Go side.

To build and start the server manually:

```sh
zdir=$(scripts/release/fetch-zstd.sh)
cmake -S bindgen/examples/zstd/server -B /tmp/hqq-zstd-build -G Ninja -DHQQ_ZSTD_DIR="$zdir"
cmake --build /tmp/hqq-zstd-build
/tmp/hqq-zstd-build/zstd_server my-zstd        # prints "ready my-zstd"
```

Calling the server from Go:

```go
c, err := zstd.Dial(ctx, "my-zstd", zstd.DefaultOptions())
if err != nil {
	return err
}
defer c.Close()

dst := make([]byte, zstd.CompressBound(uint64(len(src))))
frame, err := c.Compress(dst, src, 3)
```

Note on `ZSTD_getFrameContentSize`: the IDL bounds its input slice to `ZSTD_FRAMEHEADER_MAX` (`18` bytes), so pass `frame[:min(len(frame), int(zstd.ZSTD_FRAMEHEADER_MAX))]`; passing a longer slice fails client-side with `rt.ErrInvalidArg` before touching the ring.

## Geometry and flow-control limits

Because Zstandard workloads routinely compress multi-megabyte buffers, `zstd.DefaultOptions()` and `server/main.c` negotiate a larger ring geometry than `rt.Options{}`'s 16 KiB default:

| Parameter | Default Value | Rationale |
| :--- | ---: | :--- |
| Frame size `S` | `65,536` (`64 KiB`) | Any call whose output capacity is `<= 65,501 B` (65,520 B frame body minus 19 B framing header) fits in a single response frame (`rf = 1`) and writes directly into the response ring slot without server scratch buffering. |
| Frame count `B` | `64` | Matches `max_in_flight = 64` (8 MiB total segment size per session: `2 * 64 * 65536`). |
| `max_message` | `128 MiB` | Accommodates a 64 MiB uncompressed payload plus worst-case `CompressBound` overhead. Requests exceeding the negotiated limit fail locally with `rt.ErrInvalidArg`. |
| `max_buffer_size`, `max_segment_bytes` | `1 MiB`, `256 MiB` | Allows callers to request up to 1 MiB frames (`B = 64` -> `128 MiB` segment). |
| `scratch_keep` | `8 MiB` | Retains per-session multi-frame reassembly scratch buffers up to 8 MiB across calls. |
| `copy_request` | `off` | Reads single-frame requests in place from the ring slot (`--copy-request` enables defensive copying for comparison benchmarks). |
| `serialize` | `none` | Independent sessions execute concurrently across cores. |

**Pipelining depth (`floor(B / rf)`)**: credit flow control (`spec/toolchain/BINDGEN.md` §4.12) admits $\lfloor B / \mathit{rf} \rfloor$ concurrent calls in flight, where $\mathit{rf}$ is the number of response frames required for `CompressBound(n)` at `S = 64 KiB` and `B = 64`:

| Uncompressed Input `n` | Response Frames (`rf`) | Max Concurrent Calls in Flight |
| ---: | ---: | ---: |
| up to `16 KiB` | `1` | `64` |
| `64 KiB` | `2` | `32` |
| `256 KiB` | `5` | `12` |
| `1 MiB` | `17` | `3` |
| `2 MiB` | `33` | `1` |
| `4 MiB` and above | `>= 65` | `1` (synchronous fallback since `rf > B`) |

## Exposed IDL functions

| Function | Category | Behavior |
| :--- | :--- | :--- |
| `ZSTD_compress`, `ZSTD_decompress` | One-shot | Writes into caller-sized `out dst: bytes`; errors mapped via `@error(predicate(ZSTD_isError), detail(...))`. |
| `ZSTD_compressBound` | Scalar | Also available locally in Go as `zstd.CompressBound(n)`. |
| `ZSTD_getFrameContentSize` | Scalar | Returns `ZSTD_CONTENTSIZE_UNKNOWN` (`^uint64(0)`) and `ZSTD_CONTENTSIZE_ERROR` (`^uint64(1)`) as normal return values. |
| `ZSTD_versionNumber`, `ZSTD_versionString`, `ZSTD_minCLevel`, `ZSTD_maxCLevel` | Metadata | Zero-work scalar queries (`ZSTD_versionNumber` is `BenchmarkTrivialCall`). |
| `ZSTD_createCCtx`, `ZSTD_freeCCtx`, `ZSTD_createDCtx`, `ZSTD_freeDCtx` | Handles | `@error(null)` on creation; `free*` consumes and destroys the session handle. |
| `ZSTD_compressCCtx`, `ZSTD_decompressDCtx` | Context one-shot | Reuses context allocations across calls without re-allocating state. |
| `ZSTD_CCtx_setParameter` | `@intercept` | Permits `compressionLevel`, `windowLog` (`<= 27`), `strategy`, `contentSizeFlag`, `checksumFlag`, and `dictIDFlag`; rejects `nbWorkers` and unbounded memory parameters with `rt.ErrDenied`. |
| `ZSTD_CCtx_setPledgedSrcSize`, `ZSTD_CCtx_reset`, `ZSTD_DCtx_reset` | Context state | Resets context state or pledges uncompressed input length. |
| `ZSTD_compressStream2_flat`, `ZSTD_decompressStream_flat` | Streaming `shim` | Flattens `ZSTD_inBuffer` / `ZSTD_outBuffer` for streaming compression and decompression. |
| `ZSTD_isError`, `ZSTD_getErrorName` | `@allow(server)` | Server-only error helpers (no client opcode). |

### Deliberately omitted APIs

| Omitted API | Reason |
| :--- | :--- |
| Dictionary APIs (`ZSTD_*_usingDict`, `ZSTD_CDict`, `ZSTD_DDict`) | Expressible in the IDL via `bytes` and handles, but omitted to keep the example focused on standard frames. |
| Raw `ZSTD_inBuffer` / `ZSTD_outBuffer` structs | Structs containing raw C pointers have no wire representation (rule V38); replaced by `*_flat` shims. |
| Legacy streaming (`ZSTD_initCStream`, `ZSTD_compressStream`) | Superseded by `ZSTD_compressStream2`. |
| `ZSTD_c_nbWorkers`, `jobSize`, `overlapLog` | Server builds single-threaded per session (`ZSTD_MULTITHREAD` off); clients must not spawn internal worker threads inside the server. |
| Unbounded table parameters (`hashLog`, `chainLog`, `windowLog > 27`) | Prevents a client from requesting multi-gigabyte hash tables or emitting frames that standard decoders reject. |

## Error codes pinned by the test suite

When any `size_t` function fails, the client returns `*rt.LibError` where `zstd.ErrorCodeOf(err)` extracts the `ZSTD_ErrorCode` and `Detail` holds `ZSTD_getErrorName(ret)`. The session remains healthy after both library errors and `@intercept` policy refusals:

| Operation | `ZSTD_ErrorCode` | `Detail` String (zstd 1.5.7) |
| :--- | ---: | :--- |
| `Decompress` on non-zstd bytes | `10` | `Unknown frame descriptor` |
| `Decompress` on a truncated frame | `72` | `Src size is incorrect` |
| `Decompress` on a frame with a corrupted payload byte | `22` | `Restored data doesn't match checksum` |
| `Decompress` on a frame with a reserved block type | `20` | `Data corruption detected` |
| `Compress` or `Decompress` into an undersized `dst` slice | `70` | `Destination buffer is too small` |
| `CCtxSetParameter` with an out-of-range value | `42` | `Parameter is out of bound` |
| `CCtxSetParameter` mid-stream when stage forbids changes | `60` | `Operation not authorized at current processing stage` |
| `CompressBound` above `ZSTD_MAX_INPUT_SIZE` | `72` | `Src size is incorrect` |

## Tests

Run the full test suite (requiring `HQQ_ZSTD_SERVER` and `-tags zstdcgo` for differential checks against in-process `cgo`):

| Test | Coverage |
| :--- | :--- |
| `TestRoundTripSizesAndLevels`, `TestRoundTripIncompressible` | Verifies compression and decompression across sizes `0, 1, 63, 64, 4 KiB, 1 MiB, 64 MiB` and levels `1, 3, 19`, confirming bit-for-bit equality against in-process `cgo`. |
| `TestDecompress*`, `TestDestinationTooSmall`, `TestSetParameterPolicyAndLibraryBounds`, `TestCompressBoundRefusesImpossibleSize`, `TestFrameContentSizeResultsAreNotFailures`, `TestStreamCallsRejectBadDirective`, `TestLevelsOutsideTheLibraryRangeAreClamped` | Exercises every error code, message string, parameter policy denial, and session recovery path. |
| `TestErrorTextMatchesInProcessLibrary` | Verifies that server error codes and strings match in-process `cgo` identically. |
| `TestContextCallsMatchOneShot`, `TestParametersApplyToStreamsNotToCompressCCtx`, `TestPledgedSizeIsCheckedByTheLibrary` | Verifies context reuse, parameter scoping, and pledged source size validation. |
| `TestHandleLifecycle`, `TestHandleTableLimit` | Rejects use-after-free, double-free, stale handle generations, nil handles, cross-session handles, and handle table overflow. |
| `TestContextsAreReleasedWhenTheirOwnerGoes` | Confirms server RSS returns to baseline when a client closes or crashes while holding `CCtx` handles. |
| `TestStreamRoundTripWithIrregularChunks`, `TestStreamFlushMakesOutputDecodable`, `TestStreamDecompressSmallOutputBuffer`, `TestStreamFramesMatchInProcessLibrary` | Verifies streaming compression/decompression across arbitrary chunk boundaries and small output buffers. |
| `TestStreamLongSyntheticStreamRunsInConstantMemory` | Streams 4 GiB (`64 MiB` under `-short`) through a compress session and a decompress session, verifying end-to-end SHA-256 equality and flat server/client memory usage. |
| `TestPoolGoroutinesShareSessionsWithoutInterference`, `TestPipelinedClientsWithOwnContexts`, `TestWaitInAnyOrder` | Exercises `rt.Pool` concurrency, pipelined futures, and out-of-order future waits. |
| `TestServerKilledWhileIdle`, `TestServerKilledWithCallsInFlight`, `TestServerStopClosesSessionsGracefully` | Verifies `SIGKILL` detection and graceful server shutdown. |
| `TestCopyRequestServerProducesTheSameFrames`, `TestSmallFramesAndFewBuffers`, `TestServerMessageLimitIsGrantedAndEnforced` | Compares `--copy-request` against in-place reads and tests constrained ring geometries (`S = 4 KiB, B = 8`). |
| `TestPin*`, `TestFetch*` | Verifies `fetch-zstd.sh` digest checking, license verification, and path-traversal rejection. |

## Benchmarks

Run the cgo vs HQQ comparison benchmarks on a quiet machine:

```sh
eval "$(bindgen/examples/zstd/build.sh)"
go test -tags zstdcgo -run '^$' -bench . -benchtime 1s ./bindgen/examples/zstd/
```

`BenchmarkCompress` and `BenchmarkDecompress` sweep payload sizes from `64 B` to `64 MiB` at compression level 1 (`SetBytes` reports uncompressed bytes so `MB/s` is directly comparable across modes). `BenchmarkTrivialCall` measures `ZSTD_versionNumber()`. Setting `HQQ_ZSTD_BENCH_SPIN=1` enables the spin wait policy on both client and server (used for the crossover tables in [`bindgen/README.md`](../../README.md#measured-status)).

| Benchmark Mode | Description |
| :--- | :--- |
| `server_sync` | Sequential synchronous calls over one HQQ session. |
| `server_pipelined` | Up to 8 concurrent calls in flight on one HQQ session (up to `1 MiB`). |
| `server_sync_2sessions` | Two goroutines driving two independent HQQ sessions concurrently. |
| `server_sync_copyreq`, `server_pipelined_copyreq` | Runs against a server started with `--copy-request` (up to `256 KiB`). |
| `cgo_sync`, `cgo_2threads` | In-process `cgo` baseline on one goroutine and across two goroutines. |

## License

Zstandard is dual-licensed under BSD-3-Clause and GPL-2.0-only; this example uses Zstandard under the **BSD-3-Clause** license (`third_party/zstd/LICENSE`). The IDL, generated bindings, and Go/C glue in this directory are covered by the repository's [LICENSE](../../../LICENSE).

```text
BSD License

For Zstandard software

Copyright (c) Meta Platforms, Inc. and affiliates. All rights reserved.

Redistribution and use in source and binary forms, with or without modification,
are permitted provided that the following conditions are met:

 * Redistributions of source code must retain the above copyright notice, this
   list of conditions and the following disclaimer.

 * Redistributions in binary form must reproduce the above copyright notice,
   this list of conditions and the following disclaimer in the documentation
   and/or other materials provided with the distribution.

 * Neither the name Facebook, nor Meta, nor the names of its contributors may
   be used to endorse or promote products derived from this software without
   specific prior written permission.

THIS SOFTWARE IS PROVIDED BY THE COPYRIGHT HOLDERS AND CONTRIBUTORS "AS IS" AND
ANY EXPRESS OR IMPLIED WARRANTIES, INCLUDING, BUT NOT LIMITED TO, THE IMPLIED
WARRANTIES OF MERCHANTABILITY AND FITNESS FOR A PARTICULAR PURPOSE ARE
DISCLAIMED. IN NO EVENT SHALL THE COPYRIGHT HOLDER OR CONTRIBUTORS BE LIABLE FOR
ANY DIRECT, INDIRECT, INCIDENTAL, SPECIAL, EXEMPLARY, OR CONSEQUENTIAL DAMAGES
(INCLUDING, BUT NOT LIMITED TO, PROCUREMENT OF SUBSTITUTE GOODS OR SERVICES;
LOSS OF USE, DATA, OR PROFITS; OR BUSINESS INTERRUPTION) HOWEVER CAUSED AND ON
ANY THEORY OF LIABILITY, WHETHER IN CONTRACT, STRICT LIABILITY, OR TORT
(INCLUDING NEGLIGENCE OR OTHERWISE) ARISING IN ANY WAY OUT OF THE USE OF THIS
SOFTWARE, EVEN IF ADVISED OF THE POSSIBILITY OF SUCH DAMAGE.
```
