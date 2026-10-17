package gen

import (
	"bytes"
	"go/ast"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"gosuda.org/hqq/bindgen/idl"
)

// The type checker reads gosuda.org/hqq/bindgen/rt and its imports from source once per test binary;
// the importer is not safe for concurrent use, so the checks are serialized.
var (
	importerMu   sync.Mutex
	sourceImport types.Importer
)

// typeCheck compiles one generated file against the real bindgen/rt and returns every type error.
func typeCheck(t testing.TB, name string, src []byte) []error {
	t.Helper()
	importerMu.Lock()
	defer importerMu.Unlock()
	fset := token.NewFileSet()
	if sourceImport == nil {
		sourceImport = importer.ForCompiler(token.NewFileSet(), "source", nil)
	}
	f, err := parser.ParseFile(fset, name+"_client.go", src, parser.ParseComments)
	if err != nil {
		return []error{err}
	}
	var errs []error
	conf := types.Config{
		Importer: sourceImport,
		Error:    func(err error) { errs = append(errs, err) },
	}
	conf.Check(name, fset, []*ast.File{f}, nil)
	return errs
}

func requireCompiles(t *testing.T, name string, src []byte) {
	t.Helper()
	if errs := typeCheck(t, name, src); len(errs) > 0 {
		lines := strings.Split(string(src), "\n")
		var msg strings.Builder
		for i, err := range errs {
			if i == 8 {
				msg.WriteString("...\n")
				break
			}
			msg.WriteString(err.Error() + "\n")
			if te, ok := err.(types.Error); ok {
				if ln := te.Fset.Position(te.Pos).Line; ln >= 1 && ln <= len(lines) {
					msg.WriteString("    " + strings.TrimSpace(lines[ln-1]) + "\n")
				}
			}
		}
		t.Fatalf("%s does not compile against bindgen/rt:\n%s", name, msg.String())
	}
}

// TestGoGeneratedCompiles type-checks every committed case and a set of small shapes against the real
// runtime. The sqlite excerpt of 2.8 is one of them (11.2).
func TestGoGeneratedCompiles(t *testing.T) {
	for _, c := range goldenCases() {
		t.Run(c.name, func(t *testing.T) {
			spec, strip := c.load(t)
			requireCompiles(t, c.name, generateGo(t, spec, strip, c.source))
		})
	}
	for _, c := range shapeCases() {
		t.Run(c.name, func(t *testing.T) {
			spec := parseIDL(t, c.name+".idl", c.idl, c.strip)
			requireCompiles(t, spec.Service, generateGo(t, spec, c.strip, c.name+".idl"))
		})
	}
}

// moduleRoot is the directory of go.mod, which the go tool needs as its working directory.
func moduleRoot(t *testing.T) string {
	t.Helper()
	dir, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "go.mod")); err != nil {
		t.Fatal(err)
	}
	return dir
}

// TestGoGoldenBuilds builds and vets the committed generated packages with the go tool (the acceptance
// criterion of 11.2), and checks with the compiler's escape analysis that no response value, future or
// decoder is moved to the heap by the generated code itself.
func TestGoGoldenBuilds(t *testing.T) {
	if testing.Short() {
		t.Skip("runs the go tool")
	}
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("no go tool in PATH")
	}
	var pkgs []string
	for _, c := range goldenCases() {
		pkgs = append(pkgs, "./bindgen/gen/testdata/go/"+c.name)
	}
	run := func(args ...string) string {
		cmd := exec.Command(goBin, args...)
		cmd.Dir = moduleRoot(t)
		cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
		var out bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &out
		if err := cmd.Run(); err != nil {
			t.Fatalf("go %s: %v\n%s", strings.Join(args, " "), err, out.String())
		}
		return out.String()
	}
	run(append([]string{"vet"}, pkgs...)...)
	// The edge file decodes a nullable str into a *string, which is allocated on purpose.
	out := run("build", "-gcflags=-m", "./bindgen/gen/testdata/go/microlib", "./bindgen/gen/testdata/go/sqlite_lowlevel")
	for _, l := range strings.Split(out, "\n") {
		if strings.Contains(l, "moved to heap") {
			t.Errorf("generated code moves a variable to the heap: %s", l)
		}
	}
}

// shapeCase is a small IDL for one emitter feature.
type shapeCase struct {
	name, idl, strip string
}

func svc(body string) string { return "idl 1;\nservice svc;\n" + body + "\n" }

// longName builds an identifier of the maximum length of 128 bytes.
func longName(prefix string) string {
	return prefix + strings.Repeat("x", 128-len(prefix))
}

