# Changelog

All notable changes to **HQQ** (`gosuda.org/hqq`) are documented in this file.

The project follows [Semantic Versioning](https://semver.org/). While in `v0.x`, minor releases may introduce breaking wire-layout or API improvements accompanied by explicit migration tables below.

## Unreleased

This cycle promotes the pure-Go `database/sql` SQLite driver to `gosuda.org/hqq/sqlite`, adds `hqq-bindgen` for cgo-free Go-to-C IPC code generation, introduces `secureexec`, `lifeline`, `launch`, and `sqlite/launcher` for verified and parent-tied child servers, and optimizes both the Go and C fast paths.

### Added

- **SQLite package `gosuda.org/hqq/sqlite`**, promoted from `examples/sqlite/driver`: a `database/sql` driver registered as `hqq-sqlite`. Exported symbols: `NewConnector`, `ConnectorFromDSN`, `ParseDSN`, `Connector`, `Config`, `Driver`, `DriverName`, `WithTxMode`, `Pragma`, `Mode` (`ModeReadWriteCreate`, `ModeReadWrite`, `ModeReadOnly`), `TxMode` (`TxDeferred`, `TxImmediate`, `TxExclusive`), `Error` (`Code`, `ExtendedCode`, `Message`, `Primary`), `ProtocolError`, the predicates `IsBusy`, `IsLocked`, `IsConstraint`, `IsReadOnly`, and `IsInterrupted`, the sentinels `ErrBusy`, `ErrLocked`, `ErrConstraint`, `ErrReadOnly`, `ErrInterrupted`, and `ErrOutcomeUnknown`, and the `Code*` constants. Package behavior:
  - A lost link during `Exec` or `ExecStmt` wraps `ErrOutcomeUnknown` rather than `driver.ErrBadConn`, so `database/sql` never automatically re-runs a write whose commit state is unknown. Other lost-link errors wrap `driver.ErrBadConn` so `database/sql` can retry on a fresh connection.
  - `time.Time` parameters are stored as `TEXT` formatted with `2006-01-02 15:04:05.999999999-07:00`, preserving the UTC offset. `TEXT` values read from `DATE`, `DATETIME`, or `TIMESTAMP` columns are returned as `time.Time` when they match a supported timestamp layout, and as `string` otherwise.
  - Parameters are validated by the driver's `CheckNamedValue`: `uint64` values above `math.MaxInt64` and `NaN` floats fail locally before transmission, `[]byte` and `json.RawMessage` are encoded as `BLOB`, and unsupported Go types fail immediately.
  - `BeginTx` accepts default and `LevelSerializable` isolation levels and rejects weaker levels. `ReadOnly` transactions begin in `DEFERRED` mode with `PRAGMA query_only = 1`.
  - Each `Exec` or `Query` executes a single SQL statement; multi-statement strings are rejected with `BAD_REQUEST`.
- **SQLite test and contract suites**: the shared `database/sql` contract suite (`sqlite/internal/contract`, executed against both the Go fake server and the C server), wire codecs (`sqlite/internal/wire`), in-process fake server (`sqlite/internal/fakeserver`), and multi-process end-to-end tests against the C binary (`sqlite/e2e`).
- **C SQLite server parent lifeline**: `--parent-stdin[=force]` (alias `--exit-on-stdin-eof`) shuts the server down cleanly and rolls back active transactions when standard input reaches `EOF`; `--lifeline-grace-ms` bounds shutdown duration before forced exit.
- **Parent lifeline**: Go package `lifeline` (`Attach`, `Lifeline.Close`, `Watch`, `WatchTerminal`, `WithKeepalive`) and C header `c/include/hqq/hqq_lifeline.h` (`hqq_lifeline_options_init`, `hqq_lifeline_start`, `hqq_lifeline_watching`, `hqq_lifeline_stop`). Child processes exit automatically when their parent terminates, including via `SIGKILL`, because the OS kernel closes the parent's pipe write descriptor.
- **Verified executables**: package `secureexec` (`Verify`, `Verified`, `Start`, `Process`, `HashFile`, `ParseHash`, `CleanVaults`, `Policy`, `WithPolicy`, and `With*` options). Executes the exact bytes verified by SHA-256 without a TOCTOU gap using a write-sealed `memfd` on Linux, a private `0700` vault directory on macOS/FreeBSD/POSIX, and a `FILE_SHARE_READ`-locked file handle on Windows. Exports `ErrHashMismatch` (`*HashMismatchError`), `ErrNotRegular`, and `ErrUnsupported`.
- **Signed release manifests**: package `secureexec/manifest` (version 1 `manifest.json` with a detached ed25519 signature verified prior to JSON parsing; `Parse`, `Verify`, `Scan`, `Sign`, `VerifyFiles`, `Marshal`, `GenerateKey`, and key formatting helpers) and CLI tool `cmd/hqq-manifest` (`keygen`, `gen`, `sign`, `verify`, `hash`).
- **HQQ server launcher**: package `launch` (`Start`, `Config`, `ManifestSource`, `Server`, `NewListenerName`, `ReadyPrefix`, `DefaultLifelineFlag`, `StartError`, `ReadyTimeoutError`; sentinels `ErrConfig`, `ErrManifest`, `ErrNoArtifact`, `ErrReadyTimeout`, `ErrStopTimeout`). Verifies the target binary with `secureexec`, attaches a `--parent-stdin` lifeline, waits for the `ready <listener>` handshake line on stdout, and stops the process gracefully before killing after `StopTimeout`. Package `sqlite/launcher` (`Start`, `Open`, `OpenManaged`, `Server`, `Managed`, `Config`, `ManifestSource`, `ReleasePublicKey`) layers `database/sql` connector setup and embedded release-key lookup on top of `launch`.
- **Release pipeline**: `.github/workflows/release.yml` builds, tests, and smoke-tests `hqq-sqlite-server` natively across Linux, macOS, and Windows, generates `manifest.json`, `licenses.tar.gz`, and `SHA256SUMS`, signs the manifest with `HQQ_RELEASE_SIGNING_KEY`, and publishes GitHub Releases from `v*` tags. Supporting scripts live in `scripts/release/`, pinned SQLite amalgamation digests in `third_party/sqlite/`, and operational docs in `docs/RELEASING.md`.
- **Benchmarks**: `BenchmarkCompareRoundTripZeroCopy` plus payload-size and deadline variants of `BenchmarkAdvancedLinkRequestResponse`.
- **`hqq-bindgen`**: CLI tool `cmd/hqq-bindgen` (`-go-out`, `-go-package`, `-go-strip`, `-c-out`, `-check`, `-print-canonical`, `-print-digest`) and packages `bindgen/idl` (lexer, parser, validation rules V1 through V41, canonical formatter, SHA-256 interface digest, wire planner), `bindgen/gen` (`Go`, `C`), and `bindgen/rt` (Go client runtime: `Dial`, `Conn`, futures, `Pool`, `Enc`, `Dec`, error types). Generates a pure-Go client and C server from an `.idl` file so Go programs can invoke out-of-process C libraries over HQQ shared memory without cgo. Normative design: `spec/toolchain/BINDGEN.md`; overview and benchmarks: `bindgen/README.md`.
- **C runtime for generated servers**: `c/include/hqq/hqq_rpc.h` (`hqqrpc_*`: in-place readers/writers, generation-checked handle table, interceptors, `hqqrpc_server_*`, `hqqrpc_main`) implemented in `c/src/hqq_rpc.c`, `hqq_rpc_session.c`, and `hqq_rpc_server.c`, plus cross-platform mutex and detached-thread primitives (`hqq_plat_mutex_*`, `hqq_plat_thread_detach`) in `hqq_platform.h`. `hqqrpc_main` runs on POSIX platforms and returns `HQQ_ERR_UNSUPPORTED` on Windows.
- **`hqq-bindgen` examples and test suites**: `bindgen/e2e/microlib` (end-to-end suite against the compiled C server covering all parameter kinds, handle lifecycles and reverse-creation-order teardown, oversize payloads, pipelining, peer crash, and differential model checking), `bindgen/bench` (cgo vs HQQ benchmark suite), `bindgen/examples/zstd` (pinned zstd 1.5.7 via `scripts/release/fetch-zstd.sh` and `third_party/zstd/`), `bindgen/examples/sqlite3` (generated low-level SQLite service reusing the hand-written server's path and open-policy modules), and `.github/workflows/bindgen.yml`.
- **`hqq-sqlite-gen`**: package `sqlite/gendriver`, a `database/sql` driver built on top of the `hqq-bindgen` SQLite client. Passes all 107 test cases across 11 groups in `sqlite/internal/contract` (using `contract.Capabilities.NoInterrupt` for the query-cancellation test because `hqq-bindgen` does not support mid-call cancellation). Coexists alongside `hqq-sqlite`.

### Changed

- **SQLite package paths**: moved from `examples/sqlite/*` to top-level `sqlite` (see Migration table below). The C server remains in `examples/sqlite/server`.
- **`sqlite.Error.Code` holds the primary result code**: previously held the extended result code. `ExtendedCode` now stores the extended code (defaulting to `Code` when no extended code is present), and `Primary()` returns `Code`.
- **No automatic retry on lost links during `Exec`**: `Exec` and `ExecStmt` now wrap `ErrOutcomeUnknown` instead of `driver.ErrBadConn` when the link drops mid-call.
- **Time column scanning**: `TEXT` values in `DATE`, `DATETIME`, and `TIMESTAMP` columns now scan as `time.Time` when valid (previously scanned as raw `string`).
- **Driver-level argument validation**: parameter conversion uses the driver's `CheckNamedValue` implementation rather than `database/sql`'s default converter.
- **StandardLink fast path**: 64-byte round-trip latency dropped by ~40 ns (235 ns to 192.7 ns in-process on Apple M1; 230–237 ns to 186–187 ns across processes from Go client to C server). `Write`, `Read`, `ReadZeroCopy`, `WriteZeroCopy`, `ReserveRead`, and `ReserveWrite` avoid per-call closure/cancellation setup, derive `CopyID` from the claimed producer slot position, skip zeroing already-zero unused operand words (codified in `spec/SPEC.md` §9.1 and verified by `TestStandardLinkPacketsKeepUnusedOperandsZero`), and unroll spin-wait checks to four slot reads per step with cancellation polling every 8th step.
- **`spec/SPEC.md` §9.1**: explicitly permits senders to omit writing operand words that are already zero by ring invariant (`Format` zeroes words 4..7, which no Standard opcode modifies).
- **Lazy escalation timer in `Serve`**: `Serve` no longer allocates or resets a timer on every request. The 200 µs inline-handler escalation watchdog is armed lazily, so fast handlers never touch the timer and a single timer covers bursts of requests. Measured gain on a 64-byte `AdvancedLink.Call`: ~7% (584 ns to 541 ns in-process).
- **Monotonic arrival-time deadlines**: request handler deadlines are computed from the receive path's monotonic timestamp plus the caller's transmitted time budget, saving a redundant clock read per request.
- **Inline request execution in C (`hqq_adv_serve`)**: previously, every request arriving at a C server was handed off to a worker thread and woke all parked threads on the link (~35 µs per 64-byte call on Apple M1). `hqq_adv_serve` now mirrors Go's `Serve`: when no handler is running, the receive thread executes the incoming request inline (~0.6 µs per call: 605–632 ns mean, 541–583 ns p50, 708–792 ns p99 in `BenchmarkAdvancedCallCServer`). If an inline handler runs longer than 200 µs, a new thread takes over receiving so cancellations, responses, and recursive calls never deadlock, and overlapping requests dispatch to on-demand worker threads (up to 64). Added `c/tools/hqq_adv_echo`, `BenchmarkAdvancedCallCServer`, and `c/tests/test_adv_serve.c`.
- **Wider adaptive spin window (`WaitAdaptive`)**: increased the Go adaptive spin phase from 64 to 256 four-read steps (~6 µs instead of ~1.5 µs; `spec/constants.json` `wait.spin_iterations` and `SPEC.md` §8.4) and the C spin phase from 64 to 1024 single-read steps (~4 µs). The previous 1.5 µs window expired before a peer performing 1–3 µs of work (such as a SQLite query) could reply, pushing callers into `runtime.Gosched` thread hand-offs. With the wider spin window, a prepared point select through `hqq-sqlite-gen` dropped from 6.4 µs to 3.1 µs, `SELECT 1` from 17 µs to 3.4 µs, and a 1,000-row transactional insert from 4.8 ms to 1.0 ms, while idle CPU stays at 0.1–0.2%.
- **Shared SQLite internals**: parameter conversion, timestamp parsing, and DSN handling moved into `sqlite/internal/convert` and `sqlite/internal/dsn` so `hqq-sqlite` and `hqq-sqlite-gen` share a single implementation.

### Fixed

- **libhqq (C): uninitialized deadline fields in newly allocated request contexts**. When the C server's context pool ran dry under high concurrency, freshly `malloc`-allocated request contexts did not zero `has_deadline` and `deadline_mono`. Requests without a caller deadline could read garbage bytes as an expired deadline and be silently dropped by worker threads, hanging the caller (`sqlite/e2e` on Windows with 32 concurrent sessions). All allocated contexts are now zero-initialized, covered by `c/tests/test_adv_uninit.c` using a poison allocator.
- **Takeover boundary race in `claimSuperblock` and `claimListener`**: when an initializer stalled, the outer timeout check read the monotonic clock slightly after the inner takeover check, occasionally failing with a transient initialization error on the final iteration instead of taking over. The claim loop now retries takeover once the takeover window has elapsed.
- **CI diagnostics for SQLite e2e and launcher suites**: `sqlite/e2e` and `sqlite/launcher` run with `go test -p 1`, and the contract test runner terminates and dumps server logs after 60 s if a call stalls.
- **libhqq (C): double release of unserved queued requests in `hqq_adv_close`**. An incoming request queued for `Serve` was referenced by both the request queue and its request context; closing an `AdvancedLink` without draining requests via `hqq_adv_serve` returned the pooled message to the free list twice (use-after-free under ASan). `free_link_state` now detaches the context reference before releasing queued requests, covered by `c/tests/test_adv_close_queued.c`.
- **Windows MSVC (`cl.exe`) C11 atomics and `/W4 /WX` support**: enabled `/experimental:c11atomics` alongside `/std:c11` in `c/CMakeLists.txt` and `bindings/rust/hqq-sys/build.rs`, replaced POSIX-only `<strings.h>` and `_Static_assert` spellings with portable C11 equivalents, and guarded MSVC warnings `C4100`, `C4127`, `C4200`, `C4324`, and `C4702` across `c/` and `examples/sqlite/server`. All 22 CTest suites now build and pass under `cl.exe` on both `windows-latest` (x64) and `windows-11-arm` (arm64).

### Removed

- **Zig binding (`bindings/zig`)**: removed because it was not exercised in CI.
- **Legacy paths under `examples/sqlite/`**: `examples/sqlite/driver`, `examples/sqlite/wire`, `examples/sqlite/fakeserver`, and `examples/sqlite/e2e` (moved to `sqlite/`).
- **`hqqd` and `supervisor` package**: removed `cmd/hqqd`, `hqqd ctl`, and `gosuda.org/hqq/supervisor` (introduced in v0.6.0) to keep the repository focused on IPC transport and point-to-point child launching (`launch`, `sqlite/launcher`, `lifeline`, `secureexec`).

### Migration (v0.6.0 to Unreleased)

| v0.6.0 | Unreleased |
| :--- | :--- |
| `import _ "gosuda.org/hqq/examples/sqlite/driver"` | `import _ "gosuda.org/hqq/sqlite"` |
| package `hqqsqlite`: `hqqsqlite.NewConnector`, `hqqsqlite.Config`, `*hqqsqlite.Error`, ... | package `sqlite`: `sqlite.NewConnector`, `sqlite.Config`, `*sqlite.Error`, ... |
| `(*Error).Code` is the extended code | `(*Error).Code` is the primary code; `(*Error).ExtendedCode` is the extended code |
| `(*Error).Primary()` is `Code & 0xFF` | `(*Error).Primary()` is `Code` |
| `Exec` after a lost link: `driver.ErrBadConn` | `Exec` or `ExecStmt` after a lost link: `sqlite.ErrOutcomeUnknown` (not retried) |
| `TEXT` in a `DATE`/`DATETIME`/`TIMESTAMP` column: `string` | `time.Time` when it parses, otherwise `string` |
| `examples/sqlite/wire` (importable) | `sqlite/internal/wire` (module-internal) |
| `examples/sqlite/fakeserver` (importable) | `sqlite/internal/fakeserver` (module-internal) |
| `examples/sqlite/e2e` | `sqlite/e2e` |
| `examples/sqlite/PROTOCOL.md` | `sqlite/PROTOCOL.md` |
| `examples/sqlite/server` | unchanged |
| `hqqd`, `hqqd ctl`, `import "gosuda.org/hqq/supervisor"` | removed; use host supervision (`systemd`, `launchd`, container runtime), `sqlite/launcher` or `launch` for child servers, and `lifeline` + `secureexec` |
| `bindings/zig` | removed |

### Verification Notes and Known Limitations

- **Placeholder release public key**: `sqlite/launcher/release_public_key.hex` currently holds a placeholder string until the first signed release key is generated. Until replaced, `launcher.ReleasePublicKey()` returns `ErrNoReleaseKey`, and manifest-verified launches must supply an explicit `PublicKey`.
- **Context wake-up allocation on kernel park**: when an adaptive ring wait parks in the kernel while using a cancelable or deadline context, `waiter.park` in `internal/mpmc` registers a `context.AfterFunc` callback, which allocates once per kernel park. Use `WaitSpin` or `context.Background()` when strict zero-allocation behavior under heavy scheduling contention is required.
- **Platform test coverage**: Linux (`memfd` and `vm.memfd_noexec`) and Windows (`windows-share-lock`, Windows lifeline, `clang-cl` and MSVC builds) are verified continuously in GitHub Actions across `ci.yml`, `c.yml`, `bindgen.yml`, `sqlite.yml`, `interop.yml`, and `bindings.yml`.
- **`hqq-bindgen` platform and scheduler notes**: `hqqrpc_main` supports POSIX servers only (`HQQ_ERR_UNSUPPORTED` on Windows). On macOS arm64 (Apple M1), `BenchmarkWork` exhibits a scheduler core-placement jump around `iters=256` in synchronous mode (1,795 ns vs 396 ns cgo), whereas pinning client and server to separate cores in `linux/arm64` on the same hardware (`HQQ_BENCH_SERVER_CPU=1 taskset -c 0`) keeps synchronous IPC overhead flat at 358–476 ns across `iters=0..1024`. See `bindgen/README.md` for full data.

## v0.6.0 (2026-10-17)

This release introduces the multi-client acceptor (`NewAcceptor`, `Accept`, `Connect`), Advanced Protocol 1.1 (negotiated request cancellation, deadline and trace metadata propagation, and graceful `Shutdown`), the C11 reference library (`libhqq`), C++17 and Rust bindings, formal specification documents under `spec/`, and the SQLite service example. All Go API changes relative to v0.5.0 are additive, and the `HQQ_STL3` superblock, ring layout 2, and 64-byte packet ABI remain unchanged.

### Added

- **Multi-client acceptor**: `NewAcceptor`, `NewAcceptorContext`, `Acceptor` (`Accept`, `Close`, `Name`, `Addr`, `Stats`), `AcceptorConfig`, `AcceptorStats`, `Connect`, and `DefaultAcceptorQueueSize`. Uses listener layout `HQQ_LSN1` (`spec/ext/LISTENER_DRAFT.md`) where each client creates an isolated link segment and publishes a connect record. New sentinels: `ErrListenerBusy`, `ErrListenerClosed`, `ErrListenerGone`, `ErrAcceptorClosed`.
- **Advanced Protocol 1.1** (negotiated; 1.0 peers negotiate 1.0): `FeatureCancel` (`OpCancel`, opcode `0x07`), `FeatureMetadata` (request metadata block, frame flag `AdvFlagMeta`), `CallOptions`, `Stream.CallWith`, `AdvancedLink.CallWith`, `Request.Caller`, `Request.Trace`, `AdvancedLink.Shutdown`, `ErrShuttingDown`, `ErrCodeShuttingDown` (`0x08`), `ResponseError.Is`, `AdvancedOptions.StreamListenGrace`, `AdvancedStats.CanceledRequests`, and `AdvancedStats.RefusedRequests`. `CurrentProtocolVersion()` returns `1.1`.
- **Formal specification (`spec/`)**: `SPEC.md`, `MEMORY_MODEL.md`, `PORTING.md`, `FINDINGS.md`, `constants.json`, hardware/formal litmus tests, and extension drafts `ext/PROTOCOL_1_1_DRAFT.md` and `ext/LISTENER_DRAFT.md`.
- **C11 reference library `libhqq` (`c/`)**: Standard and Advanced layers, multi-client acceptor and listener, and public headers in `c/include/hqq/`.
- **Language bindings (`bindings/`)**: header-only C++17 wrapper (`hqq::`), Rust workspace (`hqq-sys`, `hqq`, `hqq-interop`), and Zig binding.
- **Conformance and interoperability suite**: golden byte vectors in `conformance/`, `cmd/hqq-interop`, and the two-process test matrix in `interop/`.
- **Memory-model verification (`c/verify/`)**: GenMC model-checking drivers, herd7 litmus tests, and weak-memory stress litmus tests.
- **Process supervisor (`cmd/hqqd`, `supervisor`)**: experimental process manager (subsequently removed in Unreleased).
- **SQLite service (`examples/sqlite`)**: wire protocol, `database/sql` driver `hqq-sqlite`, C server, Go fake server, and end-to-end tests.

### Changed

- **Held stream open requests**: `OpenStream` arriving before the peer calls `ListenStreams` is held for `AdvancedOptions.StreamListenGrace` (default 2 s) instead of failing immediately, and accepted in FIFO order once `ListenStreams` runs.
- **`ListenStreams` registration order**: marks the link as listening before starting the background dispatcher so concurrent stream requests cannot slip through.
- **Per-request handler contexts**: every `ServeHQQ` invocation receives its own `context.Context`, canceled on peer `OpCancel`, caller deadline expiry, stream closure, or `Serve` exit.
- **Suppressed responses for canceled requests**: handlers whose context was canceled by the caller or deadline do not transmit a response frame.
- **`Stream.Close` cancels in-flight handlers**: closing a stream cancels contexts for handlers still running on requests from that stream.
- **`Serve` after `Shutdown`**: `Serve` returns `nil` after `AdvancedLink.Shutdown` completes.
- **`Config.Features` default**: requests all supported features (`FeatureMessages | FeatureRequestResponse | FeatureCancel | FeatureMetadata`) by default.

### Fixed

- **Stalled initializer ownership checks (F-01)**: initializers now verify superblock ownership before each 256 KiB zeroing slice, each 1024-slot `Format` slice, and each publish CAS step (`spec/SPEC.md` §5.3).
- **Windows semaphore token count (F-02)**: wake operations on Windows named segments now release one semaphore token per published slot rather than one per parked process count after name reuse.
- **Future heartbeat timestamps (F-04)**: heartbeats more than 1 s ahead of the wall clock are no longer treated as perpetually fresh after backward clock jumps.
- **Refused-message eviction drain (F-07)**: subsequent chunks of a refused multi-chunk message whose key was evicted from the refusal ring are now drained cleanly instead of failing the receive path with `ErrProtocol`.
- **Abandoned multi-chunk reassembly reclamation (F-09)**: incomplete messages that stall without an `AdvFlagEnd` chunk are reclaimed when a newer message needs reassembly budget after $2^{16}$ received chunks without progress.
- **Peer liveness check with heartbeats disabled (F-10)**: when `HeartbeatInterval < 0`, each send checks the peer's superblock state so a closed peer is detected without blocking.
- **Named segment identity after replacement**: once an endpoint claims or joins a segment, a newly created empty object under the same OS name is never mistaken for the original (`spec/SPEC.md` §11.4).
- **Negotiation operand 2 documentation (F-03)**: documented operand 2 of `OpProtoNegotiate` and `OpProtoAck` as reserved zero.

### Breaking Notes

- **Go API**: strictly additive relative to v0.5.0.
- **Default feature mask**: `NegotiatedFeatures()` on `Listen` and `Dial` links now includes `FeatureCancel` and `FeatureMetadata` when both peers support Protocol 1.1. Check individual feature bits with `&` rather than testing exact equality against `FeatureMessages | FeatureRequestResponse`.
- **Stream open grace**: `OpenStream` waits up to `StreamListenGrace` (default 2 s) before failing with `ErrConnNotFound` when the peer has not called `ListenStreams`.

## v0.5.0 (2026-10-16)

This release redesigns the `AdvancedLink` Go API around typed `Stream` handles, pooled `Message` buffers, and a `net/http`-style `Serve` / `Handler` / `ResponseWriter` RPC server that achieves zero heap allocations in steady state. **The Go API of the Advanced layer changes incompatibly; the wire protocol (Advanced Protocol 1.0), 64-byte packet ABI, and `HQQ_STL3` shared-memory layout are unchanged.**

### Breaking

- **Typed `Stream` objects**: `OpenStream` and `AcceptStream` return `*Stream` instead of raw `uint64` IDs. `Stream` provides `ID`, `Send`, `Recv`, `Call`, and `Close`. Stream `0` represents the link itself (`AdvancedLink.Stream()`).
- **Pooled `*Message` results**: `Recv` and `Call` return `*Message` (`Bytes`, `Len`, `StreamID`, `Release`). `Recv` returns data messages for its stream only, never RPC requests or responses.
- **`net/http`-style `Serve` and `Handler`**: `Handler.ServeHQQ(ctx, w *ResponseWriter, req *Request)` replaces `ServeHQQ(ctx, AdvancedMessage) ([]byte, error)`. `Request` exposes `Method`, `Payload`, and `StreamID`; `ResponseWriter` exposes `Write` and `WriteError`.
- **`ResponseError` value type**: `AdvancedResponseError` is renamed to `ResponseError{Status, Payload}` and returned by value from `Call`.
- **Feature and stats renames**: `FeatureLargeCopy` is renamed to `FeatureMessages` (`1 << 0`), and `Stats.LargeMessages` / `LargeBytes` are renamed to `Stats.Messages` / `MessageBytes`.
- **Context-free `Stream.Close()`**: `Stream.Close()` takes no context, waits for room in the transmit ring to send `OpConnClose`, and marks the stream closed locally.
- **Stream close sentinels**: local stream closure returns `ErrClosed`, and remote stream closure returns `ErrPeerClosed` (after draining queued messages on `Recv`), replacing `ErrConnNotFound` on closed stream IDs.
- **Unmatched traffic dropped**: unmatched responses and data frames for unknown or closed streams are dropped and counted in `Stats().DroppedMessages`.
- **Per-queue overflow**: `OverflowPolicy` applies independently to each stream queue, the `Serve` request queue, and the stream-accept queue.
- **Removed symbols**: `AdvancedMessage`, `SendLarge`, `Receive`, `CallOn`, `Respond`, `RespondError`, `MessageHandler`, `Mux.OnMessage`, `CloseStream`, `AdvancedLink.Read` and `Write`, `ErrAdvancedActive`, `AdvancedResponseError`, `FeatureLargeCopy`, `Stats.LargeMessages`, and `Stats.LargeBytes`.

### Added

- `Stream`, `AdvancedLink.Stream`, `Message`, `Handler`, `HandlerFunc`, `Request`, `ResponseWriter`, `ResponseError`, `FeatureMessages`, `Stats.Messages`, and `Stats.MessageBytes`.
- `ResponseWriter.WriteError(status, p)` and `Mux.NotFound`.
- Zero-allocation test suite (`alloc_test.go`), stream lifecycle and escalation tests (`stream_test.go`), and Advanced microbenchmarks (`advanced_bench_test.go`).

### Changed

- **Unified receive path and synchronous fast path**: a single receive path reassembles messages and routes them to waiting `Call` channels, the `Serve` request queue, or per-stream queues. Single-waiter calls with no open streams read synchronously from the ring without a background dispatcher.
- **Adaptive inline/worker `Serve`**: `Serve` executes handlers inline on the receiver goroutine when no other handler is running, escalating to a new receiver goroutine after 200 µs and dispatching overlapping requests to pooled workers (bounded by `BufferCount`).
- **Per-link size-class buffer pools**: messages, request/response bodies, reassembly state, and call channels are pooled per link in power-of-two size classes from `64 B` to `MaxMessageSize`.

### Performance

Measured on `darwin/arm64` comparing v0.4.1 and v0.5.0 across 9 interleaved runs with `benchstat`:

| Benchmark | v0.4.1 | v0.5.0 | Change |
| :--- | :--- | :--- | :--- |
| Large message (`4 KiB` over `256 B` chunks, `bufferCount` 1024) | `1.577 µs`, `4192 B/op`, `2 allocs/op` | `1.399 µs`, `0 B/op`, `0 allocs/op` | `-11%` |
| Request-response (`1 KiB` + `1 KiB`, `256 B` chunks) | `1.508 µs`, `2240 B/op`, `4 allocs/op` | `1.29 µs`, `0 B/op`, `0 allocs/op` | `-14%` |
| Small message (`64 B`) | not present | `145.5 ns`, `0 B/op`, `0 allocs/op` | new |

### Migration (v0.4.1 to v0.5.0)

| v0.4.1 | v0.5.0 |
| :--- | :--- |
| `FeatureLargeCopy` | `FeatureMessages` |
| `Stats.LargeMessages` / `LargeBytes` | `Stats.Messages` / `MessageBytes` |
| `l.SendLarge(ctx, 0, p)` -> `(msgID, err)` | `l.Send(ctx, p)` -> `err` |
| `l.SendLarge(ctx, id, p)` | `s.Send(ctx, p)` |
| `l.Receive(ctx)` -> `AdvancedMessage` | `l.Recv(ctx)` / `s.Recv(ctx)` -> `*Message` (data only), then `m.Release()` |
| `msg.Payload` / `msg.ConnectionID` | `m.Bytes()` / `m.StreamID()` |
| `l.Call(ctx, method, req)` -> `[]byte` | `m, err := l.Call(...)`, read `m.Bytes()`, `m.Release()` |
| `l.CallOn(ctx, id, method, req)` -> `AdvancedMessage` | `s.Call(ctx, method, req)` -> `*Message` |
| `l.OpenStream(ctx)` / `AcceptStream(ctx)` -> `uint64` | returns `*Stream` |
| `l.CloseStream(ctx, id)` | `s.Close()` (no context; second call returns `ErrClosed`) |
| `Handler.ServeHQQ(ctx, AdvancedMessage) ([]byte, error)` | `ServeHQQ(ctx, w *ResponseWriter, req *Request)` |
| handler returns `payload, nil` | `w.Write(payload)` |
| handler returns `AdvancedResponseError{S, P}` or `error` | `w.WriteError(S, P)` |
| `req.MethodID` / `req.Payload` / `req.ConnectionID` | `req.Method()` / `req.Payload()` / `req.StreamID()` |
| `l.Respond` / `l.RespondError` after `Receive` | removed: run `Serve` with a `Handler` |
| `MessageHandler` / `Mux.OnMessage` | removed: use `Stream.Recv` |
| `mux.HandleFunc(id, func(ctx, AdvancedMessage) ([]byte, error))` | `mux.HandleFunc(id, func(ctx, *ResponseWriter, *Request))` |
| `errors.As(err, &AdvancedResponseError{})` | `errors.As(err, &ResponseError{})` |
| `AdvancedLink.Read` / `Write`, `ErrAdvancedActive` | removed: use a `StandardLink` before negotiating |
| `ErrConnNotFound` for a closed stream ID | `ErrClosed` (local close) / `ErrPeerClosed` (peer close) |
| unmatched response delivered to `Receive` | dropped, counted in `DroppedMessages` |
| `Respond(..., status!=0, ...)` success with non-zero status | success is always status `0`; `WriteError` carries non-zero status |

## v0.4.1 (2026-10-16)

This release hardens the public Go API, the shared-memory superblock ABI (`HQQ_STL3`), segment lifecycle and takeover rules, MPMC ring reservation completion, and the Advanced dispatcher. **Both the Go API and the shared-memory ABI change incompatibly relative to v0.2.0**: endpoints must be upgraded together, and `HQQ_STL2` segments are rejected with `ErrIncompatibleLayout`.

### Breaking

**API**:

- Removed raw `uintptr` offset constructors; use `NewStandardLink(mem []byte, ...)` and `NewAdvancedLink(mem []byte, ...)` for caller-owned memory, and `CreateStandardLink` / `OpenStandardLink` / `CreateAdvancedLink` / `OpenAdvancedLink` for named segments.
- `CreateStandardLink` and `CreateAdvancedLink` take `perm os.FileMode` plus the options struct; `Config.Mode` is renamed to `Config.Perm`.
- Renamed `AdvancedLink` stream methods: `Listen` to `ListenStreams`, `Accept` to `AcceptStream`, and `Dial` to `OpenStream` (package-level `hqq.Listen` and `hqq.Dial` are unchanged).
- Dropped `Get` prefixes on getters (`Mode`, `Type`, `ProtocolState`, `NegotiatedVersion`, `NegotiatedFeatures`, `Stats`), made `CurrentProtocolVersion()` a function, and removed `Connection`, `ConnectionState`, `Capability*`, `IsLargeCopyEnabled`, `IsRequestResponseEnabled`, and `(*AdvancedLink).Standard()`.
- `StandardLink.ReadFrom` now implements datagram truncation semantics and returns the peer's role address (`"hqq-primary"` or `"hqq-secondary"`).
- After `Close`, unread partial message tails are discarded and reads return `ErrClosed`.
- Creators (`Create*`, `Listen`) reclaim existing segments whose endpoints are all dead and fail with `ErrLinkBusy` if any endpoint is alive; openers (`Open*`, `Dial`) never initialize, re-initialize, or take over a segment.
- Failed protocol negotiation is terminal (`ErrNegotiationFailed`).
- Rejects builds on `GOARCH=mips`, `mipsle`, and `arm` with `GOARM < 7` via `arch_unsupported.go`.

**ABI**:

- Superblock magic updated from `HQQ_STL2` (`0x4851515F53544C32`) to `HQQ_STL3` (`0x4851515F53544C33`), layout version `3.0`, adding `RequiredFeatures` (offset 88) and `CreatorToken` (offset 96).

### Added

- `(*AdvancedLink).CloseStream(ctx, connectionID)`, `ErrNegotiationFailed`, and `*GeometryError` (`Want`, `Got`).
- Superblock `RequiredFeatures` and `CreatorToken`, process-level waiter aggregation in `internal/mpmc`, non-blocking control sender in `AdvancedLink`, and Linux `fallocate` backing allocation.

### Changed

- Creators reuse abandoned names safely; openers never initialize; foreign non-zero pages are rejected without modification; initializer takeover uses monotonic progress tracking; `Close` and `Unlink` verify `CreatorToken` before removing a name; and reservation completion uses a compare-and-swap guard against copied reservation structs.

### Fixed

- Fixed copied-reservation double completion, dispatcher transmit-ring deadlocks under bidirectional flood, concurrent partial-read message mixing, premature `Close` unlinking of replaced names, Linux `/dev/shm` symlink traversal (`O_NOFOLLOW`), and un-canceled waits inside `Dial`/`Listen`.

### Removed

- All deprecated v0.2.0 constructors, getters, `internal/race` `//go:norace` wrappers, and `mpmc.MPMCInit`.

### Migration

| v0.2.0 | v0.4.1 |
| :--- | :--- |
| `OpenStandardLink(offset uintptr, n, size)` | `NewStandardLink(mem, n, size, LinkOptions{})` |
| `OpenStandardLinkWithOptions(offset, n, size, opts)` | `NewStandardLink(mem []byte, n, size, opts)` |
| `OpenStandardLinkMemory(mem, n, size, opts)` | `NewStandardLink(mem, n, size, opts)` |
| `NewAdvancedLink(offset, n, size)`, `NewAdvancedLinkWithOptions(offset, n, size, opts)` | `NewAdvancedLink(mem []byte, n, size, opts AdvancedOptions)` |
| `NewAdvancedLinkMemory(mem, n, size, opts)` | `NewAdvancedLink(mem, n, size, opts)` |
| `CreateStandardLink(name, n, size, mode)` | `CreateStandardLink(name, n, size, perm, LinkOptions{})` |
| `CreateStandardLinkWithOptions(name, n, size, mode, opts)` | `CreateStandardLink(name, n, size, perm, opts)` |
| `OpenNamedStandardLink(name, n, size)`, `...WithOptions(..., opts)` | `OpenStandardLink(name, n, size, opts)` |
| `CreateAdvancedLink(name, n, size, mode)` | `CreateAdvancedLink(name, n, size, perm, AdvancedOptions{})` |
| `CreateAdvancedLinkWithOptions(name, n, size, mode, opts)` | `CreateAdvancedLink(name, n, size, perm, opts)` |
| `OpenNamedAdvancedLink(name, n, size)`, `...WithOptions(..., opts)` | `OpenAdvancedLink(name, n, size, opts)` |
| `(*AdvancedLink).Listen(ctx)` | `ListenStreams(ctx)` |
| `(*AdvancedLink).Accept(ctx)` | `AcceptStream(ctx)` |
| `(*AdvancedLink).Dial(ctx)` | `OpenStream(ctx)` |
| not present | `(*AdvancedLink).CloseStream(ctx, id)` |
| `Connection`, `ConnectionState`, `ConnectionState{Closed,Opening,Open,Closing}` | removed |
| `Capability{None,LargeBuffers,HighThroughput,LowLatency}`, `GetNegotiatedCapabilities()` | removed (capability word is sent as `0`, ignored on receipt) |
| `GetMode()` / `GetType()` | `Mode()` / `Type()` |
| `IsLargeCopyEnabled()` | `NegotiatedFeatures()&FeatureLargeCopy != 0` |
| `IsRequestResponseEnabled()` | `NegotiatedFeatures()&FeatureRequestResponse != 0` |
| `GetProtocolState()` | `ProtocolState()` |
| `GetNegotiatedVersion()` / `GetNegotiatedFeatures()` | `NegotiatedVersion()` / `NegotiatedFeatures()` |
| `GetAdvancedStats()` | `Stats()` |
| `var CurrentProtocolVersion ProtocolVersion` | `func CurrentProtocolVersion() ProtocolVersion` |
| `Config.Mode os.FileMode` | `Config.Perm os.FileMode` |
| `(*StandardLink / *AdvancedLink).LargeMessageThreshold()`, `.IsLargeMessage(n)` | package functions `LargeMessageThreshold(bufferSize)`, `IsLargeMessage(bufferSize, payloadSize)` |
| `(*AdvancedLink).Standard()` | removed |
| `Create*`/`Listen` on an existing name: `os.ErrExist` | re-initialize if both endpoints are gone; `ErrLinkBusy` if an endpoint is alive; `ErrIncompatibleLayout` / `ErrInvalidSize` / `ErrInvalidName` for unusable objects |
| `Open*`/`Dial` on a segment whose endpoints are gone: re-initialize and become Primary | transient error (`hqq: shared memory segment has no live endpoint`); `Dial` retries until a new `Listen` (or `Create*`) reuses the name |
| `ReadFrom` = `Read`; address `"hqq-remote"` or last `WriteTo` address | datagram `ReadFrom` returning peer role address (`"hqq-secondary"` on Primary, `"hqq-primary"` on Secondary) |
| `LocalAddr()` = `"hqq-local"` | `"hqq-primary"` / `"hqq-secondary"` (this endpoint's role) |
| `WriteTo(p, addr)` recorded `addr` | `addr` ignored |
| `CallOn`/`SendLarge` on any non-zero ID | `ErrConnNotFound` unless the ID is an open stream |
| negotiation repeated after failure: `ErrAlreadyNegotiated` | `ErrNegotiationFailed` (terminal) |
| no equivalent | `*GeometryError` (matches `ErrInvalidSize`, `errors.As` for `Want`/`Got`) |
| superblock `HQQ_STL2` | `HQQ_STL3`; recreate segments after upgrading and upgrade both endpoints together |
| `internal/race` helpers | removed |

## v0.2.0 (untagged)

Initial hardening milestone introducing adaptive kernel parking, superblock crash detection, high-level `Listen`/`Dial`/`Serve` APIs, and TLA+ model checking of the MPMC ring.

### Highlights

- **Cross-Process Adaptive Parking (`internal/mpmc`)**: replaced pure busy-spin loops with `WaitAdaptive` (tight spin re-check, `runtime.Gosched`, then Linux `futex`, macOS `__ulock_wait`, or Windows named semaphores), dropping idle reader CPU from ~165% to **~0.1%**.
- **Superblock v2 (`HQQ_STL2`) & Crash Detection**: added per-endpoint session tokens, periodic heartbeats, graceful close markers (`ErrPeerClosed`), abrupt crash detection (`ErrPeerGone`), stale segment reuse, initializer takeover, and 3rd-endpoint rejection (`ErrLinkBusy`).
- **Ergonomic High-Level APIs**: added `hqq.Listen` / `hqq.Dial`, `AdvancedLink.Negotiate`, `AdvancedLink.Serve` + `hqq.Mux` (`Handler` / `HandlerFunc`), `OpenStandardLinkMemory`, `AlignedBuffer`, and `hqq.Unlink`.
- **Zero `go vet` Warnings**: reworked `internal/mpmc` and `internal/mmap` pointer arithmetic to pass default `go vet ./...` (including `unsafeptr`) across Linux, macOS, and Windows.
- **TLA+ Model Checking (`internal/mpmc/MPMC.tla`)**: verified `NoRace`, `DataIntegrity`, `FIFOBound`, `FIFOClaimOrder`, and `SystemProgress` with TLC (`scripts/tlc.sh`, 662,863 states, 0 errors).

### Added

- `hqq.Listen`, `hqq.Dial`, `hqq.Mux`, `hqq.Handler`, `hqq.HandlerFunc`, `(*AdvancedLink).Serve`, `(*AdvancedLink).Negotiate`, `OpenStandardLinkMemory`, `NewAdvancedLinkMemory`, `AlignedBuffer`, and `hqq.Unlink`.
- `LinkOptions`, `OverflowPolicy`, `PeerState`, `AdvancedOptions.MaxReassemblyBytes`, and granular sentinel errors wrapping standard library base errors.
- Windows cross-process named-semaphore parking, `WaitOnAddress` unnamed parking, DACL permission translation, and `NtQuerySection` size verification.
- Multi-process crash/lifecycle tests (`lifecycle_test.go`), fuzz targets, runnable GoDoc examples, and Unix socket / OS pipe comparison benchmarks.

### Changed

- Split monolithic `link.go` into focused files (`errors.go`, `options.go`, `superblock.go`, `standard.go`, `reservation.go`, `packetconn.go`, `advanced.go`, `advanced_io.go`, `advanced_dispatch.go`, `advanced_stream.go`, `named_link.go`, `rpc.go`, `connect.go`).
- Unified MPMC producer/consumer claim paths with 64-byte slot alignment and a 512-byte cache-line-isolated header.
- Defaulted Windows shared-memory namespace to `Local\` instead of `Global\`.
- Updated `StandardLink.Read` to return `io.EOF` on graceful peer close, enforced stream ID parity in `AdvancedLink.Dial`, and dropped aborted multi-chunk messages instead of delivering partial payloads.

### Fixed

- Resolved data races in `StandardLink` deadlines and `Close`, `SIGSEGV` unmap-before-drain hazards in named links, `SIGBUS` races before `ftruncate`, shared-memory name traversal bugs, dispatcher crashes on late canceled-call responses, and panicking zero-copy callback ring wedges.
