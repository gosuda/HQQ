# Specification Audit Findings and Resolution Log

**Scope**: Baseline audit of the v0.5.0 codebase (`superblock.go`, `named_link.go`, `standard.go`, `internal/mpmc`, `internal/protocol`, `internal/shm`, `advanced*.go`, `stream.go`, `rpc.go`, `message.go`, `connect.go`) conducted while authoring the formal protocol specification ([`SPEC.md`](SPEC.md) and [`MEMORY_MODEL.md`](MEMORY_MODEL.md)).

Each finding is classified into one of three categories:

- **Class (a) Documentation only**: The implementation behavior is sound, and the specification pins the exact contract where earlier prose or comments were incomplete.
- **Class (b) Behavioral fix without ABI change**: Can be fixed in any implementation without altering a single byte of the shared-memory layout or wire format.
- **Class (c) Layout or wire format change**: Would require modifying `HQQ_STL3` or ring layout 2.

**Outcome**: Zero findings required a breaking layout or wire change (Class c), and all five Class (b) behavioral findings (`F-01`, `F-02`, `F-04`, `F-07`, `F-09`, `F-10`) have been resolved or mitigated and verified by regression tests.

## 1. Detailed Findings

Each finding below records the original defect or ambiguity, its impact, the recommended resolution, and its current verification status in the repository.

### F-01 (Class b): Stalled Initializer Resuming After Takeover

`openStandardLink` (`standard.go`) verifies `initToken` ownership before each ring `Format` slice and before each zeroing slice (`initSession.check` in `superblock.go`), while a waiting creator or in-memory caller can take over a stalled initialization after `initStale` (default 5 s, minimum 2 s) without progress. Because the ownership check and the subsequent memory store are not a single atomic transaction, an initializer process that is suspended by the OS (`SIGSTOP`, debugger breakpoint, or VM pause) for longer than `initStale` right after passing an ownership check could resume and overwrite a single slice before noticing that `initToken` changed. The final `cas(initToken, me -> 0)` always fails for the suspended initializer, preventing it from re-publishing `Ready`.

**Impact**: Reachable only if a process is paused for multiple seconds mid-initialization (not reachable via process crash).

**Resolution (mitigated and tested)**: `publishSuperblock` checks `initToken` ownership before every superblock publish step (`P1` through `P5`) in addition to checking before every 1024-slot `Format` slice (`FormatSlice`) and every 256 KiB zeroing slice (`zeroSlice`). This bounds the post-check write window to microseconds; the residual non-atomic single-slice window is documented normatively in `SPEC.md` section 5.3. Covered by `TestPublishStopsAtTakeover` and `TestInitializerStopsWithinOneSlice`.

### F-02 (Class b): Windows Semaphore Token Accumulation on Stale Waiter Count

When a consumer or producer process on Windows is killed with `TerminateProcess` while parked, its increment on `emptyWaiters` or `fullWaiters` remains non-zero until the segment is re-formatted. Previously, `Wake(side, n)` passed `n = load(waiters)` to `ReleaseSemaphore`, adding `n >= 1` tokens on every subsequent publish or release even when no process was parked. If the Windows mapping name was later reused while a handle remained open, the new session's waiters could experience a burst of spurious wakes while draining accumulated semaphore tokens.

**Resolution (fixed)**: Wakers now release at most `1` token per published or released slot (`Wake(side, 1)`), using the waiter count only as a boolean indicator (`waiters != 0`), while `WakeAll` continues to release the full count on link teardown. Covered by `TestStaleWaiterCountGrantsOneTokenPerSlot`.

### F-03 (Class a): Documentation Clarifications Applied to `SPEC.md` and `PROTOCOL.md`

