//go:build unix

package sqlite3_test

import (
	"bytes"
	"errors"
	"fmt"
	"math"
	"strings"
	"testing"

	"gosuda.org/hqq/bindgen/examples/sqlite3"
	"gosuda.org/hqq/bindgen/rt"
)

// zeroblob is an argument that binds zeroblob(N).
type zeroblob int

// textRaw is an argument that binds text from bytes (the text may hold NUL bytes).
type textRaw []byte

// pack turns Go values into the three arrays of bind_values and exec_stmt: nil, integers, float64, string
// (text), []byte (blob), textRaw (text) and zeroblob.
func pack(args ...any) (kinds []uint8, words []uint64, data []byte) {
	for _, a := range args {
		switch v := a.(type) {
		case nil:
			kinds, words = append(kinds, 5), append(words, 0)
		case int:
			kinds, words = append(kinds, 1), append(words, uint64(int64(v)))
		case int64:
			kinds, words = append(kinds, 1), append(words, uint64(v))
		case bool:
			var n uint64
			if v {
				n = 1
			}
			kinds, words = append(kinds, 1), append(words, n)
		case float64:
			kinds, words = append(kinds, 2), append(words, math.Float64bits(v))
		case string:
			kinds, words, data = append(kinds, 3), append(words, uint64(len(v))), append(data, v...)
		case textRaw:
			kinds, words, data = append(kinds, 3), append(words, uint64(len(v))), append(data, v...)
		case []byte:
			kinds, words, data = append(kinds, 4), append(words, uint64(len(v))), append(data, v...)
		case zeroblob:
			kinds, words = append(kinds, 6), append(words, uint64(v))
		default:
			panic(fmt.Sprintf("pack: unsupported argument %T", a))
		}
	}
	return
}

// open opens (and creates) a database file in the allow directory of the server.
func open(t testing.TB, c *sqlite3.Client, name string) sqlite3.Db {
	t.Helper()
	db, err := c.Sqlite3OpenV2(name, sqlite3.SQLITE_OPEN_READWRITE|sqlite3.SQLITE_OPEN_CREATE, nil)
	if err != nil {
		t.Fatalf("open %q: %v", name, err)
	}
	return db
}

// prep prepares exactly one statement.
func prep(t testing.TB, c *sqlite3.Client, db sqlite3.Db, sql string) sqlite3.Stmt {
	t.Helper()
	st, err := c.PrepareOne(db, sql)
	if err != nil {
		t.Fatalf("prepare %q: %v", abbrev(sql), err)
	}
	return st
}

func abbrev(s string) string {
	if len(s) > 80 {
		return s[:80] + "..."
	}
	return s
}

func finalize(t testing.TB, c *sqlite3.Client, st sqlite3.Stmt) {
	t.Helper()
	if err := c.Sqlite3Finalize(st); err != nil {
		t.Fatalf("finalize: %v", err)
	}
}

// run executes one statement with ExecStmt and returns changes and the last rowid.
func run(t testing.TB, c *sqlite3.Client, db sqlite3.Db, sql string, args ...any) (changes, rowid int64) {
	t.Helper()
	st := prep(t, c, db, sql)
	k, w, d := pack(args...)
	changes, rowid, err := c.ExecStmt(st, k, w, d)
	if err != nil {
		t.Fatalf("exec %q: %v", abbrev(sql), err)
	}
	finalize(t, c, st)
	return changes, rowid
}

// libErr returns the library error in err, or fails the test.
func libErr(t testing.TB, err error) *rt.LibError {
	t.Helper()
	var le *rt.LibError
	if !errors.As(err, &le) {
		t.Fatalf("error %v (%T) is not a library error", err, err)
	}
	return le
}

func statusErr(t testing.TB, err error) *rt.StatusError {
	t.Helper()
	var se *rt.StatusError
	if !errors.As(err, &se) {
		t.Fatalf("error %v (%T) is not a status error", err, err)
	}
	return se
}

// cell is a normalized result value: int64, float64, string, []byte or nil. A TEXT value is a string and
// a BLOB a []byte, so that the types of a result can be compared.
type cell = any

// readCell turns a Value into a cell. A BigText or BigBlob cell is read with column_chunk, which needs
// the statement to be positioned on that row, so a caller reads the cells of a batch's last row before the
// next fetch.
func readCell(t testing.TB, c *sqlite3.Client, st sqlite3.Stmt, col int, v sqlite3.Value) cell {
	t.Helper()
	switch v.Tag {
	case sqlite3.ValueInt:
		return v.Int
	case sqlite3.ValueFloat:
		return v.Float
	case sqlite3.ValueText:
		return v.Text
	case sqlite3.ValueTextRaw:
		return string(v.TextRaw)
	case sqlite3.ValueBlob:
		return append([]byte{}, v.Blob...)
	case sqlite3.ValueNull:
		return nil
	case sqlite3.ValueBigText:
		return string(readChunks(t, c, st, col, v.BigText))
	case sqlite3.ValueBigBlob:
		return readChunks(t, c, st, col, v.BigBlob)
	}
	t.Fatalf("unknown value tag %d", v.Tag)
	return nil
}

