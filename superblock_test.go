package hqq

import (
	"bytes"
	"context"
	"errors"
	"math"
	"math/bits"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unsafe"
)

// fastInit makes initializer takeovers and stale-endpoint decisions
// observable within a fraction of a second.
var fastInit = LinkOptions{
	HeartbeatInterval: 10 * time.Millisecond,
	PeerTimeout:       150 * time.Millisecond,
	AttachTimeout:     500 * time.Millisecond,
	initStale:         60 * time.Millisecond,
}

const (
	testBufferCount = 4
	testBufferSize  = 64

	// layoutV2Magic is the superblock magic of layout version 2.
	layoutV2Magic uint64 = 0x4851515f53544c32
)

func newSuperblockMem() ([]byte, *standardSuperblock) {
	mem := AlignedBuffer(SizeStandardLink(testBufferCount, testBufferSize))
	return mem, (*standardSuperblock)(unsafe.Pointer(unsafe.SliceData(mem)))
}

func load(p *uint64) uint64     { return atomic.LoadUint64(p) }
func store(p *uint64, v uint64) { atomic.StoreUint64(p, v) }

// TestSuperblockLayoutV3 pins the shared-memory ABI of the superblock: the
// magic, the layout version, and the offsets of the words used for required
// features and the creator token.
func TestSuperblockLayoutV3(t *testing.T) {
	if superblockMagic != 0x4851515F53544C33 {
		t.Fatalf("magic = %#x, want 0x4851515F53544C33 (HQQ_STL3)", superblockMagic)
	}
	if superblockMajor != 3 || superblockMinor != 0 || knownRequiredFeatures != 0 {
		t.Fatalf("layout version %d.%d, known features %#x", superblockMajor, superblockMinor, knownRequiredFeatures)
	}

	mem, sb := newSuperblockMem()
	for off, field := range map[int]*uint64{
		0: &sb.magic, 8: &sb.state, 16: &sb.major, 24: &sb.minor, 32: &sb.pageSize,
		40: &sb.bufferCount, 48: &sb.bufferSize, 56: &sb.layoutSize, 64: &sb.epoch,
		72: &sb.initToken, 80: &sb.initHeartbeat, 88: &sb.requiredFeatures,
		96: &sb.creatorToken, 128: &sb.endpoints[0].token, 256: &sb.endpoints[1].token,
	} {
		if got := uintptr(unsafe.Pointer(field)) - uintptr(unsafe.Pointer(&mem[0])); got != uintptr(off) {
			t.Errorf("field at offset %d is at %d", off, got)
		}
	}

	link, err := NewStandardLink(mem, testBufferCount, testBufferSize, LinkOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer link.Close()
	if load(&sb.magic) != superblockMagic || load(&sb.state) != superblockStateReady ||
		load(&sb.major) != superblockMajor || load(&sb.minor) != superblockMinor ||
		load(&sb.requiredFeatures) != 0 || load(&sb.creatorToken) != 0 {
		t.Fatalf("published superblock: %+v", *sb)
	}
}

// TestInitializingSegmentCarriesLayout checks that a segment is stamped with
// its layout version as soon as it is claimed, before it is ready.
func TestInitializingSegmentCarriesLayout(t *testing.T) {
	_, sb := newSuperblockMem()
	store(&sb.state, superblockStateInitializing)
	if !beginInit(sb, 77, 0, LinkOptions{creatorToken: 99}) {
		t.Fatal("beginInit failed on an unowned segment")
	}
	if load(&sb.major) != superblockMajor || load(&sb.minor) != superblockMinor || load(&sb.requiredFeatures) != 0 ||
		load(&sb.initToken) != 77 || load(&sb.creatorToken) != 99 || load(&sb.initHeartbeat) == 0 {
		t.Fatalf("claimed superblock: %+v", *sb)
	}
	if beginInit(sb, 88, 0, LinkOptions{}) {
		t.Fatal("beginInit succeeded although the segment is owned by another initializer")
	}
	if load(&sb.initToken) != 77 || load(&sb.creatorToken) != 99 {
		t.Fatalf("a failed beginInit changed the owner: %+v", *sb)
	}
}

func TestClaimRejectsIncompatibleLayouts(t *testing.T) {
	mutations := map[string]func(sb *standardSuperblock){
		"layout v2 magic":          func(sb *standardSuperblock) { store(&sb.magic, layoutV2Magic) },
		"foreign magic":            func(sb *standardSuperblock) { store(&sb.magic, 0xbad) },
		"other major version":      func(sb *standardSuperblock) { store(&sb.major, superblockMajor-1) },
		"unknown required feature": func(sb *standardSuperblock) { store(&sb.requiredFeatures, 1<<3) },
	}
	for name, mutate := range mutations {
		t.Run("ready/closed/"+name, func(t *testing.T) {
			// Both endpoints are gone, so the segment would be reused if it
			// were ours: the layout must be checked before the takeover.
			mem, sb := newSuperblockMem()
			a, err := NewStandardLink(mem, testBufferCount, testBufferSize, fastInit)
			if err != nil {
				t.Fatal(err)
			}
			a.Close()
			mutate(sb)
			before := *sb
			if _, err := NewStandardLink(mem, testBufferCount, testBufferSize, fastInit); !errors.Is(err, ErrIncompatibleLayout) {
				t.Fatalf("open = %v, want ErrIncompatibleLayout", err)
			}
			if load(&sb.state) != superblockStateReady || load(&sb.initToken) != 0 || sb.magic != before.magic ||
				sb.major != before.major || sb.requiredFeatures != before.requiredFeatures {
				t.Fatalf("segment was modified: %+v", *sb)
			}
		})
		t.Run("ready/live/"+name, func(t *testing.T) {
			mem, sb := newSuperblockMem()
			a, err := NewStandardLink(mem, testBufferCount, testBufferSize, fastInit)
			if err != nil {
				t.Fatal(err)
			}
			defer a.Close()
			mutate(sb)
			if _, err := NewStandardLink(mem, testBufferCount, testBufferSize, fastInit); !errors.Is(err, ErrIncompatibleLayout) {
				t.Fatalf("join = %v, want ErrIncompatibleLayout", err)
			}
		})
		t.Run("initializing/"+name, func(t *testing.T) {
			// A stalled initializer of another layout must not be taken over.
			mem, sb := newSuperblockMem()
			store(&sb.state, superblockStateInitializing)
			store(&sb.initToken, 0xabc)
			store(&sb.initHeartbeat, 1)
			store(&sb.major, superblockMajor)
			mutate(sb)
			start := time.Now()
			if _, err := NewStandardLink(mem, testBufferCount, testBufferSize, fastInit); !errors.Is(err, ErrIncompatibleLayout) {
				t.Fatalf("open = %v, want ErrIncompatibleLayout", err)
			}
			if time.Since(start) >= fastInit.initStale {
				t.Fatalf("incompatible layout detected only after %v", time.Since(start))
			}
			if load(&sb.initToken) != 0xabc || load(&sb.state) != superblockStateInitializing {
				t.Fatalf("segment was modified: %+v", *sb)
			}
		})
	}

	t.Run("unknown state", func(t *testing.T) {
		mem, sb := newSuperblockMem()
		store(&sb.state, 7)
		if _, err := NewStandardLink(mem, testBufferCount, testBufferSize, fastInit); !errors.Is(err, ErrIncompatibleLayout) {
			t.Fatalf("open = %v, want ErrIncompatibleLayout", err)
		}
	})
}

func TestGeometryMismatchReportsBothGeometries(t *testing.T) {
	mem := AlignedBuffer(SizeStandardLink(16, 128))
	a, err := NewStandardLink(mem, 8, 64, fastInit)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	_, err = NewStandardLink(mem, 16, 64, fastInit)
	var ge *GeometryError
	if !errors.As(err, &ge) || !errors.Is(err, ErrInvalidSize) {
		t.Fatalf("mismatched bufferCount = %v, want *GeometryError matching ErrInvalidSize", err)
	}
	if ge.Want.BufferCount != 16 || ge.Want.BufferSize != 64 || ge.Want.LayoutSize != SizeStandardLink(16, 64) || ge.Want.PageSize != pagesize ||
		ge.Got.BufferCount != 8 || ge.Got.BufferSize != 64 || ge.Got.LayoutSize != SizeStandardLink(8, 64) || ge.Got.PageSize != pagesize {
		t.Fatalf("GeometryError = %+v", *ge)
	}
	if _, err = NewStandardLink(mem, 8, 128, fastInit); !errors.As(err, &ge) {
		t.Fatalf("mismatched bufferSize = %v, want *GeometryError", err)
	}
}

func TestOpenerWaitsForCreatorOnEmptySegment(t *testing.T) {
	mem, sb := newSuperblockMem()
	opener := fastInit
	opener.AttachTimeout = 10 * time.Second
	opener.role = roleOpener
	type result struct {
		link *StandardLink
		err  error
	}
	done := make(chan result, 1)
	go func() {
		l, err := NewStandardLink(mem, testBufferCount, testBufferSize, opener)
		done <- result{l, err}
	}()

	// The creator has not claimed the segment: the opener must leave it be.
	time.Sleep(100 * time.Millisecond)
	if load(&sb.state) != superblockStateEmpty {
		t.Fatal("opener claimed an empty segment before AttachTimeout")
	}
	select {
	case r := <-done:
		t.Fatalf("opener returned early: %v", r.err)
	default:
	}

	creator := fastInit
	creator.role = roleCreator
	c, err := NewStandardLink(mem, testBufferCount, testBufferSize, creator)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	select {
	case r := <-done:
		if r.err != nil {
			t.Fatalf("opener = %v", r.err)
		}
		defer r.link.Close()
		if c.Mode() != LinkModePrimary || r.link.Mode() != LinkModeSecondary {
			t.Fatalf("modes = creator %v, opener %v", c.Mode(), r.link.Mode())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("opener did not attach after the creator claimed the segment")
	}
}

// TestOpenerNeverInitializesAbandonedEmptySegment: an opener over an empty
// segment whose creator never claimed it gives up with a retryable error and
// leaves the segment to a new creator, instead of becoming a Primary that
// nobody would ever join.
func TestOpenerNeverInitializesAbandonedEmptySegment(t *testing.T) {
	mem, sb := newSuperblockMem()
	opener := fastInit
	opener.AttachTimeout = 150 * time.Millisecond
	opener.role = roleOpener
	start := time.Now()
	if _, err := NewStandardLink(mem, testBufferCount, testBufferSize, opener); !errors.Is(err, errSegmentNotReady) || !dialRetryable(err) {
		t.Fatalf("opener over a creator-less empty segment = %v, want retryable errSegmentNotReady", err)
	}
	if elapsed := time.Since(start); elapsed < opener.AttachTimeout {
		t.Fatalf("opener gave up after only %v", elapsed)
	}
	if load(&sb.state) != superblockStateEmpty {
		t.Fatal("opener wrote to the abandoned segment")
	}

	creator := fastInit
	creator.role = roleCreator
	link, err := NewStandardLink(mem, testBufferCount, testBufferSize, creator)
	if err != nil {
		t.Fatalf("creator reusing the abandoned segment = %v", err)
	}
	defer link.Close()
	if link.Mode() != LinkModePrimary {
		t.Fatalf("creator mode = %v, want primary", link.Mode())
	}
}

// TestOpenerNeverReinitializesDeadSegment: when every endpoint of a Ready
// segment is gone, an opener fails with a retryable error and leaves the
// segment untouched; only a creator re-initializes it.
func TestOpenerNeverReinitializesDeadSegment(t *testing.T) {
	mem, sb := newSuperblockMem()
	first, err := NewStandardLink(mem, testBufferCount, testBufferSize, fastInit)
	if err != nil {
		t.Fatal(err)
	}
	first.Close()
	epoch := load(&sb.epoch)

	opener := fastInit
	opener.role = roleOpener
	if _, err := NewStandardLink(mem, testBufferCount, testBufferSize, opener); !errors.Is(err, errNoLiveEndpoint) || !dialRetryable(err) {
		t.Fatalf("opener over a dead segment = %v, want retryable errNoLiveEndpoint", err)
	}
	if load(&sb.epoch) != epoch || load(&sb.state) != superblockStateReady {
		t.Fatal("opener re-initialized a dead segment")
	}

	creator := fastInit
	creator.role = roleCreator
	link, err := NewStandardLink(mem, testBufferCount, testBufferSize, creator)
	if err != nil {
		t.Fatalf("creator reusing the dead segment = %v", err)
	}
	defer link.Close()
	if link.Mode() != LinkModePrimary || load(&sb.epoch) == epoch {
		t.Fatalf("creator mode = %v, epoch %d (was %d): want a re-initialized Primary", link.Mode(), load(&sb.epoch), epoch)
	}
}

func TestEmptySegmentWithDataIsNeverClaimed(t *testing.T) {
	for _, role := range []attachRole{roleAny, roleCreator, roleOpener} {
		for _, off := range []uintptr{8 * 17, 4096 - 8, pagesize - 8} {
			mem, sb := newSuperblockMem()
			word := (*uint64)(unsafe.Add(unsafe.Pointer(sb), off))
			store(word, 0xbadbad)
			opts := fastInit
			opts.role = role
			opts.AttachTimeout = 100 * time.Millisecond
			start := time.Now()
			if _, err := NewStandardLink(mem, testBufferCount, testBufferSize, opts); !errors.Is(err, ErrIncompatibleLayout) {
				t.Fatalf("role %d, word at %d: open = %v, want ErrIncompatibleLayout", role, off, err)
			}
			if time.Since(start) >= opts.AttachTimeout {
				t.Errorf("role %d, word at %d: foreign data detected only after %v", role, off, time.Since(start))
			}
			if load(&sb.state) != superblockStateEmpty || load(word) != 0xbadbad {
				t.Fatalf("role %d, word at %d: segment was written", role, off)
			}
			for i := uintptr(0); i < pagesize; i += 8 {
				if p := (*uint64)(unsafe.Add(unsafe.Pointer(sb), i)); i != off && load(p) != 0 {
					t.Fatalf("role %d, word at %d: word at %d was written", role, off, i)
				}
			}
		}
	}
}

// TestConcurrentClaimsOfEmptySegment checks that the first-page check never
// mistakes a concurrent claimer's writes for foreign data.
func TestConcurrentClaimsOfEmptySegment(t *testing.T) {
	for range 200 {
		mem, _ := newSuperblockMem()
		var start, wg sync.WaitGroup
		start.Add(1)
		links := make([]*StandardLink, 2)
		errs := make([]error, 2)
		for i := range links {
			wg.Add(1)
			go func() {
				defer wg.Done()
				start.Wait()
				links[i], errs[i] = NewStandardLink(mem, testBufferCount, testBufferSize, LinkOptions{})
			}()
		}
		start.Done()
		wg.Wait()
		for i := range links {
			if errs[i] != nil {
				t.Fatalf("concurrent open %d = %v", i, errs[i])
			}
		}
		if links[0].Mode() == links[1].Mode() {
			t.Fatalf("both endpoints are %v", links[0].Mode())
		}
		links[0].Close()
		links[1].Close()
	}
}

// TestInitTakeoverWaitsForStall covers initializers that crashed: one right
// after claiming the segment (no token yet) and ones that stopped
// heartbeating. Taking over needs the initializer's token and heartbeat to
// stay unchanged for the whole window, whatever the wall clock says.
func TestInitTakeoverWaitsForStall(t *testing.T) {
	cases := map[string]struct{ token, heartbeat uint64 }{
		"claimed, nothing recorded": {0, 0},
		"claimed, old heartbeat":    {0, 1},
		"ancient heartbeat":         {0xdead, 1},
		"heartbeat in the future":   {0xdead, uint64(time.Now().Add(time.Hour).UnixNano())},
		"no heartbeat":              {0xdead, 0},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			mem, sb := newSuperblockMem()
			store(&sb.state, superblockStateInitializing)
			store(&sb.initToken, c.token)
			if c.token != 0 {
				store(&sb.major, superblockMajor) // an initializer that owns a token has stamped its layout
			}
			store(&sb.initHeartbeat, c.heartbeat)
			start := time.Now()
			link, err := NewStandardLink(mem, testBufferCount, testBufferSize, fastInit)
			if err != nil {
				t.Fatalf("takeover = %v", err)
			}
			defer link.Close()
			if elapsed := time.Since(start); elapsed < fastInit.initStale {
				t.Fatalf("took the segment over after %v, before the %v window", elapsed, fastInit.initStale)
			}
			if link.Mode() != LinkModePrimary || load(&sb.initToken) != 0 || load(&sb.state) != superblockStateReady ||
				load(&sb.major) != superblockMajor {
				t.Fatalf("after takeover: mode %v, %+v", link.Mode(), *sb)
			}
			peer, err := NewStandardLink(mem, testBufferCount, testBufferSize, fastInit)
			if err != nil {
				t.Fatal(err)
			}
			defer peer.Close()
			if _, err := link.Write([]byte("ok")); err != nil {
				t.Fatal(err)
			}
			buf := make([]byte, testBufferSize)
			if n, err := peer.Read(buf); err != nil || string(buf[:n]) != "ok" {
				t.Fatalf("Read = %q, %v", buf[:n], err)
			}
		})
	}
}

