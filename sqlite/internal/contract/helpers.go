package contract

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"gosuda.org/hqq/sqlite"
)

// must fails the test when err is not nil.
func must(t *testing.T, err error, what string) {
	t.Helper()
	if err != nil {
		t.Fatalf("%s: %v", what, err)
	}
}

// fullSQL skips the test unless the engine is real SQLite.
func fullSQL(t *testing.T, caps Capabilities) {
	t.Helper()
	if !caps.FullSQL {
		t.Skipf("%s: needs FullSQL", caps.Engine)
	}
}

// testCtx returns a context that ends after ten seconds, so a case that blocks
// fails instead of hanging the run.
func testCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// execer runs a statement without a context: *sql.DB and *sql.Tx.
type execer interface {
	Exec(query string, args ...any) (sql.Result, error)
}

// querier runs queries without a context: *sql.DB and *sql.Tx.
type querier interface {
	Query(query string, args ...any) (*sql.Rows, error)
	QueryRow(query string, args ...any) *sql.Row
}

// connDB runs statements on one held *sql.Conn.
type connDB struct {
	t *testing.T
	c *sql.Conn
}

func (d connDB) Exec(query string, args ...any) (sql.Result, error) {
	return d.c.ExecContext(testCtx(d.t), query, args...)
}

func (d connDB) Query(query string, args ...any) (*sql.Rows, error) {
	return d.c.QueryContext(testCtx(d.t), query, args...)
}

func (d connDB) QueryRow(query string, args ...any) *sql.Row {
	return d.c.QueryRowContext(testCtx(d.t), query, args...)
}

// exec runs query and fails the test on error.
func exec(t *testing.T, e execer, query string, args ...any) sql.Result {
	t.Helper()
	res, err := e.Exec(query, args...)
	must(t, err, "exec "+query)
	return res
}

// newTable creates a table with a unique name and returns the name. cols is
// the column list without the parentheses.
func newTable(t *testing.T, e execer, prefix, cols string) string {
	t.Helper()
	name := fmt.Sprintf("%s_%d", prefix, tableSeq.Add(1))
	exec(t, e, fmt.Sprintf("CREATE TABLE %s (%s)", name, cols))
	return name
}

// scanOne runs a query that returns one row and scans its first column.
func scanOne[T any](t *testing.T, q querier, query string, args ...any) T {
	t.Helper()
	var v T
	must(t, q.QueryRow(query, args...).Scan(&v), "scan "+query)
	return v
}

// ints returns the first column of every row of query, in result order.
func ints(t *testing.T, q querier, query string, args ...any) []int64 {
	t.Helper()
	rows, err := q.Query(query, args...)
	must(t, err, "query "+query)
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var v int64
		if err := rows.Scan(&v); err != nil {
			t.Fatalf("scan %s: %v", query, err)
		}
		out = append(out, v)
	}
	must(t, rows.Err(), "rows "+query)
	return out
}

// wantInts fails unless query returns exactly want, in order.
func wantInts(t *testing.T, q querier, query string, want ...int64) {
	t.Helper()
	if got := ints(t, q, query); !slices.Equal(got, want) {
		t.Fatalf("%s: got %v, want %v", query, got, want)
	}
}

// requireSQLError fails unless err is a *sqlite.Error with the primary code.
func requireSQLError(t *testing.T, err error, code int) {
	t.Helper()
	var se *sqlite.Error
	if !errors.As(err, &se) {
		t.Fatalf("err = %v (%T), want *sqlite.Error", err, err)
	}
	if se.Code != code {
		t.Fatalf("code = %d (%s), want %d", se.Code, se.Message, code)
	}
}
