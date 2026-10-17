package rt

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
)

var errPoolClosed = fmt.Errorf("hqqbg: pool is closed: %w", net.ErrClosed)

// Pool lends one Conn-like value to one goroutine at a time (8.2).
type Pool[T any] struct {
	dial    func(ctx context.Context) (T, error)
	closeFn func(T) error
	max     int

	mu      sync.Mutex
	idle    []T
	n       int // idle values, lent values and dials in progress
	closed  bool
	waiters []chan struct{}
}

// NewPool returns a Pool of at most max values; a max below 1 means 1. dial
// opens a value and closeFn closes one.
func NewPool[T any](max int, dial func(ctx context.Context) (T, error), closeFn func(T) error) *Pool[T] {
	if max < 1 {
		max = 1
	}
	return &Pool[T]{dial: dial, closeFn: closeFn, max: max}
}

// Get reuses an idle value or dials a new one while fewer than max exist. It
// waits for a Put or Discard when the pool is at its limit.
func (p *Pool[T]) Get(ctx context.Context) (T, error) {
	var zero T
	for {
		p.mu.Lock()
		switch {
		case p.closed:
			p.mu.Unlock()
			return zero, errPoolClosed
		case len(p.idle) > 0:
			v := p.idle[len(p.idle)-1]
			p.idle[len(p.idle)-1] = zero
			p.idle = p.idle[:len(p.idle)-1]
			p.mu.Unlock()
			return v, nil
		case p.n < p.max:
			p.n++
			p.mu.Unlock()
			return p.dialNew(ctx)
		}
		w := make(chan struct{}, 1)
		p.waiters = append(p.waiters, w)
		p.mu.Unlock()

		select {
		case <-w:
		case <-ctx.Done():
			p.mu.Lock()
			if !p.removeWaiter(w) {
				// A wake-up was already sent to us: pass it on.
				p.wakeLocked()
			}
			p.mu.Unlock()
			return zero, ctx.Err()
		}
	}
}

func (p *Pool[T]) dialNew(ctx context.Context) (T, error) {
	v, err := p.dial(ctx)
	if err != nil {
		p.mu.Lock()
		p.n--
		p.wakeLocked()
		p.mu.Unlock()
		var zero T
		return zero, err
	}
	p.mu.Lock()
	closed := p.closed
	p.mu.Unlock()
	if closed {
		var zero T
		err := errors.Join(errPoolClosed, p.discard(v))
		return zero, err
	}
	return v, nil
}

// Put returns a healthy value to the pool. If the pool is closed the value is
// closed instead.
func (p *Pool[T]) Put(v T) {
	p.mu.Lock()
	if !p.closed {
		p.idle = append(p.idle, v)
		p.wakeLocked()
		p.mu.Unlock()
		return
	}
	p.mu.Unlock()
	_ = p.discard(v)
}

// Discard closes a broken value and frees its slot.
func (p *Pool[T]) Discard(v T) {
	_ = p.discard(v)
}

func (p *Pool[T]) discard(v T) error {
	err := p.closeFn(v)
	p.mu.Lock()
	p.n--
	p.wakeLocked()
	p.mu.Unlock()
	return err
}

// Close closes the idle values and refuses further Gets. Values that are
// lent out are closed when they come back.
func (p *Pool[T]) Close() error {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil
	}
	p.closed = true
	idle := p.idle
	p.idle = nil
	p.n -= len(idle)
	waiters := p.waiters
	p.waiters = nil
	p.mu.Unlock()
	for _, w := range waiters {
		w <- struct{}{}
	}
	var err error
	for _, v := range idle {
		err = errors.Join(err, p.closeFn(v))
	}
	return err
}

// wakeLocked wakes the oldest waiter. p.mu must be held.
func (p *Pool[T]) wakeLocked() {
	if len(p.waiters) == 0 {
		return
	}
	w := p.waiters[0]
	copy(p.waiters, p.waiters[1:])
	p.waiters[len(p.waiters)-1] = nil
	p.waiters = p.waiters[:len(p.waiters)-1]
	w <- struct{}{}
}

// removeWaiter drops w from the wait list and reports whether it was there.
// p.mu must be held.
func (p *Pool[T]) removeWaiter(w chan struct{}) bool {
	for i, x := range p.waiters {
		if x == w {
			copy(p.waiters[i:], p.waiters[i+1:])
			p.waiters[len(p.waiters)-1] = nil
			p.waiters = p.waiters[:len(p.waiters)-1]
			return true
		}
	}
	return false
}
