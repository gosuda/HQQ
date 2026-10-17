package rt

import (
	"errors"
	"fmt"
	"sync"
	"time"

	hqq "gosuda.org/hqq"
)

const (
	// keepBuf is the largest response buffer a recycled Call keeps.
	keepBuf = 1 << 20
	// maxFreeCalls bounds the recycled Call objects of a Conn.
	maxFreeCalls = 256
)

// rxMode says what readResponse does with the body of a completed response.
type rxMode uint8

const (
	rxBuffer  rxMode = iota // copy into the call's buffer
	rxHold                  // keep the single-frame slot reserved and decode in place
	rxDiscard               // drop the body
)

// assembly is the multi-frame response being received (4.8). It lives in the
// Conn so that a Wait that times out between two frames can resume.
type assembly struct {
	active bool
	id     uint32
	total  uint32
	got    int
}

type rfEntry struct {
	bound int64
	rf    int
}

// Conn is one session. It is NOT safe for concurrent use (8.2).
type Conn struct {
	link    *hqq.StandardLink
	closeFn func() error
	iface   *Iface
	sess    uint64
	lim     Limits
	chunk   int

	callTimeout  time.Duration
	closeTimeout time.Duration

	// One goroutine at a time owns everything below until Kill, which only
	// touches closeOnce.
	txOpen bool
	rxOpen bool
	enc    Enc

	nextID uint32

	rxHeld  bool
	rxRes   hqq.StandardReadReservation
	rxGen   uint32
	asm     assembly
	rdArmed bool

	unans   []*Call // ring of the unanswered calls, oldest first
	unansHd int
	unansN  int
	unansRF int // R of 4.12: the sum of rf over the unanswered calls

	live []*Call // every call that was submitted and not yet waited for
	free []*Call
	rfs  []rfEntry // indexed by op

	scratch []byte

	broken error // first failure of the session
	rxDead bool  // the receive side can no longer be trusted
	closed bool

	closeRes error // result of Close; owner only

	closeOnce sync.Once
	linkErr   error // result of closeFn; written inside closeOnce
}

// Enc is a request being built. At most one exists per Conn.
type Enc struct {
	c       *Conn
	op      uint16
	bodyLen int
	respMax int64
	rf      int
	multi   bool
	res     hqq.StandardWriteReservation
	buf     []byte
	body    []byte
}

// Call is a committed request. Wait, Sync and Close recycle it.
type Call struct {
	c       *Conn
	id      uint32
	op      uint16
	rf      int
	respMax int64
	pending bool
	done    bool
	inPlace bool
	flags   uint8
	status  uint32
	msg     string
	body    []byte
	buf     []byte
	idx     int
}

// ID is the request id of the call (4.12). A future stores it and passes it
// back to Wait.
func (c *Call) ID() uint32 { return c.id }

// Dec is a response body in place (or in a per-call buffer).
type Dec struct {
	Body   []byte // response body; valid until Release
	Detail bool   // F_DETAIL
	Fail   bool   // F_FAIL

	c   *Conn
	gen uint32
	op  uint16
}

func newConn(link *hqq.StandardLink, closeFn func() error, iface *Iface, cfg config, lim Limits) *Conn {
	c := &Conn{
		link:         link,
		closeFn:      closeFn,
		iface:        iface,
		sess:         sessionSeq.Add(1),
		lim:          lim,
		chunk:        lim.BufferSize - HeaderSize,
		callTimeout:  cfg.callTimeout,
		closeTimeout: cfg.closeTimeout,
		unans:        make([]*Call, lim.MaxInFlight),
		rfs:          make([]rfEntry, len(iface.Ops)+1),
	}
	c.enc.c = c
	return c
}

// Err is the sticky failure of a broken session, or nil.
func (c *Conn) Err() error { return c.broken }

// Limits returns the values granted by the server.
func (c *Conn) Limits() Limits { return c.lim }

func (c *Conn) opName(op uint16) string {
	if op >= 1 && int(op) <= len(c.iface.Ops) {
		return c.iface.Ops[op-1]
	}
	return ""
}

// Body is the request body, exactly bodyLen bytes: slot memory or scratch. Its
// previous content is undefined, so the stub MUST write every byte including
// padding. Valid until Submit or Abort.
func (e *Enc) Body() []byte { return e.body }

