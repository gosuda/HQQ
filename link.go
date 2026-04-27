package hqq

import (
	"context"
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"

	"gosuda.org/hqq/internal/mpmc"
	"gosuda.org/hqq/internal/protocol"
	"gosuda.org/hqq/internal/race"
)

// pagesize stores the system page size for memory alignment
var pagesize = uintptr(syscall.Getpagesize())

// LinkMode represents the mode of the HQQ link
// Primary mode initializes the shared memory region, while Secondary mode attaches to it
type LinkMode uint64

const (
	LinkModePrimary   LinkMode = iota // Primary mode: initializes shared memory
	LinkModeSecondary                 // Secondary mode: attaches to existing shared memory
)

// LinkType represents the type of HQQ link
type LinkType uint64

const (
	LinkTypeStandard LinkType = iota // Standard link with basic functionality
	LinkTypeAdvanced                 // Advanced link with protocol negotiation
)

// Feature flags for protocol negotiation
// These flags are used during protocol negotiation to enable optional features
const (
	FeatureNone        uint64 = 0
	FeatureCompression uint64 = 1 << iota // Enable data compression
	FeatureEncryption                     // Enable data encryption
	FeatureFlowControl                    // Enable advanced flow control
	FeatureStatistics                     // Enable enhanced statistics
	FeatureQoS                            // Enable quality of service features
)

// Capability flags for protocol negotiation
// These flags indicate system capabilities during negotiation
const (
	CapabilityNone             uint64 = 0
	CapabilityLargeBuffers     uint64 = 1 << iota // Support for large buffers
	CapabilityHighThroughput                      // High throughput mode
	CapabilityLowLatency                          // Low latency optimizations
	CapabilityReliableDelivery                    // Reliable delivery guarantees
)

// ProtocolVersion represents a protocol version using semantic versioning
type ProtocolVersion struct {
	Major uint8 // Major version for incompatible changes
	Minor uint8 // Minor version for backward-compatible additions
}

// ProtocolState represents the state of protocol negotiation
type ProtocolState int

const (
	ProtocolStateNone        ProtocolState = iota // No negotiation initiated
	ProtocolStateNegotiating                      // Negotiation in progress
	ProtocolStateNegotiated                       // Negotiation completed successfully
	ProtocolStateFailed                           // Negotiation failed
)

// ErrorCode represents HQQ-specific error codes used in error packets
type ErrorCode uint64

const (
	ErrCodeMemoryAlign    ErrorCode = 0x01 // Memory alignment violation
	ErrCodeInvalidSize    ErrorCode = 0x02 // Invalid buffer ring size
	ErrCodeMemorySmall    ErrorCode = 0x03 // Memory too small for configuration
	ErrCodeFailedInit     ErrorCode = 0x04 // Failed to initialize buffer ring
	ErrCodeConnNotFound   ErrorCode = 0x05 // Connection not found
	ErrCodeInvalidOp      ErrorCode = 0x06 // Invalid operation
	ErrCodeBufferOverflow ErrorCode = 0x07 // Buffer overflow detected
)

// Error definitions for HQQ operations
var (
	ErrMemoryAlign    = errors.New("hqq: memory alignment violation")
	ErrInvalidSize    = errors.New("hqq: invalid buffer ring size")
	ErrMemorySmall    = errors.New("hqq: memory too small")
	ErrFailedInit     = errors.New("hqq: failed to initialize buffer ring")
	ErrConnNotFound   = errors.New("hqq: connection not found")
	ErrInvalidOp      = errors.New("hqq: invalid operation")
	ErrBufferOverflow = errors.New("hqq: buffer overflow")
	ErrTimeout        = errors.New("hqq: operation timed out")
)

// StandardLink represents a standard HQQ link for inter-process communication
// It provides basic IPC functionality using shared memory and MPMC queues
type StandardLink struct {
	// Configuration
	linkMode    LinkMode // Primary or Secondary mode
	linkType    LinkType // Standard or Advanced type
	bufferCount int      // Number of buffers in each pool
	bufferSize  int      // Size of each buffer in bytes

	// MPMC Rings for bidirectional communication
	ring0          *mpmc.MPMCRing[protocol.Packet] // Primary to Secondary data ring
	ring1          *mpmc.MPMCRing[protocol.Packet] // Secondary to Primary data ring
	free0          *mpmc.MPMCRing[uint64]          // Free buffers for primary-to-secondary direction
	free1          *mpmc.MPMCRing[uint64]          // Free buffers for secondary-to-primary direction
	buffers0Offset uintptr                         // Offset to primary-to-secondary buffers
	buffers1Offset uintptr                         // Offset to secondary-to-primary buffers

	// Buffer pools for data storage
	buffers0 [][]byte // Primary to Secondary buffer pool
	buffers1 [][]byte // Secondary to Primary buffer pool

	// State management
	idGenerator   atomic.Uint64 // Generates unique IDs for copies
	receiveBuffer []byte        // Buffer for storing excess received data

	// PacketConn interface support
	localAddr     net.Addr  // Local address for PacketConn interface
	remoteAddr    net.Addr  // Remote address for PacketConn interface
	readDeadline  time.Time // Deadline for PacketConn read operations
	writeDeadline time.Time // Deadline for PacketConn write operations
}

