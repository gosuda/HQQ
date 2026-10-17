# HQQ Wire and Memory Protocol — Normative Specification

Baseline: Go implementation v0.5.0 (segment layout 3.0, magic `HQQ_STL3`; MPMC ring layout 2; protocol 1.0).
Companion documents: [MEMORY_MODEL.md](MEMORY_MODEL.md) (memory orders), [PORTING.md](PORTING.md) (how to write a port), [FINDINGS.md](FINDINGS.md) (defects and ambiguities), [constants.json](constants.json) (every numeric constant, machine readable and checked against the code by `go test -run Spec`), [litmus/](litmus/) (C11 litmus tests).

This document was derived from the code, not from `PROTOCOL.md`. Where the two differ, this document and the code win; known differences are listed in FINDINGS.md. It is written so that a second implementation (for example a C library) can be written from it alone and interoperate with the Go implementation in the same segment.

## 0. Conventions

The key words **MUST**, **MUST NOT**, **REQUIRED**, **SHOULD**, **SHOULD NOT**, **MAY**, **RECOMMENDED** are used as in RFC 2119.

Every rule carries one of two scope tags:

| Tag | Meaning |
| --- | --- |
| **[IOP]** | Interoperability-normative. It affects the bytes in shared memory, the order or atomicity of accesses to them, or behavior another process can observe. Two implementations that differ here can corrupt each other's state or deadlock. |
| **[IMPL]** | Implementation freedom or recommendation. Local threading, scheduling, caching, API shape, wait strategy and similar. Another process cannot tell the difference (or only through latency). |

Untagged prose is explanatory.

Notation: `u64`/`u32` are unsigned integers of that width stored **little-endian** and naturally aligned. `load`, `store`, `cas`, `fetch_add` are atomic accesses of the stated word with the memory order given in MEMORY_MODEL.md. `a - b` on `u64` wraps modulo 2^64; `int64(x)` reinterprets the bits as two's complement. `round_up(x, p)` is the smallest multiple of `p` that is `>= x`. `⌊ ⌋` is not used; all divisions are exact or stated.

Numeric values of constants are in [constants.json](constants.json). When this text and the JSON disagree, the JSON (which is tested) wins and this text is a bug.

## 1. Terminology and topology

* **Segment**: one contiguous block of shared memory (a named POSIX shared-memory object, a Windows named file mapping, or caller-owned memory such as a `MAP_SHARED` mapping) that carries one link.
* **Link**: a bidirectional connection between exactly two **endpoints**: the **Primary** (endpoint index 0) and the **Secondary** (endpoint index 1).
* **Direction 0** carries Primary → Secondary traffic (ring 0, buffer pool 0); **direction 1** carries Secondary → Primary traffic (ring 1, buffer pool 1). Each direction is one **ring** (a bounded Vyukov MPMC queue of 64-byte packets) plus one **buffer pool** of `bufferCount` fixed-size payload buffers; slot `i` of the ring owns buffer `i` of the pool.
* A **process** is the unit of sharing: all threads of one process on one side of a ring are one party.
* **Position** (`pos`): a `u64` that counts claims on a ring cursor; the slot of position `pos` is `pos & mask` with `mask = size - 1`. A **lap** is `pos / size`.
* **Roles** of an endpoint when it reaches a segment (section 4): **Creator** (created or recreated the name), **Opener** (attaches to an existing name), **InMemory** (caller-owned memory).

**[IOP] Cross-process topology.** For every ring there is exactly **one producer process and one consumer process**: ring 0 has the Primary as producer and the Secondary as consumer, ring 1 the other way round. A third endpoint is never admitted (section 5.5). Within one process any number of threads MAY produce or consume, provided the process as a whole behaves as one producer / one consumer with respect to the shared words (see 7.6, "SPSC mode").

## 2. Segment memory layout

### 2.1 Regions **[IOP]**

```text
offset 0                SUPERBLOCK   one page, 384 bytes used (section 3)
 PAGE_BREAK
ring0                   RING 0       header + slots, direction 0 (Primary -> Secondary)
 PAGE_BREAK
buf0                    BUFFERS 0    bufferCount * bufferSize bytes, direction 0
 PAGE_BREAK
ring1                   RING 1       direction 1 (Secondary -> Primary)
 PAGE_BREAK
buf1                    BUFFERS 1    direction 1
 PAGE_BREAK (end)
total
```

With `page = system page size` (`sysconf(_SC_PAGESIZE)`, `GetSystemInfo().dwPageSize`; a power of two, at least 4096), `count = bufferCount`, `size = bufferSize`:

```text
ringBytes = 512 + 128 * count                  (header 512 bytes, slot stride 128 bytes)
bufBytes  = count * size
ring0     = round_up(384, page)                (= page, for every page size >= 4096)
buf0      = round_up(ring0 + ringBytes, page)
ring1     = round_up(buf0  + bufBytes,  page)
buf1      = round_up(ring1 + ringBytes, page)
total     = round_up(buf1  + bufBytes,  page)
```

All offsets are from the start of the segment, which MUST be page aligned. `total` is the value stored in the superblock field `layoutSize` and the minimum size of the shared-memory object. Reference vectors for page sizes 4096, 16384 and 65536 are in `constants.json` (`segment.layout_vectors`); tests assert them.

Buffer `i` of pool `d` starts at `buf_d + i * size`. It is owned by ring slot `i` of ring `d` (section 9.4). Pools are plain memory; no word of the pool is ever accessed atomically by the protocol.

**[IOP] The region offsets are always computed from the geometry the endpoint was configured with and its own page size; the geometry stored in the superblock is only compared, never used for addressing** (the Go implementation never reads the recorded geometry to address memory). A port MAY offer a discovery extension that reads the recorded geometry of a `Ready` segment first and then opens with it (SPEC 13.2).

**[IOP] Page-size dependence.** The offsets depend on the page size. Endpoints with different page sizes cannot share a segment; the recorded `pageSize` makes the mismatch visible as a geometry mismatch (5.5). In practice all endpoints of one segment run on one kernel and agree.

### 2.2 Geometry limits **[IOP]**

