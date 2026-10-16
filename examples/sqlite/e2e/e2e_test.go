package e2e

import (
	"bytes"
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	hqqsqlite "gosuda.org/hqq/examples/sqlite/driver"
	"gosuda.org/hqq/examples/sqlite/wire"
)

// TestRoundTripValues covers every value type, including NULL and blobs.
func TestRoundTripValues(t *testing.T) {
	_, name, _ := startTestServer(t)
	db := openDB(t, name, "main")
	mustExec(t, db, "CREATE TABLE v (i INTEGER, f REAL, s TEXT, b BLOB, n TEXT, e BLOB)")
	mustExec(t, db, "INSERT INTO v VALUES (?, ?, ?, ?, ?, ?)",
		int64(-42), 1.5, "héllo", []byte{0, 1, 2, 0xff}, nil, []byte{})

	var (
		i int64
		f float64
		s string
		b []byte
		n sql.NullString
		e []byte
	)
	if err := db.QueryRow("SELECT i, f, s, b, n, e FROM v").Scan(&i, &f, &s, &b, &n, &e); err != nil {
		t.Fatal(err)
	}
	if i != -42 || f != 1.5 || s != "héllo" {
		t.Fatalf("scalars: %d %v %q", i, f, s)
	}
	if !bytes.Equal(b, []byte{0, 1, 2, 0xff}) {
		t.Fatalf("blob: %x", b)
	}
	if n.Valid {
		t.Fatalf("NULL came back as %q", n.String)
	}
	if len(e) != 0 {
		t.Fatalf("empty blob: %x", e)
	}
	var types string
	if err := db.QueryRow("SELECT typeof(i) || ',' || typeof(f) || ',' || typeof(s) || ',' || typeof(b) || ',' || typeof(n) || ',' || typeof(e) FROM v").Scan(&types); err != nil {
		t.Fatal(err)
	}
	if want := "integer,real,text,blob,null,blob"; types != want {
		t.Fatalf("stored types %q, want %q", types, want)
	}
}

// TestPreparedAndNamedStatements covers Prepare/QueryStmt/ExecStmt and :name binding.
func TestPreparedAndNamedStatements(t *testing.T) {
	_, name, _ := startTestServer(t)
	db := openDB(t, name, "main")
	mustExec(t, db, "CREATE TABLE p (id INTEGER PRIMARY KEY, name TEXT)")

	ins, err := db.Prepare("INSERT INTO p (id, name) VALUES (?, ?)")
	if err != nil {
		t.Fatal(err)
	}
	defer ins.Close()
	for i := 1; i <= 3; i++ {
		if _, err := ins.Exec(i, fmt.Sprintf("row%d", i)); err != nil {
			t.Fatal(err)
		}
	}
	mustExec(t, db, "INSERT INTO p (id, name) VALUES (:id, :name)", sql.Named("id", 10), sql.Named("name", "named"))

	var got string
	if err := db.QueryRow("SELECT name FROM p WHERE id = :id", sql.Named("id", 10)).Scan(&got); err != nil || got != "named" {
		t.Fatalf("named query: %q, %v", got, err)
	}
	sel, err := db.Prepare("SELECT name FROM p WHERE id = ?")
	if err != nil {
		t.Fatal(err)
	}
	defer sel.Close()
	if err := sel.QueryRow(2).Scan(&got); err != nil || got != "row2" {
		t.Fatalf("prepared query: %q, %v", got, err)
	}
	if n := queryInt(t, db, "SELECT count(*) FROM p"); n != 4 {
		t.Fatalf("count = %d, want 4", n)
	}

	// A name the statement does not have is a SQLite RANGE error (PROTOCOL.md section 4).
	_, err = db.Exec("SELECT :nope", sql.Named("other", 1))
	if sqliteCode(err) != 25 {
		t.Fatalf("unknown parameter: %v (code %d), want SQLITE_RANGE", err, sqliteCode(err))
	}
}

