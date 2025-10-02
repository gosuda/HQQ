# HQQ Protocol Specification

## Overview

The HQQ protocol is designed for efficient inter-process communication (IPC) using shared memory and lock-free MPMC (Multi-Producer Multi-Consumer) queues. This protocol enables high-performance data exchange between processes with minimal overhead and zero-copy transfers.

## Design Principles

1. **Zero-Copy**: Data remains in shared memory to minimize memory bandwidth usage
2. **Lock-Free**: All operations use atomic primitives for maximum concurrency
3. **Bidirectional**: Full-duplex communication with separate channels for each direction
4. **Extensible**: Protocol negotiation allows for feature discovery and future extensions
5. **Efficient**: Optimized memory layout with proper alignment to reduce cache misses

## Architecture

### Memory Layout

The HQQ protocol uses a shared memory region organized as follows:

```
<<<<< PAGE_START
MPMC_RING (Primary to Secondary)     // Lock-free ring buffer for packets
<<<<< PAGE_BREAK
BUFFERS (Primary to Secondary)       // Data buffers for payload storage
<<<<< PAGE_BREAK
MPMC_RING (Secondary to Primary)     // Lock-free ring buffer for return packets
<<<<< PAGE_BREAK
BUFFERS (Secondary to Primary)       // Data buffers for return payloads
<<<<< PAGE_END
```

### Memory Alignment

- All structures are page-aligned to reduce cache misses
- Buffer sizes are multiples of 8 bytes for proper alignment
- Ring buffers use cache-line padding to prevent false sharing

### Components

1. **MPMC Rings**: Two lock-free ring buffers for bidirectional communication
   - Ring 0: Primary to Secondary communication
   - Ring 1: Secondary to Primary communication
   
2. **Buffer Pools**: Memory regions for storing actual data payloads
   - Pre-allocated buffers indexed by position
   - Buffer indices are passed in packets, not actual data
   
3. **Packet Structure**: Standardized message format for all communications
   - Fixed-size header with operation code and operands
   - Extensible design for future protocol versions

## Packet Format

All communication uses the following packet structure:

```go
type Packet struct {
    Op       OpCode     // Operation type (1 byte)
    Operand0 uintptr    // First operand (typically ID or offset)
    Operand1 uintptr    // Second operand (typically size or flags)
    Operand2 uintptr    // Third operand (typically offset or count)
    Operand3 uintptr    // Fourth operand (reserved for future use)
    Operand4 uintptr    // Fifth operand (reserved for future use)
    Operand5 uintptr    // Sixth operand (reserved for future use)
    Operand6 uintptr    // Seventh operand (reserved for future use)
}
```

### Packet Size

- Fixed size of 56 bytes (7 operands × 8 bytes + 1 byte opcode + padding)
- Optimized for cache line efficiency
- All operands are uintptr for platform independence

## Operation Codes

### Error Handling

#### OpError (0x00)
- **Purpose**: Report errors between processes
- **Operand0**: ContextID (typically connection ID or request ID)
- **Operand1**: Error Number ( ErrorCode )
- **Usage**: Sent in response to invalid operations or system errors

### Connection Management

#### OpConnCreate (0x01)
- **Purpose**: Create a new logical connection
- **Operand0**: ConnectionID (unique identifier for the connection)
- **Response**: OpConnAccept with the same ConnectionID or OpError on failure
- **Flow**: Primary → Secondary

#### OpConnAccept (0x02)
- **Purpose**: Accept a connection request
- **Operand0**: ConnectionID (matches the OpConnCreate request)
- **Usage**: Sent in response to OpConnCreate
- **Flow**: Secondary → Primary

#### OpConnClose (0x03)
- **Purpose**: Close an existing connection
- **Operand0**: ConnectionID (identifier of the connection to close)
- **Usage**: Sent by either party to terminate a connection
- **Flow**: Bidirectional

### Data Transfer

