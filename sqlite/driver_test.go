//go:build unix

package sqlite_test

import (
	"bytes"
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"gosuda.org/hqq"
	"gosuda.org/hqq/bindgen/examples/sqlite3/sqlite3srv"
	"gosuda.org/hqq/bindgen/rt"
	"gosuda.org/hqq/sqlite"
	"gosuda.org/hqq/sqlite/internal/wire"
)

// fastLink makes heartbeats quick, so a dead server is seen within a few hundred
// milliseconds instead of seconds.
var fastLink = hqq.LinkOptions{
	HeartbeatInterval: 20 * time.Millisecond,
	PeerTimeout:       300 * time.Millisecond,
}

// genConfig is the connector configuration of the tests.
func genConfig(db string) sqlite.Config {
	return sqlite.Config{
		Database:    db,
		Timeout:     10 * time.Second,
		BusyTimeout: 5 * time.Second,
		Session:     rt.Options{Link: fastLink},
	}
}

// openGen returns a sql.DB on database db of the server, with cfg applied.
func openGen(t *testing.T, srv *sqlite3srv.Server, cfg sqlite.Config) *sql.DB {
	t.Helper()
	c, err := sqlite.NewConnector(srv.Listener(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	db := sql.OpenDB(c)
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func startGen(t *testing.T) (*sqlite3srv.Server, *sql.DB) {
	t.Helper()
	srv := sqlite3srv.Start(t, sqlite3srv.Config{})
	return srv, openGen(t, srv, genConfig("main"))
}

func mustExec(t *testing.T, db interface {
	Exec(string, ...any) (sql.Result, error)
}, query string, args ...any) sql.Result {
	t.Helper()
	res, err := db.Exec(query, args...)
	if err != nil {
		t.Fatalf("exec %q: %v", query, err)
	}
	return res
}

func queryInt(t *testing.T, db *sql.DB, query string, args ...any) int64 {
	t.Helper()
	var v int64
	if err := db.QueryRow(query, args...).Scan(&v); err != nil {
		t.Fatalf("query %q: %v", query, err)
	}
	return v
}

func sqliteCode(err error) int {
	var se *sqlite.Error
	if errors.As(err, &se) {
		return se.Code
	}
	return -1
}

func protoCode(err error) wire.ProtoCode {
	var pe *sqlite.ProtocolError
	if errors.As(err, &pe) {
		return pe.Code
	}
	return 0
}

// TestRegistered opens a database through the registry of database/sql, with a DSN
// that sets every option, and checks that the options took effect.
func TestRegistered(t *testing.T) {
	srv := sqlite3srv.Start(t, sqlite3srv.Config{})
	dsn := fmt.Sprintf("hqq://%s?db=reg.db&timeout=5s&busy=250ms&fetch=3&txlock=immediate&pragma.journal_mode=WAL&pragma.synchronous=OFF", srv.Listener())
	db, err := sql.Open(sqlite.DriverName, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var mode string
	if err := db.QueryRow("PRAGMA journal_mode").Scan(&mode); err != nil || mode != "wal" {
		t.Fatalf("journal_mode = %q, %v; want wal", mode, err)
	}
	if n := queryInt(t, db, "PRAGMA synchronous"); n != 0 {
		t.Fatalf("synchronous = %d, want 0 (OFF)", n)
	}
	if n := queryInt(t, db, "PRAGMA busy_timeout"); n != 250 {
		t.Fatalf("busy_timeout = %d, want 250", n)
	}
	if _, err := os.Stat(filepath.Join(srv.AllowDir, "reg.db")); err != nil {
		t.Fatalf("database file not created in the allow directory: %v", err)
	}
	// fetch=3 with 10 rows: four batches.
	mustExec(t, db, "CREATE TABLE r (x INTEGER)")
	mustExec(t, db, "WITH RECURSIVE c(x) AS (SELECT 1 UNION ALL SELECT x + 1 FROM c WHERE x < 10) INSERT INTO r SELECT x FROM c")
	rows, err := db.Query("SELECT x FROM r ORDER BY x")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []int64
	for rows.Next() {
		var x int64
		if err := rows.Scan(&x); err != nil {
			t.Fatal(err)
		}
		got = append(got, x)
	}
	if err := rows.Err(); err != nil || len(got) != 10 || got[9] != 10 {
		t.Fatalf("rows = %v, %v", got, err)
	}
	// txlock=immediate: a second connection cannot write while the transaction runs.
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	other, err := db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	_, err = other.ExecContext(context.Background(), "INSERT INTO r VALUES (99)")
	if !errors.Is(err, sqlite.ErrBusy) {
		t.Fatalf("write beside an IMMEDIATE transaction: %v, want ErrBusy", err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
}

// TestOpenModesAndPolicy covers the open mode and the path policy of the server.
func TestOpenModesAndPolicy(t *testing.T) {
	srv, db := startGen(t)
	mustExec(t, db, "CREATE TABLE m (v INTEGER)")
	mustExec(t, db, "INSERT INTO m VALUES (1)")
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	ro := genConfig("main")
	ro.Mode = sqlite.ModeReadOnly
	rdb := openGen(t, srv, ro)
	if n := queryInt(t, rdb, "SELECT v FROM m"); n != 1 {
		t.Fatalf("read-only read = %d", n)
	}
	if _, err := rdb.Exec("INSERT INTO m VALUES (2)"); !sqlite.IsReadOnly(err) {
		t.Fatalf("write to a read-only database: %v, want IsReadOnly", err)
	}

	rw := genConfig("absent.db")
	rw.Mode = sqlite.ModeReadWrite
	if err := openGen(t, srv, rw).Ping(); sqliteCode(err) != sqlite.CodeCantOpen {
		t.Fatalf("mode=rw on a missing database: %v, want SQLITE_CANTOPEN", err)
	}
	if _, err := os.Stat(filepath.Join(srv.AllowDir, "absent.db")); err == nil {
		t.Fatal("mode=rw created the database")
	}

	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "victim.db"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"../escape.db", "sub/../../escape.db", filepath.Join(outside, "victim.db"), ".."} {
		err := openGen(t, srv, genConfig(name)).Ping()
		if protoCode(err) != wire.ProtoForbidden {
			t.Errorf("open %q: %v, want FORBIDDEN", name, err)
		}
	}
	if err := openGen(t, srv, genConfig("inside.db")).Ping(); err != nil {
		t.Fatalf("open inside.db: %v", err)
	}
}

// TestNamedParameters checks the mapping of named and mixed arguments to the
// parameters of a statement.
func TestNamedParameters(t *testing.T) {
	_, db := startGen(t)
	mustExec(t, db, "CREATE TABLE n (id INTEGER PRIMARY KEY, a TEXT, b TEXT)")
	mustExec(t, db, "INSERT INTO n VALUES (:id, @a, $b)", sql.Named("id", 1), sql.Named("a", "x"), sql.Named("b", "y"))
	// A name used twice in the statement binds both places.
	var a, b string
	if err := db.QueryRow("SELECT a, b FROM n WHERE id = :id AND a = :a", sql.Named("id", 1), sql.Named("a", "x")).Scan(&a, &b); err != nil || a != "x" || b != "y" {
		t.Fatalf("named select: %q %q %v", a, b, err)
	}
	var s string
	if err := db.QueryRow("SELECT :v || :v", sql.Named("v", "ab")).Scan(&s); err != nil || s != "abab" {
		t.Fatalf("name used twice: %q, %v", s, err)
	}
	// Numbered parameters follow the ordinal.
	if err := db.QueryRow("SELECT ?2 || ?1", "a", "b").Scan(&s); err != nil || s != "ba" {
		t.Fatalf("numbered parameters: %q, %v", s, err)
	}
	// A prepared statement is reused with named arguments.
	st, err := db.Prepare("SELECT a FROM n WHERE id = :id")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	for range 3 {
		if err := st.QueryRow(sql.Named("id", 1)).Scan(&s); err != nil || s != "x" {
			t.Fatalf("prepared named: %q, %v", s, err)
		}
	}
	// Unknown parameter: SQLITE_RANGE.
	_, err = db.Exec("INSERT INTO n VALUES (:id, 'x', 'y')", sql.Named("nope", 1))
	if sqliteCode(err) != 25 {
		t.Fatalf("unknown parameter: %v (code %d), want SQLITE_RANGE", err, sqliteCode(err))
	}
	// Too many positional arguments: SQLITE_RANGE from the bind.
	_, err = db.Exec("INSERT INTO n VALUES (?, 'x', 'y')", 5, 6)
	if sqliteCode(err) != 25 {
		t.Fatalf("too many arguments: %v (code %d), want SQLITE_RANGE", err, sqliteCode(err))
	}
	if n := queryInt(t, db, "SELECT count(*) FROM n"); n != 1 {
		t.Fatalf("rows after the failed inserts: %d", n)
	}
}

// TestBigCells reads rows with several cells that do not fit a batch, text with a
// NUL byte, and cells of a time column.
func TestBigCells(t *testing.T) {
	_, db := startGen(t)
	mustExec(t, db, "CREATE TABLE bc (id INTEGER PRIMARY KEY, t TEXT, b BLOB, d DATETIME, small TEXT)")
	t1 := strings.Repeat("é", 100_000)
	b1 := bytes.Repeat([]byte{1, 2, 3, 4, 5, 6, 7}, 40_000)
	b2 := make([]byte, 2<<20+17)
	for i := range b2 {
		b2[i] = byte(i * 31)
	}
	mustExec(t, db, "INSERT INTO bc VALUES (1, ?, ?, '2024-03-05 10:20:30', 'one')", t1, b1)
	mustExec(t, db, "INSERT INTO bc VALUES (2, 'small', ?, '2024-03-06', 'two')", b2)
	mustExec(t, db, "INSERT INTO bc VALUES (3, ?, X'', NULL, 'three')", "nul\x00inside")
	mustExec(t, db, "INSERT INTO bc VALUES (4, ?, X'00', NULL, NULL)", strings.Repeat("x", 70_000)+"\x00tail")
	mustExec(t, db, "INSERT INTO bc VALUES (5, 'last', X'ff', NULL, 'five')")

	rows, err := db.Query("SELECT id, t, b, d, small FROM bc ORDER BY id")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	type rec struct {
		id    int64
		t     string
		b     []byte
		d     sql.NullTime
		small sql.NullString
	}
	var got []rec
	for rows.Next() {
		var r rec
		if err := rows.Scan(&r.id, &r.t, &r.b, &r.d, &r.small); err != nil {
			t.Fatal(err)
		}
		got = append(got, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(got) != 5 {
		t.Fatalf("got %d rows, want 5", len(got))
	}
	if got[0].t != t1 || !bytes.Equal(got[0].b, b1) || !got[0].d.Valid || got[0].d.Time.Year() != 2024 || got[0].small.String != "one" {
		t.Fatalf("row 1 differs: t %d bytes, b %d bytes, d %v, small %q", len(got[0].t), len(got[0].b), got[0].d, got[0].small.String)
	}
	if got[1].t != "small" || !bytes.Equal(got[1].b, b2) || got[1].d.Time.Day() != 6 || got[1].small.String != "two" {
		t.Fatalf("row 2 differs: b %d bytes, d %v", len(got[1].b), got[1].d)
	}
	if got[2].t != "nul\x00inside" || len(got[2].b) != 0 || got[2].small.String != "three" {
		t.Fatalf("row 3 (NUL in text) differs: %q %v", got[2].t, got[2].b)
	}
	if got[3].t != strings.Repeat("x", 70_000)+"\x00tail" || got[3].small.Valid {
		t.Fatalf("row 4 differs: %d bytes", len(got[3].t))
	}
	if got[4].t != "last" || !bytes.Equal(got[4].b, []byte{0xff}) {
		t.Fatalf("row 5 differs: %q %v", got[4].t, got[4].b)
	}
}

// TestHugeValues moves a blob above 16 MiB. The client reads a cell in chunks,
// so a result is bounded by SQLite and not by one message; a request is still
// bounded by one message.
func TestHugeValues(t *testing.T) {
	_, db := startGen(t)
	mustExec(t, db, "CREATE TABLE huge (id INTEGER, b BLOB)")
	payload := make([]byte, 12<<20)
	for i := range payload {
		payload[i] = byte(i*7 + 3)
	}
	mustExec(t, db, "INSERT INTO huge VALUES (1, ?)", payload)
	var back []byte
	if err := db.QueryRow("SELECT b FROM huge WHERE id = 1").Scan(&back); err != nil || !bytes.Equal(back, payload) {
		t.Fatalf("12 MiB blob: %d bytes, %v", len(back), err)
	}
	var n int
	if err := db.QueryRow("SELECT length(zeroblob(17 * 1024 * 1024))").Scan(&n); err != nil || n != 17<<20 {
		t.Fatalf("length of a 17 MiB blob: %d, %v", n, err)
	}
	var big []byte
	if err := db.QueryRow("SELECT zeroblob(17 * 1024 * 1024)").Scan(&big); err != nil || len(big) != 17<<20 {
		t.Fatalf("17 MiB blob result: %d bytes, %v", len(big), err)
	}
	// A request above the limit is refused by the driver and the connection stays usable.
	_, err := db.Exec("INSERT INTO huge VALUES (2, ?)", make([]byte, 17<<20))
	if err == nil {
		t.Fatal("a 17 MiB argument was accepted")
	}
	if errors.Is(err, driver.ErrBadConn) {
		t.Fatalf("a refused request is not a lost connection: %v", err)
	}
	if got := queryInt(t, db, "SELECT count(*) FROM huge"); got != 1 {
		t.Fatalf("rows after the refused insert: %d", got)
	}
}

// TestReadOnlyTransactionRestoresQueryOnly checks that a read-only transaction
// puts the session's query_only back, whatever ends it.
func TestReadOnlyTransactionRestoresQueryOnly(t *testing.T) {
	_, db := startGen(t)
	db.SetMaxOpenConns(1) // one session: the pragma is per session
	mustExec(t, db, "CREATE TABLE q (v INTEGER)")
	ctx := context.Background()

	for _, initial := range []int64{0, 1} {
		mustExec(t, db, fmt.Sprintf("PRAGMA query_only = %d", initial))
		for _, end := range []string{"commit", "rollback", "sql-commit", "pool-reset"} {
			if end == "pool-reset" {
				// The connection goes back to the pool with the read-only transaction
				// still open (begun below database/sql's Tx); the next checkout resets it.
				c, err := db.Conn(ctx)
				if err != nil {
					t.Fatal(err)
				}
				err = c.Raw(func(dc any) error {
					_, err := dc.(driver.ConnBeginTx).BeginTx(ctx, driver.TxOptions{ReadOnly: true})
					return err
				})
				if err != nil {
					t.Fatalf("initial %d: raw begin: %v", initial, err)
				}
				var inside int64
				if err := c.QueryRowContext(ctx, "PRAGMA query_only").Scan(&inside); err != nil || inside != 1 {
					t.Fatalf("initial %d: query_only inside = %d, %v", initial, inside, err)
				}
				if err := c.Close(); err != nil {
					t.Fatal(err)
				}
			} else {
				tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
				if err != nil {
					t.Fatalf("initial %d, end %s: begin: %v", initial, end, err)
				}
				if n := queryOnlyOf(t, tx); n != 1 {
					t.Fatalf("query_only inside the read-only transaction = %d", n)
				}
				switch end {
				case "commit":
					err = tx.Commit()
				case "rollback":
					err = tx.Rollback()
				case "sql-commit":
					// The transaction ends with SQL text; Rollback is then a no-op and
					// must still restore the setting.
					if _, err = tx.Exec("COMMIT"); err == nil {
						err = tx.Rollback()
					}
				}
				if err != nil {
					t.Fatalf("initial %d, end %s: %v", initial, end, err)
				}
			}
			if got := queryInt(t, db, "PRAGMA query_only"); got != initial {
				t.Fatalf("initial %d, end %s: query_only after = %d", initial, end, got)
			}
		}
	}
}

func queryOnlyOf(t *testing.T, tx *sql.Tx) int64 {
	t.Helper()
	var v int64
	if err := tx.QueryRow("PRAGMA query_only").Scan(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

// TestCancelAbandonsCall checks the behavior of a context that ends while a
// statement runs: the caller gets its context error at the deadline, the server
// interrupts the running statement, the connection is discarded, the pool
// recovers, and a canceled Exec does not make database/sql believe it can retry.
func TestCancelAbandonsCall(t *testing.T) {
	srv, db := startGen(t)
	const long = "WITH RECURSIVE c(x) AS (SELECT 1 UNION ALL SELECT x + 1 FROM c WHERE x < 8000000) SELECT count(*) FROM c"
	mustExec(t, db, "CREATE TABLE cx (v INTEGER)")

	for name, run := range map[string]func(ctx context.Context) error{
		"query": func(ctx context.Context) error {
			var n int64
			return db.QueryRowContext(ctx, long).Scan(&n)
		},
		"exec": func(ctx context.Context) error {
			_, err := db.ExecContext(ctx, "INSERT INTO cx SELECT count(*) FROM ("+long[:len(long)-len("SELECT count(*) FROM c")]+"SELECT x FROM c)")
			return err
		},
	} {
		ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
		start := time.Now()
		err := run(ctx)
		cancel()
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("%s: %v, want the deadline", name, err)
		}
		if d := time.Since(start); d > 3*time.Second {
			t.Fatalf("%s: returned after %v", name, d)
		}
		if errors.Is(err, driver.ErrBadConn) {
			t.Fatalf("%s: a deadline must not look like a lost connection: %v", name, err)
		}
		if n := queryInt(t, db, "SELECT 1"); n != 1 {
			t.Fatalf("%s: SELECT 1 = %d", name, n)
		}
	}
	// The abandoned sessions end on the server once their statement is interrupted.
	if !srv.Log.WaitCount(sqlite3srv.LineSessionEnded, 2, 60*time.Second) {
		t.Fatalf("abandoned sessions did not end:\n%s", srv.Log.String())
	}
}

// TestServerKilled checks the mapping of a lost link: a statement that was in
// flight has an unknown outcome and is not retried, any other call is a bad
// connection that database/sql replaces.
func TestServerKilled(t *testing.T) {
	srv := sqlite3srv.Start(t, sqlite3srv.Config{})
	cfg := genConfig("main")
	conn, err := mustConnector(t, srv, cfg).Connect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	exec := conn.(driver.ExecerContext)
	if _, err := exec.ExecContext(context.Background(), "CREATE TABLE k (v INTEGER)", nil); err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() {
		_, err := exec.ExecContext(context.Background(),
			"INSERT INTO k WITH RECURSIVE c(x) AS (SELECT 1 UNION ALL SELECT x + 1 FROM c WHERE x < 50000000) SELECT count(*) FROM c", nil)
		done <- err
	}()
	if !srv.Log.WaitCount(sqlite3srv.LineSessionStarted, 1, 10*time.Second) {
		t.Fatal("no session")
	}
	time.Sleep(100 * time.Millisecond)
	srv.KillServer()
	select {
	case err := <-done:
		if !errors.Is(err, sqlite.ErrOutcomeUnknown) {
			t.Fatalf("Exec in flight when the server died: %v, want ErrOutcomeUnknown", err)
		}
		if errors.Is(err, driver.ErrBadConn) {
			t.Fatalf("an Exec of unknown outcome must not be retried by database/sql: %v", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("Exec did not return after the server died")
	}
	if v, ok := conn.(driver.Validator); !ok || v.IsValid() {
		t.Fatal("the conn is still valid after its session failed")
	}
	if _, err := conn.(driver.QueryerContext).QueryContext(context.Background(), "SELECT 1", nil); !errors.Is(err, driver.ErrBadConn) {
		t.Fatalf("Query on a dead conn: %v, want driver.ErrBadConn", err)
	}
	if err := conn.(driver.Pinger).Ping(context.Background()); !errors.Is(err, driver.ErrBadConn) {
		t.Fatalf("Ping on a dead conn: %v, want driver.ErrBadConn", err)
	}
	// Dialing a listener whose server died fails either fast with "listener is gone"
	// (a bad connection) or when the connect timeout ends (a deadline error, not a bad
	// connection: database/sql must not retry it at once).
	short := cfg
	short.Timeout = 2 * time.Second
	if _, err := mustConnector(t, srv, short).Connect(context.Background()); !errors.Is(err, driver.ErrBadConn) && !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, hqq.ErrTimeout) {
		t.Fatalf("Connect with the server gone: %v, want driver.ErrBadConn or a timeout", err)
	}
}

// TestServerKilledIdle checks that a query on an idle connection whose server
// died is a bad connection (so database/sql may retry it on a new one).
func TestServerKilledIdle(t *testing.T) {
	srv := sqlite3srv.Start(t, sqlite3srv.Config{})
	conn, err := mustConnector(t, srv, genConfig("main")).Connect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	st, err := conn.(driver.ConnPrepareContext).PrepareContext(context.Background(), "SELECT 1")
	if err != nil {
		t.Fatal(err)
	}
	srv.KillServer()
	start := time.Now()
	_, err = st.(driver.StmtQueryContext).QueryContext(context.Background(), nil)
	if !errors.Is(err, driver.ErrBadConn) {
		t.Fatalf("query after SIGKILL: %v, want driver.ErrBadConn", err)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("crash detection took %v", d)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("Stmt.Close on a dead conn: %v", err)
	}
}

func mustConnector(t *testing.T, srv *sqlite3srv.Server, cfg sqlite.Config) *sqlite.Connector {
	t.Helper()
	c, err := sqlite.NewConnector(srv.Listener(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// TestStatementReusedWhileRowsOpen executes a prepared statement again before its
// earlier rows are read to the end, inside one transaction. The earlier rows are
// released with an error.
func TestStatementReusedWhileRowsOpen(t *testing.T) {
	_, db := startGen(t)
	mustExec(t, db, "CREATE TABLE s (id INTEGER PRIMARY KEY)")
	mustExec(t, db, "WITH RECURSIVE c(x) AS (SELECT 1 UNION ALL SELECT x + 1 FROM c WHERE x < 1000) INSERT INTO s SELECT x FROM c")
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	st, err := tx.Prepare("SELECT id FROM s WHERE id > ? ORDER BY id")
	if err != nil {
		t.Fatal(err)
	}
	first, err := st.Query(0)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	for range 5 {
		if !first.Next() {
			t.Fatalf("first rows ended early: %v", first.Err())
		}
	}
	second, err := st.Query(990)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	n := 0
	for second.Next() {
		n++
	}
	if err := second.Err(); err != nil || n != 10 {
		t.Fatalf("second rows: %d, %v", n, err)
	}
	for first.Next() {
	}
	if first.Err() == nil {
		t.Fatal("the superseded rows ended without an error")
	}
}

// TestIsolationLevels checks that an unsupported level is refused before anything
// is sent and leaves the connection usable.
func TestIsolationLevels(t *testing.T) {
	_, db := startGen(t)
	db.SetMaxOpenConns(1)
	ctx := context.Background()
	for _, lvl := range []sql.IsolationLevel{sql.LevelReadUncommitted, sql.LevelReadCommitted, sql.LevelRepeatableRead, sql.LevelSnapshot, sql.LevelLinearizable} {
		if tx, err := db.BeginTx(ctx, &sql.TxOptions{Isolation: lvl}); err == nil {
			_ = tx.Rollback()
			t.Fatalf("isolation %v accepted", lvl)
		}
	}
	c, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := c.ExecContext(ctx, "BEGIN"); err != nil {
		t.Fatalf("BEGIN after the refused levels: %v", err)
	}
	if _, err := c.ExecContext(ctx, "COMMIT"); err != nil {
		t.Fatal(err)
	}
}

// TestConcurrentConnections uses many connections of one pool at once, each with
// its own session, and checks every answer.
func TestConcurrentConnections(t *testing.T) {
	_, db := startGen(t)
	mustExec(t, db, "PRAGMA journal_mode=WAL")
	mustExec(t, db, "CREATE TABLE cc (k INTEGER PRIMARY KEY, v INTEGER)")
	const workers, rounds = 24, 40
	var wg sync.WaitGroup
	errc := make(chan error, workers)
	for w := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range rounds {
				if _, err := db.Exec("INSERT OR REPLACE INTO cc (k, v) VALUES (?, ?)", w, i); err != nil {
					errc <- fmt.Errorf("worker %d: %w", w, err)
					return
				}
				var v int64
				if err := db.QueryRow("SELECT v FROM cc WHERE k = ?", w).Scan(&v); err != nil || v != int64(i) {
					errc <- fmt.Errorf("worker %d round %d: v = %d, %v", w, i, v, err)
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errc)
	for err := range errc {
		t.Error(err)
	}
	if n := queryInt(t, db, "SELECT count(*) FROM cc"); n != workers {
		t.Fatalf("rows = %d, want %d", n, workers)
	}
}

// TestSessionLimit checks that the server's session limit surfaces as an error of
// Connect and not as a hang.
func TestSessionLimit(t *testing.T) {
	srv := sqlite3srv.Start(t, sqlite3srv.Config{Args: []string{"--max-sessions", "1"}})
	c := mustConnector(t, srv, genConfig("main"))
	first, err := c.Connect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	second, err := c.Connect(ctx)
	if err == nil {
		_ = second.Close()
		t.Fatal("a second session beyond --max-sessions was accepted")
	}
	if !errors.Is(err, rt.ErrLimit) || protoCode(err) != wire.ProtoLimit {
		t.Fatalf("second Connect: %v, want a limit error", err)
	}
	if err := first.(driver.Pinger).Ping(context.Background()); err != nil {
		t.Fatalf("the first session after the refusal: %v", err)
	}
}

// TestConnectorClosed checks Connect after Close.
func TestConnectorClosed(t *testing.T) {
	srv := sqlite3srv.Start(t, sqlite3srv.Config{})
	c := mustConnector(t, srv, genConfig("main"))
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Connect(context.Background()); err == nil {
		t.Fatal("Connect after Close succeeded")
	}
}

// TestStatementClearsOldBindings runs a prepared statement with fewer arguments than
// before, below database/sql (which checks the count): parameters nobody binds are
// NULL again, not the values of the earlier run.
func TestStatementClearsOldBindings(t *testing.T) {
	srv := sqlite3srv.Start(t, sqlite3srv.Config{})
	cn, err := mustConnector(t, srv, genConfig("main")).Connect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer cn.Close()
	ctx := context.Background()
	st, err := cn.(driver.ConnPrepareContext).PrepareContext(ctx, "SELECT ? IS NULL, ? IS NULL")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	isNull := func(args ...driver.Value) []driver.Value {
		t.Helper()
		nv := make([]driver.NamedValue, len(args))
		for i, a := range args {
			nv[i] = driver.NamedValue{Ordinal: i + 1, Value: a}
		}
		rows, err := st.(driver.StmtQueryContext).QueryContext(ctx, nv)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		dest := make([]driver.Value, 2)
		if err := rows.Next(dest); err != nil {
			t.Fatal(err)
		}
		return dest
	}
	if got := isNull(int64(1), int64(2)); got[0] != int64(0) || got[1] != int64(0) {
		t.Fatalf("bound: %v", got)
	}
	if got := isNull(int64(1)); got[0] != int64(0) || got[1] != int64(1) {
		t.Fatalf("one argument: %v, want the second parameter NULL", got)
	}
	if got := isNull(); got[0] != int64(1) || got[1] != int64(1) {
		t.Fatalf("no arguments: %v, want both parameters NULL", got)
	}
}
