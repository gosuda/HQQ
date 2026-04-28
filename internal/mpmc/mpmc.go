package mpmc

import (
	"context"
	"runtime"
	"sync/atomic"
	"time"
	"unsafe"
)

// MPMCRing implements a lock-free Multi-Producer Multi-Consumer ring buffer
// This data structure allows multiple goroutines to safely enqueue and dequeue items
// without using mutexes or other blocking synchronization primitives.
//
// The implementation uses the well-known MPMC queue algorithm with sequence numbers
// to ensure thread-safety and prevent the ABA problem.
type MPMCRing[T any] struct {
	_mask uint64  // Mask for modulo operation (size - 1, must be power of 2)
	_size uint64  // Size of the ring buffer (must be power of 2)
	_head uintptr // Pointer to the ring header in shared memory
	_data uintptr // Pointer to the ring data in shared memory
}

// ProducerSlot is a claimed producer-side ring slot.
//
// WARNING: A ProducerSlot holds a ring slot until Commit is called. It must be
// committed promptly; if it is dropped, copied, or retained without committing,
// the whole ring can suffer head-of-line (HOL) blocking. The pointer returned
// by Value is invalid after Commit returns.
type ProducerSlot[T any] struct {
	slot uint64
	pos  uint64
	elem *_melem[T]
	done bool
}

// Valid reports whether the slot was successfully reserved.
func (s *ProducerSlot[T]) Valid() bool {
	return s != nil && s.elem != nil && !s.done
}

// Slot returns the ring slot index.
func (s *ProducerSlot[T]) Slot() uint64 {
	if s == nil {
		return 0
	}
	return s.slot
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
	atomic.StoreUint64(&s.elem._seq, s.pos+1)
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
	slot uint64
	pos  uint64
	mask uint64
	elem *_melem[T]
	done bool
}

// Valid reports whether the slot was successfully reserved.
func (s *ConsumerSlot[T]) Valid() bool {
	return s != nil && s.elem != nil && !s.done
}

// Slot returns the ring slot index.
func (s *ConsumerSlot[T]) Slot() uint64 {
	if s == nil {
		return 0
	}
	return s.slot
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
	atomic.StoreUint64(&s.elem._seq, s.pos+s.mask+1)
	s.done = true
	return true
}

// MPMCInit initializes a new MPMC ring buffer in shared memory
// This function should be called only once per shared memory region.
// Returns true if initialization was successful, false if already initialized.
//
// Parameters:
//   - h: Memory offset of the shared memory region (must be page-aligned)
//   - size: Number of elements in the ring buffer (will be rounded up to power of 2)
//
// The memory layout is:
//
//	[Header (256 bytes)][Data Elements]
//
//go:nocheckptr
func MPMCInit[T any](h uintptr, size uint64) bool {
	// Round up size to the next power of 2 for efficient modulo operations
	size = _RoundUpPowerOf2(size)
	_r := (*_mring)(unsafe.Pointer(h))

	// Check if the ring is already initialized by checking the magic number
	magic := atomic.LoadUint64(&_r._magic)
	if magic == _mpmc_magic {
		return false
	}

	// Try to atomically initialize the ring by setting the magic number
	if atomic.CompareAndSwapUint64(&_r._magic, magic, _mpmc_magic) {
		// Store the size of the ring
		atomic.StoreUint64(&_r._size, size)

		// Calculate the data region offset (header + padding)
		_data := h + 256

		// Initialize all elements with their sequence numbers
		for i := uint64(0); i < size; i++ {
			_e := (*_melem[T])(unsafe.Pointer(_data + unsafe.Sizeof(_melem[T]{})*uintptr(i)))
			_e._data = *new(T) // Initialize data to zero value
			_e._seq = i        // Set initial sequence number
		}

		// Initialize read and write positions
		atomic.StoreUint64(&_r.r, 0)
		atomic.StoreUint64(&_r.w, 0)

		// Mark the ring as initialized
		atomic.StoreUint64(&_r._flag, uint64(_mpmc_init))

		return true
	}
	return false
}