// TestOpenerNeverTakesOverStalledInit: a stalled initializer is taken over by
// creators and in-memory endpoints only; an opener waits and then fails with
// a retryable error, leaving the segment as it was.
func TestOpenerNeverTakesOverStalledInit(t *testing.T) {
	mem, sb := newSuperblockMem()
	store(&sb.state, superblockStateInitializing)
	store(&sb.initToken, 0xdead)
	store(&sb.major, superblockMajor)
	store(&sb.initHeartbeat, 1)
	opener := fastInit
	opener.role = roleOpener
	opener.AttachTimeout = 4 * fastInit.initStale
	if _, err := NewStandardLink(mem, testBufferCount, testBufferSize, opener); !errors.Is(err, errInitContended) || !dialRetryable(err) {
		t.Fatalf("opener over a stalled initializer = %v, want retryable errInitContended", err)
	}
	if load(&sb.initToken) != 0xdead || load(&sb.state) != superblockStateInitializing {
		t.Fatal("opener took over a stalled initialization")
	}
}

// TestAliveInitializerIsNotTakenOver keeps an initializer making progress
// with a heartbeat that is ancient on the wall clock: only the absence of
// progress may trigger a takeover.
func TestAliveInitializerIsNotTakenOver(t *testing.T) {
	mem, sb := newSuperblockMem()
	store(&sb.state, superblockStateInitializing)
	store(&sb.initToken, 0xdead)
	store(&sb.major, superblockMajor) // an initializer that owns a token has stamped its layout
	store(&sb.initHeartbeat, 1)

	stop, stopped := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(stopped)
		for beat := uint64(2); ; beat++ {
			select {
			case <-stop:
				return
			default:
			}
			store(&sb.initHeartbeat, beat)
			time.Sleep(time.Millisecond)
		}
	}()

	patient := fastInit
	patient.initStale = 400 * time.Millisecond
	patient.AttachTimeout = 100 * time.Millisecond
	start := time.Now()
	_, err := NewStandardLink(mem, testBufferCount, testBufferSize, patient)
	if !errors.Is(err, errInitContended) || !errors.Is(err, ErrFailedInit) {
		t.Fatalf("open next to a live initializer = %v, want errInitContended", err)
	}
	if elapsed := time.Since(start); elapsed >= patient.initStale {
		t.Fatalf("waited %v for a live initializer", elapsed)
	}
	if load(&sb.initToken) != 0xdead {
		t.Fatal("a live initializer was taken over")
	}

	close(stop)
	<-stopped
	link, err := NewStandardLink(mem, testBufferCount, testBufferSize, fastInit)
	if err != nil {
		t.Fatalf("takeover after the initializer stalled = %v", err)
	}
	link.Close()
}

