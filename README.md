# HQQ: High-Throughput Shared-Memory IPC for Go

**HQQ** (`gosuda.org/hqq`) is a Go inter-process communication (IPC) library built on shared memory, lock-free Vyukov-style MPMC rings, and slot-owned payload buffers.

It delivers **sub-microsecond round-trip latency (~258 ns)** and **multi-GB/s throughput (27–64 GB/s)** while keeping idle CPU near **0%** via cross-process kernel parking (`futex` on Linux, `__ulock_wait` on macOS).

---

## Highlights

- **Two composable protocol layers over one shared-memory layout**:
  - **Standard Protocol (`StandardLink`)**: fixed-capacity messages (`Read`, `Write`, `ReadZeroCopy`, `WriteZeroCopy`, `ReserveRead`, `ReserveWrite`, and `net.PacketConn` deadlines) with zero heap allocations on the hot path.
  - **Advanced Protocol (`AdvancedLink`)**: negotiated overlay adding multi-chunk large-message transfer (`SendLarge`, `Receive`), concurrent out-of-order RPC (`Call`, `CallOn`, `Respond`, `Serve`, `Mux`), and logical stream multiplexing (`Listen`, `Accept`, `Dial`).
- **Adaptive Cross-Process Parking (`WaitAdaptive`)**: spins briefly with CPU pause/yield hints and then parks in the OS kernel (`FUTEX_WAIT` on Linux, `UL_COMPARE_AND_WAIT_SHARED` on macOS), reducing idle blocked reader CPU from ~165% to **~0.1%** without sacrificing active throughput.
- **Deterministic Crash & Lifecycle Detection (`HQQ_STL2` Superblock)**: tracks endpoint session tokens, OS PIDs, and heartbeats to distinguish graceful peer shutdown (`ErrPeerClosed`) from abrupt process termination (`ErrPeerGone`), automatically re-initialize stale segments from dead processes, and reject 3rd-party attachment collisions (`ErrLinkBusy`).
- **Formal Verification & Safety**: MPMC ring invariants (`NoRace`, `DataIntegrity`, `FIFOClaimOrder`, `SystemProgress`) are model-checked with TLC ([`internal/mpmc/MPMC.tla`](internal/mpmc/MPMC.tla)), and the entire codebase passes default `go vet ./...` (including `unsafeptr`) and `go test -race ./...`.

---

## Requirements

- Go 1.24 or newer
- `bufferCount`: power of two, `>= 2` (defaults to `128` in `hqq.Config`)
- `bufferSize`: multiple of 8 bytes, `>= 8` (defaults to `4096` in `hqq.Config`)

---

## Quick Start

### 1. High-Level Named RPC & Large-Message IPC (`Listen` / `Dial` + `Mux`)

For most inter-process RPC and large-payload workloads, use `hqq.Listen` on the primary process and `hqq.Dial` on the secondary process. Both functions create/attach the shared-memory segment and complete Advanced Protocol negotiation automatically:

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
	_ = hqq.Unlink(name)
	defer hqq.Unlink(name)

	mux := hqq.NewMux()
	mux.HandleFunc(MethodEcho, func(ctx context.Context, req hqq.AdvancedMessage) ([]byte, error) {
		return append([]byte("echo: "), req.Payload...), nil
	})

	// Primary endpoint (creates shared memory and serves requests)
	serverErr := make(chan error, 1)
	go func() {
		server, err := hqq.Listen(ctx, name, hqq.Config{})
		if err != nil {
			serverErr <- err
			return
		}
		defer server.Close()
		serverErr <- server.Serve(ctx, mux)
	}()

	// Secondary endpoint (attaches and makes RPC calls or large transfers)
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
}
```

### 2. Named `StandardLink` (`CreateStandardLink` / `OpenNamedStandardLink`)

When your messages fit within `bufferSize` and you want raw `io.ReadWriter` / zero-copy ring performance without protocol negotiation:

```go
primary, err := hqq.CreateStandardLink("my_std_link", 256, 4096, 0600)
if err != nil {
	panic(err)
}
defer primary.Delete()
defer primary.Close()

