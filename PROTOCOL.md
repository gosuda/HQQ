# HQQ Protocol Specification

## Scope

This document defines the HQQ shared-memory protocol family:

- **Standard Protocol 1.0**: the [`StandardLink`](standard.go) data path. It provides the fixed 64-byte packet ABI, bidirectional lock-free MPMC rings, ring-slot-owned payload buffers, tombstones, endpoint liveness/crash detection, and the basic `Read`/`Write`/zero-copy/reservation APIs.
- **Advanced Protocol 1.0**: an optional negotiated overlay on top of the Standard Protocol. It keeps the same shared-memory layout and packet ABI, and adds multi-chunk messages, concurrent request-response IPC, and logical stream multiplexing.

The Advanced Protocol preserves Standard Protocol ownership rules: every payload byte lives in the same-index buffer owned by the claimed data-ring slot, and every reader releases the claimed slot promptly.

Three versions are independent:

| Version | Value | Where it lives |
| --- | --- | --- |
| **Segment layout** | 3.0 (`major = 3`, `minor = 0`), magic `HQQ_STL3` | superblock `Magic`, `Major`, `Minor`; a different magic or major is `ErrIncompatibleLayout` |
| **MPMC ring layout** | 2 | ring header `layout version` word; an attacher rejects another value |
| **Protocol** (Standard and Advanced) | 1.0 | `OpProtoNegotiate` / `OpProtoAck` operand 0, `(major << 8) \| minor` |

## Shared Design Goals

1. **Fixed ABI**: packets have a stable 64-byte layout independent of Go struct padding or `uintptr` width.
2. **Little-endian encoding**: all packet words are encoded with `binary.LittleEndian`.
3. **Lock-free ownership transfer**: data ring slots own same-index payload buffers; no mutex is required on the `StandardLink` hot path.
4. **No unread-buffer overwrite**: a payload buffer is recycled only after the receiver releases the owning ring slot.
5. **Bounded memory**: all rings and payload buffers are allocated up front.
6. **Composable dispatch**: automatic copy (`Read`/`Write`), zero-copy callbacks (`ReadZeroCopy`/`WriteZeroCopy`), and explicit reservations (`ReserveRead`/`ReserveWrite`) all operate on the same packet stream.
7. **Deterministic lifecycle & crash detection**: endpoints advertise a session token and heartbeat in the superblock so peers distinguish graceful shutdown (`ErrPeerClosed`, `io.EOF` from `StandardLink.Read`) from abrupt process termination (`ErrPeerGone`).
8. **Never trample foreign data**: a segment is only initialized when its first page is entirely zero or when it is a valid segment of this layout version whose endpoints are gone.

## Shared Memory Layout

The protocol allocates a page-aligned superblock followed by two independent directional channels. Each direction has one packet ring and one fixed-size payload buffer pool:

```text
PAGE_START
  SUPERBLOCK    one page of protocol, configuration, init-state, and liveness metadata
PAGE_BREAK
  DATA_RING_0   protocol.Packet MPMC ring, Primary -> Secondary
PAGE_BREAK
  BUFFERS_0     bufferCount * bufferSize bytes, Primary -> Secondary payloads
PAGE_BREAK
  DATA_RING_1   protocol.Packet MPMC ring, Secondary -> Primary
PAGE_BREAK
  BUFFERS_1     bufferCount * bufferSize bytes, Secondary -> Primary payloads
PAGE_END
```

All regions are page-aligned (`PAGE_BREAK` rounds the running offset up to the OS page size). The superblock occupies one page (its 384 bytes must fit the smallest 4 KiB page). Supported configurations:

- `bufferCount`: a power of two with `2 <= bufferCount <= 1<<24`. One slot is not allowed: the "published" and "free for the next lap" sequence values would coincide.
- `bufferSize`: a multiple of 8 with `8 <= bufferSize <= 1<<30`.
- The total size must not overflow `uintptr` or `int` (only relevant on 32-bit platforms). `SizeStandardLink` returns `0` for any configuration that violates a bound.

For each direction, `BUFFERS[i]` is owned by data-ring slot `i`. Region offsets are always computed from the caller's `bufferCount`/`bufferSize`; the stored geometry is only compared, never used for addressing.

### Superblock (`HQQ_STL3`)

The first page is a shared initialization, configuration, and endpoint-liveness superblock (`superblockMagic = 0x4851515F53544C33`, `"HQQ_STL3"`). Every field is a little-endian `uint64` accessed atomically; field order is part of the ABI. The offsets of `RequiredFeatures` (88), `CreatorToken` (96), and the endpoint records (128) are pinned by compile-time assertions.

| Offset | Field | Meaning |
| ---: | --- | --- |
| `0` | `Magic` | `0x4851515F53544C33` (`"HQQ_STL3"`), written at publish, when the segment becomes `Ready`; a segment that is not `Ready` may still carry `0` |
| `8` | `State` | `0` (`Empty`), `1` (`Initializing`), or `2` (`Ready`); any other value is rejected with `ErrIncompatibleLayout` |
| `16` | `Major` | Layout major version `3`; written when an initializer claims the segment and again at publish. A different major is incompatible |
| `24` | `Minor` | Layout minor version `0`, informational |
| `32` | `PageSize` | OS page size used to compute region boundaries |
| `40` | `BufferCount` | Configured ring-slot/buffer count per direction |
| `48` | `BufferSize` | Configured bytes per slot-owned payload buffer |
| `56` | `LayoutSize` | Total bytes returned by `SizeStandardLink` |
| `64` | `Epoch` | Generation counter incremented on every (re)initialization, after the initializer committed |
| `72` | `InitToken` | Session token of the endpoint currently initializing the segment (`0` when none) |
| `80` | `InitHeartbeat` | Wall-clock `UnixNano` timestamp refreshed while initializing (progress marker) |
| `88` | `RequiredFeatures` | Bit set of layout features a reader must understand. None are defined: a reader that sees any set bit fails with `ErrIncompatibleLayout`. Written as `0` |
| `96` | `CreatorToken` | Identifies the instance that created the name: the random token of a `Create*`/`Listen` endpoint, or `0` when the segment was initialized by caller-owned memory (`NewStandardLink`, `NewAdvancedLink`). Openers never initialize and never write it. Written by the initializer right after it wins `InitToken` (not at publish), so every initialization replaces it |
| `104` | reserved | 3 words, zero |
| `128` | `Primary` | Primary endpoint record (128 bytes) |
| `256` | `Secondary` | Secondary endpoint record (128 bytes) |

