package gendriver

import (
	"gosuda.org/hqq/bindgen/examples/sqlite3"
)

// dbID and stmtID are the ids of the handles of a session. The driver keeps ids
// and not the generated handle types, so that the tests can fake api: a
// generated handle can only be made by a real session.
type (
	dbID   uint32
	stmtID uint32
)

// api is the part of the generated client that the driver uses, and nothing
// else: every method is one function of bindgen/examples/sqlite3 (see
// sqlite3.idl for its contract). The *Async methods submit a request and return
// the function that waits for its response; the driver pipelines with them. The
// session is not safe for concurrent use, except Kill.
type api interface {
	Close() error
	// Kill closes the link without the CLOSE exchange. It is the one method that
	// is safe from any goroutine, and how the driver stops waiting for a call
	// that cannot be canceled. The calls in flight have an unknown outcome.
	Kill()
	// Err is the sticky failure of a broken session, or nil.
	Err() error
	OpenV2(filename string, flags int32) (dbID, error)
	BusyTimeout(db dbID, ms int32) error
	GetAutocommit(db dbID) (int32, error)

	PrepareOne(db dbID, sql string) (stmtID, error)
	StmtInfo(stmt stmtID) (sqlite3.StmtMeta, error)
	StmtInfoAsync(stmt stmtID) (wait func() (sqlite3.StmtMeta, error), err error)
	Finalize(stmt stmtID) error
	FinalizeAsync(stmt stmtID) (wait func() error, err error)
	ResetAsync(stmt stmtID) (wait func() error, err error)

	BindValuesAsync(stmt stmtID, kinds []uint8, words []uint64, data []byte) (wait func() error, err error)
	ExecStmt(stmt stmtID, kinds []uint8, words []uint64, data []byte) (changes, lastRowid int64, err error)
	ExecStmtAsync(stmt stmtID, kinds []uint8, words []uint64, data []byte) (wait func() (changes, lastRowid int64, err error), err2 error)

	FetchRowsAsync(stmt stmtID, max int32) (wait func() (sqlite3.Rows, error), err error)
	ColumnChunk(stmt stmtID, col int32, offset uint64, dst []byte) ([]byte, error)
}

// genAPI is api on a generated client.
type genAPI struct {
	cl *sqlite3.Client
}

var _ api = genAPI{}

func (g genAPI) db(id dbID) sqlite3.Db {
	return sqlite3.Db{Handle: g.cl.Conn().NewHandle(uint32(id))}
}

func (g genAPI) stmt(id stmtID) sqlite3.Stmt {
	return sqlite3.Stmt{Handle: g.cl.Conn().NewHandle(uint32(id))}
}

func (g genAPI) Close() error { return g.cl.Close() }
func (g genAPI) Kill()        { g.cl.Kill() }
func (g genAPI) Err() error   { return g.cl.Conn().Err() }

func (g genAPI) OpenV2(filename string, flags int32) (dbID, error) {
	db, err := g.cl.Sqlite3OpenV2(filename, flags, nil)
	return dbID(db.ID()), err
}

func (g genAPI) BusyTimeout(db dbID, ms int32) error { return g.cl.Sqlite3BusyTimeout(g.db(db), ms) }

func (g genAPI) GetAutocommit(db dbID) (int32, error) { return g.cl.Sqlite3GetAutocommit(g.db(db)) }

func (g genAPI) PrepareOne(db dbID, sql string) (stmtID, error) {
	st, err := g.cl.PrepareOne(g.db(db), sql)
	return stmtID(st.ID()), err
}

func (g genAPI) StmtInfo(stmt stmtID) (sqlite3.StmtMeta, error) { return g.cl.StmtInfo(g.stmt(stmt)) }

func (g genAPI) StmtInfoAsync(stmt stmtID) (func() (sqlite3.StmtMeta, error), error) {
	f, err := g.cl.StmtInfoAsync(g.stmt(stmt))
	if err != nil {
		return nil, err
	}
	return f.Wait, nil
}

func (g genAPI) Finalize(stmt stmtID) error { return g.cl.Sqlite3Finalize(g.stmt(stmt)) }

func (g genAPI) FinalizeAsync(stmt stmtID) (func() error, error) {
	f, err := g.cl.Sqlite3FinalizeAsync(g.stmt(stmt))
	if err != nil {
		return nil, err
	}
	return f.Wait, nil
}

func (g genAPI) ResetAsync(stmt stmtID) (func() error, error) {
	f, err := g.cl.Sqlite3ResetAsync(g.stmt(stmt))
	if err != nil {
		return nil, err
	}
	return f.Wait, nil
}

func (g genAPI) BindValuesAsync(stmt stmtID, kinds []uint8, words []uint64, data []byte) (func() error, error) {
	f, err := g.cl.BindValuesAsync(g.stmt(stmt), kinds, words, data)
	if err != nil {
		return nil, err
	}
	return f.Wait, nil
}

func (g genAPI) ExecStmt(stmt stmtID, kinds []uint8, words []uint64, data []byte) (int64, int64, error) {
	return g.cl.ExecStmt(g.stmt(stmt), kinds, words, data)
}

func (g genAPI) ExecStmtAsync(stmt stmtID, kinds []uint8, words []uint64, data []byte) (func() (int64, int64, error), error) {
	f, err := g.cl.ExecStmtAsync(g.stmt(stmt), kinds, words, data)
	if err != nil {
		return nil, err
	}
	return f.Wait, nil
}

func (g genAPI) FetchRowsAsync(stmt stmtID, max int32) (func() (sqlite3.Rows, error), error) {
	f, err := g.cl.FetchRowsAsync(g.stmt(stmt), max)
	if err != nil {
		return nil, err
	}
	return f.Wait, nil
}

func (g genAPI) ColumnChunk(stmt stmtID, col int32, offset uint64, dst []byte) ([]byte, error) {
	return g.cl.ColumnChunk(g.stmt(stmt), col, offset, dst)
}