secondary, err := hqq.OpenNamedStandardLink("my_std_link", 256, 4096)
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

### 3. In-Process or Custom Shared Memory (`OpenStandardLinkMemory`)

No `unsafe.Pointer` arithmetic is required in user code. Pass a page-aligned `[]byte` from `hqq.AlignedBuffer` (or your own `mmap` slice) to `hqq.OpenStandardLinkMemory`:

```go
size := hqq.SizeStandardLink(128, 4096)
backing, mem := hqq.AlignedBuffer(size)
_ = backing // keep backing alive while links are open

primary, err := hqq.OpenStandardLinkMemory(mem, 128, 4096)
if err != nil {
	panic(err)
}
defer primary.Close()

secondary, err := hqq.OpenStandardLinkMemory(mem, 128, 4096)
if err != nil {
	panic(err)
}
defer secondary.Close()
```

---

## Architecture Overview

Shared memory is divided into a first-page superblock (`HQQ_STL2`) and two independent directional channels:

```text
SUPERBLOCK    protocol version, layout parameters, epoch, and endpoint PID/heartbeats
DATA_RING_0   protocol.Packet MPMC ring, Primary -> Secondary
BUFFERS_0     fixed payload buffers for direction 0 (slot i owns BUFFERS_0[i])
DATA_RING_1   protocol.Packet MPMC ring, Secondary -> Primary
BUFFERS_1     fixed payload buffers for direction 1 (slot i owns BUFFERS_1[i])
```

Each data-ring slot `i` owns the same-index payload buffer `BUFFERS[i]`. Senders claim a producer slot, fill `BUFFERS[slot]`, and publish a 64-byte `protocol.Packet`. Receivers claim the consumer slot, either copy the payload out (`Read`) or inspect it in place (`ReadZeroCopy` / `ReserveRead`), and release the slot when done. Because `BUFFERS[slot]` cannot be claimed by another producer until the consumer releases slot `i`, unread payloads can never be overwritten.

See [PROTOCOL.md](PROTOCOL.md) for the full wire and memory layout specification.

---

## Benchmarks

Measured on Apple M1 (`darwin/arm64`, Go 1.24, `go test . -bench 'Benchmark(StandardLink|AdvancedLink|Compare)' -benchmem`):

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

### Sizing & Opening

```go
// High-level Dial / Listen (creates/opens named shm + negotiates AdvancedLink)
func Listen(ctx context.Context, name string, cfg Config) (*NamedAdvancedLink, error)
func Dial(ctx context.Context, name string, cfg Config) (*NamedAdvancedLink, error)
func Unlink(name string) error

// Layout sizing & helpers
func SizeStandardLink(bufferCount int, bufferSize int) uintptr
func AlignedBuffer(size uintptr) (backing []byte, aligned []byte)
func LargeMessageThreshold(bufferSize int) int
func IsLargeMessage(bufferSize int, payloadSize int) bool

// StandardLink constructors
func OpenStandardLinkMemory(mem []byte, bufferCount int, bufferSize int) (*StandardLink, error)
func OpenStandardLinkMemoryWithOptions(mem []byte, bufferCount int, bufferSize int, opts LinkOptions) (*StandardLink, error)
func OpenStandardLink(offset uintptr, bufferCount int, bufferSize int) (*StandardLink, error)
func OpenStandardLinkWithOptions(offset uintptr, bufferCount int, bufferSize int, opts LinkOptions) (*StandardLink, error)
func CreateStandardLink(name string, bufferCount int, bufferSize int, mode os.FileMode) (*NamedStandardLink, error)
func OpenNamedStandardLink(name string, bufferCount int, bufferSize int) (*NamedStandardLink, error)

// AdvancedLink constructors
func NewAdvancedLink(offset uintptr, bufferCount int, bufferSize int) (*AdvancedLink, error)
func NewAdvancedLinkWithOptions(offset uintptr, bufferCount int, bufferSize int, opts AdvancedOptions) (*AdvancedLink, error)
func CreateAdvancedLink(name string, bufferCount int, bufferSize int, mode os.FileMode) (*NamedAdvancedLink, error)
func OpenNamedAdvancedLink(name string, bufferCount int, bufferSize int) (*NamedAdvancedLink, error)
```