// MPMCAttach attaches to an existing MPMC ring buffer in shared memory
// This function waits for the ring to be initialized and returns a handle to it.
//
// Parameters:
//   - h: Memory offset of the shared memory region (must be page-aligned)
//   - timeout: Maximum time to wait for initialization (0 = wait forever)
//
// Returns a pointer to the MPMCRing or nil if timeout occurs
//
//go:nocheckptr
func MPMCAttach[T any](h uintptr, timeout time.Duration) *MPMCRing[T] {
	_tt := time.Now()
	_r := (*_mring)(unsafe.Pointer(h))

	// Wait for the ring to be initialized
	for {
		magic := atomic.LoadUint64(&_r._magic)
		flag := atomic.LoadUint64(&_r._flag)
		size := atomic.LoadUint64(&_r._size)

		// Check if the ring is properly initialized
		if magic == _mpmc_magic && flag&uint64(_mpmc_init) != 0 {
			return &MPMCRing[T]{
				_size: size,
				_mask: size - 1, // Mask for efficient modulo operation
				_head: h,
				_data: h + 256, // Data region starts after header
			}
		}

		// Check for timeout
		if timeout > 0 && time.Since(_tt) >= timeout {
			return nil
		}

		// Yield to other goroutines while waiting
		runtime.Gosched()
	}
}

// EnqueueWithContext adds an element to the ring buffer with context support
// This function will return false if the context is cancelled before the element can be enqueued.
//
// Parameters:
//   - ctx: Context for cancellation
//   - elem: Element to enqueue
//
// Returns true if the element was successfully enqueued, false if context was cancelled
//
//go:nocheckptr
func (m *MPMCRing[T]) EnqueueWithContext(ctx context.Context, elem T) bool {
	done := ctx.Done()
	_h := (*_mring)(unsafe.Pointer(m._head))

	var c *_melem[T]
	p := atomic.LoadUint64(&_h.w) // Get current write position

	for {
		// Check for context cancellation
		select {
		case <-done:
			return false
		default:
		}

		// Get the element at the current write position
		c = (*_melem[T])(unsafe.Pointer(m._data + unsafe.Sizeof(_melem[T]{})*uintptr(p&m._mask)))
		seq := atomic.LoadUint64(&c._seq)
		diff := seq - p

		// Check if we can claim this slot
		if diff == 0 {
			// Try to atomically claim the slot by incrementing the write position
			if atomic.CompareAndSwapUint64(&_h.w, p, p+1) {
				break
			}
		} else if diff > 0 {
			// Another producer has claimed this slot, try again
			p = atomic.LoadUint64(&_h.w)
		} else {
			// This should never happen in a correctly implemented ring buffer
			panic("unreachable")
		}

		// Yield to other goroutines
		runtime.Gosched()
	}

	// Store the data in the claimed slot
	c._data = elem

	// Memory barrier to ensure the write to c._data happens before
	// we update the sequence number. This ensures consumers see the complete data.
	atomic.StoreUint64(&c._seq, p+1)
	return true
}

// Enqueue adds an element to the ring buffer
// This function will block until the element can be enqueued.
//
// Parameters:
//   - elem: Element to enqueue
//
//go:nocheckptr
func (m *MPMCRing[T]) Enqueue(elem T) {
	_h := (*_mring)(unsafe.Pointer(m._head))

	var c *_melem[T]
	p := atomic.LoadUint64(&_h.w) // Get current write position

	for {
		// Get the element at the current write position
		c = (*_melem[T])(unsafe.Pointer(m._data + unsafe.Sizeof(_melem[T]{})*uintptr(p&m._mask)))
		seq := atomic.LoadUint64(&c._seq)
		diff := seq - p

		// Check if we can claim this slot
		if diff == 0 {
			// Try to atomically claim the slot by incrementing the write position
			if atomic.CompareAndSwapUint64(&_h.w, p, p+1) {
				break
			}
		} else if diff > 0 {
			// Another producer has claimed this slot, try again
			p = atomic.LoadUint64(&_h.w)
		} else {
			// This should never happen in a correctly implemented ring buffer
			panic("unreachable")
		}

		// Yield to other goroutines
		runtime.Gosched()
	}

	// Store the data in the claimed slot
	c._data = elem

	// Memory barrier to ensure the write to c._data happens before
	// we update the sequence number
	atomic.StoreUint64(&c._seq, p+1)
}

