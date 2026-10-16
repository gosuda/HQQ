//go:build race

package race

// Copy performs a byte copy without race instrumentation for the copy itself.
//
// Shared-memory payload copies are ordered by the ring's atomic sequence
// numbers, possibly across processes, which the race detector cannot observe.
// Keeping them in tiny //go:norace helpers leaves the rest of the package
// fully visible to the race detector.
//
//go:norace
func Copy(dst, src []byte) int {
	return copy(dst, src)
}

// Zero clears a shared-memory byte range without race instrumentation.
//
//go:norace
func Zero(b []byte) {
	clear(b)
}
