//go:build unix

package sqlite3_test

import (
	"fmt"
	"testing"

	"gosuda.org/hqq/bindgen/examples/sqlite3"
	"gosuda.org/hqq/bindgen/examples/sqlite3/sqlite3srv"
	"gosuda.org/hqq/bindgen/rt"
)

// TestPipelinedBindsAndStep issues, for each row, one bind per parameter, a reset, a step, changes64
// and last_insert_rowid as futures, without waiting between them, and then waits for all of them in
// order: 25 calls per row in flight at once with 20 parameters. It checks the flow control of the
// runtime (more calls than the 64 slots of the ring), the order of execution, and the results.
func TestPipelinedBindsAndStep(t *testing.T) {
	_, c, db := session(t)
	const ncols, nrows = 20, 120
	var cols, marks []string
	for i := range ncols {
		cols = append(cols, fmt.Sprintf("c%d INTEGER", i))
		marks = append(marks, "?")
	}
	run(t, c, db, "CREATE TABLE p (id INTEGER PRIMARY KEY, "+joinComma(cols)+")")
	st := prep(t, c, db, "INSERT INTO p VALUES (NULL, "+joinComma(marks)+")")

	type row struct {
		binds   []sqlite3.Sqlite3BindInt64Future
		step    sqlite3.Sqlite3StepFuture
		changes sqlite3.Sqlite3Changes64Future
		rowid   sqlite3.Sqlite3LastInsertRowidFuture
	}
	for r := range nrows {
		var f row
		reset, err := c.Sqlite3ResetAsync(st)
		if err != nil {
			t.Fatal(err)
		}
		for i := range ncols {
			b, err := c.Sqlite3BindInt64Async(st, int32(i+1), int64(r*100+i))
			if err != nil {
				t.Fatal(err)
			}
			f.binds = append(f.binds, b)
		}
		if f.step, err = c.Sqlite3StepAsync(st); err != nil {
			t.Fatal(err)
		}
		if f.changes, err = c.Sqlite3Changes64Async(db); err != nil {
			t.Fatal(err)
		}
		if f.rowid, err = c.Sqlite3LastInsertRowidAsync(db); err != nil {
			t.Fatal(err)
		}
		if err := reset.Wait(); err != nil {
			t.Fatalf("row %d reset: %v", r, err)
		}
		for i, b := range f.binds {
			if err := b.Wait(); err != nil {
				t.Fatalf("row %d bind %d: %v", r, i, err)
			}
		}
		if rc, err := f.step.Wait(); err != nil || rc != sqlite3.SQLITE_DONE {
			t.Fatalf("row %d step: %d, %v", r, rc, err)
		}
		if n, err := f.changes.Wait(); err != nil || n != 1 {
			t.Fatalf("row %d changes: %d, %v", r, n, err)
		}
		if id, err := f.rowid.Wait(); err != nil || id != int64(r+1) {
			t.Fatalf("row %d rowid: %d, %v", r, id, err)
		}
	}
	rows := queryAll(t, c, db, 0, "SELECT * FROM p ORDER BY id")
	if len(rows) != nrows {
		t.Fatalf("%d rows, want %d", len(rows), nrows)
	}
	for r, row := range rows {
		for i := range ncols {
			requireCell(t, row[1+i], int64(r*100+i))
		}
	}
}

// TestPipelinedBindFailureDoesNotAbortStep documents a cost of pipelined binds: the server executes every
// request that was committed, so a bind that fails (here: an index out of range) does not stop the step
// that was issued behind it, and the statement runs with the parameter unbound, that is NULL. exec_stmt
// and bind_values exist to avoid this: they check everything before they bind and bind before they run.
func TestPipelinedBindFailureDoesNotAbortStep(t *testing.T) {
	_, c, db := session(t)
	run(t, c, db, "CREATE TABLE f (a INTEGER NOT NULL DEFAULT 0, b)")
	st := prep(t, c, db, "INSERT INTO f VALUES (?, ?)")
	b1, _ := c.Sqlite3BindInt64Async(st, 1, 11)
	bad, _ := c.Sqlite3BindInt64Async(st, 9, 99) // SQLITE_RANGE
	b2, _ := c.Sqlite3BindInt64Async(st, 2, 22)
	step, _ := c.Sqlite3StepAsync(st)
	if err := b1.Wait(); err != nil {
		t.Fatal(err)
	}
	if le := libErr(t, bad.Wait()); le.Code != codeRange {
		t.Fatalf("the bad bind: code %d", le.Code)
	}
	if err := b2.Wait(); err != nil {
		t.Fatal(err)
	}
	if rc, err := step.Wait(); err != nil || rc != sqlite3.SQLITE_DONE {
		t.Fatalf("the step behind the failed bind: %d, %v", rc, err)
	}
	if n := one(t, c, db, "SELECT count(*) FROM f WHERE a = 11 AND b = 22"); n != int64(1) {
		t.Fatalf("the row that the pipelined step wrote: %v", n)
	}
	// The same arguments through exec_stmt: the failure comes first and nothing runs.
	k, w, d := pack(11, 22, 99)
	if _, _, err := c.ExecStmt(st, k, w, d); libErr(t, err).Code != codeRange {
		t.Fatalf("exec_stmt with an extra value: %v", err)
	}
	if n := one(t, c, db, "SELECT count(*) FROM f"); n != int64(1) {
		t.Fatalf("exec_stmt ran a statement with a failed bind: %v rows", n)
	}
}

