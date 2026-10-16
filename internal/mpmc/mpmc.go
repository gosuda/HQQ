// Package mpmc implements a bounded, lock-free multi-producer multi-consumer
// ring buffer that lives in caller-provided memory (typically a shared-memory
// mapping) so that it can be used across processes.
//
// The algorithm is Dmitry Vyukov's bounded MPMC queue: every slot carries a
// sequence number that encodes whether it is free for the producer that owns
// position p (seq == p) or published for the consumer that owns position p
// (seq == p+1). Producers and consumers claim positions with a CAS on the
// write/read cursor and complete a slot with a CAS on its sequence number
// (p to p+1 when publishing, p+1 to p+mask+1 when releasing), which also
// provides the release/acquire ordering for the slot payload. The completing
// CAS cannot fail for the slot's owner; it only fails for a stale copy of a
// reservation that was already completed, which then neither touches the
// ring nor wakes anyone.
//
// Blocking operations use an adaptive wait strategy (see WaitPolicy): a short
// busy spin, then cooperative yielding, then parking on a 32-bit wait word in
// the ring header. Parking uses futex(2) on Linux and __ulock_wait on macOS,
// both of which work across processes for shared mappings. Windows has no
// cross-process compare-and-wait primitive: a ring handle can be given a
// Parker (see SetParker, typically named kernel semaphores) for cross-process
// parking, and otherwise parks on WaitOnAddress, which only wakes waiters of
// the same process and therefore bounds every park by a short backoff. Other
// platforms fall back to bounded exponential sleeps.
//
// Within one process, all goroutines blocked on the same wait word share a
// single kernel park: one leader parks, the others wait on a process-local
// channel and re-check when the leader returns, so N blocked goroutines pin
// one OS thread rather than N.
package mpmc

import (
	"context"
	"errors"
	"runtime"
	"sync/atomic"
	"time"
	"unsafe"
)

// Errors returned by cancellable blocking operations.
var (
	// ErrClosed reports that Cancel.Closed was closed while waiting.
	ErrClosed = errors.New("mpmc: closed")
	// ErrTimeout reports that Cancel.Deadline passed while waiting.
	ErrTimeout = errors.New("mpmc: deadline exceeded")
	// ErrCanceled reports that Cancel.Ctx was canceled while waiting. Callers
	// that own the context should report ctx.Err() instead.
	ErrCanceled = errors.New("mpmc: canceled")
)

const (
	// HeaderSize is the fixed size of the ring header. The data region starts
	// immediately after it.
	HeaderSize = 512

	// cacheLine is the padding granularity used to keep independently written
	// header words apart. 128 bytes covers both 64-byte and 128-byte cache
	// lines (some arm64 CPUs use 128-byte lines).
	cacheLine = 128

	// slotAlign is the alignment of every slot. Aligning slots prevents a
	// producer publishing slot i+1 from invalidating the cache line a consumer
	// is reading for slot i.
	slotAlign = 64

	// layoutVersion is stored in the header so attachers can reject rings
	// created with an incompatible layout.
	layoutVersion = 2
)

// Magic number to identify initialized MPMC rings.
const _mpmc_magic uint64 = 0xc9d8c1d43f096702

// _mpmcflag represents initialization flags for the ring buffer.
type _mpmcflag uint64

const (
	_mpmc_reserved = _mpmcflag(1) << iota // Reserved flag for future use
	_mpmc_init                            // Ring is initialized flag
)

// _mring is the ring header stored at the beginning of the ring memory.
// Each group of independently written words sits on its own 128-byte line.
type _mring struct {
	// Line 0: immutable after initialization.
	_magic   uint64
	_size    uint64
	_flag    uint64
	_stride  uint64
	_version uint64
	_        [cacheLine/8 - 5]uint64

	// Line 1: consumer cursor.
	r uint64
	_ [cacheLine/8 - 1]uint64

	// Line 2: producer cursor.
	w uint64
	_ [cacheLine/8 - 1]uint64

	// Line 3: wait words. They are only written when a waiter parks or a
	// parked waiter has to be woken, so the hot path only reads them.
	//
	// emptyWaiters and fullWaiters count the registered waiters of each
	// side: with waiter aggregation (see waitgroup.go) at most one per
	// process, the one parked in the kernel. A waker bumps the matching word
	// and issues a wake only while the count is non-zero. A process that is
	// killed while parked never decrements its count, so the count stays
	// raised until Format resets it. The cost is one superfluous wake
	// syscall per publish (or release) for the surviving endpoint, which
	// must close the link after ErrPeerGone anyway; correctness is not
	// affected, because waiters only ever use the count as a hint to wake.
	notEmpty     uint32 // bumped by producers to wake parked consumers
	emptyWaiters uint32 // number of consumers registered as parked on notEmpty
	notFull      uint32 // bumped by consumers to wake parked producers
	fullWaiters  uint32 // number of producers registered as parked on notFull
	_            [cacheLine/8 - 2]uint64
}