// BeginRequest applies flow control (4.12), then reserves the request slot (or
// scratch for a body longer than S - HeaderSize). bodyLen is the exact length
// of the request body; respMax is the RESP_BOUND of 4.6.3. respMax is 64-bit
// so that the stub's bound arithmetic cannot wrap on 32-bit targets. It fails
// with ErrInvalidArg if bodyLen or respMax exceeds MaxMessage.
func (c *Conn) BeginRequest(op uint16, bodyLen int, respMax int64) (*Enc, error) {
	if c.txOpen || c.rxOpen {
		return nil, ErrConcurrentUse
	}
	if !c.validRequest(op, bodyLen, respMax) {
		return nil, ErrInvalidArg
	}
	if c.closed {
		return nil, &TransportError{Func: c.opName(op), Err: hqq.ErrClosed, svc: c.iface.Service}
	}
	if c.broken != nil {
		return nil, c.brokenErr(op)
	}
	rf := c.rfOf(op, respMax)
	if !c.fits(rf) {
		if err := c.makeRoom(op, rf); err != nil {
			return nil, err
		}
	}
	e := &c.enc
	if HeaderSize+bodyLen <= c.lim.BufferSize {
		res, err := c.link.ReserveWrite()
		if err != nil {
			c.failTx(err)
			return nil, &TransportError{Func: c.opName(op), Err: err, svc: c.iface.Service}
		}
		e.res = res
		e.buf = e.res.Buffer()
		e.body = e.buf[HeaderSize : HeaderSize+bodyLen : HeaderSize+bodyLen]
		e.multi = false
	} else {
		e.body = c.scratchFor(bodyLen)
		e.multi = true
	}
	e.op, e.bodyLen, e.respMax, e.rf = op, bodyLen, respMax, rf
	c.txOpen = true
	return e, nil
}

func (c *Conn) validRequest(op uint16, bodyLen int, respMax int64) bool {
	maxMsg := int64(c.lim.MaxMessage)
	opOK := op != 0 && int(op) <= len(c.iface.Ops)
	return opOK && bodyLen >= 0 && int64(bodyLen) <= maxMsg && respMax >= 0 && respMax <= maxMsg
}

// rfOf is rf(q) of 4.12: the most response frames a call can produce. The
// quotient is cached per op for the last bound used, which for a function
// with a fixed bound is every call and for a caller-chosen cap is every call
// that repeats the cap.
func (c *Conn) rfOf(op uint16, respMax int64) int {
	e := &c.rfs[op]
	if e.rf != 0 && e.bound == respMax {
		return e.rf
	}
	rf := max(1, int((respMax+int64(c.chunk)-1)/int64(c.chunk)))
	*e = rfEntry{bound: respMax, rf: rf}
	return rf
}

// fits reports whether a call with rf response frames may be committed (4.12).
func (c *Conn) fits(rf int) bool {
	if c.unansN >= c.lim.MaxInFlight {
		return false
	}
	if rf <= c.lim.BufferCount {
		return c.unansRF+rf <= c.lim.BufferCount
	}
	return c.unansN == 0
}

// makeRoom reads and buffers responses until a call with rf response frames
// fits. The new call is not committed, so every failure here is a known
// outcome for it.
func (c *Conn) makeRoom(op uint16, rf int) error {
	armed := false
	for !c.fits(rf) {
		if c.rxDead {
			return c.brokenErr(op)
		}
		if !armed {
			if err := c.armRead(); err != nil {
				return c.readFailed(op, err)
			}
			armed = true
		}
		if err := c.readResponse(rxBuffer); err != nil {
			return c.readFailed(op, err)
		}
	}
	return nil
}

// readFailed turns the error of readResponse into the error of a call that
// was not committed: a timeout leaves the session usable, anything else broke
// it.
func (c *Conn) readFailed(op uint16, err error) error {
	if c.rxDead {
		return c.brokenErr(op)
	}
	return &TransportError{Func: c.opName(op), Err: err, svc: c.iface.Service}
}

func (c *Conn) scratchFor(n int) []byte {
	if cap(c.scratch) < n {
		c.scratch = make([]byte, n)
	}
	return c.scratch[:n]
}

