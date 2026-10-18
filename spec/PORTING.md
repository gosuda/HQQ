# Porting HQQ to a New Language

This guide provides a step-by-step roadmap for implementing the HQQ shared-memory protocol in a new language from [`SPEC.md`](SPEC.md), [`MEMORY_MODEL.md`](MEMORY_MODEL.md), and [`constants.json`](constants.json). It applies to C (`libhqq` in [`c/`](../c)), C++, Rust, and any systems language capable of mapping shared memory and issuing hardware lock-free 32-bit and 64-bit atomic operations.

**Rule Zero**: The byte layout in shared memory and the memory ordering of atomic operations are the entire cross-process contract; API shape, concurrency primitives, memory allocation, and error types belong to the host language. Rather than translating Go types line by line, implement the state machines in `SPEC.md` sections 2 through 11 and verify against the golden vectors in [`conformance/`](../conformance/README.md).

## 1. Incremental Conformance Levels

A port can be brought up and tested against the Go reference implementation in five incremental levels:

| Level | Name | Scope Implemented | `SPEC.md` Sections | Interoperability Milestone |
| --- | --- | --- | --- | --- |
| **L0** | Layout | Region offset computation, superblock/endpoint/ring/slot/packet structs with compile-time offset assertions, constants, and offline golden vectors. | 2, 3, 7.1, 9.1, 9.2, `constants.json` | Parses and validates any segment snapshot in `conformance/vectors/`. |
| **L1** | Standard Opener (spin mode) | Open an existing named segment as **Secondary**, attach to rings, `Read`/`Write` Standard `OpCopy` packets, spin+sleep waiting, heartbeat and peer monitor, and graceful detach on close. | 4 (Opener), 5.1 (`Ready` branch), 5.4, 5.5, 6, 7.3, 7.4, 9, 11 | Exchanges messages bidirectionally with a Go **Primary** endpoint. |
| **L2** | Standard Creator / Primary | Create and reuse named segments, full claim loop, ring formatting, buffer zeroing, superblock publication, stalled-initializer takeover, dead-session reuse, and safe unlink. | 4, 5 (all), 7.2, 11 | Acts as **Primary** or **Secondary** with full crash-recovery parity. |
| **L3** | OS Parking | Dekker waiter and waker handshake over Linux `futex`, macOS `__ulock_wait` / `os_sync_wait_on_address`, or Windows named semaphores. | 8 | Zero-idle-CPU blocking waits with sub-millisecond cross-process wakeups. |
| **L4** | Advanced Protocol | Version/feature negotiation (`1.1`), multiplexed streams, chunked transfers, reassembly budgets, RPC request/response, cancellation, metadata, and graceful shutdown. | 10, `PROTOCOL_1_1_DRAFT.md` | Full protocol feature parity across all `X-A01..X-A10` interop scenarios. |

The smallest practical subset for a client-only port is **L1 plus the 3-line waker duty of L3 (`K1`, `K2`, `K3`)**, which allows the port to attach to a Go or C server and wake parked peers immediately without needing OS-specific wait syscalls on its own receive path.

### 1.1 Simple Mode (Spin + Exponential Backoff Without Kernel Parking)

A port operating in **simple mode** (`WaitPolicySpin`) spins briefly and then sleeps with exponential backoff without ever parking in the kernel. Simple mode is a fully conforming implementation of `SPEC.md` section 8 because kernel parking is an optimization, not a correctness requirement:

- **Simple-mode waiter**: Never modifies `emptyWaiters`, `fullWaiters`, `notEmpty`, or `notFull`. When a ring is empty or full, it polls `seq[s]` in a bounded loop: spin for ~4-6 us (256 iterations of 4 slot reads), then yield the processor (`sched_yield` / `SwitchToThread`) for ~256 iterations, then sleep with exponential backoff doubling from `20 us` up to `2 ms`.
- **Simple-mode waker**: Even if a simple-mode waker performs nothing after `seq[s].store(pos + 1, SC)`, a parked Go or C peer will wake on its bounded 100 ms park timeout and consume the message. However, executing the waker check (`K1`, `K2`, and `K3` when `waiters != 0`) costs only one `seq_cst` load on the fast path and gives parked peers instant wakeups.
- **Mandatory park timeout**: Any implementation that does park in the kernel `MUST` bound every wait call (at most `100 ms` per park iteration) so any missed wakeup degrades to a bounded latency hiccup rather than a deadlock.

