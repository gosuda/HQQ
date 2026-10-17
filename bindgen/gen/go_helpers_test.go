package gen

import (
	"flag"
	"os"
	"path/filepath"
	"testing"

	"gosuda.org/hqq/bindgen/idl"
)

var updateGoGolden = flag.Bool("update-go", false, "rewrite the golden Go clients under testdata/go")

// loadSpec parses a file of bindgen/testdata.
func loadSpec(t testing.TB, rel, strip string) *idl.Spec {
	t.Helper()
	src, err := os.ReadFile(filepath.Join("..", "testdata", filepath.FromSlash(rel)))
	if err != nil {
		t.Fatal(err)
	}
	return parseIDL(t, rel, string(src), strip)
}

// loadGenSpec parses a file of bindgen/gen/testdata/go.
func loadGenSpec(t testing.TB, rel, strip string) *idl.Spec {
	t.Helper()
	src, err := os.ReadFile(filepath.Join("testdata", "go", filepath.FromSlash(rel)))
	if err != nil {
		t.Fatal(err)
	}
	return parseIDL(t, rel, string(src), strip)
}

func parseIDL(t testing.TB, name, src, strip string) *idl.Spec {
	t.Helper()
	spec, err := idl.Parse(name, []byte(src), idl.Options{GoStrip: strip})
	if err != nil {
		t.Fatalf("parse %s: %v", name, err)
	}
	return spec
}

// generateGo runs the Go emitter and returns its single file.
func generateGo(t testing.TB, spec *idl.Spec, strip, source string) []byte {
	t.Helper()
	files, err := Go(spec, GoOptions{Strip: strip, Source: source})
	if err != nil {
		t.Fatalf("Go(%s): %v", spec.Service, err)
	}
	if len(files) != 1 || files[0].Name != spec.Service+"_client.go" {
		t.Fatalf("Go(%s) returned %d files, first %q", spec.Service, len(files), files[0].Name)
	}
	return files[0].Data
}

// goCase is one IDL file with the options it is generated with.
type goCase struct {
	name   string // golden directory under testdata/go and package name
	load   func(testing.TB) (*idl.Spec, string)
	strip  string
	source string
}

// goldenCases are the specifications whose generated clients are committed under testdata/go.
func goldenCases() []goCase {
	return []goCase{
		{name: "microlib", strip: "ml_", source: "microlib.idl", load: func(t testing.TB) (*idl.Spec, string) {
			return loadSpec(t, "microlib/microlib.idl", "ml_"), "ml_"
		}},
		{name: "sqlite_lowlevel", source: "sqlite_lowlevel.idl", load: func(t testing.TB) (*idl.Spec, string) {
			return loadSpec(t, "sqlite_lowlevel.idl", ""), ""
		}},
		{name: "edge", source: "edge.idl", load: func(t testing.TB) (*idl.Spec, string) {
			return loadGenSpec(t, "edge.idl", ""), ""
		}},
	}
}
