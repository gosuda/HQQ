package rt

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	hqq "gosuda.org/hqq"
)

// microIface is the interface of microlib.idl (BINDGEN.md 2.7).
var microIface = &Iface{
	Service: "microlib",
	Ops: []string{"ml_add", "ml_counter_new", "ml_counter_add", "ml_counter_free", "ml_cursor_open",
		"ml_cursor_next", "ml_cursor_free", "ml_xor", "ml_hash", "ml_cells", "ml_counter_check"},
}

const (
	opAdd uint16 = iota + 1
	opCounterNew
	opCounterAdd
	opCounterFree
	opCursorOpen
	opCursorNext
	opCursorFree
	opXor
	opHash
	opCells
	opCounterCheck
)

func init() {
	d, err := hex.DecodeString("06a738744a1ea9f11fb08cc41afbe48e1891c76e60830a276e98cf8c15944e5c")
	if err != nil {
		panic(err)
	}
	copy(microIface.Digest[:], d)
}

type vectorFile struct {
	Microlib struct {
		Digest   string `json:"digest"`
		Geometry struct {
			BufferSize  int `json:"buffer_size"`
			BufferCount int `json:"buffer_count"`
		} `json:"geometry"`
		Frames []vectorFrame `json:"frames"`
	} `json:"microlib"`
	Chunking struct {
		BufferSize int           `json:"buffer_size"`
		Frames     []vectorFrame `json:"frames"`
	} `json:"chunking"`
	Wrap struct {
		Frames []vectorFrame `json:"frames"`
	} `json:"wrap"`
}

type vectorFrame struct {
	Name string `json:"name"`
	ID   uint32 `json:"id"`
	Dir  string `json:"dir"`
	Hex  string `json:"hex"`
}

// vectors maps a vector name to its frame bytes.
type vectors map[string][]byte

var (
	vecOnce sync.Once
	vecAll  vectors
	vecFile vectorFile
	vecErr  error
)

func loadVectors(tb testing.TB) (vectors, *vectorFile) {
	tb.Helper()
	vecOnce.Do(func() {
		raw, err := os.ReadFile(filepath.Join("..", "testdata", "vectors.json"))
		if err != nil {
			vecErr = err
			return
		}
		if err := json.Unmarshal(raw, &vecFile); err != nil {
			vecErr = err
			return
		}
		vecAll = vectors{}
		for _, set := range [][]vectorFrame{vecFile.Microlib.Frames, vecFile.Chunking.Frames, vecFile.Wrap.Frames} {
			for _, f := range set {
				b, err := hex.DecodeString(f.Hex)
				if err != nil {
					vecErr = fmt.Errorf("%s: %w", f.Name, err)
					return
				}
				vecAll[f.Name] = b
			}
		}
	})
	if vecErr != nil {
		tb.Fatalf("load vectors: %v", vecErr)
	}
	return vecAll, &vecFile
}

func (v vectors) get(tb testing.TB, name string) []byte {
	tb.Helper()
	b, ok := v[name]
	if !ok {
		tb.Fatalf("vector %s missing", name)
	}
	return bytes.Clone(b)
}

// frameOf builds one frame from a header and a body.
func frameOf(h header, body []byte) []byte {
	b := make([]byte, HeaderSize+len(body))
	putHeader(b, h)
	copy(b[HeaderSize:], body)
	return b
}

// splitMessage returns the frames of a message of kind first with the given
// header fields, split into chunks (BINDGEN.md 4.8).
func splitMessage(first uint8, chunk int, op uint16, id uint32, flags uint8, aux uint32, body []byte) [][]byte {
	var out [][]byte
	total := uint32(len(body))
	n := min(len(body), chunk)
	out = append(out, frameOf(header{kind: first, flags: flags, op: op, id: id, total: total, aux: aux}, body[:n]))
	for off := n; off < len(body); off += n {
		n = min(len(body)-off, chunk)
		out = append(out, frameOf(header{kind: kindCont, id: id, total: total, aux: uint32(off)}, body[off:off+n]))
	}
	return out
}

func errBody(msg string) []byte {
	b := make([]byte, StrWire(len(msg)))
	PutStr(b, 0, msg)
	return b
}

// testOpts are link options with fast heartbeats, so that closing and dying
// peers are noticed in milliseconds.
func testLinkOpts(exclusive bool) hqq.LinkOptions {
	return hqq.LinkOptions{
		HeartbeatInterval: 10 * time.Millisecond,
		PeerTimeout:       300 * time.Millisecond,
		AttachTimeout:     3 * time.Second,
		Exclusive:         exclusive,
	}
}

// pair is an in-process session link: server is the Primary (it writes the
// response direction), client is the Secondary.
type pair struct {
	server, client *hqq.StandardLink
	count, size    int
}

