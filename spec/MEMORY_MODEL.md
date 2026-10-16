# HQQ Memory Model

Required memory ordering for every word of a shared HQQ segment, the happens-before argument for payload bytes, the wait/wake (Dekker) handshake and what a conforming port must test. Companion to [SPEC.md](SPEC.md); section numbers `SPEC x.y` refer to it. Litmus tests: [litmus/](litmus/).

## 1. Vocabulary and mapping between languages

Orders are given in C11/C++11 terms and map to the other languages as follows.

| Order | C11 `<stdatomic.h>` | C++ `std::atomic` | Rust `core::sync::atomic::Ordering` | Zig `std.builtin.AtomicOrder` |
| --- | --- | --- | --- | --- |
| **RLX** relaxed (atomic, no ordering) | `memory_order_relaxed` | `memory_order_relaxed` | `Relaxed` | `.monotonic` |
| **ACQ** acquire (loads) | `memory_order_acquire` | `memory_order_acquire` | `Acquire` | `.acquire` |
| **REL** release (stores) | `memory_order_release` | `memory_order_release` | `Release` | `.release` |
| **AR** acquire-release (RMW) | `memory_order_acq_rel` | `memory_order_acq_rel` | `AcqRel` | `.acq_rel` |
| **SC** sequentially consistent | `memory_order_seq_cst` | `memory_order_seq_cst` | `SeqCst` | `.seq_cst` |
| **PLAIN** non-atomic access | ordinary load/store, `memcpy` | ordinary / `std::memcpy` | raw-pointer `read`/`write`/`copy_nonoverlapping` (never `&`/`&mut` to memory another process may change) | ordinary / `@memcpy` |

The Go reference implementation uses `sync/atomic`, whose operations are all sequentially consistent. **Every requirement below is therefore met trivially by the reference; a port in another language chooses the orders itself and MUST NOT choose a weaker order than the "required minimum" column.** Choosing SC everywhere on cold-path words (superblock, endpoint records, ring header) is allowed and RECOMMENDED for the reference C library: those words are touched at attach time and at heartbeat rate only.

C11 is a model of threads in one process. HQQ relies on the standard's statement that lock-free atomic operations should be **address-free** (C11 7.17.5.1 p2), i.e. "atomic operations on the same memory location via two different addresses will communicate atomically": two processes mapping the same physical page are treated as two threads. This is the practical contract of every mainstream implementation for `MAP_SHARED` mappings and Windows file-mapping views; it is why section 6 requires lock-free 64-bit atomics.

## 2. Every shared word: writer, reader, order

"Writer"/"Reader" name the party; **I** = the endpoint that currently owns `initToken` (the initializer), **C** = any endpoint running the claim loop, **E** = the owner of an endpoint record, **Q** = its peer, **P** = ring producer, **K** = ring consumer. Required minimum orders; **SC is always allowed instead**.

### 2.1 Superblock (SPEC 3)

