# Changelog

All notable changes to **HQQ** (`gosuda.org/hqq`) are documented in this file.

The project follows [Semantic Versioning](https://semver.org/). While in `v0.x`, minor versions may introduce breaking wire-layout improvements accompanied by migration notes below.

## v0.6.0 — 2026-10-17

The multi-client acceptor (`NewAcceptor`, `Accept`, `Connect`) and Advanced Protocol 1.1 (negotiated cancellation, deadlines and request metadata, graceful shutdown) are added. The Go API changes are additive: no exported name of v0.5.0 was removed or changed. The shared-memory layout (`HQQ_STL3`, ring layout 2) and the 64-byte packet ABI are unchanged. Protocol 1.1 adds one opcode, one frame flag, a request metadata block, and the error status `0x08` (returned by `Shutdown`); the opcode, flag, and metadata block are used only when negotiated.

### Added

- **Multi-client acceptor.** `NewAcceptor`, `NewAcceptorContext`, `Acceptor` (`Accept`, `Close`, `Name`, `Addr`, `Stats`), `AcceptorConfig`, `AcceptorStats`, `Connect`, and `DefaultAcceptorQueueSize`. A listener is a named segment (layout `HQQ_LSN1`, see `spec/ext/LISTENER_DRAFT.md`); each client creates its own link segment and announces it in the listener's queue. New errors: `ErrListenerBusy`, `ErrListenerClosed`, `ErrListenerGone`, `ErrAcceptorClosed`.
- **Advanced Protocol 1.1** (negotiated; a 1.0 peer negotiates 1.0): `FeatureCancel` (`OpCancel`, opcode `0x07`), `FeatureMetadata` (request metadata block, frame flag `AdvFlagMeta`), `CallOptions`, `Stream.CallWith`, `AdvancedLink.CallWith`, `Request.Caller`, `Request.Trace`, `AdvancedLink.Shutdown`, `ErrShuttingDown`, `ErrCodeShuttingDown` (`0x08`), `ResponseError.Is`, `AdvancedOptions.StreamListenGrace`, `AdvancedStats.CanceledRequests`, `AdvancedStats.RefusedRequests`. `CurrentProtocolVersion()` returns 1.1. With `FeatureMetadata`, a `Call` whose context has a deadline sends the remaining time with the request, and the handler's context gets that deadline.
- **Specification** (`spec/`): `SPEC.md` (segment layout, superblock, roles, initialization, liveness, ring, wait/wake, Standard and Advanced Protocol 1.0, platform naming, error codes), `MEMORY_MODEL.md`, `PORTING.md`, `FINDINGS.md`, `constants.json`, litmus tests, and the extension documents `ext/PROTOCOL_1_1_DRAFT.md` and `ext/LISTENER_DRAFT.md`.
- **C library** `libhqq` (`c/`): Standard and Advanced layers, acceptor and listener, public headers in `c/include/hqq/`.
- **Bindings** (`bindings/`): header-only C++17 wrapper (`hqq::`), Rust workspace (`hqq-sys`, `hqq`, `hqq-interop`), Zig binding.
- **Conformance and interop**: golden vectors in `conformance/`, the `cmd/hqq-interop` helper, and the two-process matrix in `interop/` (`HQQ_INTEROP_IMPLS` selects the implementations).
- **Memory-model verification** (`c/verify/`): GenMC harnesses, herd7 litmus tests, and stress litmus tests. These run in CI only.
- **hqqd** process manager (`cmd/hqqd`, `supervisor` package): starts and health-checks services, restarts them with backoff, and is controlled over an HQQ admin listener (`hqqd ctl`).
- **SQLite service example** (`examples/sqlite`): wire protocol, `database/sql` driver `hqq-sqlite`, C server, Go fake server, and end-to-end tests.

### Changed

- **Held stream requests.** `OpenStream` to a peer that has not called `ListenStreams` yet is held for `AdvancedOptions.StreamListenGrace` (default 2 s) instead of failing at once. A held request is accepted when the peer calls `ListenStreams`, in arrival order, as if it had just arrived. It is refused with `ErrConnNotFound` when the grace period ends, when the link fails, or when `Shutdown` runs. A request is refused at once when the held requests and the accepted streams not yet returned by `AcceptStream` fill `IncomingQueueSize`, and every request is refused at once when the grace is negative.
- **`ListenStreams` ordering.** The link is marked as listening before the dispatcher starts, so a stream request routed in between is no longer refused.
- **Handler contexts are per request.** Each `ServeHQQ` gets its own context. It is canceled when the caller cancels (`FeatureCancel`), when the deadline the caller sent expires (`FeatureMetadata`), when the stream the request arrived on closes, and when `Serve` ends. The context is valid only during `ServeHQQ`.
- **Canceled requests are not answered.** No response is sent when the caller gave up, or the deadline passed, while the handler ran. A request that is canceled before its handler starts is not passed to the handler.
- **`Stream.Close` cancels handlers.** The contexts of handlers still running for requests received on the closed stream are canceled. `Serve` still answers them as far as the peer's stream exists.
- **`Serve` after `Shutdown`.** After a successful `AdvancedLink.Shutdown`, `Serve` returns nil, also when called later.
- **`Config.Features` default.** The default is now all supported features (`FeatureMessages|FeatureRequestResponse|FeatureCancel|FeatureMetadata`), so `Listen` and `Dial` negotiate the 1.1 features when the peer supports them.

### Fixed

- A stalled initializer could write into a segment after another endpoint took the initialization over. Ownership is now checked before each publish step, before each 256 KiB zeroing slice, and before each 1024-slot `Format` slice. A frozen initializer can still complete one slice after takeover; the residual window is documented in `spec/SPEC.md` §5.3 (`spec/FINDINGS.md` F-01, mitigated).
- Windows named segments: a wake granted one semaphore token per parked process, so after a name was reused a stale count could release millions of tokens and cause a burst of spurious wake-ups. A wake now grants one token per published slot (F-02; compiled and vetted for Windows only).
- A heartbeat timestamp more than 1 s in the future is no longer taken as fresh. A wall clock stepped backwards made a dead endpoint look alive: a creator got `ErrLinkBusy`, and an opener joined a dead primary until the clock passed the stale value (F-04).
- When the set of refused message keys evicted an entry while that message's chunks were still arriving, the next chunk failed the whole receive path with `ErrProtocol`. Such chunks are now drained (F-07).
- A multi-chunk message whose `End` chunk never arrives keeps its memory charge. Such a message is now reclaimed when a later message would be refused, after 2^16 received chunks without progress; a reclaimed response fails its call with `ErrReassemblyLimit` (F-09).
- With heartbeats disabled (`HeartbeatInterval < 0`), each send checks the peer, so a closed or dead peer is noticed without a blocking call (F-10).
- `ListenStreams`: a stream request routed while `ListenStreams` was starting was refused (see Changed).
- Named links: once a segment is claimed or joined, a later empty object under the same name is no longer treated as this link's, so `Close` does not act on it (`spec/SPEC.md` §11.4).
- Documentation: `OpProtoNegotiate` and `OpProtoAck` operand 2 is reserved (sent as zero, ignored on receipt). It was documented as capability flags; the wire behavior is unchanged (F-03).

### Breaking notes

- **Go API:** additive only. Every exported declaration of v0.5.0 exists with the same signature. Added: the declarations listed under Added, plus `AdvancedStats.CanceledRequests`, `AdvancedStats.RefusedRequests`, and `AdvancedOptions.StreamListenGrace`.
- **Feature set:** `NegotiatedFeatures()` of a `Listen` or `Dial` link now includes `FeatureCancel` and `FeatureMetadata` when the peer supports them. Code that compares the value with `FeatureMessages|FeatureRequestResponse` must test the bits instead. Set `Config.Features` to `FeatureMessages|FeatureRequestResponse` to keep 1.0 features only.
- **Stream open timing:** `OpenStream` can wait up to `StreamListenGrace` for a peer that has not called `ListenStreams` yet, instead of failing at once.
- **Handler cancellation:** a handler that blocks on its context gets it canceled on peer cancel, deadline, stream close, or `Serve` end, and its response is then not sent.
- **Wire and ABI:** the superblock (`HQQ_STL3`), the ring layout (2), and the 64-byte packet layout are unchanged. Protocol 1.1 adds opcode `OpCancel` (`0x07`) and frame flag `AdvFlagMeta` (`1 << 6`); both are used only when negotiated, so a 1.0 peer sees 1.0 frames. A v0.5.0 responder answers a 1.1 request with minor 0 and its own features, which the 1.1 initiator accepts (read from the negotiation code; see Not verified).

### Not verified

- Cross-version interop with v0.5.0 peers: the negotiation behavior above was read from the code, not run.
- Windows: the Go library and the Windows semaphore path were compiled and vetted only; the Windows test jobs run in CI.
- The Zig binding (`bindings/zig`) is written for Zig 0.15.2 and runs in CI only.
- GenMC, herd7, and stress litmus checks (`c/verify`) run in CI only.
- Not run for this release: the full test suite, `-race`, benchmarks, and the conformance and interop suites.

## v0.5.0 — 2026-10-16

The Advanced API is redesigned around `Stream` and pooled `Message`s, with a `net/http`-like server (`Handler`, `ResponseWriter`, `Request`, `Mux`, `Serve`). Send, receive, and request-response allocate nothing in steady state. **The Go API of the Advanced layer changes incompatibly; the wire protocol (Advanced Protocol 1.0), the packet ABI, and the shared-memory layout (`HQQ_STL3`) are unchanged.** The `StandardLink` API is unchanged.

### Breaking

- **Streams are objects.** `OpenStream` and `AcceptStream` return `*Stream` (previously `uint64` IDs). `Stream` has `ID`, `Send`, `Recv`, `Call`, and `Close`. Stream 0 is the link itself (`AdvancedLink.Stream()`); `AdvancedLink.Send`, `Recv`, and `Call` are exactly `Stream().Send`, `Recv`, and `Call`.
- **Received data is a pooled `*Message`** (`Bytes`, `Len`, `StreamID`, `Release`). `Recv` and `Call` return `*Message`; `Release` is called at most once and the message must not be used afterwards. `Recv` returns data messages of its stream only, never requests or responses.
- **Servers use `Serve` with a `Handler`.** `Handler.ServeHQQ(ctx, w *ResponseWriter, req *Request)` replaces `ServeHQQ(ctx, AdvancedMessage) ([]byte, error)`. `Request` has `Method`, `Payload`, and `StreamID`; `ResponseWriter` has `Write` and `WriteError`. `Request` and `ResponseWriter` are valid only during `ServeHQQ`. Requests are consumed only by `Serve`.
- **`AdvancedResponseError` is now `ResponseError`** (`Status`, `Payload`), returned by value from `Call`, with `Payload` a copy. The message text is `hqq: response error …`.
- **`FeatureLargeCopy` is now `FeatureMessages`** (same value, `1<<0`); `Stats.LargeMessages` and `LargeBytes` are now `Stats.Messages` and `MessageBytes`. `Config.Features` defaults to `FeatureMessages|FeatureRequestResponse`.
- **`Stream.Close()` takes no context.** It waits for room in the tx ring and returns the error of sending `OpConnClose`; the stream is closed locally even if that fails. A second `Close` returns `ErrClosed`, and `Close` on stream 0 returns `ErrInvalidOp`.
- **Close semantics.** After a local close, `Send`, `Recv`, and `Call` return `ErrClosed`. After the peer closes a stream, `Recv` returns the queued messages and then `ErrPeerClosed`, and `Send` and `Call` return `ErrPeerClosed`; blocked calls wake. `ErrConnNotFound` is no longer returned for unknown or closed stream IDs (it remains the base of `ErrPeerGone` and the refusal when the peer is not listening).
- **Unmatched traffic is dropped.** A response without a waiting call is no longer delivered to a receiver: it is dropped and counted in `Stats().DroppedMessages` (a late response to a canceled call still goes to `AbandonedResponses`). Data for an unknown or closed stream is dropped and counted as well.
- **Overflow is decided per destination queue.** `OverflowPolicy` applies to each stream's queue, the request queue consumed by `Serve`, and the accept queue; `IncomingQueueSize` sizes all of them. Under `OverflowBlock`, requests fill the request queue when no `Serve` runs, and then stall the receive path. `OverflowFail` makes `Recv` return queued messages first and then `ErrIncomingOverflow`. `ErrIncomingOverflow` now refers to the queue a message is routed to.
- **A success response always has status 0.** A non-zero status exists only on error responses (`WriteError`).
- **Removed:** `AdvancedMessage`, `SendLarge`, `Receive`, `CallOn`, `Respond`, `RespondError`, `MessageHandler`, `Mux.OnMessage`, `CloseStream`, `AdvancedLink.Read` and `Write`, `ErrAdvancedActive`, `AdvancedResponseError`, `FeatureLargeCopy`, `Stats.LargeMessages` and `LargeBytes`. A `StandardLink` is the way to use raw `Read`/`Write`.

### Added

- `Stream`, `AdvancedLink.Stream`, `Message`, `Handler`, `HandlerFunc`, `Request`, `ResponseWriter`, `ResponseError`, `FeatureMessages`, `Stats.Messages`, and `Stats.MessageBytes`.
- `ResponseWriter.WriteError(status, p)`: replaces the body written so far and sets the error status; a later `Write` appends to that body. Status 0 is sent as `ErrCodeInvalidOp`; a payload larger than `MaxMessageSize` becomes status `ErrCodeBufferOverflow` with an empty payload. `ResponseWriter.Write` beyond `MaxMessageSize` writes nothing and returns `ErrBufferOverflow`. A handler that writes nothing answers with an empty success response.
- `Mux.NotFound` handles unregistered methods; without it the answer is status `ErrCodeInvalidOp` with payload `hqq: unknown method`.
- Tests: `alloc_test.go` (zero-allocation checks for `Send`/`Recv`/`Release` and `Call`/`Release` against `Serve`), `stream_test.go` (stream isolation, drop accounting, close and peer-close semantics, per-queue overflow, ordering, `Serve` escalation); `advanced_bench_test.go` (three benchmarks). The multi-process test, the fuzz target, and the examples use the new API; a stream example was added.

### Changed

- **Receive path and routing.** One receive path assembles messages and routes them: responses to the waiting `Call`, requests to the request queue consumed by `Serve`, data to the queue of its stream. A goroutine that finds no dispatcher receives synchronously; a background dispatcher starts when several goroutines wait concurrently or streams are used. A synchronous receiver never waits for a full queue under `OverflowBlock`: it hands the delivery to a helper goroutine that keeps the order, and the dispatcher takes over, so a caller's `Recv` or `Call` timeout cannot lose another goroutine's message.
- **`Serve` is adaptive.** One receiver goroutine at a time takes requests. While no handler is running, it runs each handler inline, which saves a goroutine hand-off per request. A handler still running after 200 µs hands the receiver role to a new goroutine, so a handler that waits for a later request cannot deadlock `Serve`; while any handler is running, requests go to pooled workers (started on demand, at most `BufferCount`, kept until `Serve` returns). The switch is per episode: as soon as a request arrives with nothing running, it runs inline again. A request that arrives behind a slow inline handler waits up to about 200 µs. `Serve` still returns `ctx.Err()`, nil on a graceful peer close, and otherwise the receive error.
- **Pooled buffers per link.** Messages, request and response bodies, reassembly bookkeeping, and pending-call channels are pooled per link; message buffers come in power-of-two size classes from 64 B up to exactly `MaxMessageSize`. Reassembly accounting is unchanged (each message is charged its announced size plus 256 bytes), but the buffer is rounded up to a power of two, so the memory held can reach twice the charge.
- **Streams other than 0 exist only on a link whose dispatcher is running** (`OpenStream`, `AcceptStream`, and `ListenStreams` start it), so a `Recv` or `Call` blocked on a stream always wakes through the stream's close signal.
- **The dispatcher and control-sender ring waits no longer pass the link-lifetime context to the ring**; `Close` wakes them through the closed channel of the `StandardLink`, so parked dispatcher waits do not allocate.
- `ErrClosed` keeps the message `hqq: link closed`, also when it is returned for a locally closed stream.
- `Stats.DroppedMessages` now also counts unmatched responses and data for unknown or closed streams.
- The wire protocol is unchanged. `PROTOCOL.md` describes the routing, overflow, `Serve`, and stream-close behavior of the Go API; `README.md` has an API guide and a section on buffer ownership and zero-allocation operation.

### Performance

Measured on `darwin/arm64` with v0.4.1 and v0.5.0 run interleaved (9 runs each, `benchstat`); request-response for v0.5.0 is the median of a separate quiet run after the final `Serve` change.

| Benchmark | v0.4.1 | v0.5.0 | Change |
| --- | --- | --- | --- |
| Large message (4 KiB over 256 B chunks, `bufferCount` 1024) | 1.577 µs, 4192 B/op, 2 allocs/op | 1.399 µs, 0 B/op, 0 allocs/op | −11% |
| Request-response (1 KiB + 1 KiB, 256 B chunks) | 1.508 µs, 2240 B/op, 4 allocs/op | 1.29 µs, 0 B/op, 0 allocs/op | −14% |
| Small message (64 B) | — | 145.5 ns, 0 B/op, 0 allocs/op | new |

- The v0.4.1 request-response benchmark used a hand-written `Receive`/`Respond` loop, which no longer exists; the v0.5.0 benchmark runs `Serve`. For the same `Serve`-based shape, v0.4.1 measured 5.2 µs, 2313 B/op, 5 allocs/op, so `Serve` is about 4.5× faster.
- A cancelable context on `Send`, `Call`, or `Serve` can still allocate occasionally when a wait parks and arms the context wake-up. The allocation tests use `context.Background()`, and they are skipped under `-race`, where `sync.Pool` drops items at random.

### Migration (v0.4.1 → v0.5.0)

| v0.4.1 | v0.5.0 |
| --- | --- |
| `FeatureLargeCopy` | `FeatureMessages` |
| `Stats.LargeMessages` / `LargeBytes` | `Stats.Messages` / `MessageBytes` |
| `l.SendLarge(ctx, 0, p)` → `(msgID, err)` | `l.Send(ctx, p)` → `err` |
| `l.SendLarge(ctx, id, p)` | `s.Send(ctx, p)` |
| `l.Receive(ctx)` → `AdvancedMessage` | `l.Recv(ctx)` / `s.Recv(ctx)` → `*Message` (data only), then `m.Release()` |
| `msg.Payload` / `msg.ConnectionID` | `m.Bytes()` / `m.StreamID()` |
| `l.Call(ctx, method, req)` → `[]byte` | `m, err := l.Call(...)`, read `m.Bytes()`, `m.Release()` |
| `l.CallOn(ctx, id, method, req)` → `AdvancedMessage` | `s.Call(ctx, method, req)` → `*Message` |
| `l.OpenStream(ctx)` / `AcceptStream(ctx)` → `uint64` | → `*Stream` |
| `l.CloseStream(ctx, id)` | `s.Close()` (no context; a second call returns `ErrClosed`) |
| `Handler.ServeHQQ(ctx, AdvancedMessage) ([]byte, error)` | `ServeHQQ(ctx, w *ResponseWriter, req *Request)` |
| handler returns `payload, nil` | `w.Write(payload)` |
| handler returns `AdvancedResponseError{S, P}` or an `error` | `w.WriteError(S, P)` |
| `req.MethodID` / `req.Payload` / `req.ConnectionID` | `req.Method()` / `req.Payload()` / `req.StreamID()` |
| `l.Respond` / `l.RespondError` after `Receive` | removed: run `Serve` with a `Handler` |
| `MessageHandler` / `Mux.OnMessage` | removed: use `Stream.Recv` |
| `mux.HandleFunc(id, func(ctx, AdvancedMessage) ([]byte, error))` | `mux.HandleFunc(id, func(ctx, *ResponseWriter, *Request))` |
| `errors.As(err, &AdvancedResponseError{})` | `errors.As(err, &ResponseError{})` |
| `AdvancedLink.Read` / `Write`, `ErrAdvancedActive` | removed: use a `StandardLink` before negotiating |
| `ErrConnNotFound` for a closed stream ID | `ErrClosed` (local close) / `ErrPeerClosed` (peer close) |
| unmatched response delivered to `Receive` | dropped, counted in `DroppedMessages` |
| `Respond(…, status≠0, …)` success with a status | not expressible: success is status 0, `WriteError` carries a status |

## v0.4.1 — 2026-10-16

A hardening pass over the public API, the shared-memory ABI, segment lifecycle, the MPMC ring, and the Advanced dispatcher. **Both the Go API and the shared-memory ABI change incompatibly**: all endpoints of a link must be upgraded together, and segments created by earlier revisions are rejected. This is the first tagged release; the migration table below is relative to the previous, untagged revision (v0.2.0).

### Breaking

**API**

- The offset-based (`uintptr`) constructors are gone. One constructor per kind remains: `NewStandardLink(mem []byte, …)` / `NewAdvancedLink(mem []byte, …)` for caller-owned memory (page-aligned, at least `SizeStandardLink` bytes), `CreateStandardLink` / `OpenStandardLink` / `CreateAdvancedLink` / `OpenAdvancedLink` for named segments. The `*WithOptions`, `*Memory`, and `OpenNamed*` variants are removed; options are a regular last parameter.
- `CreateStandardLink`/`CreateAdvancedLink` take `perm os.FileMode` plus the options struct; `Config.Mode` is now `Config.Perm`.
- `AdvancedLink` stream methods are renamed: `Listen` → `ListenStreams`, `Accept` → `AcceptStream`, `Dial` → `OpenStream` (package-level `Listen`/`Dial` are unchanged). New `CloseStream`.
- Getters lost their `Get` prefix: `Mode`, `Type`, `ProtocolState`, `NegotiatedVersion`, `NegotiatedFeatures`, `Stats`. `CurrentProtocolVersion` is a function, not a variable.
- Removed types and methods: `Connection`, `ConnectionState` and its constants; `Capability*` and `GetNegotiatedCapabilities` (the capability word stays on the wire, always sent as `0` and ignored on receipt); `IsLargeCopyEnabled`, `IsRequestResponseEnabled` (test `NegotiatedFeatures()&FeatureLargeCopy != 0`); the `LargeMessageThreshold`/`IsLargeMessage` methods of the link types (use the package functions); `(*AdvancedLink).Standard()`.
- `CallOn` and `SendLarge` return `ErrConnNotFound` for any non-zero connection ID that is not an open stream (previously any ID was accepted). `Respond`/`RespondError` are unchecked on purpose.
- `NewStandardLink`/`NewAdvancedLink` with a nil or empty slice return `ErrMemorySmall` (offset `0` used to return `ErrMemoryAlign`); `NewAdvancedLink` now validates the slice length too.
- `StandardLink.LocalAddr()` returns this endpoint's role address (`"hqq-primary"` or `"hqq-secondary"`, previously a constant `"hqq-local"`); `ReadFrom` returns the peer's role address (previously the last address given to `WriteTo`, or `"hqq-remote"`); `WriteTo` ignores its address.
- `ReadFrom` has datagram semantics: it copies `len(p)` bytes, discards the rest of the message, and returns `n == len(p)` with a nil error (previously it behaved like `Read` and queued the tail). A pending tail left by `Read`/`ReadZeroCopy` is returned as the next datagram.
- After `Close`, queued read tails are no longer served: `Read`, `ReadZeroCopy`, `ReserveRead`, and `ReadFrom` return `ErrClosed`.
- `CreateStandardLink`, `CreateAdvancedLink`, and `Listen` no longer fail with `os.ErrExist` on an existing name; see "Changed" for the reuse rules.
- `OpenStandardLink`, `OpenAdvancedLink`, and `Dial` no longer initialize, re-initialize, or take over a segment (they used to re-initialize a segment whose endpoints were gone and become Primary). They fail with transient errors matching `ErrFailedInit` instead; see "Changed".
- `OverflowDrop` now refuses an incoming stream with `OpError(ErrCodeBufferOverflow)` (previously: `OpConnAccept` followed by `OpConnClose`). The peer's `OpenStream` fails with `ErrBufferOverflow`.
- A failed negotiation is terminal: later `Negotiate`, `NegotiateProtocol`, and `WaitForNegotiation` calls return the new `ErrNegotiationFailed` (they returned `ErrAlreadyNegotiated`). A canceled context or timeout during negotiation also leaves the link in `ProtocolStateFailed`.
- `SizeStandardLink` returns `0` for `bufferCount > 1<<24`, `bufferSize > 1<<30`, or a size that does not fit `uintptr`/`int`; those configurations now fail with `ErrInvalidSize`.
- Builds for `GOARCH=mips`, `mipsle`, and `arm` with `GOARM` below 7 fail (`arch_unsupported.go`).
- Internal `mpmc` package: `MPMCInit` and `_RoundUpPowerOf2` are removed. `SizeMPMCRing` returns `0` and `Format` panics for a size that is not a power of two of at least 2 (it no longer rounds up); `Attach` rejects such sizes; one-slot rings are not supported.

**ABI**

- The superblock magic changes from `HQQ_STL2` (`0x4851515F53544C32`) to `HQQ_STL3` (`0x4851515F53544C33`). `HQQ_STL2` segments are rejected with `ErrIncompatibleLayout`, and v0.2.0 endpoints cannot attach to `HQQ_STL3` segments.
- `Major`/`Minor` (offsets 16/24) now hold the **layout version 3.0** (previously the protocol version 1.0). Offset 88 is `RequiredFeatures` (a bit set a reader must understand; none defined, any set bit is `ErrIncompatibleLayout`) and offset 96 is `CreatorToken` (both were reserved zeros); offsets 104..127 stay reserved. All other offsets are unchanged (`InitToken` 72, `InitHeartbeat` 80, endpoint records at 128 and 256).
- Unchanged: the MPMC ring layout (version 2, 512-byte header), the 64-byte packet ABI, and the protocol version (1.0).

### Added

- `(*AdvancedLink).CloseStream(ctx, connectionID)`: removes the local stream registration and sends `OpConnClose` to the peer, which removes its own; IDs are never reused.
- `ErrNegotiationFailed` (wraps `ErrInvalidOp`).
- `GeometryError` and `Geometry` (`errors.As`): an existing segment's recorded geometry differs from the caller's; matches `ErrInvalidSize`.
- Superblock fields `RequiredFeatures` and `CreatorToken`; roles for named endpoints (creator, opener, caller-owned memory); initializer takeover and safe unlink as specified in PROTOCOL.md.
- Waiter aggregation in `internal/mpmc`: goroutines blocked on the same ring side share one kernel park per process; `emptyWaiters`/`fullWaiters` count parked processes. `ProducerSlot.Owned()` and `ConsumerSlot.Owned()`.
- Control sender: a goroutine with a bounded queue (`max(BufferCount, 16)` replies) that sends the replies generated by the receive path, so the dispatcher never waits for the tx ring. `AdvancedStats.DroppedMessages` also counts dropped replies to `OpConnCreate`.
- `shm.NamesPersist` and `(*SharedMemory).NameRefersToSelf()` (internal), `fallocate`-based allocation on Linux.
- Tests: superblock layout and takeover, named-segment reuse and safe unlink, `ReadFrom` datagram semantics and tail FIFO, stale reservation copies, negotiation terminal state, control-sender flood and `CloseStream`, waiter aggregation; `HQQ_PERF_TESTS=1` gates CPU-ratio tests. `benchmark.yml` runs the perf tests and benchmarks with `-benchtime 100ms` (instead of `1000x`, which mostly measured warm-up); benchmark alerts stay informational (`fail-on-alert: false`) because the benchmark action treats throughput (MB/s) as smaller-is-better. Test shared-memory names carry a per-process sequence number so that tests never share a name on platforms with a coarse clock.
- TLA+: the model checks completion CAS as a guard (`CompletionCAS` invariant) and requires `BufferSize >= 2`; `scripts/tlc.sh` documents the `JAVA` environment variable.

### Changed

- **Create and Listen reuse existing names.** `CreateStandardLink`, `CreateAdvancedLink`, and `Listen` re-initialize an existing segment whose endpoints are both closed or dead (the caller becomes Primary and owns the name with a new `CreatorToken`), fail with `ErrLinkBusy` if an endpoint is alive (a creator never joins a live segment), fail with `ErrIncompatibleLayout` for a foreign or other-version object (left untouched), `ErrInvalidSize` for a smaller object, a transient `ErrFailedInit` variant for a zero-size object, and `ErrInvalidName` for a non-regular object on Linux. On Windows an existing name always means a live handle.
- **Openers never initialize.** `OpenStandardLink`, `OpenAdvancedLink`, and `Dial` only join a live segment as Secondary and never initialize, re-initialize, or take over a segment, so they never become Primary on a named segment and never write `CreatorToken`. On an `Empty` segment they wait up to `AttachTimeout` for the creator and then fail with a transient error (matches `ErrFailedInit`) without touching it; on an `Initializing` segment (stalled or not) they wait up to `AttachTimeout` and then fail with a transient error; on a `Ready` segment with no live endpoint they fail immediately with a transient `hqq: shared memory segment has no live endpoint`. A new creator (`Create*`, `Listen`) recovers the name. `Dial` retries on all of these, so it also retries while the segment has no live endpoint, until a new `Listen` reuses the name or `ctx` ends.
- **Foreign data is never overwritten.** Before any endpoint claims an `Empty` segment, the whole superblock page must be zero, otherwise the call returns `ErrIncompatibleLayout` without writing (also for `NewStandardLink` on caller-owned memory). A `Ready` segment with another magic or layout major, or an unknown `RequiredFeatures` bit, is rejected before join and before takeover.
- **Takeover is relative to first sight.** A dead initializer is taken over, by a creator or an endpoint on caller-owned memory only, after its token and heartbeat stayed unchanged for `max(PeerTimeout, 2s)` on the local monotonic clock (previously: age of a wall-clock timestamp); a stalled initializer is given its full window even past `AttachTimeout`. The initializer checks ownership and the context before `Format` and before every 8 MiB zeroing chunk, and publishes with `CAS(InitToken, me, 0)` before bumping the epoch and storing `Ready`.
- **Contexts bound every wait.** `Dial` and `Listen` return `ctx.Err()` as soon as their context is done, also inside the claim, the endpoint-liveness watch, and the ring attach (these waits were not bounded by the context before).
- **Safe unlink.** `Close` and `Unlink` remove a name only if it still refers to this endpoint's instance (Linux: same device/inode and `CreatorToken`; macOS/FreeBSD: re-open and compare `CreatorToken`); a missing or replaced name is a no-op returning nil. A failed fresh create removes its name.
- **Linux shared memory** is opened with `O_NOFOLLOW`, must be a regular file, and is allocated with `fallocate` (falls back to `ftruncate` on `EOPNOTSUPP`/`ENOSYS`), so a full tmpfs fails with `ENOSPC` at creation. Windows: an exclusive create fails with `os.ErrExist` when the mapping or any semaphore name exists.
- **Slot completion.** Completion through a copyable handle (`ProducerSlot.Commit`, `ConsumerSlot.Release`, as used by `WriteZeroCopy`, `ReserveWrite`, and `ReserveRead`) is a compare-and-swap from the claimed value: a stale copy neither modifies the ring nor wakes anyone, and `StandardWriteReservation`/`StandardReadReservation` return `ErrInvalidOp` from `Commit`/`Abort`/`Release` and nil from `Buffer`. `Enqueue`/`Dequeue` and the `*ZeroCopyCancel` paths keep plain atomic stores (the claimant is the single owner).
- **Standard reads.** One read call returns bytes of at most one message; unread tails form a FIFO and are never merged. `Read`, `ReadZeroCopy`, and `ReserveRead` now share the same receive helpers (`recvMessage`, `consume`, `classify`), `Close` waits for zero-copy callbacks that serve a tail, and `Close` from inside a zero-copy callback deadlocks (documented).
- **Dispatcher and streams.** The receive path never waits for the tx ring; stream accepts and refusals and the error responses to requests refused by `MaxReassemblyBytes` are sent by the control sender. A stream becomes visible to `AcceptStream` only after its `OpConnAccept` is on the tx ring. `OverflowBlock` for streams waits at receive time for `AcceptStream`.
- An `OpError` from the peer with an unknown error code matches `ErrProtocol`.
- `NamedStandardLink.Unlink`/`NamedAdvancedLink.Unlink` return nil for a missing name.
- The `unsafe.Pointer` open path is a single function (`NewStandardLink` → `openStandardLink`); `createAlignedBuffer` is removed.
- `mpmc`: `T` must not contain Go pointers (documented); `park_darwin.go` documents the private `__ulock_*` interface and `os_sync_wait_on_address` (macOS 14.4+) as the public alternative.
- Documentation (README, PROTOCOL.md, package comment) rewritten for the above; README benchmarks re-measured.

### Fixed

- A stale copy of an already committed or released reservation could complete the slot a second time and corrupt ring state.
- The dispatcher could block on a full tx ring while sending stream accepts or refusals, deadlocking two peers that flood each other.
- `OverflowDrop` answered a stream with an accept that was immediately followed by a close.
- Concurrent partial readers could receive bytes of two messages in one call.
- A queued read tail was still served after `Close`.
- `Close` of a creator removed the name unconditionally, so a segment that somebody else created under the same name after the original was removed or reused could be unlinked.
- On Linux a symbolic link planted under a shm name was followed (names are now opened with `O_NOFOLLOW`).
- A foreign object was claimed and overwritten whenever its state word was `0`; now the whole first page must be zero.
- Waits inside a single `Dial`/`Listen` attempt (claim, liveness watch, ring attach) did not honor the caller's context.
- Test fixes: `TestMuxServe` and `TestConcurrentReadWrite` no longer rely on timing or miscount results.
- `MPMC.tla`: the comment on a failed CAS and the dead `Liveness` definition are fixed or removed; the verification status lists what is not modeled.

### Removed

- All API listed under "Breaking".
- `internal/race`: the `//go:norace` helpers are gone. Payload copies are plain `copy`/`clear`; `go test -race ./...` reports no races without them.
- `mpmc.MPMCInit`, `_RoundUpPowerOf2`.
- Fixed 50 ms wake-latency assertions in `TestNamedLinkBlockedCallsWakePromptly` and the mpmc wake tests (now hang bounds only); CPU-ratio assertions run only with `HQQ_PERF_TESTS=1`.

### Migration

| v0.2.0 | v0.4.1 |
| --- | --- |
| `OpenStandardLink(offset uintptr, n, size)` | removed; `NewStandardLink(mem, n, size, LinkOptions{})` |
| `OpenStandardLinkWithOptions(offset, n, size, opts)` | `NewStandardLink(mem []byte, n, size, opts)` |
| `OpenStandardLinkMemory(mem, n, size, opts)` | `NewStandardLink(mem, n, size, opts)` |
| `NewAdvancedLink(offset, n, size)`, `NewAdvancedLinkWithOptions(offset, n, size, opts)` | `NewAdvancedLink(mem []byte, n, size, opts AdvancedOptions)` |
| `NewAdvancedLinkMemory(mem, n, size, opts)` | `NewAdvancedLink(mem, n, size, opts)` |
| `CreateStandardLink(name, n, size, mode)` | `CreateStandardLink(name, n, size, perm, LinkOptions{})` |
| `CreateStandardLinkWithOptions(name, n, size, mode, opts)` | `CreateStandardLink(name, n, size, perm, opts)` |
| `OpenNamedStandardLink(name, n, size)`, `…WithOptions(…, opts)` | `OpenStandardLink(name, n, size, opts)` |
| `CreateAdvancedLink(name, n, size, mode)` | `CreateAdvancedLink(name, n, size, perm, AdvancedOptions{})` |
| `CreateAdvancedLinkWithOptions(name, n, size, mode, opts)` | `CreateAdvancedLink(name, n, size, perm, opts)` |
| `OpenNamedAdvancedLink(name, n, size)`, `…WithOptions(…, opts)` | `OpenAdvancedLink(name, n, size, opts)` |
| `(*AdvancedLink).Listen(ctx)` | `ListenStreams(ctx)` |
| `(*AdvancedLink).Accept(ctx)` | `AcceptStream(ctx)` |
| `(*AdvancedLink).Dial(ctx)` | `OpenStream(ctx)` |
| — | `(*AdvancedLink).CloseStream(ctx, id)` |
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
| `ReadFrom` = `Read`; address `"hqq-remote"` or the last `WriteTo` address | datagram `ReadFrom` returning the peer's role address (`"hqq-secondary"` on a Primary link, `"hqq-primary"` on a Secondary link) |
| `LocalAddr()` = `"hqq-local"` | `"hqq-primary"` / `"hqq-secondary"` (this endpoint's role) |
| `WriteTo(p, addr)` recorded `addr` | `addr` ignored |
| `CallOn`/`SendLarge` on any non-zero ID | `ErrConnNotFound` unless the ID is an open stream |
| negotiation repeated after failure: `ErrAlreadyNegotiated` | `ErrNegotiationFailed` (terminal) |
| (no equivalent) | `*GeometryError` (matches `ErrInvalidSize`, `errors.As` for `Want`/`Got`) |
| superblock `HQQ_STL2` | `HQQ_STL3`; segments of either version are rejected by the other (`ErrIncompatibleLayout`): recreate segments after upgrading, upgrade both endpoints together |
| `internal/race` helpers | removed |

## v0.2.0 (untagged)

### Highlights

- **Cross-Process Adaptive Parking (`internal/mpmc`)**: Replaced pure busy-spin loops with adaptive waiting (`WaitAdaptive`: tight re-check, `runtime.Gosched`, then cross-process Linux `futex` / macOS `__ulock_wait` / Windows named semaphores for named segments / bounded backoff elsewhere). Idle blocked reader CPU drops from ~165% to **~0.1%**.
- **Superblock v2 (`HQQ_STL2`) & Crash Detection**: Added per-endpoint session tokens, periodic heartbeats (the recorded PID is diagnostic only), graceful close markers (`ErrPeerClosed`), abrupt crash detection (`ErrPeerGone`), reuse of segments whose endpoints all closed or died by `OpenNamed*` / `Dial`, initializer crash takeover, and 3rd-endpoint rejection (`ErrLinkBusy`).
- **Ergonomic High-Level APIs**: Added `hqq.Listen` / `hqq.Dial`, `AdvancedLink.Negotiate`, `AdvancedLink.Serve` + `hqq.Mux` (`Handler` / `HandlerFunc`), `OpenStandardLinkMemory`, `AlignedBuffer`, and standalone `hqq.Unlink`.
- **Zero `go vet` Warnings**: Reworked `internal/mpmc` and `internal/mmap` pointer arithmetic so plain `go vet ./...` (including `unsafeptr`) passes with zero warnings across Linux, macOS, and Windows.
- **TLA+ Model Checking (`internal/mpmc/MPMC.tla`)**: Repaired the TLA+ specification, removed the broken post-module proof stub, and model-checked `NoRace` (writer + reader exclusivity), `DataIntegrity`, `FIFOBound`, `FIFOClaimOrder`, and `SystemProgress` with TLC on a bounded model (`scripts/tlc.sh`, 662,863 states, 0 errors). This is a bounded check, not a proof; parking, `Format`, and crashes are not modeled.

### Added

- `hqq.Listen(ctx, name, cfg)` and `hqq.Dial(ctx, name, cfg)` for one-step named IPC setup and negotiation (`connect.go`).
- `hqq.Mux`, `hqq.Handler`, `hqq.HandlerFunc`, and `(*AdvancedLink).Serve(ctx, handler)` for ergonomic request-response RPC dispatch (`rpc.go`).
- `(*AdvancedLink).Negotiate(ctx, features)` to auto-select initiator/responder roles from `Link.Mode()`, plus simultaneous `NegotiateProtocol` collision resolution (`advanced.go`).
- `OpenStandardLinkMemory(mem []byte, bufferCount, bufferSize int, opts LinkOptions)`, `NewAdvancedLinkMemory(mem, bufferCount, bufferSize, opts AdvancedOptions)`, and `AlignedBuffer(size uintptr) []byte` for safe in-process or custom-mapped links (`standard.go`, `advanced.go`).
- `hqq.Unlink(name string)` and `shm.Unlink(name string)` to remove leftover shared-memory segments without mapping them (`named_link.go`, `internal/shm`).
- `LinkOptions` (`WaitPolicy`, `HeartbeatInterval`, `PeerTimeout`, `AttachTimeout`), `OverflowPolicy` (`OverflowBlock`, `OverflowDrop`, `OverflowFail`), and `PeerState` (`PeerNotConnected`, `PeerConnected`, `PeerClosed`, `PeerGone`) (`options.go`, `superblock.go`).
- `AdvancedOptions.MaxReassemblyBytes` (default `4 × MaxMessageSize`): bounds the memory held by all incomplete multi-chunk messages; a message that would exceed it is refused without allocating (`advanced.go`, `advanced_io.go`).
- Granular sentinel errors (`ErrClosed`, `ErrPeerClosed`, `ErrPeerGone`, `ErrLinkBusy`, `ErrIncompatibleLayout`, `ErrNotNegotiated`, `ErrAlreadyNegotiated`, `ErrNegotiationRejected`, `ErrUnexpectedPacket`, `ErrProtocol`, `ErrMessageAborted`, `ErrAdvancedActive`, `ErrNotListening`, `ErrAlreadyListening`, `ErrIncomingOverflow`, `ErrReassemblyLimit`, `ErrInvalidName`), each wrapping one base (`net.ErrClosed`, `io.EOF`, `os.ErrDeadlineExceeded`, or a legacy HQQ sentinel) for `errors.Is` (`errors.go`; see the PROTOCOL.md error table).
- Windows: cross-process parking for named segments through four named kernel semaphores per segment (`<object name>.hqq-sem0`…`3`), `WaitOnAddress` parking for unnamed memory, `mode` translated to a DACL for the mapping and its semaphores, and `CurrentSize` via `NtQuerySection` so a size mismatch reports `ErrInvalidSize` (`internal/mpmc`, `internal/shm`).
- Multi-process crash/graceful-close/stale-reuse tests (`lifecycle_test.go`), fuzz targets (`fuzz_test.go`, `internal/protocol/fuzz_test.go`), runnable GoDoc examples (`example_test.go`), and Unix domain socket / OS pipe comparison benchmarks (`compare_unix_test.go`).

### Changed

- Split the monolithic 2,448-line `link.go` into focused modules: `errors.go`, `options.go`, `superblock.go`, `standard.go`, `reservation.go`, `packetconn.go`, `advanced.go`, `advanced_io.go`, `advanced_dispatch.go`, `advanced_stream.go`, `named_link.go`, `rpc.go`, and `connect.go`.
- Consolidated 12 duplicated MPMC ring claim loops into unified `claimProducer` and `claimConsumer` paths with 64-byte slot alignment (`slotAlign = 64`) and a 512-byte cache-line-isolated header (`internal/mpmc/mpmc.go`).
- Fixed `SizeMPMCRing` double-counting of 256 header bytes (`internal/mpmc/mpmc.go`).
- Changed Windows default shared-memory namespace from `Global\` (which requires elevated `SeCreateGlobalPrivilege`) to `Local\`, while preserving explicit `Global\` prefix support (`internal/shm/shm_windows.go`).
- Replaced fragile `//go:linkname` bindings to Go runtime internal `internal/race` symbols with self-contained `//go:norace` memory helpers (`internal/race`).
- `StandardLink.Read` returns `io.EOF` itself (instead of `ErrPeerClosed`) after a graceful peer close, as the `io.Reader` contract requires; `io.Copy`, `io.ReadAll`, and `bufio.Scanner` now treat it as a normal end of stream (`standard.go`).
- `AdvancedLink.Dial` allocates even connection IDs on Primary and odd IDs on Secondary (as specified); a peer `OpConnCreate` with ID `0`, the wrong parity, or an ID in use is refused with `ErrCodeInvalidOp` instead of overwriting the existing connection (`advanced_stream.go`).
- Aborted Advanced messages are no longer delivered: `Receive`, `Serve`, and `MessageHandler` drop aborted requests and data messages, and an aborted response fails `Call`/`CallOn` with `ErrMessageAborted` instead of returning an empty success (`advanced_io.go`, `rpc.go`).
- Ring attach failures on a `Ready` segment report `ErrIncompatibleLayout` instead of a bare `ErrFailedInit` (`standard.go`).
- Windows shared-memory names are limited so that the mapping name plus the 9-character semaphore suffix fits 259 UTF-16 units (`ErrInvalidName` otherwise).

### Fixed

- Fixed data races in `StandardLink` (`readDeadline`, `writeDeadline`, `remoteAddr`, `recvRemainder`, and `Close` vs in-flight ring operations).
- Fixed `NamedStandardLink.Close` and `NamedAdvancedLink.Close` unmapping shared memory before draining in-flight readers/writers (`SIGSEGV` hazard).
- Fixed `SIGBUS` hazard when opening a named shared memory segment before the creator finished `ftruncate` (`internal/shm`).
- Fixed shared-memory name traversal (`../`, slashes, NUL, and macOS 31-byte `PSHMNAMLEN` validation) and added `O_CLOEXEC` (`internal/shm`).
- Fixed `AdvancedLink` dispatcher failing permanently when a late response arrives for a context-canceled `Call` (`advanced_dispatch.go`).
- Fixed `StandardLink.Read` / `ReadZeroCopy` / `ReserveRead` silently corrupting state if called on `OpConnCopy` frames (`standard.go`, `reservation.go`).
- Fixed `Dial` retrying forever on permanent `ErrFailedInit` variants such as `ErrIncompatibleLayout`; it now retries only while the name is missing or the listener is still sizing/initializing the segment (`connect.go`).
- Fixed a data race (double unmap / double handle close) when `Close` or `Unlink` was called concurrently on a `NamedStandardLink` / `NamedAdvancedLink` (`named_link.go`).
- Fixed a race that could recycle a `Call` result channel still holding a stale result into the global pool, delivering one call's error to an unrelated call (`advanced_io.go`, `advanced_dispatch.go`).
- Fixed unbounded growth of incomplete reassembly buffers when a sender's abort frame was never delivered (`advanced_io.go`).
- Fixed a context wake-up callback that could touch a ring's wait word after the operation returned and the mapping was unmapped (`internal/mpmc`).
- Fixed a panicking zero-copy callback wedging the ring permanently: `WriteZeroCopy` publishes a tombstone, `ReadZeroCopy` and the mpmc dequeue release the slot, and the mpmc enqueue publishes a zeroed element before the panic propagates (`standard.go`, `internal/mpmc`).
- Fixed the CI fuzz step, which named non-existent fuzz targets and therefore fuzzed nothing (`.github/workflows/ci.yml`).