// Connection represents an active connection between processes
type Connection struct {
	id        uint64          // Unique connection identifier
	state     ConnectionState // Current connection state
	createdAt time.Time       // Connection creation time
	lastUsed  time.Time       // Last activity time
}

// ConnectionState represents the state of a connection
type ConnectionState uint32

const (
	ConnectionStateClosed  ConnectionState = iota // Connection is closed
	ConnectionStateOpening                        // Connection is being established
	ConnectionStateOpen                           // Connection is active
	ConnectionStateClosing                        // Connection is being closed
)

// Standard Link Memory Layout:
//
// The shared memory region is organized as follows:
//
// <<<< PAGE_START
// MPMC_RING (Primary to Secondary)     // Ring buffer for packets from primary to secondary
// <<<< PAGE_BREAK
// FREE_RING (Primary to Secondary)     // Buffer ownership ring for primary-to-secondary buffers
// <<<< PAGE_BREAK
// BUFFERS (Primary to Secondary)       // Data buffers for primary to secondary communication
// <<<< PAGE_BREAK
// MPMC_RING (Secondary to Primary)     // Ring buffer for packets from secondary to primary
// <<<< PAGE_BREAK
// FREE_RING (Secondary to Primary)     // Buffer ownership ring for secondary-to-primary buffers
// <<<< PAGE_BREAK
// BUFFERS (Secondary to Primary)       // Data buffers for secondary to primary communication
// <<<< PAGE_END

// SizeStandardLink calculates the required memory size for a standard link
// This function determines the total shared memory size needed for the link configuration
func SizeStandardLink(bufferCount int, bufferSize int) uintptr {
	// Validate inputs
	if bufferCount <= 0 || bufferSize <= 0 {
		return 0
	}

	if !isPowerOfTwo(bufferCount) {
		return 0
	}

	if bufferCount < 2 || bufferSize < 8 || bufferSize%8 != 0 {
		return 0
	}

	dataRingSize := mpmc.SizeMPMCRing[protocol.Packet](uintptr(bufferCount))
	freeRingSize := mpmc.SizeMPMCRing[uint64](uintptr(bufferCount))
	buffersSize := uintptr(bufferSize) * uintptr(bufferCount)

	var size uintptr
	size = alignPage(size + dataRingSize)
	size = alignPage(size + freeRingSize)
	size = alignPage(size + buffersSize)
	size = alignPage(size + dataRingSize)
	size = alignPage(size + freeRingSize)
	size = alignPage(size + buffersSize)

	return size
}

func alignPage(size uintptr) uintptr {
	return ((size + pagesize - 1) / pagesize) * pagesize
}

func isPowerOfTwo(v int) bool {
	return v > 0 && v&(v-1) == 0
}

