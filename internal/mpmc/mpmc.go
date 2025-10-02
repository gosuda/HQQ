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

// DequeueWithContext removes an element from the ring buffer with context support
// This function will return false if the context is cancelled before an element can be dequeued.
//
// Parameters:
//   - ctx: Context for cancellation
//
// Returns the dequeued element and true, or zero value and false if context was cancelled
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
	return
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
