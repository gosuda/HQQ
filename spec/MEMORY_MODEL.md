# HQQ Memory Model Specification

This document specifies the required memory ordering for every atomic word in an HQQ shared-memory segment, proves the happens-before invariants governing payload buffer ownership and the Dekker wait/wake handshake, and lists the verification suites every conforming port must pass. It is the normative memory-ordering companion to [`SPEC.md`](SPEC.md); section references of the form `SPEC x.y` refer to [`SPEC.md`](SPEC.md), and executable litmus models live in [`spec/litmus/`](litmus/) and [`c/verify/`](../../c/verify).

## 1. Memory Order Vocabulary and Language Mapping

Memory orderings are stated in ISO C11 / C++11 terminology and map across implementation languages as follows:

| Shorthand | C11 `<stdatomic.h>` | C++ `std::atomic` | Rust `core::sync::atomic::Ordering` |
| --- | --- | --- | --- |
| **RLX** (relaxed atomic, no ordering) | `memory_order_relaxed` | `memory_order_relaxed` | `Relaxed` |
| **ACQ** (acquire load) | `memory_order_acquire` | `memory_order_acquire` | `Acquire` |
| **REL** (release store) | `memory_order_release` | `memory_order_release` | `Release` |
| **AR** (acquire-release RMW) | `memory_order_acq_rel` | `memory_order_acq_rel` | `AcqRel` |
| **SC** (sequentially consistent) | `memory_order_seq_cst` | `memory_order_seq_cst` | `SeqCst` |
| **PLAIN** (non-atomic access) | ordinary load/store, `memcpy` | ordinary / `std::memcpy` | raw-pointer `read`/`write`/`copy_nonoverlapping` (never create `&` or `&mut` references to shared memory concurrently writable by another process) |

In the Go reference implementation, all `sync/atomic` operations are sequentially consistent (`SC`), satisfying every minimum requirement below automatically. A port in C, C++, or Rust chooses explicit memory orders and `MUST NOT` use an order weaker than the required minimum in section 2. Using `SC` on cold-path control words (superblock fields, endpoint records, and ring headers touched only during attachment or heartbeats) is always permitted and `RECOMMENDED`.

Although the C11 standard defines its memory model in terms of threads within a single process, C11 section 7.17.5.1 p2 specifies that lock-free atomic operations should be **address-free** ("atomic operations on the same memory location via two different addresses will communicate atomically"). On every supported OS and CPU architecture (`x86_64` and `arm64` over `MAP_SHARED` or Windows file-mapping views), two processes mapping the same physical page interact through the hardware cache-coherency protocol identically to two threads in one process. This address-free requirement is why section 6 mandates hardware lock-free 32-bit and 64-bit atomics.

## 2. Shared-Memory Word Ordering Tables

In the tables below, the party roles are abbreviated as follows:

- **I**: The initializer endpoint that currently holds `initToken`.
- **C**: Any endpoint executing the attachment / claim loop.
- **E**: The endpoint that owns a specific endpoint record (`endpoints[0]` or `endpoints[1]`).
- **Q**: The peer endpoint monitoring `E`.
- **P**: The ring producer.
- **K**: The ring consumer.

All orders listed are required minimums; **`SC` is always permitted in place of a weaker atomic order**.

### 2.1 Superblock Words (`SPEC 3.1`)

The 384-byte superblock at the start of page 0 coordinates segment initialization, geometry validation, and session epochs:

| Word & Offset | Writer | Reader | Store / RMW Order | Load Order | Ordering Rationale |
| --- | --- | --- | --- | --- | --- |
| `magic` (`@0`) | `I` at publish (`P5`) | `C` | **RLX** (covered by the final `state = Ready` **REL** store); **REL** allowed | **RLX** in `Initializing` (used only to reject foreign layouts); covered by `state` **ACQ** in `Ready` | Written before the commit CAS; joining endpoints observe it after acquiring `state == Ready`. |
| `state` (`@8`) | `C`: CAS `Empty -> Initializing`, CAS `Ready -> Initializing` (**AR**); `I`: store `Ready` (**REL**) | `C`, all monitors | Store **REL**, CAS **AR** | **ACQ** | `state = Ready` is the release publication point for segment initialization: every store performed by `I` (formatted rings, zeroed payload pools, geometry, endpoint records, `magic`, `epoch`) happens-before any endpoint that acquires `state == Ready`. |
| `major`, `minor`, `requiredFeatures` (`@16`, `@24`, `@88`) | `I` at claim (`stampLayout`) and at publish | `C` | **RLX** (stamped before `initToken` CAS; second stamp covered by `state` **REL**) | **RLX** in `Initializing`; covered by `state` **ACQ** in `Ready` | Layout checks on an `Initializing` segment are a fast-fail heuristic on an in-progress header and reject only conflicting non-zero values. |
| `pageSize`, `bufferCount`, `bufferSize`, `layoutSize` (`@32..56`) | `I` at publish | Joiners (after `Ready`) | **RLX** (covered by `state` **REL**) | Covered by `state` **ACQ** | Segment geometry is immutable while `state == Ready`. |
| `epoch` (`@64`) | `I`: `fetch_add(1)` at commit | Joiners (after join), peer monitors (`checkPeer`) | **AR** | **ACQ** | Incremented once per initialization after the `initToken` commit CAS and before `state = Ready`. Endpoints compare `epoch` to detect whether the segment was re-initialized underneath them. |
| `initToken` (`@72`) | `C`/`I`: CAS `expected -> token` (`beginInit`), CAS `token -> 0` (commit) | `C` (watch), `I` (ownership check) | CAS **AR** | **ACQ** for ownership checks; **RLX** for progress watches | Serves as the initialization ownership lock; the commit CAS orders all publish stores before releasing ownership. |
| `initHeartbeat` (`@80`) | `I` (progress) | `C` (watch) | **RLX** | **RLX** | Checked only for value changes against the observer's local monotonic clock; no payload data depends on it. |
| `creatorToken` (`@96`) | `I` immediately after winning `initToken` | Any (`ownsName`, join) | **RLX** (covered by `state` **REL** for joiners) | Covered by `state` **ACQ**; **RLX** for `ownsName` | Identifies the segment creator instance for safe POSIX `shm_unlink`. |
| Reserved (`@104..127`) and trailing bytes to end of page 0 | None | `firstPageZero` (`C`) | None | **RLX** / **ACQ** (8-byte word loads) | Must remain zero; an `Empty` segment with any non-zero word on page 0 is rejected as `ErrIncompatibleLayout` (`SPEC 5.1`). |

### 2.2 Endpoint Records (`SPEC 3.2`)

Each endpoint (`0` = Primary, `1` = Secondary) owns a 128-byte record inside the superblock:

| Word | Writer | Reader | Store / RMW Order | Load Order | Ordering Rationale |
| --- | --- | --- | --- | --- | --- |
| `token` | `E` at publish (Primary: store; Secondary: CAS `0 -> token`, **AR**); `E` at close: CAS `token -> 1` (**REL**); `I` at publish: clear Secondary record | `C`, `Q` (`checkPeer`) | CAS **AR** / **REL**; atomic store at publish | **ACQ** | Governs slot ownership and liveness. Setting `token = 1` on close is a release operation: when peer `Q` observes `token == 1` with an acquire load, every ring frame published by `E` before closing is visible to `Q`, making the post-close drain rule (`SPEC 6.4`) sound. |
| `pid` | `E` at join / publish | Diagnostic tools | **RLX** | **RLX** | Diagnostic metadata only. |
| `heartbeat` | `E` every heartbeat interval (while `token == mine`) | `C` (open-time liveness), `Q` | **RLX** | **RLX** | Monitored for advancement against the reader's local monotonic clock; carries no dependent payload. |
| `interval` | `E` at join / publish | `C`, `Q` | **RLX** | **RLX** | Because `interval` and `heartbeat` are populated immediately after the `token` CAS, readers observing `token != 0` with `interval == 0` or `heartbeat == 0` treat the endpoint as alive with heartbeats not yet active (`SPEC 6.2`). |

### 2.3 Ring Header Words (`SPEC 7.1`)

