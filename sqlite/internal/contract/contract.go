// Package contract is the database/sql contract suite of the hqq-sqlite
// driver. RunContract drives one database through database/sql only, so the
// same cases run against the Go fake server and against the C server. Cases
// that need SQL beyond the fake's subset are guarded by Capabilities.FullSQL.
package contract

import (
	"database/sql"
	"sync/atomic"
	"testing"
)

// Capabilities describes the engine behind the database under test.
type Capabilities struct {
	// Engine names the engine ("fake", "c"). It names the subtests and the
	// skip messages.
	Engine string
	// FullSQL is true for real SQLite. It enables the cases that need
	// arithmetic, ORDER BY, typeof(), aggregates, recursive CTEs, and so on.
	FullSQL bool
	// NoInterrupt is true for an engine that cannot interrupt a statement that
	// is running on the server: when the caller's context ends, the driver
	// abandons the call and discards the connection, but the statement runs to
	// its end. The case "deadline interrupts a long query" then runs a statement
	// that ends by itself after a few seconds instead of one that never ends, so
	// that the server can stop afterwards. It still requires that the caller
	// gets the deadline error at the deadline and that the next statement runs.
	// The zero value, an engine that interrupts the statement, keeps the
	// original case.
	NoInterrupt bool
}

// fetchRows is the driver's default fetch size (DefaultFetchRows).
const fetchRows = 256

// manyRows is the row count of the multi-row cases. It is more than eleven
// default batches, so the fetch-ahead runs more than once.
const manyRows = 3000

// tableSeq makes table names unique within a run.
var tableSeq atomic.Uint64

// groups are the top-level case groups. Each group runs against the database
// that openDB returns for it, in order. lifecycle closes its database, so it
// must stay last.
var groups = []struct {
	name string
	run  func(t *testing.T, db *sql.DB, caps Capabilities)
}{
	{"values", testValues},
	{"column_types", testColumnTypes},
	{"rows", testRows},
	{"large", testLarge},
	{"statements", testStatements},
	{"results", testResults},
	{"transactions", testTransactions},
	{"context", testContext},
	{"errors", testErrors},
	{"pool", testPool},
	{"lifecycle", testLifecycle},
}

// RunContract runs the database/sql contract against the databases that
// openDB returns. openDB is called once per group, and it registers the
// cleanup of the database and of anything behind it.
func RunContract(t *testing.T, openDB func(t *testing.T) *sql.DB, caps Capabilities) {
	t.Helper()
	engine := caps.Engine
	if engine == "" {
		engine = "engine"
	}
	t.Run(engine, func(t *testing.T) {
		for _, g := range groups {
			t.Run(g.name, func(t *testing.T) {
				g.run(t, openDB(t), caps)
			})
		}
	})
}
