package hqq

import (
	"context"
	"math"
	"math/bits"
	"math/rand/v2"
	"os"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"

	"gosuda.org/hqq/internal/mpmc"
	"gosuda.org/hqq/internal/protocol"
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
	// superblockMagic identifies layout version 3 ("HQQ_STL3"). A segment
	// with another magic, for example "HQQ_STL2", is ErrIncompatibleLayout.
	superblockMagic uint64 = 0x4851515f53544c33

	// superblockMajor and superblockMinor are the layout version stored in
	// the major and minor fields. A different major version is
	// incompatible; the minor version is informational.
	superblockMajor uint64 = 3
	superblockMinor uint64 = 0

	// knownRequiredFeatures is the set of requiredFeatures bits this build
	// understands. No layout feature is defined yet.
	knownRequiredFeatures uint64 = 0

	superblockStateEmpty        uint64 = 0
	superblockStateInitializing uint64 = 1
	superblockStateReady        uint64 = 2

	endpointPrimary   = 0
	endpointSecondary = 1

	// zeroChunk is the largest amount of buffer memory the initializer
	// zeroes between two progress updates (the protocol bound).
	zeroChunk = 8 << 20

	// zeroSlice is the amount of buffer memory zeroed between two ownership
	// checks. It bounds what an initializer that was suspended between a
	// check and the write can still clear in a segment it no longer owns.
	zeroSlice = 256 << 10

	// detachedToken marks an endpoint slot whose owner closed gracefully.
	// It persists until the segment is re-initialized, so a peer that
	// attached and detached before the other side ever looked is still
	// reported as closed, and a new endpoint cannot join a session that
	// already ended.
	detachedToken uint64 = 1

	// Bounds of a supported configuration. They keep every size
	// computation far from overflow on 64-bit platforms; on 32-bit
	// platforms the total layout size is additionally checked against the
	// address space.
	maxBufferCount = 1 << 24
	maxBufferSize  = 1 << 30
)