// TestMultipleBatchesAndSmallFetch reads 1000 rows in batches of 7.
func TestMultipleBatchesAndSmallFetch(t *testing.T) {
	_, name, _ := startTestServer(t)
	cfg := driverConfig("main")
	cfg.FetchSize = 7
	db := openDBWith(t, name, cfg)
	mustExec(t, db, "CREATE TABLE t (x INTEGER)")
	mustExec(t, db, "WITH RECURSIVE c(x) AS (SELECT 1 UNION ALL SELECT x + 1 FROM c WHERE x < 1000) INSERT INTO t SELECT x FROM c")

	rows, err := db.Query("SELECT x FROM t ORDER BY x")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var n, sum int64
	for rows.Next() {
		var x int64
		if err := rows.Scan(&x); err != nil {
			t.Fatal(err)
		}
		n++
		sum += x
		if x != n {
			t.Fatalf("row %d is %d", n, x)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if n != 1000 || sum != 500500 {
		t.Fatalf("rows=%d sum=%d, want 1000 and 500500", n, sum)
	}
}

// TestLargeBlobsAndRowLimit moves blobs of several batches and checks that a
// row above the 16 MiB message limit is refused with SQLITE_TOOBIG.
func TestLargeBlobsAndRowLimit(t *testing.T) {
	_, name, _ := startTestServer(t)
	db := openDB(t, name, "main")
	mustExec(t, db, "CREATE TABLE big (id INTEGER, b BLOB)")
	for _, size := range []int{3 << 20, 12 << 20} {
		payload := make([]byte, size)
		for i := range payload {
			payload[i] = byte(i*7 + size)
		}
		mustExec(t, db, "INSERT INTO big VALUES (?, ?)", size, payload)
		var back []byte
		if err := db.QueryRow("SELECT b FROM big WHERE id = ?", size).Scan(&back); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(back, payload) {
			t.Fatalf("blob of %d bytes came back different", size)
		}
	}

	var n []byte
	err := db.QueryRow("SELECT zeroblob(17 * 1024 * 1024)").Scan(&n)
	if err == nil {
		t.Fatal("a 17 MiB row was returned")
	}
	if sqliteCode(err) != 18 {
		t.Fatalf("oversized row: %v (code %d), want SQLITE_TOOBIG (18)", err, sqliteCode(err))
	}
	// The connection stays usable after a SQL error (PROTOCOL.md section 7).
	if got := queryInt(t, db, "SELECT 7"); got != 7 {
		t.Fatalf("after the error: %d", got)
	}
}

// TestTransactionsCommitRollbackAndVisibility checks commit, rollback,
// isolation between sessions, and read-only transactions.
func TestTransactionsCommitRollbackAndVisibility(t *testing.T) {
	_, name, _ := startTestServer(t)
	db := openDB(t, name, "main")
	mustExec(t, db, "CREATE TABLE tx (v INTEGER)")
	ctx, cancel := ctxWithin(10 * time.Second)
	defer cancel()

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, tx, "INSERT INTO tx VALUES (1)")
	other, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	var seen int64
	if err := other.QueryRowContext(ctx, "SELECT count(*) FROM tx").Scan(&seen); err != nil || seen != 0 {
		t.Fatalf("uncommitted row visible to another session: %d, %v", seen, err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if n := queryInt(t, db, "SELECT count(*) FROM tx"); n != 0 {
		t.Fatalf("rolled back row present: %d", n)
	}

	tx, err = db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, tx, "INSERT INTO tx VALUES (2)")
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := other.QueryRowContext(ctx, "SELECT count(*) FROM tx").Scan(&seen); err != nil || seen != 1 {
		t.Fatalf("committed row not visible: %d, %v", seen, err)
	}

	ro, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	_, err = ro.Exec("INSERT INTO tx VALUES (3)")
	if sqliteCode(err) != 8 {
		t.Fatalf("write in a read-only transaction: %v (code %d), want SQLITE_READONLY", err, sqliteCode(err))
	}
	_ = ro.Rollback()
	if n := queryInt(t, db, "SELECT count(*) FROM tx"); n != 1 {
		t.Fatalf("rows after read-only tx: %d", n)
	}
}

// TestCancelMidQuery cancels a recursive CTE that would count to 1e9. The
// request must return promptly: the server interrupts the statement.
func TestCancelMidQuery(t *testing.T) {
	_, name, _ := startTestServer(t)
	db := openDB(t, name, "main")
	const long = "WITH RECURSIVE c(x) AS (SELECT 1 UNION ALL SELECT x + 1 FROM c WHERE x < 1000000000) SELECT count(*) FROM c"

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	var n int64
	err := db.QueryRowContext(ctx, long).Scan(&n)
	elapsed := time.Since(start)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("canceled query: %v, want context.DeadlineExceeded", err)
	}
	if elapsed > 5*time.Second {
		t.Fatalf("canceled query returned after %v: the server did not interrupt it", elapsed)
	}
	if got := queryInt(t, db, "SELECT 1"); got != 1 {
		t.Fatalf("after cancel: %d", got)
	}
}

// TestFiftyConcurrentConnections holds 50 connections open at once, then
// writes from all of them, and checks every row arrived.
func TestFiftyConcurrentConnections(t *testing.T) {
	_, name, _ := startTestServer(t)
	db := openDB(t, name, "main")
	db.SetMaxOpenConns(50)
	mustExec(t, db, "CREATE TABLE conc (id INTEGER PRIMARY KEY, g INTEGER)")

	const clients = 50
	ctx, cancel := ctxWithin(30 * time.Second)
	defer cancel()
	opened := make(chan struct{}, clients)
	release := make(chan struct{})
	var wg sync.WaitGroup
	errs := make(chan error, 2*clients) // each client may report two errors
	for g := range clients {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			conn, err := db.Conn(ctx)
			if err != nil {
				errs <- fmt.Errorf("conn %d: %w", g, err)
				opened <- struct{}{}
				return
			}
			defer conn.Close()
			if _, err := conn.ExecContext(ctx, "SELECT 1"); err != nil {
				errs <- fmt.Errorf("select %d: %w", g, err)
			}
			opened <- struct{}{}
			<-release
			if _, err := conn.ExecContext(ctx, "INSERT INTO conc (g) VALUES (?)", g); err != nil {
				errs <- fmt.Errorf("insert %d: %w", g, err)
			}
		}(g)
	}
	for range clients {
		<-opened
	}
	close(release)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	if n := queryInt(t, db, "SELECT count(*) FROM conc"); n != clients {
		t.Fatalf("rows = %d, want %d", n, clients)
	}
}

