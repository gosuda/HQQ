# Porting HQQ to a new language

A step-by-step guide for writing a second (third, ...) implementation of the HQQ shared-memory protocol from [SPEC.md](SPEC.md), [MEMORY_MODEL.md](MEMORY_MODEL.md) and [constants.json](constants.json). Targets: C (the reference library `libhqq`), Rust, Zig, C++, and anything that can map shared memory and use lock-free 64-bit atomics.

Rule zero: **the bytes in shared memory and the order of the atomic accesses are the contract; everything else (API, threading, allocation, error types) is yours.** Do not translate the Go API. Translate SPEC sections 2, 3, 5, 6, 7, 8, 9 and, if you want Advanced, 10.

## 1. Conformance levels (the minimal subset, then growth)

You can stop at any level; each level interoperates with the Go implementation as long as the rules of that level are met.

| Level | Name | What you implement | Spec sections | Interop meaning |
| --- | --- | --- | --- | --- |
| **L0** | Layout | Region offsets, superblock/endpoint/ring/slot/packet structs with static assertions, constants, golden vectors | 2, 3, 7.1, 9.1, 9.2, `constants.json` | Can parse any segment image. |
| **L1** | Standard Opener, simple mode | Open an existing named segment as **Secondary**, attach rings, `Read`/`Write` of Standard `copy` packets, spin+sleep waiting, heartbeat and peer monitor, detach on close | 4 (Opener), 5.1 (Ready branch), 5.4, 5.5, 6, 7.3, 7.4, 9, 11 | A port process can talk to a Go **Primary**. Never creates or initializes anything. |
| **L2** | Standard Creator/Primary | Create/reuse names, claim loop for every role, initialization, publish, takeover, reuse, safe unlink | 4, 5 (all), 7.2, 11 | Port can be the **Primary**; full lifecycle parity. |
| **L3** | Parking | Waiter and waker protocol with futex/ulock/Windows semaphores | 8 | CPU-efficient waiting; correct cross-wakes with Go. |
| **L4** | Advanced | Negotiation, streams, chunking, reassembly, request/response | 10 | Full feature parity. |

**The minimal useful subset is L1 + the waker duty of L3** (below). A port that stops at L1 is *valid*; its cost is CPU or latency, never correctness.

### 1.1 Simple mode (no kernel waits)

Simple mode = **spin, then sleep with exponential backoff, never park**. It is a conforming implementation of section 8 because parking is optional for correctness:

* A **simple-mode waiter** never touches `emptyWaiters`/`fullWaiters`/`notEmpty`/`notFull` and re-checks the slot sequence in a loop: spin ~64 iterations, then `sched_yield` ~256 iterations, then `nanosleep` doubling from 20 µs up to 2 ms (the Go fallback uses exactly this).
* A **simple-mode waker** that does nothing after publish/release is still *correct*: a Go peer that is parked on that ring wakes at the end of its bounded park (at most 100 ms) and then finds the data. This costs up to 100 ms of latency, never a hang. RECOMMENDED: still do the two-access waker duty K1/K2 (and K3 if `n != 0`): it costs one `seq_cst` store and one load and gives the Go peer instant wake-ups.
* **Never park without a timeout.** The park timeout is what makes every lost wake-up a latency event.
* Simple mode is also the right first milestone for Windows, where parking needs the four named semaphores (SPEC 11.5).

## 2. Step-by-step

