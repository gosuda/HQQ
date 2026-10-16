package hqq

import (
	"context"
	"encoding/binary"
	"errors"
	"math"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"gosuda.org/hqq/internal/protocol"
)

// Protocol 1.1 request metadata (FeatureMetadata, AdvFlagMeta).
//
// A request whose frames carry AdvFlagMeta starts its payload with a metadata
// block; TotalSize, ChunkSize and the chunk offsets count it like any other
// payload byte. The block is
//
//	offset 0  uint16  tlvLen  number of TLV bytes that follow the 4-byte header
//	offset 2  uint16  reserved, written as zero, ignored by receivers
//	offset 4  TLV...  tag (uint8), length (uint8), value (length bytes)
//
// Tags: 1 = remaining time budget in nanoseconds (uint64) relative to the
// moment the block was sent, 2 = caller ID (uint64, application-defined),
// 3 = trace ID (16 bytes). Unknown tags are skipped; a known tag with the
// wrong length, a TLV that overruns the block or a block that overruns the
// message is ErrProtocol.
const (
	metaHeaderSize = 4

	metaTagBudget = 1
	metaTagCaller = 2
	metaTagTrace  = 3

	// metaMaxBlock is the largest block this implementation sends: the
	// header and one TLV of each known tag.
	metaMaxBlock = metaHeaderSize + (2 + 8) + (2 + 8) + (2 + 16)
)

// CallOptions carries optional request metadata for Stream.CallWith and
// AdvancedLink.CallWith. The zero value sends none; it is passed by value, so
// attaching metadata does not allocate.
type CallOptions struct {
	// Caller is an application-defined caller ID, delivered to the handler
	// as Request.Caller. Zero means "none" and is not sent.
	Caller uint64

	// Trace is an application-defined 16-byte trace ID, delivered to the
	// handler as Request.Trace. The all-zero value means "none" and is not
	// sent.
	Trace [16]byte
}

// msgMeta is the parsed metadata block of a received request.
type msgMeta struct {
	present   bool      // the message carried a metadata block
	hasBudget bool      // tag 1 was present
	budget    int64     // nanoseconds from recvAt, only if hasBudget
	recvAt    time.Time // arrival of the first chunk
	caller    uint64
	trace     [16]byte
}

// putMeta encodes the block for budget (nanoseconds, <= 0: none) and opts into
// dst, which must be zeroed, and returns it; nil if there is nothing to send.
func putMeta(dst *[metaMaxBlock]byte, budget int64, opts CallOptions) []byte {
	n := metaHeaderSize
	if budget > 0 {
		dst[n], dst[n+1] = metaTagBudget, 8
		binary.LittleEndian.PutUint64(dst[n+2:], uint64(budget))
		n += 10
	}
	if opts.Caller != 0 {
		dst[n], dst[n+1] = metaTagCaller, 8
		binary.LittleEndian.PutUint64(dst[n+2:], opts.Caller)
		n += 10
	}
	if opts.Trace != ([16]byte{}) {
		dst[n], dst[n+1] = metaTagTrace, 16
		copy(dst[n+2:], opts.Trace[:])
		n += 18
	}
	if n == metaHeaderSize {
		return nil
	}
	binary.LittleEndian.PutUint16(dst[0:2], uint16(n-metaHeaderSize))
	return dst[:n]
}

// parseMeta decodes the metadata block at the start of data and returns the
// size of the block.
func parseMeta(data []byte) (msgMeta, int, error) {
	var meta msgMeta
	if len(data) < metaHeaderSize {
		return meta, 0, ErrProtocol
	}
	end := metaHeaderSize + int(binary.LittleEndian.Uint16(data[0:2]))
	if end > len(data) {
		return meta, 0, ErrProtocol
	}
	meta.present = true
	tlv := data[metaHeaderSize:end]
	for len(tlv) > 0 {
		if len(tlv) < 2 || len(tlv) < 2+int(tlv[1]) {
			return meta, 0, ErrProtocol
		}
		tag, value := tlv[0], tlv[2:2+int(tlv[1])]
		tlv = tlv[2+len(value):]
		switch tag {
		case metaTagBudget:
			if len(value) != 8 {
				return meta, 0, ErrProtocol
			}
			meta.hasBudget = true
			meta.budget = int64(min(binary.LittleEndian.Uint64(value), math.MaxInt64))
		case metaTagCaller:
			if len(value) != 8 {
				return meta, 0, ErrProtocol
			}
			meta.caller = binary.LittleEndian.Uint64(value)
		case metaTagTrace:
			if len(value) != 16 {
				return meta, 0, ErrProtocol
			}
			copy(meta.trace[:], value)
		}
	}
	return meta, end, nil
}

