package sqlite

import (
	"context"
	"errors"

	"gosuda.org/hqq/sqlite/internal/wire"
)

// tx is a transaction of a session. The session holds it until Commit or
// Rollback. database/sql calls them at most once and serializes them with the
// other use of the connection, so tx needs no lock of its own.
type tx struct {
	cn   *conn
	done bool
}

var errTxDone = errors.New("sqlite: transaction has already been committed or rolled back")

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
	return t.finish(wire.MethodCommit)
}

// Rollback implements driver.Tx.
func (t *tx) Rollback() error {
	if t.done {
		return errTxDone
	}
	t.done = true
	return t.finish(wire.MethodRollback)
}

// finish sends Commit or Rollback, with the configured timeout, since the
// driver interface gives no context to these calls.
func (t *tx) finish(m wire.Method) error {
	cn := t.cn
	ctx, cancel := context.WithTimeout(context.Background(), cn.c.cfg.Timeout)
	defer cancel()
	msg, err := cn.call(ctx, m, nil)
	if err == nil {
		msg.Release()
		return nil
	}
	if m == wire.MethodCommit && !cn.bad.Load() {
		if rb, rerr := cn.call(ctx, wire.MethodRollback, nil); rerr == nil {
			rb.Release()
		}
	}
	return err
}
