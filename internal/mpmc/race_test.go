//go:build race

package mpmc

// raceEnabled reports whether the race detector is on: sync.Pool then drops
// items at random, so pooled steady-state allocation counts are not exact.
const raceEnabled = true