// Compile-time assertion that the header is exactly HeaderSize bytes.
var (
	_ [HeaderSize - unsafe.Sizeof(_mring{})]struct{}
	_ [unsafe.Sizeof(_mring{}) - HeaderSize]struct{}
)

// _melem is a single ring slot: a sequence number followed by the payload.
type _melem[T any] struct {
	_seq  uint64
	_data T
}

// WaitPolicy selects how blocking operations wait for the ring to become
// non-full (producers) or non-empty (consumers).
type WaitPolicy uint8

const (
	// WaitAdaptive spins briefly, then yields, then parks the waiting thread
	// on a shared wait word. Idle waiters consume (almost) no CPU.
	WaitAdaptive WaitPolicy = iota

	// WaitSpin never parks. It gives the lowest wake-up latency at the cost
	// of keeping one CPU busy per blocked waiter.
	WaitSpin
)

// Tuning knobs for WaitAdaptive.
const (
	spinIterations  = 64                     // tight re-check loop before yielding
	yieldIterations = 256                    // runtime.Gosched iterations before parking
	maxPark         = 100 * time.Millisecond // upper bound for a single park
	checkInterval   = 50 * time.Millisecond  // minimum spacing of Cancel.Check calls
	minSleep        = 20 * time.Microsecond  // backoff start without cross-process parking
	maxSleep        = 2 * time.Millisecond   // backoff cap without cross-process parking
)

// Cancel describes when a blocking operation must give up. A nil *Cancel
// waits forever.
type Cancel struct {
	// Ctx cancels the wait with ErrCanceled when done. Parked waiters are
	// woken immediately.
	Ctx context.Context

	// Closed cancels the wait with ErrClosed when closed. The closer must
	// call WakeAll so that parked waiters observe it promptly.
	Closed <-chan struct{}

	// Deadline cancels the wait with ErrTimeout once passed.
	Deadline time.Time

	// NoWait makes the operation fail with ErrTimeout instead of waiting
	// when the ring is full (producers) or empty (consumers).
	NoWait bool

	// DeadlineFunc, when non-nil, is consulted in addition to Deadline on
	// every slow-path check, so that a deadline changed while the operation
	// is waiting takes effect. The setter should call WakeAll after changing
	// it so that parked waiters re-evaluate promptly. A zero time means no
	// deadline.
	DeadlineFunc func() time.Time

	// Check, when non-nil, is called once when the operation leaves the
	// spin phase and then periodically (at most every 50ms). A non-nil error
	// aborts the wait and is returned to the caller. It is typically used
	// for peer liveness.
	Check func() error
}

// deadline returns the effective deadline of c (zero if none).
func (c *Cancel) deadline() time.Time {
	d := c.Deadline
	if c.DeadlineFunc != nil {
		if f := c.DeadlineFunc(); !f.IsZero() && (d.IsZero() || f.Before(d)) {
			d = f
		}
	}
	return d
}

// MPMCRing is a handle to a ring stored in caller-provided memory. Handles are
// cheap; every process (or goroutine) may attach its own handle.
//
// The ring memory is typically shared between processes and is not scanned by
// the Go garbage collector, so T must not contain Go pointers (pointers,
// slices, strings, maps, channels, interfaces, funcs) and must be valid for
// any bit pattern another process may have written. Use plain integers,
// fixed-size arrays and structs of them.
type MPMCRing[T any] struct {
	head   *_mring
	data   unsafe.Pointer
	mask   uint64
	size   uint64
	stride uintptr
	policy WaitPolicy

	// parker, when set, replaces the in-memory wait words as the blocking
	// primitive (see Parker).
	parker Parker
}

// stride returns the slot stride for T.
func stride[T any]() uintptr {
	s := unsafe.Sizeof(_melem[T]{})
	return (s + slotAlign - 1) &^ (slotAlign - 1)
}

// SizeMPMCRing returns the number of bytes required for a ring with n slots
// of T, including the header. It returns 0 if n is not a power of two of at
// least 2, or if the size would overflow uintptr; Format rejects the same
// sizes.
func SizeMPMCRing[T any](n uintptr) uintptr {
	if !validRingSize(uint64(n)) {
		return 0
	}
	st := stride[T]()
	if n > (^uintptr(0)-HeaderSize)/st {
		return 0
	}
	return HeaderSize + st*n
}

// PointerFromAddress converts the address of memory that is either outside
// the Go heap (an mmap'ed region) or kept alive by the caller into a pointer.
//
// Reading the integer through a *unsafe.Pointer is equivalent to
// unsafe.Pointer(addr), but it keeps the conversion out of go vet's
// uintptr-to-pointer heuristic, which cannot know that addr refers to
// caller-owned memory.
func PointerFromAddress(addr uintptr) unsafe.Pointer {
	return *(*unsafe.Pointer)(unsafe.Pointer(&addr))
}

// validRingSize reports whether n is a supported slot count: a power of two
// of at least 2. A single slot cannot work because the "published" and "free
// for the next lap" sequence values coincide (pos+1 == pos+mask+1), so a
// second producer could overwrite an unconsumed element.
func validRingSize(n uint64) bool {
	return n >= 2 && n&(n-1) == 0
}

