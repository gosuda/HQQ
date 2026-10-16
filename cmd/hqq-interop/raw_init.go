package main

import (
	"fmt"
	"os"
	"time"

	"gosuda.org/hqq"
	"gosuda.org/hqq/conformance"
)

// rawHeartbeatInterval is the interval the raw publish stamps into the
// Primary record (250 ms, the library default).
const rawHeartbeatInterval = 250 * time.Millisecond

// stampLayout writes the layout words of SPEC 5.2 (beginInit) and 5.3 P1.
func (s *rawSegment) stampLayout() {
	s.store(conformance.SBMajor, conformance.LayoutMajor)
	s.store(conformance.SBMinor, conformance.LayoutMinor)
	s.store(conformance.SBRequiredFeatures, 0)
}

// beginInit is SPEC 5.2: stamp the layout, take initToken from expected, then
// record the creator token and the first progress stamp.
func (s *rawSegment) beginInit(token, expected, creatorToken uint64) bool {
	s.stampLayout()
	if !s.cas(conformance.SBInitToken, expected, token) {
		return false
	}
	s.store(conformance.SBCreatorToken, creatorToken)
	s.store(conformance.SBInitHeartbeat, rawNow())
	return true
}

// owns reports whether token still owns the initialization (SPEC 5.3).
func (s *rawSegment) owns(token uint64) bool {
	return s.load(conformance.SBInitToken) == token
}

// progress is SPEC 5.3 progress: a live initializer refreshes initHeartbeat.
func (s *rawSegment) progress(token uint64) {
	if s.owns(token) {
		s.store(conformance.SBInitHeartbeat, rawNow())
	}
}

// zeroBuffers zeroes both payload pools in chunks of at most 8 MiB, checking
// ownership before and progress after each chunk (SPEC 5.3 step 2). The first
// chunk is followed by the zeroing crash point.
func (s *rawSegment) zeroBuffers(e *env, token uint64) error {
	first := true
	for _, base := range []int{s.reg.Buf0, s.reg.Buf1} {
		total := s.count * s.size
		for off := 0; off < total; off += rawZeroChunk {
			if !s.owns(token) {
				return fmt.Errorf("%w: lost initToken while zeroing", hqq.ErrFailedInit)
			}
			end := min(off+rawZeroChunk, total)
			clear(s.mem[base+off : base+end])
			s.progress(token)
			if first {
				first = false
				e.maybeCrash("zeroing")
			}
		}
	}
	return nil
}

// publish runs P1-P5 of SPEC 5.3: stamp the layout, geometry, Secondary and
// Primary records, then the magic last. The commit CAS is left to commit.
func (s *rawSegment) publish(token uint64) error {
	if !s.owns(token) {
		return fmt.Errorf("%w: lost initToken before publish", hqq.ErrFailedInit)
	}
	s.stampLayout()
	s.store(conformance.SBPageSize, uint64(os.Getpagesize()))
	s.store(conformance.SBBufferCount, uint64(s.count))
	s.store(conformance.SBBufferSize, uint64(s.size))
	s.store(conformance.SBLayoutSize, uint64(s.reg.Total))
	sec := conformance.SBEndpointSecond
	s.store(sec+conformance.RecToken, 0)
	s.store(sec+conformance.RecPID, 0)
	s.store(sec+conformance.RecHeartbeat, 0)
	s.store(sec+conformance.RecInterval, 0)
	pri := conformance.SBEndpointPrimary
	s.store(pri+conformance.RecToken, token)
	s.store(pri+conformance.RecPID, uint64(os.Getpid()))
	s.store(pri+conformance.RecInterval, uint64(rawHeartbeatInterval))
	s.store(pri+conformance.RecHeartbeat, rawNow())
	s.store(conformance.SBMagic, conformance.MagicSuperblock)
	return nil
}

// commit is the commit CAS of SPEC 5.3 followed by the epoch increment and the
// Ready store (the release point of the initialization).
func (s *rawSegment) commit(token uint64) error {
	if !s.cas(conformance.SBInitToken, token, 0) {
		return fmt.Errorf("%w: lost initToken at commit", hqq.ErrFailedInit)
	}
	s.add(conformance.SBEpoch, 1)
	s.store(conformance.SBState, conformance.StateReady)
	return nil
}

// runRawInitVictim is the initializer of the crash scenarios (X-04
// initializing, X-08). It creates the segment and performs SPEC 5.1 (Empty to
// Initializing), 5.2, 5.3 (format, zeroing, publish) and 7.2 by hand, and
// dies at --crash-after. Points: claim, format, publish, zeroing.
func runRawInitVictim(e *env) error {
	switch e.cfg.CrashAfter {
	case "claim", "format", "publish", "zeroing":
	default:
		return failf("raw initializer needs --crash-after claim|format|publish|zeroing, got %q", e.cfg.CrashAfter)
	}
	seg, err := openRawSegment(e.cfg.Name, e.cfg.BufferCount, e.cfg.BufferSize, true)
	if err != nil {
		return err
	}
	defer seg.close()
	token, creatorToken := rawToken(), rawToken()
	if !seg.cas(conformance.SBState, conformance.StateEmpty, conformance.StateInitializing) {
		return fmt.Errorf("%w: segment was not empty", hqq.ErrFailedInit)
	}
	if !seg.beginInit(token, 0, creatorToken) {
		return fmt.Errorf("%w: lost initToken at claim", hqq.ErrFailedInit)
	}
	e.maybeCrash("claim")
	for i := range 2 {
		seg.ring(i).format()
		seg.progress(token)
	}
	e.maybeCrash("format")
	if err := seg.zeroBuffers(e, token); err != nil {
		return err
	}
	if err := seg.publish(token); err != nil {
		return err
	}
	e.maybeCrash("publish")
	if err := seg.commit(token); err != nil {
		return err
	}
	return nil
}
