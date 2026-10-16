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
	FeatureMessages        uint64 = 1 << 0 // Enable OpConnCopy messages (Stream.Send and Recv), chunked when larger than bufferSize.
	FeatureRequestResponse uint64 = 1 << 1 // Enable request-response IPC over OpConnCopy.

	// FeatureCancel (protocol 1.1) enables OpCancel: a Call whose context
	// ends while it waits tells the peer, which cancels the context of the
	// request's handler.
	FeatureCancel uint64 = 1 << 2

	// FeatureMetadata (protocol 1.1) enables the metadata block of requests
	// (AdvFlagMeta): the caller's remaining time budget, which becomes the
	// deadline of the handler's context, and the optional caller and trace
	// IDs of CallOptions.
	FeatureMetadata uint64 = 1 << 3

	supportedFeatures = FeatureMessages | FeatureRequestResponse | FeatureCancel | FeatureMetadata

	// featuresSince11 are the features that exist only from protocol 1.1.
	featuresSince11 = FeatureCancel | FeatureMetadata
)

// Advanced Protocol frame flags stored in OpConnCopy operand 5.
const (
	AdvFlagBegin uint64 = 1 << iota
	AdvFlagEnd
	AdvFlagAbort
	AdvFlagRequest
	AdvFlagResponse
	AdvFlagError

	// AdvFlagMeta (protocol 1.1, FeatureMetadata) marks a request whose
	// payload starts with a metadata block. It is set on every chunk of the
	// request, like AdvFlagRequest, and is a protocol error on any other
	// frame or when FeatureMetadata was not negotiated.
	AdvFlagMeta
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

	// defaultReassemblyFactor sizes the default
	// AdvancedOptions.MaxReassemblyBytes as a multiple of MaxMessageSize.
	defaultReassemblyFactor = 4

	// advancedAssemblyOverhead is charged against MaxReassemblyBytes for the
	// bookkeeping of every incomplete message, on top of its payload size.
	advancedAssemblyOverhead = 256

	// maxRejectedMessages bounds the memory used to drain the remaining
	// chunks of messages refused by the reassembly limit.
	maxRejectedMessages = 1024
)

// ProtocolVersion represents a protocol version using semantic versioning
type ProtocolVersion struct {
	Major uint8 // Major version for incompatible changes
	Minor uint8 // Minor version for backward-compatible additions
}

// currentProtocolVersion backs CurrentProtocolVersion.
var currentProtocolVersion = ProtocolVersion{
	Major: protocol.HQQProtocolMajorVersion,
	Minor: protocol.HQQProtocolMinorVersion,
}

// CurrentProtocolVersion returns the Advanced Protocol version implemented by
// this package.
func CurrentProtocolVersion() ProtocolVersion {
	return currentProtocolVersion
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

	// ProtocolStateFailed means a negotiation attempt failed: the peer
	// rejected it, the packets were invalid, or the attempt ended with an
	// error such as a canceled context or a closed link. Failed is terminal:
	// every later Negotiate, NegotiateProtocol or WaitForNegotiation call
	// returns ErrNegotiationFailed, and the link cannot be negotiated again.
	// Close it and create a new one.
	ProtocolStateFailed
)

// OverflowPolicy selects what the receive path does when the destination
// queue of a message is full: the queue of a stream (Stream.Recv), the
// request queue (Serve) or the queue of accepted streams (AcceptStream).
type OverflowPolicy uint8

const (
	// OverflowBlock (the default) applies backpressure: the receive path
	// waits until the consumer of that queue (Stream.Recv, Serve or
	// AcceptStream) makes room, and the wait propagates into the
	// shared-memory ring. While it waits nothing else is routed, not even
	// responses to Call, so an application must consume every stream it uses
	// and run Serve if the peer sends requests.
	OverflowBlock OverflowPolicy = iota

	// OverflowDrop discards messages that do not fit (counted in
	// AdvancedStats.DroppedMessages) and refuses streams with OpError. The
	// receive path never blocks, so responses are always routed.
	OverflowDrop

	// OverflowFail stops the receive path with ErrIncomingOverflow: pending
	// and future calls fail with that error.
	OverflowFail
)

