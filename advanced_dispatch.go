package hqq

import (
	"context"
)

// startDispatcher starts the background dispatcher once. The dispatcher owns
// the receive direction: it assembles messages and routes them (see route).
func (l *AdvancedLink) startDispatcher() error {
	if err := l.requireNegotiated(); err != nil {
		return err
	}
	l.dispatchMu.Lock()
	defer l.dispatchMu.Unlock()
	if l.closing {
		return ErrClosed
	}
	if !l.dispatchStarted.Load() {
		l.dispatchStarted.Store(true)
		go l.dispatchLoop(l.lifeCtx)
	}
	return l.currentDispatchErr()
}

func (l *AdvancedLink) dispatchLoop(ctx context.Context) {
	defer close(l.dispatchDone)
	for {
		l.receiveMu.Lock()
		m, hdr, abortErr, err := l.receiveNextLocked(ctx, false)
		l.receiveMu.Unlock()
		if err == nil {
			_, err = l.route(ctx, m, hdr, abortErr, true)
		}
		if err != nil {
			if ctx.Err() != nil {
				err = ErrClosed
			}
			l.setDispatchErr(err)
			l.failPendingCalls(err)
			l.failPendingConnections(err)
			return
		}
	}
}

func (l *AdvancedLink) setDispatchErr(err error) {
	l.dispatchErrMu.Lock()
	if l.dispatchErr == nil {
		l.dispatchErr = err
	}
	l.dispatchErrMu.Unlock()
}

func (l *AdvancedLink) currentDispatchErr() error {
	l.dispatchErrMu.RLock()
	defer l.dispatchErrMu.RUnlock()
	return l.dispatchErr
}

// dispatchFailure returns why the dispatcher stopped.
func (l *AdvancedLink) dispatchFailure() error {
	if err := l.currentDispatchErr(); err != nil {
		return err
	}
	return ErrClosed
}

// stalledDelivery is a message that found its queue full under
// OverflowBlock while the receiver was not allowed to wait (m == nil: none).
type stalledDelivery struct {
	m  *Message
	q  chan *Message
	st *Stream // owner of q, nil for the request queue
}

// route hands one received message to its destination:
//
//   - a response goes to the waiting Call; one without a waiting call (and
//     not an abandoned one) is dropped and counted in DroppedMessages;
//   - a request goes to the request queue consumed by Serve;
//   - a data message goes to the queue of its stream; one for an unknown or
//     closed stream is dropped and counted.
//
// m == nil denotes a response that was aborted or refused (hdr, abortErr),
// which fails the waiting call. blocking tells whether the caller may wait
// for the consumer of a full queue under OverflowBlock; if it may not, the
// message is returned as the stall.
func (l *AdvancedLink) route(ctx context.Context, m *Message, hdr advHeader, abortErr error, blocking bool) (stalledDelivery, error) {
	switch {
	case m == nil:
		l.routeResponse(hdr, nil, abortErr)
		return stalledDelivery{}, nil
	case m.isResponse():
		l.routeResponse(m.advHeader, m, nil)
		return stalledDelivery{}, nil
	case m.isRequest():
		return l.routeRequest(ctx, m, blocking)
	}
	st := l.lookupStream(m.streamID)
	if st == nil || st.state.Load() != 0 {
		l.dropMessage(m)
		return stalledDelivery{}, nil
	}
	return l.deliver(ctx, st.queue, st, m, blocking)
}

// lookupStream returns the open stream id, or nil.
func (l *AdvancedLink) lookupStream(id uint64) *Stream {
	if id == 0 {
		return l.stream0
	}
	l.controlMu.Lock()
	st := l.streams[id]
	l.controlMu.Unlock()
	return st
}

// dropMessage discards a message that has no destination and counts it.
func (l *AdvancedLink) dropMessage(m *Message) {
	l.stats.droppedMessages.Add(1)
	m.Release()
}

// deliver queues m on q according to the overflow policy.
func (l *AdvancedLink) deliver(ctx context.Context, q chan *Message, st *Stream, m *Message, blocking bool) (stalledDelivery, error) {
	select {
	case q <- m:
		return stalledDelivery{}, nil
	default:
	}
	switch l.overflow {
	case OverflowDrop:
		l.dropMessage(m)
		return stalledDelivery{}, nil
	case OverflowFail:
		m.Release()
		return stalledDelivery{}, ErrIncomingOverflow
	}
	if !blocking {
		return stalledDelivery{m: m, q: q, st: st}, nil
	}
	return stalledDelivery{}, l.deliverBlocking(ctx, q, st, m)
}