// standardSuperblock is the first page of a StandardLink segment. Every field
// is accessed atomically. Field order is part of the shared-memory ABI.
type standardSuperblock struct {
	magic            uint64 // @0
	state            uint64 // @8
	major            uint64 // @16 layout major version (superblockMajor)
	minor            uint64 // @24 layout minor version (superblockMinor)
	pageSize         uint64 // @32
	bufferCount      uint64 // @40
	bufferSize       uint64 // @48
	layoutSize       uint64 // @56
	epoch            uint64 // @64 incremented on every (re)initialization
	initToken        uint64 // @72 token of the endpoint currently initializing
	initHeartbeat    uint64 // @80 unix nanos of the initializer's last progress
	requiredFeatures uint64 // @88 layout features a reader must understand
	creatorToken     uint64 // @96 identifies the instance that created the name
	_                [3]uint64

	endpoints [2]endpointRecord // @128: [0] primary, [1] secondary
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

// The superblock offsets are part of the shared-memory ABI.
const (
	_ = uint(unsafe.Offsetof(standardSuperblock{}.requiredFeatures) - 88)
	_ = uint(88 - unsafe.Offsetof(standardSuperblock{}.requiredFeatures))
	_ = uint(unsafe.Offsetof(standardSuperblock{}.creatorToken) - 96)
	_ = uint(96 - unsafe.Offsetof(standardSuperblock{}.creatorToken))
	_ = uint(unsafe.Offsetof(standardSuperblock{}.endpoints) - 128)
	_ = uint(128 - unsafe.Offsetof(standardSuperblock{}.endpoints))
)

// SizeStandardLink calculates the required memory size for a standard link.
// It returns zero for invalid configurations: bufferCount must be a power of
// two between MinBufferCount and 1<<24; bufferSize must be a multiple of
// BufferSizeAlignment between MinBufferSize and 1<<30; and the resulting
// size must be addressable (it must not overflow uintptr or int, which only
// matters on 32-bit platforms).
func SizeStandardLink(bufferCount int, bufferSize int) uintptr {
	l, ok := computeLayout(bufferCount, bufferSize)
	if !ok {
		return 0
	}
	return l.total
}

func validConfig(bufferCount, bufferSize int) bool {
	return isPowerOfTwo(bufferCount) &&
		bufferCount >= MinBufferCount &&
		bufferCount <= maxBufferCount &&
		bufferSize >= MinBufferSize &&
		bufferSize <= maxBufferSize &&
		bufferSize%BufferSizeAlignment == 0
}

// segmentLayout holds the byte offsets of the regions after the superblock
// and the total size of a segment.
type segmentLayout struct {
	ring0, buf0, ring1, buf1 uintptr
	total                    uintptr
}

// computeLayout returns the layout for a configuration. ok is false if the
// configuration is invalid or its size does not fit in uintptr and int.
func computeLayout(bufferCount, bufferSize int) (l segmentLayout, ok bool) {
	if !validConfig(bufferCount, bufferSize) {
		return l, false
	}
	ringSize := mpmc.SizeMPMCRing[protocol.Packet](uintptr(bufferCount))
	if ringSize == 0 {
		return l, false
	}
	hi, buffersSize := bits.Mul(uint(bufferCount), uint(bufferSize))
	if hi != 0 || buffersSize > math.MaxInt {
		return l, false
	}
	l.ring0 = alignPage(unsafe.Sizeof(standardSuperblock{}))
	if l.buf0, ok = alignedSum(l.ring0, ringSize); !ok {
		return l, false
	}
	if l.ring1, ok = alignedSum(l.buf0, uintptr(buffersSize)); !ok {
		return l, false
	}
	if l.buf1, ok = alignedSum(l.ring1, ringSize); !ok {
		return l, false
	}
	if l.total, ok = alignedSum(l.buf1, uintptr(buffersSize)); !ok {
		return l, false
	}
	return l, l.total <= math.MaxInt
}

// alignedSum returns a+b rounded up to a page boundary, or ok == false if
// the result does not fit in uintptr.
func alignedSum(a, b uintptr) (sum uintptr, ok bool) {
	s, carry := bits.Add(uint(a), uint(b), 0)
	if carry != 0 {
		return 0, false
	}
	s, carry = bits.Add(s, uint(pagesize-1), 0)
	if carry != 0 {
		return 0, false
	}
	return uintptr(s) / pagesize * pagesize, true
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

// heartbeatFutureSlack bounds how far in the future a heartbeat timestamp may
// lie and still count as fresh. A larger lead means the wall clock was stepped
// backwards after the timestamp was written (or the writer's clock runs
// ahead); such a timestamp says nothing about liveness, so it is confirmed by
// the progress watch like a stale one.
const heartbeatFutureSlack = time.Second

// heartbeatFresh reports whether the wall-clock heartbeat ts is younger than
// timeout and not further in the future than heartbeatFutureSlack.
func heartbeatFresh(ts uint64, timeout time.Duration) bool {
	age := wallAge(ts)
	return age > -heartbeatFutureSlack && age < timeout
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
// which is robust against wall-clock jumps. The watch ends early with
// ctx.Err() when ctx is done.
func endpointAliveAtOpen(ctx context.Context, rec *endpointRecord, opts LinkOptions) (bool, error) {
	tok := atomic.LoadUint64(&rec.token)
	if !isOwnerToken(tok) {
		return false, nil
	}
	interval := time.Duration(atomic.LoadUint64(&rec.interval))
	if interval <= 0 {
		// Heartbeats disabled: liveness cannot be judged, assume alive.
		return true, nil
	}
	hb := atomic.LoadUint64(&rec.heartbeat)
	if hb == 0 {
		// Attached a moment ago; first heartbeat not written yet.
		return true, nil
	}
	if heartbeatFresh(hb, effectivePeerTimeout(opts, interval)) {
		return true, nil
	}
	window := min(3*interval, 2*time.Second)
	step := max(interval/4, time.Millisecond)
	for end := time.Now().Add(window); time.Now().Before(end); {
		if err := sleepCtx(ctx, step); err != nil {
			return false, err
		}
		if atomic.LoadUint64(&rec.heartbeat) != hb || atomic.LoadUint64(&rec.token) != tok {
			return isOwnerToken(atomic.LoadUint64(&rec.token)), nil
		}
	}
	return false, nil
}

// sleepCtx sleeps for d or until ctx is done, in which case it returns
// ctx.Err().
func sleepCtx(ctx context.Context, d time.Duration) error {
	if ctx.Done() == nil {
		time.Sleep(d)
		return nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
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

// attachRole tells claimSuperblock how an endpoint reached the segment.
type attachRole uint8

const (
	// roleAny is an endpoint on caller-owned memory: it may initialize an
	// empty segment or join one as the Secondary.
	roleAny attachRole = iota

	// roleCreator created the name (or is reusing a name whose endpoints are
	// all gone). It initializes an empty segment and re-initializes a dead
	// one, but never joins a live one: it gets ErrLinkBusy.
	roleCreator

	// roleOpener opened an existing name. It only ever joins a live segment
	// as the Secondary and never initializes one: an empty or initializing
	// segment is waited for (until AttachTimeout), and a segment whose
	// endpoints are all gone fails with errNoLiveEndpoint. Recovering an
	// abandoned name is the creator's job (Create*/Listen reuse it), so an
	// opener can never end up as a Primary that nobody will ever join.
	roleOpener
)

// claimPollInterval is how long claimSuperblock sleeps between looks at a
// segment that another endpoint is still working on.
const claimPollInterval = 200 * time.Microsecond

// initWatch records what an endpoint last saw of another endpoint's
// initialization, measured on the local monotonic clock.
type initWatch struct {
	active bool
	tok    uint64
	hb     uint64
	since  time.Time
}

// initStaleWindow is how long an initializer may show no progress before
// another endpoint takes the segment over. Progress is a change of
// initToken or initHeartbeat as observed locally, never a wall-clock age.
func (o LinkOptions) initStaleWindow() time.Duration {
	if o.initStale > 0 {
		return o.initStale
	}
	return max(o.PeerTimeout, 2*time.Second)
}

// firstPageZero reports whether the whole superblock page is zero. Words are
// read atomically because endpoints that legitimately claimed the segment
// write it concurrently.
//
//go:nocheckptr
func firstPageZero(sb *standardSuperblock) bool {
	base := unsafe.Pointer(sb)
	for off := uintptr(0); off < pagesize; off += 8 {
		if atomic.LoadUint64((*uint64)(unsafe.Add(base, off))) != 0 {
			return false
		}
	}
	return true
}

// checkLayoutCompat returns ErrIncompatibleLayout if the superblock was
// written by an incompatible layout version. It must pass before an endpoint
// joins a segment or takes one over.
//
// A segment in state Initializing may carry no layout yet (its initializer
// died right after claiming it), so only a conflicting layout is rejected
// there.
func checkLayoutCompat(sb *standardSuperblock, initializing bool) error {
	if atomic.LoadUint64(&sb.requiredFeatures)&^knownRequiredFeatures != 0 {
		return ErrIncompatibleLayout
	}
	magic := atomic.LoadUint64(&sb.magic)
	major := atomic.LoadUint64(&sb.major)
	if !initializing {
		if magic != superblockMagic || major != superblockMajor {
			return ErrIncompatibleLayout
		}
		return nil
	}
	if magic != 0 && magic != superblockMagic {
		return ErrIncompatibleLayout
	}
	if major != superblockMajor && (major != 0 || atomic.LoadUint64(&sb.initToken) != 0) {
		return ErrIncompatibleLayout
	}
	return nil
}

// claimSuperblock decides the role of a new endpoint. It returns
// mustInit == true when the caller won the right to (re)initialize the
// segment and must call publishSuperblock afterwards.
//
// opts.ctx (if set) bounds every wait: the function returns ctx.Err() as
// soon as the context is done. opts.role selects the behavior toward an
// empty segment and toward live endpoints, see attachRole.
//
//go:nocheckptr
func claimSuperblock(sb *standardSuperblock, bufferCount, bufferSize int, layoutSize uintptr, token uint64, opts LinkOptions) (mode LinkMode, mustInit bool, err error) {
	ctx := orBackground(opts.ctx)
	deadline := time.Now().Add(opts.AttachTimeout)
	staleInit := opts.initStaleWindow()
	var (
		watch      initWatch
		emptySince time.Time
	)
	for {
		if err := ctx.Err(); err != nil {
			return 0, false, err
		}
		state := atomic.LoadUint64(&sb.state)
		if state != superblockStateInitializing {
			watch.active = false
		}
		if state != superblockStateEmpty {
			emptySince = time.Time{}
		}
		stalling, emptyWait := false, false

		switch state {
		case superblockStateEmpty:
			if !firstPageZero(sb) {
				if atomic.LoadUint64(&sb.state) != superblockStateEmpty {
					continue // another endpoint claimed it meanwhile
				}
				// Not a segment of ours: leave foreign data alone.
				return 0, false, ErrIncompatibleLayout
			}
			if opts.role == roleOpener {
				// The creator has not claimed the segment yet. Wait for it;
				// if it never does (it died between creating the name and
				// claiming the segment), a new creator reuses the name.
				if emptySince.IsZero() {
					emptySince = time.Now()
				}
				if time.Since(emptySince) >= opts.AttachTimeout {
					return 0, false, errSegmentNotReady
				}
				emptyWait = true
				break
			}
			if atomic.CompareAndSwapUint64(&sb.state, superblockStateEmpty, superblockStateInitializing) {
				if !beginInit(sb, token, atomic.LoadUint64(&sb.initToken), opts) {
					return 0, false, errInitContended
				}
				return LinkModePrimary, true, nil
			}

		case superblockStateInitializing:
			tok := atomic.LoadUint64(&sb.initToken)
			hb := atomic.LoadUint64(&sb.initHeartbeat)
			if err := checkLayoutCompat(sb, true); err != nil {
				return 0, false, err
			}
			if opts.role == roleOpener {
				// Only creators and in-memory endpoints take over a stalled
				// initializer; an opener waits until AttachTimeout.
				break
			}
			// Take over from an initializer that stopped making progress
			// (it crashed while claiming or formatting the segment): neither
			// its token nor its heartbeat changed for the whole window.
			now := time.Now()
			if !watch.active || watch.tok != tok || watch.hb != hb {
				watch = initWatch{active: true, tok: tok, hb: hb, since: now}
			} else if now.Sub(watch.since) >= staleInit {
				if beginInit(sb, token, tok, opts) {
					return LinkModePrimary, true, nil
				}
				watch.active = false // somebody else moved first
			}
			stalling = true

		case superblockStateReady:
			if err := checkLayoutCompat(sb, false); err != nil {
				return 0, false, err
			}
			primaryAlive, err := endpointAliveAtOpen(ctx, &sb.endpoints[endpointPrimary], opts)
			if err != nil {
				return 0, false, err
			}
			secondaryAlive, err := endpointAliveAtOpen(ctx, &sb.endpoints[endpointSecondary], opts)
			if err != nil {
				return 0, false, err
			}
			if !primaryAlive && !secondaryAlive {
				if opts.role == roleOpener {
					// Nobody is left to talk to; a new creator may reuse the
					// segment later.
					return 0, false, errNoLiveEndpoint
				}
				// Both endpoints closed or died: the segment is reusable.
				if atomic.CompareAndSwapUint64(&sb.state, superblockStateReady, superblockStateInitializing) {
					if !beginInit(sb, token, atomic.LoadUint64(&sb.initToken), opts) {
						return 0, false, errInitContended
					}
					return LinkModePrimary, true, nil
				}
				continue
			}
			if opts.role == roleCreator {
				// A creator never joins a live segment.
				return 0, false, ErrLinkBusy
			}
			got := Geometry{
				BufferCount: int(atomic.LoadUint64(&sb.bufferCount)),
				BufferSize:  int(atomic.LoadUint64(&sb.bufferSize)),
				LayoutSize:  uintptr(atomic.LoadUint64(&sb.layoutSize)),
				PageSize:    uintptr(atomic.LoadUint64(&sb.pageSize)),
			}
			want := Geometry{BufferCount: bufferCount, BufferSize: bufferSize, LayoutSize: layoutSize, PageSize: pagesize}
			if got != want {
				return 0, false, &GeometryError{Want: want, Got: got}
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

		// A stalled initializer is given its whole window even if that runs
		// past AttachTimeout; so is a creator that has not claimed yet.
		limit := deadline
		if stalling && watch.active && watch.since.Before(deadline) {
			limit = later(deadline, watch.since.Add(staleInit))
		}
		if !emptyWait && time.Now().After(limit) {
			if stalling && watch.active && time.Since(watch.since) >= staleInit {
				// The window ended between the takeover check above and
				// this one: take over now instead of giving up.
				continue
			}
			return 0, false, errInitContended
		}
		if err := sleepCtx(ctx, claimPollInterval); err != nil {
			return 0, false, err
		}
	}
}

func later(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}

// stampLayout records the layout version and required features, so that a
// segment in state Initializing already identifies its layout.
func stampLayout(sb *standardSuperblock) {
	atomic.StoreUint64(&sb.major, superblockMajor)
	atomic.StoreUint64(&sb.minor, superblockMinor)
	atomic.StoreUint64(&sb.requiredFeatures, 0)
}

// beginInit makes token the initializer of the segment by replacing the
// initToken value expected that the caller saw (0 for a fresh claim). It
// reports false if initToken changed meanwhile, which means another
// endpoint took the initialization over. The new initializer also records
// opts.creatorToken (0 unless it created the name), so that an instance's
// identity changes with every initialization.
func beginInit(sb *standardSuperblock, token, expected uint64, opts LinkOptions) bool {
	stampLayout(sb)
	if !atomic.CompareAndSwapUint64(&sb.initToken, expected, token) {
		return false
	}
	atomic.StoreUint64(&sb.creatorToken, opts.creatorToken)
	atomic.StoreUint64(&sb.initHeartbeat, nowNanos())
	return true
}

// initSession is an endpoint's claim on the initialization of a segment.
type initSession struct {
	sb    *standardSuperblock
	token uint64
	ctx   context.Context
}

// check returns ctx.Err() if the context is done, and errInitContended if
// another endpoint took the initialization over.
func (s *initSession) check() error {
	if err := s.ctx.Err(); err != nil {
		return err
	}
	if atomic.LoadUint64(&s.sb.initToken) != s.token {
		return errInitContended
	}
	return nil
}

// progress records initializer progress so that a slow but alive initializer
// is never taken over. It does nothing once the initialization was lost.
func (s *initSession) progress() {
	if atomic.LoadUint64(&s.sb.initToken) == s.token {
		atomic.StoreUint64(&s.sb.initHeartbeat, nowNanos())
	}
}

// zero clears b in slices of zeroSlice bytes, checking ownership before every
// slice and refreshing the initializer heartbeat after every slice (so the
// time between two progress updates stays far below the protocol bound
// zeroChunk).
func (s *initSession) zero(b []byte) error {
	return zeroInChunks(b, zeroSlice, s.check, s.progress)
}

// A slice must not exceed the progress bound.
const _ = uint(zeroChunk - zeroSlice)

// zeroInChunks clears b in chunks of at most chunk bytes. check runs before
// every chunk and aborts the operation with its error; progress runs after
// every chunk.
func zeroInChunks(b []byte, chunk int, check func() error, progress func()) error {
	for len(b) > 0 {
		if err := check(); err != nil {
			return err
		}
		n := min(len(b), chunk)
		clear(b[:n])
		b = b[n:]
		progress()
	}
	return nil
}

// publishSuperblock finishes initialization and makes the segment Ready with
// the caller as the live primary endpoint. It returns the new epoch.
//
// check runs before every write step (SPEC 5.3), so an initializer that was
// suspended and lost ownership stops before its next store instead of after
// the whole publish (F-01). A check cannot be atomic with the store that
// follows it: the residual window is one step.
//
// All fields are written first; the initializer's ownership is then released
// with CAS(initToken, token, 0), which fails if another endpoint took the
// initialization over, in which case the segment is not marked Ready.
func publishSuperblock(sb *standardSuperblock, check func() error, bufferCount, bufferSize int, layoutSize uintptr, token uint64, opts LinkOptions) (uint64, error) {
	writes := [...]func(){
		func() { stampLayout(sb) },
		func() {
			atomic.StoreUint64(&sb.pageSize, uint64(pagesize))
			atomic.StoreUint64(&sb.bufferCount, uint64(bufferCount))
			atomic.StoreUint64(&sb.bufferSize, uint64(bufferSize))
			atomic.StoreUint64(&sb.layoutSize, uint64(layoutSize))
		},
		func() { clearEndpoint(&sb.endpoints[endpointSecondary]) },
		func() {
			prim := &sb.endpoints[endpointPrimary]
			atomic.StoreUint64(&prim.token, token)
			fillEndpoint(prim, opts)
		},
		func() { atomic.StoreUint64(&sb.magic, superblockMagic) },
	}
	for _, write := range writes {
		if err := check(); err != nil {
			// Another endpoint took over initialization.
			return 0, err
		}
		write()
	}
	return commitInit(sb, token)
}

// commitInit gives up the initializer's ownership with CAS(initToken, token,
// 0) and, only if that succeeded, bumps the epoch and marks the segment
// Ready. It returns errInitContended, leaving the segment as it is, when
// another endpoint owns the initialization now.
func commitInit(sb *standardSuperblock, token uint64) (uint64, error) {
	if !atomic.CompareAndSwapUint64(&sb.initToken, token, 0) {
		return 0, errInitContended
	}
	epoch := atomic.AddUint64(&sb.epoch, 1)
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