// stripMeta parses the metadata block of a completely assembled request and
// hides it from Bytes. recvAt is the arrival time of its first chunk.
func (m *Message) stripMeta(recvAt time.Time) error {
	meta, n, err := parseMeta(m.data[:m.n])
	if err != nil {
		return err
	}
	meta.recvAt = recvAt
	m.meta = meta
	m.off = n
	m.n -= n
	return nil
}

// Caller returns the caller ID the peer attached with CallOptions.Caller, or
// zero.
func (r *Request) Caller() uint64 {
	return r.msg.meta.caller
}

// Trace returns the trace ID the peer attached with CallOptions.Trace, or the
// zero value.
func (r *Request) Trace() [16]byte {
	return r.msg.meta.trace
}

// errServeStopped ends the receive loop of Serve after Shutdown completed.
var errServeStopped = errors.New("hqq: serve stopped by shutdown")

// Why a request context ended (reqCtx.errv).
const (
	ctxLive       uint32 = iota
	ctxCanceled          // stream closed, Serve ended, link closed, or the handler returned
	ctxDeadline          // the deadline the caller sent expired
	ctxPeerCancel        // the caller sent OpCancel
)

// closedDone is the Done channel of every request context that was already
// finished when Done was called.
var closedDone = func() chan struct{} {
	c := make(chan struct{})
	close(c)
	return c
}()

var reqCtxPool = sync.Pool{New: func() any { return new(reqCtx) }}

// reqCtx is the per-request context.Context handed to Handler.ServeHQQ. One
// is attached to every received request when the request is routed (so that
// OpCancel, stream close and shutdown find it even while the request still
// waits in the request queue) and recycled when the request message is
// released.
//
// Nothing here allocates in the steady state: reqCtx values and their
// deadline timer are pooled, and the Done channel is created lazily, only
// if a handler calls Done. A context whose Done channel was handed out is
// never recycled (the channel and the context may then be retained by
// goroutines the handler started); such a request allocates the channel and
// leaves the context to the garbage collector.
//
// The context is valid only during ServeHQQ. When the handler returns it is
// canceled (Err returns context.Canceled unless it ended earlier), so a
// retained context observes cancellation, but a retained context whose Done
// was never called may be reused for another request: handlers must not keep
// it.
type reqCtx struct {
	link *AdvancedLink
	key  advancedCallKey

	// Written by begin before the handler runs, read by the handler.
	parent      context.Context // the Serve context
	deadline    time.Time
	hasDeadline bool
	owner       atomic.Pointer[server] // the Serve call running the handler

	errv atomic.Uint32 // ctxLive or why the context ended; the first reason wins

	mu      sync.Mutex
	done    chan struct{} // created lazily by Done, under mu
	closed  bool          // done was closed, under mu
	hasDone atomic.Bool   // done was created: lets cancel and release skip mu on the common path

	timer      *time.Timer // created on first use, reused
	timerArmed bool
	armedFires uint64
	fires      atomic.Uint64 // completed executions of onTimer
}

var _ context.Context = (*reqCtx)(nil)

// Deadline returns the earlier of the caller's deadline (the time budget it
// sent, counted from the arrival of the request) and the Serve context's.
func (c *reqCtx) Deadline() (time.Time, bool) {
	d, ok := c.deadline, c.hasDeadline
	if pd, pok := c.parent.Deadline(); pok && (!ok || pd.Before(d)) {
		return pd, true
	}
	return d, ok
}

// Done returns a channel that is closed when the request is canceled.
func (c *reqCtx) Done() <-chan struct{} {
	if c.errv.Load() != ctxLive {
		return closedDone
	}
	c.mu.Lock()
	if c.done == nil {
		c.done = make(chan struct{})
		c.hasDone.Store(true)
	}
	d := c.done
	if c.errv.Load() != ctxLive && !c.closed {
		c.closed = true
		close(d)
	}
	c.mu.Unlock()
	return d
}

// Err returns context.Canceled or context.DeadlineExceeded once the request
// ended, nil before.
func (c *reqCtx) Err() error {
	switch c.errv.Load() {
	case ctxLive:
		if c.parent != nil {
			return c.parent.Err()
		}
		return nil
	case ctxDeadline:
		return context.DeadlineExceeded
	default:
		return context.Canceled
	}
}

// Value delegates to the Serve context.
func (c *reqCtx) Value(key any) any {
	if c.parent == nil {
		return nil
	}
	return c.parent.Value(key)
}

