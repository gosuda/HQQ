package hqq

import (
	"context"
	"errors"
	"runtime"
	"sync"
	"sync/atomic"
	"time"
)

// Handler serves Advanced requests, like http.Handler. ServeHQQ reads the
// request and writes the response with w; when it returns, Serve sends the
// response. A handler that writes nothing answers with an empty success
// response; WriteError turns the response into an error response.
//
// ctx is the context of this request (protocol 1.1). Its Done channel is
// closed, and Err returns context.Canceled or context.DeadlineExceeded, when
//
//   - the caller gave up (its Call ctx ended; FeatureCancel) or the response
//     is otherwise no longer wanted: Canceled;
//   - the deadline the caller sent expired (FeatureMetadata): DeadlineExceeded;
//     ctx.Deadline reports it;
//   - the stream the request arrived on was closed, by either side: Canceled;
//   - the Serve context ended, Serve returned (for example after the link
//     failed) or the link was closed: Canceled.
//
// ctx.Value delegates to the Serve context. The context is valid only during
// ServeHQQ: when the handler returns it is canceled and its memory is
// recycled for later requests, so a handler must not keep it, nor use it
// after it returned. A handler may derive contexts from it or wait on
// ctx.Done() in goroutines it waits for before returning. Calling Done
// allocates the channel (once per request); a handler that only polls
// ctx.Err() allocates nothing.
//
// A request that was canceled before its handler started is not handed to
// the handler at all, and a response is not sent when the caller gave up or
// the deadline passed while the handler ran. w and req are pooled: they are
// valid only until ServeHQQ returns, so a handler must not retain them or
// req.Payload(), nor use them from a goroutine it started.
type Handler interface {
	ServeHQQ(ctx context.Context, w *ResponseWriter, req *Request)
}

// HandlerFunc adapts a function to Handler.
type HandlerFunc func(ctx context.Context, w *ResponseWriter, req *Request)

// ServeHQQ calls f(ctx, w, req).
func (f HandlerFunc) ServeHQQ(ctx context.Context, w *ResponseWriter, req *Request) {
	f(ctx, w, req)
}

// Request is a request received by Serve. It is pooled and valid only during
// ServeHQQ. Its metadata accessors (Caller, Trace) return zero values when
// the caller sent none.
type Request struct {
	msg *Message
}

// Method returns the method selector chosen by the caller of Stream.Call.
func (r *Request) Method() uint64 {
	return r.msg.aux
}

// Payload returns the request payload. It is valid only during ServeHQQ and
// must not be modified.
func (r *Request) Payload() []byte {
	return r.msg.Bytes()
}

// StreamID returns the ID of the stream the request arrived on (0 for the
// link-level stream); the response is sent on the same stream.
func (r *Request) StreamID() uint64 {
	return r.msg.streamID
}

// ResponseWriter assembles the response to one request. It is pooled and
// valid only during ServeHQQ; the response is sent when ServeHQQ returns.
// The body is built in a pooled buffer, so writing it allocates nothing in
// the steady state.
type ResponseWriter struct {
	link   *AdvancedLink
	body   *Message // pooled response buffer, body.n bytes written; nil if empty
	status uint64   // error status, meaningful if failed
	failed bool
	skip   bool // the caller gave up: send no response
}

// Write appends p to the response body and returns len(p). A response body
// is limited to MaxMessageSize: a Write that would exceed it writes nothing
// and returns ErrBufferOverflow.
func (w *ResponseWriter) Write(p []byte) (int, error) {
	n := 0
	if w.body != nil {
		n = w.body.n
	}
	if uint64(len(p)) > w.link.maxMessageSize-min(uint64(n), w.link.maxMessageSize) {
		return 0, ErrBufferOverflow
	}
	need := n + len(p)
	switch {
	case w.body == nil:
		w.body = w.link.pool.get(need)
	case need > len(w.body.data):
		g := w.link.pool.get(need)
		copy(g.data, w.body.data[:n])
		w.body.Release()
		w.body = g
	}
	copy(w.body.data[n:need], p)
	w.body.n = need
	return len(p), nil
}

