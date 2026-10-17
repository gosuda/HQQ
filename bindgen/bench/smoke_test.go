//go:build cgo

package bench

import (
	"errors"
	"fmt"
	"math"
	"slices"
	"testing"

	"gosuda.org/hqq/bindgen/bench/cgobench"
	"gosuda.org/hqq/bindgen/bench/microbench"
	"gosuda.org/hqq/bindgen/rt"
)

// The tests in this file run every benchmarked operation through the cgo path and the generated
// client and compare both with an oracle written in Go (verify_test.go). They need the benchmark
// server (HQQ_BENCH_SERVER) except where the name says cgo only.

// statusDenied is HQQRPC_E_DENIED, what the interceptor of the benchmark server returns for a
// negative delta (hqq_rpc.h).
const statusDenied = 5

func TestRefWorkMatchesCgo(t *testing.T) {
	for _, iters := range []uint32{0, 1, 2, 3, 29, 1000, 4096} {
		if got, want := cgobench.Work(iters), refWork(iters); got != want {
			t.Errorf("cgo Work(%d) = %#x, Go reference %#x", iters, got, want)
		}
	}
	if got := refWork(0); got != workStart {
		t.Errorf("refWork(0) = %#x, want the start value %#x", got, uint64(workStart))
	}
}

func TestCgoXorMatchesReference(t *testing.T) {
	for _, n := range []int{0, 1, 63, 64, 1 << 10, 64 << 10} {
		src := xorSrc(n)
		dst := make([]byte, n)
		got, err := cgobench.Xor(src, 0x5A, dst)
		if err != nil {
			t.Fatalf("cgo Xor(%d bytes): %v", n, err)
		}
		if got != n || !slices.Equal(dst, refXor(src, 0x5A)) {
			t.Errorf("cgo Xor(%d bytes) wrote %d bytes that differ from the reference", n, got)
		}
	}
	if _, err := cgobench.Xor(xorSrc(8), 1, make([]byte, 7)); !errors.Is(err, cgobench.ErrCap) {
		t.Errorf("cgo Xor into a short buffer: error %v, want ErrCap", err)
	}
}

func TestCheckXorRejectsWrongResults(t *testing.T) {
	src := xorSrc(4096)
	good := refXor(src, 0x21)
	if err := checkXor(src, good, 0x21, false); err != nil {
		t.Fatalf("sampled check of a correct result: %v", err)
	}
	if err := checkXor(src, good, 0x21, true); err != nil {
		t.Fatalf("full check of a correct result: %v", err)
	}
	if err := checkXor(src, refXor(src, 0x20), 0x21, false); err == nil {
		t.Error("sampled check accepted the result of the previous call (stale buffer)")
	}
	if err := checkXor(src, good[:len(good)-1], 0x21, false); err == nil {
		t.Error("sampled check accepted a short result")
	}
	for _, i := range []int{0, 4095} { // the first and the last byte are among the samples
		bad := slices.Clone(good)
		bad[i] ^= 1
		if err := checkXor(src, bad, 0x21, false); err == nil {
			t.Errorf("sampled check accepted a result with byte %d wrong", i)
		}
	}
	bad := slices.Clone(good)
	bad[1000] ^= 1
	if err := checkXor(src, bad, 0x21, true); err == nil {
		t.Error("full check accepted a result with byte 1000 wrong")
	}
	if err := checkXor(nil, nil, 7, false); err != nil {
		t.Errorf("empty result: %v", err)
	}
}

func TestAddAgreesOnBothPaths(t *testing.T) {
	cl := client(t, clientCfg{})
	syncAfter(t, cl)
	for _, c := range [][2]int32{{0, 0}, {1, 2}, {-5, 3}, {math.MaxInt32, 1}, {math.MinInt32, -1}} {
		want := c[0] + c[1] // wraps like the library
		viaCgo := cgobench.Add(c[0], c[1])
		viaHQQ, err := cl.Add(c[0], c[1])
		if err != nil {
			t.Fatalf("Add(%d, %d): %v", c[0], c[1], err)
		}
		if viaCgo != want || viaHQQ != want {
			t.Errorf("Add(%d, %d): cgo %d, hqq %d, want %d", c[0], c[1], viaCgo, viaHQQ, want)
		}
	}
}

func TestWorkAgreesOnBothPaths(t *testing.T) {
	cl := client(t, clientCfg{})
	syncAfter(t, cl)
	for _, iters := range []uint32{0, 1, 7, 100, 1000} {
		want := refWork(iters)
		viaHQQ, err := cl.Work(iters)
		if err != nil {
			t.Fatalf("Work(%d): %v", iters, err)
		}
		if viaCgo := cgobench.Work(iters); viaCgo != want || viaHQQ != want {
			t.Errorf("Work(%d): cgo %#x, hqq %#x, want %#x", iters, viaCgo, viaHQQ, want)
		}
	}
}

