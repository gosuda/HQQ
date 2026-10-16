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
7. **Deterministic lifecycle & crash detection**: endpoints advertise PID and heartbeat metadata in the superblock so peers distinguish graceful shutdown (`ErrPeerClosed`) from abrupt process termination (`ErrPeerGone`).

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

The first page is a shared initialization, configuration, and endpoint-liveness superblock (`superblockMagic = 0x4851515F53544C32`, `"HQQ_STL2"`):

| Offset | Field | Type | Meaning |
| ---: | --- | --- | --- |
| `0` | `Magic` | `uint64` | `0x4851515F53544C32` (`"HQQ_STL2"`) |
| `8` | `State` | `uint32` | `0` (`Empty`), `1` (`Initializing`), or `2` (`Ready`) |
| `12` | `Major` | `uint16` | Standard protocol major version (`1`) |
| `14` | `Minor` | `uint16` | Standard protocol minor version (`0`) |
| `16` | `PageSize` | `uint64` | OS page size used to compute region boundaries |
| `24` | `BufferCount` | `uint64` | Configured ring-slot/buffer count per direction |
| `32` | `BufferSize` | `uint64` | Configured bytes per slot-owned payload buffer |
| `40` | `LayoutSize` | `uint64` | Total bytes returned by `SizeStandardLink` |
| `48` | `Epoch` | `uint64` | Monotonic generation counter incremented on re-initialization |
| `56` | `InitPID` | `uint64` | OS PID of the endpoint currently initializing the segment |
| `64` | `InitHeartbeat` | `uint64` | Monotonic wall-clock timestamp (`UnixNano`) refreshed during initialization |
| `128` | `Primary` | `64B` | Primary endpoint liveness record (`Token`, `PID`, `Heartbeat`, `Interval`) |
| `192` | `Secondary` | `64B` | Secondary endpoint liveness record (`Token`, `PID`, `Heartbeat`, `Interval`) |

#### Endpoint Liveness Record (64 bytes, cache-line aligned)

| Offset | Field | Type | Meaning |
| ---: | --- | --- | --- |
| `+0` | `Token` | `uint64` | `0` = never attached, `1` = gracefully detached (`detachedToken`), `>= 2` = active session token |
| `+8` | `PID` | `uint64` | OS process ID of the attached endpoint |
| `+16` | `Heartbeat` | `uint64` | Last heartbeat timestamp (`UnixNano`) |
| `+24` | `Interval` | `uint64` | Configured heartbeat interval in nanoseconds |

#### Initialization, Takeover, and Stale Segment Reuse

1. **Primary initialization**: The primary endpoint claims `State` via CAS (`Empty -> Initializing`), records `InitPID` and `InitHeartbeat`, formats both MPMC rings, zeros both payload pools while periodically refreshing `InitHeartbeat`, writes the layout fields, claims the `Primary` endpoint slot, and publishes `State = Ready`.
2. **Initializer crash takeover**: If the initializing process crashes before publishing `State = Ready`, a subsequent opener detects that `InitPID` is no longer alive (or that `InitHeartbeat` has expired past `AttachTimeout`) and safely takes over initialization.
3. **Third-endpoint rejection**: At most one Primary and one Secondary endpoint may be active simultaneously. If both endpoint slots hold live tokens, a third opener fails immediately with `ErrLinkBusy`.
4. **Stale segment re-initialization**: When `CreateStandardLink` or `CreateAdvancedLink` opens an existing named segment whose previous endpoints are both dead or detached, it automatically increments `Epoch`, re-formats the rings, and re-initializes the superblock.

### MPMC Ring Layout (v2)

Each directional packet ring uses a 512-byte header (`mpmc.HeaderSize = 512`) split into four 128-byte cache-line-isolated regions to prevent false sharing between producers, consumers, and cross-process park/wake words:

```text
Offset   0..127 : w            (uint64 write cursor)
Offset 128..255 : notEmpty     (uint32 sequence) + emptyWaiters (uint32 count)
Offset 256..383 : r            (uint64 read cursor)
Offset 384..511 : notFull      (uint32 sequence) + fullWaiters  (uint32 count)
Offset 512..end : slots[0..N-1], each rounded up to a 64-byte multiple
```

Each slot consists of an 8-byte sequence number (`seq uint64`) followed by the 64-byte `protocol.Packet` and 56 bytes of padding (`slotStride = 128` bytes), guaranteeing that no two slots share a 64-byte cache line.

When `WaitAdaptive` (the default `WaitPolicy`) is active:
- Contended producers/consumers spin briefly with CPU pause hints (` PAUSE` on amd64, `YIELD` on arm64), then increment the corresponding waiter count (`emptyWaiters` or `fullWaiters`) and park in the OS kernel:
  - **Linux**: `futex` (`FUTEX_WAIT = 0`, `FUTEX_WAKE = 1`, shared across processes without `FUTEX_PRIVATE_FLAG`)
  - **macOS**: `__ulock_wait` / `__ulock_wake` (`UL_COMPARE_AND_WAIT_SHARED = 3`)
  - **Other platforms / Windows**: bounded exponential sleep backoff (`20µs` to `2ms`)
