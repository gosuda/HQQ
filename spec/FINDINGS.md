# Spec defects and ambiguities found while writing the baseline specification

Scope: code at v0.5.0 (`git HEAD 817b04c`): `superblock.go`, `named_link.go`, `standard.go`, `internal/mpmc`, `internal/protocol`, `internal/shm`, `advanced*.go`, `stream.go`, `rpc.go`, `message.go`, `connect.go`.

Classification:

* **(a)** documentation only: the code is fine, the text (code comment, `PROTOCOL.md`, README) is missing or inaccurate, or the behavior is accidental but harmless and now pinned by the spec.
* **(b)** behavior fix without ABI change: can be fixed in any implementation at any time without touching a byte of the layout or the wire format; a second implementation SHOULD implement the corrected behavior from the start.
* **(c)** needs a layout or wire change **before** a second implementation exists.

**Result: no finding of class (c) was forced.** Items that look like candidates but were examined and *not* classified (c) are listed in section 3 so that the parent can overrule that judgement before the C library freezes the ABI.

## 1. Findings

### F-01 (b) — A stalled initializer can resume and write into a segment that has since become `Ready`

`openStandardLink` (`standard.go:184-198`) checks `initToken` ownership **before** each ring `Format` and before each 8 MiB zeroing chunk (`initSession.check`, `superblock.go:585-593`), but the check and the write are not atomic. A creator/in-memory endpoint takes the initialization over after `initStale` (default 5 s, minimum 2 s) without progress (`superblock.go:457-469`). If the original initializer is merely *frozen* (SIGSTOP, debugger, VM pause, swap storm) for longer than that window and resumes **inside** a `Format`/`clear` call that it entered legitimately, it keeps writing: it can reset slot sequences, cursors and zero payload of rings that the new initializer has already published and that the two endpoints are already using. The final `cas(initToken, me → 0)` fails (so `Ready` is not re-published by the loser), but the damage is done.

Impact: needs a pause of at least the stale window precisely during init; the window of exposure per ring is one `Format` call or one 8 MiB `memset`. Not reachable by a crash, only by a hang.

Recommendation: keep the window large; bound the write granularity (`zeroChunk` of 1 MiB, `Format` in slot-count slices with a check between slices) so that the exposure is microseconds; document that initializers must not be suspended. A complete fix would need a generation number in the ring header (ABI), which is not worth it. Class (b) because the mitigation is behavioral. SPEC 5.3 requires the pre-step checks; MEMORY_MODEL 5 refers here.

### F-02 (b) — Windows semaphore tokens: `Release(n)` with a stale waiter count

`mpmc.waitSite.wake` with a Parker calls `Wake(side, n)` with `n = load(waiters)` (`internal/mpmc/mpmc.go:420-425`), and `shm.semaphore.Release` adds `n` tokens (`internal/shm/shm_windows.go:219-227`, max `2^31-1`). A process killed while parked leaves its count raised until `Format`; every later publish/release then adds `n >= 1` tokens to a semaphore nobody waits on. If the same Windows name is **reused** (the creator opens the existing mapping and its semaphores and re-initializes — SPEC 11.5), the new session's waiters inherit up to millions of leftover tokens and return from `Park` immediately many times (spurious wakes: re-check, not loss of correctness, but a burst of CPU).

Recommendation: `Release(1)` per wake per side suffices because the specified topology has at most one parked process per side; and/or drain the four semaphores at `Format` time of a reused mapping. A port SHOULD cap `n` to 1. Class (b).

### F-03 (a) — Documentation inaccuracies found

