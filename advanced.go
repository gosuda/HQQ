package hqq

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"gosuda.org/hqq/internal/mpmc"
	"gosuda.org/hqq/internal/protocol"
)

// Feature flags for Advanced Protocol negotiation.
const (
	FeatureNone            uint64 = 0
	FeatureLargeCopy       uint64 = 1 << 0 // Enable OpConnCopy chunking for payloads larger than bufferSize.
	FeatureRequestResponse uint64 = 1 << 1 // Enable request-response IPC over OpConnCopy.

	supportedFeatures = FeatureLargeCopy | FeatureRequestResponse
)

// Capability flags for Advanced Protocol negotiation.
const (
	CapabilityNone           uint64 = 0
	CapabilityLargeBuffers   uint64 = 1 << 0 // Endpoint supports large configured payload buffers.
	CapabilityHighThroughput uint64 = 1 << 1 // Endpoint is optimized for high-throughput chunk streams.
	CapabilityLowLatency     uint64 = 1 << 2 // Endpoint is optimized for low-latency request-response calls.

	supportedCapabilities = CapabilityLargeBuffers | CapabilityHighThroughput | CapabilityLowLatency
)

// Advanced Protocol frame flags stored in OpConnCopy operand 5.
const (
	AdvFlagBegin uint64 = 1 << iota
	AdvFlagEnd
	AdvFlagAbort
	AdvFlagRequest
	AdvFlagResponse
	AdvFlagError
)

const (
	// DefaultMaxAdvancedMessageSize bounds full-message assembly when
	// AdvancedOptions.MaxMessageSize is zero.
	DefaultMaxAdvancedMessageSize uint64 = 64 << 20

	advancedKnownFlags = AdvFlagBegin | AdvFlagEnd | AdvFlagAbort | AdvFlagRequest | AdvFlagResponse | AdvFlagError

	// negotiatedBit marks a negotiated link in AdvancedLink.negotiated; the
	// low bits hold the negotiated feature flags.
	negotiatedBit uint64 = 1 << 63

	// maxAbandonedCalls bounds the memory used to recognize late responses
	// to canceled calls.
	maxAbandonedCalls = 4096
)

// ProtocolVersion represents a protocol version using semantic versioning
type ProtocolVersion struct {
	Major uint8 // Major version for incompatible changes
	Minor uint8 // Minor version for backward-compatible additions
}

// CurrentProtocolVersion is the Advanced Protocol version implemented by this
// package.
var CurrentProtocolVersion = ProtocolVersion{
	Major: protocol.HQQProtocolMajorVersion,
	Minor: protocol.HQQProtocolMinorVersion,
}

func (v ProtocolVersion) encode() uint64 {
	return uint64(v.Major)<<8 | uint64(v.Minor)
}

func decodeVersion(v uint64) ProtocolVersion {
	return ProtocolVersion{Major: uint8(v >> 8), Minor: uint8(v)}
}

// ProtocolState represents the state of protocol negotiation
type ProtocolState int32

const (
	ProtocolStateNone        ProtocolState = iota // No negotiation initiated
	ProtocolStateNegotiating                      // Negotiation in progress
	ProtocolStateNegotiated                       // Negotiation completed successfully
	ProtocolStateFailed                           // Negotiation failed
)

// OverflowPolicy selects what the Advanced dispatcher does when the incoming
// queue (messages for Receive, or streams for Accept) is full.
type OverflowPolicy uint8

const (
	// OverflowBlock (the default) applies backpressure: the dispatcher waits
	// for Receive/Accept. While it waits, responses to Call are not routed
	// either, so an application that only calls Call must not let the peer
	// send it unsolicited messages under this policy.
	OverflowBlock OverflowPolicy = iota

	// OverflowDrop discards messages that do not fit (counted in
	// AdvancedStats.DroppedMessages) and refuses streams with OpError. The
	// dispatcher never blocks, so responses are always routed.
	OverflowDrop

	// OverflowFail stops the dispatcher with ErrIncomingOverflow: pending
	// and future calls fail with that error.
	OverflowFail
)

