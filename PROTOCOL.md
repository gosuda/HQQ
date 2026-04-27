# HQQ Protocol Specification

## Scope

This document defines the HQQ shared-memory protocol family:

- **Standard Protocol 1.0**: the `StandardLink` data path. It provides the fixed packet ABI, bidirectional lock-free rings, ring-slot-owned payload buffers, tombstones, and the basic `Read`/`Write`/zero-copy/reservation APIs.
- **Advanced Protocol 1.0**: an optional overlay on top of the Standard Protocol. It keeps the same shared-memory layout and packet ABI, and adds only two protocol capabilities for now: large-data copy and request-response IPC.

The Advanced Protocol must not change Standard Protocol ownership rules. Every payload byte still lives in the same-index buffer owned by the claimed data-ring slot, and every reader must release the claimed slot promptly.

## Shared Design Goals

1. **Fixed ABI**: packets have a stable byte layout independent of Go struct padding or `uintptr` width.
2. **Little-endian encoding**: all packet words are encoded with `binary.LittleEndian`.
3. **Lock-free ownership transfer**: data ring slots own same-index payload buffers; no mutex is required for the StandardLink hot path.
4. **No unread-buffer overwrite**: a payload buffer is recycled only after the receiver releases the owning ring slot.
5. **Bounded memory**: all rings and payload buffers are allocated up front.
6. **Composable dispatch**: automatic copy, zero-copy callback, and explicit reserve/commit APIs all operate on the same packet stream.

## Shared Memory Layout

The protocol allocates two independent directions. Each direction has:

- one packet ring for published data packets
- one fixed-size payload buffer pool

```text
PAGE_START
  DATA_RING_0   protocol.Packet ring, Primary -> Secondary
PAGE_BREAK
  BUFFERS_0     bufferCount * bufferSize bytes, Primary -> Secondary payloads
PAGE_BREAK
  DATA_RING_1   protocol.Packet ring, Secondary -> Primary
PAGE_BREAK
  BUFFERS_1     bufferCount * bufferSize bytes, Secondary -> Primary payloads
PAGE_END
```

All regions are page-aligned. `bufferCount` must be a power of two and at least 2. `bufferSize` must be at least 8 bytes and a multiple of 8. For each direction, `BUFFERS[i]` is owned by data-ring slot `i`.

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

Tombstones preserve ring progress when a producer reserves a slot but cannot publish a payload. Readers skip tombstones and release the slot immediately.

### `OpStandardLinkCopy` (`0xFF`)

Transfers one payload stored in a direction-local buffer pool.

| Operand | Meaning |
| --- | --- |
| `Operand0` | `CopyID`, monotonically increasing per endpoint |
| `Operand1` | `BufferIndex`, diagnostic mirror of the claimed data-ring slot index |
| `Operand2` | `DataSize`, number of valid bytes in the buffer |
| `Operand3..6` | Reserved, must be zero |

`DataSize` must satisfy `1 <= DataSize <= bufferSize`. Empty `Write` calls do not publish packets.

### Standard Buffer Ownership

Each direction starts with every data-ring slot available. Slot `i` owns `BUFFERS[i]`.

#### Send path

1. Sender claims one producer slot from the direction's data ring.
2. Sender writes the payload into the same-index payload buffer, `BUFFERS[slot]`.
3. Sender stores `OpStandardLinkCopy` in the claimed packet slot. `Operand1` mirrors `slot`. If the reserved write cannot be completed, sender stores `OpStandardLinkTombstone` instead.
4. Sender publishes the packet by advancing the slot sequence.

#### Receive path

1. Receiver claims one consumer slot from the direction's data ring.
2. Receiver uses the claimed slot index to locate `BUFFERS[slot]` and validates `DataSize`.
3. Receiver skips tombstones. For copy packets, it either copies payload bytes out of shared memory (`Read`) or passes the shared buffer directly to the callback (`ReadZeroCopy`/`ReserveRead`).
4. Returning from the dequeue callback or calling `Release` releases the packet slot and `BUFFERS[slot]` back to producers.

This ties payload lifetime to ring-slot lifetime and prevents a producer from overwriting unread payloads.

> WARNING: Zero-copy callbacks and read reservations hold the underlying ring slot. They must finish promptly; if they block or are retained, the whole direction can suffer head-of-line (HOL) blocking. The shared buffer must not be retained after release.

