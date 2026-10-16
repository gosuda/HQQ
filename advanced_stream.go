package hqq

import (
	"context"
	"slices"
	"time"

	"gosuda.org/hqq/internal/protocol"
)

// ListenStreams starts accepting logical stream requests (OpenStream from the
// peer). Listening stops when ctx is done or the link is closed.
//
// A request that arrives before ListenStreams is held for
// AdvancedOptions.StreamListenGrace. ListenStreams accepts the held requests
// in arrival order, as if they had just arrived. A request still held when its
// grace period ends, or when the link fails, is refused with ErrConnNotFound.
func (l *AdvancedLink) ListenStreams(ctx context.Context) error {
	if err := l.requireNegotiated(); err != nil {
		return err
	}
	ctx = orBackground(ctx)
	// Listening must be set before the dispatcher starts: a stream request
	// routed in between would be held although ListenStreams is running (and
	// refused when its grace period ends). The held requests are admitted under
	// the same lock, so a request that arrives meanwhile is ordered after them.
	l.controlMu.Lock()
	if l.listening {
		l.controlMu.Unlock()
		return ErrAlreadyListening
	}
	l.listening = true
	l.admitHeldLocked()
	l.controlMu.Unlock()
	if err := l.startDispatcher(); err != nil {
		l.controlMu.Lock()
		l.listening = false
		l.controlMu.Unlock()
		return err
	}

	if done := ctx.Done(); done != nil {
		go func() {
			select {
			case <-done:
			case <-l.dispatchDone:
			}
			l.controlMu.Lock()
			l.listening = false
			l.controlMu.Unlock()
		}()
	}
	return nil
}

// AcceptStream waits for and accepts an incoming stream. The stream is
// registered when its OpConnCreate arrives, so messages the peer sends on it
// are queued (and, under OverflowBlock, applying backpressure) even before
// AcceptStream returns it.
func (l *AdvancedLink) AcceptStream(ctx context.Context) (*Stream, error) {
	ctx = orBackground(ctx)
	if err := l.requireNegotiated(); err != nil {
		return nil, err
	}
	if err := l.startDispatcher(); err != nil {
		return nil, err
	}
	l.controlMu.Lock()
	listening := l.listening
	l.controlMu.Unlock()
	if !listening {
		return nil, ErrNotListening
	}
	select {
	case st := <-l.acceptCh:
		l.releaseAcceptSlot()
		return st, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-l.dispatchDone:
		return nil, l.dispatchFailure()
	}
}

// OpenStream creates a logical stream by sending OpConnCreate and waiting for
// the peer's OpConnAccept; the peer must have called ListenStreams and
// accepts it with AcceptStream. A request that reaches a peer that is not
// listening yet is held there for the peer's StreamListenGrace: OpenStream
// succeeds if the peer calls ListenStreams in time, and fails with
// ErrConnNotFound otherwise. Primary endpoints allocate even non-zero
// stream IDs (2, 4, ...) and Secondary endpoints odd IDs (1, 3, ...), so
// both sides can OpenStream concurrently without colliding. ID 0 is the
// link-level stream.
func (l *AdvancedLink) OpenStream(ctx context.Context) (*Stream, error) {
	ctx = orBackground(ctx)
	if err := l.requireNegotiated(); err != nil {
		return nil, err
	}
	if err := l.startDispatcher(); err != nil {
		return nil, err
	}
	connID := l.nextConnectionID()
	st := newStream(l, connID)
	accepted := make(chan error, 1)
	l.controlMu.Lock()
	l.streams[connID] = st
	l.pendingConnections[connID] = accepted
	l.controlMu.Unlock()

	cleanup := func() {
		l.controlMu.Lock()
		if l.pendingConnections[connID] == accepted {
			delete(l.pendingConnections, connID)
		}
		delete(l.streams, connID)
		l.controlMu.Unlock()
	}

	if err := l.sendPacket(ctx, protocol.NewPacket(protocol.OpConnCreate, connID)); err != nil {
		cleanup()
		return nil, err
	}

	select {
	case err := <-accepted:
		if err != nil {
			cleanup()
			return nil, err
		}
		return st, nil
	case <-ctx.Done():
		cleanup()
		return nil, ctx.Err()
	case <-l.dispatchDone:
		cleanup()
		return nil, l.dispatchFailure()
	}
}

