package rt

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"
)

var errPoolClosed = fmt.Errorf("hqqbg: pool is closed: %w", net.ErrClosed)

type idleEntry[T any] struct {
	val        T
	returnedAt time.Time
}

// Pool lends one Conn-like value to one goroutine at a time (8.2).
type Pool[T any] struct {
	dial    func(ctx context.Context) (T, error)
	closeFn func(T) error
	max     int

	mu          sync.Mutex
	idle        []idleEntry[T]
	n           int // idle values, lent values and dials in progress
	closed      bool
	waiters     []chan struct{}
	maxIdle     int           // surplus idle values above maxIdle expire after idleTimeout
	idleTimeout time.Duration // <= 0 disables timed eviction
	reapTimer   *time.Timer
}

// NewPool returns a Pool of at most max values; a max below 1 means 1. dial
// opens a value and closeFn closes one.
func NewPool[T any](max int, dial func(ctx context.Context) (T, error), closeFn func(T) error) *Pool[T] {
	if max < 1 {
		max = 1
	}
	return &Pool[T]{dial: dial, closeFn: closeFn, max: max}
}

// SetIdleTimeout configures eviction of idle values. At most maxIdle warm idle
// values are retained indefinitely; surplus idle values beyond maxIdle are
// closed once they have sat idle for idleTimeout. A non-positive idleTimeout
// disables timed eviction.
func (p *Pool[T]) SetIdleTimeout(maxIdle int, idleTimeout time.Duration) {
	if maxIdle < 0 {
		maxIdle = 0
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return
	}
	p.maxIdle = maxIdle
	p.idleTimeout = idleTimeout
	if p.reapTimer != nil {
		p.reapTimer.Stop()
		p.reapTimer = nil
	}
	if p.idleTimeout > 0 && len(p.idle) > p.maxIdle {
		p.armReapLocked(time.Now())
	}
}

// armReapLocked schedules the eviction timer for the oldest surplus idle entry.
// p.mu must be held.
func (p *Pool[T]) armReapLocked(now time.Time) {
	if p.closed || p.idleTimeout <= 0 || len(p.idle) <= p.maxIdle || p.reapTimer != nil {
		return
	}
	wait := p.idle[0].returnedAt.Add(p.idleTimeout).Sub(now)
	if wait < time.Millisecond {
		wait = time.Millisecond
	}
	p.reapTimer = time.AfterFunc(wait, p.reapIdle)
}

func (p *Pool[T]) reapIdle() {
	var expired []T
	p.mu.Lock()
	p.reapTimer = nil
	if !p.closed && p.idleTimeout > 0 {
		now := time.Now()
		cut := 0
		for len(p.idle)-cut > p.maxIdle && !p.idle[cut].returnedAt.Add(p.idleTimeout).After(now) {
			expired = append(expired, p.idle[cut].val)
			cut++
		}
		if cut > 0 {
			var zero idleEntry[T]
			remaining := len(p.idle) - cut
			copy(p.idle, p.idle[cut:])
			for i := remaining; i < len(p.idle); i++ {
				p.idle[i] = zero
			}
			p.idle = p.idle[:remaining]
			p.n -= cut
			for range cut {
				p.wakeLocked()
			}
		}
		if len(p.idle) > p.maxIdle {
			p.armReapLocked(now)
		}
	}
	p.mu.Unlock()
	for _, v := range expired {
		_ = p.closeFn(v)
	}
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
			last := len(p.idle) - 1
			v := p.idle[last].val
			p.idle[last] = idleEntry[T]{}
			p.idle = p.idle[:last]
			if len(p.idle) <= p.maxIdle && p.reapTimer != nil {
				p.reapTimer.Stop()
				p.reapTimer = nil
			}
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
		now := time.Now()
		p.idle = append(p.idle, idleEntry[T]{val: v, returnedAt: now})
		p.wakeLocked()
		if p.idleTimeout > 0 && len(p.idle) > p.maxIdle {
			p.armReapLocked(now)
		}
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
	if p.reapTimer != nil {
		p.reapTimer.Stop()
		p.reapTimer = nil
	}
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
	for _, e := range idle {
		err = errors.Join(err, p.closeFn(e.val))
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