| Word | Writer | Reader | Store / RMW | Load | Why |
| --- | --- | --- | --- | --- | --- |
| `magic` @0 | I at publish (P5) | C | RLX (the later `state` release covers it); REL allowed | RLX in the `Initializing` branch (decision uses it only to reject foreign layouts); covered by the `state` ACQ in the `Ready` branch | Written before the commit CAS; readers that matter observe it through `state == Ready`. |
| `state` @8 | C: CAS `Empty→Initializing`, CAS `Ready→Initializing` (AR); I: store `Ready` (**REL**) | C, all monitors | store **REL**, CAS **AR** | **ACQ** | `Ready` is the release point of the whole initialization: every store of the initializer (rings formatted, buffers zeroed, geometry, records, `magic`, `epoch`) happens-before an endpoint that acquires `Ready`. |
| `major`, `minor`, `requiredFeatures` @16, 24, 88 | I at claim (`stampLayout`) and at publish | C | RLX (stamped before the `initToken` CAS; REL for the second stamp covered by `state`) | RLX in `Initializing`; covered by `state` ACQ in `Ready` | The layout check on an `Initializing` segment is a heuristic on a possibly half-written layout; it rejects only a *conflicting* value. |
| `pageSize`, `bufferCount`, `bufferSize`, `layoutSize` @32..56 | I at publish | joiners (after `Ready`) | RLX (covered by the `state` REL) | covered by `state` ACQ | Geometry is immutable while the segment is `Ready`. |
| `epoch` @64 | I: `fetch_add` at commit | joiners (after join), every monitor (`checkPeer`) | **AR** | ACQ (RLX is enough for the equality test, but the Secondary's initial `epoch` read follows its join CAS) | Incremented exactly once per successful initialization, after the commit CAS and before `state = Ready`; an endpoint compares it to detect "re-initialized under me". |
| `initToken` @72 | C/I: CAS `expected → token` (`beginInit`), CAS `token → 0` (commit) | C (watch), I (ownership check) | CAS **AR** | ACQ for the ownership check; RLX for the progress watch | It is the initialization ownership register. The commit CAS orders all publish stores before the release of ownership. |
| `initHeartbeat` @80 | I (progress) | C (watch) | RLX | RLX | Only compared for *change* on the reader's own monotonic clock; no data is published through it. |
| `creatorToken` @96 | I right after winning `initToken` | any (`ownsName`, join) | RLX (covered by `state` REL for readers that joined) | covered by `state` ACQ; RLX for `ownsName` | Identity of the instance. |
| reserved @104..127, unused to end of page | nobody | firstPageZero (C) | — | RLX/ACQ (each word atomically) | Must stay zero: an `Empty` segment with any non-zero word in the page is foreign (SPEC 5.1). |

### 2.2 Endpoint records (SPEC 3.2), one per endpoint

| Word | Writer | Reader | Store / RMW | Load | Why |
| --- | --- | --- | --- | --- | --- |
| `token` | E at publish (Primary: store; Secondary: CAS `0 → token`, **AR**); E at close: CAS `token → 1` (**REL**); I at publish: clear Secondary record | C, Q (`checkPeer`) | CAS **AR** / REL; plain atomic store at publish | **ACQ** | Ownership and liveness; detach is the release point of "peer has left" (a Q that sees `1` also sees every ring slot E published before closing, which makes the drain rule of SPEC 6.4 sound). |
| `pid` | E at join/publish | tools | RLX | RLX | Diagnostic only. |
| `heartbeat` | E every interval (only while `token == mine`) | C (liveness at open), Q | RLX | RLX | Compared for change / age only; the monitor's clock is local and monotonic. No data depends on it. |
| `interval` | E at join/publish | C, Q | RLX | RLX | A reader that sees `token != 0` with `interval == 0` or `heartbeat == 0` is told "alive / heartbeats disabled" (SPEC 6.2): the record is filled after the token CAS, so readers MUST tolerate the partial state. |

### 2.3 Ring header (SPEC 7.1)

| Word | Writer | Reader | Store / RMW | Load | Why |
| --- | --- | --- | --- | --- | --- |
| `magic`, `size`, `stride`, `version` (line 0) | I in `Format` | P, K in `Attach` | RLX, in the order of SPEC 7.2 | RLX after the `flag` ACQ | Immutable after `Format`. |
| `flag` (line 0) | I: `0` first, `2` **last** in `Format` | `Attach` | `0` RLX; `2` **REL** | **ACQ** (and `magic` ACQ before it) | Releases the whole formatted ring (slot images, cursors, counts) to attachers; a half-built ring is never accepted. |
| `r` (consumer cursor) | K (CAS or store) | K only (cross-process: nobody else) | RLX (CAS success RLX) | RLX | Cursors only arbitrate among threads of the same side. The slot's ownership is conveyed by `seq`, not by the cursor. |
| `w` (producer cursor) | P | P only | RLX | RLX | Same. |
| `notEmpty` (u32 @384) | P: `fetch_add(1)` after publish when `emptyWaiters != 0`; `Format`; `WakeAll` | consumer parkers (W2) and the kernel | **REL** (SC allowed and used by the reference) | **ACQ** (W2) | The wake word is the futex compare value; its change after the waker saw a registered waiter releases the waker's `seq` store to the waiter (SPEC 8.3). |
| `emptyWaiters` (u32 @388) | K: `fetch_add(+1)` (W1), `fetch_add(-1)` (W4); `Format` | P (K2) | W1 **SC**; W4 RLX or REL | K2 **SC** | Registration of parked processes; half of the Dekker pair (section 4). |
| `notFull` (u32 @392) | K: `fetch_add(1)` after release when `fullWaiters != 0` | producer parkers | **REL** | **ACQ** | As `notEmpty`, other direction. |
| `fullWaiters` (u32 @396) | P: W1/W4 | K | W1 **SC**; W4 RLX/REL | K2 **SC** | As `emptyWaiters`. |

### 2.4 Slots and payload (SPEC 7.1, 7.4, 9.4)

| Word | Writer | Reader | Store / RMW | Load | Why |
| --- | --- | --- | --- | --- | --- |
| `seq[i]` (u64, slot offset 0) | `Format` (initial `i`); P at publish (`pos + 1`); K at release (`pos + mask + 1`) | P and K (claim loops), waiters (W3) | `Format`: RLX (covered by `flag` REL); publish/release: **SC** (REL + Dekker, section 4) | claim loops: **ACQ**; wait re-check W3: **SC** | The only synchronization word of the data path. The release store publishes everything the claimant did to the packet and to buffer `i`; the acquire load in the other side's claim imports it. |
| packet (64 bytes, slot offset 8, `PLAIN`) | P between claim and publish | K between claim and release | PLAIN | PLAIN | Exclusively owned by the slot's claimant; ordered by `seq`. Word accesses are 8-byte aligned. A port MAY use relaxed 64-bit atomics here but is not required to. |
| payload buffer `i` (`PLAIN`) | P between claim and publish; I (zeroing, before `Ready`) | K between claim and release | PLAIN | PLAIN | Same as the packet: slot ownership is buffer ownership. |

## 3. Publish / consume and the payload happens-before argument

**Claim 1 (data visible to the consumer).** Let producer `P` own position `pos` and slot `s = pos & mask`. `P` performs, in program order: (a) plain writes of buffer `s` and of the packet, then (b) `seq[s].store(pos + 1, SC)` (which is at least release). Consumer `K` claims `pos` after `seq[s].load(ACQ)` returned `pos + 1` (the value written in (b)): that load *reads from* the release store (b), so (b) *synchronizes-with* the load, and every write of (a) *happens-before* `K`'s subsequent plain reads of the packet and of buffer `s`. `K` therefore reads the bytes `P` wrote. Because `seq` values are unique per lap, the load cannot read a store of another lap.

**Claim 2 (slot reuse is safe).** `K` finishes reading and executes `seq[s].store(pos + mask + 1, SC)` (release). The producer of position `pos + size` (the next lap of slot `s`) claims it only after `seq[s].load(ACQ)` returned `pos + size`. By the same synchronizes-with edge, `K`'s plain *reads* of the packet and buffer happen-before the new producer's plain *writes*. A reader never observes a half-overwritten packet, and a writer never overwrites bytes still being read. **Both edges are required**: omitting the acquire on the producer's claim load, or making the consumer's release relaxed, turns payload accesses into data races (litmus `slot_wraparound_handoff*.litmus`, `mp_publish_*.litmus`).

**Claim 3 (cursor words do not matter for data).** The `CAS` on `w` (producer claim) only decides *which thread of the producer side* owns position `pos`; it can be relaxed because the data path is governed by `seq`. A successful CAS does not imply that `seq[s]` is ready; the claim loop re-reads `seq` with acquire *before* the CAS and the CAS succeeds only if no other thread of the side took `pos` in between, so the observed `seq == pos` still stands (the slot cannot change while `w == pos`: only the claimant of `pos` writes it).

**Claim 4 (initialization).** The initializer's plain `memset` of the payload pools, its `Format` stores and its stores into the superblock all precede `state.store(Ready, REL)` in program order (SPEC 5.3). An endpoint that sees `Ready` with `state.load(ACQ)` observes all of them; therefore consumers never see stale payload from a previous session, and the first producer never races with the zeroing. The reuse CAS `Ready → Initializing` and the commit CAS on `initToken` (both AR) are the only other publication points.

**Claim 5 (drain after detach).** `token.cas(mine → 1, REL)` follows, in program order, every publish of the closing endpoint. A peer that loads `token == 1` with ACQ therefore also sees every slot published before the close, and a final non-blocking dequeue after observing the close (SPEC 6.4) returns them.

The Go implementation is `-race`-clean: the race detector models the atomics as synchronization, and the payload accesses it instruments are exactly the plain accesses above (the rings live in `MAP_SHARED` or heap memory with the same discipline).

## 4. The wait/wake handshake (Dekker pattern)

Parking exists so that a blocked process does not burn CPU. It introduces a classic lost-wake-up hazard, which the protocol closes with a store/load handshake on two memory words in each direction.

### 4.1 The protocol (SPEC 8)

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

The mirrored pair (`fullWaiters`/`notFull`) is identical with the roles exchanged: the consumer's *release* is `K1`, the producer is the waiter.

### 4.2 What must be sequentially consistent, and why

**The four accesses W1, W3, K1, K2 MUST be `seq_cst`** (or, equivalently, `W1`/`K1` may be release/relaxed RMW/store if each is *followed by a `seq_cst` fence before the other word is loaded*, i.e. `K1; fence(SC); K2` and `W1; fence(SC); W3`). `W2` MUST be at least acquire and `K3`'s `fetch_add` at least release (they carry the futex compare value). `W4` and the read-modify-write on cursors MAY be relaxed.

The pattern of `(W1, W3)` against `(K1, K2)` is **store buffering**: each side stores to one location and then loads the *other* location. The correctness property is "it is impossible that K2 reads `0` (the waker sees no registered waiter) **and** W3 reads the old `seq` (the waiter sees no data and therefore parks)".

**Why release/acquire is insufficient.** C11's release/acquire (and the hardware models below it) allow both loads to read stale values, because no total order orders the two stores relative to the two loads. Failing interleaving:

```text
initial: seq = pos (empty), emptyWaiters = 0

 Waiter W                                       Waker K
 W1: emptyWaiters.fetch_add(1)   (acq_rel)
                                                K1: seq.store(pos+1)           (release)
                                                K2: n = emptyWaiters.load()    (acquire)  -> reads 0
 W3: x = seq.load()              (acquire)      -> reads pos (the old value)
 x < target  =>  W parks (the futex word has not changed)
 n == 0      =>  K never bumps the word and never wakes
 Result: W sleeps until its park timeout (100 ms in the Go implementation; forever in a port
         that parks without a timeout).  A published packet sits in the ring unannounced.
```

Nothing in release/acquire forbids this outcome: K2 is not ordered after W1 (no synchronizes-with edge: K2 reads the *initial* value, not W1's store), and W3 is not ordered after K1 (it reads the old value). Herd-style result: [`litmus/wake_handshake_acqrel.litmus`](litmus/wake_handshake_acqrel.litmus) has the outcome `0:r0=0 /\ 1:r1=0` **allowed**; [`litmus/wake_handshake_sc.litmus`](litmus/wake_handshake_sc.litmus) and [`litmus/wake_handshake_fences.litmus`](litmus/wake_handshake_fences.litmus) **forbid** it. With a single total order `S` over the four SC accesses: `K1 <S K2` and `W1 <S W3` by program order. If K2 read `0`, K2 precedes W1 in `S` (else it would see W1's increment), so `K1 <S K2 <S W1 <S W3`, hence W3 is after K1 in `S` and must read `pos+1` (the slot is ready, no park). Otherwise `W1 <S K2`, K2 reads `>= 1`, the waker bumps the word and wakes. In both cases the waiter either does not park or is woken.

**Hardware reality** (why a "works on my x86" port is wrong):

* **x86-64 (TSO):** a plain `mov` store followed by a `mov` load can reorder (the store sits in the store buffer). Go and C `seq_cst` stores compile to `xchg`/`mov+mfence`; a C port that publishes with `memory_order_release` (`mov`) and then loads the waiter count has the failing interleaving **on x86**. The waiter's `fetch_add` is a `lock xadd` and is fully fenced, so only the waker side is exposed, but that is enough.
* **AArch64:** `stlr` (release store) followed by `ldar` (acquire load) is *not* reordered (the RCsc subset), but `ldapr` (RCpc acquire, emitted by compilers for `memory_order_acquire` with `-march=armv8.3-a+rcpc`/LSE2 targets) **may** be reordered with an earlier `stlr`. `seq_cst` compiles to `stlr`/`ldar`, which gives the required order; `release`/`acquire` may not.
* **POWER, RISC-V:** `release` store and `acquire` load are separated by lightweight fences that do not order store→load; `seq_cst` adds `hwsync`/`fence rw,rw`.

**Where the futex word fits.** The wake word `notEmpty` is not part of the SB pair; it closes a *different* window: between W3 (decision to park) and the kernel's atomic "if `*word == v` sleep". Without the word, a wake that happens in that window is lost. With it, the waker's `K3` changes the word *after* having seen `n != 0` (so after W1), and the kernel's compare at park time sees the change. The snapshot W2 must be taken **after W1** and **before W3**, and W2 must be acquire: if W2 reads the post-bump value, the acquire makes K1 visible to W3 (K1 is sequenced before K3's release `fetch_add`), so W3 sees the new `seq` and does not park; with a relaxed W2 that window re-opens. With a semaphore `Parker` (Windows) the persistent token plays the role of the word and W2 is not needed, but W1/W3/K1/K2 remain SC.

