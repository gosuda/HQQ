//go:build !race

package mpmc

import (
	"testing"
	"unsafe"
)

// TestExclusiveGuardCompilesAway: outside race builds the concurrent-use
// guard has no storage, so it costs nothing on the hot path.
func TestExclusiveGuardCompilesAway(t *testing.T) {
	if n := unsafe.Sizeof(exclusiveGuard{}); n != 0 {
		t.Fatalf("exclusiveGuard occupies %d bytes in a non-race build", n)
	}
}
