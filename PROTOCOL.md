# HQQ Protocol Overview

This document is a human-readable overview of the HQQ shared-memory protocol family. It is not normative. The normative documents are listed below. Where this overview and a normative document differ, the normative document wins.

| Document | Covers |
| --- | --- |
| [`spec/SPEC.md`](spec/SPEC.md) | Normative specification of the segment layout, superblock, roles, initialization, liveness, the ring, wait/wake, the Standard and Advanced Protocol 1.0, platform naming, and error codes. |
| [`spec/MEMORY_MODEL.md`](spec/MEMORY_MODEL.md) | Required memory order of every shared word, and the wait/wake handshake. |
| [`spec/PORTING.md`](spec/PORTING.md) | Steps and conformance levels for a second implementation. |
| [`spec/ext/PROTOCOL_1_1_DRAFT.md`](spec/ext/PROTOCOL_1_1_DRAFT.md) | What Advanced Protocol 1.1 adds: `FeatureCancel`, `FeatureMetadata`, `OpCancel`, handler lifecycle, and graceful shutdown. |
| [`spec/ext/LISTENER_DRAFT.md`](spec/ext/LISTENER_DRAFT.md) | The listener segment (`HQQ_LSN1`) of the multi-client acceptor. |
| [`spec/constants.json`](spec/constants.json) | Machine-readable protocol constants. |
| [`conformance/`](conformance/) | Golden vectors generated from the Go implementation. |

## Versions

| Version | Value | Where it is carried |
| --- | --- | --- |
| Segment layout | 3.0, magic `HQQ_STL3` | Superblock `Magic`, `Major`, `Minor`. A different magic or major is rejected with `ErrIncompatibleLayout`. |
| MPMC ring layout | 2 | Ring header; an attacher rejects another value. |
| Standard Protocol | 1.0 | Standard packet frames (`OpStandardLinkCopy`, `OpStandardLinkTombstone`). |
| Advanced Protocol | 1.1 | `OpProtoNegotiate` / `OpProtoAck` operand 0, encoded as `(major << 8) \| minor`. Features are negotiated, so a 1.0 peer negotiates 1.0. |
| Listener layout | 1.0, magic `HQQ_LSN1` | Listener segment header. |

## Shared Memory Layout

A link is one page-aligned segment: a first-page superblock, then two directional channels. Each channel has one packet ring and one payload pool.

```text
SUPERBLOCK    one page: layout version, state (Empty, Initializing, Ready), geometry,
              epoch, initializer token and heartbeat, creator token, and two
              128-byte endpoint records (token, PID, heartbeat, interval)
DATA_RING_0   Primary -> Secondary packet ring
BUFFERS_0     bufferCount x bufferSize payload bytes; slot i owns BUFFERS_0[i]
DATA_RING_1   Secondary -> Primary packet ring
BUFFERS_1     bufferCount x bufferSize payload bytes; slot i owns BUFFERS_1[i]
```

`bufferCount` is a power of two with `2 <= bufferCount <= 1<<24`; `bufferSize` is a multiple of 8 with `8 <= bufferSize <= 1<<30`. Field offsets, the state machine, and the claim, takeover, and reuse rules are in [`spec/SPEC.md`](spec/SPEC.md) §2 to §5.

Each packet ring has a 512-byte header and slots of an 8-byte sequence number followed by a 64-byte packet (stride 128). A producer claims a slot, fills the same-index payload buffer, and publishes the packet. A consumer claims the slot, reads or inspects the payload in place, and releases the slot. The payload buffer of a slot cannot be claimed again until the slot is released, so an unread payload is never overwritten. The rules are in [`spec/SPEC.md`](spec/SPEC.md) §7.

## Packet ABI

Every packet is exactly 64 bytes: eight little-endian `uint64` words. Word 0 holds the opcode in its low byte; the other bits are reserved and written as zero, and receivers ignore them. Words 1 to 7 hold operands 0 to 6. Operands that are reserved are written as zero and ignored on receipt.

| Opcode | Value | Layer | Notes |
| --- | ---: | --- | --- |
| `OpError` | `0x00` | both | Context ID and `ErrorCode`. |
| `OpConnCreate` | `0x01` | Advanced | Opens a stream. |
| `OpConnAccept` | `0x02` | Advanced | Accepts a stream. |
| `OpConnClose` | `0x03` | Advanced | Closes a stream. |
| `OpConnCopy` | `0x04` | Advanced | One chunk of a message. |
| `OpProtoNegotiate` | `0x05` | Advanced | Protocol negotiation request. |
| `OpProtoAck` | `0x06` | Advanced | Protocol negotiation answer. |
| `OpCancel` | `0x07` | Advanced 1.1 | Cancels a request. Sent only with `FeatureCancel`. |
| `OpStandardLinkTombstone` | `0xFE` | Standard | Marks a claimed slot as intentionally empty. |
| `OpStandardLinkCopy` | `0xFF` | Standard | One payload in the direction's buffer pool. |

