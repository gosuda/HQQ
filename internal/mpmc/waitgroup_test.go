package mpmc

import (
	"context"
	"errors"
	"runtime"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unsafe"
)

// aggRing formats an in-process ring of uintptr elements. The returned slice
// owns the memory and must be kept alive by the caller.
func aggRing(t testing.TB, size uint64) ([]uint64, *MPMCRing[uintptr]) {
	t.Helper()
	mem := make([]uint64, SizeMPMCRing[uintptr](uintptr(size))/8)
	p := unsafe.Pointer(&mem[0])
	Format[uintptr](p, size)
	r := Attach[uintptr](p, time.Second)
	if r == nil {
		t.Fatal("failed to attach ring")
	}
	return mem, r
}

// kernelParker is a Parker that records how many goroutines are inside Park at
// the same time per side. Park ignores its timeout (like a kernel semaphore it
// waits for a token, up to a generous cap) so that a waiter that is not woken
// shows up as a hang rather than being rescued by the ring's periodic re-check.
type kernelParker struct {
	tokens  [2]chan struct{}
	inPark  [2]atomic.Int32
	maxPark [2]atomic.Int32
	parks   [2]atomic.Int64
}

func newKernelParker() *kernelParker {
	return &kernelParker{tokens: [2]chan struct{}{
		make(chan struct{}, 1<<16), make(chan struct{}, 1<<16),
	}}
}

func (p *kernelParker) Park(side Side, _ time.Duration) {
	n := p.inPark[side].Add(1)
	for {
		m := p.maxPark[side].Load()
		if n <= m || p.maxPark[side].CompareAndSwap(m, n) {
			break
		}
	}
	p.parks[side].Add(1)
	select {
	case <-p.tokens[side]:
	case <-time.After(30 * time.Second):
	}
	p.inPark[side].Add(-1)
}

func (p *kernelParker) Wake(side Side, n uint32) {
	for ; n > 0; n-- {
		select {
		case p.tokens[side] <- struct{}{}:
		default:
			return
		}
	}
}