func TestInitSessionOwnership(t *testing.T) {
	_, sb := newSuperblockMem()
	const token, other = 11, 22
	s := &initSession{sb: sb, token: token, ctx: context.Background()}
	region := bytes.Repeat([]byte{0xff}, 1024)

	// Another endpoint owns the initialization: nothing may be touched.
	store(&sb.initToken, other)
	store(&sb.initHeartbeat, 77)
	if err := s.check(); err != errInitContended {
		t.Fatalf("check = %v, want errInitContended", err)
	}
	s.progress()
	if load(&sb.initHeartbeat) != 77 {
		t.Fatal("progress refreshed the heartbeat of an initializer that lost ownership")
	}
	if err := s.zero(region); err != errInitContended {
		t.Fatalf("zero = %v, want errInitContended", err)
	}
	if !bytes.Equal(region, bytes.Repeat([]byte{0xff}, len(region))) {
		t.Fatal("zero cleared memory without owning the initialization")
	}

	// The owner may.
	store(&sb.initToken, token)
	if err := s.check(); err != nil {
		t.Fatalf("check = %v", err)
	}
	s.progress()
	if load(&sb.initHeartbeat) == 77 {
		t.Fatal("progress did not refresh the heartbeat of the owner")
	}
	if err := s.zero(region); err != nil || !bytes.Equal(region, make([]byte, len(region))) {
		t.Fatalf("zero = %v, region cleared: %v", err, bytes.Equal(region, make([]byte, len(region))))
	}

	// A done context aborts with the context's error, not errInitContended.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	s.ctx = ctx
	if err := s.check(); !errors.Is(err, context.Canceled) {
		t.Fatalf("check on a canceled context = %v", err)
	}
}

