package wire

import "encoding/binary"

// Typed request and response bodies. Each pair of Append and Parse functions
// is the layout of one method (PROTOCOL.md section 6). Parse functions reject
// trailing bytes.

// Pragma is one PRAGMA applied by Open.
type Pragma struct {
	Name  string
	Value string
}

// OpenRequest is the body of Open.
type OpenRequest struct {
	Major, Minor uint16
	DB           string
	Flags        uint32
	BusyMS       uint32
	Pragmas      []Pragma
}

// AppendOpenRequest appends the body of an Open request.
func AppendOpenRequest(e *Encoder, r OpenRequest) {
	e.U16(r.Major)
	e.U16(r.Minor)
	e.String(r.DB)
	e.U32(r.Flags)
	e.U32(r.BusyMS)
	e.U32(uint32(len(r.Pragmas)))
	for _, p := range r.Pragmas {
		e.String(p.Name)
		e.String(p.Value)
	}
}

// OpenMajor returns the protocol major version at the start of an Open request
// body, or 0 when the body is shorter than two bytes. It reads nothing else, so
// the version can be checked before the rest of the body is parsed.
func OpenMajor(body []byte) uint16 {
	if len(body) < 2 {
		return 0
	}
	return binary.LittleEndian.Uint16(body)
}

// ParseOpenRequest parses the body of an Open request.
func ParseOpenRequest(b []byte) (OpenRequest, error) {
	d := NewDecoder(b)
	var r OpenRequest
	r.Major = d.U16()
	r.Minor = d.U16()
	r.DB = d.pathText()
	r.Flags = d.U32()
	r.BusyMS = d.U32()
	n := d.U32()
	if d.Err() == nil && n > MaxPragmas {
		d.failCode(ProtoLimit, "%d pragmas exceed %d", n, MaxPragmas)
	}
	for range n {
		if d.Err() != nil {
			break
		}
		p := Pragma{Name: d.Name(), Value: d.Name()}
		if d.Err() == nil && !ValidPragmaName(p.Name) {
			d.fail("invalid pragma name %q", p.Name)
		}
		if d.Err() == nil && !ValidPragmaValue(p.Value) {
			d.fail("invalid pragma value %q", p.Value)
		}
		r.Pragmas = append(r.Pragmas, p)
	}
	if err := d.Finish(); err != nil {
		return OpenRequest{}, err
	}
	if !ValidOpenFlags(r.Flags) {
		return OpenRequest{}, Errorf(ProtoBadRequest, "invalid open flags %#x", r.Flags)
	}
	return r, nil
}

// OpenResponse is the body of a successful Open.
type OpenResponse struct {
	Major, Minor uint16
	MaxFetchRows uint32
	MaxParams    uint32
}

// AppendOpenResponse appends the body of an Open response.
func AppendOpenResponse(e *Encoder, r OpenResponse) {
	e.U16(r.Major)
	e.U16(r.Minor)
	e.U32(r.MaxFetchRows)
	e.U32(r.MaxParams)
}

// ParseOpenResponse parses the body of an Open response.
func ParseOpenResponse(b []byte) (OpenResponse, error) {
	d := NewDecoder(b)
	r := OpenResponse{
		Major:        d.U16(),
		Minor:        d.U16(),
		MaxFetchRows: d.U32(),
		MaxParams:    d.U32(),
	}
	return r, d.Finish()
}

// ExecRequest is the body of Exec (Params) and Query (with Fetch); SQL is
// empty for ExecStmt and QueryStmt, which use Stmt instead.
type ExecRequest struct {
	SQL    string
	Params []Param
}

// AppendExecRequest appends the body of an Exec request.
func AppendExecRequest(e *Encoder, r ExecRequest) {
	e.String(r.SQL)
	e.Params(r.Params)
}

// ParseExecRequest parses the body of an Exec request.
func ParseExecRequest(b []byte) (ExecRequest, error) {
	d := NewDecoder(b)
	r := ExecRequest{SQL: sqlText(d)}
	r.Params = d.Params()
	return r, d.Finish()
}

// QueryRequest is the body of Query.
type QueryRequest struct {
	SQL    string
	Params []Param
	Fetch  uint32
}

