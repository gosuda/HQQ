package hqq

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"
)

// TestStandardLinkExclusiveSuite reruns the Standard link tests that use at
// most one writing and one reading goroutine per link with every
// newMemoryPair link opened in exclusive mode: Write, Read, ReadFrom, the
// zero-copy calls, reservations (several outstanding ones included),
// tombstones, panics inside callbacks, deadlines and partial reads.
func TestStandardLinkExclusiveSuite(t *testing.T) {
	forceExclusive = true
	defer func() { forceExclusive = false }()
	if _, p, _ := newMemoryPair(t, 2, 64, LinkOptions{}); !p.txRing.Exclusive() || !p.rxRing.Exclusive() {
		t.Fatal("newMemoryPair does not open exclusive links while forceExclusive is set")
	}

	for _, tc := range []struct {
		name string
		fn   func(*testing.T)
	}{
		{"ReadWrite", TestStandardLinkReadWrite},
		{"BidirectionalReadWrite", TestStandardLinkBidirectionalReadWrite},
		{"DoesNotOverwriteUnreadBuffers", TestStandardLinkDoesNotOverwriteUnreadBuffers},
		{"PartialReadPreservesRemainderAndReleasesBuffer", TestStandardLinkPartialReadPreservesRemainderAndReleasesBuffer},
		{"ZeroCopyReadWrite", TestStandardLinkZeroCopyReadWrite},
		{"ReserveCommitReadRelease", TestStandardLinkReserveCommitReadRelease},
		{"DispatchModesCanMix", TestStandardLinkDispatchModesCanMix},
		{"ReserveAbortTombstoneIsSkipped", TestStandardLinkReserveAbortTombstoneIsSkipped},
		{"ReserveInvalidCommitPublishesTombstone", TestStandardLinkReserveInvalidCommitPublishesTombstone},
		{"WriteReservationHoldsSlotUntilAbort", TestStandardLinkWriteReservationHoldsSlotUntilAbort},
		{"ReadReservationHoldsSlotUntilRelease", TestStandardLinkReadReservationHoldsSlotUntilRelease},
		{"Deadlines", TestStandardLinkDeadlines},
		{"HighVolumeOrderedDelivery", TestStandardLinkHighVolumeOrderedDelivery},
		{"BufferOverflow", TestStandardLinkBufferOverflow},
		{"PacketConn", TestStandardLinkPacketConn},
		{"WriteReservationCopyCannotCompleteTwice", TestWriteReservationCopyCannotCompleteTwice},
		{"ReadReservationCopyCannotReleaseTwice", TestReadReservationCopyCannotReleaseTwice},
		{"WriteZeroCopyPanicPublishesTombstone", TestWriteZeroCopyPanicPublishesTombstone},
		{"ReadZeroCopyPanicReleasesSlot", TestReadZeroCopyPanicReleasesSlot},
		{"ReadFromIsDatagram", TestReadFromIsDatagram},
		{"RemainderTailsNeverMerge", TestRemainderTailsNeverMerge},
		{"PartialReadRemainderReusesBuffer", TestPartialReadRemainderReusesBuffer},
		{"PeerClosedIsReportedAfterBufferedData", TestPeerClosedIsReportedAfterBufferedData},
		{"CloseWakesBlockedRead", TestStandardLinkCloseWakesBlockedRead},
	} {
		t.Run(tc.name, tc.fn)
	}
}

// TestStandardLinkExclusiveOptionReachesRings: LinkOptions.Exclusive switches
// both rings of the endpoint, and only when it is set.
func TestStandardLinkExclusiveOptionReachesRings(t *testing.T) {
	for _, exclusive := range []bool{false, true} {
		l := newTestStandardLink(t, 4, 64, LinkOptions{Exclusive: exclusive})
		for name, ring := range map[string]interface{ Exclusive() bool }{
			"tx": l.txRing, "rx": l.rxRing, "ring0": l.ring0, "ring1": l.ring1,
		} {
			if got := ring.Exclusive(); got != exclusive {
				t.Errorf("Exclusive=%v: %s ring exclusive = %v", exclusive, name, got)
			}
		}
	}
}

// exchangeStreams runs one writer and one reader goroutine per link (one
// stream each way) and checks that each stream arrives in order and intact.
func exchangeStreams(t *testing.T, a, b *StandardLink, n int) {
	t.Helper()
	const size = 24
	var wg sync.WaitGroup
	stream := func(tag byte, from, to *StandardLink) {
		wg.Add(2)
		go func() {
			defer wg.Done()
			msg := make([]byte, size)
			msg[8] = tag
			for i := range n {
				binary.LittleEndian.PutUint64(msg, uint64(i))
				if _, err := from.Write(msg); err != nil {
					t.Errorf("stream %d write %d: %v", tag, i, err)
					return
				}
			}
		}()
		go func() {
			defer wg.Done()
			buf := make([]byte, size)
			for i := range n {
				if _, err := to.Read(buf); err != nil {
					t.Errorf("stream %d read %d: %v", tag, i, err)
					return
				}
				if got := binary.LittleEndian.Uint64(buf); got != uint64(i) || buf[8] != tag {
					t.Errorf("stream %d: message %d arrived as (%d, tag %d)", tag, i, got, buf[8])
					return
				}
			}
		}()
	}
	stream(1, a, b)
	stream(2, b, a)
	wg.Wait()
}

