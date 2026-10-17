package contract

import (
	"database/sql"
	"errors"
	"testing"

	"gosuda.org/hqq/sqlite"
)

// constraintTable creates a table with a primary key, a UNIQUE column, and a
// NOT NULL column, and inserts the row (1, 'a', 'x').
func constraintTable(t *testing.T, db *sql.DB, prefix string) string {
	t.Helper()
	tbl := newTable(t, db, prefix, "id INTEGER PRIMARY KEY, u TEXT UNIQUE, n TEXT NOT NULL")
	exec(t, db, "INSERT INTO "+tbl+" (id, u, n) VALUES (1, 'a', 'x')")
	return tbl
}

func testErrors(t *testing.T, db *sql.DB, caps Capabilities) {
	ctx := testCtx(t)

	t.Run("constraint violations", func(t *testing.T) {
		tbl := constraintTable(t, db, "er_constraint")
		cases := []struct {
			name string
			sql  string
			ext  int
		}{
			{"unique", "INSERT INTO " + tbl + " (id, u, n) VALUES (2, 'a', 'x')", sqlite.CodeConstraintUnique},
			{"not null", "INSERT INTO " + tbl + " (id, u, n) VALUES (3, 'b', NULL)", sqlite.CodeConstraintNotNull},
			{"primary key", "INSERT INTO " + tbl + " (id, u, n) VALUES (1, 'c', 'x')", sqlite.CodeConstraintPrimaryKey},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				_, err := db.Exec(tc.sql)
				if err == nil {
					t.Fatal("constraint violation accepted")
				}
				if !sqlite.IsConstraint(err) {
					t.Fatalf("IsConstraint(%v) = false", err)
				}
				if !errors.Is(err, sqlite.ErrConstraint) {
					t.Fatalf("errors.Is(%v, ErrConstraint) = false", err)
				}
				var se *sqlite.Error
				if !errors.As(err, &se) {
					t.Fatalf("%v (%T) is not a *sqlite.Error", err, err)
				}
				if se.Code != sqlite.CodeConstraint || se.ExtendedCode != tc.ext {
					t.Fatalf("codes %d/%d, want %d/%d", se.Code, se.ExtendedCode, sqlite.CodeConstraint, tc.ext)
				}
			})
		}
		wantInts(t, db, "SELECT id FROM "+tbl, 1)
	})

	t.Run("syntax error", func(t *testing.T) {
		_, err := db.Exec("SELEC 1")
		requireSQLError(t, err, sqlite.CodeError)
	})

	t.Run("one statement per call", func(t *testing.T) {
		// PROTOCOL.md section 6.2: a second statement is BAD_REQUEST, a
		// trailing semicolon and whitespace are allowed.
		_, err := db.Exec("SELECT 1; SELECT 2")
		var pe *sqlite.ProtocolError
		if !errors.As(err, &pe) || pe.Code != 1 {
			t.Fatalf("two statements: %v, want a BAD_REQUEST *sqlite.ProtocolError", err)
		}
		_, err = db.Exec("SELECT 1;  ")
		must(t, err, "trailing semicolon")
	})

	t.Run("missing table", func(t *testing.T) {
		rows, err := db.Query("SELECT id FROM missing_table_er")
		if err == nil {
			rows.Close()
			t.Fatal("query on a missing table succeeded")
		}
		requireSQLError(t, err, sqlite.CodeError)
	})

	t.Run("SQL error keeps the connection usable", func(t *testing.T) {
		c, err := db.Conn(ctx)
		must(t, err, "conn")
		defer c.Close()
		_, err = c.ExecContext(ctx, "SELEC 1")
		requireSQLError(t, err, sqlite.CodeError)
		if got := scanOne[int64](t, connDB{t, c}, "SELECT 1"); got != 1 {
			t.Fatalf("SELECT 1 after a SQL error = %d", got)
		}
	})

	t.Run("failed statement keeps its transaction", func(t *testing.T) {
		tbl := constraintTable(t, db, "er_tx")
		tx, err := db.Begin()
		must(t, err, "begin")
		exec(t, tx, "INSERT INTO "+tbl+" (id, u, n) VALUES (4, 'd', 'y')")
		_, err = tx.Exec("INSERT INTO " + tbl + " (id, u, n) VALUES (4, 'e', 'z')")
		if !sqlite.IsConstraint(err) {
			t.Fatalf("duplicate in a transaction: %v, want a constraint error", err)
		}
		exec(t, tx, "INSERT INTO "+tbl+" (id, u, n) VALUES (5, 'f', 'w')")
		must(t, tx.Commit(), "commit")
		wantInts(t, db, "SELECT id FROM "+tbl, 1, 4, 5)
	})

	t.Run("writer blocked by another connection gets SQLITE_BUSY", func(t *testing.T) {
		tbl := constraintTable(t, db, "er_busy")
		c1, err := db.Conn(ctx)
		must(t, err, "conn 1")
		defer c1.Close()
		c2, err := db.Conn(ctx)
		must(t, err, "conn 2")
		defer c2.Close()
		holder, waiter := connDB{t, c1}, connDB{t, c2}
		exec(t, holder, "BEGIN IMMEDIATE")
		exec(t, waiter, "PRAGMA busy_timeout = 50")
		_, err = waiter.Exec("INSERT INTO " + tbl + " (id, u, n) VALUES (6, 'g', 'h')")
		if !sqlite.IsBusy(err) {
			t.Fatalf("write while another connection holds the lock: %v, want IsBusy", err)
		}
		// Readers are not blocked by a writer that has not committed.
		wantInts(t, waiter, "SELECT id FROM "+tbl+" WHERE id = 1", 1)
		exec(t, holder, "ROLLBACK")
		exec(t, waiter, "PRAGMA busy_timeout = 2000")
		exec(t, waiter, "INSERT INTO "+tbl+" (id, u, n) VALUES (6, 'g', 'h')")
		wantInts(t, waiter, "SELECT id FROM "+tbl+" WHERE id = 6", 6)
	})
}
