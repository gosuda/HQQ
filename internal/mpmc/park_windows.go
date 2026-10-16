//go:build windows

package mpmc

import (
	"math"
	"syscall"
	"time"
	"unsafe"
)

// parkingSupported is false: WaitOnAddress and WakeByAddressAll only match
// waiters and wakers that live in the same process, so a waker in another
// process never wakes a parked waiter. Cross-process parking needs a Parker
// (named kernel semaphores, see SetParker).
const parkingSupported = false

// processLocalParking is true: futexWait/futexWake wake waiters of this
// process immediately. A waiter that may also be woken from another process
// bounds its wait by a short backoff step (see minSleep/maxSleep).
const processLocalParking = true

// WaitOnAddress lives in the API set that KernelBase forwards to; it is the
// same DLL the Go runtime itself imports it from.
var (
	synch             = syscall.NewLazyDLL("api-ms-win-core-synch-l1-2-0.dll")
	procWaitOnAddress = synch.NewProc("WaitOnAddress")
	procWakeByAddrAll = synch.NewProc("WakeByAddressAll")
)

// futexWait blocks while *addr == val, for at most timeout (rounded up to a
// whole millisecond, the resolution of WaitOnAddress). Spurious wake-ups are
// allowed; callers re-check their condition.
func futexWait(addr *uint32, val uint32, timeout time.Duration) {
	ms := (timeout + time.Millisecond - 1) / time.Millisecond
	switch {
	case ms < 1:
		ms = 1
	case ms >= math.MaxUint32:
		ms = math.MaxUint32 - 1 // INFINITE is 0xFFFFFFFF
	}
	_, _, _ = procWaitOnAddress.Call(
		uintptr(unsafe.Pointer(addr)),
		uintptr(unsafe.Pointer(&val)),
		unsafe.Sizeof(val),
		uintptr(ms),
	)
}

// futexWake wakes every waiter of this process blocked on addr.
func futexWake(addr *uint32) {
	_, _, _ = procWakeByAddrAll.Call(uintptr(unsafe.Pointer(addr)))
}