// EnqueueFunc adds an element to the ring buffer using a function
// This allows for in-place construction of complex objects and can be more efficient
// than creating the object first and then copying it.
//
// Parameters:
//   - fn: Function that initializes the element (receives a pointer to the element)
//
//go:nocheckptr
func (m *MPMCRing[T]) EnqueueFunc(fn func(*T)) {
	_h := (*_mring)(unsafe.Pointer(m._head))

	var c *_melem[T]
	p := atomic.LoadUint64(&_h.w) // Get current write position

	for {
		// Get the element at the current write position
		c = (*_melem[T])(unsafe.Pointer(m._data + unsafe.Sizeof(_melem[T]{})*uintptr(p&m._mask)))
		seq := atomic.LoadUint64(&c._seq)
		diff := seq - p

		// Check if we can claim this slot
		if diff == 0 {
			// Try to atomically claim the slot by incrementing the write position
			if atomic.CompareAndSwapUint64(&_h.w, p, p+1) {
				break
			}
		} else if diff > 0 {
			// Another producer has claimed this slot, try again
			p = atomic.LoadUint64(&_h.w)
		} else {
			// This should never happen in a correctly implemented ring buffer
			panic("unreachable")
		}

		// Yield to other goroutines
		runtime.Gosched()
	}

	// Initialize the data in-place using the provided function
	fn(&c._data)

	// Memory barrier to ensure the write to c._data happens before
	// we update the sequence number
	atomic.StoreUint64(&c._seq, p+1)
}

// ReserveProducer claims one producer slot and returns it to the caller.
//
// WARNING: The returned slot must be committed promptly. Failing to call Commit
// can cause head-of-line (HOL) blocking for the whole ring.
//
//go:nocheckptr
func (m *MPMCRing[T]) ReserveProducer() ProducerSlot[T] {
	_h := (*_mring)(unsafe.Pointer(m._head))

	var c *_melem[T]
	p := atomic.LoadUint64(&_h.w) // Get current write position

	for {
		slot := p & m._mask
		c = (*_melem[T])(unsafe.Pointer(m._data + unsafe.Sizeof(_melem[T]{})*uintptr(slot)))
		seq := atomic.LoadUint64(&c._seq)
		diff := seq - p

		// Check if we can claim this slot
		if diff == 0 {
			// Try to atomically claim the slot by incrementing the write position
			if atomic.CompareAndSwapUint64(&_h.w, p, p+1) {
				return ProducerSlot[T]{
					slot: slot,
					pos:  p,
					elem: c,
				}
			}
		} else if diff > 0 {
			// Another producer has claimed this slot, try again
			p = atomic.LoadUint64(&_h.w)
		} else {
			// This should never happen in a correctly implemented ring buffer
			panic("unreachable")
		}

		// Yield to other goroutines
		runtime.Gosched()
	}
}

// ReserveProducerWithContext is ReserveProducer with context cancellation
// before a producer slot is claimed.
//
// WARNING: Once this returns a valid slot, context cancellation is no longer
// checked. The returned slot must still be committed promptly.
//
//go:nocheckptr
func (m *MPMCRing[T]) ReserveProducerWithContext(ctx context.Context) (ProducerSlot[T], bool) {
	done := ctx.Done()
	_h := (*_mring)(unsafe.Pointer(m._head))

	var c *_melem[T]
	p := atomic.LoadUint64(&_h.w) // Get current write position

	for {
		// Check for context cancellation
		select {
		case <-done:
			return ProducerSlot[T]{}, false
		default:
		}

		slot := p & m._mask
		c = (*_melem[T])(unsafe.Pointer(m._data + unsafe.Sizeof(_melem[T]{})*uintptr(slot)))
		seq := atomic.LoadUint64(&c._seq)
		diff := seq - p

		// Check if we can claim this slot
		if diff == 0 {
			// Try to atomically claim the slot by incrementing the write position
			if atomic.CompareAndSwapUint64(&_h.w, p, p+1) {
				return ProducerSlot[T]{
					slot: slot,
					pos:  p,
					elem: c,
				}, true
			}
		} else if diff > 0 {
			// Another producer has claimed this slot, try again
			p = atomic.LoadUint64(&_h.w)
		} else {
			// This should never happen in a correctly implemented ring buffer
			panic("unreachable")
		}

		// Yield to other goroutines
		runtime.Gosched()
	}
}

