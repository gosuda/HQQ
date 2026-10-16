//go:build !linux && !darwin

package mpmc

import "time"

// parkingSupported is false on platforms without a cross-process
// compare-and-wait primitive usable from pure Go. On Windows, WaitOnAddress
// only works between threads of one process, so waiters fall back to
// bounded exponential sleeps (see minSleep/maxSleep).
const parkingSupported = false

func futexWait(*uint32, uint32, time.Duration) {}

func futexWake(*uint32) {}
