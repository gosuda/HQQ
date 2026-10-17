//go:build unix

package sqlite3_test

import (
	"fmt"
	"testing"

	"gosuda.org/hqq/bindgen/examples/sqlite3"
)

// seq fills a table t(id INTEGER PRIMARY KEY, name TEXT, score REAL) with n rows through one recursive CTE,
// which is how a client loads test data without n round trips.
func seq(t testing.TB, c *sqlite3.Client, db sqlite3.Db, n int) {
	t.Helper()
	run(t, c, db, "CREATE TABLE t (id INTEGER PRIMARY KEY, name TEXT, score REAL)")
	if n == 0 {
		return
	}
	run(t, c, db, `WITH RECURSIVE s(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM s WHERE i < ?)
		INSERT INTO t SELECT i, 'name-' || i, i * 0.5 FROM s`, n)
}

// TestFetchBatching reads tables of 0, 1, exactly one batch, one batch plus one, and several batches, and
// checks the sequence of (rows, rc) that fetch_rows returns. A batch that ends at its row limit answers
// SQLITE_ROW without looking ahead, so a table of exactly 256 rows ends with an empty batch.
func TestFetchBatching(t *testing.T) {
	type step struct {
		rows int
		rc   int32
	}
	row, done := sqlite3.SQLITE_ROW, sqlite3.SQLITE_DONE
	for _, tc := range []struct {
		n    int
		max  int32
		want []step
	}{
		{0, 0, []step{{0, done}}},
		{1, 0, []step{{1, done}}},
		{255, 0, []step{{255, done}}},
		{256, 0, []step{{256, row}, {0, done}}},
		{257, 0, []step{{256, row}, {1, done}}},
		{512, 0, []step{{256, row}, {256, row}, {0, done}}},
		{1000, 0, []step{{256, row}, {256, row}, {256, row}, {232, done}}},
		{5, 1, []step{{1, row}, {1, row}, {1, row}, {1, row}, {1, row}, {0, done}}},
		{5, 2, []step{{2, row}, {2, row}, {1, done}}},
		{300, -7, []step{{256, row}, {44, done}}}, // a negative count is the default
	} {
		t.Run(fmt.Sprintf("n=%d,max=%d", tc.n, tc.max), func(t *testing.T) {
			_, c, db := session(t)
			seq(t, c, db, tc.n)
			st := prep(t, c, db, "SELECT id, name, score FROM t ORDER BY id")
			next := int64(1)
			for i, want := range tc.want {
				b := fetch(t, c, st, tc.max)
				if len(b.rows) != want.rows || b.rc != want.rc {
					t.Fatalf("batch %d: %d rows, rc %d; want %d rows, rc %d", i, len(b.rows), b.rc, want.rows, want.rc)
				}
				for _, r := range b.rows {
					requireCell(t, r[0], next)
					requireCell(t, r[1], fmt.Sprintf("name-%d", next))
					requireCell(t, r[2], float64(next)*0.5)
					next++
				}
			}
			if next != int64(tc.n)+1 {
				t.Fatalf("read %d rows, want %d", next-1, tc.n)
			}
		})
	}
}

// TestFetchTypicalBatchFits checks the sizing claim of sqlite3.idl: a batch of 256 typical rows (an
// integer, a text of about 24 bytes and a float) is one fetch_rows call and fits the response bound
// of 64 KiB with room to spare, so a table of any length is read in batches of 256.
func TestFetchTypicalBatchFits(t *testing.T) {
	_, c, db := session(t)
	seq(t, c, db, 2560)
	st := prep(t, c, db, "SELECT id, name || '0123456789abcdef', score FROM t ORDER BY id")
	for i := range 10 {
		r, err := c.FetchRows(st, 256)
		if err != nil {
			t.Fatal(err)
		}
		if got := len(r.Cells) / int(r.Ncols); got != 256 {
			t.Fatalf("batch %d has %d rows, want 256", i, got)
		}
	}
}

