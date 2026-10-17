package fakeserver

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"gosuda.org/hqq"
	"gosuda.org/hqq/sqlite/internal/wire"
)

// session is one stream: one engine connection plus its statement and cursor
// tables (PROTOCOL.md section 8). Requests run one at a time under mu, and
// teardown takes mu too, so a request is never interrupted in the middle of an
// engine call by its own teardown.
type session struct {
	srv  *Server
	ls   *linkState
	st   *hqq.Stream
	mu   sync.Mutex
	gone bool // the stream closed: nothing more is sent on it

	open    bool
	conn    *engConn
	stmts   map[uint32]*Stmt
	cursors map[uint32]*cursor
	nextID  uint32 // statement id counter
	nextCur uint32 // cursor id counter

	roActive     bool // a read-only transaction is active
	savedQueryOn bool // query_only before the read-only Begin
}

// cursor is an open result set. An ad-hoc Query owns its statement, which is
// finalized with the cursor; QueryStmt and cursors of prepared statements do
// not.
type cursor struct {
	id    uint32
	stmt  *Stmt
	owned bool
	fetch int
}

func newSession(srv *Server, ls *linkState, st *hqq.Stream) *session {
	return &session{
		srv:     srv,
		ls:      ls,
		st:      st,
		stmts:   map[uint32]*Stmt{},
		cursors: map[uint32]*cursor{},
	}
}

// watch ends the session when its stream closes (by the client, or because the
// link failed). A plain message on a stream is a protocol violation and also
// ends it.
func (s *session) watch(ctx context.Context) {
	_, _ = s.st.Recv(ctx)
	s.mu.Lock()
	s.teardownLocked()
	s.gone = true
	s.mu.Unlock()
	_ = s.st.Close()
}

// teardownLocked rolls back, finalizes, and closes the engine connection. It
// is the server behavior of PROTOCOL.md section 8.3.
func (s *session) teardownLocked() {
	if !s.open {
		return
	}
	if !s.conn.Autocommit() {
		s.srv.rolledBack.Add(1)
	}
	for id, cur := range s.cursors {
		s.releaseCursorLocked(cur)
		delete(s.cursors, id)
	}
	for id, st := range s.stmts {
		st.Finalize()
		delete(s.stmts, id)
	}
	s.conn.Close()
	s.conn = nil
	s.open = false
	s.srv.sessions.Add(-1)
}

// handle runs one request and returns the success body, or an error.
func (s *session) handle(ctx context.Context, m wire.Method, body []byte) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.gone {
		return nil, errNoSession
	}
	if m == wire.MethodClose {
		s.teardownLocked()
		return nil, nil
	}
	if m == wire.MethodOpen {
		return s.openSession(ctx, body)
	}
	if !s.open {
		if m < wire.MethodOpen || m > wire.MethodClose {
			return nil, wire.Errorf(wire.ProtoUnknownMethod, "unknown method %d", uint64(m))
		}
		return nil, wire.Errorf(wire.ProtoNoSession, "%s requires an open session", m)
	}
	out, err := s.dispatch(ctx, m, body)
	if err != nil {
		var se *sqlError
		if errors.As(err, &se) && se.rc == rcInterrupt {
			s.srv.interrupted.Add(1)
		}
	}
	return out, err
}

func (s *session) dispatch(ctx context.Context, m wire.Method, body []byte) ([]byte, error) {
	switch m {
	case wire.MethodExec:
		return s.execSQL(ctx, body)
	case wire.MethodQuery:
		return s.querySQL(ctx, body)
	case wire.MethodFetch:
		return s.fetch(ctx, body)
	case wire.MethodCloseCursor:
		return s.closeCursor(body)
	case wire.MethodPrepare:
		return s.prepare(body)
	case wire.MethodExecStmt:
		return s.execStmt(ctx, body)
	case wire.MethodQueryStmt:
		return s.queryStmt(ctx, body)
	case wire.MethodCloseStmt:
		return s.closeStmt(body)
	case wire.MethodBegin:
		return s.begin(ctx, body)
	case wire.MethodCommit:
		return s.endTx(ctx, "COMMIT")
	case wire.MethodRollback:
		return s.rollback(ctx)
	case wire.MethodPing:
		return nil, nil
	}
	return nil, wire.Errorf(wire.ProtoUnknownMethod, "unknown method %d", uint64(m))
}

