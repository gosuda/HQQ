package sqlite

import (
	"context"
	"database/sql/driver"
	"errors"

	"gosuda.org/hqq/bindgen/examples/sqlite3"
	"gosuda.org/hqq/sqlite/internal/convert"
)

// stmt is a statement of the session: a statement handle of the server, with the
// parameters and columns it reported. It is valid as long as its connection is.
// An ad-hoc statement (the one a Query or Exec on the connection makes) is
// finalized when its rows end.
type stmt struct {
	cn *conn
	id stmtID

	haveInfo bool     // the fields below are set
	nparams  int      // sqlite3_bind_parameter_count
	names    []string // the name of each parameter as SQLite spells it, or ""
	readonly bool     // sqlite3_stmt_readonly
	colNames []string
	cols     []convert.Column

	fresh  bool // never executed: it has no earlier step to reset and no bindings to clear
	adhoc  bool // finalized when its rows end
	closed bool
	active *rows // the rows that read the statement now
}

var errStmtClosed = errors.New("sqlite: statement is closed")

// loadInfo reads the parameters and columns of the statement.
func (s *stmt) loadInfo() error {
	meta, err := s.cn.api.StmtInfo(s.id)
	if err != nil {
		return err
	}
	s.setInfo(meta)
	return nil
}

// setInfo stores the answer of stmt_info.
func (s *stmt) setInfo(meta sqlite3.StmtMeta) {
	s.haveInfo = true
	s.nparams = int(meta.Nparams)
	s.names = meta.Param_names
	s.readonly = meta.Readonly
	s.colNames = make([]string, len(meta.Columns))
	s.cols = make([]convert.Column, len(meta.Columns))
	for i, c := range meta.Columns {
		s.colNames[i] = c.Name
		s.cols[i] = convert.NewColumn(c.Decltype)
	}
}

// Close implements driver.Stmt. The server finalizes the statement. Close is
// idempotent. On a conn that is already gone it does nothing.
func (s *stmt) Close() error {
	if s.closed {
		return nil
	}
	s.closed = true
	cn := s.cn
	if cn.closed.Load() || cn.bad.Load() {
		return nil // the session, and with it the statement, is gone
	}
	err := cn.runTimed("CloseStmt", func() error {
		if s.active != nil {
			s.active.abandon()
		}
		return cn.api.Finalize(s.id)
	})
	if err != nil && isServerError(err) {
		return nil // finalize repeats the error of the last step
	}
	return err
}

// NumInput implements driver.Stmt. It is the parameter count the server reports
// (sqlite3_bind_parameter_count) and never -1: the count is always known for a
// prepared statement.
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
	if s.closed {
		return nil, errStmtClosed
	}
	var res result
	err := s.cn.run(ctx, "ExecStmt", func() error {
		var err error
		res, err = s.exec(args)
		return err
	})
	if err != nil {
		return nil, err
	}
	return res, nil
}

// exec runs the statement to completion in one atomic call: reset, bind, step to
// the end, and the two counters.
func (s *stmt) exec(args []driver.NamedValue) (result, error) {
	cn := s.cn
	if s.active != nil {
		s.active.abandon()
	}
	b, err := cn.encode(args, s.names)
	if err != nil {
		return result{}, err
	}
	changes, rowid, err := cn.api.ExecStmt(s.id, b.kinds, b.words, b.data)
	if err != nil {
		return result{}, mutatingError{err}
	}
	s.fresh = false
	return result{rowsAffected: changes, lastInsertID: rowid}, nil
}

// QueryContext implements driver.StmtQueryContext.
func (s *stmt) QueryContext(ctx context.Context, args []driver.NamedValue) (driver.Rows, error) {
	if s.closed {
		return nil, errStmtClosed
	}
	var r *rows
	err := s.cn.run(ctx, "QueryStmt", func() error {
		var err error
		r, err = s.query(ctx, args)
		return err
	})
	if err != nil {
		return nil, err
	}
	return r, nil
}

