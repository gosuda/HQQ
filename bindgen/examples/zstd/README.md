# zstd through hqq-bindgen

This directory is the first real library behind hqq-bindgen: a subset of Zstandard 1.5.7 described by [zstd.idl](zstd.idl), a generated Go client ([zstd_client.go](zstd_client.go)), a C server built from the generated glue and the pinned zstd source ([server/](server)), and tests and benchmarks that drive the server over HQQ shared memory and compare it with the same library called through cgo. The Go client never uses cgo; only the comparison package [internal/cgozstd](internal/cgozstd) does, behind a build tag. The design is in [spec/toolchain/BINDGEN.md](../../../spec/toolchain/BINDGEN.md).

## What it demonstrates

| Feature of the design | Where |
| :--- | :--- |
| Error convention in the result: `@error(predicate(ZSTD_isError), detail(ZSTD_getErrorName, ret))` | every function that returns `size_t`; Go sees a `*rt.LibError` with the library's text, `ErrorCodeOf` gives the `ZSTD_ErrorCode` |
| Caller-supplied output buffers with a capacity, written in place into the response slot | `ZSTD_compress`, `ZSTD_decompress`, the context variants and both stream shims |
| Large messages as CONT frames, bounded by the IDL (`bytes<=N`) and by `max_message` | 64 MiB buffers in both directions |
| Handles with a destroy function, per-session tables, teardown on session end | `CCtx` and `DCtx` |
| Interceptor | `ZSTD_CCtx_setParameter` (parameter allowlist and a window bound) |
| `shim` functions that flatten structs with pointers | `ZSTD_compressStream2_flat`, `ZSTD_decompressStream_flat` |
| Pipelining | `*Async` calls and futures; the stream tests keep four compress calls in flight |
| Default deny | only the functions of the IDL have an opcode; `ZSTD_isError` and `ZSTD_getErrorName` are `@allow(server)` and cannot be called by a client |
| `serialize(none)` | zstd is re-entrant and contexts are independent, so sessions run inside the library at the same time |

## Files

| Path | Content |
| :--- | :--- |
| `zstd.idl` | the interface |
| `zstd_client.go` | generated Go client (package `zstd`) |
| `zstd.go` | hand-written: `DefaultOptions`, `CompressBound`, `ErrorCode`, `ErrorCodeOf` |
| `server/zstd_server.{c,h}` | generated glue |
| `server/zstd_hooks.c`, `server/main.c` | hand-written interceptor, shims, process entry and geometry |
| `server/CMakeLists.txt` | builds `zstd_server` and the zstd library from a source directory |
| `build.sh` | fetches, builds, prints the environment of the tests |
| `internal/cgozstd` | zstd called through cgo (build tag `zstdcgo`) |
| `*_test.go` | tests and benchmarks |
| `../../../scripts/release/fetch-zstd.sh` | fetches and verifies the pinned source |
| `../../../third_party/zstd` | `VERSION`, `SHA256`, `LICENSE` of the pinned release |

Regenerate with `go generate ./bindgen/examples/zstd/`. The check that the committed files are current is `go run ./cmd/hqq-bindgen -check -go-out bindgen/examples/zstd -go-strip ZSTD_ -c-out bindgen/examples/zstd/server bindgen/examples/zstd/zstd.idl`.

## Fetch, build, run

The zstd source is not in the repository. `fetch-zstd.sh` downloads the official release tarball named in `third_party/zstd/VERSION`, refuses it unless its SHA-256 equals `third_party/zstd/SHA256` (nothing is unpacked on a mismatch), checks that the tarball's `LICENSE` equals `third_party/zstd/LICENSE`, and unpacks it outside the repository (default `${TMPDIR:-/tmp}/hqq-zstd-1.5.7`). The release also publishes the same digest as `zstd-1.5.7.tar.gz.sha256`. There is no PowerShell variant: the server is POSIX only (BINDGEN.md 1.2).

`build.sh` runs the fetch, builds with CMake and Ninja, and prints a script for `eval`:

```sh
eval "$(bindgen/examples/zstd/build.sh)"
go test -race -tags zstdcgo ./bindgen/examples/zstd/...
```

