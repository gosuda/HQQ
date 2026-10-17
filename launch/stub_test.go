package launch_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"gosuda.org/hqq/internal/launchtest"
	"gosuda.org/hqq/launch"
	"gosuda.org/hqq/secureexec"
)

// The tests start this test binary as the server (internal/launchtest).
// LAUNCH_TEST_PARENT selects a helper process that starts the stub through
// launch.Start and holds it until it is killed.
const envParent = "LAUNCH_TEST_PARENT"

func TestMain(m *testing.M) {
	launchtest.Serve()
	if os.Getenv(envParent) != "" {
		runParent()
	}
	os.Exit(m.Run())
}

// runParent starts the stub with a lifeline, prints its pid and holds it until
// the test kills this process.
func runParent() {
	exe, err := os.Executable()
	if err == nil {
		exe, err = filepath.EvalSymlinks(exe)
	}
	var sum secureexec.Hash
	if err == nil {
		sum, _, err = secureexec.HashFile(exe)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		syscall.Exit(2)
	}
	listener := launchtest.NewListener()
	srv, err := launch.Start(context.Background(), launch.Config{
		Binary:       exe,
		SHA256:       sum,
		Listener:     listener,
		Args:         []string{listener},
		Env:          launchtest.Env("ok"),
		ReadyTimeout: 10 * time.Second,
		Stderr:       os.Stderr,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		syscall.Exit(2)
	}
	fmt.Printf("pid %d\n", srv.PID())
	time.Sleep(time.Hour)
	syscall.Exit(0)
}