// AdvancedOptions configures an AdvancedLink. The zero value selects the
// defaults.
type AdvancedOptions struct {
	// MaxMessageSize bounds full Advanced message assembly. Zero uses
	// DefaultMaxAdvancedMessageSize.
	MaxMessageSize uint64

	// MaxReassemblyBytes bounds the memory held by incomplete multi-chunk
	// messages (a pooled buffer of the announced size, rounded up to a
	// power of two, is taken when the first chunk arrives; each message is
	// charged its announced size plus a small fixed bookkeeping overhead).
	// A message whose first chunk would exceed the bound is refused without
	// allocating: its remaining chunks are drained and discarded, a waiting
	// Call fails with ErrReassemblyLimit, a refused request is answered with
	// an error response (status ErrCodeBufferOverflow), and a refused data
	// message is dropped. Every refused message is counted in
	// AdvancedStats.DroppedMessages.
	// Single-chunk messages are not affected. Zero uses four times
	// MaxMessageSize. A bound below MaxMessageSize refuses large messages
	// even when nothing else is in flight.
	MaxReassemblyBytes uint64

	// IncomingQueueSize is the capacity of every incoming queue: the queue
	// of each stream (messages for Stream.Recv), the request queue consumed
	// by Serve, and the queue of accepted streams. Zero uses bufferCount.
	IncomingQueueSize int

	// IncomingOverflow selects the behavior when the queue a message is
	// routed to is full.
	IncomingOverflow OverflowPolicy

	// Link configures the underlying StandardLink.
	Link LinkOptions
}

// AdvancedStats contains advanced statistics for the AdvancedLink.
type AdvancedStats struct {
	NegotiationTime    time.Duration
	Messages           uint64 // messages sent with Stream.Send
	MessageBytes       uint64 // payload bytes of those messages
	Requests           uint64
	Responses          uint64
	DroppedMessages    uint64 // messages discarded by OverflowDrop, for an unknown or closed stream, as a response without a waiting call, refused by MaxReassemblyBytes, or whose control reply (including an OpCancel) was dropped because the control queue was full
	AbandonedResponses uint64 // late responses to canceled calls that were discarded
	CanceledRequests   uint64 // requests whose handler context was canceled by the peer's OpCancel
	RefusedRequests    uint64 // requests answered with ErrCodeShuttingDown after Shutdown
}

type advancedCounters struct {
	negotiationTime    atomic.Int64
	messages           atomic.Uint64
	messageBytes       atomic.Uint64
	requests           atomic.Uint64
	responses          atomic.Uint64
	droppedMessages    atomic.Uint64
	abandonedResponses atomic.Uint64
	canceledRequests   atomic.Uint64
	refusedRequests    atomic.Uint64

	// Serve execution model (see Serve): handlers handed to worker
	// goroutines, and times the receiver role moved on because an inline
	// handler outlived inlineLimit. Internal, used by tests.
	servePooled      atomic.Uint64
	serveEscalations atomic.Uint64
}