// AppendQueryRequest appends the body of a Query request.
func AppendQueryRequest(e *Encoder, r QueryRequest) {
	e.String(r.SQL)
	e.Params(r.Params)
	e.U32(r.Fetch)
}

// ParseQueryRequest parses the body of a Query request.
func ParseQueryRequest(b []byte) (QueryRequest, error) {
	d := NewDecoder(b)
	var r QueryRequest
	r.SQL = sqlText(d)
	r.Params = d.Params()
	r.Fetch = d.U32()
	return r, d.Finish()
}

// StmtExecRequest is the body of ExecStmt.
type StmtExecRequest struct {
	Stmt   uint32
	Params []Param
}

// AppendStmtExecRequest appends the body of an ExecStmt request.
func AppendStmtExecRequest(e *Encoder, r StmtExecRequest) {
	e.U32(r.Stmt)
	e.Params(r.Params)
}

// ParseStmtExecRequest parses the body of an ExecStmt request.
func ParseStmtExecRequest(b []byte) (StmtExecRequest, error) {
	d := NewDecoder(b)
	var r StmtExecRequest
	r.Stmt = d.U32()
	r.Params = d.Params()
	return r, d.Finish()
}

// StmtQueryRequest is the body of QueryStmt.
type StmtQueryRequest struct {
	Stmt   uint32
	Params []Param
	Fetch  uint32
}

// AppendStmtQueryRequest appends the body of a QueryStmt request.
func AppendStmtQueryRequest(e *Encoder, r StmtQueryRequest) {
	e.U32(r.Stmt)
	e.Params(r.Params)
	e.U32(r.Fetch)
}

// ParseStmtQueryRequest parses the body of a QueryStmt request.
func ParseStmtQueryRequest(b []byte) (StmtQueryRequest, error) {
	d := NewDecoder(b)
	var r StmtQueryRequest
	r.Stmt = d.U32()
	r.Params = d.Params()
	r.Fetch = d.U32()
	return r, d.Finish()
}

// ExecResponse is the body of Exec and ExecStmt.
type ExecResponse struct {
	RowsAffected    int64
	LastInsertRowID int64
}

// AppendExecResponse appends the body of an Exec response.
func AppendExecResponse(e *Encoder, r ExecResponse) {
	e.I64(r.RowsAffected)
	e.I64(r.LastInsertRowID)
}

// ParseExecResponse parses the body of an Exec response.
func ParseExecResponse(b []byte) (ExecResponse, error) {
	d := NewDecoder(b)
	r := ExecResponse{RowsAffected: d.I64(), LastInsertRowID: d.I64()}
	return r, d.Finish()
}

// ResultHeader is the part of a Query or QueryStmt response before the batch.
// A batch then follows (AppendBatch / ParseBatch).
type ResultHeader struct {
	Cursor  uint32
	Columns []Column
	More    bool
}

// AppendResultHeader appends the cursor, the columns, and the more flag.
func AppendResultHeader(e *Encoder, h ResultHeader) {
	e.U32(h.Cursor)
	e.Columns(h.Columns)
	e.U8(boolByte(h.More))
}

// ParseResultHeader parses the header of a Query response. The decoder is left
// at the batch.
func ParseResultHeader(d *Decoder) ResultHeader {
	var h ResultHeader
	h.Cursor = d.U32()
	h.Columns = d.Columns()
	h.More = moreFlag(d)
	return h
}

// FetchRequest is the body of Fetch.
type FetchRequest struct {
	Cursor uint32
	NRows  uint32
}

// AppendFetchRequest appends the body of a Fetch request.
func AppendFetchRequest(e *Encoder, r FetchRequest) {
	e.U32(r.Cursor)
	e.U32(r.NRows)
}

// ParseFetchRequest parses the body of a Fetch request.
func ParseFetchRequest(b []byte) (FetchRequest, error) {
	d := NewDecoder(b)
	r := FetchRequest{Cursor: d.U32(), NRows: d.U32()}
	return r, d.Finish()
}

// ParseFetchResponseHeader parses the more flag of a Fetch response. The
// decoder is left at the batch.
func ParseFetchResponseHeader(d *Decoder) bool {
	return moreFlag(d)
}

// PrepareResponse is the body of Prepare.
type PrepareResponse struct {
	Stmt    uint32
	NParams uint32
	Columns []Column
}

