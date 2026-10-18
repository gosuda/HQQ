# `libhqq` (C11 Reference Library)

`libhqq` is the C11 implementation of the HQQ shared-memory IPC stack. It implements the **Standard** layer (named and caller-owned shared-memory segments, `HQQ_STL3` superblock lifecycle, MPMC rings, slot-owned buffers, zero-copy reservations, and adaptive kernel parking), the **Advanced** layer (Protocol 1.0 and 1.1 negotiation, multiplexed streams, pooled multi-chunk messages, concurrent `call`/`serve` RPC with cancellation and deadlines, and graceful shutdown), the multi-client **Acceptor / Listener** (`hqq_connect`, `hqq_acceptor_*`), the **Parent Lifeline** (`hqq_lifeline.h`), and the **`hqq-bindgen` C RPC runtime** (`hqq_rpc.h`). It interoperates bit-for-bit with the Go, C++17, and Rust implementations.

The normative rules governing `libhqq` live in [`spec/SPEC.md`](../spec/SPEC.md), [`spec/MEMORY_MODEL.md`](../spec/MEMORY_MODEL.md), [`spec/PORTING.md`](../spec/PORTING.md), and [`spec/FINDINGS.md`](../spec/FINDINGS.md).

## Build and test

Configure and run the test suite with CMake and Ninja:

```sh
cmake -S c -B build -G Ninja -DCMAKE_BUILD_TYPE=Release -DHQQ_WERROR=ON
cmake --build build
ctest --test-dir build --output-on-failure
```

CMake options (`-D<OPTION>=<VALUE>`):

| Option | Default | Meaning |
| :--- | :--- | :--- |
| `HQQ_WERROR` | `OFF` | Treat compiler warnings as errors (`-Wall -Wextra -Wpedantic -Werror` on GCC/Clang, `/W4 /WX` on MSVC). |
| `HQQ_SANITIZE` | none | Enable sanitizer flags: `address`, `undefined`, `address+undefined`, or `thread`. |
| `HQQ_BUILD_TESTS` | `ON` at top level | Build unit, fork, and stress tests plus CLI tools under `tools/`. |
| `HQQ_BUILD_CXX_CHECK` | `ON` | Compile-check public headers as C++17 (`test_cxx17`). |
| `HQQ_BUILD_STRESS` | `ON` | Build the bounded weak-memory litmus test runner (`verify/stress`). |
| `HQQ_BUILD_SHARED` | `OFF` | Build `libhqq` as a shared library instead of a static archive. |
| `HQQ_DARWIN_NO_PRIVATE_ULOCK` | `OFF` | On macOS, disable fallback to the private `__ulock_*` syscall interface. |

Generated test constants in `c/tests/generated/spec_constants.h` are produced from `spec/constants.json` by `python3 c/tools/gen_constants.py` (using only the Python standard library). Running `python3 c/tools/gen_constants.py --check` (or the `constants_fresh` CTest target) fails if the header is out of sync.

## Public API (`include/hqq/hqq.h`)

The Standard layer exposes whole-message reads and writes, zero-copy callbacks, and explicit slot reservations over `hqq_link`:

```c
#include "hqq/hqq.h"

hqq_options o;
hqq_options_init(&o);                        /* populate defaults */
o.wait_policy = HQQ_WAIT_ADAPTIVE;           /* or HQQ_WAIT_SPIN, HQQ_WAIT_SIMPLE */

hqq_link *l = NULL;
hqq_status st = hqq_standard_create("my-link", 8, 4096, 0600, &o, &l, NULL);
/* or: hqq_standard_open("my-link", 8, 4096, &o, &l, NULL) in the secondary process */

hqq_write(l, data, len, NULL);               /* publish one message, 1..4096 bytes */
size_t n;
hqq_read(l, buf, sizeof buf, &n, NULL);      /* HQQ_ERR_BUFFER_TOO_SMALL sets n = needed size */
hqq_peer_state(l);                           /* connected / closed / gone */
hqq_close(l);                                /* unlinks the name if this endpoint created it */
```

