package hqqsqlite

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"gosuda.org/hqq"
	"gosuda.org/hqq/examples/sqlite/wire"
)

// Compile-time checks of the database/sql interfaces of the connection types.
var (
	_ driver.Conn                           = (*conn)(nil)
	_ driver.ConnPrepareContext             = (*conn)(nil)
	_ driver.ConnBeginTx                    = (*conn)(nil)
	_ driver.ExecerContext                  = (*conn)(nil)
	_ driver.QueryerContext                 = (*conn)(nil)
	_ driver.Pinger                         = (*conn)(nil)
	_ driver.SessionResetter                = (*conn)(nil)
	_ driver.Validator                      = (*conn)(nil)
	_ driver.Stmt                           = (*stmt)(nil)
	_ driver.StmtExecContext                = (*stmt)(nil)
	_ driver.StmtQueryContext               = (*stmt)(nil)
	_ driver.Rows                           = (*rows)(nil)
	_ driver.RowsColumnTypeDatabaseTypeName = (*rows)(nil)
	_ driver.Tx                             = (*tx)(nil)
	_ driver.Result                         = result{}
)

// conn is one session: one stream of the connector's link. Its requests are
// serialized by mu, so at most one request is in flight on the stream.
type conn struct {
	c    *Connector
	link *hqq.NamedAdvancedLink
	st   *hqq.Stream

	mu       sync.Mutex  // serializes requests on st
	closed   atomic.Bool // Close was called
	inTx     bool        // a transaction was begun and not yet ended (guarded by mu)
	fetch    int         // rows per batch (set once in open)
	bad      atomic.Bool
	maxFetch int
}

// open sends Open and checks the reply.
func (cn *conn) open(ctx context.Context) error {
	flags := uint32(wire.OpenReadWrite | wire.OpenCreate)
	switch cn.c.cfg.Mode {
	case ModeReadOnly:
		flags = wire.OpenReadOnly
	case ModeReadWrite:
		flags = wire.OpenReadWrite
	}
	req := wire.OpenRequest{
		Major:  wire.ProtocolMajor,
		Minor:  wire.ProtocolMinor,
		DB:     cn.c.cfg.Database,
		Flags:  flags,
		BusyMS: busyMS(cn.c.cfg.BusyTimeout),
	}
	for _, p := range cn.c.cfg.Pragmas {
		req.Pragmas = append(req.Pragmas, wire.Pragma{Name: p.Name, Value: p.Value})
	}
	e := wire.NewEncoder(128)
	wire.AppendOpenRequest(e, req)
	msg, err := cn.call(ctx, wire.MethodOpen, e.Bytes())
	if err != nil {
		return err
	}
	resp, err := wire.ParseOpenResponse(msg.Bytes())
	msg.Release()
	if err != nil {
		cn.markBad()
		return fmt.Errorf("hqqsqlite: malformed Open response: %w", err)
	}
	if resp.Major != wire.ProtocolMajor {
		cn.markBad()
		return fmt.Errorf("hqqsqlite: server speaks protocol %d.%d", resp.Major, resp.Minor)
	}
	cn.maxFetch = int(min(resp.MaxFetchRows, wire.MaxFetchRows))
	cn.fetch = min(cn.fetch, cn.maxFetch)
	return nil
}

func busyMS(d time.Duration) uint32 {
	if d < 0 {
		return 0
	}
	return uint32(min(d.Milliseconds(), 1<<32-1))
}

// call sends one request and returns its response. The caller releases the
// message. Its context ends the call; when it does, the conn is marked bad (see
// failure).
func (cn *conn) call(ctx context.Context, m wire.Method, body []byte) (*hqq.Message, error) {
	cn.mu.Lock()
	defer cn.mu.Unlock()
	return cn.callLocked(ctx, m, body)
}

func (cn *conn) callLocked(ctx context.Context, m wire.Method, body []byte) (*hqq.Message, error) {
	if cn.closed.Load() || cn.bad.Load() {
		return nil, driver.ErrBadConn
	}
	if len(body) > wire.MaxMessageBytes {
		return nil, fmt.Errorf("hqqsqlite: %s request of %d bytes exceeds the %d byte limit",
			m, len(body), wire.MaxMessageBytes)
	}
	if err := ctx.Err(); err != nil {
		return nil, err // nothing was sent: the conn stays usable
	}
	msg, err := cn.st.CallWith(ctx, uint64(m), body, hqq.CallOptions{})
	if err != nil {
		return nil, cn.failure(ctx, err)
	}
	return msg, nil
}

