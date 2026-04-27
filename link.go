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

// Feature flags for Advanced Protocol negotiation.
const (
	FeatureNone            uint64 = 0
	FeatureLargeCopy       uint64 = 1 << iota // Enable OpConnCopy chunking for payloads larger than bufferSize.
	FeatureRequestResponse                    // Enable request-response IPC over OpConnCopy.
)

// Capability flags for Advanced Protocol negotiation.
const (
	CapabilityNone           uint64 = 0
	CapabilityLargeBuffers   uint64 = 1 << iota // Endpoint supports large configured payload buffers.
	CapabilityHighThroughput                    // Endpoint is optimized for high-throughput chunk streams.
	CapabilityLowLatency                        // Endpoint is optimized for low-latency request-response calls.
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

// StandardWriteReservation is a reserved StandardLink write slot.
//
// WARNING: A reservation holds an underlying ring slot. Commit or Abort must be
// called promptly; if the reservation is dropped, copied, or retained without
// finishing, the whole direction can suffer head-of-line (HOL) blocking. The
// buffer returned by Buffer is invalid after Commit or Abort returns.
type StandardWriteReservation struct {
	slot   mpmc.ProducerSlot[protocol.Packet]
	buffer []byte
	copyID uint64
	done   bool
}

// Slot returns the data-ring slot index owned by this reservation.
func (r *StandardWriteReservation) Slot() uint64 {
	if r == nil {
		return 0
	}
	return r.slot.Slot()
}

// Buffer returns the slot-owned payload buffer while the reservation is active.
func (r *StandardWriteReservation) Buffer() []byte {
	if r == nil || r.done {
		return nil
	}
	return r.buffer
}

// Commit publishes n bytes from the reserved payload buffer.
func (r *StandardWriteReservation) Commit(n int) error {
	if r == nil || r.done || !r.slot.Valid() {
		return ErrInvalidOp
	}
	switch {
	case n <= 0:
		r.publishTombstone(ErrCodeInvalidSize)
		return ErrInvalidSize
	case n > len(r.buffer):
		r.publishTombstone(ErrCodeBufferOverflow)
		return ErrBufferOverflow
	default:
		*r.slot.Value() = protocol.NewStandardLinkCopyPacket(
			r.copyID,
			r.slot.Slot(),
			uint64(n),
		)
		r.slot.Commit()
		r.done = true
		return nil
	}
}

// Abort publishes a tombstone for the reserved slot. Readers skip tombstones.
// Use Abort when a reserved write cannot be completed but ring progress must be
// preserved.
func (r *StandardWriteReservation) Abort(reason ErrorCode) error {
	if r == nil || r.done || !r.slot.Valid() {
		return ErrInvalidOp
	}
	if reason == 0 {
		reason = ErrCodeInvalidOp
	}
	r.publishTombstone(reason)
	return nil
}

func (r *StandardWriteReservation) publishTombstone(reason ErrorCode) {
	*r.slot.Value() = protocol.NewStandardLinkTombstonePacket(
		r.copyID,
		r.slot.Slot(),
		uint64(reason),
	)
	r.slot.Commit()
	r.done = true
}

// StandardReadReservation is a reserved StandardLink read slot.
//
// WARNING: A reservation holds an underlying ring slot. Release must be called
// promptly; if the reservation is dropped, copied, or retained without
// releasing, the whole direction can suffer head-of-line (HOL) blocking. The
// buffer returned by Buffer is invalid after Release returns.
type StandardReadReservation struct {
	slot   mpmc.ConsumerSlot[protocol.Packet]
	link   *StandardLink
	buffer []byte
	local  bool
	done   bool
}

// Slot returns the data-ring slot index owned by this reservation.
func (r *StandardReadReservation) Slot() uint64 {
	if r == nil || r.local {
		return 0
	}
	return r.slot.Slot()
}

// Buffer returns the reserved payload buffer while the reservation is active.
func (r *StandardReadReservation) Buffer() []byte {
	if r == nil || r.done {
		return nil
	}
	return r.buffer
}

// Release releases the reserved read slot.
func (r *StandardReadReservation) Release() error {
	if r == nil || r.done {
		return ErrInvalidOp
	}
	if r.local {
		r.link.receiveBuffer = r.link.receiveBuffer[:0]
		r.done = true
		return nil
	}
	if !r.slot.Release() {
		return ErrInvalidOp
	}
	r.done = true
	return nil
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
// BUFFERS (Primary to Secondary)       // Data buffers for primary to secondary communication
// <<<< PAGE_BREAK
// MPMC_RING (Secondary to Primary)     // Ring buffer for packets from secondary to primary
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
	buffersSize := uintptr(bufferSize) * uintptr(bufferCount)

	var size uintptr
	size = alignPage(size + dataRingSize)
	size = alignPage(size + buffersSize)
	size = alignPage(size + dataRingSize)
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
	buffersSize := uintptr(bufferCount) * uintptr(bufferSize)

	// Initialize or attach to first ring (Primary to Secondary)
	ring0Offset := offset
	if mpmc.MPMCInit[protocol.Packet](offset, uint64(bufferCount)) {
		link.linkMode = LinkModePrimary
	}
	offset = alignPage(offset + dataRingSize)

	// Setup first buffer pool
	link.buffers0Offset = offset
	offset = alignPage(offset + buffersSize)

	// Initialize or attach to second ring (Secondary to Primary)
	ring1Offset := offset
	mpmc.MPMCInit[protocol.Packet](offset, uint64(bufferCount))
	offset = alignPage(offset + dataRingSize)

	// Setup second buffer pool
	link.buffers1Offset = offset
	offset = alignPage(offset + buffersSize)

	// Attach to rings
	link.ring0 = mpmc.MPMCAttach[protocol.Packet](ring0Offset, time.Second)
	link.ring1 = mpmc.MPMCAttach[protocol.Packet](ring1Offset, time.Second)
	if link.ring0 == nil || link.ring1 == nil {
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

// Read implements io.Reader for the link.
//
// Read is the automatic-release hot path for the same packet stream consumed
// by ReadZeroCopy and ReserveRead. It releases the claimed ring slot before
// returning to the caller.
func (l *StandardLink) Read(b []byte) (n int, err error) {
	if len(b) == 0 {
		return 0, nil
	}
	if len(l.receiveBuffer) > 0 {
		copied := copy(b, l.receiveBuffer)
		l.receiveBuffer = l.receiveBuffer[copied:]
		return copied, nil
	}

	rx, rxBuffers := l.rx()
	for {
		var readErr error
		ok := l.dequeuePacketZeroCopy(rx, func(slot int, p *protocol.Packet) {
			if slot < 0 || slot >= len(rxBuffers) {
				readErr = ErrInvalidSize
				return
			}

			switch p.Op() {
			case protocol.OpStandardLinkTombstone:
				return
			case protocol.OpStandardLinkCopy:
				readErr = l.handleStandardLinkCopy(p, rxBuffers[slot], b, &n)
			case protocol.OpConnCopy:
				readErr = l.handleConnCopy(p, rxBuffers[slot], b, &n)
			case protocol.OpError:
				readErr = l.handleError(p)
			default:
				readErr = ErrInvalidOp
			}
		})
		if !ok {
			return 0, ErrTimeout
		}
		if readErr != nil {
			return n, readErr
		}
		if n > 0 {
			return n, nil
		}
	}
}

// Write implements io.Writer for the link.
//
// Write is the automatic-commit hot path for the same packet stream produced
// by WriteZeroCopy and ReserveWrite. It publishes a copy packet before
// returning to the caller.
func (l *StandardLink) Write(b []byte) (n int, err error) {
	bN := len(b)
	if bN == 0 {
		return 0, nil
	}

	// Check if data size exceeds buffer size
	if bN > l.bufferSize {
		return 0, ErrBufferOverflow
	}

	tx, txBuffers := l.tx()
	copyID := l.idGenerator.Add(1)
	ok := l.enqueuePacketZeroCopy(tx, func(slot int, p *protocol.Packet) {
		if slot < 0 || slot >= len(txBuffers) {
			*p = protocol.NewStandardLinkTombstonePacket(copyID, uint64(slot), uint64(ErrCodeInvalidSize))
			return
		}

		n = race.Copy(txBuffers[slot], b)
		*p = protocol.NewStandardLinkCopyPacket(
			copyID,
			uint64(slot),
			uint64(n),
		)
	})
	if !ok {
		return 0, ErrTimeout
	}
	return n, nil
}

// ReserveWrite reserves one ring-slot-owned payload buffer for writing.
//
// WARNING: Commit or Abort must be called promptly on the returned reservation;
// otherwise the whole direction can suffer head-of-line (HOL) blocking.
func (l *StandardLink) ReserveWrite() (StandardWriteReservation, error) {
	tx, txBuffers := l.tx()
	slot, ok := l.reservePacketProducer(tx)
	if !ok {
		return StandardWriteReservation{}, ErrTimeout
	}

	slotIndex := int(slot.Slot())
	copyID := l.idGenerator.Add(1)
	if slotIndex < 0 || slotIndex >= len(txBuffers) {
		reservation := StandardWriteReservation{
			slot:   slot,
			copyID: copyID,
		}
		_ = reservation.Abort(ErrCodeInvalidSize)
		return StandardWriteReservation{}, ErrInvalidSize
	}

	return StandardWriteReservation{
		slot:   slot,
		buffer: txBuffers[slotIndex],
		copyID: copyID,
	}, nil
}

// ReserveRead reserves one readable payload buffer.
//
// WARNING: Release must be called promptly on the returned reservation;
// otherwise the whole direction can suffer head-of-line (HOL) blocking.
func (l *StandardLink) ReserveRead() (StandardReadReservation, error) {
	if len(l.receiveBuffer) > 0 {
		return StandardReadReservation{
			link:   l,
			buffer: l.receiveBuffer,
			local:  true,
		}, nil
	}

	rx, rxBuffers := l.rx()
	for {
		slot, ok := l.reservePacketConsumer(rx)
		if !ok {
			return StandardReadReservation{}, ErrTimeout
		}
		slotIndex := int(slot.Slot())
		if slotIndex < 0 || slotIndex >= len(rxBuffers) {
			slot.Release()
			return StandardReadReservation{}, ErrInvalidSize
		}

		p := slot.Value()
		switch p.Op() {
		case protocol.OpStandardLinkTombstone:
			slot.Release()
			continue
		case protocol.OpStandardLinkCopy, protocol.OpConnCopy:
			size := int(p.Operand(2))
			if size <= 0 || size > len(rxBuffers[slotIndex]) {
				slot.Release()
				return StandardReadReservation{}, ErrInvalidSize
			}
			return StandardReadReservation{
				slot:   slot,
				buffer: rxBuffers[slotIndex][:size:size],
			}, nil
		case protocol.OpError:
			err := l.handleError(p)
			slot.Release()
			return StandardReadReservation{}, err
		default:
			slot.Release()
			return StandardReadReservation{}, ErrInvalidOp
		}
	}
}

// WriteZeroCopy writes one message by letting fn fill the ring-slot-owned
// shared payload buffer directly.
//
// WARNING: The callback runs while the underlying ring slot is claimed. It must
// return promptly; if it blocks or never returns, the whole ring can suffer
// head-of-line (HOL) blocking. Do not retain the buffer after the callback returns.
//
// The callback must return a payload size in the range 1..BufferSize. If it
// returns an error or an invalid size, StandardLink publishes an error packet to
// keep the ring moving and returns that error to the caller.
func (l *StandardLink) WriteZeroCopy(fn func(buffer []byte) (int, error)) (n int, err error) {
	if fn == nil {
		return 0, ErrInvalidOp
	}

	tx, txBuffers := l.tx()
	copyID := l.idGenerator.Add(1)
	var callbackErr error
	ok := l.enqueuePacketZeroCopy(tx, func(slot int, p *protocol.Packet) {
		if slot < 0 || slot >= len(txBuffers) {
			callbackErr = ErrInvalidSize
			*p = protocol.NewStandardLinkTombstonePacket(copyID, uint64(slot), uint64(ErrCodeInvalidSize))
			return
		}

		n, callbackErr = fn(txBuffers[slot])
		switch {
		case callbackErr != nil:
			*p = protocol.NewStandardLinkTombstonePacket(copyID, uint64(slot), uint64(ErrCodeInvalidOp))
		case n <= 0:
			callbackErr = ErrInvalidSize
			*p = protocol.NewStandardLinkTombstonePacket(copyID, uint64(slot), uint64(ErrCodeInvalidSize))
		case n > len(txBuffers[slot]):
			callbackErr = ErrBufferOverflow
			*p = protocol.NewStandardLinkTombstonePacket(copyID, uint64(slot), uint64(ErrCodeBufferOverflow))
		default:
			*p = protocol.NewStandardLinkCopyPacket(copyID, uint64(slot), uint64(n))
		}
	})
	if !ok {
		return 0, ErrTimeout
	}
	if callbackErr != nil {
		return 0, callbackErr
	}
	return n, nil
}

// ReadZeroCopy reads one message by passing the ring-slot-owned shared payload
// buffer directly to fn.
//
// WARNING: The callback runs while the underlying ring slot is claimed. It must
// return promptly; if it blocks or never returns, the whole ring can suffer
// head-of-line (HOL) blocking. Do not retain the buffer after the callback returns.
//
// The buffer is only valid for the duration of fn. Returning from fn releases
// the ring slot and allows a future writer to overwrite the same memory.
func (l *StandardLink) ReadZeroCopy(fn func(buffer []byte) error) (n int, err error) {
	if fn == nil {
		return 0, ErrInvalidOp
	}

	if len(l.receiveBuffer) > 0 {
		buffer := l.receiveBuffer
		n = len(buffer)
		err = fn(buffer)
		l.receiveBuffer = l.receiveBuffer[:0]
		if err != nil {
			return 0, err
		}
		return n, nil
	}

	rx, rxBuffers := l.rx()
	for {
		var readErr error
		ok := l.dequeuePacketZeroCopy(rx, func(slot int, p *protocol.Packet) {
			if slot < 0 || slot >= len(rxBuffers) {
				readErr = ErrInvalidSize
				return
			}

			switch p.Op() {
			case protocol.OpStandardLinkTombstone:
				return
			case protocol.OpStandardLinkCopy, protocol.OpConnCopy:
				size := int(p.Operand(2))
				if size <= 0 || size > len(rxBuffers[slot]) {
					readErr = ErrInvalidSize
					return
				}
				buffer := rxBuffers[slot][:size:size]
				n = size
				readErr = fn(buffer)
			case protocol.OpError:
				readErr = l.handleError(p)
			default:
				readErr = ErrInvalidOp
			}
		})
		if !ok {
			return 0, ErrTimeout
		}
		if readErr != nil {
			return 0, readErr
		}
		if n > 0 {
			return n, nil
		}
	}
}

func (l *StandardLink) tx() (*mpmc.MPMCRing[protocol.Packet], [][]byte) {
	if l.linkMode == LinkModeSecondary {
		return l.ring1, l.buffers1
	}
	return l.ring0, l.buffers0
}

func (l *StandardLink) rx() (*mpmc.MPMCRing[protocol.Packet], [][]byte) {
	if l.linkMode == LinkModeSecondary {
		return l.ring0, l.buffers0
	}
	return l.ring1, l.buffers1
}

func (l *StandardLink) reservePacketProducer(r *mpmc.MPMCRing[protocol.Packet]) (mpmc.ProducerSlot[protocol.Packet], bool) {
	if l.writeDeadline.IsZero() {
		return r.ReserveProducer(), true
	}
	ctx, cancel := contextForDeadline(l.writeDeadline)
	defer cancel()
	return r.ReserveProducerWithContext(ctx)
}

func (l *StandardLink) reservePacketConsumer(r *mpmc.MPMCRing[protocol.Packet]) (mpmc.ConsumerSlot[protocol.Packet], bool) {
	if l.readDeadline.IsZero() {
		return r.ReserveConsumer(), true
	}
	ctx, cancel := contextForDeadline(l.readDeadline)
	defer cancel()
	return r.ReserveConsumerWithContext(ctx)
}

func (l *StandardLink) dequeuePacketZeroCopy(r *mpmc.MPMCRing[protocol.Packet], fn func(slot int, p *protocol.Packet)) bool {
	if l.readDeadline.IsZero() {
		r.DequeueZeroCopy(func(slot uint64, p *protocol.Packet) {
			fn(int(slot), p)
		})
		return true
	}
	ctx, cancel := contextForDeadline(l.readDeadline)
	defer cancel()
	return r.DequeueZeroCopyWithContext(ctx, func(slot uint64, p *protocol.Packet) {
		fn(int(slot), p)
	})
}

func (l *StandardLink) enqueuePacketZeroCopy(r *mpmc.MPMCRing[protocol.Packet], fn func(slot int, p *protocol.Packet)) bool {
	if l.writeDeadline.IsZero() {
		r.EnqueueZeroCopy(func(slot uint64, p *protocol.Packet) {
			fn(int(slot), p)
		})
		return true
	}
	ctx, cancel := contextForDeadline(l.writeDeadline)
	defer cancel()
	return r.EnqueueZeroCopyWithContext(ctx, func(slot uint64, p *protocol.Packet) {
		fn(int(slot), p)
	})
}

// handleStandardLinkCopy processes a standard link copy packet
// It extracts data from the specified buffer and copies it to the destination
func (l *StandardLink) handleStandardLinkCopy(p *protocol.Packet, buffer []byte, dst []byte, dstOffset *int) error {
	size := int(p.Operand(2))
	if size <= 0 {
		return ErrInvalidSize
	}

	if size > len(buffer) {
		return ErrInvalidSize
	}

	available := len(dst) - *dstOffset
	if available <= 0 {
		l.appendReceiveBuffer(buffer[:size])
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

	return nil
}

// handleConnCopy processes a connection copy packet
// It validates the connection and copies data from the specified buffer
func (l *StandardLink) handleConnCopy(p *protocol.Packet, buffer []byte, dst []byte, dstOffset *int) error {
	size := int(p.Operand(2))
	if size <= 0 {
		return ErrInvalidSize
	}

	if size > len(buffer) {
		return ErrInvalidSize
	}

	available := len(dst) - *dstOffset
	if available <= 0 {
		l.appendReceiveBuffer(buffer[:size])
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

// AdvancedLink implements the Advanced Protocol overlay on top of StandardLink.
type AdvancedLink struct {
	slink *StandardLink // Wrapped standard link

	// Protocol negotiation state
	protocolState          ProtocolState   // Current negotiation state
	negotiatedVersion      ProtocolVersion // Negotiated protocol version
	negotiatedFeatures     uint64          // Negotiated feature flags
	negotiatedCapabilities uint64          // Negotiated capability flags

	largeCopyEnabled       bool
	requestResponseEnabled bool

	messageIDGenerator atomic.Uint64

	// Connection management
	connections sync.Map   // map[uint64]*Connection - Active connections
	listening   bool       // Whether the link is listening for connections
	connCond    *sync.Cond // Condition variable for connection notifications

	receiveMu       sync.Mutex
	assemblies      map[advancedMessageKey]*advancedAssembly
	pendingMessages []AdvancedMessage

	// Statistics
	advancedStats AdvancedStats // Advanced statistics
	statsMutex    sync.RWMutex  // Mutex for statistics access
}

// AdvancedStats contains advanced statistics for the AdvancedLink
type AdvancedStats struct {
	NegotiationTime time.Duration
	LargeMessages   uint64
	LargeBytes      uint64
	Requests        uint64
	Responses       uint64
}

// AdvancedMessage is one complete Advanced Protocol message.
type AdvancedMessage struct {
	ConnectionID uint64
	MessageID    uint64
	MethodID     uint64
	Status       uint64
	Flags        uint64
	Payload      []byte
}

func (m AdvancedMessage) IsData() bool {
	return m.Flags&(AdvFlagRequest|AdvFlagResponse|AdvFlagAbort) == 0
}

func (m AdvancedMessage) IsRequest() bool {
	return m.Flags&AdvFlagRequest != 0
}

func (m AdvancedMessage) IsResponse() bool {
	return m.Flags&AdvFlagResponse != 0
}

func (m AdvancedMessage) IsError() bool {
	return m.Flags&AdvFlagError != 0
}

func (m AdvancedMessage) IsAbort() bool {
	return m.Flags&AdvFlagAbort != 0
}

// AdvancedResponseError reports an application/protocol error response.
type AdvancedResponseError struct {
	Status  uint64
	Payload []byte
}

func (e AdvancedResponseError) Error() string {
	return "hqq: advanced response error"
}

type advancedMessageKey struct {
	connectionID uint64
	messageID    uint64
	kind         uint64
}

type advancedAssembly struct {
	message AdvancedMessage
	data    []byte
	next    uint64
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
		assemblies:    make(map[advancedMessageKey]*advancedAssembly),
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
	capabilities := CapabilityLargeBuffers | CapabilityHighThroughput | CapabilityLowLatency

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

			l.enableNegotiatedFeatures()

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
		supportedVersion := ProtocolVersion{
			Major: protocol.HQQProtocolMajorVersion,
			Minor: protocol.HQQProtocolMinorVersion,
		}
		supportedFeatures := FeatureLargeCopy | FeatureRequestResponse
		supportedCapabilities := CapabilityLargeBuffers | CapabilityHighThroughput | CapabilityLowLatency

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

		l.enableNegotiatedFeatures()

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

func (l *AdvancedLink) enableNegotiatedFeatures() {
	l.largeCopyEnabled = (l.negotiatedFeatures & FeatureLargeCopy) != 0
	l.requestResponseEnabled = (l.negotiatedFeatures & FeatureRequestResponse) != 0
}

// IsLargeCopyEnabled returns whether Advanced large-data copy is enabled.
func (l *AdvancedLink) IsLargeCopyEnabled() bool {
	return l.largeCopyEnabled
}

// IsRequestResponseEnabled returns whether Advanced request-response IPC is enabled.
func (l *AdvancedLink) IsRequestResponseEnabled() bool {
	return l.requestResponseEnabled
}

// GetAdvancedStats returns advanced statistics for the link
func (l *AdvancedLink) GetAdvancedStats() AdvancedStats {
	l.statsMutex.RLock()
	defer l.statsMutex.RUnlock()

	return l.advancedStats
}

// SendLarge publishes payload as one Advanced large-copy message. It may split
// the payload into multiple OpConnCopy chunks when len(payload) > bufferSize.
func (l *AdvancedLink) SendLarge(ctx context.Context, connectionID uint64, payload []byte) (uint64, error) {
	if err := l.requireNegotiatedFeature(FeatureLargeCopy); err != nil {
		return 0, err
	}
	messageID := l.nextMessageID()
	if err := l.sendAdvancedPayload(ctx, connectionID, messageID, 0, 0, payload); err != nil {
		return 0, err
	}
	l.statsMutex.Lock()
	l.advancedStats.LargeMessages++
	l.advancedStats.LargeBytes += uint64(len(payload))
	l.statsMutex.Unlock()
	return messageID, nil
}

// Receive returns the next complete Advanced Protocol message.
func (l *AdvancedLink) Receive(ctx context.Context) (AdvancedMessage, error) {
	return l.receiveMatching(ctx, func(AdvancedMessage) bool { return true })
}

// Call sends a link-level request and waits for its response.
func (l *AdvancedLink) Call(ctx context.Context, methodID uint64, request []byte) ([]byte, error) {
	response, err := l.CallOn(ctx, 0, methodID, request)
	if err != nil {
		return nil, err
	}
	if response.IsError() {
		return nil, AdvancedResponseError{Status: response.Status, Payload: response.Payload}
	}
	return response.Payload, nil
}

// CallOn sends a request on connectionID and waits for the matching response.
func (l *AdvancedLink) CallOn(ctx context.Context, connectionID uint64, methodID uint64, request []byte) (AdvancedMessage, error) {
	if err := l.requireNegotiatedFeature(FeatureRequestResponse); err != nil {
		return AdvancedMessage{}, err
	}
	messageID := l.nextMessageID()
	if err := l.sendAdvancedPayload(ctx, connectionID, messageID, AdvFlagRequest, methodID, request); err != nil {
		return AdvancedMessage{}, err
	}
	l.statsMutex.Lock()
	l.advancedStats.Requests++
	l.statsMutex.Unlock()

	return l.receiveMatching(ctx, func(message AdvancedMessage) bool {
		return message.ConnectionID == connectionID &&
			message.MessageID == messageID &&
			message.IsResponse()
	})
}

// Respond sends a successful response for request.
func (l *AdvancedLink) Respond(ctx context.Context, request AdvancedMessage, status uint64, response []byte) error {
	if !request.IsRequest() {
		return ErrInvalidOp
	}
	if err := l.requireNegotiatedFeature(FeatureRequestResponse); err != nil {
		return err
	}
	if err := l.sendAdvancedPayload(ctx, request.ConnectionID, request.MessageID, AdvFlagResponse, status, response); err != nil {
		return err
	}
	l.statsMutex.Lock()
	l.advancedStats.Responses++
	l.statsMutex.Unlock()
	return nil
}

// RespondError sends an error response for request.
func (l *AdvancedLink) RespondError(ctx context.Context, request AdvancedMessage, status uint64, response []byte) error {
	if !request.IsRequest() {
		return ErrInvalidOp
	}
	if err := l.requireNegotiatedFeature(FeatureRequestResponse); err != nil {
		return err
	}
	if status == 0 {
		status = uint64(ErrCodeInvalidOp)
	}
	if err := l.sendAdvancedPayload(ctx, request.ConnectionID, request.MessageID, AdvFlagResponse|AdvFlagError, status, response); err != nil {
		return err
	}
	l.statsMutex.Lock()
	l.advancedStats.Responses++
	l.statsMutex.Unlock()
	return nil
}

func (l *AdvancedLink) requireNegotiatedFeature(feature uint64) error {
	if l.protocolState == ProtocolStateNegotiated && l.negotiatedFeatures&feature == 0 {
		return ErrInvalidOp
	}
	return nil
}

func (l *AdvancedLink) nextMessageID() uint64 {
	if id := l.messageIDGenerator.Add(1); id != 0 {
		return id
	}
	return l.messageIDGenerator.Add(1)
}

func (l *AdvancedLink) sendAdvancedPayload(ctx context.Context, connectionID, messageID, baseFlags, aux uint64, payload []byte) error {
	if ctx == nil {
		ctx = context.Background()
	}
	totalSize := uint64(len(payload))
	if totalSize == 0 {
		return l.sendAdvancedChunk(ctx, connectionID, messageID, 0, 0, AdvFlagBegin|AdvFlagEnd|baseFlags, aux, nil)
	}

	offset := uint64(0)
	for offset < totalSize {
		chunkSize := uint64(l.slink.bufferSize)
		remaining := totalSize - offset
		if remaining < chunkSize {
			chunkSize = remaining
		}
		flags := baseFlags
		if offset == 0 {
			flags |= AdvFlagBegin
		}
		if offset+chunkSize == totalSize {
			flags |= AdvFlagEnd
		}
		chunk := payload[offset : offset+chunkSize]
		if err := l.sendAdvancedChunk(ctx, connectionID, messageID, totalSize, offset, flags, auxForChunk(aux, offset), chunk); err != nil {
			_ = l.trySendAdvancedAbort(ctx, connectionID, messageID, baseFlags)
			return err
		}
		offset += chunkSize
	}
	return nil
}

func auxForChunk(aux uint64, offset uint64) uint64 {
	if offset == 0 {
		return aux
	}
	return 0
}

func (l *AdvancedLink) trySendAdvancedAbort(ctx context.Context, connectionID, messageID, baseFlags uint64) error {
	return l.sendAdvancedChunk(ctx, connectionID, messageID, 0, 0, AdvFlagAbort|baseFlags, 0, nil)
}

func (l *AdvancedLink) sendAdvancedChunk(ctx context.Context, connectionID, messageID, totalSize, chunkOffset, flags, aux uint64, chunk []byte) error {
	tx, txBuffers := l.slink.tx()
	slot, ok := reserveProducerContext(ctx, tx)
	if !ok {
		if ctx != nil && ctx.Err() != nil {
			return ctx.Err()
		}
		return ErrTimeout
	}

	slotIndex := int(slot.Slot())
	if slotIndex < 0 || slotIndex >= len(txBuffers) {
		*slot.Value() = protocol.NewStandardLinkTombstonePacket(messageID, slot.Slot(), uint64(ErrCodeInvalidSize))
		slot.Commit()
		return ErrInvalidSize
	}

	if len(chunk) > len(txBuffers[slotIndex]) {
		*slot.Value() = protocol.NewConnCopyPacket(connectionID, messageID, totalSize, chunkOffset, 0, AdvFlagAbort|flags, uint64(ErrCodeBufferOverflow))
		slot.Commit()
		return ErrBufferOverflow
	}
	if len(chunk) > 0 {
		race.Copy(txBuffers[slotIndex], chunk)
	}
	*slot.Value() = protocol.NewConnCopyPacket(
		connectionID,
		messageID,
		totalSize,
		chunkOffset,
		uint64(len(chunk)),
		flags,
		aux,
	)
	slot.Commit()
	return nil
}

func (l *AdvancedLink) receiveMatching(ctx context.Context, match func(AdvancedMessage) bool) (AdvancedMessage, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	l.receiveMu.Lock()
	defer l.receiveMu.Unlock()

	for {
		for i, message := range l.pendingMessages {
			if match(message) {
				copy(l.pendingMessages[i:], l.pendingMessages[i+1:])
				l.pendingMessages[len(l.pendingMessages)-1] = AdvancedMessage{}
				l.pendingMessages = l.pendingMessages[:len(l.pendingMessages)-1]
				return message, nil
			}
		}

		message, err := l.receiveAdvancedMessageLocked(ctx)
		if err != nil {
			return AdvancedMessage{}, err
		}
		if match(message) {
			return message, nil
		}
		l.pendingMessages = append(l.pendingMessages, message)
	}
}

func (l *AdvancedLink) receiveAdvancedMessageLocked(ctx context.Context) (AdvancedMessage, error) {
	rx, rxBuffers := l.slink.rx()
	for {
		slot, ok := reserveConsumerContext(ctx, rx)
		if !ok {
			if ctx != nil && ctx.Err() != nil {
				return AdvancedMessage{}, ctx.Err()
			}
			return AdvancedMessage{}, ErrTimeout
		}

		slotIndex := int(slot.Slot())
		if slotIndex < 0 || slotIndex >= len(rxBuffers) {
			slot.Release()
			return AdvancedMessage{}, ErrInvalidSize
		}

		p := slot.Value()
		switch p.Op() {
		case protocol.OpStandardLinkTombstone:
			slot.Release()
			continue
		case protocol.OpConnCopy:
			message, complete, err := l.consumeAdvancedChunk(p, rxBuffers[slotIndex])
			slot.Release()
			if err != nil {
				return AdvancedMessage{}, err
			}
			if complete {
				return message, nil
			}
		case protocol.OpError:
			err := l.slink.handleError(p)
			slot.Release()
			return AdvancedMessage{}, err
		default:
			slot.Release()
			return AdvancedMessage{}, ErrInvalidOp
		}
	}
}

func (l *AdvancedLink) consumeAdvancedChunk(p *protocol.Packet, buffer []byte) (AdvancedMessage, bool, error) {
	connectionID := p.Operand(0)
	messageID := p.Operand(1)
	totalSize := p.Operand(2)
	chunkOffset := p.Operand(3)
	chunkSize := p.Operand(4)
	flags := p.Operand(5)
	aux := p.Operand(6)
	kind := advancedKind(flags)
	key := advancedMessageKey{connectionID: connectionID, messageID: messageID, kind: kind}

	if flags&AdvFlagAbort != 0 {
		delete(l.assemblies, key)
		return AdvancedMessage{
			ConnectionID: connectionID,
			MessageID:    messageID,
			MethodID:     methodIDFromFlags(flags, aux),
			Status:       statusFromFlags(flags, aux),
			Flags:        flags,
		}, true, nil
	}

	if chunkSize > uint64(len(buffer)) || chunkOffset > totalSize || chunkSize > totalSize-chunkOffset {
		delete(l.assemblies, key)
		return AdvancedMessage{}, false, ErrInvalidSize
	}
	if totalSize == 0 && (chunkOffset != 0 || chunkSize != 0 || flags&(AdvFlagBegin|AdvFlagEnd) != AdvFlagBegin|AdvFlagEnd) {
		delete(l.assemblies, key)
		return AdvancedMessage{}, false, ErrInvalidSize
	}
	if totalSize > 0 && chunkSize == 0 {
		delete(l.assemblies, key)
		return AdvancedMessage{}, false, ErrInvalidSize
	}

	assembly := l.assemblies[key]
	if flags&AdvFlagBegin != 0 {
		if chunkOffset != 0 {
			return AdvancedMessage{}, false, ErrInvalidSize
		}
		if totalSize > uint64(^uint(0)>>1) {
			return AdvancedMessage{}, false, ErrBufferOverflow
		}
		assembly = &advancedAssembly{
			message: AdvancedMessage{
				ConnectionID: connectionID,
				MessageID:    messageID,
				MethodID:     methodIDFromFlags(flags, aux),
				Status:       statusFromFlags(flags, aux),
				Flags:        flags &^ (AdvFlagBegin | AdvFlagEnd),
			},
			data: make([]byte, int(totalSize)),
		}
		l.assemblies[key] = assembly
	}
	if assembly == nil {
		return AdvancedMessage{}, false, ErrInvalidOp
	}
	if chunkOffset != assembly.next || uint64(len(assembly.data)) != totalSize {
		delete(l.assemblies, key)
		return AdvancedMessage{}, false, ErrInvalidSize
	}
	if chunkSize > 0 {
		race.Copy(assembly.data[chunkOffset:chunkOffset+chunkSize], buffer[:chunkSize])
	}
	assembly.next += chunkSize
	assembly.message.Flags |= flags & (AdvFlagRequest | AdvFlagResponse | AdvFlagError)

	if flags&AdvFlagEnd == 0 {
		return AdvancedMessage{}, false, nil
	}
	if assembly.next != totalSize {
		delete(l.assemblies, key)
		return AdvancedMessage{}, false, ErrInvalidSize
	}
	delete(l.assemblies, key)
	assembly.message.Flags |= AdvFlagBegin | AdvFlagEnd
	assembly.message.Payload = assembly.data
	return assembly.message, true, nil
}

func advancedKind(flags uint64) uint64 {
	return flags & (AdvFlagRequest | AdvFlagResponse)
}

func methodIDFromFlags(flags, aux uint64) uint64 {
	if flags&AdvFlagRequest != 0 {
		return aux
	}
	return 0
}

func statusFromFlags(flags, aux uint64) uint64 {
	if flags&AdvFlagResponse != 0 {
		return aux
	}
	return 0
}

func reserveProducerContext(ctx context.Context, r *mpmc.MPMCRing[protocol.Packet]) (mpmc.ProducerSlot[protocol.Packet], bool) {
	if ctx == nil || ctx.Done() == nil {
		return r.ReserveProducer(), true
	}
	return r.ReserveProducerWithContext(ctx)
}

func reserveConsumerContext(ctx context.Context, r *mpmc.MPMCRing[protocol.Packet]) (mpmc.ConsumerSlot[protocol.Packet], bool) {
	if ctx == nil || ctx.Done() == nil {
		return r.ReserveConsumer(), true
	}
	return r.ReserveConsumerWithContext(ctx)
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