It exports `HQQ_ZSTD_SERVER` (the server binary, which the tests start with `launch.Start` after hashing it), `CGO_CFLAGS` and `CGO_LDFLAGS` (the zstd library built from the same source). `-s address+undefined` additionally builds the server with AddressSanitizer and UndefinedBehaviorSanitizer and points `HQQ_ZSTD_SERVER` at it; the cgo library stays unsanitized. Without `HQQ_ZSTD_SERVER` the tests that need a server skip and the fetch-script tests still run. Without `-tags zstdcgo` the comparisons with the in-process library are not compiled, and `go build ./...` needs no zstd headers.

By hand, without the script:

```sh
zdir=$(scripts/release/fetch-zstd.sh)
cmake -S bindgen/examples/zstd/server -B /tmp/hqq-zstd-build -G Ninja -DHQQ_ZSTD_DIR="$zdir"
cmake --build /tmp/hqq-zstd-build
/tmp/hqq-zstd-build/zstd_server my-zstd        # prints "ready my-zstd"
```

The glue and the hooks build with `-Wall -Wextra -Wpedantic -Werror` (`HQQ_WERROR` defaults to ON in this project); zstd's own sources are third-party code and use the default flags. Client side:

```go
c, err := zstd.Dial(ctx, "my-zstd", zstd.DefaultOptions())
if err != nil {
	return err
}
defer c.Close()
dst := make([]byte, zstd.CompressBound(uint64(len(src))))
frame, err := c.Compress(dst, src, 3)
```

`ZSTD_getFrameContentSize` reads at most `ZSTD_FRAMEHEADER_MAX` (18) bytes: pass `frame[:min(len(frame), int(zstd.ZSTD_FRAMEHEADER_MAX))]`; a longer slice is refused with `rt.ErrInvalidArg` before it is sent. A destination slice is the output capacity; the result is its prefix that the library produced.

### Optional: system libzstd

The cgo comparison can link another libzstd, for example Homebrew's, by setting `CGO_CFLAGS=-I/opt/homebrew/include` and `CGO_LDFLAGS=-L/opt/homebrew/lib -lzstd` yourself. That path is unverified: the tests compare compressed bytes, which are only guaranteed equal between identical releases and settings. `TestVersionMatchesPin` fails when the in-process version differs from the server's.

## Geometry and limits

`rt.Options{}` selects 16 KiB frames and 16 MiB messages, which is too small for this library, so the example fixes its own defaults in two places that must agree: `zstd.DefaultOptions()` for the client and `server/main.c` for the server. The values are negotiated at connect time (BINDGEN.md 4.11, 5.5); a client may ask for another geometry up to the server's maximum.

| Setting | Value | Why |
| :--- | ---: | :--- |
| Frame size `S` | 65,536 | A call with a destination of at most 65,501 bytes has a one-frame response bound (the bound is the capacity plus 19 bytes of framing and the frame body holds 65,520 bytes), so the library writes its output directly into the response slot. That covers compressing inputs of up to about 65,200 bytes and decompressing frames to up to 65,501 bytes. |
| Frame count `B` | 64 | `max_in_flight` is 64 as well; see the table below. |
| `max_message` | 128 MiB | A 64 MiB buffer and its compressed form fit; `ZSTD_MAX_SRC` of the IDL (512 MiB) is the contract of the interface, the session limit is the effective bound. A request above the granted limit fails with `rt.ErrInvalidArg` before it is sent. |
| `max_buffer_size`, `max_segment_bytes` | 1 MiB, 256 MiB | 1 MiB frames at `B = 64` need a 128 MiB segment. |
| `scratch_keep` | 8 MiB | Multi-frame messages are assembled in per-session scratch; scratch up to 8 MiB is kept between calls. |
| `copy_request` | off | The library reads request bytes in place from the ring slot. `--copy-request` copies single-frame requests first (BINDGEN.md 7.2); the benchmarks compare the two. |
| `serialize` | none | Sessions run in the library concurrently. |

A session segment at the defaults is 8 MiB (`2 * 64 * 65536`). Every flag of `hqqrpc_main` overrides the defaults (`--buffer-size`, `--buffer-count`, `--max-message`, `--max-handles`, `--copy-request`, `--spin`, and the others of `hqq_rpc.h`).