The operand layouts are in [`spec/SPEC.md`](spec/SPEC.md) §9 and §10.

## Standard Protocol 1.0

`OpStandardLinkCopy` carries one payload of `1 <= DataSize <= bufferSize` bytes in the buffer of its claimed slot. A receiver locates the payload from the claimed slot index, never from the copy's `BufferIndex` operand, which is diagnostic. `OpStandardLinkTombstone` preserves ring progress when a reserved write is aborted or a zero-copy callback fails; readers skip it and release the slot. Reads copy at most one message per call; the unread tail of a message is queued and served before newer messages. See [`spec/SPEC.md`](spec/SPEC.md) §9.

## Advanced Protocol

### Identifiers and Frame Flags

| ID | Scope | Purpose |
| --- | --- | --- |
| `ConnectionID` | Per link | Stream ID. `0` is the link-level stream and is always open. Primary allocates even non-zero IDs (2, 4, ...), Secondary odd IDs (1, 3, ...). IDs are never reused. |
| `MessageID` | Per sender | Identifies a data message, or correlates a request with its response. Starts at 1. |
| `MethodID` | Application | Method selector of a request. |

`OpConnCopy` operand 5 carries the frame flags:

| Flag | Value | Meaning |
| --- | ---: | --- |
| `AdvFlagBegin` | `1 << 0` | First chunk of a message. |
| `AdvFlagEnd` | `1 << 1` | Last chunk of a message. |
| `AdvFlagAbort` | `1 << 2` | The sender abandoned the message; the receiver discards its partial state. |
| `AdvFlagRequest` | `1 << 3` | Chunk of a request. |
| `AdvFlagResponse` | `1 << 4` | Chunk of a response. |
| `AdvFlagError` | `1 << 5` | Response is an error. |
| `AdvFlagMeta` | `1 << 6` | Protocol 1.1: the request payload starts with a metadata block. Set on every chunk of such a request. A protocol error unless `FeatureMetadata` was negotiated. |

Unknown flag bits, both request and response flags together, and `AdvFlagError` without `AdvFlagResponse` are protocol violations. The full validation rules are in [`spec/SPEC.md`](spec/SPEC.md) §10.5.

### Stream Control

- `OpConnCreate` asks the peer to open a stream. The peer answers with `OpConnAccept`, or with `OpError` carrying a refusal code: `ErrCodeConnNotFound` when the peer does not listen (after its `StreamListenGrace`), `ErrCodeInvalidOp` for an ID of the wrong parity, an ID of `0`, or an ID in use, and `ErrCodeBufferOverflow` when the accept queue is full under `OverflowDrop`.
- `OpConnClose` closes a stream from one side. The peer delivers the messages it already has, then reports the stream closed.
- An incoming stream becomes visible to `AcceptStream` only after its `OpConnAccept` is queued, so data sent on the stream cannot overtake the accept.

### Messages

`OpConnCopy` carries one chunk of a message. Operands: `0` ConnectionID, `1` MessageID, `2` TotalSize, `3` ChunkOffset, `4` ChunkSize, `5` frame flags, `6` method or status (non-zero only in the chunk at offset 0). Chunks of a message are sent in order and never exceed `bufferSize`. A message of up to `MaxMessageSize` bytes is reassembled by the receiver, which validates the bounds of every frame and refuses a message that would exceed `MaxReassemblyBytes`. See [`spec/SPEC.md`](spec/SPEC.md) §10.4 and §10.5.

### Negotiation

Advanced APIs need a negotiated link (`ErrNotNegotiated` otherwise). The features are:

| Feature | Value | Since |
| --- | ---: | --- |
| `FeatureMessages` | `1 << 0` | 1.0 |
| `FeatureRequestResponse` | `1 << 1` | 1.0 |
| `FeatureCancel` | `1 << 2` | 1.1 |
| `FeatureMetadata` | `1 << 3` | 1.1 |

The responder answers with the minimum of the requested and its own minor version, and with the requested features that it supports and allows. A failed negotiation is terminal: the link must be closed and a new one opened. The rules are in [`spec/SPEC.md`](spec/SPEC.md) §10.1 and [`spec/ext/PROTOCOL_1_1_DRAFT.md`](spec/ext/PROTOCOL_1_1_DRAFT.md) §1.

