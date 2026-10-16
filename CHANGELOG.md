# Changelog

All notable changes to **HQQ** (`gosuda.org/hqq`) are documented in this file.

The project follows [Semantic Versioning](https://semver.org/). While in `v0.x`, minor versions may introduce breaking wire-layout improvements accompanied by migration notes below.

## [Unreleased] — v0.2.0 (Shared-Memory Layout v2 & Full Hardening)

### Highlights

- **Cross-Process Adaptive Parking (`internal/mpmc`)**: Replaced pure busy-spin loops with adaptive waiting (`WaitAdaptive`: tight re-check, `runtime.Gosched`, then cross-process Linux `futex` / macOS `__ulock_wait` / Windows named semaphores for named segments / bounded backoff elsewhere). Idle blocked reader CPU drops from ~165% to **~0.1%**.
- **Superblock v2 (`HQQ_STL2`) & Crash Detection**: Added per-endpoint session tokens, periodic heartbeats (the recorded PID is diagnostic only), graceful close markers (`ErrPeerClosed`), abrupt crash detection (`ErrPeerGone`), reuse of segments whose endpoints all closed or died by `OpenNamed*` / `Dial`, initializer crash takeover, and 3rd-endpoint rejection (`ErrLinkBusy`).
- **Ergonomic High-Level APIs**: Added `hqq.Listen` / `hqq.Dial`, `AdvancedLink.Negotiate`, `AdvancedLink.Serve` + `hqq.Mux` (`Handler` / `HandlerFunc`), `OpenStandardLinkMemory`, `AlignedBuffer`, and standalone `hqq.Unlink`.
- **Zero `go vet` Warnings**: Reworked `internal/mpmc` and `internal/mmap` pointer arithmetic so plain `go vet ./...` (including `unsafeptr`) passes with zero warnings across Linux, macOS, and Windows.
- **TLA+ Model Checking (`internal/mpmc/MPMC.tla`)**: Repaired the TLA+ specification, removed the broken post-module proof stub, and model-checked `NoRace` (writer + reader exclusivity), `DataIntegrity`, `FIFOBound`, `FIFOClaimOrder`, and `SystemProgress` with TLC on a bounded model (`scripts/tlc.sh`, 662,863 states, 0 errors). This is a bounded check, not a proof; parking, `Format`, and crashes are not modeled.

### Added

- `hqq.Listen(ctx, name, cfg)` and `hqq.Dial(ctx, name, cfg)` for one-step named IPC setup and negotiation (`connect.go`).
- `hqq.Mux`, `hqq.Handler`, `hqq.HandlerFunc`, and `(*AdvancedLink).Serve(ctx, handler)` for ergonomic request-response RPC dispatch (`rpc.go`).
- `(*AdvancedLink).Negotiate(ctx, features)` to auto-select initiator/responder roles from `Link.Mode()`, plus simultaneous `NegotiateProtocol` collision resolution (`advanced.go`).
- `OpenStandardLinkMemory(mem []byte, bufferCount, bufferSize int, opts LinkOptions)`, `NewAdvancedLinkMemory(mem, bufferCount, bufferSize, opts AdvancedOptions)`, and `AlignedBuffer(size uintptr) []byte` for safe in-process or custom-mapped links (`standard.go`, `advanced.go`).
- `hqq.Unlink(name string)` and `shm.Unlink(name string)` to remove leftover shared-memory segments without mapping them (`named_link.go`, `internal/shm`).
- `LinkOptions` (`WaitPolicy`, `HeartbeatInterval`, `PeerTimeout`, `AttachTimeout`), `OverflowPolicy` (`OverflowBlock`, `OverflowDrop`, `OverflowFail`), and `PeerState` (`PeerNotConnected`, `PeerConnected`, `PeerClosed`, `PeerGone`) (`options.go`, `superblock.go`).
- `AdvancedOptions.MaxReassemblyBytes` (default `4 × MaxMessageSize`): bounds the memory held by all incomplete multi-chunk messages; a message that would exceed it is refused without allocating (`advanced.go`, `advanced_io.go`).
- Granular sentinel errors (`ErrClosed`, `ErrPeerClosed`, `ErrPeerGone`, `ErrLinkBusy`, `ErrIncompatibleLayout`, `ErrNotNegotiated`, `ErrAlreadyNegotiated`, `ErrNegotiationRejected`, `ErrUnexpectedPacket`, `ErrProtocol`, `ErrMessageAborted`, `ErrAdvancedActive`, `ErrNotListening`, `ErrAlreadyListening`, `ErrIncomingOverflow`, `ErrReassemblyLimit`, `ErrInvalidName`), each wrapping one base (`net.ErrClosed`, `io.EOF`, `os.ErrDeadlineExceeded`, or a legacy HQQ sentinel) for `errors.Is` (`errors.go`; see the PROTOCOL.md error table).
- Windows: cross-process parking for named segments through four named kernel semaphores per segment (`<object name>.hqq-sem0`…`3`), `WaitOnAddress` parking for unnamed memory, `mode` translated to a DACL for the mapping and its semaphores, and `CurrentSize` via `NtQuerySection` so a size mismatch reports `ErrInvalidSize` (`internal/mpmc`, `internal/shm`).
- Multi-process crash/graceful-close/stale-reuse tests (`lifecycle_test.go`), fuzz targets (`fuzz_test.go`, `internal/protocol/fuzz_test.go`), runnable GoDoc examples (`example_test.go`), and Unix domain socket / OS pipe comparison benchmarks (`compare_unix_test.go`).

### Changed

- Split the monolithic 2,448-line `link.go` into focused modules: `errors.go`, `options.go`, `superblock.go`, `standard.go`, `reservation.go`, `packetconn.go`, `advanced.go`, `advanced_io.go`, `advanced_dispatch.go`, `advanced_stream.go`, `named_link.go`, `rpc.go`, and `connect.go`.
- Consolidated 12 duplicated MPMC ring claim loops into unified `claimProducer` and `claimConsumer` paths with 64-byte slot alignment (`slotAlign = 64`) and a 512-byte cache-line-isolated header (`internal/mpmc/mpmc.go`).
- Fixed `SizeMPMCRing` double-counting of 256 header bytes (`internal/mpmc/mpmc.go`).
- Changed Windows default shared-memory namespace from `Global\` (which requires elevated `SeCreateGlobalPrivilege`) to `Local\`, while preserving explicit `Global\` prefix support (`internal/shm/shm_windows.go`).
- Replaced fragile `//go:linkname` bindings to Go runtime internal `internal/race` symbols with self-contained `//go:norace` memory helpers (`internal/race`).
- `StandardLink.Read` returns `io.EOF` itself (instead of `ErrPeerClosed`) after a graceful peer close, as the `io.Reader` contract requires; `io.Copy`, `io.ReadAll`, and `bufio.Scanner` now treat it as a normal end of stream (`standard.go`).
- `AdvancedLink.Dial` allocates even connection IDs on Primary and odd IDs on Secondary (as specified); a peer `OpConnCreate` with ID `0`, the wrong parity, or an ID in use is refused with `ErrCodeInvalidOp` instead of overwriting the existing connection (`advanced_stream.go`).
- Aborted Advanced messages are no longer delivered: `Receive`, `Serve`, and `MessageHandler` drop aborted requests and data messages, and an aborted response fails `Call`/`CallOn` with `ErrMessageAborted` instead of returning an empty success (`advanced_io.go`, `rpc.go`).
- Ring attach failures on a `Ready` segment report `ErrIncompatibleLayout` instead of a bare `ErrFailedInit` (`standard.go`).
- Windows shared-memory names are limited so that the mapping name plus the 9-character semaphore suffix fits 259 UTF-16 units (`ErrInvalidName` otherwise).

### Fixed

- Fixed data races in `StandardLink` (`readDeadline`, `writeDeadline`, `remoteAddr`, `recvRemainder`, and `Close` vs in-flight ring operations).
- Fixed `NamedStandardLink.Close` and `NamedAdvancedLink.Close` unmapping shared memory before draining in-flight readers/writers (`SIGSEGV` hazard).
- Fixed `SIGBUS` hazard when opening a named shared memory segment before the creator finished `ftruncate` (`internal/shm`).
- Fixed shared-memory name traversal (`../`, slashes, NUL, and macOS 31-byte `PSHMNAMLEN` validation) and added `O_CLOEXEC` (`internal/shm`).
- Fixed `AdvancedLink` dispatcher failing permanently when a late response arrives for a context-canceled `Call` (`advanced_dispatch.go`).
- Fixed `StandardLink.Read` / `ReadZeroCopy` / `ReserveRead` silently corrupting state if called on `OpConnCopy` frames (`standard.go`, `reservation.go`).
- Fixed `Dial` retrying forever on permanent `ErrFailedInit` variants such as `ErrIncompatibleLayout`; it now retries only while the name is missing or the listener is still sizing/initializing the segment (`connect.go`).
- Fixed a data race (double unmap / double handle close) when `Close` or `Unlink` was called concurrently on a `NamedStandardLink` / `NamedAdvancedLink` (`named_link.go`).
- Fixed a race that could recycle a `Call` result channel still holding a stale result into the global pool, delivering one call's error to an unrelated call (`advanced_io.go`, `advanced_dispatch.go`).
- Fixed unbounded growth of incomplete reassembly buffers when a sender's abort frame was never delivered (`advanced_io.go`).
- Fixed a context wake-up callback that could touch a ring's wait word after the operation returned and the mapping was unmapped (`internal/mpmc`).
- Fixed a panicking zero-copy callback wedging the ring permanently: `WriteZeroCopy` publishes a tombstone, `ReadZeroCopy` and the mpmc dequeue release the slot, and the mpmc enqueue publishes a zeroed element before the panic propagates (`standard.go`, `internal/mpmc`).
- Fixed the CI fuzz step, which named non-existent fuzz targets and therefore fuzzed nothing (`.github/workflows/ci.yml`).
