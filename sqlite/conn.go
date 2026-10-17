package sqlite

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
	"gosuda.org/hqq/sqlite/internal/convert"
	"gosuda.org/hqq/sqlite/internal/wire"
)

// Compile-time checks of the database/sql interfaces of the connection types.
var (
	_ driver.Conn                           = (*conn)(nil)
	_ driver.ConnPrepareContext             = (*conn)(nil)
	_ driver.ConnBeginTx                    = (*conn)(nil)
	_ driver.ExecerContext                  = (*conn)(nil)
	_ driver.QueryerContext                 = (*conn)(nil)
	_ driver.NamedValueChecker              = (*conn)(nil)
	_ driver.Pinger                         = (*conn)(nil)
	_ driver.SessionResetter                = (*conn)(nil)
	_ driver.Validator                      = (*conn)(nil)
	_ driver.NamedValueChecker              = (*conn)(nil)
	_ driver.Stmt                           = (*stmt)(nil)
	_ driver.StmtExecContext                = (*stmt)(nil)
	_ driver.StmtQueryContext               = (*stmt)(nil)
	_ driver.Rows                           = (*rows)(nil)
	_ driver.RowsColumnTypeDatabaseTypeName = (*rows)(nil)
	_ driver.RowsColumnTypeScanType         = (*rows)(nil)
	_ driver.Tx                             = (*tx)(nil)
	_ driver.Result                         = result{}
)

// conn is one session: one stream of the connector's link. Its requests are
// serialized by mu, so at most one request is in flight on the stream.
//
// A conn is marked bad when its stream can no longer be trusted (a transport
// failure, a cancellation mid-request, or a malformed response). A bad conn
// fails every call with driver.ErrBadConn, and IsValid reports false, so
// database/sql discards it.
type conn struct {
	c     *Connector
	link  *hqq.NamedAdvancedLink
	st    *hqq.Stream
	owner *Connector // set by Driver.Open: closed together with the conn

	mu     sync.Mutex  // serializes requests on st
	closed atomic.Bool // Close was called
	bad    atomic.Bool // the stream is unusable
	fetch  int         // rows per batch, set once by open
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
		return fmt.Errorf("sqlite: malformed Open response: %w", err)
	}
	if resp.Major != wire.ProtocolMajor {
		cn.markBad()
		return fmt.Errorf("sqlite: server speaks protocol %d.%d", resp.Major, resp.Minor)
	}
	cn.fetch = min(cn.fetch, int(min(resp.MaxFetchRows, wire.MaxFetchRows)))
	return nil
}

func busyMS(d time.Duration) uint32 {
	if d < 0 {
		return 0
	}
	return uint32(min(d.Milliseconds(), 1<<32-1))
}

// call sends one request and returns its response. The caller releases the
// message.
func (cn *conn) call(ctx context.Context, m wire.Method, body []byte) (*hqq.Message, error) {
	cn.mu.Lock()
	defer cn.mu.Unlock()
	return cn.callLocked(ctx, m, body)
}

// callLocked is call with mu held.
func (cn *conn) callLocked(ctx context.Context, m wire.Method, body []byte) (*hqq.Message, error) {
	if cn.closed.Load() || cn.bad.Load() {
		return nil, driver.ErrBadConn // nothing is sent
	}
	if len(body) > wire.MaxMessageBytes {
		return nil, fmt.Errorf("sqlite: %s request of %d bytes exceeds the %d byte limit",
			m, len(body), wire.MaxMessageBytes)
	}
	if err := ctx.Err(); err != nil {
		return nil, err // nothing is sent: the conn stays usable
	}
	if linkDead(cn.link) {
		cn.failLink()
		return nil, fmt.Errorf("%w: the link is gone", driver.ErrBadConn) // nothing is sent
	}
	msg, err := cn.st.CallWith(ctx, uint64(m), body, hqq.CallOptions{})
	if err != nil {
		return nil, cn.failure(ctx, m, err)
	}
	return msg, nil
}

