//go:build darwin

package mpmc

import (
	"encoding/json"
	"os"
	"testing"
)

// TestSpecDarwinUlockConstants pins the ulock interface that implements the
// shared wait word on macOS (SPEC.md, "Wait / wake protocol").
func TestSpecDarwinUlockConstants(t *testing.T) {
	raw, err := os.ReadFile("../../spec/constants.json")
	if err != nil {
		t.Fatal(err)
	}
	var c struct {
		Platform struct {
			Darwin struct {
				WaitSyscall int `json:"ulock_wait_syscall"`
				WakeSyscall int `json:"ulock_wake_syscall"`
				Shared      int `json:"ul_compare_and_wait_shared"`
				WakeAll     int `json:"ulf_wake_all"`
			} `json:"darwin"`
		} `json:"platform"`
	}
	if err := json.Unmarshal(raw, &c); err != nil {
		t.Fatal(err)
	}
	specEq(t, "sysUlockWait", sysUlockWait, c.Platform.Darwin.WaitSyscall)
	specEq(t, "sysUlockWake", sysUlockWake, c.Platform.Darwin.WakeSyscall)
	specEq(t, "ulCompareAndWaitShared", ulCompareAndWaitShared, c.Platform.Darwin.Shared)
	specEq(t, "ulfWakeAll", ulfWakeAll, c.Platform.Darwin.WakeAll)
}