// OpenStandardLink creates or attaches to a standard link
// This function initializes a StandardLink instance in shared memory
//
//go:nocheckptr
func OpenStandardLink(offset uintptr, bufferCount int, bufferSize int) (*StandardLink, error) {
	// Validate inputs
	if offset%pagesize != 0 || bufferSize%8 != 0 {
		return nil, ErrMemoryAlign
	}

	if !isPowerOfTwo(bufferCount) {
		return nil, ErrInvalidSize
	}

	if bufferCount < 2 || bufferSize < 8 {
		return nil, ErrInvalidSize
	}

	// Validate memory size
	requiredSize := SizeStandardLink(bufferCount, bufferSize)
	if requiredSize == 0 {
		return nil, ErrInvalidSize
	}

	// Initialize link structure
	link := &StandardLink{
		linkMode:      LinkModeSecondary,
		linkType:      LinkTypeStandard,
		bufferCount:   bufferCount,
		bufferSize:    bufferSize,
		receiveBuffer: make([]byte, 0, bufferSize),
		localAddr:     &HQQAddr{LinkID: "hqq-local"},
		remoteAddr:    &HQQAddr{LinkID: "hqq-remote"},
	}

	// Calculate memory layout
	dataRingSize := mpmc.SizeMPMCRing[protocol.Packet](uintptr(bufferCount))
	freeRingSize := mpmc.SizeMPMCRing[uint64](uintptr(bufferCount))
	buffersSize := uintptr(bufferCount) * uintptr(bufferSize)

	// Initialize or attach to first ring (Primary to Secondary)
	ring0Offset := offset
	if mpmc.MPMCInit[protocol.Packet](offset, uint64(bufferCount)) {
		link.linkMode = LinkModePrimary
	}
	offset = alignPage(offset + dataRingSize)

	free0Offset := offset
	free0Initialized := mpmc.MPMCInit[uint64](offset, uint64(bufferCount))
	offset = alignPage(offset + freeRingSize)

	// Setup first buffer pool
	link.buffers0Offset = offset
	offset = alignPage(offset + buffersSize)

	// Initialize or attach to second ring (Secondary to Primary)
	ring1Offset := offset
	mpmc.MPMCInit[protocol.Packet](offset, uint64(bufferCount))
	offset = alignPage(offset + dataRingSize)

	free1Offset := offset
	free1Initialized := mpmc.MPMCInit[uint64](offset, uint64(bufferCount))
	offset = alignPage(offset + freeRingSize)

	// Setup second buffer pool
	link.buffers1Offset = offset
	offset = alignPage(offset + buffersSize)

	// Attach to rings
	link.ring0 = mpmc.MPMCAttach[protocol.Packet](ring0Offset, time.Second)
	link.ring1 = mpmc.MPMCAttach[protocol.Packet](ring1Offset, time.Second)
	link.free0 = mpmc.MPMCAttach[uint64](free0Offset, time.Second)
	link.free1 = mpmc.MPMCAttach[uint64](free1Offset, time.Second)
	if link.ring0 == nil || link.ring1 == nil || link.free0 == nil || link.free1 == nil {
		return nil, ErrFailedInit
	}

	// Setup buffer slices for easy access
	buffers0 := unsafe.Slice((*byte)(unsafe.Pointer(link.buffers0Offset)), buffersSize)
	buffers1 := unsafe.Slice((*byte)(unsafe.Pointer(link.buffers1Offset)), buffersSize)

	// Initialize buffers to zero if we're the primary process
	if link.linkMode == LinkModePrimary {
		race.Zero(buffers0)
		race.Zero(buffers1)
	}

	// Populate free-buffer rings exactly once per shared memory region. Data
	// rings provide packet backpressure; free rings provide payload-buffer
	// ownership so unread buffers are never overwritten.
	if free0Initialized {
		for i := 0; i < bufferCount; i++ {
			link.free0.Enqueue(uint64(i))
		}
	}
	if free1Initialized {
		for i := 0; i < bufferCount; i++ {
			link.free1.Enqueue(uint64(i))
		}
	}

	// Create buffer slices for indexed access
	link.buffers0 = make([][]byte, bufferCount)
	link.buffers1 = make([][]byte, bufferCount)
	for i := 0; i < bufferCount; i++ {
		start := i * bufferSize
		end := (i + 1) * bufferSize
		link.buffers0[i] = buffers0[start:end:end]
		link.buffers1[i] = buffers1[start:end:end]
	}

	return link, nil
}

// Read implements io.Reader for the link
// It reads data from the link into the provided buffer
func (l *StandardLink) Read(b []byte) (n int, err error) {
	bN := len(b)
	bOffset := 0

	// Determine which ring and buffers to use based on link mode
	var rx *mpmc.MPMCRing[protocol.Packet]
	var rxBuffers [][]byte
	var rxFree *mpmc.MPMCRing[uint64]

	if l.linkMode == LinkModeSecondary {
		// Secondary reads from ring0 (Primary to Secondary)
		rx = l.ring0
		rxBuffers = l.buffers0
		rxFree = l.free0
	} else {
		// Primary reads from ring1 (Secondary to Primary)
		rx = l.ring1
		rxBuffers = l.buffers1
		rxFree = l.free1
	}

	// First, consume any remaining data in receiveBuffer
	if len(l.receiveBuffer) > 0 {
		copied := copy(b, l.receiveBuffer)
		bOffset += copied
		l.receiveBuffer = l.receiveBuffer[copied:]
		if bOffset > 0 {
			return bOffset, nil
		}
	}

	// If we still need more data, try to dequeue from the ring
	for bOffset < bN {
		p, ok := l.dequeuePacket(rx)
		if !ok {
			return bOffset, ErrTimeout
		}

		// Handle different packet types
		switch p.Op() {
		case protocol.OpStandardLinkCopy:
			err = l.handleStandardLinkCopy(&p, rxBuffers, rxFree, b[bOffset:], &bOffset)
		case protocol.OpConnCopy:
			err = l.handleConnCopy(&p, rxBuffers, rxFree, b[bOffset:], &bOffset)
		case protocol.OpError:
			err = l.handleError(&p)
		default:
			err = ErrInvalidOp
		}
		if err != nil {
			break
		}
		if bOffset > 0 {
			break
		}
	}

	return bOffset, err
}

