# `hqq-sqlite-server`: C11 SQLite Service over HQQ

`hqq-sqlite-server` is the production C11 server for the [`hqq-sqlite` wire protocol](../../sqlite/PROTOCOL.md) (version 1.0). Built on `libhqq`'s Advanced layer and the SQLite C API, it listens on a single HQQ acceptor name and serves concurrent client processes over shared memory. The pure-Go `database/sql` driver in [`../../sqlite`](../../sqlite/README.md) is its primary client, verified against this binary by [`../../sqlite/e2e`](../../sqlite/e2e).

The server runs natively on **Linux**, **macOS**, and **Windows** (`x64` and `arm64`, built with `clang-cl` or MSVC). Platform-specific primitives are isolated behind `src/sqlsrv_port.h`, implemented by `src/sqlsrv_port_posix.c` (`pthreads`, `sigwait`, `realpath`) and `src/sqlsrv_port_win.c` (`CreateThread`, `SRWLOCK`, condition variables, console control events, `GetFinalPathNameByHandleW`), while cross-platform path validation lives in `src/sqlsrv_pathrules.c`.

## Build

Enable `HQQ_BUILD_SQLITE_SERVER=ON` when configuring the `c/` CMake project:

```sh
cmake -S c -B build -DHQQ_BUILD_SQLITE_SERVER=ON -DHQQ_WERROR=ON
cmake --build build --target hqq-sqlite-server hqqsqlite_server_unit_tests
ctest --test-dir build -R hqqsqlite_server --output-on-failure
```

This produces `build/sqlite-server/hqq-sqlite-server`.

On Windows, pass a verified SQLite amalgamation directory (`scripts/release/fetch-sqlite.ps1` downloads and checks the pinned archive). Release builds (`.github/workflows/release.yml`) compile with `clang-cl` and link the static C runtime across both `libhqq` and the server:

```bat
cmake -S c -B build -G Ninja -DCMAKE_C_COMPILER=clang-cl -DCMAKE_BUILD_TYPE=Release ^
  -DHQQ_WERROR=ON -DHQQ_BUILD_TESTS=ON -DHQQ_BUILD_SQLITE_SERVER=ON ^
  -DHQQ_SQLITE_AMALGAMATION_DIR=C:\path\to\amalgamation -DCMAKE_MSVC_RUNTIME_LIBRARY=MultiThreaded
```

This produces `build\sqlite-server\hqq-sqlite-server.exe` (MSVC `cl.exe` 17.5+ is also supported and tested in `c.yml`).

### SQLite library linkage

- **System SQLite (default on POSIX)**: located via `find_package(SQLite3 3.24)` (`libsqlite3-dev` on Ubuntu/Debian, SDK `libsqlite3.tbd` or Homebrew on macOS).
- **Pinned amalgamation**: pass `-DHQQ_SQLITE_AMALGAMATION_DIR=/path/to/amalgamation` to compile `sqlite3.c` and `sqlite3.h` directly into the binary.

At startup, `hqq-sqlite-server` verifies that SQLite is compiled thread-safe (`sqlite3_threadsafe() != 0`) and is version 3.24 or newer, refusing to start otherwise. If `sqlite3_db_config(..., SQLITE_DBCONFIG_ENABLE_LOAD_EXTENSION, 0)` reports unsupported (as on the macOS system `libsqlite3.tbd`), the server accepts it only after confirming via `sqlite3_compileoption_used("OMIT_LOAD_EXTENSION")` that extension loading was compiled out entirely.

## Command-line interface

```text
hqq-sqlite-server <listener-name> [options]

  --allow-dir DIR          allow relative database paths inside DIR (default: aliases only)
  --alias NAME=PATH        register a named database alias (repeatable)
  --max-sessions N         maximum concurrent open sessions (1..65536, default 256)
  --perm OCTAL             listener shared-memory permissions (default 0600)
  --heartbeat-ms N         link heartbeat interval in milliseconds (default: libhqq default)
  --peer-timeout-ms N      peer crash timeout in milliseconds (default: libhqq default)
  --shutdown-timeout-ms N  grace period for in-flight requests on shutdown (default 5000)
  --parent-stdin[=force]   shut down cleanly when stdin reaches EOF (alias --exit-on-stdin-eof)
  --lifeline-grace-ms N    forced exit timeout after stdin EOF (default 5000; 0 disables forced exit)
  --version, --help
```