// failure maps an error of a call. A server error response keeps the conn
// usable. Anything else leaves the stream in an unknown state, so the conn is
// marked bad and its stream is closed. That makes the server roll back the
// session (PROTOCOL.md section 8.3).
func (cn *conn) failure(ctx context.Context, err error) error {
	var re hqq.ResponseError
	if errors.As(err, &re) && !errors.Is(err, hqq.ErrShuttingDown) {
		return serverError(re)
	}
	cn.markBad()
	lost := errors.Is(err, hqq.ErrPeerGone) || errors.Is(err, hqq.ErrPeerClosed)
	if lost {
		cn.c.dropLink(cn.link)
	}
	if ctxErr := ctx.Err(); ctxErr != nil && !lost {
		return ctxErr
	}
	return fmt.Errorf("%w: %w", driver.ErrBadConn, err)
}

// markBad makes the conn unusable and closes its stream in the background.
func (cn *conn) markBad() {
	if cn.bad.CompareAndSwap(false, true) {
		go func() { _ = cn.st.Close() }()
	}
}

// discard closes the conn after a failed Connect, without a Close request.
func (cn *conn) discard() {
	cn.closed.Store(true)
	cn.markBad()
}

// serverError converts an error response to the driver's error types.
func serverError(re hqq.ResponseError) error {
	st := wire.Status(re.Status)
	msg := string(re.Payload)
	switch {
	case st.IsSQLite():
		return &Error{Code: st.SQLiteCode(), Message: msg}
	case st.IsProtocol():
		return &wire.ProtocolError{Code: st.ProtoCode(), Message: msg}
	}
	return fmt.Errorf("hqqsqlite: unknown error status %#x: %s", re.Status, msg)
}

// Prepare implements driver.Conn.
func (cn *conn) Prepare(query string) (driver.Stmt, error) {
	return cn.PrepareContext(context.Background(), query)
}

// PrepareContext implements driver.ConnPrepareContext.
func (cn *conn) PrepareContext(ctx context.Context, query string) (driver.Stmt, error) {
	e := wire.NewEncoder(len(query) + 8)
	wire.AppendPrepareRequest(e, wire.PrepareRequest{SQL: query})
	msg, err := cn.call(ctx, wire.MethodPrepare, e.Bytes())
	if err != nil {
		return nil, err
	}
	resp, err := wire.ParsePrepareResponse(msg.Bytes())
	msg.Release()
	if err != nil {
		cn.markBad()
		return nil, fmt.Errorf("hqqsqlite: malformed Prepare response: %w", err)
	}
	return &stmt{cn: cn, id: resp.Stmt, nparams: int(resp.NParams), cols: resp.Columns}, nil
}

// Close implements driver.Conn. It asks the server to end the session, then
// closes the stream. Errors are not reported: the session ends either way.
func (cn *conn) Close() error {
	cn.mu.Lock()
	defer cn.mu.Unlock()
	if cn.closed.Load() {
		return nil
	}
	if !cn.bad.Load() {
		ctx, cancel := context.WithTimeout(context.Background(), cn.c.cfg.Timeout)
		msg, err := cn.callLocked(ctx, wire.MethodClose, nil)
		cancel()
		if err == nil {
			msg.Release()
		}
	}
	cn.closed.Store(true)
	cn.bad.Store(true)
	return cn.st.Close()
}

// Begin implements driver.Conn.
func (cn *conn) Begin() (driver.Tx, error) {
	return cn.BeginTx(context.Background(), driver.TxOptions{})
}

// BeginTx implements driver.ConnBeginTx. Read-only transactions are DEFERRED
// (the protocol requires it). Isolation is always SERIALIZABLE, which is
// SQLite's only level, so other requested levels are refused.
func (cn *conn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	if !sqlIsolation(opts.Isolation) {
		return nil, fmt.Errorf("hqqsqlite: isolation level %d is not supported; SQLite is serializable", opts.Isolation)
	}
	mode := txModeFrom(ctx, cn.c.cfg.TxMode)
	req := wire.BeginRequest{Mode: mode, ReadOnly: opts.ReadOnly}
	if req.ReadOnly {
		req.Mode = wire.TxDeferred
	}
	e := wire.NewEncoder(2)
	wire.AppendBeginRequest(e, req)
	msg, err := cn.call(ctx, wire.MethodBegin, e.Bytes())
	if err != nil {
		return nil, err
	}
	msg.Release()
	cn.mu.Lock()
	cn.inTx = true
	cn.mu.Unlock()
	return &tx{cn: cn}, nil
}

// sqlIsolation reports whether level is the default or serializable.
func sqlIsolation(level driver.IsolationLevel) bool {
	return level == driver.IsolationLevel(sql.LevelDefault) || level == driver.IsolationLevel(sql.LevelSerializable)
}

// ExecContext implements driver.ExecerContext.
func (cn *conn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	params, err := wireParams(args)
	if err != nil {
		return nil, err
	}
	e := wire.NewEncoder(len(query) + 64)
	wire.AppendExecRequest(e, wire.ExecRequest{SQL: query, Params: params})
	return cn.execResult(ctx, wire.MethodExec, e.Bytes())
}