1. **Pin the constants.** Generate a header/module from `constants.json` (offsets, magics, opcodes, flags, codes, timings). Add compile-time assertions (`_Static_assert(offsetof(...))`, `const _: () = assert!(...)`, `comptime`) for every offset and size, `sizeof(packet) == 64`, ring header 512, slot stride 128, superblock size 384. Add the platform guards of MEMORY_MODEL 6 and 7 (lock-free `u64`, little-endian).
2. **Map the segment.** Named object per SPEC 11 (Linux: open `/dev/shm/<name>` with `O_NOFOLLOW|O_CLOEXEC`, `fstat` must be a regular file, check `st_size >= total` **before** mapping, `mmap(MAP_SHARED)`; macOS/FreeBSD: `shm_open("/<name>")`, 30-byte names; Windows: `OpenFileMapping` + four semaphores). Verify the base is page-aligned.
3. **Compute regions** with the formula of SPEC 2.1 using the *runtime* page size (do not hard-code 4096). Check against the golden vectors for 4 KiB/16 KiB/64 KiB in a unit test.
4. **Read the superblock** (atomic loads, SC for simplicity). Implement `claim` for the *Opener* role first: `Ready` branch only (check layout, liveness at open, geometry, `cas(endpoints[1].token, 0, token)`), plus the transient failures for `Empty`/`Initializing`. Add the *Creator* branches (L2) later.
5. **Attach the rings** (SPEC 7.3). Validate magic, flag, stride 128, version 2, size == `bufferCount`. On any mismatch detach (`cas(token, mine, 1)`) and fail.
6. **Implement the slot protocol** (SPEC 7.4, MEMORY_MODEL 3). Start with the SPSC variant (7.6) — it is a dozen lines per direction — and the memory orders of section 3 below. Unit test against `internal/mpmc/spec_constants_test.go`'s scenarios (publish/release sequence values, full, empty, second lap, wrap near `2^64`).
7. **Standard send/receive** (SPEC 9): write payload then packet then publish; receive classifies by opcode, derives the slot from the position, releases promptly. Implement the drain rule of SPEC 6.4.
8. **Liveness**: heartbeat thread (or lazy updates) writing `heartbeat` every interval, the peer monitor of SPEC 6.3, detach on close. Test against a Go peer that you `kill -9`.
9. **Creator/Primary (L2)**: the rest of the claim loop, `beginInit`, `Format`, zeroing, publish with the exact store order of SPEC 5.3, takeover, reuse, creatorToken and safe unlink (SPEC 11.4). Test by killing a Go creator at each step (see 4.3).
10. **Parking (L3)**: add the waiter protocol (SPEC 8.2) and waker duty (8.3) with the platform primitive (SPEC 8.5). Run the Dekker stress test of MEMORY_MODEL 10.5 on x86 *and* ARM hardware.
11. **Advanced (L4)**: negotiation first (two packets), then `OpConnCopy` send/receive for plain messages, then reassembly limits and abort, then request/response, then streams.
12. **Conformance**: run the interop matrix of section 6 against the Go implementation in both directions.

## 3. Ordering cheat-sheet

SC = seq_cst, ACQ = acquire, REL = release, AR = acq_rel, RLX = relaxed. The *required minimums* are in MEMORY_MODEL 2; this is how to spell them. **When in doubt use SC; it is never wrong.**

| Operation (SPEC reference) | C11 `<stdatomic.h>` | C++ `std::atomic` | Rust `Ordering` | Zig `AtomicOrder` |
| --- | --- | --- | --- | --- |
| producer/consumer claim: load `seq` (7.4) | `memory_order_acquire` | `memory_order_acquire` | `Acquire` | `.acquire` |
| publish / release: store `seq` (K1) | `memory_order_seq_cst` | `memory_order_seq_cst` | `SeqCst` | `.seq_cst` |
| waker: load `*Waiters` (K2) | `memory_order_seq_cst` | `memory_order_seq_cst` | `SeqCst` | `.seq_cst` |
| waker: `fetch_add` wake word (K3) | `memory_order_release` (SC fine) | `memory_order_release` | `Release` | `.release` |
| waiter: `fetch_add(+1)` count (W1) | `memory_order_seq_cst` | `memory_order_seq_cst` | `SeqCst` | `.seq_cst` |
| waiter: load wake word (W2) | `memory_order_acquire` or SC | `memory_order_acquire` | `Acquire` | `.acquire` |
| waiter: re-check load `seq` (W3) | `memory_order_seq_cst` | `memory_order_seq_cst` | `SeqCst` | `.seq_cst` |
| waiter: `fetch_sub(1)` count (W4) | `memory_order_release` or RLX | same | `Release` | `.release` |
| cursor `w`/`r` load / CAS / store | `memory_order_relaxed` | `memory_order_relaxed` | `Relaxed` | `.monotonic` |
| ring header fields | store RLX; `flag` store **release** last; loads `magic`/`flag` **acquire** first | same | same | same |
| superblock cold words | `memory_order_seq_cst` | `memory_order_seq_cst` | `SeqCst` | `.seq_cst` |
| `state` store `Ready` | `memory_order_release` (SC fine) | same | `Release` | `.release` |
| `state`/token load before using other words | `memory_order_acquire` | same | `Acquire` | `.acquire` |
| `initToken`/token/state CAS | `atomic_compare_exchange_strong_explicit(..., acq_rel, acquire)` | `compare_exchange_strong(..., acq_rel, acquire)` | `compare_exchange(.., AcqRel, Acquire)` | `@cmpxchgStrong(..., .acq_rel, .acquire)` |
| heartbeat, `pid`, `interval`, `initHeartbeat` | `memory_order_relaxed` | same | `Relaxed` | `.monotonic` |
| payload / packet | plain `memcpy` | plain `memcpy` | raw pointer `copy_nonoverlapping` | `@memcpy` |
| fence-based equivalent of K1/K2 and W1/W3 | `atomic_thread_fence(memory_order_seq_cst)` between the two | `std::atomic_thread_fence` | `core::sync::atomic::fence(SeqCst)` | `@fence(.seq_cst)` (older Zig) or SC accesses |
| 64-bit wrap arithmetic | `uint64_t` (unsigned wrap is defined) | `std::uint64_t` | `wrapping_add`, `wrapping_sub` | `+%`, `-%` |
| signed difference | `(int64_t)(seq - pos)` | `std::int64_t(seq - pos)` | `seq.wrapping_sub(pos) as i64` | `@as(i64, @bitCast(seq -% pos))` |

