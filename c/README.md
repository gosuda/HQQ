# libhqq (C reference library)

C11 implementation of the HQQ **Standard** link: a named or caller-owned shared
memory segment with two endpoints (Primary and Secondary), two one-direction
rings and payload pools, and whole-message `hqq_write` / `hqq_read`. It
interoperates bit for bit with the Go implementation (segment layout
`HQQ_STL3`, ring layout 2, protocol 1.0 Standard frames).

The normative description is [`spec/SPEC.md`](../spec/SPEC.md) with
[`spec/MEMORY_MODEL.md`](../spec/MEMORY_MODEL.md) (memory orders),
[`spec/PORTING.md`](../spec/PORTING.md) and [`spec/FINDINGS.md`](../spec/FINDINGS.md).

Scope: the Standard layer (links, rings, superblock engine) and the Advanced layer
(negotiation, streams, pooled messages, call/serve with cancel and deadlines,
shutdown) plus the multi-client acceptor (`hqq_connect`, `hqq_acceptor_*`); see
`include/hqq/hqq_advanced.h`.

## Build and test

```sh
cmake -S c -B build -G Ninja -DCMAKE_BUILD_TYPE=Release -DHQQ_WERROR=ON
cmake --build build
ctest --test-dir build --output-on-failure
```

Options (`-D...`):

| Option | Default | Meaning |
| --- | --- | --- |
| `HQQ_WERROR` | OFF | warnings are errors (`-Wall -Wextra -Wpedantic -Werror`, `/W4 /WX`) |
| `HQQ_SANITIZE` | none | `address`, `undefined`, `address+undefined` or `thread` |
| `HQQ_BUILD_TESTS` | ON at top level | unit, fork and stress tests, and `tools/` |
| `HQQ_BUILD_CXX_CHECK` | ON | C++17 consumer compile check of the public header |
| `HQQ_BUILD_STRESS` | ON | bounded weak-memory litmus harness (`verify/stress`) |
| `HQQ_BUILD_SHARED` | OFF | build `libhqq` as a shared library |
| `HQQ_DARWIN_NO_PRIVATE_ULOCK` | OFF | macOS: never call the private `__ulock_*` interfaces |

Generated test constants: `c/tests/generated/spec_constants.h` is produced from
`spec/constants.json` by `python3 c/tools/gen_constants.py` (stdlib only).
`python3 c/tools/gen_constants.py --check` fails if the committed header is stale;
it runs as the `constants_fresh` test.

## Public API (`include/hqq/hqq.h`)

```c
#include "hqq/hqq.h"

hqq_options o;  hqq_options_init(&o);       /* all defaults; set fields as needed */
o.wait_policy = HQQ_WAIT_ADAPTIVE;           /* or HQQ_WAIT_SPIN, HQQ_WAIT_SIMPLE */

hqq_link *l = NULL;
hqq_status st = hqq_standard_create("my-link", 8, 4096, 0600, &o, &l, NULL);
/* or: hqq_standard_open("my-link", 8, 4096, &o, &l, NULL) in the other process */

hqq_write(l, data, len, NULL);               /* one message, 1..4096 bytes */
size_t n;
hqq_read(l, buf, sizeof buf, &n, NULL);      /* HQQ_ERR_BUFFER_TOO_SMALL: n = needed size */
hqq_peer_state(l);                           /* connected / closed / gone */
hqq_close(l);                                /* removes the name if this is its creator */
```

* Caller-owned memory: `hqq_standard_open_memory(mem, size, count, bsize, ...)` with
  a page-aligned `mem` of `hqq_standard_size(count, bsize)` bytes (typically `MAP_SHARED`).
* Per call, `hqq_wait` selects the deadline (`timeout_ns`: `< 0` none, `0` never
  block, `> 0` relative) and an optional `hqq_cancel` token.
* Zero-copy: `hqq_write_zc`, `hqq_read_zc`; reservations: `hqq_write_reserve` /
  `hqq_write_commit` / `hqq_write_abort`, `hqq_read_reserve` / `hqq_read_release`
  (a copied reservation completes at most once; a stale copy returns `HQQ_ERR_INVALID_OP`).
* Errors are `hqq_status` values mapped 1:1 to the Go sentinels; `hqq_error_is()`
  reproduces the Go "wraps" relation. Geometry mismatches fill `hqq_open_info`.
* No global state; the data path (`hqq_write`, `hqq_read`, reservations, waits)
  does not allocate. A custom allocator may be passed in `hqq_options`.

