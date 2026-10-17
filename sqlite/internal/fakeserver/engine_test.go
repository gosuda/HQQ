package fakeserver

import (
	"context"
	"fmt"
	"testing"
	"time"

	"gosuda.org/hqq/sqlite/internal/wire"
)

// query runs sql and returns its rows rendered as strings, or the error.
func query(ctx context.Context, c *engConn, sql string, ps ...wire.Param) ([][]string, error) {
	s, err := c.Prepare(sql)
	if err != nil {
		return nil, err
	}
	defer s.Finalize()
	if err := s.Bind(ps); err != nil {
		return nil, err
	}
	var out [][]string
	for {
		vals, done, err := s.Step(ctx)
		if err != nil {
			return out, err
		}
		if done {
			return out, nil
		}
		row := make([]string, len(vals))
		for i, v := range vals {
			row[i] = render(v)
		}
		out = append(out, row)
	}
}

func render(v wire.Value) string {
	switch v.Type {
	case wire.TypeInteger:
		return fmt.Sprint(v.Int)
	case wire.TypeFloat:
		return fmt.Sprint(v.Float)
	case wire.TypeText:
		return "'" + string(v.Bytes) + "'"
	case wire.TypeBlob:
		return fmt.Sprintf("x%x", v.Bytes)
	}
	return "NULL"
}