### 4.3 Counts are processes, correctness does not need exactness

`emptyWaiters`/`fullWaiters` are *hints*: a waker only needs to know "is anybody possibly parked". They count **processes** (each waiting process registers once, regardless of how many of its threads are blocked; SPEC 8.1). Over-counting costs spurious wakes. A killed parked process leaves its count raised until `Format`. **Under-counting is a correctness bug**: never decrement a count you did not increment, and never reset it outside `Format`.

## 5. Heartbeat, tokens and state words

All of them are 64-bit atomics; **no tearing**. Their orders are in section 2. The additional rules:

* **Liveness is derived, not synchronized.** Heartbeats and `initHeartbeat` carry no data. A monitor compares two successive loads on its *own monotonic clock* (`checkPeer`, the Initializing watch); a stale cached value merely delays a decision. Therefore RLX is sufficient and no fences are needed.
* **Token CAS is the only arbitration** on endpoint records: Secondary join (`0 → token`), detach (`token → 1`), and the Primary's publish store (done while the initializer owns `initToken`, hence exclusive). Use AR.
* **`state` store `Ready` and the `epoch` increment are ordered**: the commit CAS on `initToken` is followed by `epoch.fetch_add`, then `state.store(Ready, REL)`. A reader that sees `Ready` sees the new epoch.
* **Clock sources.** Wall clock (`CLOCK_REALTIME`, UnixNano) is written into `heartbeat`/`initHeartbeat`; any time-based decision a monitor makes on its own uses a monotonic clock. Never compare a stored wall-clock value to the local monotonic clock.
* **A process that is frozen** (SIGSTOP, debugger, VM pause) longer than the initialization-stale window or the peer timeout *will* be treated as dead, and the segment may be taken over or reused under it (SPEC 5.1, 6.2). It detects this on resume through `epoch`/`token` (`ErrPeerGone`) — but see FINDINGS F-01 for the one hazard this leaves.

