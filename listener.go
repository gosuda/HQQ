package hqq

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"gosuda.org/hqq/internal/mmap"
	"gosuda.org/hqq/internal/mpmc"
	"gosuda.org/hqq/internal/shm"
)

// A link has exactly two endpoints, so a service that talks to many clients
// needs a rendezvous. The Acceptor owns a well-known listener segment: one
// MPMC ring of fixed-size connect records. A client (Connect) creates a link
// segment of its own, as the Primary endpoint of an ordinary link, announces
// it with a record in the ring and waits for the server to attach to it as
// the Secondary. Every client has its own segment, rings and heartbeat, so
// one client crashing or stalling never affects another. The layout of the
// listener segment is specified in spec/ext/LISTENER_DRAFT.md.
//
// Trust model. Connect records come from other processes on the same host
// and are untrusted input. The acceptor validates every field, never trusts
// a length, bounds everything it allocates or maps by its configuration
// (AcceptorConfig.MaxBufferCount, MaxBufferSize, MaxLinkSize,
// MaxHandshakes, Backlog) and only attaches to segments that carry the name
// prefix of its own listener and the instance token the record announces.
// Access control is the segment permission: a process that may open the
// listener segment for writing can connect, and can also corrupt the
// listener segment itself or fill its queue (the acceptor recovers from a
// slot a producer claimed and never published, see
// AcceptorConfig.StuckSlotTimeout, but a writer can always make connecting
// fail). Restrict AcceptorConfig.Perm to the principals that may connect.

// AcceptorConfig configures an Acceptor. The zero value selects the defaults.
type AcceptorConfig struct {
	// Perm is the permission of the created listener segment. Clients need
	// read and write access to it. Default: 0600. Accepted links are the
	// clients' segments; their permission is Config.Perm of the client, and
	// the server must be able to open them.
	Perm os.FileMode

	// QueueSize is the number of connect records the listener holds (a power
	// of two between 2 and 65536). Clients that find it full wait until the
	// acceptor makes room. Default: DefaultAcceptorQueueSize. An existing
	// listener name whose object is smaller than the requested segment is
	// not reused (ErrInvalidSize); remove it with Unlink.
	QueueSize int

	// Features is the set of features the acceptor offers on accepted links.
	// The negotiated set is its intersection with the features the client
	// requests. Default: all supported features.
	Features uint64

	// Advanced configures every accepted link, including its heartbeat
	// interval and peer timeout. Link.HeartbeatInterval is also the
	// heartbeat interval of the listener itself (a negative value disables
	// it: clients cannot tell a crashed acceptor from a slow one), and
	// Link.PeerTimeout and Link.AttachTimeout bound the waits of
	// NewAcceptor for a listener segment another process is initializing.
	Advanced AdvancedOptions

	// MaxBufferCount and MaxBufferSize bound the geometry of a link that a
	// client may ask for (defaults 4096 and 1 MiB), and MaxLinkSize the size
	// of the whole link segment, SizeStandardLink (default 256 MiB). Records
	// that exceed a bound are dropped. The limits are published in the
	// listener header, so Connect fails at once for a geometry the acceptor
	// would drop.
	MaxBufferCount int
	MaxBufferSize  int
	MaxLinkSize    uint64

	// Backlog is the number of fully negotiated links that wait for Accept
	// (default 16). While it is full no further handshake completes, the
	// record queue fills up and clients wait: this is the backpressure of a
	// server that accepts slower than clients arrive.
	Backlog int

	// MaxHandshakes bounds the handshakes (attach and negotiation, one
	// goroutine each) that run at the same time (default 16). A client that
	// stalls during its handshake occupies one of them until
	// HandshakeTimeout.
	MaxHandshakes int

	// HandshakeTimeout bounds attaching to a client's segment and
	// negotiating with it (default DefaultAttachTimeout). A client that does
	// not answer in time is dropped.
	HandshakeTimeout time.Duration

	// StuckSlotTimeout is how long a queue slot may stay claimed by a
	// producer without being published before the acceptor publishes it as
	// an empty record on the producer's behalf. Without this, a client
	// killed between claiming a slot and publishing it would block every
	// record behind it. Default: 2 seconds. A negative value disables the
	// recovery.
	StuckSlotTimeout time.Duration
}

const (
	defaultAcceptorMaxBufferCount = 4096
	defaultAcceptorMaxBufferSize  = 1 << 20
	defaultAcceptorMaxLinkSize    = 256 << 20
	defaultAcceptorBacklog        = 16
	defaultAcceptorHandshakes     = 16
	defaultStuckSlotTimeout       = 2 * time.Second
)

func (c AcceptorConfig) normalized() (AcceptorConfig, error) {
	if c.Perm == 0 {
		c.Perm = 0o600
	}
	if c.QueueSize == 0 {
		c.QueueSize = DefaultAcceptorQueueSize
	}
	if listenerLayoutSize(c.QueueSize) == 0 {
		return c, ErrInvalidSize
	}
	if c.Features == FeatureNone {
		c.Features = supportedFeatures
	}
	if c.MaxBufferCount == 0 {
		c.MaxBufferCount = defaultAcceptorMaxBufferCount
	}
	if c.MaxBufferSize == 0 {
		c.MaxBufferSize = defaultAcceptorMaxBufferSize
	}
	if c.MaxLinkSize == 0 {
		c.MaxLinkSize = defaultAcceptorMaxLinkSize
	}
	if c.MaxBufferCount < MinBufferCount || c.MaxBufferCount > maxBufferCount ||
		c.MaxBufferSize < MinBufferSize || c.MaxBufferSize > maxBufferSize {
		return c, ErrInvalidSize
	}
	if c.Backlog <= 0 {
		c.Backlog = defaultAcceptorBacklog
	}
	if c.MaxHandshakes <= 0 {
		c.MaxHandshakes = defaultAcceptorHandshakes
	}
	if c.HandshakeTimeout <= 0 {
		c.HandshakeTimeout = DefaultAttachTimeout
	}
	if c.StuckSlotTimeout == 0 {
		c.StuckSlotTimeout = defaultStuckSlotTimeout
	}
	return c, nil
}

