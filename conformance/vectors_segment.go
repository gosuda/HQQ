package conformance

import (
	"gosuda.org/hqq/internal/protocol"
)

// Image geometry shared by the superblock cases: two slots of eight bytes.
const (
	sbCount = 2
	sbSize  = 8
	// ownerInterval is the heartbeat interval written in the dead-endpoint
	// cases: 10 ms, so that the liveness watch of SPEC 6.2 ends quickly.
	ownerInterval = 10_000_000
	// staleHeartbeat is a very old wall-clock value (1 ns after the epoch):
	// the claim loop must not treat it as a fast path to takeover (SPEC 5.1).
	staleHeartbeat = 1
	// stateHeartbeat is the initHeartbeat of the initializing cases.
	stateHeartbeat = staleHeartbeat
	// recordedBig is the page size recorded in the case that must fail with
	// GeometryError on any host below 64 KiB.
	recordedBig = 65536
	// magicSTL2 is the magic of layout 2 ("HQQ_STL2" written big-endian).
	magicSTL2 uint64 = 0x4851515f53544c32
)

// sbBase returns the superblock words of a segment laid out for recorded
// page size page with the given geometry, and state zero.
func sbBase(page, count, size int) sbState {
	r, _ := ComputeRegions(page, count, size)
	return sbState{
		magic:       MagicSuperblock,
		major:       LayoutMajor,
		minor:       LayoutMinor,
		pageSize:    uint64(page),
		bufferCount: uint64(count),
		bufferSize:  uint64(size),
		layoutSize:  uint64(r.Total),
	}
}

// sbCase describes one superblock state (G-08). header returns the
// superblock words and endpoint records (nil: the zero page). full selects a
// whole-segment image with formatted rings.
type sbCase struct {
	id, title string
	// recorded is the page size written in the superblock (0: the image's
	// page family).
	recorded int
	// callerCount and callerSize are the geometry of the attach calls (0:
	// the image geometry).
	callerCount, callerSize int
	full                    bool
	header                  func(base sbState) (sbState, [2]endpointState)
	// extra writes words of the first page that header does not express.
	extra func(s *segImage)
	// roles are the expected outcomes: Creator, Opener, InMemory.
	roles [3]RoleOutcome
}

var (
	outInitPrimary = RoleOutcome{Result: "primary", Writes: "init"}
	outLinkBusy    = RoleOutcome{Error: "ErrLinkBusy", Writes: "none"}
	outIncompat    = RoleOutcome{Error: "ErrIncompatibleLayout", Writes: "none"}
	outGeometry    = RoleOutcome{Error: "ErrInvalidSize", Writes: "none"}
	outJoin        = RoleOutcome{Result: "secondary", Writes: "join"}
)