// Submit writes the header, assigns the request id, commits the frame (frames
// for an oversize body) and registers the call as pending.
func (c *Conn) Submit(e *Enc) (*Call, error) {
	if e != &c.enc || !c.txOpen {
		return nil, ErrInvalidArg
	}
	id := nextID(c.nextID)
	h := header{kind: kindRequest, op: e.op, id: id, total: uint32(e.bodyLen)}
	var (
		err       error
		committed bool
	)
	if !e.multi {
		putHeader(e.buf, h)
		err = e.res.Commit(HeaderSize + e.bodyLen)
	} else {
		committed, err = c.sendFrames(e, h)
	}
	op := e.op
	c.txOpen = false
	e.body, e.buf = nil, nil
	if err != nil {
		c.failTx(err)
		c.afterOp()
		return nil, &TransportError{Func: c.opName(op), Err: err, Unknown: committed, svc: c.iface.Service}
	}
	c.nextID = id
	call := c.newCall()
	call.id, call.op, call.rf, call.respMax = id, op, e.rf, e.respMax
	call.pending = true
	c.unans[(c.unansHd+c.unansN)%len(c.unans)] = call
	c.unansN++
	c.unansRF += e.rf
	call.idx = len(c.live)
	c.live = append(c.live, call)
	if cap(c.scratch) > keepBuf {
		c.scratch = nil
	}
	c.afterOp()
	return call, nil
}

// sendFrames publishes an oversize request as the frames of 4.8. committed
// reports whether at least one frame reached the ring.
func (c *Conn) sendFrames(e *Enc, h header) (committed bool, err error) {
	body := e.body
	for off := 0; ; {
		n := min(len(body)-off, c.chunk)
		res, err := c.link.ReserveWrite()
		if err != nil {
			return committed, err
		}
		buf := res.Buffer()
		fh := h
		if off > 0 {
			fh = header{kind: kindCont, id: h.id, total: h.total, aux: uint32(off)}
		}
		putHeader(buf, fh)
		copy(buf[HeaderSize:], body[off:off+n])
		if err := res.Commit(HeaderSize + n); err != nil {
			return committed, err
		}
		committed = true
		off += n
		if off >= len(body) {
			return true, nil
		}
	}
}

// Abort publishes a tombstone for a reserved, unsubmitted request (consumes no
// id). It costs a ring slot until the server skips it, so it is for
// programming errors, not for normal control flow.
func (c *Conn) Abort(e *Enc) {
	if e != &c.enc || !c.txOpen {
		return
	}
	c.txOpen = false
	if !e.multi {
		if err := e.res.Abort(hqq.ErrCodeInvalidOp); err != nil {
			c.failTx(err)
		}
	}
	e.body, e.buf = nil, nil
	c.afterOp()
}

func (c *Conn) newCall() *Call {
	if n := len(c.free); n > 0 {
		call := c.free[n-1]
		c.free[n-1] = nil
		c.free = c.free[:n-1]
		return call
	}
	return &Call{c: c}
}

// unregister removes call from the live set.
func (c *Conn) unregister(call *Call) {
	last := len(c.live) - 1
	moved := c.live[last]
	c.live[call.idx] = moved
	moved.idx = call.idx
	c.live[last] = nil
	c.live = c.live[:last]
}

// recycle ends a call whose response was consumed: its future can no longer
// match (8.3 item 4) and the object is reused.
func (c *Conn) recycle(call *Call) {
	c.unregister(call)
	call.pending, call.done, call.inPlace = false, false, false
	call.status, call.msg, call.body = 0, "", nil
	if cap(call.buf) > keepBuf {
		call.buf = nil
	}
	if len(c.free) < maxFreeCalls {
		c.free = append(c.free, call)
	}
}

// dropCall ends a call that is still unanswered in the ring. The object is not
// reused: the ring may still name it.
func (c *Conn) dropCall(call *Call) {
	c.unregister(call)
	call.pending, call.done, call.inPlace = false, false, false
	call.body = nil
}

func (c *Conn) unansHead() *Call {
	if c.unansN == 0 {
		return nil
	}
	return c.unans[c.unansHd]
}

