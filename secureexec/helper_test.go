package secureexec

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
)

// helperRoot holds the helper binaries built by helperBinary for the whole run.
var helperRoot string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "secureexec-helper-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	helperRoot = dir
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

var (
	helperMu    sync.Mutex
	helperPaths = map[string]string{}
)

func exeSuffix() string {
	if runtime.GOOS == "windows" {
		return ".exe"
	}
	return ""
}

// helperBinary builds (once per marker) a tiny native program that prints
// marker, its os.Args[0], and its arguments, one per line. Different markers
// give different bytes, so tests can tell which executable actually ran.
func helperBinary(t *testing.T, marker string) string {
	t.Helper()
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go tool not on PATH; cannot build helper binary")
	}
	helperMu.Lock()
	defer helperMu.Unlock()
	if p, ok := helperPaths[marker]; ok {
		return p
	}
	dir := filepath.Join(helperRoot, marker)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	src := fmt.Sprintf(`package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Println(%q)
	fmt.Println(os.Args[0])
	for _, a := range os.Args[1:] {
		fmt.Println(a)
	}
}
`, marker)
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module helper\n\ngo 1.24\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "helper"+exeSuffix())
	cmd := exec.Command("go", "build", "-trimpath", "-o", out, ".")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOWORK=off", "GOFLAGS=", "GOTOOLCHAIN=local")
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build helper %s: %v\n%s", marker, err, b)
	}
	helperPaths[marker] = out
	return out
}

// copyExecutable copies src to dst with mode 0755.
func copyExecutable(t *testing.T, src, dst string) {
	t.Helper()
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, data, 0o755); err != nil {
		t.Fatal(err)
	}
}

// victimCopy returns a fresh copy of the helper named marker inside dir, and
// the digest of that copy.
func victimCopy(t *testing.T, marker, dir string) (string, Hash) {
	t.Helper()
	dst := filepath.Join(dir, "tool"+exeSuffix())
	copyExecutable(t, helperBinary(t, marker), dst)
	return dst, fileHash(t, dst)
}

func fileHash(t *testing.T, path string) Hash {
	t.Helper()
	h, _, err := HashFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

// runVerified runs v with the given args and returns the output lines.
func runVerified(t *testing.T, v *Verified, args ...string) []string {
	t.Helper()
	var out bytes.Buffer
	cmd := v.Command(context.Background(), args...)
	cmd.Stdout = &out
	cmd.Stderr = io.Discard
	if err := cmd.Run(); err != nil {
		t.Fatalf("run (%s): %v", v.Mechanism(), err)
	}
	return splitLines(out.String())
}

func splitLines(s string) []string {
	s = strings.ReplaceAll(strings.TrimSpace(s), "\r\n", "\n")
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}
