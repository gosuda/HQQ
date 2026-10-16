package conformance

import (
	"encoding/binary"
	"testing"
	"time"

	"gosuda.org/hqq"
)

// TestPeerStateTransitions (G-09) drives the peer monitor through the public
// PeerState: not connected, connected after the join, closed when the Primary
// detaches, gone when the Primary's token is replaced, when its heartbeat
// stalls, or when the segment's epoch changes under the endpoint (SPEC 6.3).
// The Primary's record words that a peer would write are stored by the test,
// as a peer would store them (atomically).
func TestPeerStateTransitions(t *testing.T) {
	const count, size = 2, 8
	total := hqq.SizeStandardLink(count, size)

	newPair := func(t *testing.T) (mem []byte, prim *hqq.StandardLink, sec *hqq.StandardLink) {
		t.Helper()
		mem = hqq.AlignedBuffer(total)
		var err error
		prim, err = hqq.NewStandardLink(mem, count, size, testOpts())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = prim.Close() })
		sec = joinSecondary(t, mem, count, size)
		if got := sec.PeerState(); got != hqq.PeerConnected {
			t.Fatalf("after join: peer state %v, want connected", got)
		}
		return mem, prim, sec
	}

	t.Run("not-connected-before-join", func(t *testing.T) {
		t.Parallel()
		mem := hqq.AlignedBuffer(total)
		prim, err := hqq.NewStandardLink(mem, count, size, testOpts())
		if err != nil {
			t.Fatal(err)
		}
		defer prim.Close()
		if got := prim.PeerState(); got != hqq.PeerNotConnected {
			t.Fatalf("primary before any secondary: peer state %v, want not connected", got)
		}
	})

	t.Run("connected-then-closed", func(t *testing.T) {
		t.Parallel()
		_, prim, sec := newPair(t)
		if err := prim.Close(); err != nil {
			t.Fatal(err)
		}
		if got := sec.PeerState(); got != hqq.PeerClosed {
			t.Fatalf("after the primary detached: peer state %v, want closed", got)
		}
	})

	t.Run("closed-is-terminal", func(t *testing.T) {
		t.Parallel()
		mem, prim, sec := newPair(t)
		if err := prim.Close(); err != nil {
			t.Fatal(err)
		}
		if got := sec.PeerState(); got != hqq.PeerClosed {
			t.Fatalf("after the primary detached: peer state %v, want closed", got)
		}
		storeWord(mem, SBEndpointPrimary+RecToken, 0x9999999999999999)
		if got := sec.PeerState(); got != hqq.PeerClosed {
			t.Fatalf("closed is not terminal: peer state %v", got)
		}
	})

	t.Run("replaced-token-is-gone", func(t *testing.T) {
		t.Parallel()
		mem, _, sec := newPair(t)
		storeWord(mem, SBEndpointPrimary+RecToken, 0x9999999999999999)
		if got := sec.PeerState(); got != hqq.PeerGone {
			t.Fatalf("after the primary token was replaced: peer state %v, want gone", got)
		}
	})

	t.Run("epoch-changed-is-gone", func(t *testing.T) {
		t.Parallel()
		mem, _, sec := newPair(t)
		// A re-initialization under this endpoint changes the epoch (SPEC
		// 5.3): the monitor must treat the peer as gone.
		epoch := binary.LittleEndian.Uint64(mem[SBEpoch:])
		storeWord(mem, SBEpoch, epoch+1)
		if got := sec.PeerState(); got != hqq.PeerGone {
			t.Fatalf("after the epoch changed: peer state %v, want gone", got)
		}
	})

	t.Run("stalled-heartbeat-is-gone", func(t *testing.T) {
		t.Parallel()
		mem, _, sec := newPair(t)
		// Heartbeats become enabled (interval 10 ms) and the one value written
		// is never refreshed: the monitor must declare the peer gone after
		// max(PeerTimeout, 4 x interval) without a change (SPEC 6.3).
		storeWord(mem, SBEndpointPrimary+RecInterval, uint64(10*time.Millisecond))
		storeWord(mem, SBEndpointPrimary+RecHeartbeat, 12345)
		deadline := time.Now().Add(3 * time.Second)
		for sec.PeerState() != hqq.PeerGone {
			if time.Now().After(deadline) {
				t.Fatalf("peer with a stalled heartbeat still %v after 3 s", sec.PeerState())
			}
			time.Sleep(5 * time.Millisecond)
		}
	})
}
