# HQQ Wire and Memory Protocol: Normative Specification

**Baseline**: Segment layout 3.0 (magic `HQQ_STL3`), MPMC ring layout 2, and wire protocol 1.0 (with Protocol 1.1 extensions in [`ext/PROTOCOL_1_1_DRAFT.md`](ext/PROTOCOL_1_1_DRAFT.md) and listener segments in [`ext/LISTENER_DRAFT.md`](ext/LISTENER_DRAFT.md)).

**Companion documents**:
- [`MEMORY_MODEL.md`](MEMORY_MODEL.md): Atomic memory orders and happens-before proofs.
- [`PORTING.md`](PORTING.md): Incremental implementation guide for C, C++, Rust, and other languages.
- [`FINDINGS.md`](FINDINGS.md): Specification audit findings and edge-case resolutions.
- [`constants.json`](constants.json): Machine-readable numeric constants verified against the code by `go test ./conformance/...`.
- [`litmus/`](litmus/): Formal C11 memory-model litmus tests.

This specification defines the complete shared-memory layout and wire protocol so that independent implementations (such as `libhqq` in [`c/`](../c)) can be written from this text alone and interoperate with the Go implementation in the same shared-memory segment.

## 0. Conventions

Normative keywords (**MUST**, **MUST NOT**, **REQUIRED**, **SHOULD**, **SHOULD NOT**, **MAY**, **RECOMMENDED**) follow RFC 2119.

Every rule carries one of two scope tags:

| Tag | Meaning |
| --- | --- |
| **[IOP]** | **Interoperability-normative**: Governs the byte layout in shared memory, the ordering or atomicity of shared-memory accesses, or externally observable protocol behavior. Two implementations that diverge on an **[IOP]** rule can corrupt shared state or deadlock. |
| **[IMPL]** | **Implementation freedom or recommendation**: Governs process-local threading, scheduling, caching, API shape, or wait heuristics that a peer process cannot observe (except via latency). |

Notation:
- `u64` and `u32` denote unsigned 64-bit and 32-bit integers stored in **little-endian** byte order at natural alignment.
- `load`, `store`, `cas`, and `fetch_add` denote atomic operations with the memory orders specified in [`MEMORY_MODEL.md`](MEMORY_MODEL.md).
- Subtraction `a - b` on `u64` wraps modulo `2^64`; `int64(x)` reinterprets the 64-bit pattern as signed two's complement.
- `round_up(x, p)` denotes the smallest multiple of `p` that is `>= x`.
- Numeric values of all constants are pinned in [`constants.json`](constants.json).

## 1. Terminology and Topology

An HQQ link operates over the following structural elements:

- **Segment**: One contiguous region of shared memory (a POSIX `shm_open` / `/dev/shm` file, a Windows named file mapping, or caller-owned memory such as an anonymous `MAP_SHARED` region) carrying one link.
- **Link**: A bidirectional connection between two **endpoints**: the **Primary** (endpoint index `0`) and the **Secondary** (endpoint index `1`).
- **Direction 0**: Carries Primary -> Secondary traffic over `ring 0` and `buffer pool 0`.
- **Direction 1**: Carries Secondary -> Primary traffic over `ring 1` and `buffer pool 1`. Each direction comprises one bounded Vyukov MPMC **ring** of 64-byte packets and one **buffer pool** of `bufferCount` fixed-size payload buffers, where ring slot `i` exclusively owns payload buffer `i`.
- **Process**: The unit of cross-process sharing. All threads within one OS process operating on one side of a ring act as a single party.
- **Position (`pos`)**: A monotonically increasing `u64` counter tracking claims on a ring cursor. Position `pos` maps to slot index `pos & mask` (where `mask = size - 1`) and lap `pos / size`.
- **Attach roles** (section 4): **Creator** (creates or reclaims a named segment), **Opener** (attaches to an existing named segment as Secondary), and **InMemory** (attaches to caller-supplied memory).

**[IOP] Cross-process topology**: For every directional ring there is at most **one producer process and one consumer process**: on `ring 0`, the Primary is the producer and the Secondary is the consumer; on `ring 1`, the Secondary is the producer and the Primary is the consumer. A third endpoint is never admitted (section 5.5). Within a single process, multiple threads `MAY` produce or consume concurrently provided the process as a whole obeys the shared-word protocol (see section 7.6 for single-producer/single-consumer local serialization).

## 2. Segment Memory Layout

An HQQ link segment is divided into five page-aligned regions whose offsets are a pure function of `(pageSize, bufferCount, bufferSize)`.

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

Given `page = system page size` (`sysconf(_SC_PAGESIZE)` on POSIX, `GetSystemInfo().dwPageSize` on Windows; a power of two `>= 4096`), `count = bufferCount`, and `size = bufferSize`:

```text
ringBytes = 512 + 128 * count                  (header 512 bytes, slot stride 128 bytes)
bufBytes  = count * size
ring0     = round_up(384, page)                (= page, for every page size >= 4096)
buf0      = round_up(ring0 + ringBytes, page)
ring1     = round_up(buf0  + bufBytes,  page)
buf1      = round_up(ring1 + ringBytes, page)
total     = round_up(buf1  + bufBytes,  page)
```

All offsets are measured from the start of the segment, which `MUST` be page-aligned. `total` is recorded in the superblock's `layoutSize` field and defines the minimum byte size of the backing shared-memory object. Reference layout vectors for page sizes `4096`, `16384`, and `65536` are pinned in `constants.json` (`segment.layout_vectors`).

Payload buffer `i` of pool `d` begins at byte offset `buf_d + i * size` and is owned exclusively by ring slot `i` of ring `d` (section 9.4). Payload pools are plain memory and are never accessed with atomic instructions by the protocol.

**[IOP] Addressing from caller geometry**: Region offsets are always computed from the caller's configured `(bufferCount, bufferSize)` and the host's runtime `pageSize`; the geometry words stored in the superblock are used strictly for validation comparisons, never for pointer arithmetic. An implementation `MAY` offer a separate discovery helper that peeks a `Ready` superblock first and then opens the segment using the discovered geometry (section 13.2).

**[IOP] Page-size agreement**: Because region offsets depend on `page`, endpoints with different OS page sizes cannot share a segment; comparing `pageSize` in the superblock surfaces any mismatch as `GeometryError` (section 5.5).

### 2.2 Geometry Limits **[IOP]**

| Parameter | Validation Rule |
| --- | --- |
| `bufferCount` | Power of two, `2 <= bufferCount <= 2^24` (`16,777,216`). A 1-slot ring is forbidden because with `N = 1` (`mask = 0`) the published sequence value `pos + 1` and the next-lap free value `pos + mask + 1` would be identical. |
| `bufferSize` | Multiple of 8, `8 <= bufferSize <= 2^30` (`1 GiB`). |
| `total` | Must not overflow the platform's signed or unsigned address/file-offset representation. |

Any configuration outside these bounds is rejected before touching shared memory (`SizeStandardLink` returns `0`, and link creation/open returns `ErrInvalidSize`).

## 3. The Superblock

The superblock occupies the first page (`page` bytes) of the segment and holds layout metadata, initialization state, and the two endpoint records.

### 3.1 Superblock Fields **[IOP]**

Every superblock field is an 8-byte aligned `u64` accessed **atomically** (see [`MEMORY_MODEL.md`](MEMORY_MODEL.md)):

