# HQQ: Efficient Interprocess Communication

HQQ is a high-performance, lock-free inter-process communication (IPC) library designed for efficient data exchange between processes using shared memory and MPMC (Multi-Producer Multi-Consumer) queues.

## Overview

HQQ provides a zero-copy, bidirectional communication channel between processes with minimal overhead. It's designed for modern multi-core systems where performance and low latency are critical.

## Features

- **Lock-free Design**: Uses atomic primitives for maximum performance without mutexes or locks
- **Zero-copy Transfers**: Data remains in shared memory to minimize overhead and memory bandwidth usage
- **Bidirectional Communication**: Full-duplex communication channels with separate rings for each direction
- **Connection Management**: Built-in connection lifecycle management with state tracking
- **Statistics & Monitoring**: Comprehensive metrics for performance analysis and debugging
- **Memory Efficient**: Optimized memory layout with proper alignment to reduce cache misses
- **Protocol Negotiation**: Advanced links support feature negotiation for extensibility
- **PacketConn Interface**: Compatible with Go's net.PacketConn interface for network-like usage

## Architecture

HQQ uses a shared memory region organized as follows:

```
<<<<< PAGE_START
MPMC_RING (Primary to Secondary)     // Ring buffer for packets from primary to secondary
<<<<< PAGE_BREAK
BUFFERS (Primary to Secondary)       // Data buffers for primary to secondary communication
<<<<< PAGE_BREAK
MPMC_RING (Secondary to Primary)     // Ring buffer for packets from secondary to primary
<<<<< PAGE_BREAK
BUFFERS (Secondary to Primary)       // Data buffers for secondary to primary communication
<<<<< PAGE_END
```

### Key Components

1. **MPMC Rings**: Lock-free ring buffers for packet exchange between processes
2. **Buffer Pools**: Pre-allocated memory regions for storing actual data payloads
3. **Packet Structure**: Standardized message format for all communications
4. **Connection Management**: State tracking for logical connections between processes

## Link Types

### StandardLink

The StandardLink provides basic inter-process communication functionality:

- Bidirectional data transfer using shared memory
- Connection management with state tracking
- Error handling with specific error types
- Basic statistics for monitoring
- Implements io.Reader, io.Writer, and net.PacketConn interfaces

### AdvancedLink

The AdvancedLink extends StandardLink with additional capabilities:

- Protocol negotiation for feature discovery
- Enhanced flow control mechanisms
- Advanced statistics and metrics
- Quality of Service features
- Compression and encryption support (planned features)

## Quick Start

### Installation

```bash
go get gosuda.org/hqq
```

### Primary Process Example

```go
package main

import (
    "context"
    "log"
    "unsafe"
    
    "gosuda.org/hqq"
    "gosuda.org/hqq/internal/shm"
)

func main() {
    // Configuration
    bufferCount := 1024  // Must be even number
    bufferSize := 4096   // Must be multiple of 8
    
    // Calculate required memory size
    size := hqq.SizeStandardLink(bufferCount, bufferSize)
    
    // Create shared memory region
    shm, err := shm.Create(size)
    if err != nil {
        log.Fatal(err)
    }
    
    // Initialize link
    link, err := hqq.OpenStandardLink(uintptr(unsafe.Pointer(&shm[0])), bufferCount, bufferSize)
    if err != nil {
        log.Fatal(err)
    }
    defer link.Close()
    
    // Check if we're the primary process
    if link.GetMode() == hqq.LinkModePrimary {
        log.Println("Running as primary process")
        
        // Create a connection
        connID, err := link.CreateConnection(context.Background())
        if err != nil {
            log.Fatal(err)
        }
        
        // Send data
        data := []byte("Hello from primary process!")
        n, err := link.Write(data)
        if err != nil {
            log.Fatal(err)
        }
        log.Printf("Sent %d bytes", n)
        
        // Get statistics
        stats := link.GetStats()
        log.Printf("Stats: %+v", stats)
    }
}
```

### Secondary Process Example