| Parameter | Rule |
| --- | --- |
| `bufferCount` | power of two, `2 <= bufferCount <= 2^24`. One slot is not allowed: with a single slot the "published" value `pos+1` and the "free for the next lap" value `pos+mask+1` coincide, and a second producer could overwrite an unconsumed packet. |
| `bufferSize` | multiple of 8, `8 <= bufferSize <= 2^30`. |
| total size | `total` MUST be representable (no overflow of the platform's address or file-size type; relevant for 32-bit hosts). |

A configuration outside these bounds is invalid: size computation yields 0 and no segment may be created or opened with it (`ErrInvalidSize`).

## 3. The superblock

### 3.1 Layout **[IOP]**

The superblock is the first `page` bytes of the segment. Offsets and sizes are fixed (`segment.superblock_fields`). Every field is a `u64` accessed **atomically** (see MEMORY_MODEL.md); the fields are:

| Off | Field | Writer(s) | Meaning |
| ---: | --- | --- | --- |
| 0 | `magic` | initializer, at publish | `0x4851515F53544C33` (ASCII `HQQ_STL3` when written big-endian). Because words are little-endian the bytes in memory read `3 L T S _ Q Q H`. Written only at publish (step P5 in 5.3); an `Initializing` segment may still carry `0`. |
| 8 | `state` | claimers, initializer | `0` Empty, `1` Initializing, `2` Ready. Any other value is `ErrIncompatibleLayout`. |
| 16 | `major` | initializer | Layout major version, `3`. Written when an initializer claims the segment and again at publish. A different major is incompatible. |
| 24 | `minor` | initializer | Layout minor version, `0`; informational. |
| 32 | `pageSize` | initializer, at publish | Page size used to compute the regions. |
| 40 | `bufferCount` | initializer, at publish | Configured slots/buffers per direction. |
| 48 | `bufferSize` | initializer, at publish | Configured bytes per buffer. |
| 56 | `layoutSize` | initializer, at publish | `total` of section 2.1. |
| 64 | `epoch` | initializer, at commit | Generation counter: incremented (`fetch_add`) exactly once per successful (re)initialization, just before `state` becomes `Ready`. The first initialization of a zeroed segment produces `1`. |
| 72 | `initToken` | claimers | Session token of the endpoint currently (re)initializing the segment; `0` when nobody is. Ownership register of the initialization (5.2). |
| 80 | `initHeartbeat` | initializer | Wall-clock `UnixNano` of the initializer's last progress. Used only as an opaque progress marker. |
| 88 | `requiredFeatures` | initializer | Bit set of layout features a reader must understand. None are defined; a reader that sees any set bit fails with `ErrIncompatibleLayout`. Written as `0`. |
| 96 | `creatorToken` | initializer, right after winning `initToken` | Identity of the *instance that owns the name*: the random token of a Creator endpoint, or `0` when the segment was initialized by an InMemory endpoint. Replaced by **every** initialization. Openers never write it. |
| 104..127 | reserved | — | Three words, zero. |
| 128..255 | `endpoints[0]` | | Primary record (3.2). |
| 256..383 | `endpoints[1]` | | Secondary record (3.2). |
| 384..page-1 | unused | — | Zero. **The all-zero test of 5.1 covers the whole page**, so a port MUST NOT store anything there. |

`sizeof(superblock) = 384 <= 4096`, so it fits the smallest page.

### 3.2 Endpoint record (128 bytes) **[IOP]**

| Off | Field | Meaning |
| ---: | --- | --- |
| +0 | `token` | `0` = never used since the last initialization; `1` = the owner closed gracefully (**detachedToken**); any other value = random session token of the attached owner (an **owner token**: neither `0` nor `1`). |
| +8 | `pid` | OS process id of the owner; diagnostic only (PIDs are not comparable across PID namespaces). |
| +16 | `heartbeat` | Last heartbeat, wall-clock `UnixNano`; `0` = none written yet. |
| +24 | `interval` | The owner's heartbeat interval in nanoseconds; `0` = heartbeats disabled (the endpoint is then always considered alive). |
| +32..+127 | reserved | Twelve words, zero. |

**[IOP] Tokens.** An owner generates its token with a source of randomness good enough to avoid collisions between distinct sessions (the Go implementation uses a random 64-bit value, redrawn while it is `0` or `1`). A token MUST NOT be `0` or `1`.

**[IOP] Detach.** Closing an endpoint is `cas(record.token, myToken, 1)`. A failed CAS (the record was re-initialized or replaced) is not an error and MUST NOT be retried or overwritten. The value `1` persists until the segment is re-initialized, so a peer that attached and detached before the other side looked is still reported as closed, and nobody can join a session that already ended.

### 3.3 Atomicity **[IOP]**

All superblock words are 64-bit, 8-byte aligned, and MUST be accessed with lock-free 64-bit atomic operations (no tearing, no process-local locks). Plain reads of a superblock word are permitted only through a read-only mapping that nobody else uses to establish ordering (the Go `peekSuperblock`, 11.4). See MEMORY_MODEL.md section 2 for the order of each access.

### 3.4 Superblock state machine **[IOP]**

```text
            claim CAS                       commit (after CAS initToken: me -> 0)
 Empty --------------> Initializing --------------------------------------------> Ready
                         ^   |  \                                                    |
                         |   |   `-- takeover: initToken: dead -> me (state stays)    |
                         |   `------ (initializer dies: state stays Initializing)     |
                         |                                                           |
                         `-----------------------------------------------------------'
                         reuse: CAS state Ready -> Initializing, both endpoints dead
```

Transitions are made only by the claim loop (5.1) and by the initializer (5.3). `Empty` is the state of a zero page and is never re-entered by a protocol action. A port MUST NOT transition a segment to `Empty`.

## 4. Roles and entry points

How an endpoint reaches a segment determines its **role** (the code calls it `attachRole`). The role is a property of the call, not stored in memory.

| Role | Entry points (Go) | Purpose |
| --- | --- | --- |
| **Creator** | `CreateStandardLink`, `CreateAdvancedLink`, `Listen` | Created the name, or reuses a name whose endpoints are all gone. Initializes, re-initializes, takes over; never joins a live segment. |
| **Opener** | `OpenStandardLink`, `OpenAdvancedLink`, `Dial` | Attaches to an existing name as the Secondary. **Never initializes, re-initializes or takes over a segment**, so an opener can never end up as a Primary nobody will join. |
| **InMemory** | `NewStandardLink`, `NewAdvancedLink` | Caller-owned memory. Initializes an empty segment, re-initializes a dead one, takes over, or joins a live one as Secondary. |

Decision table (what the claim loop does; `AT` = `AttachTimeout`, see 5.1 for exact timing):

| Segment state | Creator | Opener | InMemory |
| --- | --- | --- | --- |
| Empty, page not all zero | `ErrIncompatibleLayout`, nothing written | same | same |
| Empty, page zero | claim (become Primary) | wait up to `AT` for a creator, then transient `errSegmentNotReady`; segment untouched | claim (become Primary) |
| Initializing | wait; take over a stalled initializer | wait up to `AT`, then transient `errInitContended`; never take over | wait; take over a stalled initializer |
| Ready, layout incompatible | `ErrIncompatibleLayout` | same | same |
| Ready, both endpoints dead | re-initialize (become Primary) | transient `errNoLiveEndpoint`, immediately | re-initialize (become Primary) |
| Ready, a live endpoint | `ErrLinkBusy` | join as Secondary (or `ErrLinkBusy`/`GeometryError`) | join as Secondary (or `ErrLinkBusy`/`GeometryError`) |
| any other `state` value | `ErrIncompatibleLayout` | same | same |

"Transient" errors (`errSegmentNotReady`, `errInitContended`, `errNoLiveEndpoint`) all match the base error `ErrFailedInit`; a retrying opener (the Go `Dial`) retries on exactly these three and on a missing name (`ErrNotExist`), with a backoff doubling from 1 ms to 100 ms **[IMPL]**. Every other error is permanent.

## 5. Initialization, claim, takeover, reuse, publish, attach

### 5.1 Claim loop **[IOP]**

Inputs: the caller's geometry (`count`, `size`, `layoutSize` computed per 2.1), its fresh `token` (an owner token), `role`, `AttachTimeout` (`AT`, default 5 s), `PeerTimeout`, `HeartbeatInterval`, and for Creators a fresh `creatorToken`. `initStale = max(PeerTimeout, 2 s)` (the window an initializer may show no progress). `poll = 200 µs`. The loop is bounded by a cancellation context if the caller has one: **every iteration first checks the context and fails with its error**, and every sleep is interruptible.

State kept by the loop: `deadline = now + AT` (from entry); `watch = {active, tok, hb, since}` (what was last seen of an initializer, on the **local monotonic clock**); `emptySince` (monotonic).

```text
loop:
  if ctx cancelled: return ctx error
  state = load(sb.state)
  if state != Initializing: watch.active = false
  if state != Empty:        emptySince = unset
  stalling = false; emptyWait = false

  switch state:

  case Empty:
      if not firstPageZero(sb):                 # all page/8 words == 0, each read atomically
          if load(sb.state) != Empty: continue  # somebody claimed it meanwhile
          return ErrIncompatibleLayout          # foreign data; write NOTHING, for every role
      if role == Opener:
          if emptySince unset: emptySince = now
          if now - emptySince >= AT: return errSegmentNotReady   # segment untouched
          emptyWait = true                      # fall through to sleep; deadline is not enforced
      else:
          if cas(sb.state, Empty -> Initializing):
              if not beginInit(expected = load(sb.initToken)): return errInitContended
              return (Primary, mustInit = true)
          # CAS lost: somebody else claimed; fall through to sleep/limit check

  case Initializing:
      tok = load(sb.initToken); hb = load(sb.initHeartbeat)
      if checkLayoutCompat(initializing = true) fails: return ErrIncompatibleLayout
      if role == Opener: pass                   # only waits; limit check below
      else:
          if not watch.active or watch.tok != tok or watch.hb != hb:
              watch = {active, tok, hb, since = now}          # progress seen: restart window
          else if now - watch.since >= initStale:
              if beginInit(expected = tok): return (Primary, mustInit = true)   # TAKEOVER
              watch.active = false                              # somebody else moved first
          stalling = true

  case Ready:
      if checkLayoutCompat(initializing = false) fails: return ErrIncompatibleLayout
      primaryAlive   = endpointAliveAtOpen(endpoints[0])        # may sleep up to ~2 s (6.2)
      secondaryAlive = endpointAliveAtOpen(endpoints[1])
      if not primaryAlive and not secondaryAlive:
          if role == Opener: return errNoLiveEndpoint           # state untouched
          if cas(sb.state, Ready -> Initializing):              # REUSE
              if not beginInit(expected = load(sb.initToken)): return errInitContended
              return (Primary, mustInit = true)
          continue
      if role == Creator: return ErrLinkBusy                    # a creator never joins
      got  = (load bufferCount, bufferSize, layoutSize, pageSize)
      want = (count, size, layoutSize, localPageSize)
      if got != want: return GeometryError(want, got)           # wraps ErrInvalidSize
      if primaryAlive and load(endpoints[1].token) == 0:
          if cas(endpoints[1].token, 0 -> token):               # JOIN as Secondary
              fillEndpoint(endpoints[1])                        # pid, interval, heartbeat (6.1)
              return (Secondary, mustInit = false)
          continue
      return ErrLinkBusy      # both alive, or the live endpoint's earlier peer already left

  default: return ErrIncompatibleLayout

  # reached by Empty(creator, lost CAS), Empty(opener), Initializing, never by Ready/default
  limit = deadline
  if stalling and watch.active and watch.since < deadline: limit = max(deadline, watch.since + initStale)
  if not emptyWait and now > limit: return errInitContended
  sleep(poll)                                    # interruptible
```

`firstPageZero` reads **every** 8-byte word of the first page (`pageSize` bytes, not 384) atomically. A freshly created zero-filled object passes it; any foreign byte anywhere in the first page makes the segment foreign.

`checkLayoutCompat(sb, initializing)`:

```text
if load(requiredFeatures) & ~knownRequiredFeatures(=0) != 0: return ErrIncompatibleLayout
magic = load(sb.magic); major = load(sb.major)
if not initializing:
    if magic != MAGIC or major != 3: return ErrIncompatibleLayout        # includes "HQQ_STL2"
    return ok
# initializing: the initializer may have died right after claiming; layout words may be missing
if magic != 0 and magic != MAGIC: return ErrIncompatibleLayout
if major != 3 and (major != 0 or load(initToken) != 0): return ErrIncompatibleLayout
return ok
```

Properties that a port MUST preserve (each is observable by another process or protects foreign data):

1. **[IOP] Foreign data is never written**: nothing is stored to a segment whose state is `Empty` with a non-zero page, nor to one whose layout check fails.
2. **[IOP] Openers never write `state`, `initToken` or `creatorToken` and never take over.**
3. **[IOP] The Initializing watch uses the local monotonic clock and compares `(initToken, initHeartbeat)` for change only.** There is no fast path from a stale wall-clock `initHeartbeat` to takeover: a slow initializer is always given its whole `initStale` window, even if that runs past `AttachTimeout`.
4. **[IOP] Takeover and reuse are exactly the CAS sequences above.** In particular the reuse CAS is `Ready -> Initializing` on `state`, followed by the `initToken` CAS in `beginInit`.
5. **[IMPL]** The poll interval (200 µs), the cancellation mechanism and sleeping strategy are free.

### 5.2 `beginInit` **[IOP]**

```text
beginInit(token, expected):
    store(major = 3); store(minor = 0); store(requiredFeatures = 0)     # stampLayout
    if not cas(initToken, expected -> token): return false               # another endpoint took over
    store(creatorToken = creatorTokenOfThisCall)    # 0 for InMemory; fresh random for Creator
    store(initHeartbeat = nowUnixNanos())
    return true
```

`expected` is the `initToken` value the claimer saw: `0` for a fresh claim or a reuse, the dead initializer's token for a takeover. `beginInit` makes the caller the **initializer**; from now on the **ownership check** is `load(initToken) == token`.

### 5.3 Initialization sequence and publish **[IOP]**

A winner of `beginInit` MUST, in this order, **checking before every step that the context is alive and that it still owns `initToken`** (if it lost ownership it MUST fail with `errInitContended` and MUST NOT touch the segment any more):

1. For each ring in order `ring0`, `ring1`: check; `Format(ring)` (7.2); then **progress** (below).
2. Zero `buf0` in chunks of at most 8 MiB: before each chunk check ownership; after each chunk progress. Then the same for `buf1`.
3. **Publish** (`publishSuperblock`):
   * **P0** check `load(initToken) == token`, else `errInitContended`.
   * **P1** `stampLayout` again: `major=3, minor=0, requiredFeatures=0`.
   * **P2** `pageSize`, `bufferCount`, `bufferSize`, `layoutSize` (stores).
   * **P3** clear the Secondary record: `token=0`, `pid=0`, `heartbeat=0`, `interval=0`.
   * **P4** write the Primary record: `token = token`, then `pid`, `interval = HeartbeatInterval ns (0 if disabled)`, `heartbeat = nowUnixNanos()`.
   * **P5** `store(magic = MAGIC)`.
   * **Commit** `cas(initToken, token -> 0)`. **Only if the CAS succeeds**: `epoch = fetch_add(epoch, 1) + 1`, then `store(state = Ready)`. A failed CAS returns `errInitContended` and leaves the segment as it is.
4. Attach to both rings (5.4).

**Progress** = `if load(initToken) == token: store(initHeartbeat = nowUnixNanos())`; it keeps a slow but live initializer from being taken over. Every write step of publish (P1–P5 below) and every slice of the zeroing and formatting first checks ownership; a check and the store after it are not atomic, so an initializer suspended between them can still complete that one store (FINDINGS F-01).

The `state = Ready` store is the **release point** of the whole initialization: every earlier store (rings formatted, buffers zeroed, superblock fields, records) happens-before any endpoint that observes `state == Ready` with an acquire load (MEMORY_MODEL.md 3.1).

**[IOP] Ordering within publish.** P1–P5 MUST precede the commit CAS; the commit CAS MUST precede the `epoch` increment and the `state` store. Readers observe `Ready` only after `epoch` is updated.

### 5.4 Attaching the rings **[IOP]**

After claim (and, for the initializer, publish) an endpoint attaches to ring 0 and ring 1: it polls `Attach` (7.3) until the ring header validates, waiting at most `AttachTimeout` in total (the Go implementation polls in 10 ms slices and checks cancellation between slices). A ring that is missing (not initialized), or has a different magic, layout version, stride or size, or whose size differs from `bufferCount`, makes the endpoint **detach** (`cas(record.token, mine, 1)`) and fail with `ErrIncompatibleLayout` (or with the context error if cancelled).

If setting up a freshly created named object (section 11.3) fails, the name is removed again, provided it still refers to this instance.

### 5.5 Joining, third endpoint, geometry **[IOP]**

* A joiner (Opener or InMemory on a `Ready` segment with a live endpoint) compares `(bufferCount, bufferSize, layoutSize, pageSize)` with its own; any difference is `GeometryError`, which matches `ErrInvalidSize` and carries both geometries.
* It joins as Secondary only if the Primary is alive **and** `endpoints[1].token == 0`, via `cas(endpoints[1].token, 0, token)` followed by writing `pid`, `interval`, `heartbeat` (in that or any order; readers tolerate a record with a token and `interval == 0`/`heartbeat == 0`, 6.2).
* In every other live case — both endpoints alive, or the live endpoint's earlier peer already closed or died (a dead endpoint may still own ring slots) — the result is `ErrLinkBusy`. The segment can only be reused after the surviving endpoint closes.
* After joining, a Secondary reads `epoch` (that value identifies its session) and starts monitoring the Primary immediately (6.3).

### 5.6 Failure rules **[IOP]**

* After an endpoint has claimed a role, any later failure of its own setup (ring attach etc.) writes `detachedToken` into its own record (CAS from its token) before returning.
* An initializer that returns an error before publishing leaves the segment `Initializing`; the next Creator/InMemory endpoint takes it over after `initStale` without progress.
* After a successful publish the Primary is `Ready`; if it dies before anybody joins, a later Creator reuses the segment once its heartbeat stops (6.2), and an Opener gets `errNoLiveEndpoint` meanwhile only when no endpoint is alive.

## 6. Liveness, heartbeat, peer state, error mapping

### 6.1 Heartbeat **[IOP for the words, IMPL for scheduling]**

An endpoint with `HeartbeatInterval > 0` writes `store(own.heartbeat = nowUnixNanos())` every interval, **only while `load(own.token) == myToken`** (it never revives a record that was re-initialized). It keeps heartbeating after a peer failure; it stays attached until it is closed. Its `interval` field holds the interval in nanoseconds. With heartbeats disabled (`HeartbeatInterval < 0`) the endpoint writes `interval = 0`, still writes one `heartbeat` when it fills its record, and is **always considered alive** by openers. Default interval: 250 ms (`timing_ns.default_heartbeat_interval`).

`fillEndpoint(rec)` = `pid = getpid()`, `interval`, `heartbeat = now`.

Timestamps are wall-clock Unix nanoseconds (`CLOCK_REALTIME`). A reader MUST NOT rely on them for anything but (a) the open-time freshness fast path (6.2) and (b) change detection.

### 6.2 Liveness at open (`endpointAliveAtOpen`) **[IOP]**

Used by the claim loop on a `Ready` segment. For one endpoint record:

```text
tok = load(token)
if tok == 0 or tok == 1:            return dead                    # unused, or detached
interval = load(interval)
if interval == 0:                   return alive                   # heartbeats disabled
hb = load(heartbeat)
if hb == 0:                         return alive                   # attached a moment ago
timeout = max(callerPeerTimeout, 4 * interval)                      # callerPeerTimeout default 5 s
if -1 s < wallAge(hb) < timeout:   return alive                   # a heartbeat more than 1 s in the future is not trusted: confirm by the watch (FINDINGS F-04)
window = min(3 * interval, 2 s);  step = max(interval / 4, 1 ms)
for until now + window:                                             # confirm by watching for progress
    sleep(step)
    if load(heartbeat) != hb or load(token) != tok:
        return isOwnerToken(load(token))
return dead
```

The watch uses the caller's clock only to bound the window; it is therefore robust against wall-clock jumps that make a live heartbeat look stale.

### 6.3 Peer monitor (`checkPeer`) **[IOP for the derivation, IMPL for scheduling]**

Each endpoint tracks its peer's record with the **local monotonic clock**. State per link: `status` (`NotConnected`=0, `Connected`=1, `Closed`=2, `Gone`=3), `token` (peer token first seen, `0` = none), `lastHB`, `lastChange`. Terminal states: once `status` is `Closed` or `Gone` it never changes. The check runs from the heartbeat tick (every interval), from the `Check` hook of a blocked ring operation (at most every 50 ms, 8.4), and once right after a Secondary joined.

```text
checkPeer():
    if status is Closed/Gone: return its error
    if load(sb.epoch) != myEpoch or load(endpoints[self].token) != myToken:
        status = Gone; return ErrPeerGone         # re-initialized under us, or we were replaced
    rec = endpoints[1 - self]
    tok = load(rec.token); now = monotonic()
    if tok == 1:                       status = Closed; return ErrPeerClosed
    if peer.token == 0:                                # peer not seen yet
        if tok != 0: peer.token = tok; lastHB = load(rec.heartbeat); lastChange = now; status = Connected
        return nil
    if tok != peer.token:              status = Gone; return ErrPeerGone    # replaced
    interval = load(rec.interval)
    if interval == 0:                  return nil                           # heartbeats disabled: assume alive
    hb = load(rec.heartbeat)
    if hb != lastHB:                   lastHB = hb; lastChange = now; return nil
    if now - lastChange > max(myPeerTimeout, 4 * interval): status = Gone; return ErrPeerGone
    return nil
```

`myEpoch` is `epoch` as read when the endpoint started (Primary: the value returned by publish; Secondary: `load(epoch)` after joining). `myPeerTimeout` is the endpoint's own `PeerTimeout` (default 5 s).

### 6.4 Effect on I/O and error mapping **[IOP]**

| Condition | Observable result |
| --- | --- |
| Peer detached (`token == 1`) | `ErrPeerClosed` (matches `io.EOF`). `StandardLink.Read`/`ReadFrom` return plain `io.EOF` instead. |
| Peer stopped heartbeating longer than the timeout, its token changed, or the segment was re-initialized under this endpoint | `ErrPeerGone` (matches `ErrConnNotFound`). |
| Local close | `ErrClosed` (matches `net.ErrClosed`). |
| Deadline | `ErrTimeout` (matches `os.ErrDeadlineExceeded`, `net.Error.Timeout() == true`). |

**[IOP] Receivers drain before reporting failure.** When a read fails because the peer closed or died, the receiver MUST make one more non-blocking attempt to dequeue and deliver a packet that was published before the failure; it reports the failure only when the ring is empty. (Data is consumed from the ring first; peer state is consulted only when the ring is empty or a write would block.)

**[IOP] Writes after failure.** A send call first checks the recorded peer status and returns the corresponding error without publishing if the peer is already known to be `Closed` or `Gone`; while blocked on a full ring it re-checks at least every 50 ms.

**[IMPL]** Whether the status is refreshed by a heartbeat thread, lazily by blocked calls, or both. An endpoint without a heartbeat thread refreshes it at the start of each send (FINDINGS F-10).

**[IMPL] Close.** `Close` sets a local closed flag, wakes blocked calls, waits for in-flight calls, stops the heartbeat, `cas(own.token, mine, 1)`, then wakes both rings once more so that a peer blocked on a ring notices promptly. The final wake is RECOMMENDED; the peer notices anyway within one check interval.

## 7. The ring protocol

A ring is a bounded Vyukov MPMC queue in shared memory. Ring layout version **2**.

### 7.1 Memory image **[IOP]**

```text
offset   size  field          meaning
  0        8   magic          0xC9D8C1D43F096702                       } line 0: immutable after Format
  8        8   size           number of slots N (power of two, >= 2)
 16        8   flag           bit0 reserved (0), bit1 = initialized (value 2)
 24        8   stride         slot stride in bytes (128)
 32        8   version        ring layout version (2)
 40..127                      padding (zero)