## 2. Step-by-Step Implementation Order

1. **Pin layout constants and static assertions**: Generate or define the constants from `spec/constants.json` and add compile-time assertions (`_Static_assert`, `static_assert`, or `const _: () = assert!(...)`) verifying `sizeof(packet) == 64`, ring header size `512`, slot stride `128`, superblock size `384`, `ATOMIC_LLONG_LOCK_FREE == 2`, and little-endian byte order.
2. **Map named shared memory (`SPEC 11`)**:
   - Linux: open `/dev/shm/<name>` with `O_NOFOLLOW|O_CLOEXEC`, check `fstat` for a regular file and `st_size >= totalSize` before calling `mmap(..., MAP_SHARED)`.
   - macOS / FreeBSD: call `shm_open("/<name>")` (enforcing the 30-byte name limit on macOS) and `mmap(..., MAP_SHARED)`.
   - Windows: open or create the four named semaphores (`.hqq-sem0..3`) and the file mapping (`Local\hqq_v3_<name>`) and map with `MapViewOfFile`.
3. **Compute page-aligned regions (`SPEC 2.1`)**: Query the host page size at runtime (`sysconf(_SC_PAGESIZE)` or `GetSystemInfo`) and verify region offsets against vector group `G-03` for 4 KiB, 16 KiB, and 64 KiB page sizes.
4. **Implement the Opener superblock attach (`L1`)**: Using atomic loads, implement the Opener `Ready` branch (`SPEC 5.1`): validate `magic`, `major`, `requiredFeatures`, `pageSize`, `bufferCount`, `bufferSize`, `layoutSize`, and Primary liveness (`SPEC 6.2`), then claim `endpoints[1].token` via `CAS(0 -> myToken)`.
5. **Attach both directional rings (`SPEC 7.3`)**: Validate `magic`, `flag == 2`, `stride == 128`, `version == 2`, and `size == bufferCount`. On any mismatch, detach (`CAS(token, myToken -> 1)`) and return `ErrIncompatibleLayout` or `ErrInvalidSize`.
6. **Implement the ring slot protocol (`SPEC 7.4`, `MEMORY_MODEL 3`)**: Implement producer claim, payload copy, `seq_cst` publish (`pos + 1`), consumer claim, payload read, and `seq_cst` release (`pos + mask + 1`). Verify against vector group `G-07` (including wraparound at `2^64 - 2`).
7. **Implement Standard `Read` and `Write` (`SPEC 9`)**: Classify incoming packets by opcode (`OpCopy` vs `OpError`), always derive the payload buffer index from `pos & mask`, release the slot before returning, and implement the post-close drain rule (`SPEC 6.4`).
8. **Add heartbeats and peer monitoring (`SPEC 6.3`)**: Refresh `endpoints[self].heartbeat` every `HeartbeatInterval` (guarded by `token == myToken`), monitor the peer's `token`, `heartbeat`, and `epoch`, and detach cleanly (`CAS(token, myToken -> 1)`) on `Close`.
9. **Add Creator / Primary initialization (`L2`)**: Implement the full claim loop (`Empty`, `Initializing` with stale takeover, and `Ready` reuse when both endpoints are dead), `Format`, payload zeroing with per-slice `initToken` checks, ordered publication (`SPEC 5.3`), and safe unlink (`SPEC 11.4`).
10. **Add OS parking (`L3`)**: Implement the Dekker waiter (`W1..W4`) and waker (`K1..K3`) sequences (`SPEC 8.2, 8.3`) and verify with the Dekker stress test on both `x86_64` and `arm64`.
11. **Add the Advanced protocol (`L4`)**: Implement `OpProtoNegotiate` / `OpProtoAck`, `OpConnCopy` chunking and reassembly budgets, RPC `Call` / `Serve`, streams (`OpConnCreate` / `OpConnClose`), and Protocol 1.1 (`OpCancel`, `AdvFlagMeta`, and `ErrCodeShuttingDown`).
12. **Wire into the interop matrix**: Build an `hqq-interop` helper binary ([`conformance/INTEROP_CLI.md`](../conformance/INTEROP_CLI.md)) and run `go test ./interop/...` against the Go and C helpers.

