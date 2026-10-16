package wire

import (
	"bytes"
	"encoding/hex"
	"math"
	"reflect"
	"testing"
)

func TestStatusEncoding(t *testing.T) {
	if got := SQLiteStatus(2067); got != 0x10813 {
		t.Fatalf("SQLiteStatus(2067) = %#x, want 0x10813", uint64(got))
	}
	if !SQLiteStatus(5).IsSQLite() || SQLiteStatus(5).SQLiteCode() != 5 {
		t.Fatal("SQLiteStatus does not round-trip")
	}
	if got := ProtocolStatus(ProtoNoCursor); got != 0x20006 {
		t.Fatalf("ProtocolStatus(NO_CURSOR) = %#x, want 0x20006", uint64(got))
	}
	if !ProtocolStatus(ProtoLimit).IsProtocol() || ProtocolStatus(ProtoLimit).ProtoCode() != ProtoLimit {
		t.Fatal("ProtocolStatus does not round-trip")
	}
	// No SQLite or protocol status may collide with HQQ's own statuses 1..8.
	for rc := range 256 {
		if SQLiteStatus(rc) < 0x10000 {
			t.Fatalf("SQLiteStatus(%d) below 0x10000", rc)
		}
	}
}

func TestValueRoundTrip(t *testing.T) {
	values := []Value{
		Integer(0), Integer(-1), Integer(math.MaxInt64), Integer(math.MinInt64),
		Float(1.5), Float(math.Inf(-1)), Float(-0.0),
		Text(""), Text("héllo\x00world"),
		Blob(nil), Blob([]byte{0, 1, 2, 255}),
		Null(),
	}
	for _, v := range values {
		e := NewEncoder(0)
		e.Value(v)
		d := NewDecoder(e.Bytes())
		got := d.Value()
		if err := d.Finish(); err != nil {
			t.Fatalf("%v: %v", v.Type, err)
		}
		if got.Type != v.Type || got.Int != v.Int || got.Float != v.Float || !bytes.Equal(got.Bytes, v.Bytes) {
			t.Fatalf("round trip of %v: got %+v want %+v", v.Type, got, v)
		}
	}
}

func TestParamsAndColumnsRoundTrip(t *testing.T) {
	params := []Param{
		{Ordinal: 1, Value: Integer(7)},
		{Ordinal: 2, Name: "name", Value: Text("x")},
		{Ordinal: 3, Value: Blob([]byte("abc"))},
		{Ordinal: 9, Name: "n", Value: Null()},
	}
	e := NewEncoder(0)
	e.Params(params)
	e.Columns([]Column{{Name: "a", Decltype: "INTEGER"}, {Name: "b"}})
	d := NewDecoder(e.Bytes())
	if got := d.Params(); !reflect.DeepEqual(normalizeParams(got), normalizeParams(params)) {
		t.Fatalf("params = %+v, want %+v", got, params)
	}
	if got := d.Columns(); !reflect.DeepEqual(got, []Column{{Name: "a", Decltype: "INTEGER"}, {Name: "b"}}) {
		t.Fatalf("columns = %+v", got)
	}
	if err := d.Finish(); err != nil {
		t.Fatal(err)
	}
}

// normalizeParams makes nil and empty byte slices compare equal.
func normalizeParams(ps []Param) []Param {
	out := make([]Param, len(ps))
	for i, p := range ps {
		if p.Value.Type == TypeBlob || p.Value.Type == TypeText {
			p.Value.Bytes = append([]byte{}, p.Value.Bytes...)
		}
		out[i] = p
	}
	return out
}

func TestRequestBodiesRoundTrip(t *testing.T) {
	e := NewEncoder(0)
	open := OpenRequest{
		Major: 1, Minor: 0, DB: "main", Flags: OpenReadWrite | OpenCreate, BusyMS: 5000,
		Pragmas: []Pragma{{Name: "journal_mode", Value: "WAL"}, {Name: "cache_size", Value: "-2000"}},
	}
	AppendOpenRequest(e, open)
	gotOpen, err := ParseOpenRequest(e.Bytes())
	if err != nil || !reflect.DeepEqual(gotOpen, open) {
		t.Fatalf("open = %+v, %v", gotOpen, err)
	}

	e.Reset()
	q := QueryRequest{SQL: "SELECT ?", Params: []Param{{Ordinal: 1, Value: Integer(1)}}, Fetch: 64}
	AppendQueryRequest(e, q)
	gotQ, err := ParseQueryRequest(e.Bytes())
	if err != nil || gotQ.SQL != q.SQL || gotQ.Fetch != 64 || len(gotQ.Params) != 1 {
		t.Fatalf("query = %+v, %v", gotQ, err)
	}

	e.Reset()
	AppendBeginRequest(e, BeginRequest{Mode: TxImmediate})
	if b, err := ParseBeginRequest(e.Bytes()); err != nil || b.Mode != TxImmediate || b.ReadOnly {
		t.Fatalf("begin = %+v, %v", b, err)
	}

	e.Reset()
	AppendExecResponse(e, ExecResponse{RowsAffected: 3, LastInsertRowID: 9})
	if r, err := ParseExecResponse(e.Bytes()); err != nil || r.RowsAffected != 3 || r.LastInsertRowID != 9 {
		t.Fatalf("exec response = %+v, %v", r, err)
	}
}