#### OpConnCopy (0x04)
- **Purpose**: Transfer data between processes over a connection
- **Operand0**: ConnectionID (identifier of the connection)
- **Operand1**: CopyID (unique identifier for this transfer)
- **Operand2**: CopySize (total size of the data to transfer)
- **Operand3**: TransferSize (size of this specific transfer)
- **Operand4**: TransferOffset (offset within the total data)
- **Operand5**: BufferSize (size of the buffer containing data)
- **Operand6**: BufferOffset (offset within the buffer)
- **Usage**: For connection-oriented data transfer

#### OpStandardLinkCopy (0xFF)
- **Purpose**: Direct data copy without connection overhead
- **Operand0**: CopyID (unique identifier for this transfer)
- **Operand1**: BufferIndex (index of the buffer containing data)
- **Operand2**: DataSize (actual size of data in the buffer)
- **Usage**: For simple, connection-less data transfer
- **Performance**: Lowest overhead data transfer method

### Protocol Negotiation

#### OpProtoNegotiate (0x05)
- **Purpose**: Initiate protocol negotiation
- **Operand0**: Protocol Version (encoded as major.minor)
- **Operand1**: Feature Flags (bitmask of requested features)
- **Operand2**: Capability Flags (bitmask of supported capabilities)
- **Encoding**: Version = (Major << 8) | Minor

#### OpProtoAck (0x06)
- **Purpose**: Acknowledge protocol negotiation
- **Operand0**: Accepted Protocol Version (encoded as major.minor)
- **Operand1**: Accepted Feature Flags (bitmask of negotiated features)
- **Operand2**: Accepted Capability Flags (bitmask of negotiated capabilities)
- **Usage**: Sent in response to OpProtoNegotiate

## Link Types

### Standard Link

The Standard Link provides basic inter-process communication functionality:

- **Bidirectional Data Transfer**: Using two MPMC rings for full-duplex communication
- **Connection Management**: State tracking for logical connections
- **Error Handling**: Comprehensive error reporting with specific error codes
- **Basic Statistics**: Packet counts, byte transfers, and error tracking
- **Interface Compatibility**: Implements io.Reader, io.Writer, and net.PacketConn

### Advanced Link

The Advanced Link is built on top of the Standard Link and extends its capabilities:

- **Protocol Negotiation**: Dynamic feature discovery and version negotiation
- **Enhanced Flow Control**: Advanced backpressure and congestion control
- **Advanced Statistics**: Detailed metrics including compression ratios and timing
- **Quality of Service**: Priority handling and bandwidth management
- **Future Features**: Framework for compression, encryption, and reliability

## Protocol Negotiation

Advanced Links perform protocol negotiation to ensure compatibility and enable features:

### Negotiation Process

1. **Version Negotiation**: Both ends agree on the highest mutually supported protocol version
2. **Feature Negotiation**: Optional features are enabled based on mutual support
3. **Capability Exchange**: Each endpoint advertises its system capabilities

### Supported Features

```go
const (
    FeatureNone        uint64 = 0
    FeatureCompression uint64 = 1 << iota  // Data compression support
    FeatureEncryption                     // Data encryption support
    FeatureFlowControl                    // Advanced flow control
    FeatureStatistics                     // Enhanced statistics
    FeatureQoS                           // Quality of Service
)
```

### Supported Capabilities

```go
const (
    CapabilityNone         uint64 = 0
    CapabilityLargeBuffers uint64 = 1 << iota  // Support for large buffers
    CapabilityHighThroughput                    // High throughput mode
    CapabilityLowLatency                        // Low latency optimizations
    CapabilityReliableDelivery                  // Reliable delivery guarantees
)
```

### Negotiation Flow

1. **Primary Initiates**: Sends OpProtoNegotiate with desired version and features
2. **Secondary Responds**: Sends OpProtoAck with negotiated parameters
3. **Feature Selection**: Intersection of requested and supported features
4. **Version Selection**: Minimum of requested and supported versions

## Link Modes

### Primary Mode

