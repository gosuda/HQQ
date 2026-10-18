//go:build unix

package sqlite_test

import (
	"database/sql"
	"os"
	"testing"
	"time"

	"gosuda.org/hqq/bindgen/examples/sqlite3/sqlite3srv"
	"gosuda.org/hqq/sqlite"
	"gosuda.org/hqq/sqlite/internal/contract"
)

func TestMain(m *testing.M) {
	code := m.Run()
	sqlite3srv.Cleanup()
	os.Exit(code)
}

// contractOpenDB starts one generated server for the calling group and returns a
// *sql.DB on a fresh database in its allow directory. The server is stopped by
// t.Cleanup, after the DB is closed.
func contractOpenDB(t *testing.T) *sql.DB {
	t.Helper()
	srv := sqlite3srv.Start(t, sqlite3srv.Config{})
	c, err := sqlite.NewConnector(srv.Listener(), sqlite.Config{
		Database:    "main",
		Timeout:     5 * time.Second,
		BusyTimeout: 2 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	db := sql.OpenDB(c) // DB.Close also closes the connector
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// TestContract runs the shared database/sql contract suite against the C
// server. It skips when no server binary is set or can be built.
func TestContract(t *testing.T) {
	contract.RunContract(t, contractOpenDB, contract.Capabilities{Engine: "c", FullSQL: true})
}
