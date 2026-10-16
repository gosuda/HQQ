package hqqsqlite_test

import (
	"bytes"
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"math/rand/v2"
	"net/url"
	"os"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gosuda.org/hqq"
	hqqsqlite "gosuda.org/hqq/examples/sqlite/driver"
	"gosuda.org/hqq/examples/sqlite/fakeserver"
	"gosuda.org/hqq/examples/sqlite/wire"
)

// fastLink makes heartbeats quick, so crash detection takes a few hundred
// milliseconds instead of seconds.
var fastLink = hqq.LinkOptions{
	HeartbeatInterval: 20 * time.Millisecond,
	PeerTimeout:       300 * time.Millisecond,
}

var listenerSeq atomic.Uint64

// listenerName returns a short unique listener name (the macOS shared memory
// name limit is 31 bytes).
func listenerName() string {
	return fmt.Sprintf("hqs%d-%d", os.Getpid()%100000, listenerSeq.Add(1))
}

// startServer starts a fake server and closes it when the test ends.
func startServer(t *testing.T, opts fakeserver.Options) (string, *fakeserver.Server) {
	t.Helper()
	name := listenerName()
	if opts.Link == (hqq.LinkOptions{}) {
		opts.Link = fastLink
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	srv, err := fakeserver.Listen(ctx, name, opts)
	if err != nil {
		t.Fatalf("listen %s: %v", name, err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	return name, srv
}

// testConfig returns the connector configuration of the tests.
func testConfig(db string) hqqsqlite.Config {
	return hqqsqlite.Config{
		Database: db,
		Timeout:  5 * time.Second,
		Link:     hqq.Config{Advanced: hqq.AdvancedOptions{Link: fastLink}},
	}
}

// openDB opens a sql.DB on a new connector for listener name.
func openDB(t *testing.T, name string, cfg hqqsqlite.Config) (*sql.DB, *hqqsqlite.Connector) {
	t.Helper()
	c, err := hqqsqlite.NewConnector(name, cfg)
	if err != nil {
		t.Fatal(err)
	}
	db := sql.OpenDB(c)
	t.Cleanup(func() { _ = db.Close() })
	return db, c
}

// serve starts a server and returns a sql.DB on the default test database.
func serve(t *testing.T) (*sql.DB, *fakeserver.Server) {
	t.Helper()
	name, srv := startServer(t, fakeserver.Options{})
	db, _ := openDB(t, name, testConfig("main"))
	return db, srv
}

func waitUntil(t *testing.T, d time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// execer is *sql.DB or *sql.Tx.
type execer interface {
	Exec(query string, args ...any) (sql.Result, error)
}

func mustExec(t *testing.T, db execer, query string, args ...any) sql.Result {
	t.Helper()
	res, err := db.Exec(query, args...)
	if err != nil {
		t.Fatalf("exec %q: %v", query, err)
	}
	return res
}

func TestRoundTripValues(t *testing.T) {
	db, _ := serve(t)
	mustExec(t, db, "CREATE TABLE v (i INTEGER, f REAL, s TEXT, b BLOB, n TEXT)")
	res := mustExec(t, db, "INSERT INTO v (i, f, s, b, n) VALUES (?, ?, ?, ?, ?)",
		int64(-7), 2.5, "héllo", []byte{0, 1, 0xff}, nil)
	if id, _ := res.LastInsertId(); id != 1 {
		t.Fatalf("LastInsertId = %d", id)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		t.Fatalf("RowsAffected = %d", n)
	}
	var (
		i  int64
		f  float64
		s  string
		b  []byte
		nn sql.NullString
	)
	if err := db.QueryRow("SELECT i, f, s, b, n FROM v").Scan(&i, &f, &s, &b, &nn); err != nil {
		t.Fatal(err)
	}
	if i != -7 || f != 2.5 || s != "héllo" || !bytes.Equal(b, []byte{0, 1, 0xff}) || nn.Valid {
		t.Fatalf("row = %d %v %q %x %+v", i, f, s, b, nn)
	}

	when := time.Date(2026, 10, 17, 1, 2, 3, 4, time.UTC)
	mustExec(t, db, "INSERT INTO v (i, s, b) VALUES (?, ?, ?)", true, when, []byte{})
	var got string
	var empty []byte
	if err := db.QueryRow("SELECT s, b FROM v WHERE i = 1").Scan(&got, &empty); err != nil {
		t.Fatal(err)
	}
	if got != when.Format(time.RFC3339Nano) || empty == nil || len(empty) != 0 {
		t.Fatalf("time/bool round trip: %q, empty blob %#v", got, empty)
	}
}

func TestNamedParametersAndPreparedStatements(t *testing.T) {
	db, _ := serve(t)
	mustExec(t, db, "CREATE TABLE n (a INTEGER, b TEXT)")
	mustExec(t, db, "INSERT INTO n (a, b) VALUES (:a, :b)", sql.Named("a", 1), sql.Named("b", "x"))
	var b string
	if err := db.QueryRow("SELECT b FROM n WHERE a = :a", sql.Named("a", 1)).Scan(&b); err != nil || b != "x" {
		t.Fatalf("named query: %q %v", b, err)
	}

	ins, err := db.Prepare("INSERT INTO n (a) VALUES (?)")
	if err != nil {
		t.Fatal(err)
	}
	defer ins.Close()
	for i := range 100 {
		if _, err := ins.Exec(i); err != nil {
			t.Fatal(err)
		}
	}
	q, err := db.Prepare("SELECT a FROM n WHERE a >= ?")
	if err != nil {
		t.Fatal(err)
	}
	defer q.Close()
	rows, err := q.Query(50)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	count := 0
	for rows.Next() {
		count++
	}
	// Values 0..99 were inserted; 50..99 satisfy the filter.
	if err := rows.Err(); err != nil || count != 50 {
		t.Fatalf("prepared query returned %d rows: %v", count, err)
	}
}

func TestMultipleBatchesAndSmallFetch(t *testing.T) {
	name, _ := startServer(t, fakeserver.Options{})
	cfg := testConfig("main")
	cfg.FetchSize = 7 // 1000 rows in 143 batches
	db, _ := openDB(t, name, cfg)

	rows, err := db.Query("SELECT value FROM hqq_series(1, 1000)")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	sum, n := 0, 0
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			t.Fatal(err)
		}
		n++
		sum += v
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if n != 1000 || sum != 500500 {
		t.Fatalf("rows = %d, sum = %d", n, sum)
	}
}

func TestLargeBlobsAreChunked(t *testing.T) {
	db, _ := serve(t)
	mustExec(t, db, "CREATE TABLE blobs (id INTEGER PRIMARY KEY, data BLOB)")
	r := rand.New(rand.NewPCG(1, 2))
	big := make([]byte, 3<<20) // several multi-chunk messages each way
	for i := range big {
		big[i] = byte(r.UintN(256))
	}
	mustExec(t, db, "INSERT INTO blobs (id, data) VALUES (1, ?)", big)
	var got []byte
	if err := db.QueryRow("SELECT data FROM blobs WHERE id = 1").Scan(&got); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, big) {
		t.Fatalf("blob of %d bytes did not round trip (got %d)", len(big), len(got))
	}
}

func TestNullsAndTypes(t *testing.T) {
	db, _ := serve(t)
	mustExec(t, db, "CREATE TABLE z (a TEXT, b INTEGER)")
	mustExec(t, db, "INSERT INTO z VALUES (NULL, NULL)")
	rows, err := db.Query("SELECT a, b FROM z")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	cols, _ := rows.ColumnTypes()
	if cols[0].DatabaseTypeName() != "TEXT" || cols[1].DatabaseTypeName() != "INTEGER" {
		t.Fatalf("declared types = %q, %q", cols[0].DatabaseTypeName(), cols[1].DatabaseTypeName())
	}
	if !rows.Next() {
		t.Fatal("no row")
	}
	var a sql.NullString
	var b sql.NullInt64
	if err := rows.Scan(&a, &b); err != nil || a.Valid || b.Valid {
		t.Fatalf("NULLs scanned as %+v %+v (%v)", a, b, err)
	}
}

func TestTransactionsCommitAndRollback(t *testing.T) {
	db, _ := serve(t)
	mustExec(t, db, "CREATE TABLE tx (id INTEGER)")
	rowsOf := func() int {
		rows, err := db.Query("SELECT id FROM tx")
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		n := 0
		for rows.Next() {
			n++
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		return n
	}

	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, tx, "INSERT INTO tx VALUES (1)")
	if got := rowsOf(); got != 0 {
		t.Fatalf("uncommitted row visible to another connection: %d", got)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if got := rowsOf(); got != 0 {
		t.Fatalf("rolled back row visible: %d", got)
	}

	tx, err = db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, tx, "INSERT INTO tx VALUES (2)")
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if got := rowsOf(); got != 1 {
		t.Fatalf("committed rows = %d", got)
	}
	if err := tx.Commit(); err == nil {
		t.Fatal("second Commit succeeded")
	}
}

func TestTxModeFromContextAndBusyError(t *testing.T) {
	name, _ := startServer(t, fakeserver.Options{})
	cfg := testConfig("main")
	cfg.BusyTimeout = 50 * time.Millisecond
	db, _ := openDB(t, name, cfg)
	mustExec(t, db, "CREATE TABLE b (id INTEGER)")

	// An IMMEDIATE transaction takes the write lock at BEGIN.
	ctx := hqqsqlite.WithTxMode(context.Background(), hqqsqlite.TxImmediate)
	holder, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Rollback()

	_, err = db.Exec("INSERT INTO b VALUES (1)")
	if !errors.Is(err, hqqsqlite.ErrBusy) {
		t.Fatalf("write while locked: %v, want ErrBusy", err)
	}
	var se *hqqsqlite.Error
	if !errors.As(err, &se) || se.Primary() != hqqsqlite.CodeBusy {
		t.Fatalf("error %#v is not a BUSY sqlite error", err)
	}
	if err := holder.Rollback(); err != nil {
		t.Fatal(err)
	}
	mustExec(t, db, "INSERT INTO b VALUES (1)")
}

func TestConstraintErrorCodes(t *testing.T) {
	db, _ := serve(t)
	mustExec(t, db, "CREATE TABLE c (id INTEGER PRIMARY KEY, email TEXT UNIQUE, req TEXT NOT NULL)")
	mustExec(t, db, "INSERT INTO c VALUES (1, 'a@x', 'r')")
	tests := []struct {
		name  string
		query string
		code  int
	}{
		{"primary key", "INSERT INTO c VALUES (1, 'b@x', 'r')", hqqsqlite.CodeConstraintPrimaryKey},
		{"unique", "INSERT INTO c VALUES (2, 'a@x', 'r')", hqqsqlite.CodeConstraintUnique},
		{"not null", "INSERT INTO c VALUES (3, 'c@x', NULL)", hqqsqlite.CodeConstraintNotNull},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := db.Exec(tc.query)
			if !errors.Is(err, hqqsqlite.ErrConstraint) {
				t.Fatalf("err = %v, want ErrConstraint", err)
			}
			var se *hqqsqlite.Error
			if !errors.As(err, &se) || se.Code != tc.code {
				t.Fatalf("code = %+v, want %d", err, tc.code)
			}
		})
	}
}