func TestZeroInChunksChecksBeforeEveryChunk(t *testing.T) {
	b := bytes.Repeat([]byte{0xff}, 10)
	checks, progress := 0, 0
	errLost := errors.New("lost")
	err := zeroInChunks(b, 3, func() error {
		checks++
		if checks == 3 {
			return errLost
		}
		return nil
	}, func() { progress++ })
	if err != errLost || checks != 3 || progress != 2 {
		t.Fatalf("err %v, %d checks, %d progress calls", err, checks, progress)
	}
	if !bytes.Equal(b[:6], make([]byte, 6)) || !bytes.Equal(b[6:], bytes.Repeat([]byte{0xff}, 4)) {
		t.Fatalf("memory after abort: %x", b)
	}

	checks, progress = 0, 0
	if err := zeroInChunks(b, 3, func() error { checks++; return nil }, func() { progress++ }); err != nil || checks != 4 || progress != 4 {
		t.Fatalf("full run: err %v, %d checks, %d progress calls", err, checks, progress)
	}
}

func TestPublishCommitsOwnership(t *testing.T) {
	const token, other = 31, 42
	size := SizeStandardLink(testBufferCount, testBufferSize)

	t.Run("owner", func(t *testing.T) {
		_, sb := newSuperblockMem()
		store(&sb.state, superblockStateInitializing)
		store(&sb.initToken, token)
		epoch, err := publishSuperblock(sb, testBufferCount, testBufferSize, size, token, fastInit)
		if err != nil || epoch != 1 {
			t.Fatalf("publish = %d, %v", epoch, err)
		}
		if load(&sb.state) != superblockStateReady || load(&sb.initToken) != 0 || load(&sb.magic) != superblockMagic ||
			load(&sb.endpoints[endpointPrimary].token) != token || load(&sb.bufferCount) != testBufferCount {
			t.Fatalf("published superblock: %+v", *sb)
		}
	})

	t.Run("commit lost", func(t *testing.T) {
		// Ownership moved between the field writes and the commit.
		_, sb := newSuperblockMem()
		store(&sb.state, superblockStateInitializing)
		store(&sb.initToken, other)
		if epoch, err := commitInit(sb, token); err != errInitContended || epoch != 0 {
			t.Fatalf("commit = %d, %v, want errInitContended", epoch, err)
		}
		if load(&sb.state) != superblockStateInitializing || load(&sb.initToken) != other || load(&sb.epoch) != 0 {
			t.Fatalf("a lost commit changed the segment: %+v", *sb)
		}
	})

	t.Run("lost before publish", func(t *testing.T) {
		_, sb := newSuperblockMem()
		store(&sb.state, superblockStateInitializing)
		store(&sb.initToken, other)
		if _, err := publishSuperblock(sb, testBufferCount, testBufferSize, size, token, fastInit); err != errInitContended {
			t.Fatalf("publish = %v, want errInitContended", err)
		}
		if load(&sb.state) != superblockStateInitializing || load(&sb.magic) != 0 || load(&sb.bufferCount) != 0 ||
			load(&sb.endpoints[endpointPrimary].token) != 0 {
			t.Fatalf("a lost publish wrote the segment: %+v", *sb)
		}
	})
}

