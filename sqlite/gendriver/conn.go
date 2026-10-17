package gendriver

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	"gosuda.org/hqq"
	"gosuda.org/hqq/bindgen/examples/sqlite3"
	"gosuda.org/hqq/sqlite/internal/convert"
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
	_ driver.Stmt                           = (*stmt)(nil)
	_ driver.StmtExecContext                = (*stmt)(nil)
	_ driver.StmtQueryContext               = (*stmt)(nil)
	_ driver.Rows                           = (*rows)(nil)
	_ driver.RowsColumnTypeDatabaseTypeName = (*rows)(nil)
	_ driver.RowsColumnTypeScanType         = (*rows)(nil)
	_ driver.Tx                             = (*tx)(nil)
	_ driver.Result                         = result{}
)

// conn is one session of the generated client on one database connection of the
// server. database/sql serializes the calls of a conn, and the session is used
// from one goroutine at a time (BINDGEN.md 8.2), so a conn has no lock. The only
// concurrent access is the guard of a call, which may Kill the session from
// another goroutine; it touches nothing but the atomics below and the session's
// Kill.
//
// A conn is marked bad when its session can no longer be trusted: a lost link, a
// call abandoned because its context ended, a malformed answer. A bad conn fails
// every call with driver.ErrBadConn, and IsValid reports false, so database/sql
// discards it.
type conn struct {
	c       *Connector
	api     api
	db      dbID
	fetch   int           // rows requested per batch
	timeout time.Duration // bound of the requests that run outside a caller context

	closed atomic.Bool // Close was called
	bad    atomic.Bool // the session is unusable
	killed atomic.Bool // a guard killed the session; killCause is set
	// killCause is why the guard killed the session. It is written before killed
	// is set and read only after killed is seen set.
	killCause error

	// deferred holds the waits of requests whose answer nobody needs: the
	// finalize of an ad-hoc statement, the reset of a statement that was not read
	// to its end. They are submitted early so that the server does the work in
	// the shadow of the caller, and collected by drain before the next call.
	deferred []func() error

	// cached are statements the driver itself runs (BEGIN, COMMIT, ...), by SQL.
	cached map[string]stmtID

	// restoreQO is the query_only value to restore after a read-only transaction
	// (PROTOCOL.md 6.9), or -1.
	restoreQO int

	// Scratch of encode. The slices alias nothing the caller owns.
	vals  []driver.Value
	idx   []int
	slots []driver.Value
	kinds []uint8
	words []uint64
	data  []byte
}

// newConn wraps a dialed session. The caller opens the database.
func newConn(c *Connector, a api) *conn {
	return &conn{c: c, api: a, fetch: c.cfg.FetchSize, timeout: c.cfg.Timeout, restoreQO: -1,
		cached: make(map[string]stmtID)}
}

// Connect opens a session: it dials the service and opens the database. ctx and
// Config.Timeout bound it.
func (c *Connector) Connect(ctx context.Context) (driver.Conn, error) {
	if c.isClosed() {
		return nil, errConnectorClosed
	}
	ctx, cancel := context.WithTimeout(ctx, c.cfg.Timeout)
	defer cancel()
	opts := c.cfg.Session
	if opts.ConnectTimeout == 0 {
		opts.ConnectTimeout = c.cfg.Timeout
	}
	if opts.CloseTimeout == 0 {
		opts.CloseTimeout = c.cfg.Timeout
	}
	cl, err := sqlite3.Dial(ctx, c.listener, opts)
	if err != nil {
		// A connect that ran out of time is not a lost link: database/sql must not
		// retry it at once. Either the caller's context or the session's own connect
		// timeout ended it.
		if cerr := ctx.Err(); cerr != nil {
			return nil, fmt.Errorf("sqlite: connecting to %q: %w (%w)", c.listener, cerr, err)
		}
		if errors.Is(err, hqq.ErrTimeout) {
			return nil, fmt.Errorf("sqlite: connecting to %q: %w", c.listener, err)
		}
		return nil, transportError(err)
	}
	cn := newConn(c, genAPI{cl})
	if err := cn.open(ctx); err != nil {
		cn.discard()
		return nil, err
	}
	return cn, nil
}

