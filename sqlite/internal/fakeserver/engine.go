package fakeserver

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"gosuda.org/hqq/sqlite/internal/wire"
)

// SQLite result codes the engine produces (sqlite3.h).
const (
	rcError             = 1
	rcBusy              = 5
	rcReadOnly          = 8
	rcInterrupt         = 9
	rcCantOpen          = 14
	rcTooBig            = 18
	rcRange             = 25
	rcConstraintNotNull = 19 | 5<<8
	rcConstraintPK      = 19 | 6<<8
	rcConstraintUnique  = 19 | 8<<8
)

// sqlError is a failure with an SQLite result code and message.
type sqlError struct {
	rc  int
	msg string
}

// Error implements error.
func (e *sqlError) Error() string { return e.msg }

// errMultiple and errEmpty are the statement-shape errors of Prepare. The
// session maps them to BAD_REQUEST.
var (
	errMultiple = errors.New("multiple statements")
	errEmpty    = errors.New("empty statement")
)

// engine holds the named databases of one server. Each name is one shared
// database; ":memory:" is private to its connection, as in SQLite.
type engine struct {
	mu  sync.Mutex
	dbs map[string]*database
}

func newEngine() *engine {
	return &engine{dbs: map[string]*database{}}
}

// database is the committed state of one database. Tables are immutable once
// published: a transaction works on its own copies (see engConn.dirty).
type database struct {
	mu     sync.Mutex // protects tables and writer
	tables map[string]*table
	writer *engConn // the connection holding the write lock, or nil
}

// table is one table. Rows are never modified in place: a change builds a new
// slice or a new row, so readers holding an older header see a stable table.
type table struct {
	name      string
	cols      []colDef
	rows      []row
	nextRowID int64
}

type row struct {
	id   int64
	vals []wire.Value
}

// open returns a connection to the named database. flags are the SQLITE_OPEN_*
// bits of the protocol; busy is the busy timeout.
func (e *engine) open(name string, flags uint32, busy time.Duration) (*engConn, error) {
	var db *database
	if name == ":memory:" {
		db = &database{tables: map[string]*table{}}
	} else {
		e.mu.Lock()
		db = e.dbs[name]
		if db == nil {
			if flags&wire.OpenCreate == 0 {
				e.mu.Unlock()
				return nil, &sqlError{rc: rcCantOpen, msg: "unable to open database: " + name}
			}
			db = &database{tables: map[string]*table{}}
			e.dbs[name] = db
		}
		e.mu.Unlock()
	}
	return &engConn{db: db, busy: busy, readOnly: flags&wire.OpenReadOnly != 0}, nil
}

// engConn is one connection: a session's view of a database, with its
// transaction state.
type engConn struct {
	db          *database
	busy        time.Duration
	readOnly    bool // opened read-only
	queryOnly   bool // PRAGMA query_only
	txActive    bool
	holdsWriter bool
	dirty       map[string]*table // tables changed by the active transaction
	dropped     map[string]bool   // tables dropped by the active transaction
	changes     int64
	lastRowID   int64
}

// Autocommit reports whether no transaction is active.
func (c *engConn) Autocommit() bool { return !c.txActive }

// Changes returns the rows changed by the last INSERT, UPDATE, or DELETE.
func (c *engConn) Changes() int64 { return c.changes }

// LastInsertRowID returns the rowid of the last successful INSERT.
func (c *engConn) LastInsertRowID() int64 { return c.lastRowID }

// Close rolls back an active transaction and releases the write lock.
func (c *engConn) Close() {
	if c.txActive {
		c.rollbackInternal()
	}
	c.releaseWriter()
}

// Exec prepares, runs, and finalizes one statement without parameters.
func (c *engConn) Exec(ctx context.Context, sql string) error {
	s, err := c.Prepare(sql)
	if err != nil {
		return err
	}
	defer s.Finalize()
	for {
		_, done, err := s.Step(ctx)
		if err != nil || done {
			return err
		}
	}
}

