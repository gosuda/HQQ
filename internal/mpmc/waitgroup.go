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

	mu     sync.Mutex
	leader bool // a goroutine is registered and parked for this word

	// wakes holds the wake-up channel of every follower blocked behind the
	// current leader. The leader sends one value on each when it returns and
	// empties the list. Channels are per follower and recycled through
	// wakeChans, so parking allocates nothing in steady state.
	wakes []chan struct{}

	// followers is the number of goroutines currently blocked behind the
	// leader. Only diagnostics and tests read it.
	followers atomic.Int32
}

// wakeChans recycles the follower wake-up channels. Every channel in the pool
// is empty: it holds no pending release.
var wakeChans = sync.Pool{New: func() any { return make(chan struct{}, 1) }}

// followerTimers recycles the timers of followers. Every timer in the pool is
// stopped and its channel is drained; Reset is safe on it because timers have
// synchronous channels (go.mod declares go 1.23 or later).
var followerTimers sync.Pool

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
// other caller gets the channel on which the current leader releases it when
// it returns. The caller must call leave with that channel afterwards.
func (g *parkGroup) enter() (leader bool, wake chan struct{}) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.leader {
		g.leader = true
		return true, nil
	}
	wake = wakeChans.Get().(chan struct{})
	g.wakes = append(g.wakes, wake)
	return false, wake
}

// exitLeader ends the leader's tenure and releases all followers. The wake
// channels are buffered and empty, so the sends never block.
func (g *parkGroup) exitLeader() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.leader = false
	for i, ch := range g.wakes {
		ch <- struct{}{}
		g.wakes[i] = nil
	}
	g.wakes = g.wakes[:0]
}

// leave ends a follower's wait on wake, whatever woke it. A follower that the
// leader has not released yet withdraws its channel from the group; otherwise
// the leader's release is consumed here if the follower did not receive it.
// Either way the channel goes back to wakeChans empty.
func (g *parkGroup) leave(wake chan struct{}) {
	g.mu.Lock()
	for i, ch := range g.wakes {
		if ch == wake {
			last := len(g.wakes) - 1
			g.wakes[i] = g.wakes[last]
			g.wakes[last] = nil
			g.wakes = g.wakes[:last]
			g.mu.Unlock()
			wakeChans.Put(wake)
			return
		}
	}
	g.mu.Unlock()
	// The leader released this follower under g.mu, so the value is in the
	// channel unless the select above already took it.
	select {
	case <-wake:
	default:
	}
	wakeChans.Put(wake)
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
			site.wakeAll()
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
	leader, wake := w.group.enter()
	if !leader {
		w.follow(c, wake, timeout)
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

// follow waits behind the leader until it releases wake, c is canceled or
// timeout elapses, whichever comes first.
func (w *waiter) follow(c *Cancel, wake chan struct{}, timeout time.Duration) {
	var done, closed <-chan struct{}
	if c != nil {
		if c.Ctx != nil {
			done = c.Ctx.Done()
		}
		closed = c.Closed
	}
	timer := getFollowerTimer(timeout)
	w.group.followers.Add(1)
	select {
	case <-wake:
	case <-done:
	case <-closed:
	case <-timer.C:
	}
	w.group.followers.Add(-1)
	timer.Stop()
	followerTimers.Put(timer)
	w.group.leave(wake)
}

// getFollowerTimer returns a timer that fires after d, recycled if possible.
func getFollowerTimer(d time.Duration) *time.Timer {
	if t, ok := followerTimers.Get().(*time.Timer); ok {
		t.Reset(d)
		return t
	}
	return time.NewTimer(d)
}