// armRead sets the read deadline of one Wait, Sync or flow-control read run:
// CallTimeout from now, or none. A deadline left over from an earlier run is
// cleared first.
func (c *Conn) armRead() error {
	if c.callTimeout > 0 {
		c.rdArmed = true
		return c.link.SetReadDeadline(time.Now().Add(c.callTimeout))
	}
	if c.rdArmed {
		c.rdArmed = false
		return c.link.SetReadDeadline(time.Time{})
	}
	return nil
}

// readResponse reads frames until the oldest unanswered call has its complete
// response. It returns the link's timeout error without touching the session;
// every other failure breaks the session (c.rxDead).
func (c *Conn) readResponse(mode rxMode) error {
	for {
		res, err := c.link.ReserveRead()
		if err != nil {
			return c.readError(err)
		}
		c.rxRes = res
		held, complete, err := c.acceptFrame(c.rxRes.Buffer(), mode)
		if err != nil {
			if rerr := c.rxRes.Release(); rerr != nil {
				err = errors.Join(err, rerr)
			}
			return c.failRx(err)
		}
		if held {
			c.rxHeld = true
			return nil
		}
		if rerr := c.rxRes.Release(); rerr != nil {
			return c.failRx(&TransportError{Err: rerr, Unknown: true, svc: c.iface.Service})
		}
		if complete {
			return nil
		}
	}
}

func (c *Conn) readError(err error) error {
	if errors.Is(err, hqq.ErrTimeout) {
		return err
	}
	return c.failRx(&TransportError{Err: err, Unknown: true, svc: c.iface.Service})
}

// failRx records the failure of the receive side. A failure of the receive
// side replaces one that only the send side has seen, because it says more
// about the calls in flight. Protocol errors and the like arrive as the
// session error; a plain error is wrapped.
func (c *Conn) failRx(err error) error {
	replace := c.broken == nil || !c.rxDead
	c.rxDead = true
	if !replace {
		return c.broken
	}
	switch err.(type) {
	case *TransportError, *ProtocolError, *StatusError:
		c.broken = err
	default:
		c.broken = &TransportError{Err: err, Unknown: true, svc: c.iface.Service}
	}
	return c.broken
}

// failTx records the failure of the send side. Responses that were already
// published are still delivered, so the receive side stays alive.
func (c *Conn) failTx(err error) {
	if c.broken == nil {
		c.broken = &TransportError{Err: err, Unknown: true, svc: c.iface.Service}
	}
}

// afterOp closes the link of a session whose receive side died, as soon as no
// reservation is open: closing it earlier would unmap a buffer the caller
// still holds.
func (c *Conn) afterOp() {
	if c.rxDead && !c.txOpen && !c.rxOpen {
		c.closeLink()
	}
}

func (c *Conn) closeLink() error {
	c.closeOnce.Do(func() { c.linkErr = c.closeFn() })
	return c.linkErr
}

// brokenErr is the error of a call that was refused because the session
// failed earlier: a known outcome.
func (c *Conn) brokenErr(op uint16) error {
	return &TransportError{
		Func: c.opName(op),
		Err:  fmt.Errorf("%w: %s", ErrSessionBroken, errText(c.broken)),
		svc:  c.iface.Service,
	}
}

// errForCall is the session failure as seen by a call that was in flight.
func (c *Conn) errForCall(op uint16) error {
	fn := c.opName(op)
	switch e := c.broken.(type) {
	case *TransportError:
		x := *e
		x.Func, x.Unknown = fn, true
		return &x
	case *ProtocolError:
		x := *e
		x.Func = fn
		return &x
	case *StatusError:
		x := *e
		x.Func = fn
		return &x
	}
	return c.broken
}

func (c *Conn) violation(reason string) error {
	return &ProtocolError{Reason: reason, Unknown: true, svc: c.iface.Service}
}

