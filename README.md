# HQQ: Efficient Shared-Memory IPC

HQQ is a Go IPC library built around shared memory and lock-free MPMC rings. The current implementation focuses on making the **StandardLink / Standard Protocol** data path correct, bounded, and fast, with an **Advanced Protocol** overlay for large-data copy and request-response IPC.

## Current Focus

Implemented:

- fixed-size 64-byte protocol packets
- little-endian packet encoding
- bidirectional primary/secondary data transfer
- lock-free data rings with ring-slot-owned payload buffers
- page-aligned superblock for deterministic shared-memory initialization
- zero-copy callback APIs for direct shared-buffer access
- Reserve/Commit APIs for explicit slot lifetime control
- implicit payload-buffer ownership transfer to prevent unread-buffer overwrite
- `io.Reader`, `io.Writer`, and `net.PacketConn` style APIs
- read/write deadline timeout behavior
- StandardLink functional and benchmark coverage
- `AdvancedLink` protocol negotiation
- background Advanced dispatcher with pending-call response routing
- Advanced Protocol large-data copy over chunked `OpConnCopy` frames
- Advanced Protocol request-response IPC
- logical Advanced stream `Dial`/`Listen`/`Accept`
- bounded Advanced message assembly through `AdvancedOptions.MaxMessageSize`
- AdvancedLink functional and benchmark coverage

Planned or experimental:

- streaming Advanced receive callbacks that avoid full message assembly
- production hardening of unsafe shared-memory internals

## Standard Protocol Overview

Shared memory is split into two independent directions:

```text
SUPERBLOCK    protocol/configuration/init-state metadata
DATA_RING_0   protocol.Packet ring, Primary -> Secondary
BUFFERS_0     fixed payload buffers for direction 0
DATA_RING_1   protocol.Packet ring, Secondary -> Primary
BUFFERS_1     fixed payload buffers for direction 1
```

Each data-ring slot owns the same-index payload buffer. Senders claim a ring slot, fill `BUFFERS[slot]`, then publish an `OpStandardLinkCopy` packet. Receivers claim the packet slot and either copy the payload out (`Read`) or process it in-place (`ReadZeroCopy`); returning from the dequeue callback releases the slot and its buffer for reuse.

See [PROTOCOL.md](PROTOCOL.md) for the full Standard Protocol specification.

## Requirements

- Go 1.24 or newer
- `bufferCount` must be a power of two and at least 2
- `bufferSize` must be at least 8 bytes and a multiple of 8
- `offset` passed to `OpenStandardLink` must be page-aligned

## Minimal In-Process Example

The example below uses a page-aligned byte slice so it can run in one process. Real IPC users should map a shared memory segment with their platform's mmap/shared-memory mechanism and pass the mapped base address to `OpenStandardLink`.

```go
package main

import (
    "fmt"
    "syscall"
    "unsafe"

    "gosuda.org/hqq"
)

func alignedBuffer(size uintptr) ([]byte, uintptr) {
    pageSize := uintptr(syscall.Getpagesize())
    backing := make([]byte, int(size)+int(pageSize))
    offset := uintptr(unsafe.Pointer(&backing[0]))
    if offset%pageSize != 0 {
        offset = ((offset / pageSize) + 1) * pageSize
    }
    return backing, offset
}

func main() {
    const bufferCount = 1024
    const bufferSize = 4096

    size := hqq.SizeStandardLink(bufferCount, bufferSize)
    backing, offset := alignedBuffer(size)
    _ = backing // keep the backing allocation alive while links are open

    primary, err := hqq.OpenStandardLink(offset, bufferCount, bufferSize)
    if err != nil {
        panic(err)
    }
    defer primary.Close()

    secondary, err := hqq.OpenStandardLink(offset, bufferCount, bufferSize)
    if err != nil {
        panic(err)
    }
    defer secondary.Close()

    if _, err := primary.Write([]byte("hello from primary")); err != nil {
        panic(err)
    }

    buf := make([]byte, bufferSize)
    n, err := secondary.Read(buf)
    if err != nil {
        panic(err)
    }
    fmt.Println(string(buf[:n]))
}
```