Each directional ring begins with a 512-byte header split across four 128-byte cache-line pairs:

| Word | Writer | Reader | Store / RMW Order | Load Order | Ordering Rationale |
| --- | --- | --- | --- | --- | --- |
| `magic`, `size`, `stride`, `version` (line 0) | `I` in `Format` | `P`, `K` in `Attach` | **RLX**, written in the sequence of `SPEC 7.2` | **RLX** after acquiring `flag == 2` | Immutable once `Format` completes. |
| `flag` (line 0) | `I`: stores `0` first, stores `2` (`ringReadyFlag`) **last** in `Format` | `Attach` | `0`: **RLX**; `2`: **REL** | **ACQ** | Releases all formatted ring slots, cursors, and counters to attaching endpoints so a partially formatted ring is never accepted. |
| `r` (consumer cursor, line 1) | `K` (CAS or store) | `K` only | **RLX** | **RLX** | Arbitrates slot claims among threads on the consumer side; cross-process data visibility is governed by `seq[i]`, not `r`. |
| `w` (producer cursor, line 2) | `P` (CAS or store) | `P` only | **RLX** | **RLX** | Arbitrates slot claims among threads on the producer side. |
| `notEmpty` (`u32 @384`, line 3) | `P`: `fetch_add(1)` after publish when `emptyWaiters != 0`; `Format`; `WakeAll` | Consumer parkers (`W2`) and OS kernel | **REL** (`SC` allowed) | **ACQ** (`W2`) | Serves as the futex/ulock compare word; incrementing it with release ordering after observing a waiter ensures the waiter's acquire load in `W2` orders `K1` before `W3` (`SPEC 8.3`). |
| `emptyWaiters` (`u32 @388`, line 3) | `K`: `fetch_add(+1)` (`W1`), `fetch_add(-1)` (`W4`); `Format` | `P` (`K2`) | `W1`: **SC**; `W4`: **RLX** or **REL** | `K2`: **SC** | Tracks parked consumer processes; forms half of the Dekker store-buffering pair (section 4). |
| `notFull` (`u32 @392`, line 3) | `K`: `fetch_add(1)` after slot release when `fullWaiters != 0` | Producer parkers (`W2`) and OS kernel | **REL** (`SC` allowed) | **ACQ** (`W2`) | Futex/ulock compare word for full-ring backpressure (`SPEC 8.3`). |
| `fullWaiters` (`u32 @396`, line 3) | `P`: `W1` / `W4` | `K` (`K2`) | `W1`: **SC**; `W4`: **RLX** or **REL** | `K2`: **SC** | Tracks parked producer processes; forms half of the Dekker store-buffering pair for backpressure. |

### 2.4 Ring Slots and Payload Buffers (`SPEC 7.1, 7.4, 9.4`)

Each ring slot `i` (`0 <= i < bufferCount`) controls exclusive access to its 64-byte packet and its `bufferSize`-byte payload buffer:

| Region | Writer | Reader | Store / RMW Order | Load Order | Ordering Rationale |
| --- | --- | --- | --- | --- | --- |
| `seq[i]` (`u64`, slot offset `0`) | `Format` (initial value `i`); `P` at publish (`pos + 1`); `K` at release (`pos + mask + 1`) | `P` and `K` (claim loops), waiters (`W3`) | `Format`: **RLX** (covered by `flag` **REL**); publish and release: **SC** (**REL** for payload + **SC** for Dekker handshake) | Claim loops: **ACQ**; wait re-check `W3`: **SC** | Primary synchronization variable on the hot data path. Storing `pos + 1` releases the packet and payload buffer `i` to the consumer; storing `pos + mask + 1` releases the slot back to the next-lap producer. |
| Packet (`64` bytes, slot offset `8`, **PLAIN**) | `P` between claim and publish | `K` between claim and release | **PLAIN** | **PLAIN** | Exclusively owned by the party holding slot `i` and ordered by `seq[i]`. Must be accessed via `memcpy` or 8-byte aligned words. |
| Payload buffer `i` (**PLAIN**) | `P` between claim and publish; `I` during pre-`Ready` zeroing | `K` between claim and release | **PLAIN** | **PLAIN** | Exclusively owned by the party holding slot `i`; slot ownership grants buffer ownership. |