### Standard Backpressure

The protocol has one bounded backpressure point per direction:

- **Data ring full**: every slot and therefore every same-index payload buffer is currently in flight; writers wait until receivers release slots.

`StandardLink` exposes write deadlines through `net.PacketConn` deadline methods. On timeout, it returns `ErrTimeout` if no slot was claimed before the deadline. After a zero-copy callback starts, cancellation is not checked until the callback returns because the slot must be published or released to preserve ring progress.

### StandardLink Semantics

- `Write` publishes at most one packet and rejects payloads larger than `bufferSize` with `ErrBufferOverflow`.
- `Read` returns after delivering data from one packet, or from a previously saved local remainder.
- `Read` and `Write` are automatic-release/automatic-commit hot-path APIs for the same packet stream used by the explicit reservation APIs.
- `WriteZeroCopy` and `ReadZeroCopy` expose the slot-owned payload buffer directly for the duration of the callback.
- `ReserveWrite` and `ReserveRead` expose explicit slot lifetime control. `ReserveWrite` must be finished with `Commit` or `Abort`; `ReserveRead` must be finished with `Release`.
- Dispatch methods can be mixed at message boundaries: `Write`, `WriteZeroCopy`, and `ReserveWrite` publish copy packets; `Read`, `ReadZeroCopy`, and `ReserveRead` consume copy packets and skip tombstones.
- If the caller's read buffer is smaller than the payload, the unread tail is copied into a local per-link remainder buffer and the shared payload buffer is released as soon as the dequeue callback returns.
- `ReadFrom` and `WriteTo` are thin `net.PacketConn` adapters over `Read` and `Write`.

## Advanced Protocol 1.0 Design

The Advanced Protocol is an overlay over Standard Protocol 1.0. It does not allocate additional rings or buffer pools. It publishes Advanced packets through the same direction-local packet rings, and each Advanced payload chunk uses the same-index buffer owned by the claimed ring slot. Do not consume Advanced frames with Standard `Read`/`ReadZeroCopy` APIs in the same direction; use `AdvancedLink.Receive`, `Call`, or `CallOn` for Advanced traffic.

Advanced packets are intended for users that need either:

1. payloads larger than `bufferSize`
2. request-response IPC, including function-call-style dispatch

### Advanced IDs

| ID | Scope | Purpose |
| --- | --- | --- |
| `ConnectionID` | Per link pair | Logical stream or endpoint. `0` is reserved for link-level request-response without an explicit stream. |
| `MessageID` | Per sender | Large-copy transfer ID or request correlation ID. Responses reuse the request's `MessageID`. |
| `MethodID` | Application-defined | Numeric function/procedure selector for request packets. |

### Advanced Frame Flags

`OpConnCopy.Operand5` stores a bitmask of frame flags:

| Flag | Value | Meaning |
| --- | ---: | --- |
| `AdvFlagBegin` | `1 << 0` | First chunk of a logical message. |
| `AdvFlagEnd` | `1 << 1` | Final chunk of a logical message. |
| `AdvFlagAbort` | `1 << 2` | Sender abandoned this logical message; receiver should discard partial state. |
| `AdvFlagRequest` | `1 << 3` | Chunk belongs to a request message. |
| `AdvFlagResponse` | `1 << 4` | Chunk belongs to a response message. |
| `AdvFlagError` | `1 << 5` | Response represents an application/protocol error. |

For single-chunk messages, both `AdvFlagBegin` and `AdvFlagEnd` are set.

### `OpConnCreate` (`0x01`)

Creates a logical Advanced stream.

| Operand | Meaning |
| --- | --- |
| `Operand0` | `ConnectionID` |
| `Operand1` | Stream flags, reserved for now and written as zero |
| `Operand2..6` | Reserved, must be zero |

The peer replies with `OpConnAccept` for the same `ConnectionID`, or `OpError` if the stream cannot be created.

### `OpConnAccept` (`0x02`)

Accepts a logical Advanced stream.

| Operand | Meaning |
| --- | --- |
| `Operand0` | `ConnectionID` |
| `Operand1..6` | Reserved, must be zero |

### `OpConnClose` (`0x03`)

Closes a logical Advanced stream.

