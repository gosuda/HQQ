// Package mpmc implements a bounded, lock-free multi-producer multi-consumer
// ring buffer that lives in caller-provided memory (typically a shared-memory
// mapping) so that it can be used across processes.
//
// The algorithm is Dmitry Vyukov's bounded MPMC queue: every slot carries a
// sequence number that encodes whether it is free for the producer that owns
// position p (seq == p) or published for the consumer that owns position p
// (seq == p+1). Producers and consumers claim positions with a CAS on the
// write/read cursor and publish with a single atomic store, which also
// provides the release/acquire ordering for the slot payload.
//
// Blocking operations use an adaptive wait strategy (see WaitPolicy): a short
// busy spin, then cooperative yielding, then parking on a 32-bit wait word in
// the ring header. Parking uses futex(2) on Linux and __ulock_wait on macOS,
// both of which work across processes for shared mappings. Other platforms
// fall back to bounded exponential sleeps.
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
	// header words apart. 128 bytes covers both 64-byte (x86, most ARM) and
	// 128-byte (Apple silicon) cache lines.
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
	notEmpty     uint32 // bumped by producers to wake parked consumers
	emptyWaiters uint32 // number of consumers parked on notEmpty
	notFull      uint32 // bumped by consumers to wake parked producers
	fullWaiters  uint32 // number of producers parked on notFull
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
	minSleep        = 20 * time.Microsecond  // sleep backoff start (no parking support)
	maxSleep        = 2 * time.Millisecond   // sleep backoff cap (no parking support)
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
type MPMCRing[T any] struct {
	head   *_mring
	data   unsafe.Pointer
	mask   uint64
	size   uint64
	stride uintptr
	policy WaitPolicy
}

// stride returns the slot stride for T.
func stride[T any]() uintptr {
	s := unsafe.Sizeof(_melem[T]{})
	return (s + slotAlign - 1) &^ (slotAlign - 1)
}

