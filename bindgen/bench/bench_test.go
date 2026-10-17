//go:build cgo

package bench

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"testing"
	"time"

	"gosuda.org/hqq/bindgen/bench/cgobench"
	"gosuda.org/hqq/bindgen/bench/microbench"
)

// The benchmarks of BINDGEN.md 10.5. Conventions:
//
//   - Every loop checks every result against an oracle (verify_test.go), in the timed region for
//     the cgo and the hqq leaves alike, so a fast path that is wrong cannot look fast.
//   - One benchmark iteration is one call: ns/op is the time per call and calls/s its inverse.
//   - "hqq-sync" is the generated synchronous method. "hqq-pipe-N" keeps N calls in flight: it
//     waits for call i-N, in order, then issues call i with the Async method. Pipeline depths are
//     powers of two, so the slot of call i is i & (N-1). The calls still in flight when the loop
//     ends are waited for with the timer running again (timedTail), so the elapsed time covers all
//     the calls and not only their issue.
//   - The loops use b.Loop, inside which b.N is 0: each keeps its own call counter i.
//   - The hqq leaves share one session per configuration (client in harness_test.go) and never
//     dial inside the timed region.

// reportCalls adds the calls/s metric for n calls timed in b.
func reportCalls(b *testing.B, n int) {
	b.Helper()
	b.ReportMetric(float64(n)/b.Elapsed().Seconds(), "calls/s")
}

// timedTail runs f, the wait for the calls still in flight, with the timer on. b.Loop stops the
// timer when the loop ends.
func timedTail(b *testing.B, f func()) {
	b.Helper()
	b.StartTimer()
	f()
	b.StopTimer()
}

func pipeName(depth int) string { return fmt.Sprintf("hqq-pipe-%d", depth) }

// ---- Add ----

func BenchmarkAdd(b *testing.B) {
	b.Run("cgo", benchAddCgo)
	b.Run("hqq-sync", benchAddSync)
	for _, depth := range []int{8, 32, 64} {
		b.Run(pipeName(depth), func(b *testing.B) { benchAddPipe(b, depth) })
	}
}

func benchAddCgo(b *testing.B) {
	b.ReportAllocs()
	i := 0
	for b.Loop() {
		if got := cgobench.Add(addA(i), addB(i)); got != addWant(i) {
			b.Fatalf("cgo Add(%d, %d) = %d, want %d", addA(i), addB(i), got, addWant(i))
		}
		i++
	}
	reportCalls(b, i)
}

func benchAddSync(b *testing.B) {
	cl := client(b, clientCfg{})
	syncAfter(b, cl)
	b.ReportAllocs()
	i := 0
	for b.Loop() {
		got, err := cl.Add(addA(i), addB(i))
		if err != nil {
			b.Fatalf("Add call %d: %v", i, err)
		}
		if got != addWant(i) {
			b.Fatalf("Add(%d, %d) = %d, want %d", addA(i), addB(i), got, addWant(i))
		}
		i++
	}
	reportCalls(b, i)
}

// benchAddPipe keeps depth Add calls in flight. depth is a power of two at most maxDepth.
func benchAddPipe(b *testing.B, depth int) {
	cl := client(b, clientCfg{})
	syncAfter(b, cl)
	var futs [maxDepth]microbench.AddFuture
	mask := depth - 1
	wait := func(j int) {
		got, err := futs[j&mask].Wait()
		if err != nil {
			b.Fatalf("Add call %d: %v", j, err)
		}
		if got != addWant(j) {
			b.Fatalf("Add call %d = %d, want %d", j, got, addWant(j))
		}
	}
	b.ReportAllocs()
	i := 0
	for b.Loop() {
		if i >= depth {
			wait(i - depth)
		}
		f, err := cl.AddAsync(addA(i), addB(i))
		if err != nil {
			b.Fatalf("AddAsync call %d: %v", i, err)
		}
		futs[i&mask] = f
		i++
	}
	timedTail(b, func() {
		for j := max(0, i-depth); j < i; j++ {
			wait(j)
		}
	})
	reportCalls(b, i)
}

// ---- Work: the crossover sweep ----

// workIters are the loop counts of the sweep. ml_work(0) is the cost of a trivial call; each
// iteration of the loop is a serial dependency chain, so the cost grows linearly. README.md says how
// to read the iteration count as nanoseconds on the machine at hand (the cgo column).
var workIters = []uint32{0, 8, 16, 32, 64, 128, 256, 512, 1024, 2048}

// workDepths are the pipeline depths of the hqq leaves; depth 1 waits for each call before it issues
// the next, the synchronous pattern with the Async methods.
var workDepths = []int{1, 4, 16, 64}