// AdvancedOptions configures an AdvancedLink. The zero value selects the
// defaults.
type AdvancedOptions struct {
	// MaxMessageSize bounds full Advanced message assembly. Zero uses
	// DefaultMaxAdvancedMessageSize.
	MaxMessageSize uint64

	// IncomingQueueSize controls the buffered queue for dispatcher-delivered
	// non-response messages and accepted stream IDs. Zero uses bufferCount.
	IncomingQueueSize int

	// IncomingOverflow selects the behavior when that queue is full.
	IncomingOverflow OverflowPolicy

	// Link configures the underlying StandardLink.
	Link LinkOptions
}

// AdvancedStats contains advanced statistics for the AdvancedLink.
type AdvancedStats struct {
	NegotiationTime    time.Duration
	LargeMessages      uint64
	LargeBytes         uint64
	Requests           uint64
	Responses          uint64
	DroppedMessages    uint64 // messages discarded by OverflowDrop
	AbandonedResponses uint64 // late responses to canceled calls that were discarded
}

type advancedCounters struct {
	negotiationTime    atomic.Int64
	largeMessages      atomic.Uint64
	largeBytes         atomic.Uint64
	requests           atomic.Uint64
	responses          atomic.Uint64
	droppedMessages    atomic.Uint64
	abandonedResponses atomic.Uint64
}

// AdvancedLink implements the Advanced Protocol overlay on top of a
// StandardLink: protocol negotiation, large messages (chunked over
// OpConnCopy), request-response calls and logical streams.
//
// Concurrency: all methods are safe for concurrent use. After negotiation a
// single dispatcher path consumes the receive direction: Receive may drive it
// synchronously, and a background dispatcher is started when concurrent calls
// or streams need asynchronous routing.
type AdvancedLink struct {
	slink *StandardLink // Wrapped standard link

	// Protocol negotiation state
	state                  atomic.Int32  // ProtocolState
	negotiated             atomic.Uint64 // negotiatedBit | features
	protocolMu             sync.RWMutex  // guards the negotiated* fields below
	negotiatedVersion      ProtocolVersion
	negotiatedFeatures     uint64
	negotiatedCapabilities uint64

	messageIDGenerator atomic.Uint64

	// Connection management (guarded by controlMu)
	connectionIDGenerator atomic.Uint64
	controlMu             sync.Mutex
	listening             bool // Whether the link is listening for connections
	connections           map[uint64]*Connection
	pendingConnections    map[uint64]chan error
	acceptCh              chan uint64

	// Receive path
	receiveMu  sync.Mutex
	assemblies map[advancedMessageKey]*advancedAssembly

	maxMessageSize uint64
	overflow       OverflowPolicy

	// Dispatcher
	dispatchMu      sync.Mutex
	closing         bool
	dispatchStarted atomic.Bool
	dispatchCancel  context.CancelFunc
	dispatchDone    chan struct{}
	dispatchErrMu   sync.RWMutex
	dispatchErr     error
	incoming        chan advancedDispatchResult

	// Pending calls (guarded by pendingMu)
	pendingMu      sync.Mutex
	pendingCalls   map[advancedCallKey]chan advancedDispatchResult
	abandoned      map[advancedCallKey]struct{}
	abandonedOrder []advancedCallKey

	stats     advancedCounters
	closeOnce sync.Once
	closeErr  error
}

// Connection represents a logical Advanced stream.
type Connection struct {
	id        uint64
	state     atomic.Uint32 // ConnectionState
	createdAt time.Time
}

// ConnectionState represents the state of a connection
type ConnectionState uint32

const (
	ConnectionStateClosed  ConnectionState = iota // Connection is closed
	ConnectionStateOpening                        // Connection is being established
	ConnectionStateOpen                           // Connection is active
	ConnectionStateClosing                        // Connection is being closed
)

func newConnection(id uint64, state ConnectionState) *Connection {
	c := &Connection{id: id, createdAt: time.Now()}
	c.state.Store(uint32(state))
	return c
}

// NewAdvancedLink creates or attaches to an advanced link in caller-owned
// memory at offset, with default options.
func NewAdvancedLink(offset uintptr, bufferCount int, bufferSize int) (*AdvancedLink, error) {
	return NewAdvancedLinkWithOptions(offset, bufferCount, bufferSize, AdvancedOptions{})
}

