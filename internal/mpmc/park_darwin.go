//go:build darwin

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

// Darwin ulock interface (xnu bsd/sys/ulock.h). These system calls back
// os_sync_wait_on_address and libdispatch; the syscall numbers are stable
// since macOS 10.12.
const (
	sysUlockWait = 515
	sysUlockWake = 516

	// ulCompareAndWaitShared waits on a 32-bit value that may be shared
	// between processes (for example in a MAP_SHARED mapping).
	ulCompareAndWaitShared = 3
	ulfWakeAll             = 0x00000100
)

// futexWait blocks while *addr == val, for at most timeout. Spurious wake-ups
// are allowed; callers re-check their condition.
func futexWait(addr *uint32, val uint32, timeout time.Duration) {
	us := timeout.Microseconds()
	if us <= 0 {
		us = 1
	} else if us > math.MaxUint32 {
		us = math.MaxUint32
	}
	_, _, _ = syscall.Syscall6(sysUlockWait,
		ulCompareAndWaitShared, uintptr(unsafe.Pointer(addr)), uintptr(val),
		uintptr(us), 0, 0)
}

// futexWake wakes every waiter blocked on addr.
func futexWake(addr *uint32) {
	_, _, _ = syscall.Syscall(sysUlockWake,
		ulCompareAndWaitShared|ulfWakeAll, uintptr(unsafe.Pointer(addr)), 0)
}
