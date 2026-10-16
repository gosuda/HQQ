//go:build linux

package mpmc

import (
	"encoding/json"
	"math"
	"os"
	"testing"
)

// TestSpecLinuxFutexConstants pins the futex operations that implement the
// shared wait word on Linux (SPEC.md, "Wait / wake protocol").
func TestSpecLinuxFutexConstants(t *testing.T) {
	raw, err := os.ReadFile("../../spec/constants.json")
	if err != nil {
		t.Fatal(err)
	}
	var c struct {
		Platform struct {
			Linux struct {
				WaitOp    int `json:"futex_wait_op"`
				WakeOp    int `json:"futex_wake_op"`
				WakeCount int `json:"futex_wake_count"`
			} `json:"linux"`
		} `json:"platform"`
	}
	if err := json.Unmarshal(raw, &c); err != nil {
		t.Fatal(err)
	}
	specEq(t, "futexWaitOp (shared, no FUTEX_PRIVATE_FLAG)", futexWaitOp, c.Platform.Linux.WaitOp)
	specEq(t, "futexWakeOp (shared, no FUTEX_PRIVATE_FLAG)", futexWakeOp, c.Platform.Linux.WakeOp)
	specEq(t, "futex wake count", math.MaxInt32, c.Platform.Linux.WakeCount)
}
