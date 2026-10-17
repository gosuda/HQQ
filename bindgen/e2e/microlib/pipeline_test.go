//go:build unix

package microlib_test

import (
	"bytes"
	"errors"
	"fmt"
	"math/rand/v2"
	"testing"

	"gosuda.org/hqq/bindgen/e2e/microlib"
	"gosuda.org/hqq/bindgen/rt"
)

// TestPipelinedAdd issues 10,000 AddAsync calls with up to depth futures in flight and checks every
// result. depth 64 matches the default ring; depth 200 is far beyond B = 2, where the stub itself
// reads and buffers responses when flow control (BINDGEN.md 4.12) forbids another commit.
func TestPipelinedAdd(t *testing.T) {
	const calls = 10_000
	s := startServer(t, serverOpts{noTrace: true})
	for _, tc := range []struct {
		name         string
		buffers, mif int
		depth        int
	}{
		{"B=64 depth=64", 0, 0, 64},
		{"B=2 depth=2", 2, 0, 2},
		{"B=2 depth=200", 2, 0, 200},
		{"B=64 max_in_flight=1 depth=8", 0, 1, 8},
		{"B=8 depth=1", 8, 0, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := dial(t, s, rt.Options{BufferCount: tc.buffers, MaxInFlight: tc.mif})
			pending := make([]microlib.AddFuture, 0, tc.depth)
			next := 0 // index of the oldest pending call
			check := func(i int, f microlib.AddFuture) {
				got, err := f.Wait()
				if err != nil {
					t.Fatalf("Wait of call %d: %v", i, err)
				}
				if want := int32(i) + 3*int32(i); got != want {
					t.Fatalf("call %d = %d, want %d", i, got, want)
				}
			}
			for i := range calls {
				if len(pending) == tc.depth {
					check(next, pending[0])
					next++
					pending = append(pending[:0], pending[1:]...)
				}
				f, err := c.AddAsync(int32(i), 3*int32(i))
				if err != nil {
					t.Fatalf("AddAsync %d: %v", i, err)
				}
				pending = append(pending, f)
			}
			for _, f := range pending {
				check(next, f)
				next++
			}
			wantNoErr(t, "Close", c.Close())
		})
	}
}

// TestOutOfOrderWait waits for the futures of one session in reverse, random and interleaved order:
// each Wait reads and buffers the responses of the earlier calls, which a later Wait returns.
func TestOutOfOrderWait(t *testing.T) {
	s := startServer(t, serverOpts{noTrace: true})
	c := dial(t, s, rt.Options{})
	rng := rand.New(rand.NewPCG(11, 22))

	for round, order := range [][]int{reversed(32), shuffled(rng, 32), interleaved(32)} {
		fs := make([]microlib.AddFuture, 32)
		for i := range fs {
			f, err := c.AddAsync(int32(round*1000+i), 1)
			wantNoErr(t, "AddAsync", err)
			fs[i] = f
		}
		for _, i := range order {
			got, err := fs[i].Wait()
			wantNoErr(t, fmt.Sprintf("Wait of future %d", i), err)
			if want := int32(round*1000+i) + 1; got != want {
				t.Fatalf("round %d future %d = %d, want %d", round, i, got, want)
			}
		}
	}
}

func reversed(n int) []int {
	out := make([]int, n)
	for i := range out {
		out[i] = n - 1 - i
	}
	return out
}

func shuffled(rng *rand.Rand, n int) []int {
	out := rng.Perm(n)
	return out
}

func interleaved(n int) []int {
	var out []int
	for i := 0; i < n; i += 2 {
		out = append(out, i)
	}
	for i := 1; i < n; i += 2 {
		out = append(out, i)
	}
	return out
}

// TestOutOfOrderWaitMixedFunctions: futures of different functions, with different response shapes
// and sizes, waited in reverse. The buffered responses are copies and stay decodable.
func TestOutOfOrderWaitMixedFunctions(t *testing.T) {
	s := startServer(t, serverOpts{noTrace: true})
	c := dial(t, s, rt.Options{BufferSize: 1024})
	ctr, err := c.CounterNew(10)
	wantNoErr(t, "CounterNew", err)

	src := bytes.Repeat([]byte{7}, 3000)
	dst := make([]byte, len(src))
	fAdd, err := c.AddAsync(1, 2)
	wantNoErr(t, "AddAsync", err)
	fHash, err := c.HashAsync("pipeline")
	wantNoErr(t, "HashAsync", err)
	fXor, err := c.XorAsync(src, 9, dst)
	wantNoErr(t, "XorAsync", err)
	fCells, err := c.CellsAsync(ctr, 40, nil)
	wantNoErr(t, "CellsAsync", err)
	fAddC, err := c.CounterAddAsync(ctr, 5)
	wantNoErr(t, "CounterAddAsync", err)
	fCheck, err := c.CounterCheckAsync(ctr, 1)
	wantNoErr(t, "CounterCheckAsync", err)

	wantLib(t, "CounterCheckAsync", fCheck.Wait(), "ml_counter_check", -1, "over limit")
	total, err := fAddC.Wait()
	wantNoErr(t, "CounterAdd", err)
	if total != 15 {
		t.Fatalf("CounterAdd total = %d, want 15", total)
	}
	cells, err := fCells.Wait()
	wantNoErr(t, "Cells", err)
	if len(cells) != 40 || cells[0] != (microlib.Cell{Tag: microlib.CellInt, Int: -2}) {
		t.Fatalf("Cells = %d cells starting %+v", len(cells), cells[0])
	}
	got, err := fXor.Wait()
	wantNoErr(t, "Xor", err)
	if !bytes.Equal(got, xorModel(src, 9)) {
		t.Fatal("Xor result differs")
	}
	h, err := fHash.Wait()
	wantNoErr(t, "Hash", err)
	if h != fnv1a("pipeline") {
		t.Fatalf("Hash = %#x", h)
	}
	sum, err := fAdd.Wait()
	wantNoErr(t, "Add", err)
	if sum != 3 {
		t.Fatalf("Add = %d, want 3", sum)
	}
}

