//go:build unix

package sqlite3_test

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"gosuda.org/hqq/bindgen/examples/sqlite3"
	"gosuda.org/hqq/bindgen/examples/sqlite3/sqlite3srv"
	"gosuda.org/hqq/bindgen/rt"
)

// SQLite result codes that the tests compare (sqlite3.h).
const (
	codeError      = 1
	codeBusy       = 5
	codeReadonly   = 8
	codeCantOpen   = 14
	codeAuth       = 23
	codeRange      = 25
	cCheck         = 275
	cNotNull       = 1299
	cPrimaryKey    = 1555
	cUnique        = 2067
	cForeignKey    = 787
	statusCantOpen = sqlite3.SVC_STATUS_SQLITE_BASE + codeCantOpen
)

// TestConstraintErrors checks the code, the extended code and the message of every kind of constraint
// failure, through exec_stmt and through sqlite3_step. The Code of the library error is the extended
// code, because the server enables extended result codes on every connection. It catches a connection
// that was opened without the setup, and a detail function that reads the wrong connection.
func TestConstraintErrors(t *testing.T) {
	_, c, db := session(t)
	run(t, c, db, "PRAGMA foreign_keys = ON")
	run(t, c, db, "CREATE TABLE p (id INTEGER PRIMARY KEY)")
	run(t, c, db, "CREATE TABLE e (id INTEGER PRIMARY KEY, u TEXT UNIQUE, n TEXT NOT NULL DEFAULT 'x', "+
		"k INTEGER CHECK (k < 10), f INTEGER REFERENCES p(id))")
	run(t, c, db, "INSERT INTO e (id, u, k) VALUES (1, 'a', 1)")

	for _, tc := range []struct {
		name string
		sql  string
		args []any
		code int64
		msg  string
	}{
		{"unique", "INSERT INTO e (id, u) VALUES (2, ?)", []any{"a"}, cUnique, "UNIQUE constraint failed: e.u"},
		{"primary key", "INSERT INTO e (id, u) VALUES (1, 'b')", nil, cPrimaryKey, "UNIQUE constraint failed: e.id"},
		{"not null", "INSERT INTO e (id, u, n) VALUES (3, 'c', ?)", []any{nil}, cNotNull, "NOT NULL constraint failed: e.n"},
		{"check", "INSERT INTO e (id, u, k) VALUES (4, 'd', ?)", []any{99}, cCheck, "CHECK constraint failed: k < 10"},
		{"foreign key", "INSERT INTO e (id, u, f) VALUES (5, 'f', ?)", []any{7}, cForeignKey, "FOREIGN KEY constraint failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := prep(t, c, db, tc.sql)
			k, w, d := pack(tc.args...)
			_, _, err := c.ExecStmt(st, k, w, d)
			le := libErr(t, err)
			if le.Code != tc.code || le.Detail != tc.msg {
				t.Fatalf("exec_stmt: code %d %q, want %d %q", le.Code, le.Detail, tc.code, tc.msg)
			}
			if ext, err := c.Sqlite3ExtendedErrcode(db); err != nil || int64(ext) != tc.code {
				t.Fatalf("extended_errcode = %d, %v; want %d", ext, err, tc.code)
			}
			// With extended result codes on, errcode is the extended code too; the primary code is the low byte.
			if code, err := c.Sqlite3Errcode(db); err != nil || int64(code) != tc.code || code&0xff != 19 {
				t.Fatalf("errcode = %d, %v; want %d (SQLITE_CONSTRAINT with a subcode)", code, err, tc.code)
			}
			if msg, err := c.Sqlite3Errmsg(db); err != nil || msg != tc.msg {
				t.Fatalf("errmsg = %q, %v; want %q", msg, err, tc.msg)
			}
			// The same failure through sqlite3_step.
			if err := c.BindValues(st, k, w, d); err != nil {
				t.Fatal(err)
			}
			rc, err := c.Sqlite3Step(st)
			le = libErr(t, err)
			if le.Code != tc.code || le.Detail != tc.msg || rc != 0 && rc != int32(tc.code) {
				t.Fatalf("step: rc %d, code %d %q, want %d %q", rc, le.Code, le.Detail, tc.code, tc.msg)
			}
			// finalize reports the error of the last step and frees the statement all the same.
			if le := libErr(t, c.Sqlite3Finalize(st)); le.Code != tc.code {
				t.Fatalf("finalize: code %d, want %d", le.Code, tc.code)
			}
			if err := c.Sqlite3Finalize(st); !errors.Is(err, rt.ErrBadHandle) {
				t.Fatalf("second finalize: %v, want a bad handle", err)
			}
		})
	}
	if n := one(t, c, db, "SELECT count(*) FROM e"); n != int64(1) {
		t.Fatalf("%v rows after the failures, want 1", n)
	}
}