| Operand | Meaning |
| --- | --- |
| `Operand0` | `ConnectionID` |
| `Operand1` | Close reason code, or zero for normal close |
| `Operand2..6` | Reserved, must be zero |

### `OpConnCopy` (`0x04`)

Transfers one Advanced chunk. This is the common data frame for both large-data copy and request-response IPC.

| Operand | Meaning |
| --- | --- |
| `Operand0` | `ConnectionID`, or `0` for link-level request-response |
| `Operand1` | `MessageID` |
| `Operand2` | `TotalSize`, the complete logical message size in bytes |
| `Operand3` | `ChunkOffset`, byte offset within the logical message |
| `Operand4` | `ChunkSize`, valid bytes in the claimed slot-owned payload buffer |
| `Operand5` | Advanced frame flags |
| `Operand6` | `MethodID` for request begin frames; status/error code for response/error frames; otherwise zero |

`ChunkSize` must satisfy `1 <= ChunkSize <= bufferSize`, except an empty logical message is encoded as a single `Begin|End` frame with `TotalSize == 0`, `ChunkOffset == 0`, and `ChunkSize == 0`. The receiver must use the claimed ring slot to locate non-empty payload bytes. `Operand3 + Operand4` must not exceed `Operand2`.

## Advanced Large-Data Copy

Large-data copy sends one logical message as one or more `OpConnCopy` chunks. A payload is considered a large message when `TotalSize > bufferSize`; this value is exposed as `LargeMessageThreshold(bufferSize)` and `link.LargeMessageThreshold()` in the public API.

### Send path

1. Allocate a new `MessageID`.
2. Split the payload into chunks where each `ChunkSize <= bufferSize`.
3. For each chunk, claim a producer slot and copy/write the chunk into `BUFFERS[slot]`.
4. Publish `OpConnCopy` with the same `ConnectionID`, same `MessageID`, same `TotalSize`, increasing `ChunkOffset`, and the chunk's `ChunkSize`.
5. Set `AdvFlagBegin` on offset `0`; set `AdvFlagEnd` on the final chunk.
6. If the sender cannot finish after publishing one or more chunks, publish an `AdvFlagAbort` frame for the same `MessageID`.

### Receive path

The receiver may choose either assembly or streaming:

- **Assembly mode**: allocate a destination of `TotalSize`, copy each chunk into `dst[ChunkOffset:ChunkOffset+ChunkSize]`, and deliver once `AdvFlagEnd` arrives and all bytes are present.
- **Streaming mode**: deliver chunks to the application in arrival order for the same `MessageID`. This avoids a second full-size allocation and is preferred for very large data.

A receiver must discard partial state if it receives `AdvFlagAbort`, invalid bounds, duplicate chunk ranges, or a stream close for the chunk's `ConnectionID`.

### Ordering

The underlying ring preserves dequeue order per direction, but different `MessageID`s may be interleaved. Receivers must key reassembly state by `(ConnectionID, MessageID)` and must not assume only one large copy is active.

## Advanced Request-Response IPC

Request-response uses `OpConnCopy` chunks with `AdvFlagRequest` or `AdvFlagResponse`. A request body and response body may each be larger than `bufferSize`; in that case they use the same large-data chunking rules.

### Request frame

| Field | Meaning |
| --- | --- |
| `ConnectionID` | Logical stream, or `0` for link-level calls |
| `MessageID` | Request ID generated by the caller |
| `TotalSize` | Encoded request body size |
| `ChunkOffset` / `ChunkSize` | Chunk bounds |
| `Flags` | `AdvFlagRequest`, plus `Begin`/`End` as appropriate |
| `Operand6` | `MethodID` on the begin frame; zero on continuation frames |

The request body encoding is application-defined. The protocol treats it as opaque bytes.

### Response frame

| Field | Meaning |
| --- | --- |
| `ConnectionID` | Same value used by the request |
| `MessageID` | Same request ID, used as the correlation ID |
| `TotalSize` | Encoded response body size |
| `ChunkOffset` / `ChunkSize` | Chunk bounds |
| `Flags` | `AdvFlagResponse`, plus `Begin`/`End` as appropriate |
| `Operand6` | Status code, or error code when `AdvFlagError` is set |