| Offset | Field | Writer(s) | Meaning |
| ---: | --- | --- | --- |
| `0` | `magic` | Initializer, at publish | `0x4851515F53544C33` (ASCII `"HQQ_STL3"` in big-endian; bytes in little-endian memory read `33 4c 54 53 5f 51 51 48`, `"3LTS_QQH"`). Written at publish step `P5` (section 5.3); an `Initializing` segment may still hold `0`. |
| `8` | `state` | Claimers, initializer | `0` (`Empty`), `1` (`Initializing`), or `2` (`Ready`). Any other value fails with `ErrIncompatibleLayout`. |
| `16` | `major` | Initializer | Layout major version (`3`). Stamped when claiming initialization and again at publish. Any other major version is incompatible. |
| `24` | `minor` | Initializer | Layout minor version (`0`, informational). |
| `32` | `pageSize` | Initializer, at publish | Host page size used to compute region offsets. |
| `40` | `bufferCount` | Initializer, at publish | Configured slots/buffers per direction. |
| `48` | `bufferSize` | Initializer, at publish | Configured payload bytes per buffer. |
| `56` | `layoutSize` | Initializer, at publish | Total segment size `total` from section 2.1. |
| `64` | `epoch` | Initializer, at commit | Generation counter incremented via `fetch_add(1)` once per successful (re)initialization immediately before `state` becomes `Ready` (`1` on first initialization). |
| `72` | `initToken` | Claimers | Session token of the endpoint currently initializing the segment (`0` when not initializing). Serves as the initialization ownership lock (section 5.2). |
| `80` | `initHeartbeat` | Initializer | Wall-clock `UnixNano` of the initializer's last progress step (used as an opaque change marker). |
| `88` | `requiredFeatures` | Initializer | Bitmask of required layout features (`0` in layout 3.0). Readers observing any set bit `MUST` fail with `ErrIncompatibleLayout`. |
| `96` | `creatorToken` | Initializer, right after winning `initToken` | Instance identity of the segment creator: a fresh random token for a `Creator` endpoint, or `0` for an `InMemory` endpoint. Replaced on every initialization; never written by `Opener` endpoints. |
| `104..127` | `reserved` | None | Three 64-bit words, always `0`. |
| `128..255` | `endpoints[0]` | Primary, initializer | Primary endpoint record (128 bytes, section 3.2). |
| `256..383` | `endpoints[1]` | Secondary, initializer | Secondary endpoint record (128 bytes, section 3.2). |
| `384..page-1` | `unused` | None | Zero. **The `firstPageZero` check in section 5.1 inspects the entire first page**, so implementations `MUST NOT` store data in bytes `384..page-1`. |

Because `sizeof(superblock) == 384 <= 4096`, the superblock always fits within a single page.

### 3.2 Endpoint Record (`128` Bytes) **[IOP]**

| Relative Offset | Field | Meaning |
| ---: | --- | --- |
| `+0` | `token` | `0` = unused since the last initialization; `1` = owner detached gracefully (`detachedToken`); any other value = random 64-bit session token of the attached owner (an **owner token**, never `0` or `1`). |
| `+8` | `pid` | OS process ID of the owner (diagnostic only; not comparable across PID namespaces). |
| `+16` | `heartbeat` | Most recent heartbeat timestamp in wall-clock `UnixNano` (`0` = none written yet). |
| `+24` | `interval` | Owner's configured heartbeat interval in nanoseconds (`0` = heartbeats disabled, treated as permanently alive). |
| `+32..+127` | `reserved` | Twelve 64-bit words, always `0`. |

**[IOP] Owner token generation**: Each endpoint draws its owner token from a CSPRNG, redrawing if the value is `0` or `1`.

**[IOP] Graceful detach**: Closing an endpoint executes `cas(record.token, myToken, 1)`. If the CAS fails (because the segment was re-initialized or taken over), the close succeeds locally without retrying or overwriting `record.token`. Keeping `token == 1` until the next re-initialization ensures that a peer attaching or polling later observes `ErrPeerClosed` (`io.EOF`) and prevents a third party from reusing a closed Secondary slot in the same epoch.

### 3.3 Atomic Access Requirement **[IOP]**

All superblock words are 8-byte aligned 64-bit words and `MUST` be accessed using hardware lock-free 64-bit atomic operations. See [`MEMORY_MODEL.md`](MEMORY_MODEL.md) section 2 for the exact memory order of each field.

### 3.4 Superblock State Machine **[IOP]**

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

State transitions are performed exclusively by the claim loop (section 5.1) and the initializer commit step (section 5.3). `Empty` (`0`) is the initial state of a freshly zeroed segment and is never re-entered.

## 4. Attach Roles and Entry Points

How a caller reaches a segment determines its **role** (`attachRole`):

| Role | Go Entry Points | Behavior |
| --- | --- | --- |
| **Creator** | `CreateStandardLink`, `CreateAdvancedLink`, `Listen` | Creates a new named segment or reclaims an existing named segment whose endpoints are both dead. Initializes, re-initializes, and takes over stalled initializations; **never joins a live segment**. |
| **Opener** | `OpenStandardLink`, `OpenAdvancedLink`, `Dial` | Attaches to an existing named segment as the Secondary endpoint. **Never initializes, re-initializes, or takes over a segment**. |
| **InMemory** | `NewStandardLink`, `NewAdvancedLink` | Operates on caller-supplied memory. Initializes an `Empty` segment, re-initializes a dead `Ready` segment, takes over a stalled `Initializing` segment, or joins a live `Ready` segment as Secondary. |

Claim-loop decision matrix (`AT` = `AttachTimeout`, default `5s`):

| Observed Segment State | Creator | Opener | InMemory |
| --- | --- | --- | --- |
| `Empty`, first page not all zero | Fail with `ErrIncompatibleLayout` (write nothing) | Fail with `ErrIncompatibleLayout` (write nothing) | Fail with `ErrIncompatibleLayout` (write nothing) |
| `Empty`, first page all zero | Claim (`Empty -> Initializing`, become Primary) | Wait up to `AT` for a creator, then return transient `errSegmentNotReady` (write nothing) | Claim (`Empty -> Initializing`, become Primary) |
| `Initializing` | Wait; take over if initializer stalls for `initStale` | Wait up to `AT`, then return transient `errInitContended` (never take over) | Wait; take over if initializer stalls for `initStale` |
| `Ready`, incompatible layout | Fail with `ErrIncompatibleLayout` | Fail with `ErrIncompatibleLayout` | Fail with `ErrIncompatibleLayout` |
| `Ready`, both endpoints dead | Re-initialize (`Ready -> Initializing`, become Primary) | Return transient `errNoLiveEndpoint` immediately | Re-initialize (`Ready -> Initializing`, become Primary) |
| `Ready`, at least one live endpoint | Fail with `ErrLinkBusy` | Join as Secondary if Primary alive and `endpoints[1].token == 0` (else `ErrLinkBusy` or `GeometryError`) | Join as Secondary if Primary alive and `endpoints[1].token == 0` (else `ErrLinkBusy` or `GeometryError`) |
| Any other `state` value | Fail with `ErrIncompatibleLayout` | Fail with `ErrIncompatibleLayout` | Fail with `ErrIncompatibleLayout` |

The three transient errors (`errSegmentNotReady`, `errInitContended`, and `errNoLiveEndpoint`) all wrap `ErrFailedInit`. High-level dialing helpers (`Dial`) retry on `ErrFailedInit` (when not `ErrLinkBusy` or `ErrIncompatibleLayout`) and `ErrNotExist` with exponential backoff from `1 ms` to `100 ms` **[IMPL]**.

## 5. Initialization, Claim, Takeover, Reuse, Publish, and Attach

This section defines the exact atomic state transitions for claiming, formatting, publishing, and attaching to a segment.

### 5.1 Claim Loop **[IOP]**

Inputs:
- Caller geometry `(count, size, layoutSize)` computed per section 2.1.
- Fresh random owner `token` (`!= 0 && != 1`).
- Caller `role` (`Creator`, `Opener`, or `InMemory`).
- `AttachTimeout` (`AT`, default `5s`), `PeerTimeout` (default `5s`), and `HeartbeatInterval` (default `250ms`).
- For `Creator` calls, a fresh random `creatorToken` (`!= 0 && != 1`).
- `initStale = max(PeerTimeout, 2s)` and poll step `poll = 200 us`.

