//go:build unix

package sqlite3_test

import (
	"bytes"
	"errors"
	"fmt"
	"math"
	"testing"

	"gosuda.org/hqq/bindgen/examples/sqlite3"
	"gosuda.org/hqq/bindgen/examples/sqlite3/sqlite3srv"
	"gosuda.org/hqq/bindgen/rt"
)

// session starts a server and one session with a fresh database "t.db" open on it.
func session(t *testing.T) (*sqlite3srv.Server, *sqlite3.Client, sqlite3.Db) {
	t.Helper()
	srv := sqlite3srv.Start(t, sqlite3srv.Config{})
	c := srv.Dial()
	db := open(t, c, "t.db")
	fast(t, c, db)
	return srv, c, db
}

// TestValueRoundTrip stores every kind of value through bind_values and reads it back through fetch_rows,
// comparing the Go value and the storage class that SQLite reports (typeof). It catches a wrong tag, a
// lost NUL, a text that comes back as a blob, an empty text that turns into NULL, and an alignment
// slip in the Value union (the cells are written at every alignment because the columns differ in size).
func TestValueRoundTrip(t *testing.T) {
	_, c, db := session(t)
	run(t, c, db, "CREATE TABLE v (id INTEGER PRIMARY KEY, a, b, c)")

	cases := []struct {
		name string
		v    any
		want cell
		typ  string
	}{
		{"zero", 0, int64(0), "integer"},
		{"minus one", -1, int64(-1), "integer"},
		{"max int64", int64(math.MaxInt64), int64(math.MaxInt64), "integer"},
		{"min int64", int64(math.MinInt64), int64(math.MinInt64), "integer"},
		{"float", 1.5, 1.5, "real"},
		{"negative zero", math.Copysign(0, -1), math.Copysign(0, -1), "real"},
		{"+Inf", math.Inf(1), math.Inf(1), "real"},
		{"-Inf", math.Inf(-1), math.Inf(-1), "real"},
		{"smallest denormal", 5e-324, 5e-324, "real"},
		{"largest float", math.MaxFloat64, math.MaxFloat64, "real"},
		{"NaN is stored as NULL", math.NaN(), nil, "null"},
		{"null", nil, nil, "null"},
		{"empty text", "", "", "text"},
		{"text", "hello", "hello", "text"},
		{"unicode text", "é世界🙂", "é世界🙂", "text"},
		{"text with NUL", textRaw("a\x00b"), "a\x00b", "text"},
		{"text of NULs", textRaw("\x00\x00\x00"), "\x00\x00\x00", "text"},
		{"text that is not UTF-8", textRaw("\xff\xfe\x80"), "\xff\xfe\x80", "text"},
		{"empty blob", []byte{}, []byte{}, "blob"},
		{"blob", []byte{0, 1, 2, 255}, []byte{0, 1, 2, 255}, "blob"},
		{"all byte values", pattern(256, 3), pattern(256, 3), "blob"},
		{"zeroblob", zeroblob(10), make([]byte, 10), "blob"},
		{"zeroblob(0)", zeroblob(0), []byte{}, "blob"},
	}
	// Three columns per row, so that cells start at different alignments: each case is stored in column a, b
	// or c, with fillers in the others.
	for i, tc := range cases {
		var args []any
		switch i % 3 {
		case 0:
			args = []any{i, tc.v, "pad", int64(7)}
		case 1:
			args = []any{i, "pad", tc.v, int64(7)}
		case 2:
			args = []any{i, 1, int64(7), tc.v}
		}
		run(t, c, db, "INSERT INTO v (id, a, b, c) VALUES (?, ?, ?, ?)", args...)
	}
	rows := queryAll(t, c, db, 0, "SELECT id, a, b, c, typeof(a), typeof(b), typeof(c) FROM v ORDER BY id")
	if len(rows) != len(cases) {
		t.Fatalf("%d rows, want %d", len(rows), len(cases))
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			row := rows[i]
			requireCell(t, row[1+i%3], tc.want)
			if got := row[4+i%3]; got != tc.typ {
				t.Fatalf("typeof = %v, want %s", got, tc.typ)
			}
		})
	}
}

