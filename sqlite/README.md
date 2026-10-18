# `sqlite`: Pure-Go `database/sql` Driver Over HQQ Shared Memory

`gosuda.org/hqq/sqlite` is a pure-Go `database/sql` driver, registered as `"hqq-sqlite"`, built on the `hqq-bindgen`-generated SQLite client ([`bindgen/examples/sqlite3`](../bindgen/examples/sqlite3/README.md)). It connects to a SQLite database hosted by a separate server process over an HQQ shared-memory listener with zero `cgo` in the Go binary. Every `database/sql` connection opens an isolated `StandardLink` session with its own `sqlite3` connection handle, transaction state, and prepared statements.

| Path | Role |
| :--- | :--- |
| [`doc.go`](doc.go) | Package GoDoc reference (`go doc gosuda.org/hqq/sqlite`). |
| [`PROTOCOL.md`](PROTOCOL.md) | SQLite service protocol reference: status codes, single-statement rules, cancellation, and resource limits. |
| `internal/wire/` | Protocol status constants (`ProtoError`, `ProtoLimit`, `ProtoForbidden`) and binary wire codecs. |
| `internal/convert/` | Parameter conversion (`CheckNamedValue`) and timestamp parsing. |
| `internal/dsn/` | DSN parser (`hqq://<listener>?...`) and configuration validation. |
| `internal/contract/` | Reusable 107-case `database/sql` behavioral contract suite (`contract.RunContract`). |
| `e2e/` | End-to-end test suite running the contract and crash-recovery tests against the compiled C server (`HQQ_SQLITE_SERVER`). |
| [`launcher/`](launcher/README.md) | Verified child-process starter that hashes `hqq-sqlite-server` with `secureexec`, attaches a parent lifeline, and returns a ready `*sql.DB`. |
| [`../bindgen/examples/sqlite3/`](../bindgen/examples/sqlite3/README.md) | Generated Go client (`sqlite3_client.go`), IDL (`sqlite3.idl`), and production C11 SQLite server (`server/`, Linux, macOS, and Windows). |

## Using the driver

Import `gosuda.org/hqq/sqlite` to register `"hqq-sqlite"` with `database/sql` and open a handle using an `hqq://` DSN:

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

When you prefer structured configuration over a DSN string or want direct access to the `driver.Connector`, construct one with `sqlite.NewConnector`:

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
db := sql.OpenDB(c)
```

`ConnectorFromDSN(dsn)` builds a `*Connector` from a DSN string, and `ParseDSN(dsn)` parses the listener name and `Config` without opening a link. `*Connector` implements `io.Closer` as a no-op because each `database/sql` connection owns its own session link.

## DSN format and configuration

DSNs take the form `hqq://<listener>?<options>`, where `<listener>` is the HQQ acceptor name. Each query parameter may appear at most once, and unrecognized parameters return an error:

| Parameter | Default | Description |
| :--- | :--- | :--- |
| `db` | required | Database alias or relative file path accepted by the server's open policy. |
| `mode` | `rwc` | Open mode: `rwc` (read-write-create), `rw` (read-write existing file), or `ro` (read-only). |
| `timeout` | `10s` | Bounds initial link connection and operations that run without a caller `context.Context` (`Commit`, `Rollback`, cursor/statement close). |
| `fetch` | `256` | Number of rows fetched per batch (`1` to `4096`). |
| `busy` | `5s` | Server-side `sqlite3_busy_timeout` duration when waiting on locks held by another session (`-1s` disables waiting). |
| `txlock` | `deferred` | Default `BEGIN` mode for read-write transactions: `deferred`, `immediate`, or `exclusive`. |
| `pragma.<name>` | none | Runs `PRAGMA <name> = <value>` when each session opens (for example `pragma.journal_mode=WAL` or `pragma.foreign_keys=1`). |

By default, SQLite opens databases in rollback-journal mode. For workloads with concurrent writers, set `pragma.journal_mode=WAL` so readers do not block writers and writers contend only against other writers. An autocommit statement that fails with `sqlite.IsBusy(err)` never committed and is safe to retry.

`sqlite.Config` exposes the exact same settings as struct fields: `Database`, `Mode`, `Timeout`, `FetchSize`, `BusyTimeout`, `TxMode`, `Pragmas`, and `Link` (custom HQQ `Config` geometry).

