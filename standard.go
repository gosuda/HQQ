package hqq

import (
	"context"
	"io"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"gosuda.org/hqq/internal/mpmc"
	"gosuda.org/hqq/internal/protocol"
)

// cacheLinePad separates independently written fields.
type cacheLinePad struct{ _ [128]byte }

// StandardLink is one endpoint of a bidirectional shared-memory link.
//
// Concurrency: all methods are safe for concurrent use. Write, WriteZeroCopy
// and ReserveWrite may be called from many goroutines (each call publishes
// exactly one message). Read, ReadZeroCopy and ReserveRead may also be
// called concurrently. One read call returns bytes of at most one message: a
// message that did not fit into a Read buffer leaves its unread tail queued
// (first in, first out) and the following read calls on this endpoint return
// the queued tails, each in a call of its own, before they consume newer
// messages. With concurrent readers, messages and tails may be received in
// any order, but never mixed within one call. Deadlines may be changed while
// calls are blocked.
//
// Lifetime: Close wakes every blocked call (they return ErrClosed), waits for
// in-flight calls (including running zero-copy callbacks), stops the
// heartbeat and detaches the endpoint. Buffers obtained from zero-copy
// callbacks or reservations must not be used after Close. Close must not be
// called from inside a zero-copy callback, see Close.
type StandardLink struct {
	// Configuration
	linkMode    LinkMode // Primary or Secondary mode
	linkType    LinkType // Standard or Advanced type
	bufferCount int      // Number of buffers in each pool
	bufferSize  int      // Size of each buffer in bytes
	opts        LinkOptions

	// Shared memory
	sb    *standardSuperblock
	self  int    // endpoint index in sb.endpoints
	token uint64 // this endpoint's owner token
	epoch uint64 // superblock epoch at attach time

	// MPMC rings and slot-owned payload buffers for both directions.
	ring0, ring1       *mpmc.MPMCRing[protocol.Packet] // 0: primary->secondary, 1: secondary->primary
	buffers0, buffers1 [][]byte
	txRing, rxRing     *mpmc.MPMCRing[protocol.Packet]
	txBufs, rxBufs     [][]byte

	// State management
	idGenerator atomic.Uint64 // Generates copy IDs
	recv        recvRemainder // FIFO of unread tails of partially read messages

	readDeadline  atomic.Pointer[time.Time]
	writeDeadline atomic.Pointer[time.Time]

	// Lifecycle
	closed    atomic.Bool
	closeCh   chan struct{}
	closeOnce sync.Once
	hbStop    chan struct{}
	hbDone    chan struct{}
	peer      peerMonitor

	_        cacheLinePad
	rxActive atomic.Int64 // in-flight receive-side calls
	_        cacheLinePad
	txActive atomic.Int64 // in-flight send-side calls
	_        cacheLinePad

	// Precomputed method values (avoid per-call allocations).
	checkFn         func() error
	readDeadlineFn  func() time.Time
	writeDeadlineFn func() time.Time
}

// NewStandardLink creates or attaches to a standard link in caller-owned
// memory. mem must be page-aligned and at least SizeStandardLink(bufferCount,
// bufferSize) bytes long, and the caller keeps it mapped for the lifetime of
// the link; it is typically a MAP_SHARED mapping, or AlignedBuffer for links
// between goroutines of one process. For links between processes prefer the
// named constructors CreateStandardLink and OpenStandardLink.
//
// The first endpoint becomes the Primary and initializes the segment; the
// second becomes the Secondary. A segment whose endpoints both closed or died
// is re-initialized. A third endpoint gets ErrLinkBusy. A segment whose first
// page is not entirely zero and that is not a valid HQQ segment is not
// touched: the call fails with ErrIncompatibleLayout, as it does for a
// segment of another layout version. A geometry that differs from the one
// the segment was initialized with fails with a *GeometryError, which
// matches ErrInvalidSize.
func NewStandardLink(mem []byte, bufferCount int, bufferSize int, opts LinkOptions) (*StandardLink, error) {
	if len(mem) == 0 {
		return nil, ErrMemorySmall
	}
	base := unsafe.Pointer(unsafe.SliceData(mem))
	if uintptr(base)%pagesize != 0 {
		return nil, ErrMemoryAlign
	}
	size := SizeStandardLink(bufferCount, bufferSize)
	if size == 0 {
		return nil, ErrInvalidSize
	}
	if uintptr(len(mem)) < size {
		return nil, ErrMemorySmall
	}
	return openStandardLink(base, bufferCount, bufferSize, opts)
}

