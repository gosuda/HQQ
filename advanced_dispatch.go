package hqq

import (
	"context"
)

// startDispatcher starts the background dispatcher once. The dispatcher owns
// the receive direction: it assembles messages, routes responses to pending
// calls and queues everything else for Receive.
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
		ctx, cancel := context.WithCancel(context.Background())
		l.dispatchCancel = cancel
		l.dispatchStarted.Store(true)
		go l.dispatchLoop(ctx)
	}
	return l.currentDispatchErr()
}

func (l *AdvancedLink) dispatchLoop(ctx context.Context) {
	defer close(l.dispatchDone)
	for {
		l.receiveMu.Lock()
		message, err := l.receiveNextLocked(ctx)
		l.receiveMu.Unlock()
		if err == nil {
			if message.IsResponse() && l.routeResponse(message) {
				continue
			}
			err = l.deliverIncoming(ctx, message)
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

// deliverIncoming queues a non-response message for Receive according to the
// overflow policy.
func (l *AdvancedLink) deliverIncoming(ctx context.Context, message AdvancedMessage) error {
	result := advancedDispatchResult{message: message}
	switch l.overflow {
	case OverflowDrop:
		select {
		case l.incoming <- result:
		default:
			l.stats.droppedMessages.Add(1)
		}
		return nil
	case OverflowFail:
		select {
		case l.incoming <- result:
			return nil
		default:
			return ErrIncomingOverflow
		}
	default:
		select {
		case l.incoming <- result:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// routeResponse hands a response to its pending call. It returns false if no
// call is waiting for it (the response is then delivered to Receive), and
// true if it was routed or recognized as a late response to a canceled call
// and discarded.
func (l *AdvancedLink) routeResponse(message AdvancedMessage) bool {
	key := advancedCallKey{connectionID: message.ConnectionID, messageID: message.MessageID}
	l.pendingMu.Lock()
	pending := l.pendingCalls[key]
	if pending != nil {
		delete(l.pendingCalls, key)
	} else if _, ok := l.abandoned[key]; ok {
		delete(l.abandoned, key)
		l.pendingMu.Unlock()
		l.stats.abandonedResponses.Add(1)
		return true
	}
	l.pendingMu.Unlock()
	if pending == nil {
		return false
	}
	// The channel has capacity 1 and only the goroutine that removed the
	// entry sends on it, so this never blocks.
	pending <- advancedDispatchResult{message: message}
	return true
}

func (l *AdvancedLink) registerPendingCall(key advancedCallKey) chan advancedDispatchResult {
	ch := pendingPool.Get().(chan advancedDispatchResult)
	l.pendingMu.Lock()
	l.pendingCalls[key] = ch
	l.pendingMu.Unlock()
	return ch
}

func (l *AdvancedLink) unregisterPendingCall(key advancedCallKey, ch chan advancedDispatchResult) {
	l.pendingMu.Lock()
	if l.pendingCalls[key] == ch {
		delete(l.pendingCalls, key)
	}
	l.pendingMu.Unlock()
}

// abandonCall removes a call that gives up before its response arrived and
// remembers its key so that a late response is discarded instead of being
// delivered to Receive.
func (l *AdvancedLink) abandonCall(key advancedCallKey, ch chan advancedDispatchResult) {
	l.pendingMu.Lock()
	defer l.pendingMu.Unlock()
	if l.pendingCalls[key] != ch {
		// Already routed or failed; the result is simply dropped. The
		// channel may still receive a value, so it is not recycled.
		return
	}
	delete(l.pendingCalls, key)
	if len(l.abandonedOrder) >= maxAbandonedCalls {
		oldest := l.abandonedOrder[0]
		l.abandonedOrder = l.abandonedOrder[1:]
		delete(l.abandoned, oldest)
	}
	l.abandoned[key] = struct{}{}
	l.abandonedOrder = append(l.abandonedOrder, key)
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