128        8   r              consumer cursor (position)               } line 1
136..255                      padding
256        8   w              producer cursor (position)               } line 2
264..383                      padding
384        4   notEmpty       wake word, bumped by producers           } line 3
388        4   emptyWaiters   number of parked processes waiting for data
392        4   notFull        wake word, bumped by consumers
396        4   fullWaiters    number of parked processes waiting for space
400..511                      padding
512 ...        slots[0..N-1]
```

`HeaderSize = 512`. Each of the four lines is 128 bytes so that independently written words never share a line on machines with 64- or 128-byte lines. A slot is:

```text
slot offset   size   field
  0            8     seq       u64, the slot sequence number
  8           64     packet    the 64-byte HQQ packet (section 9.1)
 72..127              padding  (stride 128 = round_up(8 + 64, 64))
```

Slot `i` is at `ring + 512 + 128*i`. `stride = round_up(8 + sizeof(payload), 64)`; HQQ rings always carry the 64-byte packet, so the stride is 128. Ring bytes = `512 + 128*N`.

**[IOP]** The packet bytes are plain memory (not atomics), written only by the slot's current producer and read only by its current consumer (MEMORY_MODEL.md 3).

### 7.2 Format **[IOP]**

`Format(ring, N)` runs in the sole presence of the initializer (nobody else uses the memory) and MUST produce exactly this image, storing the fields in this order so that a concurrent `Attach` never accepts a half-built ring:

```text
store(flag = 0); store(magic = 0)                           # invalidate first
for i in 0..N-1:  packet[i] = zeros;  store(seq[i] = i)
store(r = 0); store(w = 0)
store(notEmpty = 0); store(emptyWaiters = 0); store(notFull = 0); store(fullWaiters = 0)   # clears stale waiter counts
store(size = N); store(stride = 128); store(version = 2)
store(magic = MAGIC_RING)                                  # then
store(flag = 2)                                            # LAST (release)
```

Format MUST zero the wait words and both waiter counts: a process killed while parked never decremented its count, and re-initialization is the only point where the count is reset. `N` MUST be a power of two `>= 2`; anything else is a programming error (Go panics).

### 7.3 Attach **[IOP]**

```text
Attach(ring, timeout):
  loop:
     if load(magic) == MAGIC_RING and load(flag) & 2 != 0:
         if load(stride) != 128 or load(version) != 2 or not (N = load(size) is a power of two >= 2): return INCOMPATIBLE   # immediately
         return handle(N, mask = N - 1)
     if timeout != 0 and elapsed >= timeout: return NOT_READY
     sleep(100 µs)