// AlignedBuffer returns a page-aligned, zeroed byte slice of at least size
// bytes from the Go heap. It is useful for links between goroutines of one
// process (tests, examples); inter-process links need shared memory, see
// CreateStandardLink.
func AlignedBuffer(size uintptr) []byte {
	backing := make([]byte, size+pagesize)
	off := uintptr(unsafe.Pointer(unsafe.SliceData(backing))) % pagesize
	start := uintptr(0)
	if off != 0 {
		start = pagesize - off
	}
	return backing[start : start+size : start+size]
}

// ringAttachSlice is how long one mpmc.Attach call may wait; between calls
// attachRing looks at the context.
const ringAttachSlice = 10 * time.Millisecond

// attachRing attaches to the ring at p, waiting up to timeout for it to be
// initialized and giving up with ctx.Err() as soon as ctx is done. A nil ring
// with a nil error means the ring is missing or has an incompatible layout.
func attachRing(ctx context.Context, p unsafe.Pointer, timeout time.Duration) (*mpmc.MPMCRing[protocol.Packet], error) {
	deadline := time.Now().Add(timeout)
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		start := time.Now()
		if r := mpmc.Attach[protocol.Packet](p, ringAttachSlice); r != nil {
			return r, nil
		}
		// Attach returns early only for an incompatible layout; it returns
		// after the full slice when the ring is not initialized yet.
		if time.Since(start) < ringAttachSlice || !time.Now().Before(deadline) {
			return nil, nil
		}
	}
}

//go:nocheckptr
func openStandardLink(base unsafe.Pointer, bufferCount int, bufferSize int, opts LinkOptions) (*StandardLink, error) {
	layout, ok := computeLayout(bufferCount, bufferSize)
	if !ok {
		return nil, ErrInvalidSize
	}
	opts = opts.normalized()
	ctx := orBackground(opts.ctx)
	opts.ctx = nil // the link does not keep the caller's context
	requiredSize := layout.total
	token := newToken()
	sb := (*standardSuperblock)(base)

	claimOpts := opts
	claimOpts.ctx = ctx
	linkMode, mustInit, err := claimSuperblock(sb, bufferCount, bufferSize, requiredSize, token, claimOpts)
	if err != nil {
		return nil, err
	}

	buffersSize := bufferCount * bufferSize // cannot overflow: bounded by layout.total
	buffers0 := unsafe.Slice((*byte)(unsafe.Add(base, layout.buf0)), buffersSize)
	buffers1 := unsafe.Slice((*byte)(unsafe.Add(base, layout.buf1)), buffersSize)

	var epoch uint64
	if mustInit {
		// Every step first checks that this endpoint still owns the
		// initialization: an endpoint that lost it must not touch the
		// segment any more. A frozen initializer can still resume inside a
		// write phase it entered legitimately, so the check also runs
		// between the slices of a Format and of the zeroing; the residual
		// window is one slice (SPEC 5.3).
		session := &initSession{sb: sb, token: token, ctx: ctx}
		for _, ringOff := range [2]uintptr{layout.ring0, layout.ring1} {
			if err := mpmc.FormatGuarded[protocol.Packet](unsafe.Add(base, ringOff), uint64(bufferCount), session.check); err != nil {
				return nil, err
			}
			session.progress()
		}
		if err := session.zero(buffers0); err != nil {
			return nil, err
		}
		if err := session.zero(buffers1); err != nil {
			return nil, err
		}
		if epoch, err = publishSuperblock(sb, session.check, bufferCount, bufferSize, requiredSize, token, opts); err != nil {
			return nil, err
		}
	} else {
		epoch = atomic.LoadUint64(&sb.epoch)
	}

	self := endpointPrimary
	if linkMode == LinkModeSecondary {
		self = endpointSecondary
	}
	detach := func() {
		atomic.CompareAndSwapUint64(&sb.endpoints[self].token, token, detachedToken)
	}

	ring0, err := attachRing(ctx, unsafe.Add(base, layout.ring0), opts.AttachTimeout)
	if err != nil {
		detach()
		return nil, err
	}
	ring1, err := attachRing(ctx, unsafe.Add(base, layout.ring1), opts.AttachTimeout)
	if err != nil {
		detach()
		return nil, err
	}
	if ring0 == nil || ring1 == nil ||
		ring0.Size() != uint64(bufferCount) || ring1.Size() != uint64(bufferCount) {
		detach()
		return nil, ErrIncompatibleLayout
	}
	ring0.SetWaitPolicy(opts.WaitPolicy.ring())
	ring1.SetWaitPolicy(opts.WaitPolicy.ring())
	if opts.parkers[0] != nil {
		ring0.SetParker(opts.parkers[0])
	}
	if opts.parkers[1] != nil {
		ring1.SetParker(opts.parkers[1])
	}

	link := &StandardLink{
		linkMode:    linkMode,
		linkType:    LinkTypeStandard,
		bufferCount: bufferCount,
		bufferSize:  bufferSize,
		opts:        opts,
		sb:          sb,
		self:        self,
		token:       token,
		epoch:       epoch,
		ring0:       ring0,
		ring1:       ring1,
		buffers0:    splitBuffers(buffers0, bufferCount, bufferSize),
		buffers1:    splitBuffers(buffers1, bufferCount, bufferSize),
		closeCh:     make(chan struct{}),
	}
	if linkMode == LinkModeSecondary {
		link.txRing, link.txBufs = ring1, link.buffers1
		link.rxRing, link.rxBufs = ring0, link.buffers0
	} else {
		link.txRing, link.txBufs = ring0, link.buffers0
		link.rxRing, link.rxBufs = ring1, link.buffers1
	}
	link.recv.size = bufferSize
	link.checkFn = link.checkPeer
	link.readDeadlineFn = link.loadReadDeadline
	link.writeDeadlineFn = link.loadWriteDeadline

	// The secondary attaches to a live primary: start monitoring it now.
	if linkMode == LinkModeSecondary {
		_ = link.checkPeer()
	}
	if opts.HeartbeatInterval > 0 {
		link.hbStop = make(chan struct{})
		link.hbDone = make(chan struct{})
		go link.heartbeatLoop(opts.HeartbeatInterval)
	}
	return link, nil
}