**Pipelining depth.** The flow-control rule of BINDGEN.md 4.12 admits `floor(B / rf)` calls in flight, where `rf` is the number of response frames of the call's bound. For `ZSTD_compress` with the capacity of `CompressBound(n)` at the defaults:

| Input `n` | `rf` | Calls in flight |
| ---: | ---: | ---: |
| up to 16 KiB | 1 | 64 |
| 64 KiB | 2 | 32 |
| 256 KiB | 5 | 12 |
| 1 MiB | 17 | 3 |
| 2 MiB | 33 | 1 |
| 4 MiB and above | 65 or more | synchronous: `rf` exceeds `B` |

A caller that wants deeper pipelining of large buffers raises `B` (up to 4096) within `max_segment_bytes`, or compresses in chunks as the stream tests do.

## Functions in the IDL

| Function | Kind | Notes |
| :--- | :--- | :--- |
| `ZSTD_compress`, `ZSTD_decompress` | one-shot | `out dst` with capacity; `@error(predicate(ZSTD_isError), detail(...))` |
| `ZSTD_compressBound` | scalar | `CompressBound` in Go computes it locally; the call exists to check the formula |
| `ZSTD_getFrameContentSize` | scalar | returns `ZSTD_CONTENTSIZE_UNKNOWN` and `ZSTD_CONTENTSIZE_ERROR` as results, not failures |
| `ZSTD_versionNumber`, `ZSTD_versionString`, `ZSTD_minCLevel`, `ZSTD_maxCLevel` | scalar | the trivial calls of the benchmarks |
| `ZSTD_createCCtx`, `ZSTD_freeCCtx`, `ZSTD_createDCtx`, `ZSTD_freeDCtx` | handles | `@error(null)` on create; free is the destroy function and consumes the handle |
| `ZSTD_compressCCtx`, `ZSTD_decompressDCtx` | context one-shot | the library ignores advanced parameters in `ZSTD_compressCCtx` |
| `ZSTD_CCtx_setParameter` | `@intercept` | allows compression level, window log (at most 27), strategy, content size, checksum and dictionary id flags; every other parameter, including `ZSTD_c_nbWorkers`, gets `rt.ErrDenied` |
| `ZSTD_CCtx_setPledgedSrcSize`, `ZSTD_CCtx_reset`, `ZSTD_DCtx_reset` | context state | |
| `ZSTD_compressStream2_flat`, `ZSTD_decompressStream_flat` | `shim` | see below |
| `ZSTD_isError`, `ZSTD_getErrorName` | `@allow(server)` | error predicate and detail text; no opcode |

**Streaming.** `ZSTD_compressStream2(cctx, ZSTD_outBuffer *, ZSTD_inBuffer *, endOp)` takes structs that hold pointers, which have no wire form, and composite in-parameters are outside the design (BINDGEN.md V38). The shims in `server/zstd_hooks.c` unpack them: `src` and `srcSize` are the input buffer and the call returns the consumed count; `dst` with its capacity is the output buffer and the call returns the produced prefix; the result is the library's own return value (the bytes still to flush, or the next input size for the decoder). A caller loops exactly as with the C API, resending `src[consumed:]` when the output buffer filled. Calls on one context depend on each other, so one context is one pipeline: when the destination holds `CompressBound(len(src))` zstd consumes the whole input whatever it outputs, which lets the compress calls of consecutive chunks be in flight together (the stream tests do this with a depth of four); decompression depends on the output buffer and runs call by call.

## Not exposed, and why