// FormatSlice is the number of slots FormatGuarded writes between two calls
// of its check function. It bounds how much a writer that lost ownership of
// the memory while it was suspended can still write.
const FormatSlice = 1024

// Format unconditionally initializes a ring at p with exactly size slots. The
// caller must guarantee that no other party is using the memory while Format
// runs, and that it spans at least SizeMPMCRing[T](size) bytes.
//
// Format panics if size is not a power of two of at least 2: that is a
// programmer error, SizeMPMCRing returns 0 for such sizes, and silently
// rounding up would make the ring larger than the memory the caller sized for
// it.
//
// Format zeroes every slot and both wait-word pairs, which also clears waiter
// counts left behind by processes that died while parked (see _mring).
//
//go:nocheckptr
func Format[T any](p unsafe.Pointer, size uint64) {
	_ = FormatGuarded[T](p, size, nil)
}

// FormatGuarded is Format for memory whose ownership can be lost while the
// caller is suspended. check, if not nil, runs before the first write, before
// every FormatSlice slots and before the header words are completed; if it
// returns an error, FormatGuarded stops and returns that error without
// writing any more. The ring is then unusable (its magic is cleared by the
// first write), exactly as if the caller had crashed in the middle of Format.
// A check cannot make the check and the following write atomic: a caller
// suspended between them can still write up to FormatSlice slots.
//
//go:nocheckptr
func FormatGuarded[T any](p unsafe.Pointer, size uint64, check func() error) error {
	if !validRingSize(size) {
		panic("mpmc: ring size must be a power of two of at least 2")
	}
	if check != nil {
		if err := check(); err != nil {
			return err
		}
	}
	hdr := (*_mring)(p)
	atomic.StoreUint64(&hdr._flag, 0)
	atomic.StoreUint64(&hdr._magic, 0)

	st := stride[T]()
	data := unsafe.Add(p, HeaderSize)
	for i := uint64(0); i < size; i++ {
		if check != nil && i != 0 && i%FormatSlice == 0 {
			if err := check(); err != nil {
				return err
			}
		}
		e := (*_melem[T])(unsafe.Add(data, uintptr(i)*st))
		e._data = *new(T)
		atomic.StoreUint64(&e._seq, i)
	}
	if check != nil {
		if err := check(); err != nil {
			return err
		}
	}

	atomic.StoreUint64(&hdr.r, 0)
	atomic.StoreUint64(&hdr.w, 0)
	atomic.StoreUint32(&hdr.notEmpty, 0)
	atomic.StoreUint32(&hdr.emptyWaiters, 0)
	atomic.StoreUint32(&hdr.notFull, 0)
	atomic.StoreUint32(&hdr.fullWaiters, 0)
	atomic.StoreUint64(&hdr._size, size)
	atomic.StoreUint64(&hdr._stride, uint64(st))
	atomic.StoreUint64(&hdr._version, layoutVersion)
	atomic.StoreUint64(&hdr._magic, _mpmc_magic)
	atomic.StoreUint64(&hdr._flag, uint64(_mpmc_init))
	return nil
}

// MPMCAttach attaches to the ring at address h, waiting up to timeout for it
// to be initialized (0 waits forever). It returns nil on timeout or if the
// ring was created with an incompatible slot layout.
func MPMCAttach[T any](h uintptr, timeout time.Duration) *MPMCRing[T] {
	return Attach[T](PointerFromAddress(h), timeout)
}

// Attach is MPMCAttach for a pointer.
func Attach[T any](p unsafe.Pointer, timeout time.Duration) *MPMCRing[T] {
	start := time.Now()
	hdr := (*_mring)(p)
	for {
		if atomic.LoadUint64(&hdr._magic) == _mpmc_magic &&
			atomic.LoadUint64(&hdr._flag)&uint64(_mpmc_init) != 0 {
			size := atomic.LoadUint64(&hdr._size)
			st := stride[T]()
			if atomic.LoadUint64(&hdr._stride) != uint64(st) ||
				atomic.LoadUint64(&hdr._version) != layoutVersion ||
				!validRingSize(size) {
				return nil
			}
			return &MPMCRing[T]{
				head:   hdr,
				data:   unsafe.Add(p, HeaderSize),
				mask:   size - 1,
				size:   size,
				stride: st,
			}
		}
		if timeout > 0 && time.Since(start) >= timeout {
			return nil
		}
		time.Sleep(100 * time.Microsecond)
	}
}

// SetParker makes blocking operations on this handle park through p instead
// of the platform's wait-word primitive. It must not be called concurrently
// with operations on the handle, and p must stay usable until the last
// operation on the handle returned.
func (m *MPMCRing[T]) SetParker(p Parker) {
	m.parker = p
}

// SetWaitPolicy selects the wait strategy used by blocking operations on this
// handle. It must not be called concurrently with operations on the handle.
func (m *MPMCRing[T]) SetWaitPolicy(p WaitPolicy) {
	m.policy = p
}

