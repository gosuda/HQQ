package sqlite

import (
	"context"
	"database/sql/driver"
	"errors"
	"fmt"

	"gosuda.org/hqq/bindgen/examples/sqlite3"
)

// tx is a transaction of a session. The session holds it until Commit or
// Rollback. database/sql calls them at most once and serializes them with the
// other use of the connection, so tx needs no lock of its own. The transaction
// state itself is SQLite's (sqlite3_get_autocommit): the driver does not track it.
type tx struct {
	cn   *conn
	done bool
}

var errTxDone = errors.New("sqlite: transaction has already been committed or rolled back")

// beginSQL is the BEGIN statement of each transaction mode.
var beginSQL = [...]string{
	TxDeferred:  "BEGIN DEFERRED",
	TxImmediate: "BEGIN IMMEDIATE",
	TxExclusive: "BEGIN EXCLUSIVE",
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
	mode := txModeFrom(ctx, cn.c.cfg.TxMode)
	if opts.ReadOnly {
		mode = TxDeferred
	}
	err := cn.run(ctx, "Begin", func() error {
		if opts.ReadOnly {
			return cn.beginReadOnly()
		}
		return cn.execSQL(beginSQL[mode])
	})
	if err != nil {
		return nil, err
	}
	return &tx{cn: cn}, nil
}

// beginReadOnly begins a deferred transaction in which writes fail: it remembers
// the session's query_only value, begins, and sets query_only. Commit and
// Rollback restore the value.
func (cn *conn) beginReadOnly() error {
	prev, err := cn.queryOnly()
	if err != nil {
		return err
	}
	if err := cn.execSQL(beginSQL[TxDeferred]); err != nil {
		return err
	}
	if err := cn.execSQL("PRAGMA query_only = 1"); err != nil {
		// Leave no transaction behind. The error of the rollback is of no use.
		_ = cn.execSQL("ROLLBACK")
		return err
	}
	cn.restoreQO = prev
	return nil
}

// queryOnly reads the session's query_only setting.
func (cn *conn) queryOnly() (int, error) {
	id, err := cn.cachedStmt("PRAGMA query_only")
	if err != nil {
		return 0, err
	}
	resetW, err := cn.api.ResetAsync(id)
	if err != nil {
		return 0, err
	}
	fetchW, err := cn.api.FetchRowsAsync(id, 1)
	if err != nil {
		_ = resetW()
		return 0, err
	}
	rerr := resetW()
	b, ferr := fetchW()
	if rerr != nil && !isServerError(rerr) {
		return 0, rerr
	}
	if ferr != nil {
		return 0, ferr
	}
	if b.Rc != sqlite3.SQLITE_ROW && b.Rc != sqlite3.SQLITE_DONE {
		return 0, libError(int64(b.Rc), b.Errmsg)
	}
	if len(b.Cells) != 1 || b.Ncols != 1 || b.Cells[0].Tag != sqlite3.ValueInt {
		cn.markBad()
		return 0, errors.New("sqlite: malformed answer to PRAGMA query_only")
	}
	// The statement is finished (the pragma returns one row). Leave it ready.
	if b.Cells[0].Int != 0 {
		return 1, nil
	}
	return 0, nil
}

// restoreQueryOnly puts query_only back after a read-only transaction.
func (cn *conn) restoreQueryOnly() error {
	if cn.restoreQO < 0 {
		return nil
	}
	if err := cn.execSQL(fmt.Sprintf("PRAGMA query_only = %d", cn.restoreQO)); err != nil {
		return err
	}
	cn.restoreQO = -1
	return nil
}

// Commit implements driver.Tx.
//
// A failed Commit on a live session is followed by a Rollback, because SQLite
// keeps the transaction open after such a failure (for example SQLITE_BUSY,
// PROTOCOL.md section 6.10). If the link is lost during Commit, the outcome is
// unknown and the error wraps driver.ErrBadConn.
func (t *tx) Commit() error {
	if t.done {
		return errTxDone
	}
	t.done = true
	cn := t.cn
	return cn.runTimed("Commit", func() error {
		err := cn.execSQL("COMMIT")
		if err != nil {
			if isServerError(err) && !cn.bad.Load() {
				_ = cn.rollback()
				_ = cn.restoreQueryOnly()
			}
			return err
		}
		return cn.restoreQueryOnly()
	})
}

// Rollback implements driver.Tx. A Rollback outside a transaction (the
// transaction was ended with SQL text) succeeds.
func (t *tx) Rollback() error {
	if t.done {
		return errTxDone
	}
	t.done = true
	cn := t.cn
	return cn.runTimed("Rollback", func() error {
		if err := cn.rollback(); err != nil {
			return err
		}
		return cn.restoreQueryOnly()
	})
}

// rollback runs ROLLBACK. When no transaction is active it is a successful no-op:
// the one place where the protocol differs from SQLite's own ROLLBACK
// (PROTOCOL.md section 6.10).
func (cn *conn) rollback() error {
	err := cn.execSQL("ROLLBACK")
	if err == nil || !isServerError(err) {
		return err
	}
	ac, aerr := cn.api.GetAutocommit(cn.db)
	if aerr != nil {
		return aerr
	}
	if ac != 0 {
		return nil // no transaction was active
	}
	return err
}