func splitBuffers(region []byte, count, size int) [][]byte {
	out := make([][]byte, count)
	for i := range out {
		start := i * size
		end := start + size
		out[i] = region[start:end:end]
	}
	return out
}

// enterRx/enterTx register an in-flight call so Close can wait for it. The
// counter is incremented before the closed flag is checked; Close sets the
// flag before waiting for the counters, so no call can touch shared memory
// after Close returns.
func (l *StandardLink) enterRx() error {
	l.rxActive.Add(1)
	if l.closed.Load() {
		l.rxActive.Add(-1)
		return ErrClosed
	}
	return nil
}

func (l *StandardLink) exitRx() { l.rxActive.Add(-1) }

func (l *StandardLink) enterTx(checkPeer bool) error {
	l.txActive.Add(1)
	if l.closed.Load() {
		l.txActive.Add(-1)
		return ErrClosed
	}
	if checkPeer {
		// Without the heartbeat goroutine nothing else refreshes the peer
		// status between blocking calls, so a send would keep publishing
		// into a ring after the peer closed or died (F-10).
		if l.hbStop == nil {
			_ = l.checkPeer()
		}
		if err := PeerState(l.peer.status.Load()).err(); err != nil {
			l.txActive.Add(-1)
			return err
		}
	}
	return nil
}

func (l *StandardLink) exitTx() { l.txActive.Add(-1) }

func (l *StandardLink) loadReadDeadline() time.Time {
	if d := l.readDeadline.Load(); d != nil {
		return *d
	}
	return time.Time{}
}

func (l *StandardLink) loadWriteDeadline() time.Time {
	if d := l.writeDeadline.Load(); d != nil {
		return *d
	}
	return time.Time{}
}

// mapWaitErr converts ring wait errors into link errors.
func mapWaitErr(err error) error {
	switch err {
	case mpmc.ErrClosed:
		return ErrClosed
	case mpmc.ErrTimeout:
		return ErrTimeout
	default:
		return err
	}
}

