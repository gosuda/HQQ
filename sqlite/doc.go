// Package sqlite is a database/sql driver for SQLite served over HQQ, built on
// the Go client that hqq-bindgen generates from
// bindgen/examples/sqlite3/sqlite3.idl. The SQLite engine runs in its own
// process (the C server in bindgen/examples/sqlite3/server). Clients need no
// cgo: every database/sql connection is one generated-client session with its
// own shared-memory link, server thread, and SQLite connection.
//
// The driver registers itself as "hqq-sqlite" when the package is imported:
//
//	import (
//		"database/sql"
//
//		_ "gosuda.org/hqq/sqlite" // registers "hqq-sqlite"
//	)
//
//	db, err := sql.Open("hqq-sqlite", "hqq://sqlite-svc?db=main&timeout=5s&fetch=256&busy=5s")
//
// The DSN is hqq://<listener>?<options>. The host is the HQQ listener name of the
// service. Options: db (required: an alias or a path, as the server accepts it),
// mode (rwc, rw, or ro), timeout, fetch (rows per batch), busy (the server's
// lock wait), txlock (deferred, immediate, or exclusive), and pragma.<name>=value
// (applied when a session opens). Unknown or repeated options are errors. ParseDSN
// returns the parsed form.
//
// To keep the connector, for example to tune the session or link options, build
// it yourself:
//
//	c, err := sqlite.NewConnector("sqlite-svc", sqlite.Config{
//		Database:    "main",
//		Timeout:     5 * time.Second,
//		FetchSize:   256,
//		BusyTimeout: 5 * time.Second,
//	})
//	db := sql.OpenDB(c)
//
// Errors. A SQLite error from the server is a *Error with Code (the primary
// result code, such as CodeConstraint or CodeBusy) and ExtendedCode (such as
// CodeConstraintUnique; equal to Code when SQLite gave none). Use errors.As to
// read them, or the predicates IsBusy, IsLocked, IsConstraint, IsReadOnly, and
// IsInterrupted, or errors.Is with the sentinels ErrBusy, ErrLocked,
// ErrConstraint, ErrReadOnly, and ErrInterrupted. A protocol error (a malformed
// request, a limit, a session state) is a *ProtocolError. A lost link before a
// request is sent, and a lost link during a query, Begin, Commit, or Rollback,
// wrap driver.ErrBadConn, so database/sql discards the connection and retries
// the call on a new one. A lost link during Exec or ExecStmt does not: the
// statement may have run, so the error wraps ErrOutcomeUnknown and database/sql
// does not retry it.
//
// Cancellation. When the context of a call ends while the server runs it, the
// driver sends an out-of-band cancellation over the control link (which invokes
// the @cancel(sqlite3_interrupt) hook on the session's Db handle) and closes
// the session link, returning the context's error at once and discarding the
// connection.
//
// Values. Arguments: nil is NULL; all integers are INTEGER (uint64 values above
// math.MaxInt64 are an error); float32 and float64 are REAL (NaN is an error,
// ±Inf is stored); bool is 0 or 1; string is TEXT; []byte and json.RawMessage are
// BLOB (nil is an empty BLOB; use string(raw) to store JSON text); time.Time is
// TEXT in the form "2006-01-02 15:04:05.999999999-07:00", which keeps its offset;
// driver.Valuer (including sql.Null*) is honored. Named arguments use sql.Named, with or
// without the prefix (":id", "@id", "$id"). Results: INTEGER is int64, REAL is
// float64, TEXT is string, BLOB is []byte, and NULL is nil. A TEXT value of a
// column declared DATE, DATETIME, or TIMESTAMP is a time.Time when it parses
// (a date, or a time with or without a zone, UTC when none is given), and a
// string otherwise. Large text and blob cells that do not fit a batch are
// streamed in chunks via column_chunk up to SQLite's 1 GiB limit.
//
// Transactions. BeginTx accepts the default and serializable isolation levels,
// since SQLite is always serializable, and refuses others with an error.
// ReadOnly transactions begin DEFERRED with query_only set. The BEGIN mode of
// other transactions comes from Config.TxMode, or per transaction from
// WithTxMode.
//
// Each Exec or Query takes one statement. Trailing text after a second statement
// is a BAD_REQUEST error ("multiple statements").
//
// Operational notes are in sqlite/README.md.
package sqlite
