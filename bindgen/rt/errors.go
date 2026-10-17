package rt

import (
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"

	hqq "gosuda.org/hqq"
)

// Errors (BINDGEN.md 6.2).
var (
	ErrInterfaceMismatch = errors.New("hqqbg: interface digest mismatch")
	ErrSessionBroken     = errors.New("hqqbg: session is broken")
	ErrConcurrentUse     = errors.New("hqqbg: re-entrant use of a Conn")
	ErrFutureUsed        = errors.New("hqqbg: future already waited for or discarded")
	ErrForeignHandle     = errors.New("hqqbg: handle belongs to another session")
	ErrInvalidArg        = errors.New("hqqbg: invalid argument")
	ErrOutcomeUnknown    = errors.New("hqqbg: outcome unknown")

	ErrUnknownOp    = errors.New("hqqbg: unknown operation")
	ErrBadRequest   = errors.New("hqqbg: bad request")
	ErrBadHandle    = errors.New("hqqbg: bad handle")
	ErrLimit        = errors.New("hqqbg: limit exceeded")
	ErrDenied       = errors.New("hqqbg: denied")
	ErrInternal     = errors.New("hqqbg: internal error")
	ErrShuttingDown = errors.New("hqqbg: server shutting down")
)

// TransportError is a failure of the Standard link (6.1). Unknown is set when
// the request was committed and its response was not completely received.
type TransportError struct {
	Func    string
	Err     error
	Unknown bool

	svc string
}

func (e *TransportError) Error() string {
	s := errPrefix(e.svc, e.Func) + "transport error: " + errText(e.Err)
	if e.Unknown {
		s += " (outcome unknown)"
	}
	return s
}

func (e *TransportError) Unwrap() error { return e.Err }

func (e *TransportError) Is(target error) bool {
	return e.Unknown && target == ErrOutcomeUnknown
}

// ProtocolError is a frame or body that breaks the rules of 4.4 to 4.8, sent
// by a peer that is buggy or hostile. It matches hqq.ErrProtocol.
type ProtocolError struct {
	Func    string
	Reason  string
	Unknown bool

	svc string
}

func (e *ProtocolError) Error() string {
	s := errPrefix(e.svc, e.Func) + "protocol violation: " + e.Reason
	if e.Unknown {
		s += " (outcome unknown)"
	}
	return s
}

func (e *ProtocolError) Is(target error) bool {
	return target == hqq.ErrProtocol || (e.Unknown && target == ErrOutcomeUnknown)
}

// StatusError is a RESPONSE with a nonzero status (4.10): the server refused
// the call, or, for E_INTERNAL, could not finish it. The final frames of 5.4
// (statuses 7 and 8) reach the calls that were in flight and are an unknown
// outcome like E_INTERNAL.
type StatusError struct {
	Func   string
	Status uint32
	Msg    string

	svc string
}

func (e *StatusError) Error() string {
	s := errPrefix(e.svc, e.Func) + "server status " + strconv.FormatUint(uint64(e.Status), 10) + " (" + statusName(e.Status) + ")"
	if e.Msg != "" {
		s += ": " + e.Msg
	}
	return s
}

func (e *StatusError) Is(target error) bool {
	switch e.Status {
	case statusUnknownOp:
		return target == ErrUnknownOp
	case statusBadRequest:
		return target == ErrBadRequest
	case statusBadHandle:
		return target == ErrBadHandle
	case statusLimit:
		return target == ErrLimit
	case statusDenied:
		return target == ErrDenied
	case statusInternal:
		return target == ErrInternal || target == ErrOutcomeUnknown
	case statusProtocol:
		return target == hqq.ErrProtocol || target == ErrOutcomeUnknown
	case statusShuttingDown:
		return target == ErrShuttingDown || target == ErrOutcomeUnknown
	}
	return false
}

// LibError is a library failure (F_FAIL, 4.6.4): the C function ran and the
// error convention of the IDL reported a failure. Code is the library's
// return value, 0 for a NULL result.
type LibError struct {
	Func   string
	Code   int64
	Detail string

	svc string
}

func (e *LibError) Error() string {
	s := errPrefix(e.svc, e.Func) + "library error " + strconv.FormatInt(e.Code, 10)
	if e.Detail != "" {
		s += ": " + e.Detail
	}
	return s
}

// MismatchError is a HELLO_REPLY with a nonzero status (4.11). Reason is the
// status (1 digest, 2 version, 3 geometry, 4 limit, 5 bad hello); Remote is
// the digest the server reported.
type MismatchError struct {
	Reason uint32
	Local  [32]byte
	Remote [32]byte
}

func (e *MismatchError) Error() string {
	return fmt.Sprintf("hqqbg: handshake refused: %s (client digest %s, server digest %s)",
		helloReason(e.Reason), hex.EncodeToString(e.Local[:]), hex.EncodeToString(e.Remote[:]))
}

func (e *MismatchError) Is(target error) bool { return target == ErrInterfaceMismatch }

func helloReason(r uint32) string {
	switch r {
	case helloDigestMismatch:
		return "interface digest mismatch"
	case helloVersion:
		return "protocol version mismatch"
	case helloGeometry:
		return "buffer geometry mismatch"
	case helloLimit:
		return "requested limits refused"
	case helloBad:
		return "malformed HELLO"
	}
	return "status " + strconv.FormatUint(uint64(r), 10)
}

// OpenError is a refused bootstrap request (4.11). Code is the status minus
// 0x50000: 1 version, 2 geometry, 3 busy, 4 internal. Geometry and busy match
// ErrLimit.
type OpenError struct {
	Code uint32
	Msg  string
}

func (e *OpenError) Error() string {
	s := "hqqbg: bootstrap refused (code " + strconv.FormatUint(uint64(e.Code), 10) + ")"
	if e.Msg != "" {
		s += ": " + e.Msg
	}
	return s
}

func (e *OpenError) Is(target error) bool {
	return target == ErrLimit && (e.Code == openCodeBusy || e.Code == openCodeGeometry)
}

func errPrefix(svc, fn string) string {
	s := ""
	if svc != "" {
		s = svc + ": "
	}
	if fn != "" {
		s += fn + ": "
	}
	return s
}

func errText(err error) string {
	if err == nil {
		return "<nil>"
	}
	return err.Error()
}
