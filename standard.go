package hqq

import (
	"net"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"gosuda.org/hqq/internal/mpmc"
	"gosuda.org/hqq/internal/protocol"
	"gosuda.org/hqq/internal/race"
)

// cacheLinePad separates independently written fields.
type cacheLinePad struct{ _ [128]byte }

// StandardLink is one endpoint of a bidirectional shared-memory link.
//
// Concurrency: all methods are safe for concurrent use. Write, WriteZeroCopy
// and ReserveWrite may be called from many goroutines (each call publishes
// exactly one message). Read, ReadZeroCopy and ReserveRead may also be
// called concurrently; each call consumes whole messages, and the unread tail
// of a message that did not fit into a Read buffer is returned by the next
// read call on this endpoint. Deadlines may be changed while calls are
// blocked.
//
// Lifetime: Close wakes every blocked call (they return ErrClosed), waits for
// in-flight calls to finish, stops the heartbeat and detaches the endpoint.
// Buffers obtained from zero-copy callbacks or reservations must not be used
// after Close.
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
	recv        recvRemainder // Unread tail of a partially read message

	// PacketConn interface support
	localAddr     net.Addr
	remoteAddr    atomic.Pointer[addrHolder]
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

type addrHolder struct{ addr net.Addr }

// OpenStandardLink creates or attaches to a standard link in caller-owned,
// page-aligned memory at offset with default options. See
// OpenStandardLinkWithOptions.
func OpenStandardLink(offset uintptr, bufferCount int, bufferSize int) (*StandardLink, error) {
	return OpenStandardLinkWithOptions(offset, bufferCount, bufferSize, LinkOptions{})
}

// OpenStandardLinkWithOptions creates or attaches to a standard link at
// offset, the address of at least SizeStandardLink(bufferCount, bufferSize)
// bytes of page-aligned memory that the caller keeps mapped for the lifetime
// of the link (typically a MAP_SHARED mapping).
//
// The first endpoint becomes the Primary and initializes the segment; the
// second becomes the Secondary. A segment whose endpoints both closed or died
// is re-initialized. A third endpoint gets ErrLinkBusy.
//
// Prefer OpenStandardLinkMemory, which can validate the memory size, or the
// named-memory constructors CreateStandardLink / OpenNamedStandardLink.
func OpenStandardLinkWithOptions(offset uintptr, bufferCount int, bufferSize int, opts LinkOptions) (*StandardLink, error) {
	if offset == 0 || offset%pagesize != 0 {
		return nil, ErrMemoryAlign
	}
	return openStandardLink(mpmc.PointerFromAddress(offset), bufferCount, bufferSize, opts)
}

