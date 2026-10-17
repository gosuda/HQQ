# HQQ: High-Throughput Shared-Memory IPC for Go

**HQQ** (`gosuda.org/hqq`) is a Go inter-process communication (IPC) library built on shared memory, lock-free Vyukov-style MPMC rings, and slot-owned payload buffers.

It delivers **sub-microsecond round-trip latency (~190 ns)** and **multi-GB/s throughput (24–100 GB/s)** while keeping idle blocked callers parked in the kernel instead of spinning (`futex` on Linux, `__ulock_wait` on macOS, named kernel semaphores on Windows).

---

## Highlights

- **Two composable protocol layers over one shared-memory layout**:
  - **Standard Protocol (`StandardLink`)**: fixed-capacity messages (`Read`, `Write`, `ReadZeroCopy`, `WriteZeroCopy`, `ReserveRead`, `ReserveWrite`, and `net.PacketConn` with datagram `ReadFrom`/`WriteTo` and deadlines) with zero heap allocations on the hot path.
  - **Advanced Protocol (`AdvancedLink`)**: negotiated overlay adding messages of up to `MaxMessageSize` bytes chunked over the ring (`Send`, `Recv`, pooled `Message`s), concurrent out-of-order RPC with a `net/http`-like server (`Call`, `Serve`, `Handler`, `Mux`), and logical streams (`Stream`, `ListenStreams`, `AcceptStream`, `OpenStream`).
  - **Protocol 1.1 (negotiated)**: `FeatureCancel` cancels a handler when its caller gives up, `FeatureMetadata` carries the caller's remaining deadline and `CallOptions` caller and trace IDs, and `Shutdown` drains accepted requests. Peers that negotiate only 1.0 see 1.0 behavior.