Local state tracked across iterations: `deadline = now + AT`, `watch = {active, tok, hb, since}` measured on the **local monotonic clock**, and `emptySince` on the local monotonic clock:

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
          if load(sb.state) != Empty: continue  # claimed concurrently while scanning
          return ErrIncompatibleLayout          # foreign data; write NOTHING
      if role == Opener:
          if emptySince unset: emptySince = now
          if now - emptySince >= AT: return errSegmentNotReady   # segment untouched
          emptyWait = true                      # fall through to sleep
      else:
          if cas(sb.state, Empty -> Initializing):
              if not beginInit(expected = load(sb.initToken)): return errInitContended
              return (Primary, mustInit = true)

  case Initializing:
      tok = load(sb.initToken); hb = load(sb.initHeartbeat)
      if checkLayoutCompat(initializing = true) fails: return ErrIncompatibleLayout
      if role == Opener: pass                   # waits up to deadline
      else:
          if not watch.active or watch.tok != tok or watch.hb != hb:
              watch = {active, tok, hb, since = now}          # progress observed: reset window
          else if now - watch.since >= initStale:
              if beginInit(expected = tok): return (Primary, mustInit = true)   # TAKEOVER
              watch.active = false                              # another claimer won the CAS
          stalling = true

  case Ready:
      if checkLayoutCompat(initializing = false) fails: return ErrIncompatibleLayout
      primaryAlive   = endpointAliveAtOpen(endpoints[0])        # section 6.2
      secondaryAlive = endpointAliveAtOpen(endpoints[1])
      if not primaryAlive and not secondaryAlive:
          if role == Opener: return errNoLiveEndpoint           # state untouched
          if cas(sb.state, Ready -> Initializing):              # REUSE
              if not beginInit(expected = load(sb.initToken)): return errInitContended
              return (Primary, mustInit = true)
          continue
      if role == Creator: return ErrLinkBusy                    # Creator never joins a live segment
      got  = (load bufferCount, bufferSize, layoutSize, pageSize)
      want = (count, size, layoutSize, localPageSize)
      if got != want: return GeometryError(want, got)           # matches ErrInvalidSize
      if primaryAlive and load(endpoints[1].token) == 0:
          if cas(endpoints[1].token, 0 -> token):               # JOIN as Secondary
              fillEndpoint(endpoints[1])                        # pid, interval, heartbeat (section 6.1)
              return (Secondary, mustInit = false)
          continue
      return ErrLinkBusy      # both alive, or the live endpoint's earlier peer already left

  default: return ErrIncompatibleLayout

  limit = deadline
  if stalling and watch.active and watch.since < deadline:
      limit = max(deadline, watch.since + initStale)
  if not emptyWait and now > limit: return errInitContended
  sleep(poll)
```

Helper `firstPageZero(sb)` atomically loads every 8-byte word across the entire first page (`pageSize / 8` words, not just the first 384 bytes). Any non-zero word on page 0 while `state == Empty` causes immediate rejection with `ErrIncompatibleLayout`.

Helper `checkLayoutCompat(sb, initializing)`:

```text
if load(requiredFeatures) & ~knownRequiredFeatures(=0) != 0: return ErrIncompatibleLayout
magic = load(sb.magic); major = load(sb.major)
if not initializing:
    if magic != MAGIC or major != 3: return ErrIncompatibleLayout        # rejects "HQQ_STL2"
    return ok
# When initializing == true, the previous initializer may have crashed before writing magic/major:
if magic != 0 and magic != MAGIC: return ErrIncompatibleLayout
if major != 3 and (major != 0 or load(initToken) != 0): return ErrIncompatibleLayout
return ok
```

Mandatory **[IOP]** invariants of the claim loop:

1. **Zero mutation of foreign data**: Never write to an `Empty` segment with a non-zero word on page 0 or to any segment failing `checkLayoutCompat`.
2. **Read-only Openers before join**: An `Opener` never modifies `state`, `initToken`, or `creatorToken`, and never takes over an initialization.
3. **Monotonic-clock takeover watch**: The `Initializing` watch measures elapsed time without progress on the caller's local monotonic clock, never by subtracting wall-clock `initHeartbeat` from `now`. If a stalled-initializer watch begins before `deadline`, the loop extends `limit` to `max(deadline, watch.since + initStale)` so takeover can complete.
4. **Exact CAS transitions**: Takeover and reuse use the exact `state` and `initToken` CAS sequence above.

### 5.2 Claiming `initToken` (`beginInit`) **[IOP]**

```text
beginInit(token, expected):
    store(major = 3); store(minor = 0); store(requiredFeatures = 0)     # stampLayout
    if not cas(initToken, expected -> token): return false               # another endpoint won
    store(creatorToken = creatorTokenOfThisCall)    # 0 for InMemory; fresh random for Creator
    store(initHeartbeat = nowUnixNanos())
    return true
```

Once `beginInit` returns `true`, the caller is the active **initializer**, and its ownership predicate is `load(initToken) == token`.

### 5.3 Initialization Sequence and Superblock Publication **[IOP]**

The winner of `beginInit` `MUST` execute the following steps in order, **verifying before every slice and every publish step that its context is still active and `load(initToken) == token`** (returning `errInitContended` and halting all writes immediately if ownership was lost):

1. **Format both rings**: Format `ring0` in slices of at most `1024` slots (section 7.2), checking ownership before each slice and updating `initHeartbeat` after completion; repeat for `ring1`.
2. **Zero both payload pools**: Zero `buf0` in slices (at most `8 MiB` per slice; the Go and C implementations use `256 KiB` slices), checking `load(initToken) == token` before each slice and updating `initHeartbeat` after each slice; repeat for `buf1`.
3. **Publish superblock (`publishSuperblock`)**:
   - **P0**: Check `load(initToken) == token`, else return `errInitContended`.
   - **P1**: Check ownership, then store `major = 3`, `minor = 0`, `requiredFeatures = 0`.
   - **P2**: Check ownership, then store `pageSize`, `bufferCount`, `bufferSize`, `layoutSize`.
   - **P3**: Check ownership, then zero the Secondary record (`endpoints[1]`: `token = 0`, `pid = 0`, `heartbeat = 0`, `interval = 0`).
   - **P4**: Check ownership, then populate the Primary record (`endpoints[0]`: `token = myToken`, `pid = getpid()`, `interval = HeartbeatInterval`, `heartbeat = nowUnixNanos()`).
   - **P5**: Check ownership, then store `magic = 0x4851515F53544C33`.
   - **Commit**: Execute `cas(initToken, myToken -> 0)` (`acq_rel`). **If and only if the CAS succeeds**: increment `epoch` via `fetch_add(epoch, 1)` (`acq_rel`) and store `state = Ready` (`2`) with release (`REL` or `SC`) ordering. If the CAS fails, return `errInitContended` without touching `epoch` or `state`.
4. **Attach to both rings** (section 5.4).

The `state = Ready` release store is the publication point for the entire segment initialization: every preceding write to the rings, payload pools, and superblock fields happens-before any endpoint whose acquire load observes `state == Ready` ([`MEMORY_MODEL.md`](MEMORY_MODEL.md) section 3).

### 5.4 Attaching the Rings **[IOP]**

After claiming its role (and publishing if Primary), the endpoint calls `Attach` (section 7.3) on `ring0` and `ring1`, polling for up to `AttachTimeout` if a ring header is not yet marked ready. If either ring has an incompatible magic, version, stride, or size (`!= bufferCount`), the endpoint detaches (`cas(record.token, myToken, 1)`) and fails with `ErrIncompatibleLayout`.

If a `Creator` fails after creating a new named shared-memory object, it unlinks the name if it still owns the instance (section 11.4).

### 5.5 Secondary Join, Geometry Validation, and Third-Endpoint Refusal **[IOP]**

- **Geometry comparison**: A joining `Opener` or `InMemory` endpoint compares the superblock's `(bufferCount, bufferSize, layoutSize, pageSize)` against its own configuration; any mismatch returns `GeometryError` (wrapping `ErrInvalidSize`).
- **Secondary join**: Joins as Secondary if and only if the Primary is alive **and** `endpoints[1].token == 0`, executing `cas(endpoints[1].token, 0 -> myToken)` followed by `fillEndpoint(endpoints[1])`.
- **Single Secondary per epoch**: In every other case on a live segment (both endpoints alive, or `endpoints[1].token != 0` because a previous Secondary already closed or died), the joiner fails with `ErrLinkBusy`.
- **Epoch capture**: Immediately after winning the join CAS, the Secondary loads `myEpoch = load(sb.epoch)` and starts monitoring the Primary (section 6.3).

### 5.6 Setup Failure Rules **[IOP]**

- If an endpoint fails during local setup after writing its `token` into `endpoints[i].token`, it `MUST` execute `cas(endpoints[i].token, myToken, 1)` before returning.
- If an initializer fails before committing `publishSuperblock`, the segment stays in `Initializing` and the next `Creator` or `InMemory` caller takes over once `initStale` elapses.

## 6. Liveness, Heartbeats, Peer State, and Error Mapping

Endpoints track peer liveness through periodic wall-clock heartbeats paired with local monotonic progress watches.

### 6.1 Heartbeat Updates **[IOP for words, IMPL for scheduling]**

An endpoint configured with `HeartbeatInterval > 0` (default `250 ms`) stores `own.heartbeat = nowUnixNanos()` every interval **only while `load(own.token) == myToken`**. If heartbeats are disabled (`HeartbeatInterval < 0`), the endpoint writes `interval = 0` and a single initial `heartbeat = nowUnixNanos()` at attach time; peers and openers then treat it as permanently alive.

### 6.2 Open-Time Liveness Check (`endpointAliveAtOpen`) **[IOP]**

When the claim loop inspects a `Ready` segment, it evaluates each endpoint record as follows:

```text
tok = load(token)
if tok == 0 or tok == 1:            return dead                    # unused or gracefully detached
interval = load(interval)
if interval == 0:                   return alive                   # heartbeats disabled
hb = load(heartbeat)
if hb == 0:                         return alive                   # just attached
timeout = max(callerPeerTimeout, 4 * interval)                      # callerPeerTimeout default 5 s
if -1 s < wallAge(hb) < timeout:   return alive                   # fresh within 1 s future-skew tolerance (FINDINGS F-04)
window = min(3 * interval, 2 s);  step = max(interval / 4, 1 ms)
for until now + window:                                             # monotonic confirmation watch
    sleep(step)
    if load(heartbeat) != hb or load(token) != tok:
        return isOwnerToken(load(token))