// TestSyntaxAndMissingTable checks errors of prepare: the code, the message and the absence of a statement.
func TestSyntaxAndMissingTable(t *testing.T) {
	_, c, db := session(t)
	for _, tc := range []struct{ sql, want string }{
		{"SELEC 1", `near "SELEC": syntax error`},
		{"SELECT * FROM missing", "no such table: missing"},
		{"SELECT nope FROM sqlite_master", "no such column: nope"},
		{"CREATE TABLE (", `near "(": syntax error`},
	} {
		_, err := c.PrepareOne(db, tc.sql)
		le := libErr(t, err)
		if le.Code != codeError || le.Detail != tc.want {
			t.Fatalf("%q: code %d %q, want 1 %q", tc.sql, le.Code, le.Detail, tc.want)
		}
	}
}

// TestSingleStatementRule is the rule of sqlite/PROTOCOL.md 6.2 that prepare_one enforces on the server:
// white space, comments and ';' may follow the statement, nothing else may; no text at all is an empty
// statement. The failures leave no statement behind: the tests count handles with a server that has room
// for four, and close the connection at the end, which SQLite answers with SQLITE_BUSY if a statement
// was leaked.
func TestSingleStatementRule(t *testing.T) {
	srv := sqlite3srv.Start(t, sqlite3srv.Config{Args: []string{"--max-handles", "4"}})
	c := srv.Dial()
	db := open(t, c, "rule.db")

	for _, sql := range []string{
		"SELECT 1", "SELECT 1;", "SELECT 1 ;  ", "SELECT 1;\n\n", "SELECT 1 -- trailing comment",
		"SELECT 1; /* block */", "SELECT 1;;;", "  \t\n SELECT 1", "/* lead */ SELECT 1; -- c\n",
	} {
		st, err := c.PrepareOne(db, sql)
		if err != nil {
			t.Fatalf("%q: %v", sql, err)
		}
		finalize(t, c, st)
	}
	for _, sql := range []string{
		"SELECT 1; SELECT 2", "SELECT 1;SELECT 2;", "SELECT 1; SELECT", "SELECT 1; garbage here",
		"SELECT 1; -- c\nSELECT 2", "CREATE TABLE a (x); CREATE TABLE b (x)",
	} {
		for range 6 { // more attempts than the table has entries
			_, err := c.PrepareOne(db, sql)
			le := libErr(t, err)
			if le.Code != int64(sqlite3.SVC_SQL_MULTIPLE) || le.Detail != "multiple statements" {
				t.Fatalf("%q: code %d %q, want SVC_SQL_MULTIPLE", sql, le.Code, le.Detail)
			}
		}
	}
	for _, sql := range []string{"", "   ", ";", "-- only a comment", "/* c */", " ; ; "} {
		for range 6 {
			_, err := c.PrepareOne(db, sql)
			le := libErr(t, err)
			if le.Code != int64(sqlite3.SVC_SQL_EMPTY) || le.Detail != "empty statement" {
				t.Fatalf("%q: code %d %q, want SVC_SQL_EMPTY", sql, le.Code, le.Detail)
			}
		}
	}
	// A real error in the first statement is SQLite's.
	if le := libErr(t, func() error { _, err := c.PrepareOne(db, "SELEC 1; SELECT 2"); return err }()); le.Code != codeError {
		t.Fatalf("syntax error in the first statement: code %d", le.Code)
	}
	// The failure text does not leak into the next failure of another function.
	if _, err := c.PrepareOne(db, "SELECT * FROM nope"); libErr(t, err).Detail != "no such table: nope" {
		t.Fatalf("detail after the single-statement failures: %v", err)
	}
	if err := c.Sqlite3Close(db); err != nil {
		t.Fatalf("close after the failed prepares: %v (a statement was leaked)", err)
	}
}