// TestHelperCrashClient is the client process of TestClientCrashRollsBack. It
// begins a transaction, writes, reports, and waits to be killed. It runs only
// when HQQSQLITE_E2E_HELPER is set.
func TestHelperCrashClient(t *testing.T) {
	spec := os.Getenv("HQQSQLITE_E2E_HELPER")
	if spec == "" {
		t.Skip("helper process of TestClientCrashRollsBack")
	}
	parts := strings.SplitN(spec, "|", 2)
	c, err := hqqsqlite.NewConnector(parts[0], driverConfig(parts[1]))
	if err != nil {
		fmt.Fprintln(os.Stderr, "helper:", err)
		os.Exit(2)
	}
	db := sql.OpenDB(c)
	tx, err := db.Begin()
	if err != nil {
		fmt.Fprintln(os.Stderr, "helper begin:", err)
		os.Exit(2)
	}
	if _, err := tx.Exec("INSERT INTO k VALUES (99)"); err != nil {
		fmt.Fprintln(os.Stderr, "helper insert:", err)
		os.Exit(2)
	}
	fmt.Println("in-tx")
	time.Sleep(time.Hour) // the parent kills this process
}

// TestClientCrashRollsBack kills a client in the middle of a transaction. The
// server notices that the link is gone and rolls the transaction back, so
// another session can write, and the row is not there.
func TestClientCrashRollsBack(t *testing.T) {
	_, name, _ := startTestServer(t)
	db := openDB(t, name, "main")
	mustExec(t, db, "CREATE TABLE k (v INTEGER)")

	cmd := exec.Command(os.Args[0], "-test.run=^TestHelperCrashClient$", "-test.count=1")
	cmd.Env = append(os.Environ(), "HQQSQLITE_E2E_HELPER="+name+"|main")
	cmd.Stderr = os.Stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	reported := make(chan struct{})
	go func() {
		buf := make([]byte, 64)
		var got []byte
		for {
			n, err := stdout.Read(buf)
			got = append(got, buf[:n]...)
			if bytes.Contains(got, []byte("in-tx")) {
				close(reported)
				return
			}
			if err != nil {
				return
			}
		}
	}()
	select {
	case <-reported:
	case <-time.After(15 * time.Second):
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		t.Fatal("the helper did not start its transaction")
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()

	// The insert waits for the server's rollback (busy timeout 5s); the
	// server's peer timeout is 300 ms, so this is quick.
	other := openDB(t, name, "main")
	start := time.Now()
	mustExec(t, other, "INSERT INTO k VALUES (1)")
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("write after the crash took %v", d)
	}
	var v int64
	rows, err := other.Query("SELECT v FROM k ORDER BY v")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var vals []int64
	for rows.Next() {
		if err := rows.Scan(&v); err != nil {
			t.Fatal(err)
		}
		vals = append(vals, v)
	}
	if len(vals) != 1 || vals[0] != 1 {
		t.Fatalf("rows after the crash: %v, want [1] (the crashed transaction rolled back)", vals)
	}
}

