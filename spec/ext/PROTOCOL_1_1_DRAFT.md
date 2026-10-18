# HQQ Advanced Protocol 1.1 Specification

**Status**: Active specification, implemented by the Go library (`gosuda.org/hqq`), the C11 reference library (`libhqq`), and the C++17 and Rust bindings.

**Baseline**: HQQ Advanced Protocol 1.0 as defined in [`SPEC.md`](../SPEC.md) and [`PROTOCOL.md`](../../PROTOCOL.md). This specification defines the additions introduced in Protocol 1.1; all other aspects of the protocol remain unchanged from 1.0, including:

- The shared-memory segment layout (`HQQ_STL3` v3.0, MPMC ring layout v2, and page-aligned payload pools).
- The 64-byte packet ABI (eight little-endian `uint64` words: word 0 carries the opcode in bits `0..7` with bits `8..63` zero, and words 1-7 carry operands 0-6).
- The `OpConnCopy` operand layout (operand 0 connection ID, operand 1 message ID, operand 2 total size, operand 3 chunk offset, operand 4 chunk size, operand 5 flags, operand 6 aux) and all 1.0 bounds and state checks.
- Multiplexed stream lifecycle, multi-chunk reassembly budgets, and mid-transfer `AdvFlagAbort` handling.

All Protocol 1.1 capabilities are gated by negotiated feature bits. When a 1.1 feature bit is not negotiated on a link, that link behaves identically to Protocol 1.0 with respect to that feature, and receiving an unnegotiated 1.1 frame or flag triggers the same protocol error as an unknown opcode or flag in 1.0 (section 7).

Normative keywords (`MUST`, `MUST NOT`, `SHOULD`, `MAY`) follow RFC 2119. All multi-byte integers are little-endian. In RPC interactions, **requester** denotes the endpoint calling `Call`, and **responder** denotes the endpoint running `Serve` (roles are per-request, as both endpoints on a link may issue and serve requests concurrently).

## 1. Version Encoding and Feature Negotiation

Protocol 1.1 bumps the minor version in `OpProtoNegotiate` and `OpProtoAck` from `0` to `1` and introduces two additional feature bits:

| Parameter | Protocol 1.0 | Protocol 1.1 |
| --- | --- | --- |
| Encoded version in `OpProtoNegotiate` / `OpProtoAck` operand 0 | `0x0100` (`major=1, minor=0`) | `0x0101` (`(major << 8) \| minor`) |
| Feature bitmask in operand 1 | `FeatureMessages = 1 << 0` (`0x01`), `FeatureRequestResponse = 1 << 1` (`0x02`) | Adds `FeatureCancel = 1 << 2` (`0x04`) and `FeatureMetadata = 1 << 3` (`0x08`) |

The 64-byte negotiation packet layout is unchanged (operand 0 version, operand 1 features, operand 2 reserved `0`):

- **Initiator (`OpProtoNegotiate`)**: Sends its supported protocol version (`0x0101`) and requested feature mask (all four bits `0x0f` by default in a 1.1 implementation).
- **Responder (`OpProtoAck`)**: Replies with `version = (requested.major, min(requested.minor, 1))` and `features = requested & supported & allowed`. A responder `MUST` clear `FeatureCancel` and `FeatureMetadata` whenever the negotiated minor version is `0`.
- **Validation**: The initiator accepts `OpProtoAck` if and only if `ack.major == 1`, `ack.minor <= requested.minor`, and `ack.features` is a subset of `requested.features`.
- **Feature independence**: `FeatureCancel` and `FeatureMetadata` can be negotiated independently of each other, and both apply to RPC requests (`FeatureRequestResponse`).

Example 64-byte negotiation exchange when both endpoints support Protocol 1.1 and all four features (`0x0f`):

```text
OpProtoNegotiate: 05000000 00000000 | 0101000000000000 | 0f00000000000000 | 0000000000000000 | ...
OpProtoAck:       06000000 00000000 | 0101000000000000 | 0f00000000000000 | 0000000000000000 | ...
```

Version and feature interoperability matrix:

| Initiator Request | Responder Capability | Negotiated Result |
| --- | --- | --- |
| `1.1`, features `0x0f` | `1.1`, features `0x0f` | Version `1.1` (`0x0101`), features `0x0f` |
| `1.1`, features `0x0f` | `1.0` (`0x0100`, features `0x03`) | Version `1.0` (`0x0100`), features `0x03` |
| `1.0`, features `0x03` | `1.1` (`0x0101`, features `0x0f`) | Version `1.0` (`0x0100`), features `0x03` |
| `1.1`, features `0x0f` | `1.1`, restricted subset (`0x03`, `0x07`, or `0x0b`) | Version `1.1` (`0x0101`), intersection of requested and allowed bits |

## 2. Request Cancellation (`OpCancel`, Opcode `0x07`)

`OpCancel` is valid on the wire only when `FeatureCancel` was negotiated:

| Packet Word | Content |
| --- | --- |
| Word 0 (opcode) | `0x07` (`OpCancel`) |
| Word 1 (operand 0) | Connection (stream) ID of the target request (`0` for the default link stream) |
| Word 2 (operand 1) | Message ID of the target request |
| Words 3-7 (operands 2-6) | Reserved: `MUST` be written as `0` by senders and ignored by receivers |

Example 64-byte `OpCancel` frame canceling request message `1` on stream `0`:

```text
07000000 00000000  0000000000000000  0100000000000000  00..00 (40 bytes of zero)
```

Wire and behavioral semantics:

- **Advisory notification**: `OpCancel` is sent by the requester to advise the responder that the requester is no longer waiting for the response to `(streamID, messageID)`. `OpCancel` has no reply frame.
- **Responder lookup**: Upon receiving `OpCancel(streamID, messageID)`, the responder looks up that key among currently in-flight requests (either queued awaiting a worker or actively executing in a handler). If found, the responder cancels the request's handler context (section 4.2 and section 4.3). If not found (already answered, never seen, or previously dropped), the responder ignores the packet. Duplicate `OpCancel` packets for the same key are harmless.
- **When `OpCancel` is emitted**: Sent by the requester when a `Call` context is canceled or its deadline expires **after** all chunks of the request have been published to the ring. A requester `MUST NOT` send `OpCancel` if the request itself was aborted mid-send via `AdvFlagAbort` (an aborted transfer is never delivered as a request), if the link or stream closed, or if the response already arrived.
- **Non-blocking control transmission**: The requester queues `OpCancel` on its bounded outbound control queue on a best-effort basis and `MUST NOT` block the caller or the receive loop if the control queue is full (dropping the `OpCancel` and incrementing `DroppedMessages` if full). Because `OpCancel` and the responder's reply may cross in flight, the requester records the canceled key in its bounded abandoned-call set and silently discards any late response that arrives.
- **Ordering guarantee**: Because `OpCancel` is published to the same ring after the final chunk of the request, the responder always finishes reassembling and routing the request before processing `OpCancel`.
- **Unnegotiated reception**: If `FeatureCancel` was not negotiated, receiving opcode `0x07` fails the receive path with `ErrUnexpectedPacket`.

## 3. Request Metadata (`FeatureMetadata` and `AdvFlagMeta`)

When `FeatureMetadata` is negotiated, requests can carry an optional prefix block encoding a relative time budget, a 64-bit caller ID, and a 128-bit trace ID.

### 3.1 `AdvFlagMeta` (`0x40`)

Protocol 1.1 assigns bit 6 (`0x40`) of `OpConnCopy` operand 5 to `AdvFlagMeta`:

| Bit | Hex Value | Flag Name |
| --- | --- | --- |
| `0` | `0x01` | `AdvFlagBegin` |
| `1` | `0x02` | `AdvFlagEnd` |
| `2` | `0x04` | `AdvFlagAbort` |
| `3` | `0x08` | `AdvFlagRequest` |
| `4` | `0x10` | `AdvFlagResponse` |
| `5` | `0x20` | `AdvFlagError` |
| `6` | `0x40` | `AdvFlagMeta` (Protocol 1.1) |

Validation rules for `AdvFlagMeta`:

- `AdvFlagMeta` is permitted **only** on request frames (`AdvFlagRequest` set, including request `AdvFlagAbort` frames) and **only** when `FeatureMetadata` was negotiated. Setting `AdvFlagMeta` on a plain message, on a response frame, or when `FeatureMetadata` was not negotiated fails the receive path with `ErrProtocol`. Flag bits above bit 6 (`>= 0x80`) remain `ErrProtocol`.
- When sending a request with metadata, the requester sets `AdvFlagMeta` on every chunk of that request, and the receiver latches the presence of metadata from the `Begin` chunk.
- If a request has no deadline, no caller ID (`0`), and no trace ID (all zeros), the requester omits both `AdvFlagMeta` and the metadata block, producing a frame byte-identical to Protocol 1.0.

### 3.2 Metadata Block Wire Format

When `AdvFlagMeta` is set, the message body transmitted across `OpConnCopy` chunks is the concatenation `metadata_block || user_payload`. Frame fields `TotalSize`, `ChunkOffset`, and `ChunkSize` count the combined byte length, and the metadata block may span multiple chunks on small rings. After reassembling the full message buffer, the responder parses and strips `metadata_block` before passing `user_payload` to the handler.

Header layout (4 bytes):

```text
Offset  Size  Field
0       2     tlvLen    uint16 (little-endian): total byte length of the TLV sequence following byte 4
2       2     reserved  uint16: written as 0 by senders, ignored by receivers
4       ...   TLVs      tlvLen bytes of packed Tag-Length-Value records
```

Each TLV record inside the `tlvLen` slice has the format:

```text
Offset  Size    Field
0       1       tag       uint8 identifier
1       1       length    uint8 value length in bytes
2       length  value     little-endian integer or raw byte array
```

Defined TLV tags:

| Tag | Name | Value Length | Encoding and Semantics |
| --- | --- | --- | --- |
| `1` | Time budget | `8` bytes | Little-endian `uint64` nanoseconds remaining until the request deadline, measured at the moment the requester builds the request. |
| `2` | Caller ID | `8` bytes | Little-endian `uint64` application-defined caller identifier (`0` means unset and is omitted). |
| `3` | Trace ID | `16` bytes | 16-byte opaque trace identifier (all-zero means unset and is omitted). |

Parsing and clock rules:

- **Forward-compatible unknown tags**: Receivers skip unrecognized `tag` values by advancing `2 + length` bytes (still verifying that `2 + length` fits within the remaining `tlvLen` budget). If a known tag appears more than once, the last occurrence wins; senders `MUST NOT` emit duplicate tags.
- **Malformed metadata blocks**: If the reassembled message is shorter than 4 bytes, if `4 + tlvLen` exceeds the reassembled message length, if a TLV record overflows `tlvLen`, or if a known tag (`1`, `2`, or `3`) has a `length` different from the table above, the receiver fails the receive path with `ErrProtocol`.
- **Relative monotonic time budget**: Transmitting a relative duration (`budget = deadline - now` on the requester's monotonic clock) avoids any dependency on synchronized wall clocks across processes. If `budget <= 0` when `Call` is invoked, `Call` fails immediately on the requester with `ErrTimeout` (`context.DeadlineExceeded`) without transmitting any frame.
- **Responder deadline calculation**: The responder records the local monotonic timestamp `T0` when the request's `Begin` frame is dequeued and computes `deadline = T0 + min(budget, 2^63 - 1)`.

### 3.3 Wire Encoding Examples

1. **42-byte metadata block** carrying a 5-second budget (`5,000,000,000 = 0x000000012a05f200`), caller ID `0x1122334455667788`, and trace ID `01 00 .. 00`:

```text
2600 0000                                 tlvLen = 38 (0x0026), reserved = 0
01 08 00f2052a01000000                    tag 1 (budget): 5,000,000,000 ns
02 08 8877665544332211                    tag 2 (caller): 0x1122334455667788
03 10 01000000000000000000000000000000    tag 3 (trace):  01 00 .. 00
```

Combined hex (`42` bytes):

```text
26000000010800f2052a0100000002088877665544332211031001000000000000000000000000000000
```

2. **14-byte metadata block** carrying only a 5 ns budget:

```text
0a000000 0108 0500000000000000
```

3. **Single-chunk request** on stream `0`, message `1`, method `9`, user payload `"abc"` (`616263`), with the 42-byte metadata block above (`TotalSize = 42 + 3 = 45 = 0x2d`, flags `Begin|End|Request|Meta = 0x4b`, aux `9`):

```text
packet:  0400000000000000 0000000000000000 0100000000000000 2d00000000000000
         0000000000000000 2d00000000000000 4b00000000000000 0900000000000000
buffer:  26000000010800f2052a0100000002088877665544332211031001000000000000000000000000000000 616263
```

4. **Two-chunk request** with the same 45-byte body over a 32-byte ring buffer (`aux` is `9` in chunk 0 and `0` in chunk 1 per Protocol 1.0 rules):

```text
chunk 0 packet: 04.. | conn 0 | msg 1 | total 0x2d | offset 0    | size 0x20 | flags 0x49 (Begin|Request|Meta) | aux 9
chunk 0 buffer: 26000000010800f2052a01000000020888776655443322110310010000000000
chunk 1 packet: 04.. | conn 0 | msg 1 | total 0x2d | offset 0x20 | size 0x0d | flags 0x4a (End|Request|Meta)   | aux 0
chunk 1 buffer: 00000000000000000000 616263
```

## 4. Responder Request Lifecycle

On the responder side, Protocol 1.1 extends request handling with in-flight registration, per-request context cancellation, and response suppression for canceled calls.

### 4.1 Reassembly and In-Flight Registration

1. The responder reassembles all chunks of the request (`SPEC.md` section 10.5). If `AdvFlagMeta` is set on the `Begin` chunk, the metadata block is validated and stripped, exposing only `user_payload` to the handler.
2. Before placing the assembled request into the incoming request queue, the responder registers the request as **in flight** under `(connectionID, messageID)` and removes that registration when request processing finishes (whether answered, canceled, or skipped).
3. If the responder has initiated graceful shutdown (section 6), the arriving request is not queued or registered; instead, the responder immediately queues an error response with status `ErrCodeShuttingDown` (`0x08`).
4. If the incoming request queue is full, the configured overflow policy (`OverflowBlock`, `OverflowDrop`, or `OverflowFail`) applies.

### 4.2 Per-Request Handler Context

Each request dispatched to the application's `Serve` handler receives a dedicated context that is canceled as soon as the earliest of the following events occurs:

| Trigger Event | Cancellation Cause Reported to Handler |
| --- | --- |
| `OpCancel` frame arrives for `(connectionID, messageID)` (`FeatureCancel`) | Canceled (`context.Canceled`) |
| Request time budget `T0 + budget` expires (`FeatureMetadata`) | Deadline exceeded (`context.DeadlineExceeded`) |
| The stream carrying the request is closed by either endpoint | Canceled |
| The responder's `Serve` loop exits or the link is closed | Canceled |
| The handler function returns | Canceled |

When a worker dequeues the request to invoke the handler, it computes `remaining = budget - (now - T0)`. If `remaining <= 0` (or if an `OpCancel` arrived while the request was sitting in the queue), the handler is **skipped** entirely (section 4.3).

### 4.3 Skipping Canceled Requests and Suppressing Stale Responses

- **Pre-handler skip**: If a queued request is canceled or its deadline expires before a handler worker starts executing it, the responder does not invoke the handler and sends no response frame.
- **Post-handler response suppression**: If the handler returns after its request context was canceled specifically by `OpCancel` or by request deadline expiration (`T0 + budget`), the responder suppresses the response frame (because the requester has already abandoned the call). For other termination causes (such as `Serve` shutdown while a handler finishes), the responder still attempts to transmit the handler's response.

## 5. Requester `Call` Procedure

When the application invokes `Call(ctx, method, payload [, CallOptions])`:

1. **Metadata prefix construction**: If `FeatureMetadata` is negotiated, inspect `ctx` for a deadline (`budget = deadline - now`, failing locally if `<= 0`) and inspect `CallOptions` for non-zero `CallerID` and `TraceID`. If any of the three is present, prepend the 4-byte header + TLV block to `payload` and set `AdvFlagMeta` on every outgoing chunk (`MaxMessageSize` applies to `len(block) + len(payload)`).
2. **Chunk transmission**: Register the pending call under `(streamID, messageID)` and publish the request chunks. If transmission fails after at least one chunk was published, emit an `OpConnCopy` frame with `AdvFlagAbort` (`SPEC.md` section 10.4) and do not send `OpCancel`.
3. **Response wait and cancellation**: Block awaiting the response frame or `ctx.Done()`. If `ctx` ends first, move `(streamID, messageID)` into the bounded abandoned-call set, queue `OpCancel(streamID, messageID)` on the control sender if `FeatureCancel` is negotiated, and return `ctx.Err()`.

## 6. Graceful Responder Shutdown (`ErrCodeShuttingDown = 0x08`)

Graceful shutdown (`AdvancedLink.Shutdown(ctx)` in Go, `hqq_adv_shutdown` in C) allows a server to stop accepting new RPC requests while draining all requests already in flight:

- **Wire error status `0x08`**: Protocol 1.1 defines `ErrCodeShuttingDown = 0x08`.
- **Refusal of new requests**: Once shutdown begins on a responder, every newly assembled request is immediately answered with an error response frame (`flags = Begin|End|Response|Error = 0x33`, `TotalSize = 0`, `aux = 0x08`) via the outbound control queue, causing the requester's `Call` to return `ErrShuttingDown` (`ResponseError{Status: 8}`).

Example 64-byte refusal response for request message `2` on stream `0`:

```text
0400000000000000 0000000000000000 0200000000000000 0000000000000000
0000000000000000 0000000000000000 3300000000000000 0800000000000000
```

- **Draining in-flight requests**: Requests that were registered before shutdown began (whether queued or actively running in a handler) run to completion and send their normal responses. `Shutdown(ctx)` blocks until the in-flight request count reaches zero or `ctx` expires, after which `Serve` returns cleanly (`nil` / `HQQ_OK`).
- **Link usability after shutdown**: Completing `Shutdown` stops only the responder role; open streams, plain messaging, and outbound `Call` requests from this endpoint remain usable until `Close()` is called.

## 7. Wire Error and Validation Summary

| Wire Condition | Receiver Action |
| --- | --- |
| `AdvFlagMeta` set when `FeatureMetadata` was not negotiated | Fail receive path with `ErrProtocol` |
| `AdvFlagMeta` set on a frame without `AdvFlagRequest` | Fail receive path with `ErrProtocol` |
| Any flag bit above bit 6 (`flags & ~0x7f != 0`) | Fail receive path with `ErrProtocol` |
| Malformed metadata header or TLV record (section 3.2) | Fail receive path with `ErrProtocol` |
| `OpCancel` (`0x07`) received when `FeatureCancel` was not negotiated | Fail receive path with `ErrUnexpectedPacket` |
| `OpCancel` received for an unknown or completed `(streamID, messageID)` | Silently ignore |
| Request arrives while responder is shutting down | Reply with error response (`flags = 0x33`, `aux = 0x08`) |
| Request exceeds `MaxReassemblyBytes` | Reply with error response (`flags = 0x33`, `aux = 0x07`) |

Complete wire `ErrorCode` table (`OpError` operand 1 and error-response `aux`):

- `0x01`: `ErrCodeMemoryAlign` (`ErrMemoryAlign`)
- `0x02`: `ErrCodeInvalidSize` (`ErrInvalidSize`)
- `0x03`: `ErrCodeMemorySmall` (`ErrMemorySmall`)
- `0x04`: `ErrCodeFailedInit` (`ErrFailedInit`)
- `0x05`: `ErrCodeConnNotFound` (`ErrConnNotFound`)
- `0x06`: `ErrCodeInvalidOp` (`ErrInvalidOp`)
- `0x07`: `ErrCodeBufferOverflow` (`ErrBufferOverflow`)
- `0x08`: `ErrCodeShuttingDown` (`ErrShuttingDown`, Protocol 1.1)

## 8. Non-Normative Implementation Notes

In the Go reference implementation (`advanced_request.go`):

- Every incoming request is assigned a pooled request context before entering the request queue, indexed in a mutex-guarded `(streamID, messageID)` map so `OpCancel`, stream close, or link close can cancel it while still queued.
- Per-request deadline timers (`time.AfterFunc`) are pooled and reset across requests, and completion channels (`ctx.Done()`) are allocated lazily only if the handler calls `Done()`.
- Outbound `OpCancel` packets and shutdown refusal responses are transmitted asynchronously through the bounded control-sender goroutine so the receive loop never blocks on the TX ring.
