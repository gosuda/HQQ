//go:build !race

// Package race contains copy helpers for intentional, protocol-synchronized
// accesses to shared memory. Under the race detector these helpers are
// compiled with //go:norace (see race_race.go) because the detector cannot
// see the cross-process happens-before edges established by the ring
// sequence numbers; everywhere else they are plain copy/clear.
package race

// Copy copies src into dst and returns the number of bytes copied.
func Copy(dst, src []byte) int {
	return copy(dst, src)
}

// Zero clears b.
func Zero(b []byte) {
	clear(b)
}
