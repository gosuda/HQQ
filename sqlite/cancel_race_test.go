//go:build unix

package sqlite_test

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gosuda.org/hqq/bindgen/examples/sqlite3/sqlite3srv"
	"gosuda.org/hqq/sqlite"
)

// okCancelError reports whether err is an acceptable outcome of a call whose
// context was canceled at an arbitrary moment: the cancellation itself, or the
// loss of the connection that the cancellation abandoned. A SQL error, a
// malformed answer or a transport error that is not marked as a bad connection
// means the driver mishandled a call that was abandoned.
func okCancelError(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, driver.ErrBadConn) || errors.Is(err, sql.ErrConnDone) ||
		errors.Is(err, sqlite.ErrOutcomeUnknown)
}

// TestCancelRace cancels the context of queries at random moments while the
// driver has a fetch-ahead in flight and decodes batches in place: during
// QueryContext, during Next, during rows.Close, and between statements of one
// connection. The cancellation reaches the driver as a Kill and Cancel of the
// session from another goroutine (the guard), while the owner may be inside the
// generated client's decoder.
//
// It asserts: no crash, every error is the cancellation or a bad connection,
// canceled connections are discarded and replaced, and the pool keeps working.
func TestCancelRace(t *testing.T) {
	// Every canceled call abandons its session, which the server tears down a moment
	// later, so the churn of sessions needs headroom over the default limit.
	srv := sqlite3srv.Start(t, sqlite3srv.Config{Args: []string{"--max-sessions", "8192"}})
	cfg := genConfig("race.db")
	cfg.FetchSize = 8 // many batches: a fetch-ahead is almost always in flight
	db := openGen(t, srv, cfg)
	db.SetMaxOpenConns(6)
	mustExec(t, db, "CREATE TABLE r (id INTEGER PRIMARY KEY, v TEXT)")
	mustExec(t, db, "WITH RECURSIVE c(x) AS (SELECT 1 UNION ALL SELECT x + 1 FROM c WHERE x < 600) INSERT INTO r SELECT x, printf('%0100d', x) FROM c")

	const workers = 6
	budget := 3 * time.Second
	if testing.Short() {
		budget = time.Second
	}
	began := time.Now()
	deadline := began.Add(budget)
	const maxIterations = 3000
	var iterations, canceled, completed atomic.Int64
	var wg sync.WaitGroup
	fail := make(chan string, workers*4)
	report := func(format string, args ...any) {
		select {
		case fail <- fmt.Sprintf(format, args...):
		default:
		}
	}
	for w := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rng := rand.New(rand.NewPCG(uint64(w)+1, 99))
			for time.Now().Before(deadline) && iterations.Add(1) <= maxIterations {
				raceOnce(rng, db, &canceled, &completed, report)
			}
		}()
	}
	wg.Wait()
	close(fail)
	for msg := range fail {
		t.Error(msg)
	}
	if t.Failed() {
		return
	}
	if canceled.Load() == 0 || completed.Load() == 0 {
		t.Fatalf("the random cancellation never split the outcomes: %d canceled, %d completed of %d iterations",
			canceled.Load(), completed.Load(), iterations.Load())
	}

	// The pool recovered: the discarded connections were replaced and everything works.
	ctx, stopCtx := context.WithTimeout(context.Background(), 30*time.Second)
	defer stopCtx()
	for range 12 {
		var n int64
		if err := db.QueryRowContext(ctx, "SELECT count(*) FROM r").Scan(&n); err != nil || n != 600 {
			t.Fatalf("count after the storm = %d, %v", n, err)
		}
	}
	rows, err := db.QueryContext(ctx, "SELECT id, v FROM r ORDER BY id")
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for rows.Next() {
		var id int64
		var v string
		if err := rows.Scan(&id, &v); err != nil {
			t.Fatal(err)
		}
		n++
		if id != int64(n) || v != fmt.Sprintf("%0100d", n) {
			t.Fatalf("row %d: id %d, v %q", n, id, v)
		}
	}
	if err := rows.Err(); err != nil || n != 600 {
		t.Fatalf("full scan after the storm: %d rows, %v", n, err)
	}
	rows.Close()
	if st := db.Stats(); st.InUse != 0 {
		t.Fatalf("connections still in use after the storm: %+v", st)
	}
	t.Logf("storm and recovery done after %v", time.Since(began))
	t.Logf("%d iterations: %d canceled, %d completed", iterations.Load(), canceled.Load(), completed.Load())
}

// raceOnce runs one randomized scenario.
func raceOnce(rng *rand.Rand, db *sql.DB, canceled, completed *atomic.Int64, report func(string, ...any)) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// The cancellation fires at a random moment of a short window that spans
	// the whole scenario, or never.
	var timer *time.Timer
	if rng.IntN(5) != 0 {
		timer = time.AfterFunc(time.Duration(rng.IntN(1500))*time.Microsecond, cancel)
		defer timer.Stop()
	}
	check := func(what string, err error) bool {
		if err == nil {
			return true
		}
		if !okCancelError(err) {
			report("%s: unexpected error %T: %v", what, err, err)
			return false
		}
		canceled.Add(1)
		return false
	}

	switch rng.IntN(4) {
	case 0, 1: // read some rows, then close or read on, with the cancel anywhere
		rows, err := db.QueryContext(ctx, "SELECT id, v FROM r ORDER BY id")
		if !check("query", err) {
			return
		}
		stop := rng.IntN(120) + 1
		n := 0
		for rows.Next() {
			var id int64
			var v string
			if err := rows.Scan(&id, &v); err != nil {
				check("scan", err)
				break
			}
			n++
			if id != int64(n) || len(v) != 100 {
				report("row %d: id %d, %d bytes", n, id, len(v))
				break
			}
			if n == stop && rng.IntN(2) == 0 {
				break
			}
		}
		check("rows.Err", rows.Err())
		check("rows.Close", rows.Close())
		completed.Add(1)
	case 2: // a prepared statement used by one connection, canceled between statements
		c, err := db.Conn(context.Background())
		if err != nil {
			report("conn: %v", err)
			return
		}
		defer c.Close()
		for i := range 3 {
			var n int64
			err := c.QueryRowContext(ctx, "SELECT count(*) FROM r WHERE id > ?", i*100).Scan(&n)
			if !check("count", err) {
				// A canceled call may have killed the connection: the next call on
				// it may fail with the cancellation or a bad connection, not crash.
				_ = c.PingContext(context.Background())
				return
			}
			if n != int64(600-i*100) {
				report("count %d = %d", i, n)
				return
			}
			if rng.IntN(2) == 0 {
				time.Sleep(time.Duration(rng.IntN(300)) * time.Microsecond)
			}
		}
		completed.Add(1)
	case 3: // abandon the rows without closing them until the context ends
		rows, err := db.QueryContext(ctx, "SELECT id, v FROM r ORDER BY id")
		if !check("query", err) {
			return
		}
		rows.Next()
		time.Sleep(time.Duration(rng.IntN(800)) * time.Microsecond)
		// database/sql closes the rows itself when the context ends (awaitDone);
		// closing them here races with it, as an application does.
		check("rows.Close", rows.Close())
		completed.Add(1)
	}
}