## 6. Alignment and 64-bit atomics

* Every `u64` word of the superblock, the endpoint records, the ring header and the slot sequence number is **8-byte aligned** at its fixed offset from a **page-aligned** segment base (all offsets in `constants.json` are multiples of 8; wake words and waiter counts are `u32`, 4-aligned). The packet inside a slot is at slot offset 8 (8-aligned, **not** 64- or 16-aligned); read/write it with `memcpy` or 8-byte accesses only.
* **Lock-free 64-bit atomics that work across processes are required**: `_Atomic(uint64_t)` with `ATOMIC_LLONG_LOCK_FREE == 2`, `atomic_is_lock_free(&x)` true (C), `std::atomic<uint64_t>::is_always_lock_free` (C++), `AtomicU64::from_ptr` plus a platform check (Rust ≥ 1.75), `@atomicLoad(u64, ...)` on a target with native 64-bit atomics (Zig). On a 32-bit target insist on native double-word atomics (x86 `cmpxchg8b`, ARMv7 `ldrexd`/`strexd`) and **force 8-byte alignment** (`alignas(8)`; on i386 a `uint64_t` struct member is only 4-aligned by default).
* **Unsupported platforms:** a platform where 64-bit atomics are emulated with process-local locks silently loses mutual exclusion between processes. These are 32-bit MIPS (`mips`, `mipsle`) and ARM before ARMv7 (ARMv5/6). The Go implementation refuses to compile there (`arch_unsupported.go`); a port MUST do the same (`#error`, `compile_error!`, `@compileError`).
* **No lock, no emulation**: a port must not protect shared words with a mutex, a futex-based lock, or `pthread` primitives that live in process-private memory.
* Use the addresses of the shared words directly (`atomic_load_explicit((_Atomic uint64_t *)(base + off), ...)`); never keep a second *non-atomic* mapping of the same memory for access to these words, and never access them through `volatile` instead of atomics.