// query starts the statement and returns its rows, with the first batch read.
//
// The calls are pipelined: reset, bind_values and fetch_rows go out together and
// are answered in one round trip. The fetch is committed before the bind is
// known to have succeeded, and the server runs every committed request, so the
// pipeline is only used when a failed bind cannot do harm: the statement is
// read-only, or it has nothing to bind. Otherwise the bind is waited for first.
// On an ad-hoc statement without arguments the statement info is requested in
// the same flight.
func (s *stmt) query(ctx context.Context, args []driver.NamedValue) (*rows, error) {
	cn := s.cn
	if s.active != nil {
		s.active.abandon()
	}
	if !s.haveInfo && len(args) > 0 {
		if err := s.loadInfo(); err != nil {
			return nil, err
		}
	}
	b, err := cn.encode(args, s.names)
	if err != nil {
		return nil, err
	}
	needReset := !s.fresh
	needBind := !b.empty() || (s.haveInfo && s.nparams > 0 && !s.fresh)
	pipelineFetch := !needBind || s.readonly || !s.haveInfo

	var resetW, bindW func() error
	var infoW func() (sqlite3.StmtMeta, error)
	var fetchW func() (sqlite3.Rows, error)
	var firstErr error
	// waits are consumed whatever happens, so that no future is left behind.
	note := func(err error) {
		if firstErr == nil {
			firstErr = err
		}
	}
	finish := func() {
		if resetW != nil {
			if err := resetW(); err != nil && !isServerError(err) {
				note(err) // a server error repeats the last step's: not ours
			}
			resetW = nil
		}
		if bindW != nil {
			note(bindW())
			bindW = nil
		}
		if infoW != nil {
			meta, err := infoW()
			if err != nil {
				note(err)
			} else {
				s.setInfo(meta)
			}
			infoW = nil
		}
	}

	if needReset {
		if resetW, err = cn.api.ResetAsync(s.id); err != nil {
			return nil, err
		}
	}
	if needBind {
		if bindW, err = cn.api.BindValuesAsync(s.id, b.kinds, b.words, b.data); err != nil {
			finish()
			return nil, firstOr(firstErr, err)
		}
	}
	if !s.haveInfo {
		if infoW, err = cn.api.StmtInfoAsync(s.id); err != nil {
			finish()
			return nil, firstOr(firstErr, err)
		}
	}
	if pipelineFetch {
		if fetchW, err = cn.api.FetchRowsAsync(s.id, int32(cn.fetch)); err != nil {
			finish()
			return nil, firstOr(firstErr, err)
		}
	}
	s.fresh = false
	finish()
	var batch sqlite3.Rows
	if fetchW != nil {
		var ferr error
		batch, ferr = fetchW()
		if firstErr == nil {
			firstErr = ferr
		}
	} else if firstErr == nil {
		// The bind is known to have worked: fetch now.
		w, err := cn.api.FetchRowsAsync(s.id, int32(cn.fetch))
		if err != nil {
			return nil, err
		}
		if batch, err = w(); err != nil {
			return nil, err
		}
	}
	if firstErr != nil {
		return nil, firstErr
	}
	r := newRows(cn, ctx, s)
	if err := r.ingest(batch); err != nil {
		return nil, err // nothing was released: the caller finalizes an ad-hoc statement
	}
	if !s.adhoc && !r.final {
		s.active = r
	}
	return r, nil
}

// firstOr returns first when it is set, else err.
func firstOr(first, err error) error {
	if first != nil {
		return first
	}
	return err
}

// valuesToNamed converts positional legacy arguments to named ones.
func valuesToNamed(args []driver.Value) []driver.NamedValue {
	out := make([]driver.NamedValue, len(args))
	for i, v := range args {
		out[i] = driver.NamedValue{Ordinal: i + 1, Value: v}
	}
	return out
}
