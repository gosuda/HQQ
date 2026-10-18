package sqlite

import (
	"bytes"
	"context"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"math"
	"strings"
	"sync"
	"testing"
	"time"

	"gosuda.org/hqq/bindgen/examples/sqlite3"
	"gosuda.org/hqq/bindgen/rt"
	"gosuda.org/hqq/sqlite/internal/wire"
)

// fakeAPI is a scripted api. It records every call, and a blocking fetch waits
// for Kill the way a real session does.
type fakeAPI struct {
	mu     sync.Mutex
	log    []string
	killed chan struct{}
	once   sync.Once
	nextID stmtID

	meta   sqlite3.StmtMeta                                   // answer of StmtInfo, and of the later calls when metas is empty
	metas  []sqlite3.StmtMeta                                 // answers of successive StmtInfo calls
	fetch  func(n int) (sqlite3.Rows, error)                  // answer of the n-th fetch_rows (0-based)
	chunk  func(col int32, off uint64, n int) ([]byte, error) // answer of column_chunk
	block  bool                                               // fetch_rows waits for Kill
	prep   func(sql string) error                             // error of PrepareOne
	exec   func() (int64, int64, error)                       // answer of ExecStmt
	fin    func() error                                       // answer of the wait of Finalize
	nfetch int
}

func newFake() *fakeAPI {
	return &fakeAPI{killed: make(chan struct{}), nextID: 1}
}

func (f *fakeAPI) rec(s string, args ...any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.log = append(f.log, fmt.Sprintf(s, args...))
}

func (f *fakeAPI) calls() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return strings.Join(f.log, ",")
}

func (f *fakeAPI) Close() error  { f.rec("close"); return nil }
func (f *fakeAPI) Kill()         { f.rec("kill"); f.once.Do(func() { close(f.killed) }) }
func (f *fakeAPI) Cancel() error { return nil }
func (f *fakeAPI) Err() error {
	select {
	case <-f.killed:
		return rt.ErrSessionBroken
	default:
		return nil
	}
}
func (f *fakeAPI) OpenV2(string, int32) (dbID, error) { return 1, nil }
func (f *fakeAPI) BusyTimeout(dbID, int32) error      { return nil }
func (f *fakeAPI) GetAutocommit(dbID) (int32, error)  { f.rec("autocommit"); return 1, nil }
func (f *fakeAPI) PrepareOne(_ dbID, sql string) (stmtID, error) {
	f.rec("prepare")
	if f.prep != nil {
		if err := f.prep(sql); err != nil {
			return 0, err
		}
	}
	f.nextID++
	return f.nextID, nil
}

func (f *fakeAPI) nextMeta() sqlite3.StmtMeta {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.metas) > 0 {
		m := f.metas[0]
		f.metas = f.metas[1:]
		return m
	}
	return f.meta
}

func (f *fakeAPI) StmtInfo(stmtID) (sqlite3.StmtMeta, error) { f.rec("info"); return f.nextMeta(), nil }
func (f *fakeAPI) StmtInfoAsync(stmtID) (func() (sqlite3.StmtMeta, error), error) {
	f.rec("info")
	return func() (sqlite3.StmtMeta, error) { return f.nextMeta(), nil }, nil
}
func (f *fakeAPI) Finalize(stmtID) error { f.rec("finalize"); return nil }
func (f *fakeAPI) FinalizeAsync(stmtID) (func() error, error) {
	f.rec("finalize")
	return func() error {
		if f.fin != nil {
			return f.fin()
		}
		return nil
	}, nil
}
func (f *fakeAPI) ResetAsync(stmtID) (func() error, error) {
	f.rec("reset")
	return func() error { return nil }, nil
}
func (f *fakeAPI) BindValuesAsync(_ stmtID, kinds []uint8, _ []uint64, _ []byte) (func() error, error) {
	f.rec("bind%d", len(kinds))
	return func() error { return nil }, nil
}
func (f *fakeAPI) ExecStmt(stmtID, []uint8, []uint64, []byte) (int64, int64, error) {
	f.rec("exec")
	if f.exec != nil {
		return f.exec()
	}
	return 1, 7, nil
}
func (f *fakeAPI) ExecStmtAsync(stmtID, []uint8, []uint64, []byte) (func() (int64, int64, error), error) {
	f.rec("exec")
	return func() (int64, int64, error) {
		if f.exec != nil {
			return f.exec()
		}
		return 1, 7, nil
	}, nil
}
func (f *fakeAPI) FetchRowsAsync(stmtID, int32) (func() (sqlite3.Rows, error), error) {
	f.mu.Lock()
	n := f.nfetch
	f.nfetch++
	f.mu.Unlock()
	f.rec("fetch")
	return func() (sqlite3.Rows, error) {
		if f.block {
			<-f.killed
			return sqlite3.Rows{}, &rt.TransportError{Func: "fetch_rows", Err: io.ErrClosedPipe, Unknown: true}
		}
		return f.fetch(n)
	}, nil
}
func (f *fakeAPI) ColumnChunk(_ stmtID, col int32, off uint64, dst []byte) ([]byte, error) {
	f.rec("chunk%d@%d", col, off)
	b, err := f.chunk(col, off, len(dst))
	if err != nil {
		return nil, err
	}
	return dst[:copy(dst, b)], nil
}