// TestRawPrepareTail checks sqlite3_prepare_v2 as it is: the offset of the rest of the text, and the nil
// statement for text without a statement. A script is run by preparing from the tail.
func TestRawPrepareTail(t *testing.T) {
	_, c, db := session(t)
	script := "CREATE TABLE s (x); INSERT INTO s VALUES (1); INSERT INTO s VALUES (2); -- done\n"
	rest := script
	var n int
	for {
		st, tail, err := c.Sqlite3PrepareV2(db, rest)
		if err != nil {
			t.Fatal(err)
		}
		if st.IsNil() {
			break
		}
		if _, err := c.Sqlite3Step(st); err != nil {
			t.Fatal(err)
		}
		finalize(t, c, st)
		n++
		if int(tail) > len(rest) {
			t.Fatalf("tail %d beyond the text (%d)", tail, len(rest))
		}
		rest = rest[tail:]
	}
	if n != 3 {
		t.Fatalf("ran %d statements, want 3", n)
	}
	if got := one(t, c, db, "SELECT sum(x) FROM s"); got != int64(3) {
		t.Fatalf("sum = %v, want 3", got)
	}
	st, tail, err := c.Sqlite3PrepareV2(db, "  -- nothing\n")
	if err != nil || !st.IsNil() || tail != 13 {
		t.Fatalf("comment only: stmt nil %v, tail %d, err %v; want nil, 13", st.IsNil(), tail, err)
	}
}

// TestBindErrors checks the failures of the bind functions and of the bulk bind: the codes, the messages,
// and that nothing is bound or run when an argument is wrong.
func TestBindErrors(t *testing.T) {
	_, c, db := session(t)
	run(t, c, db, "CREATE TABLE b (x, y)")
	st := prep(t, c, db, "INSERT INTO b VALUES (?, ?)")

	for _, idx := range []int32{0, 3, -1, 1 << 30} {
		if le := libErr(t, c.Sqlite3BindInt64(st, idx, 1)); le.Code != codeRange || !strings.Contains(le.Detail, "out of range") {
			t.Fatalf("bind_int64 index %d: code %d %q, want SQLITE_RANGE", idx, le.Code, le.Detail)
		}
	}
	if le := libErr(t, c.BindText(st, 3, []byte("x"))); le.Code != codeRange {
		t.Fatalf("bind_text out of range: %d", le.Code)
	}
	if le := libErr(t, c.BindBlob(st, 0, []byte("x"))); le.Code != codeRange {
		t.Fatalf("bind_blob out of range: %d", le.Code)
	}

	// Arrays that disagree: the error is ours, with our code, and nothing was bound.
	bad := []struct {
		name  string
		k     []uint8
		w     []uint64
		d     []byte
		code  int64
		extra string
	}{
		{"fewer words", []uint8{1, 1}, []uint64{1}, nil, int64(sqlite3.SVC_BAD_ARGS), "differ in length"},
		{"more words", []uint8{1}, []uint64{1, 2}, nil, int64(sqlite3.SVC_BAD_ARGS), "differ in length"},
		{"unknown kind", []uint8{9}, []uint64{0}, nil, int64(sqlite3.SVC_BAD_ARGS), "unknown value kind"},
		{"kind zero", []uint8{0}, []uint64{0}, nil, int64(sqlite3.SVC_BAD_ARGS), "unknown value kind"},
		{"text longer than data", []uint8{3}, []uint64{5}, []byte("abc"), int64(sqlite3.SVC_BAD_ARGS), "exceed the data"},
		{"huge length", []uint8{3, 4}, []uint64{1 << 63, 1 << 63}, []byte("ab"), int64(sqlite3.SVC_BAD_ARGS), "exceed the data"},
		{"data longer than the values", []uint8{3}, []uint64{1}, []byte("abc"), int64(sqlite3.SVC_BAD_ARGS), "do not add up"},
		{"data without values", []uint8{1}, []uint64{1}, []byte("abc"), int64(sqlite3.SVC_BAD_ARGS), "do not add up"},
		{"more values than parameters", []uint8{1, 1, 1}, []uint64{1, 2, 3}, nil, codeRange, "out of range"},
	}
	for _, tc := range bad {
		t.Run(tc.name, func(t *testing.T) {
			le := libErr(t, c.BindValues(st, tc.k, tc.w, tc.d))
			if le.Code != tc.code || !strings.Contains(le.Detail, tc.extra) {
				t.Fatalf("bind_values: code %d %q, want %d containing %q", le.Code, le.Detail, tc.code, tc.extra)
			}
			_, _, err := c.ExecStmt(st, tc.k, tc.w, tc.d)
			le = libErr(t, err)
			if le.Code != tc.code || !strings.Contains(le.Detail, tc.extra) {
				t.Fatalf("exec_stmt: code %d %q, want %d containing %q", le.Code, le.Detail, tc.code, tc.extra)
			}
		})
	}
	finalize(t, c, st)
	if n := one(t, c, db, "SELECT count(*) FROM b"); n != int64(0) {
		t.Fatalf("%v rows: exec_stmt ran a statement with arguments that were refused", n)
	}
	// The messages of the failures above did not stay behind in the thread of the server.
	if _, err := c.PrepareOne(db, "SELECT * FROM nope"); libErr(t, err).Detail != "no such table: nope" {
		t.Fatalf("stale detail: %v", err)
	}
}