- **Adaptive cross-process parking (`WaitAdaptive`)**: re-checks the ring in a short tight loop, yields with `runtime.Gosched`, then parks in the OS kernel (`FUTEX_WAIT` on Linux, `UL_COMPARE_AND_WAIT_SHARED` on macOS, named semaphores for named segments on Windows). All goroutines of a process blocked on the same ring side share one kernel park (waiter aggregation), so N blocked goroutines pin one OS thread.
- **Crash and lifecycle detection (`HQQ_STL3` superblock)**: per-endpoint session tokens and heartbeats distinguish graceful peer shutdown (`io.EOF` from `Read`, `ErrPeerClosed` elsewhere) from abrupt termination (`ErrPeerGone`). Segments whose endpoints have all closed or died are re-initialized, a third endpoint is rejected (`ErrLinkBusy`), a dead initializer is taken over, and a foreign or other-version object is never modified.
- **Formal verification and checks**: the MPMC ring (claim, completion CAS, FIFO order, progress) is model-checked with TLC ([`internal/mpmc/MPMC.tla`](internal/mpmc/MPMC.tla), bounded model, not a proof). CI runs `go vet ./...` (including `unsafeptr`), `go test ./...`, `go test -race ./...` (not on Windows), and fuzz smoke tests.
- **Multi-client acceptor** (`NewAcceptor`, `Accept`, `Connect`): one listener name serves any number of clients. Each client has its own link segment, so a crashed client affects only its own link.
- **Other languages and tools**: C, C++, and Rust implementations, a normative specification (`spec/`), golden conformance vectors and an interop harness, `hqq-bindgen` (a generator that lets Go call a C library over shared memory without cgo), a `database/sql` driver for a SQLite service (`sqlite`), and tools for verified executables and signed release manifests (`secureexec`, `cmd/hqq-manifest`, `launch`). See [Other languages and interoperability](#other-languages-and-interoperability) and [Tools and examples](#tools-and-examples).

---

## Requirements

- Go 1.24 or newer.
- `bufferCount`: power of two, `2 ≤ bufferCount ≤ 1<<24` (default `256` in `hqq.Config`).
- `bufferSize`: multiple of 8 bytes, `8 ≤ bufferSize ≤ 1<<30` (default `4096` in `hqq.Config`).
- A target with native 64-bit atomics that are atomic across processes: `GOARCH=mips`, `GOARCH=mipsle` and `GOARCH=arm` with `GOARM` below 7 are rejected at build time (see [Build constraints](#build-constraints)).

---

## Quick Start

### 1. Named RPC and messages (`Listen` / `Dial` + `Serve`)

For most inter-process RPC and large-payload workloads, use `hqq.Listen` on the primary process and `hqq.Dial` on the secondary process. Both create or attach the shared-memory segment and complete Advanced Protocol negotiation. The server answers requests with `Serve` and a `Handler` (like `net/http`); the client sends them with `Call`:

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

	m, err := client.Call(ctx, MethodEcho, []byte("hello HQQ"))
	if err != nil {
		panic(err)
	}
	fmt.Println(string(m.Bytes())) // Output: echo: hello HQQ
	m.Release()                    // m.Bytes() is invalid from here on

	// Closing the client makes Serve return nil on the listener side, which
	// then closes the server link and removes the name.
	client.Close()
	if err := <-serverErr; err != nil {
		panic(err)
	}
}
```

A handler reports an error with `w.WriteError(status, payload)`; the caller's `Call` then returns a `hqq.ResponseError` (`errors.As`). Plain messages without request-response use `Send` and `Recv`; every `Recv` result is released with `m.Release()` (see [Zero-allocation and buffer ownership](#zero-allocation-and-buffer-ownership)).

### 2. Logical streams (`ListenStreams` / `AcceptStream` / `OpenStream`)

Streams on a negotiated link, given the Primary `server` and the Secondary `client` (`*hqq.NamedAdvancedLink` values returned by `Listen` and `Dial`). The acceptor must call `ListenStreams` first, otherwise the peer's `OpenStream` fails with `ErrConnNotFound`:

```go
// Primary: accept streams opened by the peer.
if err := server.ListenStreams(ctx); err != nil {
	panic(err)
}
go func() {
	s, err := server.AcceptStream(ctx) // visible only after its accept reply is on the ring
	if err != nil {
		return
	}
	_ = s.Send(ctx, []byte("hello on stream"))
}()

// Secondary: open a stream, use it, close it.
s, err := client.OpenStream(ctx) // Secondary IDs are odd, Primary IDs even; 0 is the link itself
if err != nil {
	panic(err)
}
m, err := s.Recv(ctx) // data messages of this stream only
if err != nil {
	panic(err)
}
fmt.Println(m.StreamID() == s.ID(), string(m.Bytes()))
m.Release()
if err := s.Close(); err != nil { // the peer's Recv returns ErrPeerClosed once its queue is empty; the ID is never reused
	panic(err)
}
```

### 3. Named `StandardLink` (`CreateStandardLink` / `OpenStandardLink`)

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

### 4. In-process or custom shared memory (`NewStandardLink`)

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

### 5. Multi-client acceptor (`NewAcceptor` / `Accept` / `Connect`)

One listener name serves any number of clients. Each client calls `Connect`, which creates its own link segment; the acceptor returns each negotiated link from `Accept`:

```go
// Server process
acc, err := hqq.NewAcceptor("my-service", hqq.AcceptorConfig{})
if err != nil {
	log.Fatal(err)
}
defer acc.Close()

mux := hqq.NewMux()
mux.HandleFunc(MethodEcho, func(ctx context.Context, w *hqq.ResponseWriter, req *hqq.Request) {
	w.Write(req.Payload())
})
for {
	link, err := acc.Accept(ctx) // one link per client
	if err != nil {
		break // ctx ended, or the acceptor was closed (ErrAcceptorClosed)
	}
	go func() {
		defer link.Close()
		_ = link.Serve(ctx, mux)
	}()
}

// Client process (any number of them)
link, err := hqq.Connect(ctx, "my-service", hqq.Config{})
if err != nil {
	panic(err)
}
defer link.Close()

m, err := link.Call(ctx, MethodEcho, []byte("hello"))
```

A client that crashes affects only its own link. See [Acceptor and `Connect` Behavior](#acceptor-and-connect-behavior) for the failure errors and the defaults.

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

The normative specification is [`spec/SPEC.md`](spec/SPEC.md), with [`spec/MEMORY_MODEL.md`](spec/MEMORY_MODEL.md) for memory orders. [PROTOCOL.md](PROTOCOL.md) is an overview of the wire and memory layout that points to these documents.

---

## Other languages and interoperability

The protocol is implemented outside Go. Each implementation is checked against this package with a two-process interop matrix and golden conformance vectors.

| Component | Location | Status |
| --- | --- | --- |
| C library `libhqq` (C11): Standard, Advanced, acceptor, and listener layers, and the parent lifeline (`hqq_lifeline.h`) | [`c/`](c/README.md), headers in [`c/include/hqq/`](c/include/hqq) | Tests and the Go/C interop pairings verified on the authoring host (macOS arm64). CI builds it on Linux, macOS, and Windows (clang-cl; MSVC is experimental). |
| C++17 header-only wrapper (`hqq::`) | [`bindings/cpp/`](bindings/cpp) | Tests, examples, and interop verified on the authoring host (macOS arm64). CI in `bindings.yml`: Linux (gcc, clang with sanitizers), macOS, and Windows (clang-cl). |
| Rust: `hqq-sys` (FFI), `hqq` (safe API), `hqq-interop` | [`bindings/rust/`](bindings/rust) | Tests, examples, and interop verified on the authoring host (macOS arm64). CI in `bindings.yml`: Linux, macOS, and Windows (MSVC). |
| Specification | [`spec/`](spec) | [`SPEC.md`](spec/SPEC.md) (Standard and Advanced Protocol 1.0, normative), [`MEMORY_MODEL.md`](spec/MEMORY_MODEL.md), [`PORTING.md`](spec/PORTING.md), [`ext/`](spec/ext) (Advanced Protocol 1.1 and the listener segment), [`constants.json`](spec/constants.json), [`litmus/`](spec/litmus) |
| Conformance vectors | [`conformance/`](conformance) | Golden vectors generated by the Go implementation, checked by `go test ./conformance/...`, and run in CI by `interop.yml`. |
| Interop harness | [`cmd/hqq-interop`](cmd/hqq-interop), [`interop/`](interop), [`conformance/INTEROP_CLI.md`](conformance/INTEROP_CLI.md) | Two-process matrix over the implementations listed in `HQQ_INTEROP_IMPLS` (for example `go=/path/to/hqq-interop-go,c=/path/to/hqq_interop`). Go, C, C++, and Rust pairings verified on the authoring host; run in CI by `interop.yml` and the binding workflows. |
| Memory-model verification | [`c/verify/`](c/verify) | GenMC harnesses over the C ring, herd7 litmus tests, and stress litmus tests. CI only. |

The wait/wake handshake must use sequentially consistent accesses (or equivalent fences) for `W1`, `W3`, `K1`, and `K2`; see [`spec/MEMORY_MODEL.md`](spec/MEMORY_MODEL.md) §4. Relaxed or acquire/release-only handshakes can lose wake-ups.

## Tools and examples

### SQLite service (`sqlite`)

The `sqlite` package is a `database/sql` driver, registered as `hqq-sqlite`, for a SQLite database that runs as a separate service reached through an HQQ listener. The client needs no cgo. Each `database/sql` connection is one HQQ stream of a shared link, and the server runs each stream as one session with its own SQLite connection, transaction state, and statements. The service is the C server in [`examples/sqlite/server`](examples/sqlite/server) (Linux, macOS and Windows with clang-cl), or the Go fake server used by the tests.

```go
import (
	"database/sql"

	_ "gosuda.org/hqq/sqlite" // registers "hqq-sqlite"
)

db, err := sql.Open("hqq-sqlite", "hqq://sqlite-svc?db=main&timeout=5s&fetch=256&busy=5s")
```

The DSN host is the listener name of the server. Its options are `db` (required: an alias or a path), `mode` (`rwc`, `rw`, or `ro`), `timeout`, `fetch`, `busy`, `txlock`, and `pragma.<name>`; unknown or repeated options are errors. [`sqlite/README.md`](sqlite/README.md) gives the full list and the mapping of values and results. To keep the connector, for example to tune the link or to close the shared link yourself, use `sqlite.NewConnector`:

```go
c, err := sqlite.NewConnector("sqlite-svc", sqlite.Config{
	Database:    "main",
	Timeout:     5 * time.Second,
	FetchSize:   256,
	BusyTimeout: 5 * time.Second,
})
if err != nil {
	return err
}
db := sql.OpenDB(c) // all connections of db share one link
```

A SQLite error is a `*sqlite.Error`: `Code` is the primary result code and `ExtendedCode` the extended one.

```go
if _, err := db.Exec("INSERT INTO users (email) VALUES (?)", email); err != nil {
	var se *sqlite.Error
	switch {
	case errors.As(err, &se) && se.ExtendedCode == sqlite.CodeConstraintUnique:
		return ErrDuplicate
	case sqlite.IsBusy(err):
		return ErrRetryLater
	}
	return err
}
```

A lost link during `Exec` returns an error that wraps `sqlite.ErrOutcomeUnknown`, and it is not retried, because the statement may have run. Other lost-link errors wrap `driver.ErrBadConn`, so `database/sql` may retry them on a new connection. A `Query` is retried the same way, so send writes through `Exec` or inside a transaction.

The database/sql contract suite (`sqlite/internal/contract`) runs against the fake server (`go test ./sqlite`) and against the C server (`sqlite/e2e`). To build and run the C server:

```sh
cmake -S c -B build -DHQQ_BUILD_SQLITE_SERVER=ON -DHQQ_WERROR=ON
cmake --build build --target hqq-sqlite-server
build/sqlite-server/hqq-sqlite-server sqlite-svc --alias main=/path/to/app.db
```

The server opens the aliases given with `--alias`, and raw paths only inside `--allow-dir`. Its wire protocol is in [`sqlite/PROTOCOL.md`](sqlite/PROTOCOL.md).

### Starting the SQLite server from Go (`sqlite/launcher`)

`sqlite/launcher` starts the C server as a child process, verifies it before it runs, and returns a `database/sql` handle for its listener:

```go
want, err := secureexec.ParseHash(digestHex) // 64 hex digits, from the release manifest
if err != nil {
	return err
}
m, err := launcher.OpenManaged(ctx, launcher.Config{
	Binary:  "/opt/app/bin/hqq-sqlite-server",
	SHA256:  want,
	Aliases: map[string]string{"main": "/var/lib/app/app.sqlite"},
}, launcher.WithDatabase("main"))
if err != nil {
	return err
}
defer m.Close() // closes the DB, then stops the server
var one int
err = m.DB.QueryRowContext(ctx, "SELECT 1").Scan(&one)
```

- `Start` hashes the binary with `secureexec` before it runs. The expected digest is `Config.SHA256`, or the entry for this platform of a signed manifest (`Config.Manifest`, whose signature is checked before the manifest is parsed). A mismatch starts nothing and returns an error that wraps `secureexec.ErrHashMismatch`.
- The server is started with `--parent-stdin`. It shuts down gracefully, rolling back open transactions, when the process that started it exits, including by SIGKILL.
- `Open` returns the `*sql.DB` and the `*Server` separately: close the DB first, then the server. `OpenManaged` returns both in one `Managed` value.
- The launcher does not restart the server; the caller does. The details are in [`sqlite/launcher/README.md`](sqlite/launcher/README.md).

### Starting any HQQ server from Go (`launch`)

`launch` is the service-agnostic part of `sqlite/launcher`. `launch.Start(ctx, cfg, args...)` verifies the server binary (`Config.SHA256`, or a signed manifest in `Config.Manifest`), starts it with a parent lifeline (`--parent-stdin` by default), waits for the `ready <listener>` line, and returns a `*launch.Server` that stops gracefully and is killed after `StopTimeout`. It does not restart or supervise. See [`launch/README.md`](launch/README.md).

### Calling C libraries from Go (`hqq-bindgen`)

`hqq-bindgen` generates a Go client and a C server from an interface description (IDL). The C library runs in its own process behind the generated server; the Go client has no cgo and talks to it over HQQ shared memory. Only functions declared in the IDL can be called, handles are checked by the server, and client and server compare an interface digest when a session opens.

```sh
go run gosuda.org/hqq/cmd/hqq-bindgen -go-out client -go-strip ml_ -c-out server mylib.idl
go run gosuda.org/hqq/cmd/hqq-bindgen -check -go-out client -go-strip ml_ -c-out server mylib.idl   # fails on stale output
```

What it is good at, measured on an Apple M1 (details and caveats in [`bindgen/README.md`](bindgen/README.md)): coarse calls. Compressing 64 KiB to 1 MiB with zstd through the generated client, with calls pipelined, is within 2% of cgo for one goroutine, and two sessions against two cgo threads are within 6% at 1 MiB. What it is not good at: tiny calls. A synchronous call with no work takes 310 ns against 25 ns for cgo, and a pipelined one 160–175 ns. The benefits it offers instead of speed are isolation (a crash in the C library ends the server process, not the Go program), no cgo in the Go build, and calls that overlap with Go work. There is no call cancellation, no callbacks, and the C server runs on POSIX systems only.

The design is [`spec/toolchain/BINDGEN.md`](spec/toolchain/BINDGEN.md); examples are [`bindgen/examples/zstd`](bindgen/examples/zstd) and [`bindgen/examples/sqlite3`](bindgen/examples/sqlite3) (with the `hqq-sqlite-gen` driver in [`sqlite/gendriver`](sqlite/gendriver), which passes the same `database/sql` contract suite as `hqq-sqlite`). Generated servers are started with [`launch`](#starting-any-hqq-server-from-go-launch).

### Verified executables

`secureexec` verifies the SHA-256 digest of a native executable and starts it without a time-of-check/time-of-use (TOCTOU) gap, so the bytes that are hashed are the bytes that run. It uses only the standard library.

Threat model in one sentence: the verifier runs as an ordinary user, the attacker is another local process or user who can write where the binary is read from, and a path swap or an in-place overwrite between the check and the exec must not change what runs; root, kernel compromise, and a compromised expected digest are out of scope.

```go
want, err := secureexec.ParseHash(expectedHex) // from a signed manifest
if err != nil {
	return err
}
v, err := secureexec.Verify("/opt/app/bin/hqq-sqlite-server", want)
if err != nil {
	return err // *HashMismatchError when the bytes differ
}
defer v.Close() // after Start; the child keeps running

cmd := v.Command(ctx, "app-db", "--parent-stdin")
cmd.Stdout = os.Stdout
cmd.Stderr = os.Stderr
if err := cmd.Start(); err != nil { // start while v is still open
	return err
}
```

`secureexec.Start` combines these steps and releases the snapshot when the child exits. The mechanism is chosen per platform:

| Platform | `Mechanism()` | How the hashed bytes are protected |
| --- | --- | --- |
| Linux | `memfd` (`vault` when memfd is unavailable) | The file is copied once into a sealed memfd, which has no path, and runs as `/proc/self/fd/3`. The seals hold against every user, including processes of the same user. |
| macOS, FreeBSD, other POSIX | `vault` | The file is copied into a new directory that only the owner can enter (mode 0700), as a read-only file (mode 0500), and its digest is checked after it is written. Other users cannot change it; processes of the same user can. |
| Windows | `windows-share-lock` | The file is opened with `FILE_SHARE_READ` only, and the handle stays open while the child starts, so no other handle can write, delete, or rename it. |
| Other platforms | none | `Verify` returns `ErrUnsupported`. |

- `secureexec.Policy{TrustOwnedReadOnly: true}` (`secureexec.WithPolicy`) skips the copy for a file, and every directory above it, that root or the current user owns and that group and other cannot write. A process of the same user can then change the file between the check and the exec.
- On POSIX the final path component must not be a symbolic link (`ELOOP`); on Windows symbolic links are resolved.
- The signed release manifest and `cmd/hqq-manifest` are described in [Release binaries and manifests](#release-binaries-and-manifests). Details are in [`secureexec/README.md`](secureexec/README.md).

### Parent lifeline

The `lifeline` package and the C library tie a child process to its parent through the child's standard input. The parent gives the child the read end of a pipe and keeps the write end. The child reads its standard input until end of file. End of file comes when every holder of the write end has closed it: the parent closes it, or the parent dies in any way, including SIGKILL, because the kernel closes its descriptors.

Parent side, in Go:

```go
cmd := exec.Command(server, "--parent-stdin")
ll, err := lifeline.Attach(cmd) // sets cmd.Stdin
if err != nil {
	return err
}
defer ll.Close() // ends the child's life when the parent returns
if err := cmd.Start(); err != nil {
	return err
}
```

Child side, in Go, called once early in `main`:

```go
ctx, stop := lifeline.Watch(context.Background())
defer stop()
<-ctx.Done() // standard input reached end of file, or failed
```

`lifeline.WithKeepalive(d)` makes the parent write one byte every `d`, so a child can notice a parent that is stopped rather than dead; `Watch` discards those bytes.

The C server takes the same protocol as `--parent-stdin` (alias `--exit-on-stdin-eof`). On end of file it shuts down gracefully, rolls back open transactions, and exits with status 0; if that takes longer than `--lifeline-grace-ms` (default 5000), it exits with status 70. Other services in C use `hqq_lifeline_start` from [`c/include/hqq/hqq_lifeline.h`](c/include/hqq/hqq_lifeline.h).

**Guarantee.** A service started with a lifeline is not left running as an orphan when its parent dies in any way that closes its descriptors, including SIGKILL and a crash.

**Limits.**

- A parent that is stopped (SIGSTOP, a debugger) rather than dead keeps the pipe open, so the child keeps running.
- Any process that inherits the write end holds the pipe open. `os.Pipe` is close-on-exec, so only a process that inherits it on purpose can.
- Only the calling thread survives `fork()`, so a child that forks and keeps running loses its lifeline thread. The C server must not fork after start.
- A grandchild that inherits the child's standard input holds it open, so end of file comes only after the grandchild exits too.
- Terminals are not watched unless forced (`lifeline.WatchTerminal`, `--parent-stdin=force`), so a closed terminal does not stop an interactive run.
- A child must read its standard input for its whole life. `Watch` cannot interrupt a blocked read, so its goroutine ends with the process.

### Release binaries and manifests

A release of the SQLite server contains `hqq-sqlite-server_<os>_<arch>` binaries, a version 1 release manifest `manifest.json` (file names, sizes, SHA-256 digests, licenses), its ed25519 signature `manifest.json.sig`, `licenses.tar.gz`, and `SHA256SUMS`. The release process, key handling, and platform list are in [`docs/RELEASING.md`](docs/RELEASING.md).

To verify a download, embed the release public key in your program, and check the manifest before any digest from it is used:

```go
pub, err := manifest.ParsePublicKey(releasePublicKey) // your copy of the release public key
if err != nil {
	return err
}
data, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
if err != nil {
	return err
}
sig, err := os.ReadFile(filepath.Join(dir, "manifest.json.sig"))
if err != nil {
	return err
}
m, err := manifest.Verify(data, sig, pub) // the signature is checked before the manifest is parsed
if err != nil {
	return err
}
f, ok := m.Find(runtime.GOOS, runtime.GOARCH)
if !ok {
	return fmt.Errorf("no build of %s for %s/%s", m.Name, runtime.GOOS, runtime.GOARCH)
}
v, err := secureexec.Verify(filepath.Join(dir, f.Name), f.SHA256)
if err != nil {
	return err // *HashMismatchError when the bytes differ
}
defer v.Close()
```

From a clone of this repository, the same checks run on the command line:

```sh
go run ./cmd/hqq-manifest verify -pub release_public_key.hex \
  -in manifest.json -sig manifest.json.sig -dir hqq-sqlite-release
sha256sum -c SHA256SUMS
```

`hqq-manifest` (`cmd/hqq-manifest`) has these commands. Its exit status is 0 on success, 1 on failure, and 2 on a usage error.

- `keygen -priv FILE -pub FILE` creates an ed25519 key pair as hex text. The private file is written with mode 0600, and an existing file is never overwritten.
- `gen -dir DIR -name NAME -release REL [-license ID]... -out FILE` scans DIR for `NAME_<os>_<arch>[.exe]` artifacts and writes an unsigned manifest.
- `sign -key FILE` or `sign -key-env VAR`, `-in FILE [-out FILE.sig]` writes the detached signature of a manifest. `-key-env` reads the private key from an environment variable, which is how CI supplies it.
- `verify -pub FILE -in FILE -sig FILE [-dir DIR]` checks the signature and, with `-dir`, the size and SHA-256 of every artifact.
- `hash FILE...` prints `sha256sum`-style lines.

---

## Benchmarks

Measured on `darwin/arm64` (Apple M1, Go 1.27, quiet machine, median of 5–6 runs of `go test . -bench 'Benchmark(StandardLink|AdvancedLink|Compare)' -benchmem`); idle CPU from `HQQ_PERF_TESTS=1 go test -run 'TestIdleLinkDoesNotBurnCPU|TestMPMCWaitSpinBurnsCPU' ./...`. Both ends of every benchmark run in one process, on two goroutines; a Go client against a C server in two processes measured 186–187 ns average for the same 64-byte round trip.

### HQQ vs Unix Domain Socket (`SOCK_STREAM`) vs OS Pipe

| Benchmark | HQQ (`WaitAdaptive`) | HQQ (`WaitSpin`) | Unix Domain Socket | OS Pipe | HQQ Speedup |
| --- | ---: | ---: | ---: | ---: | ---: |
| **64 B Round-Trip Latency** | **192 ns/op** | 193 ns/op | 3,121 ns/op | 3,341 ns/op | **16.2×–17.4× faster** |
| **4 KiB Unidirectional Throughput** | **26,170 MB/s** (157 ns/op) | 24,690 MB/s | 2,700 MB/s | 4,357 MB/s | **6.0×–9.7× higher** |
| **64 KiB Unidirectional Throughput** | **29,230 MB/s** (2.24 µs/op) | 29,010 MB/s | 3,110 MB/s | 11,770 MB/s | **2.5×–9.4× higher** |
| **Idle Blocked Callers** | **~1% CPU** (2 blocked calls, incl. heartbeats) | >100% CPU (one core per blocked call) | blocks in the kernel | blocks in the kernel | kernel parking |

### StandardLink & AdvancedLink Microbenchmarks

| Operation | Latency | Throughput | Allocs |
| --- | ---: | ---: | ---: |
| `StandardLink` Read/Write (`8 B`) | **29.0 ns/op** | **276 MB/s** | `0 B/op`, `0 allocs/op` |
| `StandardLink` Zero-Copy (`4 KiB`) | **41.1 ns/op** | **99,650 MB/s** | `0 B/op`, `0 allocs/op` |
| `StandardLink` Reserve/Commit (`4 KiB`) | **68.4 ns/op** | **59,900 MB/s** | `0 B/op`, `0 allocs/op` |
| `StandardLink` Pipeline (`64 B`) | **38.5 ns/op** | **1,662 MB/s** | `0 B/op`, `0 allocs/op` |
| `StandardLink` Pipeline (`4 KiB`) | **121.8 ns/op** | **33,630 MB/s** | `0 B/op`, `0 allocs/op` |
| `AdvancedLink` Send/Recv + `Release` (`64 B`, one chunk) | **151.8 ns/op** | **422 MB/s** | `0 B/op`, `0 allocs/op` |
| `AdvancedLink` Send/Recv + `Release` (`4 KiB` over `256 B` chunks) | **1.46 µs/op** | **2,807 MB/s** | `0 B/op`, `0 allocs/op` |
| `AdvancedLink` Call + `Release` via `Serve` (`64 B` req + `64 B` resp) | **555 ns/op** | **230 MB/s** | `0 B/op`, `0 allocs/op` |
| `AdvancedLink` Call + `Release` via `Serve` (`1 KiB` req + `1 KiB` resp, `256 B` chunks) | **1.25 µs/op** | **1,642 MB/s** | `0 B/op`, `0 allocs/op` |

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

### Acceptor and `Connect`

```go
// Server: one listener name, any number of clients (AcceptorConfig zero value = defaults)
func NewAcceptor(name string, cfg AcceptorConfig) (*Acceptor, error)
func NewAcceptorContext(ctx context.Context, name string, cfg AcceptorConfig) (*Acceptor, error)
func (a *Acceptor) Accept(ctx context.Context) (*NamedAdvancedLink, error) // ErrAcceptorClosed after Close
func (a *Acceptor) Close() error // idempotent; removes the listener name; links already accepted stay open
func (a *Acceptor) Name() string
func (a *Acceptor) Addr() net.Addr
func (a *Acceptor) Stats() AcceptorStats

// Client: one link per call, negotiated as Primary
func Connect(ctx context.Context, listenerName string, cfg Config) (*NamedAdvancedLink, error)
```

`AcceptorConfig` fields: `Perm`, `QueueSize`, `Features`, `Advanced` (`AdvancedOptions` for every accepted link), `MaxBufferCount`, `MaxBufferSize`, `MaxLinkSize`, `Backlog`, `MaxHandshakes`, `HandshakeTimeout`, `StuckSlotTimeout`. Defaults are listed under [Acceptor and `Connect` Behavior](#acceptor-and-connect-behavior). `AcceptorStats` has the fields `Records`, `Accepted`, `Malformed`, `Unreachable`, `HandshakeFailed`, and `StuckSlots`.

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

### `AdvancedLink`, `Stream` & RPC Methods

```go
// Role-aware one-call negotiation (Primary initiates, Secondary waits)
func (l *AdvancedLink) Negotiate(ctx context.Context, features uint64) error
func (l *AdvancedLink) NegotiateProtocol(ctx context.Context, version ProtocolVersion, features uint64) (bool, error)
func (l *AdvancedLink) WaitForNegotiation(ctx context.Context) (bool, error)
func (l *AdvancedLink) ProtocolState() ProtocolState
func (l *AdvancedLink) NegotiatedVersion() ProtocolVersion
func (l *AdvancedLink) NegotiatedFeatures() uint64 // test with FeatureMessages / FeatureRequestResponse

// The link-level stream (ID 0): Send, Recv and Call are shortcuts for l.Stream().X
func (l *AdvancedLink) Stream() *Stream
func (l *AdvancedLink) Send(ctx context.Context, p []byte) error
func (l *AdvancedLink) Recv(ctx context.Context) (*Message, error) // data messages only; Release the result
func (l *AdvancedLink) Call(ctx context.Context, method uint64, req []byte) (*Message, error) // Release the result
func (l *AdvancedLink) CallWith(ctx context.Context, method uint64, req []byte, opts CallOptions) (*Message, error) // protocol 1.1 metadata
func (l *AdvancedLink) Serve(ctx context.Context, h Handler) error
func (l *AdvancedLink) Shutdown(ctx context.Context) error // refuse new requests, drain accepted ones; Serve then returns nil

// Logical streams (Primary allocates even IDs 2, 4, ...; Secondary odd IDs 1, 3, ...; 0 is the link)
func (l *AdvancedLink) ListenStreams(ctx context.Context) error
func (l *AdvancedLink) AcceptStream(ctx context.Context) (*Stream, error)
func (l *AdvancedLink) OpenStream(ctx context.Context) (*Stream, error)

func (s *Stream) ID() uint64
func (s *Stream) Send(ctx context.Context, p []byte) error
func (s *Stream) Recv(ctx context.Context) (*Message, error)
func (s *Stream) Call(ctx context.Context, method uint64, req []byte) (*Message, error)
func (s *Stream) CallWith(ctx context.Context, method uint64, req []byte, opts CallOptions) (*Message, error) // protocol 1.1 metadata
func (s *Stream) Close() error // no context; ErrInvalidOp on stream 0

// Received messages (pooled)
func (m *Message) Bytes() []byte
func (m *Message) Len() int
func (m *Message) StreamID() uint64
func (m *Message) Release() // at most once; optional

// Server side (like net/http)
type Handler interface {
	ServeHQQ(ctx context.Context, w *ResponseWriter, req *Request)
}
type HandlerFunc func(ctx context.Context, w *ResponseWriter, req *Request)

func (r *Request) Method() uint64
func (r *Request) Payload() []byte // valid only during ServeHQQ
func (r *Request) StreamID() uint64
func (r *Request) Caller() uint64  // protocol 1.1: caller ID from CallOptions, or zero
func (r *Request) Trace() [16]byte // protocol 1.1: trace ID from CallOptions, or zero
func (w *ResponseWriter) Write(p []byte) (int, error)     // appends to the response body
func (w *ResponseWriter) WriteError(status uint64, p []byte) // error response; Call returns ResponseError

func NewMux() *Mux // Mux.NotFound Handler handles unregistered methods
func (m *Mux) Handle(method uint64, h Handler)
func (m *Mux) HandleFunc(method uint64, f func(ctx context.Context, w *ResponseWriter, req *Request))

type ResponseError struct { // returned by value from Call for an error response
	Status  uint64
	Payload []byte // a copy owned by the caller
}

type CallOptions struct { // request metadata, sent with FeatureMetadata
	Caller uint64   // zero: not sent
	Trace  [16]byte // all zero: not sent
}

// Link information and lifecycle
func (l *AdvancedLink) Stats() AdvancedStats
func (l *AdvancedLink) PeerState() PeerState
func (l *AdvancedLink) Mode() LinkMode
func (l *AdvancedLink) Type() LinkType
func (l *AdvancedLink) BufferCount() int
func (l *AdvancedLink) BufferSize() int
func (l *AdvancedLink) Close() error
```

- **Negotiation is terminal on failure.** `ProtocolStateFailed` cannot be left: any error during an attempt (rejection, invalid packets, a canceled context, a closed link) makes later `Negotiate`/`NegotiateProtocol`/`WaitForNegotiation` calls return `ErrNegotiationFailed`. `ErrAlreadyNegotiated` is returned only while an attempt is in progress or after it succeeded. Close the link and open a new one after a failure.
- **Streams, `Serve`, routing, and close semantics** are described in [Streams, `Serve`, and routing](#streams-serve-and-routing); buffer lifetimes in [Zero-allocation and buffer ownership](#zero-allocation-and-buffer-ownership).

### Configuration types

```go
type Config struct {
	BufferCount int          // default DefaultBufferCount (256)
	BufferSize  int          // default DefaultBufferSize (4096)
	Perm        os.FileMode  // permission of the created segment (Listen only), default 0o600
	Features    uint64       // default: all supported features (FeatureMessages|FeatureRequestResponse|FeatureCancel|FeatureMetadata)
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
	StreamListenGrace time.Duration // stream requests held before ListenStreams; default 2s, negative refuses at once
	Link               LinkOptions
}
```

The errors and their `errors.Is` relationships are listed in [PROTOCOL.md](PROTOCOL.md#error-hierarchy); the packet formats are in [`spec/SPEC.md`](spec/SPEC.md) and [`spec/ext/PROTOCOL_1_1_DRAFT.md`](spec/ext/PROTOCOL_1_1_DRAFT.md).

### Streams, `Serve`, and routing

**Streams.**

- Stream 0 is the link itself: `l.Stream()` returns it (always the same pointer), it is always open, and `Close` on it returns `ErrInvalidOp`. `l.Send`, `l.Recv`, and `l.Call` are exactly `l.Stream().Send`, `Recv`, and `Call`.
- Further streams: the acceptor calls `ListenStreams` and then `AcceptStream`; the peer's `OpenStream` waits for the accept. A stream request that arrives before `ListenStreams` is held for `AdvancedOptions.StreamListenGrace` (default 2 s) and accepted, in arrival order, when `ListenStreams` runs; one still held when the grace period ends is refused with `ErrConnNotFound`. `ListenStreams` twice returns `ErrAlreadyListening`, and `AcceptStream` before `ListenStreams` returns `ErrNotListening`. The Primary allocates even IDs (2, 4, ...), the Secondary odd IDs (1, 3, ...), and IDs are never reused. An incoming stream is registered when its `OpConnCreate` arrives, so the messages the peer sends on it are queued before `AcceptStream` returns it.
- `Send` publishes `p` as one message of up to `MaxMessageSize` bytes (`ErrBufferOverflow` otherwise, nothing is sent), split into chunks of at most `BufferSize` bytes; the peer receives it as one message. `p` may be reused as soon as `Send` returns. `Send` needs `FeatureMessages`, `Call` and `Serve` need `FeatureRequestResponse` (`ErrNotNegotiated` otherwise).
- **Ordering.** Messages sent one after the other on a stream arrive in that order. Concurrent `Send`s on a stream are not ordered relative to each other, and there is no ordering between streams. Responses to concurrent `Call`s may arrive in any order.

**Serve.**

- `Serve(ctx, h)` receives the requests of every stream of the link and answers each with `h`. `h.ServeHQQ` gets its own context (see below), a `*ResponseWriter`, and a `*Request` (`Method`, `Payload`, `StreamID`, and, with `FeatureMetadata`, `Caller` and `Trace`). When `ServeHQQ` returns, the response is sent on the stream the request arrived on, unless the caller gave up in the meantime. `Stream.Recv` never returns a request; `Serve` is the only consumer of requests.
- A handler that writes nothing answers with an empty success response. `w.Write` appends to the body (`ErrBufferOverflow` and nothing written if the body would exceed `MaxMessageSize`). `w.WriteError(status, p)` turns the response into an error response, replaces the body written so far, and makes the caller's `Call` return `ResponseError{Status, Payload}` (by value, `Payload` is a copy; test with `errors.As(err, &re)` for `var re hqq.ResponseError`). A later `Write` appends to the error body. A zero `status` is sent as `ErrCodeInvalidOp`; a `p` larger than `MaxMessageSize` becomes status `ErrCodeBufferOverflow` with an empty payload. A successful response always has status 0.
- `Mux` dispatches by method; an unregistered method gets status `ErrCodeInvalidOp` with payload `hqq: unknown method`, unless `Mux.NotFound` is set. `Serve` with a nil handler returns `ErrInvalidOp`.
- **Execution model.** One goroutine at a time, the receiver, takes requests from the request queue. While no handler is running, the receiver runs each request's handler itself (inline), which saves a goroutine hand-off per request. If an inline handler is still running after 200 µs, the receiver role passes to a new goroutine, so later requests are not stuck behind the slow handler (the slow handler finishes on its own goroutine). While any handler is running, the receiver hands requests to pooled worker goroutines, which are started on demand, up to `BufferCount`, and stay until `Serve` returns. As soon as a request arrives while no handler is running, it runs inline again, so a one-off slow handler affects one episode only. Concurrency is bounded by `BufferCount`; when all handlers are busy, `Serve` stops receiving requests until one finishes. A request that arrives behind a slow inline handler waits up to about 200 µs for the hand-over. `Serve` waits for running handlers before it returns.
- `Serve` returns `ctx.Err()` when `ctx` ends, nil when the peer closed the link gracefully or `Shutdown` completed, and the receive error otherwise.

**Deadlines, metadata, cancellation, and shutdown (protocol 1.1).** These features are negotiated with `FeatureCancel` and `FeatureMetadata`, which the default `Config.Features` and `Negotiate(ctx, 0)` request. Against a peer that did not negotiate them, the calls behave as in 1.0.

- **Deadline.** If the context of a `Call` has a deadline, the remaining time is sent with the request and becomes the deadline of the handler's context. A `Call` whose deadline has already passed returns `context.DeadlineExceeded` without sending anything.
- **Cancellation.** If the context of a waiting `Call` ends, the peer is told to cancel the handler's context (`OpCancel`, best effort). The handler sees `ctx.Done()` closed, and no response is sent.
- **Metadata.** `Stream.CallWith` and `AdvancedLink.CallWith` take a `CallOptions` with a caller ID and a 16-byte trace ID; the handler reads them with `req.Caller()` and `req.Trace()`. Zero values are not sent.
- **Handler context.** Each handler gets its own context. It is canceled when the caller cancels, when the deadline expires, when the stream closes (on either side), when `Serve` ends, and when the link is closed. It is valid only during `ServeHQQ`.
- **Shutdown.** `AdvancedLink.Shutdown(ctx)` refuses new requests (the caller's `Call` returns a `ResponseError` matching `ErrShuttingDown`), waits until the accepted requests are answered or `ctx` ends, and then makes `Serve` return nil. Other calls of the link keep working.

```go
// Caller: the remaining time and the caller and trace IDs travel with the request.
ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
defer cancel()
m, err := link.CallWith(ctx, MethodEcho, []byte("hello"), hqq.CallOptions{Caller: 42})
if err != nil {
	panic(err)
}
m.Release()

// Handler: its context ends at the caller's deadline or on cancellation.
mux.HandleFunc(MethodEcho, func(ctx context.Context, w *hqq.ResponseWriter, req *hqq.Request) {
	select {
	case <-time.After(100 * time.Millisecond):
		w.Write(req.Payload())
	case <-ctx.Done():
		// The caller gave up or its deadline passed: no response is sent.
	}
})

// Graceful stop: refuse new requests, drain the accepted ones, then close.
if err := link.Shutdown(ctx); err != nil {
	log.Printf("drain: %v", err)
}
link.Close()
```

**Routing.**

- The receive path assembles every message and routes it by kind: a response goes to the waiting `Call`, a request to the request queue consumed by `Serve`, and data to the queue of its stream. A goroutine that finds no dispatcher receives synchronously (lowest latency) and routes messages for others; a background dispatcher starts as soon as several goroutines wait concurrently or streams are used (`OpenStream`, `AcceptStream`, `ListenStreams`).
- A response without a waiting call is dropped and counted in `Stats().DroppedMessages`; a late response to a canceled call is discarded and counted in `AbandonedResponses` instead. Data for an unknown or closed stream is dropped and counted in `DroppedMessages`. An aborted message (the sender's multi-chunk `Send` failed) is never delivered; an aborted response fails the waiting `Call` with `ErrMessageAborted`. A message refused by `MaxReassemblyBytes` is dropped and counted: a refused response fails the waiting `Call` with `ErrReassemblyLimit`, a refused request is answered with an error response (status `ErrCodeBufferOverflow`).
- **Overflow is decided per destination queue** (`AdvancedOptions.IncomingQueueSize`, default `bufferCount`, applies to each stream queue, the request queue, and the accept queue). `OverflowBlock` (default): the receive path waits for that queue's consumer (`Recv`, `Serve`, `AcceptStream`) and the wait propagates into the ring; nothing else is routed meanwhile, not even responses, so consume every stream you use and run `Serve` if the peer sends requests (without `Serve`, requests fill the request queue and then stall everything). `OverflowDrop`: the message is dropped and counted, a stream is refused with `OpError(ErrCodeBufferOverflow)`, and the receive path never blocks. `OverflowFail`: the receive path stops with `ErrIncomingOverflow`; pending and future calls fail with it, and `Recv` returns the queued messages first and then `ErrIncomingOverflow`.
- A synchronous receiver never waits for a full queue under `OverflowBlock`: it hands the delivery to a helper goroutine (which keeps the order) and a dispatcher takes over, so a caller's `Recv` or `Call` timeout cannot lose another goroutine's message.
- **The receive path never waits for the tx ring.** Stream accepts and refusals, and the error responses to requests refused by `MaxReassemblyBytes`, are sent by a control-sender goroutine through a bounded queue (`max(BufferCount, 16)` replies); a reply that does not fit is dropped and, for a reply to an `OpConnCreate`, counted in `AdvancedStats.DroppedMessages`. The peer's `OpenStream`/`Call` then waits until its own context ends, so always pass a context with a deadline.

**Close and errors.**

- After a local `Stream.Close`, `Send`, `Recv`, and `Call` on it return `ErrClosed`, as does a second `Close`; the queued messages are discarded and blocked `Recv`/`Call` calls wake. `Close` takes no context: it waits for room in the tx ring, returns the error of sending `OpConnClose`, and closes the stream locally even if that fails. Requests already received on the stream are still answered by `Serve`, as far as the peer's stream exists, but the contexts of their handlers are canceled.
- After the peer closes a stream, `Recv` returns the messages still queued and then `ErrPeerClosed`; `Send` and `Call` return `ErrPeerClosed` (a `Call` that is waiting wakes). The first `Close` after a peer close returns nil, later ones `ErrClosed`.
- A closed link returns `ErrClosed` (the message of `ErrClosed` is `hqq: link closed` also for a stream), a dead peer `ErrPeerGone`, a graceful peer shutdown of the whole link `ErrPeerClosed`, and an ending context `ctx.Err()`. A closed stream gives `ErrClosed` (local close) or `ErrPeerClosed` (peer close), never `ErrConnNotFound`.

### Zero-allocation and buffer ownership

- **`Message`** (returned by `Recv` and `Call`): `Bytes()` is valid until `Release()`. Call `Release` at most once, and do not use the `Message` or a slice obtained from `Bytes()` afterwards. Calling `Release` is optional: without it the garbage collector reclaims the buffer and only the reuse is lost. `Bytes()` has no spare capacity, so `append` never overwrites the buffer. A `Message` must not be copied; `Release` on nil is a no-op.
- **`Request` and `ResponseWriter`** are pooled and valid only during `ServeHQQ`. Do not retain them or `req.Payload()` (which must not be modified), and do not use them from a goroutine the handler started. `Write` copies its argument, so the argument can be reused at once.
- **`Send`** copies `p` into the ring before it returns; `p` is not retained.
- **Zero allocations in steady state:** `Send`, `Recv` followed by `Release`, and `Call` followed by `Release` against a `Serve` handler that uses `Write`. The allocation tests cover single-chunk and multi-chunk messages, with and without a running dispatcher, and `Send`/`Recv` on stream 0 and on another stream. This holds when the context cannot be canceled (for example `context.Background()`); a cancelable context may allocate when a wait parks and arms the context wake-up. The pools are warmed up by the first operations, and idle pooled buffers can be dropped by garbage collections, so allocations reappear until the pools are refilled. Setting up a stream (`OpenStream`, `AcceptStream`) and error responses (`ResponseError` copies its payload) allocate.
- **Pools** belong to the link. Message buffers come in power-of-two size classes from 64 B up to `MaxMessageSize`; the largest class is exactly `MaxMessageSize`, so a pool never keeps a buffer larger than that. Reassembly bookkeeping and the channels of pending calls are pooled as well. A message under assembly holds one pooled buffer of its announced size rounded up to a power of two (at most `MaxMessageSize`); `MaxReassemblyBytes` is charged the announced size plus 256 bytes per message, so the memory actually held can exceed the charge, by up to 2× for the payload.

---

## Operational Guide

### Concurrency Contract

- **Goroutine-safe by default**: `Read`, `Write`, `ReadFrom`, `WriteTo`, `ReadZeroCopy`, `WriteZeroCopy`, `ReserveRead`, `ReserveWrite`, and every `AdvancedLink` and `Stream` method (`Send`, `Recv`, `Call`, `Serve`, `OpenStream`, `AcceptStream`, `Stream.Close`) are safe for concurrent use by multiple goroutines. `Serve` runs at most `BufferCount` handlers concurrently.
- **One receive path**: once `AdvancedLink` negotiation has started, the Advanced receive path owns the receive direction of the underlying rings. Use a `StandardLink` for raw `Read`/`Write`.
- **Safe `Close()`**: closing a `StandardLink`, `AdvancedLink`, `NamedStandardLink`, or `NamedAdvancedLink` wakes all parked readers/writers (they return `ErrClosed`) and waits for in-flight calls, including running zero-copy callbacks, before returning or unmapping shared memory. `Close` and `Unlink` on named links may be called concurrently and repeatedly; only the first `Close` unmaps. `AdvancedLink.Close` also stops the dispatcher and the control sender (queued control replies are discarded) and fails pending calls and `OpenStream`s with `ErrClosed`; `Recv` calls blocked on the link or on one of its streams return `ErrClosed`.
- **Not tracked by `Close`**: active reservations. A reservation's buffer is invalid once `Close` has run.
- **Graceful stop**: `AdvancedLink.Shutdown(ctx)` refuses new requests, waits until the accepted ones are answered (or `ctx` ends), and then makes `Serve` return nil. `Close` does not drain: it cancels the contexts of running handlers at once, so call `Shutdown` first for a graceful stop.

### Crash Recovery & Liveness Semantics

- **Graceful shutdown**: when a peer calls `Close()`, it writes a detached marker into the superblock and wakes parked waiters. Messages it published before closing are still delivered. After that, `StandardLink.Read` and `ReadFrom` return `io.EOF` (so `io.Copy`, `io.ReadAll`, and `bufio.Scanner` see a normal end of stream); `Write`, `ReadZeroCopy`, `ReserveRead`, `Send`, `Recv`, and `Call` return `ErrPeerClosed`, which matches `errors.Is(err, io.EOF)`; `Serve` returns nil. After `Close`, queued read tails are not served any more: reads return `ErrClosed`.
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

### Acceptor and `Connect` Behavior

- `NewAcceptor` (or `NewAcceptorContext`, which bounds the wait for a listener segment that another process is still initializing) creates the listener name and accepts clients until `Close`. If the name exists, it is reused when its acceptor closed or died (clients still waiting in the old queue fail with `ErrListenerGone`); a live acceptor makes the call fail with `ErrListenerBusy`; an object that is not a listener segment of this layout version fails with `ErrIncompatibleLayout` and is not modified.
- Each `Connect` creates the client's link segment, announces it in the listener's queue, and waits for the acceptor to attach to it as Secondary and negotiate. `Accept` returns the link when the handshake completes. A client that stalls or sends an invalid record delays neither other clients nor `Accept` callers.
- `Connect` retries like `Dial` while the listener does not exist yet, is still being initialized, or has no live acceptor, until `ctx` ends. Once its request is queued, it fails with `ErrListenerClosed` (the acceptor closed the listener), `ErrListenerGone` (the acceptor stopped heartbeating or was replaced), or an error matching `ErrTimeout` (the acceptor did not attach within `Config.Advanced.Link.AttachTimeout`). In each failure case the client's segment is removed.
- `Close` stops accepting, makes `Accept` return `ErrAcceptorClosed`, makes waiting clients fail with `ErrListenerClosed`, closes the links that completed their handshake but were not returned by `Accept`, and removes the listener name if it still refers to this acceptor. Links that `Accept` has returned stay open.
- `AcceptorConfig` defaults: `QueueSize` 64 (a power of two from 2 to 65536), `Backlog` 16, `MaxHandshakes` 16, `HandshakeTimeout` 5 s, `StuckSlotTimeout` 2 s (negative disables the recovery of queue slots that a client claimed and never published), `MaxBufferCount` 4096, `MaxBufferSize` 1 MiB, `MaxLinkSize` 256 MiB. `Connect` fails at once for a geometry that the acceptor would drop. `AcceptorStats` counts every record as `Accepted`, `Malformed`, `Unreachable`, or `HandshakeFailed`, plus `StuckSlots`.

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
  - `WaitAdaptive` (default): re-checks the ring in a short tight loop (256 iterations, about 6 µs), then yields with `runtime.Gosched` (256 iterations), then parks in the OS kernel for at most 100 ms per park. Best for almost all production deployments.
  - `WaitSpin`: pure user-space busy-spin with zero kernel transitions; every blocked call keeps one core busy. Use only when pinning dedicated cores for ultra-low tail latency.
- **`HeartbeatInterval` / `PeerTimeout`**: shorter values detect a crashed peer sooner (`PeerTimeout` is raised to at least four peer heartbeat intervals) at the cost of more superblock writes and a higher risk of false positives on a paused process.
- **`AttachTimeout`**: how long an endpoint waits for another endpoint that is initializing the segment, and how long an opener (`Open*`, `Dial`) waits for a creator to claim a segment that is still empty; after that the call fails with a transient error matching `ErrFailedInit` (`Dial` retries). It also bounds each ring attach.
- **`AdvancedOptions`**: `MaxMessageSize` and `MaxReassemblyBytes` bound memory held for incoming messages (a bound below `MaxMessageSize` refuses multi-chunk messages even when nothing else is in flight; single-chunk messages are not affected); `IncomingQueueSize` sets the capacity of every incoming queue (each stream, the request queue of `Serve`, the accept queue) and `IncomingOverflow` selects what happens when one is full.
- **`StreamListenGrace`** (default 2 s): how long a stream request (`OpenStream` of the peer) that arrives before `ListenStreams` is held. Raise it when a peer may open streams before the acceptor starts listening; a negative value refuses such requests at once.

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

# SQLite driver, launcher and secureexec (the e2e tests need a built C server)
go test -race -count=1 ./sqlite/... ./secureexec/...
HQQ_SQLITE_SERVER=/path/to/hqq-sqlite-server go test -count=1 ./sqlite/e2e/...

# hqq-bindgen: parser, emitters, runtime, end-to-end suite against the real C server
go test -race -count=1 ./bindgen/... ./cmd/hqq-bindgen
go test -tags zstdcgo ./bindgen/examples/zstd/...   # after: eval "$(bindgen/examples/zstd/build.sh)"

# TLA+ model checking of the MPMC ring (requires a Java runtime)
sh scripts/tlc.sh
JAVA=/path/to/bin/java sh scripts/tlc.sh   # if `java` on PATH is not a working runtime
```

- **`HQQ_PERF_TESTS=1`** enables the tests that compare CPU time or latency ratios (`TestIdleLinkDoesNotBurnCPU`, `TestMPMCIdleWaitDoesNotBurnCPU`, `TestMPMCWaitSpinBurnsCPU`). They are skipped otherwise because the ratios are only meaningful on a quiet machine; the benchmark workflow runs them.
- **Fuzz targets**: `FuzzPacketRoundTrip` (`internal/protocol`), `FuzzConsumeAdvancedChunk` and `FuzzOpenOverCorruptSuperblock` (root package).
- **TLC**: `scripts/tlc.sh` downloads `tla2tools.jar` if needed and checks `internal/mpmc/MPMC.tla` with `MPMC.cfg` (`BufferSize = 2`). It picks the Java runtime from `$JAVA`, then `$JAVA_HOME/bin/java`, then `java` on `PATH`; CI uses Temurin 21. The model covers the claim and completion protocol of the ring; it does not model parking, waiter counts or aggregation, `Format`, crashes, or stale copies of reservation handles.
- **CI-only checks**: GenMC and herd7 (`c/verify`, `memory-model` job of `c.yml`), stress litmus tests (Linux arm64), and the Windows jobs run in CI only. See [Other languages and interoperability](#other-languages-and-interoperability).
- **`HQQ_SQLITE_SERVER`** names a built `hqq-sqlite-server`. The end-to-end tests in `sqlite/e2e` and `sqlite/launcher` (the contract suite, the driver over a real server, the launcher, and the parent lifeline) skip without it. The other `sqlite` tests run against the Go fake server.
- **`HQQ_BINDGEN_MICROLIB_SERVER`**, **`HQQ_SQLITE3GEN_SERVER`**, **`HQQ_ZSTD_SERVER`** name prebuilt servers of the `hqq-bindgen` end-to-end suites. Without them the microlib and sqlite3 suites build their server with CMake and Ninja on first use (and skip with a reason when those are missing); the zstd suite skips its server tests and needs `bindgen/examples/zstd/build.sh` to fetch and build the pinned zstd source. `HQQ_BINDGEN_MICROLIB_SANITIZE=address+undefined` runs the microlib suite against a sanitized server.