// TestResponseBoundAboveRingWhileCallsPending: a call whose response bound needs more frames than the
// ring holds (rf > B) must wait until no other call is unanswered, and nothing may deadlock. S = 128,
// B = 4: Xor with a 4096-byte capacity needs 37 response frames.
func TestResponseBoundAboveRingWhileCallsPending(t *testing.T) {
	s := startServer(t, serverOpts{noTrace: true})
	c := dial(t, s, rt.Options{BufferSize: 128, BufferCount: 4})
	var adds []microlib.AddFuture
	for i := range 3 {
		f, err := c.AddAsync(int32(i), 1)
		wantNoErr(t, "AddAsync", err)
		adds = append(adds, f)
	}
	src := bytes.Repeat([]byte{1}, 1000)
	dst := make([]byte, 4096)
	got, err := c.Xor(src, 2, dst)
	wantNoErr(t, "Xor with rf > B", err)
	if !bytes.Equal(got, xorModel(src, 2)) {
		t.Fatal("Xor result differs")
	}
	for i, f := range adds {
		v, err := f.Wait()
		wantNoErr(t, "Wait of an Add issued before the large call", err)
		if v != int32(i)+1 {
			t.Fatalf("Add %d = %d", i, v)
		}
	}
	// And in the other order: a large call pending, then small ones.
	fx, err := c.XorAsync(src, 3, dst)
	wantNoErr(t, "XorAsync", err)
	fa, err := c.AddAsync(5, 5)
	wantNoErr(t, "AddAsync after a synchronous-class call", err)
	if v, err := fa.Wait(); err != nil || v != 10 {
		t.Fatalf("Add = (%d, %v)", v, err)
	}
	if got, err := fx.Wait(); err != nil || !bytes.Equal(got, xorModel(src, 3)) {
		t.Fatalf("Xor = (%d bytes, %v)", len(got), err)
	}
}

// TestFutureMisuse: a future is single use; Sync discards the unclaimed ones.
func TestFutureMisuse(t *testing.T) {
	s := startServer(t, serverOpts{noTrace: true})
	c := dial(t, s, rt.Options{})

	f, err := c.AddAsync(1, 1)
	wantNoErr(t, "AddAsync", err)
	if v, err := f.Wait(); err != nil || v != 2 {
		t.Fatalf("Wait = (%d, %v)", v, err)
	}
	if _, err := f.Wait(); !errors.Is(err, rt.ErrFutureUsed) {
		t.Fatalf("second Wait error = %v, want rt.ErrFutureUsed", err)
	}

	g, err := c.AddAsync(2, 2)
	wantNoErr(t, "AddAsync", err)
	wantNoErr(t, "Sync", c.Conn().Sync())
	if _, err := g.Wait(); !errors.Is(err, rt.ErrFutureUsed) {
		t.Fatalf("Wait after Sync error = %v, want rt.ErrFutureUsed", err)
	}

	var zero microlib.AddFuture
	if _, err := zero.Wait(); !errors.Is(err, rt.ErrFutureUsed) {
		t.Fatalf("Wait of the zero future error = %v, want rt.ErrFutureUsed", err)
	}
	// The session is intact after all of it.
	if v, err := c.Add(20, 22); err != nil || v != 42 {
		t.Fatalf("Add after the misuse = (%d, %v)", v, err)
	}
}

// TestHandleFromPendingCall: a handle exists only after its Wait (no dependent calls,
// BINDGEN.md 8.3 item 7), so the zero Counter of a failed or pending call is refused.
func TestHandleFromPendingCall(t *testing.T) {
	s := startServer(t, serverOpts{noTrace: true})
	c := dial(t, s, rt.Options{})
	f, err := c.CounterNewAsync(5)
	wantNoErr(t, "CounterNewAsync", err)
	if _, err := c.CounterAdd(microlib.Counter{}, 1); !errors.Is(err, rt.ErrInvalidArg) {
		t.Fatalf("CounterAdd(handle of a pending call) error = %v, want rt.ErrInvalidArg", err)
	}
	ctr, err := f.Wait()
	wantNoErr(t, "Wait", err)
	if total, err := c.CounterAdd(ctr, 1); err != nil || total != 6 {
		t.Fatalf("CounterAdd = (%d, %v)", total, err)
	}
}