```

An incompatible header is rejected at once (no waiting); a header that is not yet initialized is awaited. The loads of `magic` then `flag` MUST be acquire (7.2 releases with `flag`), so that the slot images are visible once the ring is accepted. A handle caches only `N`, `mask`, and the slot base; **cursors are always read from shared memory**.

### 7.4 Claim, publish, release **[IOP]**

Positions are `u64` counters modulo 2^64; `mask = N - 1`; slot index is `pos & mask`. Four protocol steps; the slot sequence `seq` is the only synchronization word between producer and consumer:

| Step | Precondition (read of `seq[pos&mask]`) | Action | Effect on `seq` |
| --- | --- | --- | --- |
| **producer claim** | `int64(seq - pos) == 0` | claim position `pos`: `cas(w, pos -> pos+1)` | none |
| **publish** | the caller owns `pos` | write packet and payload (plain), then `store(seq = pos + 1)` | `pos → pos+1` ("published") |
| **consumer claim** | `int64(seq - (pos+1)) == 0` | claim: `cas(r, pos -> pos+1)` | none |
| **release** | the caller owns `pos` | read packet and payload (plain), then `store(seq = pos + mask + 1)` | `pos+1 → pos+N` ("free for the next lap") |

Claim loops:

```text
claimProducer():                              claimConsumer():
  pos = load(w)                                 pos = load(r)
  loop:                                         loop:
    d = int64(load(seq[pos&mask]) - pos)          d = int64(load(seq[pos&mask]) - (pos+1))
    d == 0: if cas(w, pos, pos+1): return pos     d == 0: if cas(r, pos, pos+1): return pos
            pos = load(w)                                 pos = load(r)
    d  > 0: pos = load(w)    # another producer       d  > 0: pos = load(r)    # another consumer
            # already took pos; catch up                      # already took pos
    d  < 0: ring is FULL: wait (section 8)        d  < 0: ring is EMPTY: wait (section 8)
```

* A slot's life per lap: `seq = pos` (free for the producer of `pos`) → `pos+1` (published, owned by the consumer of `pos`) → `pos+N` (= `(pos+N)`, free for the producer of position `pos+N`, the next lap).
* Because the claimant of a position is its **only writer** until it completes the slot, completion is a plain atomic store; it cannot fail. (The Go API additionally completes handles that callers may *copy* with `cas(seq, pos -> pos+1)` / `cas(seq, pos+1 -> pos+N)` so that a stale copy of an already completed reservation fails the swap and neither modifies the ring nor wakes anyone. That is a local API-safety measure; **[IMPL]**.)
* **Wraparound.** `pos`, `r`, `w` and `seq` are all modulo 2^64. The comparisons are the signed differences `int64(seq - pos)` above; they are correct because `N <= 2^24`, far below `2^63`, so a slot's `seq` is never more than `N` positions away from any `pos` that can legitimately reach it. Arithmetic on positions MUST be done in `u64` with wrap (in C `uint64_t`; in Rust `wrapping_add`/`wrapping_sub`). Slot index is `pos & mask`.
* **Invariants (quiescent):** `w - r` ∈ `[0, N]`; `seq[i]` is `i` (never used), or `pos+1`/`pos+N` for the last position `pos` that used slot `i`. The number of published-but-unreleased slots is at most `N`. A producer that has claimed but not published (or a consumer that has claimed but not released) holds its slot **and blocks the ring behind it** (head-of-line): positions are consumed strictly in order of their position even when completed out of order, so a stalled claimant stalls the whole direction. There is no timeout or recovery for a claimant that dies between claim and completion; its peer observes the death through heartbeats and the link is abandoned.
* **FIFO.** Packets are delivered in position order.

### 7.5 What a full or empty ring means **[IOP]**

`d < 0` for the producer means the slot's previous occupant (position `pos - N`) has not been released: the ring is full. `d < 0` for the consumer means position `pos` has not been published: the ring is empty. A non-blocking operation fails at that point without changing any shared word. A blocking operation waits as specified in section 8.

### 7.6 SPSC mode (local serialization) **[IMPL]** with **[IOP]** constraints

Because the cross-process topology is one producer process and one consumer process per ring, an implementation MAY serialize its own producers (and its own consumers) with a process-local lock, or restrict itself to one producer and one consumer thread, and then use plain atomic **stores** instead of CAS on the cursor:

```text
SPSC producer:  pos = w (load, owned by this party);  if int64(load(seq[pos&mask]) - pos) < 0: FULL
                store(w = pos + 1);  write packet/payload;  store(seq = pos + 1)
SPSC consumer:  pos = r;                if int64(load(seq[pos&mask]) - (pos+1)) < 0: EMPTY
                store(r = pos + 1);  read packet/payload;  store(seq = pos + mask + 1)
```

Constraints (mandatory):

1. The **slot sequence protocol** (the table in 7.4, the memory orders of MEMORY_MODEL.md) is followed exactly; the cursor words are not read by the other side, but they MUST still be maintained (`w` = number of producer claims, `r` = number of consumer claims, modulo 2^64) so that the memory image is canonical and a process using CAS (the Go implementation) can later take over the same side in a **new session** after re-initialization, and so that debugging tools agree.
2. Cursors are **read from shared memory at attach** (always 0 at the first attach of a role in a session; the Secondary may find ring 0 already carrying packets that the Primary published before it attached).
3. The cursor store happens **before** the publish/release store of `seq` (it is the "claim" of the CAS protocol).
4. Exactly one process acts as producer and one as consumer per ring: a second process on the same side is never admitted by the superblock protocol, so SPSC mode is sound **only because** of 5.5.

CAS-based and store-based implementations interoperate on the same ring (the Go implementation is CAS-based).

## 8. Wait / wake protocol

Blocking is optional for correctness: **a port that never parks is valid** (spin plus bounded sleep backoff, "simple mode" in PORTING.md). Parking exists to save CPU. This section specifies the **cross-process handshake that parking implementations MUST follow** so that a wake-up is never lost, and the duty of **wakers**, which every implementation that wants other processes to sleep efficiently SHOULD honor.

### 8.1 Words **[IOP]**

Per ring, two sides:

| Side | Awaited by | Condition | Wake word | Waiter count | Signaled by |
| --- | --- | --- | --- | --- | --- |
| `NotEmpty` (0) | consumer | slot at position `r` is published | `notEmpty` (u32 @384) | `emptyWaiters` (u32 @388) | producer after publish; |
| `NotFull` (1) | producer | slot at position `w` is free | `notFull` (u32 @392) | `fullWaiters` (u32 @396) | consumer after release |

**[IOP] The waiter count counts parked PROCESSES, not threads.** Aggregating all blocked threads of one process into one registered "leader" is a process-local implementation choice (the Go implementation does it so that N blocked goroutines pin one OS thread). A port MAY register once per parked thread instead; counts then over-count, which only costs superfluous wake calls. A count MUST never be decremented below the number of live parkers (do not under-count: that would lose wake-ups). In the specified topology each side has at most one parked process, so a count is `0` or `1` plus residue of dead processes.

### 8.2 Waiter protocol **[IOP]**

A thread (or the process leader) that found the ring full/empty and wants to park until `seq[pos&mask]` reaches `target` (`pos` for the producer, `pos+1` for the consumer; compared as `int64(seq - target) < 0`) does, for the side's `(word, waiters)`:

```text
W1  fetch_add(waiters, 1)                       # register       (seq_cst)
W2  v = load(word)                              # snapshot       (seq_cst; acquire is the minimum)
W3  if int64(load(seq[pos&mask]) - target) < 0 and not cancelled:    # re-check  (seq_cst)
        park(word, v, timeout)                  # futex_wait(word, v) / ulock wait / semaphore wait
W4  fetch_sub(waiters, 1)                       # deregister
    re-run the claim loop