// groupOf returns the live park group of the given wait word, or nil.
func groupOf(word *uint32) *parkGroup {
	parkGroups.mu.Lock()
	defer parkGroups.mu.Unlock()
	return parkGroups.m[word]
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

// blockedConsumers is the number of goroutines parked (as leader or follower)
// on the ring's notEmpty word.
func blockedConsumers(r *MPMCRing[uintptr]) int {
	g := groupOf(&r.head.notEmpty)
	if g == nil {
		return 0
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	n := int(g.followers.Load())
	if g.leader {
		n++
	}
	return n
}

// withAggregationModes runs f once with a counting Parker and, where the
// platform has a process-local wait primitive, once with the native one.
func withAggregationModes(t *testing.T, f func(t *testing.T, r *MPMCRing[uintptr], kp *kernelParker)) {
	t.Run("parker", func(t *testing.T) {
		mem, r := aggRing(t, 256)
		defer runtime.KeepAlive(mem)
		kp := newKernelParker()
		r.SetParker(kp)
		f(t, r, kp)
	})
	t.Run("native", func(t *testing.T) {
		if !processLocalParking {
			t.Skip("no process-local wait primitive on this platform")
		}
		mem, r := aggRing(t, 256)
		defer runtime.KeepAlive(mem)
		f(t, r, nil)
	})
}

// TestParkGroupManyWaitersOneKernelPark blocks many consumers on one ring and
// checks that exactly one of them is parked in the kernel while the others
// wait process-locally, and that all of them wake when data arrives.
func TestParkGroupManyWaitersOneKernelPark(t *testing.T) {
	const waiters = 200
	withAggregationModes(t, func(t *testing.T, r *MPMCRing[uintptr], kp *kernelParker) {
		var wg sync.WaitGroup
		got := make(chan uintptr, waiters)
		for range waiters {
			wg.Add(1)
			go func() {
				defer wg.Done()
				got <- r.Dequeue()
			}()
		}

		// All waiters are blocked: one leader, every other goroutine a
		// follower.
		eventually(t, "all consumers to block", func() bool { return blockedConsumers(r) == waiters })
		g := groupOf(&r.head.notEmpty)
		if f := int(g.followers.Load()); f != waiters-1 {
			// The count is only exact while nobody is between two parks;
			// retry until it is.
			eventually(t, "waiters-1 followers", func() bool {
				return int(g.followers.Load()) == waiters-1
			})
		}
		if n := atomic.LoadUint32(&r.head.emptyWaiters); n != 1 {
			// The leader registers right after it is elected.
			eventually(t, "the leader to register", func() bool {
				return atomic.LoadUint32(&r.head.emptyWaiters) == 1
			})
		}
		if kp != nil {
			eventually(t, "the leader to park", func() bool { return kp.inPark[NotEmpty].Load() == 1 })
		}

		for i := uintptr(1); i <= waiters; i++ {
			r.Enqueue(i)
		}
		done := make(chan struct{})
		go func() { wg.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(60 * time.Second):
			t.Fatalf("only %d of %d blocked consumers were woken", len(got), waiters)
		}
		close(got)

		var vals []int
		for v := range got {
			vals = append(vals, int(v))
		}
		sort.Ints(vals)
		for i, v := range vals {
			if v != i+1 {
				t.Fatalf("consumers received %v, want 1..%d exactly once", vals, waiters)
			}
		}
		if kp != nil {
			if m := kp.maxPark[NotEmpty].Load(); m != 1 {
				t.Fatalf("up to %d goroutines were in the kernel park for one wait word, want 1", m)
			}
		}
		eventually(t, "the group to be released", func() bool { return groupOf(&r.head.notEmpty) == nil })
		if n := atomic.LoadUint32(&r.head.emptyWaiters); n != 0 {
			t.Fatalf("emptyWaiters = %d after every consumer returned, want 0", n)
		}
	})
}

// TestParkGroupPerWaiterCancellation cancels a subset of the blocked waiters
// (which may include the leader) and checks that exactly those return with
// ErrCanceled, while the others stay blocked and still receive data.
func TestParkGroupPerWaiterCancellation(t *testing.T) {
	const waiters = 60
	withAggregationModes(t, func(t *testing.T, r *MPMCRing[uintptr], kp *kernelParker) {
		type result struct {
			idx int
			val uintptr
			err error
		}
		results := make(chan result, waiters)
		cancels := make([]context.CancelFunc, waiters)
		for i := range waiters {
			ctx, cancel := context.WithCancel(context.Background())
			cancels[i] = cancel
			defer cancel()
			go func() {
				s, err := r.ReserveConsumerCancel(&Cancel{Ctx: ctx})
				res := result{idx: i, err: err}
				if err == nil {
					res.val = *s.Value()
					s.Release()
				}
				results <- res
			}()
		}
		eventually(t, "all consumers to block", func() bool { return blockedConsumers(r) == waiters })

		canceled := map[int]bool{}
		for i := 0; i < waiters; i += 2 {
			canceled[i] = true
			cancels[i]()
		}
		for range canceled {
			select {
			case res := <-results:
				if !canceled[res.idx] || !errors.Is(res.err, ErrCanceled) {
					t.Fatalf("waiter %d returned (%v, %v); only canceled waiters may return, with ErrCanceled", res.idx, res.val, res.err)
				}
			case <-time.After(30 * time.Second):
				t.Fatal("a canceled waiter never returned")
			}
		}
		select {
		case res := <-results:
			t.Fatalf("waiter %d returned (%v, %v) although it was neither canceled nor served", res.idx, res.val, res.err)
		default:
		}

		// The remaining waiters, including whichever took over leadership,
		// are all still served.
		remaining := waiters - len(canceled)
		for i := range remaining {
			r.Enqueue(uintptr(100 + i))
		}
		seen := map[uintptr]bool{}
		for i := range remaining {
			select {
			case res := <-results:
				if canceled[res.idx] || res.err != nil {
					t.Fatalf("waiter %d returned (%v, %v)", res.idx, res.val, res.err)
				}
				seen[res.val] = true
			case <-time.After(30 * time.Second):
				t.Fatalf("only %d of %d remaining waiters were served", i, remaining)
			}
		}
		if len(seen) != remaining {
			t.Fatalf("remaining waiters received %d distinct values, want %d", len(seen), remaining)
		}
		if kp != nil {
			if m := kp.maxPark[NotEmpty].Load(); m != 1 {
				t.Fatalf("up to %d goroutines were in the kernel park for one wait word, want 1", m)
			}
		}
	})
}

// TestParkGroupFollowerDeadlineAndClosed checks the cancellation sources a
// follower honors on its own while another goroutine leads: a deadline and a
// Closed channel.
func TestParkGroupFollowerDeadlineAndClosed(t *testing.T) {
	withAggregationModes(t, func(t *testing.T, r *MPMCRing[uintptr], kp *kernelParker) {
		leaderRes := make(chan uintptr, 1)
		go func() { leaderRes <- r.Dequeue() }()
		eventually(t, "the leader to block", func() bool { return blockedConsumers(r) == 1 })

		start := time.Now()
		timeoutErr := make(chan error, 1)
		go func() {
			_, err := r.ReserveConsumerCancel(&Cancel{Deadline: start.Add(50 * time.Millisecond)})
			timeoutErr <- err
		}()
		closed := make(chan struct{})
		closedErr := make(chan error, 1)
		go func() {
			closedErr <- r.DequeueZeroCopyCancel(&Cancel{Closed: closed}, func(uint64, *uintptr) {})
		}()
		eventually(t, "both followers to block", func() bool { return blockedConsumers(r) >= 2 })

		select {
		case err := <-timeoutErr:
			if !errors.Is(err, ErrTimeout) {
				t.Fatalf("deadline waiter err = %v, want ErrTimeout", err)
			}
		case <-time.After(30 * time.Second):
			t.Fatal("a follower with a deadline never returned")
		}
		close(closed)
		r.WakeAll()
		select {
		case err := <-closedErr:
			if !errors.Is(err, ErrClosed) {
				t.Fatalf("closed waiter err = %v, want ErrClosed", err)
			}
		case <-time.After(30 * time.Second):
			t.Fatal("a follower with a Closed channel never returned")
		}

		// The leader was unaffected by both departures.
		select {
		case v := <-leaderRes:
			t.Fatalf("the uncanceled waiter returned %d without data", v)
		default:
		}
		r.Enqueue(77)
		select {
		case v := <-leaderRes:
			if v != 77 {
				t.Fatalf("Dequeue = %d, want 77", v)
			}
		case <-time.After(30 * time.Second):
			t.Fatal("the leader was never woken")
		}
	})
}

// TestParkGroupFollowerBecomesLeader checks that when the only goroutine in
// the kernel leaves, a remaining follower takes over and is woken by a
// publish: the registration for the shared waiter count moves with the
// leadership.
func TestParkGroupFollowerBecomesLeader(t *testing.T) {
	withAggregationModes(t, func(t *testing.T, r *MPMCRing[uintptr], kp *kernelParker) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		leaderErr := make(chan error, 1)
		go func() {
			_, err := r.ReserveConsumerCancel(&Cancel{Ctx: ctx})
			leaderErr <- err
		}()
		eventually(t, "the first waiter to lead", func() bool { return blockedConsumers(r) == 1 })

		follower := make(chan uintptr, 1)
		go func() { follower <- r.Dequeue() }()
		eventually(t, "the follower to block", func() bool { return blockedConsumers(r) == 2 })

		cancel()
		select {
		case err := <-leaderErr:
			if !errors.Is(err, ErrCanceled) {
				t.Fatalf("leader err = %v, want ErrCanceled", err)
			}
		case <-time.After(30 * time.Second):
			t.Fatal("the canceled leader never returned")
		}
		// The follower re-parks as the new leader and registers.
		eventually(t, "the follower to take over", func() bool {
			g := groupOf(&r.head.notEmpty)
			if g == nil {
				return false
			}
			g.mu.Lock()
			defer g.mu.Unlock()
			return g.leader && g.followers.Load() == 0 && atomic.LoadUint32(&r.head.emptyWaiters) == 1
		})
		r.Enqueue(5)
		select {
		case v := <-follower:
			if v != 5 {
				t.Fatalf("Dequeue = %d, want 5", v)
			}
		case <-time.After(30 * time.Second):
			t.Fatal("the new leader was never woken")
		}
	})
}

// TestParkGroupFollowerCycleAllocs: a steady state of leader tenures with a
// follower behind them, each follower going through its wake-up channel and
// its timer, allocates nothing. Parking is allocation-free wherever the
// platform parks in process (Windows in particular has no other wait path).
func TestParkGroupFollowerCycleAllocs(t *testing.T) {
	if raceEnabled {
		t.Skip("sync.Pool drops items at random under the race detector")
	}
	g := new(parkGroup)
	w := &waiter{group: g}
	cycle := func() {
		if leader, _ := g.enter(); !leader {
			t.Error("the first caller of a group is not its leader")
		}
		_, wake := g.enter()
		g.exitLeader()
		w.follow(nil, wake, time.Hour)
	}
	for range 100 {
		cycle()
	}
	if n := testing.AllocsPerRun(1000, cycle); n != 0 {
		t.Fatalf("follower cycle allocates %v per operation, want 0", n)
	}
}
