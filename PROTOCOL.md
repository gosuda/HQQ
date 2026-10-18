# HQQ Protocol Overview

This document provides a human-readable architectural tour of the HQQ shared-memory protocol family. It is descriptive rather than normative: where this overview and a specification under `spec/` differ, the normative specification governs.

| Document | Scope |
| :--- | :--- |
| [`spec/SPEC.md`](spec/SPEC.md) | Normative specification of the segment layout, superblock, endpoint roles, initialization, liveness, MPMC ring, wait/wake protocol, Standard and Advanced Protocol 1.0, platform naming, and error codes. |
| [`spec/MEMORY_MODEL.md`](spec/MEMORY_MODEL.md) | Required atomic memory orders for every shared word and the four-step wait/wake handshake. |
| [`spec/PORTING.md`](spec/PORTING.md) | Implementation guide and conformance levels for porting HQQ to a new language. |
| [`spec/ext/PROTOCOL_1_1_DRAFT.md`](spec/ext/PROTOCOL_1_1_DRAFT.md) | Advanced Protocol 1.1 extensions: `FeatureCancel`, `FeatureMetadata`, `OpCancel`, handler lifecycle, and graceful shutdown. |
| [`spec/ext/LISTENER_DRAFT.md`](spec/ext/LISTENER_DRAFT.md) | The listener segment (`HQQ_LSN1`) used by the multi-client acceptor. |
| [`spec/constants.json`](spec/constants.json) | Machine-readable protocol constants shared across language test suites. |
| [`conformance/`](conformance/) | Golden byte vectors generated from the Go implementation. |

## Versions

Every HQQ structure carries an explicit version tag so incompatible layouts or protocol revisions fail fast at attach or negotiation time:

| Component | Version / Magic | Where it is carried |
| :--- | :--- | :--- |
| Segment layout | `3.0`, magic `HQQ_STL3` | Superblock `Magic`, `Major`, `Minor`. Any other magic or major version is rejected with `ErrIncompatibleLayout`. |
| MPMC ring layout | `2` | Ring header; an attacher rejects any other value. |
| Standard Protocol | `1.0` | Standard packet frames (`OpStandardLinkCopy`, `OpStandardLinkTombstone`). |
| Advanced Protocol | `1.1` | `OpProtoNegotiate` and `OpProtoAck` operand 0, encoded as `(major << 8) \| minor`. Features are negotiated bit-by-bit, so a 1.0 peer negotiates 1.0. |
| Listener layout | `1.0`, magic `HQQ_LSN1` | Listener segment header. |

## Shared Memory Layout

A point-to-point link occupies a single page-aligned shared-memory segment divided into a first-page superblock and two directional channels. Each channel pairs one MPMC packet ring with one fixed-slot payload buffer pool:

```text
SUPERBLOCK    one page: layout version, state (Empty, Initializing, Ready), geometry,
              epoch, initializer token and heartbeat, creator token, and two
              128-byte endpoint records (token, PID, heartbeat, interval)
DATA_RING_0   Primary -> Secondary packet ring
BUFFERS_0     bufferCount x bufferSize payload bytes; slot i owns BUFFERS_0[i]
DATA_RING_1   Secondary -> Primary packet ring
BUFFERS_1     bufferCount x bufferSize payload bytes; slot i owns BUFFERS_1[i]
```

`bufferCount` must be a power of two (`2 <= bufferCount <= 1<<24`) and `bufferSize` must be a multiple of 8 bytes (`8 <= bufferSize <= 1<<30`). Exact byte offsets, the superblock state machine, and the claim, takeover, and segment-reuse rules are specified in [`spec/SPEC.md`](spec/SPEC.md) §2 through §5.

Each packet ring begins with a 512-byte cache-line-isolated header followed by `bufferCount` slots of stride 128 bytes (an 8-byte sequence word, a 64-byte packet, and 56 bytes of padding). A producer claims slot `i`, writes its payload into `BUFFERS[i]`, and publishes the 64-byte packet. A consumer claims slot `i`, copies or inspects `BUFFERS[i]` in place, and releases the slot. Because `BUFFERS[i]` cannot be claimed by a subsequent producer until the consumer releases slot `i`, unread payloads are never overwritten. Full ring invariants live in [`spec/SPEC.md`](spec/SPEC.md) §7.