// Size returns the number of slots in the ring.
func (m *MPMCRing[T]) Size() uint64 {
	return m.size
}

// WakeAll wakes every waiter parked on this ring, in every process. Waiters
// re-check their condition and cancellation state and park again if needed.
// Call it after closing a Cancel.Closed channel.
func (m *MPMCRing[T]) WakeAll() {
	m.site(NotEmpty).wakeAll()
	m.site(NotFull).wakeAll()
}

// Side names one of the two conditions a blocked operation waits for.
type Side uint8

const (
	// NotEmpty is awaited by consumers and signaled by producers.
	NotEmpty Side = iota
	// NotFull is awaited by producers and signaled by consumers.
	NotFull
)

// Parker is a cross-process blocking primitive that replaces the platform's
// wait-word primitive for one ring, shared by every process that attaches the
// ring (for example a pair of named kernel semaphores per side).
//
// A waiter registers in the ring header's waiter count, re-checks the ring
// and then calls Park; a waker that observes a non-zero waiter count after
// publishing calls Wake. Wake grants n wake-up tokens and Park consumes one,
// so a Wake that runs between the waiter's re-check and its Park is not lost.
// Tokens granted to waiters that already gave up are harmless: they only
// cause one spurious return from Park, after which waiters re-check.
//
// Within one process only one goroutine per side (the leader of the
// goroutines blocked on that side) registers and calls Park at a time, so n
// counts parked processes, not goroutines. A process killed while parked
// leaves its count raised until the ring is re-formatted (see Format). The
// ring therefore grants a single token per published or released slot and
// uses the count only to decide whether to wake at all; WakeAll asks for the
// whole count. Surplus tokens would otherwise accumulate for every later
// slot, and after a reuse of the same name they would be inherited.
//
// Implementations must be safe for concurrent use and Wake must not block.
type Parker interface {
	// Park blocks until a wake-up token is available (consuming it) or timeout
	// elapses. It may also return early.
	Park(side Side, timeout time.Duration)
	// Wake makes up to n tokens available for side.
	Wake(side Side, n uint32)
}

// waitSite names the wait word of one side of a ring together with its
// blocking primitive.
type waitSite struct {
	word    *uint32
	waiters *uint32
	parker  Parker
	side    Side
}

// site returns the wait site of side.
func (m *MPMCRing[T]) site(side Side) waitSite {
	if side == NotEmpty {
		return waitSite{&m.head.notEmpty, &m.head.emptyWaiters, m.parker, side}
	}
	return waitSite{&m.head.notFull, &m.head.fullWaiters, m.parker, side}
}

// wakeOne grants the wake-up token of one published or released slot. Each
// slot can serve one parked waiter, so one token suffices; the registered
// count may be stale (see Parker) and must not be granted in full.
func (s waitSite) wakeOne() {
	if s.parker != nil {
		if atomic.LoadUint32(s.waiters) != 0 {
			s.parker.Wake(s.side, 1)
		}
		return
	}
	s.wakeWord()
}

// wakeAll wakes every waiter parked on s, in every process.
func (s waitSite) wakeAll() {
	if s.parker != nil {
		if n := atomic.LoadUint32(s.waiters); n != 0 {
			s.parker.Wake(s.side, n)
		}
		return
	}
	s.wakeWord()
}

// wakeWord bumps the wait word of s and wakes futex waiters on it.
func (s waitSite) wakeWord() {
	atomic.AddUint32(s.word, 1)
	futexWake(s.word)
}

//go:nocheckptr
func (m *MPMCRing[T]) elem(slot uint64) *_melem[T] {
	return (*_melem[T])(unsafe.Add(m.data, uintptr(slot)*m.stride))
}

// notifyConsumers wakes parked consumers after a slot was published.
func (m *MPMCRing[T]) notifyConsumers() {
	if atomic.LoadUint32(&m.head.emptyWaiters) != 0 {
		m.site(NotEmpty).wakeOne()
	}
}

// notifyProducers wakes parked producers after a slot was released.
func (m *MPMCRing[T]) notifyProducers() {
	if atomic.LoadUint32(&m.head.fullWaiters) != 0 {
		m.site(NotFull).wakeOne()
	}
}

// claimProducer claims the next producer position. On success the caller owns
// the returned slot until it publishes it.
func (m *MPMCRing[T]) claimProducer(c *Cancel) (uint64, *_melem[T], error) {
	h := m.head
	pos := atomic.LoadUint64(&h.w)
	for {
		e := m.elem(pos & m.mask)
		diff := int64(atomic.LoadUint64(&e._seq) - pos)
		switch {
		case diff == 0:
			if atomic.CompareAndSwapUint64(&h.w, pos, pos+1) {
				return pos, e, nil
			}
			pos = atomic.LoadUint64(&h.w)
		case diff > 0:
			// Another producer claimed this position; catch up.
			pos = atomic.LoadUint64(&h.w)
		default:
			// The ring is full. Waiting needs per-call state, kept out of
			// this fast path.
			return m.claimProducerSlow(c, pos)
		}
	}
}