// EnqueueZeroCopy claims one producer slot, passes the slot index and in-place
// element pointer to fn, then publishes the slot when fn returns.
//
// WARNING: The callback runs while the ring slot is claimed. It must return
// promptly; if it blocks or never returns, the whole ring can suffer
// head-of-line (HOL) blocking. Do not retain elem after the callback returns.
//
//go:nocheckptr
func (m *MPMCRing[T]) EnqueueZeroCopy(fn func(slot uint64, elem *T)) {
	_h := (*_mring)(unsafe.Pointer(m._head))

	var c *_melem[T]
	p := atomic.LoadUint64(&_h.w) // Get current write position

	for {
		slot := p & m._mask
		c = (*_melem[T])(unsafe.Pointer(m._data + unsafe.Sizeof(_melem[T]{})*uintptr(slot)))
		seq := atomic.LoadUint64(&c._seq)
		diff := seq - p

		// Check if we can claim this slot
		if diff == 0 {
			// Try to atomically claim the slot by incrementing the write position
			if atomic.CompareAndSwapUint64(&_h.w, p, p+1) {
				fn(slot, &c._data)
				atomic.StoreUint64(&c._seq, p+1)
				return
			}
		} else if diff > 0 {
			// Another producer has claimed this slot, try again
			p = atomic.LoadUint64(&_h.w)
		} else {
			// This should never happen in a correctly implemented ring buffer
			panic("unreachable")
		}

		// Yield to other goroutines
		runtime.Gosched()
	}
}

// EnqueueZeroCopyWithContext is EnqueueZeroCopy with context cancellation
// before a producer slot is claimed.
//
// WARNING: The callback runs while the ring slot is claimed. It must return
// promptly; if it blocks or never returns, the whole ring can suffer
// head-of-line (HOL) blocking. Do not retain elem after the callback returns.
// Context cancellation is not checked after the slot has been claimed.
//
//go:nocheckptr
func (m *MPMCRing[T]) EnqueueZeroCopyWithContext(ctx context.Context, fn func(slot uint64, elem *T)) bool {
	done := ctx.Done()
	_h := (*_mring)(unsafe.Pointer(m._head))

	var c *_melem[T]
	p := atomic.LoadUint64(&_h.w) // Get current write position

	for {
		// Check for context cancellation
		select {
		case <-done:
			return false
		default:
		}

		slot := p & m._mask
		c = (*_melem[T])(unsafe.Pointer(m._data + unsafe.Sizeof(_melem[T]{})*uintptr(slot)))
		seq := atomic.LoadUint64(&c._seq)
		diff := seq - p

		// Check if we can claim this slot
		if diff == 0 {
			// Try to atomically claim the slot by incrementing the write position
			if atomic.CompareAndSwapUint64(&_h.w, p, p+1) {
				fn(slot, &c._data)
				atomic.StoreUint64(&c._seq, p+1)
				return true
			}
		} else if diff > 0 {
			// Another producer has claimed this slot, try again
			p = atomic.LoadUint64(&_h.w)
		} else {
			// This should never happen in a correctly implemented ring buffer
			panic("unreachable")
		}

		// Yield to other goroutines
		runtime.Gosched()
	}
}

// DequeueWithContext removes an element from the ring buffer with context support
// This function will return false if the context is cancelled before an element can be dequeued.
//
// Parameters:
//   - ctx: Context for cancellation
//
// Returns the dequeued element and true, or zero value and false if context was cancelled
//
//go:nocheckptr
func (m *MPMCRing[T]) DequeueWithContext(ctx context.Context) (elem T, ok bool) {
	done := ctx.Done()
	_h := (*_mring)(unsafe.Pointer(m._head))

	var c *_melem[T]
	p := atomic.LoadUint64(&_h.r) // Get current read position

	for {
		// Check for context cancellation
		select {
		case <-done:
			ok = false
			return
		default:
		}

		// Get the element at the current read position
		c = (*_melem[T])(unsafe.Pointer(m._data + unsafe.Sizeof(_melem[T]{})*uintptr(p&m._mask)))
		seq := atomic.LoadUint64(&c._seq)
		diff := seq - (p + 1)

		// Check if this element is ready to be consumed
		if diff == 0 {
			// Try to atomically claim the slot by incrementing the read position
			if atomic.CompareAndSwapUint64(&_h.r, p, p+1) {
				// Memory barrier to ensure the read of c._data happens after
				// we've successfully claimed the slot
				atomic.LoadUint64(&c._seq) // This acts as a memory barrier
				break
			}
		} else if diff > 0 {
			// The element is not ready yet, try again
			p = atomic.LoadUint64(&_h.r)
		} else {
			// This should never happen in a correctly implemented ring buffer
			panic("unreachable")
		}

		// Yield to other goroutines
		runtime.Gosched()
	}

	// Copy the data from the claimed slot
	elem = c._data
	ok = true

	// Update the sequence number to make the slot available for producers
	atomic.StoreUint64(&c._seq, p+m._mask+1)
	return
}

