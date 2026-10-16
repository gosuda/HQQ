package interop

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// envImpls names the implementations: "label=path,label=path".
const envImpls = "HQQ_INTEROP_IMPLS"

// listTimeout bounds one `hqq-interop list` call.
const listTimeout = time.Minute

// implSpec is one entry of HQQ_INTEROP_IMPLS. An empty path asks for a Go
// build of the module (label go only).
type implSpec struct {
	label string
	path  string
}

// parseImplSpecs parses HQQ_INTEROP_IMPLS. An empty value means "go=" (build
// the Go helper).
func parseImplSpecs(v string) ([]implSpec, error) {
	if strings.TrimSpace(v) == "" {
		return []implSpec{{label: "go"}}, nil
	}
	var specs []implSpec
	seen := make(map[string]bool)
	for _, entry := range strings.Split(v, ",") {
		entry = strings.TrimSpace(entry)
		label, path, ok := strings.Cut(entry, "=")
		if !ok {
			return nil, fmt.Errorf("%s entry %q: want label=path", envImpls, entry)
		}
		label = strings.TrimSpace(label)
		path = strings.TrimSpace(path)
		if !validLabel(label) {
			return nil, fmt.Errorf("%s entry %q: label must match [a-z0-9]+", envImpls, entry)
		}
		if seen[label] {
			return nil, fmt.Errorf("%s: label %q listed twice", envImpls, label)
		}
		seen[label] = true
		if path == "" && label != "go" {
			return nil, fmt.Errorf("%s entry %q: only the go label may build from source (empty path)", envImpls, entry)
		}
		specs = append(specs, implSpec{label: label, path: path})
	}
	return specs, nil
}

func validLabel(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9') {
			return false
		}
	}
	return true
}

// resolveHelper returns the absolute path of the helper of an implementation.
// Relative paths are relative to the module root.
func resolveHelper(t *testing.T, s implSpec) string {
	t.Helper()
	if s.path == "" {
		return buildGoHelper(t)
	}
	path := s.path
	if !filepath.IsAbs(path) {
		root, err := moduleRoot()
		if err != nil {
			t.Fatalf("%s: %s helper %q: %v", envImpls, s.label, path, err)
		}
		path = filepath.Join(root, path)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("%s: %s helper %q: %v", envImpls, s.label, s.path, err)
	}
	if info.IsDir() {
		t.Fatalf("%s: %s helper %q is a directory", envImpls, s.label, s.path)
	}
	return path
}

// buildGoHelper builds ./cmd/hqq-interop into a temporary directory. A failed
// build skips the whole test, with the build output.
func buildGoHelper(t *testing.T) string {
	t.Helper()
	root, err := moduleRoot()
	if err != nil {
		t.Fatalf("building the Go helper: %v", err)
	}
	name := "hqq-interop"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	out := filepath.Join(t.TempDir(), name)
	cmd := exec.Command("go", "build", "-o", out, "./cmd/hqq-interop")
	cmd.Dir = root
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("building the Go helper (go build ./cmd/hqq-interop) failed: %v\n%s", err, b)
	}
	return out
}

// moduleRoot walks up from the working directory to the directory whose
// go.mod declares module gosuda.org/hqq.
func moduleRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		data, err := os.ReadFile(filepath.Join(dir, "go.mod"))
		if err == nil && declaresHQQ(data) {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", errors.New("no go.mod of module gosuda.org/hqq above the test directory")
		}
		dir = parent
	}
}

func declaresHQQ(gomod []byte) bool {
	for _, line := range strings.Split(string(gomod), "\n") {
		f := strings.Fields(line)
		if len(f) == 2 && f[0] == "module" {
			return f[1] == "gosuda.org/hqq"
		}
	}
	return false
}

// listCatalogue runs `<bin> list --impl <label>` and parses the catalogue.
// Any failure is a misconfiguration and fails the test.
func listCatalogue(t *testing.T, bin, label string) catalogue {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), listTimeout)
	defer cancel()
	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, bin, "list", "--impl", label)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("%s list --impl %s: %v\nstderr:\n%s", bin, label, err, stderr.String())
	}
	cat, err := parseCatalogue(stdout.Bytes())
	if err != nil {
		t.Fatalf("%s list --impl %s: %v", bin, label, err)
	}
	cat.label = label
	return cat
}
