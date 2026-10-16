package hqq

import (
	"context"
	"errors"
	"sync"
)

// Handler serves Advanced requests. ServeHQQ returns the response payload,
// or an error. Returning an AdvancedResponseError selects the status and
// payload of the error response; any other error is answered with status
// ErrCodeInvalidOp and the error text as payload.
type Handler interface {
	ServeHQQ(ctx context.Context, request AdvancedMessage) ([]byte, error)
}

// HandlerFunc adapts a function to Handler.
type HandlerFunc func(ctx context.Context, request AdvancedMessage) ([]byte, error)

// ServeHQQ calls f(ctx, request).
func (f HandlerFunc) ServeHQQ(ctx context.Context, request AdvancedMessage) ([]byte, error) {
	return f(ctx, request)
}

// MessageHandler is optionally implemented by a Handler to receive messages
// that are not requests (SendLarge data and abort notifications). Without it,
// Serve discards such messages.
type MessageHandler interface {
	HandleMessage(ctx context.Context, message AdvancedMessage)
}

// Mux dispatches requests by MethodID. The zero value is ready to use.
type Mux struct {
	mu       sync.RWMutex
	handlers map[uint64]Handler

	// NotFound handles requests for unregistered methods. If nil, they are
	// answered with an error response (status ErrCodeInvalidOp).
	NotFound Handler

	// OnMessage, if set, receives non-request messages.
	OnMessage func(ctx context.Context, message AdvancedMessage)
}

// NewMux returns an empty Mux.
func NewMux() *Mux {
	return &Mux{}
}

// Handle registers h for methodID, replacing any previous handler.
func (m *Mux) Handle(methodID uint64, h Handler) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.handlers == nil {
		m.handlers = make(map[uint64]Handler)
	}
	m.handlers[methodID] = h
}

// HandleFunc registers f for methodID.
func (m *Mux) HandleFunc(methodID uint64, f func(ctx context.Context, request AdvancedMessage) ([]byte, error)) {
	m.Handle(methodID, HandlerFunc(f))
}

// ServeHQQ dispatches request to the handler registered for its MethodID.
func (m *Mux) ServeHQQ(ctx context.Context, request AdvancedMessage) ([]byte, error) {
	m.mu.RLock()
	h := m.handlers[request.MethodID]
	m.mu.RUnlock()
	if h == nil {
		if m.NotFound != nil {
			return m.NotFound.ServeHQQ(ctx, request)
		}
		return nil, AdvancedResponseError{Status: uint64(ErrCodeInvalidOp), Payload: []byte("hqq: unknown method")}
	}
	return h.ServeHQQ(ctx, request)
}

// HandleMessage forwards non-request messages to OnMessage.
func (m *Mux) HandleMessage(ctx context.Context, message AdvancedMessage) {
	if m.OnMessage != nil {
		m.OnMessage(ctx, message)
	}
}

// Serve receives requests until ctx is done or the link fails, and answers
// each one with h on its own goroutine (at most BufferCount handlers run
// concurrently). Serve waits for running handlers before it returns.
//
// It returns ctx.Err() when ctx ends, nil when the peer closed the link
// gracefully, and the receive error otherwise. The link must be negotiated
// with FeatureRequestResponse.
func (l *AdvancedLink) Serve(ctx context.Context, h Handler) error {
	if h == nil {
		return ErrInvalidOp
	}
	if err := l.requireFeature(FeatureRequestResponse); err != nil {
		return err
	}
	ctx = orBackground(ctx)
	mh, _ := h.(MessageHandler)
	sem := make(chan struct{}, max(l.BufferCount(), 1))
	var wg sync.WaitGroup
	defer wg.Wait()

	for {
		message, err := l.Receive(ctx)
		if err != nil {
			switch {
			case ctx.Err() != nil:
				return ctx.Err()
			case errors.Is(err, ErrPeerClosed):
				return nil
			default:
				return err
			}
		}
		if !message.IsRequest() {
			if mh != nil {
				mh.HandleMessage(ctx, message)
			}
			continue
		}
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			return ctx.Err()
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			l.serveRequest(ctx, h, message)
		}()
	}
}

func (l *AdvancedLink) serveRequest(ctx context.Context, h Handler, request AdvancedMessage) {
	response, err := h.ServeHQQ(ctx, request)
	if err == nil {
		_ = l.Respond(ctx, request, 0, response)
		return
	}
	var re AdvancedResponseError
	if errors.As(err, &re) {
		_ = l.RespondError(ctx, request, re.Status, re.Payload)
		return
	}
	_ = l.RespondError(ctx, request, uint64(ErrCodeInvalidOp), []byte(err.Error()))
}