// AcceptorStats counts what an Acceptor did with the connect records it
// consumed. Every record ends in exactly one of Accepted (counted when
// Accept returns the link), Malformed, Unreachable or HandshakeFailed, or is
// still being processed.
type AcceptorStats struct {
	// Records is the number of records taken from the queue.
	Records uint64
	// Accepted is the number of links returned by Accept.
	Accepted uint64
	// Malformed is the number of records dropped by validation: reserved
	// flags, a name that is not the acceptor's link name format, an invalid
	// token, or a geometry outside the configured limits.
	Malformed uint64
	// Unreachable is the number of valid records whose link segment could
	// not be attached: it does not exist (the client gave up), its client
	// is gone, it is not the announced instance, or its geometry differs
	// from the record.
	Unreachable uint64
	// HandshakeFailed is the number of links that attached but did not
	// complete negotiation in time or were rejected.
	HandshakeFailed uint64
	// StuckSlots is the number of queue slots published on behalf of a
	// producer that claimed them and did not publish them (see
	// AcceptorConfig.StuckSlotTimeout).
	StuckSlots uint64
}

type acceptorCounters struct {
	records, accepted, malformed, unreachable, handshakeFailed, stuckSlots atomic.Uint64
}

// listenerAddr is the net.Addr of an Acceptor.
type listenerAddr string

func (a listenerAddr) Network() string { return "hqq" }
func (a listenerAddr) String() string  { return string(a) }

// Acceptor serves a listener name: it accepts any number of clients that
// Connect to it. See the file comment for the protocol and trust model.
//
// Accept is safe for concurrent use. The acceptor runs the handshakes in the
// background, so a client that stalls while attaching or negotiating delays
// neither other clients nor Accept callers.
//
// Accepted links are independent of the Acceptor: Close stops accepting and
// removes the listener name, but does not close links that were returned by
// Accept. Links that completed their handshake but were not yet returned
// by Accept are closed.
type Acceptor struct {
	name   string
	cfg    AcceptorConfig
	lopts  LinkOptions // normalized cfg.Advanced.Link, for the listener's own waits and heartbeat
	m      *namedMapping
	base   unsafe.Pointer
	h      *listenerHeader
	ring   *mpmc.MPMCRing[connectRecord]
	token  uint64
	prefix string

	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}
	ready  chan *NamedAdvancedLink
	sem    chan struct{}
	wg     sync.WaitGroup

	closeOnce sync.Once
	closeErr  error
	stats     acceptorCounters

	// Stuck-slot detection, used by the housekeeping goroutine only.
	stuckPos   uint64
	stuckSince time.Time
	stuckSeen  bool
}

// NewAcceptor creates the listener segment name and starts accepting
// clients that Connect to it. It is NewAcceptorContext with a background
// context.
func NewAcceptor(name string, cfg AcceptorConfig) (*Acceptor, error) {
	return NewAcceptorContext(context.Background(), name, cfg)
}

// NewAcceptorContext creates the listener segment name and starts accepting.
//
// If the name exists, the acceptor looks at the existing segment instead of
// failing, like CreateStandardLink:
//
//   - If its acceptor closed or died, the segment is re-initialized and the
//     caller owns the name from now on. Connect requests that were waiting
//     in the old queue are lost; their clients fail with ErrListenerGone.
//   - If its acceptor is alive, the call fails with ErrListenerBusy.
//   - If it is not a listener segment of this layout version, the call
//     fails with ErrIncompatibleLayout and the object is not modified, whatever
//     its size. A listener of this layout version that is smaller than the
//     requested segment fails with ErrInvalidSize, and an object that is not
//     sized yet with a transient ErrFailedInit; both are left alone.
//
// ctx bounds the wait for a listener segment that another process is
// initializing. Listener names are shared-memory names (see
// CreateStandardLink); on Windows a name must leave room for the ".hqq-semN"
// suffix of the wake semaphores, and the names of the links of clients, which
// are derived from it, must stay within the kernel limit too.
func NewAcceptorContext(ctx context.Context, name string, cfg AcceptorConfig) (*Acceptor, error) {
	ctx = orBackground(ctx)
	cfg, err := cfg.normalized()
	if err != nil {
		return nil, err
	}
	size := listenerLayoutSize(cfg.QueueSize)
	prefix := listenerLinkPrefix(name)
	if len(prefix) > listenerMaxPrefixLen {
		return nil, ErrInvalidName
	}

	creatorToken := newToken()
	m, err := createNamed(name, size, cfg.Perm, listenerSmallObject)
	if err != nil {
		return nil, err
	}
	m.instance = creatorToken
	a := &Acceptor{
		name:   name,
		cfg:    cfg,
		lopts:  cfg.Advanced.Link.normalized(),
		m:      m,
		base:   unsafe.Pointer(unsafe.SliceData(m.mapping)),
		token:  newToken(),
		prefix: prefix,
		done:   make(chan struct{}),
		ready:  make(chan *NamedAdvancedLink, cfg.Backlog),
		sem:    make(chan struct{}, cfg.MaxHandshakes),
	}
	a.h = (*listenerHeader)(a.base)

	opts := a.lopts
	if err := claimListener(ctx, a.h, m.mapping, a.token, creatorToken, opts); err != nil {
		_ = m.release()
		return nil, err
	}
	if err := a.initialize(ctx, size); err != nil {
		_ = m.release()
		return nil, err
	}
	// The segment was initialized under a new creatorToken: the name is ours
	// to remove.
	m.unlinkOnClose = true

	ring := mpmc.Attach[connectRecord](unsafe.Add(a.base, listenerRingOffset), 50*time.Millisecond)
	if ring == nil {
		_ = m.release()
		return nil, ErrIncompatibleLayout
	}
	ring.SetWaitPolicy(opts.WaitPolicy.ring())
	if m.parkers[0] != nil {
		ring.SetParker(m.parkers[0])
	}
	a.ring = ring

	a.ctx, a.cancel = context.WithCancel(context.Background())
	a.wg.Add(2)
	go a.housekeeping()
	go a.dispatch()
	return a, nil
}

