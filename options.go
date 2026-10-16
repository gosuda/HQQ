package hqq

import (
	"time"

	"gosuda.org/hqq/internal/mpmc"
)

// LinkMode represents the role of an endpoint. The first endpoint to open a
// segment becomes the Primary and initializes it; the second attaches as the
// Secondary.
type LinkMode uint64

const (
	LinkModePrimary   LinkMode = iota // Primary mode: initializes shared memory
	LinkModeSecondary                 // Secondary mode: attaches to existing shared memory
)

// String returns "primary" or "secondary".
func (m LinkMode) String() string {
	if m == LinkModeSecondary {
		return "secondary"
	}
	return "primary"
}

// LinkType represents the type of HQQ link.
type LinkType uint64

const (
	LinkTypeStandard LinkType = iota // Standard link with basic functionality
	LinkTypeAdvanced                 // Advanced link with protocol negotiation
)

const (
	// MinBufferCount is the minimum number of ring-slot-owned payload buffers
	// per direction.
	MinBufferCount = 2

	// MinBufferSize is the minimum payload buffer size in bytes.
	MinBufferSize = 8

	// BufferSizeAlignment is the required payload buffer size alignment.
	BufferSizeAlignment = 8
)

// WaitPolicy selects how blocking operations wait for ring space or data.
type WaitPolicy uint8

const (
	// WaitAdaptive (the default) spins briefly, then yields, then parks the
	// waiting thread on a shared wait word (futex on Linux, ulock on macOS,
	// short sleeps elsewhere). Idle links consume almost no CPU; a parked
	// waiter is woken within microseconds of new data.
	WaitAdaptive WaitPolicy = iota

	// WaitSpin never parks. It gives the lowest and most predictable
	// latency, but every blocked call keeps one CPU core fully busy. Use it
	// only on dedicated cores.
	WaitSpin
)

// Default option values.
const (
	DefaultHeartbeatInterval = 250 * time.Millisecond
	DefaultPeerTimeout       = 5 * time.Second
	DefaultAttachTimeout     = 5 * time.Second
)

// LinkOptions configures a StandardLink (and the StandardLink underlying an
// AdvancedLink). The zero value selects the defaults.
type LinkOptions struct {
	// WaitPolicy selects the blocking strategy. Default: WaitAdaptive.
	WaitPolicy WaitPolicy

	// HeartbeatInterval is how often this endpoint publishes a liveness
	// heartbeat in the shared superblock. Zero selects
	// DefaultHeartbeatInterval; a negative value disables heartbeats (the
	// peer then cannot detect a crash of this endpoint, and a segment left
	// behind by it is never considered stale).
	HeartbeatInterval time.Duration

	// PeerTimeout is how long the peer's heartbeat may stall before blocking
	// calls fail with ErrPeerGone. It is raised to at least four peer
	// heartbeat intervals. Zero selects DefaultPeerTimeout.
	PeerTimeout time.Duration

	// AttachTimeout bounds how long Open waits for another endpoint that is
	// initializing the segment. Zero selects DefaultAttachTimeout.
	AttachTimeout time.Duration
}

func (o LinkOptions) normalized() LinkOptions {
	if o.HeartbeatInterval == 0 {
		o.HeartbeatInterval = DefaultHeartbeatInterval
	}
	if o.HeartbeatInterval < 0 {
		o.HeartbeatInterval = 0
	}
	if o.PeerTimeout <= 0 {
		o.PeerTimeout = DefaultPeerTimeout
	}
	if o.AttachTimeout <= 0 {
		o.AttachTimeout = DefaultAttachTimeout
	}
	return o
}

func (p WaitPolicy) ring() mpmc.WaitPolicy {
	if p == WaitSpin {
		return mpmc.WaitSpin
	}
	return mpmc.WaitAdaptive
}

// LargeMessageThreshold returns the largest payload size that fits in one
// Standard Protocol copy packet for a link configured with bufferSize.
// Advanced Protocol callers should treat payload sizes greater than this value
// as large messages that require chunking.
func LargeMessageThreshold(bufferSize int) int {
	if bufferSize < MinBufferSize || bufferSize%BufferSizeAlignment != 0 {
		return 0
	}
	return bufferSize
}

// IsLargeMessage reports whether payloadSize is larger than a single Standard
// Protocol payload buffer for bufferSize.
func IsLargeMessage(bufferSize int, payloadSize int) bool {
	threshold := LargeMessageThreshold(bufferSize)
	return threshold > 0 && payloadSize > threshold
}
