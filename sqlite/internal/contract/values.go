package contract

import (
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"fmt"
	"math"
	"reflect"
	"testing"
	"time"
)

// timeLayout is the documented TEXT form of a time.Time.
const timeLayout = "2006-01-02 15:04:05.999999999-07:00"

// valuer is a driver.Valuer that returns v.
type valuer struct{ v driver.Value }

func (x valuer) Value() (driver.Value, error) { return x.v, nil }

// ptrValuer is a driver.Valuer with a pointer receiver that accepts nil.
type ptrValuer struct{ v int64 }

func (p *ptrValuer) Value() (driver.Value, error) {
	if p == nil {
		return nil, nil
	}
	return p.v, nil
}

// doc is a value that is encoded with json.Marshal.
type doc struct {
	A int    `json:"a"`
	B string `json:"b"`
}

var (
	int64Type   = reflect.TypeOf(int64(0))
	float64Type = reflect.TypeOf(float64(0))
	stringType  = reflect.TypeOf("")
	bytesType   = reflect.TypeOf([]byte(nil))
	timeType    = reflect.TypeOf(time.Time{})
	anyType     = reflect.TypeOf((*any)(nil)).Elem()
)

// roundTrip stores in in a column declared as decl and reads it back.
func roundTrip(t *testing.T, db *sql.DB, decl string, in any) any {
	t.Helper()
	tbl := newTable(t, db, "rt", "id INTEGER PRIMARY KEY, v "+decl)
	exec(t, db, "INSERT INTO "+tbl+" (id, v) VALUES (?, ?)", 1, in)
	return scanOne[any](t, db, "SELECT v FROM "+tbl+" WHERE id = ?", 1)
}

