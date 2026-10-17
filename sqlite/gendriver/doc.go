// Package gendriver is a database/sql driver for SQLite built on the Go client
// that hqq-bindgen generates from bindgen/examples/sqlite3/sqlite3.idl. It is a
// second implementation of the service of package sqlite (the hand-written
// protocol of sqlite/PROTOCOL.md): the two drivers have the same database/sql
// behavior, the same DSN, the same value mapping, and the same errors, and they
// run against different servers. Package sqlite is not changed by this one.
//
// The driver registers itself as "hqq-sqlite-gen" when the package is imported:
//
//	import (
//		"database/sql"
//
//		_ "gosuda.org/hqq/sqlite/gendriver" // registers "hqq-sqlite-gen"
//	)
//
//	db, err := sql.Open("hqq-sqlite-gen", "hqq://sqlite3-svc?db=main&timeout=5s&fetch=256&busy=5s")
//
// Shared code. The conversion of arguments and of values, the TEXT form of times,
// the column metadata, and the DSN are the code of package sqlite (they live in
// sqlite/internal/convert and sqlite/internal/dsn and both drivers use them).
// Errors are package sqlite's: a SQLite failure is a *sqlite.Error, a refused
// request is a *sqlite.ProtocolError, and the predicates sqlite.IsBusy,
// sqlite.IsConstraint, ... and the sentinels sqlite.ErrBusy, ... and
// sqlite.ErrOutcomeUnknown work on errors of this driver. Mode, TxMode, Pragma
// and the DSN options are the same types and options.
//
// # Differences from package sqlite
//
// Sessions. Every database/sql connection is its own generated-client session:
// one shared-memory link, one server thread, one SQLite connection. Package
// sqlite multiplexes the connections of a pool as streams of one link. Connector
// has no shared state, and Connector.Close only stops further Connect calls.
// Config.Session configures the sessions (buffers, heartbeat, message size); the
// driver's Timeout is the connect timeout of the handshake and the bound of the
// requests that run outside a caller context.
//
// Cancellation. The generated protocol has no call cancellation (BINDGEN.md
// 12.2, D15). When the context of a call ends while the server runs it, the
// driver cannot interrupt the statement: it kills the link, returns the
// context's error at once, and discards the connection. The statement is not
// stopped: the server finishes the running sqlite3_step first, and then ends the
// session, which rolls back its transaction (so a statement that was committing
// may have committed). The outcome of an abandoned Exec is unknown, and
// database/sql, which sees the context error and not driver.ErrBadConn, does not
// retry it. sqlite.IsInterrupted is never true. Package sqlite sends a cancel
// request and the server calls sqlite3_interrupt, so a canceled long statement
// stops and the session survives.
//
// Lost links. As in package sqlite, a link lost during Exec or ExecStmt wraps
// sqlite.ErrOutcomeUnknown (only the request that executes the statement counts;
// preparing it changes nothing), and a link lost during any other request wraps
// driver.ErrBadConn.
//
// Requests. A prepared statement is two round trips (prepare_one and
// stmt_info), a prepared Exec is one (exec_stmt is atomic: reset, bind, step,
// counters). A prepared Query is one round trip: reset, bind_values and
// fetch_rows go out together and are answered together; the fetch is only
// pipelined when a failed bind cannot do harm (the statement is read-only or has
// no parameters), otherwise the bind is waited for first. An ad-hoc Exec is two
// round trips, an ad-hoc Query two without arguments and three with. The
// finalize of an ad-hoc statement and the reset of a statement that was closed
// early are submitted at once and their answers collected by the next call.
// Package sqlite answers every one of these in one round trip.
//
// Batches. fetch_rows returns at most the requested number of rows (Config.
// FetchSize, at most 4096) and at most 64 KiB; a big batch is cut by size. The
// next batch is requested after the previous one is decoded (fetch-ahead), as in
// package sqlite. A text or blob that does not fit a batch is read with
// column_chunk, in 1 MiB requests, directly into its final buffer. Results are
// therefore not limited by the 16 MiB message size (package sqlite answers
// SQLITE_TOOBIG for a row above it); requests still are (a text and blob
// arguments total at most 16776960 bytes, and the whole request fits one message).
//
// Column metadata. The names and declared types of a prepared statement are read
// once, when it is prepared. If SQLite recompiles the statement after a schema
// change and the number of columns changes, they are read again; a rename that
// keeps the count is not seen until the statement is prepared again. Package
// sqlite sends the columns with every result.
//
// Transactions. The state is SQLite's (sqlite3_get_autocommit), as in package
// sqlite. BEGIN, COMMIT and ROLLBACK are statements that the driver prepares once
// per session and runs with exec_stmt; the query_only handling of a ReadOnly
// transaction is the driver's (the server does not know transactions): the value
// is read before BEGIN, and restored by Commit, Rollback and the session reset.
// A ReadOnly BeginTx is three round trips.
//
// Opening. The server's open hook applies the path and open policy of the
// hand-written server. The busy timeout and the pragmas are applied by the
// driver after the open, one statement each; a failing pragma fails Connect. A
// path outside the allow directory is a *sqlite.ProtocolError with code
// FORBIDDEN, a missing parent directory is SQLITE_CANTOPEN, as in package
// sqlite.
//
// Threading. The service is @serialize(none): the sessions run concurrently in
// the server, each with its own SQLite connection, which a thread-safe SQLite
// build allows. database/sql serializes the calls of a connection, and a
// connection is used from one goroutine at a time.
package gendriver