An opener whose geometry (`BufferCount`, `BufferSize`, `LayoutSize`, `PageSize`) differs from a `Ready` segment with a live endpoint fails with `*GeometryError`, which wraps `ErrInvalidSize` (`errors.As` yields the wanted and recorded geometry).

#### Endpoint Record (128 bytes)

| Offset | Field | Meaning |
| ---: | --- | --- |
| `+0` | `Token` | `0` = unused since initialization, `1` = owner closed gracefully (`detachedToken`), any other value = random session token of the attached owner |
| `+8` | `PID` | OS process ID of the owner; diagnostic only (PIDs are not comparable across PID namespaces) |
| `+16` | `Heartbeat` | Last heartbeat, wall-clock `UnixNano` |
| `+24` | `Interval` | Owner's heartbeat interval in nanoseconds; `0` = heartbeats disabled (the endpoint is then always considered alive) |
| `+32` | reserved | 12 words, zero |

#### Initialization, Claim, Takeover, and Reuse

**Entry roles.** How an endpoint reaches a segment selects its role (`attachRole`). **Openers never initialize, re-initialize, or take over a segment**: they only join a live one as Secondary, so an opener can never end up as a Primary that nobody will join. Recovering an abandoned name is the creator's job.

| Role | Entry points | On an `Empty` segment | On an `Initializing` segment | On a live `Ready` segment | On a `Ready` segment without live endpoints |
| --- | --- | --- | --- | --- | --- |
| `roleCreator` | `CreateStandardLink`, `CreateAdvancedLink`, `Listen` | claims it | waits; takes over a stalled initializer | never joins: `ErrLinkBusy` | re-initializes it |
| `roleOpener` | `OpenStandardLink`, `OpenAdvancedLink`, `Dial` | waits until `AttachTimeout` for the creator to claim it, then fails with the transient `errSegmentNotReady` (segment untouched) | waits until `AttachTimeout`, then fails with the transient `errInitContended`; never takes over | joins as Secondary or `ErrLinkBusy` | fails immediately with the transient `errNoLiveEndpoint` |
| `roleAny` | `NewStandardLink`, `NewAdvancedLink` (caller-owned memory) | claims it | waits; takes over a stalled initializer | joins as Secondary or `ErrLinkBusy` | re-initializes it (`CreatorToken = 0`) |

**Claim loop** (`claimSuperblock`; every iteration first checks the context, which bounds every wait, and sleeps `200 µs` between looks at a segment another endpoint is working on):