func newPair(tb testing.TB, count, size int) *pair {
	tb.Helper()
	mem := hqq.AlignedBuffer(hqq.SizeStandardLink(count, size))
	server, err := hqq.NewStandardLink(mem, count, size, testLinkOpts(false))
	if err != nil {
		tb.Fatalf("server link: %v", err)
	}
	client, err := hqq.NewStandardLink(mem, count, size, testLinkOpts(true))
	if err != nil {
		_ = server.Close()
		tb.Fatalf("client link: %v", err)
	}
	p := &pair{server: server, client: client, count: count, size: size}
	tb.Cleanup(func() {
		_ = client.Close()
		_ = server.Close()
	})
	return p
}

// connOpts adjusts the Conn that newTestConn builds.
type connOpts struct {
	maxInFlight int
	maxMessage  int
	callTimeout time.Duration
	closeTO     time.Duration
	iface       *Iface
}

func newTestConn(tb testing.TB, p *pair, o connOpts) *Conn {
	tb.Helper()
	if o.maxInFlight == 0 {
		o.maxInFlight = p.count
	}
	if o.maxMessage == 0 {
		o.maxMessage = 16 << 20
	}
	if o.closeTO == 0 {
		o.closeTO = 2 * time.Second
	}
	if o.iface == nil {
		o.iface = microIface
	}
	cfg := config{callTimeout: o.callTimeout, closeTimeout: o.closeTO}
	lim := Limits{
		BufferCount: p.count, BufferSize: p.size,
		MaxInFlight: o.maxInFlight, MaxMessage: o.maxMessage, MaxHandles: 4096,
	}
	c := newConn(p.client, p.client.Close, o.iface, cfg, lim)
	tb.Cleanup(func() { _ = c.Close() })
	return c
}

// fakeReq is one request as the fake server received it.
type fakeReq struct {
	op   uint16
	id   uint32
	body []byte
}

// fakeReply says what the fake server sends for one request.
type fakeReply struct {
	status uint32
	flags  uint8
	body   []byte
	// raw, if not nil, is written frame by frame instead of the response.
	raw [][]byte
	// wait delays the reply (and every later one) until it is closed.
	wait <-chan struct{}
	// none sends nothing.
	none bool
	// final sends a final frame (id 0) with this status after the reply.
	final uint32
	// hangup closes the server link after the reply.
	hangup bool
}

type fakeItem struct {
	reply fakeReply
	req   fakeReq
	close bool
}

// tbLike is the part of testing.TB the fakes use, so that the helper process
// of the peer-death test can provide it too.
type tbLike interface {
	Helper()
	Errorf(format string, args ...any)
	Fatalf(format string, args ...any)
	Cleanup(func())
}

// fakeServer speaks the server side of BINDGEN.md 4 on the Primary end of a
// pair: a reader goroutine receives and validates request frames the way the C
// server does, and a writer goroutine sends the replies in order.
type fakeServer struct {
	tb     tbLike
	link   *hqq.StandardLink
	chunk  int
	handle func(fakeReq) fakeReply

	queue chan fakeItem
	wg    sync.WaitGroup

	mu         sync.Mutex
	violations []string
	reqs       []fakeReq
	closeAcks  int
	sent       int // response frames written
	closeAux   uint32
}

func startFakeServer(tb testing.TB, p *pair, handle func(fakeReq) fakeReply) *fakeServer {
	tb.Helper()
	tb.Cleanup(func() { _ = p.client.Close() })
	s := startFakeServerOn(tb, p.server, p.size, handle)
	tb.Cleanup(s.stop)
	return s
}

// startFakeServerOn serves the session link link, whose buffers are size
// bytes.
func startFakeServerOn(tb tbLike, link *hqq.StandardLink, size int, handle func(fakeReq) fakeReply) *fakeServer {
	tb.Helper()
	s := &fakeServer{
		tb: tb, link: link, chunk: size - HeaderSize, handle: handle,
		queue: make(chan fakeItem, 4096),
	}
	s.wg.Add(2)
	go s.reader()
	go s.writer()
	return s
}

// stop closes the server link, waits for both goroutines and reports the
// violations the client committed.
func (s *fakeServer) stop() {
	_ = s.link.Close()
	s.wg.Wait()
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, v := range s.violations {
		s.tb.Errorf("client protocol violation seen by the fake server: %s", v)
	}
	s.violations = nil
}

func (s *fakeServer) violate(format string, args ...any) {
	s.mu.Lock()
	s.violations = append(s.violations, fmt.Sprintf(format, args...))
	s.mu.Unlock()
}

func (s *fakeServer) requests() []fakeReq {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]fakeReq(nil), s.reqs...)
}

func (s *fakeServer) requestCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.reqs)
}

