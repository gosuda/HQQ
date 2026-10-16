// Package hqq implements high-throughput, low-latency inter-process
// communication (IPC) over shared memory using lock-free Vyukov-style MPMC
// ring buffers and slot-owned payload buffers.
//
// # Architecture
//
// An HQQ link splits a page-aligned shared-memory segment into a first-page
// superblock (magic "HQQ_STL3", layout version 3.0) followed by two
// independent directional channels (Primary → Secondary and Secondary →
// Primary). Each direction pairs a lock-free packet ring with a fixed array of
// payload buffers where ring slot i owns payload buffer i. Because the buffer
// is released only after the receiver finishes reading or releases its slot
// reservation, producers cannot overwrite unread payloads without an extra
// allocator or lock. Segments of another layout version (for example
// "HQQ_STL2") are rejected with [ErrIncompatibleLayout]. The wire and memory
// layout is specified in PROTOCOL.md.
//
// Two protocol layers share the same memory layout:
//
//   - Standard Protocol ([StandardLink], [NamedStandardLink]): fixed-capacity
//     single-buffer messages up to bufferSize bytes with [StandardLink.Read],
//     [StandardLink.Write], zero-copy callbacks ([StandardLink.ReadZeroCopy],
//     [StandardLink.WriteZeroCopy]), explicit slot lifetime reservations
//     ([StandardLink.ReserveRead], [StandardLink.ReserveWrite]), and
//     [net.PacketConn] support with deadlines ([StandardLink.ReadFrom] has
//     datagram semantics, [StandardLink.WriteTo] ignores its address).
//   - Advanced Protocol ([AdvancedLink], [NamedAdvancedLink]): an optional
//     negotiated overlay supporting multi-chunk large payload transfers
//     ([AdvancedLink.SendLarge], [AdvancedLink.Receive]), concurrent
//     request-response RPC ([AdvancedLink.Call], [AdvancedLink.CallOn],
//     [AdvancedLink.Respond], [AdvancedLink.Serve], [Mux]), and logical
//     stream multiplexing ([AdvancedLink.ListenStreams],
//     [AdvancedLink.AcceptStream], [AdvancedLink.OpenStream],
//     [AdvancedLink.CloseStream]). A failed negotiation is terminal: later
//     attempts return [ErrNegotiationFailed].
//
// # Quick Start
//
// For most inter-process RPC or large-message workloads, use [Listen] on the
// primary process and [Dial] on the secondary process:
//
//	// Primary process (creates or reuses the shared memory segment)
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
// For in-process testing or custom memory mappings, pass page-aligned memory
// (for example from [AlignedBuffer]) to [NewStandardLink] or [NewAdvancedLink].
// For named segments without negotiation use [CreateStandardLink] and
// [OpenStandardLink] (or the Advanced variants).
//
// # Concurrency Contract
//
//   - [StandardLink.Write], [StandardLink.WriteZeroCopy], and
//     [StandardLink.ReserveWrite] are safe for concurrent use by multiple
//     goroutines.
//   - [StandardLink.Read], [StandardLink.ReadFrom],
//     [StandardLink.ReadZeroCopy], and [StandardLink.ReserveRead] are safe for
//     concurrent use by multiple goroutines. One read call returns bytes of at
//     most one message; the unread tail of a partially read message is queued
//     (first in, first out) and served, one tail per call, before newer
//     messages.
//   - Once negotiation of an [AdvancedLink] has started, the Advanced
//     dispatcher owns the receive direction. Direct calls to
//     [AdvancedLink.Read] or [AdvancedLink.Write] return [ErrAdvancedActive].
//   - [AdvancedLink.Call], [AdvancedLink.CallOn], [AdvancedLink.SendLarge],
//     [AdvancedLink.Respond], and [AdvancedLink.Receive] are safe for
//     concurrent use across goroutines. The receive path never waits for the
//     tx ring: stream accepts and refusals and refusal responses go through a
//     bounded control-sender queue and are dropped (and counted in
//     [AdvancedStats].DroppedMessages) when it is full, so always pass a
//     context with a deadline to [AdvancedLink.OpenStream] and calls.
//   - [StandardLink.Close] and [NamedStandardLink.Close] wake all blocked
//     readers and writers and wait for in-flight ring operations, including
//     running zero-copy callbacks, to drain before returning (and before
//     unmapping shared memory). Close must not be called from inside a
//     zero-copy callback, and reservations must be finished before Close: it
//     does not track them. Close and Unlink on named links may be called
//     concurrently.
//
// # Crash Detection and Lifecycle
//
// Each attached endpoint records a random session token and a periodic
// heartbeat timestamp in the shared superblock:
//
//   - Graceful peer shutdown sets a detached marker and wakes parked waiters.
//     Messages published before the close are still delivered; after them
//     [StandardLink.Read] and [StandardLink.ReadFrom] return [io.EOF] itself,
//     and the other blocking operations return [ErrPeerClosed] (which matches
//     [io.EOF]).
//   - An abrupt peer crash or hang (`SIGKILL`, process exit without `Close`)
//     is detected when the peer's heartbeat stalls for
//     [LinkOptions.PeerTimeout], returning [ErrPeerGone] (which matches
//     [ErrConnNotFound]). The recorded PID is diagnostic only.
//   - `Create*Link` and [Listen] create the name exclusively. If the name
//     exists they reuse the segment when both of its endpoints closed or died
//     (the caller becomes Primary and owns the name), fail with [ErrLinkBusy]
//     if an endpoint is alive, and fail with [ErrIncompatibleLayout] without
//     touching an object that is not a segment of this layout version.
//     `Open*Link` and [Dial] only ever attach as Secondary to a live segment;
//     they never initialize, re-initialize, or take over a segment. On an
//     empty or initializing segment they wait up to
//     [LinkOptions.AttachTimeout] and then fail with a transient error that
//     matches [ErrFailedInit], and on a segment whose endpoints are all gone
//     they fail immediately with such an error; [Dial] retries on these (until
//     a new [Listen] reuses the name) and while the name is missing, and
//     returns ctx.Err() as soon as its context is done. A third endpoint gets
//     [ErrLinkBusy]. A creator (or an endpoint on caller-owned memory) takes
//     over an initializer that made no progress for max(PeerTimeout, 2s).
//   - Close and Unlink remove a name only while it still refers to this
//     endpoint's own segment instance. Standalone [Unlink] removes a named
//     shared-memory object without opening or mapping it.
//
// # Security Model
//
// Shared-memory IPC assumes mutually trusted peers (or a sandboxing boundary
// where the host validates every incoming frame). Because both endpoints map
// the same physical pages with read-write permissions, a malicious peer with
// write access to the segment can corrupt ring sequence numbers or superblock
// metadata. Restrict segment permissions via the `perm` argument (for example
// `0600`) when creating named links. On Linux, names are opened with
// O_NOFOLLOW and must be regular files; anything else is [ErrInvalidName].
// A first page that is not entirely zero is never overwritten
// ([ErrIncompatibleLayout]). [AdvancedOptions].MaxMessageSize bounds one
// reassembled message and [AdvancedOptions].MaxReassemblyBytes bounds the
// memory held by all incomplete messages together.
//
// # Platform Notes
//
//   - Linux: Named shared memory is backed by `/dev/shm/<name>` (255-byte
//     names), allocated with fallocate so a full tmpfs fails at creation, and
//     adaptive waiting uses cross-process `futex` (`FUTEX_WAIT` /
//     `FUTEX_WAKE`).
//   - macOS (Darwin): POSIX `shm_open` names are limited by XNU `PSHMNAMLEN` to
//     31 bytes including the leading `/`. Adaptive waiting uses cross-process
//     `__ulock_wait` / `__ulock_wake` (`UL_COMPARE_AND_WAIT_SHARED`), a private
//     Darwin interface.
//   - Windows: Named shared memory uses `Local\<name>` by default so standard
//     unprivileged user accounts do not need `SeCreateGlobalPrivilege` (pass an
//     explicit `Global\<name>` prefix for cross-session sharing). `perm`
//     becomes a DACL (SYSTEM, the creating user for the owner bits, Everyone
//     for the other bits). Named segments park cross-process waiters on four
//     named kernel semaphores (`<name>.hqq-sem0`…`3`); unnamed memory uses
//     `WaitOnAddress`, which wakes only waiters of the same process
//     immediately. [Unlink] is a no-op: the mapping disappears with its last
//     handle.
//   - Waiting is aggregated per process: all goroutines blocked on the same
//     ring side share one kernel park, so the waiter counts in the shared
//     header count parked processes, not goroutines.
//   - Build constraints: GOARCH=mips, mipsle, and arm with GOARM below 7 fail
//     to build, because their 64-bit atomics are not atomic across processes.
package hqq