func TestCounterAddAgreesOnBothPaths(t *testing.T) {
	cl := client(t, clientCfg{})
	syncAfter(t, cl)
	const start = math.MaxInt64 - 100
	for _, tc := range []struct {
		name  string
		start int64
		delta int64
		total int64 // result when the call succeeds
	}{
		{"small", 10, 5, 15},
		{"clamped", 10, 5_000_000, 10 + cgobench.MaxDelta},
		{"zero", 10, 0, 10},
		{"negative is denied", 10, -1, 0},
		{"overflow", start, 200, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cc, err := cgobench.NewCounter(tc.start)
			if err != nil {
				t.Fatal(err)
			}
			defer cc.Free()
			hc, err := cl.CounterNew(tc.start)
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := cl.CounterFree(hc); err != nil {
					t.Errorf("CounterFree: %v", err)
				}
			}()
			cgoTotal, cgoErr := cc.Add(tc.delta)
			hqqTotal, hqqErr := cl.CounterAdd(hc, tc.delta)
			switch tc.name {
			case "negative is denied":
				var se *rt.StatusError
				if !errors.Is(cgoErr, cgobench.ErrDenied) || !errors.As(hqqErr, &se) || se.Status != statusDenied {
					t.Fatalf("cgo error %v, hqq error %v; want the denial on both (status %d)", cgoErr, hqqErr, statusDenied)
				}
			case "overflow":
				var le *rt.LibError
				if !errors.Is(cgoErr, cgobench.ErrOverflow) || !errors.As(hqqErr, &le) || le.Code != -1 || le.Detail != "overflow" {
					t.Fatalf("cgo error %v, hqq error %v; want the overflow (-1, \"overflow\") on both", cgoErr, hqqErr)
				}
			default:
				if cgoErr != nil || hqqErr != nil {
					t.Fatalf("cgo error %v, hqq error %v", cgoErr, hqqErr)
				}
				if cgoTotal != tc.total || hqqTotal != tc.total {
					t.Fatalf("total: cgo %d, hqq %d, want %d", cgoTotal, hqqTotal, tc.total)
				}
			}
			// A refused or failed call leaves the total unchanged: the next add shows it.
			cgoNext, cgoErr := cc.Add(1)
			hqqNext, hqqErr := cl.CounterAdd(hc, 1)
			if cgoErr != nil || hqqErr != nil || cgoNext != hqqNext {
				t.Errorf("next add: cgo (%d, %v), hqq (%d, %v)", cgoNext, cgoErr, hqqNext, hqqErr)
			}
		})
	}
}

// xorConfigs are the sessions BenchmarkXor uses.
var xorConfigs = map[string]clientCfg{
	"default":        {},
	"copyreq":        {copyRequest: true},
	"bigbuf":         {bufferCount: bigBufferCount, bufferSize: bigBufferSize},
	"bigbuf-copyreq": {copyRequest: true, bufferCount: bigBufferCount, bufferSize: bigBufferSize},
}

func TestXorAgreesOnBothPaths(t *testing.T) {
	for name, cfg := range xorConfigs {
		t.Run(name, func(t *testing.T) {
			cl := client(t, cfg)
			syncAfter(t, cl)
			for _, sz := range []int{0, 1, 64, 1 << 10, 16 << 10, 16<<10 + 1, 64 << 10} {
				src := xorSrc(sz)
				const key = 0xC3
				want := refXor(src, key)
				cgoDst := make([]byte, sz)
				n, err := cgobench.Xor(src, key, cgoDst)
				if err != nil || !slices.Equal(cgoDst[:n], want) {
					t.Fatalf("cgo Xor(%d bytes): %d bytes, error %v, differs from the reference", sz, n, err)
				}
				// A destination larger than the source: the result is dst[:n].
				hqqDst := make([]byte, sz+5)
				got, err := cl.Xor(src, key, hqqDst)
				if err != nil {
					t.Fatalf("Xor(%d bytes): %v", sz, err)
				}
				if !slices.Equal(got, want) {
					t.Fatalf("Xor(%d bytes) differs from the reference", sz)
				}
				if sz > 0 {
					var le *rt.LibError
					if _, err := cl.Xor(src, key, make([]byte, sz-1)); !errors.As(err, &le) || le.Code != -1 {
						t.Fatalf("Xor(%d bytes) into a destination one byte short: error %v, want a *rt.LibError with SIZE_MAX (-1)", sz, err)
					}
				}
			}
		})
	}
}