- **Caller-owned memory**: call `hqq_standard_open_memory(mem, size, count, bsize, ...)` with a page-aligned buffer of at least `hqq_standard_size(count, bsize)` bytes (such as a `MAP_SHARED` mapping).
- **Per-call deadlines and cancellation**: pass an `hqq_wait` struct specifying `timeout_ns` (`< 0` block indefinitely, `0` non-blocking poll, `> 0` relative timeout in nanoseconds) and an optional `hqq_cancel` token.
- **Zero-copy and reservations**: `hqq_write_zc` and `hqq_read_zc` invoke a callback directly on the slot buffer; `hqq_write_reserve` / `hqq_write_commit` / `hqq_write_abort` and `hqq_read_reserve` / `hqq_read_release` expose explicit reservation handles. Completing a copied reservation struct more than once fails safely with `HQQ_ERR_INVALID_OP` via a compare-and-swap guard.
- **Error model**: `hqq_status` codes map 1:1 to Go sentinels, and `hqq_error_is(err, target)` mirrors Go's `errors.Is` wrapping hierarchy. Geometry mismatches populate `hqq_open_info`.
- **Zero steady-state allocations**: `libhqq` keeps no mutable global state, and the Standard data path (`hqq_write`, `hqq_read`, reservations, and waits) performs zero heap allocations. Custom allocators can be injected via `hqq_options`.

### Wait policies

- `HQQ_WAIT_ADAPTIVE` (default): spins for 1,024 single-read steps (~4 µs), yields, and then parks in the OS kernel with a 100 ms bound (`futex` on Linux, `os_sync_wait_on_address` or `__ulock_*` on macOS, named semaphores on Windows). Wakers notify parked peers across processes automatically.
- `HQQ_WAIT_SPIN`: pure user-space busy-wait with periodic thread yields; never parks in the kernel.
- `HQQ_WAIT_SIMPLE`: spins, yields, and then sleeps with exponential backoff (20 µs to 2 ms) without registering as a kernel waiter, while still executing waker duty so Go or C peers using `HQQ_WAIT_ADAPTIVE` wake promptly.

## Advanced layer (`include/hqq/hqq_advanced.h`, `include/hqq/hqq_listener.h`)

The Advanced layer overlays stream multiplexing, chunked message reassembly, and concurrent RPC onto the same Standard segment and packet ring:

- **Setup and negotiation**: open a link with `hqq_adv_create` / `hqq_adv_open` (named) or `hqq_adv_open_memory` (caller-owned), then call `hqq_adv_negotiate` (role-aware, supporting all Protocol 1.1 feature bits) before sending messages or calls.
- **Streams**: `hqq_adv_listen_streams`, `hqq_adv_open_stream`, `hqq_adv_accept_stream`, `hqq_stream_send`, `hqq_stream_recv`, and `hqq_stream_close`. Stream `0` is accessed via `hqq_adv_stream0`.
- **RPC (`call` and `serve`)**: `hqq_stream_call` and `hqq_stream_call_with` (carrying caller ID and 16-byte trace metadata) issue requests. `hqq_adv_serve` dispatches incoming requests to `(ctx, response, request)` handlers: when no handler is running, the link's receive thread runs the request inline with zero thread hand-off (~0.6 µs round trip from a Go client on Apple M1); if an inline handler runs past 200 µs, a new thread takes over receiving so cancellations, responses, and nested calls never deadlock; and overlapping requests run on on-demand worker threads (up to 64). `hqq_adv_shutdown` refuses new requests with status `8` (`HQQ_ERRCODE_SHUTTING_DOWN`) and drains active handlers.
- **Pooled messages**: access received payloads via `hqq_msg_data` and `hqq_msg_len`, and return buffers to the per-link pool with `hqq_msg_release`. Error responses return `HQQ_ERR_RESPONSE` alongside the error payload and `hqq_msg_status`.
- **Multi-client acceptor**: servers call `hqq_acceptor_create`, `hqq_acceptor_accept`, and `hqq_acceptor_close`; clients connect with `hqq_connect` (`spec/ext/LISTENER_DRAFT.md`).
- **Thread safety and memory pools**: all Advanced functions are thread-safe (`hqq_adv_close` waits for active calls and must not be invoked from inside a `Serve` handler). After warm-up, `send`, `recv`, `call`, and `serve` allocate zero bytes from the heap, verified by `test_adv` with a counting allocator hook.

## Parent lifeline (`include/hqq/hqq_lifeline.h`)

`hqq_lifeline_start` spawns a lightweight watcher thread that monitors a file descriptor or handle (standard input by default). When the parent supervisor process terminates for any reason, including `SIGKILL`, the kernel closes the parent's pipe write descriptor, the child's read descriptor reaches `EOF`, and the lifeline invokes its callback once with `HQQ_LIFELINE_REASON_EOF` (or `HQQ_LIFELINE_REASON_ERROR` on read failure). Any keepalive bytes written to the pipe are drained and discarded. Interactive terminals are ignored unless `force` is enabled. When `grace_ms` is set, the process exits with `HQQ_LIFELINE_EXIT_CODE` (`70`) if `hqq_lifeline_stop` is not called within the grace window.