## 3. Happens-Before Proof for Payload Transfers

The ordering rules in section 2 establish five happens-before guarantees without locks on the data path:

1. **Claim 1 (Payload visibility from producer to consumer)**: Suppose producer `P` claims position `pos` and slot `s = pos & mask`. In program order, `P` performs plain writes to payload buffer `s` and the 64-byte packet at slot `s`, followed by `seq[s].store(pos + 1, SC)` (which is at least a release store). Consumer `K` claims `pos` only after `seq[s].load(ACQ)` observes `pos + 1`. Because `K`'s acquire load reads from `P`'s release store, `P`'s store synchronizes-with `K`'s load, and all plain writes by `P` to the packet and payload buffer `s` happen-before `K`'s subsequent plain reads. Because `seq` values are strictly monotonic across laps, `K` cannot observe a store from an earlier lap.
2. **Claim 2 (Race-free slot reuse across laps)**: Once consumer `K` finishes reading the packet and payload buffer `s`, it executes `seq[s].store(pos + mask + 1, SC)` (at least release). The next-lap producer for position `pos + size` (which maps to the same slot `s`) claims the slot only after `seq[s].load(ACQ)` observes `pos + size == pos + mask + 1`. By the same synchronizes-with edge, all plain reads by `K` happen-before the next-lap producer's plain writes. Omitting either the consumer's release ordering or the producer's acquire ordering introduces a read-write data race on weakly ordered CPUs (modeled in [`litmus/slot_wraparound_handoff.litmus`](litmus/slot_wraparound_handoff.litmus) and [`litmus/mp_publish_acqrel.litmus`](litmus/mp_publish_acqrel.litmus)).
3. **Claim 3 (Relaxed cursor claims)**: The CAS on `w` (or `r`) arbitrates only which thread within the same process claims position `pos`. Because the thread loads `seq[s]` with **ACQ** before the cursor CAS and `seq[s]` cannot change while `w == pos`, a relaxed CAS on `w` (or `r`) preserves the acquire edge on `seq[s]`.
4. **Claim 4 (Initialization visibility)**: The initializer's `memset` of payload pools, `Format` of ring headers, and writes to superblock geometry all precede `state.store(Ready, REL)` in program order (`SPEC 5.3`). Any endpoint whose `state.load(ACQ)` observes `Ready` sees zeroed buffers and formatted rings without racing with initialization.
5. **Claim 5 (Complete drain after graceful peer close)**: A closing endpoint executes `token.cas(mine -> 1, REL)` after its final ring publish. When its peer observes `token == 1` via an **ACQ** load, every frame published before the close is already visible in the ring and is recovered by the single non-blocking drain pass (`SPEC 6.4`).

## 4. The Wait/Wake Handshake (Dekker Store-Buffering Pattern)

OS parking (`futex`, `__ulock_wait`, `WaitOnAddress`, or Windows semaphores) prevents idle consumers and backpressured producers from burning CPU, but introduces a classic lost-wake-up hazard unless the waiter and waker synchronize through a sequentially consistent Dekker handshake.

### 4.1 Handshake Sequence (`SPEC 8`)

For a consumer waiting on an empty ring (`NotEmpty`) and a producer publishing into that ring:

```text
Consumer (waiter) for NotEmpty                    Producer (waker)
------------------------------------              -------------------------------------------
claim loop found seq[s] < target (empty)          has just written packet + payload
W1  emptyWaiters.fetch_add(1)    [SC]             K1  seq[s].store(pos+1)             [SC]
W2  v = notEmpty.load()          [ACQ or SC]      K2  n = emptyWaiters.load()         [SC]
W3  if seq[s].load() < target:   [SC]             K3  if n != 0:
        futex_wait(&notEmpty, v, timeout)                 notEmpty.fetch_add(1)       [REL or SC]
W4  emptyWaiters.fetch_sub(1)    [RLX/REL]                wake_all(&notEmpty)
    re-run the claim loop
```

