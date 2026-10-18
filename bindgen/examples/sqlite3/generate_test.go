package sqlite3_test

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// bindgenCmd is the command of the go:generate line.
const bindgenCmd = "gosuda.org/hqq/cmd/hqq-bindgen"

// TestGeneratedFilesAreFresh runs the //go:generate line of generate.go with -check: the committed Go client
// and C glue must equal what the generator writes from sqlite3.idl today. Running exactly that line
// keeps the flags, the paths and the committed files honest at once.
func TestGeneratedFilesAreFresh(t *testing.T) {
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("the go command is not in PATH")
	}
	src, err := os.ReadFile("generate.go")
	if err != nil {
		t.Fatal(err)
	}
	var args []string
	for _, line := range strings.Split(string(src), "\n") {
		rest, ok := strings.CutPrefix(line, "//go:generate ")
		if !ok {
			continue
		}
		f := strings.Fields(rest)
		if len(f) < 4 || f[0] != "go" || f[1] != "run" || f[2] != bindgenCmd {
			t.Fatalf("generate.go: %q is not a `go run %s ...` line", line, bindgenCmd)
		}
		args = f[3:]
	}
	if args == nil {
		t.Fatal("generate.go has no //go:generate line")
	}
	cmd := exec.Command(goBin, append([]string{"run", bindgenCmd, "-check"}, args...)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("hqq-bindgen -check: %v\n%s\nrun `go generate ./bindgen/examples/sqlite3/...`", err, out)
	}
}