// startAgain starts a server on name after a crash. The dead listener's name
// is free once its heartbeat is stale, so retry for a while.
func startAgain(t *testing.T, bin, name, dir string) *server {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		srv, err := startServer(bin, name, append([]string{"--allow-dir", dir}, fastServerFlags...)...)
		if err == nil {
			t.Cleanup(srv.close)
			return srv
		}
		if time.Now().After(deadline) {
			t.Fatalf("restart: %v", err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// TestServerSIGKILLThenReconnect kills the server with SIGKILL. The broken
// connection returns driver.ErrBadConn. A new server on the same name serves
// new connections, and the data written before the crash is still there.
func TestServerSIGKILLThenReconnect(t *testing.T) {
	bin := serverBinary(t)
	dir := t.TempDir()
	name := listenerName()
	srv, err := startServer(bin, name, append([]string{"--allow-dir", dir}, fastServerFlags...)...)
	if err != nil {
		t.Fatal(err)
	}
	defer srv.close()

	db := openDB(t, name, "main")
	mustExec(t, db, "CREATE TABLE k (v INTEGER)")
	mustExec(t, db, "INSERT INTO k VALUES (1)")
	conn, err := db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	srv.kill()
	start := time.Now()
	_, err = conn.QueryContext(context.Background(), "SELECT v FROM k")
	if !errors.Is(err, driver.ErrBadConn) {
		t.Fatalf("query after SIGKILL: %v, want driver.ErrBadConn", err)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("crash detection took %v", d)
	}

	_ = startAgain(t, bin, name, dir)
	if n := queryInt(t, db, "SELECT count(*) FROM k"); n != 1 {
		t.Fatalf("rows after restart: %d, want 1", n)
	}
	mustExec(t, db, "INSERT INTO k VALUES (2)")
	if n := queryInt(t, db, "SELECT count(*) FROM k"); n != 2 {
		t.Fatalf("rows after the new write: %d, want 2", n)
	}
}

// TestPathPolicy checks that paths outside --allow-dir are refused with
// FORBIDDEN, and that symbolic links out of the directory do not help.
func TestPathPolicy(t *testing.T) {
	_, name, dir := startTestServer(t)
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "victim.db"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "victim.db"), filepath.Join(dir, "link.db")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "missing.db"), filepath.Join(dir, "dangling.db")); err != nil {
		t.Fatal(err)
	}
	cases := []string{
		"../escape.db",
		"sub/../../escape.db",
		filepath.Join(outside, "victim.db"),
		"link.db",
		"dangling.db",
		"..",
	}
	for _, db := range cases {
		d := openDB(t, name, db)
		err := d.Ping()
		if protoCode(err) != wire.ProtoForbidden {
			t.Errorf("open %q: %v, want FORBIDDEN", db, err)
		}
		_ = d.Close()
	}
	// A path inside the directory is allowed.
	inside := openDB(t, name, "inside.db")
	if err := inside.Ping(); err != nil {
		t.Fatalf("open inside.db: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "inside.db")); err != nil {
		t.Fatalf("database file not created in the allow directory: %v", err)
	}
}

// TestAliasOnlyPolicy starts a server with an alias and no allow directory:
// the alias works, and every raw path is FORBIDDEN.
func TestAliasOnlyPolicy(t *testing.T) {
	bin := serverBinary(t)
	dir := t.TempDir()
	name := listenerName()
	appPath := filepath.Join(dir, "app.db")
	args := append([]string{"--alias", "app=" + appPath}, fastServerFlags...)
	srv, err := startServer(bin, name, args...)
	if err != nil {
		t.Fatal(err)
	}
	defer srv.close()

	app := openDB(t, name, "app")
	mustExec(t, app, "CREATE TABLE a (v INTEGER)")
	if _, err := os.Stat(appPath); err != nil {
		t.Fatalf("alias database not created: %v", err)
	}
	for _, raw := range []string{"main", appPath} {
		d := openDB(t, name, raw)
		if err := d.Ping(); protoCode(err) != wire.ProtoForbidden {
			t.Errorf("raw path %q: %v, want FORBIDDEN", raw, err)
		}
		_ = d.Close()
	}
}

// TestSessionLimit checks --max-sessions: the open beyond the limit is LIMIT.
func TestSessionLimit(t *testing.T) {
	_, name, _ := startTestServer(t, "--max-sessions", "2")
	db := openDB(t, name, "main")
	db.SetMaxOpenConns(4)
	ctx, cancel := ctxWithin(10 * time.Second)
	defer cancel()
	c1, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer c1.Close()
	c2, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer c2.Close()
	_, err = db.Conn(ctx)
	if protoCode(err) != wire.ProtoLimit {
		t.Fatalf("third session: %v, want LIMIT", err)
	}
}

// TestErrorMappingAndBusy checks SQLite error codes, and a busy lock that
// another session holds beyond the busy timeout.
func TestErrorMappingAndBusy(t *testing.T) {
	_, name, _ := startTestServer(t)
	db := openDB(t, name, "main")
	mustExec(t, db, "CREATE TABLE u (id INTEGER PRIMARY KEY, k TEXT UNIQUE)")
	mustExec(t, db, "INSERT INTO u VALUES (1, 'a')")
	_, err := db.Exec("INSERT INTO u VALUES (2, 'a')")
	if sqliteCode(err) != hqqsqlite.CodeConstraintUnique || !errors.Is(err, hqqsqlite.ErrConstraint) {
		t.Fatalf("unique violation: %v (code %d)", err, sqliteCode(err))
	}
	_, err = db.Exec("SELEC nonsense")
	if sqliteCode(err) != 1 {
		t.Fatalf("syntax error: %v (code %d), want SQLITE_ERROR", err, sqliteCode(err))
	}

	// A writer holds the lock; a second writer with a short busy timeout fails with BUSY.
	holder, err := db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Close()
	if _, err := holder.ExecContext(context.Background(), "BEGIN IMMEDIATE"); err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = holder.ExecContext(context.Background(), "ROLLBACK") }()
	cfg := driverConfig("main")
	cfg.BusyTimeout = 100 * time.Millisecond
	waiter := openDBWith(t, name, cfg)
	_, err = waiter.Exec("INSERT INTO u VALUES (3, 'c')")
	if !errors.Is(err, hqqsqlite.ErrBusy) {
		t.Fatalf("write under a held lock: %v, want ErrBusy", err)
	}
}

