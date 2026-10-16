package hqq

import (
	"math/bits"
	"sync"
)

// minMessageClass is the size class (log2) of the smallest pooled buffer:
// 64 bytes.
const minMessageClass = 6

// advHeader is the header announced by the first OpConnCopy frame of a
// message.
type advHeader struct {
	streamID  uint64 // logical stream, 0 for link-level traffic
	messageID uint64 // sender-assigned; a response reuses its request's ID
	flags     uint64 // AdvFlag* bits
	aux       uint64 // method of a request, status of a response
}

func (h advHeader) isRequest() bool  { return h.flags&AdvFlagRequest != 0 }
func (h advHeader) isResponse() bool { return h.flags&AdvFlagResponse != 0 }
func (h advHeader) isError() bool    { return h.flags&AdvFlagError != 0 }
func (h advHeader) isAbort() bool    { return h.flags&AdvFlagAbort != 0 }

// Message is one complete message received from a Stream (Stream.Recv) or
// the response to a Stream.Call. Its buffer comes from a pool owned by the
// link.
//
// Bytes is valid until Release. Release returns the buffer to the pool: it
// must be called at most once, and neither the Message nor a slice obtained
// from Bytes may be used afterwards. Not calling Release is safe: the garbage
// collector reclaims the buffer, the pool just loses the chance to reuse it.
// A Message must not be copied.
type Message struct {
	advHeader
	data  []byte // backing buffer, len == cap == capacity of the size class
	off   int    // start of the payload in data (after a metadata block), off+n <= len(data)
	n     int    // payload length
	class uint8  // size class index in pool.classes
	pool  *msgPool

	// Request state, set by the receive path for requests only.
	meta msgMeta
	rc   *reqCtx // context of the request, released with the message
}

// Bytes returns the message payload. The slice is valid until Release and
// has no spare capacity, so appending to it never overwrites the buffer.
func (m *Message) Bytes() []byte {
	return m.data[m.off : m.off+m.n : m.off+m.n]
}

// Len returns the payload length in bytes.
func (m *Message) Len() int {
	return m.n
}

// StreamID returns the ID of the stream the message belongs to (0 for the
// link-level stream).
func (m *Message) StreamID() uint64 {
	return m.streamID
}

// Release returns the message and its buffer to the pool. It must be called
// at most once; using m or Bytes() afterwards is invalid. Release on a nil
// message is a no-op.
func (m *Message) Release() {
	if m == nil {
		return
	}
	m.advHeader = advHeader{}
	m.off, m.n = 0, 0
	if m.meta.present {
		m.meta = msgMeta{}
	}
	if m.rc != nil {
		rc := m.rc
		m.rc = nil
		rc.release()
	}
	m.pool.put(m)
}

// msgPool recycles Messages in power-of-two size classes. Only pointers to
// Messages are pooled (a slice header put into a sync.Pool allocates). The
// largest class holds exactly maxSize bytes, so the pool never keeps a
// buffer larger than MaxMessageSize; sync.Pool drops idle entries on every
// garbage collection, so the pool does not retain memory indefinitely.
type msgPool struct {
	maxSize uint64
	classes []sync.Pool // index c holds buffers of class minMessageClass+c
}

func newMsgPool(maxSize uint64) *msgPool {
	p := &msgPool{maxSize: maxSize}
	top := classOf(min(maxSize, uint64(^uint(0)>>1)))
	p.classes = make([]sync.Pool, top+1)
	for c := range p.classes {
		size := p.classSize(c)
		class := uint8(c)
		p.classes[c].New = func() any {
			return &Message{data: make([]byte, size), class: class, pool: p}
		}
	}
	return p
}

// classOf returns the index of the smallest class that holds n bytes.
func classOf(n uint64) int {
	if n <= 1<<minMessageClass {
		return 0
	}
	return bits.Len64(n-1) - minMessageClass
}

// classSize is the buffer size of class c: a power of two, capped at
// maxSize.
func (p *msgPool) classSize(c int) uint64 {
	return min(uint64(1)<<(c+minMessageClass), p.maxSize)
}

// get returns an empty message whose buffer holds at least n bytes
// (n <= maxSize) with payload length n.
func (p *msgPool) get(n int) *Message {
	m := p.classes[classOf(uint64(n))].Get().(*Message)
	m.n = n
	return m
}

func (p *msgPool) put(m *Message) {
	p.classes[m.class].Put(m)
}
