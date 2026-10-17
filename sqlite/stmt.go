package sqlite

import (
	"context"
	"database/sql/driver"
	"sync/atomic"

	"gosuda.org/hqq/sqlite/internal/wire"
)

// stmt is a prepared statement: a statement id of the session. It is valid as
// long as its connection is. Executing it again resets it on the server.
type stmt struct {
	cn      *conn
	id      uint32
	nparams int
	cols    []wire.Column
	closed  atomic.Bool
}

// Close implements driver.Stmt. The server finalizes the statement. Close is
// idempotent. On a conn that is already gone it does nothing and reports
// driver.ErrBadConn.
func (s *stmt) Close() error {
	if s.closed.Swap(true) {
		return nil
	}
	if s.cn.closed.Load() || s.cn.bad.Load() {
		return nil // the session, and with it the statement, is gone
	}
	return s.cn.closeStmt(s.id)
}

// closeStmt sends CloseStmt for a statement of the session.
func (cn *conn) closeStmt(id uint32) error {
	ctx, cancel := context.WithTimeout(context.Background(), cn.c.cfg.Timeout)
	defer cancel()
	e := wire.NewEncoder(4)
	wire.AppendIDRequest(e, wire.IDRequest{ID: id})
	msg, err := cn.call(ctx, wire.MethodCloseStmt, e.Bytes())
	if err != nil {
		return err
	}
	msg.Release()
	return nil
}

// NumInput implements driver.Stmt. It is the parameter count the server
// reports (sqlite3_bind_parameter_count) and never -1: the count is always
// known for a prepared statement.
func (s *stmt) NumInput() int { return s.nparams }

// Exec implements driver.Stmt.
func (s *stmt) Exec(args []driver.Value) (driver.Result, error) {
	return s.ExecContext(context.Background(), valuesToNamed(args))
}

// Query implements driver.Stmt.
func (s *stmt) Query(args []driver.Value) (driver.Rows, error) {
	return s.QueryContext(context.Background(), valuesToNamed(args))
}

// ExecContext implements driver.StmtExecContext.
func (s *stmt) ExecContext(ctx context.Context, args []driver.NamedValue) (driver.Result, error) {
	params, err := wireParams(args)
	if err != nil {
		return nil, err
	}
	e := wire.NewEncoder(64)
	wire.AppendStmtExecRequest(e, wire.StmtExecRequest{Stmt: s.id, Params: params})
	return s.cn.execResult(ctx, wire.MethodExecStmt, e.Bytes())
}

// QueryContext implements driver.StmtQueryContext.
func (s *stmt) QueryContext(ctx context.Context, args []driver.NamedValue) (driver.Rows, error) {
	params, err := wireParams(args)
	if err != nil {
		return nil, err
	}
	e := wire.NewEncoder(64)
	wire.AppendStmtQueryRequest(e, wire.StmtQueryRequest{Stmt: s.id, Params: params, Fetch: uint32(s.cn.fetch)})
	return s.cn.queryRows(ctx, wire.MethodQueryStmt, e.Bytes())
}

// valuesToNamed converts positional legacy arguments to named ones.
func valuesToNamed(args []driver.Value) []driver.NamedValue {
	out := make([]driver.NamedValue, len(args))
	for i, v := range args {
		out[i] = driver.NamedValue{Ordinal: i + 1, Value: v}
	}
	return out
}