// Write implements io.Writer for the link
// It writes data from the provided buffer to the link
func (l *StandardLink) Write(b []byte) (n int, err error) {
	bN := len(b)
	if bN == 0 {
		return 0, nil
	}

	// Check if data size exceeds buffer size
	if bN > l.bufferSize {
		return 0, ErrBufferOverflow
	}

	bOffset := 0

	// Determine which ring and buffers to use based on link mode
	var tx *mpmc.MPMCRing[protocol.Packet]
	var txBuffers [][]byte
	var txFree *mpmc.MPMCRing[uint64]

	if l.linkMode == LinkModeSecondary {
		// Secondary writes to ring1 (Secondary to Primary)
		tx = l.ring1
		txBuffers = l.buffers1
		txFree = l.free1
	} else {
		// Primary writes to ring0 (Primary to Secondary)
		tx = l.ring0
		txBuffers = l.buffers0
		txFree = l.free0
	}

	bufferIndex64, ok := l.dequeueFreeBuffer(txFree)
	if !ok {
		return 0, ErrTimeout
	}
	bufferIndex := int(bufferIndex64)
	if bufferIndex < 0 || bufferIndex >= len(txBuffers) {
		return 0, ErrInvalidSize
	}

	// Copy data to buffer
	copySize := min(bN, l.bufferSize)
	race.Copy(txBuffers[bufferIndex], b[bOffset:bOffset+copySize])
	bOffset += copySize

	// Send packet
	packet := protocol.NewStandardLinkCopyPacket(
		l.idGenerator.Add(1),
		uint64(bufferIndex),
		uint64(copySize),
	)

	if !l.enqueuePacket(tx, packet) {
		// The data packet was not published; return the buffer to the free
		// pool so a timed-out writer cannot leak buffer ownership.
		txFree.Enqueue(uint64(bufferIndex))
		return 0, ErrTimeout
	}

	return bOffset, nil
}

func (l *StandardLink) dequeuePacket(r *mpmc.MPMCRing[protocol.Packet]) (protocol.Packet, bool) {
	if l.readDeadline.IsZero() {
		return r.Dequeue(), true
	}
	ctx, cancel := contextForDeadline(l.readDeadline)
	defer cancel()
	return r.DequeueWithContext(ctx)
}

func (l *StandardLink) dequeueFreeBuffer(r *mpmc.MPMCRing[uint64]) (uint64, bool) {
	if l.writeDeadline.IsZero() {
		return r.Dequeue(), true
	}
	ctx, cancel := contextForDeadline(l.writeDeadline)
	defer cancel()
	return r.DequeueWithContext(ctx)
}

func (l *StandardLink) enqueuePacket(r *mpmc.MPMCRing[protocol.Packet], packet protocol.Packet) bool {
	if l.writeDeadline.IsZero() {
		r.Enqueue(packet)
		return true
	}
	ctx, cancel := contextForDeadline(l.writeDeadline)
	defer cancel()
	return r.EnqueueWithContext(ctx, packet)
}

// handleStandardLinkCopy processes a standard link copy packet
// It extracts data from the specified buffer and copies it to the destination
func (l *StandardLink) handleStandardLinkCopy(p *protocol.Packet, buffers [][]byte, free *mpmc.MPMCRing[uint64], dst []byte, dstOffset *int) error {
	bufferIndex := int(p.Operand(1))
	size := int(p.Operand(2))

	// Validate buffer index
	if bufferIndex < 0 || bufferIndex >= len(buffers) {
		return ErrInvalidSize
	}

	buffer := buffers[bufferIndex]
	if size > len(buffer) {
		size = len(buffer)
	}

	available := len(dst) - *dstOffset
	if available <= 0 {
		l.appendReceiveBuffer(buffer[:size])
		free.Enqueue(uint64(bufferIndex))
		return nil
	}

	// Copy data to destination buffer
	if size > available {
		// Store excess in receiveBuffer
		copied := race.Copy(dst[*dstOffset:], buffer[:available])
		*dstOffset += copied
		l.appendReceiveBuffer(buffer[available:size])
	} else {
		copied := race.Copy(dst[*dstOffset:], buffer[:size])
		*dstOffset += copied
	}

	free.Enqueue(uint64(bufferIndex))
	return nil
}

// handleConnCopy processes a connection copy packet
// It validates the connection and copies data from the specified buffer
func (l *StandardLink) handleConnCopy(p *protocol.Packet, buffers [][]byte, free *mpmc.MPMCRing[uint64], dst []byte, dstOffset *int) error {
	bufferIndex := int(p.Operand(1))
	size := int(p.Operand(2))

	// Validate buffer index
	if bufferIndex < 0 || bufferIndex >= len(buffers) {
		return ErrInvalidSize
	}

	buffer := buffers[bufferIndex]
	if size > len(buffer) {
		size = len(buffer)
	}

	available := len(dst) - *dstOffset
	if available <= 0 {
		l.appendReceiveBuffer(buffer[:size])
		free.Enqueue(uint64(bufferIndex))
		return nil
	}

	// Copy data to destination buffer
	if size > available {
		// Store excess in receiveBuffer
		copied := race.Copy(dst[*dstOffset:], buffer[:available])
		*dstOffset += copied
		l.appendReceiveBuffer(buffer[available:size])
	} else {
		copied := race.Copy(dst[*dstOffset:], buffer[:size])
		*dstOffset += copied
	}

	free.Enqueue(uint64(bufferIndex))
	return nil
}

func (l *StandardLink) appendReceiveBuffer(src []byte) {
	if len(src) == 0 {
		return
	}
	offset := len(l.receiveBuffer)
	l.receiveBuffer = append(l.receiveBuffer, make([]byte, len(src))...)
	race.Copy(l.receiveBuffer[offset:], src)
}

