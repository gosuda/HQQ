package sqlite

import (
	"context"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"reflect"

	"gosuda.org/hqq"
	"gosuda.org/hqq/sqlite/internal/convert"
	"gosuda.org/hqq/sqlite/internal/wire"
)

// rows streams the batches of one cursor. While the caller reads a batch, the
// next one is fetched in the background (fetch-ahead), so at most two batches
// are held at a time, whatever the size of the result.
type rows struct {
	cn      *conn
	ctx     context.Context // context of the query: it ends the fetch-ahead too
	cursor  uint32
	cols    []wire.Column
	types   []convert.Column
	open    bool         // the server still holds the cursor
	cur     *hqq.Message // batch being read
	dec     *wire.Decoder
	left    uint32       // rows left in cur
	pending chan batch   // fetch-ahead in flight, or nil
	vals    []wire.Value // scratch row
	valid   bool         // vals holds the row the last successful Next returned
	err     error        // sticky: io.EOF, or the error that ended the rows
	closed  bool
}

// batch is the result of one Fetch, or of the first batch of a Query.
type batch struct {
	msg   *hqq.Message
	dec   *wire.Decoder // positioned at the first row
	nrows uint32
	more  bool
	err   error
}

// newRows wraps the first batch of a Query. d is positioned at its rows.
func newRows(ctx context.Context, cn *conn, msg *hqq.Message, d *wire.Decoder, h wire.ResultHeader, nrows uint32) *rows {
	r := &rows{
		cn:     cn,
		ctx:    ctx,
		cursor: h.Cursor,
		cols:   h.Columns,
		types:  make([]convert.Column, len(h.Columns)),
		open:   h.More,
		cur:    msg,
		dec:    d,
		left:   nrows,
		vals:   make([]wire.Value, len(h.Columns)),
	}
	for i, c := range h.Columns {
		r.types[i] = convert.NewColumn(c.Decltype)
	}
	if r.open {
		r.startPrefetch()
	}
	return r
}

// fetchBatch sends Fetch for n rows of the cursor and parses the reply.
func (cn *conn) fetchBatch(ctx context.Context, cursor uint32, n int) batch {
	e := wire.NewEncoder(8)
	wire.AppendFetchRequest(e, wire.FetchRequest{Cursor: cursor, NRows: uint32(n)})
	msg, err := cn.call(ctx, wire.MethodFetch, e.Bytes())
	if err != nil {
		return batch{err: err}
	}
	d := wire.NewDecoder(msg.Bytes())
	more := wire.ParseFetchResponseHeader(d)
	nrows := d.U32()
	if err := d.Err(); err != nil {
		msg.Release()
		cn.markBad()
		return batch{err: fmt.Errorf("sqlite: malformed Fetch response: %w", err)}
	}
	if more && nrows == 0 {
		msg.Release()
		cn.markBad()
		return batch{err: errors.New("sqlite: malformed Fetch response: open cursor with an empty batch")}
	}
	return batch{msg: msg, dec: d, nrows: nrows, more: more}
}

// startPrefetch fetches the next batch in the background.
func (r *rows) startPrefetch() {
	ch := make(chan batch, 1)
	r.pending = ch
	cursor, n := r.cursor, r.cn.fetch
	go func() { ch <- r.cn.fetchBatch(r.ctx, cursor, n) }()
}

// advance makes the next batch current: the prefetched one, or a fetch now.
func (r *rows) advance() error {
	var b batch
	if r.pending != nil {
		b = <-r.pending
		r.pending = nil
	} else {
		b = r.cn.fetchBatch(r.ctx, r.cursor, r.cn.fetch)
	}
	if b.err != nil {
		// Only a server error response releases the cursor. Otherwise it is
		// still open (a call that failed before it was sent, or a dead conn).
		if isServerError(b.err) {
			r.open = false
		}
		return b.err
	}
	r.open = b.more
	if r.cur != nil {
		r.cur.Release()
	}
	r.cur, r.dec, r.left = b.msg, b.dec, b.nrows
	if b.more {
		r.startPrefetch()
	}
	return nil
}

// isServerError reports whether err is an error response of the server.
func isServerError(err error) bool {
	var se *Error
	var pe *ProtocolError
	return errors.As(err, &se) || errors.As(err, &pe)
}