## 7. Endianness

All words (`u64`, `u32`, packet words) are **little-endian in memory**. The Go code stores shared words with native atomics and packets with `binary.LittleEndian` and never byte-swaps shared words, so **big-endian hosts are unsupported**. A port MUST refuse to build on a big-endian target (`#if __BYTE_ORDER__ != __ORDER_LITTLE_ENDIAN__ #error`) rather than byte-swap, because byte-swapping the atomically accessed words is not atomic as a read-modify-write unit and the Go side would disagree on magic constants. In memory the superblock magic reads as the bytes `33 4c 54 53 5f 51 51 48` (`"3LTS_QQH"`); the ring magic `0xC9D8C1D43F096702` as `02 67 09 3f d4 c1 d8 c9`.

## 8. Payload access rules

1. Payload buffers and packets are **plain memory** that is accessed **only while owning the slot**: a producer between its claim and its publish, a consumer between its claim and its release, and the initializer before `Ready`. At any time at most one party may access a given buffer.
2. A consumer MUST finish all reads (including any `memcpy` to its own storage) *before* the release store; it MUST NOT keep pointers or views into the buffer afterwards (the Go reservations and zero-copy callbacks document this as "invalid after Release"). A producer MUST NOT touch the buffer after the publish store.
3. The slot index is **always derived from the position**, never from packet operand 1.
4. Before the release/publish store, the compiler and the CPU must not move payload accesses across it: use a real release/seq_cst store, not a relaxed store plus a compiler barrier (a compiler barrier is not a CPU barrier on weakly ordered hardware).
5. Do not form language-level references/slices that alias memory owned by another process or another slot for longer than the claim (Rust: raw pointers; C++: no `std::span` over memory you do not own).
6. The packet is 64 bytes at slot offset 8; the 8 bytes before it are `seq`. Never write `seq` as part of a packet copy: copy exactly 64 bytes starting at slot offset 8.
7. Do not access the payload with atomics of a different width than the other side (there is none specified; plain `memcpy` is the contract).
8. `MAP_SHARED` is required (a `MAP_PRIVATE` mapping would give copy-on-write pages that never reach the peer). Do not cache the mapping address in a way that survives `mremap`/`munmap`.

