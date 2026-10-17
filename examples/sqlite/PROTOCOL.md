# hqq-sqlite wire protocol, version 1.0

This document is the contract between the Go client (`examples/sqlite/driver`,
driver name `hqq-sqlite`), the Go fake server (`examples/sqlite/fakeserver`),
and the C server (`examples/sqlite/server`, built on libhqq and SQLite, system
library or amalgamation). The Go encoder and decoder in `examples/sqlite/wire` implement
exactly the byte layouts below. A C implementer needs nothing else.

Key words MUST, MUST NOT, SHOULD, and MAY are used as in RFC 2119.

## 1. Overview

A SQLite service runs as its own process. Clients attach to it through one
HQQ listener (the service name). Every `database/sql` connection is one HQQ
**stream** of a shared link. The server runs each stream as one **session**
with its own SQLite connection, its own transaction state, and its own
statement and cursor tables. Sessions never share state. Sessions opened on the
same database file share the data through SQLite's normal locking.

Each request is one HQQ request message on a stream. Each request gets exactly
one response: a success response with a body, or an error response
(`ResponseError`). Requests on one stream are sent one at a time by the client:
the client MUST NOT send a request on a stream while a previous request on that
stream is unanswered, except for the cancel path in section 9.

## 2. Transport (HQQ)

| Item | Requirement |
|---|---|
| Listener | One acceptor per service name. Clients connect with `hqq.Connect(ctx, name, cfg)`. |
| Link features | The link MUST negotiate `FeatureRequestResponse` and `FeatureCancel`. `FeatureMetadata` is optional and carries the caller's remaining deadline (section 9). |
| Message size | Both endpoints MUST set `AdvancedOptions.MaxMessageSize` to at least 16 MiB (`MaxMessageBytes`, section 11). |
| Streams | The client opens one stream per `driver.Conn` (`OpenStream`). The server accepts each one with `AcceptStream` and registers a session. The server MUST call `ListenStreams` on each accepted link. A client may open a stream before the server calls `ListenStreams`. The request then waits until `ListenStreams` runs, so `OpenStream` simply completes later (bounded by its context). A server that never calls `ListenStreams` refuses the open with `ConnNotFound`. That is a configuration error, and the client does not retry it. |
| Requests | Request message: `AdvFlagRequest` set, `aux` = method id (section 6), payload = request body. |
| Responses | Response message: `aux` = status (0 for success; section 7), payload = response body (success) or UTF-8 error message (error). |
| Ordering | Requests and responses on one stream are in order. Different streams have no ordering relation. |
| Stream 0 | The link-level stream 0 carries no session. The server MUST answer every request on stream 0 with `NO_SESSION`. |
| Stream close | Either side may close a stream. Closing it ends the session (section 8.3). |

Message chunking (larger than the link buffer size) is internal to HQQ. The
protocol never sees chunks.

## 3. Primitive encodings

All integers are fixed width and little-endian. There is no padding and no
alignment. Every body is a concatenation of the layouts below.

| Type | Layout |
|---|---|
| `u8`, `u16`, `u32`, `u64` | unsigned integers, 1, 2, 4, 8 bytes |
| `i64` | two's complement, 8 bytes |
| `f64` | IEEE 754 binary64 bit pattern, 8 bytes. NaN is never sent (SQLite stores NaN as NULL). |
| `string` | `u32 n` followed by `n` bytes. No terminator. SQL text and names are UTF-8. `n` MAY be 0. |
| `Value` | `u8 tag` followed by a payload selected by the tag (table below). |
| `Param` | `u32 ordinal`, `string name`, `Value`. |
| `Params` | `u32 count`, then `count` × `Param`. |
| `Column` | `string name`, `string decltype`. `decltype` is empty when SQLite has none. |
| `Columns` | `u32 count`, then `count` × `Column`. |
| `Batch` | `u32 nrows`, then `nrows` rows. Each row is `ncols` × `Value`, in column order. `ncols` is the column count of the cursor's `Columns`. |

Value tags are the SQLite fundamental type codes, so a C server can pass
`sqlite3_column_type()` straight through:

| Tag | Type | Payload |
|---|---|---|
| 1 | INTEGER | `i64` |
| 2 | FLOAT | `f64` |
| 3 | TEXT | `string` (UTF-8 bytes; the server sends the `sqlite3_column_text()` bytes) |
| 4 | BLOB | `string`-shaped: `u32 n` followed by `n` raw bytes |
| 5 | NULL | no payload |

Any other tag is `BAD_REQUEST`.

Parameter `ordinal` is the 1-based position of the argument in the argument
list, as `database/sql` numbers it. `name` is the parameter name **without**
its prefix character (for `:id` the name is `id`), or empty for a positional
argument. Section 4 defines binding.

## 4. Parameter binding

For each `Param` in a request, in the order sent:

1. If `name` is empty, bind at index `ordinal` (`sqlite3_bind_*(stmt, ordinal, ...)`).
   If `ordinal` is greater than the statement's parameter count, fail with
   `SQLITE_RANGE` (25), message `"parameter index out of range"`.
2. If `name` is not empty, look up the index with `sqlite3_bind_parameter_index`
   for `":" + name`, then `"@" + name`, then `"$" + name`, taking the first
   non-zero result. If none matches, fail with `SQLITE_RANGE` (25), message
   `"no such parameter: " + name`. The `ordinal` of a named param is ignored
   except that it MUST be unique within the request.
3. Parameters not sent are NULL (SQLite default).

Binding by type:

| Tag | Call |
|---|---|
| INTEGER | `sqlite3_bind_int64` |
| FLOAT | `sqlite3_bind_double` |
| TEXT | `sqlite3_bind_text64(stmt, i, bytes, n, SQLITE_TRANSIENT, SQLITE_UTF8)` |
| BLOB | `sqlite3_bind_blob64(stmt, i, bytes, n, SQLITE_TRANSIENT)`; for `n == 0` use `sqlite3_bind_zeroblob64(stmt, i, 0)` |
| NULL | `sqlite3_bind_null` |

Duplicate `ordinal` values in one request are `BAD_REQUEST`. `ordinal` MUST be
in `1..MaxParams` (`BAD_REQUEST` otherwise). `count` MUST be at most
`MaxParams`.

## 5. Results: columns, cursors, batches

A query returns its column metadata once, then its rows in batches.

- `Columns` come from `sqlite3_column_count`, `sqlite3_column_name`, and
  `sqlite3_column_decltype` (NULL decltype is sent as the empty string).
- A **batch** holds at most `n` rows, where `n` is the effective fetch size
  (section 6, `Query` and `Fetch`).
- A batch also stops, before stepping the next row, when the batch's encoded
  size (its 4-byte row count plus its rows) is at least `MaxBatchBytes`. The
  first row of a batch is always included.
- Rows come from `sqlite3_step()`, one at a time, and the server steps only
  when the batch needs another row. A batch that has reached its row count or
  its byte budget stops without stepping again. The server never reads ahead of
  the rows it returns, so a cursor holds no buffered rows.
- `more = 1` means the cursor is still open on the server. It is set when the
  batch stopped because it was full (row count or byte budget) and the
  statement was not yet at `SQLITE_DONE`. A final batch MAY be empty
  (`nrows = 0`) with `more = 0`.
- `more = 0` means `SQLITE_DONE` was reached. The server then finalizes or
  resets the statement and releases the cursor id. The client MUST NOT send
  `CloseCursor` for a cursor that answered `more = 0`.
- A statement that returns no columns (INSERT, UPDATE, DELETE, DDL) is run to
  completion by the first step. Its `Query` response has `cursor = 0`, zero
  columns, `more = 0`, and an empty batch.
- `Exec` steps the statement to `SQLITE_DONE` and discards any rows.
- A cursor id is non-zero if and only if the response has `more = 1`.

Encoding one row: each value is encoded in column order. The encoder MUST check
that the response payload stays within `MaxMessageBytes`. If one row alone
would exceed it, the request fails with `SQLITE_TOOBIG` (18), message
`"row exceeds message size limit"`, and the cursor is released.

## 6. Methods