// dequeueDrained dequeues one packet from ring. If that fails because the
// peer closed or died, it makes one more attempt without waiting: the peer
// may have published its last messages right before it went away, and they
// are delivered before the failure is reported. The returned error is the raw
// ring or peer error; callers map it with mapWaitErr.
func dequeueDrained(ring *mpmc.MPMCRing[protocol.Packet], c *mpmc.Cancel, fn func(slot uint64, p *protocol.Packet)) error {
	err := ring.DequeueZeroCopyCancel(c, fn)
	if err != nil && peerLost(err) && ring.DequeueZeroCopyCancel(&mpmc.Cancel{NoWait: true}, fn) == nil {
		return nil
	}
	return err
}

// reserveDrained is dequeueDrained for a consumer reservation.
func reserveDrained(ring *mpmc.MPMCRing[protocol.Packet], c *mpmc.Cancel) (mpmc.ConsumerSlot[protocol.Packet], error) {
	slot, err := ring.ReserveConsumerCancel(c)
	if err != nil && peerLost(err) {
		if s, retryErr := ring.ReserveConsumerCancel(&mpmc.Cancel{NoWait: true}); retryErr == nil {
			return s, nil
		}
	}
	return slot, err
}

// peerLost reports whether err says that the peer closed or died.
func peerLost(err error) bool { return err == ErrPeerClosed || err == ErrPeerGone }

// dequeue consumes one packet. fn runs while the slot is claimed.
func (l *StandardLink) dequeue(fn func(slot uint64, p *protocol.Packet)) error {
	c := mpmc.Cancel{Closed: l.closeCh, DeadlineFunc: l.readDeadlineFn, Check: l.checkFn}
	return mapWaitErr(dequeueDrained(l.rxRing, &c, fn))
}

// enqueue publishes one packet. fn runs while the slot is claimed.
func (l *StandardLink) enqueue(fn func(slot uint64, p *protocol.Packet)) error {
	c := mpmc.Cancel{Closed: l.closeCh, DeadlineFunc: l.writeDeadlineFn, Check: l.checkFn}
	return mapWaitErr(l.txRing.EnqueueZeroCopyCancel(&c, fn))
}

// reserveRx reserves the next readable packet slot.
func (l *StandardLink) reserveRx() (mpmc.ConsumerSlot[protocol.Packet], error) {
	c := mpmc.Cancel{Closed: l.closeCh, DeadlineFunc: l.readDeadlineFn, Check: l.checkFn}
	slot, err := reserveDrained(l.rxRing, &c)
	return slot, mapWaitErr(err)
}

// classify interprets a packet taken from the receive ring. It returns the
// payload size of a copy packet; size 0 and a nil error mean a tombstone that
// the caller skips. Error packets yield the peer's error, anything else
// ErrUnexpectedPacket.
func (l *StandardLink) classify(p *protocol.Packet) (size int, err error) {
	switch p.Op() {
	case protocol.OpStandardLinkTombstone:
		return 0, nil
	case protocol.OpStandardLinkCopy:
		size = int(p.Operand(2))
		if size <= 0 || size > l.bufferSize {
			return 0, ErrInvalidSize
		}
		return size, nil
	case protocol.OpError:
		return 0, errorFromCode(ErrorCode(p.Operand(1)))
	default:
		return 0, ErrUnexpectedPacket
	}
}

// consume dequeues one packet and hands the payload of a copy packet to
// deliver while the ring slot is claimed. skip reports a tombstone: nothing
// was delivered and the caller should try the next packet. The slot is
// released even if deliver panics.
func (l *StandardLink) consume(deliver func(payload []byte) (int, error)) (n int, skip bool, err error) {
	var pktErr error
	err = l.dequeue(func(slot uint64, p *protocol.Packet) {
		size, cerr := l.classify(p)
		switch {
		case cerr != nil:
			pktErr = cerr
		case size == 0:
			skip = true
		default:
			n, pktErr = deliver(l.rxBufs[slot][:size:size])
		}
	})
	if err != nil {
		return 0, false, err
	}
	return n, skip, pktErr
}