return dead
```

Because the confirmation watch measures elapsed time on the caller's local monotonic clock and checks only whether `heartbeat` or `token` changes, a backward or forward wall-clock jump cannot cause a live endpoint with active heartbeats to be misclassified as dead.

### 6.3 Continuous Peer Monitor (`checkPeer`) **[IOP for derivation, IMPL for scheduling]**

Each attached endpoint tracks its peer using its **local monotonic clock** and four state variables: `status` (`NotConnected = 0`, `Connected = 1`, `Closed = 2`, `Gone = 3`), `peer.token` (`0` until first seen), `lastHB`, and `lastChange`:

```text
checkPeer():
    if status is Closed or Gone: return its error
    if load(sb.epoch) != myEpoch or load(endpoints[self].token) != myToken:
        status = Gone; return ErrPeerGone         # segment re-initialized or local slot replaced
    rec = endpoints[1 - self]
    tok = load(rec.token); now = monotonic()
    if tok == 1:                       status = Closed; return ErrPeerClosed
    if peer.token == 0:                                # peer has not attached yet
        if tok != 0:
            peer.token = tok; lastHB = load(rec.heartbeat); lastChange = now; status = Connected
        return nil
    if tok != peer.token:              status = Gone; return ErrPeerGone    # peer replaced
    interval = load(rec.interval)
    if interval == 0:                  return nil                           # heartbeats disabled
    hb = load(rec.heartbeat)
    if hb != lastHB:                   lastHB = hb; lastChange = now; return nil
    if now - lastChange > max(myPeerTimeout, 4 * interval):
        status = Gone; return ErrPeerGone
    return nil
```

### 6.4 Effect on I/O and Post-Close Drain Rule **[IOP]**

| Peer / Link Condition | Returned Error |
| --- | --- |
| Peer detached gracefully (`token == 1`) | `ErrPeerClosed` (matches `io.EOF`; `StandardLink.Read` and `ReadFrom` return `io.EOF` directly). |
| Peer heartbeat stalled past `max(PeerTimeout, 4 * interval)`, peer token changed, or `epoch` changed | `ErrPeerGone` (matches `ErrConnNotFound`). |
| Local endpoint closed | `ErrClosed` (matches `net.ErrClosed`). |
| Operation deadline expired | `ErrTimeout` (matches `os.ErrDeadlineExceeded`). |

**[IOP] Drain before reporting peer closure or death**: Whenever a receive call observes `ErrPeerClosed` or `ErrPeerGone`, it `MUST` perform one final non-blocking dequeue attempt on the receive ring and deliver any published packet before surfacing the error. Because a closing peer publishes all packets before executing `cas(token, myToken -> 1)` with release ordering, this drain rule guarantees zero lost messages on graceful close.

**[IOP] Failing sends after peer loss**: Before claiming a TX slot, a send operation checks the recorded peer status (and calls `checkPeer()` first if local heartbeats are disabled; `FINDINGS.md` F-10), failing immediately with `ErrPeerClosed` or `ErrPeerGone` if the peer has already left. While blocked on a full ring, the sender re-evaluates `checkPeer()` at least every `50 ms`.

## 7. The MPMC Ring Protocol (Layout Version 2)

Each directional ring is a bounded Vyukov MPMC queue stored in shared memory using ring layout version `2`.

### 7.1 Ring Memory Image **[IOP]**

```text
Offset    Size  Field          Description
   0         8  magic          0xC9D8C1D43F096702                       } Line 0 (immutable after Format)
   8         8  size           Number of slots N (power of two, >= 2)
  16         8  flag           Bit 0 reserved (0), bit 1 initialized (2)
  24         8  stride         Slot stride in bytes (128)
  32         8  version        Ring layout version (2)
  40..127       padding        Zero
 128         8  r              Consumer cursor (position)               } Line 1
 136..255       padding        Zero
 256         8  w              Producer cursor (position)               } Line 2
 264..383       padding        Zero
 384         4  notEmpty       Wake word incremented by producers       } Line 3
 388         4  emptyWaiters   Count of parked consumer processes
 392         4  notFull        Wake word incremented by consumers
 396         4  fullWaiters    Count of parked producer processes
 400..511       padding        Zero
 512 ...        slots[0..N-1]  N slots of 128 bytes each
```

Each 128-byte slot at byte offset `ring + 512 + 128 * i` has the layout:

```text
Slot Offset  Size  Field
   0            8  seq       u64 atomic slot sequence number
   8           64  packet    64-byte HQQ packet (plain memory, section 9.1)
  72..127      56  padding   Zero padding to 128-byte stride
```

### 7.2 Formatting a Ring (`Format`) **[IOP]**

`Format(ring, N)` runs exclusively under `initToken` ownership and `MUST` write fields in the following order so a concurrent `Attach` never observes a half-formatted ring:

```text
store(flag = 0); store(magic = 0)                           # invalidate header first
for i in 0..N-1:  packet[i] = zeros;  store(seq[i] = i)
store(r = 0); store(w = 0)
store(notEmpty = 0); store(emptyWaiters = 0); store(notFull = 0); store(fullWaiters = 0)
store(size = N); store(stride = 128); store(version = 2)
store(magic = 0xC9D8C1D43F096702)
store(flag = 2)                                            # release store LAST
```

### 7.3 Attaching to a Ring (`Attach`) **[IOP]**

```text
Attach(ring, timeout):
  loop:
     if load_acq(magic) == 0xC9D8C1D43F096702 and (load_acq(flag) & 2) != 0:
         if load(stride) != 128 or load(version) != 2 or not isPowerOfTwoGe2(load(size)):
             return INCOMPATIBLE
         return handle(N = load(size), mask = N - 1)
     if timeout != 0 and elapsed >= timeout: return NOT_READY
     sleep(100 us)
