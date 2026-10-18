# HQQ (`gosuda.org/hqq`)

**HQQ** (`gosuda.org/hqq`) is a **zero-`cgo` (`CGO_ENABLED=0`) shared-memory IPC and C-binding library for Go**, accompanied by C11 (`libhqq`), C++17, and Rust implementations. Go processes communicate with local Go or C/C++/Rust worker processes directly over memory-mapped lock-free MPMC packet rings with kernel parking (`futex` on Linux, `__ulock_wait` on macOS, named semaphores on Windows) and superblock heartbeat liveness detection, requiring zero `cgo` in the Go binary.

```bash
go get gosuda.org/hqq
```

## Supported Platforms

| OS | Architectures | Shared Memory Backing | Kernel Parking Primitive |
| :--- | :--- | :--- | :--- |
| **Linux** | `amd64`, `arm64` | `/dev/shm` (`mmap`) | `futex(2)` (`FUTEX_WAIT_BITSET`) |
| **macOS** | `arm64` (Apple Silicon), `amd64` | `$TMPDIR` (`mmap`) | `__ulock_wait` / `__ulock_wake` (`UL_COMPARE_AND_WAIT_SHARED`) |
| **Windows** | `amd64`, `arm64` | `%LOCALAPPDATA%\Temp` (`CreateFileMappingW` + `MapViewOfFile`) | Named Win32 Semaphores (`Local\hqq_sem_*`) |

---

## Quick Start

### 1. Named RPC and Messages (`Listen` / `Dial` + `Serve`)

Call `hqq.Listen` on the primary process and `hqq.Dial` on the secondary process. The server answers requests with `Serve` and a `Handler` (modeled after `net/http`); the client dispatches concurrent requests with `Call`:

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
	mux.HandleFunc(MethodEcho, func(ctx context.Context, w *hqq.ResponseWriter, req *hqq.Request) {
		w.Write([]byte("echo: "))
		w.Write(req.Payload())
	})

	serverErr := make(chan error, 1)
	go func() {
		server, err := hqq.Listen(ctx, name, hqq.Config{})
		if err != nil {
			serverErr <- err
			return
		}
		err = server.Serve(ctx, mux)
		_ = server.Close()
		serverErr <- err
	}()

	client, err := hqq.Dial(ctx, name, hqq.Config{})
	if err != nil {
		panic(err)
	}
	defer client.Close()

	m, err := client.Call(ctx, MethodEcho, []byte("hello HQQ"))
	if err != nil {
		panic(err)
	}
	fmt.Println(string(m.Bytes())) // Output: echo: hello HQQ
	m.Release()

	client.Close()
	if err := <-serverErr; err != nil {
		panic(err)
	}
}
```

### 2. Multiplexed Logical Streams (`ListenStreams` / `AcceptStream` / `OpenStream`)

```go
// Primary: accept streams opened by the peer.
if err := server.ListenStreams(ctx); err != nil {
	panic(err)
}
go func() {
	s, err := server.AcceptStream(ctx)
	if err != nil {
		return
	}
	_ = s.Send(ctx, []byte("hello on stream"))
}()

// Secondary: open a stream, receive, and close.
s, err := client.OpenStream(ctx)
if err != nil {
	panic(err)
}
m, err := s.Recv(ctx)
if err != nil {
	panic(err)
}
fmt.Println(m.StreamID() == s.ID(), string(m.Bytes()))
m.Release()
_ = s.Close()
```

### 3. Zero-Copy `StandardLink` (`CreateStandardLink` / `OpenStandardLink`)

```go
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

_, _ = secondary.ReadZeroCopy(func(payload []byte) error {
	fmt.Println(string(payload))
	return nil
})
```

### 4. Multi-Client Acceptor (`NewAcceptor` / `Accept` / `Connect`)

```go
// Server process
acc, err := hqq.NewAcceptor("my-service", hqq.AcceptorConfig{})
if err != nil {
	log.Fatal(err)
}
defer acc.Close()

for {
	link, err := acc.Accept(ctx)
	if err != nil {
		break
	}
	go func() {
		defer link.Close()
		_ = link.Serve(ctx, mux)
	}()
}