// NewAdvancedLinkWithOptions creates or attaches to an advanced link with
// explicit options. See OpenStandardLinkWithOptions for the memory contract.
func NewAdvancedLinkWithOptions(offset uintptr, bufferCount int, bufferSize int, opts AdvancedOptions) (*AdvancedLink, error) {
	slink, err := OpenStandardLinkWithOptions(offset, bufferCount, bufferSize, opts.Link)
	if err != nil {
		return nil, err
	}
	return newAdvancedLink(slink, opts), nil
}

// NewAdvancedLinkMemory creates or attaches to an advanced link in mem; see
// OpenStandardLinkMemory.
func NewAdvancedLinkMemory(mem []byte, bufferCount int, bufferSize int, opts AdvancedOptions) (*AdvancedLink, error) {
	slink, err := OpenStandardLinkMemory(mem, bufferCount, bufferSize, opts.Link)
	if err != nil {
		return nil, err
	}
	return newAdvancedLink(slink, opts), nil
}

func newAdvancedLink(slink *StandardLink, opts AdvancedOptions) *AdvancedLink {
	maxMessageSize := opts.MaxMessageSize
	if maxMessageSize == 0 {
		maxMessageSize = DefaultMaxAdvancedMessageSize
	}
	incomingQueueSize := opts.IncomingQueueSize
	if incomingQueueSize <= 0 {
		incomingQueueSize = slink.bufferCount
	}
	return &AdvancedLink{
		slink:              slink,
		connections:        make(map[uint64]*Connection),
		pendingConnections: make(map[uint64]chan error),
		acceptCh:           make(chan uint64, incomingQueueSize),
		assemblies:         make(map[advancedMessageKey]*advancedAssembly),
		maxMessageSize:     maxMessageSize,
		overflow:           opts.IncomingOverflow,
		dispatchDone:       make(chan struct{}),
		incoming:           make(chan advancedDispatchResult, incomingQueueSize),
		pendingCalls:       make(map[advancedCallKey]chan advancedDispatchResult),
		abandoned:          make(map[advancedCallKey]struct{}),
	}
}

// Read reads Standard Protocol messages. It is only allowed before
// negotiation starts; afterwards it returns ErrAdvancedActive because it
// would steal Advanced frames from the dispatcher.
func (l *AdvancedLink) Read(b []byte) (n int, err error) {
	if ProtocolState(l.state.Load()) != ProtocolStateNone {
		return 0, ErrAdvancedActive
	}
	return l.slink.Read(b)
}

// Write writes a Standard Protocol message. It is only allowed before
// negotiation starts; afterwards it returns ErrAdvancedActive.
func (l *AdvancedLink) Write(b []byte) (n int, err error) {
	if ProtocolState(l.state.Load()) != ProtocolStateNone {
		return 0, ErrAdvancedActive
	}
	return l.slink.Write(b)
}

// Close stops the dispatcher, fails pending calls and dials with ErrClosed,
// and closes the underlying StandardLink. Close is idempotent.
func (l *AdvancedLink) Close() error {
	l.closeOnce.Do(func() {
		l.dispatchMu.Lock()
		l.closing = true
		cancel := l.dispatchCancel
		started := l.dispatchStarted.Load()
		l.dispatchMu.Unlock()

		if cancel != nil {
			cancel()
		}
		l.closeErr = l.slink.Close()
		if started {
			<-l.dispatchDone
		}
		l.failPendingCalls(ErrClosed)
		l.failPendingConnections(ErrClosed)

		l.controlMu.Lock()
		l.listening = false
		for id, conn := range l.connections {
			conn.state.Store(uint32(ConnectionStateClosed))
			delete(l.connections, id)
		}
		l.controlMu.Unlock()
	})
	return l.closeErr
}

// Standard returns the underlying StandardLink (for PeerState, deadlines and
// configuration). Do not read from it after negotiation.
func (l *AdvancedLink) Standard() *StandardLink {
	return l.slink
}

