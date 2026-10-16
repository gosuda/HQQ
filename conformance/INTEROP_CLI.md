# `hqq-interop`: the interoperability helper contract

Status: normative for every helper executable (Go, C, and later Rust, Zig, C++). A helper is a program named `hqq-interop` (`hqq-interop.exe` on Windows) that runs **one role of one scenario in one process**. The driver (`interop/`, Go test package) starts one process per role and checks the JSON results. The Go helper is `cmd/hqq-interop`; its code is the reference for every rule below. Where this document and the Go code disagree, this document is the bug to fix first (report it), then the code.

Scope of the contract: what the helper prints, how it exits, how processes synchronize, what is sent on the wire and in shared memory. Nothing in this document constrains the helper's internal design.

Normative keywords follow RFC 2119 (`MUST`, `SHOULD`, `MAY`).

## 1. Subcommands

| Command | Purpose |
| --- | --- |
| `hqq-interop run [flags]` | Run one role of one scenario (section 2). |
| `hqq-interop list [--impl LABEL]` | Print the catalogue of scenarios this helper implements, as JSON (section 7). The driver runs a scenario on a pair only if both helpers list it. |
| `hqq-interop gen-vectors [--out DIR]` | Regenerate the golden vectors under `DIR` (Go helper only; other helpers MAY omit it). See `conformance/README.md`. |
| `hqq-interop version` | Print a JSON object with `helper`, `impl`, `protocol`, `segment_layout`, `ring_layout`. |

Stdout carries machine output only. Diagnostics go to stderr.

## 2. `run` flags

Long flags use `--name value` or `--name=value`. Unknown flags are a usage error (exit 2).

| Flag | Default | Meaning |
| --- | --- | --- |
| `--scenario ID` | required | Scenario ID as listed by `list`, for example `X-01` or `X-A04`. |
| `--variant NAME` | first variant | Variant name from the catalogue (`""` means the first). |
| `--step N` | `1` | 1-based step of the scenario (see section 7). |
| `--role ROLE` | required | `creator` (Primary, named create), `opener` (Secondary, named open). `a` is a synonym of `creator`, `b` of `opener`. |
| `--name NAME` | required | Segment name. The driver chooses it; at most 30 bytes, `[A-Za-z0-9_-]`, unique per run. |
| `--sync-dir DIR` | required for multi-role steps | Rendezvous directory shared by the processes of one run (section 6). |
| `--impl LABEL` | `go` | Label of this helper in the result (`go`, `c`, ...). |
| `--peer-impl LABEL` | empty | Label of the peer helper in the result. Informational. |
| `--buffer-count N` | `4` | Slots per direction; power of two. |
| `--buffer-size N` | `256` | Payload bytes per slot; multiple of 8. |
| `--size N` | `buffer-size` | Message payload size. |
| `--messages N` | `100` | Number of messages (scenario-specific meaning). |
| `--seed U64` | `1` | Seed of the deterministic payload generator (section 5). |
| `--timeout SECONDS` | `20` | Hard deadline of this process, in seconds (decimal allowed). The helper exits 3 when it passes. |
| `--crash-after POINT` | empty | Crash point: the process SIGKILLs itself at that point (section 8). |
| `--heartbeat MS` | `0` | Heartbeat interval in ms. `0` = library default (250), negative = heartbeats disabled. |
| `--peer-timeout MS` | `0` | Peer timeout in ms. `0` = library default (5000). |
| `--attach-timeout MS` | `0` | Attach timeout in ms. `0` = library default (5000). |
| `--wait adaptive\|spin` | `adaptive` | Wait policy. `spin` never parks (simple-mode behavior). |
| `--long` | off | Use the long soak sizes of scenarios that have them (driver sets it with `HQQ_INTEROP_LONG=1`). |
| `--max-message N` | `0` | Advanced `MaxMessageSize` in bytes. `0` = library default (64 MiB). Params key `max_message`. |
| `--max-reassembly N` | `0` | Advanced `MaxReassemblyBytes`. `0` = `4 x MaxMessageSize`. Params key `max_reassembly`. |
| `--queue-size N` | `0` | Advanced `IncomingQueueSize`. `0` = `buffer-count`. Params key `queue_size`. |

Variant `params` in the catalogue (section 7) are applied by the driver as the flags above, so the helper's defaults only matter when a caller runs the helper by hand.

## 3. Exit codes