// initialize formats the ring and publishes the header, the last step of
// the initialization that claimListener granted to this acceptor.
func (a *Acceptor) initialize(ctx context.Context, size uintptr) error {
	h := a.h
	check := func() error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if atomic.LoadUint64(&h.initToken) != a.token {
			return errInitContended
		}
		return nil
	}
	if err := mpmc.FormatGuarded[connectRecord](unsafe.Add(a.base, listenerRingOffset), uint64(a.cfg.QueueSize), check); err != nil {
		return err
	}
	atomic.StoreUint64(&h.initHeartbeat, nowNanos())
	if err := check(); err != nil {
		return err
	}

	stampListener(h)
	atomic.StoreUint64(&h.ringOffset, listenerRingOffset)
	atomic.StoreUint64(&h.recordSize, listenerRecordSize)
	atomic.StoreUint64(&h.capacity, uint64(a.cfg.QueueSize))
	atomic.StoreUint64(&h.layoutSize, uint64(size))
	atomic.StoreUint64(&h.maxBufferCount, uint64(a.cfg.MaxBufferCount))
	atomic.StoreUint64(&h.maxBufferSize, uint64(a.cfg.MaxBufferSize))
	atomic.StoreUint64(&h.maxLinkSize, a.cfg.MaxLinkSize)
	atomic.StoreUint64(&h.features, a.cfg.Features)
	h.prefix = [listenerMaxPrefixLen]byte{}
	copy(h.prefix[:], a.prefix)
	atomic.StoreUint64(&h.prefixLen, uint64(len(a.prefix)))

	atomic.StoreUint64(&h.serverPID, uint64(os.Getpid()))
	atomic.StoreUint64(&h.serverInterval, uint64(a.lopts.HeartbeatInterval))
	atomic.StoreUint64(&h.serverHeartbeat, nowNanos())
	atomic.StoreUint64(&h.serverToken, a.token)
	atomic.StoreUint64(&h.magic, listenerMagic)

	// Release the initialization; if another endpoint took it over meanwhile
	// the segment is not marked Ready.
	if !atomic.CompareAndSwapUint64(&h.initToken, a.token, 0) {
		return errInitContended
	}
	atomic.AddUint64(&h.epoch, 1)
	atomic.StoreUint64(&h.state, listenerStateReady)
	return nil
}

// Name returns the listener name.
func (a *Acceptor) Name() string { return a.name }

// Addr returns the listener address: network "hqq", string form the name.
func (a *Acceptor) Addr() net.Addr { return listenerAddr(a.name) }

// Stats returns the counters of the acceptor.
func (a *Acceptor) Stats() AcceptorStats {
	return AcceptorStats{
		Records:         a.stats.records.Load(),
		Accepted:        a.stats.accepted.Load(),
		Malformed:       a.stats.malformed.Load(),
		Unreachable:     a.stats.unreachable.Load(),
		HandshakeFailed: a.stats.handshakeFailed.Load(),
		StuckSlots:      a.stats.stuckSlots.Load(),
	}
}