```

### 7.4 Slot Claim, Publish, and Release **[IOP]**

All positions `pos`, cursors `r` and `w`, and slot sequence numbers `seq` are unsigned 64-bit integers wrapping modulo `2^64`, with slot index `s = pos & mask`:

| Operation | Precondition on `seq[pos & mask]` | Cursor Action | Sequence Store (`seq_cst`) |
| --- | --- | --- | --- |
| **Producer claim** | `int64(load_acq(seq) - pos) == 0` | `cas(w, pos -> pos + 1)` | None |
| **Producer publish** | Caller owns `pos` | Write payload buffer `s` and 64-byte packet | `store_sc(seq, pos + 1)` |
| **Consumer claim** | `int64(load_acq(seq) - (pos + 1)) == 0` | `cas(r, pos -> pos + 1)` | None |
| **Consumer release** | Caller owns `pos` | Read 64-byte packet and payload buffer `s` | `store_sc(seq, pos + mask + 1)` |

Producer and consumer claim loops:

```text
claimProducer():                              claimConsumer():
  pos = load(w)                                 pos = load(r)
  loop:                                         loop:
    d = int64(load_acq(seq[pos&mask]) - pos)      d = int64(load_acq(seq[pos&mask]) - (pos+1))
    d == 0: if cas(w, pos, pos+1): return pos     d == 0: if cas(r, pos, pos+1): return pos
            pos = load(w)                                 pos = load(r)
    d  > 0: pos = load(w)                         d  > 0: pos = load(r)
    d  < 0: ring is FULL: wait (section 8)        d  < 0: ring is EMPTY: wait (section 8)
```

### 7.5 Full and Empty Ring Conditions **[IOP]**

When `d < 0` in `claimProducer`, slot `pos & mask` has not yet been released from lap `(pos - N) / N` (the ring is **full**). When `d < 0` in `claimConsumer`, position `pos` has not yet been published by the producer (the ring is **empty**). Non-blocking calls (`TryWrite`, `TryRead`) return immediately without modifying any shared word; blocking calls enter the wait/wake protocol in section 8.

### 7.6 Local SPSC Serialization Mode **[IMPL with IOP constraints]**

Because at most one OS process produces and at most one OS process consumes on each ring, an implementation that serializes its own local threads with a process-local mutex (or is single-threaded per direction) `MAY` update `w` and `r` using plain atomic stores (`store_rlx(w, pos + 1)`) instead of `cas`, provided it:

1. Follows the exact `seq[s]` ordering rules of section 7.4 and [`MEMORY_MODEL.md`](MEMORY_MODEL.md).
2. Initializes its local view of `r` and `w` by reading the shared-memory cursors at `Attach` time.
3. Stores `w = pos + 1` (or `r = pos + 1`) **before** storing `seq = pos + 1` (or `seq = pos + mask + 1`) so the shared-memory cursors remain canonical at all times.

## 8. Wait / Wake Protocol (Dekker Handshake)

Kernel parking is optional for correctness (a spin-plus-sleep implementation is valid), but any implementation that parks or wakes peers `MUST` follow the Dekker handshake below so wakeups are never lost.

### 8.1 Wait Words and Process Counters **[IOP]**

| Direction | Awaited By | Target Condition | Wake Word | Waiter Counter | Signaled By |
| --- | --- | --- | --- | --- | --- |
| `NotEmpty` (`0`) | Consumer | `seq[r & mask] == r + 1` | `notEmpty` (`u32 @384`) | `emptyWaiters` (`u32 @388`) | Producer after publish |
| `NotFull` (`1`) | Producer | `seq[w & mask] == w` | `notFull` (`u32 @392`) | `fullWaiters` (`u32 @396`) | Consumer after release |

**[IOP] Waiter counters count parked processes**: Each process registers at most once per direction (or once per parked thread if it does not aggregate locally; over-counting only causes extra wake syscalls, whereas under-counting is forbidden).

### 8.2 Waiter Sequence **[IOP]**

When `claimConsumer` (with `target = pos + 1`) or `claimProducer` (with `target = pos`) observes `int64(seq - target) < 0` and decides to park on `(word, waiters)`:

```text
W1  fetch_add_sc(waiters, 1)                    # register waiter (MUST be seq_cst)
W2  v = load_acq(word)                          # snapshot futex word (acquire or seq_cst)
W3  if int64(load_sc(seq[pos&mask]) - target) < 0 and not cancelled:   # re-check (MUST be seq_cst)
        park(word, v, timeout)                  # futex_wait / ulock_wait / semaphore wait (<= 100 ms)
W4  fetch_sub_rel(waiters, 1)                   # deregister waiter
    re-run the claim loop
```

### 8.3 Waker Sequence **[IOP]**

Immediately after publishing (`store_sc(seq, pos + 1)`) or releasing (`store_sc(seq, pos + mask + 1)`):

```text
K1  store_sc(seq, ...)                          # publish or release (MUST be seq_cst)
K2  n = load_sc(waiters of opposite side)       # read waiter count (MUST be seq_cst)
K3  if n != 0:
        fetch_add_rel(word, 1)
        wake_all(word)                          # on Windows semaphores: ReleaseSemaphore(sem, 1, NULL)
