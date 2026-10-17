package gen

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestCServerMain links the generated microlib glue with the main.c of 9.6 (hqqrpc_main), starts the server on a
// listener name, waits for its ready line (9.6, "Starting the server from Go") and stops it with SIGTERM: the process must
// end with exit code 0.
func TestCServerMain(t *testing.T) {
	spec := parseSpec(t, "bindgen/testdata/microlib/microlib.idl")
	dir := t.TempDir()
	writeFiles(t, dir, genC(t, spec, "microlib.idl"))
	micro := filepath.Join(repoRoot(t), "bindgen", "testdata", "microlib")
	exe := buildProgram(t, dir, []string{micro, cTestdata(t)}, nil,
		[]string{filepath.Join(dir, "microlib_server.c"), cTestdata(t, "microlib_hooks.c"), cTestdata(t, "microlib_main.c"), filepath.Join(micro, "microlib.c")}, nil)

	name := fmt.Sprintf("bgce%d", os.Getpid()%100000)
	cmd := exec.Command(exe, name, "--max-sessions", "2")
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	defer cmd.Process.Kill()

	line := make(chan string, 1)
	go func() {
		s, _ := bufio.NewReader(out).ReadString('\n')
		line <- s
	}()
	select {
	case l := <-line:
		if want := "ready " + name; strings.TrimSpace(l) != want {
			t.Fatalf("first line %q, want %q", l, want)
		}
	case err := <-done:
		t.Fatalf("the server ended before it was ready: %v", err)
	case <-time.After(15 * time.Second):
		t.Fatal("no ready line within 15 s")
	}
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("the server did not end cleanly after SIGTERM: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("the server did not stop within 15 s of SIGTERM")
	}
}
