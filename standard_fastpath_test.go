package hqq

import (
	"errors"
	"runtime"
	"strconv"
	"testing"
	"time"

	"gosuda.org/hqq/internal/mpmc"
	"gosuda.org/hqq/internal/protocol"
)

// fastPathModes are the link configurations whose hot paths differ: the
// compare-and-swap ring with atomic in-flight counters, and the exclusive
// ring with the owner's store-based counters.
var fastPathModes = []struct {
	name string
	opts LinkOptions
}{
	{"cas", LinkOptions{WaitPolicy: WaitSpin}},
	{"exclusive", LinkOptions{WaitPolicy: WaitSpin, Exclusive: true}},
}

// rawPackets takes the packets that are queued on l's receive ring without
// interpreting them.
func rawPackets(t *testing.T, l *StandardLink) []protocol.Packet {
	t.Helper()
	var out []protocol.Packet
	for {
		slot, err := l.rxRing.ReserveConsumerCancel(&mpmc.Cancel{NoWait: true})
		if err != nil {
			return out
		}
		out = append(out, *slot.Value())
		slot.Release()
	}
}

// TestStandardLinkCopyIDCountsClaims: CopyID is the per-endpoint claim
// counter of SPEC 9.3 (the first packet carries 1, every claim adds 1,
// tombstones included). It is derived from the ring position, so an endpoint
// that attaches to a ring that has seen traffic starts at 1 again.
func TestStandardLinkCopyIDCountsClaims(t *testing.T) {
	for _, mode := range fastPathModes {
		t.Run(mode.name, func(t *testing.T) {
			_, primary, secondary := newMemoryPair(t, 8, 64, mode.opts)

			mustWrite(t, secondary, "one")
			if _, err := secondary.WriteZeroCopy(func([]byte) (int, error) { return 0, errors.New("abandoned") }); err == nil {
				t.Fatal("WriteZeroCopy with a failing callback succeeded")
			}
			res, err := secondary.ReserveWrite()
			if err != nil {
				t.Fatalf("ReserveWrite: %v", err)
			}
			if err := res.Abort(ErrCodeInvalidOp); err != nil {
				t.Fatalf("Abort: %v", err)
			}
			res, err = secondary.ReserveWrite()
			if err != nil {
				t.Fatalf("ReserveWrite: %v", err)
			}
			res.Buffer()[0] = 'x'
			if err := res.Commit(1); err != nil {
				t.Fatalf("Commit: %v", err)
			}

			packets := rawPackets(t, primary)
			wantOps := []protocol.OpCode{protocol.OpStandardLinkCopy, protocol.OpStandardLinkTombstone, protocol.OpStandardLinkTombstone, protocol.OpStandardLinkCopy}
			if len(packets) != len(wantOps) {
				t.Fatalf("%d packets queued, want %d", len(packets), len(wantOps))
			}
			for i, p := range packets {
				if p.Op() != wantOps[i] || p.Operand(0) != uint64(i+1) {
					t.Fatalf("packet %d = op %v CopyID %d, want op %v CopyID %d", i, p.Op(), p.Operand(0), wantOps[i], i+1)
				}
			}
		})
	}
}

// TestStandardLinkPacketsKeepUnusedOperandsZero: SPEC 9.1 wants the operand
// words a packet does not define to be zero. The fast path stores only the
// defined words of a copy packet, so this holds only because the ring starts
// zeroed and no Standard link call writes them: check it over several laps
// of a small ring, with tombstones in between.
func TestStandardLinkPacketsKeepUnusedOperandsZero(t *testing.T) {
	for _, mode := range fastPathModes {
		t.Run(mode.name, func(t *testing.T) {
			_, primary, secondary := newMemoryPair(t, 4, 64, mode.opts)
			checked := 0
			for lap := range 6 {
				mustWrite(t, secondary, "copy")
				if _, err := secondary.WriteZeroCopy(func(b []byte) (int, error) {
					return copy(b, "zero copy"), nil
				}); err != nil {
					t.Fatalf("WriteZeroCopy: %v", err)
				}
				if _, err := secondary.WriteZeroCopy(func([]byte) (int, error) { return 0, errors.New("abandoned") }); err == nil {
					t.Fatal("WriteZeroCopy with a failing callback succeeded")
				}
				res, err := secondary.ReserveWrite()
				if err != nil {
					t.Fatalf("ReserveWrite: %v", err)
				}
				res.Buffer()[0] = byte(lap)
				if err := res.Commit(1); err != nil {
					t.Fatalf("Commit: %v", err)
				}
				for _, p := range rawPackets(t, primary) {
					for i := 3; i < protocol.PacketOperandCount; i++ {
						if p.Operand(i) != 0 {
							t.Fatalf("lap %d: %v packet has operand %d = %#x, want 0", lap, p.Op(), i, p.Operand(i))
						}
					}
					checked++
				}
			}
			if checked != 24 {
				t.Fatalf("checked %d packets, want 24", checked)
			}
		})
	}
}