// handlePeerClose processes the peer's OpConnClose: the stream is removed
// and reports ErrPeerClosed once its queued messages are consumed. An ID
// that is unknown or still opening is ignored.
func (l *AdvancedLink) handlePeerClose(connID uint64) {
	l.controlMu.Lock()
	st := l.streams[connID]
	if st != nil && l.pendingConnections[connID] == nil {
		delete(l.streams, connID)
	} else {
		st = nil
	}
	l.controlMu.Unlock()
	if st != nil {
		st.markClosed(streamPeerClosed)
		l.cancelStreamRequests(connID)
	}
}

func (l *AdvancedLink) handleControlPacket(ctx context.Context, p *protocol.Packet) error {
	switch p.Op() {
	case protocol.OpConnCreate:
		return l.handleConnCreate(ctx, p.Operand(0))
	case protocol.OpConnAccept:
		l.handleConnAccept(p.Operand(0))
		return nil
	case protocol.OpConnClose:
		l.handlePeerClose(p.Operand(0))
		return nil
	default:
		return ErrUnexpectedPacket
	}
}

// nextConnectionID allocates a connection ID of this endpoint's parity.
func (l *AdvancedLink) nextConnectionID() uint64 {
	for {
		n := l.connectionIDGenerator.Add(1)
		if l.Mode() == LinkModePrimary {
			if id := n << 1; id != 0 {
				return id
			}
			continue
		}
		return n<<1 - 1
	}
}

// peerOwnsConnectionID reports whether connID has the parity the peer
// allocates from: odd IDs belong to the Secondary, even non-zero IDs to the
// Primary.
func (l *AdvancedLink) peerOwnsConnectionID(connID uint64) bool {
	if connID == 0 {
		return false
	}
	peerIsSecondary := l.Mode() == LinkModePrimary
	return (connID&1 == 1) == peerIsSecondary
}

// defaultStreamListenGrace is the StreamListenGrace of a zero AdvancedOptions.
const defaultStreamListenGrace = 2 * time.Second

// heldCreate is an OpConnCreate that arrived while the link was not
// listening. It holds no stream yet: the stream is registered when
// ListenStreams admits the request.
type heldCreate struct {
	connID uint64
	timer  *time.Timer // refuses the request when the grace period ends
}

// handleConnCreate answers an incoming OpConnCreate without waiting for the
// tx ring: the accept or refusal is queued on the control sender (see
// advanced_control.go). An accepted stream is registered immediately but
// reaches AcceptStream only after its OpConnAccept was enqueued on the tx
// ring, so data the acceptor sends on it cannot overtake the accept.
//
// A request that arrives while the link is not listening is held instead
// (see holdCreateLocked) and refused only if it is not admitted in time.
//
// Room in the accept queue is reserved up front according to the overflow
// policy: OverflowBlock waits for AcceptStream (the only wait, and an
// application one), OverflowDrop refuses the stream with ErrCodeBufferOverflow
// and counts it in DroppedMessages, OverflowFail returns ErrIncomingOverflow.
func (l *AdvancedLink) handleConnCreate(ctx context.Context, connID uint64) error {
	l.controlMu.Lock()
	if !l.listening {
		held := l.holdCreateLocked(connID)
		l.controlMu.Unlock()
		if !held {
			l.refuseStream(connID, ErrCodeConnNotFound)
		}
		return nil
	}
	// Refuse an ID the peer may not allocate and never replace a live
	// stream: either would be a protocol violation by the peer.
	if _, exists := l.streams[connID]; exists || !l.peerOwnsConnectionID(connID) {
		l.controlMu.Unlock()
		l.refuseStream(connID, ErrCodeInvalidOp)
		return nil
	}
	st := newStream(l, connID)
	l.streams[connID] = st
	l.controlMu.Unlock()

	switch l.overflow {
	case OverflowDrop:
		select {
		case l.acceptSlots <- struct{}{}:
		default:
			l.forgetConnection(connID)
			l.stats.droppedMessages.Add(1)
			l.refuseStream(connID, ErrCodeBufferOverflow)
			return nil
		}
	case OverflowFail:
		select {
		case l.acceptSlots <- struct{}{}:
		default:
			l.forgetConnection(connID)
			return ErrIncomingOverflow
		}
	default:
		select {
		case l.acceptSlots <- struct{}{}:
		case <-ctx.Done():
			l.forgetConnection(connID)
			return ctx.Err()
		}
	}
	if !l.enqueueControl(controlJob{packet: protocol.NewPacket(protocol.OpConnAccept, connID), kind: controlAccept, stream: st}) {
		l.forgetConnection(connID)
		l.releaseAcceptSlot()
		l.stats.droppedMessages.Add(1)
	}
	return nil
}

