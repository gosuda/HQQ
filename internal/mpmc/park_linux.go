//go:build linux

package mpmc

import (
	"math"
	"syscall"
	"time"
	"unsafe"
)

// parkingSupported reports whether futexWait/futexWake block and wake across
// processes on this platform.
const parkingSupported = true

// processLocalParking reports whether futexWait/futexWake block and wake at
// least between threads of one process. It is implied by parkingSupported.
const processLocalParking = true

const (
	// Shared (non-private) futex operations: the wait word lives in a
	// MAP_SHARED mapping and waiters may belong to different processes.
	futexWaitOp = 0 // FUTEX_WAIT
	futexWakeOp = 1 // FUTEX_WAKE
)

// futexWait blocks while *addr == val, for at most timeout. Spurious wake-ups
// are allowed; callers re-check their condition.
func futexWait(addr *uint32, val uint32, timeout time.Duration) {
	ts := syscall.NsecToTimespec(int64(timeout))
	_, _, _ = syscall.Syscall6(syscall.SYS_FUTEX,
		uintptr(unsafe.Pointer(addr)), futexWaitOp, uintptr(val),
		uintptr(unsafe.Pointer(&ts)), 0, 0)
}

// futexWake wakes every waiter blocked on addr.
func futexWake(addr *uint32) {
	_, _, _ = syscall.Syscall6(syscall.SYS_FUTEX,
		uintptr(unsafe.Pointer(addr)), futexWakeOp, math.MaxInt32, 0, 0, 0)
}
