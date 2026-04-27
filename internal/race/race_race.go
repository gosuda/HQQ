//go:build race

package race

import _ "unsafe"

//go:linkname Disable internal/race.Disable
func Disable()

//go:linkname Enable internal/race.Enable
func Enable()

func Do(fn func()) {
	Disable()
	defer Enable()
	fn()
}

// Copy performs a byte copy without race instrumentation for the copy itself.
//
// RaceDisable/Enable only suppress synchronization-event handling, not ordinary
// memory-access instrumentation. Keep intentional shared-memory payload copies
// in tiny //go:norace helpers so the rest of the StandardLink remains visible
// to the race detector.
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
