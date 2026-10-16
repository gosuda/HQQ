package hqq

import (
	"errors"
	"fmt"
	"io"
	"net"
	"os"

	"gosuda.org/hqq/internal/shm"
)

// ErrorCode represents HQQ-specific error codes carried in protocol packets
// (OpError operand 1, tombstone reasons, and Advanced error statuses).
type ErrorCode uint64

const (
	ErrCodeMemoryAlign    ErrorCode = 0x01 // Memory alignment violation
	ErrCodeInvalidSize    ErrorCode = 0x02 // Invalid buffer count, size, index, or chunk bounds
	ErrCodeMemorySmall    ErrorCode = 0x03 // Memory too small for configuration
	ErrCodeFailedInit     ErrorCode = 0x04 // Failed to initialize or attach
	ErrCodeConnNotFound   ErrorCode = 0x05 // Connection or request context not found
	ErrCodeInvalidOp      ErrorCode = 0x06 // Unknown or unsupported operation
	ErrCodeBufferOverflow ErrorCode = 0x07 // Payload exceeds configured limits
)

// Base errors. They are stable sentinels: compare with errors.Is.
var (
	ErrMemoryAlign    = errors.New("hqq: memory alignment violation")
	ErrInvalidSize    = errors.New("hqq: invalid buffer ring size")
	ErrMemorySmall    = errors.New("hqq: memory too small")
	ErrFailedInit     = errors.New("hqq: failed to initialize buffer ring")
	ErrConnNotFound   = errors.New("hqq: connection not found")
	ErrInvalidOp      = errors.New("hqq: invalid operation")
	ErrBufferOverflow = errors.New("hqq: buffer overflow")

	// ErrTimeout is returned when a read or write deadline passes. It
	// implements net.Error with Timeout() == true and matches
	// os.ErrDeadlineExceeded, as net.PacketConn users expect.
	ErrTimeout error = &timeoutError{}
)

// Refined errors. Each one wraps the base error it used to be reported as,
// so existing errors.Is checks against the base errors keep working.
var (
	// ErrClosed is returned by operations on a link, or on a Stream, that has
	// been closed locally. It matches net.ErrClosed.
	ErrClosed = &hqqError{msg: "hqq: link closed", base: net.ErrClosed}

	// ErrPeerClosed is returned by blocking operations after the peer
	// endpoint closed the link gracefully and no buffered data remains, and
	// by Send, Recv and Call of a Stream the peer closed (Recv after the
	// messages queued before the close were consumed). It matches io.EOF.
	ErrPeerClosed = &hqqError{msg: "hqq: peer closed the link", base: io.EOF}

	// ErrPeerGone is returned when the peer endpoint stopped heartbeating
	// (crashed or hung) or the shared segment was re-initialized under this
	// endpoint. The link must be closed and re-established.
	ErrPeerGone = &hqqError{msg: "hqq: peer is gone (no heartbeat)", base: ErrConnNotFound}

	// ErrLinkBusy is returned when both endpoints of a segment are already
	// attached and alive.
	ErrLinkBusy = &hqqError{msg: "hqq: link already has two live endpoints", base: ErrFailedInit}

	// ErrIncompatibleLayout is returned when the shared segment was created by
	// an incompatible HQQ version.
	ErrIncompatibleLayout = &hqqError{msg: "hqq: incompatible shared memory layout", base: ErrFailedInit}

	// ErrNotNegotiated is returned by Advanced APIs before successful
	// protocol negotiation, or when the required feature was not negotiated.
	ErrNotNegotiated = &hqqError{msg: "hqq: advanced protocol not negotiated", base: ErrInvalidOp}

	// ErrAlreadyNegotiated is returned when negotiation is attempted after it
	// succeeded or while it is in progress.
	ErrAlreadyNegotiated = &hqqError{msg: "hqq: protocol already negotiated or in progress", base: ErrInvalidOp}

	// ErrNegotiationFailed is returned when negotiation is attempted again
	// after an earlier attempt failed. Failure is terminal: negotiation frames
	// of the failed attempt may still be in flight, so the link must be closed
	// and a new one opened.
	ErrNegotiationFailed = &hqqError{msg: "hqq: protocol negotiation failed earlier; reopen the link", base: ErrInvalidOp}

	// ErrNegotiationRejected is returned when the peer rejects negotiation or
	// acknowledges an incompatible version or feature set.
	ErrNegotiationRejected = &hqqError{msg: "hqq: protocol negotiation rejected", base: ErrInvalidOp}

	// ErrUnexpectedPacket is returned when a packet arrives on an API that
	// cannot consume it, for example an Advanced frame read through the
	// Standard Read APIs. The offending packet is discarded.
	ErrUnexpectedPacket = &hqqError{msg: "hqq: unexpected packet for this API", base: ErrInvalidOp}

	// ErrProtocol is returned for malformed frames (bad flags, bounds, or
	// chunk ordering).
	ErrProtocol = &hqqError{msg: "hqq: protocol violation", base: ErrInvalidOp}

	// ErrNotListening is returned by AcceptStream before ListenStreams.
	ErrNotListening = &hqqError{msg: "hqq: not listening for connections", base: ErrInvalidOp}

	// ErrAlreadyListening is returned by a second ListenStreams.
	ErrAlreadyListening = &hqqError{msg: "hqq: already listening for connections", base: ErrInvalidOp}

	// ErrIncomingOverflow is returned when the queue a message is routed to
	// is full and AdvancedOptions.IncomingOverflow is OverflowFail.
	ErrIncomingOverflow = &hqqError{msg: "hqq: incoming message queue overflow", base: ErrBufferOverflow}

	// ErrMessageAborted is returned by Stream.Call when the peer abandoned
	// the response mid-transfer (its multi-chunk send failed). The call can
	// be retried. Aborted messages that are not responses to a call never
	// reach Stream.Recv or Serve: they are dropped, because they were never
	// complete.
	ErrMessageAborted = &hqqError{msg: "hqq: message aborted by sender", base: ErrProtocol}

	// ErrReassemblyLimit is returned by Stream.Call when this endpoint
	// refused to assemble the response because incomplete messages already
	// held AdvancedOptions.MaxReassemblyBytes. A request refused for the same
	// reason is answered with an error response whose status is
	// ErrCodeBufferOverflow, so the peer's Call fails with a
	// ResponseError.
	ErrReassemblyLimit = &hqqError{msg: "hqq: reassembly memory limit exceeded", base: ErrBufferOverflow}

	// ErrInvalidName is returned for shared-memory names that are empty,
	// contain path separators or NUL bytes, or exceed the platform limit.
	ErrInvalidName = shm.ErrInvalidName
)