// TestFetchByteBound reads rows of about 3,000 bytes: the bound of the response (64 KiB) ends a batch
// long before the row count does, and no row is lost between batches. It catches a shim that steps
// a row it cannot write.
func TestFetchByteBound(t *testing.T) {
	_, c, db := session(t)
	run(t, c, db, "CREATE TABLE r (id INTEGER PRIMARY KEY, a TEXT, b BLOB)")
	const n = 100
	for i := range n {
		run(t, c, db, "INSERT INTO r VALUES (?, ?, ?)", i, asciiText(1500, byte(i)), pattern(1500, byte(i)))
	}
	st := prep(t, c, db, "SELECT * FROM r ORDER BY id")
	next, batches := 0, 0
	for {
		b := fetch(t, c, st, 4096)
		batches++
		if len(b.rows) > 22 {
			t.Fatalf("a batch of %d rows of 3,000 bytes does not fit a 64 KiB bound", len(b.rows))
		}
		for _, r := range b.rows {
			requireCell(t, r[0], int64(next))
			requireCell(t, r[1], asciiText(1500, byte(next)))
			requireCell(t, r[2], pattern(1500, byte(next)))
			next++
		}
		if b.rc == sqlite3.SQLITE_DONE {
			break
		}
	}
	if next != n || batches < 5 {
		t.Fatalf("%d rows in %d batches, want %d rows in at least 5 batches", next, batches, n)
	}
}

// TestFetchMaxClamp checks that a count above the limit (4096) is clamped: a table of 5000 integers is
// read in batches of at most 4096 rows, and every row arrives.
func TestFetchMaxClamp(t *testing.T) {
	_, c, db := session(t)
	seq(t, c, db, 5000)
	st := prep(t, c, db, "SELECT id FROM t ORDER BY id")
	next, biggest := int64(1), 0
	for {
		b := fetch(t, c, st, 1<<30)
		if len(b.rows) > biggest {
			biggest = len(b.rows)
		}
		for _, r := range b.rows {
			requireCell(t, r[0], next)
			next++
		}
		if b.rc == sqlite3.SQLITE_DONE {
			break
		}
	}
	if next != 5001 {
		t.Fatalf("%d rows, want 5000", next-1)
	}
	// 16 bytes per integer cell: the batch ends at the response bound before the clamp when the
	// rows are this small.
	if biggest > int(sqlite3.SVC_MAX_FETCH_ROWS) || biggest < 3500 {
		t.Fatalf("largest batch %d rows, want 3500..%d", biggest, sqlite3.SVC_MAX_FETCH_ROWS)
	}
}

// TestFetchErrorAfterRows reads a result whose row 300 fails: the batches carry rows 1..299 and then the
// error, with its code and message, in the batch that ran into it. The hand-written server drops the rows
// of the failing batch; here they are delivered.
func TestFetchErrorAfterRows(t *testing.T) {
	_, c, db := session(t)
	st := prep(t, c, db, `WITH RECURSIVE c(x) AS (SELECT 1 UNION ALL SELECT x + 1 FROM c WHERE x < 1000)
		SELECT abs(CASE WHEN x = 300 THEN -9223372036854775807 - 1 ELSE x END) FROM c`)
	var got int
	var last batch
	for {
		last = fetch(t, c, st, 0)
		got += len(last.rows)
		if last.rc != sqlite3.SQLITE_ROW {
			break
		}
	}
	if last.rc == sqlite3.SQLITE_DONE || got != 299 {
		t.Fatalf("rc %d after %d rows, want an error after 299 rows", last.rc, got)
	}
	if last.rc != 1 || last.msg != "integer overflow" {
		t.Fatalf("error %d %q, want SQLITE_ERROR integer overflow", last.rc, last.msg)
	}
	// The statement reports the same error to finalize, which still frees it.
	err := c.Sqlite3Finalize(st)
	if le := libErr(t, err); le.Code != 1 {
		t.Fatalf("finalize: code %d, want 1", le.Code)
	}
	if err := c.Sqlite3Finalize(st); err == nil {
		t.Fatal("a second finalize of the consumed handle succeeded")
	}
}

// TestFetchAfterDoneRestarts documents the rule of sqlite3.idl: after SQLITE_DONE a further fetch_rows
// steps the statement again, which SQLite answers by running it from the start; sqlite3_reset first is
// the clean way to read a result twice.
func TestFetchAfterDoneRestarts(t *testing.T) {
	_, c, db := session(t)
	seq(t, c, db, 3)
	st := prep(t, c, db, "SELECT id FROM t ORDER BY id")
	if b := fetch(t, c, st, 0); len(b.rows) != 3 || b.rc != sqlite3.SQLITE_DONE {
		t.Fatalf("first read: %d rows, rc %d", len(b.rows), b.rc)
	}
	if err := c.Sqlite3Reset(st); err != nil {
		t.Fatal(err)
	}
	if b := fetch(t, c, st, 0); len(b.rows) != 3 || b.rc != sqlite3.SQLITE_DONE {
		t.Fatalf("read after reset: %d rows, rc %d", len(b.rows), b.rc)
	}
	// No reset: SQLite restarts the statement by itself.
	if b := fetch(t, c, st, 0); len(b.rows) != 3 || b.rc != sqlite3.SQLITE_DONE {
		t.Fatalf("read after done: %d rows, rc %d", len(b.rows), b.rc)
	}
}