| Code | Meaning | Result line |
| ---: | --- | --- |
| 0 | Role passed (`ok=true`). | required |
| 1 | Role failed: a check failed or an unexpected error occurred. | required, `ok=false` |
| 2 | Usage error: bad flag, unknown scenario/variant/step, missing `--sync-dir`. | none required (message on stderr) |
| 3 | Process deadline passed (`--timeout`). | required, `error_kind="HarnessTimeout"` |
| 4 | Skipped: the scenario does not apply here (platform, or the helper lacks a feature it is asked for). | required, `skipped=true`, `ok=true` |
| 5 | Internal helper error (bug, out of memory). | optional |
| killed | Self-injected crash (section 8): the process dies by SIGKILL (TerminateProcess on Windows). | the result line was printed **before** the kill, with `crashed=true`, `ok=true` |

A helper MUST print the result line before it exits with 0, 1, 3 or 4. It MUST flush stdout before a self-injected crash.

## 4. Result line

Exactly one JSON object per process, on one line, last thing on stdout. Field names are fixed:

```json
{"scenario":"X-01","variant":"","step":1,"role":"creator","impl":"go","peer_impl":"c",
 "ok":true,"skipped":false,"crashed":false,
 "error_kind":"","error":"",
 "checks":[{"name":"opener sees the message","ok":true,"detail":""}],
 "sent":1,"received":1,"elapsed_ms":42,"meta":{}}
```

* `ok` is true only when every check passed and no error occurred.
* `checks` is the list of named assertions the role made; an empty list is allowed.
* `error_kind` is empty on success. On failure it is the canonical name below. `error` is free text (diagnostic only; drivers never match on it).
* `sent`/`received` count messages of the scenario in this role (meaning per scenario).
* `meta` is optional and diagnostic (for example `{"epoch":2,"creator_token":"0x..."}`).

### 4.1 Canonical `error_kind` names

These are the names of SPEC section 12 (Go sentinels). A helper MUST report the **most specific** category that matches the error it received. The Go mapping is `cmd/hqq-interop/errors.go`; a port maps its own error codes to these names.

| `error_kind` | Condition (SPEC 12) |
| --- | --- |
| `EOF` | graceful peer close, buffered messages consumed (`io.EOF`) |
| `ErrPeerClosed` | peer detached gracefully (matches `EOF`) |
| `ErrPeerGone` | peer stopped heartbeating, was replaced, or the segment was re-initialized |
| `ErrClosed` | local link or stream closed |
| `ErrTimeout` | deadline passed |
| `ErrLinkBusy` | live endpoint present (matches `ErrFailedInit`) |
| `ErrIncompatibleLayout` | foreign magic, other layout major, unknown required feature, unknown state, non-zero page of an Empty segment, ring header mismatch (matches `ErrFailedInit`) |
| `ErrFailedInit` | initialization failures, including the transient ones (`errSegmentNotReady`, `errInitContended`, `errNoLiveEndpoint`) |
| `ErrNotExist` | the name does not exist |
| `ErrInvalidSize` | invalid geometry, too-small existing object, geometry mismatch (`GeometryError`), invalid `DataSize` or chunk bounds |
| `ErrInvalidName` | bad name (on Linux also a non-regular file) |
| `ErrBufferOverflow` | payload larger than `bufferSize` (Standard) or `MaxMessageSize` (Advanced) |
| `ErrInvalidOp` | base of protocol-state errors |
| `ErrNotNegotiated`, `ErrAlreadyNegotiated`, `ErrNegotiationFailed`, `ErrNegotiationRejected`, `ErrUnexpectedPacket`, `ErrProtocol` | negotiation and protocol-state violations (match `ErrInvalidOp`) |
| `ErrMessageAborted` | the peer abandoned a response mid-transfer (matches `ErrProtocol`) |
| `ErrIncomingOverflow`, `ErrReassemblyLimit` | queue overflow under `OverflowFail`; response refused by the reassembly limit (match `ErrBufferOverflow`) |
| `ErrConnNotFound` | stream or connection not found |
| `ErrMemoryAlign`, `ErrMemorySmall` | caller-owned memory not aligned / too small |
| `ErrListenerBusy`, `ErrListenerGone`, `ErrListenerClosed`, `ErrAcceptorClosed`, `ErrNotListening`, `ErrAlreadyListening` | acceptor and listener states (`LISTENER_DRAFT.md`) |
| `ErrShuttingDown` | the peer refused a request during graceful shutdown (status 8) |
| `ResponseError` | an application error response (`ResponseError{Status, Payload}`; the status is in `meta.status`) |
| `CheckFailed` | an assertion of the scenario failed (the check is listed in `checks`) |
| `HarnessTimeout` | exit 3 (section 3) |
| `Unknown` | anything else; a helper that prints it is reporting a gap in its mapping |

