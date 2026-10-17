# HQQ Listener Segment (draft) — layout `HQQ_LSN1`, version 1.0

Status: draft, implemented by the Go reference (`listener.go`, `listener_layout.go`) and meant to be implemented again by the C library. Normative words (MUST, SHOULD, MAY) follow RFC 2119. All integers are unsigned, stored in the byte order of the host (little-endian on every supported platform), and every 8-byte word is accessed with 64-bit atomic loads and stores (sequentially consistent or acquire/release, as `PROTOCOL.md` requires for the link superblock). The link segment (`HQQ_STL3`), the MPMC ring (v2) and the packet ABI are specified in `PROTOCOL.md` and are not changed by this document.

## 1. Purpose

A link has exactly two endpoints. A service with many clients needs a rendezvous: the **acceptor** (server) owns a well-known **listener segment**; each **client** creates a link segment of its own, as the Primary endpoint of an ordinary link, announces it with a **connect record** in the listener's ring, and waits until the acceptor attaches to it as the Secondary and answers the negotiation request. After that the pair is a normal two-endpoint link; the listener is not involved any more.

Properties that follow from the design:

- Every client has its own segment, rings, buffers and heartbeat. One client crashing or stalling never affects another client or the acceptor's other links.
- The listener ring is the only shared structure. Its records are small, fixed-size and pointer-free, and are untrusted input (§9).
- The lifecycle of an accepted link is independent of the acceptor: closing the acceptor does not close links it already handed out.

## 2. Names

