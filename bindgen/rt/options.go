package rt

import (
	"time"

	hqq "gosuda.org/hqq"
)

// Iface is the static description of a generated interface.
type Iface struct {
	Service string
	Digest  [32]byte
	Ops     []string // Ops[op-1] is the C name of opcode op, used in errors
}

// Options configures Dial. The zero value selects the defaults of section 5.5.
type Options struct {
	BufferCount    int             // 0: 64
	BufferSize     int             // 0: 16384; a multiple of 64, 128..1<<20
	MaxInFlight    int             // 0: BufferCount
	MaxMessage     int             // 0: 16 MiB; at most 1<<30
	ConnectTimeout time.Duration   // 0: 5s; bootstrap and handshake
	CloseTimeout   time.Duration   // 0: 5s; wait for CLOSE_ACK
	CallTimeout    time.Duration   // 0: none; bound on one Wait
	Link           hqq.LinkOptions // session link options; if PeerTimeout is 0, Dial sets it to 2s (0 would mean 5s)
}

// Limits are the values granted by the server in HELLO_REPLY.
type Limits struct {
	BufferCount, BufferSize int
	MaxInFlight, MaxMessage int
	MaxHandles              int
}

const (
	defaultBufferCount    = 64
	defaultBufferSize     = 16384
	defaultMaxMessage     = 16 << 20
	defaultConnectTimeout = 5 * time.Second
	defaultCloseTimeout   = 5 * time.Second
	defaultPeerTimeout    = 2 * time.Second
)

// config is Options with every default applied and every bound checked.
type config struct {
	bufferCount, bufferSize int
	maxInFlight, maxMessage int
	connectTimeout          time.Duration
	closeTimeout            time.Duration
	callTimeout             time.Duration
	link                    hqq.LinkOptions
}

func (o Options) resolve() (config, error) {
	c := config{
		bufferCount:    o.BufferCount,
		bufferSize:     o.BufferSize,
		maxInFlight:    o.MaxInFlight,
		maxMessage:     o.MaxMessage,
		connectTimeout: o.ConnectTimeout,
		closeTimeout:   o.CloseTimeout,
		callTimeout:    o.CallTimeout,
		link:           sessionLinkOptions(o.Link),
	}
	if c.bufferCount == 0 {
		c.bufferCount = defaultBufferCount
	}
	if c.bufferSize == 0 {
		c.bufferSize = defaultBufferSize
	}
	if c.maxInFlight == 0 {
		c.maxInFlight = c.bufferCount
	}
	if c.maxMessage == 0 {
		c.maxMessage = defaultMaxMessage
	}
	if c.connectTimeout == 0 {
		c.connectTimeout = defaultConnectTimeout
	}
	if c.closeTimeout == 0 {
		c.closeTimeout = defaultCloseTimeout
	}
	switch {
	case !validBufferCount(c.bufferCount),
		!validBufferSize(c.bufferSize),
		c.maxInFlight < 1 || c.maxInFlight > c.bufferCount,
		c.maxMessage < minMaxMessage || c.maxMessage > hardMaxMessage,
		c.connectTimeout < 0, c.closeTimeout < 0, c.callTimeout < 0:
		return config{}, ErrInvalidArg
	}
	return c, nil
}

func validBufferCount(n int) bool {
	return n >= minBufferCount && n <= maxBufferCount && n&(n-1) == 0
}

func validBufferSize(n int) bool {
	return n >= minBufferSize && n <= maxBufferSize && n%64 == 0
}

// sessionLinkOptions applies the rules of 4.11 to the caller's link options:
// PeerTimeout is explicit, because 0 would mean 5 s in hqq and the C server
// sets 2 s.
func sessionLinkOptions(o hqq.LinkOptions) hqq.LinkOptions {
	if o.PeerTimeout == 0 {
		o.PeerTimeout = defaultPeerTimeout
	}
	return o
}