```

### 8.3 Waker protocol **[IOP]**

After `publish` (resp. `release`):

```text
K1  store(seq = ...)                            # (seq_cst)
K2  n = load(waiters of the opposite side)      # (seq_cst)
K3  if n != 0:  fetch_add(word, 1)  then  wake_all(word)        # with a Parker: Wake(side, 1) per slot; WakeAll: Wake(side, n)
```

`word` is the NotEmpty word after a publish, the NotFull word after a release.

**[IOP] The four accesses W1, W3, K1, K2 form a store-buffering (Dekker) pattern and MUST be sequentially consistent** (or be separated by `seq_cst` fences); release/acquire is **insufficient** (MEMORY_MODEL.md 4 with the failing interleaving and `litmus/wake_handshake_*.litmus`). Without it the waker may read `waiters == 0` while the waiter reads the old `seq`: the waiter parks and nobody wakes it.

**[IOP] The wake-word protocol with a futex-style primitive.** The waiter snapshots the word at W2 and the kernel compares it atomically at park time; the waker changes the word (K3, `fetch_add`) **after** having seen `n != 0`. Hence a wake between W3 and the kernel wait is not lost: the kernel sees a changed word and returns at once. `wake_all` wakes every waiter of the word (`FUTEX_WAKE` with `INT_MAX`; `ULF_WAKE_ALL`).

**[IOP] Bounded park.** Every park MUST be bounded by a timeout so that a lost or never-sent wake costs bounded latency, not a hang. The Go implementation parks at most `100 ms`, and shortens it to the remaining deadline and to the next peer-liveness check (at most `50 ms` apart). A port SHOULD use the same bounds.

**[IOP] `WakeAll`** (used on close and when the peer state changes) bumps **both** wake words and wakes unconditionally (not gated by the counts); with a Parker, it releases `n` tokens only if the count is non-zero.

**[IOP] Stale counts.** A process killed while parked never runs W4, so its count stays raised until the ring is re-formatted (7.2). Consequences: one superfluous wake per publish/release. Correctness is not affected because the count is only a hint to wake. A waker therefore grants one token per slot, never the stale count (FINDINGS F-02).

### 8.4 Backoff schedule **[IMPL]** (Go defaults, RECOMMENDED)

A blocked operation escalates through three phases, re-checking the slot sequence each iteration:

1. spin: 256 iterations (no syscall). A Go iteration re-reads the slot 4 times, about 24 ns, so the window is about 6 µs; an implementation with a cheaper step uses proportionally more iterations (the C library: 1024 single-read steps, about 4 µs). The window must cover the reply of a peer that spends a few microseconds per request; a window of about 1.5 µs measurably slowed such calls (the Go yield phase wakes another thread per `runtime.Gosched`);
2. yield: 256 iterations of `sched_yield`/`runtime.Gosched` (deadline and liveness checks every 16th iteration);
3. park for `min(100 ms, time to deadline, time to next liveness check)`; liveness (`Check`) runs once when leaving the spin phase and then at most every 50 ms.

`WaitSpin` never parks: it re-checks forever, yielding and running the slow checks every 64th iteration.

### 8.5 Platform primitives

* **Linux [IOP]**: `futex(addr, FUTEX_WAIT = 0, v, timespec)` / `futex(addr, FUTEX_WAKE = 1, INT_MAX)` on the 32-bit wake word, **shared** (no `FUTEX_PRIVATE_FLAG`), because the word lives in a `MAP_SHARED` mapping and waiters are in different processes.
* **macOS [IOP]**: `__ulock_wait(UL_COMPARE_AND_WAIT_SHARED = 3, addr, v, timeout_us)` (syscall 515) and `__ulock_wake(UL_COMPARE_AND_WAIT_SHARED | ULF_WAKE_ALL, addr, 0)` (syscall 516, `ULF_WAKE_ALL = 0x100`). These are private Darwin interfaces; errors are ignored and parking then degrades to bounded polling. The public alternative is `os_sync_wait_on_address`/`os_sync_wake_by_address_all` (macOS 14.4+, `OS_SYNC_WAIT_ON_ADDRESS_SHARED`); a port MAY use it. Both operate on the same 32-bit wake words and are interoperable at the protocol level because only the *word value* and the *waiter count* are shared state.
* **Windows, named segments [IOP]**: Windows has no cross-process compare-and-wait on a shared-memory word (`WaitOnAddress`/`WakeByAddressAll` only match waiters and wakers of the same process). Each segment therefore has **four named kernel semaphores** (11.5): ring 0 notEmpty, ring 0 notFull, ring 1 notEmpty, ring 1 notFull. Protocol: the waiter registers (W1), re-checks (W3), and `WaitForSingleObject(sem, timeout)`; the waker, after publishing and seeing `n = load(waiters) != 0`, calls `ReleaseSemaphore(sem, 1)` per published or released slot (`ReleaseSemaphore(sem, min(n, INT_MAX))` only for `WakeAll`; errors, including `ERROR_TOO_MANY_POSTS`, are ignored). The wake word is **not** used. Because tokens persist, a wake that runs between the waiter's re-check and its wait is not lost; tokens left for waiters that already gave up only cause one spurious return, after which waiters re-check. Each semaphore is created with initial count 0 and maximum count `2^31-1`.
* **Windows, unnamed memory** and **other platforms [IMPL]**: the Go implementation uses `WaitOnAddress`/`WakeByAddressAll` (same-process only) bounded by an exponential backoff `20 µs → 2 ms`, or bare exponential sleep `20 µs → 2 ms`, so a wake from another process is noticed within one backoff step.

### 8.6 Process-local aggregation **[IMPL]**

All blocked operations of one process on the same wake word form a group; the first to park is the leader and alone runs W1–W4; the others wait on a process-local primitive and re-check when the leader returns (one of them then becomes the next leader). A follower checks its condition *before* joining the group, and all waiters of a side wait for the same slot, so a publish either finds the leader registered and wakes it, or precedes its registration and is seen by its re-check. This adds nothing to the shared memory.

## 9. Standard protocol (packet, copy, tombstone, error)

### 9.1 Packet ABI **[IOP]**

A packet is **64 bytes: eight little-endian `u64` words**.

| Word | Offset | Content |
| ---: | ---: | --- |
| 0 | 0 | opcode in the low byte; bits 8..63 reserved |
| 1..7 | 8, 16, …, 56 | operands 0..6 |

**[IOP]** The reserved bits of word 0 and every operand word that the opcode does not define MUST be zero in a published packet. A sender MAY skip the store of a word that is already zero: operands 3..6 (words 4..7) of a Standard ring are defined by no Standard opcode and are zeroed by `Format` (7.2), so a Standard sender that never writes them satisfies the rule, and an implementation relies on this only for rings it formatted or attached under the rules of 7.2 and 7.3. A sender that writes into a slot that held a packet of another layer MUST clear the words itself. **[IOP]** Receivers MUST read the opcode as `word0 & 0xFF` and MUST ignore bits 8..63 of word 0 (forward compatibility). Operand words not defined for an opcode are ignored by receivers.

### 9.2 Opcodes and operands **[IOP]**

| Opcode | Value | Layer | Operands (index: meaning) |
| --- | ---: | --- | --- |
| `OpError` | `0x00` | both | 0: ContextID (connection, copy or request id); 1: ErrorCode (section 12) |
| `OpConnCreate` | `0x01` | Advanced | 0: ConnectionID (operand 1 reserved, sent as 0) |
| `OpConnAccept` | `0x02` | Advanced | 0: ConnectionID |
| `OpConnClose` | `0x03` | Advanced | 0: ConnectionID; 1: reason (sent as 0) |
| `OpConnCopy` | `0x04` | Advanced | 0: ConnectionID (0 = link-level stream); 1: MessageID; 2: TotalSize; 3: ChunkOffset; 4: ChunkSize; 5: frame flags; 6: aux (MethodID / status) |
| `OpProtoNegotiate` | `0x05` | Advanced | 0: version `(major<<8)|minor`; 1: requested features; 2: capability word (sent as 0, ignored) |
| `OpProtoAck` | `0x06` | Advanced | 0: version; 1: negotiated features; 2: capability word (sent as 0, ignored) |
| `OpStandardLinkTombstone` | `0xFE` | Standard | 0: CopyID / reservation id; 1: slot index (diagnostic mirror); 2: reason code (ErrorCode) |
| `OpStandardLinkCopy` | `0xFF` | Standard | 0: CopyID; 1: slot index (diagnostic mirror); 2: DataSize |

Golden byte vectors for each opcode are in `constants.json` (`packet_vectors`) and asserted by tests. An unknown opcode on a path that expects defined opcodes is a protocol violation of that path (below). Packets of opcodes `0x01..0x06` carry **no payload** (their slot's buffer is unused).

### 9.3 Send path **[IOP]**

1. Claim a producer slot `s` on the sender's transmit ring (direction 0 for the Primary, 1 for the Secondary).
2. Write the payload into buffer `s` of the sender's pool (plain stores), `1 <= DataSize <= bufferSize`.
3. Store the packet into the slot: `OpStandardLinkCopy` with `CopyID`, `Operand1 = s`, `DataSize`; if the write must be abandoned after the claim (a reservation aborted, a zero-copy callback that fails, panics or returns an invalid size), store `OpStandardLinkTombstone` with `Operand2 = ErrorCode` (`ErrCodeInvalidOp` if none, `ErrCodeInvalidSize` for a size `<= 0`, `ErrCodeBufferOverflow` for a size `> bufferSize`) instead. **A claimed slot MUST always be published** (a tombstone if nothing else) or the ring stalls.
4. Publish (`store(seq = pos + 1)`) and wake (8.3).

`CopyID` is a per-endpoint counter starting at 1 (the first packet carries 1), shared by all copy and tombstone packets of that endpoint, increasing by 1 per claim; it is diagnostic. Empty writes (`len == 0`) do not publish a packet. Standard `Write` of more than `bufferSize` bytes is refused locally (`ErrBufferOverflow`); larger messages require the Advanced layer.

### 9.4 Receive path and buffer ownership **[IOP]**

Each direction starts with every slot available. **Slot `i` owns buffer `i` of its pool, and ownership of the buffer is exactly ownership of the slot**: a producer may write buffer `i` only between claiming and publishing slot `i`, a consumer may read it only between claiming and releasing slot `i`. A payload buffer is recycled only after the receiver releases the owning slot.

1. Claim a consumer slot; the slot index is derived from the claimed **position** (`pos & mask`), **never from packet operand 1** (which is a diagnostic mirror).
2. Classify the packet by opcode:

   | Opcode | Standard receive action |
   | --- | --- |
   | `OpStandardLinkTombstone` | skip; release the slot; take the next packet |
   | `OpStandardLinkCopy` | `size = Operand2`; if `size == 0` or `size > bufferSize`: release the slot and fail with `ErrInvalidSize`; else deliver `size` bytes of buffer `slot` (before release) |
   | `OpError` | release the slot; fail with the error of `Operand1` (12) |
   | anything else (including Advanced frames on a raw Standard reader) | release the slot; fail with `ErrUnexpectedPacket` |

3. Release the slot (`store(seq = pos + mask + 1)`) **promptly**, after the callback/copy, even if the delivery failed or panicked, and wake producers (8.3).

A closed or dead peer does not lose messages: see the draining rule in 6.4.

**[IMPL] Partial reads, tails and datagram semantics** (the Go `Read` leaving the unread tail of a message queued per endpoint, `ReadFrom` truncating) are local API behavior and not part of the wire protocol.

## 10. Advanced protocol

The Advanced protocol is an overlay on the Standard protocol: same segment, same rings, same packet ABI, same slot-owned buffers. Once negotiation of a link starts, **the Advanced receive path owns the receive direction of that link**; raw Standard reads on it are no longer meaningful. Protocol version 1.0.

### 10.1 Negotiation **[IOP]**

Features: `FeatureMessages = 1<<0` (plain messages: `OpConnCopy` without request/response flags), `FeatureRequestResponse = 1<<1`. Version word `(major<<8)|minor`; baseline `0x0100`.

* The **Primary initiates**: it sends `OpProtoNegotiate(version, requestedFeatures, 0)` and then reads packets until an ack arrives, skipping tombstones. The **Secondary responds**: it reads packets (skipping tombstones) until `OpProtoNegotiate`, then sends `OpProtoAck`. A role-agnostic `Negotiate(features)` does this automatically; `features = 0` requests everything supported.
* **Answer rules** (responder): if `major(version) != 1`, send `OpError(0, ErrCodeInvalidOp)` and fail (`ErrNegotiationRejected` locally). Otherwise `minor = min(requestedMinor, ownMinor)` (own minor is `0` in the baseline); `features = requestedFeatures & supportedFeatures & allowedFeatures`; send `OpProtoAck(version = (1<<8)|minor, features, 0)`; become negotiated with these values.
* **Initiator checks the ack**: reject (`ErrNegotiationRejected`, state `Failed`) if `ack.major != 1`, `ack.minor > ownMinor`, or `ack.features` contains a bit outside `requestedFeatures & supportedFeatures`. Otherwise negotiated.
* **Initiator receives `OpProtoNegotiate`** (the Secondary also initiated): if it is the Secondary, it **answers** it (the Primary's request wins) with `features = request.features & supported & ownRequestedFeatures`; if it is the Primary, it ignores it and keeps waiting for its ack.
* An `OpError` while waiting fails negotiation with `ErrNegotiationRejected`; any other opcode with `ErrUnexpectedPacket`.
* **Negotiation state** (local, not in shared memory): `None → Negotiating → Negotiated`, or `→ Failed`. **`Failed` is terminal**: every later negotiate call returns `ErrNegotiationFailed` (frames of the failed attempt may still be in flight; close the link and open a new one). `ErrAlreadyNegotiated` while `Negotiating`/`Negotiated`. Any error during an attempt (rejection, invalid packet, cancelled context, closed link) moves to `Failed`.
* Advanced calls before success fail with `ErrNotNegotiated`; `Send/Recv` require `FeatureMessages`, `Call/Serve` require `FeatureRequestResponse` (otherwise `ErrNotNegotiated`).

Negotiation packets carry no payload.

### 10.2 Identifiers and parity **[IOP]**

| ID | Scope | Rule |
| --- | --- | --- |
| `ConnectionID` (stream id) | per link | `0` is the link-level stream (always open, cannot be closed). Non-zero ids are allocated **by parity**: the Primary allocates even ids `2, 4, 6, …` (`n << 1`), the Secondary odd ids `1, 3, 5, …` (`2n - 1`). Ids are never reused on a link. |
| `MessageID` | per sender | Identifies one logical message; starts at 1 and increases by 1 per message (data, request) across **all streams** of the sender; never `0` (a counter wrap skips `0`). A response carries the **request's** `MessageID`. |
| `MethodID` | application | Selector in a request's `aux` (operand 6) on its first chunk. |

The receiving side identifies a message by `(ConnectionID, MessageID, kind)` with `kind ∈ {data, request, response}` taken from the Request/Response flags. Because each sender uses one counter for data and requests and a response reuses the request's id, keys of different kinds never collide.

### 10.3 Stream control **[IOP]**

* `OpConnCreate(connectionID)`: the opener of a stream allocates an id of its own parity, registers the stream as pending and sends it. The peer validates and replies: `OpConnAccept(connectionID)` if accepted, otherwise `OpError(connectionID, code)`:

  | Refusal | Code |
  | --- | --- |
  | not listening (`ListenStreams` not called), once the request cannot be held or its grace period has passed (below) | `ErrCodeConnNotFound` (5) |
  | id `0`, wrong parity for the sender's role (the peer allocates `peerOwnsConnectionID`: odd ids belong to the Secondary, non-zero even ids to the Primary), or already registered (the existing stream is left untouched) | `ErrCodeInvalidOp` (6) |
  | accept queue full under `OverflowDrop` | `ErrCodeBufferOverflow` (7) |

  Under `OverflowFail` a full accept queue fails the receive path (`ErrIncomingOverflow`) instead of replying. An `OpError` whose `Operand0` matches no pending `OpenStream` fails the receiving link with the error of its code (an unknown code matches `ErrProtocol`).
* **Requests before `ListenStreams` [IOP for the rule, IMPL for the bound and timers]:** an `OpConnCreate` that arrives while the link is not listening is **held** for a grace period `G` instead of being refused at once. `G` is an endpoint option (Go `AdvancedOptions.StreamListenGrace`, C `hqq_adv_options.stream_listen_grace_ns`); the default is 2 s, and a negative value disables holding. At most `Q` requests are held at once, where `Q` is the incoming queue size (`IncomingQueueSize`, default `bufferCount`), and the held requests plus the accept slots in use (streams not yet returned by `AcceptStream`) MUST NOT exceed `Q`. A request that finds the bound reached, or that arrives while `G` is negative, is refused at once with `ErrCodeConnNotFound`. Holding only queues the request; the receive path never waits for it. When `ListenStreams` runs, the held requests are processed in arrival order exactly as if they had just arrived: the parity and duplicate checks, the slot reservation and the `OpConnAccept` reply are the same. A request still held when `G` has passed is refused with `ErrCodeConnNotFound`, and so is every held request when the link fails (its receive path ends). The refusal is queued like any other reply, so it can follow the expiry by the scheduling delay of the control sender. Close discards the held requests without a reply (the Go `Shutdown` refuses them at once, as an API convenience); a peer whose request is discarded sees its own context end or the link end.
* `OpConnClose(connectionID, 0)`: the receiver unregisters the stream and marks it closed by the peer; an unknown id, or one whose open is still pending, is ignored. `Stream.Close` marks the stream closed locally first (a second close returns `ErrClosed`), unregisters it, discards its queued messages, and then sends `OpConnClose` unless the peer closed first. Closing stream 0 returns `ErrInvalidOp`.
* **Accept ordering [IOP]:** an incoming stream is registered at once, but becomes visible to `AcceptStream` only after its `OpConnAccept` was enqueued on the tx ring, so data the acceptor sends on the stream cannot overtake the accept on the wire.
* **The receive path never waits for the tx ring [IOP for the property, IMPL for the mechanism]:** if two peers flood each other, both tx rings fill, and a receiver waiting for its tx ring would wait for a peer that no longer drains. Replies generated by the receive path (`OpConnAccept`, refusals, refusal responses) are queued on a bounded control queue (`max(bufferCount, 16)` jobs) drained by a separate sender; a full queue drops the reply and counts it. A port MUST ensure that its receive loop cannot deadlock in this way.

### 10.4 `OpConnCopy`: chunking and flags **[IOP]**

Operands: `0 ConnectionID`, `1 MessageID`, `2 TotalSize`, `3 ChunkOffset`, `4 ChunkSize`, `5 flags`, `6 aux`.

Flags (operand 5): `Begin = 1<<0`, `End = 1<<1`, `Abort = 1<<2`, `Request = 1<<3`, `Response = 1<<4`, `Error = 1<<5` (known mask `0x3F`).

**Sender** (`sendAdvancedPayload`): a message of `len` bytes, `len <= MaxMessageSize` (default 64 MiB; larger is refused locally with `ErrBufferOverflow`, nothing sent), is sent as chunks in order on the transmit ring, each chunk in its own ring slot with its payload in that slot's buffer:

```text
if len == 0: one frame: TotalSize = 0, ChunkOffset = 0, ChunkSize = 0, flags = Begin|End|base, aux
else: for offset = 0; offset < len; offset += bufferSize:
         end = min(offset + bufferSize, len)
         flags = base | (offset == 0 ? Begin : 0) | (end == len ? End : 0)
         aux   = (offset == 0) ? aux : 0            # aux only in the first chunk
         send OpConnCopy(conn, msgID, len, offset, end - offset, flags, aux) with payload[offset:end] in the slot buffer
