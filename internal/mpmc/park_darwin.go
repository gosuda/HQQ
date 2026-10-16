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

// processLocalParking reports whether futexWait/futexWake block and wake at
// least between threads of one process. It is implied by parkingSupported.
const processLocalParking = true

// Darwin ulock interface (xnu bsd/sys/ulock.h).
//
// PLATFORM RISK: __ulock_wait and __ulock_wake are private Darwin interfaces
// used by libdispatch and the system libraries. Apple documents neither the
// system call numbers nor the operation codes; they have been stable since
// macOS 10.12 but may change or be restricted in any release. They are invoked
// as raw system calls because a cgo-free Go build cannot call libSystem
// wrappers. The public, supported alternative is os_sync_wait_on_address /
// os_sync_wake_by_address_all (<os/os_sync_wait_on_address.h>), available
// since macOS 14.4. futexWait ignores errors, so if the system calls ever
// stop working, parking degrades to busy re-checking (no hang, no lost data)
// rather than failing loudly.
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