// Prepare parses one statement and resolves its columns and parameters. Tables
// it names must exist.
func (c *engConn) Prepare(sql string) (*Stmt, error) {
	ast, p, err := parse(sql)
	if err != nil {
		return nil, err
	}
	s := &Stmt{c: c, ast: ast, nparams: p.maxParam, named: p.named}
	s.binds = make([]wire.Value, s.nparams+1)
	for i := range s.binds {
		s.binds[i] = wire.Null()
	}
	if s.cols, err = c.describe(ast); err != nil {
		return nil, err
	}
	return s, nil
}

// describe resolves the result columns of a statement at prepare time.
func (c *engConn) describe(st *statement) ([]wire.Column, error) {
	switch st.kind {
	case kSelect:
		var cols []colDef
		switch {
		case st.from == nil:
			for _, it := range st.items {
				if it.star {
					return nil, &sqlError{rc: rcError, msg: "no tables specified"}
				}
			}
		case st.from.series:
			cols = seriesCols
		default:
			t := c.lookup(st.from.table)
			if t == nil {
				return nil, &sqlError{rc: rcError, msg: "no such table: " + st.from.table}
			}
			cols = t.cols
		}
		var out []wire.Column
		for _, it := range st.items {
			if it.star {
				for _, col := range cols {
					out = append(out, wire.Column{Name: col.name, Decltype: col.decl})
				}
				continue
			}
			decl := ""
			if ce, ok := it.e.(colExpr); ok {
				i, err := resolveCol(cols, ce.name)
				if err != nil {
					return nil, err
				}
				decl = cols[i].decl
			}
			out = append(out, wire.Column{Name: it.name, Decltype: decl})
		}
		return out, nil
	case kInsert, kUpdate, kDelete:
		if c.lookup(st.table) == nil {
			return nil, &sqlError{rc: rcError, msg: "no such table: " + st.table}
		}
	case kDropTable:
		if c.lookup(st.table) == nil && !st.ifExists {
			return nil, &sqlError{rc: rcError, msg: "no such table: " + st.table}
		}
	}
	return nil, nil
}

// seriesCols is the single column of hqq_series.
var seriesCols = []colDef{{name: "value"}}

// lookup returns the table as this connection sees it, or nil.
func (c *engConn) lookup(name string) *table {
	if c.txActive {
		if c.dropped[name] {
			return nil
		}
		if t := c.dirty[name]; t != nil {
			return t
		}
	}
	c.db.mu.Lock()
	defer c.db.mu.Unlock()
	return c.db.tables[name]
}

// Stmt is a prepared statement of an engConn.
type Stmt struct {
	c       *engConn
	ast     *statement
	nparams int
	named   map[string]int
	binds   []wire.Value // binds[i] is parameter i (1-based); index 0 unused
	cols    []wire.Column
	started bool
	it      *selIter
	rowBuf  []wire.Value
}

// NParams returns the number of parameters (sqlite3_bind_parameter_count).
func (s *Stmt) NParams() int { return s.nparams }

// Columns returns the result columns.
func (s *Stmt) Columns() []wire.Column { return s.cols }

// Bind sets the parameters of the next execution. Parameters not in ps are
// NULL. Bytes are copied, so ps may alias a request buffer that is reused.
func (s *Stmt) Bind(ps []wire.Param) error {
	for i := range s.binds {
		s.binds[i] = wire.Null()
	}
	for _, p := range ps {
		idx := int(p.Ordinal)
		if p.Name != "" {
			idx = 0
			for _, prefix := range []string{":", "@", "$"} {
				if i, ok := s.named[prefix+p.Name]; ok {
					idx = i
					break
				}
			}
			if idx == 0 {
				return &sqlError{rc: rcRange, msg: "no such parameter: " + p.Name}
			}
		}
		if idx < 1 || idx > s.nparams {
			return &sqlError{rc: rcRange, msg: "parameter index out of range"}
		}
		s.binds[idx] = copyValue(p.Value)
	}
	return nil
}

// Reset ends the current execution. The bindings are kept until the next Bind.
func (s *Stmt) Reset() {
	s.it = nil
	s.started = false
}