// Mode returns the endpoint role of the underlying link.
func (l *AdvancedLink) Mode() LinkMode {
	return l.slink.Mode()
}

// Type returns LinkTypeAdvanced.
func (l *AdvancedLink) Type() LinkType {
	return LinkTypeAdvanced
}

// GetMode returns the endpoint role of the underlying link.
//
// Deprecated: Use Mode.
func (l *AdvancedLink) GetMode() LinkMode {
	return l.Mode()
}

// GetType returns LinkTypeAdvanced.
//
// Deprecated: Use Type.
func (l *AdvancedLink) GetType() LinkType {
	return l.Type()
}

// PeerState reports what this endpoint knows about its peer.
func (l *AdvancedLink) PeerState() PeerState {
	return l.slink.PeerState()
}

// BufferSize returns the configured payload-buffer size in bytes.
func (l *AdvancedLink) BufferSize() int {
	if l == nil {
		return 0
	}
	return l.slink.BufferSize()
}

// BufferCount returns the configured payload-buffer count per direction.
func (l *AdvancedLink) BufferCount() int {
	if l == nil {
		return 0
	}
	return l.slink.BufferCount()
}

// LargeMessageThreshold returns the largest payload size that fits in one
// Standard Protocol packet before Advanced chunking is required.
func (l *AdvancedLink) LargeMessageThreshold() int {
	if l == nil {
		return 0
	}
	return l.slink.LargeMessageThreshold()
}

// IsLargeMessage reports whether payloadSize should use Advanced chunking.
func (l *AdvancedLink) IsLargeMessage(payloadSize int) bool {
	if l == nil {
		return false
	}
	return l.slink.IsLargeMessage(payloadSize)
}

func orBackground(ctx context.Context) context.Context {
	if ctx == nil {
		return context.Background()
	}
	return ctx
}

// mapCtxErr converts ring wait errors, reporting the context's own error for
// context cancellation.
func mapCtxErr(ctx context.Context, err error) error {
	if err == mpmc.ErrCanceled {
		if ctx != nil && ctx.Err() != nil {
			return ctx.Err()
		}
		return context.Canceled
	}
	return mapWaitErr(err)
}

// sendPacket publishes one control packet.
func (l *AdvancedLink) sendPacket(ctx context.Context, packet protocol.Packet) error {
	s := l.slink
	if err := s.enterTx(true); err != nil {
		return err
	}
	defer s.exitTx()
	c := mpmc.Cancel{Ctx: ctx, Closed: s.closeCh, Check: s.checkFn}
	return mapCtxErr(ctx, s.txRing.EnqueueZeroCopyCancel(&c, func(_ uint64, p *protocol.Packet) {
		*p = packet
	}))
}

// recvPacket consumes one packet by value (used during negotiation, before
// payload-carrying frames are allowed).
func (l *AdvancedLink) recvPacket(ctx context.Context) (protocol.Packet, error) {
	var packet protocol.Packet
	err := l.dequeue(ctx, func(_ uint64, p *protocol.Packet) {
		packet = *p
	})
	return packet, err
}

// dequeue consumes one packet from the receive direction; fn runs while the
// slot is claimed.
func (l *AdvancedLink) dequeue(ctx context.Context, fn func(slot uint64, p *protocol.Packet)) error {
	s := l.slink
	if err := s.enterRx(); err != nil {
		return err
	}
	defer s.exitRx()
	c := mpmc.Cancel{Ctx: ctx, Closed: s.closeCh, Check: s.checkFn}
	err := s.rxRing.DequeueZeroCopyCancel(&c, fn)
	if err == nil {
		return nil
	}
	if err == ErrPeerClosed || err == ErrPeerGone {
		if s.rxRing.DequeueZeroCopyCancel(&mpmc.Cancel{NoWait: true}, fn) == nil {
			return nil
		}
	}
	return mapCtxErr(ctx, err)
}

func (l *AdvancedLink) beginNegotiation() error {
	if !l.state.CompareAndSwap(int32(ProtocolStateNone), int32(ProtocolStateNegotiating)) {
		return ErrAlreadyNegotiated
	}
	return nil
}

