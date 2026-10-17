# hqq-sqlite-server: the C server

`hqq-sqlite-server` serves the hqq-sqlite protocol ([../../sqlite/PROTOCOL.md](../../sqlite/PROTOCOL.md),
version 1.0) on one HQQ listener. It is written in C11 on libhqq's Advanced layer
and the SQLite C API. The Go driver in `../../sqlite` is its reference client, and
the end-to-end tests in `../../sqlite/e2e` run that driver against it.

Linux, macOS and Windows (x64 and arm64, built with clang-cl). The platform differences are in one
layer: `src/sqlsrv_port.h`, with `src/sqlsrv_port_posix.c` (pthreads, sigwait, realpath) and
`src/sqlsrv_port_win.c` (CreateThread, SRWLOCK, condition variables, console control events,
GetFinalPathNameByHandleW). The database path rules are in `src/sqlsrv_pathrules.c`.

## Build

libhqq builds the server behind an option that is OFF by default:

```sh
cmake -S c -B build -DHQQ_BUILD_SQLITE_SERVER=ON -DHQQ_WERROR=ON
cmake --build build --target hqq-sqlite-server hqqsqlite_server_unit_tests
ctest --test-dir build -R hqqsqlite_server --output-on-failure
```

The binary is `build/sqlite-server/hqq-sqlite-server`.

On Windows, build with clang-cl and an SQLite amalgamation (there is no system SQLite). The release
(`.github/workflows/release.yml`) uses the static C runtime, which must apply to the whole build,
because libhqq and the server must link the same runtime:

```bat
cmake -S c -B build -G Ninja -DCMAKE_C_COMPILER=clang-cl -DCMAKE_BUILD_TYPE=Release ^
  -DHQQ_WERROR=ON -DHQQ_BUILD_TESTS=ON -DHQQ_BUILD_SQLITE_SERVER=ON ^
  -DHQQ_SQLITE_AMALGAMATION_DIR=C:\path\to\amalgamation -DCMAKE_MSVC_RUNTIME_LIBRARY=MultiThreaded
```

The binary is `build\sqlite-server\hqq-sqlite-server.exe`. `scripts/release/fetch-sqlite.ps1` fetches
and verifies the pinned amalgamation.

### SQLite

- **System library (default).** `find_package(SQLite3 3.24)` finds it: libsqlite3-dev
  on Ubuntu, and the SDK copy (`libsqlite3.tbd`) or Homebrew on macOS.
- **Amalgamation.** `-DHQQ_SQLITE_AMALGAMATION_DIR=/path` builds against
  `sqlite3.c` and `sqlite3.h` in that directory. Nothing is downloaded and nothing is
  vendored in the repository.

The server needs a thread-safe SQLite (`sqlite3_threadsafe() != 0`) and version
3.24 or later. It refuses to start otherwise.

Some SQLite builds omit loadable extensions (`OMIT_LOAD_EXTENSION`; the macOS SDK
copy does). In such a build `SQLITE_DBCONFIG_ENABLE_LOAD_EXTENSION` is reported as
unsupported. The server accepts that only when `sqlite3_compileoption_used`
reports `OMIT_LOAD_EXTENSION`, because then there is no loader to disable.

## Run

```
hqq-sqlite-server <listener-name> [options]

  --allow-dir DIR          allow raw database paths inside DIR (default: aliases only)
  --alias NAME=PATH        database alias (repeatable)
  --max-sessions N         open sessions allowed at once (1..65536, default 256)
  --perm OCTAL             listener permission (default 0600)
  --heartbeat-ms N         link heartbeat interval (default: libhqq's)
  --peer-timeout-ms N      peer timeout (default: libhqq's)
  --shutdown-timeout-ms N  wait for running requests on shutdown (default 5000)
  --parent-stdin[=force]   shut down gracefully when standard input reaches end-of-file (the
                           parent died); alias --exit-on-stdin-eof; see "Parent lifeline"
  --lifeline-grace-ms N    forced exit if that shutdown takes longer (default 5000; 0: never)
  --version, --help
```

On start it prints `ready <listener-name>` on stdout. It exits with status 2 on a bad
option and 1 when the listener cannot be created (for example, the name is in use by a
live server).

`SIGINT` and `SIGTERM` (on Windows: the console events `CTRL_C`, `CTRL_BREAK`, `CTRL_CLOSE` and `CTRL_SHUTDOWN`) start a graceful shutdown: the acceptor closes, each link
refuses new requests (status 8, PROTOCOL.md section 2 and libhqq section 6), running
requests finish up to `--shutdown-timeout-ms`, then the streams close and every
session rolls back its open transaction. `SIGKILL` runs no cleanup; SQLite recovers its
files at the next open.

On Windows, `CTRL_CLOSE_EVENT` and `CTRL_SHUTDOWN_EVENT` give the process about five seconds before
Windows ends it, so the handler waits at most four seconds for the shutdown to finish; a longer
shutdown is cut short by Windows. `TerminateProcess` (and `SIGKILL`) runs no cleanup on either system.

