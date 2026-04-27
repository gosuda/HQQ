# HQQ Standard Protocol Specification

## Scope

This document defines the **Standard Protocol** used by `StandardLink`. The goal is a fast, lock-free, shared-memory data path for one primary endpoint and one secondary endpoint.

Current Standard Protocol version: **1.0**.

Advanced features such as protocol negotiation, logical connections, compression, encryption, QoS, and reliable delivery are outside the Standard Protocol data path. They may be layered later, but `StandardLink` is intentionally kept small and deterministic.

## Design Goals

1. **Fixed ABI**: packets have a stable byte layout independent of Go struct padding or `uintptr` width.
2. **Little-endian encoding**: all packet words are encoded with `binary.LittleEndian`.
3. **Lock-free ownership transfer**: data ring slots own same-index payload buffers; no mutex is required for the StandardLink hot path.
4. **No unread-buffer overwrite**: a payload buffer is recycled only after the receiver releases the owning ring slot.
5. **Bounded memory**: all rings and payload buffers are allocated up front.

## Shared Memory Layout

The Standard Protocol allocates two independent directions. Each direction has:

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

## Standard Data Operation

### `OpStandardLinkCopy` (`0xFF`)

Transfers one payload stored in a direction-local buffer pool.

| Operand | Meaning |
| --- | --- |
| `Operand0` | `CopyID`, monotonically increasing per endpoint |
| `Operand1` | `BufferIndex`, diagnostic mirror of the claimed data-ring slot index |
| `Operand2` | `DataSize`, number of valid bytes in the buffer |
| `Operand3..6` | Reserved, must be zero |

`DataSize` must satisfy `1 <= DataSize <= bufferSize`. Empty `Write` calls do not publish packets.

## Buffer Ownership Protocol

Each direction starts with every data-ring slot available. Slot `i` owns `BUFFERS[i]`.

### Send path

1. Sender claims one producer slot from the direction's data ring.
2. Sender writes the payload into the same-index payload buffer, `BUFFERS[slot]`.
3. Sender stores `OpStandardLinkCopy` in the claimed packet slot. `Operand1` mirrors `slot`.
4. Sender publishes the packet by advancing the slot sequence.

### Receive path

1. Receiver claims one consumer slot from the direction's data ring.
2. Receiver uses the claimed slot index to locate `BUFFERS[slot]` and validates `DataSize`.
3. Receiver either copies payload bytes out of shared memory (`Read`) or passes the shared buffer directly to the callback (`ReadZeroCopy`).
4. Returning from the dequeue callback releases the packet slot and `BUFFERS[slot]` back to producers.

This ties payload lifetime to ring-slot lifetime and prevents a producer from overwriting unread payloads.

> WARNING: Zero-copy callbacks run while the underlying ring slot is claimed. They must return promptly; if they block or never return, the whole ring can suffer head-of-line (HOL) blocking. The shared buffer must not be retained after the callback returns.

## Backpressure

The protocol has one bounded backpressure point per direction:

- **Data ring full**: every slot and therefore every same-index payload buffer is currently in flight; writers wait until receivers release slots.

`StandardLink` exposes write deadlines through `net.PacketConn` deadline methods. On timeout, it returns `ErrTimeout` if no slot was claimed before the deadline. After a zero-copy callback starts, cancellation is not checked until the callback returns because the slot must be published or released to preserve ring progress.

## StandardLink Semantics

- `Write` publishes at most one packet and rejects payloads larger than `bufferSize` with `ErrBufferOverflow`.
- `Read` returns after delivering data from one packet, or from a previously saved local remainder.
- `WriteZeroCopy` and `ReadZeroCopy` expose the slot-owned payload buffer directly for the duration of the callback.
- If the caller's read buffer is smaller than the payload, the unread tail is copied into a local per-link remainder buffer and the shared payload buffer is released as soon as the dequeue callback returns.
- `ReadFrom` and `WriteTo` are thin `net.PacketConn` adapters over `Read` and `Write`.

## Error Codes

| Error Code | Hex | Meaning |
| --- | ---: | --- |
| `ErrCodeMemoryAlign` | `0x01` | Shared memory or buffer alignment violation |
| `ErrCodeInvalidSize` | `0x02` | Invalid buffer count, buffer size, or buffer index |
| `ErrCodeMemorySmall` | `0x03` | Shared memory region is too small |
| `ErrCodeFailedInit` | `0x04` | Ring initialization or attachment failed |
| `ErrCodeConnNotFound` | `0x05` | Reserved for connection-oriented extensions |
| `ErrCodeInvalidOp` | `0x06` | Unknown or unsupported opcode |
| `ErrCodeBufferOverflow` | `0x07` | Payload exceeds configured `bufferSize` |

## Current Implementation Status

Implemented and covered by tests:

- fixed-size little-endian packet ABI
- StandardLink primary/secondary creation
- bidirectional StandardLink read/write
- slot-owned payload-buffer transfer
- zero-copy read/write callback APIs
- unread-buffer overwrite regression coverage
- partial-read remainder handling
- read/write deadline timeout behavior
- high-volume ordered delivery smoke test
- StandardLink throughput benchmarks

Not part of the Standard Protocol data path yet:

- logical connection lifecycle
- compression/encryption/QoS
- end-to-end retransmission/reliability
- cross-endian mixed-architecture process pairs beyond the packet ABI