func shapeCases() []shapeCase {
	ln := longName
	return []shapeCase{
		{name: "nothing_to_call", idl: svc("const K: u8 = 1;\n@allow(server) fn helper(a: i32) -> i32;")},
		{name: "no_params_void", idl: svc("fn nop();")},
		{name: "no_params_value", idl: svc("fn now() -> u64;")},
		{name: "go_keywords", idl: svc("fn kw(func: i32, interface: i32, select: i32, defer: i32, go: i32, map: i32, chan: i32, package: i32, fallthrough: i32, range: i32, type: i32, import: i32, var: i32) -> i32;")},
		{name: "go_keyword_function", idl: svc("fn func() -> i32;\nfn type(x: i32);\nfn range(x: i32) -> i32;")},
		{name: "go_keyword_fields", idl: svc("struct S { type: i32; func: str<=4; var: bool; }\nunion U { case 1 Map: i32; case 2 Chan: str<=4; case 3 Go; }\n@c(\"s_t\") struct F { go: u8; range: u8; }\nshim fn get_s() -> S @max_out(64);\nshim fn get_u() -> U @max_out(64);\nfn get_f() -> F;")},
		{name: "predeclared_names", idl: svc("fn p(len: i32, cap: i32, clear: i32, max: i32, min: i32, copy: i32, append: i32, make: i32, nil: i32, error: i32, string: i32, uint32: i32, rt: i32, math: i32, binary: i32, context: i32) -> i32;")},
		{name: "local_names", idl: svc("fn p(c: i32, f: i32, e: i32, d: i32, b: i32, err: i32, body: i32, call: i32, off: i32, end: i32, n: i32, v: i32, i: i32, id: i32, rc: i32, cnt: i32, r: i32, s: i32, dst: i32, p_a: i32, p: i32) -> i32;")},
		{name: "dst_param_name_taken", idl: svc("struct E { a: i32; s: str<=4; }\nshim fn rows(dst: i32, dst_: i32) -> E[]<=4 @max_out(256);")},
		{name: "optional_detail_null", idl: svc("@error(null, detail(why, x))\nfn open(x: i32) -> str<=16;\n@allow(server) fn why(x: i32) -> str<=64;")},
		{name: "optional_detail_status_out", idl: svc("@error(status, ok(0), detail(why, x))\nfn f(x: i32, out a: i32, out s: str<=8) -> i32;\n@allow(server) fn why(x: i32) -> str<=64;")},
		{name: "inout_scalars", idl: svc("fn inc(inout a: i8, inout b: u16, inout c: f32, inout d: bool, inout e: size) -> i64;")},
		{name: "out_buffers_only", idl: svc("fn fill(out a: bytes, n: u32 @len_of(a), out b: i64[]<=4, m: u8 @len_of(b));")},
		{name: "union_return", idl: svc("union U { case 1 A: i64; case 2 B: str<=8; case 3 C; }\nshim fn get_u() -> U @max_out(256);")},
		{name: "struct_array_return", idl: svc("struct P { a: u8; b: u64; }\nshim fn ps(n: i32) -> P[]<=3;")},
		{name: "handles_in_out", idl: svc("@c(\"h *\") handle H destroy hfree;\n@error(null) fn hnew() -> H;\nfn use(a: H, b: H @nullable) -> i32;\nfn mk(out a: H, out b: H) -> i32;\nfn hfree(h: H);")},
		{name: "long_names", strip: "p_", idl: svc("const " + ln("C") + ": u8 = 1;\n" +
			"struct " + ln("S") + " { " + ln("f") + ": u8; }\n" +
			"union " + ln("U") + " { case 1 " + ln("a") + ": u8; case 2 " + ln("b") + "; }\n" +
			"@c(\"h *\") handle " + ln("H") + " destroy " + ln("d") + ";\n" +
			"fn " + ln("d") + "(h: " + ln("H") + ");\n" +
			"fn " + ln("p_a") + "(" + ln("x") + ": " + ln("S") + "[], n: u32 @len_of(" + ln("x") + ")) -> u8;\n" +
			"shim fn " + ln("p_b") + "() -> " + ln("U") + " @max_out(64);\n")},
		{name: "strip_prefix", strip: "lib_", idl: svc("fn lib_a(x: i32);\nfn lib_b() -> i32;\nfn other();")},
	}
}

// TestGoShapesGenerateEveryMethod: every wire function of the shape cases and the edge file has its three
// methods, its future type and its opcode constant.
func TestGoShapesGenerateEveryMethod(t *testing.T) {
	all := map[string]*idl.Spec{"edge": loadGenSpec(t, "edge.idl", "")}
	for _, c := range shapeCases() {
		all[c.name] = parseIDL(t, c.name+".idl", c.idl, c.strip)
	}
	for name, spec := range all {
		strip := ""
		for _, c := range shapeCases() {
			if c.name == name {
				strip = c.strip
			}
		}
		out := string(generateGo(t, spec, strip, name+".idl"))
		for _, f := range spec.Wire {
			g := idl.GoName(strip, f.Name)
			for _, want := range []string{
				"func (c *Client) " + g + "(",
				"func (c *Client) " + g + "Async(",
				"func (f " + g + "Future) Wait() ",
				"type " + g + "Future struct {",
				"\thqqOp" + g + "\n",
			} {
				if !strings.Contains(out, want) && !strings.Contains(out, strings.TrimSuffix(want, "\n")+" uint16 = iota + 1\n") {
					t.Errorf("%s: no %q for %s", name, strings.TrimSpace(want), f.Name)
				}
			}
		}
	}
}