## API Reference

### Sizing and opening

```go
func SizeStandardLink(bufferCount int, bufferSize int) uintptr
func LargeMessageThreshold(bufferSize int) int
func IsLargeMessage(bufferSize int, payloadSize int) bool
func OpenStandardLink(offset uintptr, bufferCount int, bufferSize int) (*StandardLink, error)
func CreateStandardLink(name string, bufferCount int, bufferSize int, mode os.FileMode) (*NamedStandardLink, error)
func OpenNamedStandardLink(name string, bufferCount int, bufferSize int) (*NamedStandardLink, error)
```

`SizeStandardLink` returns zero for invalid configurations and includes the first-page superblock plus all page-aligned rings and payload regions. `LargeMessageThreshold(bufferSize)` returns the largest payload that fits in one Standard Protocol packet; payloads larger than that should use Advanced large-copy/request-response APIs.

`OpenStandardLink` is the low-level API for callers that already own a page-aligned mapping. `CreateStandardLink` and `OpenNamedStandardLink` create/map named shared memory and enforce page-aligned mappings for typical IPC use.

### StandardLink methods

```go
func (l *StandardLink) Read(b []byte) (int, error)
func (l *StandardLink) Write(b []byte) (int, error)
func (l *StandardLink) ReadZeroCopy(func([]byte) error) (int, error)
func (l *StandardLink) WriteZeroCopy(func([]byte) (int, error)) (int, error)
func (l *StandardLink) ReserveRead() (StandardReadReservation, error)
func (l *StandardLink) ReserveWrite() (StandardWriteReservation, error)
func (l *StandardLink) Close() error
func (l *StandardLink) BufferSize() int
func (l *StandardLink) BufferCount() int
func (l *StandardLink) LargeMessageThreshold() int
func (l *StandardLink) IsLargeMessage(payloadSize int) bool
func (l *StandardLink) GetMode() LinkMode
func (l *StandardLink) GetType() LinkType
```

`ReadZeroCopy` and `WriteZeroCopy` expose the shared payload buffer directly for the duration of the callback.

> WARNING: The callback runs while the underlying ring slot is claimed. It must return promptly; if it blocks or never returns, the whole ring can suffer head-of-line (HOL) blocking. Do not retain the buffer after the callback returns.

All dispatch methods interoperate at message boundaries. `Write`, `WriteZeroCopy`, and `ReserveWrite` all publish the same StandardLink copy packet stream; `Read`, `ReadZeroCopy`, and `ReserveRead` all consume that stream and skip tombstones. The simple `Read`/`Write` methods keep a direct automatic-release/automatic-commit hot path instead of layering through public reservation handles.

`ReserveRead` and `ReserveWrite` expose lower-level explicit lifetime control:

```go
write, err := primary.ReserveWrite()
if err != nil {
    panic(err)
}
n := copy(write.Buffer(), []byte("hello"))
if err := write.Commit(n); err != nil {
    panic(err)
}

read, err := secondary.ReserveRead()
if err != nil {
    panic(err)
}
fmt.Println(string(read.Buffer()))
_ = read.Release()
```

> WARNING: Reserved slots must be finished. Writers must call `Commit` or `Abort`; readers must call `Release`. Dropping a reservation can cause head-of-line (HOL) blocking for the whole direction. `Abort` publishes a tombstone packet so readers can skip the abandoned slot and preserve ring progress.


### AdvancedLink methods