// Read implements io.Reader. One call returns bytes of at most one message
// (or of one queued tail of a previous message): a message larger than b is
// split across Read calls, and the unread tail is returned by the following
// read calls before newer messages are consumed. After the peer closed the
// link gracefully and every buffered message was consumed, Read returns
// io.EOF itself (not ErrPeerClosed), as the io.Reader contract requires, so
// io.Copy, io.ReadAll and bufio.Scanner treat it as a normal end of stream.
//
// Read is the automatic-release hot path for the same packet stream consumed
// by ReadZeroCopy and ReserveRead. It releases the claimed ring slot before
// returning to the caller. Tombstones are skipped. Advanced Protocol frames
// are discarded with ErrUnexpectedPacket.
func (l *StandardLink) Read(b []byte) (int, error) {
	if len(b) == 0 {
		return 0, nil
	}
	return l.recvMessage(b, false)
}

// recvMessage implements Read (datagram false) and ReadFrom (datagram true):
// it serves the oldest queued tail if there is one, otherwise the next
// message. In datagram mode the part of the message that does not fit into b
// is discarded instead of queued, and an empty b still consumes one message.
func (l *StandardLink) recvMessage(b []byte, datagram bool) (int, error) {
	if err := l.enterRx(); err != nil {
		return 0, err
	}
	defer l.exitRx()

	if n, ok := l.recv.read(b, datagram); ok {
		return n, nil
	}
	// One callback per packet (no nested deliver closure): this is the
	// hottest receive path.
	for {
		var (
			n      int
			skip   bool
			pktErr error
		)
		err := l.dequeue(func(slot uint64, p *protocol.Packet) {
			size, cerr := l.classify(p)
			switch {
			case cerr != nil:
				pktErr = cerr
			case size == 0:
				skip = true
			default:
				payload := l.rxBufs[slot][:size:size]
				n = copy(b, payload)
				if n < size && !datagram {
					l.recv.store(payload[n:])
				}
			}
		})
		if err == ErrPeerClosed {
			return 0, io.EOF
		}
		if err != nil {
			return 0, err
		}
		if pktErr != nil {
			return 0, pktErr
		}
		if !skip {
			return n, nil
		}
	}
}

// Write implements io.Writer. It publishes b as one message and rejects
// payloads larger than BufferSize with ErrBufferOverflow (use an AdvancedLink
// for larger messages).
//
// Write is the automatic-commit hot path for the same packet stream produced
// by WriteZeroCopy and ReserveWrite. It publishes a copy packet before
// returning to the caller.
func (l *StandardLink) Write(b []byte) (int, error) {
	bN := len(b)
	if bN == 0 {
		return 0, nil
	}
	if bN > l.bufferSize {
		return 0, ErrBufferOverflow
	}
	if err := l.enterTx(true); err != nil {
		return 0, err
	}
	defer l.exitTx()

	copyID := l.idGenerator.Add(1)
	err := l.enqueue(func(slot uint64, p *protocol.Packet) {
		copy(l.txBufs[slot], b)
		*p = protocol.NewStandardLinkCopyPacket(copyID, slot, uint64(bN))
	})
	if err != nil {
		return 0, err
	}
	return bN, nil
}

// WriteZeroCopy writes one message by letting fn fill the ring-slot-owned
// shared payload buffer directly.
//
// WARNING: The callback runs while the underlying ring slot is claimed. It must
// return promptly; if it blocks or never returns, the whole ring can suffer
// head-of-line (HOL) blocking. Do not retain the buffer after the callback returns.
//
// The callback must return a payload size in the range 1..BufferSize. If it
// returns an error or an invalid size, a tombstone is published to keep the
// ring moving and the error is returned. If it panics, a tombstone is
// published as well and the panic continues in the caller after the slot
// is released, so a failing callback cannot wedge the link.
//
// The callback must not call Close on this link: Close waits for the
// callback to return, so the call would deadlock (like locking a sync.Mutex
// twice).
func (l *StandardLink) WriteZeroCopy(fn func(buffer []byte) (int, error)) (int, error) {
	if fn == nil {
		return 0, ErrInvalidOp
	}
	if err := l.enterTx(true); err != nil {
		return 0, err
	}
	defer l.exitTx()

	copyID := l.idGenerator.Add(1)
	c := mpmc.Cancel{Closed: l.closeCh, DeadlineFunc: l.writeDeadlineFn, Check: l.checkFn}
	slot, err := l.txRing.ReserveProducerCancel(&c)
	if err != nil {
		return 0, mapWaitErr(err)
	}
	return l.fillSlot(slot, copyID, fn)
}