// acceptFrame applies the receiver rules of 4.4 and 4.8 to one frame of the
// response direction and moves the oldest unanswered call towards completion.
// b is the whole frame: its length is DataSize.
func (c *Conn) acceptFrame(b []byte, mode rxMode) (held, complete bool, err error) {
	if len(b) < HeaderSize {
		return false, false, c.violation("frame shorter than the header")
	}
	h := loadHeader(b)
	body := b[HeaderSize:]
	if h.flags&^flagsKnown != 0 || (h.flags&flagDetail != 0 && h.flags&flagFail == 0) {
		return false, false, c.violation("invalid flags")
	}
	if c.asm.active {
		complete, err = c.acceptCont(h, body, mode)
		return false, complete, err
	}
	if h.kind != kindResponse {
		return false, false, c.violation("unexpected frame kind")
	}
	if uint64(h.total) > uint64(c.lim.MaxMessage) {
		return false, false, c.violation("total above max_message")
	}
	if len(body) != min(int(h.total), c.chunk) {
		return false, false, c.violation("frame length does not match total")
	}
	if h.id == 0 {
		return false, false, c.finalFrame(h, body)
	}
	head := c.unansHead()
	if head == nil {
		return false, false, c.violation("response without a request")
	}
	if h.id != head.id || h.op != head.op {
		return false, false, c.violation("response does not match the oldest request")
	}
	if h.aux != statusOK {
		if err := c.statusResponse(head, h, body); err != nil {
			return false, false, err
		}
		return false, true, nil
	}
	if int64(h.total) > head.respMax {
		return false, false, c.violation("response above its bound")
	}
	head.flags = h.flags
	if int(h.total) > c.chunk {
		c.asm = assembly{active: true, id: h.id, total: h.total, got: len(body)}
		if mode != rxDiscard {
			head.buf = growTo(head.buf, int(h.total))
			copy(head.buf, body)
		}
		return false, false, nil
	}
	switch mode {
	case rxHold:
		head.body = body[:len(body):len(body)]
		head.inPlace = true
		c.finish(head)
		return true, true, nil
	case rxBuffer:
		head.buf = append(head.buf[:0], body...)
		head.body = head.buf
	}
	c.finish(head)
	return false, true, nil
}

func (c *Conn) acceptCont(h header, body []byte, mode rxMode) (complete bool, err error) {
	a := &c.asm
	head := c.unansHead()
	if !contMatches(h, a) {
		return false, c.violation("continuation does not match its message")
	}
	if len(body) != min(int(a.total)-a.got, c.chunk) {
		return false, c.violation("continuation length")
	}
	if mode != rxDiscard {
		copy(head.buf[a.got:], body)
	}
	a.got += len(body)
	if a.got < int(a.total) {
		return false, nil
	}
	if mode != rxDiscard {
		head.body = head.buf
	}
	c.finish(head)
	return true, nil
}

// contMatches applies the CONT rules of 4.8 that depend on the header only.
func contMatches(h header, a *assembly) bool {
	if h.kind != kindCont || h.flags != 0 || h.op != 0 {
		return false
	}
	return h.id == a.id && h.total == a.total && int64(h.aux) == int64(a.got)
}

// callStatusOK reports whether a RESPONSE with a nonzero id may carry status
// s: the call statuses and the application range, not the final-frame
// statuses and not the reserved ranges.
func callStatusOK(s uint32) bool {
	if s >= statusUnknownOp && s <= statusInternal {
		return true
	}
	return s >= statusAppMin && s <= statusAppMax
}

// statusResponse handles a RESPONSE with a nonzero status (4.10).
func (c *Conn) statusResponse(head *Call, h header, body []byte) error {
	if h.flags != 0 {
		return c.violation("flags on an error response")
	}
	if !callStatusOK(h.aux) {
		return c.violation("status that is not valid for a call")
	}
	if h.total > ErrBodyMax {
		return c.violation("error response above ERRBODY_MAX")
	}
	msg, end, err := GetStr(body, 0, MsgMax)
	if err != nil || end != len(body) {
		return c.violation("malformed status message")
	}
	head.status, head.msg = h.aux, msg
	c.finish(head)
	return nil
}

// finalFrame handles a RESPONSE with id 0 (4.10): the server ends the session.
func (c *Conn) finalFrame(h header, body []byte) error {
	finalStatus := h.aux == statusProtocol || h.aux == statusShuttingDown
	if h.flags != 0 || h.op != 0 || !finalStatus {
		return c.violation("malformed final frame")
	}
	msg, end, err := GetStr(body, 0, MsgMax)
	if err != nil || end != len(body) {
		return c.violation("malformed final frame message")
	}
	return &StatusError{Status: h.aux, Msg: msg, svc: c.iface.Service}
}

