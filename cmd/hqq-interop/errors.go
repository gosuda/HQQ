package main

import (
	"io"
	"os"

	"gosuda.org/hqq"
)

// The canonical error_kind names of INTEROP_CLI.md, section 4. Each name is
// the most specific sentinel the Go API returns for a condition of SPEC 12.
// The order is significant: a sentinel that wraps another one (ErrPeerClosed
// wraps io.EOF, ErrLinkBusy wraps ErrFailedInit, ...) must come first.
func init() {
	errorKinds = []errorKindEntry{
		{hqq.ErrListenerBusy, "ErrListenerBusy"},
		{hqq.ErrListenerGone, "ErrListenerGone"},
		{hqq.ErrListenerClosed, "ErrListenerClosed"},
		{hqq.ErrAcceptorClosed, "ErrAcceptorClosed"},
		{hqq.ErrShuttingDown, "ErrShuttingDown"},
		{hqq.ErrPeerClosed, "ErrPeerClosed"},
		{hqq.ErrPeerGone, "ErrPeerGone"},
		{hqq.ErrClosed, "ErrClosed"},
		{hqq.ErrTimeout, "ErrTimeout"},
		{hqq.ErrLinkBusy, "ErrLinkBusy"},
		{hqq.ErrIncompatibleLayout, "ErrIncompatibleLayout"},
		{hqq.ErrNotNegotiated, "ErrNotNegotiated"},
		{hqq.ErrAlreadyNegotiated, "ErrAlreadyNegotiated"},
		{hqq.ErrNegotiationFailed, "ErrNegotiationFailed"},
		{hqq.ErrNegotiationRejected, "ErrNegotiationRejected"},
		{hqq.ErrUnexpectedPacket, "ErrUnexpectedPacket"},
		{hqq.ErrNotListening, "ErrNotListening"},
		{hqq.ErrAlreadyListening, "ErrAlreadyListening"},
		{hqq.ErrMessageAborted, "ErrMessageAborted"},
		{hqq.ErrProtocol, "ErrProtocol"},
		{hqq.ErrIncomingOverflow, "ErrIncomingOverflow"},
		{hqq.ErrReassemblyLimit, "ErrReassemblyLimit"},
		{hqq.ErrBufferOverflow, "ErrBufferOverflow"},
		{hqq.ErrInvalidSize, "ErrInvalidSize"},
		{hqq.ErrMemoryAlign, "ErrMemoryAlign"},
		{hqq.ErrMemorySmall, "ErrMemorySmall"},
		{hqq.ErrInvalidName, "ErrInvalidName"},
		{hqq.ErrInvalidOp, "ErrInvalidOp"},
		{hqq.ErrFailedInit, "ErrFailedInit"},
		{hqq.ErrConnNotFound, "ErrConnNotFound"},
		{io.EOF, "EOF"},
		{os.ErrNotExist, "ErrNotExist"},
	}
}