A successful response uses `AdvFlagResponse`. A failed call uses `AdvFlagResponse | AdvFlagError`; the payload may contain an application-defined diagnostic body.

### Request lifecycle

1. Caller assigns `MessageID` and publishes request chunk(s).
2. Callee reassembles or streams the request body, then dispatches by `MethodID`.
3. Callee publishes response chunk(s) using the same `MessageID`.
4. Caller matches response chunks to the pending call by `(ConnectionID, MessageID)`.
5. Either side may close the stream with `OpConnClose`; pending calls on that stream fail with `ErrCodeConnNotFound` or a close-specific error mapping.

### Concurrency

Multiple requests may be outstanding on the same `ConnectionID`. Responses may arrive in a different order than requests. The caller must maintain a pending-call table keyed by `(ConnectionID, MessageID)`.

## Advanced Protocol FSM Model

The implementation may optimize these flows without materializing state-machine objects, but observable behavior should follow these FSMs.

### Negotiation FSM

| State | Event | Action | Next State |
| --- | --- | --- | --- |
| `None` | local `NegotiateProtocol` | send `OpProtoNegotiate` | `Negotiating` |
| `None` | local `WaitForNegotiation` | wait for `OpProtoNegotiate` | `Negotiating` |
| `Negotiating` | receive compatible negotiate request | intersect feature/capability bits, send `OpProtoAck` | `Negotiated` |
| `Negotiating` | receive compatible ack | store negotiated version/features/capabilities | `Negotiated` |
| `Negotiating` | context expires or peer rejects | return error | `Failed` |
| `Negotiated` | duplicate negotiation attempt | reject locally | `Negotiated` |
| `Failed` | any negotiation attempt | reject locally; caller must create a new link | `Failed` |

### Large-Copy Sender FSM

| State | Event | Action | Next State |
| --- | --- | --- | --- |
| `Idle` | `SendLarge(payload)` | allocate `MessageID`, compute chunks | `ReserveChunk` |
| `ReserveChunk` | producer slot claimed | copy chunk into `BUFFERS[slot]` | `PublishChunk` |
| `PublishChunk` | chunk ready | publish `OpConnCopy`; set `Begin` and/or `End` as needed | `Done` if final, otherwise `ReserveChunk` |
| `ReserveChunk` | context expires before slot claim | return timeout/cancel error | `Failed` |
| `PublishChunk` | local validation fails after earlier chunks | attempt `AdvFlagAbort` for same `MessageID` | `Failed` |
| `Done` | return | report `MessageID` | `Idle` |

### Large-Copy Receiver FSM

| State | Event | Action | Next State |
| --- | --- | --- | --- |
| `Idle` | receive `Begin` chunk | create assembly keyed by `(ConnectionID, MessageID, kind)` | `Assembling` or `Complete` when `End` is also set |
| `Assembling` | receive next valid chunk | copy bytes to `Payload[ChunkOffset:]` | `Assembling` |
| `Assembling` | receive `End` and all bytes present | deliver complete `AdvancedMessage` | `Complete` |
| `Assembling` | receive `Abort`, invalid bounds, duplicate/out-of-order range, or close | discard assembly | `Failed` |
| `Complete` | caller consumes message | release assembly state | `Idle` |
| `Failed` | error returned or abort delivered | release assembly state | `Idle` |

### Request Caller FSM

| State | Event | Action | Next State |
| --- | --- | --- | --- |
| `Idle` | `Call`/`CallOn` | allocate `MessageID`, send request chunks with `AdvFlagRequest` | `WaitingResponse` |
| `WaitingResponse` | receive non-matching complete message | stash in pending queue | `WaitingResponse` |
| `WaitingResponse` | receive matching `AdvFlagResponse` | return payload/status | `Done` |
| `WaitingResponse` | receive matching `AdvFlagResponse|AdvFlagError` | return `AdvancedResponseError` or error message | `Done` |
| `WaitingResponse` | context expires | return timeout/cancel error | `Failed` |
| `Done` | caller returns | remove pending call state | `Idle` |

### Request Callee FSM

