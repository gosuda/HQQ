package launcher_test

// The unit tests start this test binary as a stand-in server
// (internal/launchtest). The e2e tests use LAUNCHER_E2E_PARENT for a helper
// process that starts a real server and holds it until it is killed.

import (
	"context"
	"fmt"
	"os"
	"syscall"
	"testing"
	"time"

	"gosuda.org/hqq/internal/launchtest"
	"gosuda.org/hqq/secureexec"
	"gosuda.org/hqq/sqlite/launcher"
)

const (
	envE2EParent = "LAUNCHER_E2E_PARENT" // helper: start the real server and hold it
	envE2EBinary = "LAUNCHER_E2E_BINARY"
	envE2EDir    = "LAUNCHER_E2E_DIR"
)

func TestMain(m *testing.M) {
	launchtest.Serve()
	if os.Getenv(envE2EParent) != "" {
		runE2EParent()
	}
	os.Exit(m.Run())
}

// runE2EParent starts the real server with a lifeline, prints its pid and
// listener name, and holds it until the test kills this process.
func runE2EParent() {
	bin := os.Getenv(envE2EBinary)
	h, _, err := secureexec.HashFile(bin)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		syscall.Exit(2)
	}
	srv, err := launcher.Start(context.Background(), launcher.Config{
		Binary:       bin,
		SHA256:       h,
		AllowDir:     os.Getenv(envE2EDir),
		ExtraArgs:    e2eServerFlags,
		ReadyTimeout: 10 * time.Second,
		StopTimeout:  10 * time.Second,
		Stderr:       os.Stderr,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		syscall.Exit(2)
	}
	fmt.Printf("pid %d listener %s\n", srv.PID(), srv.Listener())
	time.Sleep(time.Hour)
	syscall.Exit(0)
}