```go
package main

import (
    "log"
    "unsafe"
    
    "gosuda.org/hqq"
    "gosuda.org/hqq/internal/shm"
)

func main() {
    // Open existing shared memory region
    shm, err := shm.Open("hqq_shared_memory")
    if err != nil {
        log.Fatal(err)
    }
    
    // Attach to link
    link, err := hqq.OpenStandardLink(uintptr(unsafe.Pointer(&shm[0])), 1024, 4096)
    if err != nil {
        log.Fatal(err)
    }
    defer link.Close()
    
    // Check if we're the secondary process
    if link.GetMode() == hqq.LinkModeSecondary {
        log.Println("Running as secondary process")
        
        // Receive data
        buf := make([]byte, 1024)
        n, err := link.Read(buf)
        if err != nil {
            log.Fatal(err)
        }
        log.Printf("Received: %s", string(buf[:n]))
    }
}
```

### Advanced Link with Protocol Negotiation

```go
package main

import (
    "context"
    "log"
    "unsafe"
    
    "gosuda.org/hqq"
)

func main() {
    // ... shared memory setup ...
    
    // Create advanced link
    advLink, err := hqq.NewAdvancedLink(offset, bufferCount, bufferSize)
    if err != nil {
        log.Fatal(err)
    }
    defer advLink.Close()
    
    // Check if we're the primary process
    if advLink.GetMode() == hqq.LinkModePrimary {
        // Perform protocol negotiation
        ctx := context.Background()
        version := hqq.ProtocolVersion{Major: 1, Minor: 1}
        features := hqq.FeatureCompression | hqq.FeatureFlowControl
        
        negotiated, err := advLink.NegotiateProtocol(ctx, version, features)
        if err != nil {
            log.Fatal(err)
        }
        
        if negotiated {
            log.Println("Protocol negotiation successful")
            log.Printf("Negotiated version: %d.%d", 
                advLink.GetNegotiatedVersion().Major,
                advLink.GetNegotiatedVersion().Minor)
            log.Printf("Compression enabled: %v", advLink.IsCompressionEnabled())
        }
    }
}
```

## API Reference

### Core Functions

#### OpenStandardLink
Creates or attaches to a standard link in shared memory.

```go
func OpenStandardLink(offset uintptr, bufferCount int, bufferSize int) (*StandardLink, error)
```

Parameters:
- `offset`: Memory offset of the shared memory region (must be page-aligned)
- `bufferCount`: Number of buffers (must be even and power of 2)
- `bufferSize`: Size of each buffer (must be multiple of 8)

#### SizeStandardLink
Calculates the required memory size for a standard link.

```go
func SizeStandardLink(bufferCount int, bufferSize int) uintptr
```

#### NewAdvancedLink
Creates a new advanced link with protocol negotiation capabilities.

```go
func NewAdvancedLink(offset uintptr, bufferCount int, bufferSize int) (*AdvancedLink, error)
```

### StandardLink Methods

#### Read
Reads data from the link.

```go
func (l *StandardLink) Read(b []byte) (n int, err error)
```

#### Write
Writes data to the link.

```go
func (l *StandardLink) Write(b []byte) (n int, err error)
```

#### CreateConnection
Creates a new connection with the peer process.

```go
func (l *StandardLink) CreateConnection(ctx context.Context) (uint64, error)
```

#### Close
Closes the link and cleans up resources.

```go
func (l *StandardLink) Close() error
```

#### GetStats
Returns link statistics.

```go
func (l *StandardLink) GetStats() LinkStats
```

#### GetMode
Returns the link mode (Primary or Secondary).

```go
func (l *StandardLink) GetMode() LinkMode
```

### AdvancedLink Methods

#### NegotiateProtocol
Initiates protocol negotiation with the peer.

```go
func (l *AdvancedLink) NegotiateProtocol(ctx context.Context, version ProtocolVersion, features uint64) (bool, error)
```

#### WaitForNegotiation
Waits for a protocol negotiation request from the peer.

```go
func (l *AdvancedLink) WaitForNegotiation(ctx context.Context) (bool, error)
```

#### GetProtocolState
Returns the current protocol negotiation state.

```go
func (l *AdvancedLink) GetProtocolState() ProtocolState
```

#### GetNegotiatedVersion
Returns the negotiated protocol version.