// claimProducerSlow is claimProducer once the ring was found full at pos.
func (m *MPMCRing[T]) claimProducerSlow(c *Cancel, pos uint64) (uint64, *_melem[T], error) {
	h := m.head
	var w waiter
	defer w.finish()
	for {
		e := m.elem(pos & m.mask)
		diff := int64(atomic.LoadUint64(&e._seq) - pos)
		switch {
		case diff == 0:
			if atomic.CompareAndSwapUint64(&h.w, pos, pos+1) {
				return pos, e, nil
			}
			pos = atomic.LoadUint64(&h.w)
		case diff > 0:
			pos = atomic.LoadUint64(&h.w)
		default:
			// The slot still holds an unreleased item.
			if err := w.wait(m.policy, c, m.site(NotFull), &e._seq, pos); err != nil {
				return 0, nil, err
			}
		}
	}
}

// claimConsumer claims the next consumer position. On success the caller owns
// the returned slot until it releases it.
func (m *MPMCRing[T]) claimConsumer(c *Cancel) (uint64, *_melem[T], error) {
	h := m.head
	pos := atomic.LoadUint64(&h.r)
	for {
		e := m.elem(pos & m.mask)
		diff := int64(atomic.LoadUint64(&e._seq) - (pos + 1))
		switch {
		case diff == 0:
			if atomic.CompareAndSwapUint64(&h.r, pos, pos+1) {
				return pos, e, nil
			}
			pos = atomic.LoadUint64(&h.r)
		case diff > 0:
			// Another consumer claimed this position; catch up.
			pos = atomic.LoadUint64(&h.r)
		default:
			// The ring is empty; see claimProducer.
			return m.claimConsumerSlow(c, pos)
		}
	}
}

// claimConsumerSlow is claimConsumer once the ring was found empty at pos.
func (m *MPMCRing[T]) claimConsumerSlow(c *Cancel, pos uint64) (uint64, *_melem[T], error) {
	h := m.head
	var w waiter
	defer w.finish()
	for {
		e := m.elem(pos & m.mask)
		diff := int64(atomic.LoadUint64(&e._seq) - (pos + 1))
		switch {
		case diff == 0:
			if atomic.CompareAndSwapUint64(&h.r, pos, pos+1) {
				return pos, e, nil
			}
			pos = atomic.LoadUint64(&h.r)
		case diff > 0:
			pos = atomic.LoadUint64(&h.r)
		default:
			// The slot has not been published yet.
			if err := w.wait(m.policy, c, m.site(NotEmpty), &e._seq, pos+1); err != nil {
				return 0, nil, err
			}
		}
	}
}

// publish makes a claimed producer slot visible to consumers. The caller is
// the only owner of the claim (it holds pos on its stack), so a plain store
// suffices.
func (m *MPMCRing[T]) publish(pos uint64, e *_melem[T]) {
	atomic.StoreUint64(&e._seq, pos+1)
	m.notifyConsumers()
}

// release returns a claimed consumer slot to producers; see publish.
func (m *MPMCRing[T]) release(pos uint64, e *_melem[T]) {
	atomic.StoreUint64(&e._seq, pos+m.mask+1)
	m.notifyProducers()
}

// publishSlot is publish for a claim held by a ProducerSlot, which callers
// may copy. It completes the slot with a compare-and-swap from the claimed
// state (seq == pos), so a stale copy of an already committed slot fails the
// swap and neither modifies the ring nor wakes anyone; it then returns false.
func (m *MPMCRing[T]) publishSlot(pos uint64, e *_melem[T]) bool {
	if !atomic.CompareAndSwapUint64(&e._seq, pos, pos+1) {
		return false
	}
	m.notifyConsumers()
	return true
}

// releaseSlot is release for a claim held by a ConsumerSlot; like
// publishSlot it swaps from the claimed state (seq == pos+1) and reports
// false, without side effects, when the slot was already released.
func (m *MPMCRing[T]) releaseSlot(pos uint64, e *_melem[T]) bool {
	if !atomic.CompareAndSwapUint64(&e._seq, pos+1, pos+m.mask+1) {
		return false
	}
	m.notifyProducers()
	return true
}

// waiter holds the per-call state of a blocking operation.
type waiter struct {
	iter      int
	armed     bool
	stop      func() bool   // stops the context wake-up callback
	stopped   chan struct{} // closed when the callback returns
	nextCheck time.Time
	sleep     time.Duration

	group *parkGroup  // waiter aggregation state of word, nil until the first park
	word  *uint32     // wait word group belongs to
	timer *time.Timer // follower wake-up timer, reused across parks
}

// finish ends the blocking operation. When it returns, the context wake-up
// callback is neither running nor will it run: context.AfterFunc's stop
// function does not wait for a callback that already started, and the
// callback touches the ring's wait word (or the Parker), which the owner may
// unmap or close as soon as the operation returned.
func (w *waiter) finish() {
	if w.stop != nil {
		if !w.stop() {
			<-w.stopped
		}
		w.stop = nil
	}
	if w.group != nil {
		releaseParkGroup(w.word, w.group)
		w.group = nil
	}
	if w.timer != nil {
		w.timer.Stop()
		w.timer = nil
	}
}

