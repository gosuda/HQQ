package hqq

import (
	"context"
	"sync"
	"sync/atomic"

	"gosuda.org/hqq/internal/protocol"
)

// Stream state bits.
const (
	streamLocalClosed uint32 = 1 << iota // Close was called on this endpoint
	streamPeerClosed                     // the peer closed the stream (OpConnClose)
)

// Stream is one logical, reliable, ordered, message-oriented channel of an
// AdvancedLink. Messages sent on a stream arrive at the peer's stream with the
// same ID: Send/Recv carry plain messages, Call/Serve carry request-response
// pairs. Stream 0 is the link-level stream: it is always open and cannot be
// closed; AdvancedLink.Send, Recv and Call operate on it. Further streams are
// created by OpenStream and AcceptStream, and stay valid until Close or until
// the peer closes them. Stream IDs are never reused.
//
// Ordering: messages sent one after the other on a stream arrive in that
// order. Concurrent Sends on a stream are not ordered relative to each other,
// and there is no ordering between different streams.
//
// All methods are safe for concurrent use.
type Stream struct {
	link  *AdvancedLink
	id    uint64
	queue chan *Message // complete data messages, bounded by IncomingQueueSize

	state     atomic.Uint32 // streamLocalClosed | streamPeerClosed
	done      chan struct{} // closed when the stream is closed (either side)
	closeDone sync.Once
}

func newStream(l *AdvancedLink, id uint64) *Stream {
	s := &Stream{
		link:  l,
		id:    id,
		queue: make(chan *Message, l.queueSize),
		done:  make(chan struct{}),
	}
	return s
}

// ID returns the stream ID: 0 for the link-level stream, otherwise even
// (allocated by the Primary endpoint) or odd (allocated by the Secondary).
func (s *Stream) ID() uint64 {
	return s.id
}

// closedErr returns the error of operations on a closed stream: ErrClosed
// after a local Close, ErrPeerClosed after the peer closed it, nil if open.
func (s *Stream) closedErr() error {
	st := s.state.Load()
	switch {
	case st&streamLocalClosed != 0:
		return ErrClosed
	case st&streamPeerClosed != 0:
		return ErrPeerClosed
	}
	return nil
}

// markClosed sets a state bit and wakes the operations blocked on the
// stream. It reports whether this call set the bit.
func (s *Stream) markClosed(bit uint32) bool {
	first := s.state.Or(bit)&bit == 0
	if first {
		s.closeDone.Do(s.closeDoneCh)
	}
	return first
}

func (s *Stream) closeDoneCh() { close(s.done) }

// drain releases the messages still queued on the stream.
func (s *Stream) drain() {
	for {
		select {
		case m := <-s.queue:
			m.Release()
		default:
			return
		}
	}
}

// Send publishes p as one message of this stream, split into OpConnCopy
// chunks of at most BufferSize bytes; the peer receives it as one message
// from Recv. p may be any size up to MaxMessageSize (ErrBufferOverflow
// otherwise, nothing is sent) and may be reused as soon as Send returns.
//
// Send returns ErrClosed after a local Close of the stream or of the link,
// ErrPeerClosed after the peer closed the stream, ErrPeerGone if the peer
// died, ErrNotNegotiated without FeatureMessages and ctx.Err() if ctx ends
// while the tx ring is full.
func (s *Stream) Send(ctx context.Context, p []byte) error {
	l := s.link
	if err := l.requireFeature(FeatureMessages); err != nil {
		return err
	}
	if err := s.closedErr(); err != nil {
		return err
	}
	if err := l.sendAdvancedPayload(orBackground(ctx), s.id, l.nextMessageID(), 0, 0, p); err != nil {
		return err
	}
	l.stats.messages.Add(1)
	l.stats.messageBytes.Add(uint64(len(p)))
	return nil
}

