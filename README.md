# HQQ: High-Throughput Shared-Memory IPC for Go

**HQQ** (`gosuda.org/hqq`) is a Go inter-process communication (IPC) library built on shared memory, lock-free Vyukov-style MPMC rings, and slot-owned payload buffers.

On an earlier revision it measured **sub-microsecond round-trip latency (~258 ns)** and **multi-GB/s throughput (27–64 GB/s)** on `darwin/arm64` while idle blocked callers parked in the kernel instead of spinning (`futex` on Linux, `__ulock_wait` on macOS, named kernel semaphores on Windows). See [Benchmarks](#benchmarks): those numbers predate v0.3.0 and have not been re-measured.

> [!IMPORTANT]
> v0.3.0 is a breaking release (public API and shared-memory ABI). Segments written by earlier revisions (`HQQ_STL2`) are rejected with `ErrIncompatibleLayout` and left untouched, so remove a name left behind by an older version with `hqq.Unlink(name)` (or by deleting `/dev/shm/<name>` on Linux) before reusing it. Both endpoints of a link must run v0.3.0. See the migration table in [CHANGELOG.md](CHANGELOG.md).

---

## Highlights

- **Two composable protocol layers over one shared-memory layout**:
  - **Standard Protocol (`StandardLink`)**: fixed-capacity messages (`Read`, `Write`, `ReadZeroCopy`, `WriteZeroCopy`, `ReserveRead`, `ReserveWrite`, and `net.PacketConn` with datagram `ReadFrom`/`WriteTo` and deadlines) with zero heap allocations on the hot path.
  - **Advanced Protocol (`AdvancedLink`)**: negotiated overlay adding multi-chunk large-message transfer (`SendLarge`, `Receive`), concurrent out-of-order RPC (`Call`, `CallOn`, `Respond`, `Serve`, `Mux`), and logical stream multiplexing (`ListenStreams`, `AcceptStream`, `OpenStream`, `CloseStream`).
- **Adaptive cross-process parking (`WaitAdaptive`)**: re-checks the ring in a short tight loop, yields with `runtime.Gosched`, then parks in the OS kernel (`FUTEX_WAIT` on Linux, `UL_COMPARE_AND_WAIT_SHARED` on macOS, named semaphores for named segments on Windows). All goroutines of a process blocked on the same ring side share one kernel park (waiter aggregation), so N blocked goroutines pin one OS thread.
- **Crash and lifecycle detection (`HQQ_STL3` superblock)**: per-endpoint session tokens and heartbeats distinguish graceful peer shutdown (`io.EOF` from `Read`, `ErrPeerClosed` elsewhere) from abrupt termination (`ErrPeerGone`). Segments whose endpoints have all closed or died are re-initialized, a third endpoint is rejected (`ErrLinkBusy`), a dead initializer is taken over, and a foreign or other-version object is never modified.
- **Formal verification and checks**: the MPMC ring (claim, completion CAS, FIFO order, progress) is model-checked with TLC ([`internal/mpmc/MPMC.tla`](internal/mpmc/MPMC.tla), bounded model, not a proof). CI runs `go vet ./...` (including `unsafeptr`), `go test ./...`, `go test -race ./...` (not on Windows), and fuzz smoke tests.

---

## Requirements

- Go 1.24 or newer.
- `bufferCount`: power of two, `2 ≤ bufferCount ≤ 1<<24` (default `256` in `hqq.Config`).
- `bufferSize`: multiple of 8 bytes, `8 ≤ bufferSize ≤ 1<<30` (default `4096` in `hqq.Config`).
- A target with native 64-bit atomics that are atomic across processes: `GOARCH=mips`, `GOARCH=mipsle` and `GOARCH=arm` with `GOARM` below 7 are rejected at build time (see [Build constraints](#build-constraints)).

---

## Quick Start

### 1. Named RPC and large-message IPC (`Listen` / `Dial` + `Mux`)

For most inter-process RPC and large-payload workloads, use `hqq.Listen` on the primary process and `hqq.Dial` on the secondary process. Both create or attach the shared-memory segment and complete Advanced Protocol negotiation:

```go
package main

import (
	"context"
	"fmt"
	"time"

	"gosuda.org/hqq"
)

const MethodEcho uint64 = 1

func main() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	const name = "hqq_quickstart"

	mux := hqq.NewMux()
	mux.HandleFunc(MethodEcho, func(ctx context.Context, req hqq.AdvancedMessage) ([]byte, error) {
		return append([]byte("echo: "), req.Payload...), nil
	})

	// Primary endpoint: creates the segment and serves requests. Listen also
	// reuses a segment left behind by a crashed listener (both endpoints
	// dead) and fails with ErrLinkBusy if an endpoint is still alive.
	serverErr := make(chan error, 1)
	go func() {
		server, err := hqq.Listen(ctx, name, hqq.Config{})
		if err != nil {
			serverErr <- err
			return
		}
		err = server.Serve(ctx, mux)
		_ = server.Close() // removes the name
		serverErr <- err
	}()

	// Secondary endpoint: Dial retries until the listener has created and
	// initialized the segment, or ctx ends.
	client, err := hqq.Dial(ctx, name, hqq.Config{})
	if err != nil {
		panic(err)
	}
	defer client.Close()

	resp, err := client.Call(ctx, MethodEcho, []byte("hello HQQ"))
	if err != nil {
		panic(err)
	}
	fmt.Println(string(resp)) // Output: echo: hello HQQ

	// Closing the client makes Serve return nil on the listener side, which
	// then closes the server link and removes the name.
	client.Close()
	if err := <-serverErr; err != nil {
		panic(err)
	}
}
```

Logical streams on a negotiated link, given the Primary `server` and the Secondary `client` (`*hqq.NamedAdvancedLink` values returned by `Listen` and `Dial`). The acceptor must call `ListenStreams` first, otherwise the peer's `OpenStream` fails with `ErrConnNotFound`:

```go
// Primary: accept streams opened by the peer.
if err := server.ListenStreams(ctx); err != nil {
	panic(err)
}
go func() {
	id, err := server.AcceptStream(ctx) // visible only after its accept reply is on the ring
	if err != nil {
		return
	}
	_, _ = server.SendLarge(ctx, id, []byte("hello on stream"))
}()

// Secondary: open a stream, use it, close it.
id, err := client.OpenStream(ctx) // Secondary IDs are odd, Primary IDs even; 0 is link-level
if err != nil {
	panic(err)
}
msg, err := client.Receive(ctx)
if err != nil {
	panic(err)
}
fmt.Println(msg.ConnectionID == id, string(msg.Payload))
if err := client.CloseStream(ctx, id); err != nil { // the ID is never reused
	panic(err)
}
```

### 2. Named `StandardLink` (`CreateStandardLink` / `OpenStandardLink`)

When your messages fit within `bufferSize` and you want raw `io.ReadWriter` / zero-copy ring performance without protocol negotiation:

```go
// The creator removes the name when it closes (if it still refers to this
// segment). An existing name whose endpoints are all gone is reused.
primary, err := hqq.CreateStandardLink("my_std_link", 256, 4096, 0o600, hqq.LinkOptions{})
if err != nil {
	panic(err)
}
defer primary.Close()

secondary, err := hqq.OpenStandardLink("my_std_link", 256, 4096, hqq.LinkOptions{})
if err != nil {
	panic(err)
}
defer secondary.Close()

_, _ = primary.Write([]byte("ping"))

// Zero-copy read directly from the slot-owned shared memory buffer:
_, _ = secondary.ReadZeroCopy(func(payload []byte) error {
	fmt.Println(string(payload))
	return nil
})
```

### 3. In-process or custom shared memory (`NewStandardLink`)

No `unsafe.Pointer` arithmetic is required in user code. Pass a page-aligned `[]byte` from `hqq.AlignedBuffer` (or your own `MAP_SHARED` mapping) to `hqq.NewStandardLink`; the first endpoint becomes the Primary, the second the Secondary:

```go
mem := hqq.AlignedBuffer(hqq.SizeStandardLink(128, 4096))

primary, err := hqq.NewStandardLink(mem, 128, 4096, hqq.LinkOptions{})
if err != nil {
	panic(err)
}
defer primary.Close()

secondary, err := hqq.NewStandardLink(mem, 128, 4096, hqq.LinkOptions{})
if err != nil {
	panic(err)
}
defer secondary.Close()
```

`hqq.NewAdvancedLink(mem, 128, 4096, hqq.AdvancedOptions{})` does the same for an `AdvancedLink`; call `Negotiate` on both endpoints before using Advanced APIs.

---

## Architecture Overview

Shared memory is divided into a first-page superblock (`HQQ_STL3`, layout version 3.0) and two independent directional channels:

```text
SUPERBLOCK    layout version, required features, geometry, epoch, init/claim state, creator token, endpoint records (token/PID/heartbeat)
DATA_RING_0   protocol.Packet MPMC ring, Primary -> Secondary
BUFFERS_0     fixed payload buffers for direction 0 (slot i owns BUFFERS_0[i])
DATA_RING_1   protocol.Packet MPMC ring, Secondary -> Primary
BUFFERS_1     fixed payload buffers for direction 1 (slot i owns BUFFERS_1[i])
```

Each data-ring slot `i` owns the same-index payload buffer `BUFFERS[i]`. Senders claim a producer slot, fill `BUFFERS[slot]`, and publish a 64-byte `protocol.Packet`. Receivers claim the consumer slot, either copy the payload out (`Read`) or inspect it in place (`ReadZeroCopy` / `ReserveRead`), and release the slot when done. Because `BUFFERS[slot]` cannot be claimed by another producer until the consumer releases slot `i`, unread payloads can never be overwritten.

See [PROTOCOL.md](PROTOCOL.md) for the full wire and memory layout specification.

---

## Benchmarks

> [!NOTE]
> The tables below were measured on an earlier revision (before the v0.3.0 hardening pass) on `darwin/arm64` (Go 1.24, `go test . -bench 'Benchmark(StandardLink|AdvancedLink|Compare)' -benchmem`). They have not been re-measured. In v0.3.0, completing a slot through a copyable handle (`WriteZeroCopy`, `ReserveWrite`/`Commit`/`Abort`, `ReserveRead`/`Release`) uses a compare-and-swap instead of a plain store, and waiters are aggregated per process; re-run the benchmarks on your hardware before relying on absolute numbers.

### HQQ vs Unix Domain Socket (`SOCK_STREAM`) vs OS Pipe

| Benchmark | HQQ (`WaitAdaptive`) | HQQ (`WaitSpin`) | Unix Domain Socket | OS Pipe | HQQ Speedup |
| --- | ---: | ---: | ---: | ---: | ---: |
| **64 B Round-Trip Latency** | **258 ns/op** | 259 ns/op | 3,134 ns/op | 3,362 ns/op | **12.1×–13.0× faster** |
| **4 KiB Unidirectional Throughput** | **27,507 MB/s** (149 ns/op) | 26,722 MB/s | 2,594 MB/s | 4,312 MB/s | **6.4×–10.6× higher** |
| **64 KiB Unidirectional Throughput** | **29,774 MB/s** (2.20 µs/op) | 28,029 MB/s | 3,073 MB/s | 11,812 MB/s | **2.5×–9.7× higher** |
| **Idle Blocked Reader CPU** | **~0.1% CPU** | ~100% CPU | ~0% CPU | ~0% CPU | Kernel futex/ulock parking |

### StandardLink & AdvancedLink Microbenchmarks

| Operation | Latency | Throughput | Allocs |
| --- | ---: | ---: | ---: |
| `StandardLink` Zero-Copy (`4 KiB`) | **63.6 ns/op** | **64,364 MB/s** | `0 B/op`, `0 allocs/op` |
| `StandardLink` Reserve/Commit (`4 KiB`) | **87.6 ns/op** | **46,738 MB/s** | `0 B/op`, `0 allocs/op` |
| `StandardLink` Pipeline (`64 B`) | **50.6 ns/op** | **1,265 MB/s** | `0 B/op`, `0 allocs/op` |
| `StandardLink` Pipeline (`4 KiB`) | **138.6 ns/op** | **29,543 MB/s** | `0 B/op`, `0 allocs/op` |
| `AdvancedLink` Large Copy (`16 KiB` over `4 KiB` chunks) | **1.64 µs/op** | **2,494 MB/s** | `4,192 B/op`, `2 allocs/op` |
| `AdvancedLink` Request-Response RPC (`2 KiB` req + `2 KiB` resp) | **1.47 µs/op** | **1,396 MB/s** | `2,241 B/op`, `4 allocs/op` |

---

## API Guide

The signatures below are those of `go doc -all .`; the godoc comments are the reference for each method's error behavior.

### Sizing & Opening

```go
// High-level Listen / Dial (create or open a named segment and negotiate AdvancedLink)
func Listen(ctx context.Context, name string, cfg Config) (*NamedAdvancedLink, error)
func Dial(ctx context.Context, name string, cfg Config) (*NamedAdvancedLink, error)
func Unlink(name string) error

// Layout sizing & helpers
func SizeStandardLink(bufferCount int, bufferSize int) uintptr // 0 for an invalid or unaddressable configuration
func AlignedBuffer(size uintptr) []byte                        // page-aligned, heap-backed
func LargeMessageThreshold(bufferSize int) int
func IsLargeMessage(bufferSize int, payloadSize int) bool
func CurrentProtocolVersion() ProtocolVersion

// Caller-owned memory (page-aligned, at least SizeStandardLink bytes)
func NewStandardLink(mem []byte, bufferCount int, bufferSize int, opts LinkOptions) (*StandardLink, error)
func NewAdvancedLink(mem []byte, bufferCount int, bufferSize int, opts AdvancedOptions) (*AdvancedLink, error)

// Named segments: Create* is the creator side, Open* the opener side
func CreateStandardLink(name string, bufferCount int, bufferSize int, perm os.FileMode, opts LinkOptions) (*NamedStandardLink, error)
func OpenStandardLink(name string, bufferCount int, bufferSize int, opts LinkOptions) (*NamedStandardLink, error)
func CreateAdvancedLink(name string, bufferCount int, bufferSize int, perm os.FileMode, opts AdvancedOptions) (*NamedAdvancedLink, error)
func OpenAdvancedLink(name string, bufferCount int, bufferSize int, opts AdvancedOptions) (*NamedAdvancedLink, error)

// Named links (NamedStandardLink and NamedAdvancedLink embed their link type)
func (l *NamedStandardLink) Name() string
func (l *NamedStandardLink) Unlink() error // remove the name now if it still refers to this segment; mappings stay valid
func (l *NamedStandardLink) Close() error  // close, unmap, and remove the name if this endpoint created it and it still refers to this segment
func (l *NamedAdvancedLink) Name() string
func (l *NamedAdvancedLink) Unlink() error
func (l *NamedAdvancedLink) Close() error
```

`NewStandardLink` / `NewAdvancedLink` validate in this order: empty slice (`ErrMemorySmall`), page alignment (`ErrMemoryAlign`), configuration (`ErrInvalidSize`), slice length (`ErrMemorySmall`). A geometry that differs from an existing segment returns `*GeometryError` (matches `ErrInvalidSize`; use `errors.As` for `Want`/`Got`).

### `StandardLink` Methods

```go
func (l *StandardLink) Read(b []byte) (int, error) // io.EOF after a graceful peer Close
func (l *StandardLink) Write(b []byte) (int, error)
func (l *StandardLink) ReadFrom(p []byte) (n int, addr net.Addr, err error) // datagram semantics
func (l *StandardLink) WriteTo(p []byte, _ net.Addr) (n int, err error)     // addr is ignored
func (l *StandardLink) ReadZeroCopy(fn func(buffer []byte) error) (int, error)
func (l *StandardLink) WriteZeroCopy(fn func(buffer []byte) (int, error)) (int, error)
func (l *StandardLink) ReserveRead() (StandardReadReservation, error)
func (l *StandardLink) ReserveWrite() (StandardWriteReservation, error)
func (l *StandardLink) SetDeadline(t time.Time) error
func (l *StandardLink) SetReadDeadline(t time.Time) error
func (l *StandardLink) SetWriteDeadline(t time.Time) error
func (l *StandardLink) LocalAddr() net.Addr // HQQAddr "hqq-primary" or "hqq-secondary": this endpoint's role
func (l *StandardLink) PeerState() PeerState
func (l *StandardLink) Mode() LinkMode
func (l *StandardLink) Type() LinkType
func (l *StandardLink) BufferCount() int
func (l *StandardLink) BufferSize() int
func (l *StandardLink) Close() error

func (r *StandardWriteReservation) Buffer() []byte // nil once committed/aborted, also through a copy
func (r *StandardWriteReservation) Commit(n int) error
func (r *StandardWriteReservation) Abort(reason ErrorCode) error
func (r *StandardWriteReservation) Slot() uint64
func (r *StandardReadReservation) Buffer() []byte // nil once released, also through a copy
func (r *StandardReadReservation) Release() error
func (r *StandardReadReservation) Slot() uint64
```

- **One read call returns bytes of at most one message.** A message that did not fit into a `Read` buffer leaves its unread tail queued (FIFO, per endpoint); the following read calls return the queued tails, one per call, before newer messages. `ReadZeroCopy` and `ReserveRead` serve the whole oldest tail. With concurrent readers, messages and tails may be received in any order but are never mixed within one call.
- **`ReadFrom` is a datagram read**: it copies `len(p)` bytes and discards the rest of the message with `n == len(p)` and a nil error (an empty `p` consumes one message). A pending tail left by `Read`/`ReadZeroCopy` is returned as the next datagram (truncated like any other). The returned address is the peer's role address (`"hqq-secondary"` on a Primary link, `"hqq-primary"` on a Secondary one); `LocalAddr` returns this endpoint's own.
- Empty `Write` calls do not publish a message; payloads larger than `BufferSize` fail with `ErrBufferOverflow`.

> [!WARNING]
> `ReadZeroCopy`, `WriteZeroCopy`, `ReserveRead`, and `ReserveWrite` hold the underlying ring slot while active. Callbacks must return promptly, and reservations must always be completed (`Commit`/`Abort` for writes, `Release` for reads) to avoid head-of-line (HOL) blocking. Never retain the shared `[]byte` slice after the callback returns, after `Release`/`Commit`/`Abort`, or after `Close`. Reservation values may be copied by accident: completing a slot through a copy invalidates the original (and vice versa), so a second `Commit`/`Abort`/`Release` returns `ErrInvalidOp`, `Buffer` returns nil, and the slot is neither completed twice nor are waiters woken twice; the slot itself must still be completed once. A callback that panics does not wedge the ring: `WriteZeroCopy` publishes a tombstone and `ReadZeroCopy` releases the slot before the panic propagates.
>
> `Close` waits for running zero-copy callbacks, so it must not be called from inside one (deadlock). `Close` does not track reservations: finish every reservation before calling `Close`.

### `AdvancedLink` & RPC Methods

```go
// Role-aware one-call negotiation (Primary initiates, Secondary waits)
func (l *AdvancedLink) Negotiate(ctx context.Context, features uint64) error
func (l *AdvancedLink) NegotiateProtocol(ctx context.Context, version ProtocolVersion, features uint64) (bool, error)
func (l *AdvancedLink) WaitForNegotiation(ctx context.Context) (bool, error)
func (l *AdvancedLink) ProtocolState() ProtocolState
func (l *AdvancedLink) NegotiatedVersion() ProtocolVersion
func (l *AdvancedLink) NegotiatedFeatures() uint64 // test with FeatureLargeCopy / FeatureRequestResponse

// Large-data transfer & message reception
func (l *AdvancedLink) SendLarge(ctx context.Context, connectionID uint64, payload []byte) (uint64, error)
func (l *AdvancedLink) Receive(ctx context.Context) (AdvancedMessage, error) // aborted messages are never returned

// Request-Response RPC
func (l *AdvancedLink) Call(ctx context.Context, methodID uint64, request []byte) ([]byte, error)
func (l *AdvancedLink) CallOn(ctx context.Context, connectionID uint64, methodID uint64, request []byte) (AdvancedMessage, error)
func (l *AdvancedLink) Respond(ctx context.Context, request AdvancedMessage, status uint64, response []byte) error
func (l *AdvancedLink) RespondError(ctx context.Context, request AdvancedMessage, status uint64, response []byte) error
func (l *AdvancedLink) Serve(ctx context.Context, h Handler) error

// Logical streams (Primary allocates even IDs 2, 4, ...; Secondary odd IDs 1, 3, ...; 0 is link-level)
func (l *AdvancedLink) ListenStreams(ctx context.Context) error
func (l *AdvancedLink) AcceptStream(ctx context.Context) (uint64, error)
func (l *AdvancedLink) OpenStream(ctx context.Context) (uint64, error)
func (l *AdvancedLink) CloseStream(ctx context.Context, connectionID uint64) error

// Link information and lifecycle
func (l *AdvancedLink) Stats() AdvancedStats
func (l *AdvancedLink) PeerState() PeerState
func (l *AdvancedLink) Mode() LinkMode
func (l *AdvancedLink) Type() LinkType
func (l *AdvancedLink) BufferCount() int
func (l *AdvancedLink) BufferSize() int
func (l *AdvancedLink) Read(b []byte) (n int, err error)  // only before negotiation starts
func (l *AdvancedLink) Write(b []byte) (n int, err error) // only before negotiation starts
func (l *AdvancedLink) Close() error

// RPC dispatch
func NewMux() *Mux
func (m *Mux) Handle(methodID uint64, h Handler)
func (m *Mux) HandleFunc(methodID uint64, f func(ctx context.Context, request AdvancedMessage) ([]byte, error))
```

- **Negotiation is terminal on failure.** `ProtocolStateFailed` cannot be left: any error during an attempt (rejection, invalid packets, a canceled context, a closed link) makes later `Negotiate`/`NegotiateProtocol`/`WaitForNegotiation` calls return `ErrNegotiationFailed`. `ErrAlreadyNegotiated` is returned only while an attempt is in progress or after it succeeded. Close the link and open a new one after a failure.
- **Streams.** `CallOn` and `SendLarge` accept connection ID `0` (link-level) or an open stream; any other ID, including a closed stream, returns `ErrConnNotFound`. `CloseStream` removes the local registration, then sends `OpConnClose` to the peer, which removes its registration on receipt; closed IDs are never reused. Requests already in flight on the stream still get their responses, and `Respond` to a request received on a closed stream still works.
- **The receive path never waits for the tx ring.** Stream accepts and refusals, and the error responses to requests refused by `MaxReassemblyBytes`, are sent by a control-sender goroutine through a bounded queue (`max(BufferCount, 16)` replies); a reply that does not fit is dropped and, for a reply to an `OpConnCreate`, counted in `AdvancedStats.DroppedMessages`. The peer's `OpenStream`/`Call` then waits until its own context ends, so always pass a context with a deadline.
- **Incoming-queue overflow** (`AdvancedOptions.IncomingOverflow`, queue size `IncomingQueueSize`, default `bufferCount`): `OverflowBlock` (default) makes the dispatcher wait for `Receive`/`AcceptStream` (responses are not routed meanwhile); `OverflowDrop` drops the message (counted) or refuses the stream with `OpError(ErrCodeBufferOverflow)`; `OverflowFail` stops the dispatcher with `ErrIncomingOverflow`.

### Configuration types

```go
type Config struct {
	BufferCount int          // default DefaultBufferCount (256)
	BufferSize  int          // default DefaultBufferSize (4096)
	Perm        os.FileMode  // permission of the created segment (Listen only), default 0o600
	Features    uint64       // default FeatureLargeCopy|FeatureRequestResponse
	Advanced    AdvancedOptions
}

type LinkOptions struct {
	WaitPolicy        WaitPolicy    // WaitAdaptive (default) or WaitSpin
	HeartbeatInterval time.Duration // default 250ms; negative disables heartbeats
	PeerTimeout       time.Duration // default 5s, raised to at least 4 peer heartbeat intervals
	AttachTimeout     time.Duration // default 5s
}

type AdvancedOptions struct {
	MaxMessageSize     uint64 // default 64 MiB
	MaxReassemblyBytes uint64 // default 4 × MaxMessageSize
	IncomingQueueSize  int    // default bufferCount
	IncomingOverflow   OverflowPolicy
	Link               LinkOptions
}
```

The errors, their `errors.Is` relationships and the packet formats are specified in [PROTOCOL.md](PROTOCOL.md#error-hierarchy).

---

## Operational Guide

### Concurrency Contract

- **Goroutine-safe by default**: `Read`, `Write`, `ReadFrom`, `WriteTo`, `ReadZeroCopy`, `WriteZeroCopy`, `ReserveRead`, `ReserveWrite`, `Call`, `CallOn`, `SendLarge`, `Respond`, `Receive`, `OpenStream`, `AcceptStream`, and `CloseStream` are safe for concurrent use by multiple goroutines. `Serve` runs at most `BufferCount` handlers concurrently.
- **Protocol exclusivity**: once `AdvancedLink` negotiation has started, the Advanced dispatcher owns the underlying Standard rings. `AdvancedLink.Read` and `AdvancedLink.Write` then return `ErrAdvancedActive`.
- **Safe `Close()`**: closing a `StandardLink`, `AdvancedLink`, `NamedStandardLink`, or `NamedAdvancedLink` wakes all parked readers/writers (they return `ErrClosed`) and waits for in-flight calls, including running zero-copy callbacks, before returning or unmapping shared memory. `Close` and `Unlink` on named links may be called concurrently and repeatedly; only the first `Close` unmaps. `AdvancedLink.Close` also stops the dispatcher and the control sender (queued control replies are discarded) and fails pending calls and `OpenStream`s with `ErrClosed`.
- **Not tracked by `Close`**: active reservations. A reservation's buffer is invalid once `Close` has run.

### Crash Recovery & Liveness Semantics

- **Graceful shutdown**: when a peer calls `Close()`, it writes a detached marker into the superblock and wakes parked waiters. Messages it published before closing are still delivered. After that, `StandardLink.Read` and `ReadFrom` return `io.EOF` (so `io.Copy`, `io.ReadAll`, and `bufio.Scanner` see a normal end of stream); `Write`, `ReadZeroCopy`, `ReserveRead`, `Receive`, and `Call` return `ErrPeerClosed`, which matches `errors.Is(err, io.EOF)`. After `Close`, queued read tails are not served any more: reads return `ErrClosed`.
- **Abrupt process crash (`ErrPeerGone`)**: each endpoint refreshes its superblock heartbeat (default every `250ms`, `LinkOptions.HeartbeatInterval`). If the peer is killed (`SIGKILL`) or hangs, the surviving endpoint unblocks all waiters with `ErrPeerGone` once the heartbeat stalls for `LinkOptions.PeerTimeout` (default `5s`, raised to at least four peer heartbeat intervals), measured on the local monotonic clock. `ErrPeerGone` matches `ErrConnNotFound`. It is also returned if the segment was re-initialized under an endpoint or the endpoint was replaced. The recorded PID is diagnostic only. A negative `HeartbeatInterval` disables heartbeats: the peer cannot detect this endpoint's crash and a segment left behind by it is never considered stale.
- **Initializer crash**: an endpoint that dies while initializing a segment is replaced by a creator (`Create*`, `Listen`) or a caller-owned-memory endpoint (`NewStandardLink`, `NewAdvancedLink`) once neither the initializer's token nor its heartbeat has changed for `max(PeerTimeout, 2s)`, as observed on the local monotonic clock (there is no fast path from a stale wall-clock timestamp). Openers (`Open*`, `Dial`) never take over: they wait up to `AttachTimeout` and then fail with a transient error. A slow but live initializer refreshes its heartbeat after every chunk of at most 8 MiB of buffer zeroing and is not taken over; a stalled initializer is given its full window even if that runs past `AttachTimeout`.

### Segment Reuse, Names, and Unlink

How an endpoint treats an existing segment depends on how it reached it. **Openers never initialize, re-initialize, or take over a segment**; recovering an abandoned name is the creator's job.

| Entry point | Role | Empty segment (first page all zero) | Live segment | Segment with no live endpoint |
| --- | --- | --- | --- | --- |
| `CreateStandardLink`, `CreateAdvancedLink`, `Listen` | creator | initializes it | never joins: `ErrLinkBusy` | re-initializes it, becomes Primary, owns the name |
| `OpenStandardLink`, `OpenAdvancedLink`, `Dial` | opener | waits up to `AttachTimeout` for the creator to claim it, then fails with a transient error matching `ErrFailedInit` (the segment is left untouched; `Dial` retries) | joins as Secondary, or `ErrLinkBusy` if two live endpoints are attached or the live endpoint's earlier peer already left | fails immediately with a transient error matching `ErrFailedInit` (`hqq: shared memory segment has no live endpoint`; `Dial` retries until a new `Listen` reuses the name) |
| `NewStandardLink`, `NewAdvancedLink` | none (caller-owned memory) | initializes it | joins as Secondary, or `ErrLinkBusy` | re-initializes it, becomes Primary |

- A first page that is not entirely zero and not a valid segment of this layout version returns `ErrIncompatibleLayout` immediately for every entry point, and the object is not written. A `Ready` segment with another magic or layout major version (including `HQQ_STL2`) or with an unknown required-feature bit also returns `ErrIncompatibleLayout`, before joining or taking over.
- `Create*`/`Listen` over an existing name additionally: an object smaller than the requested segment returns `ErrInvalidSize`; a zero-size object returns a transient error matching `ErrFailedInit` (unlink it to proceed); on Linux a symbolic link, device, or directory returns `ErrInvalidName`. None of these objects are modified or resized. On Windows an existing name always means some process holds a live handle, so a creator gets `ErrLinkBusy`.
- **Safe unlink**: `Close` (for the creator) and `NamedStandardLink.Unlink`/`NamedAdvancedLink.Unlink` remove the name only if it still refers to this endpoint's own instance of the segment, so a name that was removed and created again by somebody else is never removed. Instance identity is the object (Linux: same device and inode as the open descriptor) plus the `creatorToken` stored in the superblock (macOS/FreeBSD: re-open the name read-only and compare the token). A missing or replaced name is a no-op returning nil. Only a creator's `Close` removes the name automatically; an opener's `Close` never does (it may call `Unlink` explicitly).
- **Standalone cleanup**: `hqq.Unlink(name)` removes a name without mapping it (no-op on Windows, where the segment disappears with its last handle). Running processes keep their mappings.

### `Listen` / `Dial` Behavior

- `Listen` creates the segment (rules above), then waits for the peer and negotiates; `ctx` bounds the wait for a peer and the wait for a segment another endpoint is still initializing. Closing the returned link removes the name unless somebody else replaced it.
- **`Dial` retries** with a backoff from 1 ms up to 100 ms only while the name does not exist yet (`os.ErrNotExist`), the creator has not claimed or sized the segment yet, the segment is still being initialized, or the segment has no live endpoint (transient errors that match `ErrFailedInit`; the last one lasts until a new `Listen` reuses the name). Every other error is permanent and returned immediately: for example `ErrLinkBusy`, `ErrIncompatibleLayout`, `ErrInvalidSize` (including `*GeometryError`), and `ErrInvalidName`.
- **`ctx` bounds every wait**, also inside a single attempt (claiming the segment, watching an endpoint for liveness, attaching the rings, negotiation): `Dial` and `Listen` return `ctx.Err()` as soon as `ctx` is done, even if the initializer is stuck. `Dial` and `Listen` need the same `BufferCount`/`BufferSize` on both sides.
- **Dial on a segment without live endpoints.** `Dial` never re-initializes a segment and never becomes Primary. If both endpoints are closed or dead (for example after the listener crashed and its heartbeat went stale), it keeps retrying until a new `Listen` reuses the name or `ctx` ends. Within `PeerTimeout` of a crash the dead endpoint still looks alive: `Listen` fails with `ErrLinkBusy`, and `Dial` fails with `ErrLinkBusy` or, if the dead listener never had a peer, attaches as Secondary and waits in negotiation until `ctx` ends. Start the side that should own the segment with `Listen`, and give both calls a context with a deadline.

### Security Model (Trusted Peer Assumption)

Shared-memory IPC requires both endpoints to map the same physical memory pages as read-write (`PROT_READ | PROT_WRITE`). Consequently:

- **Trusted peers**: HQQ is designed for mutually trusted processes (sidecars, worker processes, plugin hosts) or sandboxed child processes where the parent restricts segment permissions (`perm = 0600`).
- **Bounds validation**: HQQ validates packet opcodes, slot indices, and chunk offsets on every read. `AdvancedOptions.MaxMessageSize` (default `64 MiB`) bounds one reassembled message, and `AdvancedOptions.MaxReassemblyBytes` (default `4 × MaxMessageSize`) bounds the memory held by all incomplete multi-chunk messages together; a message that would exceed it is refused without allocating (`ErrReassemblyLimit` for a refused response). Geometry is bounded (`bufferCount ≤ 1<<24`, `bufferSize ≤ 1<<30`, overflow-checked sizes). However, a malicious process with write access to the raw shared-memory pages can always overwrite ring sequence numbers or superblock state. Do not expose segments with world-writable permissions (`0666`) on multi-tenant systems.
- **Planted names (Linux)**: `/dev/shm` is typically world-writable, so another user can plant a name there. Named segments are opened with `O_NOFOLLOW|O_CLOEXEC` and then `fstat`ed; anything that is not a regular file (symbolic link, device, FIFO, directory) is rejected with `ErrInvalidName` and never opened, mapped, or removed. A regular file that is not a valid segment of this layout version is left untouched (`ErrIncompatibleLayout`). Creation uses `O_EXCL` and `fallocate`, so a full tmpfs fails at creation instead of raising `SIGBUS`. This does not protect against a hostile user who can write to the file you share; use `perm = 0600` and a private name.
- **Windows DACL**: `perm` is translated to a security descriptor applied to the mapping and to its four semaphores. SYSTEM always gets full access; the owner bits apply to the creating user's SID (read → read access, write → full access); the "other" bits apply to Everyone (read → read access to the mapping, write → read and write); group bits are ignored. The creator's own handles always have full access. With the default `0o600`, a `Global\` segment created by a service running as SYSTEM is not accessible to a desktop user; grant "other" access (for example `0o606`) for that case. The permission is ignored when opening an existing mapping.

### Platform Notes

| Platform | Named segment backing | Name limit | Allocation | Cross-process parking |
| --- | --- | --- | --- | --- |
| **Linux** | `/dev/shm/<name>` (`O_NOFOLLOW`, `O_CLOEXEC`, regular files only) | 255 bytes | `fallocate` (falls back to `ftruncate` only on `EOPNOTSUPP`/`ENOSYS`); a full tmpfs fails with `ENOSPC` at creation | `futex` (`FUTEX_WAIT` / `FUTEX_WAKE`, shared, no `FUTEX_PRIVATE_FLAG`) |
| **macOS** | POSIX `shm_open` (`O_CLOEXEC`) | **31 bytes** (`PSHMNAMLEN`, including the leading `/`) | `ftruncate` (does not reserve storage) | `__ulock_wait` / `__ulock_wake` (`UL_COMPARE_AND_WAIT_SHARED`), raw system calls on a private Darwin interface; if they stop working, waiters degrade to busy re-checking rather than failing |
| **FreeBSD** | POSIX `shm_open` | 1023 bytes | `ftruncate` | none: bounded exponential sleep backoff (`20µs` to `2ms`) |
| **Windows** | `CreateFileMappingW` / `OpenFileMappingW` (`Local\<name>` default, or explicit `Global\<name>`); the name exists only while some process holds a handle, so `Unlink` is a no-op | kernel object name + 9-character semaphore suffix ≤ 259 UTF-16 units | pagefile-backed section | Named segments: four named kernel semaphores per segment (`<object name>.hqq-sem0`…`3`, `CreateSemaphoreW` / `WaitForSingleObject` / `ReleaseSemaphore`). Unnamed memory (`AlignedBuffer`, `NewStandardLink`/`NewAdvancedLink` on caller-owned memory): `WaitOnAddress`, which wakes same-process waiters immediately; cross-process waiters fall back to a bounded backoff step of at most `2ms` |
| **Other OS** | named segments unsupported (`ENOSYS`); in-process links on caller-owned memory still work | — | — | bounded exponential sleep backoff |

**Waiter aggregation.** On every platform with parking, goroutines of one process that block on the same wait word (one ring side) form a group: the first becomes the leader and is the only one registered in the shared waiter count and parked in the kernel (futex, ulock, semaphore, or `WaitOnAddress`); the others wait on a process-local channel together with their own context, close, and timer, and re-check when the leader returns. N blocked goroutines therefore pin one OS thread, and the shared `emptyWaiters`/`fullWaiters` counts are numbers of parked **processes**, not goroutines. On Windows the waker calls `ReleaseSemaphore(n)` with `n` = that count. A process killed while parked leaves its count raised until the ring is re-formatted (the segment is re-initialized); the surviving endpoint then pays one extra wake call per publish, which is harmless because it must close the link after `ErrPeerGone` anyway.

On Windows, `ReleaseSemaphore` tokens persist, so a wake that runs between a waiter's re-check and its wait is not lost; tokens left for waiters that already gave up only cause one spurious wake-up.

> [!NOTE]
> In Docker/Kubernetes on Linux, `/dev/shm` defaults to `64 MiB`. For large `2 × bufferCount × bufferSize` configurations, mount an emptyDir with `medium: Memory` or pass `--shm-size` to the container runtime. Because creation uses `fallocate`, an undersized `/dev/shm` fails with `ENOSPC` when the segment is created.

### Build constraints

`arch_unsupported.go` makes the package fail to build (with an `undefined: hqq_requires_native_64bit_atomics` error naming that file) for `GOARCH=mips`, `GOARCH=mipsle`, and `GOARCH=arm` with `GOARM=5` or `6`: on those targets Go implements 64-bit atomics with locks private to the process, so two processes sharing a segment would not exclude each other. `GOARCH=arm GOARM=7`, `386`, and all 64-bit targets with native atomics build; `go vet` passes for `linux/386` and `linux/arm` with `GOARM=7`, but `386` and `arm` have not been run.

### Tuning Guide

- **`BufferSize`**: match your 95th-percentile message size so most messages travel as a single slot without chunking (for example `4096` for control/RPC messages, `65536` for media/tensor frames). Must be a multiple of 8. Both endpoints must use the same value.
- **`BufferCount`**: how many messages or chunks can be in flight per direction before backpressure stalls the writer. Power of two (`>= 2`). `128` to `1024` works well for most workloads. A segment occupies about `2 × bufferCount × bufferSize` bytes plus the rings; the Primary zeroes all payload buffers when it initializes a segment.
- **`WaitPolicy`**:
  - `WaitAdaptive` (default): re-checks the ring in a short tight loop (64 iterations), then yields with `runtime.Gosched` (256 iterations), then parks in the OS kernel for at most 100 ms per park. Best for almost all production deployments.
  - `WaitSpin`: pure user-space busy-spin with zero kernel transitions; every blocked call keeps one core busy. Use only when pinning dedicated cores for ultra-low tail latency.
- **`HeartbeatInterval` / `PeerTimeout`**: shorter values detect a crashed peer sooner (`PeerTimeout` is raised to at least four peer heartbeat intervals) at the cost of more superblock writes and a higher risk of false positives on a paused process.
- **`AttachTimeout`**: how long an endpoint waits for another endpoint that is initializing the segment, and how long an opener (`Open*`, `Dial`) waits for a creator to claim a segment that is still empty; after that the call fails with a transient error matching `ErrFailedInit` (`Dial` retries). It also bounds each ring attach.
- **`AdvancedOptions`**: `MaxMessageSize` and `MaxReassemblyBytes` bound memory held for incoming large messages (a bound below `MaxMessageSize` refuses large messages even when nothing else is in flight); `IncomingQueueSize` and `IncomingOverflow` select how much unsolicited traffic is buffered and what happens beyond it.

---

## Testing & Verification

```bash
# Static analysis (passes default go vet including unsafeptr)
go vet ./...
GOOS=windows go vet ./...
GOOS=linux go vet ./...

# Unit, lifecycle, and multi-process tests with race detector
go test -race ./...

# CPU-ratio performance tests (skipped by default; run on a quiet machine)
HQQ_PERF_TESTS=1 go test ./... -count=1 -run 'TestIdleLinkDoesNotBurnCPU|TestMPMCIdleWaitDoesNotBurnCPU|TestMPMCWaitSpinBurnsCPU'

# Fuzz testing
go test ./internal/protocol -run '^$' -fuzz=FuzzPacketRoundTrip -fuzztime=10s
go test . -run '^$' -fuzz='^FuzzConsumeAdvancedChunk$' -fuzztime=10s
go test . -run '^$' -fuzz='^FuzzOpenOverCorruptSuperblock$' -fuzztime=10s

# TLA+ model checking of the MPMC ring (requires a Java runtime)
sh scripts/tlc.sh
JAVA=/path/to/bin/java sh scripts/tlc.sh   # if `java` on PATH is not a working runtime
```

- **`HQQ_PERF_TESTS=1`** enables the tests that compare CPU time or latency ratios (`TestIdleLinkDoesNotBurnCPU`, `TestMPMCIdleWaitDoesNotBurnCPU`, `TestMPMCWaitSpinBurnsCPU`). They are skipped otherwise because the ratios are only meaningful on a quiet machine; the benchmark workflow runs them.
- **Fuzz targets**: `FuzzPacketRoundTrip` (`internal/protocol`), `FuzzConsumeAdvancedChunk` and `FuzzOpenOverCorruptSuperblock` (root package).
- **TLC**: `scripts/tlc.sh` downloads `tla2tools.jar` if needed and checks `internal/mpmc/MPMC.tla` with `MPMC.cfg` (`BufferSize = 2`). It picks the Java runtime from `$JAVA`, then `$JAVA_HOME/bin/java`, then `java` on `PATH`; CI uses Temurin 21. The model covers the claim and completion protocol of the ring; it does not model parking, waiter counts or aggregation, `Format`, crashes, or stale copies of reservation handles.
