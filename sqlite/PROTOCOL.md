# `hqq-sqlite` Wire Protocol, Version 1.0

This specification documents the SQLite service error codes (`sqlite/internal/wire`), open-policy rules, single-statement rules, and resource limits enforced by the `gosuda.org/hqq/sqlite` driver (`"hqq-sqlite"`) and the C11 SQLite server (`bindgen/examples/sqlite3/server`, built on `libhqq` and `hqq-bindgen`).

The key words `MUST`, `MUST NOT`, `SHOULD`, and `MAY` in this document are to be interpreted as described in RFC 2119.

## 1. Overview

An `hqq-sqlite` service runs in its own OS process and accepts client connections through a single HQQ listener name. Every `database/sql` connection (`driver.Conn`) maps to one logical HQQ **stream** multiplexed over a shared `AdvancedLink`. The server runs each stream as an isolated **session** with its own `sqlite3` database connection, transaction state, prepared statement table, and open cursor table. Sessions never share in-memory protocol state; sessions that open the same underlying database file coordinate through standard SQLite file/WAL locking.

Each operation travels as one HQQ request message on a session stream and receives one response message (either a success response carrying a binary payload or an error response carrying a status code and UTF-8 message). Requests on a single stream are strictly sequential: a client `MUST NOT` send a new request on a stream while a prior request on that stream remains unanswered, except for transmitting an `OpCancel` frame as described in §9.

## 2. Transport (HQQ)

The SQLite wire protocol layers on top of an HQQ `Acceptor` and negotiated `AdvancedLink` streams:

| Transport Aspect | Requirement |
| :--- | :--- |
| Listener | One HQQ acceptor (`HQQ_LSN1`) per service name. Clients connect via `hqq.Connect(ctx, name, cfg)`. |
| Negotiated features | Both endpoints `MUST` negotiate `FeatureRequestResponse` and `FeatureCancel`. `FeatureMetadata` is optional and propagates the caller's remaining context deadline (§9). |
| Maximum message size | Both endpoints `MUST` configure `AdvancedOptions.MaxMessageSize` to at least `16 MiB` (`MaxMessageBytes`, §11). |
| Streams | The client opens one stream per `driver.Conn` (`OpenStream`). The server calls `ListenStreams` on every accepted link, accepts each stream with `AcceptStream`, and registers a session. Stream open requests arriving before `ListenStreams` are held during `StreamListenGrace`; a server that never calls `ListenStreams` refuses the open with `ErrConnNotFound`. |
| Requests | Advanced request message (`AdvFlagRequest` set), `aux` = method ID (§6), payload = binary request body. |
| Responses | Advanced response message (`AdvFlagResponse` set), `aux` = `0` on success or non-zero status on error (§7), payload = binary response body (success) or unprefixed UTF-8 error text (error). |
| Ordering | Requests and responses on the same stream execute in FIFO order. Separate streams are independent and unordered relative to each other. |
| Stream `0` | Stream `0` (the link-level stream) carries no session. The server `MUST` reject every request on stream `0` with `NO_SESSION`. |
| Stream close | Either peer may close a stream with `OpConnClose`, which terminates the session and triggers teardown (§8.3). |

Multi-chunk message fragmentation and reassembly across ring slots are handled transparently by the HQQ `AdvancedLink` layer.

## 3. Primitive encodings

All integer fields are fixed-width little-endian values packed tightly without padding or alignment bytes. Every request and response body is a contiguous concatenation of the primitive and composite types below:

| Type | Byte Layout |
| :--- | :--- |
| `u8`, `u16`, `u32`, `u64` | Unsigned little-endian integers of 1, 2, 4, and 8 bytes. |
| `i64` | Two's-complement signed little-endian integer, 8 bytes. |
| `f64` | IEEE-754 binary64 little-endian bit pattern, 8 bytes. `NaN` is never transmitted (SQLite converts `NaN` to `NULL`). |
| `string` | `u32 n` byte length followed by `n` raw bytes (no NUL terminator). SQL text and identifiers are UTF-8; `n` `MAY` be `0`. |
| `Value` | `u8 tag` followed by the tag-specific payload defined below. |
| `Param` | `u32 ordinal`, `string name`, `Value`. |
| `Params` | `u32 count` followed by `count` consecutive `Param` records. |
| `Column` | `string name`, `string decltype` (`decltype` is empty when `sqlite3_column_decltype` returns `NULL`). |
| `Columns` | `u32 count` followed by `count` consecutive `Column` records. |
| `Batch` | `u32 nrows` followed by `nrows` consecutive rows, where each row consists of `ncols` consecutive `Value` records in column order. |