Method ids are carried in the request's `aux`. Method 0 and any id not listed
are `UNKNOWN_METHOD`.

| Id | Name | Request body | Response body | Session state |
|---|---|---|---|---|
| 1 | Open | section 6.1 | section 6.1 | CLOSED → OPEN |
| 2 | Exec | `string sql`, `Params` | `i64 rows_affected`, `i64 last_insert_rowid` | OPEN |
| 3 | Query | `string sql`, `Params`, `u32 fetch` | `u32 cursor`, `Columns`, `u8 more`, `Batch` | OPEN |
| 4 | Fetch | `u32 cursor`, `u32 nrows` | `u8 more`, `Batch` | OPEN |
| 5 | CloseCursor | `u32 cursor` | empty | OPEN |
| 6 | Prepare | `string sql` | `u32 stmt`, `u32 nparams`, `Columns` | OPEN |
| 7 | ExecStmt | `u32 stmt`, `Params` | `i64 rows_affected`, `i64 last_insert_rowid` | OPEN |
| 8 | QueryStmt | `u32 stmt`, `Params`, `u32 fetch` | as Query | OPEN |
| 9 | CloseStmt | `u32 stmt` | empty | OPEN |
| 10 | Begin | `u8 mode`, `u8 read_only` | empty | OPEN |
| 11 | Commit | empty | empty | OPEN |
| 12 | Rollback | empty | empty | OPEN |
| 13 | Ping | empty | empty | OPEN |
| 14 | Close | empty | empty | any → CLOSED |

A success response has status 0. Error responses are in section 7.

### 6.1 Open

Request:

```
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

Response:

```
u16 major          server protocol major (1)
u16 minor          server protocol minor (0)
u32 max_fetch_rows MaxFetchRows
u32 max_params     MaxParams
```

Checks run in this order, and the first failure is the reply:

1. The major version: the first two bytes of the body. If `major` is not 1,
   the reply is `VERSION`. Only these two bytes are read, so a later major
   version may change the rest of the layout.
2. The body: any violation of the rules below is `BAD_REQUEST` or `LIMIT`.
3. The database policy (`FORBIDDEN`), the session limit (`LIMIT`), and the open
   itself (SQLite codes, for example `SQLITE_CANTOPEN`).

Rules:

- Exactly one of READONLY (0x1) and READWRITE (0x2) MUST be set; otherwise
  `BAD_REQUEST`. CREATE (0x4) is optional. Other bits are `BAD_REQUEST`.
- If `major` is not 1, the reply is `VERSION` with the message
  `"unsupported protocol major version"`, and the session stays CLOSED.
- The effective minor is `min(client minor, server minor)`. Version 1.0 has no
  minor-dependent features.
- The server resolves `db` as an alias from its configuration first. If no alias
  matches, it uses the value as a path only when the server policy allows raw
  paths. Otherwise the reply is `FORBIDDEN`.
- If the resolved database does not exist and CREATE is not set, the reply is
  SQLite `SQLITE_CANTOPEN` (14).
- Open applies `busy_ms` with `sqlite3_busy_timeout`. It applies each pragma as
  `PRAGMA <name> = <value>;`. Pragma names and values are validated before the
  SQL is built, so no other SQL can be injected.
- If Open succeeds, the session is OPEN. If it fails, the session stays CLOSED,
  and the server MUST close the SQLite connection it may have opened.
- Open in state OPEN is `ALREADY_OPEN`.

### 6.2 Exec

`sql` MUST contain exactly one statement. Trailing whitespace and `;` are
allowed. Any other trailing text is `BAD_REQUEST` with message
`"multiple statements"`. Text with no statement is `BAD_REQUEST` with message
`"empty statement"`.

The server prepares the statement, binds `Params` (section 4), steps to
`SQLITE_DONE`, and finalizes it. `rows_affected` is `sqlite3_changes64()`
after the step. `last_insert_rowid` is `sqlite3_last_insert_rowid()`.

### 6.3 Query and QueryStmt

Same rules as Exec for the SQL text. The `fetch` argument is the batch row
count `n`: 0 means `DefaultFetchRows` (256), and values above `MaxFetchRows`
are clamped to it. The server then binds, steps, and returns the first batch
(section 5). For QueryStmt the statement is an existing prepared statement
(section 8.2). It is reset before binding.

### 6.4 Fetch

Returns the next batch of an open cursor. `nrows` is the batch row count: 0 means
the cursor's fetch size from its Query, values above `MaxFetchRows` are
clamped. Unknown or released cursor: `NO_CURSOR`. If a step fails, the cursor
is released and the error is returned.

### 6.5 CloseCursor

Releases the cursor: the statement is reset (the internal statement of an
ad-hoc Query is finalized). An unknown or already released cursor id is
`NO_CURSOR`.

### 6.6 Prepare

Prepares one statement (same single-statement rule as Exec). The response
carries the new statement id, the parameter count
(`sqlite3_bind_parameter_count`), and the columns. The statement id and cursor
ids are separate id spaces (section 8.2).

### 6.7 ExecStmt

Like Exec, for the prepared statement `stmt`. Any open cursor of the statement is
closed first (section 8.2). The statement is reset before binding.

### 6.8 CloseStmt

Finalizes the statement and closes its open cursor, if any. Unknown id:
`NO_STATEMENT`.

### 6.9 Begin

`mode`: 0 DEFERRED, 1 IMMEDIATE, 2 EXCLUSIVE. Other values are `BAD_REQUEST`.
`read_only`: 0 or 1. When `read_only = 1`, `mode` MUST be 0 (otherwise
`BAD_REQUEST`).

- Read-write: execute `BEGIN <mode>`.
- Read-only: remember the current `query_only` value, execute `BEGIN DEFERRED`,
  then `PRAGMA query_only = 1`. COMMIT and ROLLBACK restore the remembered
  value. A write inside the transaction then fails with `SQLITE_READONLY` (8).

A Begin inside a transaction returns SQLite's error (`SQLITE_ERROR` 1,
`"cannot start a transaction within a transaction"`).

### 6.10 Commit and Rollback

Commit executes `COMMIT`, and Rollback executes `ROLLBACK` when a transaction is
active. Commit outside a transaction returns SQLite's error
(`SQLITE_ERROR`, `"cannot commit - no transaction is active"`). Rollback outside
a transaction is a successful no-op. This is the one place where the protocol
differs from SQLite's own `ROLLBACK` behavior.

If COMMIT fails with `SQLITE_BUSY`, SQLite keeps the transaction open. The
client MUST then send Rollback. The server does not do this itself.

Both restore `query_only` if a read-only Begin changed it (section 6.9).

### 6.11 Ping

Empty success response. Used to check that the session is alive.

### 6.12 Close

Ends the session. The server rolls back an active transaction, finalizes all
statements and cursors, and closes the SQLite connection, then answers success.
Afterwards the session is CLOSED. Close is idempotent: in state CLOSED it also
answers success.

## 7. Status codes and error responses

An error response carries `status` (the response `aux`) and a payload that is
the UTF-8 error message (no length prefix; the payload length is the message
length, and it MAY be empty).

| Status | Meaning |
|---|---|
| `0x10000` plus `rc` | A SQLite result code `rc`: primary (`SQLITE_BUSY` = 5) or extended (`SQLITE_CONSTRAINT_UNIQUE` = 2067), as returned by SQLite. For example `SQLITE_CONSTRAINT_UNIQUE` is status `0x10813`. The message is `sqlite3_errmsg()`. |
| `0x20000` plus `code` | A protocol error (table below). |

All status values below `0x10000` are reserved and MUST NOT be sent. They
would collide with HQQ's own error statuses.

SQLite codes the server can produce (from `sqlite3.h`): ERROR 1, BUSY 5,
LOCKED 6, NOMEM 7, READONLY 8, INTERRUPT 9, IOERR 10, CORRUPT 11, NOTFOUND 12,
FULL 13, CANTOPEN 14, PROTOCOL 15, SCHEMA 17, TOOBIG 18, CONSTRAINT 19,
MISMATCH 20, MISUSE 21, RANGE 25, NOTADB 26, and their extended forms
(`SQLITE_BUSY_TIMEOUT` 773, `SQLITE_CONSTRAINT_PRIMARYKEY` 1555,
`SQLITE_CONSTRAINT_NOTNULL` 1299, `SQLITE_CONSTRAINT_UNIQUE` 2067,
`SQLITE_CONSTRAINT_CHECK` 275, `SQLITE_CONSTRAINT_FOREIGNKEY` 787, and so on).
The server sends the code SQLite returned, unchanged.

Protocol errors:

| Code | Name | Used when |
|---|---|---|
| 1 | `BAD_REQUEST` | malformed body, trailing bytes, invalid argument, disallowed SQL shape (multiple or empty statements), invalid pragma name or value |
| 2 | `UNKNOWN_METHOD` | method id not in the table |
| 3 | `NO_SESSION` | method other than Open (or Close) in state CLOSED, or request on stream 0 |
| 4 | `ALREADY_OPEN` | Open in state OPEN |
| 5 | `NO_STATEMENT` | unknown or closed statement id |
| 6 | `NO_CURSOR` | unknown or released cursor id |
| 7 | `LIMIT` | a documented limit was exceeded (section 11) |
| 8 | `VERSION` | unsupported protocol major version |
| 9 | `FORBIDDEN` | database alias unknown and raw paths disabled |
| 10 | `INTERNAL` | server-side failure not covered by SQLite (for example out of memory in the server) |

An error response does not end the session. It does not roll back the
transaction either, although SQLite may roll back by itself for some errors
(for example `SQLITE_FULL`); the server reports the error and the client
decides. A Query or Fetch that fails releases its cursor (sections 6.3 and 6.4).

## 8. Session, transaction, statement, and cursor state

### 8.1 Session states

```
CLOSED --Open ok--> OPEN
OPEN   --Close----> CLOSED   (response success, teardown of section 8.3)
OPEN   --stream closed by either side, link lost--> CLOSED (teardown, no response)
```

An Open that fails keeps the session CLOSED. A method other than Open in state
CLOSED is `NO_SESSION`.

### 8.2 Statements and cursors

- Statement ids (Prepare) and cursor ids (Query with more = 1) are `u32`
  values from two separate counters per session. Both start at 1 and increase
  by one for each allocation. They are never reused within a session. A value
  of 0 is never a valid id. Exhausting the 2^32 range is `LIMIT`.
- A session has at most `MaxStatementsPerSession` statements and
  `MaxCursorsPerSession` open cursors.
- An ad-hoc Query or Exec (methods 2 and 3) prepares, runs, and finalizes an
  internal statement. A cursor from Query owns that internal statement, which
  is finalized when the cursor is released.
- A statement has at most one open cursor. QueryStmt and ExecStmt on a
  statement that has an open cursor first release that cursor. A later
  Fetch on the old cursor id is then `NO_CURSOR`.
- CloseStmt releases the statement's open cursor, if any.
- Many statements and cursors may be open at once, on the one connection.

### 8.3 Teardown

Teardown runs on Close, on stream close (by either side), and on link loss. In
this order:

1. Stop the running request, if any, by interrupting it (section 9).
2. If a transaction is active (`sqlite3_get_autocommit() == 0`), roll it back.
   Restore `query_only` if a read-only Begin changed it.
3. Finalize all cursors and statements.
4. Close the SQLite connection with `sqlite3_close_v2`.

Uncommitted work is never visible to other sessions, because SQLite keeps it
private until COMMIT. The server MUST complete teardown within PeerTimeout of
the client going away (section 10).

### 8.4 Transaction state

The session is in AUTOCOMMIT or IN TRANSACTION. The state is always the
SQLite truth, `sqlite3_get_autocommit()`. A transaction may be started by Begin
or by SQL text (`BEGIN`) and ended by Commit, Rollback, or SQL text
(`COMMIT`, `END`, `ROLLBACK`). The protocol does not track it separately.

## 9. Cancellation and deadlines

Cancellation uses HQQ protocol 1.1 (`FeatureCancel`):

- **Client.** If the caller's context ends while a request waits, the client
  sends `OpCancel` for that request and returns the context error. After
  that the client MUST NOT send another request on the stream. It closes
  the stream, and the server then tears the session down (section 8.3). HQQ
  discards the response to a canceled request.
- **Server.** When a request's handler context is canceled, the server MUST:
  1. call `sqlite3_interrupt()` on the session's connection, so the running
     `sqlite3_step()` returns `SQLITE_INTERRUPT` (9);
  2. check the context between steps, so an interrupt that arrives between
     statements is not lost;
  3. stop and release any cursor it was filling;
  4. NOT send a response. HQQ discards it anyway.
  The request does not end the session or the transaction by itself. The
  client's stream close does that (section 8.3).
- A request whose context ended before its handler started is not delivered to
  the handler at all.
- **Deadlines.** With `FeatureMetadata` the client sends the remaining time of
  its context. The handler context then has that deadline and cancels when it
  passes. Handle it exactly like a cancel. Use `sqlite3_interrupt()` as above.

## 10. Failures

| Event | Client observes | Server does |
|---|---|---|
| Client process crashes or hangs | nothing (client is gone) | teardown (section 8.3) within PeerTimeout (`ErrPeerGone`, or `ErrPeerClosed` on stream close). Uncommitted work is rolled back. |
| Client closes its stream | nothing | teardown, no response |
| Server crashes | `ErrPeerGone` on the next call or on a call in progress | SQLite recovers its files on next open (hot journal or WAL). Open transactions are lost. |
| Server closes gracefully | `ErrPeerClosed` | teardown of all sessions |
| Link lost (heartbeat stops) | `ErrPeerGone` | teardown of all sessions on the link |

The Go driver maps `ErrPeerGone`, `ErrPeerClosed`, and `ErrClosed` to
`driver.ErrBadConn`. It reconnects the shared link on the next use (see
`README.md`, section "Driver behavior").

**Unknown outcome.** If the connection is lost during Commit, the transaction
may or may not have been committed. The driver reports `driver.ErrBadConn` for
that Commit, and the caller MUST treat the outcome as unknown.

## 11. Limits

| Name | Value | Enforced by | Violation |
|---|---|---|---|
| `MaxMessageBytes` | 16 MiB (16 777 216) | both (request and response payload) | `LIMIT` (server); the client refuses before sending |
| `MaxSQLBytes` | 1 MiB (1 048 576) | server | `LIMIT` |
| `MaxParams` | 32 766 | server (`ordinal`, `count`) | `BAD_REQUEST` / `LIMIT` |
| `MaxColumns` | 2 000 | server | `LIMIT` |
| `MaxFetchRows` | 4 096; `DefaultFetchRows` 256 | server (clamps) | clamped, not an error |
| `MaxBatchBytes` | 1 MiB (1 048 576) | server (batch splitting, section 5) | not an error (batch splits) |
| `MaxNameBytes` | 1 024 | server (param name, column name) | `LIMIT` |
| `MaxPathBytes` | 4 096 | server (Open `db`) | `LIMIT` |
| `MaxPragmas` | 32 | server (Open) | `LIMIT` |
| `MaxStatementsPerSession` | 256 | server | `LIMIT` |
| `MaxCursorsPerSession` | 64 | server | `LIMIT` |
| `MaxSessions` | server policy, default 256 | server (Open) | `LIMIT` |

The server sets `sqlite3_limit()` for `SQLITE_LIMIT_SQL_LENGTH` to `MaxSQLBytes`,
`SQLITE_LIMIT_VARIABLE_NUMBER` to `MaxParams`, `SQLITE_LIMIT_COLUMN` to
`MaxColumns`, and `SQLITE_LIMIT_ATTACHED` to 0.

Limits protect the server and are not a security boundary against a hostile
client (section 12).

## 12. Trust model

- The service is for **same-host, trusted clients**. Access control is the
  listener: the acceptor segment's permission (`AcceptorConfig.Perm`, default
  0600) decides which local users may connect.
- The protocol does not authenticate individual requests. Any connected client may
  run any SQL the database allows.
- The server enforces the database policy: only aliases from its configuration,
  or raw paths when the policy allows them.
- The server disables `ATTACH`, `DETACH`, and loadable extensions
  (`sqlite3_db_config(..., SQLITE_DBCONFIG_ENABLE_LOAD_EXTENSION, 0)` and an
  authorizer that denies `SQLITE_ATTACH` and `SQLITE_DETACH`). Otherwise a
  client could open files the policy does not allow.
- Pragma names and values in Open are validated (section 6.1). No other
  client text is spliced into SQL.

## 13. Server implementation notes (C)

- One worker per session is recommended: requests of a session run on that
  worker in arrival order. Requests of one session are serialized, because the
  Go client never pipelines. The server MUST NOT run two requests of one session
  at once.
- Sessions are independent. The server MAY run many sessions in parallel, and
  the SQLite connections use `SQLITE_OPEN_FULLMUTEX` or are serialized by the
  session worker.
- The cancel path (section 9) needs a callback from the HQQ handler context to
  `sqlite3_interrupt()`. The C Advanced layer must provide the request context's
  done notification for this.
- Every request handler must release (finalize, reset) what it created before
  returning, on every error path.

## 14. Versioning

Major version 1 is this document. A change that alters a layout, a method id,
a status, or a limit's meaning MUST bump the major version. A compatible
addition (a new method id that old servers answer with `UNKNOWN_METHOD`) bumps
the minor version. Clients never send methods the negotiated minor version
does not define.

## 15. Design notes

- **Rows are pulled, not pushed.** Rows travel in the responses of Query and
  Fetch (`Stream.Call`), not as messages sent with `Stream.Send`. Under HQQ's
  default `OverflowBlock` policy, a message queue that is full blocks the link's
  receive path, and nothing else on the link is routed while it waits, not even
  responses to other streams' calls. A pushing server would therefore let one
  slow reader stall every session on the link. With Fetch, the client sets the
  pace, the server holds no buffered rows, and the client holds at most two
  batches (the one it reads and the one it prefetches). Errors arrive in-band
  with the batch that failed.
- **The server watches each stream with `Stream.Recv`.** Nothing is sent on a
  session stream except requests, so the watcher's `Recv` returns only when the
  stream closes or the link fails. That is how teardown (section 8.3) starts.
  A plain message on a session stream is a protocol violation and also ends the
  session.
- **Pooled connections are reset.** The driver sends Rollback before it reuses
  a connection. This is a no-op outside a transaction, so it costs one round
  trip per checkout and ensures a transaction begun with plain SQL cannot leak
  into the next user of the connection.

## 16. Worked examples

The bytes below are produced by the Go encoder and pinned by the test
`TestGoldenExamples` in `wire/wire_test.go`. Spaces separate fields for
readability only; the real messages have no spaces.

Open request for database `main`, flags READWRITE|CREATE (0x6), busy timeout
5000 ms, one pragma `journal_mode = WAL`:

```
0100 0000                 major 1, minor 0
04000000 6d61696e         db = "main"
06000000                  flags = 0x2 | 0x4
88130000                  busy_ms = 5000
01000000                  npragmas = 1
0c000000 6a6f75726e616c5f6d6f6465   name = "journal_mode"
03000000 57414c           value = "WAL"
```

Exec request `INSERT INTO t VALUES (?)` with one positional parameter 42:

```
18000000 494e5345525420494e544f20742056414c55455320283f29   sql = "INSERT INTO t VALUES (?)"
01000000                  nparams = 1
01000000 00000000         ordinal = 1, name = "" (positional)
01 2a00000000000000       INTEGER 42
```

Query response for `SELECT n FROM t` with one column `n` and one row 42. The
result is complete, so the cursor is 0 and `more` is 0:

```
00000000                  cursor = 0 (none)
01000000                  ncolumns = 1
01000000 6e 00000000      column name = "n", decltype = ""
00                        more = 0
01000000                  nrows = 1
01 2a00000000000000       INTEGER 42
```

Values, one after another: TEXT `hi`, NULL, FLOAT 1.5:

```
03 02000000 6869          TEXT "hi"
05                        NULL
02 000000000000f83f       FLOAT 1.5
```