### Protocol 1.1

With `FeatureCancel`, a requester whose `Call` context ends while it waits sends `OpCancel`, with operand 0 the stream ID (0 for the link-level stream) and operand 1 the request's MessageID. It is advisory and has no reply. The responder cancels the handler's context; a response is not sent.

With `FeatureMetadata`, a request begins with a metadata block: a length-prefixed list of tagged values that the responder skips if it does not know the tag. The known tags are the time budget (tag 1, a `uint64` in nanoseconds counted from the moment the block was sent), the caller ID (tag 2), and the trace ID (tag 3, 16 bytes). A request whose context has a deadline carries the remaining time, and the handler's context ends at the corresponding deadline on the responder's clock. The block layout is in [`spec/ext/PROTOCOL_1_1_DRAFT.md`](spec/ext/PROTOCOL_1_1_DRAFT.md) §3.

### Receive Path and Routing

After negotiation, one receive path consumes the receive direction and routes every complete message:

| Message | Destination |
| --- | --- |
| Response | The pending call with the same `(ConnectionID, MessageID)`. A response without a waiting call is dropped and counted. |
| Request | The request queue, consumed only by `Serve`, for every stream. |
| Data | The queue of its stream, consumed by `Stream.Recv`. Data for an unknown or closed stream is dropped and counted. |

Replies generated by the receive path (stream accepts and refusals, refusal responses, and cancellation frames) go through a bounded control-sender queue, so the receive path never waits for the transmit ring. Each destination queue has its own overflow policy (`OverflowBlock`, `OverflowDrop`, or `OverflowFail`). The routing and overflow rules are in [`spec/SPEC.md`](spec/SPEC.md) §10.7.

## Listener Segment (Multi-client Acceptor)

A listener is a named segment with magic `HQQ_LSN1` and layout version 1.0. Its header is followed by a ring of connect records. A client creates an ordinary link segment of its own, writes a connect record that names it, and waits. The acceptor attaches to that segment as Secondary, negotiates, and hands the link to the application. Each client has its own segment, so a crashed or stalled client does not affect other clients or the acceptor's other links. The acceptor heartbeats the listener; a client that finds the acceptor gone fails with `ErrListenerGone`. The layout, the state machine, and the validation rules are in [`spec/ext/LISTENER_DRAFT.md`](spec/ext/LISTENER_DRAFT.md).

## Memory Model

Every shared word has a required memory order ([`spec/MEMORY_MODEL.md`](spec/MEMORY_MODEL.md) §2). The wait/wake handshake needs sequentially consistent accesses for the four accesses `W1`, `W3`, `K1`, and `K2`, or the equivalent fences ([`spec/MEMORY_MODEL.md`](spec/MEMORY_MODEL.md) §4). Relaxed or acquire/release-only versions of the handshake can lose a wake-up. The data-path publish and consume rules are in §3 of the same document.

## Error Hierarchy

Each Go error wraps the base error that it matches, so `errors.Is` works against both the sentinel and its base. The protocol-level error codes and their mapping are in [`spec/SPEC.md`](spec/SPEC.md) §6.4 and §12.

All public errors support `errors.Is` against the sentinel itself and against the base it wraps. Unexported transient errors used while a named segment is being set up wrap `ErrFailedInit`. `Dial` and `Connect` retry on these and on a missing name; see their godoc for the exact cases.