// fillSlot lets fn fill the payload buffer of the claimed producer slot and
// publishes the matching packet. The slot is published even if fn panics or
// calls runtime.Goexit: the packet is a tombstone until fn returned
// successfully, so readers skip a slot whose writer failed and the panic
// continues without wedging the ring.
func (l *StandardLink) fillSlot(slot mpmc.ProducerSlot[protocol.Packet], copyID uint64, fn func(buffer []byte) (int, error)) (int, error) {
	idx := slot.Slot()
	p := slot.Value()
	*p = protocol.NewStandardLinkTombstonePacket(copyID, idx, uint64(ErrCodeInvalidOp))
	defer slot.Commit()

	buffer := l.txBufs[idx]
	n, err := fn(buffer)
	switch {
	case err != nil:
		return 0, err
	case n <= 0:
		*p = protocol.NewStandardLinkTombstonePacket(copyID, idx, uint64(ErrCodeInvalidSize))
		return 0, ErrInvalidSize
	case n > len(buffer):
		*p = protocol.NewStandardLinkTombstonePacket(copyID, idx, uint64(ErrCodeBufferOverflow))
		return 0, ErrBufferOverflow
	}
	*p = protocol.NewStandardLinkCopyPacket(copyID, idx, uint64(n))
	return n, nil
}

// ReadZeroCopy reads one message by passing the ring-slot-owned shared payload
// buffer directly to fn. If a previous Read left the tail of a message
// queued, fn receives the oldest queued tail (all of it) instead of a new
// message; the buffer then lives in the endpoint's own memory, not in the
// ring. fn never sees bytes of two messages.
//
// WARNING: The callback runs while the underlying ring slot is claimed. It must
// return promptly; if it blocks or never returns, the whole ring can suffer
// head-of-line (HOL) blocking. Do not retain the buffer after the callback returns.
//
// The buffer is only valid for the duration of fn. Returning from fn releases
// the ring slot and allows a future writer to overwrite the same memory. The
// slot is released, and the message consumed, if fn panics as well.
//
// The callback must not call Close on this link: Close waits for the
// callback to return, so the call would deadlock (like locking a sync.Mutex
// twice).
func (l *StandardLink) ReadZeroCopy(fn func(buffer []byte) error) (int, error) {
	if fn == nil {
		return 0, ErrInvalidOp
	}
	// Register before touching the remainder so that Close waits for fn in
	// every case.
	if err := l.enterRx(); err != nil {
		return 0, err
	}
	defer l.exitRx()

	if view, backing, ok := l.recv.take(); ok {
		// Give the backing store back even if fn panics.
		defer l.recv.giveBack(backing)
		if err := fn(view); err != nil {
			return 0, err
		}
		return len(view), nil
	}
	deliver := func(payload []byte) (int, error) {
		if err := fn(payload); err != nil {
			return 0, err
		}
		return len(payload), nil
	}
	for {
		n, skip, err := l.consume(deliver)
		if err != nil {
			return 0, err
		}
		if !skip {
			return n, nil
		}
	}
}

// Mode returns the endpoint role (Primary or Secondary).
func (l *StandardLink) Mode() LinkMode {
	return l.linkMode
}

// Type returns LinkTypeStandard.
func (l *StandardLink) Type() LinkType {
	return l.linkType
}

// BufferSize returns the configured payload-buffer size in bytes.
func (l *StandardLink) BufferSize() int {
	if l == nil {
		return 0
	}
	return l.bufferSize
}

// BufferCount returns the configured payload-buffer count per direction.
func (l *StandardLink) BufferCount() int {
	if l == nil {
		return 0
	}
	return l.bufferCount
}

// PeerState reports what this endpoint currently knows about its peer.
func (l *StandardLink) PeerState() PeerState {
	if l.enterRx() == nil {
		_ = l.checkPeer()
		l.exitRx()
	}
	return PeerState(l.peer.status.Load())
}