// TestGracefulShutdown sends SIGTERM. The server exits with status 0, and the
// open connection then reports driver.ErrBadConn.
func TestGracefulShutdown(t *testing.T) {
	bin := serverBinary(t)
	dir := t.TempDir()
	name := listenerName()
	srv, err := startServer(bin, name, append([]string{"--allow-dir", dir}, fastServerFlags...)...)
	if err != nil {
		t.Fatal(err)
	}
	db := openDB(t, name, "main")
	mustExec(t, db, "CREATE TABLE g (v INTEGER)")
	conn, err := db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	if err := srv.term(10 * time.Second); err != nil {
		t.Fatalf("graceful exit: %v; stderr: %s", err, srv.stderr.String())
	}
	if !strings.Contains(srv.stderr.String(), "shutting down") {
		t.Fatalf("no shutdown message: %s", srv.stderr.String())
	}
	_, err = conn.ExecContext(context.Background(), "INSERT INTO g VALUES (1)")
	if !errors.Is(err, driver.ErrBadConn) {
		t.Fatalf("after shutdown: %v, want driver.ErrBadConn", err)
	}
}

// TestStartupRejectsBadArguments checks that the server refuses a bad option
// without listening (exit status 2).
func TestStartupRejectsBadArguments(t *testing.T) {
	bin := serverBinary(t)
	cmd := exec.Command(bin, listenerName(), "--allow-dir", filepath.Join(t.TempDir(), "missing"))
	out, err := cmd.CombinedOutput()
	var ee *exec.ExitError
	if !errors.As(err, &ee) || ee.ExitCode() != 2 {
		t.Fatalf("missing --allow-dir: %v, %s", err, out)
	}
}
