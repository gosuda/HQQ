package hqq

import (
	"gosuda.org/hqq/internal/mpmc"
	"gosuda.org/hqq/internal/protocol"
)

// StandardWriteReservation is a reserved StandardLink write slot.
//
// WARNING: A reservation holds an underlying ring slot. Commit or Abort must be
// called promptly; if the reservation is dropped, copied, or retained without
// finishing, the whole direction can suffer head-of-line (HOL) blocking. The
// buffer returned by Buffer is invalid after Commit or Abort returns, and
// after the link is closed.
type StandardWriteReservation struct {
	link   *StandardLink
	slot   mpmc.ProducerSlot[protocol.Packet]
	buffer []byte
	copyID uint64
	done   bool
}

// Slot returns the data-ring slot index owned by this reservation.
func (r *StandardWriteReservation) Slot() uint64 {
	if r == nil {
		return 0
	}
	return r.slot.Slot()
}

// Buffer returns the slot-owned payload buffer while the reservation is active.
func (r *StandardWriteReservation) Buffer() []byte {
	if r == nil || r.done {
		return nil
	}
	return r.buffer
}

// Commit publishes n bytes from the reserved payload buffer. An invalid n
// publishes a tombstone instead (so the ring keeps moving) and returns
// ErrInvalidSize or ErrBufferOverflow.
func (r *StandardWriteReservation) Commit(n int) error {
	if r == nil || r.done || !r.slot.Valid() {
		return ErrInvalidOp
	}
	switch {
	case n <= 0:
		if err := r.publishTombstone(ErrCodeInvalidSize); err != nil {
			return err
		}
		return ErrInvalidSize
	case n > len(r.buffer):
		if err := r.publishTombstone(ErrCodeBufferOverflow); err != nil {
			return err
		}
		return ErrBufferOverflow
	}
	if err := r.link.enterTx(false); err != nil {
		return err
	}
	defer r.link.exitTx()
	*r.slot.Value() = protocol.NewStandardLinkCopyPacket(r.copyID, r.slot.Slot(), uint64(n))
	r.slot.Commit()
	r.done = true
	return nil
}

// Abort publishes a tombstone for the reserved slot. Readers skip tombstones.
// Use Abort when a reserved write cannot be completed but ring progress must be
// preserved.
func (r *StandardWriteReservation) Abort(reason ErrorCode) error {
	if r == nil || r.done || !r.slot.Valid() {
		return ErrInvalidOp
	}
	if reason == 0 {
		reason = ErrCodeInvalidOp
	}
	return r.publishTombstone(reason)
}

func (r *StandardWriteReservation) publishTombstone(reason ErrorCode) error {
	if err := r.link.enterTx(false); err != nil {
		return err
	}
	defer r.link.exitTx()
	*r.slot.Value() = protocol.NewStandardLinkTombstonePacket(r.copyID, r.slot.Slot(), uint64(reason))
	r.slot.Commit()
	r.done = true
	return nil
}

// StandardReadReservation is a reserved StandardLink read slot.
//
// WARNING: A reservation holds an underlying ring slot. Release must be called
// promptly; if the reservation is dropped, copied, or retained without
// releasing, the whole direction can suffer head-of-line (HOL) blocking. The
// buffer returned by Buffer is invalid after Release returns, and after the
// link is closed.
type StandardReadReservation struct {
	slot    mpmc.ConsumerSlot[protocol.Packet]
	link    *StandardLink
	buffer  []byte
	backing []byte // remainder backing store for local reservations
	local   bool
	done    bool
}

// Slot returns the data-ring slot index owned by this reservation (0 for a
// reservation that serves a locally buffered remainder).
func (r *StandardReadReservation) Slot() uint64 {
	if r == nil || r.local {
		return 0
	}
	return r.slot.Slot()
}

// Buffer returns the reserved payload buffer while the reservation is active.
func (r *StandardReadReservation) Buffer() []byte {
	if r == nil || r.done {
		return nil
	}
	return r.buffer
}

// Release releases the reserved read slot.
func (r *StandardReadReservation) Release() error {
	if r == nil || r.done {
		return ErrInvalidOp
	}
	if r.local {
		r.link.recv.giveBack(r.backing)
		r.done = true
		return nil
	}
	if err := r.link.enterRx(); err != nil {
		return err
	}
	defer r.link.exitRx()
	if !r.slot.Release() {
		return ErrInvalidOp
	}
	r.done = true
	return nil
}

// ReserveWrite reserves one ring-slot-owned payload buffer for writing.
//
// WARNING: Commit or Abort must be called promptly on the returned reservation;
// otherwise the whole direction can suffer head-of-line (HOL) blocking.
func (l *StandardLink) ReserveWrite() (StandardWriteReservation, error) {
	if err := l.enterTx(true); err != nil {
		return StandardWriteReservation{}, err
	}
	defer l.exitTx()

	c := mpmc.Cancel{Closed: l.closeCh, DeadlineFunc: l.writeDeadlineFn, Check: l.checkFn}
	slot, err := l.txRing.ReserveProducerCancel(&c)
	if err != nil {
		return StandardWriteReservation{}, mapWaitErr(err)
	}
	return StandardWriteReservation{
		link:   l,
		slot:   slot,
		buffer: l.txBufs[slot.Slot()],
		copyID: l.idGenerator.Add(1),
	}, nil
}

// ReserveRead reserves one readable payload buffer. Tombstones are skipped;
// Advanced Protocol frames are discarded with ErrUnexpectedPacket.
//
// WARNING: Release must be called promptly on the returned reservation;
// otherwise the whole direction can suffer head-of-line (HOL) blocking.
func (l *StandardLink) ReserveRead() (StandardReadReservation, error) {
	if view, backing, ok := l.recv.take(); ok {
		return StandardReadReservation{link: l, buffer: view, backing: backing, local: true}, nil
	}
	if err := l.enterRx(); err != nil {
		return StandardReadReservation{}, err
	}
	defer l.exitRx()

	for {
		c := mpmc.Cancel{Closed: l.closeCh, DeadlineFunc: l.readDeadlineFn, Check: l.checkFn}
		slot, err := l.rxRing.ReserveConsumerCancel(&c)
		if err == ErrPeerClosed || err == ErrPeerGone {
			if s, retryErr := l.rxRing.ReserveConsumerCancel(&mpmc.Cancel{NoWait: true}); retryErr == nil {
				slot, err = s, nil
			}
		}
		if err != nil {
			return StandardReadReservation{}, mapWaitErr(err)
		}

		p := slot.Value()
		switch p.Op() {
		case protocol.OpStandardLinkTombstone:
			slot.Release()
			continue
		case protocol.OpStandardLinkCopy:
			size := int(p.Operand(2))
			if size <= 0 || size > l.bufferSize {
				slot.Release()
				return StandardReadReservation{}, ErrInvalidSize
			}
			return StandardReadReservation{
				slot:   slot,
				link:   l,
				buffer: l.rxBufs[slot.Slot()][:size:size],
			}, nil
		case protocol.OpError:
			err := errorFromCode(ErrorCode(p.Operand(1)))
			slot.Release()
			return StandardReadReservation{}, err
		default:
			slot.Release()
			return StandardReadReservation{}, ErrUnexpectedPacket
		}
	}
}