The backpressure direction (`fullWaiters` / `notFull`) is symmetric, with the producer acting as the waiter and the consumer's slot-release store acting as `K1`.

### 4.2 Why `W1`, `W3`, `K1`, and `K2` Must Be Sequentially Consistent

**The four operations `W1`, `W3`, `K1`, and `K2` `MUST` use `memory_order_seq_cst`** (or, equivalently, emit a `memory_order_seq_cst` thread fence between `W1` and `W3` and between `K1` and `K2`). `W2` `MUST` be at least `memory_order_acquire`, and the `fetch_add` in `K3` `MUST` be at least `memory_order_release`.

The interaction between `(W1, W3)` and `(K1, K2)` is a classic **store-buffering (SB)** litmus pattern: each party writes to one variable and then reads the other party's variable. Safety requires that **`K2 == 0` (waker sees no waiter) and `W3 < target` (waiter sees no published slot) can never occur in the same execution**.

Under release/acquire ordering alone, C11 and hardware store buffers permit both loads to read the initial values:

```text
initial: seq = pos (empty), emptyWaiters = 0

 Waiter W                                       Waker K
 W1: emptyWaiters.fetch_add(1)   (acq_rel)
                                                K1: seq.store(pos+1)           (release)
                                                K2: n = emptyWaiters.load()    (acquire)  -> reads 0
 W3: x = seq.load()              (acquire)      -> reads pos (stale value)
 x < target  =>  W parks (notEmpty has not changed)
 n == 0      =>  K skips K3 and never wakes W
 Result: W sleeps until its park timeout even though a published message sits in the ring.
```

Why `seq_cst` eliminates this interleaving:

- Under `seq_cst`, there exists a single total order `<_S` over `{W1, W3, K1, K2}` consistent with program order (`W1 <_S W3` and `K1 <_S K2`).
- If `K2` reads `0`, then `K2 <_S W1`, which by transitivity yields `K1 <_S K2 <_S W1 <_S W3`. Because `K1 <_S W3`, `W3` `MUST` observe `pos + 1` and return immediately without parking.
- Conversely, if `W3` reads the old value `pos`, then `W3 <_S K1`, which yields `W1 <_S W3 <_S K1 <_S K2`, forcing `K2` to observe `n >= 1` and execute `K3` to wake the waiter.

Hardware impact across CPU architectures:

- **x86-64 (TSO)**: Plain stores (`memory_order_release`) are buffered in the per-core FIFO store buffer and can be reordered after a subsequent load from a different address. Compiling `K1` as a plain `mov` (release) instead of `xchg` or `mov + mfence` (`seq_cst`) makes the lost-wake-up state reachable on real x86-64 hardware (verified by [`litmus/tso_sb_check.py`](litmus/tso_sb_check.py)).
- **AArch64**: While `stlr` followed by `ldar` forbids store-to-load reordering, compilers targeting ARMv8.3-A RCpc (`-march=armv8.3-a+rcpc`) may lower `memory_order_acquire` loads to `ldapr`, which hardware is permitted to reorder ahead of an earlier `stlr`. Requiring `memory_order_seq_cst` forces `stlr` + `ldar` (or `dmb ish`), preventing the reorder.
- **POWER and RISC-V**: Lightweight release/acquire barriers (`lwsync`, `fence r,rw` / `fence rw,w`) do not order store-to-load pairs; `seq_cst` emits full `hwsync` / `fence rw,rw` barriers.

Role of the futex compare word (`W2` and `K3`):

The wake word `notEmpty` closes the window between `W3` (deciding that the ring is still empty) and the kernel's atomic `if (*notEmpty == v) sleep` inside `futex_wait`. Sampling `v = notEmpty.load(ACQ)` in `W2` **after `W1` and before `W3`** ensures that if a waker finishes `K3` (`notEmpty.fetch_add(1, REL)`) before `W2`, the acquire load in `W2` synchronizes-with `K3` and makes `K1` visible to `W3`; if the waker executes `K3` after `W2`, the kernel's `futex_wait` comparison sees `*notEmpty != v` and returns `EAGAIN` immediately without sleeping.

### 4.3 Waiter Counter Semantics