`Value` tags match SQLite's fundamental storage-class constants (`SQLITE_INTEGER` through `SQLITE_NULL`), allowing a C server to pass `sqlite3_column_type()` directly as the wire tag:

| Tag | Storage Class | Payload Layout |
| ---: | :--- | :--- |
| `1` | `INTEGER` | `i64` |
| `2` | `FLOAT` | `f64` |
| `3` | `TEXT` | `string` (UTF-8 bytes from `sqlite3_column_text()`) |
| `4` | `BLOB` | `u32 n` followed by `n` raw bytes |
| `5` | `NULL` | 0 bytes (no payload) |

Any other tag value is rejected with `BAD_REQUEST`.

In `Param`, `ordinal` is the 1-based argument position assigned by `database/sql`, and `name` is the parameter name stripped of its prefix character (`:id`, `@id`, or `$id` is transmitted as `"id"`), or `""` for positional parameters.

## 4. Parameter binding

For each `Param` in a request's `Params` list, evaluated in wire order:

1. **Positional parameter (`name == ""`)**: bind at 1-based index `ordinal` via `sqlite3_bind_*(stmt, ordinal, ...)`. If `ordinal` exceeds `sqlite3_bind_parameter_count(stmt)`, fail with `SQLITE_RANGE` (`25`) and message `"parameter index out of range"`.
2. **Named parameter (`name != ""`)**: look up the parameter index with `sqlite3_bind_parameter_index` in the order `":" + name`, `"@" + name`, `"$" + name`, selecting the first non-zero index. If all three return `0`, fail with `SQLITE_RANGE` (`25`) and message `"no such parameter: " + name`. The `ordinal` field of a named parameter is ignored for binding but `MUST` still be unique within the request and within `1..MaxParams`.
3. **Unbound parameters**: any statement parameter not bound by the request remains `NULL` (SQLite's default after `sqlite3_reset` / `sqlite3_clear_bindings`).

C API calls by `Value` tag:

| Tag | SQLite C Binding Call |
| :--- | :--- |
| `INTEGER` | `sqlite3_bind_int64(stmt, i, val)` |
| `FLOAT` | `sqlite3_bind_double(stmt, i, val)` |
| `TEXT` | `sqlite3_bind_text64(stmt, i, bytes, n, SQLITE_TRANSIENT, SQLITE_UTF8)` |
| `BLOB` | `sqlite3_bind_blob64(stmt, i, bytes, n, SQLITE_TRANSIENT)` (or `sqlite3_bind_zeroblob64(stmt, i, 0)` when `n == 0`) |
| `NULL` | `sqlite3_bind_null(stmt, i)` |

Duplicate `ordinal` values within a request, `ordinal == 0`, `ordinal > MaxParams`, or `count > MaxParams` are rejected with `BAD_REQUEST` or `LIMIT`.

## 5. Results: columns, cursors, batches

A query returns its column metadata once in the `Query` / `QueryStmt` response alongside the first row batch, and subsequent rows are pulled via `Fetch`:

- **`Columns`**: populated from `sqlite3_column_count`, `sqlite3_column_name`, and `sqlite3_column_decltype` (`NULL` declared types are encoded as `""`).
- **Row count bound**: a batch contains at most `n` rows, where `n` is the effective fetch count (`Query.fetch` or `Fetch.nrows`, clamped to `1..MaxFetchRows`, with `0` selecting `DefaultFetchRows = 256`).
- **Byte budget (`MaxBatchBytes`)**: a batch also stops before stepping the next row once its encoded byte length (the 4-byte `nrows` counter plus encoded rows) reaches or exceeds `MaxBatchBytes` (`1 MiB`). At least one row is always included in a non-empty batch if that row fits within `MaxMessageBytes`.
- **On-demand stepping**: the server calls `sqlite3_step()` one row at a time as it fills the response batch and stops stepping as soon as the row limit or byte budget is reached, without reading ahead.
- **`more = 1`**: indicates that the cursor remains open on the server because the batch filled before `sqlite3_step()` returned `SQLITE_DONE`. Because the server does not look ahead after a full batch, a query whose total rows are an exact multiple of the batch size returns a final batch with `nrows = 0` and `more = 0` on the next `Fetch`.
- **`more = 0`**: indicates `SQLITE_DONE` was reached. The server resets or finalizes the statement and releases the cursor ID automatically; the client `MUST NOT` send `CloseCursor` when `more == 0`.
- **Column-less statements**: a statement with `sqlite3_column_count == 0` (`INSERT`, `UPDATE`, `DELETE`, DDL) sent via `Query` steps once to completion and returns `cursor = 0`, `0` columns, `more = 0`, and `nrows = 0`.
- **Cursor ID invariant**: `cursor != 0` in a `Query` / `QueryStmt` response if and only if `more == 1`.
- **Oversize rows**: if a single row would cause the response payload to exceed `MaxMessageBytes` (`16 MiB`), the request fails with `SQLITE_TOOBIG` (`18`, `"row exceeds message size limit"`) and the cursor is released.

## 6. Methods

The request method ID is carried in the HQQ request's `MethodID` (`aux`) field. Method `0` and any unlisted ID return `UNKNOWN_METHOD`.

| ID | Method | Request Body | Response Body | Session State Transition |
| ---: | :--- | :--- | :--- | :--- |
| `1` | `Open` | see §6.1 | see §6.1 | `CLOSED` -> `OPEN` |
| `2` | `Exec` | `string sql`, `Params` | `i64 rows_affected`, `i64 last_insert_rowid` | `OPEN` |
| `3` | `Query` | `string sql`, `Params`, `u32 fetch` | `u32 cursor`, `Columns`, `u8 more`, `Batch` | `OPEN` |
| `4` | `Fetch` | `u32 cursor`, `u32 nrows` | `u8 more`, `Batch` | `OPEN` |
| `5` | `CloseCursor` | `u32 cursor` | empty | `OPEN` |
| `6` | `Prepare` | `string sql` | `u32 stmt`, `u32 nparams`, `Columns` | `OPEN` |
| `7` | `ExecStmt` | `u32 stmt`, `Params` | `i64 rows_affected`, `i64 last_insert_rowid` | `OPEN` |
| `8` | `QueryStmt` | `u32 stmt`, `Params`, `u32 fetch` | same as `Query` | `OPEN` |
| `9` | `CloseStmt` | `u32 stmt` | empty | `OPEN` |
| `10` | `Begin` | `u8 mode`, `u8 read_only` | empty | `OPEN` |
| `11` | `Commit` | empty | empty | `OPEN` |
| `12` | `Rollback` | empty | empty | `OPEN` |
| `13` | `Ping` | empty | empty | `OPEN` |
| `14` | `Close` | empty | empty | any -> `CLOSED` |

Successful responses use status `0`; error responses follow §7.

### 6.1 Open

Request layout:

```text
u16 major          client protocol major (must be 1)
u16 minor          client protocol minor
string db          database alias or path, 1..MaxPathBytes bytes
u32 flags          SQLITE_OPEN_* bits: READONLY 0x1, READWRITE 0x2, CREATE 0x4
u32 busy_ms        busy timeout in milliseconds (0 = fail at once)
u32 npragmas       0..MaxPragmas
npragmas times:
  string name      matches [a-z_][a-z0-9_]{0,63}
  string value     matches [A-Za-z0-9_.+-]{0,255}
```

Response layout:

```text
u16 major          server protocol major (1)
u16 minor          server protocol minor (0)
u32 max_fetch_rows MaxFetchRows
u32 max_params     MaxParams
```

Validation order and rules:

1. **Session state and major version**: if the session is already `OPEN`, fail with `ALREADY_OPEN`. Next, inspect the first two bytes (`major`): if `major != 1`, fail immediately with `VERSION` (`"unsupported protocol major version"`) without parsing the remainder of the body so future major versions can alter the layout freely.
2. **Body and limits**: exactly one of `READONLY` (`0x1`) or `READWRITE` (`0x2`) `MUST` be set, and `CREATE` (`0x4`) is optional (any other flag combination returns `BAD_REQUEST`). Pragma names and values are validated against their character-set regexes (`BAD_REQUEST` otherwise) and bounded by `MaxPragmas` (`LIMIT` otherwise).
3. **Database policy and `sqlite3_open_v2`**: the server resolves `db` against configured `--alias` names first, falling back to a relative path inside `--allow-dir` only when `--allow-dir` is enabled (`FORBIDDEN` otherwise). If the file does not exist and `CREATE` is not set, SQLite returns `SQLITE_CANTOPEN` (`14`).
4. **Session configuration**: applies `sqlite3_busy_timeout(db, busy_ms)` and executes each validated `PRAGMA <name> = <value>;`. If any step fails, the server closes the `sqlite3` handle and keeps the session in state `CLOSED`.

### 6.2 Exec

`sql` `MUST` contain exactly one SQL statement. Trailing whitespace, SQL comments, and a single trailing `;` are allowed; additional statements fail with `BAD_REQUEST` (`"multiple statements"`), and an empty/comment-only string fails with `BAD_REQUEST` (`"empty statement"`).

The server prepares the statement, binds `Params` (§4), steps to `SQLITE_DONE` (discarding any result rows), finalizes the statement, and returns `rows_affected = sqlite3_changes64()` and `last_insert_rowid = sqlite3_last_insert_rowid()`.

### 6.3 Query and QueryStmt

Enforces the same single-statement rule as `Exec`. `fetch == 0` selects `DefaultFetchRows` (`256`), and values above `MaxFetchRows` (`4096`) are clamped. The server binds `Params`, steps the first batch (§5), and returns `(cursor, Columns, more, Batch)`. For `QueryStmt`, the existing prepared statement `stmt` is reset and cleared before binding, and any previously open cursor on `stmt` is closed first (§8.2).

### 6.4 Fetch

Steps and returns the next batch of rows from open cursor `cursor`. `nrows == 0` reuses the fetch size from the originating `Query`, and values above `MaxFetchRows` are clamped. Passing an unknown or already-closed cursor ID returns `NO_CURSOR`. If `sqlite3_step()` returns an error, the cursor is closed and the SQLite error is returned.

### 6.5 CloseCursor

Closes an open cursor by resetting its underlying statement (or finalizing the temporary statement of an ad-hoc `Query`). Passing an unknown or already-closed cursor ID returns `NO_CURSOR`.

### 6.6 Prepare

Prepares a single SQL statement under the same single-statement rule as `Exec` and returns `(stmt, nparams, Columns)`. Statement IDs and cursor IDs occupy independent namespaces (§8.2).

### 6.7 ExecStmt

Closes any open cursor on `stmt`, resets and clears `stmt`, binds `Params`, steps to `SQLITE_DONE`, resets `stmt`, and returns `(rows_affected, last_insert_rowid)`.

### 6.8 CloseStmt

Closes any open cursor associated with `stmt` and finalizes `stmt`. Passing an unknown or already-closed statement ID returns `NO_STATEMENT`.

### 6.9 Begin

`mode` selects `0` (`DEFERRED`), `1` (`IMMEDIATE`), or `2` (`EXCLUSIVE`); any other value returns `BAD_REQUEST`. `read_only` is `0` or `1`; when `read_only == 1`, `mode` `MUST` be `0` (`BAD_REQUEST` otherwise).

- **Read-write (`read_only == 0`)**: executes `BEGIN DEFERRED`, `BEGIN IMMEDIATE`, or `BEGIN EXCLUSIVE`.
- **Read-only (`read_only == 1`)**: saves the current value of `PRAGMA query_only`, executes `BEGIN DEFERRED`, and sets `PRAGMA query_only = 1`. Subsequent write attempts inside the transaction fail with `SQLITE_READONLY` (`8`), and `Commit` / `Rollback` restore the saved `query_only` setting.

Calling `Begin` while a transaction is already active returns `SQLITE_ERROR` (`1`, `"cannot start a transaction within a transaction"`).

### 6.10 Commit and Rollback

`Commit` runs `COMMIT` (failing with `SQLITE_ERROR` if no transaction is active). `Rollback` runs `ROLLBACK` when a transaction is active and succeeds as a no-op when already in autocommit mode. If `COMMIT` fails with `SQLITE_BUSY`, SQLite leaves the transaction open; the client `MUST` issue `Rollback` to abort it. Both operations restore `PRAGMA query_only` if modified by a read-only `Begin`.

### 6.11 Ping

Returns an empty success response when the session is `OPEN` (`NO_SESSION` when `CLOSED`).

### 6.12 Close

Idempotently tears down the session (§8.3): rolls back any active transaction, finalizes all cursors and prepared statements, closes the `sqlite3` handle, transitions the session to `CLOSED`, and returns an empty success response.

## 7. Status codes and error responses

Error responses carry a non-zero `status` in the HQQ response's `aux` word and an unprefixed UTF-8 error string as the response payload:

| Status Range | Meaning |
| :--- | :--- |
| `0x10000 + rc` | SQLite result code `rc` (primary code such as `SQLITE_BUSY = 5`, or extended code such as `SQLITE_CONSTRAINT_UNIQUE = 2067` -> `0x10813`). Payload is `sqlite3_errmsg()`. |
| `0x20000 + code` | Service protocol error code from the table below. |

Status values below `0x10000` are reserved for HQQ transport-level error codes (`0x01`..`0x08`) and `MUST NOT` be emitted by application handlers.

Service protocol error codes (`0x20000 + code`):

| Code | Name | Condition |
| ---: | :--- | :--- |
| `1` | `BAD_REQUEST` | Malformed request body, trailing bytes, invalid flag/mode, empty or multi-statement SQL, or invalid pragma syntax. |
| `2` | `UNKNOWN_METHOD` | Request `MethodID` is not in `1..14`. |
| `3` | `NO_SESSION` | Method other than `Open` or `Close` called while session is `CLOSED`, or any request sent on stream `0`. |
| `4` | `ALREADY_OPEN` | `Open` called while session is already `OPEN`. |
| `5` | `NO_STATEMENT` | Unknown or already-closed `stmt` ID. |
| `6` | `NO_CURSOR` | Unknown or already-released `cursor` ID. |
| `7` | `LIMIT` | Request or session exceeded a resource bound in §11. |
| `8` | `VERSION` | Unsupported client protocol major version in `Open`. |
| `9` | `FORBIDDEN` | Database name rejected by the server's `--alias` / `--allow-dir` policy. |
| `10` | `INTERNAL` | Non-SQLite server failure (such as server heap allocation failure). |

Returning an error response leaves the session in state `OPEN` and does not automatically roll back an active transaction (unless SQLite itself rolled back, such as on `SQLITE_FULL`).

## 8. Session, transaction, statement, and cursor state

Each stream maintains an isolated state machine governing its connection, prepared statements, cursors, and transaction mode.

### 8.1 Session states

```text
CLOSED --Open ok--> OPEN
OPEN   --Close----> CLOSED   (response success, teardown of section 8.3)
OPEN   --stream closed by either side, link lost--> CLOSED (teardown, no response)
```

A failed `Open` leaves the session `CLOSED`. Any method other than `Open` or `Close` in state `CLOSED` returns `NO_SESSION`.

### 8.2 Statements and cursors

- Statement IDs (`Prepare`) and cursor IDs (`Query` / `QueryStmt` with `more == 1`) are allocated from two independent per-session monotonic `u32` counters starting at `1`. IDs are never reused within a session, `0` is never valid, and counter exhaustion at $2^{32}-1$ returns `LIMIT`.
- Each session allows at most `MaxStatementsPerSession` (`256`) open prepared statements and `MaxCursorsPerSession` (`64`) open cursors simultaneously.
- Ad-hoc `Query` and `Exec` calls prepare an internal statement that is finalized automatically when the call completes (or when the `Query` cursor closes).
- Each prepared statement supports at most one open cursor at a time: calling `QueryStmt`, `ExecStmt`, or `CloseStmt` on a statement with an open cursor automatically closes that cursor first (causing subsequent `Fetch` calls on the old cursor ID to return `NO_CURSOR`).

### 8.3 Teardown

Session teardown runs on `Close`, stream closure (`OpConnClose`), or link loss (`ErrPeerGone` / `ErrPeerClosed`) in the following sequence:

1. Interrupt any currently executing request via `sqlite3_interrupt()` (§9).
2. Finalize or reset all open cursors and prepared statements.
3. If a transaction is active (`sqlite3_get_autocommit(db) == 0`), execute `ROLLBACK` and restore `query_only` if modified.
4. Close the database handle with `sqlite3_close_v2(db)`.

The server `MUST` complete teardown within `PeerTimeout` of a client process crash (§10).

### 8.4 Transaction state

Transaction state is determined directly by `sqlite3_get_autocommit(db)`, whether started via the `Begin` method or raw `BEGIN` SQL and whether ended via `Commit`/`Rollback` or raw `COMMIT`/`ROLLBACK` SQL.

## 9. Cancellation and deadlines

Mid-flight query cancellation uses HQQ Advanced Protocol 1.1 (`FeatureCancel` and `FeatureMetadata`):

- **Client**: when the caller's `ctx` expires while awaiting a response, the client transmits `OpCancel` for the in-flight request, closes the session stream (triggering server teardown per §8.3), and returns `ctx.Err()`.
- **Server**: when a request handler's context is canceled (either by `OpCancel` or by expiry of the deadline propagated via `FeatureMetadata`), the server `MUST` invoke `sqlite3_interrupt(db)`, check context cancellation between `sqlite3_step()` iterations, release any cursor being populated, and suppress the response frame.

## 10. Failures

| Failure Event | Client Observation | Server Action |
| :--- | :--- | :--- |
| Client process crashes or hangs | None (client terminated) | Detects `ErrPeerGone` (or stream close) within `PeerTimeout` and runs §8.3 teardown, rolling back uncommitted work. |
| Client closes session stream | None | Runs §8.3 teardown with no response frame. |
| Server process crashes | `ErrPeerGone` on active or next call | SQLite recovers WAL / rollback journal on next database open; uncommitted transactions are rolled back. |
| Server shuts down gracefully | `ErrPeerClosed` (or `ErrShuttingDown` during drain) | Drains active requests up to shutdown timeout, then runs §8.3 teardown across all sessions. |
| Link heartbeat timeout | `ErrPeerGone` | Runs §8.3 teardown across all streams on that link. |

The Go driver maps `ErrPeerGone`, `ErrPeerClosed`, and `ErrClosed` to `driver.ErrBadConn` on idempotent/read operations (`Open`, `Prepare`, `Begin`, `Query`, `QueryStmt`, `Fetch`, `Ping`, `Commit`, `Rollback`, and session reset), while mapping link loss during `Exec` or `ExecStmt` to `sqlite.ErrOutcomeUnknown` so `database/sql` never blindly re-runs a write whose commit status is unknown.

## 11. Limits

| Limit Name | Value | Enforced By | Violation Behavior |
| :--- | :--- | :--- | :--- |
| `MaxMessageBytes` | `16 MiB` (`16,777,216`) | Client and server | `LIMIT` on server; rejected before send on client |
| `MaxSQLBytes` | `1 MiB` (`1,048,576`) | Server (`SQLITE_LIMIT_SQL_LENGTH`) | `LIMIT` |
| `MaxParams` | `32,766` | Server (`SQLITE_LIMIT_VARIABLE_NUMBER`) | `BAD_REQUEST` / `LIMIT` |
| `MaxColumns` | `2,000` | Server (`SQLITE_LIMIT_COLUMN`) | `LIMIT` |
| `MaxFetchRows` | `4,096` (`DefaultFetchRows = 256`) | Server | Clamped to `4,096` |
| `MaxBatchBytes` | `1 MiB` (`1,048,576`) | Server (§5 batching) | Splits batch before next step |
| `MaxNameBytes` | `1,024` | Server (param & column names) | `LIMIT` |
| `MaxPathBytes` | `4,096` | Server (`Open.db`) | `LIMIT` |
| `MaxPragmas` | `32` | Server (`Open.npragmas`) | `LIMIT` |
| `MaxStatementsPerSession` | `256` | Server | `LIMIT` |
| `MaxCursorsPerSession` | `64` | Server | `LIMIT` |
| `MaxSessions` | Default `256` (configurable) | Server (`Open`) | `LIMIT` |

## 12. Trust model

- **Same-host trusted clients**: OS access control on the HQQ listener segment (`AcceptorConfig.Perm`, default `0o600`) determines which local users can connect.
- **Database path sandboxing**: the server opens only configured `--alias` targets and (when `--allow-dir` is set) canonical paths strictly inside `--allow-dir`.
- **Disabled file/code escapes**: the server disables `sqlite3_load_extension`, sets `SQLITE_LIMIT_ATTACHED` to `0`, and installs a SQLite authorizer that rejects `SQLITE_ATTACH` and `SQLITE_DETACH` so a client cannot open arbitrary files on the host.
- **Validated pragmas**: pragma names and values in `Open` are restricted to strict alphanumeric patterns (§6.1) to prevent SQL injection.

## 13. Server implementation notes (C)

- Dedicate one worker thread per session so requests on a session execute serially in FIFO order while independent sessions run in parallel across cores.
- Register a cancellation notifier on the HQQ request context that triggers `sqlite3_interrupt(db)` while the session worker is inside `sqlite3_step()`.
- Release temporary statements and cursors on every error exit path.

## 14. Versioning

This specification defines major version `1`, minor version `0`. Breaking changes to wire layouts, method IDs, or status codes increment `major`; backward-compatible additions increment `minor`.

## 15. Design notes

- **Pull-based row streaming (`Fetch`)**: rows are pulled by the client via `Query` and `Fetch` RPC calls rather than pushed asynchronously via `Stream.Send`. Under HQQ's default `OverflowBlock` policy, a full stream queue pauses the link-wide receive path; pull-based batching guarantees that a slow consumer on one stream can never stall other sessions sharing the link.
- **Stream lifecycle monitoring**: the server parks a watcher on `Stream.Recv` for each session stream. Because clients only send RPC requests (`Stream.Call`) and never plain data messages (`Stream.Send`), `Stream.Recv` unblocks only when the stream closes or the link fails, immediately triggering §8.3 teardown.
- **Connection pool hygiene**: when a connection is returned to `database/sql`'s pool, `ResetSession` issues a `Rollback` (a fast no-op outside a transaction) so transactions started via raw `BEGIN` SQL cannot leak into subsequent callers.

## 16. Worked examples

The hexadecimal byte vectors below are generated by `sqlite/internal/wire` and verified in CI by `TestGoldenExamples` (`sqlite/internal/wire/wire_test.go`). Spaces separate logical fields for readability:

1. **`Open` request** for database `"main"`, flags `READWRITE | CREATE` (`0x6`), `busy_ms = 5000`, and one pragma `journal_mode = WAL`:

```text
0100 0000                 major 1, minor 0
04000000 6d61696e         db = "main"
06000000                  flags = 0x2 | 0x4
88130000                  busy_ms = 5000
01000000                  npragmas = 1
0c000000 6a6f75726e616c5f6d6f6465   name = "journal_mode"
03000000 57414c           value = "WAL"
```

2. **`Exec` request** for `INSERT INTO t VALUES (?)` with one positional parameter `42`:

```text
18000000 494e5345525420494e544f20742056414c55455320283f29   sql = "INSERT INTO t VALUES (?)"
01000000                  nparams = 1
01000000 00000000         ordinal = 1, name = "" (positional)
01 2a00000000000000       INTEGER 42
```

3. **`Query` response** for `SELECT n FROM t` returning one column `"n"` and one completed row `42` (`cursor = 0`, `more = 0`):

```text
00000000                  cursor = 0 (none)
01000000                  ncolumns = 1
01000000 6e 00000000      column name = "n", decltype = ""
00                        more = 0
01000000                  nrows = 1
01 2a00000000000000       INTEGER 42
```

4. **Consecutive encoded `Value` records**: `TEXT "hi"`, `NULL`, and `FLOAT 1.5`:

```text
03 02000000 6869          TEXT "hi"
05                        NULL
02 000000000000f83f       FLOAT 1.5
```
