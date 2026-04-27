# HQQ: Efficient Shared-Memory IPC

HQQ is a Go IPC library built around shared memory and lock-free MPMC rings. The current implementation focuses on making the **StandardLink / Standard Protocol** data path correct, bounded, and fast.

## Current Focus

Implemented for `StandardLink`:

- fixed-size 64-byte protocol packets
- little-endian packet encoding
- bidirectional primary/secondary data transfer
- lock-free data rings with ring-slot-owned payload buffers
- zero-copy callback APIs for direct shared-buffer access
- Reserve/Commit APIs for explicit slot lifetime control
- implicit payload-buffer ownership transfer to prevent unread-buffer overwrite
- `io.Reader`, `io.Writer`, and `net.PacketConn` style APIs
- read/write deadline timeout behavior
- StandardLink functional and benchmark coverage

Planned or experimental:

- `AdvancedLink` protocol negotiation
- logical connection lifecycle
- compression, encryption, flow control, QoS, and reliability extensions
- production hardening of unsafe shared-memory internals

## Standard Protocol Overview

Shared memory is split into two independent directions:

```text
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
func OpenStandardLink(offset uintptr, bufferCount int, bufferSize int) (*StandardLink, error)
```

`SizeStandardLink` returns zero for invalid configurations.

### StandardLink methods

```go
func (l *StandardLink) Read(b []byte) (int, error)
func (l *StandardLink) Write(b []byte) (int, error)
func (l *StandardLink) ReadZeroCopy(func([]byte) error) (int, error)
func (l *StandardLink) WriteZeroCopy(func([]byte) (int, error)) (int, error)
func (l *StandardLink) ReserveRead() (StandardReadReservation, error)
func (l *StandardLink) ReserveWrite() (StandardWriteReservation, error)
func (l *StandardLink) Close() error
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
go test -bench=StandardLink -run '^$' -benchtime=1s
```

Current known caveat: the low-level shared-memory ring uses unsafe pointer arithmetic. Standard functional tests pass, but full `go vet`/checkptr hardening of the MPMC internals is still a separate hardening task.
