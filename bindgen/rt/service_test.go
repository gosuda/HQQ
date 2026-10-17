package rt

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	hqq "gosuda.org/hqq"
)

// serviceOpts shapes the behavior of a fakeService.
type serviceOpts struct {
	digest      [32]byte
	maxInFlight int // server cap, 0: B
	maxMessage  int // server cap, 0: 16 MiB
	handle      func(fakeReq) fakeReply

	openFail    uint32              // nonzero: refuse OPEN with this code
	openMod     func([]byte) []byte // rewrites the OPEN response
	createDelay time.Duration       // create the segment this long after answering OPEN
	noCreate    bool                // answer OPEN, create nothing
	noReply     bool                // read HELLO, never answer
	replyMod    func([]byte) []byte // rewrites the HELLO_REPLY frame
}

// fakeService is the server side of 4.11 in Go: an Acceptor, a BG_OPEN
// handler that creates the session segment, and the session loop.
type fakeService struct {
	tb   tbLike
	name string
	opts serviceOpts
	acc  *hqq.Acceptor

	cancel context.CancelFunc
	wg     sync.WaitGroup
	hello  chan []byte

	mu       sync.Mutex
	names    []string
	sessions []*fakeSession
}

type fakeSession struct {
	link   *hqq.NamedStandardLink
	server *fakeServer
}

func randHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

// listenerName returns a fresh listener name (macOS limits names to 30
// bytes).
func listenerName() string { return "bgrt" + randHex(4) }

func startService(tb tbLike, o serviceOpts) *fakeService {
	tb.Helper()
	if o.handle == nil {
		o.handle = microHandler
	}
	s := &fakeService{tb: tb, name: listenerName(), opts: o, hello: make(chan []byte, 8)}
	acc, err := hqq.NewAcceptor(s.name, hqq.AcceptorConfig{Advanced: hqq.AdvancedOptions{Link: testLinkOpts()}})
	if err != nil {
		tb.Fatalf("NewAcceptor: %v", err)
	}
	s.acc = acc
	ctx, cancel := context.WithCancel(context.Background())
	s.cancel = cancel
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		for {
			l, err := acc.Accept(ctx)
			if err != nil {
				return
			}
			s.wg.Add(1)
			go func() {
				defer s.wg.Done()
				_ = l.Serve(ctx, hqq.HandlerFunc(s.open))
				_ = l.Close()
			}()
		}
	}()
	return s
}

func (s *fakeService) stop() {
	s.cancel()
	_ = s.acc.Close()
	// Session goroutines may wait for a HELLO that never comes: closing the
	// links wakes them. Keep closing until every goroutine has returned,
	// because a session can still be created while the first pass runs.
	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()
	for finished := false; !finished; {
		s.mu.Lock()
		sessions := append([]*fakeSession(nil), s.sessions...)
		s.mu.Unlock()
		for _, ss := range sessions {
			_ = ss.link.Close()
		}
		select {
		case <-done:
			finished = true
		case <-time.After(5 * time.Millisecond):
		}
	}
	s.mu.Lock()
	sessions := s.sessions
	names := s.names
	s.mu.Unlock()
	for _, ss := range sessions {
		if ss.server != nil {
			ss.server.stop()
		}
	}
	for _, n := range names {
		_ = hqq.Unlink(n)
	}
	_ = hqq.Unlink(s.name)
}

func (s *fakeService) openError(w *hqq.ResponseWriter, code uint32, msg string) {
	w.WriteError(openStatusBase+uint64(code), []byte(msg))
}

func (s *fakeService) open(_ context.Context, w *hqq.ResponseWriter, req *hqq.Request) {
	p := req.Payload()
	if req.Method() != openMeth || len(p) != openReq || [4]byte(p[0:4]) != openMagic {
		s.openError(w, openCodeInternal, "bad request")
		return
	}
	if le.Uint16(p[4:]) != protoMajor {
		s.openError(w, openCodeVersion, "version")
		return
	}
	if s.opts.openFail != 0 {
		s.openError(w, s.opts.openFail, "refused")
		return
	}
	count, size := int(le.Uint32(p[8:])), int(le.Uint32(p[12:]))
	count, size = cmpOr(count, defaultBufferCount), cmpOr(size, defaultBufferSize)
	if !validBufferCount(count) || !validBufferSize(size) {
		s.openError(w, openCodeGeometry, "geometry")
		return
	}
	prefix := s.name
	if len(prefix) > 8 {
		prefix = prefix[:8]
	}
	name := prefix + ".s" + randHex(8)
	s.mu.Lock()
	s.names = append(s.names, name)
	s.mu.Unlock()

	mkSession := func() *fakeSession {
		ss, err := hqq.CreateStandardLink(name, count, size, 0o600, testLinkOpts())
		if err != nil {
			s.tb.Errorf("create session segment: %v", err)
			return nil
		}
		sess := &fakeSession{link: ss}
		s.mu.Lock()
		s.sessions = append(s.sessions, sess)
		s.mu.Unlock()
		return sess
	}
	var early *fakeSession
	if !s.opts.noCreate && s.opts.createDelay == 0 {
		if early = mkSession(); early == nil {
			s.openError(w, openCodeInternal, "create")
			return
		}
	}

	resp := make([]byte, openResp+len(name))
	le.PutUint32(resp[0:], uint32(count))
	le.PutUint32(resp[4:], uint32(size))
	le.PutUint16(resp[8:], uint16(len(name)))
	copy(resp[openResp:], name)
	if s.opts.openMod != nil {
		resp = s.opts.openMod(resp)
	}
	if _, err := w.Write(resp); err != nil {
		return
	}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		switch {
		case s.opts.noCreate:
		case early != nil:
			s.session(early, count, size)
		default:
			time.Sleep(s.opts.createDelay)
			if sess := mkSession(); sess != nil {
				s.session(sess, count, size)
			}
		}
	}()
}

