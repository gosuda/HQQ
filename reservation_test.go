package hqq

import (
	"errors"
	"testing"
)

// TestWriteReservationCopyCannotCompleteTwice checks that once a reservation
// was committed, a copy of it is dead: it exposes no buffer and can neither
// commit nor abort. Before the ring completed slots with a compare-and-swap,
// the stale copy re-published the slot and corrupted the ring.
func TestWriteReservationCopyCannotCompleteTwice(t *testing.T) {
	_, primary, secondary := newMemoryPair(t, 2, 64, LinkOptions{})

	res, err := primary.ReserveWrite()
	if err != nil {
		t.Fatalf("ReserveWrite: %v", err)
	}
	stale := res
	if stale.Buffer() == nil {
		t.Fatal("a fresh reservation must expose its buffer")
	}
	copy(res.Buffer(), "first")
	if err := res.Commit(5); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	if stale.Buffer() != nil {
		t.Fatal("a copy of a committed reservation still exposes the slot buffer")
	}
	if err := stale.Commit(5); !errors.Is(err, ErrInvalidOp) {
		t.Fatalf("stale Commit = %v, want ErrInvalidOp", err)
	}
	if err := stale.Abort(0); !errors.Is(err, ErrInvalidOp) {
		t.Fatalf("stale Abort = %v, want ErrInvalidOp", err)
	}

	got := make([]byte, 64)
	n, err := secondary.Read(got)
	if err != nil || string(got[:n]) != "first" {
		t.Fatalf("Read = %q, %v; want %q", got[:n], err, "first")
	}

	// The slot completed by the stale copy would stay behind its lap and
	// wedge the direction; several more laps must still pass.
	for i := 0; i < 6; i++ {
		msg := []byte{byte('a' + i)}
		if _, err := primary.Write(msg); err != nil {
			t.Fatalf("Write %d after stale commit: %v", i, err)
		}
		n, err := secondary.Read(got)
		if err != nil || string(got[:n]) != string(msg) {
			t.Fatalf("Read %d = %q, %v; want %q", i, got[:n], err, msg)
		}
	}
}

// TestReadReservationCopyCannotReleaseTwice is the read-side counterpart: a
// copy of a released reservation must not free the slot again, which would
// hand a slot that a producer has already refilled back to the producers.
func TestReadReservationCopyCannotReleaseTwice(t *testing.T) {
	_, primary, secondary := newMemoryPair(t, 2, 64, LinkOptions{})

	if _, err := primary.Write([]byte("one")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	res, err := secondary.ReserveRead()
	if err != nil {
		t.Fatalf("ReserveRead: %v", err)
	}
	stale := res
	if string(res.Buffer()) != "one" {
		t.Fatalf("Buffer = %q, want %q", res.Buffer(), "one")
	}
	if err := res.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}

	if stale.Buffer() != nil {
		t.Fatal("a copy of a released reservation still exposes the slot buffer")
	}

	// Refill and hold the same slot's next lap, then try the stale release.
	for _, m := range []string{"two", "three"} {
		if _, err := primary.Write([]byte(m)); err != nil {
			t.Fatalf("Write %q: %v", m, err)
		}
	}
	buf := make([]byte, 64)
	if n, err := secondary.Read(buf); err != nil || string(buf[:n]) != "two" {
		t.Fatalf("Read = %q, %v; want %q", buf[:n], err, "two")
	}
	held, err := secondary.ReserveRead()
	if err != nil {
		t.Fatalf("ReserveRead: %v", err)
	}
	if held.Slot() != stale.Slot() {
		t.Fatalf("held slot %d, want the stale copy's slot %d", held.Slot(), stale.Slot())
	}
	if err := stale.Release(); !errors.Is(err, ErrInvalidOp) {
		t.Fatalf("stale Release = %v, want ErrInvalidOp", err)
	}
	if string(held.Buffer()) != "three" {
		t.Fatalf("stale Release disturbed the current owner: Buffer = %q", held.Buffer())
	}
	if err := held.Release(); err != nil {
		t.Fatalf("Release of the current owner: %v", err)
	}
}