`emptyWaiters` and `fullWaiters` count **parked processes** (at most one increment per process per direction regardless of thread count; `SPEC 8.1`). Over-counting (for example, after a parked process is killed with `SIGKILL`) causes harmless extra wake syscalls until the segment is re-formatted, whereas under-counting is a fatal correctness bug. Implementations `MUST NEVER` decrement a waiter counter they did not increment and `MUST NEVER` zero a waiter counter outside `Format`.

## 5. Heartbeats, Tokens, and Time Sources

All superblock and endpoint status words are 8-byte aligned 64-bit atomics:

- **Monotonic progress watches**: Heartbeat words (`heartbeat` and `initHeartbeat`) carry no payload dependencies; observers compare successive reads against their own local monotonic clock (`checkPeer` and the `Initializing` watch), so **RLX** loads and stores are sufficient.
- **Token CAS arbitration**: Secondary attachment (`0 -> token`), graceful detachment (`token -> 1`), and initialization claims (`initToken` CAS) use **AR** (**REL** on detach).
- **Ordered epoch publication**: In `commitInit`, the `initToken` commit CAS (`token -> 0`, **AR**) is followed by `epoch.fetch_add(1, AR)` and then `state.store(Ready, REL)` so any reader acquiring `state == Ready` observes the incremented `epoch`.
- **Clock separation**: Stored heartbeat values are wall-clock `UnixNano` timestamps used only for the initial open-time freshness hint (`heartbeatFresh`), whereas all timeout and liveness intervals are measured on the observer's local monotonic clock. Never subtract a stored heartbeat timestamp from a monotonic clock reading.
- **Process suspension**: A process paused via `SIGSTOP`, a debugger, or VM suspension for longer than `PeerTimeout` or `initStaleWindow` is treated as dead so surviving peers can recover or reclaim the segment (`SPEC 5.1, 6.2`). If the paused process later resumes, it detects replacement via `epoch` and `token` checks and returns `ErrPeerGone`.

## 6. Alignment and Lock-Free Atomic Requirements

Every conforming implementation must enforce the following alignment and hardware-atomicity constraints:

- **Natural alignment**: The shared-memory mapping base is page-aligned (`>= 4096` bytes), every `u64` control word is 8-byte aligned, and every `u32` wake word/counter is 4-byte aligned. Inside a 128-byte ring slot, `seq` is at offset `0` (8-byte aligned) and the 64-byte packet is at offset `8` (8-byte aligned, not 16-byte or 64-byte aligned).
- **Mandatory address-free lock-free atomics**: Compile-time assertions `MUST` verify `ATOMIC_INT_LOCK_FREE == 2` and `ATOMIC_LLONG_LOCK_FREE == 2` (in C), `std::atomic<uint64_t>::is_always_lock_free` (in C++), or target pointer width / atomic support (in Go and Rust). Platforms that emulate 64-bit atomics with process-private locks (`mips`, `mipsle`, pre-ARMv7 `arm`) are unsupported and `MUST` fail at compile time.
- **No process-private synchronization in shared memory**: Shared words must never be guarded by `pthread_mutex_t` or language-runtime mutexes stored in process-private memory, and must never be accessed through `volatile` casts instead of atomic intrinsics.

## 7. Endianness

All shared-memory control words (`u64`, `u32`), ring headers, and 64-byte packets are **little-endian**. Big-endian architectures are unsupported and `MUST` be rejected at compile time (`#if __BYTE_ORDER__ != __ORDER_LITTLE_ENDIAN__ #error`). In memory, the superblock magic `0x4851515F53544C33` appears as the byte sequence `33 4c 54 53 5f 51 51 48` (`"3LTS_QQH"`), and the ring magic `0xC9D8C1D43F096702` appears as `02 67 09 3f d4 c1 d8 c9`.

## 8. Payload Access Discipline

To preserve the happens-before guarantees in section 3 without data races or undefined behavior:

1. **Exclusive slot ownership**: A producer may read or write packet `s` and payload buffer `s` only after claiming `pos` (`seq[s] == pos`) and before executing `seq[s].store(pos + 1, SC)`. A consumer may read packet `s` and payload buffer `s` only after acquiring `seq[s] == pos + 1` and before executing `seq[s].store(pos + mask + 1, SC)`.
2. **No dangling pointers after release**: Zero-copy reservations and callbacks `MUST` complete all reads and copies before releasing the slot and `MUST NOT` retain pointers, slices, or references into the shared buffer after release.
3. **Slot index derivation**: Always compute the buffer index from the ring position (`s = pos & mask`), never from an untrusted packet operand.
4. **Hardware memory barriers**: Always use true atomic release/acquire/SC operations (`stlr`/`ldar`, `xchg`, `fence`), never compiler-only barriers (`asm volatile("" ::: "memory")`).
5. **No aliasing violations**: In Rust and C++, access shared payload buffers via raw pointers (`copy_nonoverlapping` / `std::memcpy`) or byte slices scoped strictly to the active reservation lifetime while no peer owns the slot.
6. **Exact 64-byte packet copy**: Copy the 64-byte packet starting at slot offset `8`; never overwrite the leading 8-byte `seq` word at offset `0`.
7. **`MAP_SHARED` mapping**: Shared segments `MUST` be mapped with `MAP_SHARED` (POSIX) or `MapViewOfFile` (Windows) so writes propagate through the shared physical page cache.

## 9. Cache-Line Isolation and Page Geometry

- **128-byte ring header lines**: Separating immutable metadata (line 0), consumer cursor `r` (line 1), producer cursor `w` (line 2), and wait/wake counters (line 3) onto distinct 128-byte boundaries prevents false sharing on both 64-byte (x86-64, most ARM64) and 128-byte (Apple Silicon M-series P-cores) cache-line architectures.
- **128-byte slot stride**: Padding each 72-byte slot (`8`-byte `seq` + `64`-byte packet) to 128 bytes ensures adjacent ring slots never share a cache line.
- **Page-aligned regions**: The superblock, both ring headers + slot arrays, and both payload buffer pools start on OS page boundaries (`SPEC 2.1`). Because region offsets depend on `pageSize` (4 KiB, 16 KiB, or 64 KiB), both endpoints on a machine validate that the superblock's `pageSize` equals `os.Getpagesize()`.

## 10. Verification Checklist for Conforming Ports

Every language implementation must pass the following nine verification gates (see [`conformance/README.md`](../conformance/README.md) and [`spec/toolchain/VERIFICATION_TOOLING.md`](toolchain/VERIFICATION_TOOLING.md)):

1. **Compile-time layout assertions**: Verify all structure sizes, field offsets (`spec/constants.json`), `ATOMIC_LLONG_LOCK_FREE == 2`, and little-endian byte order at compile time.
2. **Golden conformance vectors (`conformance/vectors/`)**: Pass all packet, layout, geometry, name, error-code, ring-image, superblock-image, golden-sequence, and Advanced frame-matrix vectors.
3. **Slot protocol unit tests**: Verify sequence publication, full/empty detection, FIFO ordering, multi-lap reuse, and `pos` wraparound across `2^64 - 2`.
4. **Multi-thread and multi-process ring stress**: Exchange checksummed variable-size payloads across tiny rings (`bufferCount` 2 and 4) on both `x86_64` and `arm64` hardware.
5. **Dekker lost-wake-up stress**: Run a parking consumer against a burst-then-idle producer on a 2-slot ring with a strict per-message latency watchdog to confirm zero lost wake-ups.
6. **Cross-language interop matrix (`interop/`)**: Pass all `X-01` through `X-21` and `X-A01` through `X-A10` scenarios in both role directions against the Go and C reference helpers.
7. **Crash-point recovery**: Verify clean recovery when a peer is killed with `SIGKILL` while parked (`W1..W4`), while waking (`K1..K3`), or mid-initialization (`claim`, `format`, `publish`, `zeroing`).
8. **Sanitizers**: Pass AddressSanitizer, UndefinedBehaviorSanitizer, and ThreadSanitizer (or Go `-race`) across the test suite.
9. **Formal model checking**: Verify the ring and handshake C11 atomic operations under GenMC (`c/verify/genmc/`) and `herd7` (`spec/litmus/`).
