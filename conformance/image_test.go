package conformance

import (
	"testing"

	"gosuda.org/hqq"
	"gosuda.org/hqq/internal/protocol"
)

// ringPacket is a packet published at the next position of ring 0 of a test
// image, with its payload in the slot buffer.
type ringPacket struct {
	pkt     protocol.Packet
	payload []byte
}

// liveImage returns a host-page segment image: a Ready segment whose Primary
// is live (interval 0, so it is always alive), whose Secondary slot is free,
// and whose ring 0 holds ring0 in order. Ring 1 is formatted and empty. The
// page family is the host's: an in-memory join needs the host page size
// (SPEC 5.5).
func liveImage(t *testing.T, count, size int, ring0 []ringPacket) []byte {
	t.Helper()
	if len(ring0) > count {
		t.Fatalf("%d packets do not fit a ring of %d slots", len(ring0), count)
	}
	s, err := newSegImage(hostPage(), count, size)
	if err != nil {
		t.Fatal(err)
	}
	s.setSuperblock(withState(sbBase(hostPage(), count, size), StateReady, 1),
		[2]endpointState{{token: normPrimaryToken}, {}})
	s.formatRing(0)
	s.formatRing(1)
	for i, p := range ring0 {
		s.publish(0, uint64(i), p.pkt, p.payload)
	}
	s.setCursors(0, 0, uint64(len(ring0)))
	return s.b
}

// callerMemory returns page-aligned caller memory of the segment size of
// count x size, holding image at its start.
func callerMemory(t *testing.T, count, size int, image []byte) []byte {
	t.Helper()
	total := hqq.SizeStandardLink(count, size)
	if total == 0 {
		t.Fatalf("no layout for %d x %d", count, size)
	}
	if uintptr(len(image)) > total {
		t.Fatalf("image of %d bytes does not fit %d", len(image), total)
	}
	mem := hqq.AlignedBuffer(total)
	copy(mem, image)
	return mem
}

// joinSecondary joins the in-memory segment as Secondary with the test
// options and fails the test if the join does not succeed.
func joinSecondary(t *testing.T, mem []byte, count, size int) *hqq.StandardLink {
	t.Helper()
	sec, err := hqq.NewStandardLink(mem, count, size, testOpts())
	if err != nil {
		t.Fatalf("join as secondary: %v", err)
	}
	if sec.Mode() != hqq.LinkModeSecondary {
		t.Fatalf("join returned mode %v, want secondary", sec.Mode())
	}
	t.Cleanup(func() { _ = sec.Close() })
	return sec
}
