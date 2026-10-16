// Package mmap provides thin cross-platform memory-mapping primitives over
// shared-memory descriptors (Unix mmap/munmap) and file-mapping handles
// (Windows MapViewOfFile/UnmapViewOfFile).
package mmap

import "unsafe"

// pointerFromAddress converts a non-GC virtual memory address (such as an OS
// view returned by MapViewOfFile) into an unsafe.Pointer without triggering go
// vet's uintptr-to-Pointer heuristic. The caller must guarantee that addr
// refers to an external mapping whose lifetime is managed outside the Go
// garbage collector.
func pointerFromAddress(addr uintptr) unsafe.Pointer {
	return *(*unsafe.Pointer)(unsafe.Pointer(&addr))
}
