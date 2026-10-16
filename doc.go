// Package hqq implements high-throughput, low-latency inter-process
// communication (IPC) over shared memory using lock-free Vyukov-style MPMC
// ring buffers and slot-owned payload buffers.
//
// # Architecture
//
// An HQQ link splits a page-aligned shared-memory segment into a first-page
// superblock followed by two independent directional channels (Primary →
// Secondary and Secondary → Primary). Each direction pairs a lock-free packet
// ring with a fixed array of payload buffers where ring slot i owns payload
// buffer i. Because the buffer is released only after the receiver finishes
// reading or releases its slot reservation, producers cannot overwrite unread
// payloads without an extra allocator or lock.
//
// Two protocol layers share the same memory layout:
//
//   - Standard Protocol ([StandardLink], [NamedStandardLink]): fixed-capacity
//     single-buffer messages up to bufferSize bytes with [StandardLink.Read],
//     [StandardLink.Write], zero-copy callbacks ([StandardLink.ReadZeroCopy],
//     [StandardLink.WriteZeroCopy]), explicit slot lifetime reservations
//     ([StandardLink.ReserveRead], [StandardLink.ReserveWrite]), and
//     [net.PacketConn] deadline support.
//   - Advanced Protocol ([AdvancedLink], [NamedAdvancedLink]): an optional
//     negotiated overlay supporting multi-chunk large payload transfers
//     ([AdvancedLink.SendLarge], [AdvancedLink.Receive]), concurrent
//     request-response RPC ([AdvancedLink.Call], [AdvancedLink.CallOn],
//     [AdvancedLink.Respond], [AdvancedLink.Serve], [Mux]), and logical
//     stream multiplexing ([AdvancedLink.Listen], [AdvancedLink.Accept],
//     [AdvancedLink.Dial]).
//
// # Quick Start
//
// For most inter-process RPC or large-message workloads, use [Listen] on the
// primary process and [Dial] on the secondary process:
//
//	// Primary process (creates the shared memory segment)
//	server, err := hqq.Listen(ctx, "my-ipc", hqq.Config{})
//	if err != nil {
//	    log.Fatal(err)
//	}
//	defer server.Close()
//
//	// Secondary process (attaches to the segment)
//	client, err := hqq.Dial(ctx, "my-ipc", hqq.Config{})
//	if err != nil {
//	    log.Fatal(err)
//	}
//	defer client.Close()
//
// For in-process testing or custom memory mappings, use [OpenStandardLinkMemory]
// or [AlignedBuffer] with [OpenStandardLink] / [NewAdvancedLink].
//
// # Concurrency Contract
//
//   - [StandardLink.Write], [StandardLink.WriteZeroCopy], and
//     [StandardLink.ReserveWrite] are safe for concurrent use by multiple
//     goroutines.
//   - [StandardLink.Read], [StandardLink.ReadZeroCopy], and
//     [StandardLink.ReserveRead] are safe for concurrent use by multiple
//     goroutines; partial-read remainders inside [StandardLink.Read] are
//     serialized by an internal mutex.
//   - Once an [AdvancedLink] completes protocol negotiation, the Advanced
//     dispatcher owns the underlying Standard ring in that direction. Direct
//     calls to [StandardLink.Read] or [StandardLink.Write] on a negotiated
//     [AdvancedLink] return [ErrAdvancedActive].
//   - [AdvancedLink.Call], [AdvancedLink.CallOn], [AdvancedLink.SendLarge],
//     [AdvancedLink.Respond], and [AdvancedLink.Receive] are safe for
//     concurrent use across goroutines.
//   - [StandardLink.Close] and [NamedStandardLink.Close] wake all blocked
//     readers and writers and wait for in-flight ring operations to drain
//     before returning (and before unmapping shared memory).
//
// # Crash Detection and Lifecycle
//
// Each attached endpoint records a monotonic session token, OS process ID, and
// periodic heartbeat timestamp in the shared superblock:
//
//   - Graceful peer shutdown sets a detached marker and wakes parked waiters,
//     causing blocked reads and writes to return [ErrPeerClosed] (which wraps
//     [io.EOF] and [net.ErrClosed]).
//   - An abrupt peer crash (`SIGKILL` or process exit without `Close`) is
//     detected via OS PID liveness probes and heartbeat expiration, returning
//     [ErrPeerGone] (which also wraps [io.EOF] and [net.ErrClosed]).
//   - Calling `Create*Link` on a leftover shared-memory segment whose previous
//     owners have exited automatically re-initializes the segment and bumps
//     its superblock epoch; if a live owner is still attached, creation fails
//     with [ErrLinkBusy].
//   - Standalone [Unlink] removes a named shared-memory object without opening
//     or mapping it.
//
// # Security Model
//
// Shared-memory IPC assumes mutually trusted peers (or a sandboxing boundary
// where the host validates every incoming frame). Because both endpoints map
// the same physical pages with read-write permissions, a malicious peer with
// write access to the segment can corrupt ring sequence numbers or superblock
// metadata. Restrict segment permissions via the `mode` argument (for example
// `0600`) when creating named links, and set [AdvancedOptions.MaxMessageSize]
// to bound reassembly allocations.
//
// # Platform Notes
//
//   - Linux: Named shared memory is backed by `/dev/shm/<name>`, and adaptive
//     waiting uses cross-process `futex` (`FUTEX_WAIT` / `FUTEX_WAKE`).
//   - macOS (Darwin): POSIX `shm_open` names are limited by XNU `PSHMNAMLEN` to
//     31 bytes including the leading `/`. Adaptive waiting uses cross-process
//     `__ulock_wait` / `__ulock_wake` (`UL_COMPARE_AND_WAIT_SHARED`).
//   - Windows: Named shared memory uses `Local\<name>` by default so standard
//     unprivileged user accounts do not need `SeCreateGlobalPrivilege`
//     (pass an explicit `Global\<name>` prefix for cross-session sharing).
//     Cross-process waiting uses bounded exponential sleep backoff.
package hqq