func TestSQLErrorKeepsConnectionUsable(t *testing.T) {
	db, _ := serve(t)
	if _, err := db.Exec("SELEC nonsense"); err == nil {
		t.Fatal("syntax error accepted")
	}
	conn, err := db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(context.Background(), "SELECT * FROM missing"); err == nil {
		t.Fatal("missing table accepted")
	}
	if err := conn.Raw(func(dc any) error {
		if !dc.(driver.Validator).IsValid() {
			return errors.New("connection invalid after SQL error")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	var one int
	if err := conn.QueryRowContext(context.Background(), "SELECT value FROM hqq_series(1, 1)").Scan(&one); err != nil || one != 1 {
		t.Fatalf("query after error: %d %v", one, err)
	}
}

func TestCancelMidQuery(t *testing.T) {
	name, srv := startServer(t, fakeserver.Options{})
	c, err := hqqsqlite.NewConnector(name, testConfig("main"))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	cn, err := c.Connect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer cn.Close()
	q := cn.(driver.QueryerContext)

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err = q.QueryContext(ctx, "SELECT hqq_sleep(10000)", nil)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("canceled query err = %v", err)
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("cancel took %v", d)
	}
	waitUntil(t, 3*time.Second, "server to observe the cancel", func() bool {
		return srv.Stats().Interrupted >= 1
	})
	if cn.(driver.Validator).IsValid() {
		t.Fatal("connection still valid after a canceled call")
	}
	if _, err := q.QueryContext(context.Background(), "SELECT 1", nil); !errors.Is(err, driver.ErrBadConn) {
		t.Fatalf("use after cancel: %v, want ErrBadConn", err)
	}
}

func TestCancelThroughSQLPool(t *testing.T) {
	db, srv := serve(t)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := db.QueryContext(ctx, "SELECT hqq_sleep(10000)"); err == nil {
		t.Fatal("canceled query succeeded")
	}
	waitUntil(t, 3*time.Second, "interrupt", func() bool { return srv.Stats().Interrupted >= 1 })
	var one int
	if err := db.QueryRow("SELECT value FROM hqq_series(1, 1)").Scan(&one); err != nil || one != 1 {
		t.Fatalf("pool after cancel: %v", err)
	}
}

func TestFiftyConcurrentConnections(t *testing.T) {
	db, srv := serve(t)
	mustExec(t, db, "CREATE TABLE conc (id INTEGER PRIMARY KEY, v INTEGER)")
	const conns = 50
	var wg sync.WaitGroup
	errs := make(chan error, conns)
	for i := range conns {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := db.Exec("INSERT INTO conc (id, v) VALUES (?, ?)", i, i*i); err != nil {
				errs <- err
				return
			}
			var v int
			if err := db.QueryRow("SELECT v FROM conc WHERE id = ?", i).Scan(&v); err != nil {
				errs <- err
				return
			}
			if v != i*i {
				errs <- fmt.Errorf("id %d: v = %d", i, v)
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	rows, err := db.Query("SELECT id FROM conc")
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for rows.Next() {
		n++
	}
	rows.Close()
	if n != conns {
		t.Fatalf("rows = %d, want %d", n, conns)
	}
	if st := srv.Stats(); st.Sessions < 2 {
		t.Fatalf("expected several sessions, got %d", st.Sessions)
	}
}

// TestLargeResultMemoryIsBounded streams a 16 MiB result. Live heap after a GC
// must stay near the fetch size, not near the result size.
func TestLargeResultMemoryIsBounded(t *testing.T) {
	name, _ := startServer(t, fakeserver.Options{})
	cfg := testConfig("main")
	cfg.FetchSize = 64
	db, _ := openDB(t, name, cfg)

	const rows = 1024
	text := string(bytes.Repeat([]byte("x"), 16<<10))
	stmt, err := db.Prepare("SELECT ? FROM hqq_series(1, ?)")
	if err != nil {
		t.Fatal(err)
	}
	defer stmt.Close()
	res, err := stmt.Query(text, rows)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Close()

	live := func() uint64 {
		runtime.GC()
		var ms runtime.MemStats
		runtime.ReadMemStats(&ms)
		return ms.HeapAlloc
	}
	base := live()
	var peak uint64
	n, total := 0, 0
	for res.Next() {
		var s string
		if err := res.Scan(&s); err != nil {
			t.Fatal(err)
		}
		n++
		total += len(s)
		if n%128 == 0 {
			peak = max(peak, live())
		}
	}
	if err := res.Err(); err != nil {
		t.Fatal(err)
	}
	if n != rows || total != rows*len(text) {
		t.Fatalf("rows = %d, bytes = %d", n, total)
	}
	const budget = 12 << 20
	if peak > base+budget {
		t.Fatalf("live heap grew to %d (base %d): result is not streamed", peak, base)
	}
}

func TestEarlyRowsCloseReleasesCursor(t *testing.T) {
	name, _ := startServer(t, fakeserver.Options{})
	cfg := testConfig("main")
	cfg.FetchSize = 16
	db, _ := openDB(t, name, cfg)
	rows, err := db.Query("SELECT value FROM hqq_series(1, 100000)")
	if err != nil {
		t.Fatal(err)
	}
	if !rows.Next() {
		t.Fatal("no first row")
	}
	if err := rows.Close(); err != nil {
		t.Fatal(err)
	}
	var one int
	if err := db.QueryRow("SELECT value FROM hqq_series(7, 7)").Scan(&one); err != nil || one != 7 {
		t.Fatalf("query after early close: %d %v", one, err)
	}
}

func TestRollbackWhenConnectionCloses(t *testing.T) {
	name, srv := startServer(t, fakeserver.Options{})
	c, err := hqqsqlite.NewConnector(name, testConfig("main"))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	ctx := context.Background()
	a, err := c.Connect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	b, err := c.Connect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()

	exec := func(cn driver.Conn, q string) {
		t.Helper()
		if _, err := cn.(driver.ExecerContext).ExecContext(ctx, q, nil); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	exec(a, "CREATE TABLE r (id INTEGER)")
	exec(a, "BEGIN")
	exec(a, "INSERT INTO r VALUES (1)")
	if err := a.Close(); err != nil { // the session ends with a transaction open
		t.Fatal(err)
	}
	waitUntil(t, 3*time.Second, "rollback on close", func() bool {
		return srv.Stats().RolledBack >= 1
	})
	rows, err := b.(driver.QueryerContext).QueryContext(ctx, "SELECT id FROM r", nil)
	if err != nil {
		t.Fatal(err)
	}
	dest := make([]driver.Value, 1)
	if err := rows.Next(dest); err == nil {
		t.Fatalf("rolled back row is visible: %v", dest)
	}
	rows.Close()
}

func TestPragmasAndReadOnlyMode(t *testing.T) {
	name, _ := startServer(t, fakeserver.Options{})
	rw, _ := openDB(t, name, testConfig("data"))
	mustExec(t, rw, "CREATE TABLE ro (id INTEGER)")

	roCfg := testConfig("data")
	roCfg.Mode = hqqsqlite.ModeReadOnly
	ro, _ := openDB(t, name, roCfg)
	_, err := ro.Exec("INSERT INTO ro VALUES (1)")
	if !errors.Is(err, hqqsqlite.ErrReadOnly) {
		t.Fatalf("write in read-only mode: %v", err)
	}

	qCfg := testConfig("data")
	qCfg.Pragmas = []hqqsqlite.Pragma{{Name: "query_only", Value: "1"}}
	q, _ := openDB(t, name, qCfg)
	if _, err := q.Exec("INSERT INTO ro VALUES (2)"); !errors.Is(err, hqqsqlite.ErrReadOnly) {
		t.Fatalf("write with query_only: %v", err)
	}
}

func TestDatabaseAliasPolicy(t *testing.T) {
	name, _ := startServer(t, fakeserver.Options{
		Aliases:      map[string]string{"main": "maindb"},
		RequireAlias: true,
	})
	db, _ := openDB(t, name, testConfig("main"))
	mustExec(t, db, "CREATE TABLE a (id INTEGER)")

	other, _ := openDB(t, name, testConfig("elsewhere"))
	_, err := other.Exec("CREATE TABLE a (id INTEGER)")
	var pe *wire.ProtocolError
	if !errors.As(err, &pe) || pe.Code != wire.ProtoForbidden {
		t.Fatalf("unknown database: %v, want FORBIDDEN", err)
	}
}

func TestConnectTimeoutWithoutServer(t *testing.T) {
	cfg := testConfig("main")
	cfg.Timeout = 300 * time.Millisecond
	c, err := hqqsqlite.NewConnector(listenerName(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	start := time.Now()
	_, err = c.Connect(context.Background())
	if err == nil {
		t.Fatal("connect to a missing service succeeded")
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("connect took %v", d)
	}
}

func TestClosedConnectorFailsConnections(t *testing.T) {
	name, _ := startServer(t, fakeserver.Options{})
	c, err := hqqsqlite.NewConnector(name, testConfig("main"))
	if err != nil {
		t.Fatal(err)
	}
	cn, err := c.Connect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := cn.(driver.QueryerContext).QueryContext(context.Background(), "SELECT 1", nil); !errors.Is(err, driver.ErrBadConn) {
		t.Fatalf("query on closed connector: %v", err)
	}
	if _, err := c.Connect(context.Background()); err == nil {
		t.Fatal("Connect after Close succeeded")
	}
}

func TestDriverRegisteredAndDSN(t *testing.T) {
	name, _ := startServer(t, fakeserver.Options{})
	dsn := fmt.Sprintf("hqq://%s?db=%s&timeout=5s&fetch=32&busy=1s&txlock=immediate&mode=rwc&pragma.query_only=0",
		name, url.QueryEscape("dsn"))
	db, err := sql.Open(hqqsqlite.DriverName, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.Ping(); err != nil {
		t.Fatal(err)
	}
	mustExec(t, db, "CREATE TABLE d (id INTEGER)")
}

func TestUnsupportedArgumentType(t *testing.T) {
	db, _ := serve(t)
	if _, err := db.Exec("SELECT ?", complex(1, 2)); err == nil {
		t.Fatal("complex argument accepted")
	}
}

func TestIsolationLevels(t *testing.T) {
	db, _ := serve(t)
	if _, err := db.BeginTx(context.Background(), &sql.TxOptions{Isolation: sql.LevelReadUncommitted}); err == nil {
		t.Fatal("read uncommitted accepted")
	}
	tx, err := db.BeginTx(context.Background(), &sql.TxOptions{Isolation: sql.LevelSerializable, ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
}

func TestParseDSN(t *testing.T) {
	good := map[string]hqqsqlite.Config{
		"hqq://svc?db=main": {Database: "main"},
		"hqq://svc?db=/p/q.db&mode=ro&timeout=2s&fetch=10&busy=-1s&txlock=exclusive&pragma.journal_mode=WAL": {
			Database: "/p/q.db", Mode: hqqsqlite.ModeReadOnly, Timeout: 2 * time.Second, FetchSize: 10,
			BusyTimeout: -time.Second, TxMode: hqqsqlite.TxExclusive,
			Pragmas: []hqqsqlite.Pragma{{Name: "journal_mode", Value: "WAL"}},
		},
	}
	for dsn, want := range good {
		listener, cfg, err := hqqsqlite.ParseDSN(dsn)
		if err != nil {
			t.Fatalf("%s: %v", dsn, err)
		}
		if listener != "svc" && listener != "" {
			t.Fatalf("%s: listener %q", dsn, listener)
		}
		if cfg.Database != want.Database || cfg.Mode != want.Mode || cfg.Timeout != want.Timeout ||
			cfg.FetchSize != want.FetchSize || cfg.BusyTimeout != want.BusyTimeout || cfg.TxMode != want.TxMode ||
			fmt.Sprint(cfg.Pragmas) != fmt.Sprint(want.Pragmas) {
			t.Fatalf("%s: cfg = %+v, want %+v", dsn, cfg, want)
		}
	}
	for _, dsn := range []string{
		"postgres://svc?db=x",
		"hqq://?db=x",
		"hqq://svc",
		"hqq://svc?db=x&bogus=1",
		"hqq://svc?db=x&fetch=many",
		"hqq://svc?db=x&mode=nope",
		"hqq://svc?db=x&timeout=soon",
		"hqq://svc?db=x&txlock=sometimes",
		"hqq://svc/path?db=x",
		"hqq://svc?db=a&db=b",
	} {
		if _, _, err := hqqsqlite.ParseDSN(dsn); err == nil {
			t.Errorf("%s: accepted", dsn)
		}
	}
}

func TestNewConnectorValidation(t *testing.T) {
	bad := []hqqsqlite.Config{
		{},
		{Database: "x", FetchSize: wire.MaxFetchRows + 1},
		{Database: "x", FetchSize: -1},
		{Database: "x", Mode: 9},
		{Database: "x", TxMode: 9},
		{Database: "x", Pragmas: []hqqsqlite.Pragma{{Name: "bad name", Value: "1"}}},
	}
	for i, cfg := range bad {
		if _, err := hqqsqlite.NewConnector("svc", cfg); err == nil {
			t.Errorf("config %d accepted", i)
		}
	}
	if _, err := hqqsqlite.NewConnector("", hqqsqlite.Config{Database: "x"}); err == nil {
		t.Error("empty listener accepted")
	}
}

// TestRawBeginRolledBackOnReuse checks that a transaction begun with plain SQL
// does not survive the connection's return to the pool: the next checkout
// rolls it back (ResetSession), so its rows are never visible.
func TestRawBeginRolledBackOnReuse(t *testing.T) {
	db, _ := serve(t)
	mustExec(t, db, "CREATE TABLE rb (id INTEGER)")
	ctx := context.Background()

	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(ctx, "BEGIN"); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(ctx, "INSERT INTO rb VALUES (1)"); err != nil {
		t.Fatal(err)
	}
	if err := conn.Close(); err != nil { // back to the pool, transaction still open
		t.Fatal(err)
	}

	rows, err := db.Query("SELECT id FROM rb")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	// The same session would see its own uncommitted row, so an invisible row
	// shows that the reset rolled the transaction back.
	if rows.Next() {
		t.Fatal("rows of an abandoned transaction are visible on reuse")
	}
}