// handleError processes an error packet
// It converts error codes to appropriate error types
func (l *StandardLink) handleError(p *protocol.Packet) error {
	errorCode := ErrorCode(p.Operand(1))

	// Convert error code to error type
	switch errorCode {
	case ErrCodeMemoryAlign:
		return ErrMemoryAlign
	case ErrCodeInvalidSize:
		return ErrInvalidSize
	case ErrCodeMemorySmall:
		return ErrMemorySmall
	case ErrCodeFailedInit:
		return ErrFailedInit
	case ErrCodeConnNotFound:
		return ErrConnNotFound
	case ErrCodeInvalidOp:
		return ErrInvalidOp
	case ErrCodeBufferOverflow:
		return ErrBufferOverflow
	default:
		return errors.New("hqq: unknown error")
	}
}

// findAvailableBuffer finds an available buffer index
// This implementation uses a simple round-robin strategy
// GetMode returns the current link mode (Primary or Secondary)
func (l *StandardLink) GetMode() LinkMode {
	return l.linkMode
}

// GetType returns the current link type (Standard or Advanced)
func (l *StandardLink) GetType() LinkType {
	return l.linkType
}

// Close closes the link and cleans up resources
func (l *StandardLink) Close() error {
	// Clear buffers
	l.buffers0 = nil
	l.buffers1 = nil
	l.receiveBuffer = nil

	return nil
}

// enqueueWithTimeout enqueues a packet with a timeout
// This function ensures the enqueue operation respects the context deadline
func (l *StandardLink) enqueueWithTimeout(ctx context.Context, ring *mpmc.MPMCRing[protocol.Packet], packet protocol.Packet) error {
	if !ring.EnqueueWithContext(ctx, packet) {
		return ctx.Err()
	}
	return nil
}

// AdvancedLink represents an advanced HQQ link with additional features
// Built on top of StandardLink to provide enhanced capabilities
type AdvancedLink struct {
	slink *StandardLink // Wrapped standard link

	// Protocol negotiation state
	protocolState          ProtocolState   // Current negotiation state
	negotiatedVersion      ProtocolVersion // Negotiated protocol version
	negotiatedFeatures     uint64          // Negotiated feature flags
	negotiatedCapabilities uint64          // Negotiated capability flags

	// Advanced features
	compressionEnabled bool // Compression is enabled
	encryptionEnabled  bool // Encryption is enabled
	flowControlEnabled bool // Flow control is enabled

	// Connection management
	connections sync.Map   // map[uint64]*Connection - Active connections
	listening   bool       // Whether the link is listening for connections
	connCond    *sync.Cond // Condition variable for connection notifications

	// Statistics
	advancedStats AdvancedStats // Advanced statistics
	statsMutex    sync.RWMutex  // Mutex for statistics access
}

// AdvancedStats contains advanced statistics for the AdvancedLink
type AdvancedStats struct {
	// Advanced stats
	CompressionRatio   float64       // Compression ratio achieved
	EncryptionOverhead uint64        // Overhead from encryption
	FlowControlEvents  uint64        // Number of flow control events
	NegotiationTime    time.Duration // Time taken for protocol negotiation
}

// NewAdvancedLink creates a new advanced link
// It wraps a StandardLink with additional negotiation capabilities
func NewAdvancedLink(offset uintptr, bufferCount int, bufferSize int) (*AdvancedLink, error) {
	slink, err := OpenStandardLink(offset, bufferCount, bufferSize)
	if err != nil {
		return nil, err
	}

	advLink := &AdvancedLink{
		slink:         slink,
		protocolState: ProtocolStateNone,
		listening:     false,
		advancedStats: AdvancedStats{},
	}
	advLink.connCond = sync.NewCond(&sync.Mutex{})

	return advLink, nil
}

// Read implements io.Reader for the advanced link
// It delegates to the underlying standard link
func (l *AdvancedLink) Read(b []byte) (n int, err error) {
	return l.slink.Read(b)
}

// Write implements io.Writer for the advanced link
// It delegates to the underlying standard link
func (l *AdvancedLink) Write(b []byte) (n int, err error) {
	return l.slink.Write(b)
}

// Close closes the advanced link
// It delegates to the underlying standard link
func (l *AdvancedLink) Close() error {
	// Close all connections
	l.connections.Range(func(key, value interface{}) bool {
		if conn, ok := value.(*Connection); ok {
			conn.state = ConnectionStateClosed
		}
		l.connections.Delete(key)
		return true
	})

	// Stop listening
	l.listening = false

	// Close the underlying standard link
	return l.slink.Close()
}

// GetMode returns the mode of the advanced link
// It delegates to the underlying standard link
func (l *AdvancedLink) GetMode() LinkMode {
	return l.slink.GetMode()
}

// GetType returns the type of the advanced link
func (l *AdvancedLink) GetType() LinkType {
	return LinkTypeAdvanced
}

