package gendriver

import (
	"context"
	"database/sql/driver"
	"errors"
	"fmt"

	"gosuda.org/hqq"
	"gosuda.org/hqq/bindgen/examples/sqlite3"
	"gosuda.org/hqq/bindgen/rt"
	"gosuda.org/hqq/sqlite"
	"gosuda.org/hqq/sqlite/internal/wire"
)

// libError converts a library failure of the generated client to the driver's
// error types: the *sqlite.Error of the existing driver for a SQLite result
// code (the server enables extended result codes, so code is the extended
// one), and a *sqlite.ProtocolError for the codes that only this service
// reports.
func libError(code int64, detail string) error {
	switch int32(code) {
	case sqlite3.SVC_SQL_EMPTY, sqlite3.SVC_SQL_MULTIPLE, sqlite3.SVC_BAD_ARGS:
		return &wire.ProtocolError{Code: wire.ProtoBadRequest, Message: detail}
	}
	return sqliteError(int(code), detail)
}

// sqliteError makes the *sqlite.Error of an extended result code.
func sqliteError(ext int, msg string) *sqlite.Error {
	return &sqlite.Error{Code: ext & 0xFF, ExtendedCode: ext, Message: msg}
}

// statusError converts a refusal of the server (a nonzero call status, BINDGEN.md
// 4.10) to the driver's error types, or returns nil when the status says that
// the call may have run or that the session cannot be trusted.
func statusError(se *rt.StatusError) error {
	switch {
	case errors.Is(se, rt.ErrOutcomeUnknown), errors.Is(se, rt.ErrShuttingDown):
		return nil
	case se.Status >= sqlite3.SVC_STATUS_SQLITE_BASE && se.Status < sqlite3.SVC_STATUS_META_TOO_LARGE:
		// The open failed with this primary code before SQLite was called.
		return sqliteError(int(se.Status-sqlite3.SVC_STATUS_SQLITE_BASE), se.Msg)
	case se.Status == sqlite3.SVC_STATUS_META_TOO_LARGE, se.Status == sqlite3.SVC_STATUS_NAME_TOO_LONG,
		errors.Is(se, rt.ErrLimit):
		return &wire.ProtocolError{Code: wire.ProtoLimit, Message: statusText(se)}
	case errors.Is(se, rt.ErrDenied):
		return &wire.ProtocolError{Code: wire.ProtoForbidden, Message: statusText(se)}
	case errors.Is(se, rt.ErrBadHandle):
		return &wire.ProtocolError{Code: wire.ProtoNoStatement, Message: statusText(se)}
	case errors.Is(se, rt.ErrBadRequest), errors.Is(se, rt.ErrUnknownOp):
		return &wire.ProtocolError{Code: wire.ProtoBadRequest, Message: statusText(se)}
	}
	return nil
}

func statusText(se *rt.StatusError) string {
	if se.Msg != "" {
		return se.Msg
	}
	return se.Error()
}

// serverError converts an answer of the server that leaves the session usable:
// a library failure or a refused call. It returns nil for every other error.
func serverError(err error) error {
	var le *rt.LibError
	if errors.As(err, &le) {
		return libError(le.Code, le.Detail)
	}
	var se *rt.StatusError
	if errors.As(err, &se) {
		return statusError(se)
	}
	return nil
}

// isServerError reports whether err is an error that serverError converts.
func isServerError(err error) bool { return serverError(err) != nil }

// isLimitError reports whether err is the client refusing a request that exceeds
// a limit before it sent anything.
func isLimitError(err error) bool { return errors.Is(err, rt.ErrInvalidArg) }

// isTransportError reports whether err is a failure of the link or of the peer
// that the driver can name (used for dialing, where rt reports the HQQ error).
func isTransportError(err error) bool {
	var te *rt.TransportError
	return errors.As(err, &te) ||
		errors.Is(err, hqq.ErrPeerGone) || errors.Is(err, hqq.ErrPeerClosed) ||
		errors.Is(err, hqq.ErrClosed) || errors.Is(err, hqq.ErrListenerGone) ||
		errors.Is(err, hqq.ErrListenerClosed) || errors.Is(err, hqq.ErrShuttingDown)
}

// transportError marks a failure of the link or of the peer as driver.ErrBadConn,
// so database/sql discards the connection and may retry on a new one. It is
// used where no statement can have run: dialing and opening a session.
func transportError(err error) error {
	if errors.Is(err, driver.ErrBadConn) {
		return err
	}
	if isTransportError(err) || errors.Is(err, rt.ErrSessionBroken) {
		return fmt.Errorf("%w: %w", driver.ErrBadConn, err)
	}
	return err
}

// failure maps the error of a call of the session to the error database/sql
// sees, and marks the conn bad when the session can no longer be trusted:
//
//   - an answer of the server (a SQLite failure, a refused call) keeps the conn
//     usable and becomes *sqlite.Error or *sqlite.ProtocolError;
//   - a request the client refused before sending it (a size limit) keeps the
//     conn usable;
//   - a canceled or expired context, whose Kill abandoned the call, is the
//     context's error;
//   - anything else is a lost or broken session. If the failed request was the
//     execution of a statement of the caller (see mutatingError) and it may have
//     run, the error wraps sqlite.ErrOutcomeUnknown: a retry could run the
//     statement twice, so database/sql must not see driver.ErrBadConn.
//     Otherwise it wraps driver.ErrBadConn.
func (cn *conn) failure(ctx context.Context, op string, err error) error {
	if err == nil {
		return nil
	}
	if se := serverError(err); se != nil {
		return se
	}
	if isLimitError(err) {
		return fmt.Errorf("sqlite: %s: request exceeds a limit: %w", op, err)
	}
	cn.markBad()
	if cn.killed.Load() {
		if cerr := ctx.Err(); cerr != nil {
			return cerr
		}
		if cn.killCause != nil {
			return cn.killCause
		}
	}
	var me mutatingError
	if errors.As(err, &me) && errors.Is(err, rt.ErrOutcomeUnknown) {
		return fmt.Errorf("%w (%s): %w", sqlite.ErrOutcomeUnknown, op, err)
	}
	return fmt.Errorf("%w: %w", driver.ErrBadConn, err)
}

// mutatingError marks the error of the request that executes a statement of the
// caller (exec_stmt): the statement may have run, whatever the error. The other
// requests of a call (prepare, info, finalize) change nothing the caller asked
// for.
type mutatingError struct{ err error }

func (e mutatingError) Error() string { return e.err.Error() }
func (e mutatingError) Unwrap() error { return e.err }