## 3. Atomic Memory Ordering Quick Reference

The table below gives the exact C11, C++, and Rust spellings for every shared-memory operation (see [`MEMORY_MODEL.md`](MEMORY_MODEL.md) section 2 for full proofs). **When in doubt on cold-path control words, use `SeqCst`.**

| Operation (`SPEC.md` Reference) | C11 `<stdatomic.h>` | C++ `std::atomic` | Rust `Ordering` |
| --- | --- | --- | --- |
| Producer / consumer claim: load `seq[s]` (`7.4`) | `memory_order_acquire` | `memory_order_acquire` | `Acquire` |
| Slot publish / release: store `seq[s]` (`K1`) | `memory_order_seq_cst` | `memory_order_seq_cst` | `SeqCst` |
| Waker: load `emptyWaiters` / `fullWaiters` (`K2`) | `memory_order_seq_cst` | `memory_order_seq_cst` | `SeqCst` |
| Waker: `fetch_add(1)` on `notEmpty` / `notFull` (`K3`) | `memory_order_release` (or `seq_cst`) | `memory_order_release` | `Release` |
| Waiter: `fetch_add(1)` on waiter count (`W1`) | `memory_order_seq_cst` | `memory_order_seq_cst` | `SeqCst` |
| Waiter: snapshot `notEmpty` / `notFull` (`W2`) | `memory_order_acquire` (or `seq_cst`) | `memory_order_acquire` | `Acquire` |
| Waiter: re-check `seq[s]` before sleeping (`W3`) | `memory_order_seq_cst` | `memory_order_seq_cst` | `SeqCst` |
| Waiter: `fetch_sub(1)` on waiter count (`W4`) | `memory_order_release` (or `relaxed`) | `memory_order_release` | `Release` |
| Cursor `w` / `r` load, CAS, or SPSC store | `memory_order_relaxed` | `memory_order_relaxed` | `Relaxed` |
| Ring header `Format` and `Attach` | Fields `relaxed`; `flag = 2` **release** last; `Attach` loads `magic`/`flag` **acquire** first | Same | Same |
| Superblock cold control words | `memory_order_seq_cst` | `memory_order_seq_cst` | `SeqCst` |
| Superblock `state = Ready` publication | `memory_order_release` (or `seq_cst`) | `memory_order_release` | `Release` |
| Superblock `state` and `token` loads | `memory_order_acquire` | `memory_order_acquire` | `Acquire` |
| `initToken`, `token`, and `state` CAS | `atomic_compare_exchange_strong_explicit(..., acq_rel, acquire)` | `compare_exchange_strong(..., acq_rel, acquire)` | `compare_exchange(..., AcqRel, Acquire)` |
| `heartbeat`, `pid`, `interval`, `initHeartbeat` | `memory_order_relaxed` | `memory_order_relaxed` | `Relaxed` |
| Packet (`64` B) and payload buffer bytes | Plain `memcpy` while owning slot | Plain `std::memcpy` | Raw-pointer `copy_nonoverlapping` |
| 64-bit unsigned wraparound arithmetic | `uint64_t` subtraction | `std::uint64_t` subtraction | `wrapping_add`, `wrapping_sub` |
| Signed sequence comparison | `(int64_t)(seq - pos)` | `static_cast<std::int64_t>(seq - pos)` | `seq.wrapping_sub(pos) as i64` |