// poll reports a cancellation that is observable without reading the clock.
func poll(c *Cancel) error {
	if c == nil {
		return nil
	}
	if c.Ctx != nil {
		if done := c.Ctx.Done(); done != nil {
			select {
			case <-done:
				return ErrCanceled
			default:
			}
		}
	}
	if c.Closed != nil {
		select {
		case <-c.Closed:
			return ErrClosed
		default:
		}
	}
	return nil
}

// slowChecks evaluates the deadline and the periodic Check callback.
func (w *waiter) slowChecks(c *Cancel, now time.Time) error {
	if c == nil {
		return nil
	}
	if d := c.deadline(); !d.IsZero() && !now.Before(d) {
		return ErrTimeout
	}
	if c.Check != nil && (w.nextCheck.IsZero() || !now.Before(w.nextCheck)) {
		w.nextCheck = now.Add(checkInterval)
		if err := c.Check(); err != nil {
			return err
		}
	}
	return nil
}

// stillBlocked reports whether the slot sequence has not reached target yet.
func stillBlocked(seq *uint64, target uint64) bool {
	return int64(atomic.LoadUint64(seq)-target) < 0
}

// wait performs one backoff step while the slot at seq has not reached
// target. It returns a non-nil error if the operation must be abandoned.
func (w *waiter) wait(policy WaitPolicy, c *Cancel, site waitSite, seq *uint64, target uint64) error {
	if err := poll(c); err != nil {
		return err
	}
	if c != nil && c.NoWait {
		return ErrTimeout
	}
	w.iter++

	if policy == WaitSpin {
		if w.iter&63 == 0 {
			if err := w.slowChecks(c, time.Now()); err != nil {
				return err
			}
			runtime.Gosched()
		}
		return nil
	}

	switch {
	case w.iter <= spinIterations:
		return nil
	case w.iter <= spinIterations+yieldIterations:
		if w.iter&15 == 0 {
			if err := w.slowChecks(c, time.Now()); err != nil {
				return err
			}
		}
		runtime.Gosched()
		return nil
	}

	now := time.Now()
	if err := w.slowChecks(c, now); err != nil {
		return err
	}
	timeout := maxPark
	if c != nil {
		if d := c.deadline(); !d.IsZero() {
			if rem := d.Sub(now); rem < timeout {
				timeout = rem
			}
		}
		if c.Check != nil {
			if d := w.nextCheck.Sub(now); d < timeout {
				timeout = d
			}
		}
	}
	if timeout <= 0 {
		return nil
	}

	if site.parker == nil && !parkingSupported {
		// No cross-process parking: poll with a bounded exponential backoff.
		if w.sleep == 0 {
			w.sleep = minSleep
		} else if w.sleep *= 2; w.sleep > maxSleep {
			w.sleep = maxSleep
		}
		if !processLocalParking {
			time.Sleep(min(w.sleep, timeout))
			return nil
		}
		// Waiters of this process wake immediately; a wake-up from another
		// process is only noticed when the backoff step ends.
		timeout = min(w.sleep, timeout)
	}

	w.park(c, site, seq, target, timeout)
	return nil
}

// ProducerSlot is a claimed producer-side ring slot.
//
// WARNING: A ProducerSlot holds a ring slot until Commit is called. It must be
// committed promptly; if it is dropped, copied, or retained without committing,
// the whole ring can suffer head-of-line (HOL) blocking. The pointer returned
// by Value is invalid after Commit returns.
type ProducerSlot[T any] struct {
	ring *MPMCRing[T]
	pos  uint64
	elem *_melem[T]
	done bool
}

// Valid reports whether the slot was successfully reserved and not committed
// through this value. It does not observe a Commit made through a copy; see
// Owned.
func (s *ProducerSlot[T]) Valid() bool {
	return s != nil && s.elem != nil && !s.done
}

// Owned reports whether the ring still treats the slot as claimed by this
// reservation: the slot is Valid and has not been published (or otherwise
// completed) through a copy of this value. The answer is a snapshot; the
// compare-and-swap inside Commit is the authority.
func (s *ProducerSlot[T]) Owned() bool {
	return s.Valid() && atomic.LoadUint64(&s.elem._seq) == s.pos
}

// Slot returns the ring slot index.
func (s *ProducerSlot[T]) Slot() uint64 {
	if s == nil || s.ring == nil {
		return 0
	}
	return s.pos & s.ring.mask
}

// Value returns a pointer to the slot element while the slot is reserved.
func (s *ProducerSlot[T]) Value() *T {
	if !s.Valid() {
		return nil
	}
	return &s.elem._data
}

// Commit publishes the reserved producer slot. It returns false, and leaves
// the ring and its waiters untouched, if the slot was already committed or is
// invalid, including when another copy of this value committed it first.
func (s *ProducerSlot[T]) Commit() bool {
	if !s.Valid() {
		return false
	}
	s.done = true
	return s.ring.publishSlot(s.pos, s.elem)
}