// failure maps an error of a request that was sent. A server error response
// keeps the conn usable. Anything else leaves the stream in an unknown state,
// so the conn is marked bad and its stream is closed. That makes the server
// roll the session back (PROTOCOL.md section 8.3).
func (cn *conn) failure(ctx context.Context, m wire.Method, err error) error {
	var re hqq.ResponseError
	if errors.As(err, &re) && !errors.Is(err, hqq.ErrShuttingDown) {
		return errorOf(wire.Status(re.Status), string(re.Payload))
	}
	cn.markBad()
	if isTransportError(err) {
		cn.c.dropLink(cn.link)
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	if m == wire.MethodExec || m == wire.MethodExecStmt {
		// A retry could run the statement twice, so database/sql must not
		// see driver.ErrBadConn here.
		return fmt.Errorf("%w (%s): %w", ErrOutcomeUnknown, m, err)
	}
	return fmt.Errorf("%w: %w", driver.ErrBadConn, err)
}

// failLink drops the shared link of a conn whose peer is gone.
func (cn *conn) failLink() {
	cn.markBad()
	cn.c.dropLink(cn.link)
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

// Prepare implements driver.Conn.
func (cn *conn) Prepare(query string) (driver.Stmt, error) {
	return cn.PrepareContext(context.Background(), query)
}

// PrepareContext implements driver.ConnPrepareContext. The statement is
// prepared on the server; it has no effect until executed.
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
		return nil, fmt.Errorf("sqlite: malformed Prepare response: %w", err)
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
	err := cn.st.Close()
	if cn.owner != nil {
		_ = cn.owner.Close()
	}
	return err
}

// Begin implements driver.Conn.
func (cn *conn) Begin() (driver.Tx, error) {
	return cn.BeginTx(context.Background(), driver.TxOptions{})
}

// BeginTx implements driver.ConnBeginTx.
//
// SQLite is always serializable, so the default and serializable isolation
// levels are accepted and every other level is refused with an error before
// anything is sent. ReadOnly transactions begin DEFERRED with query_only set
// (PROTOCOL.md section 6.9); the BEGIN mode of other transactions comes from
// Config.TxMode or WithTxMode.
func (cn *conn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	if !sqlIsolation(opts.Isolation) {
		return nil, fmt.Errorf("sqlite: isolation level %d is not supported: SQLite is always serializable; use the default or serializable level", opts.Isolation)
	}
	req := wire.BeginRequest{Mode: txModeFrom(ctx, cn.c.cfg.TxMode), ReadOnly: opts.ReadOnly}
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
	return &tx{cn: cn}, nil
}

// sqlIsolation reports whether level is the default or serializable.
func sqlIsolation(level driver.IsolationLevel) bool {
	return level == driver.IsolationLevel(sql.LevelDefault) || level == driver.IsolationLevel(sql.LevelSerializable)
}

// CheckNamedValue implements driver.NamedValueChecker. It converts every
// argument with the rules of convert.ArgValue, so database/sql's default converter is
// never used (it would store json.RawMessage as a BLOB, for example).
func (cn *conn) CheckNamedValue(nv *driver.NamedValue) error {
	v, err := convert.ArgValue(nv.Value)
	if err != nil {
		return err
	}
	nv.Value = v
	return nil
}

// ExecContext implements driver.ExecerContext. The SQL must be one statement.
func (cn *conn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	params, err := wireParams(args)
	if err != nil {
		return nil, err
	}
	e := wire.NewEncoder(len(query) + 64)
	wire.AppendExecRequest(e, wire.ExecRequest{SQL: query, Params: params})
	return cn.execResult(ctx, wire.MethodExec, e.Bytes())
}

// QueryContext implements driver.QueryerContext. The SQL must be one statement.
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
		return nil, fmt.Errorf("sqlite: malformed %s response: %w", m, err)
	}
	if h.More && nrows == 0 {
		msg.Release()
		cn.markBad()
		return nil, fmt.Errorf("sqlite: malformed %s response: open cursor with an empty batch", m)
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
		return nil, fmt.Errorf("sqlite: malformed %s response: %w", m, err)
	}
	return result{rowsAffected: resp.RowsAffected, lastInsertID: resp.LastInsertRowID}, nil
}

// Ping implements driver.Pinger. It round-trips to the server. A failure of the
// link is driver.ErrBadConn.
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
// transaction (PROTOCOL.md section 6.10), so the cost is one round trip. Any
// failure makes the conn unusable: database/sql then discards it.
func (cn *conn) ResetSession(ctx context.Context) error {
	msg, err := cn.call(ctx, wire.MethodRollback, nil)
	if err != nil {
		return driver.ErrBadConn
	}
	msg.Release()
	return nil
}

// IsValid implements driver.Validator. A conn whose stream failed, or whose
// link is gone, is not valid, and database/sql closes it instead of pooling it.
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

// LastInsertId implements driver.Result. It is sqlite3_last_insert_rowid() of
// the session after the statement: the rowid of the last successful INSERT on
// the connection. It is meaningful only after an INSERT.
func (r result) LastInsertId() (int64, error) { return r.lastInsertID, nil }

// RowsAffected implements driver.Result. It is sqlite3_changes64() after the
// statement: the rows changed by the last INSERT, UPDATE, or DELETE. A SELECT
// or DDL statement leaves it unchanged, as SQLite does.
func (r result) RowsAffected() (int64, error) { return r.rowsAffected, nil }

// wireParams converts database/sql arguments to protocol parameters.
func wireParams(args []driver.NamedValue) ([]wire.Param, error) {
	if len(args) > wire.MaxParams {
		return nil, fmt.Errorf("sqlite: %d arguments exceed the limit of %d", len(args), wire.MaxParams)
	}
	params := make([]wire.Param, 0, len(args))
	for _, a := range args {
		if a.Ordinal < 1 {
			return nil, fmt.Errorf("sqlite: argument ordinal %d is invalid", a.Ordinal)
		}
		name, err := convert.ParamName(a.Name, wire.MaxNameBytes)
		if err != nil {
			return nil, err
		}
		v, err := convert.ArgValue(a.Value)
		if err != nil {
			return nil, fmt.Errorf("sqlite: argument %d: %w", a.Ordinal, err)
		}
		wv, err := wireValue(v)
		if err != nil {
			return nil, fmt.Errorf("sqlite: argument %d: %w", a.Ordinal, err)
		}
		params = append(params, wire.Param{Ordinal: uint32(a.Ordinal), Name: name, Value: wv})
	}
	return params, nil
}