### 3.1 C11 Reference Ring Snippets

The following C11 snippet illustrates the exact layout assertions and memory orders for ring operations (see `c/src/hqq_ring.h` for the complete production implementation):

```c
#include <stdatomic.h>
#include <stdint.h>
#include <stddef.h>

#if defined(__BYTE_ORDER__) && __BYTE_ORDER__ != __ORDER_LITTLE_ENDIAN__
#error "HQQ requires a little-endian target"
#endif
#if ATOMIC_LLONG_LOCK_FREE != 2
#error "HQQ requires always-lock-free 64-bit atomics"
#endif

typedef struct {
    _Atomic uint64_t seq;        /* @0   */
    uint8_t          packet[64]; /* @8   plain memory owned by the slot claimant */
    uint8_t          pad[56];    /* @72  (stride 128) */
} hqq_slot;
_Static_assert(sizeof(hqq_slot) == 128 && offsetof(hqq_slot, packet) == 8, "slot");

typedef struct {
    uint64_t magic, size, flag, stride, version;                      uint8_t pad0[128 - 40];
    _Atomic uint64_t r;                                               uint8_t pad1[128 - 8];
    _Atomic uint64_t w;                                               uint8_t pad2[128 - 8];
    _Atomic uint32_t not_empty, empty_waiters, not_full, full_waiters; uint8_t pad3[128 - 16];
} hqq_ring_header;
_Static_assert(sizeof(hqq_ring_header) == 512 && offsetof(hqq_ring_header, w) == 256 &&
               offsetof(hqq_ring_header, not_empty) == 384, "ring header");

typedef struct { hqq_ring_header *h; hqq_slot *slots; uint64_t mask; } hqq_ring;

/* Multi-producer claim via CAS on w. Returns NULL when the ring is full. */
static hqq_slot *claim_producer(hqq_ring *r, uint64_t *pos_out) {
    uint64_t pos = atomic_load_explicit(&r->h->w, memory_order_relaxed);
    for (;;) {
        hqq_slot *s = &r->slots[pos & r->mask];
        int64_t d = (int64_t)(atomic_load_explicit(&s->seq, memory_order_acquire) - pos);
        if (d == 0) {
            if (atomic_compare_exchange_weak_explicit(&r->h->w, &pos, pos + 1,
                    memory_order_relaxed, memory_order_relaxed)) {
                *pos_out = pos;
                return s;
            }
        } else if (d > 0) {
            pos = atomic_load_explicit(&r->h->w, memory_order_relaxed);
        } else {
            return NULL;
        }
    }
}

/* Publish slot + Dekker waker check (K1, K2, K3). */
static void publish(hqq_ring *r, hqq_slot *s, uint64_t pos) {
    atomic_store_explicit(&s->seq, pos + 1, memory_order_seq_cst);                  /* K1 */
    if (atomic_load_explicit(&r->h->empty_waiters, memory_order_seq_cst) != 0) {    /* K2 */
        atomic_fetch_add_explicit(&r->h->not_empty, 1, memory_order_seq_cst);       /* K3 */
        /* futex_wake_all(&r->h->not_empty) / ulock_wake / ReleaseSemaphore(sem, 1) */
    }
}

/* Single-consumer claim (or caller-serialized consumer). Returns NULL when empty. */
static hqq_slot *claim_consumer_spsc(hqq_ring *r, uint64_t *pos_out) {
    uint64_t pos = atomic_load_explicit(&r->h->r, memory_order_relaxed);
    hqq_slot *s = &r->slots[pos & r->mask];
    if ((int64_t)(atomic_load_explicit(&s->seq, memory_order_acquire) - (pos + 1)) < 0) {
        return NULL;
    }
    atomic_store_explicit(&r->h->r, pos + 1, memory_order_relaxed);
    *pos_out = pos;
    return s;
}

/* Release slot + Dekker waker check for fullWaiters. */
static void release_slot(hqq_ring *r, hqq_slot *s, uint64_t pos) {
    atomic_store_explicit(&s->seq, pos + r->mask + 1, memory_order_seq_cst);
    if (atomic_load_explicit(&r->h->full_waiters, memory_order_seq_cst) != 0) {
        atomic_fetch_add_explicit(&r->h->not_full, 1, memory_order_seq_cst);
        /* wake_all(&r->h->not_full) */
    }
}

/* Consumer Dekker park sequence (W1..W4). */
static void park_consumer(hqq_ring *r, hqq_slot *s, uint64_t target /* pos + 1 */) {
    atomic_fetch_add_explicit(&r->h->empty_waiters, 1, memory_order_seq_cst);        /* W1 */
    uint32_t v = atomic_load_explicit(&r->h->not_empty, memory_order_seq_cst);       /* W2 */
    if ((int64_t)(atomic_load_explicit(&s->seq, memory_order_seq_cst) - target) < 0) /* W3 */
        { /* futex_wait(&r->h->not_empty, v, <= 100 ms) */ (void)v; }
    atomic_fetch_sub_explicit(&r->h->empty_waiters, 1, memory_order_release);        /* W4 */
}
```