// Dequeue removes an element from the ring buffer
// This function will block until an element is available.
//
// Returns the dequeued element
//
//go:nocheckptr
func (m *MPMCRing[T]) Dequeue() (elem T) {
	_h := (*_mring)(unsafe.Pointer(m._head))

	var c *_melem[T]
	p := atomic.LoadUint64(&_h.r) // Get current read position

	for {
		// Get the element at the current read position
		c = (*_melem[T])(unsafe.Pointer(m._data + unsafe.Sizeof(_melem[T]{})*uintptr(p&m._mask)))
		seq := atomic.LoadUint64(&c._seq)
		diff := seq - (p + 1)

		// Check if this element is ready to be consumed
		if diff == 0 {
			// Try to atomically claim the slot by incrementing the read position
			if atomic.CompareAndSwapUint64(&_h.r, p, p+1) {
				// Memory barrier to ensure the read of c._data happens after
				// we've successfully claimed the slot
				atomic.LoadUint64(&c._seq) // This acts as a memory barrier
				break
			}
		} else if diff > 0 {
			// The element is not ready yet, try again
			p = atomic.LoadUint64(&_h.r)
		} else {
			// This should never happen in a correctly implemented ring buffer
			panic("unreachable")
		}

		// Yield to other goroutines
		runtime.Gosched()
	}

	// Copy the data from the claimed slot
	elem = c._data

	// Update the sequence number to make the slot available for producers
	atomic.StoreUint64(&c._seq, p+m._mask+1)
	return
}

// DequeueFunc removes an element from the ring buffer and processes it with a function
// This allows for in-place processing of elements and can be more efficient than
// copying the element first and then processing it.
//
// Parameters:
//   - fn: Function that processes the element (receives a pointer to the element)
//
//go:nocheckptr
func (m *MPMCRing[T]) DequeueFunc(fn func(*T)) {
	_h := (*_mring)(unsafe.Pointer(m._head))

	var c *_melem[T]
	p := atomic.LoadUint64(&_h.r) // Get current read position

	for {
		// Get the element at the current read position
		c = (*_melem[T])(unsafe.Pointer(m._data + unsafe.Sizeof(_melem[T]{})*uintptr(p&m._mask)))
		seq := atomic.LoadUint64(&c._seq)
		diff := seq - (p + 1)

		// Check if this element is ready to be consumed
		if diff == 0 {
			// Try to atomically claim the slot by incrementing the read position
			if atomic.CompareAndSwapUint64(&_h.r, p, p+1) {
				// Memory barrier to ensure the read of c._data happens after
				// we've successfully claimed the slot
				atomic.LoadUint64(&c._seq) // This acts as a memory barrier
				break
			}
		} else if diff > 0 {
			// The element is not ready yet, try again
			p = atomic.LoadUint64(&_h.r)
		} else {
			// This should never happen in a correctly implemented ring buffer
			panic("unreachable")
		}

		// Yield to other goroutines
		runtime.Gosched()
	}

	// Process the data in-place using the provided function
	fn(&c._data)

	// Update the sequence number to make the slot available for producers
	atomic.StoreUint64(&c._seq, p+m._mask+1)
}