## Packet ABI

Every ring packet is a fixed 64-byte frame consisting of eight little-endian `uint64` words. Word 0 carries the opcode in its lowest byte (bits 8..63 are reserved, written as zero, and ignored on receipt). Words 1 through 7 carry operands 0 through 6. Unused operands are zeroed at ring format time and ignored by receivers.

| Opcode | Value | Layer | Meaning |
| :--- | ---: | :--- | :--- |
| `OpError` | `0x00` | both | Context ID and `ErrorCode`. |
| `OpConnCreate` | `0x01` | Advanced | Requests opening a logical stream. |
| `OpConnAccept` | `0x02` | Advanced | Accepts a stream open request. |
| `OpConnClose` | `0x03` | Advanced | Closes a logical stream from one side. |
| `OpConnCopy` | `0x04` | Advanced | Transfers one chunk of a message, request, or response. |
| `OpProtoNegotiate` | `0x05` | Advanced | Initiates Advanced Protocol negotiation. |
| `OpProtoAck` | `0x06` | Advanced | Replies to Advanced Protocol negotiation. |
| `OpCancel` | `0x07` | Advanced 1.1 | Cancels an in-flight RPC request (sent only when `FeatureCancel` is negotiated). |
| `OpStandardLinkTombstone` | `0xFE` | Standard | Marks a claimed slot as intentionally empty after an aborted reservation or callback failure. |
| `OpStandardLinkCopy` | `0xFF` | Standard | Transfers one payload of `1..bufferSize` bytes in the slot's buffer. |

Detailed operand layouts for every opcode are defined in [`spec/SPEC.md`](spec/SPEC.md) §9 and §10.

## Standard Protocol 1.0

`OpStandardLinkCopy` publishes a single message of `1 <= DataSize <= bufferSize` bytes stored in the buffer owned by the claimed ring slot. Receivers always locate the payload buffer from the ring slot index they claimed, never from the packet's diagnostic `BufferIndex` operand. `OpStandardLinkTombstone` preserves ring progress when a reserved write is aborted or a zero-copy write callback panics; readers skip tombstones and release the slot immediately. Each read call returns bytes from at most one message, queuing any unread tail in local FIFO order before claiming subsequent ring slots. See [`spec/SPEC.md`](spec/SPEC.md) §9.

## Advanced Protocol

The Advanced Protocol layers negotiated feature flags, multiplexed streams, multi-chunk message reassembly, and concurrent request-response RPC on top of the same 64-byte packet ring.

### Identifiers and Frame Flags

Every Advanced frame identifies its target stream, message sequence, and optional RPC method using three 64-bit identifiers:

| Identifier | Scope | Purpose |
| :--- | :--- | :--- |
| `ConnectionID` | Per link | Logical stream ID. `0` is the permanent link-level stream. Primary allocates even non-zero IDs (`2, 4, ...`), Secondary allocates odd IDs (`1, 3, ...`), and IDs are never reused. |
| `MessageID` | Per sender | Identifies a multi-chunk data message or correlates an RPC request with its response. Starts at `1`. |
| `MethodID` | Application | Selects the target RPC handler on a request frame. |

Operand 5 of `OpConnCopy` carries a bitmask of frame flags:

| Flag | Value | Meaning |
| :--- | ---: | :--- |
| `AdvFlagBegin` | `1 << 0` | First chunk of a message (`ChunkOffset == 0`). |
| `AdvFlagEnd` | `1 << 1` | Final chunk of a message (`ChunkOffset + ChunkSize == TotalSize`). |
| `AdvFlagAbort` | `1 << 2` | Sender abandoned a multi-chunk transfer; receiver discards partial reassembly state. |
| `AdvFlagRequest` | `1 << 3` | Frame belongs to an RPC request. |
| `AdvFlagResponse` | `1 << 4` | Frame belongs to an RPC response. |
| `AdvFlagError` | `1 << 5` | Response carries an application or protocol error status. |
| `AdvFlagMeta` | `1 << 6` | Protocol 1.1: request body begins with a length-prefixed metadata block (set on every chunk of that request; invalid unless `FeatureMetadata` was negotiated). |