### 3.1 C11 reference snippets (compiled with `cc -std=c11 -Wall`)

```c
#include <stdatomic.h>
#include <stdint.h>
#include <stddef.h>

#if defined(__BYTE_ORDER__) && __BYTE_ORDER__ != __ORDER_LITTLE_ENDIAN__
#error "HQQ requires a little-endian host"
#endif
#if ATOMIC_LLONG_LOCK_FREE != 2
#error "HQQ requires lock-free 64-bit atomics that work across processes"
#endif

typedef struct {
    _Atomic uint64_t seq;        /* @0   */
    uint8_t          packet[64]; /* @8   plain memory, owned by the slot's claimant */
    uint8_t          pad[56];    /* @72  (stride 128) */
} hqq_slot;
_Static_assert(sizeof(hqq_slot) == 128 && offsetof(hqq_slot, packet) == 8, "slot");

typedef struct {
    uint64_t magic, size, flag, stride, version;        uint8_t pad0[128 - 40];
    _Atomic uint64_t r;                                 uint8_t pad1[128 - 8];
    _Atomic uint64_t w;                                 uint8_t pad2[128 - 8];
    _Atomic uint32_t not_empty, empty_waiters, not_full, full_waiters; uint8_t pad3[128 - 16];
} hqq_ring_header;                                      /* 512 bytes, see SPEC 7.1 */
_Static_assert(sizeof(hqq_ring_header) == 512 && offsetof(hqq_ring_header, w) == 256 &&
               offsetof(hqq_ring_header, not_empty) == 384, "ring header");

typedef struct { hqq_ring_header *h; hqq_slot *slots; uint64_t mask; } hqq_ring;

/* Producer claim, CAS variant (several threads of this process). NULL = ring full. */
static hqq_slot *claim_producer(hqq_ring *r, uint64_t *pos_out) {
    uint64_t pos = atomic_load_explicit(&r->h->w, memory_order_relaxed);
    for (;;) {
        hqq_slot *s = &r->slots[pos & r->mask];
        int64_t d = (int64_t)(atomic_load_explicit(&s->seq, memory_order_acquire) - pos);
        if (d == 0) {
            if (atomic_compare_exchange_weak_explicit(&r->h->w, &pos, pos + 1,
                    memory_order_relaxed, memory_order_relaxed)) { *pos_out = pos; return s; }
        } else if (d > 0) {
            pos = atomic_load_explicit(&r->h->w, memory_order_relaxed);
        } else {
            return NULL;
        }
    }
}

/* Publish + waker duty: K1, K2, K3. After this the slot belongs to the consumer. */
static void publish(hqq_ring *r, hqq_slot *s, uint64_t pos) {
    atomic_store_explicit(&s->seq, pos + 1, memory_order_seq_cst);                  /* K1 */
    if (atomic_load_explicit(&r->h->empty_waiters, memory_order_seq_cst) != 0) {    /* K2 */
        atomic_fetch_add_explicit(&r->h->not_empty, 1, memory_order_seq_cst);       /* K3 */
        /* futex_wake_all(&r->h->not_empty) / ulock wake / ReleaseSemaphore(sem, n) */
    }
}

/* SPSC consumer: one consumer thread (or a local lock). NULL = ring empty. */
static hqq_slot *claim_consumer_spsc(hqq_ring *r, uint64_t *pos_out) {
    uint64_t pos = atomic_load_explicit(&r->h->r, memory_order_relaxed);
    hqq_slot *s = &r->slots[pos & r->mask];
    if ((int64_t)(atomic_load_explicit(&s->seq, memory_order_acquire) - (pos + 1)) < 0) return NULL;
    atomic_store_explicit(&r->h->r, pos + 1, memory_order_relaxed);   /* keep the cursor canonical */
    *pos_out = pos;
    return s;
}

/* Release + waker duty for the NotFull side. */
static void release_slot(hqq_ring *r, hqq_slot *s, uint64_t pos) {
    atomic_store_explicit(&s->seq, pos + r->mask + 1, memory_order_seq_cst);
    if (atomic_load_explicit(&r->h->full_waiters, memory_order_seq_cst) != 0) {
        atomic_fetch_add_explicit(&r->h->not_full, 1, memory_order_seq_cst);
        /* wake_all(&r->h->not_full) */
    }
}

/* Consumer parks (the caller just saw seq < target). W1..W4. */
static void park_consumer(hqq_ring *r, hqq_slot *s, uint64_t target /* pos + 1 */) {
    atomic_fetch_add_explicit(&r->h->empty_waiters, 1, memory_order_seq_cst);        /* W1 */
    uint32_t v = atomic_load_explicit(&r->h->not_empty, memory_order_seq_cst);       /* W2 */
    if ((int64_t)(atomic_load_explicit(&s->seq, memory_order_seq_cst) - target) < 0) /* W3 */
        { /* futex_wait(&r->h->not_empty, v, <= 100 ms) */ (void)v; }
    atomic_fetch_sub_explicit(&r->h->empty_waiters, 1, memory_order_release);        /* W4 */
}
```

