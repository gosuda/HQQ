package conformance

import (
	"testing"

	"gosuda.org/hqq/internal/protocol"
)

// TestErrorCodeMapping (G-11) delivers every wire error code through the
// public Standard Read path: an OpError packet on ring 0 is returned by Read
// as the error its code maps to. A code outside the table is ErrProtocol.
func TestErrorCodeMapping(t *testing.T) {
	m := loadManifest(t)
	const count, size = 16, 8
	if len(m.ErrorCodes) > count {
		t.Fatalf("%d error codes do not fit a ring of %d", len(m.ErrorCodes), count)
	}
	ring := make([]ringPacket, 0, len(m.ErrorCodes))
	for _, ec := range m.ErrorCodes {
		ring = append(ring, ringPacket{pkt: protocol.NewPacket(protocol.OpError, 0, ec.Code)})
	}
	mem := callerMemory(t, count, size, liveImage(t, count, size, ring))
	sec := joinSecondary(t, mem, count, size)
	buf := make([]byte, size)
	for _, ec := range m.ErrorCodes {
		_, err := sec.Read(buf)
		if !matchesKind(err, ec.ErrorKind) {
			t.Errorf("code %s (%#x): Read error %v, want %s", ec.Name, ec.Code, err, ec.ErrorKind)
		}
	}
}