1. `internal/protocol/protocol.go` documents `OpProtoNegotiate`/`OpProtoAck` operand 2 as "Supported/Negotiated capability flags". The code always sends `0` and ignores the received value (`advanced.go` negotiation: `protocol.NewPacket(OpProtoNegotiate, version, features, 0)`); `PROTOCOL.md` is right ("always 0, ignored"). The word is **reserved**; ports MUST send 0 and MUST ignore it (SPEC 9.2).
2. `OpConnCreate` operand 1 is called "flags (0)" in `PROTOCOL.md`; it is never written non-zero and never read. `OpConnClose` operand 1 "reason" is written as `0` and never interpreted. Both are reserved (F-08).
3. `PROTOCOL.md` states reserved bits of packet word 0 "are written as zero"; it does not say receivers ignore them. The code masks `& 0xFF` (`protocol.Packet.Op`), so receivers ignore them. SPEC 9.1 makes "ignore on receive" normative for forward compatibility.
4. `PROTOCOL.md` describes the Windows `Local\` default and `Global\` prefix, but not that the prefix match is ASCII case-insensitive and canonicalizes the spelling (`global\x` → `Global\x`), that `Global\`/`Local\` alone is invalid, and the exact length budget (259 UTF-16 units including the longest `.hqq-semN` suffix). SPEC 11.1 and `platform.windows.name_vectors` state them.
5. The macOS limit (the **whole** name including the leading slash is at most 31 bytes, so 30 bytes of name) appears only in a code comment (`shm_bsd.go`), not in `PROTOCOL.md`. SPEC 11.1.
6. `PROTOCOL.md`'s claim-loop text says a stalled initializer is "given its whole window even if that runs past `AttachTimeout`"; the precise rule (`limit = max(deadline, watch.since + initStale)` only if the watch began before `deadline`, `superblock.go:530-538`) is in SPEC 5.1.
7. The Go code comments in `mpmc.go` state the waiter count "counts registered waiters ... with waiter aggregation at most one per process"; the protocol-level definition is "parked processes". SPEC 8.1 defines it as processes and allows a port to over-count.

### F-04 (b) — A heartbeat timestamp in the future makes a dead endpoint look alive

`endpointAliveAtOpen` (`superblock.go:253`) tests `wallAge(hb) < effectivePeerTimeout(...)`. `wallAge` is negative when the wall clock was stepped **backwards** after the last heartbeat (NTP step, VM restore, manual change) or when the heartbeat was written by a host with a clock ahead. A negative age is `< timeout`, so the record is judged alive **without** the progress-confirmation watch. Result: a creator gets `ErrLinkBusy` and an opener joins a dead Primary until the wall clock passes the stale value (potentially hours). The check was designed to be robust against wall-clock jumps (the confirmation watch) but is not in this direction.

Recommendation: treat `age < 0` like "stale" and fall through to the progress watch (`sleep(step)` until `heartbeat`/`token` change or the window ends). One-line fix, no ABI change. SPEC 6.2 pseudocode documents the current behavior and calls this out; ports should implement the fix (an implementation that does is still conforming: it only turns a false "alive" into a false-negative-free "confirm by watching").

### F-05 (a) — `Negotiate(0)` versus `NegotiateProtocol(version, 0)`

`Negotiate(ctx, 0)` requests *all* supported features; `NegotiateProtocol(ctx, v, 0)` sends `requestedFeatures = 0` and (because the initiator accepts only a subset of what it asked for) negotiates **no** features. Both are valid wire behavior; the second leaves a link on which `Send`/`Call` fail with `ErrNotNegotiated`. SPEC 10.1 describes the wire rule only; a port's high-level `Negotiate(0)` SHOULD mean "everything supported".

### F-06 (a) — Abort frames: leniency of the receiver is the contract

`consumeAdvancedChunk` treats any frame with `Abort` set as an abort **before** any bounds check: `TotalSize`, `ChunkOffset`, `ChunkSize`, `Begin`, `End` and `aux` are ignored (`advanced_io.go` abort branch, SPEC 10.5 step 2). The sender always sends `0, 0, 0` and no `Begin`/`End`, which `PROTOCOL.md` documents; the receiver's leniency is undocumented. A strict port would reject frames that Go accepts. SPEC 10.5 requires acceptance.

### F-07 (b) — Eviction from the refused-message set turns into a fatal protocol error

At most 1024 refused keys are remembered (`maxRejectedMessages`). If more than 1024 newer refusals push a key out while its chunks are still arriving, the next chunk of that message finds no assembly and no refused entry and fails the **whole receive path** with `ErrProtocol` (`advanced_io.go` `rejectMessage`, documented in a code comment only). A peer can trigger this by interleaving many oversized multi-chunk messages. Recommendation: either never evict (bound the set by messages in flight instead) or treat a non-`Begin` chunk with an unknown key after a recent refusal as "drain" rather than fatal. Class (b).

### F-08 (a) — Reserved operands

The following operand words are reserved: `OpConnCreate` operand 1, `OpConnClose` operand 1 (always sent as 0), `OpProtoNegotiate`/`OpProtoAck` operand 2, all operands above those listed for every opcode, and bits 8..63 of word 0. Senders write zeros, receivers ignore them (SPEC 9.1/9.2). This is the forward-compatibility rule that the 1.1 extension work can rely on.

### F-09 (b) — No timeout for a message whose `End` chunk never arrives

A multi-chunk message whose `End` chunk never arrives (sender died after chunk 1 without the abort frame, a buggy peer, or a deliberate one) keeps its assembly buffer and its `TotalSize + 256` charge **until the link ends**. A peer can therefore exhaust `MaxReassemblyBytes` (default 256 MiB) by starting messages it never finishes, causing later large messages to be refused. Peer death is detected by the heartbeat, but a live buggy peer is not. Recommendation: an idle timeout per assembly (for example the peer timeout) or a hard limit on the number of concurrent assemblies. Class (b).

### F-10 (b) — An endpoint with heartbeats disabled does not learn about the peer's failure unless it blocks

The peer monitor (`checkPeer`) is run by the heartbeat goroutine (only started when `HeartbeatInterval > 0`, `standard.go` end of `openStandardLink`), by the `Check` hook of a **blocked** ring operation, and once at Secondary join. `enterTx(true)` only reads the recorded status. An endpoint with `HeartbeatInterval < 0` therefore keeps successfully publishing into a ring with free slots after its peer closed or died, until a call blocks. SPEC 6.4 makes the refresh strategy **[IMPL]**; a port SHOULD run `checkPeer` also at the start of each send call or at least every N sends.

### F-11 (a) — The initialization-stale window is the claimer's own `PeerTimeout`

`initStaleWindow` is `max(opts.PeerTimeout, 2 s)` of **the endpoint that wants to take over** (`superblock.go:337-342`), not a value in shared memory. A creator configured with a short timeout takes over a slower initializer sooner (minimum 2 s). This is deliberate, but it is a cross-implementation parameter: ports SHOULD use the same default (5 s) and the same 2 s floor, and SPEC 5.1 states the rule.

### F-12 (a) — Naming limits differ per platform; a portable name policy is needed by ports

Names that are valid on Linux (255 bytes) are invalid on macOS (30 bytes) and constrained on Windows (244 characters with the default prefix). `Global\` on Windows needs `SeCreateGlobalPrivilege`. A cross-platform application (and the planned process manager/acceptor) SHOULD generate names of at most 30 characters from `[A-Za-z0-9._-]`. A port MUST apply the validation of SPEC 11.1 on every platform, including the platform-independent 255-byte and character rules, so that a name accepted on one platform is not silently different on another.

### F-13 (a) — macOS parking uses private system calls

`__ulock_wait`/`__ulock_wake` are private Darwin interfaces invoked as raw syscalls (`park_darwin.go:22-31`); errors are ignored, so parking degrades to bounded polling if they ever change. For a C port the public equivalents (`os_sync_wait_on_address` / `os_sync_wake_by_address_all` with the SHARED flag, macOS 14.4+) are preferable. **[INFERENCE]** that a waiter using `os_sync_wait_on_address(..., OS_SYNC_WAIT_ON_ADDRESS_SHARED)` is woken by the Go side's `ULF_WAKE_ALL` on the same address and vice versa (both are built on the same shared-address ulock queue); this MUST be verified by the interop matrix before the C library relies on it. Because every wait is bounded by 100 ms, a mismatch costs latency only.

### F-14 (c, optional performance, no change recommended) — Packet placement in the slot

A slot is `seq` (8 bytes) + packet (64 bytes) at offset 8, padded to a 128-byte stride. The packet therefore starts 8 bytes into a 64-byte line and its last word spills into the next line; 56 of 128 bytes per slot are padding. A layout that places the packet at slot offset 64 (seq in line 0, packet on line 1) or a 64-byte packet followed by an 8-byte seq in a 128-byte slot would need the same two lines but align the packet to 64 bytes, simplifying SIMD copies. The change alters `Layout version 2`. **Not recommended**: the cost is one extra line either way, the ABI is frozen at `HQQ_STL3`/ring layout 2, and nothing in the protocol depends on packet alignment. Listed because this is the last cheap moment to change it, before a second implementation exists.

### F-15 (design constraint; (c) only if a requirement appears) — No re-join of an endpoint in the same session

Once the Secondary record has a non-zero token (attached, detached, or dead) nobody can join as Secondary until the segment is re-initialized (`superblock.go:513-524`); a Secondary that crashed cannot reconnect to the surviving Primary (`ErrLinkBusy`), and a Primary that crashed cannot be replaced while the Secondary lives (the Secondary must close first). This is intentional (a dead endpoint may still own claimed ring slots, see SPEC 7.4 head-of-line) but constrains reconnect designs (the multi-client acceptor, process manager). If "reconnect to the same segment" ever becomes a requirement it needs a **layout change** (an owner generation per endpoint, a ring ownership-recovery protocol). The Go acceptor in the working tree (`listener.go`) avoids the issue by giving every client its own link segment (the client is the Primary of a fresh segment and announces it in a connect ring), so nothing in the current spec requires a change.

### F-16 (a) — Footnote on `State`/`Magic` after a reuse

During a reuse (`Ready → Initializing`) the superblock keeps the previous session's `magic`, `pageSize`, geometry and endpoint records until publish rewrites them; only `state`, `initToken`, `creatorToken`, `major/minor/requiredFeatures` change at claim. Observers MUST therefore not interpret geometry or records while `state != Ready`. SPEC 3.1/5.3 state when each word is valid.

## 2. Confirmed non-defects (checked, nothing to fix)

* **Epoch and `Ready` ordering** (`commitInit`): epoch is incremented *after* the commit CAS and before `state = Ready`; a crash between the CAS and the state store leaves `state = Initializing` with `initToken = 0` and a valid layout; the next creator takes it over via `beginInit(expected = 0)` after the stale window. No state is lost.
* **`Format` ordering** (`flag` last, `magic` zeroed first): a concurrent `Attach` never accepts a half-built ring.
* **Wait-word snapshot window**: the futex word is read after the waiter registration and before the re-check; the waker changes it after having seen the registration (SPEC 8, MEMORY_MODEL 4). Sound under SC; unsound under release/acquire only (litmus).
* **Position wraparound**: `int64(seq - pos)` comparisons are correct for `N <= 2^24` (tested near `2^64` in `internal/mpmc/spec_constants_test.go`).
* **`CompletionCAS` on copied reservations**: a local API-safety feature; irrelevant to the wire.
* **Drain rule after peer failure**: delivers every published packet before reporting `ErrPeerClosed`/`ErrPeerGone` because each failing dequeue is retried once non-blocking (`dequeueDrained`).

## 3. Examined and not classified (c)

| Candidate | Why it is not (c) |
| --- | --- |
| Slot waste / packet alignment (F-14) | Pure performance; one extra cache line per slot transfer either way. |
| Wall-clock `UnixNano` in heartbeat words | Wall-clock is only used for the open-time freshness hint and is confirmed by a local-monotonic watch; monotonic timestamps cannot be shared across processes portably (`CLOCK_MONOTONIC` is system-wide on Linux and macOS but not documented as such on Windows). |
| 32-bit wake words and waiter counts | 2^32 simultaneous registered waiters or a wrap of the wake word is impossible in practice; the futex compare only needs inequality. |
| `pid` is diagnostic only | Correct; PID namespaces make it non-comparable. |
| Single `Major` for the whole layout | The ring header has its own version (2) and the protocol its own (1.0); three independent versions are enough. |
| Big-endian | Explicitly unsupported (MEMORY_MODEL 7). A format change (byte-order mark in the superblock) is not needed unless a big-endian target is wanted. |
| Segments are not portable across page sizes | Detected by `pageSize` in the geometry comparison; files are never shared between machines. |
| Windows semaphore names outside the segment | Names are derived deterministically from the mapping's name; no layout change needed. |
| No "closed" flag in the ring header | Closure is derived from the superblock tokens; the protocol-level shutdown for Advanced is a 1.1 frame (wire addition under a minor version, handled by negotiation). |

## 4. Notes for the parent

* Two behavior fixes (F-02, F-04) are small enough to apply to the Go implementation before the C library mirrors it; F-04 changes a liveness decision (a creator may succeed where it used to get `ErrLinkBusy`).
* The spec pins behavior that is accidental but harmless (F-06, F-10, F-11); changing any of it later would be a compatibility decision, not a bug fix.