// deliverBlocking waits until q has room for m. The message is dropped if
// its stream is closed meanwhile, and released if ctx ends first.
func (l *AdvancedLink) deliverBlocking(ctx context.Context, q chan *Message, st *Stream, m *Message) error {
	var closed <-chan struct{}
	if st != nil {
		closed = st.done
	}
	select {
	case q <- m:
		return nil
	case <-closed:
		l.dropMessage(m)
		return nil
	case <-ctx.Done():
		m.Release()
		return ctx.Err()
	}
}

// finishDelivery completes a delivery a synchronous receiver could not wait
// for (see receiveRoute) and then releases receiveMu, which the receiver
// passed on to this goroutine so that no later message overtakes it.
func (l *AdvancedLink) finishDelivery(s stalledDelivery) {
	defer l.receiveMu.Unlock()
	_ = l.deliverBlocking(l.lifeCtx, s.q, s.st, s.m)
}

// routeResponse hands a response to its pending call and reports whether it
// found one. A response of an abandoned (canceled) call is discarded and
// counted in AbandonedResponses; any other response without a waiting call
// is dropped and counted in DroppedMessages. abortErr is non-nil for an
// aborted response (m == nil: one whose sender abandoned it, or that was
// refused by the reassembly limit): it fails the pending call with abortErr
// and is silently consumed when no call waits.
func (l *AdvancedLink) routeResponse(hdr advHeader, m *Message, abortErr error) bool {
	key := advancedCallKey{streamID: hdr.streamID, messageID: hdr.messageID}
	l.pendingMu.Lock()
	pending := l.pendingCalls[key]
	if pending != nil {
		delete(l.pendingCalls, key)
	} else if _, ok := l.abandoned[key]; ok {
		delete(l.abandoned, key)
		l.pendingMu.Unlock()
		l.stats.abandonedResponses.Add(1)
		m.Release()
		return false
	}
	l.pendingMu.Unlock()
	if pending == nil {
		if m != nil {
			l.dropMessage(m)
		}
		return false
	}
	// The channel has capacity 1 and only the goroutine that removed the
	// entry sends on it, so this never blocks.
	pending <- advancedDispatchResult{msg: m, err: abortErr}
	return true
}

func (l *AdvancedLink) registerPendingCall(key advancedCallKey) chan advancedDispatchResult {
	ch := pendingPool.Get().(chan advancedDispatchResult)
	l.pendingMu.Lock()
	l.pendingCalls[key] = ch
	l.pendingMu.Unlock()
	return ch
}

// unregisterPendingCall removes the entry of a call that finished without
// consuming a result from ch, and reports whether it was still registered.
// If so, the caller was the only goroutine that could have sent on ch, which
// is therefore empty and unreachable, and ch is returned to pendingPool. If
// not, routeResponse or failPendingCalls removed the entry first and has
// sent (or is about to send) a result on ch: ch must not be recycled.
func (l *AdvancedLink) unregisterPendingCall(key advancedCallKey, ch chan advancedDispatchResult) bool {
	l.pendingMu.Lock()
	removed := l.pendingCalls[key] == ch
	if removed {
		delete(l.pendingCalls, key)
	}
	l.pendingMu.Unlock()
	if removed {
		pendingPool.Put(ch)
	}
	return removed
}

// abandonCall removes a call that gives up before its response arrived and
// remembers its key so that a late response is discarded (and counted in
// AbandonedResponses) instead of being dropped as unmatched.
//
// It reports whether the call was still pending; if not, a response (or a
// failure) was already routed to ch.
func (l *AdvancedLink) abandonCall(key advancedCallKey, ch chan advancedDispatchResult) bool {
	l.pendingMu.Lock()
	defer l.pendingMu.Unlock()
	if l.pendingCalls[key] != ch {
		// Already routed or failed; the result is simply dropped. The
		// channel may still receive a value, so it is not recycled.
		return false
	}
	delete(l.pendingCalls, key)
	if len(l.abandonedOrder) >= maxAbandonedCalls {
		oldest := l.abandonedOrder[0]
		l.abandonedOrder = l.abandonedOrder[1:]
		delete(l.abandoned, oldest)
	}
	l.abandoned[key] = struct{}{}
	l.abandonedOrder = append(l.abandonedOrder, key)
	return true
}

// giveUp abandons a call that stops waiting. If the call ended because its
// context did (canceled), the peer is told with OpCancel (FeatureCancel), so
// that its handler stops working for a response nobody reads.
func (l *AdvancedLink) giveUp(key advancedCallKey, ch chan advancedDispatchResult, canceled bool) {
	if l.abandonCall(key, ch) && canceled {
		l.cancelPending(key)
	}
}

func (l *AdvancedLink) failPendingCalls(err error) {
	l.pendingMu.Lock()
	defer l.pendingMu.Unlock()
	for key, ch := range l.pendingCalls {
		delete(l.pendingCalls, key)
		select {
		case ch <- advancedDispatchResult{err: err}:
		default:
		}
	}
}