```

**[IOP] Sequential consistency requirement**: Operations `W1`, `W3`, `K1`, and `K2` form a Dekker store-buffering pair and **`MUST` be sequentially consistent** (or use `seq_cst` fences between `W1`/`W3` and `K1`/`K2`). See [`MEMORY_MODEL.md`](MEMORY_MODEL.md) section 4.2 for the formal proof.

**[IOP] Bounded park timeout**: Every kernel `park` call `MUST` be bounded by at most `100 ms` (and shortened to the caller's deadline and the `50 ms` peer-liveness check interval).

### 8.4 Adaptive Backoff Schedule **[IMPL]**

Before parking in the kernel, the reference implementations escalate through three phases:

1. **Spin**: ~4-6 us of pause/spin reads (`256` 4-read iterations in Go, `1024` single-read iterations in C).
2. **Yield**: `256` iterations of `runtime.Gosched()` / `sched_yield()`, checking deadlines and peer liveness every 16 iterations.
3. **Kernel park**: `park(word, v, min(100ms, timeToDeadline, 50ms))`.

### 8.5 Platform Wait/Wake Primitives **[IOP]**

- **Linux**: `futex(addr, FUTEX_WAIT = 0, v, timespec)` and `futex(addr, FUTEX_WAKE = 1, INT_MAX)` **without** `FUTEX_PRIVATE_FLAG` (shared across processes via physical page address).
- **macOS**: Shared-address ulock wait/wake via `__ulock_wait(UL_COMPARE_AND_WAIT_SHARED = 3, addr, v, timeout_us)` and `__ulock_wake(UL_COMPARE_AND_WAIT_SHARED | ULF_WAKE_ALL, addr, 0)` (or `os_sync_wait_on_address` / `os_sync_wake_by_address_all` with `OS_SYNC_WAIT_ON_ADDRESS_SHARED` on macOS 14.4+).
- **Windows (named segments)**: Uses the four per-segment named kernel semaphores (`.hqq-sem0..3`, section 11.5). Waiters execute `W1`, `W3`, and `WaitForSingleObject(sem, timeout_ms)`; wakers execute `K1`, `K2`, and (when `n != 0`) `ReleaseSemaphore(sem, 1, NULL)`.

### 8.6 Process-Local Waiter Aggregation **[IMPL]**

An implementation `MAY` aggregate concurrent waiting threads/goroutines within the same process so that only one leader thread executes `W1..W4` per direction while follower threads wait on a process-private condition variable or channel.

## 9. Standard Protocol (`OpCopy`, `OpTombstone`, `OpError`)

The Standard protocol provides low-level message framing over the two directional rings.

### 9.1 64-Byte Packet ABI **[IOP]**

Every ring slot carries a **64-byte packet** composed of eight little-endian `u64` words:

| Word | Byte Offset | Content |
| ---: | ---: | --- |
| `0` | `0` | Opcode in bits `0..7`; bits `8..63` are reserved (`0`). |
| `1..7` | `8, 16, ..., 56` | Operands `0..6`. |

**[IOP]** Senders `MUST` write `0` to bits `8..63` of word 0 and to all unused operand words (for a newly formatted Standard-only ring where words `4..7` are already `0`, a Standard sender may omit re-zeroing words `4..7`). Receivers `MUST` extract the opcode as `word0 & 0xFF` and `MUST` ignore bits `8..63` of word 0 and any unused operand words.

### 9.2 Opcodes and Operands **[IOP]**

| Opcode | Hex | Layer | Operands (`0..6`) |
| --- | ---: | --- | --- |
| `OpError` | `0x00` | Both | `0`: `ContextID`; `1`: `ErrorCode` (section 12) |
| `OpConnCreate` | `0x01` | Advanced | `0`: `ConnectionID`; `1`: reserved (`0`) |
| `OpConnAccept` | `0x02` | Advanced | `0`: `ConnectionID` |
| `OpConnClose` | `0x03` | Advanced | `0`: `ConnectionID`; `1`: reserved (`0`) |
| `OpConnCopy` | `0x04` | Advanced | `0`: `ConnectionID` (`0` = default stream); `1`: `MessageID`; `2`: `TotalSize`; `3`: `ChunkOffset`; `4`: `ChunkSize`; `5`: `Flags`; `6`: `Aux` (`MethodID` or status) |
| `OpProtoNegotiate` | `0x05` | Advanced | `0`: encoded version `(major << 8) \| minor`; `1`: requested features; `2`: reserved (`0`) |
| `OpProtoAck` | `0x06` | Advanced | `0`: encoded version; `1`: negotiated features; `2`: reserved (`0`) |
| `OpCancel` | `0x07` | Advanced (1.1) | `0`: `ConnectionID`; `1`: `MessageID` (`PROTOCOL_1_1_DRAFT.md` section 2) |
| `OpStandardLinkTombstone` | `0xFE` | Standard | `0`: `CopyID`; `1`: slot index (diagnostic); `2`: `ErrorCode` |
| `OpStandardLinkCopy` | `0xFF` | Standard | `0`: `CopyID`; `1`: slot index (diagnostic); `2`: `DataSize` |

### 9.3 Standard Send Path **[IOP]**

1. Claim producer position `pos` (`slot s = pos & mask`) on the outbound ring.
2. Copy `DataSize` bytes (`1 <= DataSize <= bufferSize`) into payload buffer `s` using plain memory writes.
3. Write the 64-byte packet at slot `s`: `OpStandardLinkCopy` with `Operand0 = CopyID`, `Operand1 = s`, and `Operand2 = DataSize`. If a reservation or zero-copy callback is aborted after claiming `pos`, the sender **`MUST` still publish the slot** using `OpStandardLinkTombstone` (`0xFE`) so the ring does not stall at `pos`.
4. Publish `store_sc(seq[s], pos + 1)` and run the waker check (section 8.3).

Empty writes (`DataSize == 0`) are no-ops and do not claim or publish a slot. Payloads exceeding `bufferSize` return `ErrBufferOverflow` locally without claiming a slot.

### 9.4 Standard Receive Path and Buffer Ownership **[IOP]**

Ring slot `i` exclusively owns payload buffer `i`:

1. Claim consumer position `pos` and compute `slot = pos & mask` (**never** read the buffer index from `Operand1`).
2. Dispatch on `opcode = word0 & 0xFF`:
   - `OpStandardLinkTombstone` (`0xFE`): Release slot `pos` and loop to claim the next slot.
   - `OpStandardLinkCopy` (`0xFF`): Let `size = Operand2`. If `size == 0 || size > bufferSize`, release the slot and return `ErrInvalidSize`; otherwise copy or expose `size` bytes from payload buffer `slot`, then release the slot.
   - `OpError` (`0x00`): Release the slot and return the error mapped from `Operand1` (section 12).
   - Any other opcode: Release the slot and return `ErrUnexpectedPacket`.
3. Always release the slot (`store_sc(seq[slot], pos + mask + 1)`) and run the `NotFull` waker check (section 8.3), even if the reader's buffer or callback fails.

## 10. Advanced Protocol

The Advanced protocol layers feature negotiation, arbitrary-size chunked messages, request/response RPC, and multiplexed streams over the same segment and ring layout. Protocol 1.1 extensions (`OpCancel`, `AdvFlagMeta`, and `ErrCodeShuttingDown`) are specified in [`ext/PROTOCOL_1_1_DRAFT.md`](ext/PROTOCOL_1_1_DRAFT.md).

### 10.1 Handshake Negotiation **[IOP]**

Baseline features: `FeatureMessages = 1 << 0` (`0x01`) and `FeatureRequestResponse = 1 << 1` (`0x02`), plus Protocol 1.1 `FeatureCancel = 1 << 2` (`0x04`) and `FeatureMetadata = 1 << 3` (`0x08`).

- **Role-driven handshake**: The **Primary** sends `OpProtoNegotiate(version, requestedFeatures, 0)` and reads frames (skipping tombstones) until `OpProtoAck` arrives. The **Secondary** reads frames (skipping tombstones) until `OpProtoNegotiate` arrives, then replies with `OpProtoAck`.
- **Responder rules**: If `major != 1`, send `OpError(0, ErrCodeInvalidOp)` and transition to terminal state `Failed` (`ErrNegotiationRejected`). Otherwise compute `minor = min(requestedMinor, ownMinor)` and `features = requestedFeatures & supportedFeatures & allowedFeatures` (clearing bits `2..3` if `minor == 0`), send `OpProtoAck((1 << 8) | minor, features, 0)`, and enter `Negotiated`.
- **Simultaneous initiation**: If the Secondary also sent `OpProtoNegotiate`, the Primary ignores the incoming `OpProtoNegotiate` and waits for `OpProtoAck`, while the Secondary answers the Primary's `OpProtoNegotiate` using `request.features & supported & ownRequestedFeatures`.
- **Terminal failure**: If negotiation fails for any reason, the link enters terminal state `Failed` and all subsequent `Negotiate` calls return `ErrNegotiationFailed`.

### 10.2 Stream and Message Identifiers **[IOP]**

| Identifier | Scope | Allocation Rule |
| --- | --- | --- |
| `ConnectionID` (stream ID) | Per link | `0` is the permanent link-level stream. Positive stream IDs are allocated by parity: **Primary allocates even IDs (`2, 4, 6, ...`)**, and **Secondary allocates odd IDs (`1, 3, 5, ...`)**. Stream IDs are never reused on a link. |
| `MessageID` | Per sender | Starts at `1` and increments by `1` for each outbound data message or request across all streams (skipping `0` on wrap). Responses reuse the request's `MessageID`. |
| `MethodID` | Per request | Carried in `aux` (operand 6) of the first chunk (`ChunkOffset == 0`) of a request. |

### 10.3 Stream Multiplexing (`OpConnCreate`, `OpConnAccept`, `OpConnClose`) **[IOP]**

- **Opening a stream**: The initiator allocates a `ConnectionID` of its own parity, registers the pending stream, and sends `OpConnCreate(connectionID, 0)`. The responder replies with `OpConnAccept(connectionID)` on success, or `OpError(connectionID, code)` on refusal:
  - `ErrCodeConnNotFound` (`0x05`): Responder is not listening for streams (`ListenStreams` not called) and the pre-listen grace period (`StreamListenGrace`, default `2s`, holding up to `IncomingQueueSize` requests) has expired or is disabled.
  - `ErrCodeInvalidOp` (`0x06`): `connectionID == 0`, wrong parity for the sender's role, or duplicate `connectionID`.
  - `ErrCodeBufferOverflow` (`0x07`): Accept queue is full under `OverflowDrop`.
- **Closing a stream**: `Stream.Close` marks the stream closed locally, discards unread queued messages, and sends `OpConnClose(connectionID, 0)` unless the peer already closed it. Stream `0` cannot be closed (`ErrInvalidOp`).
- **Non-blocking control replies**: The receive loop `MUST NEVER` block waiting for space on the TX ring when sending `OpConnAccept` or error replies; all control replies are queued on a bounded asynchronous control queue so bidirectional floods cannot deadlock.

### 10.4 Chunking and Frame Flags (`OpConnCopy`) **[IOP]**

`OpConnCopy` operand 5 carries a bitmask of frame flags:
- `Begin = 1 << 0` (`0x01`)
- `End = 1 << 1` (`0x02`)
- `Abort = 1 << 2` (`0x04`)
- `Request = 1 << 3` (`0x08`)
- `Response = 1 << 4` (`0x10`)
- `Error = 1 << 5` (`0x20`)
- `Meta = 1 << 6` (`0x40`, Protocol 1.1)

Messages of length `0 <= len <= MaxMessageSize` (default `64 MiB`) are segmented into chunks of at most `bufferSize` bytes:

```text
if len == 0:
    send one frame: TotalSize = 0, ChunkOffset = 0, ChunkSize = 0, flags = Begin|End|base, aux