// WriteError turns the response into an error response: the caller's
// Stream.Call fails with ResponseError{Status: status, Payload: p}. It
// replaces whatever was written before; a later Write appends to p. A zero
// status is sent as ErrCodeInvalidOp. If p is larger than MaxMessageSize
// the status becomes ErrCodeBufferOverflow and the payload is empty.
func (w *ResponseWriter) WriteError(status uint64, p []byte) {
	if status == 0 {
		status = uint64(ErrCodeInvalidOp)
	}
	w.failed = true
	w.status = status
	if w.body != nil {
		w.body.n = 0
	}
	if _, err := w.Write(p); err != nil {
		w.status = uint64(ErrCodeBufferOverflow)
	}
}

var (
	requestPool = sync.Pool{New: func() any { return new(Request) }}
	writerPool  = sync.Pool{New: func() any { return new(ResponseWriter) }}
)

// Mux dispatches requests by method. The zero value is ready to use.
type Mux struct {
	mu       sync.RWMutex
	handlers map[uint64]Handler

	// NotFound handles requests for unregistered methods. If nil, they are
	// answered with an error response (status ErrCodeInvalidOp).
	NotFound Handler
}

// NewMux returns an empty Mux.
func NewMux() *Mux {
	return &Mux{}
}

// Handle registers h for method, replacing any previous handler.
func (m *Mux) Handle(method uint64, h Handler) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.handlers == nil {
		m.handlers = make(map[uint64]Handler)
	}
	m.handlers[method] = h
}

// HandleFunc registers f for method.
func (m *Mux) HandleFunc(method uint64, f func(ctx context.Context, w *ResponseWriter, req *Request)) {
	m.Handle(method, HandlerFunc(f))
}

var errUnknownMethod = []byte("hqq: unknown method")

// ServeHQQ dispatches req to the handler registered for its method.
func (m *Mux) ServeHQQ(ctx context.Context, w *ResponseWriter, req *Request) {
	m.mu.RLock()
	h := m.handlers[req.Method()]
	m.mu.RUnlock()
	if h == nil {
		if m.NotFound != nil {
			m.NotFound.ServeHQQ(ctx, w, req)
			return
		}
		w.WriteError(uint64(ErrCodeInvalidOp), errUnknownMethod)
		return
	}
	h.ServeHQQ(ctx, w, req)
}

// Serve receives requests, on every stream of the link, until ctx is done or
// the link fails, and answers each one with h. Handlers may run concurrently
// (at most BufferCount at a time) and Serve waits for running handlers
// before it returns.
//
// Execution model. Exactly one goroutine at a time, the receiver, takes
// requests from the request queue. While no handler is running, the
// receiver runs each request's handler itself, inline, which saves a
// goroutine hand-off per request (the low-latency path for short handlers).
// If an inline handler is still running after inlineLimit, the receiver role
// passes to a new goroutine so that later requests are not stuck behind the
// slow handler (a handler may even wait for a later request); the slow
// handler finishes on its own goroutine, which then ends. While any handler
// is running, the receiver hands requests to pooled worker goroutines
// (started on demand, at most BufferCount, kept until Serve returns); when
// all BufferCount handlers are busy it stops receiving until one finishes.
// The switch is per episode, not permanent: as soon as the receiver finds
// no handler running it runs the next request inline again, so a one-off
// stall costs one episode and steady state allocates and spawns nothing.
//
// After ServeHQQ returns, Serve sends the response on the stream the
// request arrived on and recycles the request and the response writer. Serve
// is the only consumer of requests: Stream.Recv never returns one. A request
// whose sender aborted it mid-transfer is dropped and never reaches h.
//
// Every handler receives its own context (see Handler): it ends when the
// caller cancels or its deadline expires, when the stream closes, and when
// Serve ends, which cancels the contexts of the handlers still running
// before it waits for them.
//
// It returns ctx.Err() when ctx ends, nil when the peer closed the link
// gracefully or Shutdown completed (also if it completed before Serve was
// called), and the receive error otherwise. The link must be negotiated
// with FeatureRequestResponse.
func (l *AdvancedLink) Serve(ctx context.Context, h Handler) error {
	if h == nil {
		return ErrInvalidOp
	}
	if err := l.requireFeature(FeatureRequestResponse); err != nil {
		return err
	}
	if l.serveStopped.Load() {
		return nil
	}
	ctx = orBackground(ctx)
	n := max(l.BufferCount(), 1)
	s := &server{
		l:      l,
		h:      h,
		ctx:    ctx,
		max:    n,
		slots:  make(chan struct{}, n),
		work:   make(chan *Message),
		result: make(chan error, 1),
	}
	s.timer = time.AfterFunc(time.Hour, s.escalate)
	s.timer.Stop()

	err, final := s.run()
	if final {
		s.stop()
		close(s.work)
		s.wg.Wait()
		return err
	}
	// The receiver role moved to another goroutine after an escalation;
	// this one has finished its slow handler and only waits.
	err = <-s.result
	s.wg.Wait()
	return err
}

