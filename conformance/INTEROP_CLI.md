# `hqq-interop`: The Interoperability Helper Contract

**Status**: Normative for all cross-language helper executables (Go, C, C++, and Rust).

An interoperability helper is a standalone executable named `hqq-interop` (`hqq-interop.exe` on Windows) that executes **one role of one scenario in a single OS process**. The Go test driver in [`interop/`](../interop) spawns one helper process per role, coordinates multi-step scenarios via filesystem barriers, and validates the structured JSON result lines emitted by each process. The Go helper in [`cmd/hqq-interop`](../cmd/hqq-interop) serves as the reference implementation; if this document and `cmd/hqq-interop` ever diverge, report and fix the discrepancy in this specification first.

This contract specifies only external behavior: command-line flags, standard output formatting, process exit codes, inter-process barrier synchronization, and wire/shared-memory payloads. Internal architecture and API shape are left entirely to each language implementation.

Normative keywords (`MUST`, `MUST NOT`, `SHOULD`, `MAY`) follow RFC 2119.

## 1. Subcommands

Every `hqq-interop` binary implements the following subcommands:

| Command | Purpose |
| --- | --- |
| `hqq-interop run [flags]` | Execute one role of one scenario step (section 2). |
| `hqq-interop list [--impl LABEL]` | Print the JSON catalogue of scenarios supported by this helper (section 7). The test driver runs a scenario across a pair of helpers only when both list it. |
| `hqq-interop gen-vectors [--out DIR]` | Regenerate the golden conformance vectors under `DIR` (required for the Go helper; other helpers `MAY` omit this subcommand). See [`conformance/README.md`](README.md). |
| `hqq-interop version` | Print a single-line JSON object containing `helper`, `impl`, `protocol`, `segment_layout`, and `ring_layout`. |

Standard output (`stdout`) carries machine-readable JSON only; all human-readable diagnostics and logs `MUST` be written to standard error (`stderr`).

## 2. `run` Flags

Long options accept either `--name value` or `--name=value`. Unrecognized flags `MUST` cause the helper to exit with status `2` (usage error).

| Flag | Default | Meaning |
| --- | --- | --- |
| `--scenario ID` | required | Scenario identifier from `list` (for example `X-01` or `X-A04`). |
| `--variant NAME` | first variant | Variant name from the scenario's catalogue entry (`""` selects the first variant). |
| `--step N` | `1` | 1-based step index within the scenario (section 7). |
| `--role ROLE` | required | `creator` (Primary, named segment creation) or `opener` (Secondary, named segment open). `a` is an alias for `creator`, and `b` is an alias for `opener`. |
| `--name NAME` | required | Shared-memory segment name chosen by the driver (`<= 30` ASCII bytes matching `[A-Za-z0-9_-]`, unique per test run). |
| `--sync-dir DIR` | required for multi-role steps | Shared directory used for inter-process barrier files during a test run (section 6). |
| `--impl LABEL` | `go` | Implementation label echoed in the JSON result line (`go`, `c`, `cpp`, `rust`, etc.). |
| `--peer-impl LABEL` | empty | Implementation label of the peer process echoed in the JSON result line (informational). |
| `--buffer-count N` | `4` | Ring slots per direction (power of two). |
| `--buffer-size N` | `256` | Payload capacity per slot in bytes (multiple of 8). |
| `--size N` | `buffer-size` | Application message payload size in bytes. |
| `--messages N` | `100` | Number of messages to exchange (exact semantics depend on the scenario). |
| `--seed U64` | `1` | 64-bit seed for the deterministic SplitMix64 payload generator (section 5). |
| `--timeout SECONDS` | `20` | Hard per-process execution deadline in seconds (floating-point values permitted). Exceeding this deadline exits with status `3`. |
| `--crash-after POINT` | empty | Self-injected crash point: the helper kills itself with `SIGKILL` / `TerminateProcess` upon reaching `POINT` (section 8). |
| `--heartbeat MS` | `0` | Heartbeat interval in milliseconds (`0` uses the library default of `250`, negative disables heartbeats). |
| `--peer-timeout MS` | `0` | Peer liveness timeout in milliseconds (`0` uses the library default of `5000`). |
| `--attach-timeout MS` | `0` | Segment attach timeout in milliseconds (`0` uses the library default of `5000`). |
| `--wait adaptive\|spin` | `adaptive` | Ring wait policy (`spin` disables OS futex/semaphore parking to test spin-only peers). |
| `--long` | off | Enable extended soak message counts for scenarios that define them (set by the driver when `HQQ_INTEROP_LONG=1`). |
| `--max-message N` | `0` | Advanced `MaxMessageSize` in bytes (`0` uses the library default of 64 MiB; catalogue param key `max_message`). |
| `--max-reassembly N` | `0` | Advanced `MaxReassemblyBytes` (`0` uses `4 * MaxMessageSize`; catalogue param key `max_reassembly`). |
| `--queue-size N` | `0` | Advanced `IncomingQueueSize` (`0` uses `buffer-count`; catalogue param key `queue_size`). |