else:
    for offset = 0; offset < len; offset += bufferSize:
        end = min(offset + bufferSize, len)
        flags = base | (offset == 0 ? Begin : 0) | (end == len ? End : 0)
        chunkAux = (offset == 0) ? aux : 0
        send OpConnCopy(conn, msgID, len, offset, end - offset, flags, chunkAux)
```

If sending fails after at least one chunk (`offset > 0`) was published, the sender makes a best-effort attempt (100 ms timeout) to publish an abort frame: `OpConnCopy(conn, msgID, 0, 0, 0, Abort | base, 0)`.

### 10.5 Receiver Frame Validation and Reassembly **[IOP]**

The receiver evaluates each `OpConnCopy` frame in strict step order:

```text
1. Flag validity:
   if (flags & ~knownMask) != 0               -> FAIL ErrProtocol (knownMask is 0x3F in 1.0, 0x7F with FeatureMetadata in 1.1)
   if Request and Response both set           -> FAIL ErrProtocol
   if Error set and Response not set          -> FAIL ErrProtocol
   if Meta set and (not Request or not FeatureMetadata) -> FAIL ErrProtocol
   kind = flags & (Request | Response); key = (conn, msgID, kind)

2. Abort handling (evaluated BEFORE size/offset bounds):
   if Abort set:
       drop assembly[key] if present
       if key in refusedSet: remove from refusedSet, done
       else if kind == Response: fail waiting Call with ErrMessageAborted
       done

3. Bounds validation (any failure drops assembly[key] and FAILS the receive path):
   if ChunkSize > bufferSize or ChunkOffset > TotalSize or ChunkSize > TotalSize - ChunkOffset -> ErrInvalidSize
   if TotalSize > MaxMessageSize or TotalSize > INT_MAX                                        -> ErrBufferOverflow
   if TotalSize == 0 and (ChunkOffset != 0 or ChunkSize != 0 or (flags & (Begin|End)) != (Begin|End)) -> ErrInvalidSize
   if TotalSize > 0 and ChunkSize == 0                                                         -> ErrInvalidSize
   if ChunkOffset != 0 and aux != 0                                                            -> ErrInvalidSize

4. Begin chunk:
   if Begin set:
       if ChunkOffset != 0 or assembly[key] exists or key in refusedSet                        -> ErrInvalidSize
       if End set (single-chunk message):
           if ChunkSize != TotalSize                                                           -> ErrInvalidSize
           deliver complete message (conn, msgID, flags, aux, payload[0:ChunkSize])
       else (multi-chunk message):
           charge = TotalSize + 256
           if charge > MaxReassemblyBytes or assemblyBytes > MaxReassemblyBytes - charge:
               record key in refusedSet (bounded to 1024 keys)
               if kind == Request: queue error response (aux = ErrCodeBufferOverflow = 0x07)
               else if kind == Response: fail waiting Call with ErrReassemblyLimit
               done
           create assembly[key] = {flags, aux, size = TotalSize, next = ChunkSize}
           assemblyBytes += charge

5. Continuation / End chunk (Begin not set):
   if assembly[key] does not exist:
       if key in refusedSet:
           if End set: remove key from refusedSet
           done (drain refused message)
       if refusedEvicted: done (drain orphan chunk after refusedSet eviction, FINDINGS F-07)
       FAIL ErrProtocol
   if ChunkOffset != assembly.next or TotalSize != assembly.size                               -> ErrInvalidSize
   append payload[0:ChunkSize]; assembly.next += ChunkSize
   if End set:
       if assembly.next != TotalSize                                                           -> ErrInvalidSize
       assemblyBytes -= charge; remove assembly[key]; deliver complete message
