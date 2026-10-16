package hqq

import (
	"context"

	"gosuda.org/hqq/internal/protocol"
)

// ListenStreams starts accepting logical stream requests (OpenStream from the
// peer). Listening stops when ctx is done or the link is closed. Without
// ListenStreams, incoming stream requests are refused.
func (l *AdvancedLink) ListenStreams(ctx context.Context) error {
	if err := l.requireNegotiated(); err != nil {
		return err
	}
	if err := l.startDispatcher(); err != nil {
		return err
	}
	ctx = orBackground(ctx)
	l.controlMu.Lock()
	if l.listening {
		l.controlMu.Unlock()
		return ErrAlreadyListening
	}
	l.listening = true
	l.controlMu.Unlock()

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

// AcceptStream waits for and accepts an incoming stream and returns its
// connection ID, to be used with CallOn and SendLarge.
func (l *AdvancedLink) AcceptStream(ctx context.Context) (uint64, error) {
	ctx = orBackground(ctx)
	if err := l.requireNegotiated(); err != nil {
		return 0, err
	}
	if err := l.startDispatcher(); err != nil {
		return 0, err
	}
	l.controlMu.Lock()
	listening := l.listening
	l.controlMu.Unlock()
	if !listening {
		return 0, ErrNotListening
	}
	select {
	case connID := <-l.acceptCh:
		l.releaseAcceptSlot()
		return connID, nil
	case <-ctx.Done():
		return 0, ctx.Err()
	case <-l.dispatchDone:
		return 0, l.dispatchFailure()
	}
}

// OpenStream creates a logical Advanced stream by sending OpConnCreate and
// waiting for the peer's OpConnAccept. It returns the new connection ID:
// Primary endpoints allocate even non-zero IDs (2, 4, ...) and Secondary
// endpoints odd IDs (1, 3, ...), so both sides can OpenStream concurrently
// without colliding. ID 0 is reserved for link-level traffic.
func (l *AdvancedLink) OpenStream(ctx context.Context) (uint64, error) {
	ctx = orBackground(ctx)
	if err := l.requireNegotiated(); err != nil {
		return 0, err
	}
	if err := l.startDispatcher(); err != nil {
		return 0, err
	}
	connID := l.nextConnectionID()
	accepted := make(chan error, 1)
	l.controlMu.Lock()
	l.connections[connID] = struct{}{}
	l.pendingConnections[connID] = accepted
	l.controlMu.Unlock()

	cleanup := func() {
		l.controlMu.Lock()
		if l.pendingConnections[connID] == accepted {
			delete(l.pendingConnections, connID)
		}
		delete(l.connections, connID)
		l.controlMu.Unlock()
	}

	if err := l.sendPacket(ctx, protocol.NewPacket(protocol.OpConnCreate, connID)); err != nil {
		cleanup()
		return 0, err
	}

	select {
	case err := <-accepted:
		if err != nil {
			cleanup()
			return 0, err
		}
		return connID, nil
	case <-ctx.Done():
		cleanup()
		return 0, ctx.Err()
	case <-l.dispatchDone:
		cleanup()
		return 0, l.dispatchFailure()
	}
}

// CloseStream closes the stream connectionID, which must have been returned
// by OpenStream or AcceptStream and not closed since. It removes the local
// registration and then sends OpConnClose (reason 0) to the peer, which
// removes its registration when it receives it. The registration is removed
// even if sending fails (for example, ctx ends first); the peer then keeps
// its registration until the link is closed.
//
// Connection IDs are never reused, so a closed ID stays invalid: CallOn,
// SendLarge and CloseStream on it return ErrConnNotFound. Requests already
// in flight on the stream are not affected: their responses are still
// routed, and Respond to a request received on the stream still works.
// CloseStream returns ErrConnNotFound for an ID that is not an open stream
// (including 0), and ErrNotNegotiated before negotiation.
func (l *AdvancedLink) CloseStream(ctx context.Context, connectionID uint64) error {
	if err := l.requireNegotiated(); err != nil {
		return err
	}
	l.controlMu.Lock()
	_, open := l.connections[connectionID]
	if open && l.pendingConnections[connectionID] == nil {
		delete(l.connections, connectionID)
	} else {
		open = false
	}
	l.controlMu.Unlock()
	if !open {
		return ErrConnNotFound
	}
	return l.sendPacket(orBackground(ctx), protocol.NewPacket(protocol.OpConnClose, connectionID, 0))
}

// requireStream rejects a connection ID that is neither 0 (link-level
// traffic) nor an open stream.
func (l *AdvancedLink) requireStream(connectionID uint64) error {
	if connectionID == 0 {
		return nil
	}
	l.controlMu.Lock()
	_, open := l.connections[connectionID]
	l.controlMu.Unlock()
	if !open {
		return ErrConnNotFound
	}
	return nil
}

func (l *AdvancedLink) handleControlPacket(ctx context.Context, p *protocol.Packet) error {
	switch p.Op() {
	case protocol.OpConnCreate:
		return l.handleConnCreate(ctx, p.Operand(0))
	case protocol.OpConnAccept:
		l.handleConnAccept(p.Operand(0))
		return nil
	case protocol.OpConnClose:
		l.controlMu.Lock()
		delete(l.connections, p.Operand(0))
		l.controlMu.Unlock()
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

// handleConnCreate answers an incoming OpConnCreate without waiting for the
// tx ring: the accept or refusal is queued on the control sender (see
// advanced_control.go). An accepted stream is registered immediately but
// reaches AcceptStream only after its OpConnAccept was enqueued on the tx
// ring, so data the acceptor sends on it cannot overtake the accept.
//
// Room in the accept queue is reserved up front according to the overflow
// policy: OverflowBlock waits for AcceptStream (the only wait, and an
// application one), OverflowDrop refuses the stream with ErrCodeBufferOverflow
// and counts it in DroppedMessages, OverflowFail returns ErrIncomingOverflow.
func (l *AdvancedLink) handleConnCreate(ctx context.Context, connID uint64) error {
	l.controlMu.Lock()
	if !l.listening {
		l.controlMu.Unlock()
		l.refuseStream(connID, ErrCodeConnNotFound)
		return nil
	}
	// Refuse an ID the peer may not allocate and never replace a live
	// stream: either would be a protocol violation by the peer.
	if _, exists := l.connections[connID]; exists || !l.peerOwnsConnectionID(connID) {
		l.controlMu.Unlock()
		l.refuseStream(connID, ErrCodeInvalidOp)
		return nil
	}
	l.connections[connID] = struct{}{}
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
	if !l.enqueueControl(controlJob{packet: protocol.NewPacket(protocol.OpConnAccept, connID), kind: controlAccept}) {
		l.forgetConnection(connID)
		l.releaseAcceptSlot()
		l.stats.droppedMessages.Add(1)
	}
	return nil
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
		delete(l.connections, connID)
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
		delete(l.connections, connID)
		select {
		case ch <- err:
		default:
		}
	}
}