// finish marks the oldest unanswered call as answered.
func (c *Conn) finish(call *Call) {
	call.done = true
	c.unans[c.unansHd] = nil
	c.unansHd++
	if c.unansHd == len(c.unans) {
		c.unansHd = 0
	}
	c.unansN--
	c.unansRF -= call.rf
	c.asm = assembly{}
}

func growTo(b []byte, n int) []byte {
	if cap(b) >= n {
		return b[:n]
	}
	return make([]byte, n)
}

// Wait returns the response of call. It first checks, before it reads
// anything, that call != nil, call is pending and call.ID() == id, and returns
// ErrFutureUsed otherwise (8.3). A nonzero status becomes *StatusError;
// E_INTERNAL also matches ErrOutcomeUnknown; transport failures become
// *TransportError; a link error ErrInvalidSize or ErrUnexpectedPacket marks
// the session broken (5.4). Responses of earlier calls are buffered (8.3). On
// success the caller MUST call Release.
func (c *Conn) Wait(call *Call, id uint32) (Dec, error) {
	if call == nil || call.c != c || !call.pending || call.id != id {
		return Dec{}, ErrFutureUsed
	}
	if c.rxOpen {
		return Dec{}, ErrConcurrentUse
	}
	c.rxOpen = true
	if !call.done {
		if err := c.await(call); err != nil {
			c.rxOpen = false
			if !c.rxDead {
				return Dec{}, &TransportError{Func: c.opName(call.op), Err: err, Unknown: true, svc: c.iface.Service}
			}
			e := c.errForCall(call.op)
			c.dropCall(call)
			c.afterOp()
			return Dec{}, e
		}
	}
	if call.status != statusOK {
		e := &StatusError{Func: c.opName(call.op), Status: call.status, Msg: call.msg, svc: c.iface.Service}
		c.recycle(call)
		c.rxOpen = false
		c.afterOp()
		return Dec{}, e
	}
	c.rxGen++
	d := Dec{
		Body:   call.body,
		Detail: call.flags&flagDetail != 0,
		Fail:   call.flags&flagFail != 0,
		c:      c,
		gen:    c.rxGen,
		op:     call.op,
	}
	c.recycle(call)
	return d, nil
}

// await reads responses until call is answered. The response of call itself is
// held in place when it is next.
func (c *Conn) await(call *Call) error {
	armed := false
	for !call.done {
		if c.rxDead {
			return c.broken
		}
		if !armed {
			if err := c.armRead(); err != nil {
				return c.readError(err)
			}
			armed = true
		}
		mode := rxBuffer
		if c.unansHead() == call {
			mode = rxHold
		}
		if err := c.readResponse(mode); err != nil {
			return err
		}
	}
	return nil
}

// Release releases the response slot. It is idempotent.
func (d *Dec) Release() {
	c := d.c
	if c == nil {
		return
	}
	d.c, d.Body = nil, nil
	if d.gen != c.rxGen || !c.rxOpen {
		return
	}
	c.endRx()
}

func (c *Conn) endRx() {
	if c.rxHeld {
		c.rxHeld = false
		if err := c.rxRes.Release(); err != nil {
			c.failRx(&TransportError{Err: err, Unknown: true, svc: c.iface.Service})
		}
	}
	c.rxOpen = false
	c.afterOp()
}

// Malformed marks the session broken, because the response body is not what
// the interface says, and returns the *ProtocolError for the call.
func (d *Dec) Malformed() error {
	c := d.c
	if c == nil {
		return &ProtocolError{Reason: "malformed response body", Unknown: true}
	}
	e := &ProtocolError{Func: c.opName(d.op), Reason: "malformed response body", Unknown: true, svc: c.iface.Service}
	c.failRx(&ProtocolError{Reason: "malformed response body of " + e.Func, Unknown: true, svc: c.iface.Service})
	return e
}

// LibError builds the *LibError of a response with F_FAIL; g2Size is size(G2).
// If d.Detail it decodes the detail string at round_up(g2Size, 8) (G2pad,
// 4.6.4) and requires it to end the body; otherwise it requires len(Body) ==
// g2Size. Either violation is Malformed.
func (d *Dec) LibError(fn string, code int64, g2Size int) error {
	var detail string
	if d.Detail {
		s, end, err := GetStr(d.Body, Align(g2Size, 8), DetailMax)
		if err != nil || end != len(d.Body) {
			return d.Malformed()
		}
		detail = s
	} else if len(d.Body) != g2Size {
		return d.Malformed()
	}
	svc := ""
	if d.c != nil {
		svc = d.c.iface.Service
	}
	return &LibError{Func: fn, Code: code, Detail: detail, svc: svc}
}