func testValues(t *testing.T, db *sql.DB, caps Capabilities) {
	jsonDoc, err := json.Marshal(doc{A: 1, B: "x"})
	must(t, err, "marshal")

	cases := []struct {
		name string
		decl string
		in   any
		want any
	}{
		{"int64 min", "INTEGER", int64(math.MinInt64), int64(math.MinInt64)},
		{"int64 max", "INTEGER", int64(math.MaxInt64), int64(math.MaxInt64)},
		{"int", "INTEGER", -42, int64(-42)},
		{"int8", "INTEGER", int8(-8), int64(-8)},
		{"int16", "INTEGER", int16(-16), int64(-16)},
		{"int32", "INTEGER", int32(math.MinInt32), int64(math.MinInt32)},
		{"uint8", "INTEGER", uint8(255), int64(255)},
		{"uint16", "INTEGER", uint16(65535), int64(65535)},
		{"uint32", "INTEGER", uint32(math.MaxUint32), int64(math.MaxUint32)},
		{"uint64 up to MaxInt64", "INTEGER", uint64(math.MaxInt64), int64(math.MaxInt64)},
		{"float64", "REAL", 1.5, 1.5},
		{"float64 +Inf", "REAL", math.Inf(1), math.Inf(1)},
		{"float64 -Inf", "REAL", math.Inf(-1), math.Inf(-1)},
		{"float64 -0", "REAL", math.Copysign(0, -1), 0.0},
		{"float32", "REAL", float32(0.5), 0.5},
		{"bool true", "INTEGER", true, int64(1)},
		{"bool false", "INTEGER", false, int64(0)},
		{"empty string", "TEXT", "", ""},
		{"multi-byte string", "TEXT", "héllo, 世界 🌍", "héllo, 世界 🌍"},
		{"text that looks like a time stays text", "TEXT", "2024-03-05 10:20:30", "2024-03-05 10:20:30"},
		{"blob", "BLOB", []byte{0, 1, 2, 0xff}, []byte{0, 1, 2, 0xff}},
		{"json.RawMessage", "BLOB", json.RawMessage(`{"a":1}`), []byte(`{"a":1}`)},
		{"json.Marshal output", "BLOB", jsonDoc, jsonDoc},
		{"nil", "INTEGER", nil, nil},
		{"sql.NullInt64 valid", "INTEGER", sql.NullInt64{Int64: 5, Valid: true}, int64(5)},
		{"sql.NullInt64 null", "INTEGER", sql.NullInt64{}, nil},
		{"sql.NullString valid", "TEXT", sql.NullString{String: "s", Valid: true}, "s"},
		{"sql.NullString null", "TEXT", sql.NullString{}, nil},
		{"sql.NullFloat64 valid", "REAL", sql.NullFloat64{Float64: 2.5, Valid: true}, 2.5},
		{"sql.NullFloat64 null", "REAL", sql.NullFloat64{}, nil},
		{"sql.NullBool valid", "INTEGER", sql.NullBool{Bool: true, Valid: true}, int64(1)},
		{"sql.NullBool null", "INTEGER", sql.NullBool{}, nil},
		{"sql.NullByte valid", "INTEGER", sql.NullByte{Byte: 7, Valid: true}, int64(7)},
		{"sql.NullByte null", "INTEGER", sql.NullByte{}, nil},
		{"sql.NullTime null", "DATETIME", sql.NullTime{}, nil},
		{"driver.Valuer value", "INTEGER", valuer{int64(9)}, int64(9)},
		{"driver.Valuer nil", "INTEGER", valuer{nil}, nil},
		{"typed nil pointer Valuer", "INTEGER", (*ptrValuer)(nil), nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := roundTrip(t, db, tc.decl, tc.in)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %#v (%T), want %#v (%T)", got, got, tc.want, tc.want)
			}
		})
	}

	t.Run("nil []byte is an empty blob, not NULL", func(t *testing.T) {
		tbl := newTable(t, db, "blob", "id INTEGER PRIMARY KEY, v BLOB")
		exec(t, db, "INSERT INTO "+tbl+" (id, v) VALUES (1, ?), (2, ?)", []byte(nil), nil)
		b, ok := scanOne[any](t, db, "SELECT v FROM "+tbl+" WHERE id = 1").([]byte)
		if !ok || len(b) != 0 {
			t.Fatalf("got %#v, want an empty []byte", b)
		}
		if v := scanOne[any](t, db, "SELECT v FROM "+tbl+" WHERE id = 2"); v != nil {
			t.Fatalf("NULL row read back as %#v", v)
		}
		if caps.FullSQL {
			if got := scanOne[string](t, db, "SELECT typeof(v) FROM "+tbl+" WHERE id = 1"); got != "blob" {
				t.Fatalf("typeof = %q, want blob", got)
			}
		}
	})

	t.Run("negative zero is sent as -0", func(t *testing.T) {
		got := scanOne[float64](t, db, "SELECT ?", math.Copysign(0, -1))
		if !math.Signbit(got) {
			t.Fatalf("sign of -0 lost: %v", got)
		}
	})

	t.Run("NaN is rejected before it is sent", func(t *testing.T) {
		tbl := newTable(t, db, "nan", "id INTEGER PRIMARY KEY, v REAL")
		if _, err := db.Exec("INSERT INTO "+tbl+" (id, v) VALUES (1, ?)", math.NaN()); err == nil {
			t.Fatal("NaN accepted")
		}
		wantInts(t, db, "SELECT id FROM "+tbl)
	})

	t.Run("uint64 above MaxInt64 is rejected", func(t *testing.T) {
		tbl := newTable(t, db, "u64", "id INTEGER PRIMARY KEY, v INTEGER")
		if _, err := db.Exec("INSERT INTO "+tbl+" (id, v) VALUES (1, ?)", uint64(math.MaxUint64)); err == nil {
			t.Fatal("uint64 above MaxInt64 accepted")
		}
		wantInts(t, db, "SELECT id FROM "+tbl)
	})

	t.Run("unsupported types are rejected without executing", func(t *testing.T) {
		tbl := newTable(t, db, "unsup", "id INTEGER PRIMARY KEY, v TEXT")
		for _, arg := range []any{doc{A: 1}, map[string]int{"a": 1}} {
			if _, err := db.Exec("INSERT INTO "+tbl+" (id, v) VALUES (1, ?)", arg); err == nil {
				t.Fatalf("%T accepted", arg)
			}
		}
		wantInts(t, db, "SELECT id FROM "+tbl)
	})

	t.Run("time.Time is stored in the documented TEXT form", func(t *testing.T) {
		inputs := []time.Time{
			time.Date(2024, 3, 5, 10, 20, 30, 123456789, time.UTC),
			time.Date(2024, 3, 5, 10, 20, 30, 0, time.FixedZone("KST", 9*3600)),
			time.Date(1999, 12, 31, 23, 59, 59, 500000000, time.FixedZone("west", -(5*3600+30*60))),
		}
		for _, in := range inputs {
			tbl := newTable(t, db, "tm", "id INTEGER PRIMARY KEY, d DATETIME, s TEXT")
			exec(t, db, "INSERT INTO "+tbl+" (id, d, s) VALUES (1, ?, ?)", in, in)
			if got := scanOne[time.Time](t, db, "SELECT d FROM "+tbl+" WHERE id = 1"); !got.Equal(in) {
				t.Fatalf("time %v read back as %v", in, got)
			}
			if got := scanOne[string](t, db, "SELECT s FROM "+tbl+" WHERE id = 1"); got != in.Format(timeLayout) {
				t.Fatalf("TEXT form = %q, want %q", got, in.Format(timeLayout))
			}
		}
	})

	t.Run("TEXT in DATETIME columns parses the documented forms", func(t *testing.T) {
		utc := func(y int, mo time.Month, d, h, mi, s int) time.Time {
			return time.Date(y, mo, d, h, mi, s, 0, time.UTC)
		}
		cases := []struct {
			text string
			want time.Time
		}{
			{"2024-03-05 10:20:30", utc(2024, 3, 5, 10, 20, 30)},
			{"2024-03-05T10:20:30", utc(2024, 3, 5, 10, 20, 30)},
			{"2024-03-05 10:20", utc(2024, 3, 5, 10, 20, 0)},
			{"2024-03-05T10:20", utc(2024, 3, 5, 10, 20, 0)},
			{"2024-03-05", utc(2024, 3, 5, 0, 0, 0)},
			{"2024-03-05 10:20:30+09:00", time.Date(2024, 3, 5, 10, 20, 30, 0, time.FixedZone("", 9*3600))},
			{"2024-03-05T10:20:30.5-05:00", time.Date(2024, 3, 5, 10, 20, 30, 5e8, time.FixedZone("", -5*3600))},
		}
		for _, tc := range cases {
			tbl := newTable(t, db, "tp", "id INTEGER PRIMARY KEY, d DATETIME")
			exec(t, db, "INSERT INTO "+tbl+" (id, d) VALUES (1, ?)", tc.text)
			if got := scanOne[time.Time](t, db, "SELECT d FROM "+tbl+" WHERE id = 1"); !got.Equal(tc.want) {
				t.Fatalf("%q read back as %v, want %v", tc.text, got, tc.want)
			}
		}
	})

	t.Run("named placeholders", func(t *testing.T) {
		for _, ph := range []string{":a", "@a", "$a"} {
			if got := scanOne[int64](t, db, "SELECT "+ph, sql.Named("a", 7)); got != 7 {
				t.Fatalf("%s: got %d, want 7", ph, got)
			}
		}
		tbl := newTable(t, db, "named", "id INTEGER PRIMARY KEY, v TEXT")
		exec(t, db, "INSERT INTO "+tbl+" (id, v) VALUES (:id, @v)", sql.Named("id", 1), sql.Named("v", "x"))
		if got := scanOne[string](t, db, "SELECT v FROM "+tbl+" WHERE id = $id", sql.Named("id", 1)); got != "x" {
			t.Fatalf("got %q, want x", got)
		}
	})

	t.Run("positional and named arguments together", func(t *testing.T) {
		var n int64
		var s string
		must(t, db.QueryRow("SELECT ?, :b", 1, sql.Named("b", "two")).Scan(&n, &s), "scan")
		if n != 1 || s != "two" {
			t.Fatalf("got %d, %q; want 1, two", n, s)
		}
	})

	t.Run("null arguments", func(t *testing.T) {
		tbl := newTable(t, db, "nulls", "id INTEGER PRIMARY KEY, v TEXT")
		exec(t, db, "INSERT INTO "+tbl+" (id, v) VALUES (?, ?)", 1, nil)
		if got := scanOne[any](t, db, "SELECT v FROM "+tbl+" WHERE id = 1"); got != nil {
			t.Fatalf("got %#v, want NULL", got)
		}
	})

	if caps.FullSQL {
		t.Run("typeof of typed values", func(t *testing.T) {
			var a, b, c, d string
			must(t, db.QueryRow("SELECT typeof(1), typeof('x'), typeof(1.5), typeof(NULL)").Scan(&a, &b, &c, &d), "scan")
			if a != "integer" || b != "text" || c != "real" || d != "null" {
				t.Fatalf("typeof = %s %s %s %s", a, b, c, d)
			}
		})
	}
}