### 3.2 Rust Reference Ring Snippet

In Rust, use raw pointers (`core::ptr::copy_nonoverlapping`) rather than `&mut [u8]` slices when reading or writing shared-memory slot payloads:

```rust
use core::sync::atomic::{AtomicU32, AtomicU64, Ordering};

#[repr(C)]
pub struct Slot {
    seq: AtomicU64,
    packet: [u8; 64],
    _pad: [u8; 56],
}
const _: () = assert!(core::mem::size_of::<Slot>() == 128);
const _: () = assert!(core::mem::offset_of!(Slot, packet) == 8);
#[cfg(not(target_endian = "little"))]
compile_error!("HQQ requires a little-endian target");
#[cfg(not(target_has_atomic = "64"))]
compile_error!("HQQ requires hardware 64-bit atomics");

pub unsafe fn publish(
    seq: &AtomicU64,
    empty_waiters: &AtomicU32,
    not_empty: &AtomicU32,
    pos: u64,
    wake_all: impl Fn(&AtomicU32),
) {
    seq.store(pos.wrapping_add(1), Ordering::SeqCst);          // K1
    if empty_waiters.load(Ordering::SeqCst) != 0 {             // K2
        not_empty.fetch_add(1, Ordering::SeqCst);              // K3
        wake_all(not_empty);
    }
}
```

### 3.3 C++ Reference Ring Snippet

In C++, verify `is_always_lock_free` at compile time:

```cpp
static_assert(std::endian::native == std::endian::little);
static_assert(std::atomic<std::uint64_t>::is_always_lock_free);

struct alignas(8) Slot {
    std::atomic<std::uint64_t> seq;
    std::uint8_t packet[64];
    std::uint8_t pad[56];
};
static_assert(sizeof(Slot) == 128 && offsetof(Slot, packet) == 8);
```

## 4. Common Implementation Pitfalls

The following checklist summarizes the memory-ordering, layout, and state-machine mistakes most frequently encountered when porting HQQ to a new language.

### 4.1 Memory Model and Layout Pitfalls

