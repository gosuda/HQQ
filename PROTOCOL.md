# HQQ Protocol Specification

## Scope

This document defines the HQQ shared-memory protocol family:

- **Standard Protocol 1.0**: the [`StandardLink`](standard.go) data path. It provides the fixed 64-byte packet ABI, bidirectional lock-free MPMC rings, ring-slot-owned payload buffers, tombstones, endpoint liveness/crash detection, and the basic `Read`/`Write`/zero-copy/reservation APIs.
- **Advanced Protocol 1.0**: an optional negotiated overlay on top of the Standard Protocol. It keeps the same shared-memory layout and packet ABI, and adds multi-chunk large-data copy, concurrent request-response IPC, and logical stream multiplexing.

The Advanced Protocol preserves Standard Protocol ownership rules: every payload byte lives in the same-index buffer owned by the claimed data-ring slot, and every reader releases the claimed slot promptly.

## Shared Design Goals

1. **Fixed ABI**: packets have a stable 64-byte layout independent of Go struct padding or `uintptr` width.
2. **Little-endian encoding**: all packet words are encoded with `binary.LittleEndian`.
3. **Lock-free ownership transfer**: data ring slots own same-index payload buffers; no mutex is required on the `StandardLink` hot path.
4. **No unread-buffer overwrite**: a payload buffer is recycled only after the receiver releases the owning ring slot.
5. **Bounded memory**: all rings and payload buffers are allocated up front.
6. **Composable dispatch**: automatic copy (`Read`/`Write`), zero-copy callbacks (`ReadZeroCopy`/`WriteZeroCopy`), and explicit reservations (`ReserveRead`/`ReserveWrite`) all operate on the same packet stream.
7. **Deterministic lifecycle & crash detection**: endpoints advertise a session token and heartbeat in the superblock so peers distinguish graceful shutdown (`ErrPeerClosed`, `io.EOF` from `StandardLink.Read`) from abrupt process termination (`ErrPeerGone`).

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

All regions are page-aligned. `bufferCount` must be a power of two and at least 2. `bufferSize` must be at least 8 bytes and a multiple of 8. For each direction, `BUFFERS[i]` is owned by data-ring slot `i`.

### Superblock (`HQQ_STL2`)

The first page is a shared initialization, configuration, and endpoint-liveness superblock (`superblockMagic = 0x4851515F53544C32`, `"HQQ_STL2"`). Every field is a little-endian `uint64` accessed atomically; field order is part of the ABI:

| Offset | Field | Meaning |
| ---: | --- | --- |
| `0` | `Magic` | `0x4851515F53544C32` (`"HQQ_STL2"`), written when the segment becomes `Ready` |
| `8` | `State` | `0` (`Empty`), `1` (`Initializing`), or `2` (`Ready`); any other value is rejected with `ErrIncompatibleLayout` |
| `16` | `Major` | Standard protocol major version (`1`), informational |
| `24` | `Minor` | Standard protocol minor version (`0`), informational |
| `32` | `PageSize` | OS page size used to compute region boundaries |
| `40` | `BufferCount` | Configured ring-slot/buffer count per direction |
| `48` | `BufferSize` | Configured bytes per slot-owned payload buffer |
| `56` | `LayoutSize` | Total bytes returned by `SizeStandardLink` |
| `64` | `Epoch` | Generation counter incremented on every (re)initialization |
| `72` | `InitToken` | Session token of the endpoint currently initializing the segment (`0` when none) |
| `80` | `InitHeartbeat` | Wall-clock `UnixNano` timestamp refreshed while initializing |
| `88` | reserved | 5 words, zero |
| `128` | `Primary` | Primary endpoint record (128 bytes) |
| `256` | `Secondary` | Secondary endpoint record (128 bytes) |

Region offsets are always computed from the caller's `bufferCount`/`bufferSize`; the stored geometry is only compared, never used for addressing. An opener whose geometry differs from a `Ready` segment fails with `ErrInvalidSize`.

#### Endpoint Record (128 bytes)