func BenchmarkWork(b *testing.B) {
	for _, iters := range workIters {
		want := refWork(iters)
		b.Run(fmt.Sprintf("iters=%d", iters), func(b *testing.B) {
			b.Run("cgo", func(b *testing.B) { benchWorkCgo(b, iters, want) })
			b.Run("hqq-sync", func(b *testing.B) { benchWorkSync(b, iters, want) })
			for _, depth := range workDepths {
				b.Run(pipeName(depth), func(b *testing.B) { benchWorkPipe(b, iters, want, depth) })
			}
		})
	}
}

func benchWorkCgo(b *testing.B, iters uint32, want uint64) {
	b.ReportAllocs()
	i := 0
	for b.Loop() {
		if got := cgobench.Work(iters); got != want {
			b.Fatalf("cgo Work(%d) = %#x, want %#x", iters, got, want)
		}
		i++
	}
	reportCalls(b, i)
}

func benchWorkSync(b *testing.B, iters uint32, want uint64) {
	cl := client(b, clientCfg{})
	syncAfter(b, cl)
	b.ReportAllocs()
	i := 0
	for b.Loop() {
		got, err := cl.Work(iters)
		if err != nil {
			b.Fatalf("Work call %d: %v", i, err)
		}
		if got != want {
			b.Fatalf("Work(%d) call %d = %#x, want %#x", iters, i, got, want)
		}
		i++
	}
	reportCalls(b, i)
}

func benchWorkPipe(b *testing.B, iters uint32, want uint64, depth int) {
	cl := client(b, clientCfg{})
	syncAfter(b, cl)
	var futs [maxDepth]microbench.WorkFuture
	mask := depth - 1
	wait := func(j int) {
		got, err := futs[j&mask].Wait()
		if err != nil {
			b.Fatalf("Work call %d: %v", j, err)
		}
		if got != want {
			b.Fatalf("Work(%d) call %d = %#x, want %#x", iters, j, got, want)
		}
	}
	b.ReportAllocs()
	i := 0
	for b.Loop() {
		if i >= depth {
			wait(i - depth)
		}
		f, err := cl.WorkAsync(iters)
		if err != nil {
			b.Fatalf("WorkAsync call %d: %v", i, err)
		}
		futs[i&mask] = f
		i++
	}
	timedTail(b, func() {
		for j := max(0, i-depth); j < i; j++ {
			wait(j)
		}
	})
	reportCalls(b, i)
}

// ---- CounterAdd: handle, out parameter and interceptor ----

// counterStart is the initial total of the counters of the CounterAdd leaves.
const counterStart = 1000

func BenchmarkCounterAdd(b *testing.B) {
	b.Run("cgo", benchCounterAddCgo)
	b.Run("hqq-sync", func(b *testing.B) { benchCounterAddHQQ(b, 0) })
	for _, depth := range []int{8, 64} {
		b.Run(pipeName(depth), func(b *testing.B) { benchCounterAddHQQ(b, depth) })
	}
}

func benchCounterAddCgo(b *testing.B) {
	c, err := cgobench.NewCounter(counterStart)
	if err != nil {
		b.Fatalf("cgo NewCounter: %v", err)
	}
	defer c.Free()
	want := int64(counterStart)
	b.ReportAllocs()
	i := 0
	for b.Loop() {
		want += counterDelta(i)
		got, err := c.Add(counterDelta(i))
		if err != nil {
			b.Fatalf("cgo Counter.Add call %d: %v", i, err)
		}
		if got != want {
			b.Fatalf("cgo Counter.Add call %d = %d, want %d", i, got, want)
		}
		i++
	}
	reportCalls(b, i)
}

// benchCounterAddHQQ runs CounterAdd on a counter of its own: depth 0 is the synchronous method,
// otherwise depth calls are in flight. The running total of the library is the oracle: a lost,
// repeated or reordered call changes it.
func benchCounterAddHQQ(b *testing.B, depth int) {
	cl := client(b, clientCfg{})
	syncAfter(b, cl)
	ctr, err := cl.CounterNew(counterStart)
	if err != nil {
		b.Fatalf("CounterNew: %v", err)
	}
	defer func() {
		if err := cl.CounterFree(ctr); err != nil {
			b.Errorf("CounterFree: %v", err)
		}
	}()
	want := int64(counterStart)
	b.ReportAllocs()
	i := 0
	if depth == 0 {
		for b.Loop() {
			want += counterDelta(i)
			got, err := cl.CounterAdd(ctr, counterDelta(i))
			if err != nil {
				b.Fatalf("CounterAdd call %d: %v", i, err)
			}
			if got != want {
				b.Fatalf("CounterAdd call %d = %d, want %d", i, got, want)
			}
			i++
		}
		reportCalls(b, i)
		return
	}
	var futs [maxDepth]microbench.CounterAddFuture
	mask := depth - 1
	wait := func(j int) {
		want += counterDelta(j)
		got, err := futs[j&mask].Wait()
		if err != nil {
			b.Fatalf("CounterAdd call %d: %v", j, err)
		}
		if got != want {
			b.Fatalf("CounterAdd call %d = %d, want %d", j, got, want)
		}
	}
	for b.Loop() {
		if i >= depth {
			wait(i - depth)
		}
		f, err := cl.CounterAddAsync(ctr, counterDelta(i))
		if err != nil {
			b.Fatalf("CounterAddAsync call %d: %v", i, err)
		}
		futs[i&mask] = f
		i++
	}
	timedTail(b, func() {
		for j := max(0, i-depth); j < i; j++ {
			wait(j)
		}
	})
	reportCalls(b, i)
}