// TestBindValuesClearsBindings checks that bind_values and exec_stmt start from no bindings: a parameter
// that a call does not set is NULL, not the value of the call before.
func TestBindValuesClearsBindings(t *testing.T) {
	_, c, db := session(t)
	run(t, c, db, "CREATE TABLE c (id INTEGER PRIMARY KEY, x, y)")
	st := prep(t, c, db, "INSERT INTO c VALUES (?, ?, ?)")
	for _, args := range [][]any{{1, "a", "b"}, {2, "c"}, {3}} {
		k, w, d := pack(args...)
		if _, _, err := c.ExecStmt(st, k, w, d); err != nil {
			t.Fatal(err)
		}
	}
	finalize(t, c, st)
	rows := queryAll(t, c, db, 0, "SELECT id, x, y FROM c ORDER BY id")
	want := [][]cell{{int64(1), "a", "b"}, {int64(2), "c", nil}, {int64(3), nil, nil}}
	for i := range want {
		for j := range want[i] {
			requireCell(t, rows[i][j], want[i][j])
		}
	}
}

// TestExecStmtResults checks changes and last_insert_rowid of exec_stmt, which are those of the statement
// that just ran, and that rows of a SELECT are discarded.
func TestExecStmtResults(t *testing.T) {
	_, c, db := session(t)
	run(t, c, db, "CREATE TABLE r (id INTEGER PRIMARY KEY, v)")
	if ch, id := run(t, c, db, "INSERT INTO r (v) VALUES (?)", "a"); ch != 1 || id != 1 {
		t.Fatalf("insert: changes %d, rowid %d", ch, id)
	}
	if ch, id := run(t, c, db, "INSERT INTO r (v) VALUES ('b'), ('c'), ('d')"); ch != 3 || id != 4 {
		t.Fatalf("multi insert: changes %d, rowid %d", ch, id)
	}
	if ch, _ := run(t, c, db, "UPDATE r SET v = 'x' WHERE id > ?", 1); ch != 3 {
		t.Fatalf("update: changes %d", ch)
	}
	if ch, _ := run(t, c, db, "DELETE FROM r"); ch != 4 {
		t.Fatalf("delete: changes %d", ch)
	}
	if ch, _ := run(t, c, db, "SELECT * FROM r"); ch != 0 && ch != 4 {
		// sqlite3_changes64 is the count of the last INSERT, UPDATE or DELETE: the hand-written server
		// reports the same, and a SELECT does not reset it.
		t.Fatalf("select: changes %d", ch)
	}
	if n, err := c.Sqlite3Changes64(db); err != nil || n != 4 {
		t.Fatalf("changes64 = %d, %v", n, err)
	}
	if id, err := c.Sqlite3LastInsertRowid(db); err != nil || id != 4 {
		t.Fatalf("last_insert_rowid = %d, %v", id, err)
	}
	// Rows are discarded: a SELECT of 5,000 rows is run to the end by one call.
	run(t, c, db, `WITH RECURSIVE s(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM s WHERE i < 5000) INSERT INTO r (v) SELECT i FROM s`)
	if _, _, err := execAll(t, c, db, "SELECT id, v FROM r"); err != nil {
		t.Fatal(err)
	}
}

