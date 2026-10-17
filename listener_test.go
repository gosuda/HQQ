package hqq

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"gosuda.org/hqq/internal/mpmc"
	"gosuda.org/hqq/internal/shm"
)

// listenerTestOptions are the link options of the acceptor tests: fast
// heartbeats, so that dead peers are found within a fraction of a second.
func listenerTestOptions() AdvancedOptions {
	return AdvancedOptions{Link: LinkOptions{
		HeartbeatInterval: 20 * time.Millisecond,
		PeerTimeout:       300 * time.Millisecond,
		AttachTimeout:     3 * time.Second,
	}}
}

func listenerTestConfig() Config {
	return Config{BufferCount: 16, BufferSize: 256, Advanced: listenerTestOptions()}
}

func newTestAcceptor(t *testing.T, name string, cfg AcceptorConfig) *Acceptor {
	t.Helper()
	if cfg.Advanced.Link.HeartbeatInterval == 0 {
		cfg.Advanced = listenerTestOptions()
	}
	a, err := NewAcceptor(name, cfg)
	if err != nil {
		if errors.Is(err, syscall.ENOSYS) {
			t.Skipf("shared memory is not supported on this platform: %v", err)
		}
		t.Fatalf("NewAcceptor: %v", err)
	}
	t.Cleanup(func() { _ = a.Close() })
	return a
}

var echoHandler = HandlerFunc(func(_ context.Context, w *ResponseWriter, req *Request) {
	_, _ = w.Write(req.Payload())
})

// echoServer accepts links from several goroutines and serves echo requests
// on every one of them. served receives the result of every Serve.
type echoServer struct {
	cancel context.CancelFunc
	wg     sync.WaitGroup
	served chan serveResult
}

type serveResult struct {
	name string
	err  error
}

func serveEcho(t *testing.T, a *Acceptor, acceptors int) *echoServer {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	s := &echoServer{cancel: cancel, served: make(chan serveResult, 256)}
	for range acceptors {
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			for {
				l, err := a.Accept(ctx)
				if err != nil {
					return
				}
				s.wg.Add(1)
				go func() {
					defer s.wg.Done()
					err := l.Serve(ctx, echoHandler)
					name := l.Name()
					_ = l.Close()
					select {
					case s.served <- serveResult{name, err}:
					default:
					}
				}()
			}
		}()
	}
	t.Cleanup(s.stop)
	return s
}

func (s *echoServer) stop() {
	s.cancel()
	s.wg.Wait()
}

func echoCall(ctx context.Context, l *NamedAdvancedLink, payload []byte) error {
	resp, err := l.Call(ctx, 1, payload)
	if err != nil {
		return err
	}
	defer resp.Release()
	if !bytes.Equal(resp.Bytes(), payload) {
		return fmt.Errorf("echo mismatch: got %d bytes, want %d", resp.Len(), len(payload))
	}
	return nil
}

func waitUntil(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %v waiting for %s", timeout, what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func testContext(t *testing.T, d time.Duration) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), d)
	t.Cleanup(cancel)
	return ctx
}

// TestListenerLayoutABI pins the shared-memory ABI of the listener segment.
func TestListenerLayoutABI(t *testing.T) {
	if listenerMagic != 0x4851515f4c534e31 {
		t.Fatalf("magic = %#x, want 0x4851515f4c534e31 (HQQ_LSN1)", listenerMagic)
	}
	if listenerMajor != 1 || listenerMinor != 0 || listenerKnownRequiredFeatures != 0 {
		t.Fatalf("layout version %d.%d", listenerMajor, listenerMinor)
	}
	var h listenerHeader
	for off, field := range map[uintptr]*uint64{
		0: &h.magic, 8: &h.state, 16: &h.major, 24: &h.minor, 32: &h.ringOffset, 40: &h.recordSize,
		48: &h.capacity, 56: &h.layoutSize, 64: &h.epoch, 72: &h.initToken, 80: &h.initHeartbeat,
		88: &h.requiredFeatures, 96: &h.creatorToken, 128: &h.serverToken, 136: &h.serverPID,
		144: &h.serverHeartbeat, 152: &h.serverInterval, 160: &h.maxBufferCount, 168: &h.maxBufferSize,
		176: &h.maxLinkSize, 184: &h.features, 192: &h.prefixLen,
	} {
		if got := uintptr(unsafe.Pointer(field)) - uintptr(unsafe.Pointer(&h)); got != off {
			t.Errorf("header field at offset %d is at %d", off, got)
		}
	}
	if got := uintptr(unsafe.Pointer(&h.prefix)) - uintptr(unsafe.Pointer(&h)); got != 200 {
		t.Errorf("prefix at %d, want 200", got)
	}
	var r connectRecord
	for off, field := range map[uintptr]*uint64{
		0: &r.clientToken, 8: &r.bufferCount, 16: &r.bufferSize, 24: &r.features,
		32: &r.flags, 40: &r.nameLen, 48: &r.clientPID, 56: &r.reserved,
	} {
		if got := uintptr(unsafe.Pointer(field)) - uintptr(unsafe.Pointer(&r)); got != off {
			t.Errorf("record field at offset %d is at %d", off, got)
		}
	}
	if got := uintptr(unsafe.Pointer(&r.name)) - uintptr(unsafe.Pointer(&r)); got != 64 {
		t.Errorf("record name at %d, want 64", got)
	}
	if unsafe.Sizeof(r) != 248 || unsafe.Sizeof(h) > 4096 {
		t.Errorf("sizes: record %d, header %d", unsafe.Sizeof(r), unsafe.Sizeof(h))
	}

	if size := listenerLayoutSize(64); size != alignPage(4096+512+64*256) {
		t.Errorf("layout size for 64 records = %d", size)
	}
	for _, bad := range []int{0, 1, 3, 100, maxAcceptorQueueSize * 2} {
		if listenerLayoutSize(bad) != 0 {
			t.Errorf("listenerLayoutSize(%d) != 0", bad)
		}
	}
}

// TestListenerRingABIPin pins the assumptions about internal/mpmc that
// stuck-slot recovery relies on: the offsets of the cursors and the slot
// stride.
func TestListenerRingABIPin(t *testing.T) {
	const slots = 4
	mem := AlignedBuffer(listenerLayoutSize(slots))
	base := unsafe.Pointer(unsafe.SliceData(mem))
	ringBase := unsafe.Add(base, listenerRingOffset)
	mpmc.Format[connectRecord](ringBase, slots)
	ring := mpmc.Attach[connectRecord](ringBase, 0)
	word := func(off uintptr) uint64 { return atomic.LoadUint64((*uint64)(unsafe.Add(ringBase, off))) }
	seq := func(slot uint64) uint64 { return word(ringHeaderSize + uintptr(slot)*connectSlotStride) }
	r, w := func() uint64 { return word(ringReadCursorOffset) }, func() uint64 { return word(ringWriteCursorOffset) }

	for i := uint64(0); i < slots; i++ {
		if seq(i) != i {
			t.Fatalf("fresh slot %d has sequence %d", i, seq(i))
		}
	}
	ring.Enqueue(connectRecord{clientToken: 7})
	ring.Enqueue(connectRecord{clientToken: 8})
	if r() != 0 || w() != 2 || seq(0) != 1 || seq(1) != 2 || seq(2) != 2 {
		t.Fatalf("after two enqueues: r=%d w=%d seq=%d,%d,%d", r(), w(), seq(0), seq(1), seq(2))
	}
	if got := ring.Dequeue(); got.clientToken != 7 {
		t.Fatalf("dequeued %d", got.clientToken)
	}
	if r() != 1 || w() != 2 || seq(0) != 0+slots {
		t.Fatalf("after a dequeue: r=%d w=%d seq0=%d", r(), w(), seq(0))
	}

	// A claimed, unpublished slot looks like this: cursor ahead, sequence
	// at the free value.
	slot := ring.ReserveProducer()
	if w() != 3 || seq(2) != 2 || r() != 1 {
		t.Fatalf("claimed slot: r=%d w=%d seq2=%d", r(), w(), seq(2))
	}
	slot.Commit()
}