// session reads HELLO, removes the name and answers, then serves requests.
func (s *fakeService) session(sess *fakeSession, count, size int) {
	link := sess.link.StandardLink
	r, err := link.ReserveRead()
	if err != nil {
		return
	}
	frame := make([]byte, len(r.Buffer()))
	copy(frame, r.Buffer())
	_ = r.Release()
	select {
	case s.hello <- frame:
	default:
	}
	_ = sess.link.Unlink()
	if s.opts.noReply {
		return
	}
	status := uint32(helloOK)
	h := loadHeader(frame)
	switch {
	case len(frame) != HeaderSize+helloBody || h.kind != kindHello || h.total != helloBody || [4]byte(frame[16:20]) != magic:
		status = helloBad
	case le.Uint16(frame[20:]) != protoMajor:
		status = helloVersion
	case [32]byte(frame[48:80]) != s.opts.digest:
		status = helloDigestMismatch
	case int(le.Uint32(frame[28:])) != size || int(le.Uint32(frame[32:])) != count:
		status = helloGeometry
	}
	body := make([]byte, helloBody)
	copy(body, magic[:])
	le.PutUint16(body[4:], protoMajor)
	copy(body[32:], s.opts.digest[:])
	if status == helloOK {
		mif := cmpOr(int(le.Uint32(frame[36:])), count)
		mif = min(mif, cmpOr(s.opts.maxInFlight, count), count)
		mm := cmpOr(int(le.Uint32(frame[40:])), defaultMaxMessage)
		mm = min(mm, cmpOr(s.opts.maxMessage, defaultMaxMessage))
		le.PutUint32(body[12:], uint32(mif))
		le.PutUint32(body[16:], uint32(mm))
		le.PutUint32(body[20:], 4096)
	}
	reply := frameOf(header{kind: kindHelloReply, aux: status, total: helloBody}, body)
	if s.opts.replyMod != nil {
		reply = s.opts.replyMod(reply)
	}
	w, err := link.ReserveWrite()
	if err != nil {
		return
	}
	copy(w.Buffer(), reply)
	if err := w.Commit(len(reply)); err != nil || status != helloOK {
		return
	}
	sess.server = startFakeServerOn(s.tb, link, size, s.opts.handle)
}

func cmpOr(v, def int) int {
	if v == 0 {
		return def
	}
	return v
}

func dialOpts() Options {
	return Options{
		ConnectTimeout: 5 * time.Second,
		CloseTimeout:   2 * time.Second,
		Link:           testLinkOpts(),
	}
}

func TestDialAndCall(t *testing.T) {
	vecs, _ := loadVectors(t)
	svc := startService(t, serviceOpts{digest: microIface.Digest})
	t.Cleanup(svc.stop)
	c, err := Dial(context.Background(), svc.name, microIface, dialOpts())
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	select {
	case got := <-svc.hello:
		if want := vecs.get(t, "HELLO"); string(got) != string(want) {
			t.Errorf("HELLO = %s, want %s", hexOf(got), hexOf(want))
		}
	case <-time.After(time.Second):
		t.Fatal("no HELLO")
	}
	want := Limits{BufferCount: 64, BufferSize: 16384, MaxInFlight: 64, MaxMessage: 16 << 20, MaxHandles: 4096}
	if got := c.Limits(); got != want {
		t.Errorf("Limits = %+v, want %+v", got, want)
	}
	cl := &stubClient{c}
	for i := range int32(5) {
		if got, err := cl.Add(i, i); err != nil || got != 2*i {
			t.Fatalf("Add = %d, %v", got, err)
		}
	}
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// The server removed the segment name when it read HELLO.
	svc.mu.Lock()
	name := svc.names[0]
	svc.mu.Unlock()
	requireNameGone(t, name)
}