func execAll(t *testing.T, c *sqlite3.Client, db sqlite3.Db, sql string) (int64, int64, error) {
	t.Helper()
	st := prep(t, c, db, sql)
	defer finalize(t, c, st)
	return c.ExecStmt(st, nil, nil, nil)
}

// TestBusyWithTwoSessions has one session hold the write lock while another writes. With a busy timeout
// of zero the second fails at once with SQLITE_BUSY. With a timeout it waits, and the first session
// commits in the meantime and the second write succeeds: two sessions run SQLite at the same time (the service
// is @serialize(none)). Under serialize(session) the waiting call would hold the lock that the commit
// needs, and the wait could only end in a timeout.
func TestBusyWithTwoSessions(t *testing.T) {
	srv := sqlite3srv.Start(t, sqlite3srv.Config{})
	a := srv.Dial()
	b := srv.Dial()
	dba := open(t, a, "busy.db")
	fast(t, a, dba)
	run(t, a, dba, "CREATE TABLE k (v INTEGER)")
	dbb := open(t, b, "busy.db")

	run(t, a, dba, "BEGIN IMMEDIATE")
	run(t, a, dba, "INSERT INTO k VALUES (1)")

	// No busy timeout: BUSY at once.
	if err := b.Sqlite3BusyTimeout(dbb, 0); err != nil {
		t.Fatal(err)
	}
	st := prep(t, b, dbb, "INSERT INTO k VALUES (2)")
	start := time.Now()
	_, _, err := b.ExecStmt(st, nil, nil, nil)
	le := libErr(t, err)
	if le.Code != codeBusy || le.Detail != "database is locked" {
		t.Fatalf("write under a held lock: code %d %q, want SQLITE_BUSY", le.Code, le.Detail)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("the busy answer took %v", d)
	}

	// A busy timeout: b waits while a commits.
	if err := b.Sqlite3BusyTimeout(dbb, 20000); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	wg.Add(1)
	commitErr := make(chan error, 1)
	go func() {
		defer wg.Done()
		time.Sleep(300 * time.Millisecond)
		st, err := a.PrepareOne(dba, "COMMIT")
		if err == nil {
			_, _, err = a.ExecStmt(st, nil, nil, nil)
		}
		commitErr <- err
	}()
	start = time.Now()
	_, _, err = b.ExecStmt(st, nil, nil, nil)
	waited := time.Since(start)
	wg.Wait()
	if err := <-commitErr; err != nil {
		t.Fatalf("commit of the first session: %v", err)
	}
	if err != nil {
		t.Fatalf("write with a busy timeout: %v (waited %v)", err, waited)
	}
	if waited < 200*time.Millisecond || waited > 10*time.Second {
		t.Fatalf("the write waited %v, want about 300ms", waited)
	}
	finalize(t, b, st)
	if n := one(t, a, dba, "SELECT count(*) FROM k"); n != int64(2) {
		t.Fatalf("%v rows, want 2", n)
	}
}