// Finalize releases the statement.
func (s *Stmt) Finalize() {
	s.Reset()
}

// Step runs the statement until its next row. It returns done = true when
// there are no more rows. Row values are valid until the next Step.
func (s *Stmt) Step(ctx context.Context) (vals []wire.Value, done bool, err error) {
	if s.ast.kind != kSelect {
		if s.started {
			return nil, true, nil
		}
		s.started = true
		return nil, true, s.c.execute(ctx, s)
	}
	if !s.started {
		it, err := s.c.newSelect(ctx, s)
		if err != nil {
			return nil, true, err
		}
		s.it = it
		s.started = true
	}
	if s.it == nil {
		return nil, true, nil
	}
	vals, ok, err := s.it.next(ctx)
	if err != nil || !ok {
		s.it = nil
		return nil, true, err
	}
	return vals, false, nil
}

// execute runs a statement that returns no rows.
func (c *engConn) execute(ctx context.Context, s *Stmt) error {
	st := s.ast
	switch st.kind {
	case kCreateTable:
		return c.write(ctx, func() error { return c.createTable(st) })
	case kDropTable:
		return c.write(ctx, func() error { return c.dropTable(st) })
	case kInsert:
		return c.write(ctx, func() error { return c.insert(ctx, s) })
	case kUpdate:
		return c.write(ctx, func() error { return c.update(ctx, s) })
	case kDelete:
		return c.write(ctx, func() error { return c.deleteRows(ctx, s) })
	case kBegin:
		return c.begin(ctx, st.mode)
	case kCommit:
		if !c.txActive {
			return &sqlError{rc: rcError, msg: "cannot commit - no transaction is active"}
		}
		return c.commitInternal()
	case kRollback:
		if !c.txActive {
			return &sqlError{rc: rcError, msg: "cannot rollback - no transaction is active"}
		}
		c.rollbackInternal()
		return nil
	case kPragma:
		return c.pragma(ctx, s)
	}
	return &sqlError{rc: rcError, msg: "unsupported statement"}
}

func (c *engConn) pragma(ctx context.Context, s *Stmt) error {
	st := s.ast
	if st.pragmaV == nil {
		return nil
	}
	v := st.pragmaV
	if ce, ok := v.(colExpr); ok { // PRAGMA name = word
		v = litExpr{wire.Text(ce.name)}
	}
	val, err := s.eval(ctx, v, nil, nil)
	if err != nil {
		return err
	}
	switch st.pragma {
	case "query_only":
		c.queryOnly = asInt(val) != 0
	case "busy_timeout":
		c.busy = time.Duration(asInt(val)) * time.Millisecond
	}
	return nil
}

// write runs fn as a write. Outside a transaction it is an implicit
// transaction of its own. A failing statement leaves the transaction as it was
// before the statement, as SQLite does.
func (c *engConn) write(ctx context.Context, fn func() error) error {
	if c.readOnly || c.queryOnly {
		return &sqlError{rc: rcReadOnly, msg: "attempt to write a readonly database"}
	}
	implicit := !c.txActive
	if implicit {
		c.txActive = true
		c.dirty = map[string]*table{}
		c.dropped = map[string]bool{}
	}
	if err := c.lockWriter(ctx); err != nil {
		if implicit {
			c.rollbackInternal()
		}
		return err
	}
	savedDirty := maps.Clone(c.dirty)
	savedDropped := maps.Clone(c.dropped)
	if err := fn(); err != nil {
		c.dirty, c.dropped = savedDirty, savedDropped
		if implicit {
			c.rollbackInternal()
		}
		return err
	}
	if implicit {
		return c.commitInternal()
	}
	return nil
}