// ReserveConsumer claims one consumer slot and returns it to the caller.
//
// WARNING: The returned slot must be released promptly. Failing to call Release
// can cause head-of-line (HOL) blocking for the whole ring.
//
//go:nocheckptr
func (m *MPMCRing[T]) ReserveConsumer() ConsumerSlot[T] {
	_h := (*_mring)(unsafe.Pointer(m._head))

	var c *_melem[T]
	p := atomic.LoadUint64(&_h.r) // Get current read position

	for {
		slot := p & m._mask
		c = (*_melem[T])(unsafe.Pointer(m._data + unsafe.Sizeof(_melem[T]{})*uintptr(slot)))
		seq := atomic.LoadUint64(&c._seq)
		diff := seq - (p + 1)

		// Check if this element is ready to be consumed
		if diff == 0 {
			// Try to atomically claim the slot by incrementing the read position
			if atomic.CompareAndSwapUint64(&_h.r, p, p+1) {
				// Memory barrier to ensure the read of c._data happens after
				// we've successfully claimed the slot
				atomic.LoadUint64(&c._seq) // This acts as a memory barrier
				return ConsumerSlot[T]{
					slot: slot,
					pos:  p,
					mask: m._mask,
					elem: c,
				}
			}
		} else if diff > 0 {
			// The element is not ready yet, try again
			p = atomic.LoadUint64(&_h.r)
		} else {
			// This should never happen in a correctly implemented ring buffer
			panic("unreachable")
		}

		// Yield to other goroutines
		runtime.Gosched()
	}
}

// ReserveConsumerWithContext is ReserveConsumer with context cancellation
// before a consumer slot is claimed.
//
// WARNING: Once this returns a valid slot, context cancellation is no longer
// checked. The returned slot must still be released promptly.
//
//go:nocheckptr
func (m *MPMCRing[T]) ReserveConsumerWithContext(ctx context.Context) (ConsumerSlot[T], bool) {
	done := ctx.Done()
	_h := (*_mring)(unsafe.Pointer(m._head))

	var c *_melem[T]
	p := atomic.LoadUint64(&_h.r) // Get current read position

	for {
		// Check for context cancellation
		select {
		case <-done:
			return ConsumerSlot[T]{}, false
		default:
		}

		slot := p & m._mask
		c = (*_melem[T])(unsafe.Pointer(m._data + unsafe.Sizeof(_melem[T]{})*uintptr(slot)))
		seq := atomic.LoadUint64(&c._seq)
		diff := seq - (p + 1)

		// Check if this element is ready to be consumed
		if diff == 0 {
			// Try to atomically claim the slot by incrementing the read position
			if atomic.CompareAndSwapUint64(&_h.r, p, p+1) {
				// Memory barrier to ensure the read of c._data happens after
				// we've successfully claimed the slot
				atomic.LoadUint64(&c._seq) // This acts as a memory barrier
				return ConsumerSlot[T]{
					slot: slot,
					pos:  p,
					mask: m._mask,
					elem: c,
				}, true
			}
		} else if diff > 0 {
			// The element is not ready yet, try again
			p = atomic.LoadUint64(&_h.r)
		} else {
			// This should never happen in a correctly implemented ring buffer
			panic("unreachable")
		}

		// Yield to other goroutines
		runtime.Gosched()
	}
}

// DequeueZeroCopy claims one consumer slot, passes the slot index and in-place
// element pointer to fn, then releases the slot back to producers when fn
// returns.
//
// WARNING: The callback runs while the ring slot is claimed. It must return
// promptly; if it blocks or never returns, the whole ring can suffer
// head-of-line (HOL) blocking. Do not retain elem after the callback returns.
//
//go:nocheckptr
func (m *MPMCRing[T]) DequeueZeroCopy(fn func(slot uint64, elem *T)) {
	_h := (*_mring)(unsafe.Pointer(m._head))

	var c *_melem[T]
	p := atomic.LoadUint64(&_h.r) // Get current read position

	for {
		slot := p & m._mask
		c = (*_melem[T])(unsafe.Pointer(m._data + unsafe.Sizeof(_melem[T]{})*uintptr(slot)))
		seq := atomic.LoadUint64(&c._seq)
		diff := seq - (p + 1)

		// Check if this element is ready to be consumed
		if diff == 0 {
			// Try to atomically claim the slot by incrementing the read position
			if atomic.CompareAndSwapUint64(&_h.r, p, p+1) {
				// Memory barrier to ensure the read of c._data happens after
				// we've successfully claimed the slot
				atomic.LoadUint64(&c._seq) // This acts as a memory barrier
				fn(slot, &c._data)
				atomic.StoreUint64(&c._seq, p+m._mask+1)
				return
			}
		} else if diff > 0 {
			// The element is not ready yet, try again
			p = atomic.LoadUint64(&_h.r)
		} else {
			// This should never happen in a correctly implemented ring buffer
			panic("unreachable")
		}

		// Yield to other goroutines
		runtime.Gosched()
	}
}