- The callback executes on the lifeline thread and should only trigger an orderly shutdown (setting an atomic flag, writing to a self-pipe, or signaling the main loop); it must not call `hqq_lifeline_stop` directly.
- `hqq_lifeline_stop` cancels the watcher without firing the callback and joins the helper thread (using a `poll` self-pipe on POSIX and `CancelSynchronousIo` on Windows).
- Used in production by `hqq-sqlite-server --parent-stdin` ([`examples/sqlite/server/README.md`](../examples/sqlite/server/README.md)).

## RPC runtime for generated servers (`include/hqq/hqq_rpc.h`)

`hqq_rpc.h` provides the C server runtime targeted by `hqq-bindgen` (specified in [`spec/toolchain/BINDGEN.md`](../spec/toolchain/BINDGEN.md) and [`bindgen/README.md`](../bindgen/README.md)). It serves one session per dedicated `StandardLink`: it validates incoming frames, decodes arguments in place from the request slot, maintains the per-session generation-checked handle table (destroying child handles before parents on teardown), executes pre-call interceptors and C library functions under the session/server execution lock, and encodes responses directly into reserved transmit slots. `hqqrpc_main` implements the standard CLI entry point (`ready <listener>` handshake, `--parent-stdin`, `SIGTERM`/`SIGINT` handling) on POSIX platforms and returns `HQQ_ERR_UNSUPPORTED` on Windows.

## Platforms

`libhqq` supports Linux, macOS, Windows, and other POSIX systems with C11 lock-free atomics:

| Platform | Named Segment Backing | Cross-Process Parking |
| :--- | :--- | :--- |
| Linux | `/dev/shm/<name>`, `O_NOFOLLOW` regular-file verification, `fallocate` | `futex` (`FUTEX_WAIT` / `FUTEX_WAKE`, shared) |
| macOS | `shm_open("/<name>")`, 30-character name limit | `os_sync_wait_on_address` (`SHARED`) when available, else `__ulock_*` |
| Windows | `CreateFileMappingW` + four named semaphores, DACL translated from `perm` | Named kernel semaphores (`WaitOnAddress` for unnamed memory) |
| Other POSIX (FreeBSD, ...) | `shm_open` | `HQQ_WAIT_SIMPLE` bounded backoff |

`libhqq` requires always-lock-free 32-bit and 64-bit C11 `<stdatomic.h>` operations (supported by GCC, Clang, `clang-cl`, and MSVC 17.5+ with `/std:c11 /experimental:c11atomics`), a little-endian target, and a 64-bit address space (`spec/SPEC.md` §13.3).

## Layout of the sources

| Path | Content |
| :--- | :--- |
| `include/hqq/hqq.h` | Core Standard layer public header (C11 and C++17 `extern "C"`). |
| `include/hqq/hqq_advanced.h`, `hqq_listener.h` | Advanced layer, streams, RPC, and multi-client acceptor headers. |
| `include/hqq/hqq_lifeline.h` | Parent lifeline header. |
| `include/hqq/hqq_rpc.h` | `hqq-bindgen` C server runtime header. |
| `src/hqq_ring.h` | OS-independent MPMC ring core (slot protocol, cursors, waker/waiter state machine). |
| `src/hqq_layout.h` | Segment geometry, `HQQ_STL3` superblock, endpoint records, and 64-byte packets. |
| `src/hqq_superblock.c` | Superblock claim loop, initialization, takeover, and liveness checks (`SPEC.md` §5, §6.2). |
| `src/hqq_link.c` | Standard link reads, writes, zero-copy callbacks, reservations, and heartbeats (`SPEC.md` §6–§9). |
| `src/hqq_advanced.c`, `src/hqq_listener.c` | Advanced protocol engine, stream router, adaptive `serve`, and `HQQ_LSN1` acceptor. |
| `src/hqq_named.c`, `src/hqq_names.c` | Named segment lifecycle, safe unlink (`CreatorToken`), and name validation (`SPEC.md` §11). |
| `src/hqq_platform_posix.c`, `src/hqq_platform_win32.c` | OS-specific shared memory, clocks, threads, mutexes, and kernel parking. |
| `src/hqq_lifeline.c` | Parent lifeline implementation (POSIX `poll`/self-pipe, Windows `ReadFile`/`CancelSynchronousIo`). |
| `src/hqq_rpc.c`, `hqq_rpc_session.c`, `hqq_rpc_server.c` | `hqq-bindgen` C runtime (wire codecs, handle table, session loop, `hqqrpc_main`). |
| `tests/` | Dependency-free C test suite (`test_*.c`) and golden vector checks against `spec/constants.json`. |
| `tools/hqq_echo.c`, `tools/hqq_adv_echo.c` | CLI echo helpers for Standard links and `BenchmarkAdvancedCallCServer`. |
| `verify/` | GenMC model-checking drivers and herd7 / weak-memory litmus tests. |