// NegotiateProtocol initiates protocol negotiation with the specified features
// It sends a negotiation request and waits for acknowledgment
func (l *AdvancedLink) NegotiateProtocol(ctx context.Context, version ProtocolVersion, features uint64) (bool, error) {
	if l.protocolState != ProtocolStateNone {
		return false, errors.New("hqq: protocol already negotiated or in progress")
	}

	l.protocolState = ProtocolStateNegotiating
	startTime := time.Now()
	defer func() {
		l.statsMutex.Lock()
		l.advancedStats.NegotiationTime = time.Since(startTime)
		l.statsMutex.Unlock()
	}()

	// Prepare negotiation packet
	versionEncoded := uint64(version.Major)<<8 | uint64(version.Minor)
	capabilities := CapabilityLargeBuffers | CapabilityHighThroughput

	packet := protocol.NewPacket(
		protocol.OpProtoNegotiate,
		versionEncoded,
		features,
		capabilities,
	)

	// Determine which ring to use based on link mode
	var tx *mpmc.MPMCRing[protocol.Packet]
	var rx *mpmc.MPMCRing[protocol.Packet]

	if l.slink.linkMode == LinkModeSecondary {
		tx = l.slink.ring1
		rx = l.slink.ring0
	} else {
		tx = l.slink.ring0
		rx = l.slink.ring1
	}

	// Send negotiation request
	if err := l.slink.enqueueWithTimeout(ctx, tx, packet); err != nil {
		l.protocolState = ProtocolStateFailed
		return false, err
	}

	// Wait for acknowledgment. This loop runs inline so successful negotiation
	// does not leave behind a goroutine that can keep consuming the shared ring.
	for {
		p, ok := rx.DequeueWithContext(ctx)
		if !ok {
			l.protocolState = ProtocolStateFailed
			return false, ctx.Err()
		}
		switch p.Op() {
		case protocol.OpProtoAck:
			// Extract negotiated parameters
			negotiatedVersion := uint16(p.Operand(0))
			l.negotiatedVersion = ProtocolVersion{
				Major: uint8(negotiatedVersion >> 8),
				Minor: uint8(negotiatedVersion & 0xFF),
			}
			l.negotiatedFeatures = p.Operand(1)
			l.negotiatedCapabilities = p.Operand(2)

			// Enable features based on negotiation
			l.compressionEnabled = (l.negotiatedFeatures & FeatureCompression) != 0
			l.encryptionEnabled = (l.negotiatedFeatures & FeatureEncryption) != 0
			l.flowControlEnabled = (l.negotiatedFeatures & FeatureFlowControl) != 0

			l.protocolState = ProtocolStateNegotiated
			return true, nil
		case protocol.OpError:
			l.protocolState = ProtocolStateFailed
			return false, errors.New("hqq: negotiation rejected by peer")
		}
	}
}

// WaitForNegotiation waits for a protocol negotiation request from the peer
// It processes incoming negotiation requests and sends appropriate responses
func (l *AdvancedLink) WaitForNegotiation(ctx context.Context) (bool, error) {
	if l.protocolState != ProtocolStateNone {
		return false, errors.New("hqq: protocol already negotiated or in progress")
	}

	l.protocolState = ProtocolStateNegotiating
	startTime := time.Now()
	defer func() {
		l.statsMutex.Lock()
		l.advancedStats.NegotiationTime = time.Since(startTime)
		l.statsMutex.Unlock()
	}()

	// Determine which ring to use based on link mode
	var tx *mpmc.MPMCRing[protocol.Packet]
	var rx *mpmc.MPMCRing[protocol.Packet]

	if l.slink.linkMode == LinkModeSecondary {
		tx = l.slink.ring1
		rx = l.slink.ring0
	} else {
		tx = l.slink.ring0
		rx = l.slink.ring1
	}

	// Wait for negotiation request inline to avoid lingering ring consumers.
	for {
		p, ok := rx.DequeueWithContext(ctx)
		if !ok {
			l.protocolState = ProtocolStateFailed
			return false, ctx.Err()
		}
		if p.Op() != protocol.OpProtoNegotiate {
			continue
		}

		// Extract requested parameters
		requestedVersion := uint16(p.Operand(0))
		requestedFeatures := p.Operand(1)
		requestedCapabilities := p.Operand(2)

		// Determine our capabilities
		supportedVersion := ProtocolVersion{Major: 1, Minor: 1}
		supportedFeatures := FeatureCompression | FeatureFlowControl | FeatureStatistics
		supportedCapabilities := CapabilityLargeBuffers | CapabilityHighThroughput

		// Negotiate version (use the minimum of requested and supported)
		if uint8(requestedVersion>>8) > supportedVersion.Major ||
			(uint8(requestedVersion>>8) == supportedVersion.Major && uint8(requestedVersion&0xFF) > supportedVersion.Minor) {
			// Use our maximum supported version
			l.negotiatedVersion = supportedVersion
		} else {
			// Use requested version
			l.negotiatedVersion = ProtocolVersion{
				Major: uint8(requestedVersion >> 8),
				Minor: uint8(requestedVersion & 0xFF),
			}
		}

		// Negotiate features (use intersection of requested and supported)
		l.negotiatedFeatures = requestedFeatures & supportedFeatures

		// Negotiate capabilities (use intersection of requested and supported)
		l.negotiatedCapabilities = requestedCapabilities & supportedCapabilities

		// Enable features based on negotiation
		l.compressionEnabled = (l.negotiatedFeatures & FeatureCompression) != 0
		l.encryptionEnabled = (l.negotiatedFeatures & FeatureEncryption) != 0
		l.flowControlEnabled = (l.negotiatedFeatures & FeatureFlowControl) != 0

		// Send acknowledgment
		versionEncoded := uint64(l.negotiatedVersion.Major)<<8 | uint64(l.negotiatedVersion.Minor)
		ackPacket := protocol.NewPacket(
			protocol.OpProtoAck,
			versionEncoded,
			l.negotiatedFeatures,
			l.negotiatedCapabilities,
		)

		tx.Enqueue(ackPacket)
		l.protocolState = ProtocolStateNegotiated
		return true, nil
	}
}