### Wait policies

* `HQQ_WAIT_ADAPTIVE` (default): spin, yield, then park on a kernel wait with a
  100 ms bound (futex on Linux, `os_sync_*` or `__ulock_*` on macOS, named
  semaphores on Windows). Peers in other processes are woken by the waker duty.
* `HQQ_WAIT_SPIN`: never parks, yields periodically.
* `HQQ_WAIT_SIMPLE`: spin, yield, then sleep with exponential backoff (20 us to
  2 ms). Never registers as a waiter, makes no kernel wait, and still performs
  the waker duty, so Go peers that park on this side are woken promptly.

## Advanced layer (`include/hqq/hqq_advanced.h`, `include/hqq/hqq_listener.h`)

The Advanced layer is an overlay on a Standard link (same segment, rings and packet
ABI). Its receive direction is owned by a dispatcher thread after negotiation.

* `hqq_adv_create` / `hqq_adv_open` (named) and `hqq_adv_open_memory`: open the link;
  `hqq_adv_negotiate` (role-agnostic, feature bits of protocol 1.1) must succeed before
  messages or calls.
* Streams: `hqq_adv_listen_streams`, `hqq_adv_open_stream`, `hqq_adv_accept_stream`,
  `hqq_stream_send`, `hqq_stream_recv`, `hqq_stream_close`; stream 0 is `hqq_adv_stream0`.
* Calls: `hqq_stream_call` / `hqq_stream_call_with` (caller and trace metadata);
  `hqq_adv_serve` runs a handler callback `(ctx, response, request)`: on the link's receive thread while no handler runs (no thread hand-off), on worker threads while handlers overlap, and a handler that holds the receive thread for 200 µs is escalated to a new receive thread (the model of the Go `Serve`);
  `hqq_adv_shutdown` refuses new requests with status 8 and waits for in-flight ones.
* Messages are pooled: `hqq_msg_data`, `hqq_msg_len`, `hqq_msg_release`. An error
  response returns `HQQ_ERR_RESPONSE` with the message; `hqq_msg_status` gives its status.
* Acceptor: `hqq_acceptor_create` / `hqq_acceptor_accept` / `hqq_acceptor_close`;
  clients use `hqq_connect` (`ext/LISTENER_DRAFT.md`).

Thread safety: every function may be called from any thread; `hqq_adv_close` waits for
calls in progress and must not be called from a handler. Messages and request contexts
are owned by one caller at a time.

Memory: the data path (send, receive, call, serve) takes buffers from per-link size-class
pools. After warm-up it makes no allocator calls; `test_adv` checks this with a counting
allocator hook.

Verified: `test_adv` (negotiation, chunking, streams, calls, steady-state allocation),
`test_adv_uninit` (concurrent budget-less calls on an allocator that poisons every block),
`test_listener` (round trips, busy listener, bad records, closed acceptor), and the
interop matrix (`interop/`, scenarios X-A01..X-A10, all four Go/C pairings).

## Parent lifeline (`include/hqq/hqq_lifeline.h`)

`hqq_lifeline_start` watches a descriptor (default: standard input) on a helper thread.
A supervisor keeps the write end of a pipe; when it dies, for any reason including
SIGKILL, the read end reaches end-of-file and the callback fires once with
`HQQ_LIFELINE_REASON_EOF` (other read errors give `HQQ_LIFELINE_REASON_ERROR`). Bytes
received are discarded, so keepalives are allowed. Terminals are not watched unless
`force` is set. With `grace_ms`, the process exits with `HQQ_LIFELINE_EXIT_CODE` if
`hqq_lifeline_stop` does not follow the callback within that time.

* The callback runs on the helper thread. It must only request an orderly shutdown (set a
  flag, write a self-pipe, or `pthread_kill` the thread that waits), and it must not call
  `hqq_lifeline_stop`.
* `hqq_lifeline_stop` detaches without firing and joins the thread. POSIX wakes the thread
  through a private self-pipe (poll, no polling loop). Windows sets a flag and calls
  `CancelSynchronousIo` until the blocked `ReadFile` returns.
* The server uses it for `--parent-stdin` (`examples/sqlite/server/README.md`).

Verified on POSIX by `test_lifeline`. The Windows backend is compiled and run only in CI.

## RPC runtime for generated servers (`include/hqq/hqq_rpc.h`)