| Sentinel | Wraps | Meaning |
| --- | --- | --- |
| `io.EOF` (returned as is) | — | `StandardLink.Read` and `ReadFrom` after the peer closed gracefully and every buffered message was consumed. |
| `ErrClosed` | `net.ErrClosed` | Local link was closed, or the stream was closed locally (`Stream.Close`). Also `Acceptor.Accept` after `Close` (`ErrAcceptorClosed`). |
| `ErrPeerClosed` | `io.EOF` | Remote peer closed its link gracefully (all APIs except `StandardLink.Read` and `ReadFrom`), or closed the stream (`Send` and `Call` of that stream; `Recv` once its queued messages are consumed). |
| `ErrPeerGone` | `ErrConnNotFound` | Remote peer stopped heartbeating (crashed or hung), or the segment was re-initialized under this endpoint. |
| `ErrConnNotFound` | — | Base of `ErrPeerGone`. Also the refusal of `OpenStream` when the peer did not listen in time, and a held stream request refused at the end of its grace period. Not returned for unknown or closed stream IDs. |
| `ErrTimeout` | `os.ErrDeadlineExceeded` | Deadline passed. Implements `net.Error` with `Timeout() == true`. |
| `ErrLinkBusy` | `ErrFailedInit` | Two live endpoints are attached, the live endpoint's earlier peer already left, or a creator (`Create*`, `Listen`, `NewAcceptor`) found a live segment. |
| `ErrListenerBusy` | `ErrLinkBusy` | `NewAcceptor` found a live acceptor on the listener name. |
| `ErrListenerClosed` | `ErrPeerClosed` | `Connect`: the acceptor closed the listener while the request was waiting. |
| `ErrListenerGone` | `ErrPeerGone` | `Connect`: the acceptor stopped heartbeating or was replaced while the request was waiting. Connect again. |
| `ErrIncompatibleLayout` | `ErrFailedInit` | Foreign magic or other layout major (including `HQQ_STL2`), unknown `RequiredFeatures` bit, unknown `State`, a non-zero first page of an `Empty` segment, or a ring header that does not match this layout version. |
| `ErrInvalidSize` | — | Invalid geometry or size bounds, a named object smaller than the requested segment, or geometry that differs from an existing segment. |
| `*GeometryError` (`errors.As`) | `ErrInvalidSize` | The recorded geometry of an existing segment differs from the caller's. Carries `Want` and `Got` (`Geometry`). |
| `ErrMemoryAlign` | — | Caller-owned memory is not page-aligned. |
| `ErrMemorySmall` | — | Caller-owned memory is empty or shorter than `SizeStandardLink`. |
| `ErrFailedInit` | — | Base of initialization failures, including the transient ones for openers. |
| `ErrInvalidName` | — | Shared memory name is empty, contains separators or NUL, or exceeds the platform limit. On Linux also a name that is a symbolic link or not a regular file. |
| `ErrInvalidOp` | — | Base of the protocol-state errors below. Also `Stream.Close` on stream 0 and `Serve` with a nil handler. |
| `ErrBufferOverflow` | — | Payload larger than `bufferSize` (`StandardLink.Write`), or `MaxMessageSize` (`Stream.Send`, `ResponseWriter.Write`, incoming frame). |
| `ErrNotNegotiated` | `ErrInvalidOp` | Advanced API called before protocol negotiation succeeded, or the feature it needs was not negotiated. |
| `ErrAlreadyNegotiated` | `ErrInvalidOp` | Negotiation attempted while one is in progress or after one succeeded. |
| `ErrNegotiationFailed` | `ErrInvalidOp` | Negotiation attempted after an earlier attempt failed. The link must be reopened. |
| `ErrNegotiationRejected` | `ErrInvalidOp` | Peer rejected negotiation, or acknowledged an incompatible version or feature set. |
| `ErrUnexpectedPacket` | `ErrInvalidOp` | Packet that the calling API cannot consume, for example an Advanced frame on `StandardLink.Read`, or `OpCancel` without `FeatureCancel`. |
| `ErrProtocol` | `ErrInvalidOp` | Malformed frame bounds or flags, a protocol state violation, or a peer error with an unknown code. |
| `ErrMessageAborted` | `ErrProtocol` | `Stream.Call`: the peer abandoned the response mid-transfer. |
| `ErrNotListening` / `ErrAlreadyListening` | `ErrInvalidOp` | `AcceptStream` before `ListenStreams`, or a second `ListenStreams`. |
| `ErrShuttingDown` | `ErrInvalidOp` | `Stream.Call` was refused because the peer is in `Shutdown` (`ResponseError` with status `ErrCodeShuttingDown`). |
| `ErrIncomingOverflow` | `ErrBufferOverflow` | The queue a message is routed to (a stream, the request queue, or the accept queue) is full under `OverflowFail`. |
| `ErrReassemblyLimit` | `ErrBufferOverflow` | `Stream.Call`: the response was refused under `MaxReassemblyBytes`. |
| `ResponseError` (`errors.As`) | — | `Stream.Call` returns it by value when the peer's handler answered with `ResponseWriter.WriteError`. Carries `Status` and a copy of the payload. |

`ErrorCode` values carried in `OpError`, tombstones, and error statuses: `0x01` memory alignment, `0x02` invalid size, `0x03` memory too small, `0x04` failed init, `0x05` connection not found, `0x06` invalid operation, `0x07` buffer overflow, `0x08` shutting down (Advanced 1.1). Each maps back to the base error above.

## Application Protocols

Services built on the Advanced Protocol define their own protocols over its streams and request-response messages. This document covers only the HQQ layer. The SQLite service protocol, version 1.0, which has its own version number, is in [`sqlite/PROTOCOL.md`](sqlite/PROTOCOL.md): each `database/sql` connection is one stream, and each request gets one response.