// Client process
link, err := hqq.Connect(ctx, "my-service", hqq.Config{})
if err != nil {
	panic(err)
}
defer link.Close()
```

### 5. Calling C Libraries Without `cgo` (`hqq-bindgen` + `launch`)

Generate a pure-Go client and an isolated C11 shared-memory server from a C header (`.h`) or `.idl` file, then launch the verified server binary via `gosuda.org/hqq/launch`:

```sh
# 1. Bootstrap .idl from a C header
go run gosuda.org/hqq/cmd/hqq-bindgen init -prefix MYLIB_ -idl-out mylib.idl mylib.h

# 2. Generate pure-Go client and C11 server dispatcher
go run gosuda.org/hqq/cmd/hqq-bindgen -go-out client -go-strip MYLIB_ -c-out server mylib.idl
```

```go
srv, err := launch.Start(ctx, launch.Config{
	Binary: "/opt/app/bin/mylib_server",
	SHA256: expectedHash,
})
if err != nil {
	return err
}
defer srv.Close()
```

Official zero-`cgo` C library modules built on `hqq-bindgen`:
- [**`gosuda.org/hqq-sqlite3`**](https://github.com/gosuda/hqq-sqlite3): Pure-Go `database/sql` SQLite driver (`"sqlite3"`) and `hqq-sqlite-server`.
- [**`gosuda.org/hqq-zstd`**](https://github.com/gosuda/hqq-zstd): Pure-Go Zstandard (`libzstd`) client and `zstd_server`.

---

## Public API Summary

### Sizing & Opening (`package hqq`)

```go
func Listen(ctx context.Context, name string, cfg Config) (*NamedAdvancedLink, error)
func Dial(ctx context.Context, name string, cfg Config) (*NamedAdvancedLink, error)
func Unlink(name string) error

func SizeStandardLink(bufferCount int, bufferSize int) uintptr
func AlignedBuffer(size uintptr) []byte
func LargeMessageThreshold(bufferSize int) int
func IsLargeMessage(bufferSize int, payloadSize int) bool
func CurrentProtocolVersion() ProtocolVersion

func NewStandardLink(mem []byte, bufferCount int, bufferSize int, opts LinkOptions) (*StandardLink, error)
func NewAdvancedLink(mem []byte, bufferCount int, bufferSize int, opts AdvancedOptions) (*AdvancedLink, error)

func CreateStandardLink(name string, bufferCount int, bufferSize int, perm os.FileMode, opts LinkOptions) (*NamedStandardLink, error)
func OpenStandardLink(name string, bufferCount int, bufferSize int, opts LinkOptions) (*NamedStandardLink, error)
func CreateAdvancedLink(name string, bufferCount int, bufferSize int, perm os.FileMode, opts AdvancedOptions) (*NamedAdvancedLink, error)
func OpenAdvancedLink(name string, bufferCount int, bufferSize int, opts AdvancedOptions) (*NamedAdvancedLink, error)
```

### Multi-Client Acceptor (`package hqq`)

```go
func NewAcceptor(name string, cfg AcceptorConfig) (*Acceptor, error)
func NewAcceptorContext(ctx context.Context, name string, cfg AcceptorConfig) (*Acceptor, error)
func (a *Acceptor) Accept(ctx context.Context) (*NamedAdvancedLink, error)
func (a *Acceptor) Close() error
func (a *Acceptor) Name() string
func (a *Acceptor) Addr() net.Addr
func (a *Acceptor) Stats() AcceptorStats