// fakeConn returns a conn on the fake, with the connector defaults.
func fakeConn(t *testing.T, f *fakeAPI) *conn {
	t.Helper()
	c, err := NewConnector("fake", Config{Database: "x", Timeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	cn := newConn(c, f)
	cn.db = 1
	return cn
}

func intCol(name string) sqlite3.StmtMeta {
	return sqlite3.StmtMeta{Readonly: true, Columns: []sqlite3.Column{{Name: name, Decltype: "INTEGER"}}}
}

func intRows(rc int32, errmsg string, vals ...int64) sqlite3.Rows {
	r := sqlite3.Rows{Rc: rc, Ncols: 1, Errmsg: errmsg}
	for _, v := range vals {
		r.Cells = append(r.Cells, sqlite3.Value{Tag: sqlite3.ValueInt, Int: v})
	}
	return r
}

// drainRows reads every row of one integer column and returns them with the end error.
func drainRows(t *testing.T, rs driver.Rows) ([]int64, error) {
	t.Helper()
	var out []int64
	dest := make([]driver.Value, 1)
	for {
		if err := rs.Next(dest); err != nil {
			if err == io.EOF {
				err = nil
			}
			return out, err
		}
		out = append(out, dest[0].(int64))
	}
}

func query(t *testing.T, cn *conn, ctx context.Context, q string, args ...driver.NamedValue) (driver.Rows, error) {
	t.Helper()
	return cn.QueryContext(ctx, q, args)
}

// TestBigCellsBeforePrefetch checks the order of the requests of a batch whose last
// row has a cell that did not fit: its chunks are read before the next batch is
// requested, because fetch_rows steps first and would step over the row.
func TestBigCellsBeforePrefetch(t *testing.T) {
	f := newFake()
	f.meta = sqlite3.StmtMeta{Readonly: true, Columns: []sqlite3.Column{{Name: "b", Decltype: "BLOB"}}}
	blob := make([]byte, 2<<20+5)
	for i := range blob {
		blob[i] = byte(i)
	}
	f.fetch = func(n int) (sqlite3.Rows, error) {
		if n == 0 {
			return sqlite3.Rows{Rc: sqlite3.SQLITE_ROW, Ncols: 1, Cells: []sqlite3.Value{
				{Tag: sqlite3.ValueBlob, Blob: []byte{1}},
				{Tag: sqlite3.ValueBigBlob, BigBlob: uint64(len(blob))},
			}}, nil
		}
		return sqlite3.Rows{Rc: sqlite3.SQLITE_DONE, Ncols: 1, Cells: []sqlite3.Value{{Tag: sqlite3.ValueNull}}}, nil
	}
	f.chunk = func(col int32, off uint64, n int) ([]byte, error) { return blob[off:min(int(off)+n, len(blob))], nil }
	cn := fakeConn(t, f)
	rs, err := query(t, cn, context.Background(), "SELECT b")
	if err != nil {
		t.Fatal(err)
	}
	defer rs.Close()
	dest := make([]driver.Value, 1)
	var got [][]byte
	for rs.Next(dest) == nil {
		if b, ok := dest[0].([]byte); ok {
			got = append(got, b)
		} else {
			got = append(got, nil)
		}
	}
	if len(got) != 3 || !bytes.Equal(got[0], []byte{1}) || !bytes.Equal(got[1], blob) || got[2] != nil {
		t.Fatalf("rows: %d, big cell equal %v", len(got), len(got) > 1 && bytes.Equal(got[1], blob))
	}
	want := "prepare,info,fetch,chunk0@0,chunk0@1048576,chunk0@2097152,fetch"
	if calls := f.calls(); !strings.HasPrefix(calls, want) {
		t.Fatalf("calls = %s\nwant prefix %s", calls, want)
	}
}

// TestRowsBeforeError checks that the rows read before a failing step are
// returned, then the error, and that the error is sticky.
func TestRowsBeforeError(t *testing.T) {
	f := newFake()
	f.meta = intCol("x")
	f.fetch = func(n int) (sqlite3.Rows, error) {
		if n == 0 {
			return intRows(sqlite3.SQLITE_ROW, "", 1, 2), nil
		}
		return intRows(CodeConstraintUnique, "UNIQUE constraint failed: t.x", 3), nil
	}
	cn := fakeConn(t, f)
	rs, err := query(t, cn, context.Background(), "SELECT x")
	if err != nil {
		t.Fatal(err)
	}
	defer rs.Close()
	got, err := drainRows(t, rs)
	if fmt.Sprint(got) != "[1 2 3]" {
		t.Fatalf("rows = %v", got)
	}
	var se *Error
	if !errors.As(err, &se) || se.Code != CodeConstraint || se.ExtendedCode != CodeConstraintUnique || !strings.Contains(se.Message, "t.x") {
		t.Fatalf("end error = %v", err)
	}
	if err2 := rs.Next(make([]driver.Value, 1)); err2 == nil || err2 == io.EOF {
		t.Fatalf("Next after the error = %v, want the sticky error", err2)
	}
	if !cn.IsValid() {
		t.Fatal("a failing step left the conn invalid")
	}
}

// TestGuardAbandonsCall checks that a context that ends while a call is blocked
// returns the context error promptly, makes the conn bad, and does not look like a
// lost connection.
func TestGuardAbandonsCall(t *testing.T) {
	f := newFake()
	f.meta = intCol("x")
	f.block = true
	cn := fakeConn(t, f)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := query(t, cn, ctx, "SELECT x")
	if !errors.Is(err, context.DeadlineExceeded) || errors.Is(err, driver.ErrBadConn) {
		t.Fatalf("err = %v, want the deadline and not ErrBadConn", err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("the call was abandoned after %v", d)
	}
	if cn.IsValid() {
		t.Fatal("the conn is valid after an abandoned call")
	}
	if _, err := query(t, cn, context.Background(), "SELECT 1"); !errors.Is(err, driver.ErrBadConn) {
		t.Fatalf("call on the abandoned conn: %v, want driver.ErrBadConn", err)
	}
	if got := f.calls(); strings.Count(got, "prepare") != 1 {
		t.Fatalf("calls after the abandonment: %s", got)
	}
}

// TestGuardTimeoutWithoutContext checks the bound of the calls that have no caller
// context (Commit, Rollback, closing rows).
func TestGuardTimeoutWithoutContext(t *testing.T) {
	f := newFake()
	f.meta = intCol("x")
	f.fetch = func(n int) (sqlite3.Rows, error) { return intRows(sqlite3.SQLITE_ROW, "", 1), nil }
	cn := fakeConn(t, f)
	cn.timeout = 50 * time.Millisecond
	rs, err := query(t, cn, context.Background(), "SELECT x")
	if err != nil {
		t.Fatal(err)
	}
	f.block = true // the prefetch is in flight and the server hangs
	start := time.Now()
	_ = rs.Close()
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("Close of rows with a hung prefetch took %v", d)
	}
	if cn.IsValid() {
		t.Fatal("the conn is valid after the timeout killed the session")
	}
}

// TestErrorMapping checks the error classes of the calls of the session.
func TestErrorMapping(t *testing.T) {
	lost := &rt.TransportError{Func: "x", Err: io.ErrClosedPipe, Unknown: true}
	refused := &rt.TransportError{Func: "x", Err: io.ErrClosedPipe}
	lib := &rt.LibError{Func: "exec_stmt", Code: CodeConstraintUnique, Detail: "UNIQUE failed"}
	multi := &rt.LibError{Func: "prepare_one", Code: int64(sqlite3.SVC_SQL_MULTIPLE), Detail: "multiple statements"}

	t.Run("exec in flight is unknown", func(t *testing.T) {
		f := newFake()
		f.exec = func() (int64, int64, error) { return 0, 0, lost }
		cn := fakeConn(t, f)
		_, err := cn.ExecContext(context.Background(), "INSERT", nil)
		if !errors.Is(err, ErrOutcomeUnknown) || errors.Is(err, driver.ErrBadConn) {
			t.Fatalf("err = %v, want ErrOutcomeUnknown and not ErrBadConn", err)
		}
		if cn.IsValid() {
			t.Fatal("conn is valid after a lost link")
		}
	})
	t.Run("exec refused before sending is a bad conn", func(t *testing.T) {
		f := newFake()
		f.exec = func() (int64, int64, error) { return 0, 0, refused }
		cn := fakeConn(t, f)
		if _, err := cn.ExecContext(context.Background(), "INSERT", nil); !errors.Is(err, driver.ErrBadConn) {
			t.Fatalf("err = %v, want ErrBadConn", err)
		}
	})
	t.Run("prepare failure is a bad conn", func(t *testing.T) {
		f := newFake()
		f.prep = func(string) error { return lost }
		cn := fakeConn(t, f)
		_, err := cn.ExecContext(context.Background(), "INSERT", nil)
		if !errors.Is(err, driver.ErrBadConn) || errors.Is(err, ErrOutcomeUnknown) {
			t.Fatalf("err = %v, want ErrBadConn and not unknown (nothing ran)", err)
		}
	})
	t.Run("fetch failure is a bad conn", func(t *testing.T) {
		f := newFake()
		f.meta = intCol("x")
		f.fetch = func(int) (sqlite3.Rows, error) { return sqlite3.Rows{}, lost }
		cn := fakeConn(t, f)
		if _, err := query(t, cn, context.Background(), "SELECT x"); !errors.Is(err, driver.ErrBadConn) {
			t.Fatalf("err = %v, want ErrBadConn", err)
		}
	})
	t.Run("library failure keeps the conn", func(t *testing.T) {
		f := newFake()
		f.exec = func() (int64, int64, error) { return 0, 0, lib }
		cn := fakeConn(t, f)
		_, err := cn.ExecContext(context.Background(), "INSERT", nil)
		var se *Error
		if !errors.As(err, &se) || se.ExtendedCode != CodeConstraintUnique || !IsConstraint(err) {
			t.Fatalf("err = %v", err)
		}
		if !cn.IsValid() {
			t.Fatal("conn is invalid after a SQL error")
		}
	})
	t.Run("single statement rule is BAD_REQUEST", func(t *testing.T) {
		f := newFake()
		f.prep = func(string) error { return multi }
		cn := fakeConn(t, f)
		_, err := cn.ExecContext(context.Background(), "SELECT 1; SELECT 2", nil)
		var pe *ProtocolError
		if !errors.As(err, &pe) || pe.Code != wire.ProtoBadRequest {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("refusals of the server", func(t *testing.T) {
		for _, tc := range []struct {
			st   *rt.StatusError
			want wire.ProtoCode
			sql  int
		}{
			{&rt.StatusError{Status: 5}, wire.ProtoForbidden, 0},
			{&rt.StatusError{Status: 4}, wire.ProtoLimit, 0},
			{&rt.StatusError{Status: 3}, wire.ProtoNoStatement, 0},
			{&rt.StatusError{Status: 2}, wire.ProtoBadRequest, 0},
			{&rt.StatusError{Status: sqlite3.SVC_STATUS_META_TOO_LARGE}, wire.ProtoLimit, 0},
			{&rt.StatusError{Status: sqlite3.SVC_STATUS_SQLITE_BASE + 14, Msg: "no dir"}, 0, 14},
		} {
			if got := statusError(tc.st); got == nil {
				t.Errorf("status %d: no mapping", tc.st.Status)
			} else if tc.sql != 0 {
				var se *Error
				if !errors.As(got, &se) || se.Code != tc.sql {
					t.Errorf("status %d: %v", tc.st.Status, got)
				}
			} else {
				var pe *ProtocolError
				if !errors.As(got, &pe) || pe.Code != tc.want {
					t.Errorf("status %d: %v, want protocol code %v", tc.st.Status, got, tc.want)
				}
			}
		}
		// Internal errors and shutdown may follow the call: not a refusal.
		for _, s := range []uint32{6, 7, 8} {
			if got := statusError(&rt.StatusError{Status: s}); got != nil {
				t.Errorf("status %d mapped to %v", s, got)
			}
		}
	})
	t.Run("request over a limit keeps the conn", func(t *testing.T) {
		f := newFake()
		f.exec = func() (int64, int64, error) { return 0, 0, rt.ErrInvalidArg }
		cn := fakeConn(t, f)
		_, err := cn.ExecContext(context.Background(), "INSERT", nil)
		if err == nil || errors.Is(err, driver.ErrBadConn) || !cn.IsValid() {
			t.Fatalf("err = %v, valid %v", err, cn.IsValid())
		}
	})
}

// TestDeferredRequests checks the requests whose answers nobody needs: an ad-hoc
// statement is finalized in the shadow of the next call, a server error of that
// request is dropped, and a lost session marks the conn bad.
func TestDeferredRequests(t *testing.T) {
	f := newFake()
	cn := fakeConn(t, f)
	f.fin = func() error { return &rt.LibError{Func: "sqlite3_finalize", Code: 19, Detail: "repeated"} }
	if _, err := cn.ExecContext(context.Background(), "INSERT", nil); err != nil {
		t.Fatal(err)
	}
	if got := f.calls(); got != "prepare,exec,finalize" {
		t.Fatalf("calls = %s: the finalize must be submitted with the exec", got)
	}
	if len(cn.deferred) != 1 {
		t.Fatalf("deferred = %d", len(cn.deferred))
	}
	if err := cn.Ping(context.Background()); err != nil {
		t.Fatalf("Ping after a deferred server error: %v", err)
	}
	if len(cn.deferred) != 0 || !cn.IsValid() {
		t.Fatalf("deferred = %d, valid %v", len(cn.deferred), cn.IsValid())
	}
	f.fin = func() error {
		return &rt.TransportError{Func: "sqlite3_finalize", Err: io.ErrClosedPipe, Unknown: true}
	}
	if _, err := cn.ExecContext(context.Background(), "INSERT", nil); err != nil {
		t.Fatal(err)
	}
	if err := cn.Ping(context.Background()); !errors.Is(err, driver.ErrBadConn) {
		t.Fatalf("Ping after a deferred transport error: %v, want driver.ErrBadConn", err)
	}
}

// TestSchemaChange checks that a prepared statement whose column count changed (SQLite
// recompiled it) is read with the new columns.
func TestSchemaChange(t *testing.T) {
	f := newFake()
	f.metas = []sqlite3.StmtMeta{intCol("x"), {Columns: []sqlite3.Column{{Name: "x", Decltype: "INTEGER"}, {Name: "y", Decltype: "TEXT"}}}}
	f.fetch = func(int) (sqlite3.Rows, error) {
		return sqlite3.Rows{Rc: sqlite3.SQLITE_DONE, Ncols: 2, Cells: []sqlite3.Value{
			{Tag: sqlite3.ValueInt, Int: 1}, {Tag: sqlite3.ValueText, Text: "a"},
		}}, nil
	}
	cn := fakeConn(t, f)
	rs, err := query(t, cn, context.Background(), "SELECT *")
	if err != nil {
		t.Fatal(err)
	}
	defer rs.Close()
	if fmt.Sprint(rs.Columns()) != "[x y]" {
		t.Fatalf("columns = %v", rs.Columns())
	}
	dest := make([]driver.Value, 2)
	if err := rs.Next(dest); err != nil || dest[0] != int64(1) || dest[1] != "a" {
		t.Fatalf("row = %v, %v", dest, err)
	}
}

// TestMalformedAnswers checks that an answer that breaks the contract of the IDL
// marks the conn bad instead of being guessed at.
func TestMalformedAnswers(t *testing.T) {
	for name, rows := range map[string]sqlite3.Rows{
		"cells not a multiple": {Rc: sqlite3.SQLITE_DONE, Ncols: 2, Cells: []sqlite3.Value{{Tag: sqlite3.ValueNull}}},
		"open batch is empty":  {Rc: sqlite3.SQLITE_ROW, Ncols: 1},
		"unknown tag":          {Rc: sqlite3.SQLITE_DONE, Ncols: 1, Cells: []sqlite3.Value{{Tag: 99}}},
		"big cell not last":    {Rc: sqlite3.SQLITE_ROW, Ncols: 1, Cells: []sqlite3.Value{{Tag: sqlite3.ValueBigText, BigText: 5}, {Tag: sqlite3.ValueInt}}},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFake()
			f.meta = intCol("x")
			f.fetch = func(int) (sqlite3.Rows, error) { return rows, nil }
			f.chunk = func(int32, uint64, int) ([]byte, error) { return []byte("hello"), nil }
			cn := fakeConn(t, f)
			rs, err := query(t, cn, context.Background(), "SELECT x")
			if err == nil {
				_, err = drainRows(t, rs)
				rs.Close()
			}
			if err == nil {
				t.Fatal("malformed answer accepted")
			}
			if cn.IsValid() {
				t.Fatal("conn is valid after a malformed answer")
			}
		})
	}
}

// TestEncode checks the arrays of bind_values.
func TestEncode(t *testing.T) {
	cn := fakeConn(t, newFake())
	named := func(ord int, name string, v any) driver.NamedValue {
		return driver.NamedValue{Ordinal: ord, Name: name, Value: v}
	}
	b, err := cn.encode([]driver.NamedValue{
		named(1, "", int64(-5)), named(2, "", 1.5), named(3, "", "héllo"), named(4, "", []byte{9, 8}), named(5, "", nil),
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(b.kinds) != "[1 2 3 4 5]" || b.words[0] != uint64(math.MaxUint64-4) || b.words[1] != math.Float64bits(1.5) ||
		b.words[2] != uint64(len("héllo")) || b.words[3] != 2 || string(b.data) != "héllo\x09\x08" {
		t.Fatalf("kinds %v words %v data %q", b.kinds, b.words, b.data)
	}

	// Names map to the parameter that has them, the prefix being tried in the order : @ $.
	names := []string{"", ":a", "@a", "$b"}
	b, err = cn.encode([]driver.NamedValue{named(1, "a", int64(1)), named(2, "b", int64(2))}, names)
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(b.kinds) != "[5 1 5 1]" || b.words[1] != 1 || b.words[3] != 2 {
		t.Fatalf("named: kinds %v words %v", b.kinds, b.words)
	}
	if _, err := cn.encode([]driver.NamedValue{named(1, "zzz", int64(1))}, names); err == nil {
		t.Fatal("unknown parameter name accepted")
	} else if se := (*Error)(nil); !errors.As(err, &se) || se.Code != 25 {
		t.Fatalf("unknown name: %v, want SQLITE_RANGE", err)
	}

	// The later of two arguments for one parameter wins, with its bytes.
	b, err = cn.encode([]driver.NamedValue{named(1, "", "first"), named(1, "", "second")}, nil)
	if err != nil || fmt.Sprint(b.kinds) != "[3]" || string(b.data) != "second" || b.words[0] != 6 {
		t.Fatalf("duplicate: %v %v %q, %v", b.kinds, b.words, b.data, err)
	}

	// A single large value is not copied.
	big := make([]byte, 1<<20)
	b, err = cn.encode([]driver.NamedValue{named(1, "", int64(1)), named(2, "", big)}, nil)
	if err != nil || len(b.data) != len(big) || &b.data[0] != &big[0] {
		t.Fatalf("a single blob was copied (err %v)", err)
	}

	// Errors of the caller's input are not session failures.
	for name, arg := range map[string]driver.NamedValue{
		"ordinal":  named(0, "", int64(1)),
		"NaN":      named(1, "", math.NaN()),
		"struct":   named(1, "", struct{}{}),
		"too long": named(1, "", make([]byte, int(sqlite3.SVC_MAX_BIND_BYTES)+1)),
	} {
		_, err := cn.encode([]driver.NamedValue{arg}, nil)
		var ue userError
		if !errors.As(err, &ue) {
			t.Errorf("%s: %v is not a userError", name, err)
		}
	}
	if empty, err := cn.encode(nil, nil); err != nil || !empty.empty() {
		t.Fatalf("no arguments: %+v, %v", empty, err)
	}
}

// TestRollbackOutsideTransactionIsNoOp checks the statements of the transaction
// methods on the fake: no round trip is spent on a Rollback outside a
// transaction beyond the check.
func TestRollbackOutsideTransactionIsNoOp(t *testing.T) {
	f := newFake()
	cn := fakeConn(t, f)
	f.exec = func() (int64, int64, error) {
		return 0, 0, &rt.LibError{Func: "exec_stmt", Code: 1, Detail: "cannot rollback - no transaction is active"}
	}
	// autocommit() answers 1: the transaction is gone.
	if err := cn.rollback(); err != nil {
		t.Fatalf("rollback outside a transaction: %v", err)
	}
	if !strings.Contains(f.calls(), "autocommit") {
		t.Fatalf("calls = %s: the transaction state is SQLite's, not the driver's", f.calls())
	}
}