func (l *AdvancedLink) setProtocolFailed() {
	l.state.Store(int32(ProtocolStateFailed))
}

func (l *AdvancedLink) setNegotiated(version ProtocolVersion, features uint64, capabilities uint64) {
	l.protocolMu.Lock()
	l.negotiatedVersion = version
	l.negotiatedFeatures = features
	l.negotiatedCapabilities = capabilities
	l.protocolMu.Unlock()
	l.negotiated.Store(negotiatedBit | features)
	l.state.Store(int32(ProtocolStateNegotiated))
}

// Negotiate negotiates the Advanced Protocol with the peer, choosing the
// role automatically: the Primary endpoint sends the request and the
// Secondary answers it. Both endpoints simply call Negotiate. features selects
// the requested features; zero requests all supported features. The result
// is the intersection of both sides' features.
func (l *AdvancedLink) Negotiate(ctx context.Context, features uint64) error {
	if features == FeatureNone {
		features = supportedFeatures
	}
	var ok bool
	var err error
	if l.slink.linkMode == LinkModePrimary {
		ok, err = l.NegotiateProtocol(ctx, CurrentProtocolVersion, features)
	} else {
		ok, err = l.waitForNegotiation(ctx, features)
	}
	if err == nil && !ok {
		err = ErrNegotiationRejected
	}
	return err
}

// NegotiateProtocol sends a negotiation request and waits for the peer's
// acknowledgment. The peer must call WaitForNegotiation, Negotiate, or
// NegotiateProtocol: if both endpoints call NegotiateProtocol at the same
// time, the Primary's request wins and the Secondary answers it.
func (l *AdvancedLink) NegotiateProtocol(ctx context.Context, version ProtocolVersion, features uint64) (bool, error) {
	ctx = orBackground(ctx)
	if err := l.beginNegotiation(); err != nil {
		return false, err
	}
	start := time.Now()
	defer func() { l.stats.negotiationTime.Store(int64(time.Since(start))) }()

	request := protocol.NewPacket(protocol.OpProtoNegotiate, version.encode(), features, supportedCapabilities)
	if err := l.sendPacket(ctx, request); err != nil {
		l.setProtocolFailed()
		return false, err
	}

	for {
		p, err := l.recvPacket(ctx)
		if err != nil {
			l.setProtocolFailed()
			return false, err
		}
		switch p.Op() {
		case protocol.OpStandardLinkTombstone:
			continue
		case protocol.OpProtoAck:
			ackVersion := decodeVersion(p.Operand(0))
			ackFeatures := p.Operand(1)
			ackCapabilities := p.Operand(2)
			if ackVersion.Major != protocol.HQQProtocolMajorVersion ||
				ackVersion.Minor > protocol.HQQProtocolMinorVersion ||
				ackFeatures&^(features&supportedFeatures) != 0 ||
				ackCapabilities&^supportedCapabilities != 0 {
				l.setProtocolFailed()
				return false, ErrNegotiationRejected
			}
			l.setNegotiated(ackVersion, ackFeatures, ackCapabilities)
			return true, nil
		case protocol.OpProtoNegotiate:
			if l.slink.linkMode == LinkModeSecondary {
				// Simultaneous negotiation: the Primary's request wins and
				// the Primary ignores ours, so answer it.
				return l.answerNegotiation(ctx, p, features)
			}
			// We are the Primary: the Secondary will answer our request.
			continue
		case protocol.OpError:
			l.setProtocolFailed()
			return false, ErrNegotiationRejected
		default:
			l.setProtocolFailed()
			return false, ErrUnexpectedPacket
		}
	}
}

// WaitForNegotiation waits for the peer's negotiation request and answers it
// with the intersection of the requested and supported features.
func (l *AdvancedLink) WaitForNegotiation(ctx context.Context) (bool, error) {
	return l.waitForNegotiation(ctx, supportedFeatures)
}

