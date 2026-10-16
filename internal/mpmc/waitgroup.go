package mpmc

import (
	"context"
	"sync"
	"sync/atomic"
	"time"
)

// Waiter aggregation.
//
// A parked waiter blocks in the kernel (futex, __ulock_wait, a Parker's
// semaphore, WaitOnAddress) and therefore holds an OS thread for as long as
// it is parked. Without aggregation N goroutines blocked on one ring would pin
// N threads. Instead, all blocked operations of this process that wait on the
// same wait word form a parkGroup and at most one of them, the leader, parks
// in the kernel. The leader alone is counted in the shared waiter count, so
// wakers in other processes see exactly the registration they always did and
// wake the kernel primitive the leader sleeps on. Every other waiter, a
// follower, blocks on a process-local channel together with its own
// cancellation sources (context, Closed channel, timer). When the leader
// returns from the kernel (wake-up, timeout, cancellation), it releases all
// followers; they re-check their condition and park again, where the first
// one to arrive becomes the next leader.
//
// This layer is process-local: it adds no shared state and nothing to the
// shared-memory ABI.
//
// Why no wake-up is lost. A follower checks its condition before it joins the
// group, and a publish (or release) that happens after that check either
// finds the leader registered in the shared waiter count and wakes it, or
// happens before the leader registers, in which case the leader's re-check
// after registering sees it and does not park. In both cases the leader
// returns and releases the follower. A follower that finds no active leader
// becomes the leader itself and runs the unmodified registration protocol.

// parkGroup is the per-wait-word state of the waiter aggregation.
type parkGroup struct {
	refs int // operations holding the group; guarded by parkGroups.mu

	mu      sync.Mutex
	leader  bool          // a goroutine is registered and parked for this word
	release chan struct{} // closed when the current leader returns; nil if no follower has joined

	// followers is the number of goroutines currently blocked behind the
	// leader. Only diagnostics and tests read it.
	followers atomic.Int32
}

// parkGroups indexes the live groups by wait word address. Groups only exist
// while an operation that reached the parking phase holds a reference, and are
// recycled through free so that steady-state parking does not allocate.
var parkGroups = struct {
	mu   sync.Mutex
	m    map[*uint32]*parkGroup
	free []*parkGroup
}{m: make(map[*uint32]*parkGroup)}

const maxFreeParkGroups = 64

func acquireParkGroup(word *uint32) *parkGroup {
	parkGroups.mu.Lock()
	g := parkGroups.m[word]
	if g == nil {
		if n := len(parkGroups.free); n > 0 {
			g = parkGroups.free[n-1]
			parkGroups.free[n-1] = nil
			parkGroups.free = parkGroups.free[:n-1]
		} else {
			g = new(parkGroup)
		}
		parkGroups.m[word] = g
	}
	g.refs++
	parkGroups.mu.Unlock()
	return g
}

func releaseParkGroup(word *uint32, g *parkGroup) {
	parkGroups.mu.Lock()
	if g.refs--; g.refs == 0 {
		delete(parkGroups.m, word)
		if len(parkGroups.free) < maxFreeParkGroups {
			parkGroups.free = append(parkGroups.free, g)
		}
	}
	parkGroups.mu.Unlock()
}

// enter joins the group. The first caller while no leader is active becomes
// the leader (and must call exitLeader when it returns from the kernel); every
// other caller gets the channel that is closed when the current leader
// returns.
func (g *parkGroup) enter() (leader bool, release <-chan struct{}) {
	g.mu.Lock()
	if !g.leader {
		g.leader = true
		g.mu.Unlock()
		return true, nil
	}
	if g.release == nil {
		g.release = make(chan struct{})
	}
	ch := g.release
	g.mu.Unlock()
	return false, ch
}

// exitLeader ends the leader's tenure and releases all followers.
func (g *parkGroup) exitLeader() {
	g.mu.Lock()
	g.leader = false
	if g.release != nil {
		close(g.release)
		g.release = nil
	}
	g.mu.Unlock()
}

// armContext registers the context wake-up callback once per operation. The
// callback wakes the kernel primitive so that a leader parked on behalf of a
// canceled context returns. Followers do not need it: they select on the
// context themselves.
func (w *waiter) armContext(c *Cancel, site waitSite) {
	if w.armed {
		return
	}
	w.armed = true
	if c != nil && c.Ctx != nil && c.Ctx.Done() != nil {
		stopped := make(chan struct{})
		w.stopped = stopped
		w.stop = context.AfterFunc(c.Ctx, func() {
			defer close(stopped)
			site.wake()
		})
	}
}

// park blocks until the leader of the wait word returns (follower), or parks
// in the kernel on behalf of every waiter of this process (leader), for at
// most timeout. Callers re-check their condition afterwards.
func (w *waiter) park(c *Cancel, site waitSite, seq *uint64, target uint64, timeout time.Duration) {
	if w.group == nil {
		w.group = acquireParkGroup(site.word)
		w.word = site.word
	}
	leader, release := w.group.enter()
	if !leader {
		w.follow(c, release, timeout)
		return
	}
	defer w.group.exitLeader()

	w.armContext(c, site)
	atomic.AddUint32(site.waiters, 1)
	defer atomic.AddUint32(site.waiters, ^uint32(0))
	v := atomic.LoadUint32(site.word)
	if stillBlocked(seq, target) && poll(c) == nil {
		if site.parker != nil {
			site.parker.Park(site.side, timeout)
		} else {
			futexWait(site.word, v, timeout)
		}
	}
}

// follow waits behind the leader until it returns, c is canceled or timeout
// elapses, whichever comes first.
func (w *waiter) follow(c *Cancel, release <-chan struct{}, timeout time.Duration) {
	var done, closed <-chan struct{}
	if c != nil {
		if c.Ctx != nil {
			done = c.Ctx.Done()
		}
		closed = c.Closed
	}
	if w.timer == nil {
		w.timer = time.NewTimer(timeout)
	} else {
		w.timer.Reset(timeout)
	}
	w.group.followers.Add(1)
	select {
	case <-release:
	case <-done:
	case <-closed:
	case <-w.timer.C:
	}
	w.group.followers.Add(-1)
	w.timer.Stop()
}