// AppendPrepareResponse appends the body of a Prepare response.
func AppendPrepareResponse(e *Encoder, r PrepareResponse) {
	e.U32(r.Stmt)
	e.U32(r.NParams)
	e.Columns(r.Columns)
}

// ParsePrepareResponse parses the body of a Prepare response.
func ParsePrepareResponse(b []byte) (PrepareResponse, error) {
	d := NewDecoder(b)
	var r PrepareResponse
	r.Stmt = d.U32()
	r.NParams = d.U32()
	r.Columns = d.Columns()
	return r, d.Finish()
}

// PrepareRequest is the body of Prepare.
type PrepareRequest struct {
	SQL string
}

// AppendPrepareRequest appends the body of a Prepare request.
func AppendPrepareRequest(e *Encoder, r PrepareRequest) {
	e.String(r.SQL)
}

// ParsePrepareRequest parses the body of a Prepare request.
func ParsePrepareRequest(b []byte) (PrepareRequest, error) {
	d := NewDecoder(b)
	r := PrepareRequest{SQL: sqlText(d)}
	return r, d.Finish()
}

// IDRequest is the body of CloseCursor and CloseStmt.
type IDRequest struct {
	ID uint32
}

// AppendIDRequest appends the body of a CloseCursor or CloseStmt request.
func AppendIDRequest(e *Encoder, r IDRequest) {
	e.U32(r.ID)
}

// ParseIDRequest parses the body of a CloseCursor or CloseStmt request.
func ParseIDRequest(b []byte) (IDRequest, error) {
	d := NewDecoder(b)
	r := IDRequest{ID: d.U32()}
	return r, d.Finish()
}

// BeginRequest is the body of Begin.
type BeginRequest struct {
	Mode     TxMode
	ReadOnly bool
}

// AppendBeginRequest appends the body of a Begin request.
func AppendBeginRequest(e *Encoder, r BeginRequest) {
	e.U8(uint8(r.Mode))
	e.U8(boolByte(r.ReadOnly))
}

// ParseBeginRequest parses the body of a Begin request. It enforces the rules
// of PROTOCOL.md section 6.9.
func ParseBeginRequest(b []byte) (BeginRequest, error) {
	d := NewDecoder(b)
	mode := d.U8()
	ro := d.U8()
	if err := d.Finish(); err != nil {
		return BeginRequest{}, err
	}
	if mode > uint8(TxExclusive) {
		return BeginRequest{}, Errorf(ProtoBadRequest, "invalid transaction mode %d", mode)
	}
	if ro > 1 {
		return BeginRequest{}, Errorf(ProtoBadRequest, "invalid read_only flag %d", ro)
	}
	if ro == 1 && mode != uint8(TxDeferred) {
		return BeginRequest{}, Errorf(ProtoBadRequest, "read-only transactions must be DEFERRED")
	}
	return BeginRequest{Mode: TxMode(mode), ReadOnly: ro == 1}, nil
}

// pathText reads a database path of at most MaxPathBytes and copies it.
func (d *Decoder) pathText() string {
	b := d.Bytes()
	if d.Err() != nil {
		return ""
	}
	if len(b) == 0 {
		d.fail("empty database name")
		return ""
	}
	if len(b) > MaxPathBytes {
		d.failCode(ProtoLimit, "database name of %d bytes exceeds %d", len(b), MaxPathBytes)
		return ""
	}
	return string(b)
}

// sqlText reads the SQL text of a request and checks MaxSQLBytes.
func sqlText(d *Decoder) string {
	b := d.Bytes()
	if d.Err() != nil {
		return ""
	}
	if len(b) > MaxSQLBytes {
		d.failCode(ProtoLimit, "SQL of %d bytes exceeds %d", len(b), MaxSQLBytes)
		return ""
	}
	return string(b)
}

// moreFlag reads a 0 or 1 byte.
func moreFlag(d *Decoder) bool {
	v := d.U8()
	if d.Err() != nil {
		return false
	}
	if v > 1 {
		d.fail("invalid more flag %d", v)
		return false
	}
	return v == 1
}

func boolByte(v bool) uint8 {
	if v {
		return 1
	}
	return 0
}