| Left out | Reason |
| :--- | :--- |
| Dictionaries (`ZSTD_*_usingDict`, `ZSTD_CDict`, `ZSTD_DDict`, `ZDICT_*`) | Expressible (two `bytes` in-parameters, handles for prepared dictionaries) but not part of this subset; the tests are dictionary-free. |
| `ZSTD_inBuffer` and `ZSTD_outBuffer` as arguments | Structs with pointer members and composite in-parameters; replaced by the two shims. |
| `ZSTD_compressStream`, `ZSTD_initCStream` and the other older streaming entry points | Superseded by `ZSTD_compressStream2` and `ZSTD_decompressStream`. |
| `ZSTD_c_nbWorkers`, `jobSize`, `overlapLog` | The server is built without `ZSTD_MULTITHREAD` and a client must not start threads in the server. `ZSTD_c_nbWorkers` is declared in the IDL so that a client can name it, and the interceptor refuses it. |
| `hashLog`, `chainLog`, `searchLog`, `minMatch`, `targetLength`, long-distance matching, `windowLog` above 27 | `hashLog` and `chainLog` accept values up to 30 and would let a client request multi-GiB tables; a window above 27 produces frames that the server's own decoder refuses by default. |
| Advanced decompression parameters (`ZSTD_DCtx_setParameter`) | Not needed for the subset. |
| Frame inspection beyond `ZSTD_getFrameContentSize`, `ZSTD_estimate*Size`, static and advanced-allocator contexts, sequence producers | Out of scope for the example. |
| A level policy | `ZSTD_compress` and `ZSTD_compressCCtx` are not intercepted, because an interceptor makes the server copy every `bytes` in-argument into private memory (BINDGEN.md 4.7.1 step 5) and would remove the in-place read. The library clamps a level above `ZSTD_maxCLevel()` (22) or below `ZSTD_minCLevel()` to the limit instead of failing. Levels 20 to 22 use hundreds of MiB per context, so a deployment that serves untrusted clients limits sessions (`--max-sessions`) or the process's memory. |

## Errors

A failed `size_t` call returns a `*rt.LibError`: `Code` is `int64(ret)`, minus the `ZSTD_ErrorCode`; `Detail` is the text of `ZSTD_getErrorName`. `zstd.ErrorCodeOf(err)` returns the code. The session stays usable after a library failure and after `rt.ErrDenied`. The failures that the tests pin, with the text of release 1.5.7:

| Call | Code | Detail |
| :--- | ---: | :--- |
| `Decompress` of data that is not a frame | 10 | `Unknown frame descriptor` |
| `Decompress` of a truncated frame, whatever the cut | 72 | `Src size is incorrect` |
| `Decompress` of a checksummed frame with a changed byte | 22 | `Restored data doesn't match checksum` |
| `Decompress` of a frame with a reserved block type | 20 | `Data corruption detected` |
| `Compress` or `Decompress` into a buffer that is too small | 70 | `Destination buffer is too small` |
| `CCtxSetParameter` with a value outside the library's bounds | 42 | `Parameter is out of bound` |
| `CCtxSetParameter` of a frame parameter after compression started | 60 | `Operation not authorized at current processing stage` |
| `CompressBound` of a size above `ZSTD_MAX_INPUT_SIZE` | 72 | `Src size is incorrect` |

An empty input to `Decompress` is not an error: the library takes it for no frames and returns zero bytes. A level outside the library's range is clamped, not rejected.

## Tests

The tests need `HQQ_ZSTD_SERVER`; the cgo comparisons also need `-tags zstdcgo`.

