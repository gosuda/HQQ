package e2e

import (
	"database/sql"
	"testing"

	"gosuda.org/hqq/sqlite/internal/contract"
)

// contractOpenDB starts one C server process for the calling test and returns a
// *sql.DB on a fresh database in its allow directory. The server is stopped by
// t.Cleanup, after the DB is closed.
func contractOpenDB(t *testing.T) *sql.DB {
	t.Helper()
	_, name, _ := startTestServer(t)
	return openDB(t, name, "main")
}

// TestContract runs the shared database/sql contract suite against the C server.
// It skips without HQQ_SQLITE_SERVER, like the other tests in this package.
func TestContract(t *testing.T) {
	contract.RunContract(t, contractOpenDB, contract.Capabilities{Engine: "c", FullSQL: true})
}
