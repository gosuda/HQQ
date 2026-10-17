package contract

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"gosuda.org/hqq/sqlite"
)

func testPool(t *testing.T, db *sql.DB, caps Capabilities) {
	tbl := newTable(t, db, "pool", "k INTEGER PRIMARY KEY, v INTEGER")

	t.Run("max open connections serialize use", func(t *testing.T) {
		db.SetMaxOpenConns(1)
		defer db.SetMaxOpenConns(0)
		c, err := db.Conn(testCtx(t))
		must(t, err, "conn")
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		defer cancel()
		var one int64
		if err := db.QueryRowContext(ctx, "SELECT 1").Scan(&one); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("query with the only connection held: %v, want the deadline", err)
		}
		must(t, c.Close(), "return conn")
		if got := scanOne[int64](t, db, "SELECT 1"); got != 1 {
			t.Fatalf("SELECT 1 = %d", got)
		}
	})

	t.Run("max idle connections", func(t *testing.T) {
		db.SetMaxIdleConns(1)
		defer db.SetMaxIdleConns(2)
		ctx := testCtx(t)
		var conns []*sql.Conn
		for range 3 {
			c, err := db.Conn(ctx)
			must(t, err, "conn")
			conns = append(conns, c)
		}
		for _, c := range conns {
			must(t, c.Close(), "return conn")
		}
		if idle := db.Stats().Idle; idle > 1 {
			t.Fatalf("idle connections = %d, want at most 1", idle)
		}
	})

	t.Run("connection lifetime recycles connections", func(t *testing.T) {
		db.SetConnMaxLifetime(time.Millisecond)
		defer db.SetConnMaxLifetime(0)
		for i := range 20 {
			exec(t, db, "INSERT INTO "+tbl+" (k, v) VALUES (?, ?)", 1000+i, i)
			time.Sleep(2 * time.Millisecond)
		}
		if got := ints(t, db, "SELECT k FROM "+tbl+" WHERE k >= 1000"); len(got) != 20 {
			t.Fatalf("got %d rows, want 20", len(got))
		}
	})

	t.Run("ping", func(t *testing.T) {
		must(t, db.Ping(), "ping")
	})

	t.Run("concurrent use with one key per goroutine", func(t *testing.T) {
		const workers, rounds = 32, 50
		errc := make(chan error, workers)
		var wg sync.WaitGroup
		for w := range workers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				errc <- useKey(db, tbl, 2000+w, rounds)
			}()
		}
		wg.Wait()
		close(errc)
		for err := range errc {
			if err != nil {
				t.Error(err)
			}
		}
	})
}

// busyBudget bounds how long one statement of useKey retries SQLITE_BUSY.
//
// The workload is 32 writers, each committing autocommit statements to one
// database file in the default rollback-journal mode. SQLite's busy handler
// sleeps and retries with no queue or fairness, so a writer can lose the lock
// for longer than the connector's busy timeout (5 s in the e2e runs). The
// slowest statement measured 5.4 s while every statement still succeeded
// eventually. SQLite does not bound lock wait time under contention, so the
// test does not treat a BUSY as failure. A BUSY from an autocommit statement
// means the statement did not commit, so repeating it is safe. The test still
// requires every write and read-back to succeed within this budget, and every
// value to match.
const busyBudget = 60 * time.Second

// retryBusy runs fn until it does not return SQLITE_BUSY, or busyBudget passes.
func retryBusy(fn func() error) error {
	deadline := time.Now().Add(busyBudget)
	for {
		err := fn()
		if err == nil || !sqlite.IsBusy(err) || time.Now().After(deadline) {
			return err
		}
		time.Sleep(time.Millisecond)
	}
}

// useKey inserts key k and updates it rounds times, reading each value back.
// It returns the first failure.
func useKey(db *sql.DB, tbl string, k, rounds int) error {
	if err := retryBusy(func() error {
		_, err := db.Exec("INSERT INTO "+tbl+" (k, v) VALUES (?, 0)", k)
		return err
	}); err != nil {
		return fmt.Errorf("insert %d: %w", k, err)
	}
	for i := 1; i < rounds; i++ {
		if err := retryBusy(func() error {
			_, err := db.Exec("UPDATE "+tbl+" SET v = ? WHERE k = ?", i, k)
			return err
		}); err != nil {
			return fmt.Errorf("update %d: %w", k, err)
		}
		var v int64
		if err := retryBusy(func() error {
			return db.QueryRow("SELECT v FROM "+tbl+" WHERE k = ?", k).Scan(&v)
		}); err != nil {
			return fmt.Errorf("select %d: %w", k, err)
		}
		if v != int64(i) {
			return fmt.Errorf("key %d: read %d, want %d", k, v, i)
		}
	}
	return nil
}