## Parent lifeline

`--parent-stdin` (alias `--exit-on-stdin-eof`) ties the server's life to the process that
started it. The supervisor creates a pipe, keeps the write end, and passes the read end as
the server's standard input. The server watches stdin with libhqq's parent lifeline
(`hqq_lifeline.h`). Bytes the supervisor writes are discarded, so keepalives are allowed.
When the supervisor exits, for any reason, the OS closes the write end and the server reads
end-of-file. It then runs the SIGTERM shutdown path (listener closed, running requests
finish, sessions roll back) and exits with status 0.

- `--lifeline-grace-ms N` (default 5000, 0: never) bounds that shutdown. If the process is
  still running N ms after the end-of-file, it exits with status 70
  (`HQQ_LIFELINE_EXIT_CODE`) at once, without atexit handlers or flushing.
- A terminal on stdin is not watched: the server prints one warning line and runs on, so
  an interactive run is not stopped by a closed terminal. `--parent-stdin=force` watches it
  anyway.
- Standard input must be a pipe the supervisor holds. With `/dev/null` or a closed
  descriptor the server sees end-of-file (or an error) at once and shuts down straight
  away. A regular file is read to its end first, then the server shuts down.

### Zombie-free guarantee and its limits

With `--parent-stdin`, a server whose supervisor died is not left running as an orphan
serving a listener nobody controls. This holds when the supervisor dies in any way that
closes its descriptors, including SIGKILL, a crash, or an exit that skips cleanup.

It does not hold when:

- The supervisor is stopped (SIGSTOP or a debugger) rather than dead. The write end stays
  open and the server keeps running.
- Another process holds the write end. Every holder must close it, and a child started
  without close-on-exec keeps the pipe open after the supervisor dies. The supervisor must
  create the pipe close-on-exec.
- The server forks and the forked process keeps running. Only the calling thread survives
  `fork()`, so the lifeline thread does not exist in the child and nothing watches its
  stdin. The server must not daemonize or fork after start.
- The server's stdin is duplicated to a grandchild that keeps it open. The pipe then
  reaches end-of-file only after that grandchild exits too.

Covered and tested: the Go e2e tests close the supervisor's end with a transaction open,
and SIGKILL a separate pipe holder (`sqlite/e2e/lifeline_test.go`). The C unit tests
(`c/tests/test_lifeline.c`) cover the helper.

## Database policy

- **Aliases** (`--alias NAME=PATH`) are checked first. The path is canonicalized at
  start-up and may lie anywhere.
- **Raw paths** are used only with `--allow-dir DIR`. A relative path is taken relative
  to `DIR`. The canonical result (symbolic links resolved) must lie strictly inside
  `DIR`. A path with a `..` component is refused by name. A symbolic link that leads
  outside is refused, and so is a dangling link.
- Anything else is `FORBIDDEN` (9). A missing parent directory is SQLite `CANTOPEN`.
- `ATTACH`, `DETACH` (authorizer) and loadable extensions are disabled in every
  session.