func (s *fakeServer) reader() {
	defer s.wg.Done()
	defer close(s.queue)
	var (
		last     uint32
		asm      []byte
		asmHdr   header
		asmTotal int
	)
	for {
		r, err := s.link.ReserveRead()
		if err != nil {
			return
		}
		b := r.Buffer()
		if len(b) < HeaderSize {
			s.violate("short frame %d", len(b))
			_ = r.Release()
			return
		}
		h := loadHeader(b)
		body := bytes.Clone(b[HeaderSize:])
		size := len(b)
		_ = r.Release()
		if h.flags != 0 {
			s.violate("flags %#x on kind %d", h.flags, h.kind)
		}
		if asm != nil {
			if h.kind != kindCont || h.id != asmHdr.id || h.total != asmHdr.total || int(h.aux) != len(asm) ||
				len(body) != min(asmTotal-len(asm), s.chunk) {
				s.violate("bad continuation %+v", h)
				return
			}
			asm = append(asm, body...)
			if len(asm) < asmTotal {
				continue
			}
			s.dispatch(asmHdr, asm, &last)
			asm = nil
			continue
		}
		switch h.kind {
		case kindRequest:
			if len(body) != min(int(h.total), s.chunk) {
				s.violate("request length %d for total %d", len(body), h.total)
				return
			}
			if h.aux != 0 {
				s.violate("request aux %d", h.aux)
			}
			if int(h.total) > s.chunk {
				asmHdr, asmTotal = h, int(h.total)
				asm = append(make([]byte, 0, asmTotal), body...)
				continue
			}
			s.dispatch(h, body, &last)
		case kindClose:
			if size != HeaderSize || h.total != 0 {
				s.violate("malformed CLOSE")
			}
			s.queue <- fakeItem{close: true}
			// The client writes nothing after CLOSE; keep reading until the
			// link ends so that the writer can finish.
			continue
		default:
			s.violate("unexpected kind %d", h.kind)
			return
		}
	}
}

func (s *fakeServer) dispatch(h header, body []byte, last *uint32) {
	if want := nextID(*last); h.id != want {
		s.violate("request id %d, want %d", h.id, want)
	}
	*last = h.id
	req := fakeReq{op: h.op, id: h.id, body: body}
	s.mu.Lock()
	s.reqs = append(s.reqs, req)
	s.mu.Unlock()
	s.queue <- fakeItem{reply: s.handle(req), req: req}
}

func (s *fakeServer) writeFrame(b []byte) error {
	w, err := s.link.ReserveWrite()
	if err != nil {
		return err
	}
	copy(w.Buffer(), b)
	if err := w.Commit(len(b)); err != nil {
		return err
	}
	s.mu.Lock()
	s.sent++
	s.mu.Unlock()
	return nil
}

func (s *fakeServer) writer() {
	defer s.wg.Done()
	for it := range s.queue {
		var stop bool
		if it.close {
			stop = s.writeCloseAck() != nil
		} else {
			stop = s.writeReply(it)
		}
		if stop {
			return
		}
	}
}

func (s *fakeServer) writeCloseAck() error {
	if err := s.writeFrame(frameOf(header{kind: kindCloseAck, aux: s.closeAux}, nil)); err != nil {
		return err
	}
	s.mu.Lock()
	s.closeAcks++
	s.mu.Unlock()
	return nil
}

// writeReply sends one reply and reports whether the writer must stop.
func (s *fakeServer) writeReply(it fakeItem) (stop bool) {
	rp := it.reply
	if rp.wait != nil {
		<-rp.wait
	}
	if !rp.none {
		frames := rp.raw
		if frames == nil {
			frames = splitMessage(kindResponse, s.chunk, it.req.op, it.req.id, rp.flags, rp.status, rp.body)
		}
		for _, f := range frames {
			if s.writeFrame(f) != nil {
				return true
			}
		}
	}
	if rp.final != 0 {
		final := frameOf(header{kind: kindResponse, aux: rp.final, total: uint32(StrWire(5))}, errBody("final"))
		if s.writeFrame(final) != nil {
			return true
		}
	}
	if rp.hangup {
		_ = s.link.Close()
		return true
	}
	return false
}

func (s *fakeServer) closeAckCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closeAcks
}

// i32Body is the response body of an Add: ret is a fixed i32.
func i32Body(v int32) []byte {
	b := make([]byte, 4)
	le.PutUint32(b, uint32(v))
	return b
}

// microHandler answers the calls the tests need, with the semantics of
// microlib.c: Add, Xor and CounterCheck (always failing with detail).
func microHandler(req fakeReq) fakeReply {
	switch req.op {
	case opAdd:
		return fakeReply{body: i32Body(int32(le.Uint32(req.body[0:])) + int32(le.Uint32(req.body[4:])))}
	case opXor:
		key := req.body[0]
		capacity := int(le.Uint32(req.body[4:]))
		n := int(le.Uint32(req.body[8:]))
		src := req.body[12 : 12+n]
		if capacity < n {
			b := make([]byte, 8)
			le.PutUint64(b, ^uint64(0))
			return fakeReply{flags: flagFail, body: b}
		}
		b := make([]byte, 8+4+n)
		le.PutUint64(b, uint64(n))
		le.PutUint32(b[8:], uint32(n))
		for i, c := range src {
			b[12+i] = c ^ key
		}
		return fakeReply{body: b}
	case opCounterCheck:
		b := make([]byte, 8+StrWire(10))
		le.PutUint32(b, ^uint32(0))
		PutStr(b, 8, "over limit")
		return fakeReply{flags: flagFail | flagDetail, body: b}
	}
	return fakeReply{status: statusUnknownOp, body: errBody("unknown op")}
}