// TestBadHandles checks the handle errors: a finalized statement, a handle of the other type, a handle of
// another session, and the nil handle.
func TestBadHandles(t *testing.T) {
	srv := sqlite3srv.Start(t, sqlite3srv.Config{})
	c1 := srv.Dial()
	c2 := srv.Dial()
	db1 := open(t, c1, "h1.db")
	db2 := open(t, c2, "h2.db")
	st := prep(t, c1, db1, "SELECT 1")
	finalize(t, c1, st)

	if _, err := c1.Sqlite3Step(st); !errors.Is(err, rt.ErrBadHandle) {
		t.Fatalf("step on a finalized statement: %v, want ErrBadHandle", err)
	}
	st = prep(t, c1, db1, "SELECT 1")
	if _, err := c1.Sqlite3Step(sqlite3.Stmt{Handle: db1.Handle}); !errors.Is(err, rt.ErrBadHandle) {
		t.Fatalf("a Db used as a Stmt: %v, want ErrBadHandle", err)
	}
	if _, err := c1.Sqlite3Changes64(sqlite3.Db{Handle: st.Handle}); !errors.Is(err, rt.ErrBadHandle) {
		t.Fatalf("a Stmt used as a Db: %v, want ErrBadHandle", err)
	}
	if _, err := c2.Sqlite3Step(st); !errors.Is(err, rt.ErrForeignHandle) {
		t.Fatalf("a statement of another session: %v, want ErrForeignHandle", err)
	}
	if _, err := c1.PrepareOne(db2, "SELECT 1"); !errors.Is(err, rt.ErrForeignHandle) {
		t.Fatalf("a connection of another session: %v, want ErrForeignHandle", err)
	}
	if _, err := c1.Sqlite3Step(sqlite3.Stmt{}); err == nil {
		t.Fatal("step on the nil handle succeeded")
	}
	// None of that disturbed the session.
	if rc, err := c1.Sqlite3Step(st); err != nil || rc != sqlite3.SQLITE_ROW {
		t.Fatalf("step: %d, %v", rc, err)
	}
	// A handle that was closed with its connection is stale, and its slot may be reused.
	if err := c1.Sqlite3Close(db1); err != nil {
		t.Fatal(err)
	}
	if _, err := c1.Sqlite3Step(st); !errors.Is(err, rt.ErrBadHandle) {
		t.Fatalf("step on a statement of a closed connection: %v, want ErrBadHandle", err)
	}
	if err := c1.Sqlite3Close(db1); !errors.Is(err, rt.ErrBadHandle) {
		t.Fatalf("second close: %v, want ErrBadHandle", err)
	}
}

// TestSqlAndColumnLimits checks the limits that the hand-written server sets with sqlite3_limit
// (PROTOCOL.md section 11): 2,000 columns, 32,766 variables, a SQL text of 1 MiB.
func TestSqlAndColumnLimits(t *testing.T) {
	_, c, db := session(t)

	cols := func(n int) string {
		var sb strings.Builder
		sb.WriteString("SELECT ")
		for i := range n {
			if i > 0 {
				sb.WriteString(", ")
			}
			fmt.Fprintf(&sb, "%d AS c%d", i, i)
		}
		return sb.String()
	}
	// 2,000 columns: the widest row that is allowed is written as 2,000 cells, and the metadata fits.
	st := prep(t, c, db, cols(2000))
	// A row of 2,000 cells is 32 KB: the batch ends after it, because a second row of that width could not
	// be guaranteed to fit (the shim never steps a row it cannot write), and the next call says DONE.
	b := fetch(t, c, st, 0)
	if len(b.rows) != 1 || len(b.rows[0]) != 2000 || b.rc != sqlite3.SQLITE_ROW {
		t.Fatalf("2,000 columns: %d rows, rc %d", len(b.rows), b.rc)
	}
	for i, v := range b.rows[0] {
		requireCell(t, v, int64(i))
	}
	if b2 := fetch(t, c, st, 0); len(b2.rows) != 0 || b2.rc != sqlite3.SQLITE_DONE {
		t.Fatalf("2,000 columns, second batch: %d rows, rc %d", len(b2.rows), b2.rc)
	}
	info, err := c.StmtInfo(st)
	if err != nil || len(info.Columns) != 2000 || info.Columns[1999].Name != "c1999" {
		t.Fatalf("stmt_info of 2,000 columns: %d columns, %v", len(info.Columns), err)
	}
	finalize(t, c, st)
	if _, err := c.PrepareOne(db, cols(2001)); libErr(t, err).Code != codeError {
		t.Fatalf("2,001 columns: %v, want SQLITE_ERROR", err)
	}

	if _, err := c.PrepareOne(db, "SELECT ?32766"); err != nil {
		t.Fatalf("variable 32766: %v", err)
	}
	if le := libErr(t, func() error { _, err := c.PrepareOne(db, "SELECT ?32767"); return err }()); le.Code != codeError {
		t.Fatalf("variable 32767: code %d %q", le.Code, le.Detail)
	}

	// The wire bound of the text is the limit of SQLite, 1 MiB.
	exact := "SELECT 1" + strings.Repeat(" ", int(sqlite3.SVC_MAX_SQL)-len("SELECT 1"))
	st = prep(t, c, db, exact)
	finalize(t, c, st)
	if _, err := c.PrepareOne(db, exact+" "); err == nil {
		t.Fatal("a SQL text of 1 MiB + 1 was accepted")
	}
	// A refusal before the commit leaves the session usable.
	if got := one(t, c, db, "SELECT 41 + 1"); got != int64(42) {
		t.Fatalf("after the refusal: %v", got)
	}
}