Because the test driver expands variant `params` from the catalogue (section 7) into explicit command-line flags when invoking `hqq-interop run`, flag defaults only take effect when a developer runs a helper manually or when a variant omits a parameter key.

## 3. Process Exit Codes

A helper process communicates its final disposition through its exit code and its single-line JSON result object:

| Exit Code | Meaning | JSON Result Line Requirement |
| ---: | --- | --- |
| `0` | Role passed all checks (`ok=true`). | Required |
| `1` | Role failed a check or encountered an unexpected error (`ok=false`). | Required (`ok=false`) |
| `2` | Command-line usage error (unknown flag, unknown scenario/variant/step, or missing `--sync-dir`). | None required (diagnostic on `stderr`) |
| `3` | Process execution deadline expired (`--timeout`). | Required (`error_kind="HarnessTimeout"`) |
| `4` | Scenario skipped on this platform or unsupported optional feature. | Required (`skipped=true`, `ok=true`) |
| `5` | Internal helper failure (panic, assertion bug, or out-of-memory). | Optional |
| killed | Self-injected crash (section 8) via `SIGKILL` or `TerminateProcess`. | Required **before** self-kill (`crashed=true`, `ok=true`) |

A helper `MUST` write and flush its JSON result line to `stdout` before exiting with code `0`, `1`, `3`, or `4`, and before triggering a self-injected crash.

## 4. Result Line Format

Each `hqq-interop run` invocation `MUST` emit exactly one single-line JSON object as the final line on `stdout`:

```json
{"scenario":"X-01","variant":"","step":1,"role":"creator","impl":"go","peer_impl":"c","ok":true,"skipped":false,"crashed":false,"error_kind":"","error":"","checks":[{"name":"opener sees the message","ok":true,"detail":""}],"sent":1,"received":1,"elapsed_ms":42,"meta":{}}
```

Field semantics:

- `ok`: `true` if and only if every assertion in `checks` passed and no unexpected error occurred.
- `checks`: Ordered list of named assertions evaluated by this role (`[]` is permitted when the role only serves a peer).
- `error_kind`: Empty string `""` when `ok` is `true`; otherwise set to one of the canonical error identifiers in section 4.1.
- `error`: Free-form human-readable diagnostic string; the driver never matches against this text.
- `sent` and `received`: Number of scenario messages sent and received by this role.
- `meta`: Optional map of diagnostic key-value pairs (such as `{"epoch":2,"creator_token":"0x..."}` or `{"status":42}`).

### 4.1 Canonical `error_kind` Identifiers

When a scenario verifies an error condition (or fails unexpectedly), the helper `MUST` map its native library error to the **most specific** canonical identifier from `SPEC.md` section 12:

| `error_kind` | Condition (`SPEC.md` Section 12) |
| --- | --- |
| `EOF` | Graceful peer close after all buffered messages have been consumed (`io.EOF`). |
| `ErrPeerClosed` | Peer detached gracefully (matches `EOF`). |
| `ErrPeerGone` | Peer stopped heartbeating, was replaced by a new process, or the segment was re-initialized. |
| `ErrClosed` | Local link or stream was already closed. |
| `ErrTimeout` | Operation deadline expired. |
| `ErrLinkBusy` | A live endpoint already occupies the requested slot (matches `ErrFailedInit`). |
| `ErrIncompatibleLayout` | Unrecognized magic word, incompatible layout major version, unknown required feature bit, invalid state value, non-zero page size in an `Empty` segment, or ring header mismatch (matches `ErrFailedInit`). |
| `ErrFailedInit` | Segment initialization failure, including transient contention (`errSegmentNotReady`, `errInitContended`, `errNoLiveEndpoint`). |
| `ErrNotExist` | Named shared-memory segment does not exist. |
| `ErrInvalidSize` | Invalid ring geometry, existing backing file too small, geometry mismatch (`GeometryError`), or invalid `DataSize` / chunk bounds. |
| `ErrInvalidName` | Malformed segment name (or non-regular backing file on Linux). |
| `ErrBufferOverflow` | Payload exceeds `bufferSize` (Standard link) or `MaxMessageSize` (Advanced link). |
| `ErrInvalidOp` | Base category for protocol state violations. |
| `ErrNotNegotiated`, `ErrAlreadyNegotiated`, `ErrNegotiationFailed`, `ErrNegotiationRejected`, `ErrUnexpectedPacket`, `ErrProtocol` | Protocol handshake and frame-ordering violations (subtypes of `ErrInvalidOp`). |
| `ErrMessageAborted` | Peer aborted a multi-packet response mid-transfer (subtype of `ErrProtocol`). |
| `ErrIncomingOverflow`, `ErrReassemblyLimit` | Incoming queue full under `OverflowFail`, or response rejected by `MaxReassemblyBytes` (subtypes of `ErrBufferOverflow`). |
| `ErrConnNotFound` | Target stream or connection ID does not exist. |
| `ErrMemoryAlign`, `ErrMemorySmall` | Caller-supplied memory buffer is insufficiently aligned or smaller than required. |
| `ErrListenerBusy`, `ErrListenerGone`, `ErrListenerClosed`, `ErrAcceptorClosed`, `ErrNotListening`, `ErrAlreadyListening` | Multi-client acceptor and listener errors (`spec/ext/LISTENER_DRAFT.md`). |
| `ErrShuttingDown` | Peer rejected a new request during graceful shutdown (Protocol 1.1 status `8`). |
| `ResponseError` | Application-level RPC error response (`ResponseError{Status, Payload}`, with the numeric status recorded in `meta.status`). |
| `CheckFailed` | A scenario assertion failed (details recorded in `checks`). |
| `HarnessTimeout` | Per-process `--timeout` watchdog expired (exit code `3`). |
| `Unknown` | Fallback when an implementation has not mapped an internal error code (indicates a bug in the helper). |

Wire error codes carried in `OpError` packets map to the same canonical names: `ErrCodeInvalidSize` maps to `ErrInvalidSize`, `ErrCodeConnNotFound` maps to `ErrConnNotFound`, and any unrecognized wire error code maps to `ErrProtocol`.

## 5. Deterministic Payload Generation

Both endpoints in a test scenario generate and verify message bodies deterministically from `(seed, seq, size)` without exchanging out-of-band test fixtures. Reference implementations live in [`conformance/payload.go`](payload.go) (`Payload` and `VerifyPayload`), and golden outputs are pinned in `conformance/vectors/manifest.json`.

Given modulus `M = 2^64`, initialize the SplitMix64 stream for `(seed, seq)` as follows:

```text
s  = (seed XOR (seq * 0xD1B54A32D192ED03)) mod M
next():
  s = (s + 0x9E3779B97F4A7C15) mod M
  z = s
  z = ((z XOR (z >> 30)) * 0xBF58476D1CE4E5B9) mod M
  z = ((z XOR (z >> 27)) * 0x94D049BB133111EB) mod M
  return z XOR (z >> 31)
body(n) = the first n bytes of the concatenation of next() words, each serialized in little-endian order
```

Construct a payload of `size` bytes according to three size regimes:

- `size == 0`: Empty byte slice.
- `1 <= size <= 15`: `body(size)`.
- `size >= 16`: `seq` as 8-byte little-endian `uint64` `||` `crc32-IEEE(body(size - 12))` as 4-byte little-endian `uint32` `||` `body(size - 12)`.

Golden verification values:

- `seed = 1, seq = 0, size = 1`: `c1`
- `seed = 1, seq = 0, size = 8`: `c15c0289ec2d0a91`
- `seed = 1, seq = 0, size = 16`: `0000000000000000 add8412c c15c0289`
- `seed = 0xdeadbeef, seq = 7, size = 40`: `0700000000000000 47a0b112 7536cc3826b6b2d2c8375c13ebdfc0ab9ca7d35f20744cb4782227b3`