```

### 10.6 Request / Response Correlation **[IOP]**

- `Call(stream, method, request)` allocates `msgID = nextMessageID()`, registers a pending waiter under `(streamID, msgID)`, and sends the request (`Request` flag, `aux = method`).
- Responses (`Response` flag) are matched by `(ConnectionID, MessageID)`. If `Error` is set on the response, `Call` returns `ResponseError{Status: aux, Payload: body}`.

### 10.7 Message Routing and Queue Overflow **[IOP for destinations, IMPL for queueing]**

Completed messages are routed in arrival order:
- **Responses**: Delivered directly to the pending `Call` registered for `(ConnectionID, MessageID)`.
- **Requests**: Routed to the link's incoming request queue (consumed by `Serve`).
- **Plain data messages**: Routed to the receive queue of stream `ConnectionID` (dropped if the stream is unknown or closed).

### 10.8 Wire Error Propagation **[IOP]**

When a receiver encounters a protocol violation (`ErrProtocol`, `ErrInvalidSize`, `ErrBufferOverflow`, `ErrUnexpectedPacket`), it terminates its local receive loop and fails pending operations.

## 11. Platform Naming, Object Creation, and Safe Unlinking

Shared-memory backing objects are mapped and unlinked using platform-specific primitives that prevent symlink traversal, `SIGBUS` faults, and accidental unlinking of a replacement segment.

### 11.1 Segment Name Validation **[IOP]**

After stripping at most one leading `/`, the base name `base` `MUST` be non-empty, `MUST NOT` be `"."` or `".."`, `MUST NOT` contain `/`, `\`, or NUL (`\x00`), and `MUST NOT` exceed `255` bytes (otherwise return `ErrInvalidName`):

| Platform | Kernel Backing Object | Platform Length Limit |
| --- | --- | --- |
| Linux | Regular file `/dev/shm/<base>` | `<= 255` bytes |
| macOS | `shm_open("/<base>")` | **Total path including leading `/` `<= 31` bytes (`PSHMNAMLEN`)**, so `base` is at most **`30` bytes** |
| FreeBSD | `shm_open("/<base>")` | Total path `<= 1023` bytes |
| Windows | File mapping `Local\hqq_v3_<base>` (or `Global\` / `Local\`) | Kernel name plus `.hqq-sem0` (9 chars) `<= 259` UTF-16 code units |

### 11.2 Linux `/dev/shm` Object Handling **[IOP]**

- Open with `O_RDWR | O_CLOEXEC | O_NOFOLLOW` (plus `O_CREAT | O_EXCL` on creation) and verify via `fstat` that the descriptor refers to a **regular file** (else return `ErrInvalidName` without modifying it).
- Allocate backing pages with `fallocate(fd, 0, 0, size)` (falling back to `ftruncate` only on `EOPNOTSUPP` / `ENOSYS`) so tmpfs exhaustion fails at creation time rather than raising `SIGBUS` during ring writes.
- Map with `mmap(NULL, size, PROT_READ | PROT_WRITE, MAP_SHARED, fd, 0)`.

### 11.3 Creation and Existing-Object Rules **[IOP]**

- **Creators (`Create` / `Listen`)**: Attempt exclusive creation (`O_CREAT | O_EXCL`). If the name already exists, open the existing object (`O_RDWR`) without truncating:
  - Size `0` returns transient `errSegmentNotReady`; size `< total` returns `ErrInvalidSize` (never resized or modified); size `>= total` maps the first `total` bytes and enters the Creator claim loop (section 5.1).
- **Openers (`Open` / `Dial`)**: Open existing objects with `O_RDWR` (never `O_CREAT`), check `st_size` **before** mapping (`0` -> `errSegmentNotReady`, `< total` -> `ErrInvalidSize`), map the first `total` bytes, and enter the Opener claim loop.

### 11.4 Instance Ownership and Safe Unlinking **[IOP]**

Closing a `Creator` endpoint (or calling `Unlink`) removes the POSIX shared-memory name **if and only if the name still refers to the segment instance with matching `creatorToken`**:

| Platform | Pre-Unlink Identity Check |
| --- | --- |
| Linux | `fstat(open_fd)` matches `lstat("/dev/shm/<base>")` (`st_dev` and `st_ino`) **and** `load(sb.creatorToken) == myInstanceToken` (or `state == Empty` for an unclaimed fresh file). |
| macOS, FreeBSD | Re-open `shm_open("/<base>", O_RDONLY)`, verify `st_size >= pageSize`, map page 0 read-only, and verify `load(sb.creatorToken) == myInstanceToken` (or `state == Empty`). |
| Windows | No-op: kernel file mappings and semaphores disappear automatically when their last handle closes. |

### 11.5 Windows File Mappings and Named Semaphores **[IOP]**

Each named segment on Windows is accompanied by four named kernel semaphores created **before** the file mapping:

| Index | Suffix | Purpose |
| ---: | --- | --- |
| `0` | `.hqq-sem0` | `ring 0` `notEmpty` |
| `1` | `.hqq-sem1` | `ring 0` `notFull` |
| `2` | `.hqq-sem2` | `ring 1` `notEmpty` |
| `3` | `.hqq-sem3` | `ring 1` `notFull` |

## 12. Wire Error Codes and Error Hierarchy **[IOP]**

Wire `ErrorCode` values carried in `OpError.Operand1`, `OpStandardLinkTombstone.Operand2`, and Advanced error-response `aux`:

| Code | Constant | Mapped Sentinel |
| ---: | --- | --- |
| `0x01` | `ErrCodeMemoryAlign` | `ErrMemoryAlign` |
| `0x02` | `ErrCodeInvalidSize` | `ErrInvalidSize` |
| `0x03` | `ErrCodeMemorySmall` | `ErrMemorySmall` |
| `0x04` | `ErrCodeFailedInit` | `ErrFailedInit` |
| `0x05` | `ErrCodeConnNotFound` | `ErrConnNotFound` |
| `0x06` | `ErrCodeInvalidOp` | `ErrInvalidOp` |
| `0x07` | `ErrCodeBufferOverflow` | `ErrBufferOverflow` |
| `0x08` | `ErrCodeShuttingDown` | `ErrShuttingDown` (Protocol 1.1) |
| other | Unrecognized wire code | Matches `ErrProtocol` (`ErrInvalidOp`) |

Public error hierarchy (`errors.Is` wrapping relations):

| Sentinel | Wraps | Condition |
| --- | --- | --- |
| `io.EOF` | None | `StandardLink.Read` / `ReadFrom` after graceful peer close and ring drain |
| `ErrClosed` | `net.ErrClosed` | Local link or stream closed |
| `ErrPeerClosed` | `io.EOF` | Peer detached gracefully (`token == 1`) or closed stream |
| `ErrPeerGone` | `ErrConnNotFound` | Peer heartbeat timed out, token replaced, or segment re-initialized |
| `ErrTimeout` | `os.ErrDeadlineExceeded` | Operation deadline expired |
| `ErrLinkBusy` | `ErrFailedInit` | Live endpoint already occupies the requested slot |
| `ErrIncompatibleLayout` | `ErrFailedInit` | Foreign magic, wrong major version, unknown `requiredFeatures`, non-zero `Empty` page, or invalid ring header |
| `ErrInvalidSize` | None | Invalid geometry, smaller existing file, `GeometryError`, or out-of-bounds chunk/payload size |
| `ErrMemoryAlign` / `ErrMemorySmall` | None | Caller-supplied memory not page-aligned or smaller than `total` |
| `ErrFailedInit` | None | Base error for initialization and transient attach failures |
| `ErrInvalidName` | None | Malformed segment name or non-regular file in `/dev/shm` |
| `ErrBufferOverflow` | None | Payload exceeds `bufferSize` (Standard) or `MaxMessageSize` (Advanced) |
| `ErrInvalidOp` | None | Base error for protocol state violations |
| `ErrNotNegotiated`, `ErrAlreadyNegotiated`, `ErrNegotiationFailed`, `ErrNegotiationRejected`, `ErrUnexpectedPacket`, `ErrProtocol` | `ErrInvalidOp` | Handshake and frame-validation errors |
| `ErrMessageAborted` | `ErrProtocol` | Peer aborted an in-flight response via `AdvFlagAbort` |
| `ErrIncomingOverflow`, `ErrReassemblyLimit` | `ErrBufferOverflow` | Queue overflow under `OverflowFail` or response exceeding `MaxReassemblyBytes` |

## 13. Interoperability Summary

The rules below separate the mandatory cross-process contract from local implementation choices.

### 13.1 Normative Cross-Process Contract (`[IOP]`)

Two implementations in different languages interoperate safely when both adhere to:
1. Page-aligned region offsets (section 2) and superblock/endpoint record layouts (section 3).
2. Superblock claim, takeover, reuse, and ordered publication state machines (section 5) and heartbeat/liveness rules (section 6).
3. Ring layout v2 formatting, attachment, and `seq[s]` synchronization (section 7) with the atomic orders of [`MEMORY_MODEL.md`](MEMORY_MODEL.md).
4. The Dekker waiter/waker handshake (`W1..W4`, `K1..K3`) and OS wait primitives (section 8).
5. The 64-byte packet ABI and slot-owned payload buffer discipline (section 9).
6. Advanced negotiation, stream parity, chunk reassembly, and Protocol 1.1 extensions (section 10 and `PROTOCOL_1_1_DRAFT.md`).
7. Platform naming, creation, and safe-unlink verification (section 11) and wire error codes (section 12).

### 13.2 Local Implementation Freedom (`[IMPL]`)

Each implementation is free to choose its own concurrency model, SPSC local locking (section 7.6), spin-only simple mode (section 8), process-local waiter aggregation (section 8.6), backoff constants, buffer pooling, and API surface.

### 13.3 Unsupported Target Architectures **[IOP]**

HQQ requires little-endian byte order and hardware address-free lock-free 32-bit and 64-bit atomics. Big-endian targets and 32-bit targets without lock-free 64-bit atomics (`mips`, `mipsle`, pre-ARMv7 `arm`) are unsupported and `MUST` be rejected at compile time ([`MEMORY_MODEL.md`](MEMORY_MODEL.md) sections 6 and 7).

## 14. Reference Golden Vectors

Machine-readable golden vectors verifying this specification live in [`spec/constants.json`](constants.json) and [`conformance/vectors/`](../conformance/README.md):

- **Packet byte vectors**: `constants.json` (`packet_vectors`) and `conformance/vectors/manifest.json` (`packets`).
- **Region offset vectors**: `constants.json` (`segment.layout_vectors`) and `conformance/vectors/manifest.json` (`layouts`).
- **Minimal `Ready` segment image** (checked by `TestSpecSegmentImage` and `conformance/` group `G-06` for `count = 4`, `size = 64`, 16 KiB pages, heartbeats disabled): `magic = 0x4851515F53544C33`, `state = 2`, `major = 3`, `minor = 0`, `pageSize = 16384`, `bufferCount = 4`, `bufferSize = 64`, `layoutSize = 81920` (`ring0 = 16384`, `buf0 = 32768`, `ring1 = 49152`, `buf1 = 65536`), `epoch = 1`, `initToken = 0`, `requiredFeatures = 0`, `creatorToken = 0`, `endpoints[0] = {ownerToken, pid, heartbeat, 0}`, `endpoints[1] = {0, 0, 0, 0}`; both rings have `magic = 0xC9D8C1D43F096702`, `size = 4`, `flag = 2`, `stride = 128`, `version = 2`, `r = w = 0`, `seq[i] = i`, and zeroed packets.

## 15. Audit Findings and Edge-Case Resolutions

See [`FINDINGS.md`](FINDINGS.md) for the complete log of specification audit findings (`F-01` through `F-16`) and their regression tests.