// Accept waits for the next client and returns its link, negotiated and
// ready for Send, Recv, Call, Serve and streams. The link is the Secondary
// endpoint of the segment the client created; closing it detaches from it
// (the client removes the name). Records that fail validation, or whose
// client is gone or does not complete its handshake, never fail Accept: they
// are dropped and counted in Stats.
//
// Accept returns ctx.Err() when ctx is done and ErrAcceptorClosed after
// Close. It is safe to call from several goroutines.
func (a *Acceptor) Accept(ctx context.Context) (*NamedAdvancedLink, error) {
	ctx = orBackground(ctx)
	select {
	case <-a.done:
		return nil, ErrAcceptorClosed
	default:
	}
	select {
	case l := <-a.ready:
		a.stats.accepted.Add(1)
		return l, nil
	case <-a.done:
		return nil, ErrAcceptorClosed
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// Close stops accepting, wakes blocked Accept calls (they return
// ErrAcceptorClosed), marks the listener closed so that waiting clients fail
// with ErrListenerClosed, closes the links that completed their handshake
// but were not returned by Accept, and removes the listener name if it still
// refers to this acceptor's segment. Links already returned by Accept stay
// open. Close is idempotent.
func (a *Acceptor) Close() error {
	a.closeOnce.Do(func() {
		close(a.done)
		a.cancel()
		a.ring.WakeAll()
		a.wg.Wait()
		for drained := false; !drained; {
			select {
			case l := <-a.ready:
				_ = l.Close()
			default:
				drained = true
			}
		}
		atomic.CompareAndSwapUint64(&a.h.serverToken, a.token, detachedToken)
		a.closeErr = a.m.release()
	})
	return a.closeErr
}

// housekeeping refreshes the heartbeat of the listener and recovers stuck
// queue slots.
func (a *Acceptor) housekeeping() {
	defer a.wg.Done()
	interval := a.lopts.HeartbeatInterval
	tick := interval
	if tick <= 0 {
		tick = 100 * time.Millisecond
	}
	if t := a.cfg.StuckSlotTimeout; t > 0 {
		tick = max(min(tick, t/4), time.Millisecond)
	}
	t := time.NewTicker(tick)
	defer t.Stop()
	var lastBeat time.Time
	for {
		select {
		case <-a.done:
			return
		case now := <-t.C:
			if interval > 0 && now.Sub(lastBeat) >= interval {
				lastBeat = now
				if atomic.LoadUint64(&a.h.serverToken) == a.token {
					atomic.StoreUint64(&a.h.serverHeartbeat, nowNanos())
				}
			}
			a.recoverStuckSlot(now)
		}
	}
}

// recoverStuckSlot publishes an empty record in the slot at the consumer
// cursor if a producer claimed it and did not publish it for
// StuckSlotTimeout: the slot is claimed (the producer cursor is ahead) but
// its sequence word still has the free value. The producer is a client that
// died, or hangs, between claiming the slot and committing it; without this
// every record behind the slot would wait forever. The empty record fails
// validation and is counted as Malformed. If the producer was only slow and
// commits later, its commit fails and its record is lost; the client's
// Connect then times out.
func (a *Acceptor) recoverStuckSlot(now time.Time) {
	if a.cfg.StuckSlotTimeout <= 0 {
		return
	}
	ring := unsafe.Add(a.base, listenerRingOffset)
	r := atomic.LoadUint64((*uint64)(unsafe.Add(ring, ringReadCursorOffset)))
	w := atomic.LoadUint64((*uint64)(unsafe.Add(ring, ringWriteCursorOffset)))
	seq := (*uint64)(unsafe.Add(ring, ringHeaderSize+uintptr(r&uint64(a.cfg.QueueSize-1))*connectSlotStride))
	if int64(w-r) <= 0 || atomic.LoadUint64(seq) != r {
		a.stuckSeen = false
		return
	}
	if !a.stuckSeen || a.stuckPos != r {
		a.stuckSeen, a.stuckPos, a.stuckSince = true, r, now
		return
	}
	if now.Sub(a.stuckSince) < a.cfg.StuckSlotTimeout {
		return
	}
	if atomic.CompareAndSwapUint64(seq, r, r+1) {
		a.stats.stuckSlots.Add(1)
		a.ring.WakeAll()
	}
	a.stuckSeen = false
}

// dispatch consumes the connect records, validates them and starts a
// handshake for every valid one.
func (a *Acceptor) dispatch() {
	defer a.wg.Done()
	for {
		select {
		case <-a.done:
			return
		default:
		}
		var rec connectRecord
		c := mpmc.Cancel{Closed: a.done}
		if err := a.ring.DequeueZeroCopyCancel(&c, func(_ uint64, e *connectRecord) { rec = *e }); err != nil {
			return
		}
		a.stats.records.Add(1)
		name, err := a.validate(&rec)
		if err != nil {
			a.stats.malformed.Add(1)
			continue
		}
		select {
		case a.sem <- struct{}{}:
		case <-a.done:
			return
		}
		a.wg.Add(1)
		go a.handshake(rec, name)
	}
}

// validate checks every field of a record, which is untrusted input, and
// returns the link segment name it announces.
func (a *Acceptor) validate(rec *connectRecord) (string, error) {
	if rec.flags != 0 {
		return "", errBadRecord
	}
	name, ok := rec.recordName()
	if !ok || !validLinkName(a.prefix, name) {
		return "", errBadRecord
	}
	if !isOwnerToken(rec.clientToken) {
		return "", errBadRecord
	}
	if rec.bufferCount > uint64(a.cfg.MaxBufferCount) || rec.bufferSize > uint64(a.cfg.MaxBufferSize) {
		return "", errBadRecord
	}
	count, size := int(rec.bufferCount), int(rec.bufferSize)
	layout, ok := computeLayout(count, size)
	if !ok || uint64(layout.total) > a.cfg.MaxLinkSize {
		return "", errBadRecord
	}
	return name, nil
}

// handshake attaches to the link segment of one client and negotiates.
func (a *Acceptor) handshake(rec connectRecord, name string) {
	defer a.wg.Done()
	defer func() { <-a.sem }()

	ctx, cancel := context.WithTimeout(a.ctx, a.cfg.HandshakeTimeout)
	defer cancel()
	opts := a.cfg.Advanced
	link, err := openNamedAdvancedInstance(ctx, name, int(rec.bufferCount), int(rec.bufferSize), false, 0, rec.clientToken, opts)
	if err != nil {
		if a.ctx.Err() != nil {
			return
		}
		a.stats.unreachable.Add(1)
		if errors.Is(err, errNoLiveEndpoint) {
			reapDeadSegment(name, rec.clientToken)
		}
		return
	}
	if err := link.Negotiate(ctx, a.cfg.Features); err != nil {
		_ = link.Close()
		if a.ctx.Err() == nil {
			a.stats.handshakeFailed.Add(1)
			if errors.Is(err, ErrPeerGone) {
				// The client died after announcing its segment.
				reapDeadSegment(name, rec.clientToken)
			}
		}
		return
	}
	select {
	case a.ready <- link:
	case <-a.done:
		_ = link.Close()
	}
}

// reapDeadSegment removes the name of the link segment of a client that died
// without closing it (nobody else would), provided the name still refers to
// the instance the client announced.
func reapDeadSegment(name string, token uint64) {
	if !shm.NamesPersist {
		return
	}
	if _, got, found, err := peekSuperblock(name); err == nil && found && got == token {
		_ = shm.Unlink(name)
	}
}

// stampListener records the layout version and required features, so that a
// segment in state Initializing already identifies its layout.
func stampListener(h *listenerHeader) {
	atomic.StoreUint64(&h.major, listenerMajor)
	atomic.StoreUint64(&h.minor, listenerMinor)
	atomic.StoreUint64(&h.requiredFeatures, 0)
}

// beginListenerInit makes token the initializer of the segment by replacing
// the initToken value expected that the caller saw (0 for a fresh claim). It
// reports false if initToken changed meanwhile, which means another
// endpoint took the initialization over. It also records the instance
// identity creatorToken.
func beginListenerInit(h *listenerHeader, token, expected, creatorToken uint64) bool {
	stampListener(h)
	if !atomic.CompareAndSwapUint64(&h.initToken, expected, token) {
		return false
	}
	atomic.StoreUint64(&h.creatorToken, creatorToken)
	atomic.StoreUint64(&h.initHeartbeat, nowNanos())
	return true
}

// listenerHeaderZero reports whether the whole header is zero.
func listenerHeaderZero(mem []byte) bool {
	base := unsafe.Pointer(unsafe.SliceData(mem))
	for off := uintptr(0); off < listenerHeaderSize; off += 8 {
		if atomic.LoadUint64((*uint64)(unsafe.Add(base, off))) != 0 {
			return false
		}
	}
	return true
}

// listenerSmallObject decides the error for an existing object shorter than
// the listener segment an acceptor asks for (see smallObjectError). Only a
// listener of this layout version is ErrInvalidSize; its header is inspected
// through a mapping of its first listenerHeaderSize bytes. Anything else (a
// link, foreign data, or an object too short to hold a header) is
// ErrIncompatibleLayout, so the size of an object alone never decides
// whether it is a listener.
func listenerSmallObject(smem *shm.SharedMemory, cur int64) error {
	if cur < listenerHeaderSize {
		return ErrIncompatibleLayout
	}
	// Only the first listenerHeaderSize bytes are mapped and read, all of
	// which lie inside the object, so no access reaches past its end.
	page, err := mmap.Map(smem.FD(), 0, listenerHeaderSize, mmap.PROT_READ|mmap.PROT_WRITE, mmap.MAP_SHARED)
	if err != nil {
		return err
	}
	defer func() { _ = mmap.UnMap(page) }()
	if checkListenerCompat((*listenerHeader)(unsafe.Pointer(unsafe.SliceData(page))), false) != nil {
		return ErrIncompatibleLayout
	}
	return ErrInvalidSize
}

// checkListenerCompat returns ErrIncompatibleLayout if the header was
// written by an incompatible layout version. A segment in state
// Initializing may carry no layout yet (its initializer died right after
// claiming it), so only a conflicting layout is rejected there.
func checkListenerCompat(h *listenerHeader, initializing bool) error {
	if atomic.LoadUint64(&h.requiredFeatures)&^listenerKnownRequiredFeatures != 0 {
		return ErrIncompatibleLayout
	}
	magic := atomic.LoadUint64(&h.magic)
	major := atomic.LoadUint64(&h.major)
	if !initializing {
		if magic != listenerMagic || major != listenerMajor {
			return ErrIncompatibleLayout
		}
		return nil
	}
	if magic != 0 && magic != listenerMagic {
		return ErrIncompatibleLayout
	}
	if major != listenerMajor && (major != 0 || atomic.LoadUint64(&h.initToken) != 0) {
		return ErrIncompatibleLayout
	}
	return nil
}

// listenerServerAlive decides whether the acceptor recorded in h is alive.
// The fast path trusts a recent wall-clock heartbeat; a heartbeat that looks
// stale is confirmed by watching for progress for a few intervals, which is
// robust against wall-clock jumps. The watch ends early with ctx.Err() when
// ctx is done.
func listenerServerAlive(ctx context.Context, h *listenerHeader, opts LinkOptions) (bool, error) {
	tok := atomic.LoadUint64(&h.serverToken)
	if !isOwnerToken(tok) {
		return false, nil
	}
	interval := time.Duration(atomic.LoadUint64(&h.serverInterval))
	if interval <= 0 || interval > time.Hour {
		// Heartbeats disabled: liveness cannot be judged, assume alive.
		return true, nil
	}
	hb := atomic.LoadUint64(&h.serverHeartbeat)
	if hb == 0 || heartbeatFresh(hb, effectivePeerTimeout(opts, interval)) {
		return true, nil
	}
	window := min(3*interval, 2*time.Second)
	step := max(interval/4, time.Millisecond)
	for end := time.Now().Add(window); time.Now().Before(end); {
		if err := sleepCtx(ctx, step); err != nil {
			return false, err
		}
		if atomic.LoadUint64(&h.serverHeartbeat) != hb || atomic.LoadUint64(&h.serverToken) != tok {
			return isOwnerToken(atomic.LoadUint64(&h.serverToken)), nil
		}
	}
	return false, nil
}

// claimListener decides whether the creator of a listener name may
// initialize the segment. It returns nil when the caller won the right to
// (re)initialize it, and must call Acceptor.initialize afterwards. It follows
// claimSuperblock with roleCreator: an empty segment is initialized, one whose
// acceptor is dead is re-initialized, one with a live acceptor fails with
// ErrListenerBusy, one that is not ours with ErrIncompatibleLayout, and a
// stalled initializer is taken over after its window. ctx bounds every wait.
func claimListener(ctx context.Context, h *listenerHeader, mem []byte, token, creatorToken uint64, opts LinkOptions) error {
	deadline := time.Now().Add(opts.AttachTimeout)
	staleInit := opts.initStaleWindow()
	var watch initWatch
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		state := atomic.LoadUint64(&h.state)
		if state != listenerStateInitializing {
			watch.active = false
		}
		stalling := false

		switch state {
		case listenerStateEmpty:
			if !listenerHeaderZero(mem) {
				if atomic.LoadUint64(&h.state) != listenerStateEmpty {
					continue // another endpoint claimed it meanwhile
				}
				// Not a segment of ours: leave foreign data alone.
				return ErrIncompatibleLayout
			}
			if atomic.CompareAndSwapUint64(&h.state, listenerStateEmpty, listenerStateInitializing) {
				if !beginListenerInit(h, token, atomic.LoadUint64(&h.initToken), creatorToken) {
					return errInitContended
				}
				return nil
			}

		case listenerStateInitializing:
			tok := atomic.LoadUint64(&h.initToken)
			hb := atomic.LoadUint64(&h.initHeartbeat)
			if err := checkListenerCompat(h, true); err != nil {
				return err
			}
			now := time.Now()
			if !watch.active || watch.tok != tok || watch.hb != hb {
				watch = initWatch{active: true, tok: tok, hb: hb, since: now}
			} else if now.Sub(watch.since) >= staleInit {
				if beginListenerInit(h, token, tok, creatorToken) {
					return nil
				}
				watch.active = false // somebody else moved first
			}
			stalling = true

		case listenerStateReady:
			if err := checkListenerCompat(h, false); err != nil {
				return err
			}
			alive, err := listenerServerAlive(ctx, h, opts)
			if err != nil {
				return err
			}
			if alive {
				return ErrListenerBusy
			}
			// The acceptor closed or died: the segment is reusable.
			if atomic.CompareAndSwapUint64(&h.state, listenerStateReady, listenerStateInitializing) {
				if !beginListenerInit(h, token, atomic.LoadUint64(&h.initToken), creatorToken) {
					return errInitContended
				}
				return nil
			}
			continue

		default:
			return ErrIncompatibleLayout
		}

		// A stalled initializer is given its whole window even if that runs
		// past AttachTimeout.
		limit := deadline
		if stalling && watch.active && watch.since.Before(deadline) {
			limit = later(deadline, watch.since.Add(staleInit))
		}
		if time.Now().After(limit) {
			if stalling && watch.active && time.Since(watch.since) >= staleInit {
				// The window ended between the takeover check above and
				// this one: take over now instead of giving up.
				continue
			}
			return errInitContended
		}
		if err := sleepCtx(ctx, claimPollInterval); err != nil {
			return err
		}
	}
}

// ---------------------------------------------------------------------
// Client side.

// connectTimeoutError is the error Connect returns when no acceptor attached
// within the attach timeout. It matches ErrTimeout.
func connectTimeoutError(d time.Duration) error {
	return fmt.Errorf("hqq: connect: the acceptor did not attach within %v: %w", d, ErrTimeout)
}

// listenerClient is a client's view of a listener segment.
type listenerClient struct {
	m      *namedMapping
	h      *listenerHeader
	ring   *mpmc.MPMCRing[connectRecord]
	prefix string
	watch  listenerWatch

	maxBufferCount, maxBufferSize, maxLinkSize uint64
}

func (lc *listenerClient) close() {
	_ = lc.m.release()
}

// listenerWatch watches the acceptor of one listener instance from a
// client, using the local monotonic clock so that wall-clock jumps cannot
// fake a dead acceptor.
type listenerWatch struct {
	h           *listenerHeader
	creator     uint64
	epoch       uint64
	token       uint64
	hb          uint64
	since       time.Time
	peerTimeout time.Duration
}

func newListenerWatch(h *listenerHeader, peerTimeout time.Duration) listenerWatch {
	return listenerWatch{
		h:           h,
		creator:     atomic.LoadUint64(&h.creatorToken),
		epoch:       atomic.LoadUint64(&h.epoch),
		token:       atomic.LoadUint64(&h.serverToken),
		hb:          atomic.LoadUint64(&h.serverHeartbeat),
		since:       time.Now(),
		peerTimeout: peerTimeout,
	}
}

// check returns nil while the acceptor that the watch started with is
// alive, ErrListenerClosed after it closed the listener, and ErrListenerGone
// if the listener was re-initialized or the acceptor stopped heartbeating.
func (w *listenerWatch) check() error {
	h := w.h
	if atomic.LoadUint64(&h.state) != listenerStateReady ||
		atomic.LoadUint64(&h.creatorToken) != w.creator ||
		atomic.LoadUint64(&h.epoch) != w.epoch {
		return ErrListenerGone
	}
	tok := atomic.LoadUint64(&h.serverToken)
	if tok == detachedToken || tok == 0 {
		return ErrListenerClosed
	}
	if tok != w.token {
		return ErrListenerGone
	}
	interval := time.Duration(atomic.LoadUint64(&h.serverInterval))
	if interval <= 0 || interval > time.Hour {
		return nil
	}
	now := time.Now()
	if hb := atomic.LoadUint64(&h.serverHeartbeat); hb != w.hb {
		w.hb, w.since = hb, now
		return nil
	}
	if now.Sub(w.since) > max(4*interval, w.peerTimeout) {
		return ErrListenerGone
	}
	return nil
}

// openListener maps the listener segment name and attaches to its ring. It
// never modifies the header. The errors that mean "the listener is not
// there yet" (see connectRetryable) are transient.
func openListener(ctx context.Context, name string, opts LinkOptions) (*listenerClient, error) {
	smem, err := shm.OpenSharedMemory(name, listenerHeaderSize, os.O_RDWR, 0)
	if err != nil {
		return nil, err
	}
	m := &namedMapping{name: name, sharedMemory: smem, parkers: ringParkers(smem)}
	ok := false
	defer func() {
		if !ok {
			_ = m.release()
		}
	}()

	cur, err := smem.CurrentSize()
	switch {
	case err != nil:
		return nil, err
	case cur == 0:
		return nil, errSegmentNotReady
	case cur < listenerHeaderSize:
		return nil, ErrIncompatibleLayout
	}

	// Map the first page to read the layout, then the whole segment.
	page, err := mmap.Map(smem.FD(), 0, int(pagesize), mmap.PROT_READ|mmap.PROT_WRITE, mmap.MAP_SHARED)
	if err != nil {
		return nil, err
	}
	layoutSize, err := inspectListener(unsafe.Pointer(unsafe.SliceData(page)), page, cur)
	if err != nil {
		_ = mmap.UnMap(page)
		return nil, err
	}
	if err := mmap.UnMap(page); err != nil {
		return nil, err
	}
	mapped, err := mmap.Map(smem.FD(), 0, int(layoutSize), mmap.PROT_READ|mmap.PROT_WRITE, mmap.MAP_SHARED)
	if err != nil {
		return nil, err
	}
	m.mapping = mapped
	base := unsafe.Pointer(unsafe.SliceData(mapped))
	if uintptr(base)%pagesize != 0 {
		return nil, ErrMemoryAlign
	}
	// The listener may have been replaced between the two mappings.
	if got, err := inspectListener(base, mapped, cur); err != nil || got != layoutSize {
		if err == nil {
			err = errListenerNotReady
		}
		return nil, err
	}
	h := (*listenerHeader)(base)

	alive, err := listenerServerAlive(ctx, h, opts)
	if err != nil {
		return nil, err
	}
	if !alive {
		return nil, errListenerDead
	}

	ring := mpmc.Attach[connectRecord](unsafe.Add(base, listenerRingOffset), 50*time.Millisecond)
	if ring == nil {
		return nil, ErrIncompatibleLayout
	}
	ring.SetWaitPolicy(opts.WaitPolicy.ring())
	if m.parkers[0] != nil {
		ring.SetParker(m.parkers[0])
	}

	plen := atomic.LoadUint64(&h.prefixLen)
	if plen == 0 || plen > listenerMaxPrefixLen {
		return nil, ErrIncompatibleLayout
	}
	lc := &listenerClient{
		m:              m,
		h:              h,
		ring:           ring,
		prefix:         string(h.prefix[:plen]),
		watch:          newListenerWatch(h, opts.PeerTimeout),
		maxBufferCount: atomic.LoadUint64(&h.maxBufferCount),
		maxBufferSize:  atomic.LoadUint64(&h.maxBufferSize),
		maxLinkSize:    atomic.LoadUint64(&h.maxLinkSize),
	}
	ok = true
	return lc, nil
}

// inspectListener validates the header of a listener segment at base (mem
// is the mapping that holds it, cur the size of the object) and returns the
// size of the segment. A listener that is not Ready yet is a transient
// errListenerNotReady; anything that is not a listener of this layout is
// ErrIncompatibleLayout.
func inspectListener(base unsafe.Pointer, mem []byte, cur int64) (uintptr, error) {
	h := (*listenerHeader)(base)
	switch atomic.LoadUint64(&h.state) {
	case listenerStateEmpty:
		if !listenerHeaderZero(mem) {
			return 0, ErrIncompatibleLayout
		}
		return 0, errListenerNotReady
	case listenerStateInitializing:
		if err := checkListenerCompat(h, true); err != nil {
			return 0, err
		}
		return 0, errListenerNotReady
	case listenerStateReady:
	default:
		return 0, ErrIncompatibleLayout
	}
	if err := checkListenerCompat(h, false); err != nil {
		return 0, err
	}
	capacity := atomic.LoadUint64(&h.capacity)
	if capacity > maxAcceptorQueueSize ||
		atomic.LoadUint64(&h.ringOffset) != listenerRingOffset ||
		atomic.LoadUint64(&h.recordSize) != listenerRecordSize {
		return 0, ErrIncompatibleLayout
	}
	size := listenerLayoutSize(int(capacity))
	if size == 0 || atomic.LoadUint64(&h.layoutSize) != uint64(size) {
		return 0, ErrIncompatibleLayout
	}
	if cur < int64(size) {
		return 0, ErrIncompatibleLayout
	}
	return size, nil
}

// admits checks the geometry of cfg against the limits the acceptor
// published, so that a request that would be dropped fails at once.
func (lc *listenerClient) admits(cfg Config) error {
	if !validConfig(cfg.BufferCount, cfg.BufferSize) {
		return ErrInvalidSize
	}
	layout, _ := computeLayout(cfg.BufferCount, cfg.BufferSize)
	if uint64(cfg.BufferCount) > lc.maxBufferCount || uint64(cfg.BufferSize) > lc.maxBufferSize ||
		uint64(layout.total) > lc.maxLinkSize {
		return fmt.Errorf("%w: the acceptor accepts at most %d buffers of %d bytes in a segment of %d bytes",
			ErrInvalidSize, lc.maxBufferCount, lc.maxBufferSize, lc.maxLinkSize)
	}
	return nil
}

// Connect asks the acceptor of the listener segment listenerName for a link
// and returns it, negotiated, as the Primary endpoint. cfg configures the
// link like Listen and Dial do; BufferCount and BufferSize are chosen by the
// client (within the limits of the acceptor, see AcceptorConfig), and Perm
// is the permission of the link segment, which the acceptor must be able to
// open. Closing the returned link removes its segment.
//
// Like Dial, Connect retries while the listener does not exist yet, is not
// initialized yet or has no live acceptor (until a new acceptor reuses the
// name), until ctx is done. Once the request is queued, Connect no longer
// retries: it fails with ErrListenerClosed if the acceptor closes the
// listener, with ErrListenerGone if the acceptor stops heartbeating (within
// Config.Advanced.Link.PeerTimeout, at least four heartbeat intervals) or is
// replaced, and with an error matching ErrTimeout if the acceptor did not
// attach within Config.Advanced.Link.AttachTimeout (default
// DefaultAttachTimeout) after the request was queued, which also is what a
// request the acceptor dropped looks like. A request that waits for room in
// a full queue is bounded by ctx and by the same acceptor checks. In every
// failure case the client's link segment is removed again.
func Connect(ctx context.Context, listenerName string, cfg Config) (*NamedAdvancedLink, error) {
	ctx = orBackground(ctx)
	cfg = cfg.normalized()
	if !validConfig(cfg.BufferCount, cfg.BufferSize) {
		return nil, ErrInvalidSize
	}
	backoff := time.Millisecond
	for {
		link, err := connectOnce(ctx, listenerName, cfg)
		if err == nil {
			return link, nil
		}
		if !connectRetryable(err) {
			return nil, err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, 100*time.Millisecond)
	}
}

// connectRetryable reports whether a failed attempt may succeed later: the
// listener has not been created or published yet, or its acceptor is gone
// and a new acceptor may reuse the name. Every other error is permanent.
func connectRetryable(err error) bool {
	return errors.Is(err, os.ErrNotExist) ||
		errors.Is(err, errSegmentNotReady) ||
		errors.Is(err, errListenerNotReady) ||
		errors.Is(err, errListenerDead)
}

// connectPollInterval is how often Connect looks at the acceptor and at its
// link while it waits for the acceptor to attach.
const connectPollInterval = 10 * time.Millisecond

func connectOnce(ctx context.Context, listenerName string, cfg Config) (*NamedAdvancedLink, error) {
	lopts := cfg.Advanced.Link.normalized()
	lc, err := openListener(ctx, listenerName, lopts)
	if err != nil {
		return nil, err
	}
	defer lc.close()
	if err := lc.admits(cfg); err != nil {
		return nil, err
	}

	var (
		link *NamedAdvancedLink
		name string
	)
	for attempt := 0; ; attempt++ {
		name = newLinkName(lc.prefix)
		link, err = openNamedAdvanced(ctx, name, cfg.BufferCount, cfg.BufferSize, true, cfg.Perm, cfg.Advanced)
		if errors.Is(err, ErrLinkBusy) && attempt < 8 {
			continue // the random name collided with a live link
		}
		if err != nil {
			return nil, err
		}
		break
	}

	rec := connectRecord{
		clientToken: link.instance,
		bufferCount: uint64(cfg.BufferCount),
		bufferSize:  uint64(cfg.BufferSize),
		features:    cfg.Features,
		clientPID:   uint64(os.Getpid()),
	}
	if !rec.setName(name) {
		_ = link.Close()
		return nil, ErrInvalidName
	}
	c := mpmc.Cancel{Ctx: ctx, Check: lc.watch.check}
	err = lc.ring.EnqueueZeroCopyCancel(&c, func(_ uint64, e *connectRecord) { *e = rec })
	if err != nil {
		_ = link.Close()
		return nil, mapCtxErr(ctx, err)
	}

	// Wait for the acceptor to attach and answer the negotiation request,
	// which is already queued in the link. While nobody attached, the link
	// cannot tell a dead acceptor from a slow one: watch the listener.
	deadline := time.Now().Add(lopts.AttachTimeout)
	wctx, cancel := context.WithCancelCause(ctx)
	stop, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		t := time.NewTicker(connectPollInterval)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
			}
			if link.PeerState() != PeerNotConnected {
				return // the acceptor attached; the link monitors it from now on
			}
			if err := lc.watch.check(); err != nil {
				cancel(err)
				return
			}
			if time.Now().After(deadline) {
				cancel(connectTimeoutError(lopts.AttachTimeout))
				return
			}
		}
	}()
	err = link.Negotiate(wctx, cfg.Features)
	close(stop)
	<-done
	cause := context.Cause(wctx)
	cancel(nil)
	if err != nil {
		_ = link.Close()
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if cause != nil {
			return nil, cause
		}
		return nil, err
	}
	return link, nil
}