// ---- Xor: buffers in and out ----

// xorVariant is one hqq column of BenchmarkXor.
//
// The default session has S = 16 KiB: 64 B and 1 KiB messages are one frame in place, 16 KiB and
// 64 KiB messages are chunked (4.8). The "bigbuf" session has S = 256 KiB, so every size is one
// frame in place. "copyreq" is a server started with --copy-request (7.2): the server copies the
// request body out of the ring before it calls the library, the open question of 12.2.
type xorVariant struct {
	name  string
	cfg   clientCfg
	depth int // 0: synchronous
}

var xorVariants = []xorVariant{
	{"hqq-sync", clientCfg{}, 0},
	{"hqq-sync-copyreq", clientCfg{copyRequest: true}, 0},
	{"hqq-sync-bigbuf", clientCfg{bufferCount: bigBufferCount, bufferSize: bigBufferSize}, 0},
	{"hqq-sync-bigbuf-copyreq", clientCfg{copyRequest: true, bufferCount: bigBufferCount, bufferSize: bigBufferSize}, 0},
	{"hqq-pipe-8", clientCfg{}, 8},
	{"hqq-pipe-8-copyreq", clientCfg{copyRequest: true}, 8},
}

var xorSizes = []struct {
	name string
	n    int
}{{"64B", 64}, {"1KiB", 1 << 10}, {"16KiB", 16 << 10}, {"64KiB", 64 << 10}}

// xorFull reports whether call i gets the full byte-for-byte comparison; every other call compares
// eight bytes spread over the result (checkXor). The last call of a leaf is always compared in full:
// the pipelined leaves do it in the tail, the others in one more call after the timed loop.
func xorFull(i int) bool { return i == 0 || i&1023 == 0 }

func BenchmarkXor(b *testing.B) {
	for _, sz := range xorSizes {
		b.Run(sz.name, func(b *testing.B) {
			src := xorSrc(sz.n)
			b.Run("cgo", func(b *testing.B) { benchXorCgo(b, src) })
			for _, v := range xorVariants {
				b.Run(v.name, func(b *testing.B) { benchXorHQQ(b, src, v) })
			}
		})
	}
}

func benchXorCgo(b *testing.B, src []byte) {
	dst := make([]byte, len(src))
	b.SetBytes(int64(len(src)))
	b.ReportAllocs()
	i := 0
	for b.Loop() {
		key := xorKey(i)
		n, err := cgobench.Xor(src, key, dst)
		if err != nil {
			b.Fatalf("cgo Xor call %d: %v", i, err)
		}
		if err := checkXor(src, dst[:n], key, xorFull(i)); err != nil {
			b.Fatalf("cgo Xor call %d: %v", i, err)
		}
		i++
	}
	reportCalls(b, i)
	key := xorKey(i)
	n, err := cgobench.Xor(src, key, dst)
	if err == nil {
		err = checkXor(src, dst[:n], key, true)
	}
	if err != nil {
		b.Fatalf("cgo Xor final call: %v", err)
	}
}