// TestFetchStatementWithoutColumns runs an INSERT through fetch_rows: no columns, no rows, SQLITE_DONE,
// and the statement did run.
func TestFetchStatementWithoutColumns(t *testing.T) {
	_, c, db := session(t)
	seq(t, c, db, 0)
	st := prep(t, c, db, "INSERT INTO t VALUES (1, 'x', 1.0)")
	b := fetch(t, c, st, 0)
	if b.rc != sqlite3.SQLITE_DONE || len(b.rows) != 0 {
		t.Fatalf("rc %d, %d rows", b.rc, len(b.rows))
	}
	if n := one(t, c, db, "SELECT count(*) FROM t"); n != int64(1) {
		t.Fatalf("count = %v, want 1", n)
	}
}

// TestFetchPrefetch issues the next fetch_rows right after it decoded the previous batch, before the rows
// are used, which is the pipelining that is safe (BigText and BigBlob cells of the last row are read
// first). It reads 3,000 rows and checks the order.
func TestFetchPrefetch(t *testing.T) {
	_, c, db := session(t)
	seq(t, c, db, 3000)
	st := prep(t, c, db, "SELECT id, name FROM t ORDER BY id")
	next := int64(1)
	fut, err := c.FetchRowsAsync(st, 256)
	if err != nil {
		t.Fatal(err)
	}
	for {
		r, err := fut.Wait()
		if err != nil {
			t.Fatal(err)
		}
		more := r.Rc == sqlite3.SQLITE_ROW
		if more {
			if fut, err = c.FetchRowsAsync(st, 256); err != nil {
				t.Fatal(err)
			}
		}
		b := decodeBatch(t, c, st, r)
		for _, row := range b.rows {
			requireCell(t, row[0], next)
			next++
		}
		if !more {
			break
		}
	}
	if next != 3001 {
		t.Fatalf("%d rows, want 3000", next-1)
	}
}

// TestPipelinedFetchStepsOverBigRow documents the hazard that the prefetch rule exists for: a fetch_rows that
// was issued before the previous response was read steps the statement past a row whose big cell has not
// been read yet, and column_chunk then reads the cell of the row the statement is on now. The server has
// no state between calls to prevent it (sqlite3.idl, fetch_rows). The driver must not pipeline fetch_rows
// ahead of a response that may end in a marker.
func TestPipelinedFetchStepsOverBigRow(t *testing.T) {
	_, c, db := session(t)
	run(t, c, db, "CREATE TABLE h (id INTEGER PRIMARY KEY, b BLOB)")
	run(t, c, db, "INSERT INTO h VALUES (1, ?)", pattern(100000, 1))
	run(t, c, db, "INSERT INTO h VALUES (2, ?)", pattern(100000, 2))
	st := prep(t, c, db, "SELECT b FROM h ORDER BY id")
	f1, err := c.FetchRowsAsync(st, 0)
	if err != nil {
		t.Fatal(err)
	}
	f2, err := c.FetchRowsAsync(st, 0) // issued before f1 was read
	if err != nil {
		t.Fatal(err)
	}
	r1, err := f1.Wait()
	if err != nil {
		t.Fatal(err)
	}
	if len(r1.Cells) != 1 || r1.Cells[0].Tag != sqlite3.ValueBigBlob {
		t.Fatalf("first batch: %+v, want one big cell", r1.Cells)
	}
	r2, err := f2.Wait()
	if err != nil {
		t.Fatal(err)
	}
	if len(r2.Cells) != 1 || r2.Cells[0].Tag != sqlite3.ValueBigBlob {
		t.Fatalf("second batch: %+v, want the second row as a big cell", r2.Cells)
	}
	// The chunk of "row 1" is the data of row 2 (or an error): the statement has moved on.
	got := readChunks(t, c, st, 0, 100000)
	if string(got) == string(pattern(100000, 1)) {
		t.Fatal("column_chunk returned row 1 after the statement moved to row 2: the hazard documented in sqlite3.idl is gone, update it")
	}
}