// TestBindPathsAgree binds the same values one at a time (bind_int64, bind_double, bind_null, bind_text,
// bind_blob, bind_zeroblob64), with bind_values, and with exec_stmt, and checks that the three rows are
// equal. It catches a path that disagrees with another about the type or the bytes of a value.
func TestBindPathsAgree(t *testing.T) {
	_, c, db := session(t)
	run(t, c, db, "CREATE TABLE b (id INTEGER PRIMARY KEY, i, d, n, t, x, z, e)")
	const ins = "INSERT INTO b VALUES (?, ?, ?, ?, ?, ?, ?, ?)"
	vals := []any{int64(-42), 2.25, nil, "text\x00with nul", pattern(100, 9), zeroblob(7), []byte{}}

	// 1: one call per parameter (the cost of pipelined binds, here synchronous).
	st := prep(t, c, db, ins)
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(c.Sqlite3BindInt64(st, 1, 1))
	must(c.Sqlite3BindInt64(st, 2, -42))
	must(c.Sqlite3BindDouble(st, 3, 2.25))
	must(c.Sqlite3BindNull(st, 4))
	must(c.BindText(st, 5, []byte("text\x00with nul")))
	must(c.BindBlob(st, 6, pattern(100, 9)))
	must(c.Sqlite3BindZeroblob64(st, 7, 7))
	must(c.BindBlob(st, 8, []byte{}))
	if rc, err := c.Sqlite3Step(st); err != nil || rc != sqlite3.SQLITE_DONE {
		t.Fatalf("step: %d, %v", rc, err)
	}
	must(c.Sqlite3Reset(st))
	must(c.Sqlite3ClearBindings(st))

	// 2: bind_values, then step.
	k, w, d := pack(append([]any{2}, vals...)...)
	must(c.BindValues(st, k, w, d))
	if rc, err := c.Sqlite3Step(st); err != nil || rc != sqlite3.SQLITE_DONE {
		t.Fatalf("step: %d, %v", rc, err)
	}
	finalize(t, c, st)

	// 3: exec_stmt.
	if ch, _ := run(t, c, db, ins, append([]any{3}, vals...)...); ch != 1 {
		t.Fatalf("changes = %d, want 1", ch)
	}

	rows := queryAll(t, c, db, 0, "SELECT i, d, n, t, x, z, e, typeof(t), typeof(x), typeof(e) FROM b ORDER BY id")
	if len(rows) != 3 {
		t.Fatalf("%d rows, want 3", len(rows))
	}
	for i := 1; i < 3; i++ {
		for j := range rows[0] {
			if !equalCell(rows[0][j], rows[i][j]) {
				t.Fatalf("row %d column %d: %s differs from the one-by-one binding %s",
					i, j, describe(rows[i][j]), describe(rows[0][j]))
			}
		}
	}
	want := []cell{int64(-42), 2.25, nil, "text\x00with nul", pattern(100, 9), make([]byte, 7), []byte{}, "text", "blob", "blob"}
	for j, w := range want {
		requireCell(t, rows[0][j], w)
	}
}

// TestLargeValues round-trips text and blobs around the frame size (a request or a response of more than
// 16,368 body bytes continues in CONT frames) and around the batch bound (a value of about 64 KiB or more
// is not inlined in a batch: it comes back as a BigText or BigBlob marker and is read in pieces).
func TestLargeValues(t *testing.T) {
	_, c, db := session(t)
	run(t, c, db, "CREATE TABLE l (id INTEGER PRIMARY KEY, t TEXT, b BLOB)")
	sizes := []int{1, 16000, 16356, 16368, 16384, 40000, 50000, 60000, 64000, 65000, 65535, 65536, 70000,
		200000, 1 << 20, 3 << 20, 7 << 20}
	for i, n := range sizes {
		text := asciiText(n, byte(i))
		blob := pattern(n, byte(i))
		run(t, c, db, "INSERT INTO l VALUES (?, ?, ?)", i, text, blob)
	}
	for i, n := range sizes {
		t.Run(fmt.Sprint(n), func(t *testing.T) {
			rows := queryAll(t, c, db, 0, "SELECT id, t, b, length(t), length(b) FROM l WHERE id = ?", i)
			if len(rows) != 1 {
				t.Fatalf("%d rows", len(rows))
			}
			requireCell(t, rows[0][0], int64(i))
			requireCell(t, rows[0][1], asciiText(n, byte(i)))
			requireCell(t, rows[0][2], pattern(n, byte(i)))
			requireCell(t, rows[0][3], int64(n))
			requireCell(t, rows[0][4], int64(n))
		})
	}

	// Which sizes are inlined and which are markers: the first row of a batch has the whole bound.
	for _, tc := range []struct {
		n   int
		big bool
	}{{16000, false}, {40000, false}, {70000, true}, {1 << 20, true}} {
		st := prep(t, c, db, "SELECT b FROM l WHERE length(b) = ?")
		k, w, d := pack(tc.n)
		if err := c.BindValues(st, k, w, d); err != nil {
			t.Fatal(err)
		}
		r, err := c.FetchRows(st, 0)
		if err != nil {
			t.Fatal(err)
		}
		if len(r.Cells) != 1 {
			t.Fatalf("size %d: %d cells", tc.n, len(r.Cells))
		}
		isBig := r.Cells[0].Tag == sqlite3.ValueBigBlob
		if isBig != tc.big {
			t.Fatalf("size %d: tag %d, big = %v, want %v", tc.n, r.Cells[0].Tag, isBig, tc.big)
		}
		if isBig && r.Cells[0].BigBlob != uint64(tc.n) {
			t.Fatalf("size %d: the marker says %d bytes", tc.n, r.Cells[0].BigBlob)
		}
		finalize(t, c, st)
	}
}