`hqq_rpc.h` is the runtime of the C servers that `hqq-bindgen` generates (design: `spec/toolchain/BINDGEN.md`, overview: `bindgen/README.md`). It serves one session per Standard link: it validates every frame, decodes arguments in place from the request slot, keeps the handle table of the session (generations, creation order, children destroyed first on teardown), runs interceptors and the library call under the execution lock of the server, and encodes the response in place into the response slot. `hqqrpc_main` is the process entry of a generated server (command line, `ready <listener>` line, `--parent-stdin`, SIGTERM and SIGINT). Every ring wait uses a cancel token and a bound, so a stalled client cannot hold a session thread. The runtime uses `hqq_plat_mutex_*` and `hqq_plat_thread_detach` from `hqq_platform.h`; on Windows `hqqrpc_main` returns `HQQ_ERR_UNSUPPORTED`.

Sources: `src/hqq_rpc.c` (readers, writers, scratch, handle table, execution lock), `src/hqq_rpc_session.c` (frame receive and validation, assembly, response reservation, teardown, the link-less test entry `hqqrpc_test_exchange`), `src/hqq_rpc_server.c` (bootstrap, session threads, shutdown, `hqqrpc_main`), `src/hqq_rpc_internal.h`. They are in the `HQQ_SOURCES` block, so the Rust build compiles them (portable C11, feature-test macros first).

## Platforms

| Platform | Named segment | Parking |
| --- | --- | --- |
| Linux | `/dev/shm/<name>`, regular file checks, `fallocate` | `futex` (shared, no private flag) |
| macOS | `shm_open("/<name>")`, 30-byte names | `os_sync_wait_on_address` (SHARED) when present, else `__ulock_*` (as Go does) |
| Windows | file mapping plus four named semaphores, DACL from `perm` | named semaphores (`WaitOnAddress` for unnamed memory) |
| other POSIX (FreeBSD, ...) | `shm_open` | simple mode |

The library requires always-lock-free 32- and 64-bit C11 atomics, a little-endian
host, and a 64-bit address space that can map the segment (see SPEC 13.3). Big-endian
and 32-bit MIPS/ARMv5-6 targets are rejected at compile time.

## Layout of the sources

| Path | Content |
| --- | --- |
| `include/hqq/hqq.h` | the public API (C11 and C++17, `extern "C"`) |
| `src/hqq_ring.h` | ring core: slot protocol, cursors, waker and waiter, OS independent (only `<stdatomic.h>`; the kernel waits are function pointers) |
| `src/hqq_layout.h` | segment layout, superblock and endpoint records, packets |
| `src/hqq_superblock.c` | claim loop, initialization, publish, liveness at open (SPEC 5, 6.2) |
| `src/hqq_link.c` | the link: peer monitor, heartbeat, reads, writes, reservations, close (SPEC 6, 7, 8, 9) |
| `src/hqq_named.c`, `src/hqq_names.c` | named segments, safe unlink, name validation (SPEC 11) |
| `src/hqq_platform_posix.c`, `src/hqq_platform_win32.c` | the operating-system layer |
| `src/hqq_lifeline.c` | parent lifeline: a helper thread that watches stdin (or a descriptor or handle) and fires once on EOF or a read error (`include/hqq/hqq_lifeline.h`); POSIX poll/self-pipe, Windows ReadFile/CancelSynchronousIo |
| `tests/` | dependency-free tests (`test_*.c`), golden vectors from `spec/constants.json` |
| `tools/hqq_echo.c` | interop CLI (create or open a named link, echo or client mode) |
| `tools/hqq_adv_echo.c` | Advanced-link echo server (creates the link as Primary, negotiates every feature, runs `hqq_adv_serve` with an echo handler); `BenchmarkAdvancedCallCServer` in `interop` measures a Go client against it (`HQQ_ADV_ECHO=<path>`) |
| `verify/` | memory-model tooling (litmus, GenMC); not built by the library |

## Tests