// Transient attach errors. All wrap ErrFailedInit; Dial retries only on
// these (and on a missing name), never on a permanent ErrFailedInit variant
// such as ErrIncompatibleLayout.
var (
	// errSegmentNotReady is returned while a named segment exists but its
	// creator has not sized it yet.
	errSegmentNotReady = &hqqError{msg: "hqq: shared memory segment is not initialized yet", base: ErrFailedInit}

	// errInitContended is returned when another endpoint kept initializing
	// the segment past AttachTimeout, or took over this endpoint's
	// initialization.
	errInitContended = &hqqError{msg: "hqq: shared memory segment initialization did not complete", base: ErrFailedInit}

	// errNoLiveEndpoint is returned to an opener (OpenStandardLink,
	// OpenAdvancedLink, Dial) when every endpoint of the segment closed or
	// died. Openers never re-initialize a segment; a new creator (Create*,
	// Listen) reuses it.
	errNoLiveEndpoint = &hqqError{msg: "hqq: shared memory segment has no live endpoint", base: ErrFailedInit}
)

// hqqError is an error with a stable message that wraps a broader base error.
type hqqError struct {
	msg  string
	base error
}

func (e *hqqError) Error() string { return e.msg }
func (e *hqqError) Unwrap() error { return e.base }

// timeoutError is the concrete type of ErrTimeout.
type timeoutError struct{}

func (*timeoutError) Error() string   { return "hqq: operation timed out" }
func (*timeoutError) Timeout() bool   { return true }
func (*timeoutError) Temporary() bool { return true }
func (*timeoutError) Unwrap() error   { return os.ErrDeadlineExceeded }

var _ net.Error = (*timeoutError)(nil)

// unknownCodeError reports an OpError packet with an unknown code. It
// matches ErrProtocol: the peer sent something this version does not
// understand.
type unknownCodeError struct{ code ErrorCode }

func (e unknownCodeError) Error() string {
	return fmt.Sprintf("hqq: peer reported unknown error code %#x", uint64(e.code))
}

func (e unknownCodeError) Unwrap() error { return ErrProtocol }

// GeometryError reports that an existing shared-memory segment was laid out
// for a different configuration than the caller asked for. It matches
// ErrInvalidSize.
type GeometryError struct {
	// Want is the configuration requested by the caller; Got is the one
	// recorded in the segment.
	Want, Got Geometry
}

// Geometry is the layout configuration of a segment.
type Geometry struct {
	BufferCount, BufferSize int
	LayoutSize              uintptr
	PageSize                uintptr
}

func (e *GeometryError) Error() string {
	return fmt.Sprintf("hqq: segment geometry mismatch: want %d x %d bytes (layout %d, page %d), segment has %d x %d bytes (layout %d, page %d)",
		e.Want.BufferCount, e.Want.BufferSize, e.Want.LayoutSize, e.Want.PageSize,
		e.Got.BufferCount, e.Got.BufferSize, e.Got.LayoutSize, e.Got.PageSize)
}

func (e *GeometryError) Unwrap() error { return ErrInvalidSize }

// errorFromCode converts a protocol error code to an error.
func errorFromCode(code ErrorCode) error {
	switch code {
	case ErrCodeMemoryAlign:
		return ErrMemoryAlign
	case ErrCodeInvalidSize:
		return ErrInvalidSize
	case ErrCodeMemorySmall:
		return ErrMemorySmall
	case ErrCodeFailedInit:
		return ErrFailedInit
	case ErrCodeConnNotFound:
		return ErrConnNotFound
	case ErrCodeInvalidOp:
		return ErrInvalidOp
	case ErrCodeBufferOverflow:
		return ErrBufferOverflow
	default:
		return unknownCodeError{code}
	}
}

// ResponseError reports an error response to a request. Stream.Call returns
// it when the peer's handler answered with ResponseWriter.WriteError. Payload
// is a copy owned by the caller.
type ResponseError struct {
	Status  uint64
	Payload []byte
}

func (e ResponseError) Error() string {
	if len(e.Payload) > 0 && len(e.Payload) <= 256 {
		return fmt.Sprintf("hqq: response error (status %d): %s", e.Status, e.Payload)
	}
	return fmt.Sprintf("hqq: response error (status %d, %d byte payload)", e.Status, len(e.Payload))
}