| Test | What it catches |
| :--- | :--- |
| `TestRoundTripSizesAndLevels`, `TestRoundTripIncompressible` | Wrong bytes or lengths at 0, 1, 63, 64, 4 KiB, 1 MiB and 64 MiB (CONT frames in both directions), levels 1, 3, 19 (19 up to 1 MiB), and frames that differ from the in-process library |
| `TestDecompress*`, `TestDestinationTooSmall`, `TestSetParameterPolicyAndLibraryBounds`, `TestCompressBoundRefusesImpossibleSize`, `TestFrameContentSizeResultsAreNotFailures`, `TestStreamCallsRejectBadDirective`, `TestLevelsOutsideTheLibraryRangeAreClamped` | Each error path, with its code and text; a session that breaks after a library failure; policy refusals |
| `TestErrorTextMatchesInProcessLibrary` | A server failure whose code or text differs from the library called in process |
| `TestContextCallsMatchOneShot`, `TestParametersApplyToStreamsNotToCompressCCtx`, `TestPledgedSizeIsCheckedByTheLibrary` | State left in a context between calls; parameters that reach or miss the library |
| `TestHandleLifecycle`, `TestHandleTableLimit` | Use after free, double free, a recycled index with a stale generation, a nil handle, a handle of another session, a full handle table |
| `TestContextsAreReleasedWhenTheirOwnerGoes` | Contexts that survive an orderly close or a vanished client (resident size of the server; a live session is the control) |
| `TestStreamRoundTripWithIrregularChunks`, `TestStreamFlushMakesOutputDecodable`, `TestStreamDecompressSmallOutputBuffer`, `TestStreamFramesMatchInProcessLibrary` | Stream calls that lose, duplicate or reorder bytes; chunk boundaries; flush; a decoder that returns before consuming its input |
| `TestStreamLongSyntheticStreamRunsInConstantMemory` | A 4 GiB stream (64 MiB with `-short`, or `HQQ_ZSTD_STREAM_MIB`) through a compress session and a decompress session with the SHA-256 of both ends compared, and a per-chunk leak in the server or the client (resident size and heap after a 64 MiB warm-up) |
| `TestPoolGoroutinesShareSessionsWithoutInterference`, `TestPipelinedClientsWithOwnContexts`, `TestWaitInAnyOrder` | Responses delivered to the wrong caller, frames mixed between sessions, buffered responses of futures waited for out of order |
| `TestServerKilledWhileIdle`, `TestServerKilledWithCallsInFlight`, `TestServerStopClosesSessionsGracefully` | Hangs after a server crash, a call that reports success or a known outcome after one, a session that stays usable |
| `TestCopyRequestServerProducesTheSameFrames`, `TestSmallFramesAndFewBuffers`, `TestServerMessageLimitIsGrantedAndEnforced` | The copy path differing from the in-place path; flow control with 4 KiB frames and 8 buffers; the granted message limit |
| `TestPin*`, `TestFetch*` | The fetch script accepting a corrupted or unpinned archive, a drifted license, member names that escape the directory, or an unpack inside the repository |

## Benchmarks

```sh
eval "$(bindgen/examples/zstd/build.sh)"
go test -tags zstdcgo -run '^$' -bench . -benchtime 1s ./bindgen/examples/zstd/
```

`BenchmarkCompress` and `BenchmarkDecompress` run level 1 on text data of 64 B, 256 B, 1 KiB, 4 KiB, 16 KiB, 64 KiB, 256 KiB, 1 MiB, 4 MiB, 16 MiB and 64 MiB; `SetBytes` is the uncompressed size, so MB/s is comparable between the two and between modes. `BenchmarkTrivialCall` is `ZSTD_versionNumber`, which has no payload and no library work.

| Mode | Meaning |
| :--- | :--- |
| `server_sync` | one call at a time on one session |
| `server_pipelined` | eight calls in flight on one session (up to 1 MiB; above that flow control admits one call, see the table above) |
| `server_sync_2sessions` | two goroutines with one session each |
| `server_sync_copyreq`, `server_pipelined_copyreq` | the first two against a server started with `--copy-request`, up to 256 KiB: in-place reads versus a copy of each request |
| `cgo_sync`, `cgo_2threads` | the library in process on one goroutine and on two |

The crossover payload size is the size at which `server_sync` or `server_pipelined` reaches `cgo_sync`; `cgo_2threads` and `server_sync_2sessions` put both sides on two cores. `HQQ_ZSTD_BENCH_SPIN=1` runs both ends with the spin wait policy, which BINDGEN.md 1.3 uses for pipelining figures. The benchmarks check the length of every result. Run them on a quiet machine: the servers and the client share cores.

## License

Zstandard is dual-licensed under the BSD-3-Clause license and the GPL-2.0-only license, at the user's option. This example uses and distributes it under the BSD-3-Clause license. The source is not committed: it is fetched from the official release and verified against `third_party/zstd/SHA256`. `third_party/zstd/LICENSE` is the BSD text of the pinned release (the script refuses a tarball whose `LICENSE` differs). A binary that contains zstd (`zstd_server`) must reproduce the notice, the conditions and the disclaimer below in its documentation. The files of this directory, including the generated ones, are under the license of the repository ([LICENSE](../../../LICENSE)).

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
