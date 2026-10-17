package sqlite_test

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"gosuda.org/hqq"
	"gosuda.org/hqq/sqlite"
	"gosuda.org/hqq/sqlite/internal/contract"
	"gosuda.org/hqq/sqlite/internal/fakeserver"
)

// contractLink makes heartbeats quick, as the driver tests do.
var contractLink = hqq.LinkOptions{
	HeartbeatInterval: 20 * time.Millisecond,
	PeerTimeout:       300 * time.Millisecond,
}

var contractListenerSeq atomic.Uint64

// contractServer starts a fake server on a fresh listener and returns its name.
// The listener name is short: the macOS shared memory name limit is 31 bytes.
func contractServer(t *testing.T) string {
	t.Helper()
	name := fmt.Sprintf("cfk%d-%d", os.Getpid()%100000, contractListenerSeq.Add(1))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	srv, err := fakeserver.Listen(ctx, name, fakeserver.Options{Link: contractLink})
	if err != nil {
		t.Fatalf("listen %s: %v", name, err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	return name
}

// contractConfig is the connector configuration of the contract runs.
func contractConfig() sqlite.Config {
	return sqlite.Config{
		Database:    "main",
		Timeout:     5 * time.Second,
		BusyTimeout: 2 * time.Second,
		Link:        hqq.Config{Advanced: hqq.AdvancedOptions{Link: contractLink}},
	}
}

// contractFakeDB opens a sql.DB on a new fake server through NewConnector.
func contractFakeDB(t *testing.T) *sql.DB {
	t.Helper()
	name := contractServer(t)
	c, err := sqlite.NewConnector(name, contractConfig())
	if err != nil {
		t.Fatal(err)
	}
	db := sql.OpenDB(c)
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// contractDSNDB opens a sql.DB on a new fake server through the driver registry.
func contractDSNDB(t *testing.T) *sql.DB {
	t.Helper()
	name := contractServer(t)
	db, err := sql.Open(sqlite.DriverName, fmt.Sprintf("hqq://%s?db=main&timeout=5s&busy=2s", name))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestContractFake(t *testing.T) {
	contract.RunContract(t, contractFakeDB, contract.Capabilities{Engine: "fake"})
}

func TestContractDSN(t *testing.T) {
	contract.RunContract(t, contractDSNDB, contract.Capabilities{Engine: "fake"})
}