- **Initialization**: Sets up the shared memory region and initializes structures
- **Memory Management**: Responsible for allocating and managing shared memory
- **Ring Initialization**: Creates and initializes MPMC rings and buffer pools
- **Leadership**: Acts as the primary endpoint in the communication pair

### Secondary Mode

- **Attachment**: Connects to existing shared memory region
- **Structure Usage**: Uses pre-initialized structures without modification
- **Memory Access**: Read/write access to shared memory but no layout changes
- **Followership**: Acts as the secondary endpoint in the communication pair

## Flow Control

### Buffer Management

- **Pre-allocation**: All buffers are allocated during initialization
- **Index-based Access**: Buffer indices are passed in packets, not memory addresses
- **Ownership Model**: Clear ownership semantics for buffer access
- **Reuse Strategy**: Buffers are returned to pool after use

### Backpressure Mechanisms

- **Ring Buffer Limits**: MPMC rings provide natural backpressure when full
- **Producer Throttling**: Producers must wait when rings are full
- **Consumer Pacing**: Consumers process packets at their own pace
- **Resource Protection**: Prevents resource exhaustion through bounded queues

## Error Handling

### Error Codes

| Error Code | Hex Value | Description |
|------------|-----------|-------------|
| ErrCodeMemoryAlign | 0x01 | Memory alignment violation |
| ErrCodeInvalidSize | 0x02 | Invalid buffer ring size |
| ErrCodeMemorySmall | 0x03 | Memory too small for configuration |
| ErrCodeFailedInit | 0x04 | Failed to initialize buffer ring |
| ErrCodeConnNotFound | 0x05 | Connection not found |
| ErrCodeInvalidOp | 0x06 | Invalid operation |
| ErrCodeBufferOverflow | 0x07 | Buffer overflow detected |

### Error Reporting

- **Synchronous Errors**: Immediate error responses for invalid operations
- **Asynchronous Errors**: Error packets for operational failures
- **Context Preservation**: Error packets include context for debugging
- **Recovery Strategies**: Defined recovery procedures for common errors

## Performance Considerations

### Lock-Free Design

- **Atomic Operations**: All shared state modifications use atomic primitives
- **Memory Barriers**: Proper memory ordering for cross-CPU visibility
- **Cache Optimization**: Cache-line padding to prevent false sharing
- **Wait-free Algorithms**: Algorithms that complete in bounded time

### Memory Alignment

- **Page Alignment**: All structures aligned to page boundaries
- **Buffer Alignment**: Buffer sizes are multiples of 8 bytes
- **Cache Efficiency**: Optimized for modern CPU cache hierarchies
- **NUMA Awareness**: Consideration for NUMA architectures

### Zero-Copy Transfers

- **Shared Memory**: Data remains in shared memory throughout transfer
- **Index Passing**: Only indices and metadata are copied between processes
- **Direct Access**: Both processes access the same physical memory
- **Bandwidth Efficiency**: Minimizes memory bandwidth usage

## Usage Examples

### Basic Data Transfer

#### Primary Process
```go
// Create shared memory region
size := sizeStandardLink(bufferCount, bufferSize)
shm, err := shm.Create(size)
if err != nil {
    log.Fatal(err)
}

// Initialize link
link, err := openStandardLink(uintptr(unsafe.Pointer(&shm[0])), bufferCount, bufferSize)
if err != nil {
    log.Fatal(err)
}

// Send data
packet := protocol.Packet{
    Op:       protocol.OpStandardLinkCopy,
    Operand0: copyID,
    Operand1: bufferIndex,
    Operand2: dataSize,
}
link.ring0.Enqueue(packet)
```

#### Secondary Process
```go
// Open existing shared memory region
shm, err := shm.Open(name)
if err != nil {
    log.Fatal(err)
}

// Attach to link
link, err := openStandardLink(uintptr(unsafe.Pointer(&shm[0])), bufferCount, bufferSize)
if err != nil {
    log.Fatal(err)
}

// Receive data
buf := make([]byte, bufferSize)
n, err := link.Read(buf)
if err != nil {
    log.Fatal(err)
}
```

