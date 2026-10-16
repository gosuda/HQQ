package hqq

import (
	"net"
	"time"
)

var _ net.PacketConn = (*StandardLink)(nil)

// Addresses of the two endpoints of a link: LocalAddr reports this endpoint's
// and ReadFrom the peer's, so the two sides of a link agree.
var (
	primaryAddr   net.Addr = &HQQAddr{LinkID: "hqq-primary"}
	secondaryAddr net.Addr = &HQQAddr{LinkID: "hqq-secondary"}
)

// ReadFrom implements net.PacketConn with datagram semantics: one call
// returns at most one message. If p is shorter than the message, ReadFrom
// copies len(p) bytes, discards the rest and returns n == len(p) with a nil
// error (like UDP on Linux); an empty p consumes and discards one message.
// The returned address is fixed: the peer endpoint, an HQQAddr named
// "hqq-secondary" on a Primary link and "hqq-primary" on a Secondary one.
//
// If a previous Read or ReadZeroCopy left the tail of a message unread, the
// oldest such tail is the next datagram: ReadFrom returns it (truncated like
// any other datagram) before consuming a newer message. Mixing Read and
// ReadFrom therefore never merges two messages, but ReadFrom cannot tell a
// tail from a whole message.
//
// After the peer closed the link gracefully and every buffered message was
// consumed, ReadFrom returns io.EOF, like Read. All other errors are those of
// Read.
func (l *StandardLink) ReadFrom(p []byte) (n int, addr net.Addr, err error) {
	n, err = l.recvMessage(p, true)
	if err != nil {
		return 0, nil, err
	}
	return n, l.peerAddr(), nil
}

func (l *StandardLink) peerAddr() net.Addr {
	if l.linkMode == LinkModeSecondary {
		return primaryAddr
	}
	return secondaryAddr
}

// WriteTo implements net.PacketConn. A link has exactly one peer, so addr is
// ignored: p is always sent to the peer, as one message (see Write).
func (l *StandardLink) WriteTo(p []byte, _ net.Addr) (n int, err error) {
	return l.Write(p)
}

// LocalAddr implements net.PacketConn. It is the HQQAddr "hqq-primary" on a
// Primary link and "hqq-secondary" on a Secondary one.
func (l *StandardLink) LocalAddr() net.Addr {
	if l.linkMode == LinkModeSecondary {
		return secondaryAddr
	}
	return primaryAddr
}

// SetDeadline implements net.PacketConn. It sets both the read and the write
// deadline; see SetReadDeadline.
func (l *StandardLink) SetDeadline(t time.Time) error {
	if err := l.SetReadDeadline(t); err != nil {
		return err
	}
	return l.SetWriteDeadline(t)
}

// SetReadDeadline implements net.PacketConn. Blocked and future read calls
// fail with ErrTimeout once t passes; a zero t disables the deadline. It may
// be called concurrently with blocked reads.
func (l *StandardLink) SetReadDeadline(t time.Time) error {
	return l.setDeadline(&l.readDeadline, t, true)
}

// SetWriteDeadline implements net.PacketConn. Blocked and future write calls
// fail with ErrTimeout once t passes (a zero-copy callback that already started
// is not interrupted); a zero t disables the deadline.
func (l *StandardLink) SetWriteDeadline(t time.Time) error {
	return l.setDeadline(&l.writeDeadline, t, false)
}

func (l *StandardLink) setDeadline(slot interface {
	Store(*time.Time)
}, t time.Time, read bool) error {
	if t.IsZero() {
		slot.Store(nil)
	} else {
		slot.Store(&t)
	}
	// Wake parked calls so they re-evaluate the new deadline.
	if read {
		if l.enterRx() != nil {
			return ErrClosed
		}
		l.rxRing.WakeAll()
		l.exitRx()
	} else {
		if l.enterTx(false) != nil {
			return ErrClosed
		}
		l.txRing.WakeAll()
		l.exitTx()
	}
	return nil
}

// HQQAddr represents an HQQ network address.
// This implements the net.Addr interface for HQQ links.
type HQQAddr struct {
	LinkID string // Identifier for the HQQ link
}

// Network returns the network type
func (a *HQQAddr) Network() string {
	return "hqq"
}

// String returns the string representation of the address
func (a *HQQAddr) String() string {
	return a.LinkID
}
