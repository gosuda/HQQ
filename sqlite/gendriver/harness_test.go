//go:build unix

package gendriver_test

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"testing"
	"time"

	"gosuda.org/hqq/bindgen/examples/sqlite3/sqlite3srv"
	"gosuda.org/hqq/launch"
	"gosuda.org/hqq/secureexec"
	"gosuda.org/hqq/sqlite/gendriver"
)

// oldServerEnv names hqq-sqlite-server (examples/sqlite/server), the hand-written
// service that the driver hqq-sqlite talks to. The generated server is found or
// built by sqlite3srv (HQQ_SQLITE3GEN_SERVER, bindgen/examples/sqlite3/build.sh).
// A test or benchmark whose server is not available skips.
const oldServerEnv = "HQQ_SQLITE_SERVER"

// target is one engine of the benchmarks: a server and the database/sql driver
// that talks to it.
type target struct {
	name   string // sub-benchmark name
	driver string // database/sql driver name
	start  func(tb testing.TB) *server
}

var (
	oldTarget = target{name: "hqq-sqlite", driver: "hqq-sqlite", start: startOld}
	genTarget = target{name: "hqq-sqlite-gen", driver: gendriver.DriverName, start: startGenServer}
)

// server is a running server process.
type server struct {
	listener string
	dir      string
}

// startOld runs the hand-written server on a fresh listener and database
// directory, and stops it when tb ends.
func startOld(tb testing.TB) *server {
	tb.Helper()
	bin := os.Getenv(oldServerEnv)
	if bin == "" {
		tb.Skipf("%s is not set: build hqq-sqlite-server (examples/sqlite/server/README.md) and point it there", oldServerEnv)
	}
	sum, _, err := secureexec.HashFile(bin)
	if err != nil {
		tb.Fatalf("hash %s: %v", bin, err)
	}
	listener, err := launch.NewListenerName("sqo")
	if err != nil {
		tb.Fatal(err)
	}
	dir := tb.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	srv, err := launch.Start(ctx, launch.Config{
		Binary:   bin,
		SHA256:   sum,
		Listener: listener,
		Args:     []string{listener, "--allow-dir", dir},
		Stderr:   os.Stderr,
	})
	if err != nil {
		tb.Fatalf("start hqq-sqlite-server: %v", err)
	}
	tb.Cleanup(func() {
		if err := srv.Close(); err != nil {
			tb.Logf("stop hqq-sqlite-server: %v", err)
		}
	})
	return &server{listener: listener, dir: dir}
}

// startGenServer runs the generated server on a fresh listener and database
// directory, and stops it when tb ends.
func startGenServer(tb testing.TB) *server {
	tb.Helper()
	srv := sqlite3srv.Start(tb, sqlite3srv.Config{})
	return &server{listener: srv.Listener(), dir: srv.AllowDir}
}

// open returns a *sql.DB on database db of the server, through the registry of
// database/sql. opts is appended to the DSN ("&fetch=256", for example). The DB
// is closed when tb ends, before the server stops.
func (tg target) open(tb testing.TB, s *server, db, opts string) *sql.DB {
	tb.Helper()
	d, err := sql.Open(tg.driver, fmt.Sprintf("hqq://%s?db=%s%s", s.listener, db, opts))
	if err != nil {
		tb.Fatalf("open %s: %v", tg.name, err)
	}
	tb.Cleanup(func() { _ = d.Close() })
	return d
}
