package mpmc

import "time"

// Semaphore is a counting semaphore shared between processes, such as a named
// kernel semaphore.
type Semaphore interface {
	// Wait blocks until a token is available, consuming it, or timeout
	// elapses.
	Wait(timeout time.Duration)
	// Release adds n tokens. It must not block and ignores a full semaphore.
	Release(n uint32)
}

// SemaphoreParker is a Parker backed by one Semaphore per Side.
type SemaphoreParker struct {
	notEmpty, notFull Semaphore
}

// NewSemaphoreParker returns a Parker that parks consumers on notEmpty and
// producers on notFull. Every process attached to the ring must use
// semaphores that refer to the same underlying objects.
func NewSemaphoreParker(notEmpty, notFull Semaphore) *SemaphoreParker {
	return &SemaphoreParker{notEmpty: notEmpty, notFull: notFull}
}

func (p *SemaphoreParker) sem(side Side) Semaphore {
	if side == NotEmpty {
		return p.notEmpty
	}
	return p.notFull
}

// Park implements Parker.
func (p *SemaphoreParker) Park(side Side, timeout time.Duration) {
	p.sem(side).Wait(timeout)
}

// Wake implements Parker.
func (p *SemaphoreParker) Wake(side Side, n uint32) {
	p.sem(side).Release(n)
}