// QueryContext implements driver.QueryerContext.
func (cn *conn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	params, err := wireParams(args)
	if err != nil {
		return nil, err
	}
	e := wire.NewEncoder(len(query) + 64)
	wire.AppendQueryRequest(e, wire.QueryRequest{SQL: query, Params: params, Fetch: uint32(cn.fetch)})
	return cn.queryRows(ctx, wire.MethodQuery, e.Bytes())
}

// queryRows runs a Query or QueryStmt request and returns the rows.
func (cn *conn) queryRows(ctx context.Context, m wire.Method, body []byte) (driver.Rows, error) {
	msg, err := cn.call(ctx, m, body)
	if err != nil {
		return nil, err
	}
	d := wire.NewDecoder(msg.Bytes())
	h := wire.ParseResultHeader(d)
	nrows := d.U32()
	if err := d.Err(); err != nil {
		msg.Release()
		cn.markBad()
		return nil, fmt.Errorf("hqqsqlite: malformed %s response: %w", m, err)
	}
	return newRows(ctx, cn, msg, d, h, nrows), nil
}

// execResult runs an Exec or ExecStmt request.
func (cn *conn) execResult(ctx context.Context, m wire.Method, body []byte) (driver.Result, error) {
	msg, err := cn.call(ctx, m, body)
	if err != nil {
		return nil, err
	}
	resp, err := wire.ParseExecResponse(msg.Bytes())
	msg.Release()
	if err != nil {
		cn.markBad()
		return nil, fmt.Errorf("hqqsqlite: malformed %s response: %w", m, err)
	}
	return result{rowsAffected: resp.RowsAffected, lastInsertID: resp.LastInsertRowID}, nil
}

// Ping implements driver.Pinger.
func (cn *conn) Ping(ctx context.Context) error {
	msg, err := cn.call(ctx, wire.MethodPing, nil)
	if err != nil {
		return err
	}
	msg.Release()
	return nil
}

// ResetSession implements driver.SessionResetter. Before a pooled conn is
// reused, the server rolls back any transaction still open on the session,
// including one begun with plain SQL. Rollback is a no-op outside a
// transaction (PROTOCOL.md section 6.10), so the cost is one round trip.
func (cn *conn) ResetSession(ctx context.Context) error {
	if cn.bad.Load() {
		return driver.ErrBadConn
	}
	msg, err := cn.call(ctx, wire.MethodRollback, nil)
	if err != nil {
		return driver.ErrBadConn
	}
	msg.Release()
	cn.setInTx(false)
	return nil
}

// IsValid implements driver.Validator.
func (cn *conn) IsValid() bool {
	if cn.bad.Load() || cn.closed.Load() {
		return false
	}
	return !linkDead(cn.link)
}

// result implements driver.Result.
type result struct {
	rowsAffected int64
	lastInsertID int64
}

// LastInsertId implements driver.Result.
func (r result) LastInsertId() (int64, error) { return r.lastInsertID, nil }

// RowsAffected implements driver.Result.
func (r result) RowsAffected() (int64, error) { return r.rowsAffected, nil }

// wireParams converts database/sql arguments to protocol parameters.
func wireParams(args []driver.NamedValue) ([]wire.Param, error) {
	if len(args) > wire.MaxParams {
		return nil, fmt.Errorf("hqqsqlite: %d arguments exceed the limit of %d", len(args), wire.MaxParams)
	}
	params := make([]wire.Param, 0, len(args))
	for _, a := range args {
		v, err := wireValue(a.Value)
		if err != nil {
			return nil, fmt.Errorf("hqqsqlite: argument %d: %w", a.Ordinal, err)
		}
		if a.Ordinal < 1 {
			return nil, fmt.Errorf("hqqsqlite: argument ordinal %d is invalid", a.Ordinal)
		}
		params = append(params, wire.Param{Ordinal: uint32(a.Ordinal), Name: a.Name, Value: v})
	}
	return params, nil
}

// wireValue converts a driver.Value to a protocol value. Byte slices become
// BLOBs, strings TEXT, booleans 0 or 1, and times RFC 3339 TEXT with
// nanoseconds.
func wireValue(v driver.Value) (wire.Value, error) {
	switch x := v.(type) {
	case nil:
		return wire.Null(), nil
	case int64:
		return wire.Integer(x), nil
	case float64:
		return wire.Float(x), nil
	case bool:
		if x {
			return wire.Integer(1), nil
		}
		return wire.Integer(0), nil
	case []byte:
		return wire.Blob(x), nil
	case string:
		return wire.Text(x), nil
	case time.Time:
		return wire.Text(x.Format(time.RFC3339Nano)), nil
	}
	return wire.Value{}, fmt.Errorf("unsupported type %T", v)
}