// inlineLimit is how long a handler may run on the receiving goroutine
// before the receiver role passes to another goroutine (an escalation).
const inlineLimit = 200 * time.Microsecond

// Phases in the state word of the inline handler of the receiver, which holds
// epoch<<2 | phase.
const (
	phaseHandling  uint64 = 1 // the receiver runs a handler inline
	phaseEscalated uint64 = 2 // inlineLimit passed: the receiver role moved on
	phaseDone      uint64 = 3 // the inline handler returned in time
	phaseMask      uint64 = 3
)

// server is the state of one Serve call.
type server struct {
	l      *AdvancedLink
	h      Handler
	ctx    context.Context
	max    int
	slots  chan struct{} // one token per handler in flight that the receiver does not run inline
	work   chan *Message // unbuffered: a send succeeds only if a worker is idle
	result chan error    // Serve's result, sent by the final receiver if it is not Serve's goroutine
	// stopped is set when receiving ended, before the handlers still running
	// are canceled: a handler that starts later is not run (see
	// reqCtx.begin).
	stopped atomic.Bool

	// Inline execution: timer fires escalate if a handler outlives
	// inlineLimit; state holds the epoch and phase of the current inline run
	// (epochs tell a stale outcome from the current one); fires counts the
	// executions of escalate.
	timer *time.Timer
	state atomic.Uint64
	fires atomic.Uint64

	// Touched only by the current receiver (the role is handed over by a go
	// statement after the receiver's atomic state update, which orders them).
	epoch   uint64
	started int // worker goroutines started

	wg sync.WaitGroup // every goroutine except Serve's own
}

// stop ends the handlers of this Serve call: the ones running are canceled
// and the ones handed to a worker but not yet started will not run.
func (s *server) stop() {
	s.stopped.Store(true)
	s.l.cancelServerRequests(s)
}

// mapError converts the error that ended the receive loop into Serve's
// result.
func (s *server) mapError(err error) error {
	switch {
	case s.ctx.Err() != nil:
		return s.ctx.Err()
	case s.l.serveStopped.Load():
		return nil
	case errors.Is(err, ErrPeerClosed):
		return nil
	default:
		return err
	}
}

// run is the receiver loop. It returns (result, true) when receiving ended,
// and (nil, false) when the receiver role moved to another goroutine because
// the handler this goroutine ran inline outlived inlineLimit.
func (s *server) run() (error, bool) {
	for {
		m, err := s.l.receiveQueued(s.ctx, s.l.requests, nil)
		if err != nil {
			return s.mapError(err), true
		}
		if len(s.slots) != 0 {
			// Handlers are running: do not wait for them, hand over.
			if !s.dispatch(m) {
				return s.ctx.Err(), true
			}
			continue
		}
		// Nothing is running (only the receiver takes slots, so the
		// check cannot become stale): run the handler inline, watched by
		// the escalation timer.
		s.epoch++
		h := s.epoch<<2 | phaseHandling
		armed := s.fires.Load()
		s.state.Store(h)
		s.timer.Reset(inlineLimit)
		req, w := s.handle(m)
		if !s.timer.Stop() {
			// The timer fired: wait until its callback is done, so that
			// it cannot act on the next handler.
			for s.fires.Load() == armed {
				runtime.Gosched()
			}
		}
		if !s.state.CompareAndSwap(h, h&^phaseMask|phaseDone) {
			// Escalated while the handler ran: another goroutine receives
			// the requests now. Release the slot escalate took for this
			// handler (before responding, see worker), respond and end.
			<-s.slots
			s.respond(m, req, w)
			return nil, false
		}
		s.respond(m, req, w)
	}
}

