# sqlite: a database/sql driver for SQLite served over HQQ

`gosuda.org/hqq/sqlite` is a `database/sql` driver, registered as `hqq-sqlite`,
for a SQLite database that runs as a separate service. The service is reached
through an HQQ listener. The driver needs no cgo. Each `database/sql`
connection is one HQQ stream of a shared link, and the server runs each stream
as one session with its own SQLite connection, transaction state, and
statements.

| Path | What it is |
|---|---|
| [`doc.go`](doc.go) | The package documentation (`go doc gosuda.org/hqq/sqlite`). |
| [`PROTOCOL.md`](PROTOCOL.md) | The wire protocol 1.0: byte layouts, methods, status codes, state, cancellation, limits. The contract for every implementation. |
| `internal/wire/` | The protocol encodings, shared by the driver and the fake server. Internal: it is an implementation detail. |
| `internal/fakeserver/` | A Go stand-in for the C server (HQQ acceptor, sessions, an in-memory engine with a small SQL subset). Used by the tests. |
| `internal/contract/` | The database/sql contract suite, run against any service (`contract.RunContract`). |
| `e2e/` | End-to-end tests that run the contract and the driver tests against the C server. They skip without `HQQ_SQLITE_SERVER`. |
| [`launcher/`](launcher) | Starts the C server as a child process, after verifying its digest, and returns a `database/sql` handle for it (see its README). |
| [`../examples/sqlite/server/`](../examples/sqlite/server) | The C server (libhqq and SQLite; the system library, or an amalgamation via `HQQ_SQLITE_AMALGAMATION_DIR`). Linux, macOS and Windows (clang-cl). See its README. |

## Using the driver

Import the package for its side effect, which registers the driver, and open
a database with a DSN:

```go
import (
	"database/sql"

	_ "gosuda.org/hqq/sqlite" // registers "hqq-sqlite"
)

db, err := sql.Open("hqq-sqlite", "hqq://sqlite-svc?db=main&timeout=5s&fetch=256&busy=5s")
if err != nil {
	return err
}
defer db.Close()
```

Build a connector when the link needs tuning, or when the caller keeps the
connector to close the shared link later:

```go
c, err := sqlite.NewConnector("sqlite-svc", sqlite.Config{
	Database:    "main",
	Timeout:     5 * time.Second,
	FetchSize:   256,
	BusyTimeout: 5 * time.Second,
})
if err != nil {
	return err
}
db := sql.OpenDB(c) // all connections of db share one link
```

`ConnectorFromDSN(dsn)` returns a connector for a DSN, and
`ParseDSN(dsn)` returns the listener name and the parsed `Config`. Closing the
`*sql.DB` closes the connector (it is an `io.Closer`), and so the link.

## DSN

`hqq://<listener>?<options>`. The host is the listener name. Every option may
appear once, and unknown options are errors.

| Option | Default | Meaning |
|---|---|---|
| `db` | required | Database alias or path, as the server accepts it. The server decides which names are allowed. |
| `mode` | `rwc` | `rwc` open or create, `rw` open an existing database for writing, `ro` read-only. |
| `timeout` | `10s` | Bounds connecting, and the requests that run outside a caller context (Commit, Rollback, closing cursors and statements). |
| `fetch` | `256` | Rows per batch, 1 to 4096. |
| `busy` | `5s` | How long the server waits for a lock held by another session. `-1s` disables the wait. |
| `txlock` | `deferred` | BEGIN mode: `deferred`, `immediate`, or `exclusive`. |
| `pragma.<name>` | none | A PRAGMA the server applies when a session opens (for example `pragma.query_only=1`). |

The server leaves `journal_mode` at SQLite's default (rollback journal). With many
concurrent writers on one database file, SQLite's busy handler has no queue, so a
writer can starve past `busy`; set `pragma.journal_mode=WAL` for write-heavy
concurrent workloads. An autocommit statement that fails with `IsBusy` did not
commit and is safe to retry.

The `Config` fields are the same options: `Database`, `Mode`, `Timeout`,
`FetchSize`, `BusyTimeout`, `TxMode`, `Pragmas`, and `Link` (the HQQ link
settings; the features and the message size are fixed by the protocol).

## Errors

- **SQLite errors** are `*sqlite.Error`. `Code` is the primary result code, for
  example `CodeConstraint` (19), and `ExtendedCode` is the extended code, for
  example `CodeConstraintUnique` (2067). `ExtendedCode` equals `Code` when SQLite
  gave none. `Message` is `sqlite3_errmsg()`. The helpers `IsBusy`, `IsLocked`,
  `IsConstraint`, `IsReadOnly`, and `IsInterrupted` test the primary code, and
  `errors.Is` matches the sentinels `ErrBusy`, `ErrLocked`, `ErrConstraint`,
  `ErrReadOnly`, and `ErrInterrupted` the same way.

  ```go
  if _, err := db.Exec("INSERT INTO users (email) VALUES (?)", email); err != nil {
  	var se *sqlite.Error
  	switch {
  	case errors.As(err, &se) && se.ExtendedCode == sqlite.CodeConstraintUnique:
  		return ErrDuplicate
  	case sqlite.IsBusy(err):
  		return ErrRetryLater
  	}
  	return err
  }
  ```