1. `OpProtoNegotiate` and `OpProtoAck` operand 2 is reserved: senders `MUST` write `0` and receivers `MUST` ignore the word (`SPEC.md` section 9.2).
2. `OpConnCreate` operand 1 and `OpConnClose` operand 1 are reserved (`0` on send, ignored on receive; `SPEC.md` section 9.2).
3. Reserved upper bits `8..63` of packet word 0 are written as `0` by senders and masked off (`& 0xFF`) by receivers (`SPEC.md` section 9.1).
4. Windows kernel-object naming rules (case-insensitive `Local\` and `Global\` prefix canonicalization, rejection of bare `Local\`/`Global\`, and the 259 UTF-16 code-unit budget including `.hqq-semN`) are specified in `SPEC.md` section 11.1 and tested by `conformance/` group `G-05`.
5. The macOS `shm_open` 31-byte total path limit (30-byte segment name after the leading `/`) is specified in `SPEC.md` section 11.1.
6. The stalled-initializer deadline extension rule (`limit = max(deadline, watch.since + initStale)` when the watch began before `deadline`) is specified in `SPEC.md` section 5.1.
7. Ring waiter counters (`emptyWaiters`, `fullWaiters`) are defined at the protocol level as counting **parked processes** (at most one registration per process per direction) while permitting implementations to over-count at the cost of harmless spurious wakes (`SPEC.md` section 8.1).

**Status**: All seven clarifications are incorporated into `SPEC.md`, `PROTOCOL.md`, and code comments.

### F-04 (Class b): Future-Dated Heartbeat Timestamp Bypassing the Liveness Watch

Previously, `endpointAliveAtOpen` checked `wallAge(hb) < effectivePeerTimeout(...)`. If the system wall clock stepped backward (for example, via an NTP adjustment or VM snapshot restore) after an endpoint wrote a heartbeat, `wallAge(hb)` became negative (`< timeout`), causing `endpointAliveAtOpen` to classify a dead endpoint as immediately alive without running the monotonic progress-confirmation watch.

**Resolution (fixed)**: `heartbeatFresh` now treats a heartbeat timestamp as immediately fresh only when `0 <= age < timeout` (with up to `heartbeatFutureSlack = 1s` of negative-age tolerance for minor clock skew); any timestamp further in the future falls through to the monotonic progress-confirmation watch, which checks whether `heartbeat` or `token` actually advances. Covered by `TestEndpointAliveAtOpenFutureHeartbeat` and `TestClaimReusesSegmentWithFutureHeartbeat`.

### F-05 (Class a): `Negotiate(0)` Versus `NegotiateProtocol(version, 0)`

At the high-level API, `Negotiate(ctx, 0)` defaults `0` to all supported features (`FeaturesAll`), whereas `NegotiateProtocol(ctx, version, 0)` transmits `requestedFeatures = 0` literally on the wire, negotiating zero active features. `SPEC.md` section 10.1 documents the wire rule (`NegotiateProtocol`), and high-level API docs state the `Negotiate(0)` convenience default.

### F-06 (Class a): Receiver Leniency on `Abort` Frames

In `consumeAdvancedChunk`, any frame with the `Abort` flag set is processed as an immediate message abort before evaluating `TotalSize`, `ChunkOffset`, `ChunkSize`, `Begin`, `End`, or `aux` bounds (`SPEC.md` section 10.5 step 2). While senders always emit `0` for those fields on an abort frame, `SPEC.md` section 10.5 makes the receiver's leniency normative so all implementations accept the same frame matrix (`conformance/` group `G-10`).

### F-07 (Class b): Eviction From the Refused-Message Set

An Advanced receiver tracks up to `maxRejectedMessages` (1024) recently refused multi-chunk message keys so subsequent continuation chunks of a refused message are silently drained. Previously, if a peer interleaved more than 1024 refused multi-chunk messages concurrently, evicting an older key from the bounded set caused its next continuation chunk to fail the entire receive loop with `ErrProtocol`.

**Resolution (fixed)**: Once at least one entry has been evicted from the bounded refused-message set (`rejectedEvicted`), continuation chunks (`!Begin`) for unknown keys are silently drained rather than faulting the link with `ErrProtocol`. Covered by `TestAdvancedReassemblyLimitDefaultAndRejectedBound`.

### F-08 (Class a): Forward-Compatibility Rule for Reserved Packet Words

Senders `MUST` zero all unused operand words and bits `8..63` of packet word 0, and receivers `MUST` ignore them (`SPEC.md` sections 9.1 and 9.2). Protocol 1.1 (`spec/ext/PROTOCOL_1_1_DRAFT.md`) relies on this invariant for request metadata and cancellation frames.

### F-09 (Class b): Reclaiming Abandoned Multi-Chunk Reassemblies

If a buggy or stalled peer begins a multi-chunk message (`Begin` without `End`) and stops sending continuation chunks without emitting an `Abort` frame while continuing to heartbeat, that incomplete assembly would otherwise hold its `TotalSize + 256` byte reservation against `MaxReassemblyBytes` indefinitely.

**Resolution (fixed)**: An incomplete assembly that receives no chunk for `advancedAssemblyIdleChunks` (`65536`) incoming chunks is reclaimed whenever a new multi-chunk message would otherwise exceed `MaxReassemblyBytes`: its memory budget is freed, any late continuation chunks are drained, and an abandoned response fails its caller with `ErrReassemblyLimit` (an abandoned request receives a refusal error response). Measuring staleness in received chunks rather than wall-clock time ensures a legitimately idle link never expires an assembly prematurely. Covered by `TestAdvancedIdleAssemblyIsReclaimed`.

### F-10 (Class b): Peer Liveness Check on Send When Heartbeats Are Disabled

When heartbeats are disabled (`HeartbeatInterval < 0`), no background heartbeat goroutine runs `checkPeer`. Previously, non-blocking `Write` calls into a ring with free slots only read the cached peer status word and did not call `checkPeer` until the ring filled and blocked.

**Resolution (fixed)**: When heartbeats are disabled on a link, `enterTx(true)` invokes `checkPeer` before every send operation so peer closure or death is detected immediately. Links with heartbeats enabled keep the zero-overhead cached read on the send fast path. Covered by `TestSendNoticesPeerCloseWithoutHeartbeat`.

### F-11 (Class a): `initStaleWindow` Uses the Claimer's `PeerTimeout`

The stale-initializer takeover threshold (`initStaleWindow = max(opts.PeerTimeout, 2s)`) is evaluated using the claiming endpoint's `PeerTimeout` option (default 5 s, floor 2 s) rather than a value stored in shared memory (`SPEC.md` section 5.1).

### F-12 (Class a): Cross-Platform Segment Name Length Policy

Because macOS `shm_open` limits the kernel name (including the leading `/`) to 31 bytes (leaving 30 bytes for the HQQ segment name), cross-platform applications and tests `SHOULD` use segment names of at most 30 ASCII characters from `[A-Za-z0-9._-]`. All implementations `MUST` enforce the platform-independent 255-byte and character-set validation rules in `SPEC.md` section 11.1 in addition to host OS limits.

### F-13 (Class a): macOS Shared-Memory Parking (`__ulock_wait` / `os_sync_wait_on_address`)

On macOS, the Go runtime and `libhqq` park on shared-memory 32-bit wake words using Darwin's shared-address ulock queue (`__ulock_wait` / `__ulock_wake` with `ULF_WAKE_ALL`, or `os_sync_wait_on_address` with `OS_SYNC_WAIT_ON_ADDRESS_SHARED`). Cross-process wakeups between Go and C/C++/Rust on macOS are verified continuously by scenario `X-12` in the interop matrix, and every wait is bounded by the 100 ms poll fallback (`SPEC.md` section 8.3).

### F-14 (Class a): Slot Stride and Packet Offset Alignment

Each ring slot is 128 bytes (`seq` of 8 bytes at offset 0, followed by the 64-byte packet at offset 8, followed by 56 bytes of padding). Because the 64-byte packet begins at offset 8, its final 8-byte word crosses into the second 64-byte cache line of the 128-byte slot. Both cache lines are part of the exclusive slot stride, and keeping offset 8 preserves ABI compatibility across `HQQ_STL3` / ring layout 2. Implementations `MUST` access packet words using 8-byte alignment or `memcpy` (`MEMORY_MODEL.md` section 6).

### F-15 (Class a): Single Secondary Attachment Per Segment Epoch

Once the Secondary endpoint record has a non-zero `token` (whether attached, gracefully detached with `token == 1`, or dead), no new Secondary can attach until the segment is re-initialized at a new epoch (`SPEC.md` section 5.2). This prevents a new Secondary from racing with ring slots still owned by a crashed predecessor (`SPEC.md` section 7.4). Multi-client servers (such as `Acceptor` in `spec/ext/LISTENER_DRAFT.md` and `hqq-bindgen` servers in `spec/toolchain/BINDGEN.md`) support arbitrary client churn by assigning each client session its own dedicated Standard or Advanced link segment.

### F-16 (Class a): Superblock Fields During `Ready -> Initializing` Reuse

When a creator reclaims a dead `Ready` segment (`Ready -> Initializing`), the previous session's `magic`, `pageSize`, geometry, and endpoint records remain in memory until `publishSuperblock` overwrites them prior to the commit CAS. Observers `MUST NOT` trust geometry or endpoint records while `state != Ready` (`SPEC.md` sections 3.1 and 5.3).

## 2. Verified Invariants Requiring No Change

The audit also verified the following six protocol invariants and confirmed they are race-free as designed:

- **Epoch increment and `Ready` publication ordering (`commitInit`)**: `epoch` is incremented after the `initToken` commit CAS and before storing `state = Ready` with release ordering. If a creator crashes between the `initToken` CAS and the `state = Ready` store, the segment remains in `Initializing` with `initToken == 0` and the next creator cleanly takes over after `initStaleWindow`.
- **Ring `Format` publication order**: Zeroing `magic` and `flag` first and storing `flag = 2` (`ringReadyFlag`) last with release ordering guarantees that a concurrent `Attach` never observes a partially formatted ring.
- **Dekker wait-word snapshot window**: Sampling `notEmpty` / `notFull` (`W2`) with acquire ordering between `W1` (`emptyWaiters.fetch_add(1)` with `seq_cst`) and `W3` (`seq[s].load()` with `seq_cst`) closes both the store-buffering window and the pre-futex-sleep race (`MEMORY_MODEL.md` section 4.2).
- **64-bit sequence wraparound**: Signed difference comparisons `int64(seq - pos)` remain exact for all valid ring sizes (`bufferCount <= 2^24`) across `2^64` wraparound (`TestWraparoundNearMaxUint64`).
- **Reservation completion CAS**: `CompletionCAS` on reservation handles is purely a local caller-safety check and has no effect on wire state.
- **Post-close ring drain**: Retrying one non-blocking dequeue after observing `ErrPeerClosed` or `ErrPeerGone` (`dequeueDrained`), combined with the release ordering of the detach CAS (`token -> 1`), guarantees that every frame published by a peer before closing is delivered before `EOF` is returned.

## 3. Design Candidates Evaluated Against Layout Changes

The following table summarizes candidates that were evaluated and confirmed not to require a layout or wire change:

| Candidate | Rationale for Keeping Current Layout |
| --- | --- |
| Slot packet offset (`F-14`) | Moving the packet from offset 8 to offset 64 still touches two 64-byte cache lines per slot transfer; keeping offset 8 preserves the `HQQ_STL3` / ring layout 2 ABI. |
| Wall-clock `UnixNano` in heartbeat words | Heartbeat timestamps are used only as an open-time freshness hint (guarded by `heartbeatFresh` and confirmed by a local monotonic watch), whereas `CLOCK_MONOTONIC` epoch origins are not portable across OS kernels or Windows processes. |
| 32-bit wake words and waiter counts | Linux `futex`, macOS `__ulock_wait`, and Windows `WaitOnAddress` operate natively on 32-bit words; `2^32` simultaneous parked processes is unreachable, and wake words only require inequality comparison. |
| Diagnostic-only `pid` field | PID namespaces in containers make cross-process PID liveness checks unreliable; token + heartbeat monitoring works across container boundaries that share a mount or IPC namespace. |
| Three independent version numbers | Segment layout (`major`/`minor` 3.0), ring layout (`version` 2), and wire protocol (`1.1`) evolve independently without coupling. |
| Little-endian memory layout | Every target architecture (x86-64, AArch64, RISC-V, modern POWER le) is little-endian, and native 64-bit atomic RMW operations require native byte order (`MEMORY_MODEL.md` section 7). |
| Host page-size alignment | Shared memory is purely local to a single host kernel, and `pageSize` in the superblock rejects cross-page-size mismatches deterministically. |
| Deterministic Windows semaphore names | Deriving `.hqq-sem0..3` from the mapping name avoids storing kernel handle values in shared memory. |
