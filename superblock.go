package hqq

import (
	"math/rand/v2"
	"os"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"

	"gosuda.org/hqq/internal/mpmc"
	"gosuda.org/hqq/internal/protocol"
	"gosuda.org/hqq/internal/race"
)

// pagesize stores the system page size for memory alignment.
var pagesize = uintptr(syscall.Getpagesize())

// Standard Link Memory Layout:
//
//	<<<< PAGE_START
//	SUPERBLOCK                           // metadata, init state, endpoint liveness
//	<<<< PAGE_BREAK
//	MPMC_RING (Primary to Secondary)     // packet ring, direction 0
//	<<<< PAGE_BREAK
//	BUFFERS (Primary to Secondary)       // slot-owned payload buffers, direction 0
//	<<<< PAGE_BREAK
//	MPMC_RING (Secondary to Primary)     // packet ring, direction 1
//	<<<< PAGE_BREAK
//	BUFFERS (Secondary to Primary)       // slot-owned payload buffers, direction 1
//	<<<< PAGE_END

const (
	// superblockMagic identifies layout version 2 ("HQQ_STL2"): endpoint
	// liveness records and the cache-aligned ring layout.
	superblockMagic uint64 = 0x4851515f53544c32

	superblockStateEmpty        uint64 = 0
	superblockStateInitializing uint64 = 1
	superblockStateReady        uint64 = 2

	endpointPrimary   = 0
	endpointSecondary = 1

	// zeroChunk bounds the work between initializer progress updates.
	zeroChunk = 8 << 20

	// detachedToken marks an endpoint slot whose owner closed gracefully.
	// It persists until the segment is re-initialized, so a peer that
	// attached and detached before the other side ever looked is still
	// reported as closed, and a new endpoint cannot join a session that
	// already ended.
	detachedToken uint64 = 1
)

// standardSuperblock is the first page of a StandardLink segment. Every field
// is accessed atomically. Field order is part of the shared-memory ABI.
type standardSuperblock struct {
	magic         uint64
	state         uint64
	major         uint64
	minor         uint64
	pageSize      uint64
	bufferCount   uint64
	bufferSize    uint64
	layoutSize    uint64
	epoch         uint64 // incremented on every (re)initialization
	initToken     uint64 // token of the endpoint currently initializing
	initHeartbeat uint64 // unix nanos of the initializer's last progress
	_             [5]uint64

	endpoints [2]endpointRecord // [0] primary, [1] secondary
}

// endpointRecord describes one endpoint slot. Token 0 means the slot was never
// used since initialization, detachedToken means its owner closed, and any
// other value is the owner token of an attached endpoint.
type endpointRecord struct {
	token     uint64 // random non-zero owner token
	pid       uint64 // diagnostic only (PIDs are not comparable across namespaces)
	heartbeat uint64 // unix nanos, refreshed every interval
	interval  uint64 // heartbeat interval in nanoseconds; 0 = heartbeats disabled
	_         [12]uint64
}

var _ [4096 - unsafe.Sizeof(standardSuperblock{})]struct{} // must fit in the smallest page

// SizeStandardLink calculates the required memory size for a standard link.
// It returns zero for invalid configurations: bufferCount must be a power of
// two and at least MinBufferCount; bufferSize must be at least MinBufferSize
// and a multiple of BufferSizeAlignment.
func SizeStandardLink(bufferCount int, bufferSize int) uintptr {
	if !validConfig(bufferCount, bufferSize) {
		return 0
	}
	dataRingSize := mpmc.SizeMPMCRing[protocol.Packet](uintptr(bufferCount))
	buffersSize := uintptr(bufferSize) * uintptr(bufferCount)

	var size uintptr
	size = alignPage(size + unsafe.Sizeof(standardSuperblock{}))
	size = alignPage(size + dataRingSize)
	size = alignPage(size + buffersSize)
	size = alignPage(size + dataRingSize)
	size = alignPage(size + buffersSize)
	return size
}

func validConfig(bufferCount, bufferSize int) bool {
	return isPowerOfTwo(bufferCount) &&
		bufferCount >= MinBufferCount &&
		bufferSize >= MinBufferSize &&
		bufferSize%BufferSizeAlignment == 0
}