// escalate is the timer callback: if the inline handler is still running, it
// is accounted as a running handler and a new goroutine takes over receiving.
func (s *server) escalate() {
	if st := s.state.Load(); st&phaseMask == phaseHandling {
		select {
		case s.slots <- struct{}{}:
			if s.state.CompareAndSwap(st, st&^phaseMask|phaseEscalated) {
				s.l.stats.serveEscalations.Add(1)
				s.wg.Add(1)
				s.fires.Add(1) // before the new receiver exists
				go s.receiver()
				return
			}
			<-s.slots
		default:
		}
	}
	s.fires.Add(1)
}

// receiver is a receiver goroutine started by escalate. If it is the one
// that ends receiving, it hands the result to Serve and stops the workers.
func (s *server) receiver() {
	defer s.wg.Done()
	if err, final := s.run(); final {
		s.stop()
		s.result <- err
		close(s.work)
	}
}

// dispatch runs m on a worker, taking a slot first: it waits while
// BufferCount handlers are in flight, hands m to an idle worker, and starts
// a worker if none is idle. It returns false if ctx ended first.
func (s *server) dispatch(m *Message) bool {
	select {
	case s.slots <- struct{}{}:
	case <-s.ctx.Done():
		m.Release()
		return false
	}
	s.l.stats.servePooled.Add(1)
	select {
	case s.work <- m:
		return true
	default:
	}
	if s.started < s.max {
		s.started++
		s.wg.Add(1)
		go s.worker(m)
		return true
	}
	// A slot was free, so a worker is idle or about to be.
	select {
	case s.work <- m:
		return true
	case <-s.ctx.Done():
		<-s.slots
		m.Release()
		return false
	}
}

// worker runs handlers handed over by dispatch. A handler's slot is released
// as soon as the handler returns, before its response is sent: the caller
// can only send its next request after it received the response, so that
// request finds nothing in flight and runs inline again. Releasing after
// sending would let a sequential caller keep Serve in the pooled mode for
// good.
func (s *server) worker(m *Message) {
	defer s.wg.Done()
	s.runPooled(m)
	for m := range s.work {
		s.runPooled(m)
	}
}

// runPooled runs one handed-over request on a worker.
func (s *server) runPooled(m *Message) {
	req, w := s.handle(m)
	<-s.slots
	s.respond(m, req, w)
}

// handle runs the handler for request message m.
//
// A request that was canceled before it started is not handed to the
// handler: handle returns (nil, nil) and respond only releases m.
func (s *server) handle(m *Message) (*Request, *ResponseWriter) {
	rc := m.rc
	if !rc.begin(s, m) {
		rc.end()
		return nil, nil
	}
	req := requestPool.Get().(*Request)
	req.msg = m
	w := writerPool.Get().(*ResponseWriter)
	w.link = s.l
	s.h.ServeHQQ(rc, w, req)
	w.skip = rc.end()
	return req, w
}

// respond sends the response assembled in w and recycles everything.
func (s *server) respond(m *Message, req *Request, w *ResponseWriter) {
	if req == nil {
		m.Release()
		return
	}
	if !w.skip {
		s.l.sendResponse(s.ctx, w, m)
	}
	w.body.Release()
	*w = ResponseWriter{}
	writerPool.Put(w)
	req.msg = nil
	requestPool.Put(req)
	m.Release()
}

// sendResponse publishes the response assembled in w for the request header
// req on the stream the request arrived on.
func (l *AdvancedLink) sendResponse(ctx context.Context, w *ResponseWriter, req *Message) {
	flags := AdvFlagResponse
	var status uint64
	if w.failed {
		flags |= AdvFlagError
		status = w.status
	}
	var payload []byte
	if w.body != nil {
		payload = w.body.Bytes()
	}
	if err := l.sendAdvancedPayload(ctx, req.streamID, req.messageID, flags, status, nil, payload); err == nil {
		l.stats.responses.Add(1)
	}
}
