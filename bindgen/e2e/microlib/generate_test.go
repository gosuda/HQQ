package microlib_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// bindgenCmd is the command of the go:generate lines.
const bindgenCmd = "gosuda.org/hqq/cmd/hqq-bindgen"

// generateArgs returns the arguments of the hqq-bindgen command of the //go:generate line in
// dir/generate.go. Running exactly that line, with -check, is what keeps the wiring (flags, paths) and
// the committed files honest at once.
func generateArgs(t *testing.T, dir string) []string {
	t.Helper()
	src, err := os.ReadFile(filepath.Join(dir, "generate.go"))
	if err != nil {
		t.Fatalf("reading generate.go: %v", err)
	}
	var found []string
	for _, line := range strings.Split(string(src), "\n") {
		rest, ok := strings.CutPrefix(line, "//go:generate ")
		if !ok {
			continue
		}
		f := strings.Fields(rest)
		if len(f) < 4 || f[0] != "go" || f[1] != "run" || f[2] != bindgenCmd {
			t.Fatalf("%s/generate.go: %q is not a `go run %s ...` line", dir, line, bindgenCmd)
		}
		found = append(found, f[3:]...)
	}
	if found == nil {
		t.Fatalf("%s/generate.go has no //go:generate line", dir)
	}
	return found
}

// goTool returns the go command or skips the test.
func goTool(t *testing.T) string {
	t.Helper()
	p, err := exec.LookPath("go")
	if err != nil {
		t.Skip("the go command is not in PATH")
	}
	return p
}

// TestGeneratedFilesAreFresh runs the //go:generate line of each package with -check: the committed
// generated files (the Go client, the C server glue, and the v2 client) must equal what the generator
// writes from the IDL today.
func TestGeneratedFilesAreFresh(t *testing.T) {
	goBin := goTool(t)
	for _, dir := range []string{".", "v2"} {
		t.Run(dir, func(t *testing.T) {
			args := append([]string{"run", bindgenCmd, "-check"}, generateArgs(t, dir)...)
			cmd := exec.Command(goBin, args...)
			cmd.Dir = dir
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("the committed generated files are stale (run `go generate ./bindgen/e2e/microlib/...`): %v\n%s", err, out)
			}
		})
	}
}

// TestStaleGeneratedFileIsDetected proves that the freshness check is not vacuous: with one byte of one
// committed file changed in a copy, -check fails and names the file.
func TestStaleGeneratedFileIsDetected(t *testing.T) {
	goBin := goTool(t)
	tmp := t.TempDir()
	for _, name := range []string{"microlib_client.go", "server/microlib_server.h", "server/microlib_server.c"} {
		b, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("reading %s: %v", name, err)
		}
		if err := os.MkdirAll(filepath.Dir(filepath.Join(tmp, name)), 0o755); err != nil {
			t.Fatal(err)
		}
		if name == "server/microlib_server.c" {
			b = append(b, "/* stale */\n"...)
		}
		if err := os.WriteFile(filepath.Join(tmp, name), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	idlPath, err := filepath.Abs("../../testdata/microlib/microlib.idl")
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(goBin, "run", bindgenCmd, "-check", "-go-out", tmp, "-go-strip", "ml_", "-c-out", filepath.Join(tmp, "server"), idlPath)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("-check accepted a modified server/microlib_server.c:\n%s", out)
	}
	if !strings.Contains(string(out), "microlib_server.c is out of date") || strings.Contains(string(out), "microlib_client.go") {
		t.Fatalf("-check output does not name exactly the stale file:\n%s", out)
	}
}
