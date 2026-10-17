// Package launchtest is the stand-in server and the helpers shared by the tests
// of launch and sqlite/launcher.
//
// The tests start their own test binary as the server: TestMain calls Serve,
// which runs the stub instead of the tests when the environment selects a mode
// (EnvStub). The stub follows the process contract of the C servers: the
// listener name is the first argument, "ready <listener>" goes to stdout once
// it serves, and it exits when standard input reaches end of file (when it was
// given the lifeline flag) or on SIGTERM.
package launchtest

import (
	"crypto/ed25519"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"gosuda.org/hqq/secureexec"
	"gosuda.org/hqq/secureexec/manifest"
)

const (
	// EnvStub selects the stub mode; see Serve.
	EnvStub = "LAUNCHTEST_STUB"
	// EnvReport names a file the stub writes a JSON Report to as soon as it
	// starts.
	EnvReport = "LAUNCHTEST_REPORT"
	// EnvLifelineFlag is the argument that puts the stub in lifeline mode.
	// Unset: "--parent-stdin".
	EnvLifelineFlag = "LAUNCHTEST_LIFELINE_FLAG"
)

// Report is what the stub records about its own start.
type Report struct {
	PID  int
	Args []string // os.Args[1:]
	Dir  string   // working directory
	Env  []string
}

// Serve runs the stub and exits the process when EnvStub is set, and returns
// immediately otherwise. Call it first in TestMain.
//
// Modes:
//
//	ok                 ready, then wait for the stop signal; exit 0
//	noisy              output before the ready line, which is split across two writes, and after it
//	ignore-eof         ready, then ignore SIGTERM and stdin: only a kill stops it
//	no-ready           print "booting" on stderr, never ready
//	wrong-ready        print ready lines that are not exactly "ready <listener>", never ready
//	unterminated-ready print "ready <listener>" without the newline, never ready
//	exit-early         print "boom" on stderr, exit 3
//	crash-after-ready  ready, then exit 5
//	flood              write more than 4 KiB to stderr ending in "END", exit 4
func Serve() {
	mode := os.Getenv(EnvStub)
	if mode == "" {
		return
	}
	runStub(mode)
	syscall.Exit(0)
}

func runStub(mode string) {
	if p := os.Getenv(EnvReport); p != "" {
		dir, _ := os.Getwd()
		b, _ := json.Marshal(Report{PID: os.Getpid(), Args: os.Args[1:], Dir: dir, Env: os.Environ()})
		// Rename, so a reader that sees the file sees all of it.
		tmp := p + ".tmp"
		_ = os.WriteFile(tmp, b, 0o600)
		_ = os.Rename(tmp, p)
	}
	listener := ""
	if len(os.Args) > 1 {
		listener = os.Args[1]
	}
	flag := os.Getenv(EnvLifelineFlag)
	if flag == "" {
		flag = "--parent-stdin"
	}
	lifeline := slices.Contains(os.Args, flag)
	ready := func() { fmt.Printf("ready %s\n", listener) }

	switch mode {
	case "ok":
		wait := armStop(lifeline)
		ready()
		wait()
	case "noisy":
		wait := armStop(lifeline)
		fmt.Print("chatter\n")
		fmt.Fprint(os.Stderr, "warn\n")
		fmt.Print("rea")
		time.Sleep(50 * time.Millisecond)
		fmt.Printf("dy %s\n", listener)
		fmt.Print("after\n")
		wait()
	case "ignore-eof":
		signal.Ignore(syscall.SIGTERM)
		ready()
		time.Sleep(time.Hour)
	case "no-ready":
		fmt.Fprintln(os.Stderr, "booting")
		time.Sleep(time.Hour)
	case "wrong-ready":
		fmt.Printf("ready %sx\nready other\n", listener)
		time.Sleep(time.Hour)
	case "unterminated-ready":
		fmt.Printf("ready %s", listener)
		time.Sleep(time.Hour)
	case "exit-early":
		fmt.Fprintln(os.Stderr, "boom")
		syscall.Exit(3)
	case "crash-after-ready":
		ready()
		time.Sleep(100 * time.Millisecond)
		syscall.Exit(5)
	case "flood":
		for range 5 {
			fmt.Fprintln(os.Stderr, strings.Repeat("a", 1000))
		}
		fmt.Fprintln(os.Stderr, "END")
		syscall.Exit(4)
	default:
		fmt.Fprintln(os.Stderr, "unknown stub mode", mode)
		syscall.Exit(2)
	}
}

