package shm

// SharedMemory represents a shared memory region for inter-process communication
// This structure provides a cross-platform interface for creating and managing
// shared memory segments that can be accessed by multiple processes.
//
// Shared memory is the foundation of HQQ's high-performance IPC, allowing
// processes to exchange data without kernel-mediated copying.
type SharedMemory struct {
	name string  // Name/identifier of the shared memory region
	size int     // Size of the shared memory region in bytes
	fd   uintptr // File descriptor or handle for the shared memory region
}

// Name returns the name/identifier of the shared memory region
// This name is used to identify the shared memory segment across processes
func (s *SharedMemory) Name() string {
	return s.name
}

// Size returns the size of the shared memory region in bytes
// This is the total size allocated for the shared memory segment
func (s *SharedMemory) Size() int {
	return s.size
}

// FD returns the file descriptor or handle for the shared memory region
// The exact type and meaning of this value depends on the platform:
// - On Unix-like systems: file descriptor from shm_open() or mmap()
// - On Windows: handle from CreateFileMapping()
//
// This low-level handle is used by the MPMC ring buffer implementation
// to directly access the shared memory region
func (s *SharedMemory) FD() uintptr {
	return s.fd
}
