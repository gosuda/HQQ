package hqqsqlite

import (
	"context"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"

	"gosuda.org/hqq"
	"gosuda.org/hqq/examples/sqlite/wire"
)

// rows streams the batches of one cursor. While the caller reads a batch, the
// next one is fetched in the background (fetch-ahead), so at most two batches
// are held at a time, whatever the size of the result.
type rows struct {
	cn      *conn
	ctx     context.Context // context of the query: it ends the fetch-ahead too
	cursor  uint32
	cols    []wire.Column
	open    bool         // the server still holds the cursor
	cur     *hqq.Message // batch being read
	dec     *wire.Decoder
	left    uint32       // rows left in cur
	pending chan batch   // fetch-ahead in flight, or nil
	vals    []wire.Value // scratch row
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
		open:   h.More,
		cur:    msg,
		dec:    d,
		left:   nrows,
		vals:   make([]wire.Value, len(h.Columns)),
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
		return batch{err: fmt.Errorf("%w: malformed Fetch response: %w", driver.ErrBadConn, err)}
	}
	if more && nrows == 0 {
		msg.Release()
		cn.markBad()
		return batch{err: fmt.Errorf("%w: Fetch returned an empty batch with more rows", driver.ErrBadConn)}
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
	var pe *wire.ProtocolError
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
func (r *rows) ColumnTypeDatabaseTypeName(i int) string {
	return r.cols[i].Decltype
}

// Next implements driver.Rows.
func (r *rows) Next(dest []driver.Value) error {
	for r.left == 0 {
		if !r.open {
			return io.EOF
		}
		if err := r.advance(); err != nil {
			return err
		}
	}
	r.dec.Row(r.vals)
	if err := r.dec.Err(); err != nil {
		r.cn.markBad()
		return fmt.Errorf("%w: malformed row: %w", driver.ErrBadConn, err)
	}
	for i := range r.vals {
		dest[i] = driverValue(r.vals[i])
	}
	r.left--
	return nil
}

// Close implements driver.Rows. A cursor that still has rows is closed on the
// server. An open fetch-ahead is waited for first, so requests never overlap.
func (r *rows) Close() error {
	if r.closed {
		return nil
	}
	r.closed = true
	if r.pending != nil {
		b := <-r.pending
		r.pending = nil
		if b.msg != nil {
			b.msg.Release()
		}
		if b.err == nil {
			r.open = b.more
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

// driverValue converts a protocol value to a driver.Value. Text and blobs are
// copied, because the message buffer is released with the batch.
func driverValue(v wire.Value) driver.Value {
	switch v.Type {
	case wire.TypeInteger:
		return v.Int
	case wire.TypeFloat:
		return v.Float
	case wire.TypeText:
		return string(v.Bytes)
	case wire.TypeBlob:
		b := make([]byte, len(v.Bytes))
		copy(b, v.Bytes)
		return b
	}
	return nil
}