// superblockCases returns the state table of G-08. The expectations follow
// SPEC section 4 (decision table) and the claim loop of SPEC 5.1; the
// transient errors (errSegmentNotReady, errInitContended, errNoLiveEndpoint)
// are ErrFailedInit.
func superblockCases() []sbCase {
	failInit := RoleOutcome{Error: "ErrFailedInit", Writes: "none"}
	return []sbCase{
		{
			id: "empty-zero", title: "Empty segment: zero first page",
			roles: [3]RoleOutcome{outInitPrimary, failInit, outInitPrimary},
		},
		{
			id: "empty-foreign-last-word", title: "Empty state with one non-zero byte in the last word of the first page",
			extra: func(s *segImage) { s.setWord(s.page-8, 1) },
			roles: [3]RoleOutcome{outIncompat, outIncompat, outIncompat},
		},
		{
			id: "state-unknown", title: "state = 7 (unknown)",
			header: func(base sbState) (sbState, [2]endpointState) {
				base.state = 7
				return base, [2]endpointState{}
			},
			roles: [3]RoleOutcome{outIncompat, outIncompat, outIncompat},
		},
		{
			id: "initializing-live-token", title: "Initializing with a nonzero initToken and an initHeartbeat that never changes (takeover after the stale window)",
			header: func(base sbState) (sbState, [2]endpointState) {
				base.state = StateInitializing
				base.magic, base.pageSize, base.bufferCount, base.bufferSize, base.layoutSize = 0, 0, 0, 0, 0
				base.initToken = normInitToken
				base.initHeartbeat = stateHeartbeat
				return base, [2]endpointState{}
			},
			roles: [3]RoleOutcome{outInitPrimary, failInit, outInitPrimary},
		},
		{
			id: "initializing-token-zero", title: "Initializing with initToken = 0 (takeover after the stale window)",
			header: func(base sbState) (sbState, [2]endpointState) {
				base.state = StateInitializing
				base.magic, base.pageSize, base.bufferCount, base.bufferSize, base.layoutSize = 0, 0, 0, 0, 0
				return base, [2]endpointState{}
			},
			roles: [3]RoleOutcome{outInitPrimary, failInit, outInitPrimary},
		},
		{
			id: "initializing-major-4", title: "Initializing with layout major 4",
			header: func(base sbState) (sbState, [2]endpointState) {
				base.state = StateInitializing
				base.major = 4
				base.initToken = normInitToken
				base.initHeartbeat = stateHeartbeat
				return base, [2]endpointState{}
			},
			roles: [3]RoleOutcome{outIncompat, outIncompat, outIncompat},
		},
		{
			id: "ready-magic-stl2", title: "Ready with magic HQQ_STL2",
			header: func(base sbState) (sbState, [2]endpointState) {
				base.state = StateReady
				base.magic = magicSTL2
				base.epoch = 1
				return base, [2]endpointState{}
			},
			roles: [3]RoleOutcome{outIncompat, outIncompat, outIncompat},
		},
		{
			id: "ready-required-feature", title: "Ready with requiredFeatures bit 0 set",
			header: func(base sbState) (sbState, [2]endpointState) {
				base.state = StateReady
				base.epoch = 1
				base.requiredFeatures = 1
				return base, [2]endpointState{}
			},
			roles: [3]RoleOutcome{outIncompat, outIncompat, outIncompat},
		},
		{
			id: "ready-both-dead", title: "Ready, both endpoints dead (owner tokens, interval > 0, heartbeat 1)",
			header: func(base sbState) (sbState, [2]endpointState) {
				base.state = StateReady
				base.epoch = 1
				base.creatorToken = normCreatorToken
				return base, [2]endpointState{
					{token: normPrimaryToken, heartbeat: staleHeartbeat, interval: ownerInterval},
					{token: normSecondaryToken, heartbeat: staleHeartbeat, interval: ownerInterval},
				}
			},
			roles: [3]RoleOutcome{outInitPrimary, failInit, outInitPrimary},
		},
		{
			id: "ready-primary-detached", title: "Ready, primary detached (token 1), secondary empty",
			header: func(base sbState) (sbState, [2]endpointState) {
				base.state = StateReady
				base.epoch = 1
				base.creatorToken = normCreatorToken
				return base, [2]endpointState{{token: TokenDetached}, {}}
			},
			roles: [3]RoleOutcome{outInitPrimary, failInit, outInitPrimary},
		},
		{
			id: "ready-primary-live-secondary-free", title: "Ready, primary live (interval 0), secondary free; rings formatted so that a join can attach",
			full: true,
			header: func(base sbState) (sbState, [2]endpointState) {
				base.state = StateReady
				base.epoch = 1
				base.creatorToken = normCreatorToken
				return base, [2]endpointState{{token: normPrimaryToken}, {}}
			},
			roles: [3]RoleOutcome{outLinkBusy, outJoin, outJoin},
		},
		{
			id: "ready-both-live", title: "Ready, both endpoints live (interval 0)",
			header: func(base sbState) (sbState, [2]endpointState) {
				base.state = StateReady
				base.epoch = 1
				base.creatorToken = normCreatorToken
				return base, [2]endpointState{{token: normPrimaryToken}, {token: normSecondaryToken}}
			},
			roles: [3]RoleOutcome{outLinkBusy, outLinkBusy, outLinkBusy},
		},
		{
			id: "ready-primary-live-secondary-detached", title: "Ready, primary live, secondary owned by a detached endpoint (token 1)",
			header: func(base sbState) (sbState, [2]endpointState) {
				base.state = StateReady
				base.epoch = 1
				base.creatorToken = normCreatorToken
				return base, [2]endpointState{{token: normPrimaryToken}, {token: TokenDetached}}
			},
			roles: [3]RoleOutcome{outLinkBusy, outLinkBusy, outLinkBusy},
		},
		{
			id: "geometry-mismatch-count", title: "Ready, primary live and secondary free; the attach calls use bufferCount 4",
			callerCount: 4, callerSize: sbSize,
			header: func(base sbState) (sbState, [2]endpointState) {
				base.state = StateReady
				base.epoch = 1
				base.creatorToken = normCreatorToken
				return base, [2]endpointState{{token: normPrimaryToken}, {}}
			},
			roles: [3]RoleOutcome{outLinkBusy, outGeometry, outGeometry},
		},
		{
			id: "geometry-mismatch-size", title: "Ready, primary live and secondary free; the attach calls use bufferSize 16",
			callerCount: sbCount, callerSize: 16,
			header: func(base sbState) (sbState, [2]endpointState) {
				base.state = StateReady
				base.epoch = 1
				base.creatorToken = normCreatorToken
				return base, [2]endpointState{{token: normPrimaryToken}, {}}
			},
			roles: [3]RoleOutcome{outLinkBusy, outGeometry, outGeometry},
		},
		{
			id: "ready-recorded-page-65536", title: "Ready, primary live and secondary free, recorded page size 65536 (never the host page below 64 KiB)",
			recorded: recordedBig,
			header: func(base sbState) (sbState, [2]endpointState) {
				base.state = StateReady
				base.epoch = 1
				base.creatorToken = normCreatorToken
				return base, [2]endpointState{{token: normPrimaryToken}, {}}
			},
			roles: [3]RoleOutcome{outLinkBusy, outGeometry, outGeometry},
		},
	}
}

