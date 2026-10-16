# Changelog

All notable changes to **HQQ** (`gosuda.org/hqq`) are documented in this file.

The project follows [Semantic Versioning](https://semver.org/). While in `v0.x`, minor versions may introduce breaking wire-layout improvements accompanied by migration notes below.

## [Unreleased] — v0.2.0 (Shared-Memory Layout v2 & Full Hardening)

### Highlights

- **Cross-Process Adaptive Parking (`internal/mpmc`)**: Replaced pure busy-spin loops with two-phase adaptive waiting (`WaitAdaptive`: brief spin + cross-process Linux `futex` / macOS `__ulock_wait` / bounded sleep backoff). Idle blocked reader CPU drops from ~165% to **~0.1%**.
- **Superblock v2 (`HQQ_STL2`) & Crash Detection**: Added per-endpoint session tokens, OS PID tracking, periodic heartbeats, graceful close markers (`ErrPeerClosed`), abrupt crash detection (`ErrPeerGone`), stale segment auto-reinitialization, initializer crash takeover, and 3rd-endpoint rejection (`ErrLinkBusy`).
- **Ergonomic High-Level APIs**: Added `hqq.Listen` / `hqq.Dial`, `AdvancedLink.Negotiate`, `AdvancedLink.Serve` + `hqq.Mux` (`Handler` / `HandlerFunc`), `OpenStandardLinkMemory`, `AlignedBuffer`, and standalone `hqq.Unlink`.
- **Zero `go vet` Warnings**: Reworked `internal/mpmc` and `internal/mmap` pointer arithmetic so plain `go vet ./...` (including `unsafeptr`) passes with zero warnings across Linux, macOS, and Windows.
- **Formal TLA+ Verification (`internal/mpmc/MPMC.tla`)**: Repaired the TLA+ specification, removed the broken post-module proof stub, and verified `NoRace` (writer + reader exclusivity), `DataIntegrity`, `FIFOBound`, `FIFOClaimOrder`, and `SystemProgress` with TLC (`scripts/tlc.sh`, 662,863 states, 0 errors).

### Added

- `hqq.Listen(ctx, name, cfg)` and `hqq.Dial(ctx, name, cfg)` for one-step named IPC setup and negotiation (`connect.go`).
- `hqq.Mux`, `hqq.Handler`, `hqq.HandlerFunc`, and `(*AdvancedLink).Serve(ctx, handler)` for ergonomic request-response RPC dispatch (`rpc.go`).
- `(*AdvancedLink).Negotiate(ctx, features)` to auto-select initiator/responder roles from `Link.Mode()`, plus simultaneous `NegotiateProtocol` collision resolution (`advanced.go`).
- `OpenStandardLinkMemory(mem []byte, bufferCount, bufferSize int)` and `AlignedBuffer(size uintptr)` for safe in-process or custom-mapped links (`standard.go`).
- `hqq.Unlink(name string)` and `shm.Unlink(name string)` to remove leftover shared-memory segments without mapping them (`named_link.go`, `internal/shm`).
- `LinkOptions` (`WaitPolicy`, `HeartbeatInterval`, `PeerTimeout`, `AttachTimeout`), `OverflowPolicy` (`OverflowBlock`, `OverflowDrop`, `OverflowFail`), and `PeerState` (`PeerNotConnected`, `PeerConnected`, `PeerClosed`, `PeerGone`) (`options.go`, `superblock.go`).
- Granular sentinel errors (`ErrClosed`, `ErrPeerClosed`, `ErrPeerGone`, `ErrLinkBusy`, `ErrIncompatibleLayout`, `ErrNotNegotiated`, `ErrAlreadyNegotiated`, `ErrNegotiationRejected`, `ErrUnexpectedPacket`, `ErrProtocol`, `ErrAdvancedActive`, `ErrNotListening`, `ErrAlreadyListening`, `ErrIncomingOverflow`, `ErrInvalidName`) with `Unwrap()` support for `io.EOF`, `net.ErrClosed`, `os.ErrDeadlineExceeded`, and legacy HQQ sentinels (`errors.go`).
- Multi-process crash/graceful-close/stale-reuse tests (`lifecycle_test.go`), fuzz targets (`fuzz_test.go`, `internal/protocol/fuzz_test.go`), runnable GoDoc examples (`example_test.go`), and Unix domain socket / OS pipe comparison benchmarks (`compare_unix_test.go`).

### Changed

- Split the monolithic 2,448-line `link.go` into focused modules: `errors.go`, `options.go`, `superblock.go`, `standard.go`, `reservation.go`, `packetconn.go`, `advanced.go`, `advanced_io.go`, `advanced_dispatch.go`, `advanced_stream.go`, `named_link.go`, `rpc.go`, and `connect.go`.
- Consolidated 12 duplicated MPMC ring claim loops into unified `claimProducer` and `claimConsumer` paths with 64-byte slot alignment (`slotAlign = 64`) and a 512-byte cache-line-isolated header (`internal/mpmc/mpmc.go`).
- Fixed `SizeMPMCRing` double-counting of 256 header bytes (`internal/mpmc/mpmc.go`).
- Changed Windows default shared-memory namespace from `Global\` (which requires elevated `SeCreateGlobalPrivilege`) to `Local\`, while preserving explicit `Global\` prefix support (`internal/shm/shm_windows.go`).
- Replaced fragile `//go:linkname` bindings to Go runtime internal `internal/race` symbols with self-contained `//go:norace` memory helpers (`internal/race`).

### Fixed

- Fixed data races in `StandardLink` (`readDeadline`, `writeDeadline`, `remoteAddr`, `recvRemainder`, and `Close` vs in-flight ring operations).
- Fixed `NamedStandardLink.Close` and `NamedAdvancedLink.Close` unmapping shared memory before draining in-flight readers/writers (`SIGSEGV` hazard).
- Fixed `SIGBUS` hazard when opening a named shared memory segment before the creator finished `ftruncate` (`internal/shm`).
- Fixed shared-memory name traversal (`../`, slashes, NUL, and macOS 31-byte `PSHMNAMLEN` validation) and added `O_CLOEXEC` (`internal/shm`).
- Fixed `AdvancedLink` dispatcher failing permanently when a late response arrives for a context-canceled `Call` (`advanced_dispatch.go`).
- Fixed `StandardLink.Read` / `ReadZeroCopy` / `ReserveRead` silently corrupting state if called on `OpConnCopy` frames (`standard.go`, `reservation.go`).
