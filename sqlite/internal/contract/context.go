package contract

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"gosuda.org/hqq/sqlite"
)

func testContext(t *testing.T, db *sql.DB, caps Capabilities) {
	t.Run("canceled context runs nothing", func(t *testing.T) {
		tbl := newTable(t, db, "cx_canceled", "id INTEGER PRIMARY KEY, v TEXT")
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, err := db.ExecContext(ctx, "INSERT INTO "+tbl+" (id, v) VALUES (1, 'x')"); !errors.Is(err, context.Canceled) {
			t.Fatalf("Exec with a canceled context: %v", err)
		}
		if _, err := db.QueryContext(ctx, "SELECT id FROM "+tbl); !errors.Is(err, context.Canceled) {
			t.Fatalf("Query with a canceled context: %v", err)
		}
		wantInts(t, db, "SELECT id FROM "+tbl)
	})

	t.Run("deadline interrupts a long query", func(t *testing.T) {
		query := "SELECT hqq_sleep(10000)"
		if caps.FullSQL {
			query = "WITH RECURSIVE c(x) AS (SELECT 1 UNION ALL SELECT x + 1 FROM c) SELECT count(*) FROM c"
			if caps.NoInterrupt {
				// About a second of work, well past the deadline, and finite.
				query = "WITH RECURSIVE c(x) AS (SELECT 1 UNION ALL SELECT x + 1 FROM c WHERE x < 10000000) SELECT count(*) FROM c"
			}
		}
		ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
		defer cancel()
		start := time.Now()
		var n int64
		err := db.QueryRowContext(ctx, query).Scan(&n)
		if err == nil {
			t.Fatalf("long query finished with %d", n)
		}
		if !errors.Is(err, context.DeadlineExceeded) && !sqlite.IsInterrupted(err) {
			t.Fatalf("long query error = %v, want the deadline", err)
		}
		if d := time.Since(start); d > 5*time.Second {
			t.Fatalf("query ran %v past its 200ms deadline", d)
		}
		if got := scanOne[int64](t, db, "SELECT 1"); got != 1 {
			t.Fatalf("SELECT 1 after the interrupt = %d", got)
		}
	})

	t.Run("canceling a transaction context rolls it back", func(t *testing.T) {
		tbl := newTable(t, db, "cx_tx", "id INTEGER PRIMARY KEY, v TEXT")
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		tx, err := db.BeginTx(ctx, nil)
		must(t, err, "begin")
		exec(t, tx, "INSERT INTO "+tbl+" (id, v) VALUES (2, 'in tx')")
		cancel()
		if err := tx.Commit(); err == nil {
			t.Fatal("Commit after cancel succeeded")
		}
		wantInts(t, db, "SELECT id FROM "+tbl+" WHERE id = 2")
		// The rollback must release the write lock: this write goes through.
		exec(t, db, "INSERT INTO "+tbl+" (id, v) VALUES (2, 'after cancel')")
		wantInts(t, db, "SELECT id FROM "+tbl+" WHERE id = 2", 2)
	})

	t.Run("Ping and Conn honor the context", func(t *testing.T) {
		ctx := testCtx(t)
		must(t, db.PingContext(ctx), "ping")
		c, err := db.Conn(ctx)
		must(t, err, "conn")
		must(t, c.PingContext(ctx), "conn ping")
		must(t, c.Close(), "conn close")
		canceled, cancel := context.WithCancel(context.Background())
		cancel()
		if _, err := db.Conn(canceled); !errors.Is(err, context.Canceled) {
			t.Fatalf("Conn with a canceled context: %v", err)
		}
	})
}