func testColumnTypes(t *testing.T, db *sql.DB, caps Capabilities) {
	tbl := newTable(t, db, "ct", "id INTEGER PRIMARY KEY, n integer, s TEXT, r REAL, b BLOB, d DATETIME, w WIDGET, u")
	exec(t, db, "INSERT INTO "+tbl+" (id, n, s, r, b, d, w, u) VALUES (1, 7, 'x', 1.5, ?, '2024-03-05 10:20:30', 'w', 'untyped')", []byte{1, 2})
	exec(t, db, "INSERT INTO "+tbl+" (id) VALUES (2)")

	// The declared types are known before the first row. u has no declared
	// type, so its type is the storage class of the current row.
	cols := []columnSpec{
		{"id", "INTEGER", int64Type},
		{"n", "INTEGER", int64Type},
		{"s", "TEXT", stringType},
		{"r", "REAL", float64Type},
		{"b", "BLOB", bytesType},
		{"d", "DATETIME", timeType},
		{"w", "WIDGET", anyType},
		{"u", "", anyType},
	}
	query := "SELECT id, n, s, r, b, d, w, u FROM " + tbl + " WHERE id = ?"
	for _, tc := range []struct {
		id    int64
		wantU string
	}{{1, "TEXT"}, {2, "NULL"}} {
		t.Run(fmt.Sprintf("row %d", tc.id), func(t *testing.T) {
			rows, err := db.Query(query, tc.id)
			must(t, err, "query")
			defer rows.Close()
			cts, err := rows.ColumnTypes()
			must(t, err, "ColumnTypes")
			checkColumnTypes(t, cols, cts, "")
			if !rows.Next() {
				t.Fatalf("no row %d: %v", tc.id, rows.Err())
			}
			cts, err = rows.ColumnTypes()
			must(t, err, "ColumnTypes after Next")
			checkColumnTypes(t, cols, cts, tc.wantU)
		})
	}

	t.Run("expression columns", func(t *testing.T) {
		rows, err := db.Query("SELECT 1, 'a', 1.5, X'01', NULL, ?", 2.5)
		must(t, err, "query")
		defer rows.Close()
		cts, err := rows.ColumnTypes()
		must(t, err, "ColumnTypes")
		for i, ct := range cts {
			if ct.DatabaseTypeName() != "" {
				t.Fatalf("column %d before the first row: %q, want \"\"", i, ct.DatabaseTypeName())
			}
		}
		if !rows.Next() {
			t.Fatalf("no row: %v", rows.Err())
		}
		cts, err = rows.ColumnTypes()
		must(t, err, "ColumnTypes after Next")
		want := []string{"INTEGER", "TEXT", "REAL", "BLOB", "NULL", "REAL"}
		for i, ct := range cts {
			if ct.DatabaseTypeName() != want[i] {
				t.Fatalf("column %d: %q, want %q", i, ct.DatabaseTypeName(), want[i])
			}
			if st := ct.ScanType(); st != anyType {
				t.Fatalf("column %d: ScanType %v, want interface{}", i, st)
			}
		}
	})

	if caps.FullSQL {
		t.Run("expression storage classes with arithmetic", func(t *testing.T) {
			rows, err := db.Query("SELECT 1 + 1, 'a' || 'b', CAST(1 AS TEXT)")
			must(t, err, "query")
			defer rows.Close()
			if !rows.Next() {
				t.Fatalf("no row: %v", rows.Err())
			}
			cts, err := rows.ColumnTypes()
			must(t, err, "ColumnTypes")
			want := []string{"INTEGER", "TEXT", "TEXT"}
			for i, ct := range cts {
				if ct.DatabaseTypeName() != want[i] {
					t.Fatalf("column %d: %q, want %q", i, ct.DatabaseTypeName(), want[i])
				}
			}
		})
	}
}

// columnSpec is the expected metadata of one result column.
type columnSpec struct {
	name, dbType string
	scan         reflect.Type
}

// checkColumnTypes compares cts with cols. untypedWant is the expected
// DatabaseTypeName of the column without a declared type (the last one).
func checkColumnTypes(t *testing.T, cols []columnSpec, cts []*sql.ColumnType, untypedWant string) {
	t.Helper()
	if len(cts) != len(cols) {
		t.Fatalf("got %d columns, want %d", len(cts), len(cols))
	}
	for i, c := range cols {
		ct := cts[i]
		if ct.Name() != c.name {
			t.Errorf("column %d: name %q, want %q", i, ct.Name(), c.name)
		}
		want := c.dbType
		if i == len(cols)-1 {
			want = untypedWant
		}
		if got := ct.DatabaseTypeName(); got != want {
			t.Errorf("column %s: DatabaseTypeName %q, want %q", c.name, got, want)
		}
		if got := ct.ScanType(); got != c.scan {
			t.Errorf("column %s: ScanType %v, want %v", c.name, got, c.scan)
		}
		if _, ok := ct.Nullable(); ok {
			t.Errorf("column %s: Nullable reports support", c.name)
		}
	}
	if t.Failed() {
		t.FailNow()
	}
}