## Error semantics and retry safety

The driver distinguishes SQLite engine errors, wire-protocol errors, retryable link drops, and ambiguous write drops:

- **SQLite engine errors (`*sqlite.Error`)**: `Code` holds the primary SQLite result code (such as `CodeConstraint = 19`), `ExtendedCode` holds the extended result code (such as `CodeConstraintUnique = 2067`, or equals `Code` when no extended code applies), and `Message` holds `sqlite3_errmsg()`. Helper predicates `IsBusy`, `IsLocked`, `IsConstraint`, `IsReadOnly`, and `IsInterrupted` test the primary code, and `errors.Is` matches the sentinels `ErrBusy`, `ErrLocked`, `ErrConstraint`, `ErrReadOnly`, and `ErrInterrupted`:

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

- **Service protocol errors (`*sqlite.ProtocolError`)**: returned when a request violates the service contract in [`PROTOCOL.md`](PROTOCOL.md) §7, such as `1` (`BAD_REQUEST`, including multi-statement SQL strings), `7` (`LIMIT`), or `9` (`FORBIDDEN`, when the server's path/alias policy rejects the database name).
- **Lost connections on reads and setup (`driver.ErrBadConn`)**: if the shared-memory link dies during `Connect`, `Query`, `Prepare`, `Begin`, `Ping`, `Commit`, or `Rollback`, the returned error wraps `driver.ErrBadConn`. `database/sql` discards the broken session and automatically retries `Connect`, `Query`, `Prepare`, `Begin`, or `Ping` on a fresh connection. Because `Query` is treated as retryable by `database/sql`, never send mutating statements (such as `INSERT ... RETURNING`) through `Query` outside a transaction; use `Exec` or an explicit transaction instead.
- **Lost connections during `Exec` (`sqlite.ErrOutcomeUnknown`)**: if the link dies while an `Exec` or `ExecStmt` call is in flight, the server may have already committed the write before crashing. The driver therefore returns an error wrapping `sqlite.ErrOutcomeUnknown` and deliberately **does not** wrap `driver.ErrBadConn`, preventing `database/sql` from silently re-executing a non-idempotent write.
- **Context cancellation (`context.Canceled` / `context.DeadlineExceeded`)**: if `ctx` is already expired before a call starts, the driver returns `ctx.Err()` immediately and keeps the connection healthy. If `ctx` expires while a request is in flight on the server, the driver kills the local session wait and sends a `BG_CANCEL` request (`@cancel(sqlite3_interrupt)` on the server), discards the connection session, and lets the server roll back any open transaction on that session.

## Parameter and result type mappings

Parameters are validated and converted by the driver's `CheckNamedValue` before any frame is sent:

| Go Parameter Type | SQLite Storage Class & Rules |
| :--- | :--- |
| `nil` | `NULL`. |
| Signed integers (`int`, `int8`..`int64`); unsigned integers `<= math.MaxInt64` | `INTEGER` (`uint64 > math.MaxInt64` fails before send). |
| `float32`, `float64` | `REAL`. `math.IsNaN` is rejected with an error (SQLite converts `NaN` to `NULL`; pass `nil` explicitly if you want `NULL`). `±Inf` is preserved. |
| `bool` | `INTEGER` (`1` for `true`, `0` for `false`). |
| `string` | `TEXT`. |
| `[]byte`, `json.RawMessage` | `BLOB` (a `nil` slice encodes as a zero-length `BLOB`, not `NULL`; convert `json.RawMessage` to `string` if you want `TEXT`). |
| `time.Time` | `TEXT` formatted as `2006-01-02 15:04:05.999999999-07:00` (preserving timezone offset). |
| `driver.Valuer` (including `sql.Null*`) | Evaluates `.Value()` (nil pointer `Valuer` becomes `NULL`). |
| Pointers and defined types over primitives | Dereferenced/unwrapped and converted by underlying kind. |
| Structs, maps, slices, channels | Rejected locally with an error naming the unsupported type. |

Named parameters (`sql.Named("id", 7)`) match `:id`, `@id`, or `$id` in the SQL text and can be mixed with positional `?` parameters. Unbound parameter slots default to `NULL` per SQLite rules.

Result values returned from `Rows`:

| SQLite Storage Class | Returned Go Type |
| :--- | :--- |
| `INTEGER` | `int64` |
| `REAL` | `float64` |
| `TEXT` | `string`, or `time.Time` when the column's declared type is `DATE`, `DATETIME`, or `TIMESTAMP` (case-insensitive, ignoring length suffixes such as `DATETIME(6)`) and the text parses under one of the supported layouts (`2006-01-02 15:04:05.999999999-07:00`, RFC 3339, timezone-less UTC equivalents, `2006-01-02 15:04`, or `2006-01-02`). Unparseable strings remain `string`. |
| `BLOB` | `[]byte` (a fresh caller-owned copy). |
| `NULL` | `nil` |

## Column metadata, batches, and transactions

- **`Rows.ColumnTypes()`**: `DatabaseTypeName()` returns the uppercased declared column type (or the current row's storage class `INTEGER`, `REAL`, `TEXT`, `BLOB`, or `NULL` for untyped expressions). `ScanType()` maps declared affinities to `int64`, `string`, `float64`, `[]byte`, `time.Time` (for `DATE`/`DATETIME`/`TIMESTAMP`), or `any`.
- **Prefetched row batches and chunked large cells**: query results stream in batches of up to `fetch` rows (default `256`, within a 64 KiB batch buffer). While the caller iterates over a normal batch, the driver prefetches the next batch in the background so at most two batches reside in memory at once. Cells larger than the remaining batch buffer are returned as `BigText` or `BigBlob` markers and streamed in 1 MiB chunks via `column_chunk` up to 1 GiB.
- **Single-statement rule**: each `Exec`, `Query`, or `Prepare` accepts exactly one SQL statement (trailing whitespace, comments, and a single `;` are permitted; multiple statements return `BAD_REQUEST`).
- **Transactions**: `BeginTx` supports `sql.LevelDefault` and `sql.LevelSerializable`. Setting `ReadOnly: true` starts a `BEGIN DEFERRED` transaction with `PRAGMA query_only = 1`. For read-write transactions, the lock mode defaults to `Config.TxMode` (`deferred`, `immediate`, or `exclusive`) and can be overridden per transaction via `sqlite.WithTxMode(ctx, sqlite.TxImmediate)`. If `Commit` fails (for example with `SQLITE_BUSY`), the driver automatically issues a `Rollback` so the session never remains stuck inside an open transaction.

## Session isolation and server security

Each `*sql.Conn` opened from a `*Connector` or `*sql.DB` dials a dedicated `StandardLink` session served by its own thread in the C server. If a session link dies (`ErrPeerGone` or `ErrPeerClosed`), `database/sql` discards the broken connection and dials a fresh session on the next request.

The C server (`bindgen/examples/sqlite3/server`) enforces database path sandboxing (`--alias` and `--allow-dir`), disables `ATTACH`, `DETACH`, and `sqlite3_load_extension`, and supports `--parent-stdin` so it rolls back open transactions and exits cleanly if its parent process dies (including via `SIGKILL`). See [`PROTOCOL.md`](PROTOCOL.md) §12 and [`../bindgen/examples/sqlite3/README.md`](../bindgen/examples/sqlite3/README.md).

## Running the tests

Execute the unit, contract, and end-to-end test suites from the repository root:

```sh
go vet ./sqlite/...
go test -race -count=1 ./sqlite/...      # runs against the in-memory fake and (when built) C server
HQQ_SQLITE_SERVER=/path/to/hqq-sqlite-server go test -count=1 ./sqlite/...
```

- `internal/wire`, `internal/convert`, `internal/dsn`: verify status encodings, parameter conversions, timestamp layouts, and DSN parsing.
- `internal/contract`: 107-case `database/sql` behavioral contract run against both the in-memory Go fake and the C server (`sqlite/contract_test.go`, `sqlite/e2e/contract_test.go`).
- `driver_test.go`, `cancel_race_test.go`, `e2e/e2e_test.go`: test DSN parsing, mid-flight cancellation, early `Rows.Close`, bounded memory on large scans, `ErrOutcomeUnknown` vs `driver.ErrBadConn` retry rules, and automatic reconnection after `SIGKILL` of the server process.