| Offset | Field | Meaning |
| ---: | --- | --- |
| `+0` | `Token` | `0` = unused since initialization, `1` = owner closed gracefully (`detachedToken`), any other value = random session token of the attached owner |
| `+8` | `PID` | OS process ID of the owner; diagnostic only (PIDs are not comparable across PID namespaces) |
| `+16` | `Heartbeat` | Last heartbeat, wall-clock `UnixNano` |
| `+24` | `Interval` | Owner's heartbeat interval in nanoseconds; `0` = heartbeats disabled (the endpoint is then always considered alive) |
| `+32` | reserved | 12 words, zero |

#### Initialization, Takeover, and Segment Reuse

1. **Initialization**: An opener that finds `State = Empty` claims it via CAS (`Empty -> Initializing`), stores its token in `InitToken` and refreshes `InitHeartbeat`, formats both MPMC rings, and zeros both payload pools (refreshing `InitHeartbeat` every 8 MiB). It then checks that it still owns `InitToken` (otherwise another endpoint took over and it fails), increments `Epoch`, writes the version and layout fields, clears the `Secondary` record and claims the `Primary` record, writes `Magic`, clears `InitToken`, and publishes `State = Ready`.
2. **Initializer crash takeover**: If `State` stays `Initializing` and `InitHeartbeat` is older than `max(PeerTimeout, 2s)`, an opener takes over by CAS on `InitToken` and restarts initialization. An opener that waits longer than `AttachTimeout` for initialization to finish fails with an error matching `ErrFailedInit`; `Dial` retries on it.
3. **Attaching**: On a `Ready` segment with a live Primary whose `Secondary.Token` is `0`, the opener claims the Secondary record via CAS.
4. **Liveness at open**: an endpoint record is alive if its token is an owner token and its heartbeat is younger than `max(PeerTimeout, 4 × Interval)`. A heartbeat that looks stale is confirmed by watching it for `min(3 × Interval, 2s)`, so wall-clock jumps do not declare a live endpoint dead.
5. **Third-endpoint rejection**: If both records are alive, or the live endpoint's earlier peer already closed or died (a dead endpoint may still own ring slots), the opener fails with `ErrLinkBusy`.
6. **Segment reuse**: If neither record is alive, the opener re-initializes the `Ready` segment (`Ready -> Initializing`, step 1) and becomes Primary with a new `Epoch`. `CreateStandardLink`, `CreateAdvancedLink`, and `Listen` never reach this step for an existing name: they create the name exclusively and fail with `os.ErrExist`. `OpenNamed*` and `Dial` do reuse such segments.

### MPMC Ring Layout (v2)

Each directional packet ring uses a 512-byte header (`mpmc.HeaderSize = 512`) split into four 128-byte lines so that independently written words never share a cache line (128 bytes covers both 64-byte and 128-byte cache lines):

```text
Offset   0..127 : magic, size, init flag, slot stride, layout version (uint64 each; immutable after init)
Offset 128..255 : r             (uint64 consumer cursor)
Offset 256..383 : w             (uint64 producer cursor)
Offset 384..399 : notEmpty      (uint32 wake word, bumped by producers)
                  emptyWaiters  (uint32 number of parked consumers)
                  notFull       (uint32 wake word, bumped by consumers)
                  fullWaiters   (uint32 number of parked producers)
Offset 512..end : slots[0..N-1]
```

Each slot is an 8-byte sequence number (`seq uint64`) followed by the 64-byte `protocol.Packet`, rounded up to a 64-byte multiple (stride 128 bytes). An attacher rejects a ring whose magic, layout version, stride, or power-of-two size does not match.

When `WaitAdaptive` (the default `WaitPolicy`) is active, a blocked producer or consumer:

1. re-checks the slot sequence in a tight loop (64 iterations), then
2. yields with `runtime.Gosched` (256 iterations), then
3. increments `emptyWaiters`/`fullWaiters`, loads the wake word, re-checks the sequence, and parks for at most 100 ms (or until its deadline or next liveness check):
   - **Linux**: `futex` on the wake word (`FUTEX_WAIT = 0`, `FUTEX_WAKE = 1`, shared across processes without `FUTEX_PRIVATE_FLAG`)
   - **macOS**: `__ulock_wait` / `__ulock_wake` on the wake word (`UL_COMPARE_AND_WAIT_SHARED = 3`)
   - **Windows, named segments**: there is no cross-process compare-and-wait primitive, so each segment has four named kernel semaphores (`<mapping object name>.hqq-sem0` … `.hqq-sem3`: ring 0 notEmpty/notFull, ring 1 notEmpty/notFull). The waiter calls `WaitForSingleObject` on the semaphore; the waker, after publishing, reads the waiter count `n` and calls `ReleaseSemaphore(n)`. Because a released token persists, a wake that runs between the waiter's re-check and its wait is not lost; tokens left for waiters that already gave up only cause one spurious wake-up.
   - **Windows, unnamed memory**: `WaitOnAddress` / `WakeByAddressAll`, which wake waiters of the same process only; waiters woken from another process notice within a 1–2 ms backoff.
   - **Other platforms**: bounded exponential sleep backoff (`20µs` to `2ms`).

Publishing or releasing a slot issues a kernel wake only when `emptyWaiters > 0` or `fullWaiters > 0`, keeping the uncontended fast path syscall-free.

The core Vyukov MPMC state machine is formally specified in [`internal/mpmc/MPMC.tla`](internal/mpmc/MPMC.tla) and model-checked with TLC via [`scripts/tlc.sh`](scripts/tlc.sh) for writer/reader exclusivity (`NoRace`), payload integrity (`DataIntegrity`), FIFO claim order (`FIFOClaimOrder`), and lock-free system progress (`SystemProgress`).

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

## Standard Protocol 1.0

### `OpStandardLinkTombstone` (`0xFE`)

Marks a claimed slot as intentionally empty.

| Operand | Meaning |
| --- | --- |
| `Operand0` | `CopyID` or reservation ID |
| `Operand1` | `BufferIndex`, diagnostic mirror of the claimed data-ring slot index |
| `Operand2` | `ErrorCode` or reason code |
| `Operand3..6` | Reserved, must be zero |

Tombstones preserve ring progress when a producer reserves a slot but aborts before publishing a payload. Readers skip tombstones and release the slot immediately.

### `OpStandardLinkCopy` (`0xFF`)

Transfers one payload stored in a direction-local buffer pool.

| Operand | Meaning |
| --- | --- |
| `Operand0` | `CopyID`, monotonically increasing per endpoint |
| `Operand1` | `BufferIndex`, diagnostic mirror of the claimed data-ring slot index |
| `Operand2` | `DataSize`, number of valid bytes in the buffer (`1 <= DataSize <= bufferSize`) |
| `Operand3..6` | Reserved, must be zero |

Empty `Write` calls do not publish packets.

### Standard Buffer Ownership

Each direction starts with every data-ring slot available. Slot `i` owns `BUFFERS[i]`.

#### Send path

1. Sender claims one producer slot from the direction's data ring.
2. Sender writes the payload into `BUFFERS[slot]`.
3. Sender stores `OpStandardLinkCopy` in the claimed packet slot (`Operand1` mirrors `slot`). If a reserved write is aborted, sender stores `OpStandardLinkTombstone` instead.
4. Sender publishes the packet by advancing the slot sequence and wakes any parked consumer.

#### Receive path

1. Receiver claims one consumer slot from the direction's data ring.
2. Receiver locates `BUFFERS[slot]` from the claimed slot index and validates `DataSize`.
3. Receiver skips tombstones. If it encounters an Advanced frame (`OpConnCopy`, `OpProtoNegotiate`, etc.) on the raw Standard read path, it releases the slot and returns `ErrUnexpectedPacket`.
4. Returning from the dequeue callback or calling `Release` releases the packet slot and `BUFFERS[slot]` back to producers and wakes any parked producer.

> [!WARNING]
> Zero-copy callbacks and read reservations hold the underlying ring slot. They must finish promptly; if they block or are retained, the whole direction can suffer head-of-line (HOL) blocking. The shared buffer must not be retained after release.

## Advanced Protocol 1.0 Design