// Recv returns the next complete data message of this stream, in the order the
// peer sent them. It never returns a message of another stream, a request
// (those are consumed by Serve) or a response. The caller owns the message
// and should Release it when done.
//
// Messages the sender abandoned mid-transfer (after a failed multi-chunk
// send) were never complete and are dropped silently.
//
// After a local Close, Recv returns ErrClosed. After the peer closed the
// stream, Recv returns the messages still queued and then ErrPeerClosed.
// A Recv blocked when the stream is closed returns promptly. After the link
// failed, Recv returns the error of the failure (ErrClosed, ErrPeerClosed or
// ErrPeerGone) once the queued messages are consumed. ctx.Err() is returned
// if ctx ends first.
func (s *Stream) Recv(ctx context.Context) (*Message, error) {
	l := s.link
	if err := l.requireNegotiated(); err != nil {
		return nil, err
	}
	return l.receiveQueued(orBackground(ctx), s.queue, s)
}

// Call sends a request on this stream and waits for the matching response.
// The peer answers it through Serve. The response message must be Released
// by the caller. Many calls may be outstanding at once and responses may
// arrive in any order. If ctx ends first, Call returns ctx.Err() and a late
// response is discarded.
//
// An error response is returned as ResponseError; a response the peer
// abandoned mid-transfer fails with ErrMessageAborted, and one this endpoint
// refused under AdvancedOptions.MaxReassemblyBytes with ErrReassemblyLimit.
// Call returns ErrClosed after a local Close, and ErrPeerClosed after the
// peer closed the stream, also when that happens while Call is waiting.
func (s *Stream) Call(ctx context.Context, method uint64, req []byte) (*Message, error) {
	l := s.link
	if err := l.requireFeature(FeatureRequestResponse); err != nil {
		return nil, err
	}
	if err := s.closedErr(); err != nil {
		return nil, err
	}
	ctx = orBackground(ctx)
	messageID := l.nextMessageID()
	key := advancedCallKey{streamID: s.id, messageID: messageID}
	pending := l.registerPendingCall(key)

	if err := l.sendAdvancedPayload(ctx, s.id, messageID, AdvFlagRequest, method, req); err != nil {
		l.unregisterPendingCall(key, pending)
		return nil, err
	}
	l.stats.requests.Add(1)
	m, err := l.awaitResponse(ctx, key, pending, s)
	if err != nil {
		return nil, err
	}
	if m.isError() {
		// Error path: the payload is copied so that the message can be
		// recycled.
		re := ResponseError{Status: m.aux, Payload: append([]byte(nil), m.Bytes()...)}
		m.Release()
		return nil, re
	}
	return m, nil
}

// Close closes the stream: it sends OpConnClose to the peer, whose stream then
// reports ErrPeerClosed after its queued messages, and removes the stream
// from this endpoint. Receive operations blocked on the stream return
// ErrClosed, and so does every later use of it, including a second Close.
// Messages queued but not received are discarded. Close waits for room in
// the tx ring and returns the error of sending OpConnClose; the stream is
// closed locally even then, and the peer keeps its side until the link closes.
//
// Stream IDs are never reused. Requests already in flight on the stream are
// not affected: Serve still answers them. The link-level stream 0 cannot be
// closed: Close returns ErrInvalidOp.
func (s *Stream) Close() error {
	if s.id == 0 {
		return ErrInvalidOp
	}
	l := s.link
	if err := l.requireNegotiated(); err != nil {
		return err
	}
	peerClosed := s.state.Load()&streamPeerClosed != 0
	if !s.markClosed(streamLocalClosed) {
		return ErrClosed
	}
	l.forgetConnection(s.id)
	s.drain()
	if peerClosed {
		return nil // the peer already forgot the stream
	}
	return l.sendPacket(context.Background(), protocol.NewPacket(protocol.OpConnClose, s.id, 0))
}

// Stream returns the link-level stream (ID 0), which is always open. Every
// call returns the same pointer.
func (l *AdvancedLink) Stream() *Stream {
	return l.stream0
}

// Send is l.Stream().Send.
func (l *AdvancedLink) Send(ctx context.Context, p []byte) error {
	return l.stream0.Send(ctx, p)
}

// Recv is l.Stream().Recv.
func (l *AdvancedLink) Recv(ctx context.Context) (*Message, error) {
	return l.stream0.Recv(ctx)
}

// Call is l.Stream().Call.
func (l *AdvancedLink) Call(ctx context.Context, method uint64, req []byte) (*Message, error) {
	return l.stream0.Call(ctx, method, req)
}