## Tests

Every CTest target runs standalone without external test frameworks:

| Test Target | Coverage |
| :--- | :--- |
| `constants`, `constants_fresh` | Verifies every byte offset, struct size, magic value, opcode, error code, and platform limit against `spec/constants.json` and checks that `spec_constants.h` is up to date. |
| `ring` | Ring format/attach rules, producer/consumer slot protocol, full/empty transitions, FIFO ordering, lap wraparound at $2^{64}-2$, copied reservations, and 3-producer / 3-consumer MPMC stress. |
| `superblock` | Superblock claim decision table: foreign/unknown states, layout mismatches, live/dead endpoint joins, reuse, progress-watched takeover, and opener write prohibition. |
| `link` | Caller-owned memory links: whole-message I/O, `HQQ_ERR_BUFFER_TOO_SMALL`, zero-copy callbacks, reservations and tombstones, deadlines, cancellation, peer close drain, and parked wake-ups. |
| `simple` | Delivery, timeout, and cancellation across `HQQ_WAIT_ADAPTIVE`, `HQQ_WAIT_SPIN`, and `HQQ_WAIT_SIMPLE`. |
| `named` (POSIX) | Shared-memory name validation, creator/opener rules, transient startup errors, foreign/undersized objects, safe unlink, and segment reuse after peer death. |
| `fork` (POSIX) | Multi-process echo with full-slot payloads, opener-first startup, `SIGKILL` crash detection with drain, and graceful close. |
| `lifeline` (POSIX) | Parent lifeline `EOF` detection, keepalive draining, terminal skip/force, clean `hqq_lifeline_stop`, grace-period forced exit, and `SIGKILL` of the parent process. |
| `stress` | Multi-threaded bidirectional ring stress over 2-slot and 256-slot rings with per-message CRC verification. |
| `cxx17`, `smoke` | C++17 header compilation/link check and atomic lock-free self-test. |
| `platform_mutex` | Cross-platform mutex mutual exclusion under contention and lifecycle rules. |
| `adv`, `adv_uninit`, `adv_close_queued`, `adv_serve` | Advanced negotiation, multi-chunk reassembly, streams, steady-state zero allocations, poison-allocator uninitialized-context regression, queued-request close cleanup, and inline/escalated `hqq_adv_serve`. |
| `listener` | Multi-client acceptor round trips, busy listener rejection, malformed connect records, and acceptor close. |
| `rpc`, `rpc_calls`, `rpc_handles`, `rpc_fuzz`, `rpc_live`, `rpc_main` | `hqq-bindgen` C runtime: golden wire vectors, in-place and scratch body decoding, response chunking, handle table generations and reverse-order teardown, deterministic frame fuzzing, live POSIX sessions, and `hqqrpc_main`. |

## Interoperability with Go

In addition to the automated `interop/` test matrix (`X-01` through `X-21` and `X-A01` through `X-A10` across all four Go/C role pairings), direct cross-process tests verify:

- **Bidirectional echo and graceful close**: C creator + Go opener and Go creator + C opener across 4,096-byte buffers and 2-slot 8/64-byte rings, confirming `io.EOF` on Go and `HQQ_ERR_PEER_CLOSED` on C.
- **Crash detection**: killing a peer with `SIGKILL` triggers `HQQ_ERR_PEER_GONE` in C and `ErrPeerGone` in Go as soon as the configured `PeerTimeout` expires.
- **Cross-runtime kernel wake-ups**: parked C waiters woken by Go producers and parked Go waiters woken by C producers complete without lost wake-ups across both `os_sync_wait_on_address` and `__ulock_*` on macOS and `futex` on Linux.