// superblockVectors builds the superblock images of every case and page family (G-08).
func superblockVectors(files map[string][]byte) ([]SuperblockCase, error) {
	var out []SuperblockCase
	for _, c := range superblockCases() {
		families := PageFamilies
		if c.recorded != 0 {
			// The recorded-page case is a 16 KiB image that records 64 KiB.
			families = []int{16384}
		}
		for _, page := range families {
			rec := page
			if c.recorded != 0 {
				rec = c.recorded
			}
			s, err := newSegImage(page, sbCount, sbSize)
			if err != nil {
				return nil, err
			}
			base := sbBase(rec, sbCount, sbSize)
			if c.header != nil {
				sb, ep := c.header(base)
				s.setSuperblock(sb, ep)
				if c.full {
					s.formatRing(0)
					s.formatRing(1)
				}
			}
			if c.extra != nil {
				c.extra(s)
			}
			file := s.b[:page]
			if c.full {
				file = s.b
			}
			name := familyPath("images", page, c.id+".bin")
			if err := putFile(files, name, append([]byte(nil), file...)); err != nil {
				return nil, err
			}
			callerCount, callerSize := c.callerCount, c.callerSize
			if callerCount == 0 {
				callerCount = sbCount
			}
			if callerSize == 0 {
				callerSize = sbSize
			}
			out = append(out, SuperblockCase{
				ID:                c.id,
				Title:             c.title,
				PageSize:          page,
				RecordedPageSize:  rec,
				File:              name,
				FileBytes:         len(file),
				Full:              c.full,
				SegmentBytes:      s.reg.Total,
				BufferCount:       sbCount,
				BufferSize:        sbSize,
				CallerBufferCount: callerCount,
				CallerBufferSize:  callerSize,
				Normalized:        normalizedWords(s.b),
				Creator:           c.roles[0],
				Opener:            c.roles[1],
				InMemory:          c.roles[2],
			})
		}
	}
	return out, nil
}

