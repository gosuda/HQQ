//go:build unix

package microlib_test

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	hqq "gosuda.org/hqq"
	"gosuda.org/hqq/bindgen/idl"
)

// A hand-written client of the wire protocol (BINDGEN.md 4). The generated client cannot misbehave,
// so the tests that need a client that ignores flow control, sends a malformed frame or a malformed
// bootstrap use this one. It shares no code with bindgen/rt: the layout below is written from the
// document.

var le = binary.LittleEndian

const (
	kindRequest    = 0x01
	kindResponse   = 0x02
	kindHello      = 0x04
	kindHelloReply = 0x05
	kindClose      = 0x08
	kindCloseAck   = 0x09

	opAdd = 1 // ml_add, the first function of microlib.idl

	openMethod       = 1
	openCodeVersion  = 1
	openCodeGeometry = 2
	openCodeBusy     = 3
	openCodeInternal = 4
)

// microlibDigest returns the interface digest of microlib.idl, computed by the parser.
func microlibDigest(t testing.TB, file string) [32]byte {
	t.Helper()
	src, err := os.ReadFile("../../testdata/microlib/" + file)
	if err != nil {
		t.Fatalf("reading %s: %v", file, err)
	}
	spec, err := idl.Parse(file, src, idl.Options{GoStrip: "ml_"})
	if err != nil {
		t.Fatalf("parsing %s: %v", file, err)
	}
	return spec.Digest()
}

// openRequestBytes builds the 16-byte OPEN request of 4.11.
func openRequestBytes(major, minor uint16, count, size uint32) []byte {
	b := make([]byte, 16)
	copy(b, "HQBO")
	le.PutUint16(b[4:], major)
	le.PutUint16(b[6:], minor)
	le.PutUint32(b[8:], count)
	le.PutUint32(b[12:], size)
	return b
}

// controlLink connects to the listener of s the way rt.Dial does: a negotiated Advanced link of four
// 4096-byte buffers.
func controlLink(t testing.TB, s *server) *hqq.NamedAdvancedLink {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), failBound)
	defer cancel()
	ctl, err := hqq.Connect(ctx, s.name, hqq.Config{
		BufferCount: 4,
		BufferSize:  4096,
		Features:    hqq.FeatureRequestResponse,
	})
	if err != nil {
		t.Fatalf("hqq.Connect(%s): %v\nserver stderr:\n%s", s.name, err, s.stderr.String())
	}
	return ctl
}

// openCall sends payload as method on a fresh control link and returns the Advanced response status
// and payload of an error, or the OPEN response body.
type openReply struct {
	count, size uint32
	name        string
	code        uint32 // OPEN error code, 0 on success
	msg         string
}

func callOpen(t testing.TB, ctl *hqq.NamedAdvancedLink, method uint64, payload []byte) openReply {
	t.Helper()
	r, err := tryCallOpen(ctl, method, payload)
	if err != nil {
		t.Fatalf("control link call: %v", err)
	}
	return r
}

// tryCallOpen is callOpen that returns a transport failure instead of failing the test.
func tryCallOpen(ctl *hqq.NamedAdvancedLink, method uint64, payload []byte) (openReply, error) {
	ctx, cancel := contextWithFail()
	defer cancel()
	msg, err := ctl.Call(ctx, method, payload)
	if err != nil {
		var re hqq.ResponseError
		if errors.As(err, &re) && re.Status > 0x50000 && re.Status < 0x60000 {
			return openReply{code: uint32(re.Status - 0x50000), msg: string(re.Payload)}, nil
		}
		return openReply{}, err
	}
	defer msg.Release()
	b := msg.Bytes()
	if len(b) < 12 {
		return openReply{}, fmt.Errorf("OPEN response of %d bytes", len(b))
	}
	n := int(le.Uint16(b[8:]))
	if len(b) != 12+n {
		return openReply{}, fmt.Errorf("OPEN response of %d bytes with a name of %d", len(b), n)
	}
	return openReply{count: le.Uint32(b[0:]), size: le.Uint32(b[4:]), name: string(b[12:])}, nil
}

// rawConn is a session whose frames the test writes and reads itself.
type rawConn struct {
	t    testing.TB
	nl   *hqq.NamedStandardLink
	link *hqq.StandardLink
}

// frame is one frame as received.
type frame struct {
	kind, flags byte
	op          uint16
	id          uint32
	total, aux  uint32
	body        []byte
}

func (f frame) String() string {
	return fmt.Sprintf("{kind %#x flags %#x op %d id %d total %d aux %d body %d bytes}", f.kind, f.flags, f.op, f.id, f.total, f.aux, len(f.body))
}

// rawOpen runs the bootstrap and attaches the session link at geometry count x size, with the
// liveness timing of the suite, without sending HELLO. The caller owns the connection.
func rawOpen(t testing.TB, s *server, count, size uint32, link hqq.LinkOptions) *rawConn {
	t.Helper()
	r := openOnNewControlLink(t, s, openMethod, openRequestBytes(1, 0, count, size))
	if r.code != 0 {
		t.Fatalf("OPEN refused with code %d: %s", r.code, r.msg)
	}
	nl, err := hqq.OpenStandardLink(r.name, int(r.count), int(r.size), link)
	if err != nil {
		t.Fatalf("attaching the session link %s: %v", r.name, err)
	}
	c := &rawConn{t: t, nl: nl, link: nl.StandardLink}
	t.Cleanup(func() { _ = c.nl.Close() })
	return c
}

