package contract

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"testing"
	"time"

	"gosuda.org/hqq/sqlite"
)

// fill inserts rows 1..n with v set to the decimal id, in one transaction.
func fill(t *testing.T, db *sql.DB, tbl string, n int) {
	t.Helper()
	tx, err := db.Begin()
	must(t, err, "begin")
	st, err := tx.Prepare("INSERT INTO " + tbl + " (id, v) VALUES (?, ?)")
	if err != nil {
		_ = tx.Rollback()
		t.Fatalf("prepare: %v", err)
	}
	defer st.Close()
	for i := 1; i <= n; i++ {
		if _, err := st.Exec(i, strconv.Itoa(i)); err != nil {
			_ = tx.Rollback()
			t.Fatalf("insert %d: %v", i, err)
		}
	}
	must(t, tx.Commit(), "commit")
}

// checkSeq fails unless got holds the ids 1..n in order.
func checkSeq(t *testing.T, got []int64, n int) {
	t.Helper()
	if len(got) != n {
		t.Fatalf("got %d rows, want %d", len(got), n)
	}
	for i, v := range got {
		if v != int64(i+1) {
			t.Fatalf("row %d: id %d, want %d", i, v, i+1)
		}
	}
}

func testRows(t *testing.T, db *sql.DB, caps Capabilities) {
	tbl := newTable(t, db, "rows", "id INTEGER PRIMARY KEY, v TEXT")
	fill(t, db, tbl, manyRows)

	t.Run("empty result", func(t *testing.T) {
		checkSeq(t, ints(t, db, "SELECT id FROM "+tbl+" WHERE id < 0"), 0)
	})

	t.Run("one row", func(t *testing.T) {
		checkSeq(t, ints(t, db, "SELECT id FROM "+tbl+" WHERE id <= 1"), 1)
	})

	for _, n := range []int{fetchRows, fetchRows + 1, manyRows} {
		t.Run(fmt.Sprintf("%d rows", n), func(t *testing.T) {
			checkSeq(t, ints(t, db, "SELECT id FROM "+tbl+" WHERE id <= ?", n), n)
		})
	}

	t.Run("LIMIT stops early", func(t *testing.T) {
		checkSeq(t, ints(t, db, "SELECT id FROM "+tbl+" LIMIT ?", 5), 5)
	})

	t.Run("text values of every row", func(t *testing.T) {
		rows, err := db.Query("SELECT id, v FROM " + tbl)
		must(t, err, "query")
		defer rows.Close()
		n := 0
		for rows.Next() {
			var id int64
			var v string
			must(t, rows.Scan(&id, &v), "scan")
			if v != strconv.FormatInt(id, 10) {
				t.Fatalf("row %d: v = %q", id, v)
			}
			n++
		}
		must(t, rows.Err(), "rows")
		if n != manyRows {
			t.Fatalf("got %d rows, want %d", n, manyRows)
		}
	})

	t.Run("early close releases the connection", func(t *testing.T) {
		db.SetMaxOpenConns(1)
		defer db.SetMaxOpenConns(0)
		rows, err := db.Query("SELECT id FROM " + tbl)
		must(t, err, "query")
		for i := range 5 {
			if !rows.Next() {
				t.Fatalf("row %d missing: %v", i, rows.Err())
			}
		}
		must(t, rows.Close(), "close")
		must(t, rows.Err(), "rows.Err after Close")
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		var id int64
		must(t, db.QueryRowContext(ctx, "SELECT id FROM "+tbl+" WHERE id = 1").Scan(&id), "reuse after close")
		if id != 1 {
			t.Fatalf("id = %d, want 1", id)
		}
	})

	t.Run("Next after the end stays false", func(t *testing.T) {
		rows, err := db.Query("SELECT id FROM " + tbl + " WHERE id <= 2")
		must(t, err, "query")
		defer rows.Close()
		n := 0
		for rows.Next() {
			n++
		}
		if n != 2 {
			t.Fatalf("got %d rows, want 2", n)
		}
		for range 2 {
			if rows.Next() {
				t.Fatal("Next returned true after the end")
			}
		}
		must(t, rows.Err(), "rows")
	})

	t.Run("canceled context fails rows.Err mid-stream", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		rows, err := db.QueryContext(ctx, "SELECT id FROM "+tbl)
		must(t, err, "query")
		defer rows.Close()
		if !rows.Next() {
			t.Fatalf("no first row: %v", rows.Err())
		}
		cancel()
		for range manyRows {
			if !rows.Next() {
				break
			}
		}
		err = rows.Err()
		if err == nil {
			t.Fatal("rows.Err is nil after the context was canceled")
		}
		if !errors.Is(err, context.Canceled) && !sqlite.IsInterrupted(err) {
			t.Fatalf("rows.Err = %v, want the cancellation", err)
		}
	})

	t.Run("failing query returns no rows", func(t *testing.T) {
		rows, err := db.Query("SELECT id FROM missing_" + tbl)
		if err == nil {
			rows.Close()
			t.Fatal("query on a missing table succeeded")
		}
		if rows != nil {
			t.Fatal("rows returned together with an error")
		}
		requireSQLError(t, err, sqlite.CodeError)
	})

	t.Run("aggregates and ORDER BY", func(t *testing.T) {
		fullSQL(t, caps)
		if got := scanOne[int64](t, db, "SELECT count(*) FROM "+tbl); got != manyRows {
			t.Fatalf("count(*) = %d, want %d", got, manyRows)
		}
		wantInts(t, db, "SELECT id FROM "+tbl+" ORDER BY id DESC LIMIT 3", manyRows, manyRows-1, manyRows-2)
	})

	t.Run("failing row after the first batch", func(t *testing.T) {
		fullSQL(t, caps)
		rows, err := db.Query(`WITH RECURSIVE c(x) AS (SELECT 1 UNION ALL SELECT x + 1 FROM c WHERE x < 1000)
			SELECT abs(CASE WHEN x = 300 THEN -9223372036854775807 - 1 ELSE x END) FROM c`)
		must(t, err, "query")
		defer rows.Close()
		n := 0
		for rows.Next() {
			n++
		}
		if err := rows.Err(); err == nil {
			t.Fatal("rows.Err is nil after the failing row")
		}
		if n >= 300 {
			t.Fatalf("%d rows came back before the failing row 300", n)
		}
	})
}

