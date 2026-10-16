package hqq

import (
	"net"
	"time"
)

var _ net.PacketConn = (*StandardLink)(nil)

// ReadFrom implements net.PacketConn. The returned address is the last address
// passed to WriteTo, or a fixed "hqq-remote" address.
func (l *StandardLink) ReadFrom(p []byte) (n int, addr net.Addr, err error) {
	n, err = l.Read(p)
	if err != nil {
		return 0, nil, err
	}
	return n, l.remoteAddr.Load().addr, nil
}

// WriteTo implements net.PacketConn. A link has exactly one peer, so addr is
// only recorded (and returned by later ReadFrom calls); it does not select a
// destination.
func (l *StandardLink) WriteTo(p []byte, addr net.Addr) (n int, err error) {
	if addr != nil {
		l.remoteAddr.Store(&addrHolder{addr: addr})
	}
	return l.Write(p)
}

// LocalAddr implements net.PacketConn.
func (l *StandardLink) LocalAddr() net.Addr {
	return l.localAddr
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