// ringCaseSpec builds one ring image of ring cases (G-07).
type ringCaseSpec struct {
	id, title string
	count     int
	size      int
	build     func(s *segImage, seed uint64) (ring0, ring1 RingState, deq []RingWord, write []RingWord)
}

// ringPayload is the payload of message pos of a ring case.
func ringPayload(seed, pos uint64, size int) []byte {
	return Payload(seed, pos, size)
}

// publishRing stores the published messages of direction d at the given positions
// (payloads Payload(seed, pos, size)).
func publishRing(s *segImage, d int, seed uint64, positions ...uint64) {
	for _, pos := range positions {
		slot := int(pos & uint64(s.count-1))
		pkt := protocol.NewStandardLinkCopyPacket(pos+1, uint64(slot), uint64(s.size))
		s.publish(d, pos, pkt, ringPayload(seed, pos, s.size))
	}
}

// messageResults returns the expected Dequeue results for the given positions.
func messageResults(seed uint64, size int, positions ...uint64) []RingWord {
	var out []RingWord
	for _, pos := range positions {
		out = append(out, RingWord{Kind: "message", Hex: hexOf(ringPayload(seed, pos, size))})
	}
	return out
}

const ringSeed = 0x5eed

// ringCases returns the ring image cases of G-07. Ring 0 carries the
// messages a Secondary reads; ring 1 is the Secondary's transmit ring.
func ringCases() []ringCaseSpec {
	const m64 = ^uint64(0) // 2^64 - 1
	return []ringCaseSpec{
		{
			id: "formatted-empty", title: "Both rings formatted and empty; one write fits ring 1",
			count: 4, size: 16,
			build: func(s *segImage, seed uint64) (RingState, RingState, []RingWord, []RingWord) {
				return RingState{}, RingState{}, []RingWord{{Kind: "empty"}},
					[]RingWord{{Kind: "written", N: s.size}}
			},
		},
		{
			id: "wrapped", title: "Cursors past three laps: r = 12, w = 15 with N = 4 (SPEC 7.4 slot sequences)",
			count: 4, size: 16,
			build: func(s *segImage, seed uint64) (RingState, RingState, []RingWord, []RingWord) {
				publishRing(s, 0, seed, 12, 13, 14)
				s.setSlotSeq(0, 3, 15) // slot 3 was released by position 11: free for position 15
				s.setCursors(0, 12, 15)
				return RingState{R: 12, W: 15, Published: []Hex64{12, 13, 14}}, RingState{},
					append(messageResults(seed, s.size, 12, 13, 14), RingWord{Kind: "empty"}), nil
			},
		},
		{
			id: "wrapped-2pow64", title: "Positions near 2^64: r = 2^64-2, w = 1 (modulo arithmetic), published M-2, M-1, 0",
			count: 4, size: 16,
			build: func(s *segImage, seed uint64) (RingState, RingState, []RingWord, []RingWord) {
				publishRing(s, 0, seed, m64-1, m64, 0)
				s.setSlotSeq(0, 1, 1) // slot 1 was released by position 2^64-3: free for position 1
				s.setCursors(0, m64-1, 1)
				return RingState{R: Hex64(m64 - 1), W: 1, Published: []Hex64{Hex64(m64 - 1), Hex64(m64), 0}}, RingState{},
					append(messageResults(seed, s.size, m64-1, m64, 0), RingWord{Kind: "empty"}), nil
			},
		},
		{
			id: "empty-at-2pow64", title: "Empty rings with r = w = 2^64-2; each slot's sequence is the first position of the next lap that maps to it; ring 1 accepts a write across the wrap",
			count: 4, size: 16,
			build: func(s *segImage, seed uint64) (RingState, RingState, []RingWord, []RingWord) {
				const base = ^uint64(0) - 1 // 2^64 - 2
				for slot := range s.count {
					// The smallest position p >= base with p mod N == slot, in
					// modulo arithmetic: N divides 2^64, so the offset mod N is exact.
					p := base + ((uint64(slot) - base) & uint64(s.count-1))
					s.setSlotSeq(0, slot, p)
					s.setSlotSeq(1, slot, p)
				}
				s.setCursors(0, base, base)
				s.setCursors(1, base, base)
				return RingState{R: Hex64(base), W: Hex64(base)}, RingState{R: Hex64(base), W: Hex64(base)},
					[]RingWord{{Kind: "empty"}},
					[]RingWord{{Kind: "written", N: s.size}}
			},
		},
		{
			id: "full", title: "Ring 1 full (two published, not yet released): a Secondary Write times out",
			count: 2, size: 8,
			build: func(s *segImage, seed uint64) (RingState, RingState, []RingWord, []RingWord) {
				publishRing(s, 1, seed, 0, 1)
				s.setCursors(1, 0, 2)
				return RingState{}, RingState{R: 0, W: 2, Published: []Hex64{0, 1}},
					[]RingWord{{Kind: "empty"}},
					[]RingWord{{Kind: "error", Error: "ErrTimeout"}}
			},
		},
		{
			id: "half-published", title: "Ring 0: position 2 claimed but not published (w advanced, seq unchanged)",
			count: 4, size: 16,
			build: func(s *segImage, seed uint64) (RingState, RingState, []RingWord, []RingWord) {
				publishRing(s, 0, seed, 0, 1)
				s.setCursors(0, 0, 3)
				return RingState{R: 0, W: 3, Published: []Hex64{0, 1}, Claimed: []Hex64{2}}, RingState{},
					append(messageResults(seed, s.size, 0, 1), RingWord{Kind: "empty"}), nil
			},
		},
	}
}