// AdvancedLink implements the Advanced Protocol overlay on top of a
// StandardLink: protocol negotiation, messages of up to MaxMessageSize
// (chunked over OpConnCopy), request-response calls and logical streams.
//
// Messages travel on Streams. The link itself is stream 0: Send, Recv and
// Call are shortcuts for l.Stream().Send, Recv and Call, and OpenStream and
// AcceptStream create further streams. Received messages are pooled: see
// Message.
//
// Concurrency: all methods are safe for concurrent use. After negotiation a
// single receive path consumes the receive direction and routes every
// complete message: data to its stream's queue, requests to the request
// queue consumed by Serve, responses to the waiting Call. A goroutine that
// finds no dispatcher may receive synchronously (lowest latency) and route
// messages for others; a background dispatcher is started as soon as
// several goroutines wait concurrently or streams are used. The receive path
// never waits for the tx ring: the replies it generates (stream accepts and
// refusals, error responses to requests refused by MaxReassemblyBytes or by
// Shutdown, and OpCancel frames of canceled calls) are sent by a separate
// control-sender goroutine through a bounded queue; a reply that does not fit
// is dropped and counted in AdvancedStats.DroppedMessages.
//
// Protocol 1.1 (FeatureCancel, FeatureMetadata): a Call whose context has a
// deadline sends the remaining time with the request, and a Call whose
// context ends while it waits tells the peer (OpCancel); Serve hands every
// handler a context of its own that reflects both, the closing of its stream
// and the end of Serve (see Handler). CallOptions attach a caller and trace
// ID. Shutdown stops accepting requests and drains the running ones. Peers
// that negotiate only the 1.0 features see the 1.0 behavior.
type AdvancedLink struct {
	slink *StandardLink // Wrapped standard link

	// Protocol negotiation state
	state              atomic.Int32  // ProtocolState
	negotiated         atomic.Uint64 // negotiatedBit | features
	protocolMu         sync.RWMutex  // guards the negotiated* fields below
	negotiatedVersion  ProtocolVersion
	negotiatedFeatures uint64

	messageIDGenerator atomic.Uint64

	// Streams (guarded by controlMu). stream0 is the always-open link-level
	// stream; streams holds the open and opening streams with a non-zero ID.
	connectionIDGenerator atomic.Uint64
	controlMu             sync.Mutex
	listening             bool // Whether the link is listening for connections
	stream0               *Stream
	streams               map[uint64]*Stream
	pendingConnections    map[uint64]chan error
	acceptCh              chan *Stream
	// acceptSlots reserves room in acceptCh for streams whose accept reply is
	// queued on the control sender: a slot is taken when OpConnCreate is
	// received and released by AcceptStream (or when the accept fails).
	acceptSlots chan struct{}

	// Receive path
	receiveMu  sync.Mutex
	assemblies map[advancedMessageKey]*advancedAssembly
	// assemblyBytes is the memory charged by assemblies against
	// maxReassemblyBytes. rejected remembers (FIFO, at most
	// maxRejectedMessages) the messages refused by that limit whose remaining
	// chunks must be discarded. All of these are guarded by receiveMu.
	assemblyBytes      uint64
	maxReassemblyBytes uint64
	rejected           map[advancedMessageKey]struct{}
	rejectedOrder      []advancedMessageKey

	maxMessageSize uint64
	overflow       OverflowPolicy
	queueSize      int      // capacity of every incoming queue
	pool           *msgPool // buffers of received messages and responses

	// lifeCtx is canceled by Close; the dispatcher, the control sender and
	// deliveries handed over by a synchronous receiver run under it.
	lifeCtx    context.Context
	lifeCancel context.CancelFunc

	// Dispatcher
	dispatchMu      sync.Mutex
	closing         bool
	dispatchStarted atomic.Bool
	dispatchDone    chan struct{}
	dispatchErrMu   sync.RWMutex
	dispatchErr     error
	requests        chan *Message // requests, consumed only by Serve

	// Control sender: sends the replies generated by the receive path (see
	// controlJob). ctrlQueue and ctrlStarted's transition are guarded by
	// dispatchMu; ctrlQueue is read lock-free once ctrlStarted.
	ctrlStarted atomic.Bool
	ctrlQueue   chan controlJob
	ctrlDone    chan struct{}

	// Pending calls (guarded by pendingMu)
	pendingMu      sync.Mutex
	pendingCalls   map[advancedCallKey]chan advancedDispatchResult
	abandoned      map[advancedCallKey]struct{}
	abandonedOrder []advancedCallKey

	// Requests received and not yet answered, with the context of each (see
	// reqCtx). inflightN counts them; shuttingDown, drained and serveStop
	// implement Shutdown.
	inflightMu    sync.Mutex
	inflight      map[advancedCallKey]*reqCtx
	inflightN     atomic.Int64
	shuttingDown  atomic.Bool
	drained       chan struct{} // capacity 1: the last in-flight request ended
	serveStopped  atomic.Bool   // Shutdown completed: Serve returns nil
	serveStop     chan struct{} // closed with serveStopped
	serveStopOnce sync.Once
	serveWakeFn   func() time.Time // ring DeadlineFunc of receivers that serve

	stats     advancedCounters
	closeOnce sync.Once
	closeErr  error
}

// NewAdvancedLink creates or attaches to an Advanced link in mem. mem must be
// page-aligned and at least SizeStandardLink(bufferCount, bufferSize) bytes
// long; it is typically a shared mapping, or AlignedBuffer for links between
// goroutines of one process. See NewStandardLink for the endpoint roles.
func NewAdvancedLink(mem []byte, bufferCount int, bufferSize int, opts AdvancedOptions) (*AdvancedLink, error) {
	slink, err := NewStandardLink(mem, bufferCount, bufferSize, opts.Link)
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
	maxReassemblyBytes := opts.MaxReassemblyBytes
	if maxReassemblyBytes == 0 {
		maxReassemblyBytes = maxMessageSize * defaultReassemblyFactor
		if maxReassemblyBytes/defaultReassemblyFactor != maxMessageSize {
			maxReassemblyBytes = ^uint64(0)
		}
	}
	incomingQueueSize := opts.IncomingQueueSize
	if incomingQueueSize <= 0 {
		incomingQueueSize = slink.bufferCount
	}
	l := &AdvancedLink{
		slink:              slink,
		streams:            make(map[uint64]*Stream),
		pendingConnections: make(map[uint64]chan error),
		acceptCh:           make(chan *Stream, incomingQueueSize),
		acceptSlots:        make(chan struct{}, incomingQueueSize),
		assemblies:         make(map[advancedMessageKey]*advancedAssembly),
		maxMessageSize:     maxMessageSize,
		maxReassemblyBytes: maxReassemblyBytes,
		overflow:           opts.IncomingOverflow,
		queueSize:          incomingQueueSize,
		pool:               newMsgPool(maxMessageSize),
		dispatchDone:       make(chan struct{}),
		ctrlDone:           make(chan struct{}),
		requests:           make(chan *Message, incomingQueueSize),
		pendingCalls:       make(map[advancedCallKey]chan advancedDispatchResult),
		abandoned:          make(map[advancedCallKey]struct{}),
		inflight:           make(map[advancedCallKey]*reqCtx),
		drained:            make(chan struct{}, 1),
		serveStop:          make(chan struct{}),
	}
	l.serveWakeFn = l.serveWakeDeadline
	l.lifeCtx, l.lifeCancel = context.WithCancel(context.Background())
	l.stream0 = newStream(l, 0)
	return l
}