| State | Event | Action | Next State |
| --- | --- | --- | --- |
| `Idle` | receive complete `AdvFlagRequest` | deliver `AdvancedMessage` to application dispatcher | `Dispatching` |
| `Dispatching` | application produces successful result | send response chunks with `AdvFlagResponse` | `Responding` |
| `Dispatching` | application produces error | send response chunks with `AdvFlagResponse|AdvFlagError` | `Responding` |
| `Responding` | final response chunk published | clear request-local state | `Idle` |
| `Dispatching`/`Responding` | context expires or stream closes | return error and discard local response state | `Failed` |
| `Failed` | error observed | clear request-local state | `Idle` |

## Protocol Negotiation

`OpProtoNegotiate` and `OpProtoAck` are reserved for enabling the Advanced Protocol over a `StandardLink` pair.

### `OpProtoNegotiate` (`0x05`)

| Operand | Meaning |
| --- | --- |
| `Operand0` | Protocol version encoded as `(major << 8) | minor` |
| `Operand1` | Requested feature flags |
| `Operand2` | Supported capability flags |
| `Operand3..6` | Reserved, must be zero |

### `OpProtoAck` (`0x06`)

| Operand | Meaning |
| --- | --- |
| `Operand0` | Accepted protocol version encoded as `(major << 8) | minor` |
| `Operand1` | Negotiated feature flags |
| `Operand2` | Negotiated capability flags |
| `Operand3..6` | Reserved, must be zero |

### Advanced feature flags

Only the following feature bits are defined for this design:

| Feature | Value | Meaning |
| --- | ---: | --- |
| `FeatureLargeCopy` | `1 << 0` | Enables `OpConnCopy` chunking for payloads larger than `bufferSize`. |
| `FeatureRequestResponse` | `1 << 1` | Enables request-response IPC over `OpConnCopy`. |

Undefined feature bits must be ignored by the receiver and must not be echoed in `OpProtoAck`.

### Advanced capability flags

Only the following capability bits are defined for this design:

| Capability | Value | Meaning |
| --- | ---: | --- |
| `CapabilityLargeBuffers` | `1 << 0` | Endpoint supports large configured payload buffers. |
| `CapabilityHighThroughput` | `1 << 1` | Endpoint is optimized for high-throughput chunk streams. |
| `CapabilityLowLatency` | `1 << 2` | Endpoint is optimized for low-latency request-response calls. |

## Error Codes

| Error Code | Hex | Meaning |
| --- | ---: | --- |
| `ErrCodeMemoryAlign` | `0x01` | Shared memory or buffer alignment violation |
| `ErrCodeInvalidSize` | `0x02` | Invalid buffer count, buffer size, buffer index, or chunk bounds |
| `ErrCodeMemorySmall` | `0x03` | Shared memory region is too small |
| `ErrCodeFailedInit` | `0x04` | Ring initialization or attachment failed |
| `ErrCodeConnNotFound` | `0x05` | Connection or request context not found |
| `ErrCodeInvalidOp` | `0x06` | Unknown or unsupported opcode, method, or frame flag combination |
| `ErrCodeBufferOverflow` | `0x07` | Payload or assembled message exceeds configured or negotiated limits |

## Current Implementation Status

Implemented and covered by tests:

- fixed-size little-endian packet ABI
- StandardLink primary/secondary creation
- bidirectional StandardLink read/write
- slot-owned payload-buffer transfer
- zero-copy read/write callback APIs
- Reserve/Commit read/write APIs
- tombstone packets for abandoned write reservations
- unread-buffer overwrite regression coverage
- partial-read remainder handling
- read/write deadline timeout behavior
- mixed dispatch coverage across `Read`, `Write`, zero-copy, and reserve APIs
- high-volume ordered delivery smoke test
- StandardLink throughput benchmarks

Implemented and covered by Advanced tests/benchmarks:

- Advanced Protocol negotiation using only `FeatureLargeCopy` and `FeatureRequestResponse`
- Advanced large-data copy over `OpConnCopy`
- Advanced request-response IPC over `OpConnCopy`
- public large-message threshold APIs
- multi-process StandardLink and AdvancedLink smoke tests
- Advanced error responses through `AdvFlagResponse | AdvFlagError`
- Advanced packet constructor coverage and throughput benchmarks

Designed but not implemented yet:

- explicit logical stream lifecycle APIs for Advanced `ConnectionID`s
- streaming large-copy receive callbacks that avoid full message assembly
- cross-endian mixed-architecture process pairs beyond the packet ABI