// readChunks reads n bytes of a cell of the current row, in pieces of up to 256 KiB.
func readChunks(t testing.TB, c *sqlite3.Client, st sqlite3.Stmt, col int, n uint64) []byte {
	t.Helper()
	out := make([]byte, 0, n)
	buf := make([]byte, 256<<10)
	for uint64(len(out)) < n {
		got, err := c.ColumnChunk(st, int32(col), uint64(len(out)), buf)
		if err != nil {
			t.Fatalf("column_chunk col %d at %d: %v", col, len(out), err)
		}
		if len(got) == 0 {
			t.Fatalf("column_chunk col %d returned no bytes at %d of %d", col, len(out), n)
		}
		out = append(out, got...)
	}
	return out
}

// batch is one decoded fetch_rows result.
type batch struct {
	rc   int32
	rows [][]cell
	msg  string
}

// fetch runs fetch_rows once and decodes it, reading the big cells of the last row.
func fetch(t testing.TB, c *sqlite3.Client, st sqlite3.Stmt, max int32) batch {
	t.Helper()
	r, err := c.FetchRows(st, max)
	if err != nil {
		t.Fatalf("fetch_rows: %v", err)
	}
	return decodeBatch(t, c, st, r)
}

func decodeBatch(t testing.TB, c *sqlite3.Client, st sqlite3.Stmt, r sqlite3.Rows) batch {
	t.Helper()
	b := batch{rc: r.Rc, msg: r.Errmsg}
	n := int(r.Ncols)
	if n == 0 {
		if len(r.Cells) != 0 {
			t.Fatalf("%d cells for a statement without columns", len(r.Cells))
		}
		return b
	}
	if len(r.Cells)%n != 0 {
		t.Fatalf("%d cells for %d columns", len(r.Cells), n)
	}
	for i := 0; i < len(r.Cells); i += n {
		row := make([]cell, n)
		for j := range n {
			v := r.Cells[i+j]
			if (v.Tag == sqlite3.ValueBigText || v.Tag == sqlite3.ValueBigBlob) && i+n != len(r.Cells) {
				t.Fatalf("a big cell in row %d, which is not the last row of the batch", i/n)
			}
			row[j] = readCell(t, c, st, j, v)
		}
		b.rows = append(b.rows, row)
	}
	return b
}

// queryAll runs a query to the end with the given batch size and returns every row.
func queryAll(t testing.TB, c *sqlite3.Client, db sqlite3.Db, max int32, sql string, args ...any) [][]cell {
	t.Helper()
	st := prep(t, c, db, sql)
	defer finalize(t, c, st)
	if len(args) > 0 {
		k, w, d := pack(args...)
		if err := c.BindValues(st, k, w, d); err != nil {
			t.Fatalf("bind %q: %v", abbrev(sql), err)
		}
	}
	return drain(t, c, st, max)
}

// drain fetches batches until the statement is done and returns the rows.
func drain(t testing.TB, c *sqlite3.Client, st sqlite3.Stmt, max int32) [][]cell {
	t.Helper()
	var rows [][]cell
	for {
		b := fetch(t, c, st, max)
		rows = append(rows, b.rows...)
		switch b.rc {
		case sqlite3.SQLITE_ROW:
		case sqlite3.SQLITE_DONE:
			return rows
		default:
			t.Fatalf("fetch_rows ended with code %d: %s", b.rc, b.msg)
		}
	}
}

// one runs a query that returns one row of one column and returns the cell.
func one(t testing.TB, c *sqlite3.Client, db sqlite3.Db, sql string, args ...any) cell {
	t.Helper()
	rows := queryAll(t, c, db, 0, sql, args...)
	if len(rows) != 1 || len(rows[0]) != 1 {
		t.Fatalf("%q returned %v, want one row of one column", abbrev(sql), rows)
	}
	return rows[0][0]
}

// equalCell compares two cells; a float is equal when its bits are.
func equalCell(a, b cell) bool {
	switch x := a.(type) {
	case []byte:
		y, ok := b.([]byte)
		return ok && bytes.Equal(x, y)
	case float64:
		y, ok := b.(float64)
		return ok && math.Float64bits(x) == math.Float64bits(y)
	default:
		return a == b
	}
}

func describe(v cell) string {
	switch x := v.(type) {
	case string:
		if len(x) > 40 {
			return fmt.Sprintf("string(%d bytes %q...)", len(x), x[:20])
		}
	case []byte:
		if len(x) > 40 {
			return fmt.Sprintf("blob(%d bytes)", len(x))
		}
	}
	return fmt.Sprintf("%T(%v)", v, v)
}

func requireCell(t testing.TB, got, want cell) {
	t.Helper()
	if !equalCell(got, want) {
		t.Fatalf("got %s, want %s", describe(got), describe(want))
	}
}

// pattern returns n deterministic bytes.
func pattern(n int, seed byte) []byte {
	b := make([]byte, n)
	x := uint32(seed)*2654435761 + 1
	for i := range b {
		x = x*1664525 + 1013904223
		b[i] = byte(x >> 24)
	}
	return b
}

// asciiText returns n bytes of text without NUL.
func asciiText(n int, seed byte) string {
	var sb strings.Builder
	sb.Grow(n)
	for i := range n {
		sb.WriteByte('a' + byte((i*7+int(seed))%26))
	}
	return sb.String()
}

// fast turns off the syncs of a test database: the tests check behavior, not durability, and a commit
// that waits for the disk takes about half a second on some hosts.
func fast(t testing.TB, c *sqlite3.Client, db sqlite3.Db) {
	t.Helper()
	run(t, c, db, "PRAGMA synchronous = OFF")
}