## 9. False sharing, cache lines, pages

* **Header lines are 128 bytes** so that the cursors (`r`, `w`) and the wait words/counters never share a line, on machines with 64-byte or 128-byte lines (some AArch64 cores). Immutable fields share line 0; `r` is alone on line 1; `w` alone on line 2; the four wake words share line 3 because they are only written when a waiter parks or has to be woken.
* **Slots** are 128 bytes apart (`stride = round_up(8 + 64, 64)`): `seq` and the first seven packet words are in the first 64-byte line of the slot, the last packet word (operand 6) and padding in the second. Neighbouring slots never share a line. The packet is therefore not line-aligned (8-byte offset); this is part of the ABI (FINDINGS F-14).
* Producer and consumer write different lines of the same slot in the common case (producer: `seq`+packet; consumer: `seq` only) — an unavoidable transfer per message.
* **Page-size dependence:** the superblock occupies one page and every region starts on a page boundary, so the offsets depend on the page size (SPEC 2.1). 4 KiB, 16 KiB (Apple Silicon, some AArch64 Linux) and 64 KiB (POWER, some AArch64) pages occur in practice; segments are valid only among endpoints that agree on the page size. Huge pages are not part of the layout.
* A port SHOULD keep its own per-endpoint state (cached cursors, counters) on lines that do not overlap the shared header.