func alignPage(size uintptr) uintptr {
	return ((size + pagesize - 1) / pagesize) * pagesize
}

func isPowerOfTwo(v int) bool {
	return v > 0 && v&(v-1) == 0
}

func newToken() uint64 {
	for {
		if t := rand.Uint64(); t != 0 && t != detachedToken {
			return t
		}
	}
}

func isOwnerToken(t uint64) bool {
	return t != 0 && t != detachedToken
}

func nowNanos() uint64 {
	return uint64(time.Now().UnixNano())
}

// wallAge returns how long ago the unix-nano timestamp ts was taken. It is
// negative if the wall clock moved backwards.
func wallAge(ts uint64) time.Duration {
	return time.Since(time.Unix(0, int64(ts)))
}

// effectivePeerTimeout never declares a peer dead before it missed several
// of its own heartbeats.
func effectivePeerTimeout(opts LinkOptions, interval time.Duration) time.Duration {
	if t := 4 * interval; t > opts.PeerTimeout {
		return t
	}
	return opts.PeerTimeout
}

// endpointAliveAtOpen decides whether an endpoint record belongs to a live
// endpoint. The fast path trusts a recent wall-clock heartbeat; a heartbeat
// that looks stale is confirmed by watching for progress for a few intervals,
// which is robust against wall-clock jumps.
func endpointAliveAtOpen(rec *endpointRecord, opts LinkOptions) bool {
	tok := atomic.LoadUint64(&rec.token)
	if !isOwnerToken(tok) {
		return false
	}
	interval := time.Duration(atomic.LoadUint64(&rec.interval))
	if interval <= 0 {
		// Heartbeats disabled: liveness cannot be judged, assume alive.
		return true
	}
	hb := atomic.LoadUint64(&rec.heartbeat)
	if hb == 0 {
		// Attached a moment ago; first heartbeat not written yet.
		return true
	}
	if wallAge(hb) < effectivePeerTimeout(opts, interval) {
		return true
	}
	window := min(3*interval, 2*time.Second)
	step := max(interval/4, time.Millisecond)
	for end := time.Now().Add(window); time.Now().Before(end); {
		time.Sleep(step)
		if atomic.LoadUint64(&rec.heartbeat) != hb || atomic.LoadUint64(&rec.token) != tok {
			return isOwnerToken(atomic.LoadUint64(&rec.token))
		}
	}
	return false
}

func clearEndpoint(rec *endpointRecord) {
	atomic.StoreUint64(&rec.token, 0)
	atomic.StoreUint64(&rec.pid, 0)
	atomic.StoreUint64(&rec.heartbeat, 0)
	atomic.StoreUint64(&rec.interval, 0)
}

func fillEndpoint(rec *endpointRecord, opts LinkOptions) {
	atomic.StoreUint64(&rec.pid, uint64(os.Getpid()))
	atomic.StoreUint64(&rec.interval, uint64(opts.HeartbeatInterval))
	atomic.StoreUint64(&rec.heartbeat, nowNanos())
}