// OpenStandardLinkMemory is OpenStandardLinkWithOptions for a memory slice.
// mem must be page-aligned and at least SizeStandardLink(bufferCount,
// bufferSize) bytes long; it is typically a shared mapping, or AlignedBuffer
// for links between goroutines of one process.
func OpenStandardLinkMemory(mem []byte, bufferCount int, bufferSize int, opts LinkOptions) (*StandardLink, error) {
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

// layoutOffsets returns the offsets of the four regions after the superblock.
func layoutOffsets(bufferCount, bufferSize int) (ring0, buf0, ring1, buf1 uintptr) {
	dataRingSize := mpmc.SizeMPMCRing[protocol.Packet](uintptr(bufferCount))
	buffersSize := uintptr(bufferCount) * uintptr(bufferSize)
	ring0 = alignPage(unsafe.Sizeof(standardSuperblock{}))
	buf0 = alignPage(ring0 + dataRingSize)
	ring1 = alignPage(buf0 + buffersSize)
	buf1 = alignPage(ring1 + dataRingSize)
	return
}

//go:nocheckptr
func openStandardLink(base unsafe.Pointer, bufferCount int, bufferSize int, opts LinkOptions) (*StandardLink, error) {
	if bufferSize%BufferSizeAlignment != 0 || !validConfig(bufferCount, bufferSize) {
		return nil, ErrInvalidSize
	}
	opts = opts.normalized()
	requiredSize := SizeStandardLink(bufferCount, bufferSize)
	token := newToken()
	sb := (*standardSuperblock)(base)

	linkMode, mustInit, err := claimSuperblock(sb, bufferCount, bufferSize, requiredSize, token, opts)
	if err != nil {
		return nil, err
	}

	ring0Off, buf0Off, ring1Off, buf1Off := layoutOffsets(bufferCount, bufferSize)
	buffersSize := bufferCount * bufferSize
	buffers0 := unsafe.Slice((*byte)(unsafe.Add(base, buf0Off)), buffersSize)
	buffers1 := unsafe.Slice((*byte)(unsafe.Add(base, buf1Off)), buffersSize)

	var epoch uint64
	if mustInit {
		mpmc.Format[protocol.Packet](unsafe.Add(base, ring0Off), uint64(bufferCount))
		mpmc.Format[protocol.Packet](unsafe.Add(base, ring1Off), uint64(bufferCount))
		zeroWithProgress(sb, buffers0)
		zeroWithProgress(sb, buffers1)
		if epoch, err = publishSuperblock(sb, bufferCount, bufferSize, requiredSize, token, opts); err != nil {
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

	ring0 := mpmc.Attach[protocol.Packet](unsafe.Add(base, ring0Off), opts.AttachTimeout)
	ring1 := mpmc.Attach[protocol.Packet](unsafe.Add(base, ring1Off), opts.AttachTimeout)
	if ring0 == nil || ring1 == nil ||
		ring0.Size() != uint64(bufferCount) || ring1.Size() != uint64(bufferCount) {
		detach()
		return nil, ErrFailedInit
	}
	ring0.SetWaitPolicy(opts.WaitPolicy.ring())
	ring1.SetWaitPolicy(opts.WaitPolicy.ring())

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
		localAddr:   &HQQAddr{LinkID: "hqq-local"},
		closeCh:     make(chan struct{}),
	}
	link.remoteAddr.Store(&addrHolder{addr: &HQQAddr{LinkID: "hqq-remote"}})
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

// dequeue consumes one packet. fn runs while the slot is claimed.
func (l *StandardLink) dequeue(fn func(slot uint64, p *protocol.Packet)) error {
	c := mpmc.Cancel{Closed: l.closeCh, DeadlineFunc: l.readDeadlineFn, Check: l.checkFn}
	err := l.rxRing.DequeueZeroCopyCancel(&c, fn)
	if err == nil {
		return nil
	}
	if err == ErrPeerClosed || err == ErrPeerGone {
		// The peer may have published its last messages right before it
		// went away; deliver them before reporting the failure.
		if l.rxRing.DequeueZeroCopyCancel(&mpmc.Cancel{NoWait: true}, fn) == nil {
			return nil
		}
	}
	return mapWaitErr(err)
}

// enqueue publishes one packet. fn runs while the slot is claimed.
func (l *StandardLink) enqueue(fn func(slot uint64, p *protocol.Packet)) error {
	c := mpmc.Cancel{Closed: l.closeCh, DeadlineFunc: l.writeDeadlineFn, Check: l.checkFn}
	return mapWaitErr(l.txRing.EnqueueZeroCopyCancel(&c, fn))
}

// Read implements io.Reader. It returns one message (or the unread tail of a
// previous message); a message larger than b is split across Read calls.
//
// Read is the automatic-release hot path for the same packet stream consumed
// by ReadZeroCopy and ReserveRead. It releases the claimed ring slot before
// returning to the caller. Tombstones are skipped. Advanced Protocol frames
// are discarded with ErrUnexpectedPacket.
func (l *StandardLink) Read(b []byte) (int, error) {
	if len(b) == 0 {
		return 0, nil
	}
	if n, ok := l.recv.read(b); ok {
		return n, nil
	}
	if err := l.enterRx(); err != nil {
		return 0, err
	}
	defer l.exitRx()

	for {
		var n int
		var readErr error
		err := l.dequeue(func(slot uint64, p *protocol.Packet) {
			switch p.Op() {
			case protocol.OpStandardLinkTombstone:
			case protocol.OpStandardLinkCopy:
				n, readErr = l.copyOut(p, l.rxBufs[slot], b)
			case protocol.OpError:
				readErr = errorFromCode(ErrorCode(p.Operand(1)))
			default:
				readErr = ErrUnexpectedPacket
			}
		})
		if err != nil {
			return 0, err
		}
		if readErr != nil {
			return n, readErr
		}
		if n > 0 {
			return n, nil
		}
	}
}

// copyOut copies a Standard copy packet's payload into dst and keeps the
// unread tail in the remainder buffer.
func (l *StandardLink) copyOut(p *protocol.Packet, buffer []byte, dst []byte) (int, error) {
	size := int(p.Operand(2))
	if size <= 0 || size > len(buffer) {
		return 0, ErrInvalidSize
	}
	n := race.Copy(dst, buffer[:size])
	if n < size {
		l.recv.store(buffer[n:size])
	}
	return n, nil
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
		race.Copy(l.txBufs[slot], b)
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
// ring moving and the error is returned.
func (l *StandardLink) WriteZeroCopy(fn func(buffer []byte) (int, error)) (int, error) {
	if fn == nil {
		return 0, ErrInvalidOp
	}
	if err := l.enterTx(true); err != nil {
		return 0, err
	}
	defer l.exitTx()

	copyID := l.idGenerator.Add(1)
	var n int
	var callbackErr error
	err := l.enqueue(func(slot uint64, p *protocol.Packet) {
		buffer := l.txBufs[slot]
		n, callbackErr = fn(buffer)
		switch {
		case callbackErr != nil:
			*p = protocol.NewStandardLinkTombstonePacket(copyID, slot, uint64(ErrCodeInvalidOp))
		case n <= 0:
			callbackErr = ErrInvalidSize
			*p = protocol.NewStandardLinkTombstonePacket(copyID, slot, uint64(ErrCodeInvalidSize))
		case n > len(buffer):
			callbackErr = ErrBufferOverflow
			*p = protocol.NewStandardLinkTombstonePacket(copyID, slot, uint64(ErrCodeBufferOverflow))
		default:
			*p = protocol.NewStandardLinkCopyPacket(copyID, slot, uint64(n))
		}
	})
	if err != nil {
		return 0, err
	}
	if callbackErr != nil {
		return 0, callbackErr
	}
	return n, nil
}

// ReadZeroCopy reads one message by passing the ring-slot-owned shared payload
// buffer directly to fn.
//
// WARNING: The callback runs while the underlying ring slot is claimed. It must
// return promptly; if it blocks or never returns, the whole ring can suffer
// head-of-line (HOL) blocking. Do not retain the buffer after the callback returns.
//
// The buffer is only valid for the duration of fn. Returning from fn releases
// the ring slot and allows a future writer to overwrite the same memory.
func (l *StandardLink) ReadZeroCopy(fn func(buffer []byte) error) (int, error) {
	if fn == nil {
		return 0, ErrInvalidOp
	}
	if view, backing, ok := l.recv.take(); ok {
		err := fn(view)
		l.recv.giveBack(backing)
		if err != nil {
			return 0, err
		}
		return len(view), nil
	}
	if err := l.enterRx(); err != nil {
		return 0, err
	}
	defer l.exitRx()

	for {
		var n int
		var readErr error
		err := l.dequeue(func(slot uint64, p *protocol.Packet) {
			switch p.Op() {
			case protocol.OpStandardLinkTombstone:
			case protocol.OpStandardLinkCopy:
				size := int(p.Operand(2))
				if size <= 0 || size > l.bufferSize {
					readErr = ErrInvalidSize
					return
				}
				n = size
				readErr = fn(l.rxBufs[slot][:size:size])
			case protocol.OpError:
				readErr = errorFromCode(ErrorCode(p.Operand(1)))
			default:
				readErr = ErrUnexpectedPacket
			}
		})
		if err != nil {
			return 0, err
		}
		if readErr != nil {
			return 0, readErr
		}
		if n > 0 {
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

// GetMode returns the endpoint role.
//
// Deprecated: Use Mode.
func (l *StandardLink) GetMode() LinkMode {
	return l.Mode()
}

// GetType returns the link type.
//
// Deprecated: Use Type.
func (l *StandardLink) GetType() LinkType {
	return l.Type()
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

// LargeMessageThreshold returns the largest payload size that this StandardLink
// can publish in one Standard Protocol packet.
func (l *StandardLink) LargeMessageThreshold() int {
	if l == nil {
		return 0
	}
	return LargeMessageThreshold(l.bufferSize)
}

// IsLargeMessage reports whether payloadSize should be sent through the
// Advanced Protocol large-message path for this link configuration.
func (l *StandardLink) IsLargeMessage(payloadSize int) bool {
	if l == nil {
		return false
	}
	return IsLargeMessage(l.bufferSize, payloadSize)
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
// returns makes Close block forever.
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

// recvRemainder holds the unread tail of a partially read message. Its
// backing buffer is reused across partial reads; zero-copy consumers borrow
// it and give it back.
type recvRemainder struct {
	pending  atomic.Bool
	mu       sync.Mutex
	buf      []byte // backing store, nil while lent out
	off, end int
	size     int // preferred capacity (BufferSize)
}

func (r *recvRemainder) store(src []byte) {
	r.mu.Lock()
	if r.off < r.end {
		// Concurrent readers produced two tails; keep both, in order.
		r.buf = append(r.buf[:r.end], src...)
		r.end += len(src)
	} else {
		if cap(r.buf) < len(src) {
			r.buf = make([]byte, max(len(src), r.size))
		}
		r.buf = r.buf[:cap(r.buf)]
		race.Copy(r.buf, src)
		r.off, r.end = 0, len(src)
	}
	r.pending.Store(true)
	r.mu.Unlock()
}

func (r *recvRemainder) read(dst []byte) (int, bool) {
	if !r.pending.Load() {
		return 0, false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.off == r.end {
		return 0, false
	}
	n := copy(dst, r.buf[r.off:r.end])
	r.off += n
	if r.off == r.end {
		r.off, r.end = 0, 0
		r.pending.Store(false)
	}
	return n, true
}

// take lends out the whole remainder. giveBack must be called with backing.
func (r *recvRemainder) take() (view, backing []byte, ok bool) {
	if !r.pending.Load() {
		return nil, nil, false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.off == r.end {
		return nil, nil, false
	}
	view = r.buf[r.off:r.end:r.end]
	backing = r.buf
	r.buf = nil
	r.off, r.end = 0, 0
	r.pending.Store(false)
	return view, backing, true
}

func (r *recvRemainder) giveBack(backing []byte) {
	r.mu.Lock()
	if r.buf == nil {
		r.buf = backing[:cap(backing)]
	}
	r.mu.Unlock()
}