// Close stops the dispatcher and the control sender, fails pending calls and
// dials with ErrClosed, and closes the underlying StandardLink. Control
// replies still queued are discarded. Receive operations blocked on the link
// or on one of its streams return ErrClosed. The contexts of the handlers that
// are running are canceled at once and Serve returns ErrClosed; Close does not
// wait for the handlers and does not drain: use Shutdown before Close for a
// graceful stop. A Shutdown that waits returns ErrClosed. Close is idempotent.
func (l *AdvancedLink) Close() error {
	l.closeOnce.Do(func() {
		l.dispatchMu.Lock()
		l.closing = true
		started := l.dispatchStarted.Load()
		ctrlStarted := l.ctrlStarted.Load()
		l.dispatchMu.Unlock()

		l.lifeCancel()
		l.cancelAllRequests()
		l.closeErr = l.slink.Close()
		if started {
			<-l.dispatchDone
		}
		if ctrlStarted {
			<-l.ctrlDone
		}
		l.failPendingCalls(ErrClosed)
		l.failPendingConnections(ErrClosed)

		l.controlMu.Lock()
		l.listening = false
		clear(l.streams)
		l.controlMu.Unlock()
	})
	return l.closeErr
}

// Mode returns the endpoint role of the underlying link.
func (l *AdvancedLink) Mode() LinkMode {
	return l.slink.Mode()
}