// lockWriter takes the database write lock, waiting up to the busy timeout.
func (c *engConn) lockWriter(ctx context.Context) error {
	if c.holdsWriter {
		return nil
	}
	deadline := time.Now().Add(c.busy)
	for {
		c.db.mu.Lock()
		if c.db.writer == nil {
			c.db.writer = c
			c.holdsWriter = true
			c.db.mu.Unlock()
			return nil
		}
		c.db.mu.Unlock()
		if c.busy <= 0 || !time.Now().Before(deadline) {
			return &sqlError{rc: rcBusy, msg: "database is locked"}
		}
		select {
		case <-ctx.Done():
			return interrupted()
		case <-time.After(time.Millisecond):
		}
	}
}

func (c *engConn) releaseWriter() {
	c.db.mu.Lock()
	if c.db.writer == c {
		c.db.writer = nil
	}
	c.db.mu.Unlock()
	c.holdsWriter = false
}

func (c *engConn) begin(ctx context.Context, mode wire.TxMode) error {
	if c.txActive {
		return &sqlError{rc: rcError, msg: "cannot start a transaction within a transaction"}
	}
	c.txActive = true
	c.dirty = map[string]*table{}
	c.dropped = map[string]bool{}
	if mode != wire.TxDeferred {
		if err := c.lockWriter(ctx); err != nil {
			c.rollbackInternal()
			return err
		}
	}
	return nil
}

// commitInternal publishes the transaction's tables.
func (c *engConn) commitInternal() error {
	c.db.mu.Lock()
	for name := range c.dropped {
		delete(c.db.tables, name)
	}
	for name, t := range c.dirty {
		c.db.tables[name] = t
	}
	if c.db.writer == c {
		c.db.writer = nil
	}
	c.db.mu.Unlock()
	c.holdsWriter = false
	c.txActive = false
	c.dirty, c.dropped = nil, nil
	return nil
}

// rollbackInternal discards the transaction.
func (c *engConn) rollbackInternal() {
	c.releaseWriter()
	c.txActive = false
	c.dirty, c.dropped = nil, nil
}

// writable returns the transaction's private copy of a table, creating it on
// the first change. The copy shares the committed rows until a change replaces
// them.
func (c *engConn) writable(name string) (*table, error) {
	t := c.lookup(name)
	if t == nil {
		return nil, &sqlError{rc: rcError, msg: "no such table: " + name}
	}
	nt := *t
	c.dirty[name] = &nt
	delete(c.dropped, name)
	return &nt, nil
}

func (c *engConn) createTable(st *statement) error {
	if c.lookup(st.table) != nil {
		if st.ifExists {
			return nil
		}
		return &sqlError{rc: rcError, msg: "table " + st.table + " already exists"}
	}
	c.dirty[st.table] = &table{name: st.table, cols: st.cols}
	delete(c.dropped, st.table)
	return nil
}

func (c *engConn) dropTable(st *statement) error {
	if c.lookup(st.table) == nil {
		if st.ifExists {
			return nil
		}
		return &sqlError{rc: rcError, msg: "no such table: " + st.table}
	}
	delete(c.dirty, st.table)
	c.dropped[st.table] = true
	return nil
}

// insert runs an INSERT statement.
func (c *engConn) insert(ctx context.Context, s *Stmt) error {
	st := s.ast
	c.changes = 0
	t, err := c.writable(st.table)
	if err != nil {
		return err
	}
	idx := make([]int, 0, len(t.cols))
	if len(st.insCols) == 0 {
		for i := range t.cols {
			idx = append(idx, i)
		}
	}
	for _, name := range st.insCols {
		i, err := resolveCol(t.cols, name)
		if err != nil {
			return err
		}
		idx = append(idx, i)
	}
	for _, exprs := range st.rows {
		if len(exprs) != len(idx) {
			return &sqlError{rc: rcError, msg: fmt.Sprintf("%d values for %d columns", len(exprs), len(idx))}
		}
		vals := make([]wire.Value, len(t.cols))
		for i := range vals {
			vals[i] = wire.Null()
		}
		for i, e := range exprs {
			v, err := s.eval(ctx, e, nil, nil)
			if err != nil {
				return err
			}
			vals[idx[i]] = copyValue(v)
		}
		// An INTEGER PRIMARY KEY is the rowid: NULL takes the next one, and an
		// integer value names the row.
		id := t.nextRowID + 1
		if pk := t.rowidAlias(); pk >= 0 {
			switch vals[pk].Type {
			case wire.TypeNull:
				vals[pk] = wire.Integer(id)
			case wire.TypeInteger:
				id = vals[pk].Int
			}
		}
		if err := checkRow(t, vals, -1); err != nil {
			return err
		}
		t.nextRowID = max(t.nextRowID, id)
		t.rows = append(t.rows, row{id: id, vals: vals})
		c.changes++
		c.lastRowID = id
	}
	return nil
}