// claimSuperblock decides the role of a new endpoint. It returns
// mustInit == true when the caller won the right to (re)initialize the
// segment and must call publishSuperblock afterwards.
//
//go:nocheckptr
func claimSuperblock(sb *standardSuperblock, bufferCount, bufferSize int, layoutSize uintptr, token uint64, opts LinkOptions) (mode LinkMode, mustInit bool, err error) {
	deadline := time.Now().Add(opts.AttachTimeout)
	staleInit := max(opts.PeerTimeout, 2*time.Second)
	for {
		switch state := atomic.LoadUint64(&sb.state); state {
		case superblockStateEmpty:
			if atomic.CompareAndSwapUint64(&sb.state, superblockStateEmpty, superblockStateInitializing) {
				beginInit(sb, token)
				return LinkModePrimary, true, nil
			}

		case superblockStateInitializing:
			// Take over from an initializer that stopped making progress
			// (it crashed while formatting the segment).
			tok := atomic.LoadUint64(&sb.initToken)
			hb := atomic.LoadUint64(&sb.initHeartbeat)
			if tok != 0 && hb != 0 && wallAge(hb) > staleInit &&
				atomic.CompareAndSwapUint64(&sb.initToken, tok, token) {
				atomic.StoreUint64(&sb.initHeartbeat, nowNanos())
				return LinkModePrimary, true, nil
			}

		case superblockStateReady:
			if atomic.LoadUint64(&sb.magic) != superblockMagic {
				return 0, false, ErrIncompatibleLayout
			}
			primaryAlive := endpointAliveAtOpen(&sb.endpoints[endpointPrimary], opts)
			secondaryAlive := endpointAliveAtOpen(&sb.endpoints[endpointSecondary], opts)
			if !primaryAlive && !secondaryAlive {
				// Both endpoints closed or died: the segment is reusable.
				if atomic.CompareAndSwapUint64(&sb.state, superblockStateReady, superblockStateInitializing) {
					beginInit(sb, token)
					return LinkModePrimary, true, nil
				}
				continue
			}
			if atomic.LoadUint64(&sb.bufferCount) != uint64(bufferCount) ||
				atomic.LoadUint64(&sb.bufferSize) != uint64(bufferSize) ||
				atomic.LoadUint64(&sb.layoutSize) != uint64(layoutSize) ||
				atomic.LoadUint64(&sb.pageSize) != uint64(pagesize) {
				return 0, false, ErrInvalidSize
			}
			sec := &sb.endpoints[endpointSecondary]
			if primaryAlive && atomic.LoadUint64(&sec.token) == 0 {
				if atomic.CompareAndSwapUint64(&sec.token, 0, token) {
					fillEndpoint(sec, opts)
					return LinkModeSecondary, false, nil
				}
				continue
			}
			// Either both endpoints are alive, or the session of the live
			// endpoint already had a peer that closed or died (a dead
			// endpoint may still own ring slots). The segment can only be
			// reused after the surviving endpoint closes.
			return 0, false, ErrLinkBusy

		default:
			return 0, false, ErrIncompatibleLayout
		}

		if time.Now().After(deadline) {
			return 0, false, errInitContended
		}
		time.Sleep(200 * time.Microsecond)
	}
}

func beginInit(sb *standardSuperblock, token uint64) {
	atomic.StoreUint64(&sb.initToken, token)
	atomic.StoreUint64(&sb.initHeartbeat, nowNanos())
}

// zeroWithProgress clears b in chunks and records initializer progress so
// that a slow but alive initializer is never taken over.
func zeroWithProgress(sb *standardSuperblock, b []byte) {
	for len(b) > 0 {
		n := min(len(b), zeroChunk)
		race.Zero(b[:n])
		b = b[n:]
		atomic.StoreUint64(&sb.initHeartbeat, nowNanos())
	}
}

// publishSuperblock finishes initialization and makes the segment Ready with
// the caller as the live primary endpoint. It returns the new epoch.
func publishSuperblock(sb *standardSuperblock, bufferCount, bufferSize int, layoutSize uintptr, token uint64, opts LinkOptions) (uint64, error) {
	if atomic.LoadUint64(&sb.initToken) != token {
		// Another endpoint took over initialization.
		return 0, errInitContended
	}
	epoch := atomic.AddUint64(&sb.epoch, 1)
	atomic.StoreUint64(&sb.major, protocol.HQQProtocolMajorVersion)
	atomic.StoreUint64(&sb.minor, protocol.HQQProtocolMinorVersion)
	atomic.StoreUint64(&sb.pageSize, uint64(pagesize))
	atomic.StoreUint64(&sb.bufferCount, uint64(bufferCount))
	atomic.StoreUint64(&sb.bufferSize, uint64(bufferSize))
	atomic.StoreUint64(&sb.layoutSize, uint64(layoutSize))

	clearEndpoint(&sb.endpoints[endpointSecondary])
	prim := &sb.endpoints[endpointPrimary]
	atomic.StoreUint64(&prim.token, token)
	fillEndpoint(prim, opts)

	atomic.StoreUint64(&sb.magic, superblockMagic)
	atomic.StoreUint64(&sb.initToken, 0)
	atomic.StoreUint64(&sb.state, superblockStateReady)
	return epoch, nil
}