// Type returns LinkTypeAdvanced.
func (l *AdvancedLink) Type() LinkType {
	return LinkTypeAdvanced
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

// ringCancel returns the ring wait conditions for an operation bounded by
// ctx. Waits under the link's own lifetime context (the dispatcher, the
// control sender) leave Ctx unset: Close closes the Standard link, whose
// Closed channel already wakes them, and a cancelable Ctx would arm a
// context wake-up (and allocate) every time such a wait parks.
func (l *AdvancedLink) ringCancel(ctx context.Context) mpmc.Cancel {
	s := l.slink
	c := mpmc.Cancel{Closed: s.closeCh, Check: s.checkFn}
	if ctx != l.lifeCtx {
		c.Ctx = ctx
	}
	return c
}

// sendPacket publishes one control packet.
func (l *AdvancedLink) sendPacket(ctx context.Context, packet protocol.Packet) error {
	s := l.slink
	if err := s.enterTx(true); err != nil {
		return err
	}
	defer s.exitTx()
	c := l.ringCancel(ctx)
	return mapCtxErr(ctx, s.txRing.EnqueueZeroCopyCancel(&c, func(_ uint64, p *protocol.Packet) {
		*p = packet
	}))
}

// recvPacket consumes one packet by value (used during negotiation, before
// payload-carrying frames are allowed).
func (l *AdvancedLink) recvPacket(ctx context.Context) (protocol.Packet, error) {
	var packet protocol.Packet
	err := l.dequeue(ctx, false, func(_ uint64, p *protocol.Packet) {
		packet = *p
	})
	return packet, err
}

// dequeue consumes one packet from the receive direction; fn runs while the
// slot is claimed.
//
// serving marks the receive of Serve: it ends once Shutdown completed.
func (l *AdvancedLink) dequeue(ctx context.Context, serving bool, fn func(slot uint64, p *protocol.Packet)) error {
	s := l.slink
	if err := s.enterRx(); err != nil {
		return err
	}
	defer s.exitRx()
	c := l.ringCancel(ctx)
	if serving {
		c.DeadlineFunc = l.serveWakeFn
	}
	if err := dequeueDrained(s.rxRing, &c, fn); err != nil {
		return mapCtxErr(ctx, err)
	}
	return nil
}

// beginNegotiation moves None to Negotiating. Any other state is an error:
// ErrNegotiationFailed after a failed attempt (terminal), ErrAlreadyNegotiated
// while an attempt is in progress or after it succeeded.
func (l *AdvancedLink) beginNegotiation() error {
	if l.state.CompareAndSwap(int32(ProtocolStateNone), int32(ProtocolStateNegotiating)) {
		return nil
	}
	if ProtocolState(l.state.Load()) == ProtocolStateFailed {
		return ErrNegotiationFailed
	}
	return ErrAlreadyNegotiated
}

func (l *AdvancedLink) setProtocolFailed() {
	l.state.Store(int32(ProtocolStateFailed))
}

func (l *AdvancedLink) setNegotiated(version ProtocolVersion, features uint64) {
	l.protocolMu.Lock()
	l.negotiatedVersion = version
	l.negotiatedFeatures = features
	l.protocolMu.Unlock()
	l.negotiated.Store(negotiatedBit | features)
	l.state.Store(int32(ProtocolStateNegotiated))
}

// Negotiate negotiates the Advanced Protocol with the peer, choosing the
// role automatically: the Primary endpoint sends the request and the
// Secondary answers it. Both endpoints simply call Negotiate. features selects
// the requested features; zero requests all supported features. The result
// is the intersection of both sides' features.
//
// A link is negotiated at most once: Negotiate returns ErrAlreadyNegotiated
// while a negotiation is in progress or after it succeeded, and
// ErrNegotiationFailed after a failed attempt (ProtocolStateFailed is
// terminal).
func (l *AdvancedLink) Negotiate(ctx context.Context, features uint64) error {
	if features == FeatureNone {
		features = supportedFeatures
	}
	var ok bool
	var err error
	if l.slink.linkMode == LinkModePrimary {
		ok, err = l.NegotiateProtocol(ctx, currentProtocolVersion, features)
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
//
// It returns ErrAlreadyNegotiated if a negotiation is in progress or
// succeeded, and ErrNegotiationFailed if an earlier attempt failed. Any
// error from the attempt itself (including a canceled ctx) leaves the link
// in the terminal ProtocolStateFailed.
func (l *AdvancedLink) NegotiateProtocol(ctx context.Context, version ProtocolVersion, features uint64) (bool, error) {
	ctx = orBackground(ctx)
	if err := l.beginNegotiation(); err != nil {
		return false, err
	}
	start := time.Now()
	defer func() { l.stats.negotiationTime.Store(int64(time.Since(start))) }()

	request := protocol.NewPacket(protocol.OpProtoNegotiate, version.encode(), features, 0)
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
			if ackVersion.Major != protocol.HQQProtocolMajorVersion ||
				ackVersion.Minor > protocol.HQQProtocolMinorVersion ||
				ackFeatures&^(features&supportedFeatures) != 0 {
				l.setProtocolFailed()
				return false, ErrNegotiationRejected
			}
			l.setNegotiated(ackVersion, ackFeatures)
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
// with the intersection of the requested and supported features. Its
// result codes and failure semantics are those of NegotiateProtocol.
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
	if version.Minor < 1 {
		features &^= featuresSince11
	}

	ack := protocol.NewPacket(protocol.OpProtoAck, version.encode(), features, 0)
	if err := l.sendPacket(ctx, ack); err != nil {
		l.setProtocolFailed()
		return false, err
	}
	l.setNegotiated(version, features)
	return true, nil
}

// ProtocolState returns the current protocol negotiation state.
func (l *AdvancedLink) ProtocolState() ProtocolState {
	return ProtocolState(l.state.Load())
}

// NegotiatedVersion returns the negotiated protocol version.
func (l *AdvancedLink) NegotiatedVersion() ProtocolVersion {
	l.protocolMu.RLock()
	defer l.protocolMu.RUnlock()
	return l.negotiatedVersion
}

// NegotiatedFeatures returns the negotiated feature flags.
func (l *AdvancedLink) NegotiatedFeatures() uint64 {
	l.protocolMu.RLock()
	defer l.protocolMu.RUnlock()
	return l.negotiatedFeatures
}

// Stats returns advanced statistics for the link.
func (l *AdvancedLink) Stats() AdvancedStats {
	return AdvancedStats{
		NegotiationTime:    time.Duration(l.stats.negotiationTime.Load()),
		Messages:           l.stats.messages.Load(),
		MessageBytes:       l.stats.messageBytes.Load(),
		Requests:           l.stats.requests.Load(),
		Responses:          l.stats.responses.Load(),
		DroppedMessages:    l.stats.droppedMessages.Load(),
		AbandonedResponses: l.stats.abandonedResponses.Load(),
		CanceledRequests:   l.stats.canceledRequests.Load(),
		RefusedRequests:    l.stats.refusedRequests.Load(),
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
