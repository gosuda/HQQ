# hqq-sqlite: SQLite as a service over HQQ

SQLite runs in its own process and is reached through an HQQ listener. Each
`database/sql` connection is one HQQ stream, and the server runs each stream
as one session with its own SQLite connection. No cgo is needed on the client.

| Path | What it is |
|---|---|
| [PROTOCOL.md](PROTOCOL.md) | The wire protocol 1.0: byte layouts, methods, errors, state, cancellation, limits. The contract for every implementation. |
| `wire/` | Go encodings of the protocol, shared by the driver and the fake server. |
| `driver/` | The `database/sql` driver, package `hqqsqlite`, registered as `hqq-sqlite`. |
| `fakeserver/` | A Go stand-in for the C server: HQQ acceptor, sessions, and an in-memory engine with a small SQL subset. Used by the tests. |
| `server/` | The C server (libhqq and SQLite: the system library by default, or an amalgamation via `HQQ_SQLITE_AMALGAMATION_DIR`); see `server/README.md`. |

## Using the driver

Start a service on a listener name, then open a connection with a DSN:

```go
import (
	"database/sql"

	_ "gosuda.org/hqq/examples/sqlite/driver" // registers "hqq-sqlite"
)

db, err := sql.Open("hqq-sqlite", "hqq://sqlite-svc?db=main&timeout=5s&fetch=256&busy=5s")
```

Or build a connector when the link needs tuning:

```go
c, err := hqqsqlite.NewConnector("sqlite-svc", hqqsqlite.Config{
	Database:    "main",
	Timeout:     5 * time.Second,
	FetchSize:   256,
	BusyTimeout: 5 * time.Second,
})
db := sql.OpenDB(c) // all connections of db share one link
```

### DSN

`hqq://<listener>?<options>`. The host is the listener name, and unknown
options are errors.

| Option | Default | Meaning |
|---|---|---|
| `db` | required | Database alias or path, as the server accepts it. |
| `mode` | `rwc` | `rwc` open or create, `rw` open existing for writing, `ro` read-only. |
| `timeout` | `10s` | Bounds connecting and the requests outside a caller context. |
| `fetch` | `256` | Rows per batch (1 to 4096). |
| `busy` | `5s` | Server-side wait for another session's lock. `-1s` disables it. |
| `txlock` | `deferred` | BEGIN mode: `deferred`, `immediate`, `exclusive`. |
| `pragma.<name>` | none | PRAGMA applied when a session opens (for example `pragma.query_only=1`). |

## Driver behavior

- **Connections and the link.** Each connection is a stream. The `Connector`
  dials the link once and shares it. When the link dies (`ErrPeerGone`,
  `ErrPeerClosed`), it is dropped, and the next `Connect` dials again.
  Concurrent connects wait for a single dial.
- **Errors.** Transport and peer failures are `driver.ErrBadConn`, so
  `database/sql` discards the connection and retries. SQLite errors are
  `*hqqsqlite.Error` with `Code` (extended code when given) and `Primary()`.
  `errors.Is` matches `ErrBusy`, `ErrLocked`, `ErrConstraint`, `ErrReadOnly`,
  and `ErrInterrupted` by primary code. Protocol errors are
  `*wire.ProtocolError`.
- **Cancellation.** A context that ends during a request sends `OpCancel`. The
  server interrupts the statement. The connection is then marked bad and its
  stream closed, so the server rolls the session back. Connections are never
  reused in an unknown state.
- **Rows.** Batches stream. While one batch is read, the next is fetched, so at
  most two batches are held. Closing rows early closes the server cursor.
- **Transactions.** `BeginTx` sends Begin. `ReadOnly` begins DEFERRED with
  `query_only`. The BEGIN mode comes from `Config.TxMode`, or per transaction
  from `hqqsqlite.WithTxMode(ctx, hqqsqlite.TxImmediate)`. Only the default
  and serializable isolation levels are accepted (SQLite's only level). A
  failed Commit is followed by a Rollback. If the connection is lost during
  Commit, the outcome is unknown and the error is `driver.ErrBadConn`.
- **Pooling.** Before a pooled connection is reused, `ResetSession` sends
  Rollback. It is a no-op outside a transaction, so a transaction begun with
  plain `BEGIN` never survives into the next checkout. This costs one round
  trip per checkout.
- **Arguments.** `[]byte` is a BLOB, `string` is TEXT, `bool` is 0 or 1, and
  `time.Time` is RFC 3339 TEXT with nanoseconds. Named arguments use
  `sql.Named` with the name without its prefix (`:id` is `"id"`).
- **Limits.** A request is refused before it is sent when it exceeds 16 MiB.
  `Exec` and `Query` take one statement each.

## Running the tests

```sh
go vet ./examples/sqlite/...
go test -race -count=1 ./examples/sqlite/...   # about 4 s
```

- `wire/`: round trips, every strict prefix of a valid body is rejected,
  status encoding, and golden bytes pinned from PROTOCOL.md section 16.
- `fakeserver/`: table-driven engine tests (SQL, parameters, constraints,
  transactions, busy and read-only behavior, interrupts), without a network.
- `driver/`: end-to-end over real HQQ links. It covers types and NULLs, named
  and prepared statements, multi-batch results with a fetch of 7, 3 MiB blobs
  (multi-chunk), transactions and visibility across connections, constraint
  and busy codes, cancellation mid-query (the server observes it), 50
  concurrent connections, a 16 MiB stream with bounded live heap, early
  `Rows.Close`, rollback on connection close, pragmas and read-only mode,
  the alias policy, connect timeouts, and the DSN grammar.
- `TestServerCrashReconnect` starts the server in a child process (the test
  binary re-executed with `TestHelperServer`), kills it with SIGKILL, checks
  that the broken connection returns `driver.ErrBadConn` within the peer
  timeout, then starts a new server on the same name and checks that the pool
  dials a new link.

## The fake server

`fakeserver.Listen(ctx, name, fakeserver.Options{...})` runs the protocol on an
HQQ acceptor. Its engine is an in-memory stand-in, not SQLite. It keeps the
behavior the driver depends on: sessions, transactions with a single writer
and a busy timeout, copy-on-write isolation (uncommitted rows are invisible),
rollback on teardown, interrupts on cancel, statement and cursor ids,
batches, and the result codes of constraint violations.

Its SQL subset: `CREATE TABLE` (with `PRIMARY KEY`, `NOT NULL`, `UNIQUE`),
`DROP TABLE`, `INSERT ... VALUES`, `SELECT` with a select list, `FROM t`, a
`WHERE` of comparisons (`=`, `!=`, `<>`, `<`, `<=`, `>`, `>=`) joined by `AND`,
and `LIMIT`, `UPDATE`, `DELETE`, `BEGIN`/`COMMIT`/`ROLLBACK`, and `PRAGMA`.
Two functions exist for tests: `hqq_sleep(ms)` (interruptible) and
`FROM hqq_series(a, b)`. Not supported: aggregates, joins, `ORDER BY`,
`GROUP BY`, and subqueries.

## Open issues

- `Exec` accepts one statement. Migration scripts must be split by the caller.
- `rows_affected` is SQLite's changes count for the last DML statement, so
  after a SELECT it still holds the previous value. This is SQLite's behavior.