- Publishing a slot or releasing a slot bumps `notEmpty` or `notFull` and issues a kernel wake only when `emptyWaiters > 0` or `fullWaiters > 0`, keeping the uncontended fast path completely syscall-free.

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
| `AdvFlagAbort` | `1 << 2` | Sender abandoned this logical message; receiver discards partial state. |
| `AdvFlagRequest` | `1 << 3` | Chunk belongs to a request message. |
| `AdvFlagResponse` | `1 << 4` | Chunk belongs to a response message. |
| `AdvFlagError` | `1 << 5` | Response represents an application/protocol error. |

For single-chunk messages, both `AdvFlagBegin` and `AdvFlagEnd` are set.

### `OpConnCreate` (`0x01`), `OpConnAccept` (`0x02`), `OpConnClose` (`0x03`)

- `OpConnCreate`: `Operand0 = ConnectionID`, `Operand1 = flags (0)`. Peer replies with `OpConnAccept` (`Operand0 = ConnectionID`) if listening, or `OpError` (`Operand0 = ConnectionID`, `Operand1 = ErrCodeConnNotFound`) if not listening.
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
- **Role-agnostic negotiation**: `AdvancedLink.Negotiate(ctx, features)` automatically initiates negotiation on the Primary endpoint and waits for negotiation on the Secondary endpoint.
- **Simultaneous initiation resolution**: If both endpoints call `NegotiateProtocol` concurrently, each endpoint in state `Negotiating` that receives a peer's `OpProtoNegotiate` packet computes the intersection of features/capabilities, replies with `OpProtoAck`, and transitions to `Negotiated` without deadlocking.

### Dispatcher, Late-Response Handling, and Overflow Policy

- **Single-chunk fast path**: Single-frame messages (`AdvFlagBegin | AdvFlagEnd`) allocate and copy directly without inserting into the multi-chunk reassembly map.
- **Late responses to canceled calls**: When `Call` or `CallOn` times out or its `context.Context` is canceled while waiting for a response, the `(ConnectionID, MessageID)` key is recorded in a bounded ring of canceled calls. Subsequent response chunks for that call are silently drained and discarded rather than aborting the dispatcher.
- **Incoming queue backpressure (`OverflowPolicy`)**:
  - `OverflowBlock` (default): the dispatcher blocks until the application consumes from `Receive` or `Accept`, naturally propagating backpressure into the shared-memory ring.
  - `OverflowDrop`: drops the newest incoming non-response message when the queue (`MaxIncomingMessages`) is full.
  - `OverflowFail`: fails the dispatcher with `ErrIncomingOverflow`.

## Error Hierarchy

All public errors support `errors.Is` both against specific HQQ sentinels and against standard library sentinels (`io.EOF`, `net.ErrClosed`, `os.ErrDeadlineExceeded`):

| Sentinel | Unwraps To | Meaning |
| --- | --- | --- |
| `ErrClosed` | `net.ErrClosed` | Local link was closed |
| `ErrPeerClosed` | `io.EOF`, `net.ErrClosed` | Remote peer closed its link gracefully |
| `ErrPeerGone` | `io.EOF`, `net.ErrClosed` | Remote peer process exited or crashed without closing |
| `ErrTimeout` | `os.ErrDeadlineExceeded` | Operation timed out (implements `net.Error` with `Timeout() == true`) |
| `ErrLinkBusy` | `ErrFailedInit` | Both endpoint slots are already occupied by live processes |
| `ErrIncompatibleLayout` | `ErrInvalidSize` | Existing shared memory layout parameters do not match caller arguments |
| `ErrInvalidName` | — | Shared memory name is empty, contains slashes/NUL, or exceeds OS length limits |
| `ErrNotNegotiated` | `ErrInvalidOp` | Advanced API called before protocol negotiation succeeded |
| `ErrAlreadyNegotiated` | `ErrInvalidOp` | Protocol negotiation attempted more than once |
| `ErrNegotiationRejected` | `ErrFailedInit` | Peer rejected protocol negotiation |
| `ErrUnexpectedPacket` | `ErrInvalidOp` | Unexpected opcode observed on the active link mode |
| `ErrProtocol` | `ErrInvalidOp` | Malformed frame bounds or protocol state violation |
| `ErrAdvancedActive` | `ErrInvalidOp` | Standard `Read`/`Write` called on a negotiated `AdvancedLink` |
| `ErrIncomingOverflow` | `ErrBufferOverflow` | Incoming queue exceeded `MaxIncomingMessages` under `OverflowFail` |
