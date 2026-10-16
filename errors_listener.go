package hqq

// Errors of the multi-client acceptor (see Acceptor and Connect). They are
// stable sentinels: compare with errors.Is.
var (
	// ErrListenerBusy is returned by NewAcceptor when the listener name is
	// served by a live acceptor. It matches ErrLinkBusy.
	ErrListenerBusy = &hqqError{msg: "hqq: listener already has a live acceptor", base: ErrLinkBusy}

	// ErrListenerClosed is returned by Connect when the acceptor closed the
	// listener while the connect request was waiting. It matches
	// ErrPeerClosed (and so io.EOF).
	ErrListenerClosed = &hqqError{msg: "hqq: listener closed", base: ErrPeerClosed}

	// ErrListenerGone is returned by Connect when the acceptor stopped
	// heartbeating (crashed or hung) or was replaced by a new acceptor while
	// the connect request was waiting. The request is lost: Connect again.
	// It matches ErrPeerGone.
	ErrListenerGone = &hqqError{msg: "hqq: listener is gone (no heartbeat)", base: ErrPeerGone}

	// ErrAcceptorClosed is returned by Acceptor.Accept after Close. It
	// matches ErrClosed (and net.ErrClosed).
	ErrAcceptorClosed = &hqqError{msg: "hqq: acceptor closed", base: ErrClosed}
)

// Internal errors of the acceptor. The transient ones wrap ErrFailedInit like
// the attach errors of named links; Connect retries them.
var (
	// errListenerNotReady is returned while the listener name exists but
	// its acceptor has not published it yet (or is re-initializing it).
	errListenerNotReady = &hqqError{msg: "hqq: listener is not initialized yet", base: ErrFailedInit}

	// errListenerDead is returned to a client that finds a listener whose
	// acceptor closed or died. A new acceptor reuses the name.
	errListenerDead = &hqqError{msg: "hqq: listener has no live acceptor", base: ErrFailedInit}

	// errInstanceMismatch is returned to an opener that expected a specific
	// instance of a segment (the one a client announced) and found another.
	errInstanceMismatch = &hqqError{msg: "hqq: link segment is not the announced instance", base: ErrConnNotFound}

	// errBadRecord classifies a connect record that failed validation.
	errBadRecord = &hqqError{msg: "hqq: invalid connect record", base: ErrProtocol}
)