### 3.2 Rust (compiled with `rustc 2021`)

```rust
use core::sync::atomic::{fence, AtomicU32, AtomicU64, Ordering};

#[repr(C)]
pub struct Slot { seq: AtomicU64, packet: [u8; 64], _pad: [u8; 56] }
const _: () = assert!(core::mem::size_of::<Slot>() == 128);
const _: () = assert!(core::mem::offset_of!(Slot, packet) == 8);
#[cfg(not(target_endian = "little"))] compile_error!("HQQ requires a little-endian host");
#[cfg(not(target_has_atomic = "64"))] compile_error!("HQQ requires native 64-bit atomics");

// Never create `&mut [u8]` over the packet or payload of a slot you do not own:
// use raw pointers (`ptr::copy_nonoverlapping`) only while you own the slot.
pub unsafe fn publish(seq: &AtomicU64, empty_waiters: &AtomicU32, not_empty: &AtomicU32,
                      pos: u64, wake_all: impl Fn(&AtomicU32)) {
    seq.store(pos.wrapping_add(1), Ordering::SeqCst);               // K1
    if empty_waiters.load(Ordering::SeqCst) != 0 {                  // K2
        not_empty.fetch_add(1, Ordering::SeqCst);                   // K3
        wake_all(not_empty);
    }
}
// Fence form (equivalent): seq.store(.., Release); fence(Ordering::SeqCst); empty_waiters.load(Relaxed) ...
// AtomicU64::from_ptr(base.add(off) as *mut u64) (Rust >= 1.75) gives atomics over mapped memory.
```

### 3.3 C++ (compiled with `clang++ -std=c++20`)

```cpp
static_assert(std::endian::native == std::endian::little);
static_assert(std::atomic<std::uint64_t>::is_always_lock_free);
struct alignas(8) Slot { std::atomic<std::uint64_t> seq; std::uint8_t packet[64]; std::uint8_t pad[56]; };
static_assert(sizeof(Slot) == 128 && offsetof(Slot, packet) == 8);
// Placement: the segment is raw bytes; use std::atomic_ref<std::uint64_t>(*reinterpret_cast<std::uint64_t*>(p))
// (C++20) or construct std::atomic<> objects with placement new ONCE at Format time only (lifetime rules!).
// publish: s->seq.store(pos + 1, std::memory_order_seq_cst); if (h->empty_waiters.load(std::memory_order_seq_cst)) {...}
```