// update runs an UPDATE statement.
func (c *engConn) update(ctx context.Context, s *Stmt) error {
	st := s.ast
	t, err := c.writable(st.table)
	if err != nil {
		return err
	}
	setIdx := make([]int, len(st.setCols))
	for k, name := range st.setCols {
		if setIdx[k], err = resolveCol(t.cols, name); err != nil {
			return err
		}
	}
	newRows := slices.Clone(t.rows)
	var changed int64
	for i, r := range t.rows {
		ok, err := s.matches(ctx, st.where, t.cols, r.vals)
		if err != nil {
			return err
		}
		if !ok {
			continue
		}
		nv := slices.Clone(r.vals)
		for k, e := range st.setVals {
			v, err := s.eval(ctx, e, t.cols, r.vals)
			if err != nil {
				return err
			}
			nv[setIdx[k]] = copyValue(v)
		}
		if err := checkRow(t, nv, i); err != nil {
			return err
		}
		newRows[i] = row{id: r.id, vals: nv}
		changed++
	}
	t.rows = newRows
	c.changes = changed
	return nil
}

// deleteRows runs a DELETE statement.
func (c *engConn) deleteRows(ctx context.Context, s *Stmt) error {
	st := s.ast
	t, err := c.writable(st.table)
	if err != nil {
		return err
	}
	kept := make([]row, 0, len(t.rows))
	var removed int64
	for _, r := range t.rows {
		ok, err := s.matches(ctx, st.where, t.cols, r.vals)
		if err != nil {
			return err
		}
		if ok {
			removed++
			continue
		}
		kept = append(kept, r)
	}
	t.rows = kept
	c.changes = removed
	return nil
}

// checkRow enforces NOT NULL, PRIMARY KEY, and UNIQUE for vals. self is the
// index of the row being replaced, or -1 for an insert.
func checkRow(t *table, vals []wire.Value, self int) error {
	for i, col := range t.cols {
		v := vals[i]
		if v.Type == wire.TypeNull {
			if col.notNull {
				return &sqlError{rc: rcConstraintNotNull, msg: "NOT NULL constraint failed: " + t.name + "." + col.name}
			}
			continue
		}
		if !col.primaryKey && !col.unique {
			continue
		}
		for j, r := range t.rows {
			if j == self || !valuesEqual(r.vals[i], v) {
				continue
			}
			rc := rcConstraintUnique
			if col.primaryKey {
				rc = rcConstraintPK
			}
			return &sqlError{rc: rc, msg: "UNIQUE constraint failed: " + t.name + "." + col.name}
		}
	}
	return nil
}

// rowidAlias returns the index of the INTEGER PRIMARY KEY column, which SQLite
// makes the rowid, or -1.
func (t *table) rowidAlias() int {
	for i, c := range t.cols {
		if c.primaryKey && strings.EqualFold(c.decl, "INTEGER") {
			return i
		}
	}
	return -1
}

// selIter produces the rows of one SELECT.
type selIter struct {
	s       *Stmt
	cols    []colDef // environment for expressions
	rows    []row    // table mode: snapshot of the committed or transaction rows
	idx     int
	mode    selMode
	cur     int64 // series mode: next value
	end     int64 // series mode: last value
	emitted int64
	limit   int64 // -1: unlimited
	done    bool  // selOnce: the one row was produced
}

type selMode uint8

const (
	selOnce selMode = iota
	selSeries
	selTable
)