Frames with unknown flag bits, both `AdvFlagRequest` and `AdvFlagResponse` set, or `AdvFlagError` set without `AdvFlagResponse` are rejected as protocol violations. Full frame validation rules are in [`spec/SPEC.md`](spec/SPEC.md) §10.5.

### Stream Control

- `OpConnCreate` asks the peer to open a logical stream. The peer replies with `OpConnAccept`, or rejects the stream with `OpError` carrying a refusal code: `ErrCodeConnNotFound` when the peer is not listening for streams (after `StreamListenGrace` expires), `ErrCodeInvalidOp` when the requested stream ID has the wrong parity, is `0`, or is already in use, and `ErrCodeBufferOverflow` when the accept queue is full under `OverflowDrop`.
- `OpConnClose` closes a stream from the sender's side. The receiving peer delivers any messages already queued on that stream before reporting `ErrPeerClosed`.
- An incoming stream is exposed to `AcceptStream` only after its `OpConnAccept` reply has been queued for transmission, guaranteeing that data frames sent by the opener after `OpenStream` returns never arrive before the stream is registered.

### Messages

`OpConnCopy` carries one chunk of a message with seven operands: `0` `ConnectionID`, `1` `MessageID`, `2` `TotalSize`, `3` `ChunkOffset`, `4` `ChunkSize`, `5` frame flags, and `6` method ID or error status (non-zero only in the first chunk at offset `0`). Senders emit chunks sequentially in offset order with `ChunkSize <= bufferSize`. Receivers validate chunk bounds on every frame, reassemble messages up to `MaxMessageSize` bytes, and refuse messages whose reassembly would push aggregate in-flight memory past `MaxReassemblyBytes`. See [`spec/SPEC.md`](spec/SPEC.md) §10.4 and §10.5.

### Negotiation

All Advanced operations require a completed protocol handshake (`ErrNotNegotiated` otherwise). Four feature bits are defined across Protocol 1.0 and 1.1:

| Feature | Value | Introduced in |
| :--- | ---: | :--- |
| `FeatureMessages` | `1 << 0` | 1.0 |
| `FeatureRequestResponse` | `1 << 1` | 1.0 |
| `FeatureCancel` | `1 << 2` | 1.1 |
| `FeatureMetadata` | `1 << 3` | 1.1 |

The responder replies with `min(requestedMinor, supportedMinor)` and the bitwise intersection of requested and locally enabled features. Negotiation failure is terminal for that link instance: once in `ProtocolStateFailed`, the link must be closed and reopened. Full handshake rules live in [`spec/SPEC.md`](spec/SPEC.md) §10.1 and [`spec/ext/PROTOCOL_1_1_DRAFT.md`](spec/ext/PROTOCOL_1_1_DRAFT.md) §1.

### Protocol 1.1

When `FeatureCancel` is negotiated, a caller whose `Call` context is canceled or expires while waiting sends an advisory `OpCancel` frame (`operand 0` = stream ID, `operand 1` = request `MessageID`). The server cancels the running handler's context and suppresses the response frame.

When `FeatureMetadata` is negotiated, a request carrying metadata sets `AdvFlagMeta` and prefixes its body with a length-prefixed sequence of tagged fields (`tag 1`: remaining time budget in nanoseconds as a `uint64`; `tag 2`: `uint64` caller ID; `tag 3`: 16-byte trace ID). Unknown tags are skipped so future minor revisions remain forward-compatible. The responder converts the incoming time budget into a deadline on its local monotonic clock for the handler's context. Wire encoding details are in [`spec/ext/PROTOCOL_1_1_DRAFT.md`](spec/ext/PROTOCOL_1_1_DRAFT.md) §3.

### Receive Path and Routing

Once negotiation completes, a single receive path drains the incoming ring and routes reassembled messages to three destination queues:

| Message Kind | Destination |
| :--- | :--- |
| Response | Wakes the pending `Call` matching `(ConnectionID, MessageID)`. Unmatched responses are dropped and counted. |
| Request | Enqueued to the link-wide request queue consumed by `Serve`. |
| Data | Enqueued to the target stream's receive queue consumed by `Stream.Recv`. Data for unknown or closed streams is dropped and counted. |