### 3.4 Zig (**not compiled**: no Zig toolchain was available; treat as a sketch)

```zig
// publish (K1..K3)
@atomicStore(u64, &slot.seq, pos +% 1, .seq_cst);
if (@atomicLoad(u32, &hdr.empty_waiters, .seq_cst) != 0) {
    _ = @atomicRmw(u32, &hdr.not_empty, .Add, 1, .seq_cst);
    // futex wake all
}
// claim: const d = @as(i64, @bitCast(@atomicLoad(u64, &slot.seq, .acquire) -% pos));
comptime { std.debug.assert(@sizeOf(Slot) == 128 and @offsetOf(Slot, "packet") == 8); }
// Use extern struct with explicit padding; std.atomic.Value(u64) is also fine.
```

## 4. Pitfalls

### 4.1 Memory model and layout

1. **Publishing with `release` and then loading the waiter count** is the classic Dekker bug (works on most hardware most of the time, loses wake-ups under load, fails fast on x86 when the compiler emits `mov`). Use SC or SC fences (MEMORY_MODEL 4). Run the stress test.
2. **Relaxed `seq` store** (even with a compiler barrier) breaks payload visibility on ARM/POWER.
3. **Mixing atomic and plain access to the same word**, or going through `volatile`.
4. **Using the cursor words for synchronization.** They are claim arbiters only; ownership of a slot is `seq`.
5. **Taking the slot index from packet operand 1** instead of the position.
6. **Plain-aligned structs**: i386 aligns `uint64_t` members to 4 bytes; use explicit offsets or `alignas(8)`. Never rely on `sizeof` of a C struct you did not assert.
7. **Reading `r`/`w` once at attach and caching them forever** in a process that may share the side with another thread — or, in SPSC mode, *not storing* them. Keep the shared cursors canonical (SPEC 7.6).
8. **Wrong wraparound arithmetic**: signed overflow in C is undefined; subtract in `uint64_t`, then cast to `int64_t`. In Rust use `wrapping_*`; in Zig use `+%`/`-%` (debug builds trap on plain `+`).
9. **Treating the 64-byte packet as 8-aligned to 64**: it sits at slot offset 8; use `memcpy` of 64 bytes.
10. **Copying `seq` along with the packet** (copy exactly the packet).
11. **Holding a view of a payload buffer after release.**
12. **`MAP_PRIVATE`** instead of `MAP_SHARED`; **mapping shorter than `total`**; **mapping before checking the object size** (SIGBUS).
13. **Big-endian or 32-bit-MIPS/ARMv5-6 targets**: refuse to build (MEMORY_MODEL 6, 7).

### 4.2 Protocol

