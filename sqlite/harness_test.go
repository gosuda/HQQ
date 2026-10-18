//go:build unix

package sqlite_test

import (
	"database/sql"
	"fmt"
	"testing"

	"gosuda.org/hqq/bindgen/examples/sqlite3/sqlite3srv"
	"gosuda.org/hqq/sqlite"
)

// server is a running server process.
type server struct {
	listener string
	dir      string
}

// startGenServer runs the C server on a fresh listener and database
// directory, and stops it when tb ends.
func startGenServer(tb testing.TB) *server {
	tb.Helper()
	srv := sqlite3srv.Start(tb, sqlite3srv.Config{})
	return &server{listener: srv.Listener(), dir: srv.AllowDir}
}

// openBenchDB returns a *sql.DB on database db of the server, through the
// registry of database/sql. opts is appended to the DSN ("&fetch=256", for
// example). The DB is closed when tb ends, before the server stops.
func openBenchDB(tb testing.TB, s *server, db, opts string) *sql.DB {
	tb.Helper()
	d, err := sql.Open(sqlite.DriverName, fmt.Sprintf("hqq://%s?db=%s%s", s.listener, db, opts))
	if err != nil {
		tb.Fatalf("open: %v", err)
	}
	tb.Cleanup(func() { _ = d.Close() })
	return d
}
