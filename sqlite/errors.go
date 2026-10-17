package sqlite

import (
	"errors"
	"fmt"

	"gosuda.org/hqq/sqlite/internal/wire"
)

// SQLite result codes the driver names. The values are those of sqlite3.h.
// Primary codes are the low byte of an extended code.
const (
	CodeError                = 1
	CodeBusy                 = 5
	CodeLocked               = 6
	CodeReadOnly             = 8
	CodeInterrupt            = 9
	CodeCantOpen             = 14
	CodeTooBig               = 18
	CodeConstraint           = 19
	CodeBusyTimeout          = 5 | 3<<8  // 773
	CodeConstraintCheck      = 19 | 1<<8 // 275
	CodeConstraintForeignKey = 19 | 3<<8 // 787
	CodeConstraintNotNull    = 19 | 5<<8 // 1299
	CodeConstraintPrimaryKey = 19 | 6<<8 // 1555
	CodeConstraintUnique     = 19 | 8<<8 // 2067
)

// Sentinels for errors.Is. An *Error matches the sentinel of its primary code.
var (
	ErrBusy        = errors.New("sqlite: database is locked by another session")
	ErrLocked      = errors.New("sqlite: table is locked")
	ErrConstraint  = errors.New("sqlite: constraint violation")
	ErrReadOnly    = errors.New("sqlite: attempt to write a readonly database")
	ErrInterrupted = errors.New("sqlite: operation interrupted")

	// ErrOutcomeUnknown is wrapped by the error of an Exec or ExecStmt whose
	// request was in flight when the link was lost. The statement may or may
	// not have run. The connection is discarded, and database/sql does not
	// retry such a call, because a retry could run the statement twice.
	ErrOutcomeUnknown = errors.New("sqlite: connection lost while the statement was in flight; its outcome is unknown")
)

// Error is an error reported by SQLite on the server.
//
// Code is the primary result code (for example CodeConstraint). ExtendedCode is
// the extended result code (for example CodeConstraintUnique), or equal to Code
// when SQLite gave no extended code. Message is sqlite3_errmsg().
type Error struct {
	Code         int
	ExtendedCode int
	Message      string
}

// Error implements error.
func (e *Error) Error() string {
	return fmt.Sprintf("sqlite: %s (code %d)", e.Message, e.ExtendedCode)
}

// Primary returns the primary result code. It equals Code.
func (e *Error) Primary() int { return e.Code }

// Is reports whether target is the sentinel of the error's primary code.
func (e *Error) Is(target error) bool {
	switch target {
	case ErrBusy:
		return e.Code == CodeBusy
	case ErrLocked:
		return e.Code == CodeLocked
	case ErrConstraint:
		return e.Code == CodeConstraint
	case ErrReadOnly:
		return e.Code == CodeReadOnly
	case ErrInterrupted:
		return e.Code == CodeInterrupt
	}
	return false
}

// ProtocolError is an error response of the service that is not a SQLite
// error: a malformed request, a violated limit, or a session state error. Its
// Code is one of the protocol error codes (PROTOCOL.md section 7), for example
// 1 for BAD_REQUEST or 7 for LIMIT.
type ProtocolError = wire.ProtocolError

// errorOf converts an error response of the service to the driver's types.
func errorOf(st wire.Status, msg string) error {
	switch {
	case st.IsSQLite():
		ext := st.SQLiteCode()
		return &Error{Code: ext & 0xFF, ExtendedCode: ext, Message: msg}
	case st.IsProtocol():
		return &wire.ProtocolError{Code: st.ProtoCode(), Message: msg}
	}
	return fmt.Errorf("sqlite: unknown error status %#x: %s", uint64(st), msg)
}

// IsBusy reports whether err is a SQLITE_BUSY error (the database is locked by
// another session and the busy timeout ran out).
func IsBusy(err error) bool { return hasCode(err, CodeBusy) }

// IsLocked reports whether err is a SQLITE_LOCKED error (a table is locked
// within the same connection or shared cache).
func IsLocked(err error) bool { return hasCode(err, CodeLocked) }

// IsConstraint reports whether err is a constraint violation of any kind. Use
// the ExtendedCode of the *Error to tell the kinds apart.
func IsConstraint(err error) bool { return hasCode(err, CodeConstraint) }

// IsReadOnly reports whether err is a write to a read-only database or
// transaction.
func IsReadOnly(err error) bool { return hasCode(err, CodeReadOnly) }

// IsInterrupted reports whether err is SQLITE_INTERRUPT: the statement was
// interrupted by the cancellation of its request on the server.
func IsInterrupted(err error) bool { return hasCode(err, CodeInterrupt) }

// hasCode reports whether err holds an *Error with primary code code.
func hasCode(err error, code int) bool {
	var se *Error
	return errors.As(err, &se) && se.Code == code
}