// open opens the database and applies the busy timeout and the pragmas of the
// configuration, as the Open request of the hand-written protocol does.
func (cn *conn) open(ctx context.Context) error {
	cfg := cn.c.cfg
	flags := sqlite3.SQLITE_OPEN_READWRITE | sqlite3.SQLITE_OPEN_CREATE
	switch cfg.Mode {
	case ModeReadOnly:
		flags = sqlite3.SQLITE_OPEN_READONLY
	case ModeReadWrite:
		flags = sqlite3.SQLITE_OPEN_READWRITE
	}
	stop := cn.guard(ctx, 0)
	err := func() error {
		db, err := cn.api.OpenV2(cfg.Database, flags)
		if err != nil {
			return err
		}
		cn.db = db
		if err := cn.api.BusyTimeout(db, busyMS(cfg.BusyTimeout)); err != nil {
			return err
		}
		for _, p := range cfg.Pragmas {
			if err := cn.execSQL("PRAGMA " + p.Name + " = " + p.Value); err != nil {
				return err
			}
		}
		return nil
	}()
	stop()
	if err != nil {
		return cn.openFailure(ctx, err)
	}
	return nil
}

// openFailure maps a failure of the open sequence. A lost link is
// driver.ErrBadConn (nothing a retry could run twice).
func (cn *conn) openFailure(ctx context.Context, err error) error {
	if se := serverError(err); se != nil {
		return se
	}
	if cn.killed.Load() && ctx.Err() != nil {
		return ctx.Err()
	}
	return transportError(err)
}

func busyMS(d time.Duration) int32 {
	if d < 0 {
		return 0
	}
	return int32(min(d.Milliseconds(), 1<<31-1))
}

// guard arms the abandonment of the calls that follow: when ctx ends, or timeout
// (if positive) passes, the session is killed, which makes the blocked call
// return. The generated protocol has no cancellation (BINDGEN.md 12.2), so the
// call that was in flight is abandoned and its outcome is unknown; the conn is
// bad afterwards. The returned function disarms the guard and reports whether it
// fired. A context that never ends and no timeout cost nothing.
func (cn *conn) guard(ctx context.Context, timeout time.Duration) (stop func() bool) {
	if timeout > 0 {
		tctx, cancel := context.WithTimeout(ctx, timeout)
		return cn.arm(tctx, cancel)
	}
	if ctx.Done() == nil {
		return func() bool { return false }
	}
	return cn.arm(ctx, func() {})
}

// arm kills the session when ctx ends. cancel is called when the guard is
// disarmed.
//
// The kill runs on another goroutine than the owner of the session. Kill is the
// one call of the session that is safe from there (bindgen/rt Conn.Kill), and
// the fields the kill sets are atomics or are published by killed. stop does not
// return while a kill that has started is still running, so that when it reports
// that the guard fired, killed and killCause are set and no kill is left that
// could overlap a later call of the owner.
func (cn *conn) arm(ctx context.Context, cancel context.CancelFunc) (stop func() bool) {
	finished := make(chan struct{})
	disarm := context.AfterFunc(ctx, func() {
		defer close(finished)
		cn.killCause = context.Cause(ctx)
		cn.killed.Store(true)
		cn.bad.Store(true)
		cn.api.Kill()
	})
	return func() bool {
		fired := !disarm()
		cancel()
		if fired {
			<-finished
		}
		return fired
	}
}

