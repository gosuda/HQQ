// Package wire implements the encodings of the hqq-sqlite protocol 1.0 (see
// examples/sqlite/PROTOCOL.md). The Go driver and the Go fake server share it,
// so the byte layouts exist in exactly one place.
//
// Encoding is fixed-width little-endian with no padding. Decoders never trust
// their input: every length is checked against the bytes that remain and
// against the limits of the protocol.
package wire

import "fmt"

// Protocol version implemented by this package.
const (
	ProtocolMajor = 1
	ProtocolMinor = 0
)

// Method is the method id carried in the aux field of a request.
type Method uint64

// Method ids. The numeric values are part of the protocol.
const (
	MethodOpen        Method = 1
	MethodExec        Method = 2
	MethodQuery       Method = 3
	MethodFetch       Method = 4
	MethodCloseCursor Method = 5
	MethodPrepare     Method = 6
	MethodExecStmt    Method = 7
	MethodQueryStmt   Method = 8
	MethodCloseStmt   Method = 9
	MethodBegin       Method = 10
	MethodCommit      Method = 11
	MethodRollback    Method = 12
	MethodPing        Method = 13
	MethodClose       Method = 14
)

var methodNames = [...]string{
	MethodOpen:        "Open",
	MethodExec:        "Exec",
	MethodQuery:       "Query",
	MethodFetch:       "Fetch",
	MethodCloseCursor: "CloseCursor",
	MethodPrepare:     "Prepare",
	MethodExecStmt:    "ExecStmt",
	MethodQueryStmt:   "QueryStmt",
	MethodCloseStmt:   "CloseStmt",
	MethodBegin:       "Begin",
	MethodCommit:      "Commit",
	MethodRollback:    "Rollback",
	MethodPing:        "Ping",
	MethodClose:       "Close",
}

// String returns the method name, or "Method(n)" for an unknown id.
func (m Method) String() string {
	if m >= 1 && int(m) < len(methodNames) {
		return methodNames[m]
	}
	return fmt.Sprintf("Method(%d)", uint64(m))
}

// Limits of protocol 1.0 (PROTOCOL.md section 11).
const (
	MaxMessageBytes         = 16 << 20 // request and response payload
	MaxSQLBytes             = 1 << 20
	MaxParams               = 32766
	MaxColumns              = 2000
	MaxFetchRows            = 4096
	DefaultFetchRows        = 256
	MaxBatchBytes           = 1 << 20 // encoded row data per batch
	MaxNameBytes            = 1024
	MaxPathBytes            = 4096
	MaxPragmas              = 32
	MaxStatementsPerSession = 256
	MaxCursorsPerSession    = 64
	MaxSessions             = 256 // default server policy
)

// Open flags. The values are the SQLITE_OPEN_* bits.
const (
	OpenReadOnly  = 0x1
	OpenReadWrite = 0x2
	OpenCreate    = 0x4
)

// TxMode is the BEGIN mode of a transaction.
type TxMode uint8

// Transaction modes.
const (
	TxDeferred  TxMode = 0
	TxImmediate TxMode = 1
	TxExclusive TxMode = 2
)

// String returns the SQL keyword of the mode.
func (m TxMode) String() string {
	switch m {
	case TxDeferred:
		return "DEFERRED"
	case TxImmediate:
		return "IMMEDIATE"
	case TxExclusive:
		return "EXCLUSIVE"
	}
	return fmt.Sprintf("TxMode(%d)", uint8(m))
}

// ValueType is the tag of a Value. The numbers are the SQLite fundamental
// type codes (SQLITE_INTEGER and so on).
type ValueType uint8

// Value tags.
const (
	TypeInteger ValueType = 1
	TypeFloat   ValueType = 2
	TypeText    ValueType = 3
	TypeBlob    ValueType = 4
	TypeNull    ValueType = 5
)