Once the listener is initialized, the server writes `ready <listener-name>\n` to standard output and flushes it. It exits with status `2` on invalid CLI arguments and status `1` if the listener cannot be created (for example when another live server already holds `<listener-name>`).

On `SIGINT` or `SIGTERM` (and on Windows console events `CTRL_C_EVENT`, `CTRL_BREAK_EVENT`, `CTRL_CLOSE_EVENT`, and `CTRL_SHUTDOWN_EVENT`), the server closes the listener, places every active link into `Shutdown` (refusing new requests with status `8`), waits up to `--shutdown-timeout-ms` (capped at 4 s on Windows `CTRL_CLOSE` / `CTRL_SHUTDOWN`) for running requests to complete, closes all streams, and rolls back any open transactions before exiting with status `0`.

## Parent lifeline (`--parent-stdin`)

Passing `--parent-stdin` (or `--exit-on-stdin-eof`) binds the server's lifetime to its parent process via [`hqq_lifeline.h`](../../../c/include/hqq/hqq_lifeline.h). The parent creates a pipe, passes the read descriptor as the server's `stdin`, and keeps the write descriptor open. When the parent exits for any reason, including `SIGKILL` or a runtime crash, the OS kernel closes the write descriptor, the server's lifeline thread reads `EOF`, and the server runs its normal graceful shutdown path (draining in-flight requests and rolling back uncommitted transactions) before exiting with status `0`.

- **`--lifeline-grace-ms N` (default `5000`)**: if graceful shutdown has not finished `N` ms after `stdin` reached `EOF`, the lifeline thread terminates the process immediately with exit code `70` (`HQQ_LIFELINE_EXIT_CODE`).
- **Terminal protection**: if `stdin` is a terminal (`isatty`), the server prints a warning and skips watching `stdin` so closing an interactive shell does not kill the server unless `--parent-stdin=force` is specified.
- **Operational requirements**: the parent must create the pipe with close-on-exec (`O_CLOEXEC`) so unrelated child processes do not inherit the write end and hold the pipe open after the parent dies, and the server itself never calls `fork()`.

## Database path and security policy