- **Protocol errors** are `*sqlite.ProtocolError` (an alias of the wire type). `Code`
  is a protocol code of PROTOCOL.md section 7: 1 BAD_REQUEST (also a second
  statement in one call), 7 LIMIT, 9 FORBIDDEN (the database is not allowed by the
  server's policy), and so on.

- **Lost connections.** When the link dies, the error wraps `driver.ErrBadConn`
  and `database/sql` discards the connection and retries the call on a new one
  (`database/sql` retries at most twice). This holds for a connect, a Query, a
  Prepare, a Begin, a Ping, a Commit, and a Rollback. The Commit case is the one
  place where the outcome of the call is unknown: the transaction may have
  committed. A Query is retried like a read, so a statement that writes through
  `Query` (for example `INSERT ... RETURNING`) can run twice after a lost link:
  send writes through `Exec`, or inside a transaction.

- **Lost connections during Exec.** An Exec or ExecStmt whose request was in
  flight when the link died returns an error that wraps `sqlite.ErrOutcomeUnknown`,
  and does not wrap `driver.ErrBadConn`. The statement may or may not have run, so
  `database/sql` must not retry it. The connection is discarded. A write that
  matters should run in a transaction, and its outcome should be checked by
  reading the data again.

- **Cancellation** returns `context.Canceled` or `context.DeadlineExceeded`
  (not `ErrBadConn`). A context that is already done is checked before anything is
  sent, so the connection stays usable. A context that ends while a request is in
  flight sends a cancel to the server, which interrupts the statement. The
  connection is then unusable and is discarded, and the server rolls its
  transaction back.

## Values

Arguments are converted by the driver's `CheckNamedValue`, so the rules below
hold for every call, including `db.Query` and `Stmt`:

| Go | SQLite |
|---|---|
| `nil` | NULL |
| all signed integers; unsigned integers up to `math.MaxInt64` | INTEGER. A larger `uint64` is an error. |
| `float32`, `float64` | REAL. `NaN` is an error: SQLite stores NaN as NULL, and the driver does not guess, so pass `nil` to store NULL. `±Inf` is stored as is. |
| `bool` | INTEGER 0 or 1 |
| `string` | TEXT |
| `[]byte`, `json.RawMessage` | BLOB. A nil slice is an empty BLOB, not NULL. Use `string(raw)` to store JSON text. |
| `time.Time` | TEXT in the form `2006-01-02 15:04:05.999999999-07:00`, which keeps the value's offset. |
| `driver.Valuer` (`sql.Null*` included) | the Value it returns, which must not be a Valuer. A nil pointer Valuer is NULL. |
| pointers and named types | dereferenced, and converted by their kind |
| anything else (structs, maps, channels) | an error that names the type, before anything is sent |

Named arguments use `sql.Named`, with or without the prefix: `sql.Named("id", 7)`
binds `:id`, `@id`, or `$id`. Positional and named arguments may be mixed.
Parameters that are not supplied are NULL, as in SQLite. The argument count is
checked against the parameter count of a prepared statement, but not for a
direct `Exec` or `Query`.

Results:

| SQLite | Go |
|---|---|
| INTEGER | `int64` |
| REAL | `float64` |
| TEXT | `string`. A TEXT value of a column declared `DATE`, `DATETIME`, or `TIMESTAMP` (case-insensitive, any length suffix such as `(10)` is ignored) is a `time.Time` when it parses, and a `string` otherwise. |
| BLOB | `[]byte` (a copy) |
| NULL | `nil` |

Parsed time text is one of: `2006-01-02 15:04:05.999999999-07:00`,
RFC 3339 with `T` and an offset or `Z`, the same two forms without an offset
(read as UTC), `2006-01-02 15:04`, `2006-01-02T15:04`, or `2006-01-02`. Times are
UTC when the text has no offset. A time column holding an integer or real value
keeps that value.

`json.RawMessage` values are BLOBs, like `[]byte`. Scan TEXT JSON into `string`
or `[]byte`, then unmarshal.

## Result columns

`Rows.ColumnTypes` reports:

- `DatabaseTypeName`: the declared type, uppercased (for example `VARCHAR(20)`
  or `DATETIME`). A column with no declared type (an expression) reports the
  storage class of its value in the current row: `INTEGER`, `REAL`, `TEXT`,
  `BLOB`, or `NULL`. Before the first row, and after the last, it is empty.
- `ScanType`, from the declared type under SQLite's affinity rules: INTEGER
  (contains "INT") `int64`, TEXT (contains "CHAR", "CLOB", or "TEXT") `string`,
  REAL (contains "REAL", "FLOA", or "DOUB") `float64`, BLOB `[]byte`, DATE,
  DATETIME, and TIMESTAMP `time.Time`, and `any` otherwise. SQLite is dynamically
  typed, so a value can differ from its column's type: scan into `any` when
  unsure.
- `Nullable`: not reported. The protocol carries no NOT NULL information.
- `Length`: not reported. SQLite does not enforce declared lengths.

`LastInsertId` is `sqlite3_last_insert_rowid()` of the session after the
statement. `RowsAffected` is `sqlite3_changes64()` after the statement, the rows
changed by the last INSERT, UPDATE, or DELETE; a SELECT or DDL leaves it as it was.
Use them only after the statement they describe.

## Rows and batches

A result streams in batches of `fetch` rows. While the caller reads one batch,
the next is fetched in the background, so at most two batches are held. A large
result therefore uses bounded memory. Closing `Rows` early releases the cursor on
the server. A `TEXT` or `BLOB` value is limited by the message size: a row must
fit in 16 MiB. A larger row fails with a `*sqlite.Error` whose code is
`CodeTooBig` (SQLITE_TOOBIG), and the cursor is released.

Each `Exec` or `Query` takes one statement. A second statement is a BAD_REQUEST
error, and so is an empty statement. A trailing semicolon and whitespace are
allowed. There are no multiple result sets.

## Transactions

`BeginTx` accepts the default and the serializable isolation levels, since SQLite
is always serializable. Other levels return an error before anything is sent.
`ReadOnly` transactions begin DEFERRED with `query_only` set, so a write fails
with an error for which `IsReadOnly` holds. The BEGIN mode of other transactions
is `Config.TxMode` (or the `txlock` option), overridden for one transaction by
`sqlite.WithTxMode(ctx, sqlite.TxImmediate)` in the context passed to `BeginTx`.

A failed Commit is followed by a Rollback, because SQLite keeps the transaction
open after some commit errors (such as `SQLITE_BUSY`). A transaction begun with
plain `BEGIN` on a pooled connection is rolled back before the connection is
reused (`ResetSession`).

## Connection pool

Each connection is a stream of one link. The connector dials the link once and
shares it among the connections. When the link dies, it is dropped, and the next
connect dials again. Concurrent connects wait for one dial. `SetMaxOpenConns`,
`SetMaxIdleConns`, and `SetConnMaxLifetime` work as usual. `Ping` round-trips to
the server, and `IsValid` discards a connection whose stream failed.

## Server

The server is the C server in `../examples/sqlite/server` (or the Go fake server
in `internal/fakeserver`, which runs the same protocol with an in-memory engine
with a small SQL subset, for tests). The server is a single process that listens
on an HQQ name. It enforces a database policy: only the aliases in its
configuration, and raw paths only when the policy allows them. It disables
`ATTACH`, `DETACH`, and loadable extensions. It is for same-host, trusted clients:
access control is the listener's permission. It does not authenticate requests.
See PROTOCOL.md section 12.

The C server with `--parent-stdin` also shuts down gracefully when the process that started it exits, including by SIGKILL (see the Parent lifeline section of `../examples/sqlite/server/README.md`). `sqlite/launcher` passes the flag itself; a different parent must keep the write end of a pipe open on the server's standard input and pass `--parent-stdin`.

## Tests

```sh
go vet ./sqlite/...
go test -race -count=1 ./sqlite/...      # fake server; the e2e tests skip
HQQ_SQLITE_SERVER=/path/to/hqq-sqlite-server go test -count=1 ./sqlite/e2e/...
```

- `internal/wire`: round trips, every strict prefix of a valid body is rejected,
  status encoding, and golden bytes from PROTOCOL.md section 16.
- `internal/fakeserver`: engine and session tests, without a network.
- `internal/contract`: the database/sql contract, run by `contract_test.go`
  against the fake server, and by `e2e/contract_test.go` against the C server.
  It exercises the driver through `database/sql` only: every value type, NULL,
  scanning, column types, multi-row and large results, statements, transactions,
  context, errors, the pool, and concurrency. Cases that need real SQLite
  features (arithmetic, ORDER BY, aggregates, `typeof`) run only with
  `Capabilities.FullSQL`.
- `driver_test.go`, `crash_test.go`: driver behavior over real HQQ links:
  DSN and connectors, cancellation, early `Rows.Close`, large result memory,
  the alias policy, read-only mode, link loss during requests (Exec is not
  retried; Query is), and the server crash with reconnect (SIGKILL of a helper
  process).
- `e2e/`: the C server, the contract, and the server's process behavior (client
  crashes, SIGKILL and reconnect, graceful shutdown, session limits, path policy).

## Open issues

- `Exec` accepts one statement; migration scripts must be split by the caller.
- `rows_affected` after a SELECT still holds the previous value. This is SQLite's
  behavior.
- The fake engine is not SQLite. Its SQL subset and its behaviors differ in
  details (affinity, rowid reuse, functions), so the contract asserts only the
  behavior both share.
