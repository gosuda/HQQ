package launcher_test

// The unit tests start this test binary as a stand-in server. LAUNCHER_STUB
// selects the behaviour; TestMain runs it instead of the tests. The e2e tests
// use LAUNCHER_E2E_PARENT for a helper process that starts a real server and
// holds it until it is killed.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/signal"
	"slices"
	"syscall"
	"testing"
	"time"

	"gosuda.org/hqq/secureexec"
	"gosuda.org/hqq/sqlite/launcher"
)

const (
	envStub      = "LAUNCHER_STUB"       // stub mode, see runStub
	envMark      = "LAUNCHER_MARK"       // the stub creates this file when it starts
	envArgs      = "LAUNCHER_ARGS"       // the stub writes its arguments (JSON) here
	envE2EParent = "LAUNCHER_E2E_PARENT" // helper: start the real server and hold it
	envE2EBinary = "LAUNCHER_E2E_BINARY"
	envE2EDir    = "LAUNCHER_E2E_DIR"
)

func TestMain(m *testing.M) {
	if mode := os.Getenv(envStub); mode != "" {
		runStub(mode)
	}
	if os.Getenv(envE2EParent) != "" {
		runE2EParent()
	}
	os.Exit(m.Run())
}

// runStub behaves like hqq-sqlite-server for the launcher's needs. The listener
// name is the first argument, as in the real server.
func runStub(mode string) {
	if p := os.Getenv(envMark); p != "" {
		_ = os.WriteFile(p, []byte("started"), 0o600)
	}
	if p := os.Getenv(envArgs); p != "" {
		b, _ := json.Marshal(os.Args[1:])
		_ = os.WriteFile(p, b, 0o600)
	}
	listener := ""
	if len(os.Args) > 1 {
		listener = os.Args[1]
	}
	lifeline := slices.Contains(os.Args, "--parent-stdin")
	ready := func() { fmt.Printf("ready %s\n", listener) }

	switch mode {
	case "ok":
		ready()
		waitStop(lifeline)
	case "noisy":
		// Output before the ready line is forwarded; the ready line itself is
		// split across two writes, which the scanner must still recognise.
		fmt.Print("chatter\n")
		fmt.Fprint(os.Stderr, "warn\n")
		fmt.Print("rea")
		time.Sleep(50 * time.Millisecond)
		fmt.Printf("dy %s\n", listener)
		fmt.Print("after\n")
		waitStop(lifeline)
	case "ignore-eof":
		// Never shuts down on its own: Stop must kill it.
		signal.Ignore(syscall.SIGTERM)
		ready()
		time.Sleep(time.Hour)
	case "no-ready":
		fmt.Fprintln(os.Stderr, "booting")
		time.Sleep(time.Hour)
	case "exit-early":
		fmt.Fprintln(os.Stderr, "boom")
		syscall.Exit(3)
	case "crash-after-ready":
		ready()
		time.Sleep(100 * time.Millisecond)
		syscall.Exit(5)
	default:
		fmt.Fprintln(os.Stderr, "unknown stub mode", mode)
		syscall.Exit(2)
	}
	syscall.Exit(0)
}

// waitStop returns when stdin reaches end of file (lifeline) or on SIGTERM.
func waitStop(lifeline bool) {
	if lifeline {
		_, _ = io.Copy(io.Discard, os.Stdin)
		return
	}
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGTERM, os.Interrupt)
	<-ch
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