func TestDialDigestMismatch(t *testing.T) {
	svc := startService(t, serviceOpts{digest: microIface.Digest})
	t.Cleanup(svc.stop)
	other := &Iface{Service: "microlib", Ops: microIface.Ops}
	other.Digest[5] = 0x55
	_, err := Dial(context.Background(), svc.name, other, dialOpts())
	var me *MismatchError
	if !errors.As(err, &me) || me.Reason != helloDigestMismatch || me.Local != other.Digest || me.Remote != microIface.Digest {
		t.Fatalf("Dial = %v, want MismatchError with both digests", err)
	}
	if !errors.Is(err, ErrInterfaceMismatch) {
		t.Error("does not match ErrInterfaceMismatch")
	}
	// No segment is left behind: the server removed the name at HELLO and
	// the client's link is closed.
	svc.mu.Lock()
	name := svc.names[0]
	svc.mu.Unlock()
	requireNameGone(t, name)
}

func TestDialOpenErrors(t *testing.T) {
	for code := uint32(1); code <= 4; code++ {
		svc := startService(t, serviceOpts{digest: microIface.Digest, openFail: code})
		_, err := Dial(context.Background(), svc.name, microIface, dialOpts())
		svc.stop()
		var oe *OpenError
		if !errors.As(err, &oe) || oe.Code != code || oe.Msg != "refused" {
			t.Fatalf("code %d: Dial = %v", code, err)
		}
		if got := errors.Is(err, ErrLimit); got != (code == 2 || code == 3) {
			t.Errorf("code %d: matches ErrLimit = %v", code, got)
		}
	}
}

func TestDialGrantedLimits(t *testing.T) {
	svc := startService(t, serviceOpts{digest: microIface.Digest, maxInFlight: 8, maxMessage: 8192})
	t.Cleanup(svc.stop)
	o := dialOpts()
	o.BufferCount, o.BufferSize = 16, 1024
	c, err := Dial(context.Background(), svc.name, microIface, o)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	want := Limits{BufferCount: 16, BufferSize: 1024, MaxInFlight: 8, MaxMessage: 8192, MaxHandles: 4096}
	if got := c.Limits(); got != want {
		t.Fatalf("Limits = %+v, want %+v", got, want)
	}
	if _, err := c.BeginRequest(opAdd, 8193, 4); !errors.Is(err, ErrInvalidArg) {
		t.Errorf("body above the granted max_message: %v", err)
	}
	cl := &stubClient{c}
	var futs []addFuture
	for i := range int32(30) {
		f, err := cl.AddAsync(i, 1)
		if err != nil {
			t.Fatal(err)
		}
		futs = append(futs, f)
		if c.unansN > 8 {
			t.Fatalf("%d calls in flight, granted 8", c.unansN)
		}
	}
	for i, f := range futs {
		if got, err := f.Wait(); err != nil || got != int32(i)+1 {
			t.Fatalf("Wait %d = %d, %v", i, got, err)
		}
	}
}

func TestDialRetriesUntilSegmentExists(t *testing.T) {
	svc := startService(t, serviceOpts{digest: microIface.Digest, createDelay: 250 * time.Millisecond})
	t.Cleanup(svc.stop)
	start := time.Now()
	c, err := Dial(context.Background(), svc.name, microIface, dialOpts())
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	if d := time.Since(start); d < 200*time.Millisecond {
		t.Errorf("Dial returned after %v, before the segment existed", d)
	}
	if got, err := (&stubClient{c}).Add(2, 3); err != nil || got != 5 {
		t.Fatalf("Add = %d, %v", got, err)
	}
}

func TestDialFailsWhenSegmentNeverAppears(t *testing.T) {
	svc := startService(t, serviceOpts{digest: microIface.Digest, noCreate: true})
	t.Cleanup(svc.stop)
	o := dialOpts()
	o.ConnectTimeout = 400 * time.Millisecond
	start := time.Now()
	_, err := Dial(context.Background(), svc.name, microIface, o)
	if !errors.Is(err, hqq.ErrTimeout) {
		t.Fatalf("Dial = %v, want a timeout", err)
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Errorf("Dial took %v", d)
	}
}

// TestDialCleansUpWhenHelloIsNotAnswered: the server created the segment and
// never answers, so the client removes the name itself (4.11).
func TestDialCleansUpWhenHelloIsNotAnswered(t *testing.T) {
	svc := startService(t, serviceOpts{digest: microIface.Digest, noReply: true})
	t.Cleanup(svc.stop)
	o := dialOpts()
	o.ConnectTimeout = 400 * time.Millisecond
	_, err := Dial(context.Background(), svc.name, microIface, o)
	if !errors.Is(err, hqq.ErrTimeout) {
		t.Fatalf("Dial = %v, want a timeout", err)
	}
	svc.mu.Lock()
	name := svc.names[0]
	svc.mu.Unlock()
	// The creator is still alive and holds the segment; only the name is
	// gone.
	requireNameGone(t, name)
}