### `StandardLink` Methods

```go
func (l *StandardLink) Read(b []byte) (int, error)
func (l *StandardLink) Write(b []byte) (int, error)
func (l *StandardLink) ReadZeroCopy(fn func([]byte) error) (int, error)
func (l *StandardLink) WriteZeroCopy(fn func([]byte) (int, error)) (int, error)
func (l *StandardLink) ReserveRead() (StandardReadReservation, error)
func (l *StandardLink) ReserveWrite() (StandardWriteReservation, error)
func (l *StandardLink) PeerState() PeerState
func (l *StandardLink) Mode() LinkMode
func (l *StandardLink) Type() LinkType
func (l *StandardLink) Close() error
```

> [!WARNING]
> `ReadZeroCopy`, `WriteZeroCopy`, `ReserveRead`, and `ReserveWrite` hold the underlying ring slot while active. Callbacks must return promptly, and reservations must always be completed (`Commit`/`Abort` for writes, `Release` for reads) to avoid head-of-line (HOL) blocking. Never retain the shared `[]byte` slice after the callback returns or after calling `Release`.

### `AdvancedLink` & RPC Methods

```go
// Role-aware one-call negotiation (Primary initiates, Secondary waits)
func (l *AdvancedLink) Negotiate(ctx context.Context, features uint64) (bool, error)
func (l *AdvancedLink) NegotiateProtocol(ctx context.Context, version ProtocolVersion, features uint64) (bool, error)
func (l *AdvancedLink) WaitForNegotiation(ctx context.Context) (bool, error)

// Large-data transfer & message reception
func (l *AdvancedLink) SendLarge(ctx context.Context, connectionID uint64, payload []byte) (messageID uint64, err error)
func (l *AdvancedLink) Receive(ctx context.Context) (AdvancedMessage, error)

// Request-Response RPC
func (l *AdvancedLink) Call(ctx context.Context, methodID uint64, request []byte) ([]byte, error)
func (l *AdvancedLink) CallOn(ctx context.Context, connectionID uint64, methodID uint64, request []byte) (AdvancedMessage, error)
func (l *AdvancedLink) Respond(ctx context.Context, request AdvancedMessage, status uint64, response []byte) error
func (l *AdvancedLink) RespondError(ctx context.Context, request AdvancedMessage, status uint64, response []byte) error
func (l *AdvancedLink) Serve(ctx context.Context, handler Handler) error

// Logical streams
func (l *AdvancedLink) Listen(ctx context.Context) error
func (l *AdvancedLink) Accept(ctx context.Context) (uint64, error)
func (l *AdvancedLink) Dial(ctx context.Context) (uint64, error)
```

---

## Operational Guide

### Concurrency Contract

- **Goroutine-safe by default**: `Read`, `Write`, `ReadZeroCopy`, `WriteZeroCopy`, `ReserveRead`, `ReserveWrite`, `Call`, `CallOn`, `SendLarge`, `Respond`, and `Receive` are safe for concurrent use by multiple goroutines.
- **Protocol exclusivity**: Once `AdvancedLink` negotiation succeeds, the Advanced dispatcher owns the underlying Standard ring. Calling `StandardLink.Read` or `StandardLink.Write` directly on a negotiated `AdvancedLink` returns `ErrAdvancedActive`.
- **Safe `Close()`**: Closing a `StandardLink`, `AdvancedLink`, `NamedStandardLink`, or `NamedAdvancedLink` wakes all parked readers/writers and waits for in-flight ring callbacks to finish before returning or unmapping shared memory.

### Crash Recovery & Liveness Semantics