- **Windows rules** (the same checks on every host, in `sqlsrv_pathrules.c`). Accepted: a drive-absolute
  path (`C:\` or `C:/`) or a relative one, with `/` or `\` as separator. Refused by name: UNC and
  `\\?\` / `\\.\` paths, rooted paths without a drive (`\x`), drive-relative paths (`C:x`), `:` anywhere
  but the drive (alternate data streams), `< > " | ? *` and control characters, empty components,
  components ending in `.` or a space, `..` components, and reserved device names (CON, PRN, AUX, NUL,
  COM0-COM9, LPT0-LPT9, CONIN$, CONOUT$, with or without an extension). Symbolic links and junctions are
  followed by the canonical path (`GetFinalPathNameByHandleW`), which must be a local `X:\` path inside
  the allow directory. The containment test ignores ASCII case; a non-ASCII case difference is refused.
- The check runs when the session opens. It is not a sandbox: a local process that changes the directory
  after that can still redirect a later open (the same holds on POSIX).
- The allow directory and every alias must be on a local or mapped drive. UNC shares and `\\?\UNC\`
  paths are refused at start-up (the path cannot be canonicalized to a local `X:\` path).

## Design

| Piece | Where | What it does |
|---|---|---|
| Acceptor | `src/sqlsrv_link.c` | One thread: `hqq_acceptor_accept`. Each accepted link gets `hqq_adv_listen_streams` before anything else. |
| Link | `sqlsrv_link.c` | Two threads: `hqq_adv_serve` (requests) and the stream loop (`hqq_adv_accept_stream`). The stream loop drains the link at the end. |
| Watcher | `sqlsrv_link.c` | One detached thread per session: `hqq_stream_recv`. Its return ends the session: stop, teardown, stream close, then the link forgets the session. |
| Session | `src/sqlsrv_session.c` | One worker thread per session, which owns the SQLite connection and runs all requests in arrival order. |
| Handler | `sqlsrv_session.c` (`sqlsrv_sess_request`) | Runs inline on the link's receive thread while no handler runs, and on a Serve worker otherwise. It hands the job to the session worker, waits, and writes the response. On context end it calls `sqlite3_interrupt` and re-interrupts until the job stops. It sends nothing for a canceled request. |

Each session is independent: its own connection, statements, cursors, and
transaction. A long query blocks only its own session. The session worker checks the
request context between steps, so an interrupt that arrives between two steps is not
lost.

While no handler runs, a request runs on the link's receive thread (escalated after 200 µs);
concurrent requests run on libhqq's Serve worker pool, which grows to 64 workers per
link (`ADV_MAX_WORKERS`). A request holds its thread until it is answered, so one link
serves at most 64 concurrent worker requests plus the inline slot. The Go driver shares one link per connector,
which is enough for the 50-connection test.

Rows are pulled, batches are bounded by row count, by the 1 MiB batch budget, and by
the 16 MiB message limit. A row is measured before it is encoded. When a row fits a
message of its own but not the current response (a Query header can take several MiB),
it is kept in the cursor and returned by the next Fetch.

### Limits

PROTOCOL.md section 11 is enforced. The server also sets `sqlite3_limit` for SQL
length (1 MiB), variables (32766), columns (2000), and attached databases (0).

## Tests

- `tests/test_units.c` (`hqqsqlite_server_units`): codec and golden encodings from
  PROTOCOL.md section 16, status values, limits, pragma and flag validation, and the
  path policy (traversal, symbolic links, dangling links, aliases, NUL), and the Windows path rules. The
  Windows rules are pure functions that take the style as an argument, so they run on every host.
- `../../sqlite/e2e` (Go): starts the server (`HQQ_SQLITE_SERVER` names the binary; the tests
  skip without it) and runs the driver against it: values, prepared and named
  statements, batches, large blobs and the 16 MiB row limit, transactions and
  read-only mode, cancellation of a 1e9-row recursive CTE, 50 concurrent connections,
  a client crash inside a transaction (rollback observed by another session), SIGKILL
  of the server with reconnect, the path policy, the session limit, error codes,
  busy locks, graceful shutdown, argument checks, and the parent lifeline: EOF on the
  supervisor's pipe shuts the server down with status 0 and rolls back an open transaction;
  without `--parent-stdin` EOF is ignored; SIGKILL of the pipe's holder shuts it down.

Sanitized run: build with `-DHQQ_SANITIZE=address+undefined` and point
`HQQ_SQLITE_SERVER` at that binary (see `.github/workflows/sqlite.yml`).

## Protocol notes

Points where PROTOCOL.md is silent or differs from the implementation. Each was
decided as stated; the maintainers of PROTOCOL.md should confirm them.

1. **Stream open before `ListenStreams`.** Section 2 says the request waits until
   `ListenStreams` runs. The server receives a link only after the client's connect
   returns, so a stream open can arrive first. libhqq holds such a request for
   `stream_listen_grace_ns` (default 2 s, the server's setting) and admits it when
   `ListenStreams` runs; after the grace it is refused with `ConnNotFound`. The Go
   library does the same (`AdvancedOptions.StreamListenGrace`). Without the grace the
   client's first open lost the race in every run (the table row "not listening" in
   section 2 describes only the refusal after the grace).
2. **Check order of Open.** `ALREADY_OPEN` is answered before the version check, as the
   Go fake server does. Section 6.1 lists only the body, policy, and open checks. A
   state check comes first because a session that is already open has no next step.
3. **Open response minor.** The response always carries minor 0, the server's minor.
   The effective minor is not echoed.
4. **Ordinal range.** The `1..MaxParams` ordinal rule is applied to named parameters
   too (their ordinals must be unique and in range, even though they are ignored for
   binding).
5. **Parameter count.** More than 32766 parameters is `LIMIT`, not `BAD_REQUEST`
   (section 11 allows either).
6. **`more` and the last batch.** A full batch reports `more = 1` without stepping, so
   a statement that ended exactly at the batch boundary is confirmed by a final empty
   batch with `more = 0` (section 5 allows this).
7. **Row held for the next batch.** A row that fits a message but not the current
   response is returned by the next Fetch (section 5 only says that a row which fits
   no message is `TOOBIG`).
8. **Column names.** A column name longer than 1024 bytes is `LIMIT`. Decltypes are not
   limited (they come from the schema).
9. **Teardown order.** Cursors are released before the rollback, not after. This avoids
   a rollback with an active read statement. The observable result is the same.
10. **Pragmas.** Open validates the name and value alphabets (section 6.1). Pragma
    names are not checked against a list, so a client can set a pragma such as
    `writable_schema`. Section 12 treats clients as trusted.
11. **Concurrency bound.** At most 64 requests per link (libhqq's worker bound), which
    section 13 does not mention.
12. **Stream lookup.** A request on a stream that is not registered yet waits up to
    5 seconds for its registration, then gets `NO_SESSION`. A request with no stream at
    all (stream 0) is `NO_SESSION` at once.
13. **Extended result codes.** Enabled on every connection, so the status is the
    extended code (for example `0x10813`), as section 7 shows.