// String returns the SQLite name of the type.
func (t ValueType) String() string {
	switch t {
	case TypeInteger:
		return "INTEGER"
	case TypeFloat:
		return "FLOAT"
	case TypeText:
		return "TEXT"
	case TypeBlob:
		return "BLOB"
	case TypeNull:
		return "NULL"
	}
	return fmt.Sprintf("ValueType(%d)", uint8(t))
}

// Status values of an error response (PROTOCOL.md section 7).
const (
	statusClassSQLite   = 1
	statusClassProtocol = 2
)

// StatusOK is the status of a success response.
const StatusOK Status = 0

// Status is the aux value of a response.
type Status uint64

// SQLiteStatus returns the status of a SQLite result code rc (primary or
// extended).
func SQLiteStatus(rc int) Status {
	return Status(statusClassSQLite)<<16 | Status(uint32(rc)&0xFFFF)
}

// ProtocolStatus returns the status of a protocol error code.
func ProtocolStatus(code ProtoCode) Status {
	return Status(statusClassProtocol)<<16 | Status(code)&0xFFFF
}

// IsSQLite reports whether s carries a SQLite result code.
func (s Status) IsSQLite() bool { return s>>16 == statusClassSQLite }

// IsProtocol reports whether s carries a protocol error code.
func (s Status) IsProtocol() bool { return s>>16 == statusClassProtocol }

// SQLiteCode returns the SQLite result code of s. It is meaningful only if
// IsSQLite reports true.
func (s Status) SQLiteCode() int { return int(s & 0xFFFF) }

// ProtoCode returns the protocol error code of s. It is meaningful only if
// IsProtocol reports true.
func (s Status) ProtoCode() ProtoCode { return ProtoCode(s & 0xFFFF) }

// ProtoCode is a protocol error code (PROTOCOL.md section 7).
type ProtoCode uint32

// Protocol error codes. The numbers are part of the protocol.
const (
	ProtoBadRequest    ProtoCode = 1
	ProtoUnknownMethod ProtoCode = 2
	ProtoNoSession     ProtoCode = 3
	ProtoAlreadyOpen   ProtoCode = 4
	ProtoNoStatement   ProtoCode = 5
	ProtoNoCursor      ProtoCode = 6
	ProtoLimit         ProtoCode = 7
	ProtoVersion       ProtoCode = 8
	ProtoForbidden     ProtoCode = 9
	ProtoInternal      ProtoCode = 10
)

var protoNames = [...]string{
	ProtoBadRequest:    "BAD_REQUEST",
	ProtoUnknownMethod: "UNKNOWN_METHOD",
	ProtoNoSession:     "NO_SESSION",
	ProtoAlreadyOpen:   "ALREADY_OPEN",
	ProtoNoStatement:   "NO_STATEMENT",
	ProtoNoCursor:      "NO_CURSOR",
	ProtoLimit:         "LIMIT",
	ProtoVersion:       "VERSION",
	ProtoForbidden:     "FORBIDDEN",
	ProtoInternal:      "INTERNAL",
}

// String returns the protocol name of the code.
func (c ProtoCode) String() string {
	if c >= 1 && int(c) < len(protoNames) {
		return protoNames[c]
	}
	return fmt.Sprintf("PROTO(%d)", uint32(c))
}

// ProtocolError is a protocol-level failure: a malformed message, a violated
// limit, or a request the session cannot serve. Servers send it as an error
// response with status ProtocolStatus(Code); clients receive it as such.
type ProtocolError struct {
	Code    ProtoCode
	Message string
}

// Error implements error.
func (e *ProtocolError) Error() string {
	return fmt.Sprintf("hqq-sqlite: %s: %s", e.Code, e.Message)
}

// Errorf returns a ProtocolError with a formatted message.
func Errorf(code ProtoCode, format string, args ...any) *ProtocolError {
	return &ProtocolError{Code: code, Message: fmt.Sprintf(format, args...)}
}
