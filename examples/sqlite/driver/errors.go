package hqqsqlite

import (
	"errors"
	"fmt"
)

// SQLite result codes the driver reports by name. Codes are the values of
// sqlite3.h: primary codes, and extended codes where they are distinguished.
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
)

// Error is an error reported by SQLite on the server. Code is the SQLite
// result code: the extended code if SQLite gave one, otherwise the primary
// code. Use Primary for the primary code.
type Error struct {
	Code    int
	Message string
}

// Error implements error.
func (e *Error) Error() string {
	return fmt.Sprintf("sqlite: %s (code %d)", e.Message, e.Code)
}

// Primary returns the primary result code, for example CodeConstraint for
// CodeConstraintUnique.
func (e *Error) Primary() int {
	return e.Code & 0xFF
}

// Is reports whether target is the sentinel of the error's primary code.
func (e *Error) Is(target error) bool {
	switch target {
	case ErrBusy:
		return e.Primary() == CodeBusy
	case ErrLocked:
		return e.Primary() == CodeLocked
	case ErrConstraint:
		return e.Primary() == CodeConstraint
	case ErrReadOnly:
		return e.Primary() == CodeReadOnly
	case ErrInterrupted:
		return e.Primary() == CodeInterrupt
	}
	return false
}