- **Graceful Shutdown (`ErrPeerClosed`)**: When a peer calls `Close()`, it writes a detached marker into the superblock and wakes parked waiters. Blocked `Read`, `Write`, `Receive`, or `Call` operations return `ErrPeerClosed` (which satisfies `errors.Is(err, io.EOF)` and `errors.Is(err, net.ErrClosed)`).
- **Abrupt Process Crash (`ErrPeerGone`)**: Each endpoint updates its superblock heartbeat (default `100ms`) and records its OS PID. If the peer process is killed (`SIGKILL`) or crashes, the surviving endpoint detects the dead PID or expired heartbeat (default `500ms`) and unblocks all waiters with `ErrPeerGone` (which also wraps `io.EOF` and `net.ErrClosed`).
- **Stale Segment Re-initialization**: If both previous endpoints have exited without unlinking the shared-memory file, calling `CreateStandardLink`, `CreateAdvancedLink`, or `Listen` on the same name automatically increments the superblock epoch and re-formats the segment. If a live process still holds the segment, creation fails with `ErrLinkBusy`.
- **Standalone Cleanup**: Call `hqq.Unlink(name)` to remove a leftover shared-memory segment without mapping it.

### Security Model (Trusted Peer Assumption)

Shared-memory IPC requires both endpoints to map the same physical memory pages as read-write (`PROT_READ | PROT_WRITE`). Consequently:
- **Trusted Peers**: HQQ is designed for mutually trusted processes (e.g., sidecars, worker processes, plugin hosts) or sandboxed child processes where the parent restricts segment permissions (`mode = 0600`).
- **Bounds Validation**: HQQ validates packet opcodes, slot indices, and chunk offsets on every read and bounds multi-chunk allocations via `AdvancedOptions.MaxMessageSize` (default `16 MiB`). However, a malicious process with write access to the raw shared-memory pages can always overwrite ring sequence numbers or superblock state. Do not expose shared-memory segments with world-writable permissions (`0666`) on multi-tenant systems.

### Platform Notes

| Platform | Shared Memory Backing | Name Limit | Cross-Process Parking |
| --- | --- | --- | --- |
| **Linux** | `/dev/shm/<name>` (`O_CLOEXEC`) | 255 bytes | `futex` (`FUTEX_WAIT` / `FUTEX_WAKE`) |
| **macOS** | POSIX `shm_open` (`O_CLOEXEC`) | **31 bytes** (`PSHMNAMLEN`, including `/`) | `__ulock_wait` / `__ulock_wake` (`UL_COMPARE_AND_WAIT_SHARED`) |
| **Windows** | `CreateFileMappingW` / `OpenFileMappingW` | 255 bytes (`Local\<name>` default, or explicit `Global\<name>`) | Bounded exponential sleep backoff (`20µs–2ms`) |

> [!NOTE]
> In Docker/Kubernetes on Linux, `/dev/shm` defaults to `64 MiB`. For large `bufferCount * bufferSize` configurations, mount an emptyDir with `medium: Memory` or pass `--shm-size` to the container runtime.

### Tuning Guide

- **`BufferSize`**: Match your 95th-percentile message size so most messages travel as a single slot without chunking (e.g., `4096` for control/RPC messages, `65536` for media/tensor frames). Must be a multiple of 8.
- **`BufferCount`**: Controls how many messages or chunks can be in flight per direction before backpressure stalls the writer. Must be a power of two (`>= 2`). `128` to `1024` works well for most workloads.
- **`WaitPolicy`**:
  - `WaitAdaptive` (default): spins for a few hundred iterations with CPU pause hints and then parks in the OS kernel. Best for almost all production deployments.
  - `WaitSpin`: pure user-space busy-spin with zero kernel transitions. Use only when pinning dedicated cores for ultra-low tail latency.

---

## Testing & Verification

```bash
# Static analysis (passes default go vet including unsafeptr)
go vet ./...

# Unit, lifecycle, and multi-process tests with race detector
go test -race ./...

# Fuzz testing
go test ./internal/protocol -run '^$' -fuzz=FuzzPacketRoundTrip -fuzztime=10s
go test . -run '^$' -fuzz=FuzzStandardLinkReadCorruptedPacket -fuzztime=10s
go test . -run '^$' -fuzz=FuzzAdvancedDispatcherCorruptedFrames -fuzztime=10s

# TLA+ model checking of the MPMC ring (requires Java 11+)
sh scripts/tlc.sh
```