The C helper's error codes map to the same names: `ErrCodeInvalidSize` is `ErrInvalidSize` on the wire, `ErrCodeConnNotFound` is `ErrConnNotFound`, and an unknown code is `ErrProtocol` (SPEC 12).

## 5. Deterministic payloads

Both sides of a scenario generate and verify message bodies from `(seed, seq, size)`. The algorithm is exact; `conformance.Payload` and `conformance.VerifyPayload` are its Go reference, and `conformance/vectors/manifest.json` carries golden outputs.

Let `M = 2^64`. SplitMix64 stream for `(seed, seq)`:

```text
s  = (seed XOR (seq * 0xD1B54A32D192ED03)) mod M
next():
  s = (s + 0x9E3779B97F4A7C15) mod M
  z = s
  z = ((z XOR (z >> 30)) * 0xBF58476D1CE4E5B9) mod M
  z = ((z XOR (z >> 27)) * 0x94D049BB133111EB) mod M
  return z XOR (z >> 31)
body(n) = the first n bytes of the concatenation of next() words, each written little-endian
```

Payload of `size` bytes:

* `size == 0`: empty.
* `1 <= size <= 15`: `body(size)`.
* `size >= 16`: `seq` as u64 LE (8 bytes) | `crc32-IEEE(body)` as u32 LE (4 bytes) | `body(size - 12)`.

Golden values (seed 1, seq 0): `size 1` = `c1`; `size 8` = `c15c0289ec2d0a91`; `size 16` = `0000000000000000 add8412c c15c0289`. Seed `0xdeadbeef`, seq 7, size 40 = `0700000000000000 47a0b112 7536cc3826b6b2d2c8375c13ebdfc0ab9ca7d35f20744cb4782227b3`. (Both the Go and an independent Python implementation produce these bytes.)