// TestPipelinedChunkReads reads one 3 MiB cell with all its column_chunk calls in flight at once. Reads do not
// move the statement, so they may be pipelined; the flow control keeps the number of calls in flight
// within the ring. It checks the reassembly and the ownership of each destination slice.
func TestPipelinedChunkReads(t *testing.T) {
	_, c, db := session(t)
	run(t, c, db, "CREATE TABLE big (b BLOB)")
	want := pattern(3<<20, 5)
	run(t, c, db, "INSERT INTO big VALUES (?)", want)
	st := prep(t, c, db, "SELECT b FROM big")
	r, err := c.FetchRows(st, 0)
	if err != nil || len(r.Cells) != 1 || r.Cells[0].Tag != sqlite3.ValueBigBlob {
		t.Fatalf("fetch: %+v, %v", r.Cells, err)
	}
	total := int(r.Cells[0].BigBlob)
	const piece = 128 << 10
	var futs []sqlite3.ColumnChunkFuture
	for off := 0; off < total; off += piece {
		f, err := c.ColumnChunkAsync(st, 0, uint64(off), make([]byte, piece))
		if err != nil {
			t.Fatal(err)
		}
		futs = append(futs, f)
	}
	var got []byte
	for i, f := range futs {
		b, err := f.Wait()
		if err != nil {
			t.Fatalf("chunk %d: %v", i, err)
		}
		got = append(got, b...)
	}
	requireCell(t, got, want)
	// Past the end: no bytes, no error; beyond the end: SQLITE_RANGE.
	if b, err := c.ColumnChunk(st, 0, uint64(total), make([]byte, 10)); err != nil || len(b) != 0 {
		t.Fatalf("read at the end: %d bytes, %v", len(b), err)
	}
	if _, err := c.ColumnChunk(st, 0, uint64(total)+1, make([]byte, 10)); libErr(t, err).Code != codeRange {
		t.Fatalf("read beyond the end: %v", err)
	}
	if _, err := c.ColumnChunk(st, 1, 0, make([]byte, 10)); libErr(t, err).Code != codeRange {
		t.Fatalf("read of a column that does not exist: %v", err)
	}
	// A cell that is not text or blob.
	st2 := prep(t, c, db, "SELECT 42")
	if _, err := c.Sqlite3Step(st2); err != nil {
		t.Fatal(err)
	}
	if _, err := c.ColumnChunk(st2, 0, 0, make([]byte, 10)); libErr(t, err).Code != codeRange {
		t.Fatalf("read of an integer: %v", err)
	}
	// A statement that is not on a row.
	st3 := prep(t, c, db, "SELECT b FROM big")
	if _, err := c.ColumnChunk(st3, 0, 0, make([]byte, 10)); libErr(t, err).Code != codeRange {
		t.Fatalf("read before the first step: %v", err)
	}
}

// TestSmallGeometry runs the service over tiny and small frames. A response bound above the number of
// ring slots (fetch_rows needs 17 slots at 4 KiB frames, stmt_info 257) makes the call synchronous
// (BINDGEN.md 4.12), and every large message crosses many CONT frames; none of that may change a result.
func TestSmallGeometry(t *testing.T) {
	for _, g := range []struct{ count, size int }{{4, 4096}, {2, 4096}, {2, 256}, {128, 1024}} {
		t.Run(fmt.Sprintf("B=%d,S=%d", g.count, g.size), func(t *testing.T) {
			srv := sqlite3srv.Start(t, sqlite3srv.Config{})
			c := srv.Dial(func(o *rt.Options) { o.BufferCount, o.BufferSize = g.count, g.size })
			db := open(t, c, "geo.db")
			fast(t, c, db)
			run(t, c, db, "CREATE TABLE g (id INTEGER PRIMARY KEY, t TEXT, b BLOB)")
			blob := pattern(150000, 1)
			text := asciiText(90000, 2)
			for i := range 30 {
				run(t, c, db, "INSERT INTO g VALUES (?, ?, ?)", i, fmt.Sprintf("row %d", i), pattern(100+i*7, byte(i)))
			}
			run(t, c, db, "INSERT INTO g VALUES (100, ?, ?)", text, blob)
			rows := queryAll(t, c, db, 0, "SELECT id, t, b FROM g ORDER BY id")
			if len(rows) != 31 {
				t.Fatalf("%d rows", len(rows))
			}
			for i := range 30 {
				requireCell(t, rows[i][1], fmt.Sprintf("row %d", i))
				requireCell(t, rows[i][2], pattern(100+i*7, byte(i)))
			}
			requireCell(t, rows[30][1], text)
			requireCell(t, rows[30][2], blob)
			st := prep(t, c, db, "SELECT id, t FROM g WHERE id > ? AND t <> ?")
			info, err := c.StmtInfo(st)
			if err != nil || info.Nparams != 2 || len(info.Columns) != 2 {
				t.Fatalf("stmt_info: %+v, %v", info, err)
			}
			// More small calls in flight than ring slots.
			var futs []sqlite3.Sqlite3GetAutocommitFuture
			for range 300 {
				f, err := c.Sqlite3GetAutocommitAsync(db)
				if err != nil {
					t.Fatal(err)
				}
				futs = append(futs, f)
			}
			for _, f := range futs {
				if v, err := f.Wait(); err != nil || v != 1 {
					t.Fatalf("get_autocommit: %d, %v", v, err)
				}
			}
		})
	}
}