- The **listener name** is a shared-memory name (as for link segments: POSIX `shm_open` name, optionally with one leading `/`; on Windows a kernel object name, optionally prefixed `Global\` or `Local\`; see `PROTOCOL.md`, named segments). It is chosen by the application.
- A **link name** is the name of a client's link segment: `prefix` followed by exactly 16 lowercase hexadecimal digits (`[0-9a-f]`), the random part, which a client SHOULD take from a cryptographically seeded 64-bit random value written as a zero-padded hex number.
- The **link name prefix** is chosen by the acceptor and **stored in the listener header** (`prefixLen`, `prefix`). Clients MUST use the stored prefix verbatim and MUST NOT derive it. The Go reference derives it from the listener name as follows (a C acceptor SHOULD do the same):
  1. If the name starts with `Global\` or `Local\` (case-insensitive, and followed by at least one more character), that scope prefix is kept as it was written and removed from the rest.
  2. One leading `/` of the rest is dropped.
  3. At most the first 12 bytes of the rest are kept; every byte outside `[A-Za-z0-9_-]` becomes `_`.
  4. A `.` is appended.
  Examples: `/hq1a-3-xyz` → `hq1a-3-xyz.`; `averyverylongservicename` → `averyverylon.`; `Global\shared` → `Global\shared.`.
- The prefix MUST be 1 to 32 bytes. With this derivation a link name without a scope is at most 12 + 1 + 16 = 29 bytes, which fits the 31-byte limit for POSIX shared memory names on macOS (the limit includes the leading `/` that `shm_open` needs, so a name without it may be at most 30 bytes). On Windows both the listener name and the link names, each with a `.hqq-semN` suffix, MUST fit the 259-character kernel object name limit.
- The random part and the prefix are not secrets. Name checks are for namespacing and validation, not for authentication.

## 3. Segment layout

```text
offset 0       HEADER                     4096 bytes (uint64 words, §3.1)
offset 4096    MPMC RING of connect records   capacity slots, internal/mpmc layout v2 (§3.3)
               zero padding up to a multiple of the page size
```

`ringOffset = 4096` regardless of the page size. The **layout size** of a segment is `align_page(4096 + 512 + capacity × 256)`, where `align_page` rounds up to the page size of the host (the ring size is `512 + capacity × 256` bytes, see §3.3). `capacity` is a power of two, 2 ≤ capacity ≤ 65536.

### 3.1 Header

All words are `uint64` unless noted. "Written by" tells who stores the word; a reader MUST NOT write any word except the ones marked *client*.

| Offset | Name | Meaning |
| ---: | --- | --- |
| 0 | `magic` | `0x4851515f4c534e31` (`"HQQ_LSN1"` read as a big-endian number). Written last during initialization (§5.2). |
| 8 | `state` | `0` Empty, `1` Initializing, `2` Ready (§4). |
| 16 | `major` | Layout major version, `1`. A different major version is incompatible. |
| 24 | `minor` | Layout minor version, `0`. Informational: a reader MUST accept any minor version of its major version. |
| 32 | `ringOffset` | `4096`. |
| 40 | `recordSize` | `248`, the size of a connect record. |
| 48 | `capacity` | Number of ring slots. |
| 56 | `layoutSize` | Size of the whole segment in bytes (`align_page(4096 + 512 + capacity × 256)` for the writer's page size). |
| 64 | `epoch` | Incremented by 1 at the end of every (re)initialization. |
| 72 | `initToken` | Token of the endpoint that is initializing the segment, `0` otherwise. |
| 80 | `initHeartbeat` | Unix nanoseconds of the initializer's last progress. |
| 88 | `requiredFeatures` | Layout features a reader MUST understand. Version 1.0 defines none; a reader that sees a bit it does not know MUST fail with `ErrIncompatibleLayout`. |
| 96 | `creatorToken` | Random non-zero token that identifies this *instance* of the segment (the one that created the name). Stored by the initializer when it claims the segment and replaced on every initialization. |
| 104 | reserved | 3 words, zero. |
| 128 | `serverToken` | Owner token of the acceptor: `0` none, `1` (`detachedToken`) the acceptor closed gracefully, any other value the live (or crashed) acceptor. Tokens are random and never `0` or `1`. |
| 136 | `serverPID` | Process id of the acceptor. Diagnostic only; not comparable across PID namespaces. |
| 144 | `serverHeartbeat` | Unix nanoseconds, refreshed by the acceptor every `serverInterval`. |
| 152 | `serverInterval` | Heartbeat interval in nanoseconds; `0` means heartbeats are disabled. |
| 160 | `maxBufferCount` | Largest `bufferCount` the acceptor accepts in a record. |
| 168 | `maxBufferSize` | Largest `bufferSize` the acceptor accepts. |
| 176 | `maxLinkSize` | Largest link segment size (`SizeStandardLink(bufferCount, bufferSize)`, `PROTOCOL.md`) the acceptor accepts. |
| 184 | `features` | Features the acceptor offers on accepted links (`FeatureMessages = 1`, `FeatureRequestResponse = 2`, …). |
| 192 | `prefixLen` | Length of `prefix`, 1..32. |
| 200 | `prefix` | 32 bytes: the link name prefix, not NUL-terminated, unused bytes zero. |
| 232..4095 | reserved | Zero. |

Binding to the link superblock: the offsets of `state` (8) and `creatorToken` (96) are the same as in the link superblock (`HQQ_STL3`), as are `major`, `minor`, `epoch`, `initToken`, `initHeartbeat` and `requiredFeatures` (16, 24, 64, 72, 80, 88). An implementation can therefore reuse its name-ownership code (§8) for both segment kinds without knowing which one it looks at.

### 3.2 Connect record

A record is 248 bytes, the payload of one ring slot; with the 8-byte slot sequence word it fills exactly 256 bytes.

| Offset | Name | Meaning |
| ---: | --- | --- |
| 0 | `clientToken` | The `creatorToken` of the client's link segment (offset 96 of its superblock). MUST be neither `0` nor `1`. |
| 8 | `bufferCount` | `BufferCount` of the link segment. |
| 16 | `bufferSize` | `BufferSize` of the link segment. |
| 24 | `features` | Features the client will request in its negotiation request. Informational: the acceptor does not reject on it. |
| 32 | `flags` | MUST be `0` when written. A receiver MUST drop a record with a non-zero value (a flag it does not understand may change the meaning of the record). |
| 40 | `nameLen` | Length of `name` in bytes. |
| 48 | `clientPID` | Process id of the client. Diagnostic only. |
| 56 | `reserved` | MUST be written as `0`; receivers ignore it. |
| 64 | `name` | 184 bytes: the link name, not NUL-terminated. A writer MUST zero the unused bytes; a receiver MUST NOT read beyond `nameLen`. |

### 3.3 Ring

The ring at `ringOffset` is an `internal/mpmc` ring (layout v2, `PROTOCOL.md` "MPMC Ring Layout (v2)") whose slot payload is a connect record: 512-byte ring header, then `capacity` slots of stride 256 (8-byte sequence word at slot offset 0, the record at slot offset 8). Magic, size, flag, stride (`256`) and version are those the ring `Format` writes. Many clients are producers, the acceptor is the only consumer. All producers and the consumer use the standard claim, publish and release protocol of the ring, including its wait/park protocol. On Windows the ring parks on the first two of the four semaphores that accompany every shared-memory segment (`.hqq-sem0` notEmpty, `.hqq-sem1` notFull; `.hqq-sem2` and `.hqq-sem3` are unused).

The stuck-slot recovery of §6.4 reads the consumer cursor `r` (ring offset 128), the producer cursor `w` (ring offset 256) and the sequence word of slot `r & (capacity − 1)` (ring offset `512 + (r & mask) × 256`).

## 4. State machine

```text
        claim (CAS 0→1)                       publish (CAS initToken, epoch++)
Empty ───────────────► Initializing ────────────────────────────────► Ready
                            ▲   │ initializer stalls: another creator takes over   │
                            └───┘                                                  │
                            ▲        acceptor dead or closed: CAS 2→1              │
                            └──────────────────────────────────────────────────────┘
```

`state` never returns to Empty. Only a *creator* (§5) moves a segment into Initializing; clients never write the state.

## 5. Acceptor: creating and reusing the name

The acceptor sizes the object to the layout size and then claims it. Creation follows the creator rules of links (`PROTOCOL.md`, "Named Segments"):

1. Create the object exclusively. If the name exists, open the existing object instead: an object smaller than the layout size is **not** reused and not modified: a listener of this layout version (magic `HQQ_LSN1`, major 1, no unknown required feature in its header) is `ErrInvalidSize` (the operator removes it with `Unlink`), and any other object (a link segment, foreign data, or an object shorter than the 4096-byte header) is `ErrIncompatibleLayout`. Whether a shorter object is a listener never depends on its size alone. A zero-size object is the transient `ErrFailedInit`, a larger object is used as it is, and on Linux a name that is not a regular file is `ErrInvalidName`.
2. Claim loop (every iteration first checks the caller's context; waits between looks are 200 µs):
   - **Empty.** If the 4096 header bytes are not all zero (every word read atomically) and `state` is still Empty, the object is foreign: fail with `ErrIncompatibleLayout` **without writing anything**. If `state` changed, look again. Otherwise CAS `state: 0 → 1`, then `beginInit(expected = initToken just read, normally 0)`.
   - **Initializing.** Reject a conflicting layout: `magic` neither `0` nor `HQQ_LSN1`, `requiredFeatures` with an unknown bit, or `major` not `1` unless `major = 0` and `initToken = 0` (a claim that died before it stamped the layout). Then watch `initToken` and `initHeartbeat` on the **local monotonic clock**; once neither changed for `max(PeerTimeout, 2 s)` since the pair was first seen, take over with `beginInit(expected = the observed initToken)`; if the CAS fails somebody else moved first, start watching again. A stalled initializer is given its whole window even if that exceeds `AttachTimeout`; a creator that has waited longer than that for an initializer that keeps making progress fails with the transient `errInitContended`.
   - **Ready.** Check the layout (`magic = HQQ_LSN1`, `major = 1`, no unknown `requiredFeatures` bit; otherwise `ErrIncompatibleLayout`, nothing is written). Then decide whether the acceptor is alive (§7.1). If it is alive, fail with `ErrListenerBusy` (matches `ErrLinkBusy`). Otherwise CAS `state: 2 → 1` and `beginInit(expected = current initToken)`.
   - Any other `state` value: `ErrIncompatibleLayout`.
3. `beginInit(expected)`: store `major`, `minor`, `requiredFeatures = 0`; CAS `initToken: expected → myToken` (a failed CAS means another endpoint took over: `errInitContended`); on success store `creatorToken` (a fresh random token that becomes the identity of this instance) and `initHeartbeat`.

### 5.1 Initialization

The winner of `beginInit` checks, before every step, that it still owns `initToken` and that its context is alive; if it lost ownership it MUST NOT touch the segment any more.

1. Format the ring: `capacity` slots, `seq[i] = i`, `r = w = 0`, wait words and waiter counts zero (`mpmc.Format`), then refresh `initHeartbeat`.
2. Publish, in this order: `major`, `minor`, `requiredFeatures`; `ringOffset`, `recordSize`, `capacity`, `layoutSize`; `maxBufferCount`, `maxBufferSize`, `maxLinkSize`, `features`; `prefix` (zero-filled first) and `prefixLen`; `serverPID`, `serverInterval`, `serverHeartbeat = now`, `serverToken = myServerToken` (a random token, not `0` or `1`); `magic`.
3. CAS `initToken: myToken → 0`. Only if that succeeds: increment `epoch` and store `state = 2` (Ready). A failed CAS leaves the segment as it is (`errInitContended`).

A creator that fails after it created the object removes the name again if the name still refers to this instance (§8).

### 5.2 Heartbeat

While it runs, the acceptor stores `serverHeartbeat = now` (unix nanoseconds) every `serverInterval`, as long as `serverToken` is still its own token. A negative configured interval disables the heartbeat (`serverInterval = 0`): clients can then not tell a crashed acceptor from a slow one, and a crashed listener is not reusable without `Unlink`.

## 6. Acceptor: serving

### 6.1 Dispatch

The acceptor consumes the ring with a single consumer. For every record (copied out of the slot at once, then the slot is released) it validates (§6.2). A record that fails validation is dropped and counted as *malformed*. For a valid record it starts a **handshake** (§6.3), bounded in number and time by configuration, so that one client that stalls never delays the others. Records are never trusted: no field is used before it was checked, and nothing is allocated or mapped in proportion to an unchecked field.

### 6.2 Record validation

A record is valid iff all of the following hold; the acceptor MUST check all of them before it uses any field:

1. `flags = 0`.
2. `1 ≤ nameLen ≤ 184`, and `name[0..nameLen)` equals the stored `prefix` followed by exactly 16 lowercase hexadecimal digits (so `nameLen = prefixLen + 16`).
3. `clientToken` is neither `0` nor `1`.
4. `bufferCount ≤ maxBufferCount` and `bufferSize ≤ maxBufferSize` (compared as 64-bit values before any narrowing), `bufferCount` is a power of two ≥ 2, `bufferSize` is a multiple of 8 and ≥ 8, `bufferCount ≤ 2^24` and `bufferSize ≤ 2^30`, and the segment size computed from them (`PROTOCOL.md`: superblock page, two rings and two buffer pools, each region page aligned) neither overflows nor exceeds `maxLinkSize`.

`features` and `reserved` are not checked.

### 6.3 Handshake

For a valid record, bounded by a handshake timeout (default 5 s, started when the handshake begins):

1. Open the link segment `name` as a link **opener** (`PROTOCOL.md`: never creates, never initializes), with `bufferCount` and `bufferSize` from the record, and with an **instance check**: read the `creatorToken` of the segment (superblock offset 96) *before* attaching and require it to equal `clientToken`; after the attach read it again and require the same (otherwise detach and fail). A mismatch, a missing segment, a segment without live endpoint, a geometry mismatch or any other open error drops the record and counts it as *unreachable*. The acceptor never joins a segment whose instance token is not the one announced, so a record can not make it take the Secondary slot of somebody else's link.
2. Negotiate: act as the Secondary and answer the client's negotiation request with the intersection of the requested features and the acceptor's `features`. A failure (timeout, rejection, peer gone) closes the link, counts the record as *handshake failed*, and drops it.
3. Deliver the negotiated link to the application (the `Accept` call), after which its lifecycle is independent of the acceptor. A link that is negotiated but not yet taken by the application is held in a bounded backlog; while the backlog and the handshake slots are full, no further record is processed and the ring fills up, which is the backpressure clients experience.

**Cleanup of dead clients.** If the open fails with "no live endpoint", or the negotiation fails with the peer-gone error, and the segment's `creatorToken` still equals `clientToken`, the acceptor SHOULD remove the segment name (nobody else will: its creator is dead). It MUST NOT remove a name whose token differs. (Windows names disappear with their last handle.)

### 6.4 Stuck-slot recovery

A producer that dies (or hangs) between claiming a slot (CAS on `w`) and publishing it leaves the slot with `seq = pos` while `w > pos`; the consumer would wait for it forever and every record behind it with it. The acceptor therefore watches the slot at the consumer cursor: when `w − r > 0` (signed 64-bit) and `seq[r & mask] = r` has been observed continuously, for the same `r`, for at least `stuckSlotTimeout` (default 2 s, measured on the local monotonic clock, checked at least four times per timeout), the acceptor publishes the slot on the producer's behalf with CAS `seq: r → r + 1` and wakes the consumer. The slot then holds whatever the dead producer left in it, typically zeros, which fails validation and is dropped as malformed. If the producer was only slow and commits later, its completion CAS fails and its record is lost (its `Connect` times out); a late data write into a slot that has meanwhile been reused can corrupt the record of another client, which validation then rejects or the instance check catches. The recovery can be disabled.

### 6.5 Closing

`Close` stops the dispatcher and the handshakes, wakes blocked `Accept` calls (they fail with "acceptor closed"), closes the links that completed their handshake but were not delivered, stores `serverToken = 1` (detached) with a compare-and-swap from its own token, and removes the listener name if it still refers to its instance (§8). Links already delivered stay open.

## 7. Client: connecting

### 7.1 Liveness of the acceptor (used by clients and by creators)

An acceptor is **alive** if `serverToken` is neither `0` nor `1` and one of the following holds: `serverInterval = 0` (heartbeats disabled: assumed alive), `serverHeartbeat = 0`, or the heartbeat is younger than `max(PeerTimeout, 4 × serverInterval)` by the wall clock. A heartbeat that looks older is confirmed by watching `serverHeartbeat` and `serverToken` for progress for `min(3 × serverInterval, 2 s)` on the local monotonic clock, so wall-clock jumps can not declare a live acceptor dead. `PeerTimeout` is the reader's own configuration (default 5 s). A `serverInterval` larger than one hour is treated as disabled.

### 7.2 Opening the listener

A client never initializes the listener and never writes the header. It opens the object read/write (it needs the ring), and:

1. A missing name is the transient `ErrNotExist`. A zero-size object is the transient `errSegmentNotReady`. An object smaller than 4096 bytes is `ErrIncompatibleLayout`.
2. Map the first page and inspect the header: `state = Empty` with an all-zero header or `Initializing` (with a compatible layout) is the transient `errListenerNotReady`; `Empty` with a non-zero header, a wrong `magic` or `major`, an unknown `requiredFeatures` bit, `ringOffset ≠ 4096`, `recordSize ≠ 248`, `capacity` not a power of two between 2 and 65536, `layoutSize` not equal to the layout size of `capacity` for the client's page size, or an object smaller than `layoutSize` is `ErrIncompatibleLayout` (permanent; the object is not modified).
3. Map `layoutSize` bytes and repeat the inspection on the full mapping (the listener may have been replaced in between; a difference is transient).
4. Decide whether the acceptor is alive (§7.1); if not, the transient `errListenerDead`.
5. Attach to the ring (`mpmc.Attach`; an incompatible ring is `ErrIncompatibleLayout`), take `prefix` (`prefixLen` must be 1..32), and snapshot `creatorToken`, `epoch`, `serverToken`, `serverHeartbeat` (the *watch*, §7.4).

### 7.3 Connect

1. Check the geometry: `bufferCount` and `bufferSize` valid for a link, within `maxBufferCount`, `maxBufferSize` and `maxLinkSize`; if not, fail at once with `ErrInvalidSize` (the acceptor would drop it).
2. Create the link segment exactly as a link creator does (Primary endpoint, new `creatorToken`), under `prefix` + 16 random hex digits. A collision with a live segment (`ErrLinkBusy`) is retried with a new name (up to 8 times).
3. Fill a connect record (`clientToken` = the `creatorToken` of the new segment, geometry, requested features, `flags = 0`, `name`, `nameLen`, PID; all other bytes zero) and enqueue it with the ring's claim/publish protocol. A full ring is waited for, bounded by the caller's context and by the watch (§7.4); the record is the **last** thing written into the claimed slot before it is published, and the claim-to-publish window MUST be kept as short as possible.
4. Start the negotiation as the Primary (the request is queued in the link's own ring; no peer is needed for that) and wait for the answer, bounded by the caller's context, by the watch while no acceptor has attached (the Secondary token of the link superblock is still `0`), and by the **attach timeout** (default 5 s, started when the record was published). Once the acceptor has attached, the ordinary link peer monitor takes over.
5. On any failure the client closes its link (which removes its segment) and returns the error.

If the open of §7.2 fails with a transient error (name missing, not ready, no live acceptor), the client MAY retry with exponential backoff (1 ms doubling to 100 ms) until its context ends, like `Dial`; the Go reference does. Once a record was enqueued it does not retry.

### 7.4 Watch

While a client waits (for ring space or for the acceptor to attach) it evaluates, at least every 50 ms, in this order:

| Condition | Error |
| --- | --- |
| `state ≠ Ready`, or `creatorToken` or `epoch` differs from the snapshot (the listener was re-initialized; the queued record is lost) | `ErrListenerGone` |
| `serverToken` is `0` or `1` (closed) | `ErrListenerClosed` |
| `serverToken` differs from the snapshot (replaced acceptor) | `ErrListenerGone` |
| `serverInterval ≠ 0` (and ≤ 1 h) and `serverHeartbeat` has not changed for more than `max(4 × serverInterval, PeerTimeout)` on the local monotonic clock | `ErrListenerGone` |
| attach timeout elapsed without the acceptor attaching | an error matching `ErrTimeout` |

## 8. Name ownership and unlinking

`Close` of the acceptor removes the listener name, and a failed initialization removes a name it created, **only** if the name still refers to this instance: on Linux the `fstat` of the open descriptor must match `lstat` of the name (device and inode) and the `creatorToken` in the mapping must equal the instance token; on macOS and FreeBSD the name is opened read-only again and the `creatorToken` of its first page is compared; Windows names disappear with their last handle and nothing is removed. A name that is gone or replaced is a no-op. Because `state` and `creatorToken` have the superblock's offsets (§3.1), this is the same code as for links. A client's `Close` removes its link segment by the same rule.

## 9. Trust model

Connect records come from other processes on the same host. They are untrusted-but-same-host input:

- The acceptor validates every field (§6.2), never trusts a length, and bounds everything it allocates, maps or runs concurrently by its own configuration: the geometry of accepted links (`maxBufferCount`, `maxBufferSize`, `maxLinkSize`), the number of simultaneous handshakes, the size of the delivery backlog, the handshake timeout.
- It only attaches to a segment whose name has its own prefix and whose `creatorToken` is the one the record announces, so a record can not point it at an unrelated segment of another service. The token of a segment is readable by whoever can map the segment, which is the same set of processes that could have created it.
- **Access control is the segment permission**: a process that can open the listener for writing can connect, and can also corrupt the header or the ring, or fill the queue, whatever the acceptor does. The acceptor recovers from a slot that a producer claimed and did not publish (§6.4), but a process with write access can always make connecting fail. Create the listener with permissions that allow exactly the principals that may connect (on Windows: owner bits and "other" bits, see `PROTOCOL.md`). Accepted links are the clients' segments; their permission is chosen by the client, and the acceptor's user needs access to them.
- An acceptor never executes, interprets or forwards a field of a record other than as described here.

## 10. Errors

| Error | Where | Meaning |
| --- | --- | --- |
| `ErrListenerBusy` (matches `ErrLinkBusy`, `ErrFailedInit`) | acceptor creation | the name is served by a live acceptor |
| `ErrIncompatibleLayout` | acceptor creation, client open | not a listener of this layout version; the object is never modified |
| `ErrInvalidSize` | acceptor creation, client | existing listener of this layout smaller than the layout size (creation); geometry outside the acceptor's limits or invalid |
| `ErrInvalidName` | both | invalid shared-memory name |
| `errListenerNotReady`, `errSegmentNotReady`, `errListenerDead`, `ErrNotExist` | client open | transient: the listener is not (yet or any more) usable; retried |
| `ErrListenerClosed` (matches `ErrPeerClosed`, `io.EOF`) | waiting client | the acceptor closed the listener |
| `ErrListenerGone` (matches `ErrPeerGone`) | waiting client | the acceptor stopped heartbeating, was replaced, or the listener was re-initialized |
| error matching `ErrTimeout` | waiting client | the acceptor did not attach within the attach timeout (also what a dropped record looks like) |
| `ErrAcceptorClosed` (matches `ErrClosed`, `net.ErrClosed`) | `Accept` | the acceptor was closed |

Records that are malformed, whose segment is unreachable, or whose handshake fails never fail `Accept`; they are dropped and counted (`Malformed`, `Unreachable`, `HandshakeFailed`; `StuckSlots` for §6.4).

## 11. Restart rules

- A **graceful** `Close` removes the name (or, on Windows, it disappears with the last handle). Clients that are waiting get `ErrListenerClosed`; clients that start afterwards retry until the next acceptor exists.
- A **crash** leaves the object. A new acceptor with the same name reuses it as soon as the heartbeat of the old one has been confirmed stale (§7.1: at most `max(PeerTimeout, 4 × interval)` plus the confirmation window), re-initializes it (new `creatorToken`, `epoch + 1`, empty ring) and queued records of the old instance are lost; waiting clients fail with `ErrListenerGone` and may connect again. If the new acceptor needs a bigger ring than the object has, it fails with `ErrInvalidSize` until the operator calls `Unlink`.
- Links accepted by a crashed acceptor report `ErrPeerGone` through the ordinary link peer monitor.

## 12. Timing defaults (Go reference)

| Parameter | Default |
| --- | --- |
| queue size (`capacity`) | 64 |
| heartbeat interval | 250 ms |
| `PeerTimeout` | 5 s |
| `AttachTimeout` (client wait for attach; creator wait for an initializer) | 5 s |
| handshake timeout (acceptor) | 5 s |
| stuck-slot timeout | 2 s |
| max handshakes in flight | 16 |
| delivery backlog | 16 |
| `maxBufferCount` / `maxBufferSize` / `maxLinkSize` | 4096 / 1 MiB / 256 MiB |
| client watch period | 10 ms (at least every 50 ms) |

## 13. Notes for implementers

- The record size 248 is chosen so that the ring slot is exactly 256 bytes (one 8-byte sequence word, 248 bytes of record); a C struct for the record MUST have exactly the offsets of §3.2 and `sizeof == 248` (all members are 64-bit words followed by a `uint8_t[184]`, so natural alignment produces no padding).
- The header words at 8 and 96 are what the safe-unlink code of a link implementation reads from the first page of an object; do not move them.
- Everything a client or the acceptor reads from the segment is read with atomic loads, even the ones that are immutable after initialization, and everything is validated after the segment was mapped, never before.