1. **Publishing `seq` with `release` before loading `emptyWaiters`**: Causes lost wake-ups on x86-64 (store buffering) and ARM64 (RCpc). Always use `seq_cst` for `W1`, `W3`, `K1`, and `K2` (`MEMORY_MODEL.md` section 4.2).
2. **Relaxed `seq` stores or compiler-only barriers**: Breaks payload visibility on weakly ordered architectures (ARM64, POWER, RISC-V).
3. **Mixing atomic and non-atomic accesses to control words** or using `volatile` instead of `<stdatomic.h>`.
4. **Relying on cursor words (`r`, `w`) for cross-process synchronization**: Slot ownership and payload visibility are governed exclusively by `seq[s]`.
5. **Indexing payload buffers from packet operand 1**: Always compute `s = pos & mask` from the claimed ring position.
6. **Assuming 8-byte struct alignment on 32-bit x86**: Force `alignas(8)` and verify every offset with `_Static_assert`.
7. **Failing to update `r` or `w` in SPSC mode**: Always keep the shared-memory cursors canonical (`SPEC 7.6`).
8. **Signed overflow in sequence comparisons**: Subtract in unsigned `uint64_t` first, then cast the difference to `int64_t`.
9. **Assuming the 64-byte packet is 16-byte or 64-byte aligned**: It starts at slot offset `8`; access it via `memcpy` or 8-byte aligned words.
10. **Overwriting `seq` during packet copy**: Copy 64 bytes starting at `slot + 8`, never `slot + 0`.
11. **Retaining pointers into a payload buffer after releasing the slot**.
12. **Mapping with `MAP_PRIVATE` or mapping before checking `st_size >= totalSize`** (which triggers `SIGBUS` on truncated files).
13. **Compiling on big-endian or lock-emulated 32-bit architectures**: Reject at compile time.

### 4.2 Protocol State Machine Pitfalls

14. **Writing into an `Empty` segment without verifying that all of page 0 is zero**: Read every 64-bit word of the first page atomically before claiming `Empty -> Initializing`.
15. **Allowing an Opener to initialize, take over, or reuse a segment**: Openers only attach to `Ready` segments with a live Primary.
16. **Comparing stored wall-clock heartbeats against the local monotonic clock**: Store wall-clock `UnixNano` in heartbeat words, and use the local monotonic clock to measure elapsed watch durations.
17. **Publishing `state = Ready` before incrementing `epoch`** or ordering publish stores after the `initToken` commit CAS.
18. **Skipping `initToken` ownership checks between initialization slices** (`FINDINGS.md` F-01).
19. **Allowing a second Secondary to attach in the same epoch**: Once `endpoints[1].token != 0` (even if `1` or dead), any further opener or creator must receive `ErrLinkBusy` until the segment is re-initialized (`FINDINGS.md` F-15).
20. **Detaching with a blind store instead of `CAS(token, myToken -> 1)`**.
21. **Writing heartbeats without checking `token == myToken`**.
22. **Treating `token != 0, interval == 0` or `heartbeat == 0` as dead**: A newly joined Secondary fills `interval` and `heartbeat` right after its token CAS; treat `0` as alive (`SPEC 6.2`).
23. **Returning from a failed `Read` without releasing the claimed consumer slot**.
24. **Abandoning a claimed producer slot without publishing a tombstone frame** (`OpTombstone` or `OpError`).
25. **Reporting `ErrPeerClosed` or `ErrPeerGone` before draining remaining published slots** (`SPEC 6.4`).
26. **Decrementing a waiter counter you did not increment** or resetting waiter counters outside `Format`.
27. **Parking without a bounded timeout** (cap every kernel wait to `<= 100 ms`).
28. **Releasing `n = waiters` tokens on Windows semaphores**: Grant `1` token per wake (`ReleaseSemaphore(sem, 1, NULL)`) so stale waiter counts from killed processes do not accumulate millions of tokens (`FINDINGS.md` F-02).
29. **Calling `ftruncate` more than once on macOS `shm_open` descriptors** (Darwin returns `EINVAL` on a second `ftruncate`).
30. **Omitting `fallocate` on Linux `/dev/shm`** (use `fallocate` so out-of-space errors return `ENOSPC` at creation time rather than `SIGBUS` during ring writes).
31. **Blocking the Advanced receive loop on the TX ring** (`SPEC 10.3`).
32. **Omitting the `TotalSize + 256` per-assembly memory charge** against `MaxReassemblyBytes` (`SPEC 10.5`).
33. **Violating stream ID parity** (Primary opens positive even IDs; Secondary opens odd IDs; stream `0` is the link-level stream).
34. **Rejecting `Abort` frames due to non-zero size or offset operands** (`FINDINGS.md` F-06).
35. **Failing to zero reserved packet bits or rejecting non-zero reserved bits on receive** (`FINDINGS.md` F-08).