// SizeMPMCRing returns the number of bytes required for a ring with n slots
// of T, including the header.
func SizeMPMCRing[T any](n uintptr) uintptr {
	return HeaderSize + stride[T]()*n
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

// _RoundUpPowerOf2 rounds up a number to the next power of 2.
//
// Algorithm from: https://graphics.stanford.edu/~seander/bithacks.html#RoundUpPowerOf2
func _RoundUpPowerOf2(v uint64) uint64 {
	v--
	v |= v >> 1
	v |= v >> 2
	v |= v >> 4
	v |= v >> 8
	v |= v >> 16
	v |= v >> 32
	v++
	return v
}

// MPMCInit initializes a ring at address h unless an initialized ring is
// already there, in which case it returns false. size is rounded up to a
// power of two; the memory must hold SizeMPMCRing[T](size) bytes.
//
// Use Format to unconditionally (re)initialize memory the caller owns
// exclusively.
func MPMCInit[T any](h uintptr, size uint64) bool {
	p := PointerFromAddress(h)
	hdr := (*_mring)(p)
	magic := atomic.LoadUint64(&hdr._magic)
	if magic == _mpmc_magic && atomic.LoadUint64(&hdr._flag)&uint64(_mpmc_init) != 0 {
		return false
	}
	if !atomic.CompareAndSwapUint64(&hdr._magic, magic, 0) {
		return false
	}
	Format[T](p, size)
	return true
}

// Format unconditionally initializes a ring at p with size slots (rounded up
// to a power of two). The caller must guarantee that no other party is using
// the memory while Format runs.
//
//go:nocheckptr
func Format[T any](p unsafe.Pointer, size uint64) {
	size = _RoundUpPowerOf2(size)
	hdr := (*_mring)(p)
	atomic.StoreUint64(&hdr._flag, 0)
	atomic.StoreUint64(&hdr._magic, 0)

	st := stride[T]()
	data := unsafe.Add(p, HeaderSize)
	for i := uint64(0); i < size; i++ {
		e := (*_melem[T])(unsafe.Add(data, uintptr(i)*st))
		e._data = *new(T)
		atomic.StoreUint64(&e._seq, i)
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
				size == 0 || size&(size-1) != 0 {
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
	wakeWord(&m.head.notEmpty)
	wakeWord(&m.head.notFull)
}

func wakeWord(word *uint32) {
	atomic.AddUint32(word, 1)
	futexWake(word)
}

//go:nocheckptr
func (m *MPMCRing[T]) elem(slot uint64) *_melem[T] {
	return (*_melem[T])(unsafe.Add(m.data, uintptr(slot)*m.stride))
}

// notifyConsumers wakes parked consumers after a slot was published.
func notifyConsumers(h *_mring) {
	if atomic.LoadUint32(&h.emptyWaiters) != 0 {
		wakeWord(&h.notEmpty)
	}
}

// notifyProducers wakes parked producers after a slot was released.
func notifyProducers(h *_mring) {
	if atomic.LoadUint32(&h.fullWaiters) != 0 {
		wakeWord(&h.notFull)
	}
}

// claimProducer claims the next producer position. On success the caller owns
// the returned slot until it publishes it.
func (m *MPMCRing[T]) claimProducer(c *Cancel) (uint64, *_melem[T], error) {
	h := m.head
	pos := atomic.LoadUint64(&h.w)
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
			// Another producer claimed this position; catch up.
			pos = atomic.LoadUint64(&h.w)
		default:
			// The ring is full: the slot still holds an unreleased item.
			if err := w.wait(m.policy, c, &h.notFull, &h.fullWaiters, &e._seq, pos); err != nil {
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
			// Another consumer claimed this position; catch up.
			pos = atomic.LoadUint64(&h.r)
		default:
			// The ring is empty: the slot has not been published yet.
			if err := w.wait(m.policy, c, &h.notEmpty, &h.emptyWaiters, &e._seq, pos+1); err != nil {
				return 0, nil, err
			}
		}
	}
}

// publish makes a claimed producer slot visible to consumers.
func (m *MPMCRing[T]) publish(pos uint64, e *_melem[T]) {
	atomic.StoreUint64(&e._seq, pos+1)
	notifyConsumers(m.head)
}

// release returns a claimed consumer slot to producers.
func (m *MPMCRing[T]) release(pos uint64, e *_melem[T]) {
	atomic.StoreUint64(&e._seq, pos+m.mask+1)
	notifyProducers(m.head)
}

// waiter holds the per-call state of a blocking operation.
type waiter struct {
	iter      int
	armed     bool
	stop      func() bool
	nextCheck time.Time
	sleep     time.Duration
}

func (w *waiter) finish() {
	if w.stop != nil {
		w.stop()
		w.stop = nil
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
func (w *waiter) wait(policy WaitPolicy, c *Cancel, word, waiters *uint32, seq *uint64, target uint64) error {
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

	if !parkingSupported {
		if w.sleep == 0 {
			w.sleep = minSleep
		} else if w.sleep *= 2; w.sleep > maxSleep {
			w.sleep = maxSleep
		}
		time.Sleep(min(w.sleep, timeout))
		return nil
	}

	if !w.armed {
		w.armed = true
		if c != nil && c.Ctx != nil && c.Ctx.Done() != nil {
			w.stop = context.AfterFunc(c.Ctx, func() { wakeWord(word) })
		}
	}

	atomic.AddUint32(waiters, 1)
	v := atomic.LoadUint32(word)
	if stillBlocked(seq, target) && poll(c) == nil {
		futexWait(word, v, timeout)
	}
	atomic.AddUint32(waiters, ^uint32(0))
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

// Valid reports whether the slot was successfully reserved and not committed.
func (s *ProducerSlot[T]) Valid() bool {
	return s != nil && s.elem != nil && !s.done
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

// Commit publishes the reserved producer slot. It returns false if the slot was
// already committed or is invalid.
func (s *ProducerSlot[T]) Commit() bool {
	if !s.Valid() {
		return false
	}
	s.ring.publish(s.pos, s.elem)
	s.done = true
	return true
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

// Valid reports whether the slot was successfully reserved and not released.
func (s *ConsumerSlot[T]) Valid() bool {
	return s != nil && s.elem != nil && !s.done
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

// Release returns the reserved consumer slot to producers. It returns false if
// the slot was already released or is invalid.
func (s *ConsumerSlot[T]) Release() bool {
	if !s.Valid() {
		return false
	}
	s.ring.release(s.pos, s.elem)
	s.done = true
	return true
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
// WARNING: The callback runs while the ring slot is claimed. It must return
// promptly; if it blocks or never returns, the whole ring can suffer
// head-of-line (HOL) blocking. Do not retain elem after the callback returns.
// Cancellation is not checked after the slot has been claimed.
func (m *MPMCRing[T]) EnqueueZeroCopyCancel(c *Cancel, fn func(slot uint64, elem *T)) error {
	pos, e, err := m.claimProducer(c)
	if err != nil {
		return err
	}
	fn(pos&m.mask, &e._data)
	m.publish(pos, e)
	return nil
}

// DequeueZeroCopyCancel claims one consumer slot, passes the slot index and
// in-place element pointer to fn, then releases the slot when fn returns.
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
	fn(pos&m.mask, &e._data)
	m.release(pos, e)
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