## 10. What a conforming port must test

The parent will place these under `conformance/`; each is necessary, none is sufficient alone.

1. **Static layout checks** (compile time): every offset and size of `constants.json` (`_Static_assert(offsetof(...))`, `const_assert!`, `comptime`), `sizeof(superblock) == 384`, `sizeof(packet) == 64`, header 512, stride 128, `ATOMIC_LLONG_LOCK_FREE == 2`, little-endian.
2. **Golden vectors**: the 9 packet vectors and the 12 layout vectors for three page sizes of `constants.json`; parse a Go-written segment image (`TestSpecSegmentImage`'s image) and produce a byte-identical image for the same operation sequence.
3. **Slot protocol unit tests** equal to `internal/mpmc/spec_constants_test.go`: publish/release `seq` values, full/empty, FIFO, second lap, wraparound of `pos` near `2^64` (start positions `2^64 - 2`).
4. **Two-thread and two-process stress** (bounded, a few seconds): producer writes `n` messages each carrying a counter and a checksum of its payload; the consumer verifies order, integrity and that the payload was never torn; ring of 2 and 4 slots to force constant wraparound and constant full/empty; run on x86-64 **and** AArch64 hardware (weak ordering bugs do not show on x86 for acquire/release errors in the data path; Dekker errors do show on x86 for a `release`-only waker).
5. **Dekker stress / lost-wake-up test**: tiny ring, one parking consumer, one producer that publishes then immediately idles; assert that every message is delivered within the park timeout and, with the timeout set to an hour and a watchdog that forces the test to fail if a message takes more than a small multiple of scheduling delay, that no wake-up is ever lost (this test fails quickly on x86 when K1 is a release `mov`). Also run it with the peer being the Go implementation.
6. **Interop matrix** (Go ⇄ port, both roles): create/open, negotiate, 1 B, bufferSize, bufferSize+1, multi-chunk 64 MiB message, request/response with an error response, stream open/close, peer crash (`kill -9`) detection within the peer timeout, graceful close drain, takeover of a segment whose initializer died (SIGKILL between claim and publish), reuse after both endpoints died, `ErrLinkBusy` for a third endpoint, geometry mismatch.
7. **Kill/crash recovery**: kill the waker between K1 and K3 (stale count) and the waiter between W1 and W4 (count raised); the surviving endpoint must still make progress (bounded parks) and a re-`Format` must clear the counts.
8. **ThreadSanitizer / Miri / loom (where available)** on the single-process two-thread version of the ring to detect plain-memory races on the payload.
9. **Litmus**: [herd7](litmus/RESULTS.md) results for the files in `litmus/` (a port's chosen orders must fall in the allowed set of the SC variants; the `*_bug` variants must remain bugs).
