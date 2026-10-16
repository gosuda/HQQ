//go:build !linux && !darwin && !windows

package mpmc

import "time"

// parkingSupported is false on platforms without a cross-process
// compare-and-wait primitive usable from pure Go: waiters fall back to
// bounded exponential sleeps (see minSleep/maxSleep) unless the ring has a
// Parker.
const parkingSupported = false

// processLocalParking is false: there is no wait primitive at all, so
// futexWait and futexWake are no-ops and waiters sleep.
const processLocalParking = false

func futexWait(*uint32, uint32, time.Duration) {}

func futexWake(*uint32) {}