// TestTruncatedInputIsRejected checks that every strict prefix of a valid body
// fails to parse: no field may be read past the end, and no prefix may be
// accepted as a shorter valid message.
func TestTruncatedInputIsRejected(t *testing.T) {
	cases := map[string]func(e *Encoder){
		"open": func(e *Encoder) {
			AppendOpenRequest(e, OpenRequest{Major: 1, DB: "db", Flags: OpenReadWrite, Pragmas: []Pragma{{"a", "b"}}})
		},
		"query": func(e *Encoder) {
			AppendQueryRequest(e, QueryRequest{SQL: "SELECT ?", Params: []Param{{1, "", Text("xy")}, {2, "n", Float(2)}}, Fetch: 3})
		},
		"stmtExec": func(e *Encoder) {
			AppendStmtExecRequest(e, StmtExecRequest{Stmt: 4, Params: []Param{{1, "", Blob([]byte{1, 2})}}})
		},
		"begin": func(e *Encoder) { AppendBeginRequest(e, BeginRequest{Mode: TxExclusive}) },
		"fetch": func(e *Encoder) { AppendFetchRequest(e, FetchRequest{Cursor: 1, NRows: 2}) },
	}
	parsers := map[string]func([]byte) error{
		"open": func(b []byte) error { _, err := ParseOpenRequest(b); return err },
		"query": func(b []byte) error {
			_, err := ParseQueryRequest(b)
			return err
		},
		"stmtExec": func(b []byte) error { _, err := ParseStmtExecRequest(b); return err },
		"begin":    func(b []byte) error { _, err := ParseBeginRequest(b); return err },
		"fetch":    func(b []byte) error { _, err := ParseFetchRequest(b); return err },
	}
	for name, enc := range cases {
		e := NewEncoder(0)
		enc(e)
		full := append([]byte{}, e.Bytes()...)
		if err := parsers[name](full); err != nil {
			t.Fatalf("%s: full body rejected: %v", name, err)
		}
		for n := range len(full) {
			if err := parsers[name](full[:n]); err == nil {
				t.Fatalf("%s: prefix of %d/%d bytes accepted", name, n, len(full))
			}
		}
		if err := parsers[name](append(full, 0)); err == nil {
			t.Fatalf("%s: trailing byte accepted", name)
		}
	}
}

func TestDecoderRejectsInvalidMessages(t *testing.T) {
	tests := []struct {
		name string
		body func() []byte
		code ProtoCode
	}{
		{"bad value tag", func() []byte { return []byte{9} }, ProtoBadRequest},
		{"ordinal zero", func() []byte {
			e := NewEncoder(0)
			e.U32(1)
			e.U32(0)
			e.String("")
			e.Value(Null())
			return e.Bytes()
		}, ProtoBadRequest},
		{"duplicate ordinal", func() []byte {
			e := NewEncoder(0)
			e.U32(2)
			for range 2 {
				e.U32(1)
				e.String("")
				e.Value(Null())
			}
			return e.Bytes()
		}, ProtoBadRequest},
		{"too many params", func() []byte {
			e := NewEncoder(0)
			e.U32(MaxParams + 1)
			return e.Bytes()
		}, ProtoLimit},
		{"string past end", func() []byte { return []byte{5, 0, 0, 0, 'a'} }, ProtoBadRequest},
		{"invalid more flag", func() []byte {
			e := NewEncoder(0)
			e.U32(1)
			e.Columns(nil)
			e.U8(2)
			return e.Bytes()
		}, ProtoBadRequest},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			d := NewDecoder(tc.body())
			d.Params()
			if tc.name == "invalid more flag" {
				d = NewDecoder(tc.body())
				ParseResultHeader(d)
			}
			err := d.Err()
			pe, ok := err.(*ProtocolError)
			if !ok || pe.Code != tc.code {
				t.Fatalf("err = %v, want code %s", err, tc.code)
			}
		})
	}
}

