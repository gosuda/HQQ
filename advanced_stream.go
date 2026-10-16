package hqq

import (
	"context"

	"gosuda.org/hqq/internal/protocol"
)

// Listen starts accepting logical stream requests (Dial from the peer).
// Listening stops when ctx is done or the link is closed. Without Listen,
// incoming stream requests are refused.
func (l *AdvancedLink) Listen(ctx context.Context) error {
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

// Accept waits for and accepts an incoming stream and returns its
// connection ID, to be used with CallOn and SendLarge.
func (l *AdvancedLink) Accept(ctx context.Context) (uint64, error) {
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
		return connID, nil
	case <-ctx.Done():
		return 0, ctx.Err()
	case <-l.dispatchDone:
		return 0, l.dispatchFailure()
	}
}

// Dial creates a logical Advanced stream by sending OpConnCreate and waiting
// for the peer's OpConnAccept. It returns the new connection ID: Primary
// endpoints allocate even non-zero IDs (2, 4, ...) and Secondary endpoints
// odd IDs (1, 3, ...), so both sides can Dial concurrently without
// colliding. ID 0 is reserved for link-level traffic.
func (l *AdvancedLink) Dial(ctx context.Context) (uint64, error) {
	ctx = orBackground(ctx)
	if err := l.requireNegotiated(); err != nil {
		return 0, err
	}
	if err := l.startDispatcher(); err != nil {
		return 0, err
	}
	connID := l.nextConnectionID()
	conn := newConnection(connID, ConnectionStateOpening)
	accepted := make(chan error, 1)
	l.controlMu.Lock()
	l.connections[connID] = conn
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
		conn.state.Store(uint32(ConnectionStateOpen))
		return connID, nil
	case <-ctx.Done():
		cleanup()
		return 0, ctx.Err()
	case <-l.dispatchDone:
		cleanup()
		return 0, l.dispatchFailure()
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
		l.controlMu.Lock()
		if conn := l.connections[p.Operand(0)]; conn != nil {
			conn.state.Store(uint32(ConnectionStateClosed))
			delete(l.connections, p.Operand(0))
		}
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

func (l *AdvancedLink) handleConnCreate(ctx context.Context, connID uint64) error {
	refuse := func(code ErrorCode) error {
		return l.sendPacket(ctx, protocol.NewPacket(protocol.OpError, connID, uint64(code)))
	}
	l.controlMu.Lock()
	if !l.listening {
		l.controlMu.Unlock()
		return refuse(ErrCodeConnNotFound)
	}
	// Refuse an ID the peer may not allocate and never replace a live
	// stream: either would be a protocol violation by the peer.
	if _, exists := l.connections[connID]; exists || !l.peerOwnsConnectionID(connID) {
		l.controlMu.Unlock()
		return refuse(ErrCodeInvalidOp)
	}
	l.connections[connID] = newConnection(connID, ConnectionStateOpen)
	l.controlMu.Unlock()

	forget := func() {
		l.controlMu.Lock()
		delete(l.connections, connID)
		l.controlMu.Unlock()
	}
	if err := l.sendPacket(ctx, protocol.NewPacket(protocol.OpConnAccept, connID)); err != nil {
		forget()
		return err
	}

	switch l.overflow {
	case OverflowDrop:
		select {
		case l.acceptCh <- connID:
		default:
			forget()
			l.stats.droppedMessages.Add(1)
			_ = l.sendPacket(ctx, protocol.NewPacket(protocol.OpConnClose, connID, uint64(ErrCodeBufferOverflow)))
		}
		return nil
	case OverflowFail:
		select {
		case l.acceptCh <- connID:
			return nil
		default:
			forget()
			return ErrIncomingOverflow
		}
	default:
		select {
		case l.acceptCh <- connID:
			return nil
		case <-ctx.Done():
			forget()
			return ctx.Err()
		}
	}
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

// routeControlError delivers an OpError that refers to a pending Dial. It
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
