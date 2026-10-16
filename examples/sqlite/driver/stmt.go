package hqqsqlite

import (
	"context"
	"database/sql/driver"

	"gosuda.org/hqq/examples/sqlite/wire"
)

// stmt is a prepared statement: a statement id of the session.
type stmt struct {
	cn      *conn
	id      uint32
	nparams int
	cols    []wire.Column
	closed  bool
}

// Close implements driver.Stmt. The server finalizes the statement.
func (s *stmt) Close() error {
	if s.closed {
		return nil
	}
	s.closed = true
	ctx, cancel := context.WithTimeout(context.Background(), s.cn.c.cfg.Timeout)
	defer cancel()
	e := wire.NewEncoder(4)
	wire.AppendIDRequest(e, wire.IDRequest{ID: s.id})
	msg, err := s.cn.call(ctx, wire.MethodCloseStmt, e.Bytes())
	if err != nil {
		return err
	}
	msg.Release()
	return nil
}

// NumInput implements driver.Stmt.
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