// TestStandardLinkCopyIDStartsAtOnePerEndpoint: an endpoint attaching to a
// send ring whose producer cursor already moved still numbers its packets
// from 1.
func TestStandardLinkCopyIDStartsAtOnePerEndpoint(t *testing.T) {
	for _, mode := range fastPathModes {
		t.Run(mode.name, func(t *testing.T) {
			mem := AlignedBuffer(SizeStandardLink(8, 64))
			primary, err := NewStandardLink(mem, 8, 64, mode.opts)
			if err != nil {
				t.Fatalf("NewStandardLink: %v", err)
			}
			defer primary.Close()
			// Advance the producer cursor of the secondary's send ring, as
			// an earlier user of the ring would have.
			for range 3 {
				slot, err := primary.rxRing.ReserveProducerCancel(&mpmc.Cancel{NoWait: true})
				if err != nil {
					t.Fatalf("claim: %v", err)
				}
				*slot.Value() = protocol.NewStandardLinkTombstonePacket(0, slot.Slot(), uint64(ErrCodeInvalidOp))
				slot.CommitOwned()
			}
			rawPackets(t, primary)

			secondary, err := NewStandardLink(mem, 8, 64, mode.opts)
			if err != nil {
				t.Fatalf("second NewStandardLink: %v", err)
			}
			defer secondary.Close()
			mustWrite(t, secondary, "one")
			mustWrite(t, secondary, "two")
			packets := rawPackets(t, primary)
			if len(packets) != 2 || packets[0].Operand(0) != 1 || packets[1].Operand(0) != 2 {
				t.Fatalf("packets = %v, want CopyIDs 1 and 2", packets)
			}
		})
	}
}

// TestCloseWaitsForZeroCopyCallbacks: a running zero-copy callback is an
// in-flight call in every mode, so Close waits for it, on the receive and on
// the send side. In exclusive mode the owner registers through its own
// counters, which Close has to wait for as well.
func TestCloseWaitsForZeroCopyCallbacks(t *testing.T) {
	for _, mode := range fastPathModes {
		for _, side := range []string{"read", "write"} {
			t.Run(mode.name+"/"+side, func(t *testing.T) {
				_, primary, secondary := newMemoryPair(t, 4, 64, mode.opts)
				closing, other := secondary, primary
				if side == "write" {
					closing, other = primary, secondary
				}
				mustWrite(t, other, "0123456789")

				inCallback := make(chan struct{})
				release := make(chan struct{})
				callDone := make(chan error, 1)
				go func() {
					var err error
					if side == "read" {
						_, err = closing.ReadZeroCopy(func([]byte) error {
							close(inCallback)
							<-release
							return nil
						})
					} else {
						_, err = closing.WriteZeroCopy(func(b []byte) (int, error) {
							close(inCallback)
							<-release
							return copy(b, "x"), nil
						})
					}
					callDone <- err
				}()
				<-inCallback

				closeDone := make(chan struct{})
				go func() {
					closing.Close()
					close(closeDone)
				}()
				deadline := time.Now().Add(2 * time.Second)
				for !closing.closed.Load() {
					if time.Now().After(deadline) {
						t.Fatal("Close never started")
					}
					runtime.Gosched()
				}
				select {
				case <-closeDone:
					t.Fatal("Close returned while a zero-copy callback was still running")
				case <-time.After(20 * time.Millisecond):
				}
				close(release)
				if err := waitErr(t, callDone, 2*time.Second); err != nil {
					t.Fatalf("zero-copy call = %v", err)
				}
				select {
				case <-closeDone:
				case <-time.After(2 * time.Second):
					t.Fatal("Close did not return after the callback finished")
				}
			})
		}
	}
}

// TestStandardLinkPingPongOnFewProcessors: the two ends of a link in one
// process complete a ping-pong when the Go scheduler has one or two
// processors, for every wait policy. A waiter spins on the ring, so it must
// yield to its peer's goroutine often enough for the peer to run.
func TestStandardLinkPingPongOnFewProcessors(t *testing.T) {
	const rounds = 20000
	for _, procs := range []int{1, 2} {
		for _, policy := range []struct {
			name string
			p    WaitPolicy
		}{{"spin", WaitSpin}, {"adaptive", WaitAdaptive}} {
			for _, exclusive := range []bool{false, true} {
				name := policy.name
				if exclusive {
					name += "-exclusive"
				}
				t.Run(name+"/procs="+strconv.Itoa(procs), func(t *testing.T) {
					old := runtime.GOMAXPROCS(procs)
					defer runtime.GOMAXPROCS(old)
					_, a, b := newMemoryPair(t, 4, 64, LinkOptions{WaitPolicy: policy.p, Exclusive: exclusive})

					done := make(chan error, 1)
					go func() {
						buf := make([]byte, 64)
						for range rounds {
							n, err := b.Read(buf)
							if err != nil {
								done <- err
								return
							}
							if _, err := b.Write(buf[:n]); err != nil {
								done <- err
								return
							}
						}
						done <- nil
					}()
					go func() {
						msg := make([]byte, 64)
						buf := make([]byte, 64)
						for range rounds {
							if _, err := a.Write(msg); err != nil {
								done <- err
								return
							}
							if _, err := a.Read(buf); err != nil {
								done <- err
								return
							}
						}
						done <- nil
					}()
					for range 2 {
						if err := waitErr(t, done, 30*time.Second); err != nil {
							t.Fatal(err)
						}
					}
				})
			}
		}
	}
}