// TestNameLimits checks the parameter and column names: bind_parameter_name and stmt_info, and the
// refusal of a column name above SVC_MAX_NAME bytes.
func TestNameLimits(t *testing.T) {
	_, c, db := session(t)
	st := prep(t, c, db, "SELECT :id, @name, $x, ?, ?7, :id")
	info, err := c.StmtInfo(st)
	if err != nil {
		t.Fatal(err)
	}
	if info.Nparams != 7 || !info.Readonly {
		t.Fatalf("nparams %d, readonly %v; want 7, true", info.Nparams, info.Readonly)
	}
	// ?, ?7, :id: a plain ? is the next index after the largest so far (4), ?7 is 7, and :id is 1 again.
	want := []string{":id", "@name", "$x", "", "", "", "?7"}
	if len(info.Param_names) != len(want) {
		t.Fatalf("names %q, want %q", info.Param_names, want)
	}
	for i := range want {
		if info.Param_names[i] != want[i] {
			t.Fatalf("name %d = %q, want %q (all: %q)", i+1, info.Param_names[i], want[i], info.Param_names)
		}
	}
	if idx, err := c.Sqlite3BindParameterIndex(st, "@name"); err != nil || idx != 2 {
		t.Fatalf("bind_parameter_index(@name) = %d, %v", idx, err)
	}
	if idx, err := c.Sqlite3BindParameterIndex(st, ":none"); err != nil || idx != 0 {
		t.Fatalf("bind_parameter_index(:none) = %d, %v", idx, err)
	}
	if n, err := c.Sqlite3BindParameterName(st, 3); err != nil || n == nil || *n != "$x" {
		t.Fatalf("bind_parameter_name(3) = %v, %v", n, err)
	}
	if n, err := c.Sqlite3BindParameterName(st, 4); err != nil || n != nil {
		t.Fatalf("bind_parameter_name(4) = %v, %v; want nil", n, err)
	}
	finalize(t, c, st)

	long := strings.Repeat("n", int(sqlite3.SVC_MAX_NAME)+1)
	st = prep(t, c, db, `SELECT 1 AS "`+long+`"`)
	_, err = c.StmtInfo(st)
	if se := statusErr(t, err); se.Status != sqlite3.SVC_STATUS_NAME_TOO_LONG {
		t.Fatalf("stmt_info with a long column name: status %d, want %d", se.Status, sqlite3.SVC_STATUS_NAME_TOO_LONG)
	}
	// The direct accessor refuses it the same way the codec does (the shim of stmt_info made no change).
	if _, err := c.Sqlite3ColumnName(st, 0); err == nil {
		t.Fatal("column_name of a name above its bound succeeded")
	}
	okName := strings.Repeat("n", int(sqlite3.SVC_MAX_NAME))
	st2 := prep(t, c, db, `SELECT 1 AS "`+okName+`"`)
	info, err = c.StmtInfo(st2)
	if err != nil || info.Columns[0].Name != okName {
		t.Fatalf("a column name of exactly SVC_MAX_NAME bytes: %v", err)
	}
	// The session survived the application status.
	if got := one(t, c, db, "SELECT 1"); got != int64(1) {
		t.Fatalf("after the refusal: %v", got)
	}
}