14. **Writing into an `Empty` segment whose page is not zero.** The zero check covers the **whole page** (page size bytes), every word read atomically. Never "just write magic" into an object you opened.
15. **Openers must never initialize, reuse or take over.** An opener on a dead segment fails with the transient `errNoLiveEndpoint`; reuse is the creator's job.
16. **Clocks.** Wall clock for the words, monotonic clock for decisions (initializer watch, peer monitor). Do not use the wall clock to decide takeover.
17. **Forgetting to update `epoch` before `Ready`**, or ordering publish stores after the commit CAS.
18. **Not checking `initToken` ownership before each initialization step**, or continuing after losing it.
19. **Third endpoints**: `ErrLinkBusy` also when the live endpoint's *earlier peer is detached or dead* (`endpoints[1].token != 0`).
20. **Detach is a CAS from your own token to 1**, never a blind store (the record may belong to a newer session).
21. **Heartbeat writes must be guarded by `token == mine`.**
22. **Partial records.** After the join CAS the record's `interval`/`heartbeat` are still `0` for a moment: treat `token != 0, interval == 0` as alive / heartbeats disabled; treat `heartbeat == 0` as alive (SPEC 6.2).
23. **Standard receive must release the slot even on failure** (invalid size, unexpected opcode, panic/exception in the callback). A claimed slot that is never released stalls the ring.
24. **A claimed producer slot must always be published** (tombstone if the write is abandoned). Exceptions/early returns between claim and publish are the usual leak.
25. **Drain before reporting peer failure** (SPEC 6.4).
26. **Waiter counts count processes.** Never decrement a count you did not increment; over-counting is harmless, under-counting loses wake-ups.
27. **Park with a timeout always.** A park without a timeout converts a lost wake into a hang.
28. **Windows**: tokens leak in the semaphore (cap `Release` at 1 per wake, FINDINGS F-02); create the four semaphores **before** the mapping; use the canonical suffixes; names are `Local\` unless prefixed.
29. **macOS**: the name limit is **30** characters plus the slash; `ftruncate` only once on a shm object; `shm_open` names are not paths; the unlink identity check is by `creatorToken` because there are no inodes.
30. **Linux**: `O_NOFOLLOW`, regular-file check, `fallocate` (not just `ftruncate`) to turn "tmpfs full" into an error instead of `SIGBUS`; remove a half-created object on failure.
31. **Advanced receive loop must never wait for the tx ring** (SPEC 10.3); give control replies their own sender and a bounded queue.
32. **Reassembly limits are mandatory**: charge `TotalSize + 256`, refuse the first chunk of an over-budget message, drain the rest (SPEC 10.5).
33. **Stream-id parity** (Primary even, Secondary odd) and `MessageID` per sender across all streams.
34. **`Abort` frames are accepted regardless of their other operands.**
35. **Do not interpret reserved bits and operands** (SPEC 9.1, FINDINGS F-08); write zeros.

## 5. What to deliver per language

* A constants module generated from `constants.json` plus the compile-time assertions of step 1.
* The ring (`format`, `attach`, claim/publish/release, SPSC and, if you want in-process multi-thread without a lock, CAS variants) with unit tests for every vector.
* The link lifecycle (claim loop, init, publish, join, liveness, close/unlink) with the decision table test (see `TestSpecClaimTable` in the root package for the expected cases).
* A `conformance` driver that speaks the stdin/stdout protocol the parent defines for the interop matrix.
* A statement of the conformance level (section 1) and of the memory orders chosen.

## 6. Conformance test list (to be built by the parent under `conformance/`)

### 6.1 Golden vectors (offline, no second process)

| ID | Test | Source of truth |
| --- | --- | --- |
| G-01 | Every `constants.json` offset/size/value is asserted at compile time or by a unit test | `constants.json` |
| G-02 | 9 packet vectors: encode (operands → bytes) and decode (bytes → opcode + operands), byte-identical | `packet_vectors` |
| G-03 | 12 region-layout vectors (3 page sizes × 4 geometries) | `segment.layout_vectors` |
| G-04 | Invalid geometries yield size 0 / `ErrInvalidSize` (count 0,1,3; size 0,4,12; bounds + 1) | `geometry` |
| G-05 | Name validation vectors for the host platform (invalid vectors; macOS length 30/31; Windows kernel-name vectors) | `platform` |
| G-06 | **Golden segment images**: the Go implementation dumps a segment after (a) Primary ready, (b) Secondary joined, (c) one message in flight, (d) after one message consumed, (e) both closed, (f) re-initialized (epoch 2); the port parses each image and a port-built image for the same sequence matches except the documented variable words (tokens, `pid`, `heartbeat`, `initHeartbeat`, `creatorToken`) | Go `TestSpecSegmentImage` scenario, extended |
| G-07 | Ring unit tests: publish `seq = pos+1`, release `seq = pos+mask+1`, full/empty detection without side effects, FIFO, second lap, wrap at `2^64 - 2`, `Attach` rejecting mutated headers (version, stride, size not a power of two, size 1, magic, init flag) | `internal/mpmc/spec_constants_test.go` |
| G-08 | Claim-loop decision table (every row of SPEC 4 and the pseudocode of 5.1) against crafted superblock images, including: foreign page, unknown state, `HQQ_STL2`, unknown required feature, stalled initializer takeover with a short stale window, opener never writes | root `TestSpecClaimTable` |
| G-09 | Peer-state derivation: not connected → connected → closed; replaced token; stalled heartbeat; epoch change | root `TestSpecPeerStateDerivation`, `TestSpecLiveness` |
| G-10 | Advanced frame validation matrix: every rejection of SPEC 10.5 (unknown flags, request+response, error without response, bounds, zero-size rules, offset/aux, begin/abort/refused handling, limits) with the expected error and expected effect (drop/fail/drain) | new golden frames |
| G-11 | Error-code mapping (7 codes + unknown) | `error_codes` |

### 6.2 Interop matrix (two processes; both implementations in both roles)

Rows: `(A=go, B=port)` and `(A=port, B=go)`, A = Creator/Primary, B = Opener/Secondary; also port↔port for L2+. Each cell bounded to a few seconds.

| ID | Scenario | Expected |
| --- | --- | --- |
| X-01 | create, open, exchange 1 B, `bufferSize` B, both directions, close in both orders | data intact; the other side sees close (`io.EOF`/`PeerClosed`); name removed by the creator |
| X-02 | geometry mismatch (count, size) | `GeometryError` / `ErrInvalidSize` |
| X-03 | third endpoint; secondary re-join after its own close; creator vs live segment | `ErrLinkBusy` |
| X-04 | opener before creator; opener on an initializing segment; opener on a dead segment | transient errors; no write by the opener (compare the image) |
| X-05 | ring full/backpressure: producer blocked with a 2-slot ring, then consumer drains; cancel by deadline | order preserved; `ErrTimeout` at the deadline; no data loss |
| X-06 | 100k messages with sequence number + CRC, both directions concurrently, ring of 2 and 256 slots | in order, intact, no stall |
| X-07 | `kill -9` of the peer (parked and running); peer timeout default and short | `ErrPeerGone` within `max(PeerTimeout, 4 × interval)`; buffered messages delivered first |
| X-08 | `kill -9` of the initializer between claim and publish; between ring format and publish; during buffer zeroing (large geometry) | takeover by the next creator after the stale window; opener keeps waiting / fails transient |
| X-09 | reuse of a name after both endpoints died; creatorToken changes; epoch increments | new Primary, old endpoints observe `ErrPeerGone` on resume |
| X-10 | safe unlink: replace the name behind a live creator, then close the creator | the replacement survives (Linux and macOS); close of a missing name is a no-op |
| X-11 | invalid names per platform; long names on macOS | `ErrInvalidName` |
| X-12 | parked consumer woken by a producer of the other implementation; parked producer woken by a consumer of the other implementation (ring of 2, idle 50 ms between messages, park timeout raised to 2 s in a test build) | each message delivered within a few ms; **no lost wake-up** (Dekker) |
| X-13 | Windows: four semaphores, create/open order, leftover tokens after `kill -9` of a parked process, mapping name vanishes with the last handle | Windows CI only |
| X-14 | simple-mode port vs Go: port never parks | correct; Go-side parked waits end within the 100 ms bound |
| X-15 | negotiation: each role initiates, simultaneous negotiation, feature subsets (messages only, request/response only, none), version major mismatch, `OpError` during negotiation, `Failed` is terminal | per SPEC 10.1 |
| X-16 | messages of 0 B, 1 B, `bufferSize`, `bufferSize + 1`, `3 × bufferSize + 7`, `MaxMessageSize`; `MaxMessageSize + 1` refused locally; sender abort after chunk 1 | intact / `ErrBufferOverflow` / drop; the receiver survives an abort |
| X-17 | reassembly limit: several large multi-chunk messages in flight with a small `MaxReassemblyBytes`; refused request answered with `ErrCodeBufferOverflow`, refused response fails the call with `ErrReassemblyLimit`, data dropped | per SPEC 10.5 |
| X-18 | request/response: concurrent calls, out-of-order completion, error response with payload, cancelled call whose late response is discarded, unknown method | per SPEC 10.6 |
| X-19 | streams: open/accept, refusal (not listening → 5, bad parity/duplicate → 6, accept queue full under drop → 7), data on the stream, close (peer sees `PeerClosed` after queued data), stream 0 not closable, ids never reused | per SPEC 10.3 |
| X-20 | flood from both sides with a tiny ring and a slow consumer: no deadlock (receive path never waits for the tx ring) | both progress |
| X-21 | protocol violations from a crafted peer (raw ring writer): unknown flags, invalid sizes, non-Begin chunk without assembly, duplicate Begin | receive path fails with the specified error; the process does not crash or hang |

### 6.3 Platform matrix

Linux x86-64, Linux AArch64 (weak ordering; the Dekker and publish stress tests must run here), macOS arm64 (16 KiB pages), Windows x86-64 (semaphores), plus 32-bit x86 if a port claims it (alignment!). FreeBSD optional.

## 7. When the spec and the code disagree

The code (with `go test -run Spec`) is the oracle for the numeric constants; if you find a behavior the spec states that Go does not do (or vice versa), it is a spec bug: report it. Known gaps and accidental behaviors are in [FINDINGS.md](FINDINGS.md).