func benchXorHQQ(b *testing.B, src []byte, v xorVariant) {
	cl := client(b, v.cfg)
	syncAfter(b, cl)
	b.SetBytes(int64(len(src)))
	i := 0
	if v.depth == 0 {
		dst := make([]byte, len(src))
		b.ReportAllocs()
		for b.Loop() {
			key := xorKey(i)
			got, err := cl.Xor(src, key, dst)
			if err != nil {
				b.Fatalf("Xor call %d: %v", i, err)
			}
			if err := checkXor(src, got, key, xorFull(i)); err != nil {
				b.Fatalf("Xor call %d: %v", i, err)
			}
			i++
		}
		reportCalls(b, i)
		key := xorKey(i)
		got, err := cl.Xor(src, key, dst)
		if err == nil {
			err = checkXor(src, got, key, true)
		}
		if err != nil {
			b.Fatalf("Xor final call: %v", err)
		}
		return
	}
	depth := v.depth
	mask := depth - 1
	var futs [maxDepth]microbench.XorFuture
	dsts := make([][]byte, depth)
	for s := range dsts {
		dsts[s] = make([]byte, len(src))
	}
	wait := func(j int, full bool) {
		got, err := futs[j&mask].Wait()
		if err != nil {
			b.Fatalf("Xor call %d: %v", j, err)
		}
		if err := checkXor(src, got, xorKey(j), full); err != nil {
			b.Fatalf("Xor call %d: %v", j, err)
		}
	}
	b.ReportAllocs()
	for b.Loop() {
		if i >= depth {
			wait(i-depth, xorFull(i-depth))
		}
		f, err := cl.XorAsync(src, xorKey(i), dsts[i&mask])
		if err != nil {
			b.Fatalf("XorAsync call %d: %v", i, err)
		}
		futs[i&mask] = f
		i++
	}
	timedTail(b, func() {
		for j := max(0, i-depth); j < i; j++ {
			wait(j, xorFull(j) || j == i-1)
		}
	})
	reportCalls(b, i)
}

// ---- RoundTrip: the link next to the generated call ----

// echoSize is the payload of the raw round trip, the 64 bytes of 1.3.
const echoSize = 64

func BenchmarkRoundTrip(b *testing.B) {
	b.Run("echo", benchEcho)
	b.Run("hqq-sync", benchAddSync)
}

// benchEcho is a Standard link round trip to the C hqq_echo server: reserve, fill, commit, then
// reserve, compare and release the echo, the calls rt makes without the framing.
func benchEcho(b *testing.B) {
	link := echoLink(b)
	msg := bytes.Repeat([]byte{0xA5}, echoSize)
	b.ReportAllocs()
	i := 0
	for b.Loop() {
		binary.LittleEndian.PutUint64(msg, uint64(i))
		w, err := link.ReserveWrite()
		if err != nil {
			b.Fatalf("echo %d: ReserveWrite: %v", i, err)
		}
		copy(w.Buffer(), msg)
		if err := w.Commit(echoSize); err != nil {
			b.Fatalf("echo %d: Commit: %v", i, err)
		}
		r, err := link.ReserveRead()
		if err != nil {
			b.Fatalf("echo %d: ReserveRead: %v", i, err)
		}
		ok := bytes.Equal(r.Buffer(), msg)
		if err := r.Release(); err != nil {
			b.Fatalf("echo %d: Release: %v", i, err)
		}
		if !ok {
			b.Fatalf("echo %d: reply differs from the message", i)
		}
		i++
	}
	reportCalls(b, i)
}

// ---- WaitCosts: the per-call costs listed in 1.3 item 4 ----

// waitBatch is the number of calls of one batch of BenchmarkWaitCosts: issued together, then waited
// for. All three leaves run the same batches, so they differ only in the order of the waits and in
// CallTimeout.
const waitBatch = 8

// waitCallTimeout is the CallTimeout of the callTimeout leaf. It is long enough never to fire; the
// cost measured is the SetReadDeadline of every Wait (6.3).
const waitCallTimeout = time.Minute

func BenchmarkWaitCosts(b *testing.B) {
	b.Run("in-order", func(b *testing.B) { benchWaitBatches(b, clientCfg{}, false) })
	// The first Wait of a reverse batch finds the responses of the calls issued before it unread:
	// rt copies them out of the ring into per-call buffers (8.3).
	b.Run("out-of-order", func(b *testing.B) { benchWaitBatches(b, clientCfg{}, true) })
	b.Run("callTimeout", func(b *testing.B) { benchWaitBatches(b, clientCfg{callTimeout: waitCallTimeout}, false) })
}

func benchWaitBatches(b *testing.B, cfg clientCfg, reverse bool) {
	cl := client(b, cfg)
	syncAfter(b, cl)
	var futs [waitBatch]microbench.AddFuture
	// waitAll waits for the k calls of the batch that starts at call first.
	waitAll := func(first, k int) {
		for n := range k {
			s := n
			if reverse {
				s = k - 1 - n
			}
			got, err := futs[s].Wait()
			if err != nil {
				b.Fatalf("Add call %d: %v", first+s, err)
			}
			if got != addWant(first+s) {
				b.Fatalf("Add call %d = %d, want %d", first+s, got, addWant(first+s))
			}
		}
	}
	b.ReportAllocs()
	i, k := 0, 0 // calls issued, calls in the open batch
	for b.Loop() {
		f, err := cl.AddAsync(addA(i), addB(i))
		if err != nil {
			b.Fatalf("AddAsync call %d: %v", i, err)
		}
		futs[k] = f
		i++
		if k++; k == waitBatch {
			waitAll(i-k, k)
			k = 0
		}
	}
	timedTail(b, func() { waitAll(i-k, k) })
	reportCalls(b, i)
}