func (s *session) openSession(ctx context.Context, body []byte) ([]byte, error) {
	if s.open {
		return nil, wire.Errorf(wire.ProtoAlreadyOpen, "session is already open")
	}
	// The version is checked before the rest of the body is parsed
	// (PROTOCOL.md section 6.1).
	if major := wire.OpenMajor(body); major != wire.ProtocolMajor {
		return nil, wire.Errorf(wire.ProtoVersion, "unsupported protocol major version %d", major)
	}
	req, err := wire.ParseOpenRequest(body)
	if err != nil {
		return nil, err
	}
	name, err := s.srv.resolve(req.DB)
	if err != nil {
		return nil, err
	}
	if !s.srv.reserveSession() {
		return nil, wire.Errorf(wire.ProtoLimit, "too many sessions (limit %d)", s.srv.opts.MaxSessions)
	}
	conn, err := s.srv.eng.open(name, req.Flags, msDuration(req.BusyMS))
	if err != nil {
		s.srv.sessions.Add(-1)
		return nil, err
	}
	s.conn = conn
	s.open = true
	s.srv.opened.Add(1)
	for _, p := range req.Pragmas {
		if err := conn.Exec(ctx, fmt.Sprintf("PRAGMA %s = %s", p.Name, p.Value)); err != nil {
			s.teardownLocked()
			return nil, err
		}
	}
	e := wire.NewEncoder(16)
	wire.AppendOpenResponse(e, wire.OpenResponse{
		Major:        wire.ProtocolMajor,
		Minor:        wire.ProtocolMinor,
		MaxFetchRows: wire.MaxFetchRows,
		MaxParams:    wire.MaxParams,
	})
	return e.Bytes(), nil
}

func (s *session) execSQL(ctx context.Context, body []byte) ([]byte, error) {
	req, err := wire.ParseExecRequest(body)
	if err != nil {
		return nil, err
	}
	st, err := s.prepareText(req.SQL)
	if err != nil {
		return nil, err
	}
	defer st.Finalize()
	if err := st.Bind(req.Params); err != nil {
		return nil, err
	}
	if err := runToDone(ctx, st); err != nil {
		return nil, err
	}
	return execResponse(s.conn), nil
}

func (s *session) querySQL(ctx context.Context, body []byte) ([]byte, error) {
	req, err := wire.ParseQueryRequest(body)
	if err != nil {
		return nil, err
	}
	st, err := s.prepareText(req.SQL)
	if err != nil {
		return nil, err
	}
	if err := st.Bind(req.Params); err != nil {
		st.Finalize()
		return nil, err
	}
	return s.startQuery(ctx, st, true, req.Fetch)
}

func (s *session) queryStmt(ctx context.Context, body []byte) ([]byte, error) {
	req, err := wire.ParseStmtQueryRequest(body)
	if err != nil {
		return nil, err
	}
	st, err := s.stmt(req.Stmt)
	if err != nil {
		return nil, err
	}
	s.releaseCursorOf(st)
	st.Reset()
	if err := st.Bind(req.Params); err != nil {
		return nil, err
	}
	return s.startQuery(ctx, st, false, req.Fetch)
}

func (s *session) execStmt(ctx context.Context, body []byte) ([]byte, error) {
	req, err := wire.ParseStmtExecRequest(body)
	if err != nil {
		return nil, err
	}
	st, err := s.stmt(req.Stmt)
	if err != nil {
		return nil, err
	}
	s.releaseCursorOf(st)
	st.Reset()
	if err := st.Bind(req.Params); err != nil {
		return nil, err
	}
	if err := runToDone(ctx, st); err != nil {
		return nil, err
	}
	return execResponse(s.conn), nil
}