### Connection-Oriented Communication

#### Connection Creation
```go
// Create connection
connID, err := link.CreateConnection(context.Background())
if err != nil {
    log.Fatal(err)
}

// Send data over connection
packet := protocol.Packet{
    Op:       protocol.OpConnCopy,
    Operand0: connID,
    Operand1: copyID,
    Operand2: dataSize,
    // ... other operands
}
link.ring0.Enqueue(packet)
```

### Advanced Link with Protocol Negotiation

#### Primary Process
```go
// Create advanced link
advLink, err := NewAdvancedLink(offset, bufferCount, bufferSize)
if err != nil {
    log.Fatal(err)
}

// Perform protocol negotiation
ctx := context.Background()
version := ProtocolVersion{Major: 1, Minor: 1}
features := FeatureCompression | FeatureEncryption

negotiated, err := advLink.NegotiateProtocol(ctx, version, features)
if err != nil {
    log.Fatal(err)
}

if !negotiated {
    log.Println("Negotiation failed, falling back to standard mode")
}
```

#### Secondary Process
```go
// Create advanced link
advLink, err := NewAdvancedLink(offset, bufferCount, bufferSize)
if err != nil {
    log.Fatal(err)
}

// Wait for negotiation request
ctx := context.Background()
negotiated, err := advLink.WaitForNegotiation(ctx)
if err != nil {
    log.Fatal(err)
}

if negotiated {
    log.Println("Advanced features negotiated successfully")
} else {
    log.Println("Using standard mode")
}
```

## Versioning

### Semantic Versioning

The protocol uses semantic versioning:
- **Major Version**: Incompatible API changes
- **Minor Version**: Backward-compatible feature additions
- **Patch Version**: Bug fixes and optimizations

### Version Negotiation

- **Backward Compatibility**: Newer versions support older protocol versions
- **Feature Detection**: Dynamic feature discovery through negotiation
- **Graceful Degradation**: Fallback to basic features when negotiation fails

### Current Version

- **Protocol Version**: 1.1
- **Major**: 1 (stable API)
- **Minor**: 1 (with protocol negotiation support)

## Security Considerations

### Shared Memory Access

- **Permission Control**: OS-level permissions control shared memory access
- **Process Isolation**: Only authorized processes can access shared memory
- **Data Protection**: Sensitive data should be encrypted when needed

### Protocol Security

- **Input Validation**: All packet operands are validated before use
- **Bounds Checking**: Buffer indices and sizes are strictly validated
- **Resource Limits**: Built-in protection against resource exhaustion

## Future Extensions

### Planned Features

1. **Compression Support**: Optional data compression for large payloads
2. **Encryption**: End-to-end encryption for sensitive data
3. **Reliability**: Acknowledgments and retransmission for guaranteed delivery
4. **Multicast**: One-to-many communication patterns
5. **Dynamic Buffering**: Adaptive buffer sizing based on workload

### Extension Mechanisms

- **Feature Flags**: Bitmask-based feature negotiation
- **Reserved Operands**: Unused operands for future extensions
- **Version Negotiation**: Framework for protocol evolution
- **Capability Exchange**: Dynamic capability discovery

## Implementation Notes

### Platform Considerations

- **Memory Alignment**: Different platforms have different alignment requirements
- **Atomic Operations**: Implementation varies across CPU architectures
- **Cache Behavior**: Cache coherency protocols affect performance
- **NUMA Effects**: Non-Uniform Memory Access impacts performance

### Optimization Techniques

- **Cache Line Padding**: Prevents false sharing between CPU cores
- **Memory Prefetching**: Improves cache hit rates for sequential access
- **Branch Prediction**: Optimizes conditional branches in hot paths
- **SIMD Instructions**: Vector operations for data processing

### Debugging Support

- **Packet Tracing**: Optional logging of all packet transfers
- **Statistics Collection**: Detailed metrics for performance analysis
- **Error Reporting**: Comprehensive error information for debugging
- **Memory Debugging**: Tools for detecting memory corruption and leaks