Control replies generated on the receive path (stream accepts/refusals, reassembly-limit error responses, and cancellation frames) are handed off to a bounded control-sender queue so the receive loop never blocks on the transmit ring. Each destination queue enforces the configured `OverflowPolicy` (`OverflowBlock`, `OverflowDrop`, or `OverflowFail`); see [`spec/SPEC.md`](spec/SPEC.md) §10.7.

## Listener Segment (Multi-client Acceptor)

A multi-client listener is a shared-memory segment with magic `HQQ_LSN1` and layout version 1.0 containing a header and an MPMC ring of connect records. When a client calls `Connect`, it creates its own dedicated `HQQ_STL3` link segment as Primary, publishes the segment name and geometry into the listener ring, and waits. The server's `Acceptor` pops the connect record, attaches to the client's segment as Secondary, completes Advanced negotiation, and returns the live link from `Accept`. Because every client gets its own link segment, a crashed or misbehaving client never corrupts other clients or the listener. The acceptor refreshes a heartbeat in the listener header; clients that observe a stale listener heartbeat fail with `ErrListenerGone`. See [`spec/ext/LISTENER_DRAFT.md`](spec/ext/LISTENER_DRAFT.md).

## Memory Model

Every shared word in the superblock, ring header, and slot sequence array has a prescribed atomic memory order ([`spec/MEMORY_MODEL.md`](spec/MEMORY_MODEL.md) §2). In particular, the four wait/wake accesses `W1`, `W3`, `K1`, and `K2` must execute with sequential consistency (`memory_order_seq_cst` or acquire/release paired with a full `seq_cst` fence; [`spec/MEMORY_MODEL.md`](spec/MEMORY_MODEL.md) §4). Weakening those four operations to plain acquire/release allows store-buffer reordering on x86 and ARM, where a producer sees zero waiters while a consumer simultaneously sees an empty ring and sleeps forever.

## Error Hierarchy

Every public Go error wraps its category base error so callers can match either the specific sentinel or its broader class with `errors.Is`. Unexported transient startup errors wrap `ErrFailedInit`; `Dial` and `Connect` retry automatically on those transients and on `os.ErrNotExist` (see [`spec/SPEC.md`](spec/SPEC.md) §6.4 and §12).