- **Aliases (`--alias NAME=PATH`)**: matched first before checking `--allow-dir`. Alias target paths are canonicalized at server startup and may point anywhere on local storage.
- **Sandboxed relative paths (`--allow-dir DIR`)**: when `--allow-dir` is configured, a database name that does not match an alias is resolved relative to `DIR`. Any path containing `..` components, absolute paths outside `DIR`, dangling symlinks, or symlinks resolving outside `DIR` are rejected with `FORBIDDEN` (`9`). Missing parent directories return SQLite `CANTOPEN` (`14`). When `--allow-dir` is omitted, only registered aliases are allowed.
- **Windows path rules (`sqlsrv_pathrules.c`)**: rejects UNC shares (`\\server\share`), device/verbatim prefixes (`\\?\`, `\\.\`), drive-relative paths (`C:file`), alternate data streams (`:`), wildcard/control characters (`< > " | ? *`), trailing dots or spaces on path components, `..` components, and reserved DOS device names (`CON`, `PRN`, `AUX`, `NUL`, `COM0`..`COM9`, `LPT0`..`LPT9`, `CONIN$`, `CONOUT$`). Resolved paths (`GetFinalPathNameByHandleW`) must reside on a local `X:\` drive strictly inside `--allow-dir`.
- **SQLite engine restrictions**: every connection disables `sqlite3_load_extension`, sets `SQLITE_LIMIT_ATTACHED` to `0`, and registers an authorizer denying `SQLITE_ATTACH` and `SQLITE_DETACH`.

## Internal architecture

| Component | Source File | Responsibility |
| :--- | :--- | :--- |
| Acceptor loop | `src/sqlsrv_link.c` | Runs `hqq_acceptor_accept` on a dedicated thread and enables `hqq_adv_listen_streams` on each accepted client link. |
| Per-link loops | `src/sqlsrv_link.c` | Runs `hqq_adv_serve` (for RPC requests) and `hqq_adv_accept_stream` (for new session streams) on each client link. |
| Stream watcher | `src/sqlsrv_link.c` | Spawns one detached thread per session calling `hqq_stream_recv`. When `hqq_stream_recv` returns (on stream close or link death), it interrupts any active query, tears down the session, and frees its state. |
| Session worker | `src/sqlsrv_session.c` | Dedicates one worker thread per session that owns the `sqlite3*` connection and executes session requests sequentially in FIFO order. |
| Request handler | `src/sqlsrv_session.c` (`sqlsrv_sess_request`) | Invoked by `hqq_adv_serve` (inline on the receive thread when idle, or on a `libhqq` worker thread up to 64 concurrent requests per link). Submits the request to the session worker, awaits completion, and invokes `sqlite3_interrupt` if the request context is canceled or times out. |

## Tests

- **C unit tests (`tests/test_units.c`, CTest target `hqqsqlite_server_units`)**: tests wire codecs, golden byte vectors from `sqlite/PROTOCOL.md` §16, status mappings, resource limits, pragma/flag validators, and both POSIX and Windows path-policy rules.
- **Go end-to-end suite (`../../sqlite/e2e`)**: launches `HQQ_SQLITE_SERVER` and exercises all parameter/result types, prepared statements, cursor batching, 16 MiB row limits, transactions, mid-query cancellation of a $10^9$-row recursive CTE, 50 concurrent connections, client crash rollback, server `SIGKILL` recovery, path-policy refusals, and `--parent-stdin` lifeline shutdown.
- **Sanitizer builds**: pass `-DHQQ_SANITIZE=address+undefined` to CMake and point `HQQ_SQLITE_SERVER` at the resulting binary (run automatically in `.github/workflows/sqlite.yml`).

## Implementation notes on `sqlite/PROTOCOL.md`

The following implementation details refine edge cases in [`sqlite/PROTOCOL.md`](../../sqlite/PROTOCOL.md):

1. **Stream open grace before `ListenStreams` (§2)**: `libhqq` holds incoming `OpConnCreate` frames for `stream_listen_grace_ns` (default 2 s) so a client that calls `OpenStream` immediately after `Connect` returns never races against the server's `hqq_adv_listen_streams` call.
2. **`Open` state check order (§6.1)**: `ALREADY_OPEN` is checked before inspecting the request body's `major` version word.
3. **`Open` response `minor` version (§6.1)**: the server always replies with its own minor version (`0`).
4. **Named parameter ordinals (§4)**: `1..MaxParams` range and uniqueness checks apply to both positional and named parameter `ordinal` fields.
5. **Parameter count overflow (§4, §11)**: sending more than `MaxParams` (`32,766`) parameters returns `LIMIT`.
6. **Exact-multiple batch termination (§5)**: because the server never steps ahead after a full batch, a query whose row count is an exact multiple of the batch size returns `more = 1` on the full batch and `nrows = 0, more = 0` on the following `Fetch`.
7. **Oversize single-batch row carry-over (§5)**: if a row fits within `MaxMessageBytes` (`16 MiB`) on its own but would push the current `Query` response (which also carries column metadata) past `MaxMessageBytes`, the row is held in the cursor and delivered on the first `Fetch`.
8. **Column name limit (§5, §11)**: column names exceeding `MaxNameBytes` (`1,024` bytes) return `LIMIT`; declared type strings (`decltype`) are unbounded because they come from the database schema.
9. **Teardown order (§8.3)**: open cursors and statements are finalized/reset before executing `ROLLBACK`, avoiding `SQLITE_BUSY` on rollback when a read statement is still stepped.
10. **Trusted pragma names (§6.1, §12)**: `Open` validates pragma name/value character sets rather than restricting pragma names to a fixed list, consistent with §12's trusted-client model.
11. **Per-link concurrency bound (§13)**: each client link dispatches up to 64 concurrent worker threads plus the inline receive slot (`ADV_MAX_WORKERS = 64`).
12. **Early request stream lookup (§2)**: if an RPC request arrives on a newly accepted stream before the server's stream-accept loop finishes registering the session struct, the handler waits up to 5 s for registration before returning `NO_SESSION` (requests on stream `0` return `NO_SESSION` immediately).
13. **Extended result codes (§7)**: `sqlite3_extended_result_codes(db, 1)` is enabled on every connection so `status` always carries the extended SQLite code (for example `0x10813` for `SQLITE_CONSTRAINT_UNIQUE`).