// armStop installs the SIGTERM handler (unless the lifeline is the stop
// signal) and returns the wait function. It must run before the ready line is
// printed: the launcher may send SIGTERM as soon as it sees that line.
func armStop(lifeline bool) func() {
	if lifeline {
		return func() { _, _ = io.Copy(io.Discard, os.Stdin) }
	}
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGTERM, os.Interrupt)
	return func() { <-ch }
}

// Binary describes the running test binary, which is the stub.
type Binary struct {
	Path string // symlink-resolved path, what secureexec.Verify needs
	Hash secureexec.Hash
	Size int64
}

// Self returns the digest and path of the running test binary.
func Self(t testing.TB) Binary {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	exe, err = filepath.EvalSymlinks(exe)
	if err != nil {
		t.Fatal(err)
	}
	h, n, err := secureexec.HashFile(exe)
	if err != nil {
		t.Fatal(err)
	}
	return Binary{Path: exe, Hash: h, Size: n}
}

// Env returns the test's environment plus EnvStub=mode and extra.
func Env(mode string, extra ...string) []string {
	return append(append(os.Environ(), EnvStub+"="+mode), extra...)
}

var listenerSeq atomic.Int64

// NewListener returns a short unique listener name (macOS limits names to 31
// bytes).
func NewListener() string {
	return fmt.Sprintf("lnch%x%x", os.Getpid()&0xfff, listenerSeq.Add(1))
}

// ReportFile returns a fresh path under t.TempDir() and the environment entry
// that makes the stub write its Report there.
func ReportFile(t testing.TB) (path, env string) {
	t.Helper()
	path = filepath.Join(t.TempDir(), "report.json")
	return path, EnvReport + "=" + path
}

// ReadReport reads the Report the stub wrote to path.
func ReadReport(t testing.TB, path string) Report {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("stub report: %v", err)
	}
	var r Report
	if err := json.Unmarshal(b, &r); err != nil {
		t.Fatalf("stub report %q: %v", b, err)
	}
	return r
}

// Exists reports whether path exists.
func Exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// WaitForFile blocks until path exists, or fails the test after timeout.
func WaitForFile(t testing.TB, path string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !Exists(path) {
		if time.Now().After(deadline) {
			t.Fatalf("%s did not appear within %v", path, timeout)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// WaitGone blocks until process pid is gone, or fails the test after timeout.
func WaitGone(t testing.TB, pid int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for ProbeProcess(pid) == nil {
		if time.Now().After(deadline) {
			t.Fatalf("process %d still running after %v", pid, timeout)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// SignedManifest returns manifest.json and its signature for one artifact.
func SignedManifest(t testing.TB, signer ed25519.PrivateKey, goos, goarch, name string, sum secureexec.Hash, size int64) (data, sig []byte) {
	t.Helper()
	m := &manifest.Manifest{
		Version: manifest.Version,
		Name:    "stub-server",
		Release: "v0.0.0-test",
		Files: []manifest.File{{
			OS: goos, Arch: goarch, Name: name, SHA256: sum, Size: size,
		}},
		Licenses: []string{"MIT"},
	}
	data, err := manifest.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	sig, err = manifest.Sign(data, signer)
	if err != nil {
		t.Fatal(err)
	}
	return data, sig
}
