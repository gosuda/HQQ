package hqqsqlite_test

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"testing"
	"time"

	hqqsqlite "gosuda.org/hqq/examples/sqlite/driver"
	"gosuda.org/hqq/examples/sqlite/fakeserver"
)

// TestHelperServer is the server process of TestServerCrashReconnect. It runs
// only when the parent sets HQQSQLITE_HELPER_LISTENER, and serves until the
// parent kills it.
func TestHelperServer(t *testing.T) {
	name := os.Getenv("HQQSQLITE_HELPER_LISTENER")
	if name == "" {
		t.Skip("helper process of TestServerCrashReconnect")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := fakeserver.Listen(ctx, name, fakeserver.Options{Link: fastLink}); err != nil {
		fmt.Fprintln(os.Stderr, "helper listen:", err)
		os.Exit(2)
	}
	time.Sleep(time.Hour)
}

// listenAgain listens on name again after a crash: the dead acceptor's name is
// reused once its heartbeat is stale, so retry for a moment.
func listenAgain(t *testing.T, name string) *fakeserver.Server {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		srv, err := fakeserver.Listen(ctx, name, fakeserver.Options{Link: fastLink})
		cancel()
		if err == nil {
			t.Cleanup(func() { _ = srv.Close() })
			return srv
		}
		if time.Now().After(deadline) {
			t.Fatalf("listen again: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestServerCrashReconnect kills the server process with SIGKILL. Calls on the
// broken connection fail with driver.ErrBadConn, and the connector dials the
// link again once a new server listens on the same name.
func TestServerCrashReconnect(t *testing.T) {
	if testing.Short() {
		t.Skip("starts a server process")
	}
	name := listenerName()
	cmd := exec.Command(os.Args[0], "-test.run=^TestHelperServer$", "-test.count=1")
	cmd.Env = append(os.Environ(), "HQQSQLITE_HELPER_LISTENER="+name)
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	killed := false
	defer func() {
		if !killed {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	}()

	c, err := hqqsqlite.NewConnector(name, testConfig("main"))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	db := sql.OpenDB(c)
	defer db.Close()

	// The first Exec waits for the helper to publish its listener.
	mustExec(t, db, "CREATE TABLE k (v INTEGER)")
	mustExec(t, db, "INSERT INTO k VALUES (1)")

	conn, err := db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	if err := cmd.Process.Kill(); err != nil { // SIGKILL: no cleanup runs
		t.Fatal(err)
	}
	_ = cmd.Wait()
	killed = true

	start := time.Now()
	_, err = conn.QueryContext(context.Background(), "SELECT v FROM k")
	if !errors.Is(err, driver.ErrBadConn) {
		t.Fatalf("query after crash: %v, want driver.ErrBadConn", err)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("crash detection took %v", d)
	}

	srv := listenAgain(t, name)
	// The pool discards the broken connection and dials a new link.
	mustExec(t, db, "CREATE TABLE k2 (v INTEGER)")
	var v int
	if err := db.QueryRow("SELECT v FROM k2").Scan(&v); err != sql.ErrNoRows {
		t.Fatalf("query on new server: %v (want no rows)", err)
	}
	if srv.Stats().Opened < 1 {
		t.Fatal("the new server opened no session")
	}
}