```

`base` is `0` for data, `Request` for a request (aux = MethodID), `Response` for a response (aux = status, 0 on success), `Response|Error` for an error response (aux = status code, payload = error payload). **Chunks of a message are never interleaved with another message's chunks by the same sender thread, but chunks of different messages of one sender (different threads) may interleave on the ring**; the receiver reassembles by key. Every chunk except the last carries `bufferSize` payload bytes in the Go implementation, but receivers MUST NOT rely on this (they accept any `ChunkSize >= 1`, see below).

**Abort.** If sending chunk `offset > 0` fails (for example the context ended), the sender makes a best effort to send an **abort frame** with an independent short deadline (100 ms): `OpConnCopy(conn, msgID, 0, 0, 0, Abort | base, 0)` (no `Begin`/`End`). A failure on the very first chunk sends nothing.

### 10.5 Receiver validation and reassembly **[IOP]**

For each `OpConnCopy` the receiver, holding the slot, validates and acts in this order. "Fail" means the **receive path fails** with the stated error (the link's Advanced receive loop stops and the error is returned to pending and future calls); the partial message is dropped first.

```text
1. if flags & ~0x3F != 0                       -> FAIL ErrProtocol
   if Request and Response both set            -> FAIL ErrProtocol
   if Error set and Response not set           -> FAIL ErrProtocol
   kind = flags & (Request | Response); key = (conn, msgID, kind)

