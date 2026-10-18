package sqlite

import (
	"context"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"reflect"

	"gosuda.org/hqq/bindgen/examples/sqlite3"
	"gosuda.org/hqq/sqlite/internal/convert"
)

// rows streams the batches of one statement. After it has decoded a batch, it
// requests the next one (fetch-ahead), so the server steps while the caller reads,
// and at most two batches are held at a time, whatever the size of the result.
//
// The prefetch is submitted only after the previous batch is fully decoded,
// including the cells that did not fit it: fetch_rows steps first, so a request
// that went out earlier would step over a row whose big cells are still to be
// read (sqlite3.idl, Value).
type rows struct {
	cn   *conn
	ctx  context.Context // context of the query: it bounds the waits of the rows
	st   *stmt
	ncol int

	cells []sqlite3.Value // current batch, row-major
	pos   int             // index in cells of the next row
	// bigs holds the value of each BigText and BigBlob cell of the last row of the
	// batch, by index in cells.
	bigs map[int]driver.Value

	pending  func() (sqlite3.Rows, error) // fetch-ahead in flight, or nil
	final    bool                         // the batch in cells is the last: the statement ended
	endErr   error                        // io.EOF, or the failure of the step that ended the statement
	cur      []sqlite3.Value              // the row the last successful Next returned
	valid    bool                         // cur is set
	err      error                        // sticky: the error Next returns
	closed   bool
	released bool // release ran
}

func newRows(cn *conn, ctx context.Context, st *stmt) *rows {
	return &rows{cn: cn, ctx: ctx, st: st, endErr: io.EOF}
}

// ingest makes b the current batch. It reads the big cells of its last row, and
// requests the next batch unless the statement ended.
func (r *rows) ingest(b sqlite3.Rows) error {
	cn := r.cn
	if int(b.Ncols) != len(r.st.cols) {
		// The schema changed since the statement was prepared: SQLite recompiled it.
		if err := r.st.loadInfo(); err != nil {
			return err
		}
	}
	r.ncol = int(b.Ncols)
	switch {
	case r.ncol != len(r.st.cols),
		r.ncol == 0 && len(b.Cells) > 0,
		r.ncol > 0 && len(b.Cells)%r.ncol != 0:
		cn.markBad()
		return fmt.Errorf("sqlite: malformed fetch_rows answer: %d columns, %d cells, statement has %d columns",
			b.Ncols, len(b.Cells), len(r.st.cols))
	}
	r.cells, r.pos, r.bigs = b.Cells, 0, nil
	switch b.Rc {
	case sqlite3.SQLITE_ROW:
		if len(b.Cells) == 0 {
			cn.markBad()
			return errors.New("sqlite: malformed fetch_rows answer: open batch without rows")
		}
	case sqlite3.SQLITE_DONE:
		r.final = true
	default:
		// The step failed. The rows read before it are in the batch, and the
		// caller sees them before the error.
		r.final = true
		r.endErr = libError(int64(b.Rc), b.Errmsg)
	}
	if err := r.readBig(); err != nil {
		return err
	}
	if r.final {
		r.release()
		return nil
	}
	w, err := cn.api.FetchRowsAsync(r.st.id, int32(cn.fetch))
	if err != nil {
		return err
	}
	r.pending = w
	return nil
}

// readBig reads the cells of the last row of the batch that did not fit it. The
// statement is positioned on that row until the next fetch.
func (r *rows) readBig() error {
	if r.ncol == 0 || len(r.cells) == 0 {
		return nil
	}
	last := len(r.cells) - r.ncol
	for i, c := range r.cells {
		if c.Tag != sqlite3.ValueBigText && c.Tag != sqlite3.ValueBigBlob {
			continue
		}
		if i < last {
			r.cn.markBad()
			return errors.New("sqlite: malformed fetch_rows answer: a big cell before the last row")
		}
		var n uint64
		text := c.Tag == sqlite3.ValueBigText
		if text {
			n = c.BigText
		} else {
			n = c.BigBlob
		}
		v, err := r.readCell(i-last, n, text)
		if err != nil {
			return err
		}
		if r.bigs == nil {
			r.bigs = make(map[int]driver.Value)
		}
		r.bigs[i] = v
	}
	return nil
}

// readCell reads the n bytes of column col of the current row with column_chunk.
func (r *rows) readCell(col int, n uint64, text bool) (driver.Value, error) {
	if n > maxCell {
		r.cn.markBad()
		return nil, fmt.Errorf("sqlite: a cell of %d bytes is larger than the driver reads", n)
	}
	buf := make([]byte, n)
	for off := uint64(0); off < n; {
		end := min(off+uint64(sqlite3.SVC_MAX_CHUNK), n)
		got, err := r.cn.api.ColumnChunk(r.st.id, int32(col), off, buf[off:end])
		if err != nil {
			return nil, err
		}
		if len(got) == 0 {
			r.cn.markBad()
			return nil, fmt.Errorf("sqlite: column_chunk returned no bytes at offset %d of %d", off, n)
		}
		off += uint64(len(got))
	}
	if text {
		return r.st.cols[col].TextValue(buf), nil
	}
	return buf, nil
}

// Columns implements driver.Rows.
func (r *rows) Columns() []string { return r.st.colNames }