func (c *engConn) newSelect(ctx context.Context, s *Stmt) (*selIter, error) {
	st := s.ast
	it := &selIter{s: s, limit: -1}
	if st.limit != nil {
		v, err := s.eval(ctx, st.limit, nil, nil)
		if err != nil {
			return nil, err
		}
		if n := asInt(v); n >= 0 {
			it.limit = n
		}
	}
	switch {
	case st.from == nil:
		it.mode = selOnce
	case st.from.series:
		a, err := s.eval(ctx, st.from.args[0], nil, nil)
		if err != nil {
			return nil, err
		}
		b, err := s.eval(ctx, st.from.args[1], nil, nil)
		if err != nil {
			return nil, err
		}
		it.mode, it.cols = selSeries, seriesCols
		it.cur, it.end = asInt(a), asInt(b)
	default:
		t := c.lookup(st.from.table)
		if t == nil {
			return nil, &sqlError{rc: rcError, msg: "no such table: " + st.from.table}
		}
		it.mode, it.cols, it.rows = selTable, t.cols, t.rows
	}
	return it, nil
}

// next returns the next row of the SELECT. The slice is valid until the next
// call.
func (it *selIter) next(ctx context.Context) ([]wire.Value, bool, error) {
	if err := ctx.Err(); err != nil {
		return nil, false, interrupted()
	}
	if it.limit >= 0 && it.emitted >= it.limit {
		return nil, false, nil
	}
	for {
		vals, ok := it.raw()
		if !ok {
			return nil, false, nil
		}
		match, err := it.s.matches(ctx, it.s.ast.where, it.cols, vals)
		if err != nil {
			return nil, false, err
		}
		if !match {
			continue
		}
		out, err := it.s.project(ctx, it.cols, vals)
		if err != nil {
			return nil, false, err
		}
		it.emitted++
		return out, true, nil
	}
}

// raw returns the next source row of the SELECT before WHERE and the select
// list: the one row of a FROM-less SELECT, a series value, or a table row.
func (it *selIter) raw() ([]wire.Value, bool) {
	switch it.mode {
	case selOnce:
		if it.done {
			return nil, false
		}
		it.done = true
		return nil, true
	case selSeries:
		if it.cur > it.end {
			return nil, false
		}
		v := []wire.Value{wire.Integer(it.cur)}
		it.cur++
		return v, true
	}
	if it.idx >= len(it.rows) {
		return nil, false
	}
	it.idx++
	return it.rows[it.idx-1].vals, true
}

// project evaluates the select list over one row.
func (s *Stmt) project(ctx context.Context, cols []colDef, vals []wire.Value) ([]wire.Value, error) {
	out := s.rowBuf[:0]
	for _, it := range s.ast.items {
		if it.star {
			out = append(out, vals...)
			continue
		}
		v, err := s.eval(ctx, it.e, cols, vals)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	s.rowBuf = out
	return out, nil
}

// matches reports whether the row satisfies the WHERE conditions.
func (s *Stmt) matches(ctx context.Context, conds []cond, cols []colDef, vals []wire.Value) (bool, error) {
	for _, c := range conds {
		i, err := resolveCol(cols, c.col)
		if err != nil {
			return false, err
		}
		v, err := s.eval(ctx, c.val, cols, vals)
		if err != nil {
			return false, err
		}
		if !compareHolds(c.op, vals[i], v) {
			return false, nil
		}
	}
	return true, nil
}

// eval evaluates an expression. cols and vals are the row environment.
func (s *Stmt) eval(ctx context.Context, e expr, cols []colDef, vals []wire.Value) (wire.Value, error) {
	switch x := e.(type) {
	case litExpr:
		return x.v, nil
	case paramExpr:
		if x.idx >= 1 && x.idx < len(s.binds) {
			return s.binds[x.idx], nil
		}
		return wire.Null(), nil
	case colExpr:
		i, err := resolveCol(cols, x.name)
		if err != nil {
			return wire.Value{}, err
		}
		return vals[i], nil
	case callExpr:
		if x.fn != "hqq_sleep" {
			return wire.Value{}, &sqlError{rc: rcError, msg: "no such function: " + x.fn}
		}
		if len(x.args) != 1 {
			return wire.Value{}, &sqlError{rc: rcError, msg: "wrong number of arguments to function hqq_sleep()"}
		}
		arg, err := s.eval(ctx, x.args[0], cols, vals)
		if err != nil {
			return wire.Value{}, err
		}
		ms := max(asInt(arg), 0)
		timer := time.NewTimer(time.Duration(ms) * time.Millisecond)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return wire.Value{}, interrupted()
		case <-timer.C:
		}
		return wire.Integer(ms), nil
	}
	return wire.Value{}, &sqlError{rc: rcError, msg: "unsupported expression"}
}