func TestListenerLinkNames(t *testing.T) {
	for in, want := range map[string]string{
		"/hq1a-3-xyz":                     "hq1a-3-xyz.",
		"svc":                             "svc.",
		"averyverylongservicename":        "averyverylon.",
		"we.ird:name":                     "we_ird_name.",
		`Global\shared`:                   `Global\shared.`,
		`local\x`:                         `local\x.`,
		"/" + strings.Repeat("z", 200):    strings.Repeat("z", listenerStemLen) + ".",
		"a" + strings.Repeat("-", 3) + "": "a---.",
	} {
		if want == "" {
			continue // not a valid shared-memory name
		}
		got := listenerLinkPrefix(in)
		if got != want {
			t.Errorf("prefix(%q) = %q, want %q", in, got, want)
		}
		if len(got) > listenerMaxPrefixLen {
			t.Errorf("prefix(%q) is %d bytes", in, len(got))
		}
		name := newLinkName(got)
		if !validLinkName(got, name) {
			t.Errorf("newLinkName(%q) = %q is not valid", got, name)
		}
		if !strings.Contains(got, `\`) && len(name) > 30 {
			t.Errorf("link name %q (%d bytes) exceeds the 30 bytes macOS allows", name, len(name))
		}
	}
	prefix := "svc."
	for _, name := range []string{
		"", "svc.", "svc.0123456789abcde", "svc.0123456789abcdef0", "svc.0123456789ABCDEF",
		"svc.0123456789abcdeg", "svx.0123456789abcdef", "/svc.0123456789abcdef", "svc/0123456789abcdef",
	} {
		if validLinkName(prefix, name) {
			t.Errorf("validLinkName(%q) = true", name)
		}
	}
	if !validLinkName(prefix, "svc.0123456789abcdef") {
		t.Error("valid name rejected")
	}
	if a, b := newLinkName(prefix), newLinkName(prefix); a == b {
		t.Errorf("two link names are equal: %q", a)
	}
}

func TestAcceptorManyClients(t *testing.T) {
	const clients = 32
	name := newPortableSharedMemoryName()
	a := newTestAcceptor(t, name, AcceptorConfig{})
	if a.Name() != name || a.Addr().String() != name || a.Addr().Network() != "hqq" {
		t.Fatalf("Name/Addr = %q, %v", a.Name(), a.Addr())
	}
	serveEcho(t, a, 4)

	ctx := testContext(t, 20*time.Second)
	cfg := listenerTestConfig()
	var wg sync.WaitGroup
	var failures atomic.Int32
	names := make([]string, clients)
	for i := range clients {
		wg.Add(1)
		go func() {
			defer wg.Done()
			fail := func(format string, args ...any) {
				failures.Add(1)
				t.Errorf("client %d: "+format, append([]any{i}, args...)...)
			}
			l, err := Connect(ctx, name, cfg)
			if err != nil {
				fail("Connect: %v", err)
				return
			}
			names[i] = l.Name()
			defer l.Close()
			if l.Mode() != LinkModePrimary || l.ProtocolState() != ProtocolStateNegotiated {
				fail("mode %v, protocol state %v", l.Mode(), l.ProtocolState())
			}
			for j := range 10 {
				if err := echoCall(ctx, l, patternedBytes(cfg.BufferSize*(j%3)+j+1, byte(i))); err != nil {
					fail("Call %d: %v", j, err)
					return
				}
			}
		}()
	}
	wg.Wait()
	if failures.Load() != 0 {
		return
	}

	st := a.Stats()
	if st.Records != clients || st.Accepted != clients || st.Malformed+st.Unreachable+st.HandshakeFailed != 0 {
		t.Fatalf("stats = %+v, want %d records and accepted links", st, clients)
	}
	seen := map[string]bool{}
	for _, n := range names {
		if seen[n] {
			t.Fatalf("link name %q used twice", n)
		}
		seen[n] = true
	}
	// Every client closed its link (and with it its segment).
	for _, n := range names {
		eventually(t, "client segment removal", func() bool { return !nameExists(t, n) })
	}
}

func TestAcceptorSendRecv(t *testing.T) {
	name := newPortableSharedMemoryName()
	a := newTestAcceptor(t, name, AcceptorConfig{})
	ctx := testContext(t, 10*time.Second)

	accepted := make(chan *NamedAdvancedLink, 1)
	go func() {
		l, err := a.Accept(ctx)
		if err != nil {
			t.Errorf("Accept: %v", err)
		}
		accepted <- l
	}()
	client, err := Connect(ctx, name, listenerTestConfig())
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer client.Close()
	server := <-accepted
	if server == nil {
		t.FailNow()
	}
	defer server.Close()
	if server.Mode() != LinkModeSecondary || server.NegotiatedFeatures() != client.NegotiatedFeatures() {
		t.Fatalf("server mode %v, features %#x vs %#x", server.Mode(), server.NegotiatedFeatures(), client.NegotiatedFeatures())
	}
	if server.Name() != client.Name() {
		t.Fatalf("names differ: %q and %q", server.Name(), client.Name())
	}
	if err := client.Send(ctx, []byte("hello")); err != nil {
		t.Fatal(err)
	}
	m, err := server.Recv(ctx)
	if err != nil || string(m.Bytes()) != "hello" {
		t.Fatalf("server Recv = %v, %v", m, err)
	}
	m.Release()
	if err := server.Send(ctx, []byte("world")); err != nil {
		t.Fatal(err)
	}
	m, err = client.Recv(ctx)
	if err != nil || string(m.Bytes()) != "world" {
		t.Fatalf("client Recv = %v, %v", m, err)
	}
	m.Release()
}

// TestAcceptorFeatureNegotiation: the accepted link gets the intersection of
// the acceptor's and the client's features.
func TestAcceptorFeatureNegotiation(t *testing.T) {
	name := newPortableSharedMemoryName()
	a := newTestAcceptor(t, name, AcceptorConfig{Features: FeatureMessages})
	ctx := testContext(t, 10*time.Second)
	go func() {
		if l, err := a.Accept(ctx); err == nil {
			t.Cleanup(func() { _ = l.Close() })
		}
	}()
	cfg := listenerTestConfig()
	cfg.Features = FeatureMessages | FeatureRequestResponse
	client, err := Connect(ctx, name, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if got := client.NegotiatedFeatures(); got != FeatureMessages {
		t.Fatalf("negotiated features = %#x, want %#x", got, FeatureMessages)
	}
}

// TestAcceptorConnectLimits: a geometry the acceptor would drop fails at
// once on the client.
func TestAcceptorConnectLimits(t *testing.T) {
	name := newPortableSharedMemoryName()
	a := newTestAcceptor(t, name, AcceptorConfig{MaxBufferCount: 32, MaxBufferSize: 512})
	ctx := testContext(t, 10*time.Second)
	for _, tc := range []struct{ count, size int }{{64, 256}, {16, 1024}, {3, 256}, {16, 100}} {
		cfg := listenerTestConfig()
		cfg.BufferCount, cfg.BufferSize = tc.count, tc.size
		start := time.Now()
		if l, err := Connect(ctx, name, cfg); !errors.Is(err, ErrInvalidSize) {
			if l != nil {
				_ = l.Close()
			}
			t.Errorf("Connect(%d x %d) = %v, want ErrInvalidSize", tc.count, tc.size, err)
		}
		if time.Since(start) > time.Second {
			t.Errorf("Connect(%d x %d) took %v", tc.count, tc.size, time.Since(start))
		}
	}
	if st := a.Stats(); st.Records != 0 {
		t.Fatalf("the acceptor saw %d records of rejected requests", st.Records)
	}
	cfg := listenerTestConfig()
	cfg.BufferCount, cfg.BufferSize = 32, 512
	serveEcho(t, a, 1)
	l, err := Connect(ctx, name, cfg)
	if err != nil {
		t.Fatalf("Connect at the limit: %v", err)
	}
	defer l.Close()
	if err := echoCall(ctx, l, []byte("limit")); err != nil {
		t.Fatal(err)
	}
}

func TestAcceptorConfigErrors(t *testing.T) {
	for _, cfg := range []AcceptorConfig{
		{QueueSize: 3}, {QueueSize: 1}, {QueueSize: maxAcceptorQueueSize * 2},
		{MaxBufferCount: 1}, {MaxBufferSize: 4},
	} {
		if a, err := NewAcceptor(newPortableSharedMemoryName(), cfg); !errors.Is(err, ErrInvalidSize) {
			if a != nil {
				_ = a.Close()
			}
			t.Errorf("NewAcceptor(%+v) = %v, want ErrInvalidSize", cfg, err)
		}
	}
	if a, err := NewAcceptor("", AcceptorConfig{}); !errors.Is(err, ErrInvalidName) {
		if a != nil {
			_ = a.Close()
		}
		t.Errorf("NewAcceptor with an empty name = %v", err)
	}
	if l, err := Connect(context.Background(), newPortableSharedMemoryName(), Config{BufferCount: 3}); !errors.Is(err, ErrInvalidSize) {
		if l != nil {
			_ = l.Close()
		}
		t.Errorf("Connect with a bad geometry = %v", err)
	}
	if l, err := Connect(context.Background(), "a/b", listenerTestConfig()); !errors.Is(err, ErrInvalidName) {
		if l != nil {
			_ = l.Close()
		}
		t.Errorf("Connect with a bad name = %v", err)
	}
}

func TestAcceptorNameCollision(t *testing.T) {
	name := newPortableSharedMemoryName()
	a := newTestAcceptor(t, name, AcceptorConfig{})
	serveEcho(t, a, 1)

	b, err := NewAcceptor(name, AcceptorConfig{Advanced: listenerTestOptions()})
	if !errors.Is(err, ErrListenerBusy) || !errors.Is(err, ErrLinkBusy) || !errors.Is(err, ErrFailedInit) {
		if b != nil {
			_ = b.Close()
		}
		t.Fatalf("second NewAcceptor = %v, want ErrListenerBusy", err)
	}
	// The failed attempt must not have disturbed the live acceptor, nor
	// removed its name.
	ctx := testContext(t, 10*time.Second)
	l, err := Connect(ctx, name, listenerTestConfig())
	if err != nil {
		t.Fatalf("Connect after a failed second acceptor: %v", err)
	}
	defer l.Close()
	if err := echoCall(ctx, l, []byte("still here")); err != nil {
		t.Fatal(err)
	}
}

// TestAcceptorOverLiveLink: a listener name that is a live link segment is a
// foreign object.
func TestAcceptorOverLiveLink(t *testing.T) {
	name := newPortableSharedMemoryName()
	link, err := CreateAdvancedLink(name, 16, 256, 0o600, listenerTestOptions())
	if err != nil {
		t.Fatal(err)
	}
	defer link.Close()
	if a, err := NewAcceptor(name, AcceptorConfig{}); !errors.Is(err, ErrIncompatibleLayout) {
		if a != nil {
			_ = a.Close()
		}
		t.Fatalf("NewAcceptor over a link segment = %v, want ErrIncompatibleLayout", err)
	}
	if l, err := Connect(testContext(t, 5*time.Second), name, listenerTestConfig()); !errors.Is(err, ErrIncompatibleLayout) {
		if l != nil {
			_ = l.Close()
		}
		t.Fatalf("Connect to a link segment = %v, want ErrIncompatibleLayout", err)
	}
	if st := link.PeerState(); st != PeerNotConnected {
		t.Fatalf("the link was touched: peer state %v", st)
	}
}

func TestAcceptorNeverTouchesForeignObject(t *testing.T) {
	size := int(listenerLayoutSize(DefaultAcceptorQueueSize))

	t.Run("data in the header", func(t *testing.T) {
		name := newPortableSharedMemoryName()
		snapshot := createForeignObject(t, name, size, func(page []byte) {
			copy(page[100:], "somebody else's data")
		})
		before := snapshot()
		if a, err := NewAcceptor(name, AcceptorConfig{}); !errors.Is(err, ErrIncompatibleLayout) {
			if a != nil {
				_ = a.Close()
			}
			t.Fatalf("NewAcceptor over foreign data = %v, want ErrIncompatibleLayout", err)
		}
		start := time.Now()
		if l, err := Connect(testContext(t, 10*time.Second), name, listenerTestConfig()); !errors.Is(err, ErrIncompatibleLayout) {
			if l != nil {
				_ = l.Close()
			}
			t.Fatalf("Connect over foreign data = %v, want ErrIncompatibleLayout", err)
		}
		if time.Since(start) > time.Second {
			t.Fatalf("Connect retried a permanent error for %v", time.Since(start))
		}
		if !bytes.Equal(snapshot(), before) {
			t.Fatal("foreign object was modified")
		}
		if !nameExists(t, name) {
			t.Fatal("foreign name was removed")
		}
	})

	t.Run("foreign magic in a Ready header", func(t *testing.T) {
		name := newPortableSharedMemoryName()
		snapshot := createForeignObject(t, name, size, func(page []byte) {
			h := (*listenerHeader)(unsafe.Pointer(&page[0]))
			store(&h.state, listenerStateReady)
			store(&h.magic, 0xbad)
		})
		before := snapshot()
		if a, err := NewAcceptor(name, AcceptorConfig{}); !errors.Is(err, ErrIncompatibleLayout) {
			if a != nil {
				_ = a.Close()
			}
			t.Fatalf("NewAcceptor = %v, want ErrIncompatibleLayout", err)
		}
		if !bytes.Equal(snapshot(), before) {
			t.Fatal("foreign object was modified")
		}
	})

	t.Run("newer major version", func(t *testing.T) {
		name := newPortableSharedMemoryName()
		snapshot := createForeignObject(t, name, size, func(page []byte) {
			h := (*listenerHeader)(unsafe.Pointer(&page[0]))
			store(&h.state, listenerStateReady)
			store(&h.magic, listenerMagic)
			store(&h.major, listenerMajor+1)
		})
		before := snapshot()
		if a, err := NewAcceptor(name, AcceptorConfig{}); !errors.Is(err, ErrIncompatibleLayout) {
			if a != nil {
				_ = a.Close()
			}
			t.Fatalf("NewAcceptor = %v, want ErrIncompatibleLayout", err)
		}
		if l, err := Connect(testContext(t, 5*time.Second), name, listenerTestConfig()); !errors.Is(err, ErrIncompatibleLayout) {
			if l != nil {
				_ = l.Close()
			}
			t.Fatalf("Connect = %v, want ErrIncompatibleLayout", err)
		}
		if !bytes.Equal(snapshot(), before) {
			t.Fatal("object was modified")
		}
	})

	t.Run("unknown required feature", func(t *testing.T) {
		name := newPortableSharedMemoryName()
		snapshot := createForeignObject(t, name, size, func(page []byte) {
			h := (*listenerHeader)(unsafe.Pointer(&page[0]))
			store(&h.state, listenerStateReady)
			store(&h.magic, listenerMagic)
			store(&h.major, listenerMajor)
			store(&h.requiredFeatures, 1<<7)
		})
		before := snapshot()
		if a, err := NewAcceptor(name, AcceptorConfig{}); !errors.Is(err, ErrIncompatibleLayout) {
			if a != nil {
				_ = a.Close()
			}
			t.Fatalf("NewAcceptor = %v, want ErrIncompatibleLayout", err)
		}
		if !bytes.Equal(snapshot(), before) {
			t.Fatal("object was modified")
		}
	})

	t.Run("too small", func(t *testing.T) {
		name := newPortableSharedMemoryName()
		snapshot := createForeignObject(t, name, int(pagesize), func(page []byte) {
			copy(page, "tiny")
		})
		before := snapshot()
		if a, err := NewAcceptor(name, AcceptorConfig{}); !errors.Is(err, ErrIncompatibleLayout) {
			if a != nil {
				_ = a.Close()
			}
			t.Fatalf("NewAcceptor over a small foreign object = %v, want ErrIncompatibleLayout", err)
		}
		if !bytes.Equal(snapshot(), before) || !nameExists(t, name) {
			t.Fatal("small foreign object was modified or removed")
		}
	})
}

// TestAcceptorSmallObjectSizes: whether an existing object shorter than the
// listener segment is a listener does not depend on its size. Whether a
// link segment (or any foreign data) is shorter or longer than the listener
// depends on the page size of the host, so the sizes are constructed here
// rather than derived from it. A foreign object is ErrIncompatibleLayout at
// every size and is left alone; a shorter listener of this layout version
// is ErrInvalidSize.
func TestAcceptorSmallObjectSizes(t *testing.T) {
	full := int(listenerLayoutSize(DefaultAcceptorQueueSize))
	for _, tc := range []struct {
		name string
		size int
	}{
		{"below the header", listenerHeaderSize / 2},
		{"header only", listenerHeaderSize},
		{"two headers", 2 * listenerHeaderSize},
		{"one short of the layout", full - 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			name := newPortableSharedMemoryName()
			snapshot := createForeignObject(t, name, tc.size, func(page []byte) {
				copy(page[100:], "a link or other data, not a listener")
			})
			before := snapshot()
			if a, err := NewAcceptor(name, AcceptorConfig{}); !errors.Is(err, ErrIncompatibleLayout) {
				if a != nil {
					_ = a.Close()
				}
				t.Fatalf("NewAcceptor over a foreign %d-byte object = %v, want ErrIncompatibleLayout", tc.size, err)
			}
			if l, err := Connect(testContext(t, time.Second), name, listenerTestConfig()); !errors.Is(err, ErrIncompatibleLayout) {
				if l != nil {
					_ = l.Close()
				}
				t.Fatalf("Connect over a foreign %d-byte object = %v, want ErrIncompatibleLayout", tc.size, err)
			}
			if !bytes.Equal(snapshot(), before) || !nameExists(t, name) {
				t.Fatal("foreign object was modified or removed")
			}
		})
	}

	t.Run("listener of this layout", func(t *testing.T) {
		name := newPortableSharedMemoryName()
		short, err := NewAcceptor(name, AcceptorConfig{QueueSize: 16, Advanced: listenerTestOptions()})
		if err != nil {
			t.Fatal(err)
		}
		defer short.Close()
		if a, err := NewAcceptor(name, AcceptorConfig{Advanced: listenerTestOptions()}); !errors.Is(err, ErrInvalidSize) {
			if a != nil {
				_ = a.Close()
			}
			t.Fatalf("NewAcceptor over a shorter listener = %v, want ErrInvalidSize", err)
		}
		if !nameExists(t, name) {
			t.Fatal("the shorter listener's name was removed")
		}
	})
}

// TestAcceptorTakesOverStalledInitializer: an initializer that stopped
// making progress is replaced after its window, and a live one is waited for
// within the context.
func TestAcceptorTakesOverStalledInitializer(t *testing.T) {
	size := listenerLayoutSize(DefaultAcceptorQueueSize)
	craft := func(t *testing.T, name string) {
		t.Helper()
		m, err := createNamed(name, size, 0o600, listenerSmallObject)
		if err != nil {
			t.Fatal(err)
		}
		h := (*listenerHeader)(unsafe.Pointer(unsafe.SliceData(m.mapping)))
		store(&h.state, listenerStateInitializing)
		store(&h.major, listenerMajor)
		store(&h.initToken, 0xabc)
		store(&h.initHeartbeat, nowNanos())
		m.unlinkOnClose = false
		t.Cleanup(func() {
			_ = m.release()
			_ = Unlink(name)
		})
	}

	t.Run("takeover", func(t *testing.T) {
		name := newPortableSharedMemoryName()
		craft(t, name)
		opts := listenerTestOptions()
		opts.Link.initStale = 150 * time.Millisecond
		start := time.Now()
		a, err := NewAcceptor(name, AcceptorConfig{Advanced: opts})
		if err != nil {
			t.Fatalf("NewAcceptor over a stalled initializer: %v", err)
		}
		defer a.Close()
		if d := time.Since(start); d < 100*time.Millisecond {
			t.Fatalf("took over after %v, before the stale window", d)
		}
		serveEcho(t, a, 1)
		ctx := testContext(t, 10*time.Second)
		l, err := Connect(ctx, name, listenerTestConfig())
		if err != nil {
			t.Fatal(err)
		}
		defer l.Close()
		if err := echoCall(ctx, l, []byte("ok")); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("bounded by the context", func(t *testing.T) {
		name := newPortableSharedMemoryName()
		craft(t, name)
		ctx := testContext(t, 150*time.Millisecond)
		start := time.Now()
		a, err := NewAcceptorContext(ctx, name, AcceptorConfig{Advanced: listenerTestOptions()})
		if !errors.Is(err, context.DeadlineExceeded) {
			if a != nil {
				_ = a.Close()
			}
			t.Fatalf("NewAcceptorContext = %v, want DeadlineExceeded", err)
		}
		if time.Since(start) > time.Second {
			t.Fatalf("returned after %v", time.Since(start))
		}
	})

	t.Run("Connect waits for a listener that is initializing", func(t *testing.T) {
		name := newPortableSharedMemoryName()
		craft(t, name)
		ctx := testContext(t, 150*time.Millisecond)
		l, err := Connect(ctx, name, listenerTestConfig())
		if !errors.Is(err, context.DeadlineExceeded) {
			if l != nil {
				_ = l.Close()
			}
			t.Fatalf("Connect = %v, want DeadlineExceeded", err)
		}
	})
}

func TestConnectWaitsForListener(t *testing.T) {
	name := newPortableSharedMemoryName()
	ctx := testContext(t, 20*time.Second)

	type result struct {
		l   *NamedAdvancedLink
		err error
	}
	done := make(chan result, 1)
	go func() {
		l, err := Connect(ctx, name, listenerTestConfig())
		done <- result{l, err}
	}()
	time.Sleep(100 * time.Millisecond)
	select {
	case r := <-done:
		t.Fatalf("Connect returned before the listener existed: %v", r.err)
	default:
	}
	a := newTestAcceptor(t, name, AcceptorConfig{})
	serveEcho(t, a, 1)
	r := <-done
	if r.err != nil {
		t.Fatalf("Connect: %v", r.err)
	}
	defer r.l.Close()
	if err := echoCall(ctx, r.l, []byte("late listener")); err != nil {
		t.Fatal(err)
	}
}

func TestConnectContextCancel(t *testing.T) {
	name := newPortableSharedMemoryName()
	ctx := testContext(t, 100*time.Millisecond)
	start := time.Now()
	l, err := Connect(ctx, name, listenerTestConfig())
	if !errors.Is(err, context.DeadlineExceeded) {
		if l != nil {
			_ = l.Close()
		}
		t.Fatalf("Connect without a listener = %v, want DeadlineExceeded", err)
	}
	if time.Since(start) > time.Second {
		t.Fatalf("Connect returned after %v", time.Since(start))
	}
}

func TestAcceptorCloseWakesAccept(t *testing.T) {
	name := newPortableSharedMemoryName()
	a := newTestAcceptor(t, name, AcceptorConfig{})

	const waiters = 3
	errs := make(chan error, waiters)
	for range waiters {
		go func() {
			_, err := a.Accept(context.Background())
			errs <- err
		}()
	}
	time.Sleep(50 * time.Millisecond)
	if err := a.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	for range waiters {
		select {
		case err := <-errs:
			if !errors.Is(err, ErrAcceptorClosed) || !errors.Is(err, ErrClosed) {
				t.Fatalf("Accept after Close = %v, want ErrAcceptorClosed", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("Accept was not woken by Close")
		}
	}
	if err := a.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if _, err := a.Accept(context.Background()); !errors.Is(err, ErrAcceptorClosed) {
		t.Fatalf("Accept on a closed acceptor = %v", err)
	}
	if nameExists(t, name) {
		t.Fatal("Close left the listener name behind")
	}

	// Accept honors its context.
	b := newTestAcceptor(t, newPortableSharedMemoryName(), AcceptorConfig{})
	ctx := testContext(t, 50*time.Millisecond)
	if _, err := b.Accept(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Accept = %v, want DeadlineExceeded", err)
	}
}

// TestAcceptorCloseKeepsAcceptedLinks: Close does not close links that were
// returned by Accept, but closes the ones that were negotiated and not
// accepted yet.
func TestAcceptorCloseKeepsAcceptedLinks(t *testing.T) {
	name := newPortableSharedMemoryName()
	a := newTestAcceptor(t, name, AcceptorConfig{})
	ctx := testContext(t, 20*time.Second)

	accepted := make(chan *NamedAdvancedLink, 1)
	go func() {
		l, _ := a.Accept(ctx)
		accepted <- l
	}()
	kept, err := Connect(ctx, name, listenerTestConfig())
	if err != nil {
		t.Fatal(err)
	}
	defer kept.Close()
	server := <-accepted
	if server == nil {
		t.FailNow()
	}
	defer server.Close()
	go server.Serve(ctx, echoHandler)

	// A second client completes its handshake but is never accepted.
	orphan, err := Connect(ctx, name, listenerTestConfig())
	if err != nil {
		t.Fatal(err)
	}
	defer orphan.Close()

	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	if err := echoCall(ctx, kept, []byte("after close")); err != nil {
		t.Fatalf("accepted link after Close: %v", err)
	}
	waitUntil(t, 5*time.Second, "the unaccepted link to be closed", func() bool {
		return orphan.PeerState() == PeerClosed
	})
	if _, err := orphan.Call(ctx, 1, []byte("x")); !errors.Is(err, ErrPeerClosed) {
		t.Fatalf("Call on a link closed by Close = %v, want ErrPeerClosed", err)
	}
}

// TestAcceptorRestart: the name is reused by a new acceptor and clients that
// retry reach it.
func TestAcceptorRestart(t *testing.T) {
	name := newPortableSharedMemoryName()
	a := newTestAcceptor(t, name, AcceptorConfig{})
	serveEcho(t, a, 1)
	ctx := testContext(t, 30*time.Second)

	l, err := Connect(ctx, name, listenerTestConfig())
	if err != nil {
		t.Fatal(err)
	}
	if err := echoCall(ctx, l, []byte("first")); err != nil {
		t.Fatal(err)
	}
	_ = l.Close()
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}

	// A client that starts while the server is down waits for it.
	done := make(chan error, 1)
	var late *NamedAdvancedLink
	go func() {
		var err error
		late, err = Connect(ctx, name, listenerTestConfig())
		done <- err
	}()
	time.Sleep(100 * time.Millisecond)

	b := newTestAcceptor(t, name, AcceptorConfig{})
	serveEcho(t, b, 1)
	if err := <-done; err != nil {
		t.Fatalf("Connect across a restart: %v", err)
	}
	defer late.Close()
	if err := echoCall(ctx, late, []byte("second")); err != nil {
		t.Fatal(err)
	}
}

// TestAcceptorBackpressure: with no capacity left the record queue fills up
// and clients wait; when the server accepts again they all get through.
func TestAcceptorBackpressure(t *testing.T) {
	const clients = 10
	name := newPortableSharedMemoryName()
	a := newTestAcceptor(t, name, AcceptorConfig{QueueSize: 2, Backlog: 1, MaxHandshakes: 1})
	ctx := testContext(t, 30*time.Second)

	var completed atomic.Int32
	links := make(chan *NamedAdvancedLink, clients)
	errs := make(chan error, clients)
	for range clients {
		go func() {
			l, err := Connect(ctx, name, listenerTestConfig())
			if err != nil {
				errs <- err
				return
			}
			completed.Add(1)
			links <- l
		}()
	}
	// Nobody accepts: at most Backlog + MaxHandshakes handshakes finish, the
	// record queue (2 slots) and the dispatcher hold two more, the rest of
	// the clients block in the queue.
	waitUntil(t, 10*time.Second, "the first client to connect", func() bool { return completed.Load() >= 1 })
	time.Sleep(300 * time.Millisecond)
	if n := completed.Load(); n > 2 {
		t.Fatalf("%d clients completed while nobody accepts, want at most 2", n)
	}
	select {
	case err := <-errs:
		t.Fatalf("a waiting client failed: %v", err)
	default:
	}

	// A client with a short deadline gives up cleanly.
	short := testContext(t, 200*time.Millisecond)
	start := time.Now()
	if l, err := Connect(short, name, listenerTestConfig()); !errors.Is(err, context.DeadlineExceeded) {
		if l != nil {
			_ = l.Close()
		}
		t.Fatalf("Connect on a full queue = %v, want DeadlineExceeded", err)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatalf("Connect on a full queue returned after %v", time.Since(start))
	}

	serveEcho(t, a, 2)
	waitUntil(t, 20*time.Second, "all clients to connect", func() bool { return completed.Load() == clients })
	for range clients {
		l := <-links
		if err := echoCall(ctx, l, []byte("through")); err != nil {
			t.Errorf("Call: %v", err)
		}
		_ = l.Close()
	}
	select {
	case err := <-errs:
		t.Fatalf("a client failed: %v", err)
	default:
	}
}

// TestConnectTimesOutWhenNotServed: an acceptor that does not get to a
// request makes Connect fail after the attach timeout, and the client's
// segment is gone.
func TestConnectTimesOutWhenNotServed(t *testing.T) {
	name := newPortableSharedMemoryName()
	a := newTestAcceptor(t, name, AcceptorConfig{QueueSize: 8, Backlog: 1, MaxHandshakes: 1})
	ctx := testContext(t, 20*time.Second)

	// Occupy the acceptor: two clients complete their handshake and are not
	// accepted, the third one is read by the dispatcher and waits for a
	// handshake slot.
	var held []*NamedAdvancedLink
	for range 2 {
		l, err := Connect(ctx, name, listenerTestConfig())
		if err != nil {
			t.Fatal(err)
		}
		held = append(held, l)
	}
	defer func() {
		for _, l := range held {
			_ = l.Close()
		}
	}()
	go func() {
		l, err := Connect(ctx, name, listenerTestConfig())
		if err == nil {
			t.Cleanup(func() { _ = l.Close() })
		}
	}()
	waitUntil(t, 5*time.Second, "the dispatcher to read the third record", func() bool { return a.Stats().Records == 3 })

	cfg := listenerTestConfig()
	cfg.Advanced.Link.AttachTimeout = 300 * time.Millisecond
	before := countShmWithPrefix(t, listenerLinkPrefix(name))
	start := time.Now()
	l, err := Connect(ctx, name, cfg)
	if !errors.Is(err, ErrTimeout) || !errors.Is(err, os.ErrDeadlineExceeded) {
		if l != nil {
			_ = l.Close()
		}
		t.Fatalf("Connect to a busy acceptor = %v, want a timeout", err)
	}
	if d := time.Since(start); d < 250*time.Millisecond || d > 3*time.Second {
		t.Fatalf("timed out after %v, want about 300ms", d)
	}
	if before >= 0 {
		if after := countShmWithPrefix(t, listenerLinkPrefix(name)); after != before {
			t.Fatalf("%d client segments before, %d after the failed Connect", before, after)
		}
	}
}

// countShmWithPrefix counts the shared memory names that start with prefix
// where that can be listed (Linux), and returns -1 elsewhere.
func countShmWithPrefix(t *testing.T, prefix string) int {
	t.Helper()
	if runtime.GOOS != "linux" {
		return -1
	}
	entries, err := os.ReadDir("/dev/shm")
	if err != nil {
		return -1
	}
	n := 0
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), prefix) {
			n++
		}
	}
	return n
}

// rawClient is a hand-made client for tests that put crafted records into
// the ring.
func rawClient(t *testing.T, listener string) *listenerClient {
	t.Helper()
	lc, err := openListener(testContext(t, 10*time.Second), listener, listenerTestOptions().Link)
	if err != nil {
		t.Fatalf("openListener: %v", err)
	}
	t.Cleanup(lc.close)
	return lc
}

func (lc *listenerClient) enqueue(t *testing.T, rec connectRecord) {
	t.Helper()
	c := mpmc.Cancel{Ctx: testContext(t, 10*time.Second)}
	if err := lc.ring.EnqueueZeroCopyCancel(&c, func(_ uint64, e *connectRecord) { *e = rec }); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
}

func validTestRecord(lc *listenerClient) connectRecord {
	rec := connectRecord{clientToken: newToken(), bufferCount: 16, bufferSize: 256}
	rec.setName(newLinkName(lc.prefix))
	return rec
}

func TestAcceptorDropsMalformedRecords(t *testing.T) {
	name := newPortableSharedMemoryName()
	a := newTestAcceptor(t, name, AcceptorConfig{QueueSize: 128, MaxBufferCount: 1024, MaxBufferSize: 4096})
	lc := rawClient(t, name)

	mutate := func(f func(r *connectRecord)) connectRecord {
		r := validTestRecord(lc)
		f(&r)
		return r
	}
	rawName := func(r *connectRecord, n string, length uint64) {
		r.name = [listenerMaxNameLen]byte{}
		copy(r.name[:], n)
		r.nameLen = length
	}
	good := newLinkName(lc.prefix)
	bad := []connectRecord{
		{}, // all zero
		mutate(func(r *connectRecord) { r.flags = 1 }),
		mutate(func(r *connectRecord) { r.flags = ^uint64(0) }),
		mutate(func(r *connectRecord) { r.nameLen = 0 }),
		mutate(func(r *connectRecord) { r.nameLen = listenerMaxNameLen + 1 }),
		mutate(func(r *connectRecord) { r.nameLen = 1 << 63 }),
		mutate(func(r *connectRecord) { r.nameLen = ^uint64(0) }),
		mutate(func(r *connectRecord) { rawName(r, good, uint64(len(good))-1) }),
		mutate(func(r *connectRecord) { rawName(r, good+"0", uint64(len(good))+1) }),
		mutate(func(r *connectRecord) { rawName(r, strings.ToUpper(good), uint64(len(good))) }),
		mutate(func(r *connectRecord) { rawName(r, "/"+good[1:], uint64(len(good))) }),
		mutate(func(r *connectRecord) {
			rawName(r, "other."+good[len(lc.prefix):], uint64(len("other."))+listenerSuffixLen)
		}),
		mutate(func(r *connectRecord) { rawName(r, "../../etc/passwd", 16) }),
		mutate(func(r *connectRecord) { rawName(r, lc.prefix+"zzzzzzzzzzzzzzzz", uint64(len(lc.prefix))+16) }),
		mutate(func(r *connectRecord) { rawName(r, lc.prefix+"0000000000000000\x00", uint64(len(lc.prefix))+17) }),
		mutate(func(r *connectRecord) { r.clientToken = 0 }),
		mutate(func(r *connectRecord) { r.clientToken = detachedToken }),
		mutate(func(r *connectRecord) { r.bufferCount = 0 }),
		mutate(func(r *connectRecord) { r.bufferCount = 3 }),
		mutate(func(r *connectRecord) { r.bufferCount = 2048 }),
		mutate(func(r *connectRecord) { r.bufferCount = 1 << 63 }),
		mutate(func(r *connectRecord) { r.bufferCount = ^uint64(0) }),
		mutate(func(r *connectRecord) { r.bufferSize = 0 }),
		mutate(func(r *connectRecord) { r.bufferSize = 7 }),
		mutate(func(r *connectRecord) { r.bufferSize = 8192 }),
		mutate(func(r *connectRecord) { r.bufferSize = 1 << 40 }),
		mutate(func(r *connectRecord) { r.bufferSize = ^uint64(0) }),
	}
	rng := rand.New(rand.NewPCG(1, 2))
	for range 60 {
		var r connectRecord
		raw := unsafe.Slice((*byte)(unsafe.Pointer(&r)), unsafe.Sizeof(r))
		for i := range raw {
			raw[i] = byte(rng.Uint32())
		}
		bad = append(bad, r)
	}
	for _, r := range bad {
		lc.enqueue(t, r)
	}
	waitUntil(t, 10*time.Second, "all malformed records to be dropped", func() bool {
		return a.Stats().Records == uint64(len(bad))
	})
	if st := a.Stats(); st.Malformed != uint64(len(bad)) || st.Unreachable+st.HandshakeFailed+st.Accepted != 0 {
		t.Fatalf("stats = %+v, want %d malformed records", st, len(bad))
	}

	// The acceptor still serves clients.
	serveEcho(t, a, 1)
	ctx := testContext(t, 10*time.Second)
	l, err := Connect(ctx, name, listenerTestConfig())
	if err != nil {
		t.Fatalf("Connect after malformed records: %v", err)
	}
	defer l.Close()
	if err := echoCall(ctx, l, []byte("fine")); err != nil {
		t.Fatal(err)
	}
}

// TestAcceptorUnreachableRecords: well-formed records that do not lead to a
// usable link are dropped, and never attach to a segment that is not the
// announced one.
func TestAcceptorUnreachableRecords(t *testing.T) {
	name := newPortableSharedMemoryName()
	a := newTestAcceptor(t, name, AcceptorConfig{QueueSize: 32})
	lc := rawClient(t, name)
	want := uint64(0)
	drop := func(r connectRecord) {
		lc.enqueue(t, r)
		want++
	}

	// 1. The segment does not exist.
	drop(validTestRecord(lc))

	// 2. A live link segment under the right name, but another instance:
	// it must not be joined.
	other := validTestRecord(lc)
	otherName, _ := other.recordName()
	victim, err := CreateAdvancedLink(otherName, 16, 256, 0o600, listenerTestOptions())
	if err != nil {
		t.Fatal(err)
	}
	defer victim.Close()
	drop(other)

	// 3. The right instance, but the record lies about the geometry.
	liar := validTestRecord(lc)
	liarName, _ := liar.recordName()
	target, err := CreateAdvancedLink(liarName, 16, 256, 0o600, listenerTestOptions())
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	liar.clientToken = target.instance
	liar.bufferSize = 512
	drop(liar)

	waitUntil(t, 10*time.Second, "records to be dropped", func() bool {
		st := a.Stats()
		return st.Records == want && st.Unreachable == want
	})
	if st := a.Stats(); st.Malformed != 0 || st.Accepted != 0 {
		t.Fatalf("stats = %+v", st)
	}
	if victim.PeerState() != PeerNotConnected || target.PeerState() != PeerNotConnected {
		t.Fatalf("a segment was joined: victim %v, target %v", victim.PeerState(), target.PeerState())
	}
}

// TestAcceptorStuckSlot: a producer that claimed a queue slot and never
// published it does not block the records behind it.
func TestAcceptorStuckSlot(t *testing.T) {
	name := newPortableSharedMemoryName()
	a := newTestAcceptor(t, name, AcceptorConfig{QueueSize: 8, StuckSlotTimeout: 150 * time.Millisecond})
	serveEcho(t, a, 1)
	lc := rawClient(t, name)

	stuck := lc.ring.ReserveProducer() // a client that dies right here
	if !stuck.Valid() {
		t.Fatal("could not claim a slot")
	}
	ctx := testContext(t, 20*time.Second)
	start := time.Now()
	l, err := Connect(ctx, name, listenerTestConfig())
	if err != nil {
		t.Fatalf("Connect behind a stuck slot: %v", err)
	}
	defer l.Close()
	if d := time.Since(start); d < 100*time.Millisecond {
		t.Fatalf("connected after %v: the slot was not stuck", d)
	}
	if err := echoCall(ctx, l, []byte("behind a stuck slot")); err != nil {
		t.Fatal(err)
	}
	st := a.Stats()
	if st.StuckSlots != 1 || st.Malformed != 1 || st.Records != 2 || st.Accepted != 1 {
		t.Fatalf("stats = %+v, want 1 stuck slot published as 1 malformed record", st)
	}
	// The late commit of the dead producer is refused and harmless.
	if stuck.Commit() {
		t.Fatal("late Commit of a recovered slot succeeded")
	}
}

// TestAcceptorStalledClientDoesNotBlockOthers: a client that attaches
// nothing and answers nothing occupies one handshake, not the acceptor.
func TestAcceptorStalledClientDoesNotBlockOthers(t *testing.T) {
	name := newPortableSharedMemoryName()
	a := newTestAcceptor(t, name, AcceptorConfig{HandshakeTimeout: 800 * time.Millisecond})
	serveEcho(t, a, 1)
	lc := rawClient(t, name)

	// The stalled client: a live link segment (no heartbeat, so it can never
	// be told from a slow one) that never sends its negotiation request.
	rec := validTestRecord(lc)
	segName, _ := rec.recordName()
	opts := listenerTestOptions()
	opts.Link.HeartbeatInterval = -1
	stalled, err := CreateAdvancedLink(segName, 16, 256, 0o600, opts)
	if err != nil {
		t.Fatal(err)
	}
	defer stalled.Close()
	rec.clientToken = stalled.instance
	lc.enqueue(t, rec)
	waitUntil(t, 5*time.Second, "the handshake to start", func() bool { return stalled.PeerState() == PeerConnected })

	ctx := testContext(t, 20*time.Second)
	start := time.Now()
	l, err := Connect(ctx, name, listenerTestConfig())
	if err != nil {
		t.Fatalf("Connect next to a stalled client: %v", err)
	}
	defer l.Close()
	if err := echoCall(ctx, l, []byte("not blocked")); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d > 600*time.Millisecond {
		t.Fatalf("a healthy client needed %v next to a stalled one", d)
	}

	waitUntil(t, 5*time.Second, "the stalled handshake to time out", func() bool { return a.Stats().HandshakeFailed == 1 })
	if st := a.Stats(); st.Accepted != 1 || st.Records != 2 {
		t.Fatalf("stats = %+v", st)
	}
}

// ---------------------------------------------------------------------
// Multi-process tests.

const listenerChildEnv = "HQQ_LISTENER_CHILD"

// TestHQQListenerChild is the body of the child processes of the acceptor
// tests; it does nothing in a normal run.
func TestHQQListenerChild(t *testing.T) {
	if os.Getenv(listenerChildEnv) != "1" {
		t.Skip("helper for multi-process tests")
	}
	args := multiProcessChildArgs(t)
	role, name := args[0], args[1]
	// A child never outlives a test run that forgot to kill it.
	time.AfterFunc(60*time.Second, func() { os.Exit(2) })

	switch role {
	case "server":
		// A server that accepts nothing: Backlog 1 and one handshake at a
		// time, so that later clients wait in the queue.
		a, err := NewAcceptor(name, AcceptorConfig{QueueSize: mustAtoi(t, args[2]), Backlog: 1, MaxHandshakes: 1, Advanced: listenerTestOptions()})
		if err != nil {
			t.Fatalf("child NewAcceptor: %v", err)
		}
		_ = a
		fmt.Println("READY")
		select {}

	case "client-crash":
		// Connect, use the link, then die without closing anything.
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		l, err := Connect(ctx, name, listenerTestConfig())
		if err != nil {
			t.Fatalf("child Connect: %v", err)
		}
		if err := echoCall(ctx, l, []byte("child call")); err != nil {
			t.Fatalf("child Call: %v", err)
		}
		fmt.Println("CONNECTED " + l.Name())
		time.Sleep(100 * time.Millisecond)
		os.Exit(multiProcessCrashExitCode)

	case "client-enqueue":
		// Announce a link segment, print its name and wait to be killed.
		lc, err := openListener(context.Background(), name, listenerTestOptions().Link)
		if err != nil {
			t.Fatalf("child openListener: %v", err)
		}
		segName := newLinkName(lc.prefix)
		link, err := openNamedAdvanced(context.Background(), segName, 16, 256, true, 0o600, listenerTestOptions())
		if err != nil {
			t.Fatalf("child create: %v", err)
		}
		rec := connectRecord{clientToken: link.instance, bufferCount: 16, bufferSize: 256}
		rec.setName(segName)
		c := mpmc.Cancel{Ctx: context.Background()}
		if err := lc.ring.EnqueueZeroCopyCancel(&c, func(_ uint64, e *connectRecord) { *e = rec }); err != nil {
			t.Fatalf("child enqueue: %v", err)
		}
		fmt.Println("ENQUEUED " + segName)
		select {}
	}
}

type listenerChild struct {
	cmd    *exec.Cmd
	stdout *bufio.Reader
	stderr *bytes.Buffer
}

func startListenerChild(t *testing.T, role, name string, extra ...string) *listenerChild {
	t.Helper()
	args := append([]string{"-test.run=^TestHQQListenerChild$", "-test.v", "--", role, name}, extra...)
	cmd := exec.Command(os.Args[0], args...)
	cmd.Env = append(os.Environ(), listenerChildEnv+"=1")
	c := &listenerChild{cmd: cmd, stderr: new(bytes.Buffer)}
	cmd.Stderr = c.stderr
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting child: %v", err)
	}
	c.stdout = bufio.NewReader(out)
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	return c
}

// line returns the next line of the child's output that starts with prefix,
// without the prefix.
func (c *listenerChild) line(t *testing.T, prefix string) string {
	t.Helper()
	for {
		l, err := c.stdout.ReadString('\n')
		if err != nil {
			t.Fatalf("waiting for %q from the child: %v\nchild stderr:\n%s", prefix, err, c.stderr.String())
		}
		if rest, ok := strings.CutPrefix(strings.TrimSpace(l), prefix); ok {
			return strings.TrimSpace(rest)
		}
	}
}

func (c *listenerChild) kill(t *testing.T) {
	t.Helper()
	_ = c.cmd.Process.Kill()
	_ = c.cmd.Wait()
}

// TestAcceptorClientCrash: a client that dies does not disturb the others,
// and the acceptor's link to it reports ErrPeerGone.
func TestAcceptorClientCrash(t *testing.T) {
	name := newPortableSharedMemoryName()
	a := newTestAcceptor(t, name, AcceptorConfig{})
	srv := serveEcho(t, a, 2)
	ctx := testContext(t, 30*time.Second)

	healthy, err := Connect(ctx, name, listenerTestConfig())
	if err != nil {
		t.Fatal(err)
	}
	defer healthy.Close()
	if err := echoCall(ctx, healthy, []byte("before")); err != nil {
		t.Fatal(err)
	}

	child := startListenerChild(t, "client-crash", name)
	crashedName := child.line(t, "CONNECTED ")
	t.Cleanup(func() { _ = Unlink(crashedName) })
	err = child.cmd.Wait()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != multiProcessCrashExitCode {
		t.Fatalf("child exit = %v, want status %d\nchild stderr:\n%s", err, multiProcessCrashExitCode, child.stderr.String())
	}

	// The server's link to the dead client fails with ErrPeerGone...
	start := time.Now()
	deadline := time.After(10 * time.Second)
	for {
		select {
		case r := <-srv.served:
			if r.name != crashedName {
				t.Fatalf("link %q ended: %v", r.name, r.err)
			}
			if !errors.Is(r.err, ErrPeerGone) {
				t.Fatalf("Serve on the crashed client's link = %v, want ErrPeerGone", r.err)
			}
			t.Logf("crash detected after %v", time.Since(start))
			goto detected
		case <-deadline:
			t.Fatal("the crashed client was not detected")
		}
	}
detected:
	// ...and the other client carries on.
	for i := range 5 {
		if err := echoCall(ctx, healthy, patternedBytes(1000, byte(i))); err != nil {
			t.Fatalf("healthy client after the crash: %v", err)
		}
	}
	// New clients connect.
	l, err := Connect(ctx, name, listenerTestConfig())
	if err != nil {
		t.Fatalf("Connect after a client crash: %v", err)
	}
	defer l.Close()
	if err := echoCall(ctx, l, []byte("new")); err != nil {
		t.Fatal(err)
	}
}

// TestAcceptorDeadClientsInQueue: clients that die after announcing their
// segment, before the acceptor gets to them, are dropped and their segments
// are removed.
func TestAcceptorDeadClientsInQueue(t *testing.T) {
	name := newPortableSharedMemoryName()
	a := newTestAcceptor(t, name, AcceptorConfig{QueueSize: 8, Backlog: 1, MaxHandshakes: 1})
	ctx := testContext(t, 30*time.Second)

	// Occupy the acceptor so that the next records wait.
	var held []*NamedAdvancedLink
	for range 2 {
		l, err := Connect(ctx, name, listenerTestConfig())
		if err != nil {
			t.Fatal(err)
		}
		held = append(held, l)
	}
	defer func() {
		for _, l := range held {
			_ = l.Close()
		}
	}()

	var (
		segs     []string
		children []*listenerChild
	)
	for range 2 {
		child := startListenerChild(t, "client-enqueue", name)
		children = append(children, child)
		seg := child.line(t, "ENQUEUED ")
		segs = append(segs, seg)
		t.Cleanup(func() { _ = Unlink(seg) })
		if !nameExists(t, seg) {
			t.Fatalf("segment %q of a live client does not exist", seg)
		}
	}
	// Kill the clients while their records are queued, then let the server
	// drain the queue.
	for _, c := range children {
		c.kill(t)
	}
	serveEcho(t, a, 1)

	waitUntil(t, 15*time.Second, "the dead clients to be dropped", func() bool {
		st := a.Stats()
		return st.Records == 4 && st.Accepted == 2 && st.Unreachable+st.HandshakeFailed == 2
	})
	if shm.NamesPersist {
		for _, s := range segs {
			eventually(t, "the segment of a dead client to be removed", func() bool { return !nameExists(t, s) })
		}
	}
}

// TestAcceptorServerCrash: clients that wait when the acceptor dies fail
// within the peer timeout, and the name is reused by a new acceptor.
func TestAcceptorServerCrash(t *testing.T) {
	name := newPortableSharedMemoryName()
	const queue = 8
	child := startListenerChild(t, "server", name, fmt.Sprint(queue))
	child.line(t, "READY")

	ctx := testContext(t, 40*time.Second)
	cfg := listenerTestConfig()
	cfg.Advanced.Link.AttachTimeout = 20 * time.Second // the crash, not the timeout, must end the wait
	const clients = 5
	type result struct {
		l   *NamedAdvancedLink
		err error
		at  time.Time
	}
	results := make(chan result, clients)
	for range clients {
		go func() {
			l, err := Connect(ctx, name, cfg)
			results <- result{l, err, time.Now()}
		}()
	}

	// The child holds one link as ready and one in its blocked handshake;
	// the other clients wait.
	var connected []*NamedAdvancedLink
	defer func() {
		for _, l := range connected {
			_ = l.Close()
		}
	}()
	for range 2 {
		r := <-results
		if r.err != nil {
			t.Fatalf("Connect: %v\nchild stderr:\n%s", r.err, child.stderr.String())
		}
		connected = append(connected, r.l)
	}
	select {
	case r := <-results:
		if r.l != nil {
			connected = append(connected, r.l)
		}
		t.Fatalf("a third client finished (%v) although the server accepts at most two", r.err)
	case <-time.After(200 * time.Millisecond):
	}

	killed := time.Now()
	child.kill(t)
	for range clients - 2 {
		select {
		case r := <-results:
			if r.l != nil {
				connected = append(connected, r.l)
				t.Fatal("a waiting client connected to a dead server")
			}
			if !errors.Is(r.err, ErrListenerGone) || !errors.Is(r.err, ErrPeerGone) {
				t.Fatalf("waiting client failed with %v, want ErrListenerGone", r.err)
			}
			if d := r.at.Sub(killed); d > 5*time.Second {
				t.Fatalf("detected the dead server after %v", d)
			} else {
				t.Logf("dead server detected after %v", d)
			}
		case <-time.After(15 * time.Second):
			t.Fatal("a waiting client did not notice the dead server")
		}
	}
	// The links the dead server had attached to report it too.
	for _, l := range connected {
		waitUntil(t, 5*time.Second, "the link to notice its dead peer", func() bool { return l.PeerState() == PeerGone })
	}

	if shm.NamesPersist {
		// The dead listener's name is still there; a bigger queue does not
		// fit its object, and the object is left alone.
		if a, err := NewAcceptor(name, AcceptorConfig{QueueSize: 1024, Advanced: listenerTestOptions()}); !errors.Is(err, ErrInvalidSize) {
			if a != nil {
				_ = a.Close()
			}
			t.Fatalf("NewAcceptor with a bigger queue over a dead listener = %v, want ErrInvalidSize", err)
		}
		if !nameExists(t, name) {
			t.Fatal("the dead listener's name was removed")
		}
		// A client that arrives now finds a dead listener and waits.
		short := testContext(t, 200*time.Millisecond)
		if l, err := Connect(short, name, listenerTestConfig()); !errors.Is(err, context.DeadlineExceeded) {
			if l != nil {
				_ = l.Close()
			}
			t.Fatalf("Connect to a dead listener = %v, want to wait for the context", err)
		}
	}

	// The same configuration reuses the dead listener.
	b := newTestAcceptor(t, name, AcceptorConfig{QueueSize: queue})
	serveEcho(t, b, 1)
	l, err := Connect(ctx, name, listenerTestConfig())
	if err != nil {
		t.Fatalf("Connect to the restarted server: %v", err)
	}
	defer l.Close()
	if err := echoCall(ctx, l, []byte("restarted")); err != nil {
		t.Fatal(err)
	}
}

// TestAcceptorServerCloseWhileClientsWait: Close tells waiting clients.
func TestAcceptorServerCloseWhileClientsWait(t *testing.T) {
	name := newPortableSharedMemoryName()
	a := newTestAcceptor(t, name, AcceptorConfig{QueueSize: 8, Backlog: 1, MaxHandshakes: 1})
	ctx := testContext(t, 30*time.Second)
	cfg := listenerTestConfig()
	cfg.Advanced.Link.AttachTimeout = 20 * time.Second

	var held []*NamedAdvancedLink
	defer func() {
		for _, l := range held {
			_ = l.Close()
		}
	}()
	for range 2 {
		l, err := Connect(ctx, name, cfg)
		if err != nil {
			t.Fatal(err)
		}
		held = append(held, l)
	}
	errs := make(chan error, 2)
	for range 2 {
		go func() {
			l, err := Connect(ctx, name, cfg)
			if l != nil {
				_ = l.Close()
			}
			errs <- err
		}()
	}
	waitUntil(t, 5*time.Second, "the clients to queue", func() bool { return a.Stats().Records >= 3 })
	time.Sleep(50 * time.Millisecond)
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		select {
		case err := <-errs:
			// Close marks the listener closed; a client may instead have
			// seen its name disappear first and retry, which ends with the
			// context, so only the closed error is accepted here.
			if !errors.Is(err, ErrListenerClosed) {
				t.Fatalf("waiting client after Close = %v, want ErrListenerClosed", err)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("a waiting client did not notice Close")
		}
	}
}

func TestListenerWatch(t *testing.T) {
	mem := AlignedBuffer(pagesize)
	h := (*listenerHeader)(unsafe.Pointer(unsafe.SliceData(mem)))
	store(&h.state, listenerStateReady)
	store(&h.creatorToken, 5)
	store(&h.epoch, 1)
	store(&h.serverToken, 77)
	store(&h.serverInterval, uint64(10*time.Millisecond))
	store(&h.serverHeartbeat, nowNanos())
	w := newListenerWatch(h, 50*time.Millisecond)
	if err := w.check(); err != nil {
		t.Fatalf("fresh listener: %v", err)
	}
	store(&h.serverHeartbeat, nowNanos()+1)
	if err := w.check(); err != nil {
		t.Fatalf("beating listener: %v", err)
	}
	time.Sleep(80 * time.Millisecond)
	if err := w.check(); !errors.Is(err, ErrListenerGone) {
		t.Fatalf("stalled heartbeat = %v, want ErrListenerGone", err)
	}

	for name, tc := range map[string]struct {
		change func()
		want   error
	}{
		"closed":         {func() { store(&h.serverToken, detachedToken) }, ErrListenerClosed},
		"no server":      {func() { store(&h.serverToken, 0) }, ErrListenerClosed},
		"other server":   {func() { store(&h.serverToken, 78) }, ErrListenerGone},
		"new instance":   {func() { store(&h.creatorToken, 6) }, ErrListenerGone},
		"new epoch":      {func() { store(&h.epoch, 2) }, ErrListenerGone},
		"reinitializing": {func() { store(&h.state, listenerStateInitializing) }, ErrListenerGone},
	} {
		store(&h.state, listenerStateReady)
		store(&h.creatorToken, 5)
		store(&h.epoch, 1)
		store(&h.serverToken, 77)
		store(&h.serverHeartbeat, nowNanos())
		w := newListenerWatch(h, 50*time.Millisecond)
		tc.change()
		if err := w.check(); !errors.Is(err, tc.want) {
			t.Errorf("%s: check = %v, want %v", name, err, tc.want)
		}
	}
	if !errors.Is(ErrListenerClosed, io.EOF) || !errors.Is(ErrListenerBusy, ErrLinkBusy) {
		t.Error("listener errors do not wrap their base errors")
	}
}

// An acceptor heartbeat far in the future (clock stepped backwards) is
// confirmed by the progress watch, not trusted (F-04).
func TestListenerServerAliveFutureHeartbeat(t *testing.T) {
	mem := AlignedBuffer(pagesize)
	h := (*listenerHeader)(unsafe.Pointer(unsafe.SliceData(mem)))
	store(&h.serverToken, 77)
	store(&h.serverInterval, uint64(5*time.Millisecond))
	store(&h.serverHeartbeat, uint64(time.Now().Add(time.Hour).UnixNano()))
	opts := LinkOptions{PeerTimeout: 150 * time.Millisecond}.normalized()
	alive, err := listenerServerAlive(t.Context(), h, opts)
	if err != nil || alive {
		t.Fatalf("listenerServerAlive = %v, %v; want dead", alive, err)
	}
	store(&h.serverHeartbeat, nowNanos())
	if alive, err = listenerServerAlive(t.Context(), h, opts); err != nil || !alive {
		t.Fatalf("listenerServerAlive with a fresh heartbeat = %v, %v", alive, err)
	}
}