The Advanced Protocol is an overlay over Standard Protocol 1.0. It publishes Advanced packets through the same direction-local packet rings, and each Advanced payload chunk uses the same-index buffer owned by the claimed ring slot. Once an `AdvancedLink` is negotiated, direct `StandardLink.Read` and `StandardLink.Write` calls on that link are rejected with `ErrAdvancedActive`.

### Advanced IDs

| ID | Scope | Purpose |
| --- | --- | --- |
| `ConnectionID` | Per link pair | Logical stream or endpoint (`0` is reserved for link-level calls and large transfers). Even non-zero IDs are allocated by Primary (`2, 4, ...`); odd IDs by Secondary (`1, 3, ...`). |
| `MessageID` | Per sender | Large-copy transfer ID or request correlation ID. Responses reuse the request's `MessageID`. |
| `MethodID` | Application-defined | Numeric function/procedure selector for request packets. |

### Advanced Frame Flags (`OpConnCopy.Operand5`)

| Flag | Value | Meaning |
| --- | ---: | --- |
| `AdvFlagBegin` | `1 << 0` | First chunk of a logical message. |
| `AdvFlagEnd` | `1 << 1` | Final chunk of a logical message. |
| `AdvFlagAbort` | `1 << 2` | Sender abandoned this logical message; receiver discards partial state. Aborted requests and data messages are never delivered to `Receive`, `Serve`, or a `MessageHandler`; an aborted response fails the waiting call with `ErrMessageAborted`. |
| `AdvFlagRequest` | `1 << 3` | Chunk belongs to a request message. |
| `AdvFlagResponse` | `1 << 4` | Chunk belongs to a response message. |
| `AdvFlagError` | `1 << 5` | Response represents an application/protocol error. |

For single-chunk messages, both `AdvFlagBegin` and `AdvFlagEnd` are set.

### `OpConnCreate` (`0x01`), `OpConnAccept` (`0x02`), `OpConnClose` (`0x03`)

- `OpConnCreate`: `Operand0 = ConnectionID`, `Operand1 = flags (0)`. Peer replies with `OpConnAccept` (`Operand0 = ConnectionID`) if listening, or `OpError` (`Operand0 = ConnectionID`, `Operand1 = ErrCodeConnNotFound`) if not listening. A create whose ID is `0`, has the wrong parity for the sender's role, or is already in use is refused with `OpError` (`Operand1 = ErrCodeInvalidOp`); the existing connection is left untouched.
- `OpConnClose`: `Operand0 = ConnectionID`, `Operand1 = reason`.

### `OpConnCopy` (`0x04`)

Transfers one Advanced chunk for large-data copy or request-response IPC:

| Operand | Meaning |
| --- | --- |
| `Operand0` | `ConnectionID`, or `0` for link-level traffic |
| `Operand1` | `MessageID` |
| `Operand2` | `TotalSize`, complete logical message size in bytes |
| `Operand3` | `ChunkOffset`, byte offset within the logical message |
| `Operand4` | `ChunkSize`, valid bytes in the claimed slot-owned payload buffer |
| `Operand5` | Advanced frame flags |
| `Operand6` | `MethodID` for request begin frames; status/error code for response/error frames; otherwise zero |

### Protocol Negotiation (`OpProtoNegotiate` `0x05`, `OpProtoAck` `0x06`)

Negotiation is mandatory before using Advanced APIs.
- **Role-agnostic negotiation**: `AdvancedLink.Negotiate(ctx, features)` initiates negotiation on the Primary endpoint and waits for negotiation on the Secondary endpoint.
- **Simultaneous initiation resolution**: If both endpoints call `NegotiateProtocol` concurrently, the Primary's request wins: the Secondary answers the Primary's `OpProtoNegotiate` with `OpProtoAck` (intersection of features), and the Primary ignores the Secondary's request.
- A failed negotiation leaves the link in `ProtocolStateFailed`; it cannot be renegotiated.

### Dispatcher, Late-Response Handling, and Overflow Policy