// ConsumerSlot is a claimed consumer-side ring slot.
//
// WARNING: A ConsumerSlot holds a ring slot until Release is called. It must be
// released promptly; if it is dropped, copied, or retained without releasing,
// the whole ring can suffer head-of-line (HOL) blocking. The pointer returned
// by Value is invalid after Release returns.
type ConsumerSlot[T any] struct {
	ring *MPMCRing[T]
	pos  uint64
	elem *_melem[T]
	done bool
}

// Valid reports whether the slot was successfully reserved and not released
// through this value. It does not observe a Release made through a copy; see
// Owned.
func (s *ConsumerSlot[T]) Valid() bool {
	return s != nil && s.elem != nil && !s.done
}

// Owned reports whether the ring still treats the slot as claimed by this
// reservation: the slot is Valid and has not been released through a copy of
// this value. The answer is a snapshot; the compare-and-swap inside Release is
// the authority.
func (s *ConsumerSlot[T]) Owned() bool {
	return s.Valid() && atomic.LoadUint64(&s.elem._seq) == s.pos+1
}

// Slot returns the ring slot index.
func (s *ConsumerSlot[T]) Slot() uint64 {
	if s == nil || s.ring == nil {
		return 0
	}
	return s.pos & s.ring.mask
}

// Value returns a pointer to the slot element while the slot is reserved.
func (s *ConsumerSlot[T]) Value() *T {
	if !s.Valid() {
		return nil
	}
	return &s.elem._data
}

// Release returns the reserved consumer slot to producers. It returns false,
// and leaves the ring and its waiters untouched, if the slot was already
// released or is invalid, including when another copy of this value released
// it first.
func (s *ConsumerSlot[T]) Release() bool {
	if !s.Valid() {
		return false
	}
	s.done = true
	return s.ring.releaseSlot(s.pos, s.elem)
}

func ctxCancel(ctx context.Context) *Cancel {
	if ctx == nil || ctx.Done() == nil {
		return nil
	}
	return &Cancel{Ctx: ctx}
}

// ReserveProducerCancel claims one producer slot, honoring c while waiting.
//
// WARNING: Once this returns a valid slot, cancellation is no longer checked.
// The returned slot must be committed promptly.
func (m *MPMCRing[T]) ReserveProducerCancel(c *Cancel) (ProducerSlot[T], error) {
	pos, e, err := m.claimProducer(c)
	if err != nil {
		return ProducerSlot[T]{}, err
	}
	return ProducerSlot[T]{ring: m, pos: pos, elem: e}, nil
}

// ReserveConsumerCancel claims one consumer slot, honoring c while waiting.
//
// WARNING: Once this returns a valid slot, cancellation is no longer checked.
// The returned slot must be released promptly.
func (m *MPMCRing[T]) ReserveConsumerCancel(c *Cancel) (ConsumerSlot[T], error) {
	pos, e, err := m.claimConsumer(c)
	if err != nil {
		return ConsumerSlot[T]{}, err
	}
	return ConsumerSlot[T]{ring: m, pos: pos, elem: e}, nil
}

// EnqueueZeroCopyCancel claims one producer slot, passes the slot index and
// in-place element pointer to fn, then publishes the slot when fn returns.
//
// If fn panics (or ends its goroutine with runtime.Goexit), the slot is still
// published so that the ring keeps moving, but with a zeroed element instead
// of whatever fn had written, and the panic continues. Consumers therefore
// see one zero element in place of the abandoned write; callers that must
// distinguish it should recover inside fn and write a marker element
// themselves.
//
// WARNING: The callback runs while the ring slot is claimed. It must return
// promptly; if it blocks or never returns, the whole ring can suffer
// head-of-line (HOL) blocking. Do not retain elem after the callback returns.
// Cancellation is not checked after the slot has been claimed.
func (m *MPMCRing[T]) EnqueueZeroCopyCancel(c *Cancel, fn func(slot uint64, elem *T)) error {
	pos, e, err := m.claimProducer(c)
	if err != nil {
		return err
	}
	completed := false
	defer func() {
		if !completed {
			var zero T
			e._data = zero
		}
		m.publish(pos, e)
	}()
	fn(pos&m.mask, &e._data)
	completed = true
	return nil
}

// DequeueZeroCopyCancel claims one consumer slot, passes the slot index and
// in-place element pointer to fn, then releases the slot when fn returns. The
// slot is also released if fn panics (or calls runtime.Goexit), so a failing
// callback consumes its element but cannot wedge the ring.
//
// WARNING: The callback runs while the ring slot is claimed. It must return
// promptly; if it blocks or never returns, the whole ring can suffer
// head-of-line (HOL) blocking. Do not retain elem after the callback returns.
// Cancellation is not checked after the slot has been claimed.
func (m *MPMCRing[T]) DequeueZeroCopyCancel(c *Cancel, fn func(slot uint64, elem *T)) error {
	pos, e, err := m.claimConsumer(c)
	if err != nil {
		return err
	}
	defer m.release(pos, e)
	fn(pos&m.mask, &e._data)
	return nil
}