// PeerState describes what an endpoint knows about its peer.
type PeerState uint32

const (
	// PeerNotConnected: no peer has attached to the segment yet.
	PeerNotConnected PeerState = iota
	// PeerConnected: a peer is attached and its heartbeat (if enabled) is
	// current.
	PeerConnected
	// PeerClosed: the peer detached gracefully with Close.
	PeerClosed
	// PeerGone: the peer stopped heartbeating, was replaced, or the segment
	// was re-initialized under this endpoint.
	PeerGone
)

// String returns a human-readable peer state.
func (s PeerState) String() string {
	switch s {
	case PeerNotConnected:
		return "not-connected"
	case PeerConnected:
		return "connected"
	case PeerClosed:
		return "closed"
	case PeerGone:
		return "gone"
	default:
		return "unknown"
	}
}

// peerMonitor tracks the peer endpoint record using the local monotonic
// clock, so wall-clock jumps cannot fake a dead peer.
type peerMonitor struct {
	status atomic.Uint32 // PeerState

	mu         sync.Mutex
	token      uint64
	lastHB     uint64
	lastChange time.Time
}

func (s PeerState) err() error {
	switch s {
	case PeerClosed:
		return ErrPeerClosed
	case PeerGone:
		return ErrPeerGone
	default:
		return nil
	}
}

// heartbeatLoop refreshes this endpoint's heartbeat and watches the peer
// until hbStop is closed. It keeps heartbeating after a peer failure: this
// endpoint stays attached (and must not look stale) until it is closed.
func (l *StandardLink) heartbeatLoop(interval time.Duration) {
	defer close(l.hbDone)
	t := time.NewTicker(interval)
	defer t.Stop()
	self := &l.sb.endpoints[l.self]
	woken := false
	for {
		select {
		case <-l.hbStop:
			return
		case <-t.C:
			if atomic.LoadUint64(&self.token) == l.token {
				atomic.StoreUint64(&self.heartbeat, nowNanos())
			}
			if err := l.checkPeer(); err != nil && !woken {
				// Wake blocked calls of this endpoint so they report the
				// peer failure without waiting for their next check.
				woken = true
				l.ring0.WakeAll()
				l.ring1.WakeAll()
			}
		}
	}
}

// checkPeer updates and returns the peer status as an error (nil while the
// peer is not connected yet or alive).
func (l *StandardLink) checkPeer() error {
	if err := PeerState(l.peer.status.Load()).err(); err != nil {
		return err
	}
	m := &l.peer
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := PeerState(m.status.Load()).err(); err != nil {
		return err
	}

	// The segment was re-initialized, or this endpoint was replaced: this
	// link no longer owns its role.
	if atomic.LoadUint64(&l.sb.epoch) != l.epoch ||
		atomic.LoadUint64(&l.sb.endpoints[l.self].token) != l.token {
		m.status.Store(uint32(PeerGone))
		return ErrPeerGone
	}

	rec := &l.sb.endpoints[1-l.self]
	tok := atomic.LoadUint64(&rec.token)
	now := time.Now()
	if tok == detachedToken {
		m.status.Store(uint32(PeerClosed))
		return ErrPeerClosed
	}
	if m.token == 0 {
		if tok != 0 {
			m.token = tok
			m.lastHB = atomic.LoadUint64(&rec.heartbeat)
			m.lastChange = now
			m.status.Store(uint32(PeerConnected))
		}
		return nil
	}
	if tok != m.token {
		m.status.Store(uint32(PeerGone))
		return ErrPeerGone
	}
	interval := time.Duration(atomic.LoadUint64(&rec.interval))
	if interval <= 0 {
		return nil
	}
	if hb := atomic.LoadUint64(&rec.heartbeat); hb != m.lastHB {
		m.lastHB = hb
		m.lastChange = now
		return nil
	}
	if now.Sub(m.lastChange) > effectivePeerTimeout(l.opts, interval) {
		m.status.Store(uint32(PeerGone))
		return ErrPeerGone
	}
	return nil
}