```go
func NewAdvancedLink(offset uintptr, bufferCount int, bufferSize int) (*AdvancedLink, error)
func NewAdvancedLinkWithOptions(offset uintptr, bufferCount int, bufferSize int, opts AdvancedOptions) (*AdvancedLink, error)
func CreateAdvancedLink(name string, bufferCount int, bufferSize int, mode os.FileMode) (*NamedAdvancedLink, error)
func OpenNamedAdvancedLink(name string, bufferCount int, bufferSize int) (*NamedAdvancedLink, error)
func (l *AdvancedLink) NegotiateProtocol(ctx context.Context, version ProtocolVersion, features uint64) (bool, error)
func (l *AdvancedLink) WaitForNegotiation(ctx context.Context) (bool, error)
func (l *AdvancedLink) SendLarge(ctx context.Context, connectionID uint64, payload []byte) (messageID uint64, err error)
func (l *AdvancedLink) Receive(ctx context.Context) (AdvancedMessage, error)
func (l *AdvancedLink) Call(ctx context.Context, methodID uint64, request []byte) ([]byte, error)
func (l *AdvancedLink) CallOn(ctx context.Context, connectionID uint64, methodID uint64, request []byte) (AdvancedMessage, error)
func (l *AdvancedLink) Respond(ctx context.Context, request AdvancedMessage, status uint64, response []byte) error
func (l *AdvancedLink) RespondError(ctx context.Context, request AdvancedMessage, status uint64, response []byte) error
func (l *AdvancedLink) Listen(ctx context.Context) error
func (l *AdvancedLink) Accept(ctx context.Context) (uint64, error)
func (l *AdvancedLink) Dial(ctx context.Context) (uint64, error)
func (l *AdvancedLink) BufferSize() int
func (l *AdvancedLink) BufferCount() int
func (l *AdvancedLink) LargeMessageThreshold() int
func (l *AdvancedLink) IsLargeMessage(payloadSize int) bool
```

Advanced feature flags are intentionally narrow: `FeatureLargeCopy` and `FeatureRequestResponse`. Advanced APIs require successful negotiation first. A payload becomes a large message when `payloadSize > link.LargeMessageThreshold()`; the threshold is the configured payload `bufferSize`. `SendLarge` splits payloads larger than `bufferSize` into chunked `OpConnCopy` frames. `Call`/`CallOn` use the same chunking for request and response bodies and correlate responses by `MessageID`.

After negotiation, `AdvancedLink` uses a single dispatcher path for Advanced traffic. Direct `Receive` can drive that path synchronously for low latency, and a background dispatcher is started when asynchronous pending-call or stream-control routing is needed. The dispatcher assembles chunks, routes responses to a pending-call table keyed by `(ConnectionID, MessageID)`, and delivers data/request messages to `Receive`. This supports multiple concurrent outstanding calls and out-of-order responses without frame stealing. `Receive` assembles complete Advanced messages before returning; future streaming APIs may avoid full assembly for very large payloads. Do not read Advanced traffic through the Standard `Read` APIs.

`AdvancedOptions.MaxMessageSize` bounds full-message assembly and defaults to `DefaultMaxAdvancedMessageSize`. Oversized sends or receives fail with `ErrBufferOverflow`.

Minimal request-response flow:

```go
// Server side
req, err := server.Receive(ctx)
if err != nil {
    panic(err)
}
if req.IsRequest() && req.MethodID == 7 {
    _ = server.Respond(ctx, req, 0, []byte("ok"))
}

// Client side
resp, err := client.Call(ctx, 7, []byte("ping"))
if err != nil {
    panic(err)
}
fmt.Println(string(resp))
```

`StandardLink` also implements `net.PacketConn`:

```go
func (l *StandardLink) ReadFrom(p []byte) (int, net.Addr, error)
func (l *StandardLink) WriteTo(p []byte, addr net.Addr) (int, error)
func (l *StandardLink) LocalAddr() net.Addr
func (l *StandardLink) SetDeadline(t time.Time) error
func (l *StandardLink) SetReadDeadline(t time.Time) error
func (l *StandardLink) SetWriteDeadline(t time.Time) error
```

## Testing and Benchmarks

```bash
go test ./...
go test . -run 'TestMultiProcess|TestLargeMessageThresholdAPI' -v
go test -bench='(StandardLink|AdvancedLink)' -run '^$' -benchtime=1s
```

Current known caveat: the low-level shared-memory ring uses unsafe pointer arithmetic. Standard functional tests pass, but full `go vet`/checkptr hardening of the MPMC internals is still a separate hardening task.