A receiver validates the `i`-th message (`0`-based) on a stream by checking `payload == Payload(seed, i, len(payload))`. Scenarios that open multiple concurrent streams derive a per-stream seed `seed + stream_index` as documented in the scenario definition.

## 6. Rendezvous and Barrier Synchronization

Processes participating in the same test run share two resources managed by the driver: the shared-memory segment name (`--name`) and a temporary synchronization directory (`--sync-dir`).

To execute `barrier(phase)` in step `N`, a process playing role `R` with peer role `P` performs three steps:

1. Create the empty marker file `<sync-dir>/s<N>-<phase>.<R>` if it does not already exist.
2. Poll every `2 ms` until the peer marker file `<sync-dir>/s<N>-<phase>.<P>` appears.
3. Return `ErrTimeout` if the process deadline expires before the peer marker file appears.

Phase names are scenario-specific ASCII identifiers. Helpers `MUST` use filesystem barriers rather than `sleep` delays whenever one role depends on another role reaching a protocol state (for example, waiting for the creator to finish `CreateStandardLink` before the opener calls `OpenStandardLink`, except in scenarios that explicitly test `ErrNotExist` or `ErrFailedInit`).

## 7. Scenario Catalogue Schema (`list`)

Running `hqq-interop list` prints a single JSON document describing all scenarios and variants implemented by the helper:

```json
{
  "helper": "hqq-interop",
  "impl": "go",
  "protocol": "1.1",
  "layout": "HQQ_STL3",
  "runtime": "go1.27",
  "scenarios": [
    {
      "id": "X-01",
      "title": "create, open, exchange, close",
      "kind": "standard",
      "platforms": ["any"],
      "timeout_s": 30,
      "variants": [
        {
          "name": "creator-closes-first",
          "params": {"buffer_size": 256, "messages": 1},
          "steps": [
            {
              "roles": ["creator", "opener"],
              "expect": {"creator": "ok", "opener": "ok"}
            }
          ]
        }
      ]
    }
  ]
}
```

Catalogue field rules:

- `kind`: One of `"standard"` (`SPEC.md` section 9), `"advanced"` (`SPEC.md` section 10 and `PROTOCOL_1_1_DRAFT.md`), or `"acceptor"` (`LISTENER_DRAFT.md`).
- `platforms`: `["any"]` or a list of `GOOS` identifiers (`"linux"`, `"darwin"`, `"freebsd"`, `"windows"`). Because Windows kernel shared-memory mappings disappear as soon as the last open handle closes (whereas POSIX `shm_open` segments persist until unlinked), scenarios that test attachment to a dead segment after all processes exit list only POSIX platforms.
- `variants[].params`: Per-variant flag overrides (`buffer_count`, `buffer_size`, `size`, `messages`, `seed`, `heartbeat_ms`, `peer_timeout_ms`, `attach_timeout_ms`, `max_message`, `max_reassembly`, `queue_size`). Omitted keys use the helper's default flag values.
- `steps`: Executed sequentially by the driver. Each step spawns one process per role in `roles` with the shared `--name` and `--sync-dir`, plus optional per-role `args` (such as `["--crash-after=claim"]`).
- `expect`: Per-role outcome expectation: `"ok"` (exit `0` with `ok=true`), `"crash"` (killed at a crash point after printing `crashed=true, ok=true`), `"ok|crash"` (either), or `"skip"` (exit `4`).
- **Unique variant names**: Every scenario with more than one variant `MUST` assign each variant a unique, non-empty `name`.

## 8. Self-Injected Crash Points

Passing `--crash-after POINT` instructs the helper to terminate itself abruptly (`SIGKILL` on POSIX, `TerminateProcess(GetCurrentProcess(), 137)` on Windows) the moment execution reaches `POINT`. Immediately before terminating, the helper `MUST` print its JSON result line with `"crashed":true,"ok":true` and flush `stdout`:

| Crash Point | Execution Point |
| --- | --- |
| `msgs:N` | Immediately after this role has sent or received `N` messages. |
| `blocked` | Immediately before parking on an empty ring in a blocking receive call. |
| `claim` | After claiming initialization (`Empty -> Initializing` and setting `initToken`), before formatting rings. |
| `format` | After formatting the ring headers, before publishing superblock geometry. |
| `publish` | After writing superblock geometry stamps, before the final `Initializing -> Ready` CAS commit. |
| `zeroing` | Midway through zeroing payload buffers during segment initialization. |
| `exit` | After completing all scenario assertions, before calling `Close()` on the link (simulating an ungraceful exit). |

If a helper does not implement a requested crash point, it `MUST` exit with code `4` (`skipped=true`) so the driver records a skip rather than a false pass. The mid-initialization crash points (`claim`, `format`, `publish`, `zeroing`) manipulate the mapped superblock and ring words directly according to `SPEC.md` sections 5 and 7 to leave the segment in states unreachable through normal public API calls.

## 9. Role Pairings Across Implementations

For every two-role scenario and variant supported by a pair of helpers `(A, B)`, the test driver executes both directions:

1. `creator = A`, `opener = B`
2. `creator = B`, `opener = A` (omitted when `A == B`, since both runs are identical)

Single-role scenarios run once per registered implementation. In Advanced protocol scenarios, `creator` acts as the Primary endpoint (sending `OpProtoNegotiate` first) and `opener` acts as the Secondary endpoint.

## 10. Checklist for Implementing a New Helper

To bring up an `hqq-interop` helper in a new language or binding:

1. Implement command-line parsing for all flags in section 2, exiting with code `2` on unknown flags.
2. Implement `list` returning the subset of scenarios your helper supports (the driver automatically intersects scenario lists across pairs).
3. Implement `Payload` and `VerifyPayload` from section 5 and verify them against the golden hex values before testing live links.
4. Map your library's error types to the canonical `error_kind` names in section 4.1.
5. Implement `run` for each supported scenario using the file barriers in section 6, emitting the single-line JSON result from section 4 and exiting with the codes in section 3.
6. Register the compiled binary in `.github/workflows/interop.yml` and test locally via `HQQ_INTEROP_IMPLS`.

## 11. Scenario Catalogue

The authoritative scenario list is generated by `hqq-interop list` in the Go reference helper (`cmd/hqq-interop`). Each scenario maps to the conformance test IDs defined in [`spec/PORTING.md`](../spec/PORTING.md).

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
| X-08 | standard | linux, darwin, freebsd | `claim`: c (c:crash); c,o (c:ok,o:ok)<br>`format`: c (c:crash); c,o (c:ok,o:ok)<br>`publish`: c (c:crash); c,o (c:ok,o:ok)<br>`zeroing`: c (c:crash); c,o (c:ok,o:ok) | X-08 | kill -9 of the initializer between claim and publish |
| X-09 | standard | linux, darwin, freebsd | `reuse`: c,o (c:crash,o:crash); c,o (c:crash,o:crash); o (o:ok) | X-09 | reuse of a name after both endpoints died (epoch and creatorToken change) |
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
| X-A05 | advanced | any | `basic`, `bad-parity`, `duplicate`, `held`: c,o (c:ok,o:ok) each | X-19 | streams: refusal, held requests before ListenStreams (SPEC 10.3), parity, ids never reused, close semantics |
| X-A06 | advanced | any | `flood`: c,o (c:ok,o:ok)<br>`flood-chunked`: c,o (c:ok,o:ok) | X-20 | flood from both sides on a tiny ring with a slow consumer |
| X-A07 | advanced | any | `cancel`: c,o (c:ok,o:ok)<br>`nocancel`: c,o (c:ok,o:ok) | 1.1 sections 2, 4.2 | cancel: OpCancel ends the handler, unknown key ignored; no FeatureCancel |
| X-A08 | advanced | any | `metadata`: c,o (c:ok,o:ok)<br>`without-metadata`: c,o (c:ok,o:ok) | 1.1 sections 3, 5 | request metadata: caller, trace, time budget; expired deadline sends nothing |
| X-A09 | advanced | any | `default`: c,o (c:ok,o:ok) | 1.1 section 6 | graceful shutdown: refusal with status 8, in-flight request completes |
| X-A10 | acceptor | any | `default`: c,o (c:ok,o:ok) | LISTENER_DRAFT | acceptor: Listen/Dial/Connect, busy listener, acceptor close |

<!-- CATALOGUE:END -->