// startQuery runs a statement to its first batch. A statement without columns
// is run to completion and returns an empty result.
func (s *session) startQuery(ctx context.Context, st *Stmt, owned bool, fetch uint32) ([]byte, error) {
	cols := st.Columns()
	if len(cols) == 0 {
		defer func() {
			if owned {
				st.Finalize()
			}
		}()
		if err := runToDone(ctx, st); err != nil {
			return nil, err
		}
		e := wire.NewEncoder(16)
		wire.AppendResultHeader(e, wire.ResultHeader{})
		e.U32(0) // empty batch
		return e.Bytes(), nil
	}
	if len(s.cursors) >= wire.MaxCursorsPerSession {
		if owned {
			st.Finalize()
		}
		return nil, wire.Errorf(wire.ProtoLimit, "too many open cursors (limit %d)", wire.MaxCursorsPerSession)
	}
	s.nextCur++
	cur := &cursor{id: s.nextCur, stmt: st, owned: owned, fetch: clampFetch(fetch)}
	s.cursors[cur.id] = cur
	e := wire.NewEncoder(4096)
	e.U32(cur.id) // patched to 0 when the result is already complete
	e.Columns(cols)
	morePos := e.Len()
	e.U8(0) // patched below
	_, more, err := s.fillBatch(ctx, cur, e, cur.fetch)
	if err != nil {
		s.dropCursor(cur)
		return nil, err
	}
	if more {
		e.PatchU8(morePos, 1)
	} else {
		s.dropCursor(cur)
		e.PatchU32(0, 0)
	}
	return e.Bytes(), nil
}

func (s *session) fetch(ctx context.Context, body []byte) ([]byte, error) {
	req, err := wire.ParseFetchRequest(body)
	if err != nil {
		return nil, err
	}
	cur := s.cursors[req.Cursor]
	if cur == nil {
		return nil, wire.Errorf(wire.ProtoNoCursor, "no open cursor %d", req.Cursor)
	}
	n := clampFetch(req.NRows)
	if req.NRows == 0 {
		n = cur.fetch
	}
	e := wire.NewEncoder(4096)
	morePos := e.Len()
	e.U8(0)
	_, more, err := s.fillBatch(ctx, cur, e, n)
	if err != nil {
		s.dropCursor(cur)
		return nil, err
	}
	if !more {
		s.dropCursor(cur)
	} else {
		e.PatchU8(morePos, 1)
	}
	return e.Bytes(), nil
}

func (s *session) closeCursor(body []byte) ([]byte, error) {
	req, err := wire.ParseIDRequest(body)
	if err != nil {
		return nil, err
	}
	cur := s.cursors[req.ID]
	if cur == nil {
		return nil, wire.Errorf(wire.ProtoNoCursor, "no open cursor %d", req.ID)
	}
	s.dropCursor(cur)
	return nil, nil
}

// fillBatch appends one batch of at most n rows to e, following
// PROTOCOL.md section 5, and reports whether the cursor has more rows.
func (s *session) fillBatch(ctx context.Context, cur *cursor, e *wire.Encoder, n int) (uint32, bool, error) {
	pos := e.BatchStart()
	var nrows uint32
	for {
		if int(nrows) >= n {
			e.BatchEnd(pos, nrows)
			return nrows, true, nil
		}
		if nrows > 0 && e.Len()-pos >= wire.MaxBatchBytes {
			e.BatchEnd(pos, nrows)
			return nrows, true, nil
		}
		vals, done, err := cur.stmt.Step(ctx)
		if err != nil {
			return 0, false, err
		}
		if done {
			e.BatchEnd(pos, nrows)
			return nrows, false, nil
		}
		for _, v := range vals {
			e.Value(v)
		}
		if e.Len() > wire.MaxMessageBytes {
			return 0, false, &sqlError{rc: rcTooBig, msg: "row exceeds message size limit"}
		}
		nrows++
	}
}

// releaseCursorLocked releases a cursor: the statement is reset, or finalized
// when the cursor owns it.
func (s *session) releaseCursorLocked(cur *cursor) {
	if cur.owned {
		cur.stmt.Finalize()
	} else {
		cur.stmt.Reset()
	}
}

// dropCursor removes a cursor and releases it.
func (s *session) dropCursor(cur *cursor) {
	if s.cursors[cur.id] == cur {
		delete(s.cursors, cur.id)
	}
	s.releaseCursorLocked(cur)
}

// releaseCursorOf closes the open cursor of a prepared statement, if any.
func (s *session) releaseCursorOf(st *Stmt) {
	for _, cur := range s.cursors {
		if cur.stmt == st {
			s.dropCursor(cur)
		}
	}
}