```go
func (l *AdvancedLink) GetNegotiatedVersion() ProtocolVersion
```

#### IsCompressionEnabled
Returns whether compression is enabled.

```go
func (l *AdvancedLink) IsCompressionEnabled() bool
```

## Protocol Specification

For detailed information about the HQQ protocol, including packet formats, operations, and error handling, see [PROTOCOL.md](PROTOCOL.md).

## Performance Considerations

### Buffer Configuration

- **Buffer Count**: Should be a power of 2 for optimal performance (e.g., 1024, 2048)
- **Buffer Size**: Must be a multiple of 8 bytes for proper alignment
- **Memory Alignment**: All structures are page-aligned to reduce cache misses

### Best Practices

1. Use appropriate buffer sizes based on your data patterns
2. Monitor link statistics to identify bottlenecks
3. Properly close connections when done to free resources
4. Handle errors gracefully to maintain system stability
5. Use context with timeouts for connection operations
6. Consider using AdvancedLink for production systems with protocol negotiation

### Performance Tips

1. **Batch Small Messages**: Combine small messages into larger buffers when possible
2. **Monitor Statistics**: Regularly check statistics to detect performance issues
3. **Use Appropriate Buffer Sizes**: Match buffer sizes to your typical message sizes
4. **Avoid Frequent Small Writes**: Minimize small write operations to reduce ring buffer contention

## Error Handling

HQQ provides comprehensive error handling with specific error types:

```go
var (
    ErrMemoryAlign     = errors.New("hqq: memory alignment violation")
    ErrInvalidSize     = errors.New("hqq: invalid buffer ring size")
    ErrMemorySmall     = errors.New("hqq: memory too small")
    ErrFailedInit      = errors.New("hqq: failed to initialize buffer ring")
    ErrConnNotFound    = errors.New("hqq: connection not found")
    ErrInvalidOp       = errors.New("hqq: invalid operation")
    ErrBufferOverflow  = errors.New("hqq: buffer overflow")
    ErrTimeout         = errors.New("hqq: operation timed out")
)
```

## Examples

See the `examples/` directory for complete working examples of HQQ usage, including:
- Basic IPC between processes
- Connection management
- Protocol negotiation
- Performance benchmarking

## Thread Safety

- StandardLink is safe for concurrent use by multiple goroutines
- All operations use atomic primitives and lock-free algorithms
- Statistics updates are protected by mutexes for consistency
- Connection management uses sync.Map for safe concurrent access

## Limitations

1. **Shared Memory Dependency**: Requires shared memory support on the target platform
2. **Buffer Size Limitation**: Single messages cannot exceed the configured buffer size
3. **Same Host Only**: Communication is limited to processes on the same host
4. **Platform Specific**: Some optimizations are platform-specific (Linux, macOS, Windows)

## Troubleshooting

### Common Issues

1. **Memory Alignment Errors**: Ensure shared memory is page-aligned
2. **Buffer Overflow**: Check that message sizes don't exceed buffer size
3. **Connection Timeouts**: Verify both processes are running and accessible
4. **Performance Issues**: Monitor statistics and adjust buffer configurations

### Debugging

1. Enable verbose logging to trace packet flow
2. Use statistics to identify bottlenecks
3. Check for memory alignment issues with tools like valgrind
4. Monitor system resources (CPU, memory) during operation

## Contributing

1. Fork the repository
2. Create a feature branch (`git checkout -b feature/amazing-feature`)
3. Make your changes
4. Add tests for new functionality
5. Ensure all tests pass (`go test ./...`)
6. Submit a pull request

### Development Guidelines

- Follow Go coding conventions
- Add comprehensive comments for public APIs
- Include tests for new functionality
- Update documentation for API changes
- Ensure backward compatibility when possible

## License

This project is licensed under the MIT License - see the [LICENSE](LICENSE) file for details.

## Acknowledgments

- Inspired by lock-free queue designs in high-performance computing
- Built with Go's unsafe package for zero-overhead memory access
- Designed for modern multi-core systems
- Thanks to all contributors who have helped improve this project

## Support

For questions, issues, or contributions:
- Create an issue on GitHub
- Check the documentation and examples
- Review the protocol specification for technical details
