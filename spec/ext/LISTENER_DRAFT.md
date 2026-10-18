# HQQ Listener Segment Specification (`HQQ_LSN1`, Version 1.0)

**Status**: Active specification, implemented by the Go library (`listener.go`, `listener_layout.go`), the C11 reference library (`c/src/hqq_acceptor.c`), and the C++17 and Rust bindings.

Normative keywords (`MUST`, `MUST NOT`, `SHOULD`, `MAY`) follow RFC 2119. All integers are unsigned and stored in little-endian byte order, and every 8-byte control word is accessed through 64-bit atomic loads and stores (`seq_cst` or acquire/release as specified in [`MEMORY_MODEL.md`](../MEMORY_MODEL.md)). Per-client link segments (`HQQ_STL3`), MPMC rings (layout v2), and the 64-byte packet ABI remain unchanged as specified in [`SPEC.md`](../SPEC.md).

## 1. Purpose

An HQQ link connects exactly two processes. To allow a single server process to accept connections from an arbitrary number of concurrent clients, the server (**acceptor**) creates a shared **listener segment** (`HQQ_LSN1`) under a well-known name. Each connecting **client** creates its own dedicated link segment (`HQQ_STL3`) as the Primary endpoint, publishes a 248-byte **connect record** into the listener segment's MPMC ring, and waits for the acceptor to attach to that client segment as the Secondary endpoint and reply to `OpProtoNegotiate`. Once negotiation completes, the client and server communicate over their private two-endpoint link without further involvement from the listener segment.

Key properties of this architecture:

- **Fault isolation per client**: Every client session has its own shared-memory mapping, rings, payload buffers, and heartbeat. A crashing, stalling, or misbehaving client cannot corrupt or block another client's link.
- **Minimal untrusted surface**: The listener ring is the only structure shared across multiple clients. Its connect records are fixed-size (248 bytes), contain no pointers, and are validated strictly as untrusted input (section 6.2 and section 9).
- **Independent link lifetimes**: Closing the acceptor stops new connection handshakes without tearing down links that have already been accepted and handed to the application.

## 2. Segment Naming Rules

Listener names and per-client link names follow the platform naming rules of `SPEC.md` section 11.1 with an acceptor-chosen prefix:

- **Listener name**: A standard HQQ shared-memory segment name chosen by the application (POSIX `shm_open` name with an optional leading `/`; Windows kernel object name with an optional `Local\` or `Global\` prefix).
- **Per-client link name**: Consists of the listener's `prefix` followed by exactly 16 lowercase hexadecimal digits (`[0-9a-f]`) generated from a cryptographically seeded 64-bit random number.
- **Link name prefix**: Chosen by the acceptor at initialization and **stored in the listener header** (`prefixLen` and `prefix`). Clients `MUST` read and use the stored prefix verbatim rather than deriving it themselves. Both the Go and C acceptors derive the prefix from the listener name using four steps:
  1. If the listener name begins with `Global\` or `Local\` (ASCII case-insensitive, followed by at least one character), retain that scope prefix as written and strip it from the remainder.
  2. Strip one leading `/` from the remainder if present.
  3. Take at most the first 12 bytes of the remainder, replacing any byte outside `[A-Za-z0-9_-]` with `_`.
  4. Append a trailing `.`.
  - Examples: `/hq1a-3-xyz` becomes `hq1a-3-xyz.`; `averyverylongservicename` becomes `averyverylon.`; `Global\shared` becomes `Global\shared.`.
- **Length budget**: `prefixLen` `MUST` be between `1` and `32` bytes inclusive. With the 12-byte base derivation above, an unprefixed link name is at most `12 + 1 + 16 = 29` bytes, fitting within the 30-byte macOS `shm_open` limit (31 bytes including the leading `/`). On Windows, both the listener name and the derived link names (plus their `.hqq-sem0..3` semaphore suffixes) `MUST` fit within the 259 UTF-16 code-unit kernel object path limit.
- **Non-secret identifiers**: Neither the prefix nor the 16-digit random suffix is a cryptographic secret; they provide collision-free namespacing and sanity validation, while access control is enforced by OS shared-memory permissions (section 9).

## 3. Listener Segment Layout

A listener segment consists of a 4096-byte header followed immediately at offset `4096` by a single MPMC ring (layout v2) of 248-byte connect records, padded to a multiple of the host page size:

```text
offset 0       HEADER                         4096 bytes (uint64 words, section 3.1)
offset 4096    MPMC RING of connect records   capacity slots, layout v2 (section 3.3)
               Zero padding to a multiple of the host page size
```

Regardless ofwhether the host page size is 4 KiB, 16 KiB, or 64 KiB, `ringOffset` is always `4096`. Total segment size is `layoutSize = align_page(4096 + 512 + capacity * 256)`, where `capacity` is a power of two in `[2, 65536]`.

### 3.1 Listener Header (`4096` Bytes)

All header fields are 64-bit little-endian words (`uint64`) unless noted otherwise. Only an initializing or serving acceptor writes to the header; clients `MUST NOT` modify any header word:

| Byte Offset | Field | Description |
| ---: | --- | --- |
| `0` | `magic` | `0x4851515F4C534E31` (`"HQQ_LSN1"`). Written last during header publication (section 5.1). |
| `8` | `state` | `0` (`Empty`), `1` (`Initializing`), or `2` (`Ready`) (section 4). |
| `16` | `major` | Layout major version (`1`). Any other value is rejected with `ErrIncompatibleLayout`. |
| `24` | `minor` | Layout minor version (`0`). Readers `MUST` accept any minor version when `major == 1`. |
| `32` | `ringOffset` | Byte offset of the MPMC ring (`4096`). |
| `40` | `recordSize` | Payload size of each connect record (`248`). |
| `48` | `capacity` | Number of slots in the connect ring (power of two, `2 <= capacity <= 65536`). |
| `56` | `layoutSize` | Total segment size in bytes (`align_page(4096 + 512 + capacity * 256)` for the host page size). |
| `64` | `epoch` | Incremented by `1` at the end of every successful initialization. |
| `72` | `initToken` | Token of the acceptor currently initializing the segment (`0` when not initializing). |
| `80` | `initHeartbeat` | Wall-clock `UnixNano` timestamp of the initializer's most recent progress step. |
| `88` | `requiredFeatures` | Required layout feature bitmask (`0` in version 1.0). Any unrecognized bit fails with `ErrIncompatibleLayout`. |
| `96` | `creatorToken` | Random non-zero instance token written when claiming initialization; identifies this segment instance for safe unlinking (section 8). |
| `104..127` | `reserved` | Three 64-bit words, written as `0`. |
| `128` | `serverToken` | Owner token of the active acceptor: `0` (none), `1` (`detachedToken`, closed gracefully), or any other value (live or crashed acceptor). |
| `136` | `serverPID` | Process ID of the acceptor (diagnostic only). |
| `144` | `serverHeartbeat` | Wall-clock `UnixNano` timestamp refreshed by the acceptor every `serverInterval`. |
| `152` | `serverInterval` | Acceptor heartbeat interval in nanoseconds (`0` when heartbeats are disabled). |
| `160` | `maxBufferCount` | Maximum `bufferCount` the acceptor permits in a client connect record. |
| `168` | `maxBufferSize` | Maximum `bufferSize` in bytes the acceptor permits in a client connect record. |
| `176` | `maxLinkSize` | Maximum total link segment size (`SizeStandardLink`) the acceptor permits. |
| `184` | `features` | Protocol feature bitmask offered by the acceptor on accepted links (`FeatureMessages`, `FeatureRequestResponse`, `FeatureCancel`, `FeatureMetadata`). |
| `192` | `prefixLen` | Byte length of `prefix` (`1..32`). |
| `200..231` | `prefix` | 32-byte link name prefix (not NUL-terminated; trailing bytes beyond `prefixLen` are zero). |
| `232..4095` | `reserved` | Zero. |

Notice that `state` (`@8`), `major` (`@16`), `minor` (`@24`), `epoch` (`@64`), `initToken` (`@72`), `initHeartbeat` (`@80`), `requiredFeatures` (`@88`), and `creatorToken` (`@96`) occupy the exact same byte offsets as in the link superblock (`HQQ_STL3`), allowing implementations to share safe-unlink and initialization-claim helpers across both segment types.

### 3.2 Connect Record (`248` Bytes)

Each connect record occupies the 248-byte payload region of one 256-byte ring slot (immediately following the 8-byte slot sequence word `seq` at slot offset `0`):

| Record Offset | Field | Description |
| ---: | --- | --- |
| `0` | `clientToken` | The `creatorToken` of the client's newly created link segment (superblock offset `96`). `MUST` be neither `0` nor `1`. |
| `8` | `bufferCount` | `bufferCount` of the client's link segment. |
| `16` | `bufferSize` | `bufferSize` of the client's link segment. |
| `24` | `features` | Protocol feature bits the client intends to request during negotiation (informational). |
| `32` | `flags` | Reserved flags word; writers `MUST` store `0`, and acceptors `MUST` drop any record with `flags != 0`. |
| `40` | `nameLen` | Byte length of `name` (`prefixLen + 16`). |
| `48` | `clientPID` | Process ID of the client (diagnostic only). |
| `56` | `reserved` | Reserved word; writers `MUST` store `0` and receivers ignore it. |
| `64..247` | `name` | 184-byte link segment name (not NUL-terminated; bytes from `nameLen` to `183` `MUST` be zero). |

### 3.3 Connect Ring Layout

The connect ring at `ringOffset = 4096` uses `internal/mpmc` layout v2 (`SPEC.md` section 7.1) with `stride = 256`: a 512-byte ring header followed by `capacity` slots of 256 bytes each (`8`-byte `seq` at slot offset `0` and the `248`-byte connect record at slot offset `8`). Multiple client processes act as concurrent MPMC producers, while the single acceptor process acts as the sole consumer. On Windows, parking uses the first two named semaphores accompanying the listener mapping (`.hqq-sem0` for `notEmpty` and `.hqq-sem1` for `notFull`; `.hqq-sem2` and `.hqq-sem3` remain unused).

## 4. Listener State Machine

The listener segment `state` word transitions through three states:

```text
         claim (CAS 0 -> 1)                    publish (CAS initToken, epoch++)
Empty --------------------> Initializing ---------------------------------> Ready
                                ^   | initializer stalls: another acceptor    |
                                +---+ takes over                              |
                                ^          acceptor dead or closed: CAS 2 -> 1|
                                +---------------------------------------------+
```

Once initialized, `state` never returns to `Empty` (`0`). Only an acceptor (section 5) transitions `state`; clients never write to `state`.

## 5. Acceptor Creation and Segment Reuse

When starting a listener, the acceptor sizes or opens the shared-memory object and runs the creator claim loop:

1. **Create or open the backing object**: Attempt exclusive creation (`O_CREAT|O_EXCL`). If the name already exists, open the existing object without truncating it:
   - If the existing object is smaller than `layoutSize`: do not modify it. If it has `magic == HQQ_LSN1`, `major == 1`, and no unknown `requiredFeatures`, return `ErrInvalidSize`; otherwise (for example, if it is a link segment or shorter than 4096 bytes), return `ErrIncompatibleLayout`.
   - If the existing object has size `0`, treat it as transient (`ErrFailedInit`).
   - On Linux, if the path in `/dev/shm` is not a regular file, return `ErrInvalidName`.
2. **Execute the claim loop** (checking the caller's deadline on each iteration and sleeping `200 us` between retries):
   - **`Empty` (`state == 0`)**: Atomically read all 512 64-bit words of the 4096-byte header. If any word is non-zero while `state` remains `0`, fail with `ErrIncompatibleLayout` without writing to the segment. Otherwise, execute `CAS(state, 0 -> 1)` and `beginInit(expected = initToken)`.
   - **`Initializing` (`state == 1`)**: Reject incompatible headers (`magic` neither `0` nor `HQQ_LSN1`, unknown `requiredFeatures`, or `major != 1` unless `major == 0 && initToken == 0`). Monitor `(initToken, initHeartbeat)` on the **local monotonic clock**; if neither advances for `max(PeerTimeout, 2s)`, take over via `beginInit(expected = observedInitToken)`.
   - **`Ready` (`state == 2`)**: Verify `magic == HQQ_LSN1`, `major == 1`, and `requiredFeatures == 0` (else `ErrIncompatibleLayout`). Check whether the existing acceptor is alive (section 7.1). If alive, fail with `ErrListenerBusy` (matching `ErrLinkBusy`). If dead or closed (`serverToken == 1`), execute `CAS(state, 2 -> 1)` and `beginInit(expected = currentInitToken)`.
   - **Any other `state`**: Return `ErrIncompatibleLayout`.
3. **`beginInit(expected)`**: Store `major = 1`, `minor = 0`, and `requiredFeatures = 0`; execute `CAS(initToken, expected -> myInitToken)` (returning transient `errInitContended` on failure); then store a fresh random `creatorToken` and `initHeartbeat = now_unix_nano()`.

### 5.1 Header and Ring Initialization

Before each initialization step, the winning acceptor verifies that `initToken == myInitToken` and that its caller context has not expired, aborting immediately if ownership was lost:

1. Format the connect ring (`capacity` slots, `stride = 256`, `seq[i] = i`, `r = w = 0`, wake words and waiter counters zeroed, `flag = 2` written last with release ordering) and refresh `initHeartbeat`.
2. Write the header fields in order: `major`, `minor`, `requiredFeatures`; `ringOffset = 4096`, `recordSize = 248`, `capacity`, `layoutSize`; `maxBufferCount`, `maxBufferSize`, `maxLinkSize`, `features`; zero-filled `prefix` and `prefixLen`; `serverPID`, `serverInterval`, `serverHeartbeat = now_unix_nano()`, `serverToken = myServerToken` (random, neither `0` nor `1`); and finally `magic = HQQ_LSN1`.
3. Commit via `CAS(initToken, myInitToken -> 0)`. If and only if the CAS succeeds, increment `epoch` (`fetch_add(1)`) and store `state = Ready` (`2`) with release ordering.

### 5.2 Acceptor Heartbeat

While running, the acceptor updates `serverHeartbeat = now_unix_nano()` every `serverInterval` as long as `serverToken == myServerToken`. Setting a negative heartbeat interval disables heartbeats (`serverInterval = 0`).

## 6. Acceptor Connection Handling

The acceptor drains the connect ring and performs bounded, concurrent client handshakes.

### 6.1 Record Dispatch

A single consumer loop dequeues each 248-byte connect record from the ring, copies it into process-local memory, and releases the ring slot immediately. If the record fails validation (section 6.2), it is dropped and increments the `Malformed` counter. If valid, the acceptor spawns a bounded handshake task (section 6.3) so a slow or stalled client cannot block other connecting clients.

### 6.2 Connect Record Validation

An acceptor `MUST` verify all four rules before using any field of a dequeued connect record:

1. `flags == 0`.
2. `1 <= nameLen <= 184`, `nameLen == prefixLen + 16`, `name[0..prefixLen)` matches the listener's `prefix`, and `name[prefixLen..nameLen)` consists of 16 lowercase ASCII hex digits (`[0-9a-f]`).
3. `clientToken != 0 && clientToken != 1`.
4. `bufferCount <= maxBufferCount` and `bufferSize <= maxBufferSize` (compared as unsigned 64-bit integers), `bufferCount` is a power of two in `[2, 2^24]`, `bufferSize` is a multiple of 8 in `[8, 2^30]`, and `SizeStandardLink(bufferCount, bufferSize)` neither overflows nor exceeds `maxLinkSize`.

### 6.3 Link Attachment and Handshake

For each valid record, bounded by `HandshakeTimeout` (default `5s`):

1. **Open and verify instance identity**: Open the named link segment `name` as a Secondary opener (`OpenStandardLink` / `OpenAdvancedLink`, which never creates or initializes) using `bufferCount` and `bufferSize` from the record. Before claiming the Secondary slot, read the link superblock's `creatorToken` (`@96`) and verify `creatorToken == record.clientToken`; re-verify `creatorToken == record.clientToken` immediately after claiming the Secondary slot (detaching if it changed). If opening or verification fails, drop the record and increment `Unreachable`.
2. **Negotiate protocol features**: Act as the Secondary responder in `Negotiate`, answering the client's `OpProtoNegotiate` with `requestedFeatures & acceptor.features`. If negotiation fails or times out, close the link, increment `HandshakeFailed`, and drop the record.
3. **Enqueue for `Accept`**: Place the negotiated link into the bounded delivery backlog for the application's `Accept` call. When both the delivery backlog and in-flight handshake slots are full, the acceptor pauses dequeuing from the connect ring, applying natural ring backpressure to connecting clients.
4. **Dead-client segment cleanup**: If opening fails because the client died before attach (`errNoLiveEndpoint`) or negotiation fails with `ErrPeerGone`, and the link segment's `creatorToken` still equals `record.clientToken`, the acceptor `SHOULD` unlink the orphaned client segment name on POSIX.

### 6.4 Stuck-Slot Recovery

If a client process crashes or gets killed with `SIGKILL` after claiming a producer slot (`CAS` on `w`) in the connect ring but before publishing `seq[s] = pos + 1`, the consumer cursor `r` would otherwise stall indefinitely at `pos = r`. To recover automatically:

- The acceptor monitors the consumer cursor `r` whenever `int64(w - r) > 0` and `seq[r & mask] == r`.
- If `r` and `seq[r & mask] == r` remain unchanged continuously for `stuckSlotTimeout` (default `2s` on the local monotonic clock, sampled at least four times during the window), the acceptor forces publication on the dead producer's behalf via `CAS(seq[r & mask], r -> r + 1)`, increments `StuckSlots`, and wakes the consumer.
- The consumer then reads the uninitialized or partial slot, fails record validation (section 6.2) or instance-token verification (section 6.3), and discards it safely.

### 6.5 Acceptor Shutdown (`Close`)

Calling `Close` on the acceptor stops record dispatch, cancels in-flight handshakes, wakes any blocked `Accept` callers with `ErrAcceptorClosed`, closes any completed links still sitting in the delivery backlog, executes `CAS(serverToken, myServerToken -> 1)`, and unlinks the listener segment name if `creatorToken` still matches (section 8). Links already returned by `Accept` remain open and unaffected.

## 7. Client Connection Procedure

Clients connect to a listener by validating acceptor liveness, creating a dedicated link segment, and publishing a connect record.

### 7.1 Evaluating Acceptor Liveness

Both connecting clients and new acceptors consider an existing acceptor **alive** if `serverToken` is neither `0` nor `1` and any of the following holds:

- `serverInterval == 0` or `serverInterval > 1 hour` (heartbeats disabled),
- `serverHeartbeat == 0` (just initialized), or
- `wallAge(serverHeartbeat) < max(PeerTimeout, 4 * serverInterval)` (and within the 1 s future-clock-skew slack).

If the wall-clock timestamp appears older, the observer watches `(serverHeartbeat, serverToken)` on its **local monotonic clock** for `min(3 * serverInterval, 2s)`; if either word advances, the acceptor is alive.

### 7.2 Opening the Listener Segment

A client opens the listener segment read-write without creating or initializing it:

1. If the name does not exist, return transient `ErrNotExist`. If the backing file size is `0`, return transient `errSegmentNotReady`. If smaller than 4096 bytes, fail permanently with `ErrIncompatibleLayout`.
2. Map the 4096-byte header page and inspect it:
   - `state == Empty` with all 4096 bytes zero, or `state == Initializing` with a compatible header, returns transient `errListenerNotReady`.
   - `state == Empty` with any non-zero header word, `magic != HQQ_LSN1`, `major != 1`, unknown `requiredFeatures`, `ringOffset != 4096`, `recordSize != 248`, invalid `capacity`, or `layoutSize` mismatch fails permanently with `ErrIncompatibleLayout`.
3. Map the full `layoutSize` bytes and re-verify the header words.
4. Verify that the acceptor is alive (section 7.1); if dead or closed, return transient `errListenerDead`.
5. Attach to the connect ring at offset `4096` (`mpmc.Attach`), read `prefix` (`1 <= prefixLen <= 32`), and snapshot `(creatorToken, epoch, serverToken, serverHeartbeat)` for the connection watch (section 7.4).

### 7.3 Executing `Connect`

1. Validate that the requested `bufferCount` and `bufferSize` are valid link geometries and satisfy `bufferCount <= maxBufferCount`, `bufferSize <= maxBufferSize`, and `SizeStandardLink <= maxLinkSize` (otherwise return `ErrInvalidSize` immediately).
2. Create a new link segment as Primary under `prefix + <16 random lowercase hex digits>`, retrying up to 8 times with a fresh random suffix if `ErrLinkBusy` occurs.
3. Construct the 248-byte connect record (`clientToken = link.creatorToken`, `bufferCount`, `bufferSize`, `features`, `flags = 0`, `nameLen`, `clientPID`, `reserved = 0`, `name`) and publish it into the listener's connect ring while monitoring the listener watch (section 7.4).
4. Queue `OpProtoNegotiate` as Primary on the new link's TX ring and wait for the acceptor to attach (`endpoints[1].token != 0`) and reply with `OpProtoAck`, bounded by the caller's deadline, `AttachTimeout` (default `5s`), and the listener watch.
5. If any step fails, close and unlink the client's link segment before returning the error.

### 7.4 Client Watch Conditions

While waiting for connect-ring space or waiting for the acceptor to attach to the client's link segment, the client polls the listener header at least every `50 ms`:

| Observed Listener Condition | Returned Error |
| --- | --- |
| `state != Ready`, or `creatorToken` / `epoch` changed from the initial snapshot | `ErrListenerGone` |
| `serverToken` is `0` or `1` | `ErrListenerClosed` |
| `serverToken` changed to a different non-zero token | `ErrListenerGone` |
| `serverInterval != 0` and `serverHeartbeat` has not advanced for `max(4 * serverInterval, PeerTimeout)` on the local monotonic clock | `ErrListenerGone` |
| `AttachTimeout` elapsed without the acceptor claiming the link's Secondary slot | `ErrTimeout` |

## 8. Safe Name Ownership and Unlinking

When an acceptor closes (or aborts a failed initialization), it unlinks the POSIX shared-memory name **only** if that name still refers to the segment instance it created:

- On Linux, `fstat` on the open descriptor must match `lstat` of `/dev/shm/<name>` (`st_dev` and `st_ino`) and the mapped `creatorToken` (`@96`) must match the instance token.
- On macOS and FreeBSD, the name is reopened read-only and `creatorToken` (`@96`) on page 0 is compared against the instance token.
- On Windows, kernel mappings disappear automatically when the last handle closes.

Clients apply the exact same ownership check when unlinking their per-client link segments on close.

## 9. Trust and Permission Model

Connect records arrive over shared memory from arbitrary local processes that have write permission to the listener segment:

- **Strict validation and bounded resources**: The acceptor validates every record field before use (section 6.2) and bounds all memory mappings and concurrency via `maxBufferCount`, `maxBufferSize`, `maxLinkSize`, `MaxHandshakes`, `Backlog`, and `HandshakeTimeout`.
- **Instance token binding**: Because the acceptor checks that the target link segment's `name` begins with `prefix` and that its superblock `creatorToken` equals `record.clientToken` both before and after claiming the Secondary slot, a forged record cannot trick the acceptor into attaching to another service's segment.
- **Filesystem / kernel ACLs as the access boundary**: Any local user with write access to the listener segment can enqueue connect records or corrupt the shared ring. Operators `MUST` set segment creation permissions (`Perm`, default `0600`) so only authorized local UIDs can open the listener segment.

## 10. Error Summary

| Error Sentinel | Origin | Condition |
| --- | --- | --- |
| `ErrListenerBusy` (matches `ErrLinkBusy`, `ErrFailedInit`) | Acceptor creation | A live acceptor already serves this listener name. |
| `ErrIncompatibleLayout` | Acceptor creation, client open | Object is not a valid `HQQ_LSN1` v1.x listener segment (never modified). |
| `ErrInvalidSize` | Acceptor creation, client connect | Existing listener segment is smaller than `layoutSize`, or client geometry exceeds the listener's limits. |
| `ErrInvalidName` | Both | Segment name violates platform naming rules. |
| `errListenerNotReady`, `errSegmentNotReady`, `errListenerDead`, `ErrNotExist` | Client open | Transient readiness errors retried automatically by `Dial` / `Connect`. |
| `ErrListenerClosed` (matches `ErrPeerClosed`, `io.EOF`) | Waiting client | Acceptor closed the listener gracefully (`serverToken == 1`). |
| `ErrListenerGone` (matches `ErrPeerGone`) | Waiting client | Acceptor crashed, stopped heartbeating, or the listener was re-initialized. |
| `ErrTimeout` | Waiting client | Acceptor did not attach within `AttachTimeout` (also returned if the connect record was rejected). |
| `ErrAcceptorClosed` (matches `ErrClosed`, `net.ErrClosed`) | Acceptor `Accept` | Acceptor was closed locally. |

## 11. Restart Semantics

- **Graceful acceptor restart**: `Close()` marks `serverToken = 1` and unlinks the listener name. Waiting clients receive `ErrListenerClosed`, and subsequent `Connect` calls retry on `ErrNotExist` until the replacement acceptor finishes initializing.
- **Crash recovery**: If an acceptor crashes (`SIGKILL`), the replacement acceptor waits for the stale heartbeat confirmation window (`max(PeerTimeout, 4 * serverInterval)` plus confirmation watch), transitions the segment through `Ready -> Initializing -> Ready` with a new `creatorToken` and `epoch + 1`, and formats a clean ring. Clients waiting on the old epoch observe `ErrListenerGone` and can immediately reconnect.

## 12. Default Configuration Parameters

| Parameter | Default Value |
| --- | --- |
| Connect ring `capacity` (`QueueSize`) | `64` slots |
| Acceptor `HeartbeatInterval` | `250 ms` |
| `PeerTimeout` | `5 s` |
| `AttachTimeout` | `5 s` |
| Acceptor `HandshakeTimeout` | `5 s` |
| Acceptor `StuckSlotTimeout` | `2 s` |
| `MaxHandshakes` (concurrent handshakes in flight) | `16` |
| `Backlog` (negotiated links awaiting `Accept`) | `16` |
| `maxBufferCount` / `maxBufferSize` / `maxLinkSize` | `4096` / `1 MiB` / `256 MiB` |
| Client watch polling interval | `10 ms` (required `<= 50 ms`) |

## 13. Notes for Implementers

- **Exact 248-byte connect record**: Seven 8-byte `uint64` fields (`56` bytes), one 8-byte `reserved` word (`64` bytes total), and `uint8_t name[184]` sum to `248` bytes with natural 8-byte alignment and zero internal compiler padding; together with the 8-byte slot `seq` word, each slot is `256` bytes (`stride = 256`).
- **Fixed superblock identity offsets**: Never alter offsets `8` (`state`) or `96` (`creatorToken`), as the shared safe-unlink routine inspects those offsets across both `HQQ_STL3` and `HQQ_LSN1` segments.
- **Atomic reads after mapping**: Read all header words via atomic loads after mapping the segment, and validate every field copied out of shared memory before acting on it.