// cancel ends the context with reason code; the first reason wins.
func (c *reqCtx) cancel(code uint32) bool {
	if !c.errv.CompareAndSwap(ctxLive, code) {
		return false
	}
	// Done stores hasDone before it re-reads errv and cancel stores errv
	// before it reads hasDone, so one of them closes a channel that Done
	// created concurrently.
	if c.hasDone.Load() {
		c.mu.Lock()
		if c.done != nil && !c.closed {
			c.closed = true
			close(c.done)
		}
		c.mu.Unlock()
	}
	return true
}

// onTimer is the deadline timer callback.
func (c *reqCtx) onTimer() {
	c.cancel(ctxDeadline)
	c.fires.Add(1)
}

// begin prepares the context for the handler of m run by s. It returns false
// if the request must not be handled: it was already canceled (by the peer, a
// closed stream, or an expired deadline) or the Serve context ended.
func (c *reqCtx) begin(s *server, m *Message) bool {
	c.parent = s.ctx
	c.owner.Store(s)
	if c.errv.Load() != ctxLive || s.stopped.Load() || s.ctx.Err() != nil {
		return false
	}
	if m.meta.hasBudget {
		remaining := time.Duration(m.meta.budget) - time.Since(m.meta.recvAt)
		if remaining <= 0 {
			c.cancel(ctxDeadline)
			return false
		}
		c.deadline, c.hasDeadline = time.Now().Add(remaining), true
		if c.timer == nil {
			c.timer = time.AfterFunc(time.Hour, c.onTimer)
			c.timer.Stop()
		}
		c.armedFires = c.fires.Load()
		c.timerArmed = true
		c.timer.Reset(remaining)
	}
	return true
}

// end finishes the handler: it stops the timer, cancels the context and
// reports whether the response is pointless because the caller gave up (it
// sent OpCancel or its deadline expired).
func (c *reqCtx) end() (abandoned bool) {
	if c.timerArmed {
		if !c.timer.Stop() {
			// The callback started: wait until it is done, so that it cannot
			// act on the next request this context serves.
			for c.fires.Load() == c.armedFires {
				runtime.Gosched()
			}
		}
		c.timerArmed = false
	}
	code := c.errv.Load()
	abandoned = code == ctxDeadline || code == ctxPeerCancel
	c.cancel(ctxCanceled)
	c.owner.Store(nil)
	return abandoned
}

// release unregisters the request, accounts its end and recycles the
// context. It runs when the request message is released.
func (c *reqCtx) release() {
	l := c.link
	l.inflightMu.Lock()
	if l.inflight[c.key] == c {
		delete(l.inflight, c.key)
	}
	l.inflightMu.Unlock()

	if !c.hasDone.Load() {
		c.link = nil
		c.parent = nil
		c.deadline, c.hasDeadline = time.Time{}, false
		c.errv.Store(ctxLive)
		reqCtxPool.Put(c)
	}
	l.requestDone()
}

// newRequestCtx attaches a context to the request message m, registers it
// and counts the request as in flight.
func (l *AdvancedLink) newRequestCtx(m *Message) {
	c := reqCtxPool.Get().(*reqCtx)
	c.link = l
	c.key = advancedCallKey{streamID: m.streamID, messageID: m.messageID}
	m.rc = c
	l.inflightN.Add(1)
	l.inflightMu.Lock()
	l.inflight[c.key] = c
	l.inflightMu.Unlock()
}

// requestDone accounts the end of a request and wakes Shutdown when the
// last one ended.
func (l *AdvancedLink) requestDone() {
	if l.inflightN.Add(-1) == 0 && l.shuttingDown.Load() {
		select {
		case l.drained <- struct{}{}:
		default:
		}
	}
}

// routeRequest hands a received request to the request queue consumed by
// Serve, or refuses it if Shutdown was called.
func (l *AdvancedLink) routeRequest(ctx context.Context, m *Message, blocking bool) (stalledDelivery, error) {
	l.newRequestCtx(m)
	if l.shuttingDown.Load() {
		l.stats.refusedRequests.Add(1)
		l.refuseRequest(m.advHeader, ErrCodeShuttingDown)
		m.Release()
		return stalledDelivery{}, nil
	}
	return l.deliver(ctx, l.requests, nil, m, blocking)
}

// cancelRequests cancels the in-flight requests selected by match.
func (l *AdvancedLink) cancelRequests(code uint32, match func(*reqCtx) bool) {
	if l.inflightN.Load() == 0 {
		return
	}
	l.inflightMu.Lock()
	for _, c := range l.inflight {
		if match(c) {
			c.cancel(code)
		}
	}
	l.inflightMu.Unlock()
}