func (s *session) prepare(body []byte) ([]byte, error) {
	req, err := wire.ParsePrepareRequest(body)
	if err != nil {
		return nil, err
	}
	if len(s.stmts) >= wire.MaxStatementsPerSession {
		return nil, wire.Errorf(wire.ProtoLimit, "too many statements (limit %d)", wire.MaxStatementsPerSession)
	}
	st, err := s.prepareText(req.SQL)
	if err != nil {
		return nil, err
	}
	s.nextID++
	s.stmts[s.nextID] = st
	e := wire.NewEncoder(64)
	wire.AppendPrepareResponse(e, wire.PrepareResponse{
		Stmt:    s.nextID,
		NParams: uint32(st.NParams()),
		Columns: st.Columns(),
	})
	return e.Bytes(), nil
}

func (s *session) closeStmt(body []byte) ([]byte, error) {
	req, err := wire.ParseIDRequest(body)
	if err != nil {
		return nil, err
	}
	st, err := s.stmt(req.ID)
	if err != nil {
		return nil, err
	}
	s.releaseCursorOf(st)
	st.Finalize()
	delete(s.stmts, req.ID)
	return nil, nil
}

func (s *session) begin(ctx context.Context, body []byte) ([]byte, error) {
	req, err := wire.ParseBeginRequest(body)
	if err != nil {
		return nil, err
	}
	if !req.ReadOnly {
		return nil, s.conn.Exec(ctx, "BEGIN "+req.Mode.String())
	}
	if err := s.conn.Exec(ctx, "BEGIN DEFERRED"); err != nil {
		return nil, err
	}
	s.savedQueryOn = s.conn.queryOnly
	s.roActive = true
	return nil, s.conn.Exec(ctx, "PRAGMA query_only = 1")
}

// endTx runs COMMIT and restores query_only after a read-only transaction.
func (s *session) endTx(ctx context.Context, sql string) ([]byte, error) {
	if s.conn.Autocommit() {
		return nil, &sqlError{rc: rcError, msg: "cannot commit - no transaction is active"}
	}
	err := s.conn.Exec(ctx, sql)
	s.restoreReadOnly(ctx)
	return nil, err
}

// rollback is a no-op outside a transaction (PROTOCOL.md section 6.10).
func (s *session) rollback(ctx context.Context) ([]byte, error) {
	if s.conn.Autocommit() {
		return nil, nil
	}
	err := s.conn.Exec(ctx, "ROLLBACK")
	s.restoreReadOnly(ctx)
	return nil, err
}

func (s *session) restoreReadOnly(ctx context.Context) {
	if !s.roActive || !s.conn.Autocommit() {
		return
	}
	s.roActive = false
	val := "0"
	if s.savedQueryOn {
		val = "1"
	}
	_ = s.conn.Exec(ctx, "PRAGMA query_only = "+val)
}

// prepareText prepares one statement and maps the shape errors.
func (s *session) prepareText(sql string) (*Stmt, error) {
	st, err := s.conn.Prepare(sql)
	switch {
	case errors.Is(err, errMultiple):
		return nil, wire.Errorf(wire.ProtoBadRequest, "multiple statements")
	case errors.Is(err, errEmpty):
		return nil, wire.Errorf(wire.ProtoBadRequest, "empty statement")
	}
	return st, err
}

// stmt returns a prepared statement of the session.
func (s *session) stmt(id uint32) (*Stmt, error) {
	st := s.stmts[id]
	if st == nil {
		return nil, wire.Errorf(wire.ProtoNoStatement, "no statement %d", id)
	}
	return st, nil
}

// runToDone steps a statement until it is done, discarding rows.
func runToDone(ctx context.Context, st *Stmt) error {
	for {
		_, done, err := st.Step(ctx)
		if err != nil || done {
			return err
		}
	}
}

func execResponse(c *engConn) []byte {
	e := wire.NewEncoder(16)
	wire.AppendExecResponse(e, wire.ExecResponse{RowsAffected: c.Changes(), LastInsertRowID: c.LastInsertRowID()})
	return e.Bytes()
}

func msDuration(ms uint32) time.Duration {
	return time.Duration(ms) * time.Millisecond
}

func clampFetch(n uint32) int {
	if n == 0 {
		return wire.DefaultFetchRows
	}
	return int(min(n, wire.MaxFetchRows))
}