// Sync waits for every unanswered call and discards unclaimed results.
func (c *Conn) Sync() error {
	if c.txOpen || c.rxOpen {
		return ErrConcurrentUse
	}
	var rerr error
	armed := false
	for c.unansN > 0 && !c.rxDead {
		if !armed {
			if rerr = c.armRead(); rerr != nil {
				rerr = c.readError(rerr)
				break
			}
			armed = true
		}
		if rerr = c.readResponse(rxDiscard); rerr != nil {
			break
		}
	}
	if c.rxDead {
		c.abandon()
		c.afterOp()
		return c.broken
	}
	if rerr != nil {
		c.discardAnswered()
		return &TransportError{Err: rerr, Unknown: true, svc: c.iface.Service}
	}
	c.discardAnswered()
	return nil
}

// discardAnswered recycles every call whose response was received but not
// claimed.
func (c *Conn) discardAnswered() {
	for i := 0; i < len(c.live); {
		call := c.live[i]
		if call.done {
			c.recycle(call) // swaps another call into slot i
			continue
		}
		i++
	}
}

// abandon ends every call, answered or not, and empties the ring. Responses
// that are still to come are not read: the caller closes the session.
func (c *Conn) abandon() {
	for len(c.live) > 0 {
		call := c.live[len(c.live)-1]
		if call.done {
			c.recycle(call)
		} else {
			c.dropCall(call)
		}
	}
	clear(c.unans)
	c.unansHd, c.unansN, c.unansRF = 0, 0, 0
	c.asm = assembly{}
}

// Close writes CLOSE, reads and discards responses until CLOSE_ACK or
// CloseTimeout, and closes the link. It is idempotent and must not run while
// another goroutine is blocked in Wait (8.2).
func (c *Conn) Close() error {
	if c.txOpen || c.rxOpen {
		return ErrConcurrentUse
	}
	if c.closed {
		return c.closeRes
	}
	c.closed = true
	c.abandon()
	var err error
	if c.broken == nil {
		err = c.closeSession()
	}
	err = errors.Join(err, c.closeLink())
	c.closeRes = err
	return err
}

func (c *Conn) transportErr(err error) error {
	return &TransportError{Err: err, svc: c.iface.Service}
}

func (c *Conn) closeSession() error {
	deadline := time.Now().Add(c.closeTimeout)
	if err := c.link.SetWriteDeadline(deadline); err != nil {
		return c.transportErr(err)
	}
	w, err := c.link.ReserveWrite()
	if err != nil {
		return c.transportErr(err)
	}
	putHeader(w.Buffer(), header{kind: kindClose})
	if err := w.Commit(HeaderSize); err != nil {
		return c.transportErr(err)
	}
	if err := c.link.SetReadDeadline(deadline); err != nil {
		return c.transportErr(err)
	}
	c.rdArmed = true
	for {
		r, err := c.link.ReserveRead()
		if err != nil {
			return c.transportErr(err)
		}
		b := r.Buffer()
		n := len(b)
		var h header
		if n >= HeaderSize {
			h = loadHeader(b)
		}
		if err := r.Release(); err != nil {
			return c.transportErr(err)
		}
		if n < HeaderSize {
			return c.violation("frame shorter than the header")
		}
		switch h.kind {
		case kindResponse, kindCont:
		case kindCloseAck:
			if n != HeaderSize || h.flags != 0 || h.op != 0 || h.id != 0 || h.total != 0 {
				return c.violation("malformed CLOSE_ACK")
			}
			return nil
		default:
			return c.violation("unexpected frame kind while closing")
		}
	}
}

// Kill closes the link without the CLOSE exchange; the one call that is safe
// from any goroutine. Calls in flight have an unknown outcome (8.2). It must
// not run while the owner holds an open Enc or Dec: the buffers they point
// into are unmapped.
func (c *Conn) Kill() {
	_ = c.closeLink()
}