// holdCreateLocked holds an OpConnCreate that arrives while the link is not
// listening, for StreamListenGrace. It reports false when the request must be
// refused at once: holding is disabled (negative grace), the link is closing,
// or the bound is reached. The bound keeps the held requests and the accept
// slots taken by streams that AcceptStream has not returned yet within
// IncomingQueueSize, so ListenStreams always finds room for every held request.
// Caller holds controlMu.
func (l *AdvancedLink) holdCreateLocked(connID uint64) bool {
	if l.streamListenGrace < 0 || l.lifeCtx.Err() != nil || len(l.held)+len(l.acceptSlots) >= l.queueSize {
		return false
	}
	h := &heldCreate{connID: connID}
	// The callback needs controlMu, which the caller holds: h.timer is set
	// before it can run.
	h.timer = time.AfterFunc(l.streamListenGrace, func() { l.expireHeld(h) })
	l.held = append(l.held, h)
	return true
}

// admitHeldLocked admits the held requests in arrival order, as if each had
// just arrived while listening. Caller holds controlMu.
func (l *AdvancedLink) admitHeldLocked() {
	held := l.held
	l.held = nil
	for _, h := range held {
		h.timer.Stop() // a callback that already runs finds h gone
		l.admitCreateLocked(h.connID)
	}
}

// admitCreateLocked is handleConnCreate for a held request, under controlMu.
// It never waits: the bound of holdCreateLocked leaves room for every held
// request in the accept queue. A request that finds no slot anyway (accept
// slots taken before the link stopped listening) is refused with
// ErrCodeBufferOverflow rather than waiting under controlMu.
func (l *AdvancedLink) admitCreateLocked(connID uint64) {
	if _, exists := l.streams[connID]; exists || !l.peerOwnsConnectionID(connID) {
		l.refuseStream(connID, ErrCodeInvalidOp)
		return
	}
	select {
	case l.acceptSlots <- struct{}{}:
	default:
		l.stats.droppedMessages.Add(1)
		l.refuseStream(connID, ErrCodeBufferOverflow)
		return
	}
	st := newStream(l, connID)
	l.streams[connID] = st
	if !l.enqueueControl(controlJob{packet: protocol.NewPacket(protocol.OpConnAccept, connID), kind: controlAccept, stream: st}) {
		delete(l.streams, connID)
		l.releaseAcceptSlot()
		l.stats.droppedMessages.Add(1)
	}
}

// expireHeld refuses a held request whose grace period ended. A request that
// ListenStreams or releaseHeld already took is no longer in the list.
func (l *AdvancedLink) expireHeld(h *heldCreate) {
	l.controlMu.Lock()
	defer l.controlMu.Unlock()
	i := slices.Index(l.held, h)
	if i < 0 {
		return
	}
	l.held = slices.Delete(l.held, i, i+1)
	l.refuseStream(h.connID, ErrCodeConnNotFound)
}

// releaseHeld ends the hold of every held request. With refuse, each one is
// refused (the link failed or is shutting down); otherwise each is discarded
// and counted, like the control replies that Close discards.
func (l *AdvancedLink) releaseHeld(refuse bool) {
	l.controlMu.Lock()
	defer l.controlMu.Unlock()
	for _, h := range l.held {
		h.timer.Stop()
		if refuse {
			l.refuseStream(h.connID, ErrCodeConnNotFound)
		} else {
			l.stats.droppedMessages.Add(1)
		}
	}
	l.held = nil
}

func (l *AdvancedLink) handleConnAccept(connID uint64) {
	l.controlMu.Lock()
	pending := l.pendingConnections[connID]
	delete(l.pendingConnections, connID)
	l.controlMu.Unlock()
	if pending != nil {
		pending <- nil // buffered, single sender
	}
}

// routeControlError delivers an OpError that refers to a pending OpenStream. It
// returns false if the error does not belong to one.
func (l *AdvancedLink) routeControlError(p *protocol.Packet) bool {
	connID := p.Operand(0)
	l.controlMu.Lock()
	pending := l.pendingConnections[connID]
	if pending != nil {
		delete(l.pendingConnections, connID)
		delete(l.streams, connID)
	}
	l.controlMu.Unlock()
	if pending == nil {
		return false
	}
	pending <- errorFromCode(ErrorCode(p.Operand(1)))
	return true
}

func (l *AdvancedLink) failPendingConnections(err error) {
	l.controlMu.Lock()
	defer l.controlMu.Unlock()
	for connID, ch := range l.pendingConnections {
		delete(l.pendingConnections, connID)
		delete(l.streams, connID)
		select {
		case ch <- err:
		default:
		}
	}
}