func TestEndpointAliveAtOpenStopsWithContext(t *testing.T) {
	_, sb := newSuperblockMem()
	rec := &sb.endpoints[endpointPrimary]
	store(&rec.token, 5)
	store(&rec.interval, uint64(time.Second))
	store(&rec.heartbeat, 1) // stale: the watch window is 2s
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	alive, err := endpointAliveAtOpen(ctx, rec, fastInit)
	if !errors.Is(err, context.DeadlineExceeded) || alive {
		t.Fatalf("endpointAliveAtOpen = %v, %v", alive, err)
	}
	if time.Since(start) > time.Second {
		t.Fatalf("watch ran for %v past its context", time.Since(start))
	}
}

func TestClaimStopsWithContext(t *testing.T) {
	mem, sb := newSuperblockMem()
	store(&sb.state, superblockStateInitializing)
	store(&sb.initToken, 0xabc)
	store(&sb.major, superblockMajor)
	store(&sb.initHeartbeat, nowNanos())
	slow := fastInit
	slow.AttachTimeout = 10 * time.Second
	slow.initStale = 10 * time.Second
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	slow.ctx = ctx
	start := time.Now()
	if _, err := NewStandardLink(mem, testBufferCount, testBufferSize, slow); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("open = %v, want context.DeadlineExceeded", err)
	}
	if time.Since(start) > time.Second {
		t.Fatalf("open took %v despite its context", time.Since(start))
	}
}