// ColumnTypeDatabaseTypeName implements driver.RowsColumnTypeDatabaseTypeName.
// It is the uppercased declared type of the column. A column with no declared
// type (an expression, for example) reports the storage class of its value in
// the current row: INTEGER, REAL, TEXT, BLOB, or NULL. Before the first row and
// after the end, it is "".
func (r *rows) ColumnTypeDatabaseTypeName(i int) string {
	if d := r.st.cols[i].Decl; d != "" {
		return d
	}
	if !r.valid {
		return ""
	}
	switch r.cur[i].Tag {
	case sqlite3.ValueInt:
		return convert.ClassInteger
	case sqlite3.ValueFloat:
		return convert.ClassReal
	case sqlite3.ValueText, sqlite3.ValueTextRaw, sqlite3.ValueBigText:
		return convert.ClassText
	case sqlite3.ValueBlob, sqlite3.ValueBigBlob:
		return convert.ClassBlob
	case sqlite3.ValueNull:
		return convert.ClassNull
	}
	return ""
}

// ColumnTypeScanType implements driver.RowsColumnTypeScanType. See
// convert.NewColumn. SQLite is dynamically typed, so a value may not match the
// affinity type: scan into an interface, a string, or a []byte in that case.
func (r *rows) ColumnTypeScanType(i int) reflect.Type { return r.st.cols[i].Scan }

// Next implements driver.Rows. It returns io.EOF after the last row, and the
// same error on every later call once the rows have failed.
func (r *rows) Next(dest []driver.Value) error {
	if r.err != nil {
		return r.err
	}
	r.valid = false
	for r.pos >= len(r.cells) {
		if r.final {
			r.err = r.endErr
			return r.err
		}
		if err := r.advance(); err != nil {
			r.err = err
			return err
		}
	}
	row := r.cells[r.pos : r.pos+r.ncol]
	for i := range row {
		v, err := r.value(r.pos+i, i, row[i])
		if err != nil {
			r.cn.markBad()
			r.err = err
			return err
		}
		dest[i] = v
	}
	r.cur = row
	r.pos += r.ncol
	r.valid = true
	return nil
}

// advance makes the next batch current: the prefetched one, or a fetch now.
func (r *rows) advance() error {
	return r.cn.run(r.ctx, "Fetch", func() error {
		w := r.pending
		r.pending = nil
		if w == nil {
			var err error
			if w, err = r.cn.api.FetchRowsAsync(r.st.id, int32(r.cn.fetch)); err != nil {
				return err
			}
		}
		b, err := w()
		if err != nil {
			// The statement is left where it was. A server error response ends
			// the rows: nothing more can be fetched from them.
			r.final = true
			return err
		}
		return r.ingest(b)
	})
}

// value converts cell c of column col, at index idx in the batch, to a driver.Value.
// Text and blobs of the batch are owned by the rows: the generated client copied
// them out of the message.
func (r *rows) value(idx, col int, c sqlite3.Value) (driver.Value, error) {
	switch c.Tag {
	case sqlite3.ValueInt:
		return c.Int, nil
	case sqlite3.ValueFloat:
		return c.Float, nil
	case sqlite3.ValueText:
		return r.st.cols[col].StringValue(c.Text), nil
	case sqlite3.ValueBlob:
		if c.Blob == nil {
			return []byte{}, nil
		}
		return c.Blob, nil
	case sqlite3.ValueNull:
		return nil, nil
	case sqlite3.ValueTextRaw:
		return r.st.cols[col].TextValue(c.TextRaw), nil
	case sqlite3.ValueBigText, sqlite3.ValueBigBlob:
		if v, ok := r.bigs[idx]; ok {
			return v, nil
		}
	}
	return nil, fmt.Errorf("sqlite: malformed fetch_rows answer: cell tag %d", c.Tag)
}

// Close implements driver.Rows. The statement is released: an ad-hoc statement is
// finalized, and a prepared one that was not read to its end is reset, so it
// does not hold its read lock. A fetch-ahead in flight is waited for first, so
// requests never overlap.
func (r *rows) Close() error {
	if r.closed {
		return nil
	}
	r.closed = true
	if r.err == nil {
		r.err = io.EOF
	}
	cn := r.cn
	if cn.closed.Load() || cn.bad.Load() {
		return nil // the session is gone, and the server released the statement
	}
	if r.pending != nil {
		w := r.pending
		r.pending = nil
		// The wait is bounded by the connector's timeout and by the query's
		// context: database/sql closes rows when their context ends.
		stop := cn.guard(r.ctx, cn.timeout)
		_, err := w()
		if stop() || (err != nil && !isServerError(err)) {
			cn.markBad()
			return nil
		}
	}
	r.release()
	return nil
}

// abandon drops the rows because another execution of the statement starts.
// The caller is inside a call of the conn.
func (r *rows) abandon() {
	if r.closed {
		return
	}
	r.closed = true
	r.err = errSuperseded
	if r.pending != nil {
		w := r.pending
		r.pending = nil
		if _, err := w(); err != nil && !isServerError(err) {
			r.cn.markBad()
		}
	}
	r.st.active = nil
}

// release lets go of the statement of the rows once: it is finalized if it is
// ad-hoc, and reset if it is prepared and the rows did not read it to its end.
func (r *rows) release() {
	if r.st.active == r {
		r.st.active = nil
	}
	if r.released {
		return
	}
	r.released = true
	switch {
	case r.st.adhoc:
		r.cn.deferFinalize(r.st.id)
	case !r.final:
		r.cn.deferReset(r.st.id)
	}
}

// maxCell is the largest text or blob the driver reads, SQLite's own limit
// (SQLITE_MAX_LENGTH, 1e9 bytes, rounded up to a power of two).
const maxCell = 1 << 30