2. if Abort set:                                # accepted whatever the other operands are
       drop assembly[key] if any
       if key was in the "refused" set: forget it, silently done      # already surfaced at refusal
       else outcome = ABORTED(header)           # a response fails the waiting call (ErrMessageAborted);
                                                # an aborted request or data message is dropped silently
       done (no further checks)

3. validate (each failure drops assembly[key], forgets the refused entry, and FAILS the path):
       ChunkSize > bufferSize, or ChunkOffset > TotalSize, or ChunkSize > TotalSize - ChunkOffset   -> ErrInvalidSize
       TotalSize > MaxMessageSize                                                                     -> ErrBufferOverflow
       TotalSize == 0 and (ChunkOffset != 0 or ChunkSize != 0 or flags & (Begin|End) != Begin|End)    -> ErrInvalidSize
       TotalSize > 0 and ChunkSize == 0                                                               -> ErrInvalidSize
       ChunkOffset != 0 and aux != 0                                                                  -> ErrInvalidSize
       TotalSize > INT_MAX of the platform                                                            -> ErrBufferOverflow

4. if Begin set:
       if ChunkOffset != 0, or assembly[key] exists, or key is in the refused set -> ErrInvalidSize
       if End set (single-chunk message):
            if ChunkSize != TotalSize                                             -> ErrInvalidSize
            complete: message = payload[0:ChunkSize] with header (conn, msgID, flags, aux); route it (10.7)
       else (multi-chunk):
            charge = TotalSize + 256
            if charge > MaxReassemblyBytes or assemblyBytes > MaxReassemblyBytes - charge:
                 REFUSE: remember key in the refused set (at most 1024 keys; oldest evicted);
                         outcome = REFUSED(header with Abort flag); this chunk is consumed, done   # see below
            else: create assembly[key] = {hdr = flags minus (Begin|End), aux, size = TotalSize, next = 0}
                  assemblyBytes += charge

5. now an assembly must exist:
       if none: if key in the refused set: (drain) if End set, forget the key; discard this chunk and continue
                else, when no refused entry was ever evicted (FINDINGS F-07)     -> FAIL ErrProtocol
       if ChunkOffset != assembly.next or assembly.size != TotalSize         -> ErrInvalidSize
       append payload[0:ChunkSize] at ChunkOffset; assembly.next += ChunkSize
       if End not set: done (partial)
       if assembly.next != TotalSize                                          -> ErrInvalidSize
       complete: message with header (conn, msgID, assembly flags | Begin | End, aux of the first chunk);
                 assemblyBytes -= charge; drop assembly; route it