// Enqueue adds an element to the ring, blocking until a slot is available.
func (m *MPMCRing[T]) Enqueue(elem T) {
	pos, e, _ := m.claimProducer(nil)
	e._data = elem
	m.publish(pos, e)
}

// EnqueueWithContext adds an element to the ring. It returns false if ctx is
// done before a slot becomes available.
func (m *MPMCRing[T]) EnqueueWithContext(ctx context.Context, elem T) bool {
	pos, e, err := m.claimProducer(ctxCancel(ctx))
	if err != nil {
		return false
	}
	e._data = elem
	m.publish(pos, e)
	return true
}

// EnqueueFunc adds an element by letting fn initialize it in place.
func (m *MPMCRing[T]) EnqueueFunc(fn func(*T)) {
	pos, e, _ := m.claimProducer(nil)
	fn(&e._data)
	m.publish(pos, e)
}

// ReserveProducer claims one producer slot, blocking until one is available.
//
// WARNING: The returned slot must be committed promptly. Failing to call Commit
// can cause head-of-line (HOL) blocking for the whole ring.
func (m *MPMCRing[T]) ReserveProducer() ProducerSlot[T] {
	s, _ := m.ReserveProducerCancel(nil)
	return s
}

// ReserveProducerWithContext is ReserveProducer with context cancellation
// before a producer slot is claimed.
//
// WARNING: Once this returns a valid slot, context cancellation is no longer
// checked. The returned slot must still be committed promptly.
func (m *MPMCRing[T]) ReserveProducerWithContext(ctx context.Context) (ProducerSlot[T], bool) {
	s, err := m.ReserveProducerCancel(ctxCancel(ctx))
	return s, err == nil
}

// EnqueueZeroCopy is EnqueueZeroCopyCancel without cancellation.
//
// WARNING: The callback runs while the ring slot is claimed; see
// EnqueueZeroCopyCancel.
func (m *MPMCRing[T]) EnqueueZeroCopy(fn func(slot uint64, elem *T)) {
	_ = m.EnqueueZeroCopyCancel(nil, fn)
}

// EnqueueZeroCopyWithContext is EnqueueZeroCopyCancel with a context.
//
// WARNING: The callback runs while the ring slot is claimed; see
// EnqueueZeroCopyCancel.
func (m *MPMCRing[T]) EnqueueZeroCopyWithContext(ctx context.Context, fn func(slot uint64, elem *T)) bool {
	return m.EnqueueZeroCopyCancel(ctxCancel(ctx), fn) == nil
}

// Dequeue removes an element from the ring, blocking until one is available.
func (m *MPMCRing[T]) Dequeue() T {
	pos, e, _ := m.claimConsumer(nil)
	elem := e._data
	m.release(pos, e)
	return elem
}

// DequeueWithContext removes an element from the ring. It returns false if
// ctx is done before an element becomes available.
func (m *MPMCRing[T]) DequeueWithContext(ctx context.Context) (elem T, ok bool) {
	pos, e, err := m.claimConsumer(ctxCancel(ctx))
	if err != nil {
		return elem, false
	}
	elem = e._data
	m.release(pos, e)
	return elem, true
}

// DequeueFunc removes an element by letting fn process it in place.
func (m *MPMCRing[T]) DequeueFunc(fn func(*T)) {
	pos, e, _ := m.claimConsumer(nil)
	fn(&e._data)
	m.release(pos, e)
}

// ReserveConsumer claims one consumer slot, blocking until one is available.
//
// WARNING: The returned slot must be released promptly. Failing to call Release
// can cause head-of-line (HOL) blocking for the whole ring.
func (m *MPMCRing[T]) ReserveConsumer() ConsumerSlot[T] {
	s, _ := m.ReserveConsumerCancel(nil)
	return s
}

// ReserveConsumerWithContext is ReserveConsumer with context cancellation
// before a consumer slot is claimed.
//
// WARNING: Once this returns a valid slot, context cancellation is no longer
// checked. The returned slot must still be released promptly.
func (m *MPMCRing[T]) ReserveConsumerWithContext(ctx context.Context) (ConsumerSlot[T], bool) {
	s, err := m.ReserveConsumerCancel(ctxCancel(ctx))
	return s, err == nil
}

// DequeueZeroCopy is DequeueZeroCopyCancel without cancellation.
//
// WARNING: The callback runs while the ring slot is claimed; see
// DequeueZeroCopyCancel.
func (m *MPMCRing[T]) DequeueZeroCopy(fn func(slot uint64, elem *T)) {
	_ = m.DequeueZeroCopyCancel(nil, fn)
}

// DequeueZeroCopyWithContext is DequeueZeroCopyCancel with a context.
//
// WARNING: The callback runs while the ring slot is claimed; see
// DequeueZeroCopyCancel.
func (m *MPMCRing[T]) DequeueZeroCopyWithContext(ctx context.Context, fn func(slot uint64, elem *T)) bool {
	return m.DequeueZeroCopyCancel(ctxCancel(ctx), fn) == nil
}