// TestColumnMetadata checks column_count, column_name, column_decltype and stmt_info against each other.
func TestColumnMetadata(t *testing.T) {
	_, c, db := session(t)
	run(t, c, db, "CREATE TABLE m (id INTEGER PRIMARY KEY, name VARCHAR(20), blobby BLOB, untyped)")
	st := prep(t, c, db, "SELECT id, name AS who, blobby, untyped, 1 + 1, 'x' FROM m WHERE id > ? AND name = :n")
	n, err := c.Sqlite3ColumnCount(st)
	if err != nil || n != 6 {
		t.Fatalf("column_count = %d, %v", n, err)
	}
	info, err := c.StmtInfo(st)
	if err != nil {
		t.Fatal(err)
	}
	names := []string{"id", "who", "blobby", "untyped", "1 + 1", "'x'"}
	decl := []string{"INTEGER", "VARCHAR(20)", "BLOB", "", "", ""}
	if len(info.Columns) != 6 {
		t.Fatalf("%d columns", len(info.Columns))
	}
	for i := range 6 {
		nm, err := c.Sqlite3ColumnName(st, int32(i))
		if err != nil || nm == nil || *nm != names[i] || info.Columns[i].Name != names[i] {
			t.Fatalf("column %d name: %v %v / %q, want %q", i, nm, err, info.Columns[i].Name, names[i])
		}
		dt, err := c.Sqlite3ColumnDecltype(st, int32(i))
		if err != nil || info.Columns[i].Decltype != decl[i] {
			t.Fatalf("column %d decltype: %v %v / %q, want %q", i, dt, err, info.Columns[i].Decltype, decl[i])
		}
		if (decl[i] == "") != (dt == nil) || dt != nil && *dt != decl[i] {
			t.Fatalf("column %d decltype pointer %v, want %q (nil for none)", i, dt, decl[i])
		}
	}
	if nm, err := c.Sqlite3ColumnName(st, 6); err != nil || nm != nil {
		t.Fatalf("column_name out of range: %v, %v; want nil", nm, err)
	}
	if info.Nparams != 2 || info.Param_names[0] != "" || info.Param_names[1] != ":n" || !info.Readonly {
		t.Fatalf("params: %+v", info)
	}
	if ro, err := c.Sqlite3StmtReadonly(st); err != nil || ro == 0 {
		t.Fatalf("stmt_readonly = %d, %v", ro, err)
	}
	st2 := prep(t, c, db, "DELETE FROM m")
	if ro, err := c.Sqlite3StmtReadonly(st2); err != nil || ro != 0 {
		t.Fatalf("stmt_readonly of DELETE = %d, %v", ro, err)
	}
	if i2, _ := c.StmtInfo(st2); i2.Readonly || len(i2.Columns) != 0 {
		t.Fatalf("stmt_info of DELETE: %+v", i2)
	}
}

// TestVersion checks the two version functions.
func TestVersion(t *testing.T) {
	_, c, _ := session(t)
	n, err := c.Sqlite3LibversionNumber()
	if err != nil || n < 3037000 {
		t.Fatalf("libversion_number = %d, %v", n, err)
	}
	s, err := c.Sqlite3Libversion()
	if err != nil || !strings.HasPrefix(s, fmt.Sprintf("%d.%d.%d", n/1000000, n/1000%1000, n%1000)) {
		t.Fatalf("libversion = %q, %v; number %d", s, err, n)
	}
}