```

* **Reassembly limits [IOP]**: `MaxMessageSize` (default `64 MiB`) bounds `TotalSize`; `MaxReassemblyBytes` (default `4 × MaxMessageSize`, saturating) bounds the sum of `TotalSize + 256` over all incomplete multi-chunk messages; single-chunk messages are not charged. The memory actually held may exceed the charge (buffers are rounded up to a power of two) — that is an implementation matter. A port MUST NOT accept a multi-chunk message that would exceed the budget; it MUST drain the remaining chunks of a refused message (until its `End` chunk) instead of failing the path. Receiver policy, not wire behavior: an incomplete message whose last chunk is at least `2^16` received chunks old may be reclaimed when a multi-chunk message needs its budget; it is then treated as refused (a response fails with `ErrReassemblyLimit`, a request is answered as below) and its remaining chunks are drained (FINDINGS F-09).
* **Refusal handling [IOP]**: a refused **request** is answered with an error response: `OpConnCopy(conn, msgID, 0, 0, 0, Begin|End|Response|Error, aux = ErrCodeBufferOverflow)` (best effort; only if `FeatureRequestResponse` was negotiated; queued on the control sender); a refused **response** fails the waiting call with `ErrReassemblyLimit`; a refused **data** message is dropped. All refusals are counted.
* **Frame validity [IOP]**: a non-`Begin` chunk with no assembly and no refused entry is `ErrProtocol`, unless an entry was ever evicted from the refused set (at most 1024 keys), after which such chunks are drained (FINDINGS F-07). Later chunks continue exactly at the previous end (`ChunkOffset == next`), use the announced `TotalSize`, and the `End` chunk completes `TotalSize` exactly. A message whose `End` never arrives is reclaimed as described above (FINDINGS F-09).

### 10.6 Request / response correlation **[IOP]**

`Call(stream, method, request)`: `messageID = nextMessageID()`; register a pending call under `key = (streamID, messageID)`; send the request message (`Request`, aux = method); wait for the response. Responses may arrive in any order; many calls may be outstanding.

A complete **response** (`Response` set) is delivered to the pending call registered under `(ConnectionID, MessageID)`. A response without a waiting call is dropped and counted, **unless** its call was cancelled: when a call times out, its context ends, or its stream is closed while it waits, the key is recorded in a bounded set of abandoned calls (4096 entries, oldest evicted) and the late response is discarded and counted as abandoned. If `Error` is set the call fails with a `ResponseError{Status = aux, Payload}`; an aborted response fails the call with `ErrMessageAborted`; one refused by the local reassembly limit with `ErrReassemblyLimit`. The responder sends the response on the stream the request arrived on, with the request's `MessageID`.

### 10.7 Routing and overflow **[IOP for destinations, IMPL for queueing]**

After the receive path assembled a message it routes it **in arrival order**:

| Message | Destination |
| --- | --- |
| response (`Response`) | the pending call with the same `(ConnectionID, MessageID)` |
| request (`Request`) | the link's request queue (consumed only by `Serve`), for every stream; the request is *not* dropped because its stream is unknown |
| data (neither) | the queue of stream `ConnectionID`; data for an unknown or closed stream is dropped and counted |

Each destination queue holds `IncomingQueueSize` messages (default `bufferCount`); when full, the **overflow policy** applies: `OverflowBlock` (default): the receive path waits for the consumer (backpressure propagates into the ring; nothing else is routed meanwhile, not even responses); `OverflowDrop`: drop the message (stream creation is refused with `ErrCodeBufferOverflow`); `OverflowFail`: fail the receive path with `ErrIncomingOverflow`. These are **[IMPL]** policies; the wire effect is only the refusal code above.

### 10.8 Advanced errors on the wire **[IOP]**

Protocol violations of the peer fail the local receive path; nothing is sent back except the `OpError` cases listed (negotiation rejection, stream refusal). The sender of a violating frame learns about the failure only through the link ending or its pending operations failing.

## 11. Platform naming, creation and unlinking

### 11.1 Name validation **[IOP]**

A name is a string. One leading `/` is removed; the remainder `base` MUST be non-empty, MUST NOT be `"."` or `".."`, MUST NOT contain `/`, `\` or NUL, and MUST NOT exceed 255 bytes (`ErrInvalidName` otherwise). Vectors: `constants.json` `platform.name_validation.invalid_vectors`.

| Platform | Object | Extra limit |
| --- | --- | --- |
| Linux | regular file `/dev/shm/<base>` | none beyond 255 |
| macOS | `shm_open("/<base>")` | **the whole string including the leading slash is at most 31 bytes (`PSHMNAMLEN`)**, so `base` has at most 30 bytes |
| FreeBSD | `shm_open("/<base>")` | at most 1023 bytes including the slash |
| Windows | file mapping `Local\<base>` (default) | the kernel object name plus the longest semaphore suffix (`.hqq-sem0`, 9 chars) must fit 259 UTF-16 units |

On Windows a name that starts with `Global\` or `Local\` (ASCII case-insensitive, and followed by at least one more character) keeps that namespace prefix (normalized to the canonical spelling) and the rest is validated as `base`; any other name gets `Local\`. `Global\` or `Local\` alone is invalid: the whole string is then validated as a base name and the backslash is forbidden. Vectors: `platform.windows.name_vectors`.

### 11.2 Linux object handling **[IOP]**

* Open flags: `O_RDWR`, `O_CLOEXEC`, `O_NOFOLLOW`, plus `O_CREAT|O_EXCL` when creating. After opening, `fstat` MUST show a **regular file**; otherwise `ErrInvalidName` and nothing is modified (`/dev/shm` is typically world-writable: names can be planted).
* A created object is allocated with `fallocate(fd, 0, 0, size)` (retry on `EINTR`; fall back to `ftruncate` only on `EOPNOTSUPP`/`ENOSYS`) so that a full tmpfs fails at creation and not with `SIGBUS` later. A failed creation removes the object again.
* Mapping: `mmap(NULL, size, PROT_READ|PROT_WRITE, MAP_SHARED, fd, 0)`; the result MUST be page aligned.
* Default permission when creating through the high-level `Listen`: `0600`.

### 11.3 Create, open, existing names **[IOP]**

* `Create`/`Listen` create the object **exclusively** (`O_EXCL`). If the name exists they open the existing object (`O_RDWR`, not creating, with up to three retries if it vanished between the two calls) and apply the Creator rules of section 4:
  * an object **smaller** than the requested segment returns `ErrInvalidSize`, a **zero-size** object the transient `errSegmentNotReady` (neither is resized or modified); a larger object is used as is (the first `total` bytes are mapped);
  * a segment without live endpoints is re-initialized and owned by the caller (new `creatorToken`);
  * a segment with a live endpoint returns `ErrLinkBusy`;
  * a foreign object or other layout version returns `ErrIncompatibleLayout` and is not modified;
  * Linux only: a name that is not a regular file returns `ErrInvalidName`.
* `Open`/`Dial` never create and never initialize: a missing name is `ErrNotExist`; a zero-size object is the transient `errSegmentNotReady`; a smaller object is `ErrInvalidSize`; an empty or initializing segment and a segment without live endpoints fail with the transient errors of section 4.
* **Creator sizes the object right after creating it.** An opener that sees size 0 treats it as "creator has not finished"; touching a mapping beyond the end of the object would raise `SIGBUS`, so an opener MUST check the object size **before** mapping.
* A created object's **`CreatorToken`** is stored by the initializer at claim (5.2) and identifies the instance.

### 11.4 Name ownership and safe unlink **[IOP]**

A Creator owns the name: closing a Creator removes the name automatically; closing an Opener never does. `Unlink` (any endpoint of the instance) and the Creator's `Close` remove the name **only if it still refers to this endpoint's instance**; a missing or replaced name is a no-op returning success. The *instance identity* is the object plus the `creatorToken` in its superblock: a Creator's own token; an Opener's is the `creatorToken` it read from the segment right after joining.

| Platform | Identity test before `unlink` |
| --- | --- |
| Linux | `fstat` of the open descriptor has the same `(st_dev, st_ino)` as `lstat` of the path **and** the `creatorToken` in the mapping equals the endpoint's instance token (`creatorToken != 0`), or (for a freshly created, never claimed object) the superblock state is still `Empty` |
| macOS, FreeBSD (no inode identity for shm names) | re-open the name read-only (`shm_open(..., O_RDONLY)`), `fstat` it (size must be at least one page), map the first page read-only and compare its `creatorToken` (`!= 0` and equal to the endpoint's instance token) or `state == Empty` for a fresh object; a name that cannot be opened or is too small is "not ours" |
| Windows | never anything to remove: names disappear with their last handle; `Unlink` is a no-op |

The decision is made **while the mapping and descriptor are still open**; the name is removed after the descriptor is closed.

### 11.5 Windows names and semaphores **[IOP]**

Per segment there are `SemaphoreCount = 4` named kernel semaphores whose names are the mapping's kernel object name plus a suffix:

| Index | Suffix | Role |
| ---: | --- | --- |
| 0 | `.hqq-sem0` | ring 0 notEmpty (waiters: Secondary consumer; waker: Primary producer) |
| 1 | `.hqq-sem1` | ring 0 notFull |
| 2 | `.hqq-sem2` | ring 1 notEmpty |
| 3 | `.hqq-sem3` | ring 1 notFull |

* **Creation order**: the creator makes the four semaphores **before** the mapping, so every endpoint that can open the mapping finds the semaphores. With `O_EXCL` semantics, creation fails with "exists" if any of the semaphore names or the mapping name already exists (closing the handles it opened). An existing name always means a live handle somewhere (Windows names vanish with their last handle), so a creator that finds an existing name opens it and follows the Creator rules (a live segment gives `ErrLinkBusy`).
* **Open order**: open the mapping (`FILE_MAP_READ|FILE_MAP_WRITE|SECTION_QUERY`, the latter to learn the size with `NtQuerySection` and refuse a too-small mapping), then open the four semaphores (`SYNCHRONIZE|SEMAPHORE_MODIFY_STATE`).
* **Size**: the mapping is created with `CreateFileMapping(INVALID_HANDLE_VALUE, sa, PAGE_READWRITE, size, name)` (paging-file backed). A fresh section is zero-filled.
* Semaphores: initial count 0, maximum `2^31 - 1`. `Wait(timeout)`: `WaitForSingleObject` with the timeout rounded up to whole milliseconds, at least 1 ms. `Release(n)`: `ReleaseSemaphore(h, n, NULL)` with `n` clamped to `2^31 - 1`, errors ignored.
* Security: the creator's `mode` is translated to a DACL (owner bits to the creating user, "other" bits to Everyone, SYSTEM always full; group bits ignored); the same DACL is applied to the semaphores (read allows waiting, write allows releasing). **[IMPL]** for ports; a port SHOULD provide equivalent least-privilege defaults.

## 12. Error codes and error mapping **[IOP for codes on the wire]**

`ErrorCode` values carried in `OpError.Operand1`, tombstone `Operand2`, and Advanced error-response `aux`:

| Code | Name | Maps to |
| ---: | --- | --- |
| 0x01 | `ErrCodeMemoryAlign` | `ErrMemoryAlign` |
| 0x02 | `ErrCodeInvalidSize` | `ErrInvalidSize` |
| 0x03 | `ErrCodeMemorySmall` | `ErrMemorySmall` |
| 0x04 | `ErrCodeFailedInit` | `ErrFailedInit` |
| 0x05 | `ErrCodeConnNotFound` | `ErrConnNotFound` |
| 0x06 | `ErrCodeInvalidOp` | `ErrInvalidOp` |
| 0x07 | `ErrCodeBufferOverflow` | `ErrBufferOverflow` |
| other | — | an error matching `ErrProtocol` ("peer reported unknown error code") |

An application status in an Advanced error response (`Response|Error`, `aux`) is **not restricted to this table**: handlers may send any non-zero `u64` status (a zero status is sent as `ErrCodeInvalidOp`). The receiver exposes it as `ResponseError{Status, Payload}`.

Error taxonomy (names are those of the Go API; a port SHOULD provide the equivalent categories). Every public error matches itself and the base it wraps:

| Sentinel | Wraps | When |
| --- | --- | --- |
| `io.EOF` | — | `StandardLink.Read/ReadFrom` after a graceful peer close once buffered messages are consumed |
| `ErrClosed` | `net.ErrClosed` | local link or stream closed |
| `ErrPeerClosed` | `io.EOF` | peer detached gracefully; peer closed the stream |
| `ErrPeerGone` | `ErrConnNotFound` | peer stopped heartbeating, was replaced, or the segment was re-initialized |
| `ErrTimeout` | `os.ErrDeadlineExceeded` | deadline passed |
| `ErrLinkBusy` | `ErrFailedInit` | segment has a live endpoint for a creator; two live endpoints for a joiner; the live endpoint's earlier peer already left |
| `ErrIncompatibleLayout` | `ErrFailedInit` | foreign magic, other layout major (including `HQQ_STL2`), unknown `requiredFeatures` bit, unknown `state`, non-zero page of an `Empty` segment, ring header that does not match |
| `ErrInvalidSize` | — | invalid geometry or size bounds; an existing smaller object; geometry mismatch (`GeometryError`); invalid `DataSize`/chunk bounds |
| `ErrMemoryAlign` / `ErrMemorySmall` | — | caller-owned memory not page-aligned / too small |
| `ErrFailedInit` | — | base of initialization failures, including the transient ones |
| `ErrInvalidName` | — | bad name; on Linux also not a regular file |
| `ErrBufferOverflow` | — | payload larger than `bufferSize` (Standard) or `MaxMessageSize` (Advanced) |
| `ErrInvalidOp` | — | base of protocol-state errors |
| `ErrNotNegotiated`, `ErrAlreadyNegotiated`, `ErrNegotiationFailed`, `ErrNegotiationRejected`, `ErrUnexpectedPacket`, `ErrProtocol` | `ErrInvalidOp` | negotiation and protocol-state violations |
| `ErrMessageAborted` | `ErrProtocol` | the peer abandoned a response mid-transfer |
| `ErrIncomingOverflow`, `ErrReassemblyLimit` | `ErrBufferOverflow` | queue overflow under `OverflowFail`; response refused by the reassembly limit |

## 13. Interoperability summary

### 13.1 What is normative for interoperability

All **[IOP]**-tagged rules. Summary of the contract two processes of different implementations share:

1. The segment layout (2), the superblock, endpoint record and tokens (3), including the page size and geometry rules.
2. The claim/init/takeover/reuse/publish/join state machines on the superblock words, with their CAS operations and store order (5), and the liveness/heartbeat/peer-state derivation (6) — any implementation may *observe* these words, so they must mean the same thing.
3. The ring memory image, Format, Attach and the slot sequence protocol (7), with the memory orders of MEMORY_MODEL.md.
4. The waiter/waker handshake and counters (8.2, 8.3), and the platform primitives on which processes of different implementations meet (8.5): futex on Linux, ulock on macOS, the four named semaphores on Windows.
5. Packet ABI, opcodes, operands, Standard copy/tombstone/error semantics, slot-owned buffers (9).
6. The Advanced wire protocol: negotiation, ids and parity, stream control, chunking, flags, abort, validation, reassembly limits, request/response correlation, refusal responses (10).
7. Name mapping and safe-unlink rules (11), error codes on the wire (12).

### 13.2 What is implementation freedom

* Threading model, process-local locks and the SPSC mode of 7.6 (as long as the shared-word protocol is followed).
* **Simple mode:** no parking at all (spin plus bounded sleep backoff). Valid; cost is CPU or latency. A simple-mode implementation never registers as a waiter; peers that park on it are woken by their bounded park timeout (at most 100 ms) unless the simple-mode side also performs the waker duty (8.3), which is RECOMMENDED (it is two cheap accesses).
* Process-local waiter aggregation (8.6), backoff constants (8.4), heartbeat scheduling, deadlines API, `Read` tails / datagram truncation, zero-copy and reservation APIs, pooled `Message` buffers, queueing and overflow policies, control-queue sizing, `Serve` execution model.
* A **geometry-discovery** extension: an Opener MAY first peek at a `Ready` superblock (read-only mapping, plain reads of `state`, `pageSize`, `bufferCount`, `bufferSize`, `layoutSize`, `creatorToken`) and then open with the recorded geometry, as long as all validation rules of 5.5 still run on the real mapping.
* Additional API surface (listeners, acceptors) built on the same words, provided it does not change the meaning of a superblock/ring word (changes to the protocol itself are versioned through `Major`/`ring layout version`/protocol version).

### 13.3 Unsupported platforms **[IOP]**

Lock-free 64-bit atomics that are **atomic across processes** are required. 32-bit MIPS and ARM before ARMv7 (where 64-bit atomics are emulated with process-local locks) are unsupported. **Big-endian hosts are unsupported** (all words are little-endian and the code does not byte-swap shared words). Details in MEMORY_MODEL.md section 7.

## 14. Reference vectors

* Packet bytes: `constants.json` → `packet_vectors` (9 vectors; the hex strings are 64 bytes).
* Region offsets for three page sizes and four geometries: `segment.layout_vectors`.
* A minimal conforming image (after a Primary `Ready` on caller-owned memory, `count = 4`, `size = 64`, 16 KiB pages, heartbeats disabled), checked by `TestSpecSegmentImage`: `magic = 0x4851515F53544C33`, `state = 2`, `major = 3`, `minor = 0`, `pageSize = 16384`, `bufferCount = 4`, `bufferSize = 64`, `layoutSize = 81920` (`ring0 = 16384`, `buf0 = 32768`, `ring1 = 49152`, `buf1 = 65536`), `epoch = 1`, `initToken = 0`, `requiredFeatures = 0`, `creatorToken = 0` (caller-owned memory), `endpoints[0] = {owner token, pid, heartbeat, 0}`, `endpoints[1] = {0,0,0,0}`; both rings: `magic = 0xC9D8C1D43F096702`, `size = 4`, `flag = 2`, `stride = 128`, `version = 2`, `r = w = 0`, `seq[i] = i`, packets zero.

## 15. Defects and ambiguities

See [FINDINGS.md](FINDINGS.md). Each finding is classified (a) documentation only, (b) behavior fix without ABI change, (c) needs a layout/wire change before a second implementation exists.
