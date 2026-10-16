package hqqsqlite

import (
	"context"
	"errors"

	"gosuda.org/hqq/examples/sqlite/wire"
)

// tx is a transaction of a session. The session holds it until Commit or
// Rollback.
type tx struct {
	cn   *conn
	done bool
}

var errTxDone = errors.New("hqqsqlite: transaction has already been committed or rolled back")

// Commit implements driver.Tx.
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

// finish sends Commit or Rollback. When a Commit fails on a live session, the
// transaction may still be open on the server (for example after SQLITE_BUSY),
// so a Rollback is sent after it (PROTOCOL.md section 6.10).
func (t *tx) finish(m wire.Method) error {
	cn := t.cn
	ctx, cancel := context.WithTimeout(context.Background(), cn.c.cfg.Timeout)
	defer cancel()
	msg, err := cn.call(ctx, m, nil)
	if err == nil {
		msg.Release()
		cn.setInTx(false)
		return nil
	}
	if m == wire.MethodCommit && !cn.bad.Load() {
		if rb, rerr := cn.call(ctx, wire.MethodRollback, nil); rerr == nil {
			rb.Release()
		}
	}
	cn.setInTx(false)
	return err
}

// setInTx records whether the conn has a transaction open.
func (cn *conn) setInTx(v bool) {
	cn.mu.Lock()
	cn.inTx = v
	cn.mu.Unlock()
}