func TestPipelinedCallsKeepTheirResults(t *testing.T) {
	cl := client(t, clientCfg{})
	syncAfter(t, cl)

	for _, depth := range []int{1, 8, 64} {
		t.Run(fmt.Sprintf("add-depth-%d", depth), func(t *testing.T) {
			const calls = 300
			futs := make([]microbench.AddFuture, 0, depth)
			next := 0 // oldest call not yet waited for
			for i := range calls {
				if len(futs) == depth {
					got, err := futs[0].Wait()
					if err != nil || got != addWant(next) {
						t.Fatalf("Add call %d = %d, %v; want %d", next, got, err, addWant(next))
					}
					futs, next = futs[1:], next+1
				}
				f, err := cl.AddAsync(addA(i), addB(i))
				if err != nil {
					t.Fatalf("AddAsync call %d: %v", i, err)
				}
				futs = append(futs, f)
			}
			for _, f := range futs {
				got, err := f.Wait()
				if err != nil || got != addWant(next) {
					t.Fatalf("Add call %d = %d, %v; want %d", next, got, err, addWant(next))
				}
				next++
			}
		})
	}

	t.Run("work-distinct-results", func(t *testing.T) {
		const calls = 32
		futs := make([]microbench.WorkFuture, calls)
		for i := range futs {
			f, err := cl.WorkAsync(uint32(i))
			if err != nil {
				t.Fatalf("WorkAsync(%d): %v", i, err)
			}
			futs[i] = f
		}
		for i, f := range futs {
			if got, err := f.Wait(); err != nil || got != refWork(uint32(i)) {
				t.Fatalf("Work(%d) = %#x, %v; want %#x", i, got, err, refWork(uint32(i)))
			}
		}
	})

	t.Run("out-of-order-waits", func(t *testing.T) {
		var futs [waitBatch]microbench.AddFuture
		for i := range futs {
			f, err := cl.AddAsync(addA(i), addB(i))
			if err != nil {
				t.Fatalf("AddAsync call %d: %v", i, err)
			}
			futs[i] = f
		}
		for i := waitBatch - 1; i >= 0; i-- {
			if got, err := futs[i].Wait(); err != nil || got != addWant(i) {
				t.Fatalf("Add call %d = %d, %v; want %d", i, got, err, addWant(i))
			}
		}
	})

	t.Run("counter-total-follows-order", func(t *testing.T) {
		ctr, err := cl.CounterNew(counterStart)
		if err != nil {
			t.Fatal(err)
		}
		defer func() {
			if err := cl.CounterFree(ctr); err != nil {
				t.Errorf("CounterFree: %v", err)
			}
		}()
		const calls = 40
		futs := make([]microbench.CounterAddFuture, calls)
		for i := range futs {
			f, err := cl.CounterAddAsync(ctr, counterDelta(i))
			if err != nil {
				t.Fatalf("CounterAddAsync call %d: %v", i, err)
			}
			futs[i] = f
		}
		want := int64(counterStart)
		for i, f := range futs {
			want += counterDelta(i)
			if got, err := f.Wait(); err != nil || got != want {
				t.Fatalf("CounterAdd call %d = %d, %v; want %d", i, got, err, want)
			}
		}
	})

	t.Run("xor-pipelined", func(t *testing.T) {
		for _, sz := range []int{64, 16 << 10, 64 << 10} {
			src := xorSrc(sz)
			const depth = 8
			var futs [depth]microbench.XorFuture
			var dsts [depth][]byte
			for i := range futs {
				dsts[i] = make([]byte, sz)
				f, err := cl.XorAsync(src, xorKey(i), dsts[i])
				if err != nil {
					t.Fatalf("XorAsync(%d bytes) call %d: %v", sz, i, err)
				}
				futs[i] = f
			}
			for i, f := range futs {
				got, err := f.Wait()
				if err == nil {
					err = checkXor(src, got, xorKey(i), true)
				}
				if err != nil {
					t.Fatalf("Xor(%d bytes) call %d: %v", sz, i, err)
				}
			}
		}
	})
}

func TestCallTimeoutSessionAgrees(t *testing.T) {
	cl := client(t, clientCfg{callTimeout: waitCallTimeout})
	syncAfter(t, cl)
	for i := range 20 {
		if got, err := cl.Add(addA(i), addB(i)); err != nil || got != addWant(i) {
			t.Fatalf("Add call %d = %d, %v; want %d", i, got, err, addWant(i))
		}
	}
}

func TestEchoRoundTrip(t *testing.T) {
	link := echoLink(t)
	for i := range 20 {
		msg := xorSrc(echoSize)
		msg[0] = byte(i)
		w, err := link.ReserveWrite()
		if err != nil {
			t.Fatalf("ReserveWrite %d: %v", i, err)
		}
		copy(w.Buffer(), msg)
		if err := w.Commit(echoSize); err != nil {
			t.Fatalf("Commit %d: %v", i, err)
		}
		r, err := link.ReserveRead()
		if err != nil {
			t.Fatalf("ReserveRead %d: %v", i, err)
		}
		got := slices.Clone(r.Buffer())
		if err := r.Release(); err != nil {
			t.Fatalf("Release %d: %v", i, err)
		}
		if !slices.Equal(got, msg) {
			t.Fatalf("echo %d differs from the message", i)
		}
	}
}
