# Changelog

All notable changes to **HQQ** (`gosuda.org/hqq`) are documented in this file.

The project follows [Semantic Versioning](https://semver.org/). While in `v0.x`, minor versions may introduce breaking wire-layout improvements accompanied by migration notes below.

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
