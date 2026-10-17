package mpmc

import "sync/atomic"

// Exclusive mode is the store-based SPSC variant of the slot protocol
// (spec/SPEC.md 7.6). The handle's owner promises that at most one goroutine
// at a time performs producer-side operations (Enqueue*, ReserveProducer*,
// ProducerSlot.Commit) and at most one goroutine at a time performs
// consumer-side operations (Dequeue*, ReserveConsumer*, ConsumerSlot.Release).
// The producer side and the consumer side may run concurrently with each
// other. The claim then needs no compare-and-swap:
//
//	producer: pos = load(w); seq[pos] == pos?; store(w = pos+1); ...; store(seq = pos+1)
//	consumer: pos = load(r); seq[pos] == pos+1?; store(r = pos+1); ...; store(seq = pos+mask+1)
//
// The cursor words stay canonical (w counts producer claims, r consumer
// claims), the cursor store precedes the seq store, and the cursors are read
// from shared memory on every claim, so an exclusive handle interoperates
// with CAS handles and implementations on the same ring memory: an exclusive
// producer with a CAS consumer, a CAS producer with an exclusive consumer,
// and a later CAS (or exclusive) attach to a ring an exclusive handle used.
// What is not allowed is two parties on the same side of one ring, one of
// them exclusive, active at the same time.
//
// Violating the contract corrupts the ring: two claimants can take the same
// position. Builds with the race tag detect overlapping claims on one side
// and panic (exclusive_race.go).

// SetExclusive switches the handle between the CAS protocol (the default)
// and exclusive mode, see above. It must be called before the first
// operation on the handle and not concurrently with operations on it. The
// handle is not required to be exclusive on both sides of the ring: the
// flag only says that this handle's own side is serialized by its owner.
func (m *MPMCRing[T]) SetExclusive(on bool) {
	m.exclusive = on
}

// Exclusive reports whether the handle is in exclusive mode.
func (m *MPMCRing[T]) Exclusive() bool {
	return m.exclusive
}

// claimProducerExclusive is claimProducer for an exclusive handle.
func (m *MPMCRing[T]) claimProducerExclusive(c *Cancel) (uint64, *_melem[T], error) {
	m.guard.enterProducer()
	h := m.head
	pos := atomic.LoadUint64(&h.w)
	e := m.elem(pos & m.mask)
	if int64(atomic.LoadUint64(&e._seq)-pos) < 0 {
		// The ring is full. Waiting needs per-call state, kept out of this
		// fast path.
		pos, e, err := m.claimProducerExclusiveSlow(c, pos, e)
		m.guard.exitProducer()
		return pos, e, err
	}
	atomic.StoreUint64(&h.w, pos+1)
	m.guard.exitProducer()
	return pos, e, nil
}

// claimProducerExclusiveSlow is claimProducerExclusive once the ring was
// found full at pos. The position cannot move while we wait: this side is the
// only writer of w.
func (m *MPMCRing[T]) claimProducerExclusiveSlow(c *Cancel, pos uint64, e *_melem[T]) (uint64, *_melem[T], error) {
	var w waiter
	defer w.finish()
	for int64(atomic.LoadUint64(&e._seq)-pos) < 0 {
		if err := w.wait(m.policy, c, m.site(NotFull), &e._seq, pos); err != nil {
			return 0, nil, err
		}
	}
	atomic.StoreUint64(&m.head.w, pos+1)
	return pos, e, nil
}

// claimConsumerExclusive is claimConsumer for an exclusive handle.
func (m *MPMCRing[T]) claimConsumerExclusive(c *Cancel) (uint64, *_melem[T], error) {
	m.guard.enterConsumer()
	h := m.head
	pos := atomic.LoadUint64(&h.r)
	e := m.elem(pos & m.mask)
	if int64(atomic.LoadUint64(&e._seq)-(pos+1)) < 0 {
		// The ring is empty; see claimProducerExclusive.
		pos, e, err := m.claimConsumerExclusiveSlow(c, pos, e)
		m.guard.exitConsumer()
		return pos, e, err
	}
	atomic.StoreUint64(&h.r, pos+1)
	m.guard.exitConsumer()
	return pos, e, nil
}

// claimConsumerExclusiveSlow is claimConsumerExclusive once the ring was
// found empty at pos.
func (m *MPMCRing[T]) claimConsumerExclusiveSlow(c *Cancel, pos uint64, e *_melem[T]) (uint64, *_melem[T], error) {
	var w waiter
	defer w.finish()
	for int64(atomic.LoadUint64(&e._seq)-(pos+1)) < 0 {
		if err := w.wait(m.policy, c, m.site(NotEmpty), &e._seq, pos+1); err != nil {
			return 0, nil, err
		}
	}
	atomic.StoreUint64(&m.head.r, pos+1)
	return pos, e, nil
}

// publishSlotExclusive is publishSlot for an exclusive handle. The swap from
// the claimed state becomes a load and a store: only this side's single
// goroutine completes slots, so nothing can change seq between the two. A
// stale copy of an already committed slot still fails the check (seq has
// moved on to pos+1 or beyond) and neither modifies the ring nor wakes
// anyone.
func (m *MPMCRing[T]) publishSlotExclusive(pos uint64, e *_melem[T]) bool {
	if atomic.LoadUint64(&e._seq) != pos {
		return false
	}
	atomic.StoreUint64(&e._seq, pos+1)
	m.notifyConsumers()
	return true
}

// releaseSlotExclusive is releaseSlot for an exclusive handle; see
// publishSlotExclusive.
func (m *MPMCRing[T]) releaseSlotExclusive(pos uint64, e *_melem[T]) bool {
	if atomic.LoadUint64(&e._seq) != pos+1 {
		return false
	}
	atomic.StoreUint64(&e._seq, pos+m.mask+1)
	m.notifyProducers()
	return true
}
