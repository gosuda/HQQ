package gen

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestGoGeneratedBehavior compiles the generated clients against a scripted stand-in for bindgen/rt
// (testdata/go/behavior) and runs tests against them: the vector session of 4.14 through the microlib
// client, and hand-computed requests and responses for the sqlite excerpt and for edge.idl, which covers
// every parameter and return shape. The stand-in fills every request body with 0xA5 and refuses a second
// decoder, so missing padding and an unreleased Dec both fail. It reuses the real codec helpers.
func TestGoGeneratedBehavior(t *testing.T) {
	if testing.Short() {
		t.Skip("runs the go tool")
	}
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("no go tool in PATH")
	}
	read := func(path string) []byte {
		t.Helper()
		b, err := os.ReadFile(filepath.FromSlash(path))
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	root := t.TempDir()
	put := func(rel string, data []byte) {
		t.Helper()
		full := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	put("go.mod", []byte("module fakemod\n\ngo 1.24\n"))
	put("rt/fake.go", read("testdata/go/behavior/fake_rt.go.txt"))
	put("rt/codec.go", read("../rt/codec.go"))

	common := read("testdata/go/behavior/common_test.go.txt")
	drivers := map[string]string{
		"microlib":        "microlib_test.go.txt",
		"sqlite_lowlevel": "sqlite_test.go.txt",
		"edge":            "edge_test.go.txt",
	}
	for _, c := range goldenCases() {
		spec, strip := c.load(t)
		client := generateGo(t, spec, strip, c.source)
		client = bytes.ReplaceAll(client, []byte(`"gosuda.org/hqq/bindgen/rt"`), []byte(`"fakemod/rt"`))
		pkg := spec.Service
		put(c.name+"/"+pkg+"_client.go", client)
		put(c.name+"/common_test.go", bytes.ReplaceAll(common, []byte("package PKG"), []byte("package "+pkg)))
		driver := read("testdata/go/behavior/" + drivers[c.name])
		put(c.name+"/driver_test.go", bytes.ReplaceAll(driver, []byte("package PKG"), []byte("package "+pkg)))
		if c.name == "microlib" {
			put(c.name+"/vectors.json", read("../testdata/vectors.json"))
		}
	}

	cmd := exec.Command(goBin, "test", "-count=1", "./...")
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOWORK=off", "GOFLAGS=", "GOTOOLCHAIN=local")
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Run(); err != nil {
		t.Fatalf("go test in the scripted module: %v\n%s", err, limitLines(out.String(), 120))
	}
}

func limitLines(s string, n int) string {
	lines := strings.Split(s, "\n")
	if len(lines) > n {
		lines = append(lines[:n], "...")
	}
	return strings.Join(lines, "\n")
}