// cancelStreamRequests cancels the requests of stream id (the stream was
// closed).
func (l *AdvancedLink) cancelStreamRequests(id uint64) {
	l.cancelRequests(ctxCanceled, func(c *reqCtx) bool { return c.key.streamID == id })
}

// cancelAllRequests cancels every in-flight request (the link is closing).
func (l *AdvancedLink) cancelAllRequests() {
	l.cancelRequests(ctxCanceled, func(*reqCtx) bool { return true })
}

// cancelServerRequests cancels the requests whose handler s is running (Serve
// ended).
func (l *AdvancedLink) cancelServerRequests(s *server) {
	l.cancelRequests(ctxCanceled, func(c *reqCtx) bool { return c.owner.Load() == s })
}

// handleCancelPacket processes the peer's OpCancel: the context of the
// request it names is canceled. A request that is unknown (never received,
// or already answered) is ignored.
func (l *AdvancedLink) handleCancelPacket(p *protocol.Packet) {
	key := advancedCallKey{streamID: p.Operand(0), messageID: p.Operand(1)}
	l.inflightMu.Lock()
	c := l.inflight[key]
	canceled := c != nil && c.cancel(ctxPeerCancel)
	l.inflightMu.Unlock()
	if canceled {
		l.stats.canceledRequests.Add(1)
	}
}

// cancelPending tells the peer that the call key stopped waiting for its
// response (FeatureCancel). It is best effort and never blocks: the packet
// goes through the control sender, and is counted in DroppedMessages if its
// queue is full.
func (l *AdvancedLink) cancelPending(key advancedCallKey) {
	if l.negotiated.Load()&FeatureCancel == 0 {
		return
	}
	job := controlJob{packet: protocol.NewPacket(protocol.OpCancel, key.streamID, key.messageID), kind: controlReply}
	if !l.enqueueControl(job) {
		l.stats.droppedMessages.Add(1)
	}
}

// Shutdown gracefully stops serving: it stops accepting new requests (a
// request that arrives from now on is answered with an error response whose
// status is ErrCodeShuttingDown; the peer's Call returns a ResponseError
// matching ErrShuttingDown), waits until every request already accepted has
// been handled and answered, or ctx ends, and then makes Serve return nil.
//
// Shutdown returns nil once nothing is in flight, ctx.Err() if ctx ended
// first, and ErrClosed if the link was closed meanwhile. A Serve call must
// be running for requests that are still queued to drain; without one,
// Shutdown waits for ctx. Shutdown is not undone by an error: new requests
// stay refused, so a caller whose ctx expired typically calls Close next,
// which cancels the contexts of the handlers still running. After a
// successful Shutdown, Serve (also a later call) returns nil immediately;
// calls the link makes (Call, Send, ...) keep working. Shutdown is
// idempotent and safe for concurrent use. Typical order:
//
//	err := link.Shutdown(ctx)
//	link.Close()
//
// Stream requests held for ListenStreams are refused at once with
// ErrConnNotFound (see AdvancedOptions.StreamListenGrace).
//
// The link must be negotiated.
func (l *AdvancedLink) Shutdown(ctx context.Context) error {
	if err := l.requireNegotiated(); err != nil {
		return err
	}
	ctx = orBackground(ctx)
	l.shuttingDown.Store(true)
	l.releaseHeld(true)
	for l.inflightN.Load() > 0 {
		select {
		case <-l.drained:
		case <-ctx.Done():
			return ctx.Err()
		case <-l.lifeCtx.Done():
			return ErrClosed
		}
	}
	l.stopServe()
	return nil
}

// stopServe makes Serve return nil: it wakes a Serve that waits for a request
// on the dispatcher's queue or on the ring.
func (l *AdvancedLink) stopServe() {
	l.serveStopped.Store(true)
	l.serveStopOnce.Do(func() { close(l.serveStop) })
	if s := l.slink; s.enterRx() == nil {
		s.rxRing.WakeAll()
		s.exitRx()
	}
}

// serveWakeDeadline is the ring DeadlineFunc of a receiver that serves
// requests: once Shutdown completed it reports a passed deadline, which ends
// the wait of a Serve parked on the ring. It is zero (no deadline) before.
func (l *AdvancedLink) serveWakeDeadline() time.Time {
	if l.serveStopped.Load() {
		return time.Unix(0, 1)
	}
	return time.Time{}
}