// enter checks that the conn can start a call, and collects the answers of the
// deferred requests. It sends nothing for a conn that is closed, bad, or whose
// context is already done: the conn stays usable in the last case.
func (cn *conn) enter(ctx context.Context) error {
	if cn.closed.Load() || cn.bad.Load() {
		return driver.ErrBadConn
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(cn.deferred) > 0 {
		cn.drain()
		if cn.bad.Load() {
			return driver.ErrBadConn
		}
	}
	return nil
}

// drain waits for the deferred requests. A server answer is dropped (finalize
// repeats the error of the last step, and nobody asked); a lost session marks
// the conn bad.
func (cn *conn) drain() {
	stop := cn.guard(context.Background(), cn.timeout)
	for _, w := range cn.deferred {
		if err := w(); err != nil && !isServerError(err) {
			cn.markBad()
		}
	}
	cn.deferred = cn.deferred[:0]
	if stop() {
		cn.markBad()
	}
}

// markBad makes the conn unusable and closes its link, which ends the session on
// the server.
func (cn *conn) markBad() {
	if cn.bad.CompareAndSwap(false, true) {
		cn.api.Kill()
	}
}

// discard closes the conn after a failed Connect, without a Close request.
func (cn *conn) discard() {
	cn.closed.Store(true)
	cn.markBad()
}

// run runs one call of a driver method: fn talks to the session and returns raw
// errors, which run maps (see failure). ctx bounds the whole of fn, with the
// abandonment of guard; a nil fn error with a fired guard stays nil, because the
// work was done, but the conn is bad.
func (cn *conn) run(ctx context.Context, op string, fn func() error) error {
	if err := cn.enter(ctx); err != nil {
		return err
	}
	return cn.runEntered(ctx, op, fn)
}

// runEntered is run after enter.
func (cn *conn) runEntered(ctx context.Context, op string, fn func() error) error {
	stop := cn.guard(ctx, 0)
	err := fn()
	if stop() {
		cn.markBad()
	}
	return cn.mapFailure(ctx, op, err)
}

// runTimed is run for the calls that have no caller context: it is bounded by the
// connector's timeout.
func (cn *conn) runTimed(op string, fn func() error) error {
	if cn.closed.Load() || cn.bad.Load() {
		return driver.ErrBadConn
	}
	if len(cn.deferred) > 0 {
		cn.drain()
		if cn.bad.Load() {
			return driver.ErrBadConn
		}
	}
	stop := cn.guard(context.Background(), cn.timeout)
	err := fn()
	if stop() {
		cn.markBad()
	}
	return cn.mapFailure(context.Background(), op, err)
}

// mapFailure maps the error of fn: errors of the caller's input pass through.
func (cn *conn) mapFailure(ctx context.Context, op string, err error) error {
	var ue userError
	if errors.As(err, &ue) {
		return ue.err
	}
	return cn.failure(ctx, op, err)
}

// Prepare implements driver.Conn.
func (cn *conn) Prepare(query string) (driver.Stmt, error) {
	return cn.PrepareContext(context.Background(), query)
}

// PrepareContext implements driver.ConnPrepareContext. The statement is
// prepared on the server and its parameters and columns are read; it has no
// effect until executed.
func (cn *conn) PrepareContext(ctx context.Context, query string) (driver.Stmt, error) {
	var st *stmt
	err := cn.run(ctx, "Prepare", func() error {
		s, err := cn.prepare(query)
		if err != nil {
			return err
		}
		if err := s.loadInfo(); err != nil {
			cn.deferFinalize(s.id)
			return err
		}
		st = s
		return nil
	})
	if err != nil {
		return nil, err
	}
	return st, nil
}

// prepare compiles one statement (the single-statement rule is the server's).
func (cn *conn) prepare(query string) (*stmt, error) {
	id, err := cn.api.PrepareOne(cn.db, query)
	if err != nil {
		return nil, err
	}
	return &stmt{cn: cn, id: id, fresh: true}, nil
}

// deferFinalize finalizes a statement in the background of the next call.
func (cn *conn) deferFinalize(id stmtID) {
	w, err := cn.api.FinalizeAsync(id)
	if err != nil {
		cn.markBad()
		return
	}
	cn.deferred = append(cn.deferred, w)
}

// deferReset resets a statement in the background of the next call.
func (cn *conn) deferReset(id stmtID) {
	w, err := cn.api.ResetAsync(id)
	if err != nil {
		cn.markBad()
		return
	}
	cn.deferred = append(cn.deferred, w)
}

// cachedStmt returns the statement for sql, which the driver runs itself (BEGIN,
// COMMIT, a PRAGMA). It is prepared once per session.
func (cn *conn) cachedStmt(sql string) (stmtID, error) {
	if id, ok := cn.cached[sql]; ok {
		return id, nil
	}
	id, err := cn.api.PrepareOne(cn.db, sql)
	if err != nil {
		return 0, err
	}
	cn.cached[sql] = id
	return id, nil
}

// execSQL runs a statement of the driver's own, without parameters or result.
func (cn *conn) execSQL(sql string) error {
	id, err := cn.cachedStmt(sql)
	if err != nil {
		return err
	}
	_, _, err = cn.api.ExecStmt(id, nil, nil, nil)
	return err
}

// Close implements driver.Conn. It asks the server to end the session. Errors are
// not reported: the session ends either way.
func (cn *conn) Close() error {
	if cn.closed.Swap(true) {
		return nil
	}
	if cn.bad.Load() {
		cn.api.Kill()
		return nil
	}
	stop := cn.guard(context.Background(), cn.timeout)
	for _, w := range cn.deferred {
		_ = w()
	}
	cn.deferred = nil
	err := cn.api.Close()
	if stop() {
		cn.api.Kill()
	}
	cn.bad.Store(true)
	_ = err
	return nil
}

// Begin implements driver.Conn.
func (cn *conn) Begin() (driver.Tx, error) {
	return cn.BeginTx(context.Background(), driver.TxOptions{})
}

// CheckNamedValue implements driver.NamedValueChecker. It converts every
// argument with the rules of convert.ArgValue, so database/sql's default
// converter is never used (it would store json.RawMessage as a BLOB, for
// example).
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
	var res result
	err := cn.run(ctx, "Exec", func() error {
		st, err := cn.prepare(query)
		if err != nil {
			return err
		}
		// The statement is finalized in the shadow of the next call, whatever
		// happens below.
		defer cn.deferFinalize(st.id)
		if needsNames(args) {
			if err := st.loadInfo(); err != nil {
				return err
			}
		}
		res, err = st.exec(args)
		return err
	})
	if err != nil {
		return nil, err
	}
	return res, nil
}

