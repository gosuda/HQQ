package contract

import (
	"database/sql"
	"errors"
	"fmt"
	"testing"

	"gosuda.org/hqq/sqlite"
)

// seededTable creates a table with a unique name and inserts the row (1, 'base').
// Each subtest that changes data uses its own table.
func seededTable(t *testing.T, db *sql.DB, prefix string) string {
	t.Helper()
	tbl := newTable(t, db, prefix, "id INTEGER PRIMARY KEY, v TEXT")
	exec(t, db, "INSERT INTO "+tbl+" (id, v) VALUES (1, 'base')")
	return tbl
}

func testTransactions(t *testing.T, db *sql.DB, caps Capabilities) {
	ctx := testCtx(t)

	t.Run("commit publishes the writes", func(t *testing.T) {
		tbl := seededTable(t, db, "txn_commit")
		tx, err := db.Begin()
		must(t, err, "begin")
		exec(t, tx, "INSERT INTO "+tbl+" (id, v) VALUES (2, 'committed')")
		must(t, tx.Commit(), "commit")
		wantInts(t, db, "SELECT id FROM "+tbl, 1, 2)
	})

	t.Run("rollback discards the writes", func(t *testing.T) {
		tbl := seededTable(t, db, "txn_rollback")
		tx, err := db.Begin()
		must(t, err, "begin")
		exec(t, tx, "INSERT INTO "+tbl+" (id, v) VALUES (3, 'rolled back')")
		must(t, tx.Rollback(), "rollback")
		wantInts(t, db, "SELECT id FROM "+tbl, 1)
	})

	t.Run("statements inside a transaction see its own writes", func(t *testing.T) {
		tbl := seededTable(t, db, "txn_own")
		tx, err := db.Begin()
		must(t, err, "begin")
		defer tx.Rollback()
		exec(t, tx, "INSERT INTO "+tbl+" (id, v) VALUES (4, 'mine')")
		wantInts(t, tx, "SELECT id FROM "+tbl+" WHERE id = 4", 4)
	})

	t.Run("second Commit reports ErrTxDone", func(t *testing.T) {
		tx, err := db.Begin()
		must(t, err, "begin")
		must(t, tx.Commit(), "commit")
		if err := tx.Commit(); !errors.Is(err, sql.ErrTxDone) {
			t.Fatalf("second Commit: %v, want ErrTxDone", err)
		}
		if _, err := tx.Exec("SELECT 1"); !errors.Is(err, sql.ErrTxDone) {
			t.Fatalf("Exec after Commit: %v, want ErrTxDone", err)
		}
	})

	t.Run("uncommitted rows are invisible to other connections", func(t *testing.T) {
		tbl := seededTable(t, db, "txn_visible")
		c1, err := db.Conn(ctx)
		must(t, err, "conn 1")
		defer c1.Close()
		c2, err := db.Conn(ctx)
		must(t, err, "conn 2")
		defer c2.Close()
		tx, err := c1.BeginTx(ctx, nil)
		must(t, err, "begin on conn 1")
		exec(t, tx, "INSERT INTO "+tbl+" (id, v) VALUES (5, 'pending')")
		wantInts(t, connDB{t, c2}, "SELECT id FROM "+tbl+" WHERE id = 5")
		must(t, tx.Commit(), "commit")
		wantInts(t, connDB{t, c2}, "SELECT id FROM "+tbl+" WHERE id = 5", 5)
	})

	t.Run("read-only transaction rejects writes", func(t *testing.T) {
		tbl := seededTable(t, db, "txn_ro")
		tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
		must(t, err, "begin read-only")
		_, err = tx.Exec("INSERT INTO " + tbl + " (id, v) VALUES (6, 'nope')")
		if !sqlite.IsReadOnly(err) {
			t.Fatalf("write in a read-only transaction: %v, want IsReadOnly", err)
		}
		wantInts(t, tx, "SELECT id FROM "+tbl+" WHERE id = 1", 1)
		must(t, tx.Rollback(), "rollback")
		wantInts(t, db, "SELECT id FROM "+tbl+" WHERE id = 6")
	})

	t.Run("isolation levels", func(t *testing.T) {
		for _, lvl := range []sql.IsolationLevel{sql.LevelDefault, sql.LevelSerializable} {
			tx, err := db.BeginTx(ctx, &sql.TxOptions{Isolation: lvl})
			must(t, err, fmt.Sprintf("begin with isolation %v", lvl))
			must(t, tx.Rollback(), "rollback")
		}
		db.SetMaxOpenConns(1)
		defer db.SetMaxOpenConns(0)
		if tx, err := db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadUncommitted}); err == nil {
			_ = tx.Rollback()
			t.Fatal("unsupported isolation level accepted")
		}
		// The failed BeginTx must not leave a transaction open on the connection.
		c, err := db.Conn(ctx)
		must(t, err, "conn")
		defer c.Close()
		exec(t, connDB{t, c}, "BEGIN")
		exec(t, connDB{t, c}, "COMMIT")
	})

	t.Run("BEGIN in SQL text is rolled back when the connection returns to the pool", func(t *testing.T) {
		tbl := seededTable(t, db, "txn_reset")
		db.SetMaxOpenConns(1)
		defer db.SetMaxOpenConns(0)
		c, err := db.Conn(ctx)
		must(t, err, "conn")
		exec(t, connDB{t, c}, "BEGIN")
		exec(t, connDB{t, c}, "INSERT INTO "+tbl+" (id, v) VALUES (7, 'left open')")
		must(t, c.Close(), "return conn")
		wantInts(t, db, "SELECT id FROM "+tbl+" WHERE id = 7")
		exec(t, db, "INSERT INTO "+tbl+" (id, v) VALUES (7, 'after')")
		wantInts(t, db, "SELECT id FROM "+tbl+" WHERE id = 7", 7)
	})

	t.Run("nested BEGIN through SQL text fails", func(t *testing.T) {
		tx, err := db.Begin()
		must(t, err, "begin")
		if _, err := tx.Exec("BEGIN"); err == nil {
			_ = tx.Rollback()
			t.Fatal("nested BEGIN accepted")
		}
		must(t, tx.Rollback(), "rollback after failed nested BEGIN")
	})
}