// GetProtocolState returns the current protocol negotiation state
func (l *AdvancedLink) GetProtocolState() ProtocolState {
	return l.protocolState
}

// GetNegotiatedVersion returns the negotiated protocol version
func (l *AdvancedLink) GetNegotiatedVersion() ProtocolVersion {
	return l.negotiatedVersion
}

// GetNegotiatedFeatures returns the negotiated feature flags
func (l *AdvancedLink) GetNegotiatedFeatures() uint64 {
	return l.negotiatedFeatures
}

// GetNegotiatedCapabilities returns the negotiated capability flags
func (l *AdvancedLink) GetNegotiatedCapabilities() uint64 {
	return l.negotiatedCapabilities
}

// IsCompressionEnabled returns whether compression is enabled
func (l *AdvancedLink) IsCompressionEnabled() bool {
	return l.compressionEnabled
}

// IsEncryptionEnabled returns whether encryption is enabled
func (l *AdvancedLink) IsEncryptionEnabled() bool {
	return l.encryptionEnabled
}

// IsFlowControlEnabled returns whether flow control is enabled
func (l *AdvancedLink) IsFlowControlEnabled() bool {
	return l.flowControlEnabled
}

// GetAdvancedStats returns advanced statistics for the link
func (l *AdvancedLink) GetAdvancedStats() AdvancedStats {
	l.statsMutex.RLock()
	defer l.statsMutex.RUnlock()

	return l.advancedStats
}

// Listen starts listening for incoming connection requests
// This puts the AdvancedLink into a listening mode where it can accept connections
func (l *AdvancedLink) Listen(ctx context.Context) error {
	if l.listening {
		return errors.New("hqq: already listening for connections")
	}

	l.listening = true

	// Start a goroutine to handle incoming connection requests
	go l.handleConnectionRequests(ctx)

	return nil
}

// Accept waits for and accepts an incoming connection request
// It returns the connection ID when a connection is established
func (l *AdvancedLink) Accept(ctx context.Context) (uint64, error) {
	if !l.listening {
		return 0, errors.New("hqq: not listening for connections")
	}

	// Use condition variable for efficient waiting
	resultChan := make(chan uint64, 1)
	errChan := make(chan error, 1)

	go func() {
		l.connCond.L.Lock()
		defer l.connCond.L.Unlock()

		for {
			// Check if we have any pending connection requests
			var rx *mpmc.MPMCRing[protocol.Packet]
			if l.slink.linkMode == LinkModeSecondary {
				rx = l.slink.ring0
			} else {
				rx = l.slink.ring1
			}

			var foundConnID uint64
			found := false

			rx.DequeueFunc(func(p *protocol.Packet) {
				if p.Op() == protocol.OpConnCreate {
					connID := p.Operand(0)

					// Create connection object
					conn := &Connection{
						id:        uint64(connID),
						state:     ConnectionStateOpening,
						createdAt: time.Now(),
						lastUsed:  time.Now(),
					}

					l.connections.Store(uint64(connID), conn)

					// Send acceptance
					var tx *mpmc.MPMCRing[protocol.Packet]
					if l.slink.linkMode == LinkModeSecondary {
						tx = l.slink.ring1
					} else {
						tx = l.slink.ring0
					}

					ackPacket := protocol.NewPacket(protocol.OpConnAccept, connID)

					tx.Enqueue(ackPacket)

					// Update connection state
					conn.state = ConnectionStateOpen
					foundConnID = uint64(connID)
					found = true
				}
			})

			if found {
				resultChan <- foundConnID
				return
			}

			// Wait for notification or timeout
			done := make(chan struct{})
			go func() {
				l.connCond.Wait()
				close(done)
			}()

			select {
			case <-done:
				// Woke up, try again
				continue
			case <-ctx.Done():
				errChan <- ctx.Err()
				return
			}
		}
	}()

	select {
	case connID := <-resultChan:
		return connID, nil
	case err := <-errChan:
		return 0, err
	case <-ctx.Done():
		return 0, ctx.Err()
	}
}