// testLarge checks values of a few megabytes and results whose rows each take
// about a megabyte, so that batches are split by size.
func testLarge(t *testing.T, db *sql.DB, caps Capabilities) {
	const mib = 1 << 20

	t.Run("3 MiB text", func(t *testing.T) {
		want := bytes.Repeat([]byte("é"), 3*mib/2) // two bytes per rune
		tbl := newTable(t, db, "lt", "id INTEGER PRIMARY KEY, v TEXT")
		exec(t, db, "INSERT INTO "+tbl+" (id, v) VALUES (1, ?)", string(want))
		if got := scanOne[string](t, db, "SELECT v FROM "+tbl+" WHERE id = 1"); got != string(want) {
			t.Fatalf("text round trip: got %d bytes, want %d", len(got), len(want))
		}
	})

	t.Run("3 MiB blob", func(t *testing.T) {
		want := make([]byte, 3*mib)
		for i := range want {
			want[i] = byte(i % 251)
		}
		tbl := newTable(t, db, "lb", "id INTEGER PRIMARY KEY, v BLOB")
		exec(t, db, "INSERT INTO "+tbl+" (id, v) VALUES (1, ?)", want)
		if got := scanOne[[]byte](t, db, "SELECT v FROM "+tbl+" WHERE id = 1"); !bytes.Equal(got, want) {
			t.Fatalf("blob round trip: got %d bytes, want %d", len(got), len(want))
		}
	})

	t.Run("several rows near 1 MiB", func(t *testing.T) {
		sizes := []int{10, mib - 64, mib - 64, 5, mib - 64, mib - 64, 1}
		tbl := newTable(t, db, "lr", "id INTEGER PRIMARY KEY, v BLOB")
		for i, n := range sizes {
			exec(t, db, "INSERT INTO "+tbl+" (id, v) VALUES (?, ?)", i+1, bytes.Repeat([]byte{byte('a' + i)}, n))
		}
		rows, err := db.Query("SELECT id, v FROM " + tbl)
		must(t, err, "query")
		defer rows.Close()
		n := 0
		for rows.Next() {
			var id int64
			var v []byte
			must(t, rows.Scan(&id, &v), "scan")
			want := bytes.Repeat([]byte{byte('a' + n)}, sizes[n])
			if int(id) != n+1 || !bytes.Equal(v, want) {
				t.Fatalf("row %d: id %d, %d bytes; want id %d, %d bytes", n, id, len(v), n+1, len(want))
			}
			n++
		}
		must(t, rows.Err(), "rows")
		if n != len(sizes) {
			t.Fatalf("got %d rows, want %d", n, len(sizes))
		}
	})
}