// TestBigCellsInManyRows reads a result whose rows hold big cells and small cells in several columns.
// A batch ends after a row that has a big cell, so rows after it arrive in later batches, in order. It
// catches a batch that steps over a big row (a lost row) and a chunk read at the wrong position.
func TestBigCellsInManyRows(t *testing.T) {
	_, c, db := session(t)
	run(t, c, db, "CREATE TABLE m (id INTEGER PRIMARY KEY, a TEXT, b BLOB, c INTEGER)")
	const rows = 40
	sizeOf := func(i int) int { // alternate small, medium and big values; some rows have none
		switch i % 5 {
		case 0:
			return 10
		case 1:
			return 30000
		case 2:
			return 100000
		case 3:
			return 0
		}
		return 300000
	}
	for i := range rows {
		n := sizeOf(i)
		run(t, c, db, "INSERT INTO m VALUES (?, ?, ?, ?)", i, asciiText(n, byte(i)), pattern(n, byte(i)), int64(i)*3)
	}
	got := queryAll(t, c, db, 0, "SELECT id, a, b, c FROM m ORDER BY id")
	if len(got) != rows {
		t.Fatalf("%d rows, want %d", len(got), rows)
	}
	for i, row := range got {
		n := sizeOf(i)
		requireCell(t, row[0], int64(i))
		requireCell(t, row[1], asciiText(n, byte(i)))
		requireCell(t, row[2], pattern(n, byte(i)))
		requireCell(t, row[3], int64(i)*3)
	}
}

// TestBigRowAndBatchLimit sends rows so wide that only a few fit a batch, with every cell inlined, and
// then rows with 40 big cells each: the markers of a row always fit, whatever the cells are.
func TestBigRowAndBatchLimit(t *testing.T) {
	_, c, db := session(t)
	// 40 columns of 3,000 bytes: 120 KB per row, more than the batch bound, so cells become markers.
	var cols, marks []string
	var args []any
	for i := range 40 {
		cols = append(cols, fmt.Sprintf("c%d", i))
		marks = append(marks, "?")
		args = append(args, asciiText(3000, byte(i)))
	}
	run(t, c, db, "CREATE TABLE w ("+joinComma(cols)+")")
	for range 3 {
		run(t, c, db, "INSERT INTO w VALUES ("+joinComma(marks)+")", args...)
	}
	rows := queryAll(t, c, db, 0, "SELECT * FROM w")
	if len(rows) != 3 {
		t.Fatalf("%d rows, want 3", len(rows))
	}
	for _, row := range rows {
		for i, v := range row {
			requireCell(t, v, asciiText(3000, byte(i)))
		}
	}
}

func joinComma(s []string) string {
	var b bytes.Buffer
	for i, x := range s {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(x)
	}
	return b.String()
}

// TestBindLimits checks the largest value that one request binds (SVC_MAX_BIND_BYTES) and that a larger
// one is refused by the client before anything is sent, without breaking the session.
func TestBindLimits(t *testing.T) {
	_, c, db := session(t)
	run(t, c, db, "CREATE TABLE big (b BLOB)")
	st := prep(t, c, db, "INSERT INTO big VALUES (?)")

	full := make([]byte, sqlite3.SVC_MAX_BIND_BYTES)
	for i := range full {
		full[i] = byte(i)
	}
	if err := c.BindBlob(st, 1, full); err != nil {
		t.Fatalf("bind of the largest value: %v", err)
	}
	if _, err := c.Sqlite3Step(st); err != nil {
		t.Fatalf("step: %v", err)
	}
	if err := c.Sqlite3Reset(st); err != nil {
		t.Fatal(err)
	}

	err := c.BindBlob(st, 1, make([]byte, sqlite3.SVC_MAX_BIND_BYTES+1))
	if err == nil {
		t.Fatal("a value above the bound was accepted")
	}
	var se *rt.StatusError
	if !errors.Is(err, rt.ErrInvalidArg) && !errors.As(err, &se) {
		t.Fatalf("a value above the bound: %v, want ErrInvalidArg or a refusal by the server", err)
	}
	// The session still works.
	if n := one(t, c, db, "SELECT length(b) FROM big"); n != int64(sqlite3.SVC_MAX_BIND_BYTES) {
		t.Fatalf("stored length %v", n)
	}
	finalize(t, c, st)
}
