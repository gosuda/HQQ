package gen

import (
	"path/filepath"
	"strings"
	"testing"
)

func cTestdata(t testing.TB, parts ...string) string {
	t.Helper()
	return filepath.Join(append([]string{repoRoot(t), "bindgen", "gen", "testdata", "c"}, parts...)...)
}

// compileObjects compiles sources with the flags of 11.2 (-std=c11 -Wall -Wextra -Wpedantic -Werror) and does not link.
func compileObjects(t testing.TB, dir string, includes []string, sources ...string) {
	t.Helper()
	cc := needCC(t)
	for _, src := range sources {
		args := append([]string{}, strictFlags...)
		args = append(args, "-I"+filepath.Join(repoRoot(t), "c", "include"), "-I"+dir)
		for _, inc := range includes {
			args = append(args, "-I"+inc)
		}
		args = append(args, "-c", src, "-o", filepath.Join(dir, strings.TrimSuffix(filepath.Base(src), ".c")+".o"))
		runCmd(t, dir, cc, args...)
	}
}

// TestCCompilesMicrolib compiles the generated microlib glue and its test hooks against hqq_rpc.h and microlib.h.
func TestCCompilesMicrolib(t *testing.T) {
	dir := t.TempDir()
	files := genC(t, parseSpec(t, "bindgen/testdata/microlib/microlib.idl"), "microlib.idl")
	writeFiles(t, dir, files)
	micro := filepath.Join(repoRoot(t), "bindgen", "testdata", "microlib")
	compileObjects(t, dir, []string{micro, cTestdata(t)},
		filepath.Join(dir, "microlib_server.c"), cTestdata(t, "microlib_hooks.c"), filepath.Join(micro, "microlib.c"))
}

// TestCCompilesMicrobench compiles the glue of the benchmark interface with its interceptor hook.
func TestCCompilesMicrobench(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, genC(t, parseSpec(t, "bindgen/testdata/microlib/microbench.idl"), "microbench.idl"))
	micro := filepath.Join(repoRoot(t), "bindgen", "testdata", "microlib")
	compileObjects(t, dir, []string{micro, cTestdata(t)},
		filepath.Join(dir, "microbench_server.c"), cTestdata(t, "microbench_hooks.c"), filepath.Join(micro, "microlib.c"))
}

// TestCNoHandleTypes: a service without handle types has NULL tables and still compiles.
func TestCNoHandleTypes(t *testing.T) {
	dir := t.TempDir()
	files := genC(t, parseText(t, "idl 1;\nservice plain;\ninclude \"plain.h\";\nfn pl_add(a: i32, b: i32) -> i32;\n"), "plain.idl")
	writeFiles(t, dir, files)
	writeFiles(t, dir, map[string][]byte{"plain.h": []byte("#include <stdint.h>\nint32_t pl_add(int32_t a, int32_t b);\n")})
	compileObjects(t, dir, nil, filepath.Join(dir, "plain_server.c"))
	if !strings.Contains(string(files["plain_server.c"]), "    0, NULL,\n") {
		t.Errorf("a service without handle types needs a NULL table:\n%s", files["plain_server.c"])
	}
}

// TestCCompilesSQLite compiles the sqlite excerpt of 2.8 against a stub sqlite3.h with the shims and the
// interceptor stubbed, and links it with the runtime when it is available.
func TestCCompilesSQLite(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, genC(t, parseSpec(t, "bindgen/testdata/sqlite_lowlevel.idl"), "sqlite_lowlevel.idl"))
	stubs := cTestdata(t, "stubs")
	compileObjects(t, dir, []string{stubs, cTestdata(t)},
		filepath.Join(dir, "sqlite_lowlevel_server.c"), cTestdata(t, "sqlite_stub.c"))
}

// TestCCompilesEdge compiles the glue of edge.idl with its library and hooks.
func TestCCompilesEdge(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, genC(t, parseSpec(t, "bindgen/gen/testdata/c/edge.idl"), "edge.idl"))
	compileObjects(t, dir, []string{cTestdata(t)},
		filepath.Join(dir, "edge_server.c"), cTestdata(t, "edge_hooks.c"), cTestdata(t, "edge.c"))
}

// TestCCompilesShapes compiles the glue of shapes.idl, which has the argument shapes that no other test runs:
// every template of the emitter is checked by the compiler with -Wall -Wextra -Wpedantic -Werror against the
// declarations of shapes.h, so a wrong C type, a missing cast or an unused local is a failure.
func TestCCompilesShapes(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, genC(t, parseSpec(t, "bindgen/gen/testdata/c/shapes.idl"), "shapes.idl"))
	compileObjects(t, dir, []string{cTestdata(t)}, filepath.Join(dir, "shapes_server.c"))
}