// QueryContext implements driver.QueryerContext. The SQL must be one statement.
func (cn *conn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	var r *rows
	err := cn.run(ctx, "Query", func() error {
		st, err := cn.prepare(query)
		if err != nil {
			return err
		}
		st.adhoc = true
		r, err = st.query(ctx, args)
		if err != nil {
			// The rows do not exist, so nobody else finalizes the statement.
			cn.deferFinalize(st.id)
			return err
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return r, nil
}

// Ping implements driver.Pinger. It round-trips to the server. A failure of the
// link is driver.ErrBadConn.
func (cn *conn) Ping(ctx context.Context) error {
	return cn.run(ctx, "Ping", func() error {
		_, err := cn.api.GetAutocommit(cn.db)
		return err
	})
}

// ResetSession implements driver.SessionResetter. Before a pooled conn is
// reused, a transaction still open on the session, including one begun with
// plain SQL, is rolled back, and the query_only setting of a read-only
// transaction is restored. The transaction state is SQLite's own
// (sqlite3_get_autocommit), so the cost of a clean conn is one round trip. Any
// failure makes the conn unusable: database/sql then discards it.
func (cn *conn) ResetSession(ctx context.Context) error {
	err := cn.run(ctx, "ResetSession", func() error {
		ac, err := cn.api.GetAutocommit(cn.db)
		if err != nil {
			return err
		}
		if ac == 0 {
			if err := cn.execSQL("ROLLBACK"); err != nil {
				return err
			}
		}
		return cn.restoreQueryOnly()
	})
	if err != nil {
		return driver.ErrBadConn
	}
	return nil
}

// IsValid implements driver.Validator. A conn whose session failed is not valid,
// and database/sql closes it instead of pooling it.
func (cn *conn) IsValid() bool {
	if cn.bad.Load() || cn.closed.Load() {
		return false
	}
	return cn.api.Err() == nil
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

// sqlIsolation reports whether level is the default or serializable.
func sqlIsolation(level driver.IsolationLevel) bool {
	return level == driver.IsolationLevel(sql.LevelDefault) || level == driver.IsolationLevel(sql.LevelSerializable)
}

// errSuperseded is the error of rows whose statement another execution of it has
// taken over.
var errSuperseded = errors.New("sqlite: the statement was executed again: its earlier rows are released")