// helloFields are the fields of a HELLO body (4.11) that the tests vary.
type helloFields struct {
	major, minor            uint16
	flags                   uint32
	size, count             uint32
	maxInFlight, maxMessage uint32
	reserved                uint32
	digest                  [32]byte
}

// sendHelloFields commits a HELLO frame with exactly these fields.
func (c *rawConn) sendHelloFields(h helloFields) {
	c.t.Helper()
	body := make([]byte, 64)
	copy(body, "HQBG")
	le.PutUint16(body[4:], h.major)
	le.PutUint16(body[6:], h.minor)
	le.PutUint32(body[8:], h.flags)
	le.PutUint32(body[12:], h.size)
	le.PutUint32(body[16:], h.count)
	le.PutUint32(body[20:], h.maxInFlight)
	le.PutUint32(body[24:], h.maxMessage)
	le.PutUint32(body[28:], h.reserved)
	copy(body[32:], h.digest[:])
	c.send(kindHello, 0, 0, 0, 64, 0, body)
}

// sendHello commits a valid HELLO with the given digest and geometry and the server's default limits.
func (c *rawConn) sendHello(digest [32]byte, size, count uint32) {
	c.t.Helper()
	c.sendHelloFields(helloFields{major: 1, size: size, count: count, digest: digest})
}

// hello sends HELLO and returns the reply.
func (c *rawConn) hello(digest [32]byte, size, count uint32) frame {
	c.t.Helper()
	c.sendHello(digest, size, count)
	return c.recv(failBound)
}

// send commits one frame. total and aux are written as given, so that a test can lie.
func (c *rawConn) send(kind, flags byte, op uint16, id, total, aux uint32, body []byte) {
	c.t.Helper()
	if err := c.trySend(kind, flags, op, id, total, aux, body); err != nil {
		c.t.Fatalf("sending frame kind %#x id %d: %v", kind, id, err)
	}
}

func (c *rawConn) trySend(kind, flags byte, op uint16, id, total, aux uint32, body []byte) error {
	if err := c.link.SetWriteDeadline(time.Now().Add(failBound)); err != nil {
		return err
	}
	w, err := c.link.ReserveWrite()
	if err != nil {
		return err
	}
	buf := w.Buffer()
	if len(buf) < 16+len(body) {
		_ = w.Abort(0)
		return fmt.Errorf("frame of %d bytes does not fit a buffer of %d", 16+len(body), len(buf))
	}
	buf[0], buf[1] = kind, flags
	le.PutUint16(buf[2:], op)
	le.PutUint32(buf[4:], id)
	le.PutUint32(buf[8:], total)
	le.PutUint32(buf[12:], aux)
	copy(buf[16:], body)
	return w.Commit(16 + len(body))
}

// recv reads one frame within d and fails the test on an error.
func (c *rawConn) recv(d time.Duration) frame {
	c.t.Helper()
	f, err := c.tryRecv(d)
	if err != nil {
		c.t.Fatalf("receiving a frame: %v", err)
	}
	return f
}

func (c *rawConn) tryRecv(d time.Duration) (frame, error) {
	if err := c.link.SetReadDeadline(time.Now().Add(d)); err != nil {
		return frame{}, err
	}
	r, err := c.link.ReserveRead()
	if err != nil {
		return frame{}, err
	}
	b := r.Buffer()
	if len(b) < 16 {
		_ = r.Release()
		return frame{}, fmt.Errorf("frame of %d bytes, shorter than a header", len(b))
	}
	f := frame{
		kind: b[0], flags: b[1], op: le.Uint16(b[2:]), id: le.Uint32(b[4:]),
		total: le.Uint32(b[8:]), aux: le.Uint32(b[12:]),
		body: append([]byte(nil), b[16:]...),
	}
	return f, r.Release()
}

// rawSession is rawOpen followed by an accepted HELLO for microlib.idl.
func rawSession(t testing.TB, s *server, count, size uint32) *rawConn {
	t.Helper()
	c := rawOpen(t, s, count, size, hqq.LinkOptions{})
	rep := c.hello(microlibDigest(t, "microlib.idl"), size, count)
	if rep.kind != kindHelloReply || rep.aux != 0 {
		t.Fatalf("HELLO answered with %v, want HELLO_REPLY status 0", rep)
	}
	return c
}

// addRequest is the body of ml_add(a, b).
func addRequest(a, b int32) []byte {
	body := make([]byte, 8)
	le.PutUint32(body[0:], uint32(a))
	le.PutUint32(body[4:], uint32(b))
	return body
}

// sendAdd commits a REQUEST for ml_add with the given id.
func (c *rawConn) sendAdd(id uint32, a, b int32) {
	c.t.Helper()
	body := addRequest(a, b)
	c.send(kindRequest, 0, opAdd, id, uint32(len(body)), 0, body)
}

// openOnNewControlLink sends payload as BG_OPEN on a control link of its own and returns the answer.
func openOnNewControlLink(t testing.TB, s *server, method uint64, payload []byte) openReply {
	t.Helper()
	ctl := controlLink(t, s)
	defer ctl.Close()
	return callOpen(t, ctl, method, payload)
}

// openRefused asserts the OPEN refusal code of a request on its own control link.
func openRefused(t testing.TB, s *server, method uint64, payload []byte, code uint32) openReply {
	t.Helper()
	r := openOnNewControlLink(t, s, method, payload)
	if r.code != code {
		t.Fatalf("OPEN answered code %d (%q) with name %q, want refusal code %d", r.code, r.msg, r.name, code)
	}
	return r
}