1. **`State = Empty`**: if the whole superblock page is not zero (every word read atomically), another endpoint may have claimed it meanwhile (re-read `State`); otherwise the segment is foreign and the call returns `ErrIncompatibleLayout` *without writing anything*, for every role. Otherwise an opener waits for the creator (above) and, after `AttachTimeout`, fails with `errSegmentNotReady` without touching the segment; creators and caller-owned-memory endpoints claim it via CAS `State: Empty -> Initializing`, then `beginInit`.
2. **`beginInit(expected)`**: store `Major`, `Minor`, `RequiredFeatures = 0`; CAS `InitToken: expected -> myToken` (`expected` is the `InitToken` the claimer saw: `0` for a fresh claim, the dead initializer's token for a takeover); on success store `CreatorToken` and `InitHeartbeat`. A failed CAS means another endpoint took the initialization over (`errInitContended`).
3. **`State = Initializing`** (another endpoint is initializing): reject a conflicting layout first (`Magic` neither `0` nor ours, `Major` neither `0` nor `3`, or `Major = 0` with a non-zero `InitToken`). A claim that died right after the `State` CAS has no layout and `InitToken = 0` and is taken over like any other. An opener only waits: once `AttachTimeout` has passed it fails with `errInitContended`. A creator or caller-owned-memory endpoint watches `InitToken` and `InitHeartbeat` on its **local monotonic clock**; once neither changed for `max(PeerTimeout, 2s)` since it first saw the current pair, it takes over with `beginInit(expectedToken)`. There is no fast path from a stale wall-clock `InitHeartbeat`. A stalled initializer is given its whole window even if that runs past `AttachTimeout`; otherwise a claimer that waits longer than `AttachTimeout` fails with an error matching `ErrFailedInit` (`errInitContended`; `Dial` retries on it).
4. **`State = Ready`**: first the layout must be compatible: `Magic = HQQ_STL3`, `Major = 3`, and no unknown `RequiredFeatures` bit, otherwise `ErrIncompatibleLayout` (this includes `HQQ_STL2` segments). The check runs before joining and before taking over. Then:
   - **Liveness at open**: an endpoint record is alive if its token is an owner token and either heartbeats are disabled (`Interval = 0`), or no heartbeat was written yet, or its heartbeat is younger than `max(PeerTimeout, 4 × Interval)`. A heartbeat that looks stale is confirmed by watching the record for progress for `min(3 × Interval, 2s)`, so wall-clock jumps cannot declare a live endpoint dead.
   - **Segment reuse**: if neither record is alive, an opener fails immediately with `errNoLiveEndpoint` (message `hqq: shared memory segment has no live endpoint`, matches `ErrFailedInit`; `Dial` retries until a new `Listen` reuses the name). A creator or caller-owned-memory endpoint does CAS `State: Ready -> Initializing`, then `beginInit`, and becomes Primary with a new `Epoch`; the caller's geometry replaces the recorded one.
   - **Creator on a live segment**: `ErrLinkBusy`.
   - **Geometry**: compare against the caller's; a mismatch returns `*GeometryError`.
   - **Attaching**: if the Primary is alive and `Secondary.Token = 0`, the opener (or caller-owned-memory endpoint) claims the Secondary record via CAS `Token: 0 -> myToken` and fills it.
   - **Third-endpoint rejection**: in every other case (both records alive, or the live endpoint's earlier peer already closed or died, because a dead endpoint may still own ring slots) the call returns `ErrLinkBusy`.
5. Any other `State` value returns `ErrIncompatibleLayout`.

**Initialization** (the winner of `beginInit`; before every step it checks the context and that it still owns `InitToken`, otherwise it fails and must not touch the segment any more):

1. Format both MPMC rings (`mpmc.Format` zeroes the slots and the wait-word pairs, which also clears stale waiter counts).
2. Zero both payload pools in chunks of at most 8 MiB, refreshing `InitHeartbeat` after every chunk (only while it still owns `InitToken`).
3. **Publish**: verify `InitToken` is still ours; store `Major`, `Minor`, `RequiredFeatures`; store `PageSize`, `BufferCount`, `BufferSize`, `LayoutSize`; clear the `Secondary` record; store the `Primary` record (token, PID, interval, heartbeat); store `Magic`; then CAS `InitToken: myToken -> 0`. Only if that CAS succeeds does it increment `Epoch` and store `State = Ready`; a failed CAS returns `errInitContended` and leaves the segment as it is.
4. Attach to both rings (`mpmc.Attach`, in 10 ms slices, checking the context between slices, up to `AttachTimeout`). A ring that is missing or has a different magic, layout version, stride, or size makes the endpoint detach (`Token -> 1`) and fail with `ErrIncompatibleLayout` (or the context error).

Failure rules: if attaching the rings fails after the endpoint claimed a role, it writes `detachedToken` into its record before returning. If setting up a freshly created object (`Create*`, `Listen`) fails, the name is removed again, provided it still refers to this instance.

#### Named Segments: Create, Open, Safe Unlink

- `Create*` and `Listen` create the object exclusively (`O_EXCL`). If the name exists they open the existing object instead and apply the creator rules above:
  - an object smaller than the requested segment returns `ErrInvalidSize`, a zero-size object the transient `ErrFailedInit` variant (neither is resized or modified); a larger object is used as is;
  - a segment without live endpoints is re-initialized and owned by the caller (new `CreatorToken`);
  - a segment with a live endpoint returns `ErrLinkBusy`;
  - a foreign object or other layout version returns `ErrIncompatibleLayout` and is not modified;
  - Linux only: a name that is not a regular file (symbolic link, device, FIFO, directory) returns `ErrInvalidName`.
- `Open*` and `Dial` never create and never initialize: a missing name is `os.ErrNotExist`; a zero-size object is the transient `errSegmentNotReady`; a smaller object is `ErrInvalidSize`; an empty or initializing segment, and a segment without live endpoints, fail with the transient errors described above (all match `ErrFailedInit`).
- **Name ownership.** `Close` of a creator removes the name automatically; `Close` of an opener never does. `Unlink` (any endpoint of the instance) and the creator's `Close` remove the name only if it still refers to this endpoint's *instance*: on Linux, `fstat` of the open descriptor matches `lstat` of the name (same device and inode) and the `CreatorToken` in the mapping equals the endpoint's instance token (a creator's own token; an opener's is the `CreatorToken` it read from the segment after joining); on macOS and FreeBSD the name is re-opened read-only, its first page mapped, and its `CreatorToken` compared. A missing or replaced name is a no-op that returns nil. Windows names disappear with their last handle and `Unlink` is a no-op.
- **Windows.** An existing mapping or semaphore name always means a live handle somewhere (the creator makes the semaphores before the mapping, and an exclusive create fails with `os.ErrExist` if any of them exists, closing the handles it opened). A creator that finds an existing name therefore opens it and follows the creator rules above: a live segment yields `ErrLinkBusy`.

### MPMC Ring Layout (v2)

Each directional packet ring uses a 512-byte header (`mpmc.HeaderSize = 512`) split into four 128-byte lines so that independently written words never share a cache line (128 bytes covers both 64-byte and 128-byte cache lines):

```text
Offset   0..127 : magic, size, init flag, slot stride, layout version (uint64 each; immutable after init)
Offset 128..255 : r             (uint64 consumer cursor)
Offset 256..383 : w             (uint64 producer cursor)
Offset 384..399 : notEmpty      (uint32 wake word, bumped by producers)
                  emptyWaiters  (uint32 number of parked processes waiting for data)
                  notFull       (uint32 wake word, bumped by consumers)
                  fullWaiters   (uint32 number of parked processes waiting for space)
Offset 512..end : slots[0..N-1]
```

Each slot is an 8-byte sequence number (`seq uint64`) followed by the 64-byte `protocol.Packet`, rounded up to a 64-byte multiple (stride 128 bytes). The number of slots `N` is a power of two of at least 2. An attacher rejects a ring whose magic, layout version, stride, or power-of-two size does not match. The payload type of a ring must not contain Go pointers (it lives in memory the garbage collector does not scan, and is shared with another process).

#### Slot protocol and completion semantics

`Format` sets `seq[i] = i`, `r = w = 0`, and clears the wait words. With `pos` a position (an index modulo 2^64; slot `pos & mask`):

- **Producer claim**: `d = int64(seq[pos&mask] - pos)`. `d == 0`: CAS `w: pos -> pos+1` claims the slot; `d > 0`: another producer got `pos`, reload `w`; `d < 0`: the ring is full, wait.
- **Publish**: `seq = pos + 1`, then wake consumers if `emptyWaiters != 0`.
- **Consumer claim**: `d = int64(seq[pos&mask] - (pos+1))`. `d == 0`: CAS `r: pos -> pos+1` claims the slot; `d > 0`: reload `r`; `d < 0`: the ring is empty, wait.
- **Release**: `seq = pos + mask + 1`, then wake producers if `fullWaiters != 0`.

The claimant of a position is its only writer, so a plain atomic store completes the slot. The paths that hold the claim on a stack frame (`Enqueue*`, `Dequeue*`, `EnqueueZeroCopyCancel`, `DequeueZeroCopyCancel`) store `seq` directly. A claim that callers may **copy** (`ProducerSlot` and `ConsumerSlot`, used by `StandardLink.WriteZeroCopy`, `ReserveWrite`, and `ReserveRead`) completes with a compare-and-swap from the claimed value (`seq == pos` for `Commit`, `seq == pos + 1` for `Release`): a stale copy of an already completed slot fails the swap, neither modifies the ring nor wakes anyone, and returns failure (`ErrInvalidOp` in the public reservation API). `StandardWriteReservation.Buffer` and `StandardReadReservation.Buffer` return nil once the slot is no longer owned. A panicking zero-copy callback still publishes (a tombstone for `WriteZeroCopy`, a zeroed element for the raw ring) or releases its slot, so a failing callback cannot wedge the ring.

#### Wait / park protocol

When `WaitAdaptive` (the default `WaitPolicy`) is active, a blocked producer or consumer:

1. re-checks the slot sequence in a tight loop (64 iterations), then
2. yields with `runtime.Gosched` (256 iterations; deadline and liveness checks every 16th), then
3. parks for at most 100 ms (or until its deadline, or its next liveness check, which runs at most every 50 ms), as follows.

`WaitSpin` never parks: it re-checks forever, calling `runtime.Gosched` and the slow checks every 64th iteration.

**Parking and process-local aggregation.** All blocked operations of one process on the same wait word form a group. The first operation to park becomes the **leader**; it alone runs the protocol below. Every other operation is a **follower**: it blocks on a process-local channel together with its own context, close channel, and timer, and re-checks when the leader returns (one of them then becomes the next leader). Waiter counts in the shared header therefore count **parked processes**, not goroutines, and N blocked goroutines pin one OS thread. The layer is process-local: it adds nothing to the shared-memory ABI. The leader's protocol:

1. increment `emptyWaiters` (consumers) or `fullWaiters` (producers);
2. load the wake word, re-check the slot sequence and cancellation;
3. if still blocked, wait on the platform primitive for at most the park timeout;
4. decrement the count.

A waker publishes (or releases) the slot first and then reads the waiter count; if it is non-zero it bumps the wake word and issues the kernel wake (or, with a `Parker`, calls `Wake(side, n)` with `n` = the count). A follower checks its condition before joining the group, and all waiters of a side wait for the same slot, so a publish either finds the leader registered and wakes it, or precedes its registration and is seen by its re-check; a stale mismatch delays a follower by at most one park.

Platform primitives:

- **Linux**: `futex` on the wake word (`FUTEX_WAIT = 0`, `FUTEX_WAKE = 1`, shared across processes without `FUTEX_PRIVATE_FLAG`; the wake wakes all waiters).
- **macOS**: `__ulock_wait` / `__ulock_wake` on the wake word (`UL_COMPARE_AND_WAIT_SHARED = 3`, wake with `ULF_WAKE_ALL`). These are private Darwin interfaces invoked as raw system calls; errors are ignored, so if they ever stop working parking degrades to busy re-checking.
- **Windows, named segments**: there is no cross-process compare-and-wait primitive, so each segment has four named kernel semaphores (`<mapping object name>.hqq-sem0` … `.hqq-sem3`: ring 0 notEmpty/notFull, ring 1 notEmpty/notFull) behind a `Parker`. The leader calls `WaitForSingleObject` on the semaphore; the waker, after publishing, reads the waiter count `n` (parked processes) and calls `ReleaseSemaphore(n)`. Because a released token persists, a wake that runs between the waiter's re-check and its wait is not lost; tokens left for waiters that already gave up only cause one spurious wake-up.
- **Windows, unnamed memory**: `WaitOnAddress` / `WakeByAddressAll`, which only match waiters and wakers of the same process; the wait is bounded by the backoff step (`20µs` doubling to `2ms`), so a wake-up from another process is noticed within one step.
- **Other platforms**: bounded exponential sleep backoff (`20µs` to `2ms`), no aggregation.

A process killed while parked never decrements its count; the count stays raised until the ring is re-formatted (the segment is re-initialized). The only cost is one superfluous wake call per publish or release for the surviving endpoint, which must close the link after `ErrPeerGone` anyway; correctness is not affected because waiters only use the count as a hint to wake.

The core Vyukov MPMC state machine, including the completion compare-and-swap as a guard (`CompletionCAS`), is formally specified in [`internal/mpmc/MPMC.tla`](internal/mpmc/MPMC.tla) and model-checked with TLC via [`scripts/tlc.sh`](scripts/tlc.sh) on a bounded model (`BufferSize = 2` in CI, `4` manually) for writer/reader exclusivity (`NoRace`), payload integrity (`DataIntegrity`), FIFO claim order (`FIFOClaimOrder`), and lock-free system progress (`SystemProgress`). The model does not cover parking and waiter counts, the aggregation layer, `Format`, crashes, or stale copies of handles; this is a bounded check, not a proof.

## Packet ABI

Every packet is exactly **64 bytes**: eight little-endian `uint64` words.

```text
word 0: opcode in low byte; remaining bits reserved and written as zero
word 1: operand 0
word 2: operand 1
word 3: operand 2
word 4: operand 3
word 5: operand 4
word 6: operand 5
word 7: operand 6
```

In Go this is represented as `protocol.Packet [64]byte`. Use `protocol.NewPacket`, `Packet.Op`, and `Packet.Operand` instead of relying on native struct layout.

| Opcode | Value | Layer | Operands |
| --- | ---: | --- | --- |
| `OpError` | `0x00` | both | `0` ContextID (connection, copy, or request ID), `1` `ErrorCode` |
| `OpConnCreate` | `0x01` | Advanced | `0` ConnectionID |
| `OpConnAccept` | `0x02` | Advanced | `0` ConnectionID |
| `OpConnClose` | `0x03` | Advanced | `0` ConnectionID, `1` reason |
| `OpConnCopy` | `0x04` | Advanced | see below |
| `OpProtoNegotiate` | `0x05` | Advanced | `0` version, `1` requested features, `2` capability word (always `0`, ignored) |
| `OpProtoAck` | `0x06` | Advanced | `0` version, `1` negotiated features, `2` capability word (always `0`, ignored) |
| `OpStandardLinkTombstone` | `0xFE` | Standard | see below |
| `OpStandardLinkCopy` | `0xFF` | Standard | see below |

## Standard Protocol 1.0

### `OpStandardLinkTombstone` (`0xFE`)

Marks a claimed slot as intentionally empty.

| Operand | Meaning |
| --- | --- |
| `Operand0` | `CopyID` or reservation ID |
| `Operand1` | `BufferIndex`, diagnostic mirror of the claimed data-ring slot index |
| `Operand2` | `ErrorCode` or reason code |
| `Operand3..6` | Reserved, must be zero |

Tombstones preserve ring progress when a producer reserves a slot but aborts before publishing a payload (`StandardWriteReservation.Abort`, an invalid `Commit` size, a `WriteZeroCopy` callback that fails or panics). Readers skip tombstones and release the slot immediately.

### `OpStandardLinkCopy` (`0xFF`)

Transfers one payload stored in a direction-local buffer pool.

| Operand | Meaning |
| --- | --- |
| `Operand0` | `CopyID`, monotonically increasing per endpoint |
| `Operand1` | `BufferIndex`, diagnostic mirror of the claimed data-ring slot index |
| `Operand2` | `DataSize`, number of valid bytes in the buffer (`1 <= DataSize <= bufferSize`) |
| `Operand3..6` | Reserved, must be zero |

Empty `Write` calls do not publish packets. A receiver locates the payload from the claimed slot index, never from `Operand1`.

### Standard Buffer Ownership

Each direction starts with every data-ring slot available. Slot `i` owns `BUFFERS[i]`.

#### Send path

1. Sender claims one producer slot from the direction's data ring.
2. Sender writes the payload into `BUFFERS[slot]`.
3. Sender stores `OpStandardLinkCopy` in the claimed packet slot (`Operand1` mirrors `slot`). If a reserved write is aborted, sender stores `OpStandardLinkTombstone` instead.
4. Sender publishes the packet by completing the slot sequence and wakes any parked consumer.

#### Receive path

1. Receiver claims one consumer slot from the direction's data ring.
2. Receiver locates `BUFFERS[slot]` from the claimed slot index and validates `DataSize` (`1..bufferSize`, else `ErrInvalidSize`).
3. Receiver skips tombstones. An `OpError` packet yields the peer's error. If it encounters an Advanced frame (`OpConnCopy`, `OpProtoNegotiate`, etc.) on the raw Standard read path, it releases the slot and returns `ErrUnexpectedPacket`.
4. Returning from the dequeue callback or calling `Release` releases the packet slot and `BUFFERS[slot]` back to producers and wakes any parked producer.

A closed or dead peer does not lose messages: when a read fails because the peer closed or died, the receiver makes one more non-blocking attempt, so messages published before the failure are delivered first.

#### Partial reads and tails

`Read` copies at most `len(b)` bytes of one message. The unread tail is copied into a per-endpoint FIFO of tails (each with its own buffer; tails are never merged). One read call serves only the oldest tail, or a new message if there is none, so no call returns bytes of two messages. `ReadZeroCopy` and `ReserveRead` serve the whole oldest tail from the endpoint's own memory instead of a ring slot. `ReadFrom` (datagram semantics) truncates: it copies `len(p)` bytes and discards the rest, and serves a pending tail as one datagram. After `Close`, tails are no longer served.

#### `net.PacketConn` addresses

`LocalAddr` returns `HQQAddr{"hqq-primary"}` on a Primary link and `HQQAddr{"hqq-secondary"}` on a Secondary one; `ReadFrom` returns the peer's address (the opposite name); `WriteTo` ignores its address argument.

> [!WARNING]
> Zero-copy callbacks and read reservations hold the underlying ring slot. They must finish promptly; if they block or are retained, the whole direction can suffer head-of-line (HOL) blocking. The shared buffer must not be retained after release. `Close` waits for running callbacks (so it must not be called from one) but does not track reservations; the buffer of a reservation is invalid after `Close`.

## Advanced Protocol 1.0 Design

The Advanced Protocol is an overlay over Standard Protocol 1.0. It publishes Advanced packets through the same direction-local packet rings, and each Advanced payload chunk uses the same-index buffer owned by the claimed ring slot. Once negotiation of an `AdvancedLink` has started, the Advanced receive path owns the receive direction of that link.

### Advanced IDs

| ID | Scope | Purpose |
| --- | --- | --- |
| `ConnectionID` | Per link pair | Logical stream (`Stream.ID`); `0` is the link-level stream, which is always open. Even non-zero IDs are allocated by Primary (`2, 4, ...`); odd IDs by Secondary (`1, 3, ...`). IDs are never reused on a link. |
| `MessageID` | Per sender | Identifies a data message, or correlates a request with its response; starts at `1` (never `0`). Responses reuse the request's `MessageID`. |
| `MethodID` | Application-defined | Numeric function/procedure selector for request packets (`Request.Method`). |

A message is identified on the receive side by `(ConnectionID, MessageID, kind)` where `kind` is request, response, or plain data.

### Advanced Frame Flags (`OpConnCopy.Operand5`)

| Flag | Value | Meaning |
| --- | ---: | --- |
| `AdvFlagBegin` | `1 << 0` | First chunk of a logical message. |
| `AdvFlagEnd` | `1 << 1` | Final chunk of a logical message. |
| `AdvFlagAbort` | `1 << 2` | Sender abandoned this logical message; receiver discards partial state. Aborted requests and data messages are never delivered to `Stream.Recv` or `Serve`; an aborted response fails the waiting call with `ErrMessageAborted`. |
| `AdvFlagRequest` | `1 << 3` | Chunk belongs to a request message. |
| `AdvFlagResponse` | `1 << 4` | Chunk belongs to a response message. |
| `AdvFlagError` | `1 << 5` | Response represents an application/protocol error. |

For single-chunk messages, both `AdvFlagBegin` and `AdvFlagEnd` are set. A frame with an unknown flag bit, with both `AdvFlagRequest` and `AdvFlagResponse`, or with `AdvFlagError` but not `AdvFlagResponse`, is a protocol violation (`ErrProtocol`). An abort frame carries `TotalSize = ChunkOffset = ChunkSize = 0`, the request/response flags of the abandoned message, and no `Begin`/`End`; a sender that fails after the first chunk sends it with a short independent deadline.

### Stream Control: `OpConnCreate` (`0x01`), `OpConnAccept` (`0x02`), `OpConnClose` (`0x03`), `OpError` (`0x00`)

- `OpConnCreate`: `Operand0 = ConnectionID`, `Operand1 = flags (0)`. The peer replies with `OpConnAccept` (`Operand0 = ConnectionID`) if it accepts, or with `OpError` (`Operand0 = ConnectionID`, `Operand1 = ErrorCode`) if it refuses; the opener's `OpenStream` then returns the matching sentinel:

  | Refusal | `ErrorCode` |
  | --- | --- |
  | not listening (`ListenStreams` not called) | `ErrCodeConnNotFound` |
  | ID `0`, wrong parity for the sender's role, or already in use (the existing stream is left untouched) | `ErrCodeInvalidOp` |
  | accept queue full under `OverflowDrop` | `ErrCodeBufferOverflow` |

  Under `OverflowFail` a full accept queue fails the receive path with `ErrIncomingOverflow` instead of replying.
- `OpConnClose`: `Operand0 = ConnectionID`, `Operand1 = reason` (`Stream.Close` sends `0`). The receiver unregisters the stream and marks it closed by the peer; an unknown ID, or one whose `OpenStream` is still pending, is ignored.
- An `OpError` that matches no pending `OpenStream` fails the receiving side with the error of its code (an unknown code yields an error matching `ErrProtocol`).

**`Stream.Close()`** takes no context. It marks the stream closed locally first (a second `Close` returns `ErrClosed`), unregisters it, discards its queued messages, and then sends `OpConnClose`, unless the peer closed the stream first; the stream stays closed locally even if sending fails, in which case the peer keeps its registration until the link closes. Closing stream `0` returns `ErrInvalidOp`, and the first `Close` after a peer close returns nil. Stream IDs are never reused. Requests the peer already sent on the stream are not affected: `Serve` still answers them on that stream.

**Accept ordering.** An incoming stream is registered immediately but becomes visible to `AcceptStream` only after its `OpConnAccept` has been enqueued on the tx ring, so data the acceptor sends on the stream cannot overtake the accept.

### `OpConnCopy` (`0x04`)

Transfers one Advanced chunk of a message (plain data, request, or response):

| Operand | Meaning |
| --- | --- |
| `Operand0` | `ConnectionID`, or `0` for link-level traffic |
| `Operand1` | `MessageID` |
| `Operand2` | `TotalSize`, complete logical message size in bytes |
| `Operand3` | `ChunkOffset`, byte offset within the logical message |
| `Operand4` | `ChunkSize`, valid bytes in the claimed slot-owned payload buffer |
| `Operand5` | Advanced frame flags |
| `Operand6` | `MethodID` for request begin frames; status/error code for response/error frames; otherwise zero. Non-zero only in the chunk at offset `0` |

Chunks of one message are sent in order with `ChunkSize <= bufferSize`. A zero-length message is a single frame with `TotalSize = ChunkOffset = ChunkSize = 0` and both `Begin` and `End`. `Stream.Send` requires `FeatureMessages`; `Stream.Call` and `Serve` (which sends the responses) require `FeatureRequestResponse`; otherwise `ErrNotNegotiated`. A payload larger than `MaxMessageSize` is refused locally with `ErrBufferOverflow`.

### Protocol Negotiation (`OpProtoNegotiate` `0x05`, `OpProtoAck` `0x06`)

Negotiation is mandatory before using Advanced APIs (`ErrNotNegotiated` otherwise).

- **Features**: `FeatureMessages = 1 << 0` (plain messages, `OpConnCopy` without request/response flags), `FeatureRequestResponse = 1 << 1`. Operand 0 encodes the protocol version as `(major << 8) | minor`; operand 2 (capabilities) is always sent as `0` and ignored on receipt.
- **Role-agnostic negotiation**: `AdvancedLink.Negotiate(ctx, features)` initiates negotiation on the Primary endpoint and waits for negotiation on the Secondary endpoint (`features = 0` requests everything supported).
- **Answer rules**: the responder rejects (with `OpError(0, ErrCodeInvalidOp)`) a request whose major version differs from `1`; otherwise it answers with the requested minor version capped at its own (`0`) and the intersection of requested, supported, and allowed features. The initiator rejects (`ErrNegotiationRejected`) an ack whose major differs, whose minor is higher than its own, or which contains a feature it did not request or does not support. Tombstones are skipped; an `OpError` fails negotiation with `ErrNegotiationRejected`; any other opcode with `ErrUnexpectedPacket`.
- **Simultaneous initiation resolution**: If both endpoints call `NegotiateProtocol` concurrently, the Primary's request wins: the Secondary answers the Primary's `OpProtoNegotiate` with `OpProtoAck` (intersection of features), and the Primary ignores the Secondary's request.
- **`ProtocolStateFailed` is terminal.** Any error during an attempt (peer rejection, invalid packets, a canceled or expired context, a closed link) moves the link to `ProtocolStateFailed`. Every later `Negotiate`, `NegotiateProtocol`, or `WaitForNegotiation` returns `ErrNegotiationFailed`, because negotiation frames of the failed attempt may still be in flight; the link must be closed and a new one opened. `ErrAlreadyNegotiated` is returned only while an attempt is in progress or after it succeeded.

### Receive Path, Routing, Control Sender, and Overflow Policy

After negotiation the receive direction has a single consumer path. A goroutine that waits for a message (`Stream.Recv`, `Stream.Call`, `Serve`) and finds no dispatcher receives synchronously, which gives the lowest latency, and routes every message it receives to its destination. A background **dispatcher** is started when several goroutines wait concurrently or when streams need asynchronous routing (`ListenStreams`, `AcceptStream`, and `OpenStream` start it). Either way the receive path assembles messages, handles stream control frames, and routes each complete message in the order it was received:

| Message | Destination |
| --- | --- |
| response (`AdvFlagResponse`) | the pending `Call` with the same `(ConnectionID, MessageID)`; a response without a waiting call is dropped and counted in `AdvancedStats.DroppedMessages`, unless its call was canceled (below) |
| request (`AdvFlagRequest`) | the request queue of the link, consumed only by `Serve`, for every stream |
| data (neither flag) | the queue of its stream, consumed by `Stream.Recv`; data for a stream that is unknown or closed is dropped and counted in `DroppedMessages` |

- **The receive path never waits for the tx ring.** If two peers flood each other, both tx rings fill up, and a receiver that waited for its tx ring would wait for a peer that no longer drains. The replies the receive path generates (`OpConnAccept`, `OpError` refusals of `OpConnCreate`, error responses to requests refused by `MaxReassemblyBytes`) are queued on a **control sender**: a goroutine started lazily on the first reply and stopped by `Close`, fed by a bounded queue of `max(BufferCount, 16)` jobs. It is the only goroutine that waits for the tx ring on behalf of the receive path. A full queue never blocks the receiver: the reply is dropped. A dropped reply to an `OpConnCreate` is counted in `AdvancedStats.DroppedMessages` and leaves the peer's `OpenStream` waiting until its own context ends; a dropped error response to a refused request (already counted when the request was refused) leaves the peer's `Call` waiting likewise.
- **Single-chunk fast path**: single-frame messages (`AdvFlagBegin | AdvFlagEnd`) take a pooled buffer and copy the payload directly, without inserting into the multi-chunk reassembly map.
- **Late responses to canceled calls**: when `Call` times out, its `context.Context` is canceled, or its stream is closed while it waits for a response, the `(ConnectionID, MessageID)` key is recorded in a bounded set of canceled calls (4096 entries, oldest evicted). Subsequent response chunks for that call are silently drained and discarded (counted in `AbandonedResponses`) rather than aborting the receive path.
- **Per-queue backpressure (`OverflowPolicy`)**: every destination queue has `AdvancedOptions.IncomingQueueSize` entries (default `bufferCount`): the queue of each stream, the request queue, and the queue of accepted streams. A slot in the accept queue is reserved when a stream's `OpConnCreate` arrives, so the accept reply can always be delivered. The policy applies to the queue a message is routed to:
  - `OverflowBlock` (default): the receive path waits until the consumer of that queue (`Stream.Recv`, `Serve`, `AcceptStream`) makes room, propagating backpressure into the shared-memory ring. While it waits, nothing else is routed, not even responses, so an application must consume every stream it uses and run `Serve` if the peer sends requests. This is the only wait left on the receive path, and it depends on the application, not on the tx ring. A synchronous receiver does not wait itself: it hands the delivery to a helper goroutine that keeps the order, and a dispatcher takes over, so a caller's `Recv` or `Call` timeout cannot lose another goroutine's message.
  - `OverflowDrop`: drops the message that does not fit, counted in `DroppedMessages`. A stream whose `OpConnCreate` finds the accept queue full is **refused**: the stream is not registered and the peer receives `OpError` with `ErrCodeBufferOverflow`; this is also counted in `DroppedMessages`. The receive path never blocks, so responses are always routed.
  - `OverflowFail`: fails the receive path with `ErrIncomingOverflow`; pending and future calls fail with that error, and `Stream.Recv` returns the messages still queued before it.
- **Stream close.** The peer, on receiving `OpConnClose`, unregisters the stream and marks it closed: the messages it routed before the close are still queued and `Recv` returns them before `ErrPeerClosed`; `Send` and `Call` return `ErrPeerClosed` at once, and a waiting `Recv` or `Call` wakes. A local `Stream.Close` unregisters the stream, discards its queued messages, and wakes blocked operations, which return `ErrClosed`. Data that arrives for a closed stream is dropped and counted. Requests already in flight are not affected: `Serve` still answers them on the stream they arrived on.
- **`Serve` execution.** `Serve` consumes the request queue with one receiver goroutine at a time. While no handler is running, the receiver runs each handler inline; if an inline handler is still running after 200 µs, the receiver role passes to a new goroutine. While any handler is running, requests go to pooled worker goroutines, started on demand up to `BufferCount` and kept until `Serve` returns; when all are busy, receiving stops until one finishes, and the overflow policy of the request queue applies. When a request arrives and nothing is running, it runs inline again. `Serve` returns `ctx.Err()` when its context ends, nil when the peer closed the link gracefully, and the receive error otherwise.

### Reassembly and Bounds

- `AdvancedOptions.MaxMessageSize` (default 64 MiB) bounds one message's `TotalSize` (`ErrBufferOverflow`).
- Per-frame validation (`ErrInvalidSize` unless noted): `ChunkSize <= bufferSize`; `ChunkOffset <= TotalSize` and `ChunkSize <= TotalSize - ChunkOffset`; `TotalSize == 0` only as the single frame described above; `TotalSize > 0` requires `ChunkSize > 0`; `Operand6` is zero in chunks with a non-zero offset; a `Begin` frame has `ChunkOffset == 0`, no assembly or refusal already exists for its key, and (single chunk) `ChunkSize == TotalSize`; later chunks must continue exactly at the previous end (`ChunkOffset` equals the bytes received so far) and use the announced `TotalSize`; the `End` chunk must complete `TotalSize`. A non-`Begin` chunk with no assembly and no refusal is `ErrProtocol`. A validation failure drops the partial message and fails the receive path with the error.
- **Memory bound**: the first chunk of a multi-chunk message takes a pooled buffer of `TotalSize` bytes rounded up to a power of two (at most `MaxMessageSize`) and is charged `TotalSize + 256` against `AdvancedOptions.MaxReassemblyBytes` (default `4 × MaxMessageSize`, saturating); the charge is the announced size, so the buffers actually held can exceed it. A first chunk that would exceed the budget (alone, or together with the messages already being assembled) is refused without taking a buffer; its remaining chunks are drained. A refused request is answered with an error response (status `ErrCodeBufferOverflow`, queued on the control sender), a refused response fails the waiting call with `ErrReassemblyLimit`, and a refused data message is dropped; all are counted in `AdvancedStats.DroppedMessages`. At most 1024 refused messages are tracked for draining; a message pushed out of that set fails with `ErrProtocol` on its next chunk.

## Error Hierarchy

All public errors support `errors.Is` against the sentinel itself and against the base it wraps. Unexported transient errors used while a named segment is being set up (`segment not initialized yet`, `initialization did not complete`, `segment has no live endpoint`) wrap `ErrFailedInit`; `Dial` retries only on these and on a missing name.

| Sentinel | Wraps | Meaning |
| --- | --- | --- |
| `io.EOF` (returned as is) | — | `StandardLink.Read` and `ReadFrom` after the peer closed gracefully and every buffered message was consumed |
| `ErrClosed` | `net.ErrClosed` | Local link was closed, or the stream was closed locally (`Stream.Close`) |
| `ErrPeerClosed` | `io.EOF` | Remote peer closed its link gracefully (all APIs except `StandardLink.Read` and `ReadFrom`), or closed the stream (`Send` and `Call` of that stream, `Recv` once its queued messages are consumed) |
| `ErrPeerGone` | `ErrConnNotFound` | Remote peer stopped heartbeating (crashed or hung), or the segment was re-initialized under this endpoint |
| `ErrTimeout` | `os.ErrDeadlineExceeded` | Deadline passed (implements `net.Error` with `Timeout() == true`) |
| `ErrLinkBusy` | `ErrFailedInit` | Two live endpoints are attached, the live endpoint's earlier peer already left, or a creator (`Create*`, `Listen`) found a live segment |
| `ErrIncompatibleLayout` | `ErrFailedInit` | Foreign magic or other layout major (including `HQQ_STL2`), unknown `RequiredFeatures` bit, unknown `State`, a non-zero first page of an `Empty` segment, or a ring header that does not match this HQQ version |
| `ErrInvalidSize` | — | Invalid geometry or size bounds, a named object smaller than the requested segment, or geometry that differs from an existing segment |
| `*GeometryError` (`errors.As`) | `ErrInvalidSize` | The recorded geometry of an existing segment differs from the caller's; carries `Want` and `Got` (`Geometry{BufferCount, BufferSize, LayoutSize, PageSize}`) |
| `ErrMemoryAlign` | — | Caller-owned memory is not page-aligned |
| `ErrMemorySmall` | — | Caller-owned memory is empty or shorter than `SizeStandardLink` |
| `ErrFailedInit` | — | Base of initialization failures, including transient ones for openers (an object that is not sized yet or an `Empty` segment whose creator never claimed it; an initializer that did not finish within `AttachTimeout`; a segment without any live endpoint) |
| `ErrInvalidName` | — | Shared memory name is empty, contains separators/NUL, or exceeds the platform limit; on Linux also a name that is a symbolic link or not a regular file |
| `ErrConnNotFound` | — | Base of `ErrPeerGone`; also the refusal of `OpenStream` because the peer was not listening. Not returned for unknown or closed stream IDs (those give `ErrClosed` or `ErrPeerClosed`) |
| `ErrInvalidOp` | — | Base of the protocol-state errors below; also `Stream.Close` on stream 0 and `Serve` with a nil handler |
| `ErrBufferOverflow` | — | Payload larger than `bufferSize` (`StandardLink.Write`) or `MaxMessageSize` (`Stream.Send`, `ResponseWriter.Write`, incoming frame) |
| `ErrNotNegotiated` | `ErrInvalidOp` | Advanced API called before protocol negotiation succeeded, or the required feature was not negotiated |
| `ErrAlreadyNegotiated` | `ErrInvalidOp` | Protocol negotiation attempted while one is in progress or after one succeeded |
| `ErrNegotiationFailed` | `ErrInvalidOp` | Protocol negotiation attempted after an earlier attempt failed (`ProtocolStateFailed` is terminal; reopen the link) |
| `ErrNegotiationRejected` | `ErrInvalidOp` | Peer rejected negotiation or acknowledged an incompatible version/feature set |
| `ErrUnexpectedPacket` | `ErrInvalidOp` | Packet that the calling API cannot consume (for example an Advanced frame on `StandardLink.Read`) |
| `ErrProtocol` | `ErrInvalidOp` | Malformed frame bounds or flags, protocol state violation, or a peer error with an unknown code |
| `ErrMessageAborted` | `ErrProtocol` | `Stream.Call`: the peer abandoned the response mid-transfer |
| `ErrNotListening` / `ErrAlreadyListening` | `ErrInvalidOp` | `AcceptStream` before `ListenStreams` / second `ListenStreams` |
| `ErrIncomingOverflow` | `ErrBufferOverflow` | The queue a message is routed to (a stream, the request queue, or the accept queue) is full under `OverflowFail` |
| `ErrReassemblyLimit` | `ErrBufferOverflow` | `Stream.Call`: the response was refused under `MaxReassemblyBytes` |
| `ResponseError` (`errors.As`) | — | `Stream.Call` returns it by value when the peer's handler answered with `ResponseWriter.WriteError`; carries `Status` and a copy of the payload |

`ErrorCode` values carried in `OpError`, tombstones, and error statuses: `0x01` memory alignment, `0x02` invalid size, `0x03` memory too small, `0x04` failed init, `0x05` connection not found, `0x06` invalid operation, `0x07` buffer overflow; each maps back to the base error above.