func Connect(ctx context.Context, listenerName string, cfg Config) (*NamedAdvancedLink, error)
```

### `StandardLink` Methods

```go
func (l *StandardLink) Read(b []byte) (int, error)
func (l *StandardLink) Write(b []byte) (int, error)
func (l *StandardLink) ReadFrom(p []byte) (n int, addr net.Addr, err error)
func (l *StandardLink) WriteTo(p []byte, _ net.Addr) (n int, err error)
func (l *StandardLink) ReadZeroCopy(fn func(buffer []byte) error) (int, error)
func (l *StandardLink) WriteZeroCopy(fn func(buffer []byte) (int, error)) (int, error)
func (l *StandardLink) ReserveRead() (StandardReadReservation, error)
func (l *StandardLink) ReserveWrite() (StandardWriteReservation, error)
func (l *StandardLink) SetDeadline(t time.Time) error
func (l *StandardLink) SetReadDeadline(t time.Time) error
func (l *StandardLink) SetWriteDeadline(t time.Time) error
func (l *StandardLink) LocalAddr() net.Addr
func (l *StandardLink) PeerState() PeerState
func (l *StandardLink) Mode() LinkMode
func (l *StandardLink) Type() LinkType
func (l *StandardLink) BufferCount() int
func (l *StandardLink) BufferSize() int
func (l *StandardLink) Close() error
```

### `AdvancedLink`, `Stream` & RPC Methods

```go
func (l *AdvancedLink) Negotiate(ctx context.Context, features uint64) error
func (l *AdvancedLink) NegotiateProtocol(ctx context.Context, version ProtocolVersion, features uint64) (bool, error)
func (l *AdvancedLink) WaitForNegotiation(ctx context.Context) (bool, error)
func (l *AdvancedLink) Stream() *Stream
func (l *AdvancedLink) Send(ctx context.Context, p []byte) error
func (l *AdvancedLink) Recv(ctx context.Context) (*Message, error)
func (l *AdvancedLink) Call(ctx context.Context, method uint64, req []byte) (*Message, error)
func (l *AdvancedLink) CallWith(ctx context.Context, method uint64, req []byte, opts CallOptions) (*Message, error)
func (l *AdvancedLink) Serve(ctx context.Context, h Handler) error
func (l *AdvancedLink) Shutdown(ctx context.Context) error
func (l *AdvancedLink) ListenStreams(ctx context.Context) error
func (l *AdvancedLink) AcceptStream(ctx context.Context) (*Stream, error)
func (l *AdvancedLink) OpenStream(ctx context.Context) (*Stream, error)
func (l *AdvancedLink) Close() error

func (s *Stream) ID() uint64
func (s *Stream) Send(ctx context.Context, p []byte) error
func (s *Stream) Recv(ctx context.Context) (*Message, error)
func (s *Stream) Call(ctx context.Context, method uint64, req []byte) (*Message, error)
func (s *Stream) CallWith(ctx context.Context, method uint64, req []byte, opts CallOptions) (*Message, error)
func (s *Stream) Close() error

func (m *Message) Bytes() []byte
func (m *Message) Len() int
func (m *Message) StreamID() uint64
func (m *Message) Release()
```

### Configuration Types

```go
type Config struct {
	BufferCount int
	BufferSize  int
	Perm        os.FileMode
	Features    uint64
	Advanced    AdvancedOptions
}

type LinkOptions struct {
	WaitPolicy        WaitPolicy    // WaitAdaptive (default) or WaitSpin
	HeartbeatInterval time.Duration // default 250ms
	PeerTimeout       time.Duration // default 5s
	AttachTimeout     time.Duration // default 5s
}

type AdvancedOptions struct {
	MaxMessageSize     uint64
	MaxReassemblyBytes uint64
	IncomingQueueSize  int
	IncomingOverflow   OverflowPolicy
	StreamListenGrace  time.Duration
	Link               LinkOptions
}
```

---

## Documentation

| Document | Description |
| :--- | :--- |
| [**`docs/ARCHITECTURE.md`**](docs/ARCHITECTURE.md) | Shared-memory segment layout (`HQQ_STL3`), benchmarks, stream routing, crash recovery, OS platform notes, and verification commands. |
| [**`PROTOCOL.md`**](PROTOCOL.md) | Packet wire format, opcodes, and error sentinel hierarchy. |
| [**`bindgen/README.md`**](bindgen/README.md) | `hqq-bindgen` IDL code generator, C header bootstrapper, runtime (`bindgen/rt`), and `cgo` crossover analysis. |
| [**`launch/README.md`**](launch/README.md) | Supervised child-process launcher with `secureexec` binary verification and `--parent-stdin` lifeline. |
| [**`secureexec/README.md`**](secureexec/README.md) | TOCTOU-free executable verification (`memfd`, private vault, Windows share lock) and signed release manifests (`cmd/hqq-manifest`). |
| [**`c/README.md`**](c/README.md) | C11 reference library (`libhqq`) and `hqqrpc_main` server runtime. |
| [**`spec/SPEC.md`**](spec/SPEC.md) | Normative HQQ protocol specification and [`spec/MEMORY_MODEL.md`](spec/MEMORY_MODEL.md). |