func TestBeginRules(t *testing.T) {
	tests := []struct {
		name string
		req  BeginRequest
		raw  []byte
		ok   bool
	}{
		{name: "deferred", raw: []byte{0, 0}, ok: true},
		{name: "exclusive", raw: []byte{2, 0}, ok: true},
		{name: "read-only deferred", raw: []byte{0, 1}, ok: true},
		{name: "read-only immediate refused", raw: []byte{1, 1}},
		{name: "bad mode", raw: []byte{3, 0}},
		{name: "bad read_only", raw: []byte{0, 2}},
	}
	for _, tc := range tests {
		_, err := ParseBeginRequest(tc.raw)
		if (err == nil) != tc.ok {
			t.Fatalf("%s: err = %v, want ok=%v", tc.name, err, tc.ok)
		}
	}
}

func TestPragmaAndFlagValidation(t *testing.T) {
	for name, want := range map[string]bool{"journal_mode": true, "_x1": true, "1x": false, "Journal": false, "": false} {
		if got := ValidPragmaName(name); got != want {
			t.Fatalf("ValidPragmaName(%q) = %v", name, got)
		}
	}
	for value, want := range map[string]bool{"WAL": true, "-2000": true, "a b": false, "x;DROP": false, "": true} {
		if got := ValidPragmaValue(value); got != want {
			t.Fatalf("ValidPragmaValue(%q) = %v", value, got)
		}
	}
	for flags, want := range map[uint32]bool{
		OpenReadOnly:                 true,
		OpenReadWrite | OpenCreate:   true,
		OpenReadOnly | OpenReadWrite: false,
		OpenCreate:                   false,
		OpenReadWrite | 0x8:          false,
		OpenReadOnly | OpenCreate:    true,
	} {
		if got := ValidOpenFlags(flags); got != want {
			t.Fatalf("ValidOpenFlags(%#x) = %v", flags, got)
		}
	}
}

// TestGoldenExamples pins the byte layouts shown in PROTOCOL.md section 16.
func TestGoldenExamples(t *testing.T) {
	for _, tc := range goldenExamples() {
		if got := hex.EncodeToString(tc.body); got != tc.hex {
			t.Errorf("%s:\n got %s\nwant %s", tc.name, got, tc.hex)
		}
	}
}

type golden struct {
	name string
	body []byte
	hex  string
}

func goldenExamples() []golden {
	open := NewEncoder(0)
	AppendOpenRequest(open, OpenRequest{
		Major: 1, Minor: 0, DB: "main", Flags: OpenReadWrite | OpenCreate, BusyMS: 5000,
		Pragmas: []Pragma{{Name: "journal_mode", Value: "WAL"}},
	})

	exec := NewEncoder(0)
	AppendExecRequest(exec, ExecRequest{
		SQL:    "INSERT INTO t VALUES (?)",
		Params: []Param{{Ordinal: 1, Value: Integer(42)}},
	})

	qres := NewEncoder(0)
	AppendResultHeader(qres, ResultHeader{Cursor: 0, Columns: []Column{{Name: "n"}}, More: false})
	qres.U32(1) // one row
	qres.Value(Integer(42))

	vals := NewEncoder(0)
	vals.Value(Text("hi"))
	vals.Value(Null())
	vals.Value(Float(1.5))

	return []golden{
		{"Open request", open.Bytes(),
			"0100" + "0000" + // major, minor
				"04000000" + "6d61696e" + // db = "main"
				"06000000" + // flags = READWRITE|CREATE
				"88130000" + // busy_ms = 5000
				"01000000" + // one pragma
				"0c000000" + "6a6f75726e616c5f6d6f6465" + // "journal_mode"
				"03000000" + "57414c"}, // "WAL"
		{"Exec request", exec.Bytes(),
			"18000000" + "494e5345525420494e544f20742056414c55455320283f29" + // "INSERT INTO t VALUES (?)"
				"01000000" + // one param
				"01000000" + "00000000" + // ordinal 1, positional
				"01" + "2a00000000000000"}, // INTEGER 42
		{"Query response (complete, one row)", qres.Bytes(), "00000000" + "01000000" + "01000000" + "6e" + "00000000" +
			"00" + "01000000" + "01" + "2a00000000000000"},
		{"Values", vals.Bytes(), "03" + "02000000" + "6869" + "05" + "02" + "000000000000f83f"},
	}
}