| Sentinel | Wraps | Meaning |
| :--- | :--- | :--- |
| `io.EOF` (returned directly) | none | `StandardLink.Read` and `ReadFrom` after the peer closed gracefully and all buffered messages were drained. |
| `ErrClosed` | `net.ErrClosed` | Local link or stream was closed (`Close`, `Stream.Close`), or `Acceptor.Accept` after `Close` (`ErrAcceptorClosed`). |
| `ErrPeerClosed` | `io.EOF` | Remote peer closed its link gracefully (all methods except `StandardLink.Read` and `ReadFrom`), or closed the stream (`Send` and `Call` immediately; `Recv` after draining queued messages). |
| `ErrPeerGone` | `ErrConnNotFound` | Remote peer stopped heartbeating (crashed or hung), or the segment was re-initialized under this endpoint. |
| `ErrConnNotFound` | none | Base of `ErrPeerGone`; also returned by `OpenStream` when the peer did not call `ListenStreams` within `StreamListenGrace`. Never returned for unknown or closed stream IDs. |
| `ErrTimeout` | `os.ErrDeadlineExceeded` | Operation deadline expired. Implements `net.Error` with `Timeout() == true`. |
| `ErrLinkBusy` | `ErrFailedInit` | Two live endpoints are already attached, the live endpoint's earlier peer already left, or a creator (`Create*`, `Listen`, `NewAcceptor`) found a live segment. |
| `ErrListenerBusy` | `ErrLinkBusy` | `NewAcceptor` found a live acceptor already running on the listener name. |
| `ErrListenerClosed` | `ErrPeerClosed` | `Connect`: the acceptor closed the listener while the client's connect request was waiting. |
| `ErrListenerGone` | `ErrPeerGone` | `Connect`: the acceptor stopped heartbeating or was replaced while the connect request was waiting. |
| `ErrIncompatibleLayout` | `ErrFailedInit` | Foreign magic or layout major version (including `HQQ_STL2`), unknown `RequiredFeatures` bit, unknown `State`, non-zero first page on an `Empty` segment, or mismatched ring layout version. |
| `ErrInvalidSize` | none | Invalid geometry or size bounds, a named object smaller than the requested segment, or geometry differing from an existing segment. |
| `*GeometryError` (`errors.As`) | `ErrInvalidSize` | Existing segment's recorded geometry differs from the caller's requested geometry. Exposes `Want` and `Got` (`Geometry`). |
| `ErrMemoryAlign` | none | Caller-owned memory slice is not page-aligned. |
| `ErrMemorySmall` | none | Caller-owned memory slice is empty or shorter than `SizeStandardLink`. |
| `ErrFailedInit` | none | Base error for initialization failures, including transient states retried by `Dial` and `Connect`. |
| `ErrInvalidName` | none | Shared-memory name is empty, contains path separators or NUL, exceeds the platform length limit, or (on Linux) refers to a symlink or non-regular file. |
| `ErrInvalidOp` | none | Base error for protocol-state violations; also returned by `Stream.Close` on stream `0`, stale reservation completions, and `Serve` with a `nil` handler. |
| `ErrBufferOverflow` | none | Payload exceeds `bufferSize` (`StandardLink.Write`) or `MaxMessageSize` (`Stream.Send`, `ResponseWriter.Write`, incoming frame). |
| `ErrNotNegotiated` | `ErrInvalidOp` | Advanced API invoked before negotiation succeeded, or the required feature bit was not negotiated. |
| `ErrAlreadyNegotiated` | `ErrInvalidOp` | Negotiation called while another negotiation is in progress or after negotiation already succeeded. |
| `ErrNegotiationFailed` | `ErrInvalidOp` | Negotiation called after an earlier attempt failed. Close and reopen the link. |
| `ErrNegotiationRejected` | `ErrInvalidOp` | Peer rejected negotiation or acknowledged an incompatible version or feature mask. |
| `ErrUnexpectedPacket` | `ErrInvalidOp` | Frame opcode cannot be consumed by the calling layer (for example an Advanced frame read via `StandardLink.Read`, or `OpCancel` without `FeatureCancel`). |
| `ErrProtocol` | `ErrInvalidOp` | Malformed frame bounds or flags, invalid state transition, or an `OpError` carrying an unrecognized error code. |
| `ErrMessageAborted` | `ErrProtocol` | `Stream.Call`: the peer aborted the response mid-transfer. |
| `ErrNotListening` / `ErrAlreadyListening` | `ErrInvalidOp` | `AcceptStream` called before `ListenStreams`, or `ListenStreams` called more than once. |
| `ErrShuttingDown` | `ErrInvalidOp` | `Stream.Call` refused because the peer is draining in `Shutdown` (`ResponseError` with status `ErrCodeShuttingDown`). |
| `ErrIncomingOverflow` | `ErrBufferOverflow` | Destination queue (stream, request queue, or accept queue) overflowed under `OverflowFail`. |
| `ErrReassemblyLimit` | `ErrBufferOverflow` | `Stream.Call`: multi-chunk response refused because reassembly would exceed `MaxReassemblyBytes`. |
| `ResponseError` (`errors.As`) | none | Returned by value from `Stream.Call` when the server handler called `ResponseWriter.WriteError`. Carries `Status` and a caller-owned `Payload` copy. |

Wire `ErrorCode` values carried inside `OpError`, tombstones, and RPC error statuses map directly to the base errors above: `0x01` memory alignment, `0x02` invalid size, `0x03` memory too small, `0x04` failed init, `0x05` connection not found, `0x06` invalid operation, `0x07` buffer overflow, and `0x08` shutting down (Advanced 1.1).

## Application Protocols

Higher-level services layer their own framing on top of Advanced streams and RPC calls, or directly over dedicated Standard links:

- **SQLite service protocol (`sqlite/PROTOCOL.md`)**: version 1.0 application protocol over `AdvancedLink` streams where each `database/sql` connection owns one stream and exchanges typed SQLite frames.
- **`hqq-bindgen` RPC wire protocol (`spec/toolchain/BINDGEN.md`)**: generated Go-to-C RPC protocol where a control link bootstraps per-session `StandardLink` rings with generation-checked handle tables.