// Columns implements driver.Rows.
func (r *rows) Columns() []string {
	names := make([]string, len(r.cols))
	for i, c := range r.cols {
		names[i] = c.Name
	}
	return names
}

// ColumnTypeDatabaseTypeName implements driver.RowsColumnTypeDatabaseTypeName.
// It is the uppercased declared type of the column. A column with no declared
// type (an expression, for example) reports the storage class of its value in
// the current row: INTEGER, REAL, TEXT, BLOB, or NULL. Before the first row
// and after the end, it is "".
func (r *rows) ColumnTypeDatabaseTypeName(i int) string {
	if d := r.types[i].Decl; d != "" {
		return d
	}
	if !r.valid {
		return ""
	}
	switch r.vals[i].Type {
	case wire.TypeInteger:
		return convert.ClassInteger
	case wire.TypeFloat:
		return convert.ClassReal
	case wire.TypeText:
		return convert.ClassText
	case wire.TypeBlob:
		return convert.ClassBlob
	case wire.TypeNull:
		return convert.ClassNull
	}
	return ""
}

// ColumnTypeScanType implements driver.RowsColumnTypeScanType. See
// convert.NewColumn. SQLite is dynamically typed, so a value may not match the
// affinity type: scan into an interface, a string, or a []byte in that case.
func (r *rows) ColumnTypeScanType(i int) reflect.Type {
	return r.types[i].Scan
}

// Next implements driver.Rows. It returns io.EOF after the last row, and the
// same error on every later call once the rows have failed.
func (r *rows) Next(dest []driver.Value) error {
	if r.err != nil {
		return r.err
	}
	r.valid = false
	for r.left == 0 {
		if !r.open {
			r.err = io.EOF
			return io.EOF
		}
		if err := r.advance(); err != nil {
			r.err = err
			return err
		}
	}
	r.dec.Row(r.vals)
	if err := r.dec.Err(); err != nil {
		r.cn.markBad()
		r.err = fmt.Errorf("sqlite: malformed row: %w", err)
		return r.err
	}
	for i := range r.vals {
		dest[i] = r.driverValue(i, r.vals[i])
	}
	r.valid = true
	r.left--
	return nil
}

// driverValue converts a protocol value of column i to a driver.Value. Text
// and blobs are copied, because the message buffer is released with the batch.
// A TEXT value of a DATE, DATETIME, or TIMESTAMP column becomes a time.Time when
// it parses (see convert.ParseTime); otherwise it stays a string.
func (r *rows) driverValue(i int, v wire.Value) driver.Value {
	switch v.Type {
	case wire.TypeInteger:
		return v.Int
	case wire.TypeFloat:
		return v.Float
	case wire.TypeText:
		return r.types[i].TextValue(v.Bytes)
	case wire.TypeBlob:
		b := make([]byte, len(v.Bytes))
		copy(b, v.Bytes)
		return b
	}
	return nil
}

// Close implements driver.Rows. A cursor that still has rows is closed on the
// server. An open fetch-ahead is waited for first, so requests never overlap.
func (r *rows) Close() error {
	if r.closed {
		return nil
	}
	r.closed = true
	if r.err == nil {
		r.err = io.EOF
	}
	if r.pending != nil {
		b := <-r.pending
		r.pending = nil
		if b.msg != nil {
			b.msg.Release()
		}
		if b.err == nil {
			r.open = b.more
		} else {
			r.open = false
		}
	}
	if r.cur != nil {
		r.cur.Release()
		r.cur, r.dec = nil, nil
	}
	if !r.open {
		return nil
	}
	r.open = false
	if r.cn.bad.Load() || r.cn.closed.Load() {
		return nil // the session is gone: the server released the cursor
	}
	return r.cn.closeCursor(r.cursor)
}

// closeCursor sends CloseCursor, with the configured timeout.
func (cn *conn) closeCursor(cursor uint32) error {
	ctx, cancel := context.WithTimeout(context.Background(), cn.c.cfg.Timeout)
	defer cancel()
	e := wire.NewEncoder(4)
	wire.AppendIDRequest(e, wire.IDRequest{ID: cursor})
	msg, err := cn.call(ctx, wire.MethodCloseCursor, e.Bytes())
	if err != nil {
		return err
	}
	msg.Release()
	return nil
}