// DequeueZeroCopyWithContext is DequeueZeroCopy with context cancellation
// before a consumer slot is claimed.
//
// WARNING: The callback runs while the ring slot is claimed. It must return
// promptly; if it blocks or never returns, the whole ring can suffer
// head-of-line (HOL) blocking. Do not retain elem after the callback returns.
// Context cancellation is not checked after the slot has been claimed.
//
//go:nocheckptr
func (m *MPMCRing[T]) DequeueZeroCopyWithContext(ctx context.Context, fn func(slot uint64, elem *T)) bool {
	done := ctx.Done()
	_h := (*_mring)(unsafe.Pointer(m._head))

	var c *_melem[T]
	p := atomic.LoadUint64(&_h.r) // Get current read position

	for {
		// Check for context cancellation
		select {
		case <-done:
			return false
		default:
		}

		slot := p & m._mask
		c = (*_melem[T])(unsafe.Pointer(m._data + unsafe.Sizeof(_melem[T]{})*uintptr(slot)))
		seq := atomic.LoadUint64(&c._seq)
		diff := seq - (p + 1)

		// Check if this element is ready to be consumed
		if diff == 0 {
			// Try to atomically claim the slot by incrementing the read position
			if atomic.CompareAndSwapUint64(&_h.r, p, p+1) {
				// Memory barrier to ensure the read of c._data happens after
				// we've successfully claimed the slot
				atomic.LoadUint64(&c._seq) // This acts as a memory barrier
				fn(slot, &c._data)
				atomic.StoreUint64(&c._seq, p+m._mask+1)
				return true
			}
		} else if diff > 0 {
			// The element is not ready yet, try again
			p = atomic.LoadUint64(&_h.r)
		} else {
			// This should never happen in a correctly implemented ring buffer
			panic("unreachable")
		}

		// Yield to other goroutines
		runtime.Gosched()
	}
}

// Magic number to identify initialized MPMC rings
// This is a random 64-bit value used to detect if a ring buffer has been initialized
const _mpmc_magic uint64 = 0xc9d8c1d43f096701

// _mpmcflag represents initialization flags for the ring buffer
type _mpmcflag uint64

const (
	_mpmc_reserved = _mpmcflag(1) << iota // Reserved flag for future use
	_mpmc_init                            // Ring is initialized flag
)

// Cache line size for padding to prevent false sharing
// 128 bits (16 bytes) padding to prevent cache line contention
const _CACHE_LINE = 16

// _mring represents the header structure for the MPMC ring buffer
// This structure is stored at the beginning of the shared memory region
type _mring struct {
	_magic uint64 // Magic number for initialization detection
	_size  uint64 // Size of the ring buffer (power of 2)
	_flag  uint64 // Initialization flags
	/* ======== Cache line boundary ======== */
	r   uint64                  // Read position (consumer index)
	_p0 [_CACHE_LINE - 4]uint64 // Padding to prevent false sharing
	w   uint64                  // Write position (producer index)
	_p1 [_CACHE_LINE - 1]uint64 // Padding to prevent false sharing
}

// _melem represents a single element in the MPMC ring buffer
// Each element contains the actual data and a sequence number for synchronization
type _melem[T any] struct {
	_data T      // The actual data stored in the element
	_seq  uint64 // Sequence number for synchronization
}

// _RoundUpPowerOf2 rounds up a number to the next power of 2
// This is required for efficient modulo operations using bit masking
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

// SizeMPMCRing calculates the total memory size required for an MPMC ring buffer
// This includes the header structure and all elements
//
// Parameters:
//   - len: Number of elements in the ring buffer
//
// Returns the total size in bytes
func SizeMPMCRing[T any](len uintptr) uintptr {
	return 256 + unsafe.Sizeof(_mring{}) + unsafe.Sizeof(_melem[T]{})*len
}