// Close closes the endpoint: blocked calls return ErrClosed, in-flight calls
// are waited for, the heartbeat stops and the endpoint detaches from the
// segment so that the peer observes ErrPeerClosed. Close is idempotent.
//
// Close waits for zero-copy callbacks that are running; a callback that never
// returns makes Close block forever. Consequently Close must not be called
// from inside a ReadZeroCopy or WriteZeroCopy callback (it would wait for
// itself, like locking a sync.Mutex twice), and an active reservation must be
// finished with Commit, Abort or Release before Close is called: its buffer
// is invalid once Close has run.
func (l *StandardLink) Close() error {
	l.closeOnce.Do(func() {
		l.closed.Store(true)
		close(l.closeCh)
		l.ring0.WakeAll()
		l.ring1.WakeAll()
		for l.rxActive.Load() != 0 || l.txActive.Load() != 0 {
			time.Sleep(50 * time.Microsecond)
		}
		if l.hbStop != nil {
			close(l.hbStop)
			<-l.hbDone
		}
		atomic.CompareAndSwapUint64(&l.sb.endpoints[l.self].token, l.token, detachedToken)
		// Let the peer's blocked calls notice the detach promptly.
		l.ring0.WakeAll()
		l.ring1.WakeAll()
	})
	return nil
}

// recvRemainder is a FIFO of the unread tails of partially read messages.
// Each tail keeps its own backing buffer, so two tails are never merged and
// no read returns bytes of two messages. Backing buffers are recycled
// through a small free list; zero-copy consumers borrow a tail's buffer and
// give it back.
type recvRemainder struct {
	pending atomic.Bool // the queue holds at least one tail
	mu      sync.Mutex
	queue   []tail   // queue[head:] are the tails, oldest first
	head    int      //
	free    [][]byte // spare backing buffers, at most maxSpareTails
	size    int      // preferred buffer capacity (BufferSize)
}

// tail is the unread part buf[off:end] of one message.
type tail struct {
	buf      []byte
	off, end int
}

// maxSpareTails bounds the recycled buffers kept per endpoint; there is one
// queued tail per reader that was mid-message at the same time, so a handful
// covers every realistic workload.
const maxSpareTails = 4

// store queues a copy of src as the newest tail.
func (r *recvRemainder) store(src []byte) {
	r.mu.Lock()
	buf := r.getBuf(len(src))
	n := copy(buf, src)
	if r.head > 0 && len(r.queue) == cap(r.queue) {
		// Reclaim the consumed prefix instead of growing the queue.
		live := copy(r.queue, r.queue[r.head:])
		clear(r.queue[live:])
		r.queue = r.queue[:live]
		r.head = 0
	}
	r.queue = append(r.queue, tail{buf: buf, end: n})
	r.pending.Store(true)
	r.mu.Unlock()
}

// read copies from the oldest tail into dst. With discard the tail is dropped
// even if dst was too short for it. ok is false if no tail is queued.
func (r *recvRemainder) read(dst []byte, discard bool) (n int, ok bool) {
	if !r.pending.Load() {
		return 0, false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.head == len(r.queue) {
		return 0, false
	}
	t := &r.queue[r.head]
	n = copy(dst, t.buf[t.off:t.end])
	t.off += n
	if discard || t.off == t.end {
		r.putBuf(t.buf)
		r.popLocked()
	}
	return n, true
}

// take lends out all of the oldest tail. giveBack must be called with
// backing once the caller is done with view.
func (r *recvRemainder) take() (view, backing []byte, ok bool) {
	if !r.pending.Load() {
		return nil, nil, false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.head == len(r.queue) {
		return nil, nil, false
	}
	t := r.queue[r.head]
	r.popLocked()
	return t.buf[t.off:t.end:t.end], t.buf, true
}

func (r *recvRemainder) giveBack(backing []byte) {
	r.mu.Lock()
	r.putBuf(backing)
	r.mu.Unlock()
}

// popLocked drops the oldest tail from the queue. r.mu must be held.
func (r *recvRemainder) popLocked() {
	r.queue[r.head] = tail{}
	r.head++
	if r.head == len(r.queue) {
		r.queue = r.queue[:0]
		r.head = 0
		r.pending.Store(false)
	}
}

// getBuf returns a buffer of length (= capacity) at least n. r.mu must be held.
func (r *recvRemainder) getBuf(n int) []byte {
	if k := len(r.free); k > 0 {
		b := r.free[k-1]
		r.free = r.free[:k-1]
		if cap(b) >= n {
			return b
		}
	}
	return make([]byte, max(n, r.size))
}

// putBuf recycles a backing buffer. r.mu must be held.
func (r *recvRemainder) putBuf(b []byte) {
	if len(r.free) < maxSpareTails {
		r.free = append(r.free, b[:cap(b)])
	}
}