## 5. Deliverables for a Complete Language Port

A complete port in this repository provides:

- Compile-time layout assertions against `spec/constants.json`.
- MPMC ring implementation (`Format`, `Attach`, claim/publish/release, and Dekker parking) verified against `G-07`.
- Link lifecycle state machine (`Creator`, `Opener`, `InMemory`, heartbeats, peer liveness, and safe unlink) verified against `G-06`, `G-08`, and `G-09`.
- An `hqq-interop` CLI helper binary implementing [`conformance/INTEROP_CLI.md`](../conformance/INTEROP_CLI.md) and registered in `.github/workflows/interop.yml`.

## 6. Conformance and Interop Verification Matrix

Every language port is verified in two stages: offline against static golden byte vectors (`G-01` through `G-09`), and live across two OS processes (`I-01` through `I-18`).

### 6.1 Offline Golden Vectors (`conformance/vectors/`)

| Group ID | Verification Target | Source of Truth |
| --- | --- | --- |
| `G-01` | Compile-time or unit-test verification of all `constants.json` offsets, sizes, magics, and opcodes | `spec/constants.json` |
| `G-02` | 16 wire packet vectors (9 base + 7 extended/1.1 frames): byte-identical encode and decode | `manifest.json` (`packets`) |
| `G-03` | 16 segment region-layout vectors across 4 KiB, 16 KiB, and 64 KiB page sizes | `manifest.json` (`layouts`) |
| `G-04` | 12 valid and invalid geometry combinations (`bufferCount` and `bufferSize` bounds) | `manifest.json` (`geometry`) |
| `G-05` | 24 platform segment-name vectors (POSIX, macOS 30/31-byte limit, Windows kernel paths) | `manifest.json` (`names`) |
| `G-06` | 6-step normalized full-segment lifecycle sequence (`Primary ready` through `epoch 2` reuse) | `vectors/sequences/<page>/` |
| `G-07` | 12 whole-segment ring images testing `Read`, `Write`, wraparound at `2^64`, and `Attach` validation | `manifest.json` (`rings`) |
| `G-08` | 31 superblock state images testing Creator, Opener, and InMemory attach decisions | `manifest.json` (`superblocks`) |
| `G-09` | Peer state transitions (`NotConnected -> Connected -> Closed`, token replacement, stale heartbeat, epoch change) | `TestPeerStateTransitions` |
| `G-10` | 44-row Advanced frame validation matrix (`SPEC 10.5` and `PROTOCOL_1_1_DRAFT.md`) | `manifest.json` (`advanced_frames`) |
| `G-11` | 11 wire error-code mappings (codes `0x01..0x08` plus unknown code fallback) | `manifest.json` (`error_codes`) |

### 6.2 Two-Process Cross-Language Interop Matrix (`interop/`)

For every pair of implementations `(A, B)`, the interop test runner executes each scenario in both role assignments (`creator=A, opener=B` and `creator=B, opener=A`):

