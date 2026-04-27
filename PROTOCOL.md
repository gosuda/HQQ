# HQQ Standard Protocol Specification

## Scope

This document defines the **Standard Protocol** used by `StandardLink`. The goal is a fast, lock-free, shared-memory data path for one primary endpoint and one secondary endpoint.

Advanced features such as protocol negotiation, logical connections, compression, encryption, QoS, and reliable delivery are outside the Standard Protocol data path. They may be layered later, but `StandardLink` is intentionally kept small and deterministic.

## Design Goals

1. **Fixed ABI**: packets have a stable byte layout independent of Go struct padding or `uintptr` width.
2. **Little-endian encoding**: all packet words are encoded with `binary.LittleEndian`.
3. **Lock-free ownership transfer**: data rings and free-buffer rings use atomic MPMC queues; no mutex is required for the StandardLink hot path.
4. **No unread-buffer overwrite**: payload buffers are recycled only after the receiver copies the payload out of shared memory.
5. **Bounded memory**: all rings and payload buffers are allocated up front.

## Shared Memory Layout

The Standard Protocol allocates two independent directions. Each direction has:

- one packet ring for published data packets
- one free-buffer ring for returning payload-buffer ownership
- one fixed-size payload buffer pool

```text
PAGE_START
  DATA_RING_0   protocol.Packet ring, Primary -> Secondary
PAGE_BREAK
  FREE_RING_0   uint64 ring, free buffer indexes for DATA_RING_0
PAGE_BREAK
  BUFFERS_0     bufferCount * bufferSize bytes, Primary -> Secondary payloads
PAGE_BREAK
  DATA_RING_1   protocol.Packet ring, Secondary -> Primary
PAGE_BREAK
  FREE_RING_1   uint64 ring, free buffer indexes for DATA_RING_1
PAGE_BREAK
  BUFFERS_1     bufferCount * bufferSize bytes, Secondary -> Primary payloads
PAGE_END
```

All regions are page-aligned. `bufferCount` must be a power of two and at least 2. `bufferSize` must be at least 8 bytes and a multiple of 8.

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
| `Operand1` | `BufferIndex`, index into the sender direction's payload buffer pool |
| `Operand2` | `DataSize`, number of valid bytes in the buffer |
| `Operand3..6` | Reserved, must be zero |

`DataSize` must satisfy `0 <= DataSize <= bufferSize`.

## Buffer Ownership Protocol

Each direction starts with every buffer index in its free ring.

### Send path

1. Sender dequeues `BufferIndex` from the direction's free ring.
2. Sender copies the payload into `BUFFERS[BufferIndex]`.
3. Sender enqueues `OpStandardLinkCopy` to the direction's data ring.

### Receive path

1. Receiver dequeues one packet from the direction's data ring.
2. Receiver validates `BufferIndex` and `DataSize`.
3. Receiver copies payload bytes out of shared memory into the caller buffer or the link's local remainder buffer.
4. Receiver enqueues `BufferIndex` back to the direction's free ring.

This makes buffer reuse explicit and prevents a producer from overwriting unread payloads.

## Backpressure

The protocol has two bounded backpressure points:

- **Free ring empty**: all payload buffers for the direction are currently in flight; writers wait until receivers return a buffer.
- **Data ring full**: data packets are not being consumed fast enough; writers wait before publishing more packets.

`StandardLink` exposes write deadlines through `net.PacketConn` deadline methods. On timeout, it returns `ErrTimeout` and returns any unpublished acquired buffer to the free ring.

## StandardLink Semantics

- `Write` publishes at most one packet and rejects payloads larger than `bufferSize` with `ErrBufferOverflow`.
- `Read` returns after delivering data from one packet, or from a previously saved local remainder.
- If the caller's read buffer is smaller than the payload, the unread tail is copied into a local per-link remainder buffer and the shared payload buffer is immediately returned to the free ring.
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
- explicit free-buffer ownership transfer
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
