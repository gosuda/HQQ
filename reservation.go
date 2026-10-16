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
// after the link is closed. Finish the reservation before calling Close.
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

// Buffer returns the slot-owned payload buffer while the reservation is
// active. It returns nil once the reservation was committed or aborted,
// including through a copy of this value.
func (r *StandardWriteReservation) Buffer() []byte {
	if r == nil || r.done || !r.slot.Owned() {
		return nil
	}
	return r.buffer
}

// Commit publishes n bytes from the reserved payload buffer. An invalid n
// publishes a tombstone instead (so the ring keeps moving) and returns
// ErrInvalidSize or ErrBufferOverflow. It returns ErrInvalidOp if the
// reservation was already completed, including through a copy of this value.
func (r *StandardWriteReservation) Commit(n int) error {
	if r == nil || r.done || !r.slot.Owned() {
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
	r.done = true
	if !r.slot.Commit() {
		return ErrInvalidOp
	}
	return nil
}

// Abort publishes a tombstone for the reserved slot. Readers skip tombstones.
// Use Abort when a reserved write cannot be completed but ring progress must be
// preserved. It returns ErrInvalidOp if the reservation was already completed,
// including through a copy of this value.
func (r *StandardWriteReservation) Abort(reason ErrorCode) error {
	if r == nil || r.done || !r.slot.Owned() {
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
	r.done = true
	if !r.slot.Commit() {
		return ErrInvalidOp
	}
	return nil
}

// StandardReadReservation is a reserved StandardLink read slot.
//
// WARNING: A reservation holds an underlying ring slot. Release must be called
// promptly; if the reservation is dropped, copied, or retained without
// releasing, the whole direction can suffer head-of-line (HOL) blocking. The
// buffer returned by Buffer is invalid after Release returns, and after the
// link is closed. Release the reservation before calling Close.
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
// It returns nil once the reservation was released, including through a copy
// of this value.
func (r *StandardReadReservation) Buffer() []byte {
	if r == nil || r.done || (!r.local && !r.slot.Owned()) {
		return nil
	}
	return r.buffer
}

// Release releases the reserved read slot. It returns ErrInvalidOp if the
// reservation was already released, including through a copy of this value;
// a stale copy neither frees the slot a second time nor wakes producers.
func (r *StandardReadReservation) Release() error {
	if r == nil || r.done {
		return ErrInvalidOp
	}
	if r.local {
		r.link.recv.giveBack(r.backing)
		r.done = true
		return nil
	}
	if !r.slot.Owned() {
		return ErrInvalidOp
	}
	if err := r.link.enterRx(); err != nil {
		return err
	}
	defer r.link.exitRx()
	r.done = true
	if !r.slot.Release() {
		return ErrInvalidOp
	}
	return nil
}

// ReserveWrite reserves one ring-slot-owned payload buffer for writing.
//
// WARNING: Commit or Abort must be called promptly on the returned reservation;
// otherwise the whole direction can suffer head-of-line (HOL) blocking.
//
// Close must not be called while a reservation is active: finish it with
// Commit or Abort first. Its buffer is invalid once Close has run.
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

// ReserveRead reserves one readable payload buffer. If a previous Read left
// the tail of a message queued, the reservation serves the oldest queued tail
// (all of it) instead of a new message. Tombstones are skipped; Advanced
// Protocol frames are discarded with ErrUnexpectedPacket.
//
// WARNING: Release must be called promptly on the returned reservation;
// otherwise the whole direction can suffer head-of-line (HOL) blocking.
//
// Close must not be called while a reservation is active: Release it first.
// Its buffer is invalid once Close has run.
func (l *StandardLink) ReserveRead() (StandardReadReservation, error) {
	if err := l.enterRx(); err != nil {
		return StandardReadReservation{}, err
	}
	defer l.exitRx()

	if view, backing, ok := l.recv.take(); ok {
		return StandardReadReservation{link: l, buffer: view, backing: backing, local: true}, nil
	}
	for {
		slot, err := l.reserveRx()
		if err != nil {
			return StandardReadReservation{}, err
		}
		size, err := l.classify(slot.Value())
		if err != nil {
			slot.Release()
			return StandardReadReservation{}, err
		}
		if size == 0 {
			slot.Release()
			continue
		}
		return StandardReadReservation{
			slot:   slot,
			link:   l,
			buffer: l.rxBufs[slot.Slot()][:size:size],
		}, nil
	}
}