A receiver verifies a message of index `i` (the i-th message of the scenario's stream, 0-based) by checking `payload == Payload(seed, i, len)`. Scenarios that send several streams use a distinct seed per stream (`seed + stream-index`, documented per scenario).

## 6. Rendezvous and synchronization

The processes of one run share (a) the segment named by `--name` and (b) the directory `--sync-dir`. The driver creates the directory per run and removes it afterwards.

**Barrier.** `barrier(phase)` for role `R` with peer role `P`, in step `N`:

1. create the file `<sync-dir>/s<N>-<phase>.<R>` if it does not exist (empty; creating it again is harmless);
2. poll every 2 ms until `<sync-dir>/s<N>-<phase>.<P>` exists;
3. fail with `ErrTimeout` if the process deadline passes first.

Phase names are scenario-defined ASCII identifiers. Barriers are the only inter-process ordering a scenario uses besides the shared segment itself; a scenario that needs an ordering MUST use a barrier, never a sleep alone (sleeps are allowed for timing *assertions*, for example waiting for a heartbeat to age).

**Segment readiness.** A creator's barrier after `CreateStandardLink` (or the Advanced equivalent) is what makes the segment openable. An opener MUST NOT rely on retry alone unless the scenario is about transient errors (`ErrFailedInit`, `ErrNotExist`).

## 7. Catalogue, variants and steps (`list`)

`list` prints:

```json
{"helper":"hqq-interop","impl":"go","protocol":"1.1","layout":"HQQ_STL3","runtime":"...",
 "scenarios":[
  {"id":"X-01","title":"create, open, exchange, close","kind":"standard",
   "platforms":["any"],"timeout_s":30,
   "variants":[
     {"name":"creator-closes-first",
      "params":{"buffer_size":256,"messages":1},
      "steps":[{"roles":["creator","opener"],"expect":{"creator":"ok","opener":"ok"}}]}
   ]}
 ]}
```

* `kind`: `standard` (SPEC 9), `advanced` (SPEC 10 and `ext/PROTOCOL_1_1_DRAFT.md`), `acceptor` (`ext/LISTENER_DRAFT.md`).
* `platforms`: `any`, or GOOS names (`windows`, `linux`, `darwin`). The helper exits 4 elsewhere; the driver does not start it.
* `variants[].params`: the flags to pass (`buffer_count`, `buffer_size`, `size`, `messages`, `seed`, `heartbeat_ms`, `peer_timeout_ms`, `attach_timeout_ms`). Absent keys keep the helper defaults.
* `steps`: executed in order. Each step starts one process per listed role, with the same `--name`, `--sync-dir`, and the params of the variant. `args` (optional, per role) adds flags such as `--crash-after`.
* `expect` per role: `ok` (exit 0 and `ok=true`), `crash` (killed at a crash point, with `crashed=true` printed), `ok|crash` (either), `skip` (exit 4). A step passes when every role meets its expectation.

The driver runs the cross product of: scenarios listed by both helpers, variants, step order, and role assignments (see section 9).

Variant rules: a scenario with more than one variant MUST give each variant a unique, non-empty name (an empty `--variant` selects the first variant, so an empty name would be ambiguous).

## 8. Crash points

`--crash-after POINT` makes the helper kill itself (SIGKILL; TerminateProcess on Windows) when it reaches `POINT`. Before the kill the helper prints its result line with `crashed=true, ok=true` and flushes stdout. Points are scenario-defined, from this vocabulary:

| Point | Meaning |
| --- | --- |
| `msgs:N` | after N messages have been successfully sent or received by this role |
| `blocked` | just before a blocking receive on an empty ring (the process is about to park) |
| `claim` | after an initialization claim (superblock `Empty -> Initializing`, `initToken` set) and before any further write |
| `format` | after the rings were formatted, before publish |
| `publish` | after the superblock stamps and before the commit CAS |
| `zeroing` | while zeroing payload buffers during initialization |
| `exit` | after the scenario's last check, before the link is closed (no graceful close) |

A helper that does not implement a point MUST exit 4 for that step (the driver then reports `skipped`, not a pass). The raw points (`claim`, `format`, `publish`, `zeroing`) are implemented by writing the superblock and ring words directly with the protocol of SPEC 5 and 7, so the helper crashes in a state the public API cannot reach.

The driver never sends a signal to a helper. Victims of a crash are identified by the `crashed` flag of their result line, not by their exit status.

A crash expectation is met only by a process that printed `crashed=true` and then died by signal or with a non-zero status.

## 9. Role assignments

Named scenarios have one creator and one opener. For a pair of helpers `(A, B)` the driver runs, per scenario and variant:

1. `creator=A, opener=B`;
2. `creator=B, opener=A` (skipped when `A == B`, since the two runs are identical).

Scenarios whose steps list only one role run that role once per implementation. Advanced scenarios use the same mapping: the creator is the Primary (it sends `OpProtoNegotiate` first); the opener is the Secondary.

The driver passes `--peer-impl` as the label of the other helper and `--impl` as its own label.

## 10. Implementing the contract in another language

1. Parse the flags of section 2 (unknown flags exit 2).
2. Implement `list` from the catalogue you support (a subset is fine; the driver intersects lists).
3. Implement `run` for each scenario you list: map the segment (SPEC 11), do the role's operations, call barriers (section 6), print one result line (section 4), exit per section 3.
4. Implement `Payload` (section 5) and verify with the golden values before anything else.
5. Map your errors to `error_kind` names (section 4.1).
6. Add your helper's build to `.github/workflows/interop.yml` (see the comment block there) and register the executable path in `HQQ_INTEROP_IMPLS`, as described in "Adding an implementation" in `conformance/README.md`.

## 11. Scenario catalogue

The authoritative catalogue is the output of `hqq-interop list` of the Go helper. Each scenario's pass criteria are the checks its role makes (listed in its result line); the mapping to the PORTING.md matrix IDs is given in the "Ref" column.

<!-- CATALOGUE:BEGIN -->

Condensed from the output of `hqq-interop list` (Go helper), which is authoritative. `c` = creator, `o` = opener. Steps run in order; `(c:ok,o:ok)` is the expectation per role. Variant names are unique and non-empty (section 7).

| ID | Kind | Platforms | Variants (steps: roles, expect) | Ref | Title |
| --- | --- | --- | --- | --- | --- |
| X-01 | standard | any | `creator-closes-first`: c,o (c:ok,o:ok)<br>`opener-closes-first`: c,o (c:ok,o:ok) | X-01 | create, open, exchange, close |
| X-02 | standard | any | `count-double`: c,o (c:ok,o:ok)<br>`size-double`: c,o (c:ok,o:ok)<br>`count-half`: c,o (c:ok,o:ok)<br>`size-half`: c,o (c:ok,o:ok) | X-02 | geometry mismatch (count, size) |
| X-03 | standard | any | `default`: c,o (c:ok,o:ok) | X-03 | third endpoint, re-join after own close, creator vs live segment |
| X-04 | standard | any | `before-creator`: c,o (c:ok,o:ok)<br>`initializing`: c (c:crash); o (o:ok)<br>`dead`: c,o (c:crash,o:ok); o (o:ok) | X-04 | opener before creator, opener on an initializing or dead segment |
| X-05 | standard | any | `ring2`: c,o (c:ok,o:ok) | X-05 | ring full / backpressure, deadline on a full ring |
| X-06 | standard | any | `ring2`: c,o (c:ok,o:ok)<br>`ring256`: c,o (c:ok,o:ok) | X-06 | bidirectional stream, sequence and CRC, ring of 2 and 256 |
| X-07 | standard | any | `creator-killed-running`: c,o (c:crash,o:ok)<br>`opener-killed-parked`: c,o (c:ok,o:crash)<br>`short-peer-timeout`: c,o (c:crash,o:ok) | X-07 | kill -9 of the peer (running and parked), default and short peer timeout |
| X-08 | standard | any | `claim`: c (c:crash); c,o (c:ok,o:ok)<br>`format`: c (c:crash); c,o (c:ok,o:ok)<br>`publish`: c (c:crash); c,o (c:ok,o:ok)<br>`zeroing`: c (c:crash); c,o (c:ok,o:ok) | X-08 | kill -9 of the initializer between claim and publish |
| X-09 | standard | any | `reuse`: c,o (c:crash,o:crash); c,o (c:crash,o:crash); o (o:ok) | X-09 | reuse of a name after both endpoints died (epoch and creatorToken change) |
| X-10 | standard | linux,darwin | `default`: c (c:ok) | X-10 | safe unlink: replaced name survives the creator's close |
| X-11 | standard | any | `default`: c (c:ok) | X-11 | invalid names per platform, long names on macOS |
| X-12 | standard | any | `ring2`: c,o (c:ok,o:ok) | X-12 | parked consumer woken by a producer (ping-pong, ring of 2) |
| X-13 | standard | windows | `semaphores`: c,o (c:ok,o:ok); c,o (c:ok,o:crash); c,o (c:ok,o:ok) | X-13 | Windows named semaphores: ping-pong, parked kill, session reuse |
| X-14 | standard | any | `adaptive`: c,o (c:ok,o:ok)<br>`spin-peer`: c,o (c:ok,o:ok) | X-14 | simple-mode peer: round trips with 300 ms idle |
| X-21 | standard | any | `violations`: c,o (c:ok,o:ok) | X-21 | protocol violations from a crafted peer on the Standard receive path |
| X-A01 | advanced | any | `all-features`, `opener-initiates`, `simultaneous`, `messages-only`, `request-response-only`, `none`, `bad-major`, `op-error`: c,o (c:ok,o:ok) each | X-15 | negotiation, creator- and opener-initiated, subsets, rejection |
| X-A02 | advanced | any | `sizes`: c,o (c:ok,o:ok)<br>`sender-abort-after-chunk-1`: c,o (c:ok,o:ok) | X-16 | message sizes 0 .. MaxMessageSize, local refusal, abort |
| X-A03 | advanced | any | `reassembly-limit`: c,o (c:ok,o:ok) | X-17 | reassembly limit: refused request, refused response, refused data |
| X-A04 | advanced | any | `default`: c,o (c:ok,o:ok) | X-18 | request/response: concurrent calls, error payload, cancelled call, unknown method |
| X-A05 | advanced | any | `basic`, `bad-parity`, `duplicate`: c,o (c:ok,o:ok) each | X-19 | streams: refusal, parity, ids never reused, close semantics |
| X-A06 | advanced | any | `flood`: c,o (c:ok,o:ok)<br>`flood-chunked`: c,o (c:ok,o:ok) | X-20 | flood from both sides on a tiny ring with a slow consumer |
| X-A07 | advanced | any | `cancel`: c,o (c:ok,o:ok)<br>`nocancel`: c,o (c:ok,o:ok) | 1.1 sections 2, 4.2 | cancel: OpCancel ends the handler, unknown key ignored; no FeatureCancel |
| X-A08 | advanced | any | `metadata`: c,o (c:ok,o:ok)<br>`without-metadata`: c,o (c:ok,o:ok) | 1.1 sections 3, 5 | request metadata: caller, trace, time budget; expired deadline sends nothing |
| X-A09 | advanced | any | `default`: c,o (c:ok,o:ok) | 1.1 section 6 | graceful shutdown: refusal with status 8, in-flight request completes |
| X-A10 | acceptor | any | `default`: c,o (c:ok,o:ok) | LISTENER_DRAFT | acceptor: Listen/Dial/Connect, busy listener, acceptor close |

<!-- CATALOGUE:END -->
