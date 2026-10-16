package conformance

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"gosuda.org/hqq"
)

// ringShortDeadline bounds the Read and Write calls of ring cases: an empty
// or full ring answers ErrTimeout after this long.
const ringShortDeadline = 100 * time.Millisecond

// TestRingImages (G-07) joins each ring image as Secondary on caller memory
// and checks the results of successive Reads (ring 0) and Writes (ring 1)
// through the public API. The images are whole segments of the host page
// family; other families are skipped per case.
func TestRingImages(t *testing.T) {
	m := loadManifest(t)
	for _, rc := range m.Rings {
		t.Run(fmt.Sprintf("%s/page%d", rc.ID, rc.PageSize), func(t *testing.T) {
			if rc.PageSize != hostPage() {
				t.Skipf("image of page family %d; this host uses %d", rc.PageSize, hostPage())
			}
			t.Parallel()
			image := readVector(t, rc.File)
			mem := callerMemory(t, rc.BufferCount, rc.BufferSize, image)
			sec := joinSecondary(t, mem, rc.BufferCount, rc.BufferSize)
			if sec.Mode() != hqq.LinkModeSecondary {
				t.Fatalf("mode %v", sec.Mode())
			}
			buf := make([]byte, rc.BufferSize)
			for i, want := range rc.Dequeue {
				if err := sec.SetReadDeadline(time.Now().Add(ringShortDeadline)); err != nil {
					t.Fatal(err)
				}
				n, err := sec.Read(buf)
				checkRingResult(t, fmt.Sprintf("read %d", i), want, buf[:n], err)
			}
			for i, want := range rc.Write {
				if err := sec.SetWriteDeadline(time.Now().Add(ringShortDeadline)); err != nil {
					t.Fatal(err)
				}
				payload := Payload(rc.Seed, uint64(i), rc.BufferSize)
				n, err := sec.Write(payload)
				if want.Kind == "written" {
					if err != nil || n != want.N {
						t.Errorf("write %d: n=%d err=%v, want %d bytes", i, n, err, want.N)
					}
					continue
				}
				checkRingResult(t, fmt.Sprintf("write %d", i), want, nil, err)
			}
		})
	}
}

// checkRingResult compares one ring result with its expectation.
func checkRingResult(t *testing.T, what string, want RingWord, got []byte, err error) {
	t.Helper()
	switch want.Kind {
	case "message":
		if err != nil {
			t.Errorf("%s: error %v, want the message %s", what, err, want.Hex)
			return
		}
		if h := hexOf(got); h != want.Hex {
			t.Errorf("%s: got %s, want %s", what, h, want.Hex)
		}
	case "empty":
		if !errors.Is(err, hqq.ErrTimeout) {
			t.Errorf("%s: error %v (bytes %x), want ErrTimeout (empty)", what, err, got)
		}
	case "error":
		if !matchesKind(err, want.Error) {
			t.Errorf("%s: error %v, want %s", what, err, want.Error)
		}
	default:
		t.Fatalf("%s: unknown expectation %q", what, want.Kind)
	}
}

// TestRingAttachValidation (G-07) mutates one header word of a freshly
// generated ring and checks that the join fails with ErrIncompatibleLayout
// and that the secondary slot is released again.
func TestRingAttachValidation(t *testing.T) {
	m := loadManifest(t)
	var base *RingCase
	for i := range m.Rings {
		if m.Rings[i].ID == "formatted-empty" && m.Rings[i].PageSize == hostPage() {
			base = &m.Rings[i]
		}
	}
	if base == nil {
		t.Fatal("no formatted-empty ring case for the host page family")
	}
	image := readVector(t, base.File)
	reg, ok := ComputeRegions(hostPage(), base.BufferCount, base.BufferSize)
	if !ok {
		t.Fatal("no layout for the ring case")
	}
	mutations := []struct {
		name string
		ring int // 0 or 1
		off  int // header word offset
		val  uint64
	}{
		{"version", 0, RingVersionOff, 3},
		{"version of ring 1", 1, RingVersionOff, 1},
		{"stride", 0, RingStride, 256},
		{"size not a power of two", 0, RingSize, 3},
		{"size 1", 0, RingSize, 1},
		{"size differs from bufferCount", 0, RingSize, uint64(base.BufferCount * 2)},
		{"magic", 0, RingMagic, 0},
		{"flag without init", 1, RingFlag, 0},
	}
	for _, mu := range mutations {
		t.Run(mu.name, func(t *testing.T) {
			t.Parallel()
			mem := callerMemory(t, base.BufferCount, base.BufferSize, image)
			ringOff := reg.Ring0
			if mu.ring == 1 {
				ringOff = reg.Ring1
			}
			storeWord(mem, ringOff+mu.off, mu.val)
			sec, err := hqq.NewStandardLink(mem, base.BufferCount, base.BufferSize, testOpts())
			if sec != nil {
				_ = sec.Close()
				t.Fatalf("join succeeded on a mutated ring")
			}
			if !errors.Is(err, hqq.ErrIncompatibleLayout) {
				t.Fatalf("join error %v, want ErrIncompatibleLayout", err)
			}
			if tok := segmentWord(mem, SBEndpointSecond+RecToken); tok != TokenDetached {
				t.Errorf("secondary token after the failed join is %#x, want detached", tok)
			}
		})
	}
}