func newTestConn(t *testing.T, name string) *engConn {
	t.Helper()
	c, err := newEngine().open(name, wire.OpenReadWrite|wire.OpenCreate, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	return c
}

func TestEngineStatements(t *testing.T) {
	ctx := context.Background()
	tests := []struct {
		name  string
		setup []string
		sql   string
		want  [][]string
		rc    int // expected SQLite code of the error, 0 for none
	}{
		{
			name:  "select star with where and limit",
			setup: []string{"CREATE TABLE t (id INTEGER PRIMARY KEY, v TEXT)", "INSERT INTO t (id, v) VALUES (1, 'a'), (2, 'b'), (3, 'a')"},
			sql:   "SELECT id, v FROM t WHERE v = 'a' LIMIT 1",
			want:  [][]string{{"1", "'a'"}},
		},
		{
			name:  "alias and expression names",
			setup: []string{"CREATE TABLE t (v INTEGER)", "INSERT INTO t VALUES (5)"},
			sql:   "SELECT v AS x, 7, NULL FROM t",
			want:  [][]string{{"5", "7", "NULL"}},
		},
		{
			name: "series with filter",
			sql:  "SELECT value FROM hqq_series(1, 10) WHERE value = 4",
			want: [][]string{{"4"}},
		},
		{
			name: "series limit stops early",
			sql:  "SELECT value FROM hqq_series(1, 1000000) LIMIT 3",
			want: [][]string{{"1"}, {"2"}, {"3"}},
		},
		{
			name:  "update changes only matching rows",
			setup: []string{"CREATE TABLE t (id INTEGER, v INTEGER)", "INSERT INTO t VALUES (1, 10), (2, 20)", "UPDATE t SET v = 99 WHERE id = 2"},
			sql:   "SELECT id, v FROM t",
			want:  [][]string{{"1", "10"}, {"2", "99"}},
		},
		{
			name:  "delete",
			setup: []string{"CREATE TABLE t (id INTEGER)", "INSERT INTO t VALUES (1), (2), (3)", "DELETE FROM t WHERE id = 2"},
			sql:   "SELECT id FROM t",
			want:  [][]string{{"1"}, {"3"}},
		},
		{
			name:  "not null constraint",
			setup: []string{"CREATE TABLE t (id INTEGER NOT NULL)"},
			sql:   "INSERT INTO t VALUES (NULL)",
			rc:    rcConstraintNotNull,
		},
		{
			name:  "primary key duplicate",
			setup: []string{"CREATE TABLE t (id INTEGER PRIMARY KEY)", "INSERT INTO t VALUES (1)"},
			sql:   "INSERT INTO t VALUES (1)",
			rc:    rcConstraintPK,
		},
		{
			name:  "unique duplicate",
			setup: []string{"CREATE TABLE t (email TEXT UNIQUE)", "INSERT INTO t VALUES ('a')"},
			sql:   "INSERT INTO t VALUES ('a')",
			rc:    rcConstraintUnique,
		},
		{
			name:  "multi-row insert is atomic per statement",
			setup: []string{"CREATE TABLE t (id INTEGER PRIMARY KEY)", "INSERT INTO t VALUES (1)"},
			sql:   "INSERT INTO t VALUES (2), (1)",
			rc:    rcConstraintPK,
		},
		{
			name:  "integer primary key is the rowid",
			setup: []string{"CREATE TABLE t (id INTEGER PRIMARY KEY, v TEXT)", "INSERT INTO t (id, v) VALUES (10, 'a')", "INSERT INTO t (v) VALUES ('b')"},
			sql:   "SELECT id, v FROM t",
			want:  [][]string{{"10", "'a'"}, {"11", "'b'"}},
		},
		{
			name: "no such table",
			sql:  "SELECT * FROM missing",
			rc:   rcError,
		},
		{
			name: "syntax error",
			sql:  "SELEC 1",
			rc:   rcError,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := newTestConn(t, ":memory:")
			for _, s := range tc.setup {
				if err := c.Exec(ctx, s); err != nil {
					t.Fatalf("setup %q: %v", s, err)
				}
			}
			got, err := query(ctx, c, tc.sql)
			if tc.rc != 0 {
				se, ok := err.(*sqlError)
				if !ok || se.rc != tc.rc {
					t.Fatalf("err = %v, want rc %d", err, tc.rc)
				}
				return
			}
			if err != nil {
				t.Fatalf("query: %v", err)
			}
			if fmt.Sprint(got) != fmt.Sprint(tc.want) {
				t.Fatalf("rows = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestEngineParameters(t *testing.T) {
	ctx := context.Background()
	c := newTestConn(t, ":memory:")
	s, err := c.Prepare("SELECT ?, :x, ?2, @y, ?")
	if err != nil {
		t.Fatal(err)
	}
	// SQLite numbering: ?1, :x=?2, ?2 (reuse), @y=?3, ? = next after max (4).
	if s.NParams() != 4 {
		t.Fatalf("NParams = %d, want 4", s.NParams())
	}
	if err := s.Bind([]wire.Param{
		{Ordinal: 1, Value: wire.Integer(1)},
		{Ordinal: 2, Name: "x", Value: wire.Text("x")},
		{Ordinal: 3, Name: "y", Value: wire.Integer(3)},
		{Ordinal: 4, Value: wire.Null()},
	}); err != nil {
		t.Fatal(err)
	}
	vals, _, err := s.Step(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if render(vals[0]) != "1" || render(vals[1]) != "'x'" || render(vals[2]) != "'x'" || render(vals[3]) != "3" {
		t.Fatalf("values = %v", vals)
	}
	if err := s.Bind([]wire.Param{{Ordinal: 1, Name: "missing", Value: wire.Integer(1)}}); err == nil {
		t.Fatal("unknown named parameter accepted")
	}
	if err := s.Bind([]wire.Param{{Ordinal: 9, Value: wire.Integer(1)}}); err == nil {
		t.Fatal("out-of-range ordinal accepted")
	}
}

func TestEngineTransactions(t *testing.T) {
	ctx := context.Background()
	db := newEngine()
	a, _ := db.open("shared", wire.OpenReadWrite|wire.OpenCreate, 50*time.Millisecond)
	b, _ := db.open("shared", wire.OpenReadWrite|wire.OpenCreate, 50*time.Millisecond)
	defer a.Close()
	defer b.Close()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(a.Exec(ctx, "CREATE TABLE t (id INTEGER)"))
	must(a.Exec(ctx, "BEGIN"))
	must(a.Exec(ctx, "INSERT INTO t VALUES (1)"))

	// Uncommitted rows are private to the writer.
	if got, _ := query(ctx, b, "SELECT id FROM t"); len(got) != 0 {
		t.Fatalf("other session sees uncommitted rows: %v", got)
	}
	// The writer lock is held: another writer times out with BUSY.
	err := b.Exec(ctx, "INSERT INTO t VALUES (2)")
	if se, ok := err.(*sqlError); !ok || se.rc != rcBusy {
		t.Fatalf("second writer err = %v, want BUSY", err)
	}
	must(a.Exec(ctx, "ROLLBACK"))
	if got, _ := query(ctx, b, "SELECT id FROM t"); len(got) != 0 {
		t.Fatalf("rollback left rows: %v", got)
	}
	must(b.Exec(ctx, "INSERT INTO t VALUES (3)"))
	if got, _ := query(ctx, a, "SELECT id FROM t"); fmt.Sprint(got) != "[[3]]" {
		t.Fatalf("after commit rows = %v", got)
	}
	if err := a.Exec(ctx, "COMMIT"); err == nil {
		t.Fatal("COMMIT outside a transaction succeeded")
	}
}

func TestEngineReadOnlyAndQueryOnly(t *testing.T) {
	ctx := context.Background()
	db := newEngine()
	rw, _ := db.open("ro", wire.OpenReadWrite|wire.OpenCreate, time.Second)
	defer rw.Close()
	if err := rw.Exec(ctx, "CREATE TABLE t (id INTEGER)"); err != nil {
		t.Fatal(err)
	}
	ro, err := db.open("ro", wire.OpenReadOnly, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	if err := ro.Exec(ctx, "INSERT INTO t VALUES (1)"); err == nil {
		t.Fatal("write on read-only connection succeeded")
	} else if se := err.(*sqlError); se.rc != rcReadOnly {
		t.Fatalf("rc = %d, want READONLY", se.rc)
	}
	if _, err := db.open("missing", wire.OpenReadWrite, time.Second); err == nil {
		t.Fatal("opening a missing database without CREATE succeeded")
	}
}

func TestEngineInterruptIsObserved(t *testing.T) {
	c := newTestConn(t, ":memory:")
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(20*time.Millisecond, cancel)
	start := time.Now()
	_, err := query(ctx, c, "SELECT hqq_sleep(10000)")
	se, ok := err.(*sqlError)
	if !ok || se.rc != rcInterrupt {
		t.Fatalf("err = %v, want INTERRUPT", err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("interrupt took %v", d)
	}
}

func TestEngineChangesCount(t *testing.T) {
	ctx := context.Background()
	c := newTestConn(t, ":memory:")
	if err := c.Exec(ctx, "CREATE TABLE t (id INTEGER PRIMARY KEY, v INTEGER)"); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		sql  string
		want int64
	}{
		{"INSERT INTO t (v) VALUES (1), (2), (3)", 3},
		{"UPDATE t SET v = 9 WHERE v < 3", 2},
		{"DELETE FROM t WHERE v = 9", 2},
	} {
		if err := c.Exec(ctx, tc.sql); err != nil {
			t.Fatalf("%s: %v", tc.sql, err)
		}
		if got := c.Changes(); got != tc.want {
			t.Fatalf("%s: changes = %d, want %d", tc.sql, got, tc.want)
		}
	}
}