| Test | What it checks |
| --- | --- |
| `constants` | every offset, size, magic, opcode, error code, timing and platform limit against `spec/constants.json`; layout, packet and name vectors |
| `ring` | format and attach rules, slot protocol, full and empty, FIFO, laps, wraparound at 2^64-2, copied reservations, 3-producer / 3-consumer MPMC exactly-once |
| `superblock` | claim decision table: foreign and unknown states, layout incompatibility, joins, live and dead endpoints, reuse, takeover with progress watching, publish ownership, opener never writes |
| `link` | caller-owned memory: messages, too-small reads, zero-copy, reservations and tombstones, deadlines, cancellation, peer close with drain, peer replaced, peer stalled, parked wake-ups |
| `simple` | the three wait policies deliver, time out and cancel |
| `named` (POSIX) | name validation, create/open rules, transient errors, foreign and undersized objects, safe unlink, reuse after death |
| `fork` (POSIX) | two processes: echo including full-size messages, opener first, crash detection with drain, graceful close |
| `lifeline` (POSIX) | parent lifeline: EOF fires once and the child exits, keepalives are drained, a pseudo-terminal is not watched unless forced, stop does not fire, a bad descriptor fires ERROR, start/stop safety, forced exit after the grace period and a stop in time, and SIGKILL of the supervisor (the service exits within about 1 s) |
| `stress` | bounded threads on one link, rings of 2 and 256 slots, both directions, exactly-once and checksums |
| `cxx17`, `smoke` | the public header compiles as C++17 and links; version and atomics self-test |
| `platform_mutex` | the platform mutex: mutual exclusion under contention, destroy rules |
| `rpc` | every vector of the design document byte for byte through a copy of the generated microlib glue, HELLO rules, every frame-rule violation, request ids and wrap, the reader and writer functions at their boundaries |
| `rpc_calls` | in-place and scratch bodies at the frame boundary, response chunking, exactly one frame and no tombstone on every failure after the response was reserved, status filter, handle rollback |
| `rpc_handles` | handle ids, generation wrap, limits, failed destroy, `@retain_on_fail`, cascade, teardown order, a randomized model comparison |
| `rpc_fuzz`, `fuzz_rpc_frame_smoke`, `fuzz_rpc_writer_smoke` | deterministic mutation of the frame receiver and the body decoder: well-formed output, nothing rejected before the call reaches the library, every handle destroyed |
| `rpc_live` (POSIX) | a real session over shared memory: bootstrap, pipelining, oversize bodies, violations, peer death by SIGKILL, write stall, idle timeout, control-link and geometry limits, shutdown with a stuck call |
| `rpc_main` (POSIX) | `hqqrpc_main`: usage, exit codes, ready line, signals, `--parent-stdin` |
| `adv_close_queued` | closing an Advanced link that holds queued, unserved requests releases each request once (regression test of a double release in `hqq_adv_close`) |
| `adv_serve` | `hqq_adv_serve`: sequential requests run inline without a thread hand-off, a handler that waits for a later request escalates and does not deadlock, a handler that calls the peer on the same link completes, cancel and deadline reach an inline handler, `Serve` end, shutdown and close wait for running handlers, concurrent handlers, `Serve` restart |

## Interoperability with the Go implementation

Checked by hand on macOS (arm64) with a throwaway Go driver that used the public
Go API (`CreateStandardLink`, `OpenStandardLink`, `Read`, `Write`, `Close`):

* C creator + Go opener, and Go creator + C opener, 60 round trips each with
  4096-byte buffers (full-size messages every tenth message), and 200 round trips
  with buffers of 8 and 64 bytes on a ring of two slots. All echoes intact; each side
  saw the other's graceful close (Go: `io.EOF`; C: `HQQ_ERR_PEER_CLOSED`).
* The peer crashes without `Close`: a Go opener that died after one echo was reported
  by C as `HQQ_ERR_PEER_GONE` 1 s later (peer timeout 1 s); a C opener killed with
  `SIGKILL` was reported by the Go creator as `ErrPeerGone` 1.4 s later.
* A name removed by the C creator is `ErrNotExist` for Go (`os.ErrNotExist`).
* Parked waits across implementations (macOS arm64, a 300 ms echo delay, steady-state
  round trip minus the delay): a parked C waiter woken by a Go waker took 0.9 ms (`os_sync`)
  and 0.6 ms (`ulock`); a parked Go waiter woken by a C waker took 5.6 ms (`os_sync`) and
  4.2 ms (`ulock`); every C-to-C pairing of the two primitives took 5 to 7 ms. No wake was
  lost: a lost wake would show as about 100 ms, the park bound.

## Known limits

* The Windows layer is compiled in CI only (no local Windows toolchain); it was reviewed
  by reading, not executed.
* ThreadSanitizer runs in CI; locally only ASan+UBSan were run.
* A link whose endpoint record has been used cannot be joined again in the same
  session (SPEC F-15); reconnection needs a new segment.