- **Single-chunk fast path**: Single-frame messages (`AdvFlagBegin | AdvFlagEnd`) allocate and copy directly without inserting into the multi-chunk reassembly map.
- **Reassembly bounds**: `AdvancedOptions.MaxMessageSize` (default 64 MiB) bounds one message's `TotalSize`. The first chunk of a multi-chunk message allocates `TotalSize` bytes and is charged `TotalSize + 256` against `AdvancedOptions.MaxReassemblyBytes` (default `4 × MaxMessageSize`). A first chunk that would exceed the budget is refused without allocating; its remaining chunks are drained. A refused request is answered with an error response (status `ErrCodeBufferOverflow`), a refused response fails the waiting call with `ErrReassemblyLimit`, and a refused data message is dropped; all are counted in `AdvancedStats.DroppedMessages`. At most 1024 refused messages are tracked for draining.
- **Late responses to canceled calls**: When `Call` or `CallOn` times out or its `context.Context` is canceled while waiting for a response, the `(ConnectionID, MessageID)` key is recorded in a bounded ring of canceled calls. Subsequent response chunks for that call are silently drained and discarded rather than aborting the dispatcher.
- **Incoming queue backpressure (`OverflowPolicy`)**:
  - `OverflowBlock` (default): the dispatcher blocks until the application consumes from `Receive` or `Accept`, propagating backpressure into the shared-memory ring. While it blocks, responses are not routed either.
  - `OverflowDrop`: drops the newest incoming non-response message when the queue (`AdvancedOptions.IncomingQueueSize`, default `bufferCount`) is full.
  - `OverflowFail`: fails the dispatcher with `ErrIncomingOverflow`.

## Error Hierarchy

All public errors support `errors.Is` against the sentinel itself and against the base it wraps:

| Sentinel | Wraps | Meaning |
| --- | --- | --- |
| `io.EOF` (returned as is) | — | `StandardLink.Read` after the peer closed gracefully and every buffered message was consumed |
| `ErrClosed` | `net.ErrClosed` | Local link was closed |
| `ErrPeerClosed` | `io.EOF` | Remote peer closed its link gracefully (all APIs except `StandardLink.Read`) |
| `ErrPeerGone` | `ErrConnNotFound` | Remote peer stopped heartbeating (crashed or hung), or the segment was re-initialized under this endpoint |
| `ErrTimeout` | `os.ErrDeadlineExceeded` | Deadline passed (implements `net.Error` with `Timeout() == true`) |
| `ErrLinkBusy` | `ErrFailedInit` | Two live endpoints are attached, or the live endpoint's earlier peer already left |
| `ErrIncompatibleLayout` | `ErrFailedInit` | Foreign magic, unknown superblock state, or ring header that does not match this HQQ version |
| `ErrInvalidSize` | — | Invalid geometry, or geometry that differs from an existing segment |
| `ErrInvalidName` | — | Shared memory name is empty, contains separators/NUL, or exceeds the platform limit |
| `ErrNotNegotiated` | `ErrInvalidOp` | Advanced API called before protocol negotiation succeeded |
| `ErrAlreadyNegotiated` | `ErrInvalidOp` | Protocol negotiation attempted more than once |
| `ErrNegotiationRejected` | `ErrInvalidOp` | Peer rejected negotiation or acknowledged an incompatible version/feature set |
| `ErrUnexpectedPacket` | `ErrInvalidOp` | Packet that the calling API cannot consume (for example an Advanced frame on `StandardLink.Read`) |
| `ErrProtocol` | `ErrInvalidOp` | Malformed frame bounds or protocol state violation |
| `ErrMessageAborted` | `ErrProtocol` | `Call`/`CallOn`: the peer abandoned the response mid-transfer |
| `ErrAdvancedActive` | `ErrInvalidOp` | Standard `Read`/`Write` called on a negotiated `AdvancedLink` |
| `ErrNotListening` / `ErrAlreadyListening` | `ErrInvalidOp` | `Accept` before `Listen` / second `Listen` |
| `ErrIncomingOverflow` | `ErrBufferOverflow` | Incoming queue exceeded `IncomingQueueSize` under `OverflowFail` |
| `ErrReassemblyLimit` | `ErrBufferOverflow` | `Call`/`CallOn`: the response was refused under `MaxReassemblyBytes` |
