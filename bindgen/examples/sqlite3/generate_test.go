package sqlite3_test

import (
	"os"
	"os/exec"
	"path/filepath"
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

// TestPolicyIsNotForked checks that the server takes the path and open policy from the hand-written server's
// sources: the CMake file compiles them through a relative path, and this directory holds no copy.
func TestPolicyIsNotForked(t *testing.T) {
	cm, err := os.ReadFile(filepath.Join("server", "CMakeLists.txt"))
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"sqlsrv_pathrules.c", "sqlsrv_policy.c", "sqlsrv_proto.c", "sqlsrv_port_posix.c"} {
		if !strings.Contains(string(cm), `"${SQLSRV_SRC}/`+f+`"`) {
			t.Errorf("server/CMakeLists.txt does not compile %s from the hand-written server", f)
		}
		orig := filepath.Join("..", "..", "..", "examples", "sqlite", "server", "src", f)
		if _, err := os.Stat(orig); err != nil {
			t.Errorf("the hand-written source %s is missing: %v", orig, err)
		}
	}
	if !strings.Contains(string(cm), "../../../../examples/sqlite/server/src") {
		t.Error("server/CMakeLists.txt does not point at examples/sqlite/server/src")
	}
	err = filepath.WalkDir(".", func(path string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() && strings.HasPrefix(d.Name(), "sqlsrv_") {
			t.Errorf("%s: a copy of a hand-written server file", path)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}
