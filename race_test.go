//go:build race

package hqq

// raceEnabled reports whether the race detector is on: sync.Pool randomly
// drops items then, so allocation counts are not meaningful.
const raceEnabled = true