func (l *AdvancedLink) waitForNegotiation(ctx context.Context, allowed uint64) (bool, error) {
	ctx = orBackground(ctx)
	if err := l.beginNegotiation(); err != nil {
		return false, err
	}
	start := time.Now()
	defer func() { l.stats.negotiationTime.Store(int64(time.Since(start))) }()

	for {
		p, err := l.recvPacket(ctx)
		if err != nil {
			l.setProtocolFailed()
			return false, err
		}
		switch p.Op() {
		case protocol.OpStandardLinkTombstone:
			continue
		case protocol.OpProtoNegotiate:
			return l.answerNegotiation(ctx, p, allowed)
		case protocol.OpError:
			l.setProtocolFailed()
			return false, ErrNegotiationRejected
		default:
			l.setProtocolFailed()
			return false, ErrUnexpectedPacket
		}
	}
}

// answerNegotiation acknowledges a negotiation request packet.
func (l *AdvancedLink) answerNegotiation(ctx context.Context, p protocol.Packet, allowed uint64) (bool, error) {
	requested := decodeVersion(p.Operand(0))
	if requested.Major != protocol.HQQProtocolMajorVersion {
		_ = l.sendPacket(ctx, protocol.NewPacket(protocol.OpError, 0, uint64(ErrCodeInvalidOp)))
		l.setProtocolFailed()
		return false, ErrNegotiationRejected
	}
	version := ProtocolVersion{Major: requested.Major, Minor: min(requested.Minor, uint8(protocol.HQQProtocolMinorVersion))}
	features := p.Operand(1) & supportedFeatures & allowed
	capabilities := p.Operand(2) & supportedCapabilities

	ack := protocol.NewPacket(protocol.OpProtoAck, version.encode(), features, capabilities)
	if err := l.sendPacket(ctx, ack); err != nil {
		l.setProtocolFailed()
		return false, err
	}
	l.setNegotiated(version, features, capabilities)
	return true, nil
}

// GetProtocolState returns the current protocol negotiation state.
func (l *AdvancedLink) GetProtocolState() ProtocolState {
	return ProtocolState(l.state.Load())
}

// GetNegotiatedVersion returns the negotiated protocol version.
func (l *AdvancedLink) GetNegotiatedVersion() ProtocolVersion {
	l.protocolMu.RLock()
	defer l.protocolMu.RUnlock()
	return l.negotiatedVersion
}

// GetNegotiatedFeatures returns the negotiated feature flags.
func (l *AdvancedLink) GetNegotiatedFeatures() uint64 {
	l.protocolMu.RLock()
	defer l.protocolMu.RUnlock()
	return l.negotiatedFeatures
}

// GetNegotiatedCapabilities returns the negotiated capability flags.
func (l *AdvancedLink) GetNegotiatedCapabilities() uint64 {
	l.protocolMu.RLock()
	defer l.protocolMu.RUnlock()
	return l.negotiatedCapabilities
}

// IsLargeCopyEnabled returns whether Advanced large-data copy is enabled.
func (l *AdvancedLink) IsLargeCopyEnabled() bool {
	return l.requireFeature(FeatureLargeCopy) == nil
}

// IsRequestResponseEnabled returns whether Advanced request-response IPC is enabled.
func (l *AdvancedLink) IsRequestResponseEnabled() bool {
	return l.requireFeature(FeatureRequestResponse) == nil
}

// GetAdvancedStats returns advanced statistics for the link.
func (l *AdvancedLink) GetAdvancedStats() AdvancedStats {
	return AdvancedStats{
		NegotiationTime:    time.Duration(l.stats.negotiationTime.Load()),
		LargeMessages:      l.stats.largeMessages.Load(),
		LargeBytes:         l.stats.largeBytes.Load(),
		Requests:           l.stats.requests.Load(),
		Responses:          l.stats.responses.Load(),
		DroppedMessages:    l.stats.droppedMessages.Load(),
		AbandonedResponses: l.stats.abandonedResponses.Load(),
	}
}

func (l *AdvancedLink) requireNegotiated() error {
	if l.negotiated.Load()&negotiatedBit == 0 {
		return ErrNotNegotiated
	}
	return nil
}

func (l *AdvancedLink) requireFeature(feature uint64) error {
	n := l.negotiated.Load()
	if n&negotiatedBit == 0 || n&feature == 0 {
		return ErrNotNegotiated
	}
	return nil
}