// TestStandardLinkExclusiveInteroperatesWithCAS: an exclusive endpoint talks
// to a CAS endpoint, in both role assignments and in both directions at the
// same time, over a ring small enough to wrap many times.
func TestStandardLinkExclusiveInteroperatesWithCAS(t *testing.T) {
	n := 20000
	if raceEnabled {
		n = 2000
	}
	for _, tc := range []struct {
		name                         string
		primaryExclusive, secondaryX bool
	}{
		{"exclusive-primary/cas-secondary", true, false},
		{"cas-primary/exclusive-secondary", false, true},
		{"exclusive-primary/exclusive-secondary", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mem := AlignedBuffer(SizeStandardLink(8, 64))
			primary, err := NewStandardLink(mem, 8, 64, LinkOptions{Exclusive: tc.primaryExclusive})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = primary.Close() })
			secondary, err := NewStandardLink(mem, 8, 64, LinkOptions{Exclusive: tc.secondaryX})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = secondary.Close() })
			exchangeStreams(t, primary, secondary, n)
		})
	}
}

// TestNamedStandardLinkExclusive: the option travels through the named
// constructors (shared memory, spin policy), and an exclusive creator works
// with a CAS opener.
func TestNamedStandardLinkExclusive(t *testing.T) {
	name := newPortableSharedMemoryName()
	creator, err := CreateStandardLink(name, 8, 64, 0o600, LinkOptions{Exclusive: true, WaitPolicy: WaitSpin})
	if err != nil {
		t.Fatalf("CreateStandardLink: %v", err)
	}
	defer creator.Close()
	opener, err := OpenStandardLink(name, 8, 64, LinkOptions{Exclusive: true, WaitPolicy: WaitSpin})
	if err != nil {
		t.Fatalf("OpenStandardLink: %v", err)
	}
	defer opener.Close()
	if !creator.txRing.Exclusive() || !creator.rxRing.Exclusive() || !opener.txRing.Exclusive() || !opener.rxRing.Exclusive() {
		t.Fatal("named constructors dropped LinkOptions.Exclusive")
	}
	exchangeStreams(t, creator.StandardLink, opener.StandardLink, 2000)
}

// TestAdvancedConstructorsRejectExclusive: an AdvancedLink sends from many
// goroutines, so every Advanced entry point that takes LinkOptions refuses
// Exclusive with ErrExclusiveUnsupported before it touches any memory or
// name.
func TestAdvancedConstructorsRejectExclusive(t *testing.T) {
	opts := AdvancedOptions{Link: LinkOptions{Exclusive: true}}
	cfg := Config{BufferCount: 8, BufferSize: 64, Advanced: opts}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	check := func(what string, err error) {
		t.Helper()
		if !errors.Is(err, ErrExclusiveUnsupported) {
			t.Errorf("%s: error = %v, want ErrExclusiveUnsupported", what, err)
		}
		if !errors.Is(err, ErrInvalidOp) {
			t.Errorf("%s: error %v does not match ErrInvalidOp", what, err)
		}
	}

	mem := AlignedBuffer(SizeStandardLink(8, 64))
	_, err := NewAdvancedLink(mem, 8, 64, opts)
	check("NewAdvancedLink", err)
	for _, b := range mem[:pagesize] {
		if b != 0 {
			t.Fatal("NewAdvancedLink touched the memory before rejecting the option")
		}
	}

	name := newPortableSharedMemoryName()
	_, err = CreateAdvancedLink(name, 8, 64, 0o600, opts)
	check("CreateAdvancedLink", err)
	if l, err := OpenStandardLink(name, 8, 64, LinkOptions{}); !errors.Is(err, os.ErrNotExist) {
		if l != nil {
			_ = l.Close()
		}
		t.Errorf("CreateAdvancedLink created the name before rejecting the option (open: %v)", err)
	}
	_, err = OpenAdvancedLink(name, 8, 64, opts)
	check("OpenAdvancedLink", err)

	_, err = Listen(ctx, name, cfg)
	check("Listen", err)
	_, err = Dial(ctx, name, cfg)
	check("Dial", err)
	_, err = Connect(ctx, name, cfg)
	check("Connect", err)
	_, err = NewAcceptor(name, AcceptorConfig{Advanced: opts})
	check("NewAcceptor", err)
	if l, err := OpenStandardLink(name, 8, 64, LinkOptions{}); !errors.Is(err, os.ErrNotExist) {
		if l != nil {
			_ = l.Close()
		}
		t.Errorf("a rejected Listen or NewAcceptor created the name (open: %v)", err)
	}

	// Without the option the same constructors work.
	opts.Link.Exclusive = false
	a, err := NewAdvancedLink(mem, 8, 64, opts)
	if err != nil {
		t.Fatal(fmt.Errorf("NewAdvancedLink without Exclusive: %w", err))
	}
	_ = a.Close()
}