// createConnection creates a new connection to a listening peer
// It sends a connection request and waits for acceptance
// This is a private method used internally by the link
func (l *AdvancedLink) createConnection(ctx context.Context) (uint64, error) {
	connID := l.slink.idGenerator.Add(1)

	conn := &Connection{
		id:        connID,
		state:     ConnectionStateOpening,
		createdAt: time.Now(),
		lastUsed:  time.Now(),
	}

	l.connections.Store(connID, conn)

	// Send connection request
	packet := protocol.NewPacket(protocol.OpConnCreate, connID)

	var tx *mpmc.MPMCRing[protocol.Packet]
	if l.slink.linkMode == LinkModeSecondary {
		tx = l.slink.ring1
	} else {
		tx = l.slink.ring0
	}

	if err := l.slink.enqueueWithTimeout(ctx, tx, packet); err != nil {
		l.connections.Delete(connID)
		return 0, err
	}

	// Wait for acceptance
	if err := l.waitForConnectionAccept(ctx, connID); err != nil {
		l.connections.Delete(connID)
		return 0, err
	}

	conn.state = ConnectionStateOpen

	return connID, nil
}

// handleConnectionRequests continuously processes incoming connection requests
// This runs in a separate goroutine when Listen is called
func (l *AdvancedLink) handleConnectionRequests(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			l.listening = false
			return
		default:
			var rx *mpmc.MPMCRing[protocol.Packet]

			if l.slink.linkMode == LinkModeSecondary {
				rx = l.slink.ring0
			} else {
				rx = l.slink.ring1
			}

			processed := false
			rx.DequeueFunc(func(p *protocol.Packet) {
				processed = true
				if p.Op() == protocol.OpConnCreate {
					// Notify waiting Accept calls
					l.connCond.Broadcast()
					return
				}
			})

			// If no packet was processed, yield to prevent busy waiting
			if !processed {
				time.Sleep(time.Millisecond)
			}
		}
	}
}

// waitForConnectionAccept waits for a connection to be accepted
// It monitors the receive ring for an acceptance packet
func (l *AdvancedLink) waitForConnectionAccept(ctx context.Context, connID uint64) error {
	var rx *mpmc.MPMCRing[protocol.Packet]
	if l.slink.linkMode == LinkModeSecondary {
		rx = l.slink.ring0
	} else {
		rx = l.slink.ring1
	}

	done := make(chan error, 1)
	go func() {
		for {
			select {
			case <-ctx.Done():
				done <- ctx.Err()
				return
			default:
				processed := false
				rx.DequeueFunc(func(p *protocol.Packet) {
					processed = true
					if p.Op() == protocol.OpConnAccept && p.Operand(0) == connID {
						done <- nil
						return
					}
					if p.Op() == protocol.OpError && p.Operand(0) == connID {
						done <- ErrConnNotFound
						return
					}
				})

				// If no packet was processed, yield to prevent busy waiting
				if !processed {
					time.Sleep(time.Millisecond)
				}
			}
		}
	}()

	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func contextForDeadline(deadline time.Time) (context.Context, context.CancelFunc) {
	if deadline.IsZero() {
		return context.Background(), func() {}
	}
	timeout := time.Until(deadline)
	if timeout <= 0 {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		return ctx, func() {}
	}
	return context.WithTimeout(context.Background(), timeout)
}

// min returns the minimum of two integers
// This is a helper function for buffer size calculations
func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// PacketConn interface implementation
// These methods allow StandardLink to be used as a net.PacketConn

// ReadFrom implements net.PacketConn.ReadFrom
func (l *StandardLink) ReadFrom(p []byte) (n int, addr net.Addr, err error) {
	n, err = l.Read(p)
	if err != nil {
		return 0, nil, err
	}
	return n, l.remoteAddr, nil
}

// WriteTo implements net.PacketConn.WriteTo
func (l *StandardLink) WriteTo(p []byte, addr net.Addr) (n int, err error) {
	// Check if data size exceeds buffer size
	if len(p) > l.bufferSize {
		return 0, ErrBufferOverflow
	}

	// Store the remote address if provided
	if addr != nil {
		l.remoteAddr = addr
	}

	return l.Write(p)
}

// LocalAddr implements net.PacketConn.LocalAddr
func (l *StandardLink) LocalAddr() net.Addr {
	return l.localAddr
}

// SetDeadline implements net.PacketConn.SetDeadline
func (l *StandardLink) SetDeadline(t time.Time) error {
	l.readDeadline = t
	l.writeDeadline = t
	return nil
}

// SetReadDeadline implements net.PacketConn.SetReadDeadline
func (l *StandardLink) SetReadDeadline(t time.Time) error {
	l.readDeadline = t
	return nil
}

// SetWriteDeadline implements net.PacketConn.SetWriteDeadline
func (l *StandardLink) SetWriteDeadline(t time.Time) error {
	l.writeDeadline = t
	return nil
}

// HQQAddr represents an HQQ network address
// This implements the net.Addr interface for HQQ links
type HQQAddr struct {
	LinkID string // Identifier for the HQQ link
}

// Network returns the network type
func (a *HQQAddr) Network() string {
	return "hqq"
}

// String returns the string representation of the address
func (a *HQQAddr) String() string {
	return a.LinkID
}