| Scenario ID | Description | Expected Outcome |
| --- | --- | --- |
| `X-01` | Create, open, exchange messages in both directions, and close in both orders (`creator-closes-first`, `opener-closes-first`) | Payloads verified; peer observes `EOF` / `ErrPeerClosed`; segment unlinked cleanly. |
| `X-02` | Geometry mismatch (`bufferCount` and `bufferSize` doubled or halved) | Opener fails with `ErrInvalidSize` (`GeometryError`). |
| `X-03` | Third endpoint attach, Secondary re-attach after close, and Creator vs live segment | Rejected with `ErrLinkBusy`. |
| `X-04` | Opener before Creator, Opener on `Initializing` segment, Opener on dead segment | Transient errors; Opener never mutates the segment. |
| `X-05` | Ring full / backpressure on a 2-slot ring, consumer drain, and write deadline expiration | FIFO order preserved; expired write returns `ErrTimeout`. |
| `X-06` | Bidirectional high-volume stream with SplitMix64 + CRC-32 verification on 2-slot and 256-slot rings | All messages delivered intact and in order without stalls. |
| `X-07` | `SIGKILL` of peer (while running and while parked) with default and short `PeerTimeout` | Survivor drains buffered frames and reports `ErrPeerGone` within timeout. |
| `X-08` | `SIGKILL` of initializer at `claim`, `format`, `publish`, and `zeroing` crash points | Next Creator takes over after `initStaleWindow` and completes initialization. |
| `X-09` | Name reuse after both endpoints die (`epoch` increments and `creatorToken` changes) | New session succeeds; any resumed old endpoint observes `ErrPeerGone`. |
| `X-10` | Safe unlink when a segment name was replaced behind a live Creator | Replacement segment survives when the original Creator closes. |
| `X-11` | Invalid segment names per platform and 31-byte names on macOS | Rejected with `ErrInvalidName`. |
| `X-12` | Cross-language Dekker wakeup ping-pong on a 2-slot ring | Every message wakes the parked peer within milliseconds; zero lost wake-ups. |
| `X-13` | Windows named semaphores: ping-pong, parked `TerminateProcess`, and session reuse | Verified on Windows CI. |
| `X-14` | Simple-mode (`--wait spin`) peer interoperating with an adaptive parking peer | Clean round-trips across 300 ms idle gaps. |
| `X-21` | Crafted protocol violations on the Standard receive path | Receiver reports the expected protocol error without crashing or hanging. |
| `X-A01` (`X-15`) | Advanced negotiation: creator-initiated, opener-initiated, simultaneous, feature subsets, and rejections | Matches `SPEC 10.1` and `PROTOCOL_1_1_DRAFT.md` section 1. |
| `X-A02` (`X-16`) | Message sizes `0 .. MaxMessageSize`, local overflow refusal, and mid-message sender abort | Intact delivery, `ErrBufferOverflow`, and clean abort recovery. |
| `X-A03` (`X-17`) | Reassembly budget enforcement (`MaxReassemblyBytes`) across requests, responses, and data messages | Request answered with `0x07`, response fails with `ErrReassemblyLimit`, data dropped. |
| `X-A04` (`X-18`) | Concurrent RPC calls, out-of-order responses, application `ResponseError`, canceled call, and unknown method | Matches `SPEC 10.6`. |
| `X-A05` (`X-19`) | Multiplexed streams: pre-`ListenStreams` hold window, parity checks, duplicate ID rejection, and stream close | Matches `SPEC 10.3`. |
| `X-A06` (`X-20`) | Simultaneous bidirectional flood on a tiny ring with a slow consumer | Both sides complete without control-queue deadlock. |
| `X-A07` | Protocol 1.1 `OpCancel`: handler context cancellation, unknown key ignored, and unnegotiated `FeatureCancel` | Matches `PROTOCOL_1_1_DRAFT.md` section 2. |
| `X-A08` | Protocol 1.1 `AdvFlagMeta`: caller ID, trace ID, relative time budget, and locally expired deadline | Matches `PROTOCOL_1_1_DRAFT.md` section 3. |
| `X-A09` | Protocol 1.1 graceful shutdown: new requests refused with status `8` while in-flight requests complete | Matches `PROTOCOL_1_1_DRAFT.md` section 6. |
| `X-A10` | Multi-client `Acceptor` (`HQQ_LSN1`): `Listen` / `Dial` / `Connect`, busy listener rejection, and acceptor close | Matches `LISTENER_DRAFT.md`. |

### 6.3 Target Platform Matrix

All supported ports are verified across Linux `x86_64`, Linux `arm64` (native weak-memory hardware), macOS `arm64` (16 KiB page geometry), and Windows `x64` (`clang-cl` with named semaphores).

## 7. Resolving Specification Questions

The Go reference implementation and `conformance/` test suite (`go test ./conformance/...`) serve as the executable oracle for all numeric constants and state transitions. Historical audit findings and edge-case resolutions are cataloged in [`FINDINGS.md`](FINDINGS.md).