// interrupted is the error of an operation ended by its context.
func interrupted() error {
	return &sqlError{rc: rcInterrupt, msg: "interrupted"}
}

// resolveCol returns the index of a column by name.
func resolveCol(cols []colDef, name string) (int, error) {
	for i, c := range cols {
		if c.name == name {
			return i, nil
		}
	}
	return 0, &sqlError{rc: rcError, msg: "no such column: " + name}
}

// valuesEqual is SQL equality: NULL equals nothing.
func valuesEqual(a, b wire.Value) bool {
	return compareHolds("=", a, b)
}

// compareHolds evaluates "a op b" with SQL semantics: a comparison with NULL is
// false. Values of different storage classes order as SQLite orders them:
// numbers, then text, then blobs.
func compareHolds(op string, a, b wire.Value) bool {
	if a.Type == wire.TypeNull || b.Type == wire.TypeNull {
		return false
	}
	c := compareValues(a, b)
	switch op {
	case "=", "==":
		return c == 0
	case "!=", "<>":
		return c != 0
	case "<":
		return c < 0
	case "<=":
		return c <= 0
	case ">":
		return c > 0
	case ">=":
		return c >= 0
	}
	return false
}

// compareValues orders two non-NULL values: -1, 0, or 1.
func compareValues(a, b wire.Value) int {
	ra, rb := storageRank(a), storageRank(b)
	if ra != rb {
		return cmpInt(ra, rb)
	}
	switch ra {
	case 0:
		if a.Type == wire.TypeInteger && b.Type == wire.TypeInteger {
			return cmpInt64(a.Int, b.Int)
		}
		fa, fb := asFloat(a), asFloat(b)
		switch {
		case fa < fb:
			return -1
		case fa > fb:
			return 1
		}
		return 0
	case 1, 2:
		return bytesCompare(a.Bytes, b.Bytes)
	}
	return 0
}

// storageRank is the SQLite ordering class: numbers 0, text 1, blobs 2.
func storageRank(v wire.Value) int {
	switch v.Type {
	case wire.TypeText:
		return 1
	case wire.TypeBlob:
		return 2
	}
	return 0
}

func cmpInt(a, b int) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

func cmpInt64(a, b int64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

func bytesCompare(a, b []byte) int {
	return strings.Compare(string(a), string(b))
}

func isNumber(v wire.Value) bool {
	return v.Type == wire.TypeInteger || v.Type == wire.TypeFloat
}

func asFloat(v wire.Value) float64 {
	if v.Type == wire.TypeFloat {
		return v.Float
	}
	return float64(v.Int)
}

// asInt converts a value to an integer the way SQLite converts for LIMIT and
// function arguments: NULL is 0, text is parsed as far as it is numeric.
func asInt(v wire.Value) int64 {
	switch v.Type {
	case wire.TypeInteger:
		return v.Int
	case wire.TypeFloat:
		return int64(v.Float)
	case wire.TypeText:
		n, _ := strconv.ParseInt(string(v.Bytes), 10, 64)
		return n
	}
	return 0
}

// copyValue returns v with its bytes copied.
func copyValue(v wire.Value) wire.Value {
	if v.Type == wire.TypeText || v.Type == wire.TypeBlob {
		v.Bytes = append([]byte{}, v.Bytes...)
	}
	return v
}