func TestAttachRingStopsWithContext(t *testing.T) {
	// A ring header that never becomes initialized.
	mem := AlignedBuffer(pagesize)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	r, err := attachRing(ctx, unsafe.Pointer(unsafe.SliceData(mem)), 10*time.Second)
	if r != nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("attachRing = %v, %v", r, err)
	}
	if time.Since(start) > time.Second {
		t.Fatalf("attachRing took %v despite its context", time.Since(start))
	}

	// Without a context it gives up after the timeout, with no error.
	if r, err := attachRing(context.Background(), unsafe.Pointer(unsafe.SliceData(mem)), 30*time.Millisecond); r != nil || err != nil {
		t.Fatalf("attachRing on an uninitialized ring = %v, %v", r, err)
	}
}

func TestSizeStandardLinkBounds(t *testing.T) {
	if SizeStandardLink(4, 64) == 0 {
		t.Fatal("SizeStandardLink(4, 64) = 0")
	}
	// Not representable as int on every platform: skipped where they overflow.
	huge := []struct{ count, size int64 }{
		{1 << 25, 64},
		{1 << 40, 64},
		{1 << 62, 64},
		{4, 1 << 31},
		{4, 1 << 40},
		{4, math.MaxInt &^ 7},
		{1 << 62, math.MaxInt &^ 7},
		{math.MaxInt/2 + 1, 8},
		{1 << 24, 1 << 31},
	}
	for _, c := range huge {
		count, size := int(c.count), int(c.size)
		if int64(count) != c.count || int64(size) != c.size {
			continue
		}
		if got := SizeStandardLink(count, size); got != 0 {
			t.Errorf("SizeStandardLink(%d, %d) = %d, want 0", count, size, got)
		}
		if _, err := NewStandardLink(AlignedBuffer(pagesize), count, size, LinkOptions{}); !errors.Is(err, ErrInvalidSize) {
			t.Errorf("NewStandardLink(%d, %d) = %v, want ErrInvalidSize", count, size, err)
		}
		if _, err := CreateStandardLink(newPortableSharedMemoryName(), count, size, 0o600, LinkOptions{}); !errors.Is(err, ErrInvalidSize) {
			t.Errorf("CreateStandardLink(%d, %d) = %v, want ErrInvalidSize", count, size, err)
		}
	}

	// Within the bounds, the result depends on whether the layout is
	// addressable: the rings and buffers of the largest ring count need
	// 4 GiB, and the largest configuration more than 2^54 bytes.
	for _, c := range []struct{ count, size int }{{maxBufferCount, 64}, {maxBufferCount, maxBufferSize}} {
		got := SizeStandardLink(c.count, c.size)
		if bits.UintSize == 32 && got != 0 {
			t.Errorf("SizeStandardLink(%d, %d) = %d on a 32-bit platform, want 0", c.count, c.size, got)
		}
		if bits.UintSize == 64 && (got == 0 || got > math.MaxInt) {
			t.Errorf("SizeStandardLink(%d, %d) = %d on a 64-bit platform", c.count, c.size, got)
		}
	}
}

func TestCheckedLayoutArithmetic(t *testing.T) {
	if _, ok := alignedSum(^uintptr(0)-10, 100); ok {
		t.Error("alignedSum overflowed without notice")
	}
	if _, ok := alignedSum(^uintptr(0)-pagesize/2, 1); ok {
		t.Error("alignedSum rounding overflowed without notice")
	}
	if got, ok := alignedSum(1, pagesize); !ok || got != 2*pagesize {
		t.Errorf("alignedSum(1, pagesize) = %d, %v", got, ok)
	}

	l, ok := computeLayout(testBufferCount, testBufferSize)
	if !ok {
		t.Fatal("computeLayout failed")
	}
	if l.ring0 != pagesize || l.buf0 <= l.ring0 || l.ring1 <= l.buf0 || l.buf1 <= l.ring1 || l.total <= l.buf1 ||
		l.total != SizeStandardLink(testBufferCount, testBufferSize) {
		t.Fatalf("layout = %+v", l)
	}
	for _, off := range []uintptr{l.ring0, l.buf0, l.ring1, l.buf1, l.total} {
		if off%pagesize != 0 {
			t.Fatalf("offset %d is not page aligned in %+v", off, l)
		}
	}
}
