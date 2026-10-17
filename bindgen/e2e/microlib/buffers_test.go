//go:build unix

package microlib_test

import (
	"bytes"
	"errors"
	"fmt"
	"math/rand/v2"
	"slices"
	"strings"
	"testing"

	"gosuda.org/hqq/bindgen/rt"
)

// frameBody returns the body bytes one frame carries at buffer size s (BINDGEN.md 4.2: CHUNK = S - 16).
func frameBody(s int) int { return s - rt.HeaderSize }

// xorSizes returns the source lengths worth trying at buffer size s: around the point where the
// request body (12 bytes of fixed group and length, then src) stops fitting one frame, around the
// next few chunk boundaries, and the extremes of the IDL bound.
func xorSizes(s int) []int {
	chunk := frameBody(s)
	set := map[int]bool{0: true, 1: true, 7: true, 8: true, xorMax - 1: true, xorMax: true}
	for k := 1; k <= 3; k++ {
		edge := k*chunk - 12
		for d := -2; d <= 2; d++ {
			if n := edge + d; n >= 0 && n <= xorMax {
				set[n] = true
			}
		}
	}
	out := make([]int, 0, len(set))
	for n := range set {
		out = append(out, n)
	}
	slices.Sort(out)
	return out
}

// TestXorAllSizes drives ml_xor through every framing case at three buffer sizes: a request in one
// frame and in CONT frames (up to 65536 bytes, 16 or 586 frames), a response in place in the reserved
// slot and assembled in scratch, with the capacity at, above and below the length, and a capacity of
// 1 MiB whose response bound exceeds the ring (the client then issues the call synchronously). The
// result must be byte exact and the bytes of the destination beyond the result untouched.
func TestXorAllSizes(t *testing.T) {
	s := startServer(t, serverOpts{noTrace: true})
	for _, size := range []int{128, 4096, 16384} {
		t.Run(fmt.Sprintf("S=%d", size), func(t *testing.T) {
			c := dial(t, s, rt.Options{BufferSize: size})
			rng := rand.New(rand.NewPCG(uint64(size), 7))
			for _, n := range xorSizes(size) {
				src := make([]byte, n)
				for i := range src {
					src[i] = byte(rng.Uint32())
				}
				key := byte(rng.Uint32())
				for _, capacity := range []int{n, n + 1, n + frameBody(size), 1 << 20} {
					dst := bytes.Repeat([]byte{0xEE}, capacity)
					got, err := c.Xor(src, key, dst)
					if err != nil {
						t.Fatalf("Xor(n=%d, cap=%d): %v", n, capacity, err)
					}
					if !bytes.Equal(got, xorModel(src, key)) {
						t.Fatalf("Xor(n=%d, cap=%d): result differs from src ^ %#x at byte %d",
							n, capacity, key, firstDiff(got, xorModel(src, key)))
					}
					for i := n; i < len(dst); i++ {
						if dst[i] != 0xEE {
							t.Fatalf("Xor(n=%d, cap=%d) wrote dst[%d] beyond the result", n, capacity, i)
						}
					}
				}
				if n > 0 {
					// Too small a destination: the request is executed and the library reports failure.
					_, err := c.Xor(src, key, make([]byte, n-1))
					wantLib(t, fmt.Sprintf("Xor(n=%d, cap=n-1)", n), err, "ml_xor", -1, "")
				}
			}
			// After all that the session is in step.
			if got, err := c.Add(1, 2); err != nil || got != 3 {
				t.Fatalf("Add after the size sweep = (%d, %v)", got, err)
			}
		})
	}
}

// TestOversizeRequestWithoutResponseBody sends the largest request through CONT frames at the smallest
// buffer size and gets an answer that fits one frame: the library fails (capacity 0) after it read all
// 65536 bytes, which the trace proves.
func TestOversizeRequestWithoutResponseBody(t *testing.T) {
	s := startServer(t, serverOpts{})
	c := dial(t, s, rt.Options{BufferSize: 128})
	src := bytes.Repeat([]byte{0xA5}, xorMax)
	_, err := c.Xor(src, 1, nil)
	wantLib(t, "Xor(65536, cap 0)", err, "ml_xor", -1, "")
	wantNoErr(t, "Close", c.Close())
	if got := s.traceFuncs("ml_xor"); !slices.Equal(got, []string{"ml_xor n=65536 key=1 cap=0 ret=18446744073709551615"}) {
		t.Fatalf("library saw %q", got)
	}
}

// TestOversizeResponseFromSmallRequest: ml_cells answers up to 8192 bytes of a union array to a
// 8-byte request. At buffer sizes 128 and 256 the response bound needs 74 and 35 frames, and the
// shim's writer produces it in scratch; the client must reassemble and decode it.
func TestOversizeResponseFromSmallRequest(t *testing.T) {
	s := startServer(t, serverOpts{noTrace: true})
	for _, size := range []int{128, 256, 1024} {
		t.Run(fmt.Sprintf("S=%d", size), func(t *testing.T) {
			c := dial(t, s, rt.Options{BufferSize: size})
			ctr, err := c.CounterNew(0)
			wantNoErr(t, "CounterNew", err)
			for _, max := range []int32{1, 9, 10, 64, 256} {
				got, err := c.Cells(ctr, max, nil)
				wantNoErr(t, "Cells", err)
				if want := cellsModel(max); !slices.Equal(got, want) {
					t.Fatalf("Cells(%d) at S=%d differs from the model: %d cells, want %d", max, size, len(got), len(want))
				}
			}
		})
	}
}

// TestMessageLimit: the server's max_message is granted to the client, which refuses a request or a
// response bound above it before it commits anything.
func TestMessageLimit(t *testing.T) {
	s := startServer(t, serverOpts{noTrace: true, args: []string{"--max-message", "8192"}})
	c := dial(t, s, rt.Options{})
	if got := c.Conn().Limits().MaxMessage; got != 8192 {
		t.Fatalf("Limits().MaxMessage = %d, want the server's 8192", got)
	}
	big := make([]byte, 8192)
	_, err := c.Xor(big, 0, big)
	if !errors.Is(err, rt.ErrInvalidArg) {
		t.Fatalf("Xor with a 8192-byte src error = %v, want rt.ErrInvalidArg (body above max_message)", err)
	}
	ok := make([]byte, 4000)
	if _, err := c.Xor(ok, 3, ok); err != nil {
		t.Fatalf("Xor of 4000 bytes within the limit: %v", err)
	}
	_, err = c.Xor(ok[:10], 3, make([]byte, 8192)) // response bound 19 + 8192 > 8192
	if !errors.Is(err, rt.ErrInvalidArg) {
		t.Fatalf("Xor with cap 8192 error = %v, want rt.ErrInvalidArg (response bound above max_message)", err)
	}
	if got, err := c.Add(2, 2); err != nil || got != 4 {
		t.Fatalf("Add after the refusals = (%d, %v)", got, err)
	}
}

// TestStringsOfAllLengthsSurvive sends every length of a str in-parameter from 0 to its bound of 255.
func TestStringsOfAllLengthsSurvive(t *testing.T) {
	s := startServer(t, serverOpts{noTrace: true})
	c := dial(t, s, rt.Options{})
	for n := 0; n <= 255; n++ {
		in := strings.Repeat("\xff", n) // not valid UTF-8, bytes pass through unchanged
		got, err := c.Hash(in)
		wantNoErr(t, "Hash", err)
		if got != fnv1a(in) {
			t.Fatalf("Hash of %d 0xff bytes = %#x, want %#x", n, got, fnv1a(in))
		}
	}
}