// ringVectors builds the ring images for every page family.
func ringVectors(files map[string][]byte) ([]RingCase, error) {
	var out []RingCase
	for _, spec := range ringCases() {
		for _, page := range PageFamilies {
			s, err := newSegImage(page, spec.count, spec.size)
			if err != nil {
				return nil, err
			}
			base := sbBase(page, spec.count, spec.size)
			s.setSuperblock(withState(base, StateReady, 1), [2]endpointState{{token: normPrimaryToken}, {}})
			s.formatRing(0)
			s.formatRing(1)
			r0, r1, deq, wr := spec.build(s, ringSeed)
			name := familyPath("rings", page, spec.id+".bin")
			if err := putFile(files, name, append([]byte(nil), s.b...)); err != nil {
				return nil, err
			}
			out = append(out, RingCase{
				ID:          spec.id,
				Title:       spec.title,
				PageSize:    page,
				File:        name,
				BufferCount: spec.count,
				BufferSize:  spec.size,
				Ring0:       nonNilRing(r0),
				Ring1:       nonNilRing(r1),
				Dequeue:     deq,
				Write:       wr,
				Seed:        ringSeed,
			})
		}
	}
	return out, nil
}

// nonNilRing returns s with empty (not nil) position lists, so that they are
// encoded as JSON arrays.
func nonNilRing(s RingState) RingState {
	if s.Published == nil {
		s.Published = []Hex64{}
	}
	if s.Claimed == nil {
		s.Claimed = []Hex64{}
	}
	return s
}

// withState returns sb with the state, epoch and creator set for a Ready
// segment written by a creator.
func withState(sb sbState, state, epoch uint64) sbState {
	sb.state = state
	sb.epoch = epoch
	sb.creatorToken = normCreatorToken
	return sb
}
