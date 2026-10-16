//go:build unix

package mpmc_test

import (
	"os"
	"syscall"
	"testing"
	"time"

	"gosuda.org/hqq/internal/mpmc"
)

func processCPUTime(t *testing.T) time.Duration {
	t.Helper()
	var ru syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &ru); err != nil {
		t.Fatalf("getrusage: %v", err)
	}
	return time.Duration(ru.Utime.Nano() + ru.Stime.Nano())
}

// requirePerfTests skips CPU-ratio tests unless HQQ_PERF_TESTS=1: they compare
// process CPU time to wall time, which a loaded or noisy machine can skew.
func requirePerfTests(t *testing.T) {
	t.Helper()
	if os.Getenv("HQQ_PERF_TESTS") != "1" {
		t.Skip("CPU-ratio test; set HQQ_PERF_TESTS=1 to run")
	}
}

// TestMPMCIdleWaitDoesNotBurnCPU guards against regressions to busy-wait
// behavior: a consumer blocked on an empty ring must park instead of spinning.
func TestMPMCIdleWaitDoesNotBurnCPU(t *testing.T) {
	requirePerfTests(t)
	buffer, r := newRing(t, 8)
	defer func() { _ = buffer }()

	const wait = 500 * time.Millisecond
	before := processCPUTime(t)
	start := time.Now()
	_, err := r.ReserveConsumerCancel(&mpmc.Cancel{Deadline: start.Add(wait)})
	wall := time.Since(start)
	cpu := processCPUTime(t) - before
	if err == nil {
		t.Fatal("ReserveConsumerCancel succeeded on an empty ring")
	}
	t.Logf("blocked for %v wall, %v process CPU (%.1f%%)", wall, cpu, 100*float64(cpu)/float64(wall))
	// The whole test process (including the Go runtime) must stay well
	// below one busy core. A spinning waiter would consume ~100%.
	if cpu > wall/4 {
		t.Fatalf("idle wait used %v CPU over %v wall; waiter is busy-spinning", cpu, wall)
	}
}

// TestMPMCWaitSpinBurnsCPU documents the trade-off of WaitSpin.
func TestMPMCWaitSpinBurnsCPU(t *testing.T) {
	requirePerfTests(t)
	buffer, r := newRing(t, 8)
	defer func() { _ = buffer }()
	r.SetWaitPolicy(mpmc.WaitSpin)

	const wait = 200 * time.Millisecond
	before := processCPUTime(t)
	start := time.Now()
	_, _ = r.ReserveConsumerCancel(&mpmc.Cancel{Deadline: start.Add(wait)})
	wall := time.Since(start)
	cpu := processCPUTime(t) - before
	t.Logf("WaitSpin blocked for %v wall, %v process CPU", wall, cpu)
	if cpu < wall/2 {
		t.Fatalf("WaitSpin used only %v CPU over %v wall; expected busy waiting", cpu, wall)
	}
}