func TestDialContextCancel(t *testing.T) {
	svc := startService(t, serviceOpts{digest: microIface.Digest, noReply: true})
	t.Cleanup(svc.stop)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(150 * time.Millisecond)
		cancel()
	}()
	start := time.Now()
	_, err := Dial(ctx, svc.name, microIface, dialOpts())
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Dial = %v, want context.Canceled", err)
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Errorf("Dial took %v after cancel", d)
	}
}

func TestDialNoListener(t *testing.T) {
	o := dialOpts()
	o.ConnectTimeout = 200 * time.Millisecond
	_, err := Dial(context.Background(), listenerName(), microIface, o)
	if !errors.Is(err, hqq.ErrTimeout) {
		t.Fatalf("Dial = %v, want a timeout", err)
	}
}

func TestDialMalformedAnswers(t *testing.T) {
	cases := []struct {
		name string
		o    serviceOpts
	}{
		{"OPEN name length", serviceOpts{openMod: func(b []byte) []byte { return b[:len(b)-1] }}},
		{"OPEN name slash", serviceOpts{openMod: func(b []byte) []byte { b[openResp] = '/'; return b }}},
		{"OPEN geometry", serviceOpts{openMod: func(b []byte) []byte { b[0] = 3; return b }}},
		{"OPEN short", serviceOpts{openMod: func(b []byte) []byte { return b[:5] }}},
		{"HELLO_REPLY kind", serviceOpts{replyMod: func(b []byte) []byte { b[0] = kindResponse; return b }}},
		{"HELLO_REPLY short", serviceOpts{replyMod: func(b []byte) []byte { return b[:60] }}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc.o.digest = microIface.Digest
			svc := startService(t, tc.o)
			t.Cleanup(svc.stop)
			o := dialOpts()
			o.ConnectTimeout = 500 * time.Millisecond
			_, err := Dial(context.Background(), svc.name, microIface, o)
			var pe *ProtocolError
			if !errors.As(err, &pe) {
				t.Fatalf("Dial = %v, want *ProtocolError", err)
			}
		})
	}
}

func TestDialInvalidOptions(t *testing.T) {
	for _, o := range []Options{
		{BufferCount: 3}, {BufferCount: 1}, {BufferCount: 8192}, {BufferSize: 100}, {BufferSize: 64}, {BufferSize: 2 << 20},
		{MaxInFlight: -1}, {BufferCount: 8, MaxInFlight: 9}, {MaxMessage: 1000}, {MaxMessage: 1<<30 + 1},
		{ConnectTimeout: -1}, {CloseTimeout: -1}, {CallTimeout: -1},
	} {
		if _, err := Dial(context.Background(), "x", microIface, o); !errors.Is(err, ErrInvalidArg) {
			t.Errorf("%+v: %v", o, err)
		}
	}
	if _, err := Dial(context.Background(), "x", nil, Options{}); !errors.Is(err, ErrInvalidArg) {
		t.Errorf("nil interface: %v", err)
	}
}

func TestSessionLinkOptions(t *testing.T) {
	o := sessionLinkOptions(hqq.LinkOptions{})
	if o.PeerTimeout != 2*time.Second {
		t.Errorf("zero options become %+v", o)
	}
	o = sessionLinkOptions(hqq.LinkOptions{PeerTimeout: 7 * time.Second, WaitPolicy: hqq.WaitSpin})
	if o.PeerTimeout != 7*time.Second || o.WaitPolicy != hqq.WaitSpin {
		t.Errorf("explicit options changed: %+v", o)
	}
	cfg, err := Options{}.resolve()
	if err != nil {
		t.Fatal(err)
	}
	want := config{
		bufferCount: 64, bufferSize: 16384, maxInFlight: 64, maxMessage: 16 << 20,
		connectTimeout: 5 * time.Second, closeTimeout: 5 * time.Second,
		link: hqq.LinkOptions{PeerTimeout: 2 * time.Second},
	}
	if cfg != want {
		t.Errorf("defaults: %+v, want %+v", cfg, want)
	}
}

func TestRetryableOpen(t *testing.T) {
	if !retryableOpen(os.ErrNotExist) {
		t.Error("ErrNotExist")
	}
	if !retryableOpen(fmt.Errorf("x: %w", hqq.ErrFailedInit)) {
		t.Error("ErrFailedInit")
	}
	for _, err := range []error{hqq.ErrLinkBusy, hqq.ErrIncompatibleLayout, hqq.ErrInvalidSize, hqq.ErrInvalidName, hqq.ErrMemoryAlign} {
		if retryableOpen(err) {
			t.Errorf("%v is final", err)
		}
	}
}
