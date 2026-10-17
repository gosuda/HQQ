package gen

import (
	"bytes"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// cGoldenCases are the IDL files whose generated C is committed under testdata/c/golden.
var cGoldenCases = []struct{ idl, source string }{
	{"bindgen/testdata/microlib/microlib.idl", "microlib.idl"},
	{"bindgen/testdata/microlib/microbench.idl", "microbench.idl"},
	{"bindgen/testdata/sqlite_lowlevel.idl", "sqlite_lowlevel.idl"},
	{"bindgen/gen/testdata/c/edge.idl", "edge.idl"},
}

// TestCGolden pins the generated text. Run `go test ./bindgen/gen -run TestCGolden -update-c` to rewrite it.
func TestCGolden(t *testing.T) {
	root := repoRoot(t)
	dir := filepath.Join(root, "bindgen", "gen", "testdata", "c", "golden")
	for _, tc := range cGoldenCases {
		t.Run(tc.source, func(t *testing.T) {
			files := genC(t, parseSpec(t, tc.idl), tc.source)
			// the emitter is a function of its input
			again := genC(t, parseSpec(t, tc.idl), tc.source)
			for name, data := range files {
				if !bytes.Equal(data, again[name]) {
					t.Errorf("%s differs between two runs", name)
				}
				path := filepath.Join(dir, name)
				if *updateCGolden {
					if err := os.WriteFile(path, data, 0o644); err != nil {
						t.Fatal(err)
					}
					continue
				}
				want, err := os.ReadFile(path)
				if err != nil {
					t.Fatalf("%v (run with -update-c to create the golden)", err)
				}
				if !bytes.Equal(want, data) {
					t.Errorf("%s differs from the golden %s (run with -update-c after checking the change):\n%s", name, path, firstDiff(string(want), string(data)))
				}
			}
		})
	}
}

// firstDiff describes the first differing line of two texts.
func firstDiff(want, got string) string {
	w, g := strings.Split(want, "\n"), strings.Split(got, "\n")
	for i := 0; i < len(w) || i < len(g); i++ {
		var a, b string
		if i < len(w) {
			a = w[i]
		}
		if i < len(g) {
			b = g[i]
		}
		if a != b {
			return "line " + itoa(i+1) + "\n  want: " + a + "\n  got:  " + b
		}
	}
	return "no difference"
}

func itoa(n int) string {
	var b [20]byte
	i := len(b)
	for {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
		if n == 0 {
			break
		}
	}
	return string(b[i:])
}

// ---- BINDGEN.md 9.6 ---------------------------------------------------------------------------------

var (
	cFence     = regexp.MustCompile("(?s)```c\n(.*?)\n```")
	blockCmt   = regexp.MustCompile(`(?s)/\*.*?\*/`)
	spaceRuns  = regexp.MustCompile(`\s+`)
	opFunction = regexp.MustCompile(`(?s)(?:/\*.*?\*/\n)?static hqqrpc_status (op_\w+)\(hqqrpc_call \*c\) \{\n.*?\n\}\n`)
	apiTokens  = regexp.MustCompile(`\b(hqqrpc_[a-z]\w*|HQQRPC_VAR_SLACK|HQQRPC_DETAIL_RESERVE|HQQRPC_E_\w+|ml_\w+|microlib_\w+|destroy_\w+|memcpy|memset)\b`)
)

// docBlock returns the C block that follows the marker in the design document.
func docBlock(t *testing.T, doc, marker string) string {
	t.Helper()
	i := strings.Index(doc, marker)
	if i < 0 {
		t.Fatalf("BINDGEN.md has no %q", marker)
	}
	m := cFence.FindStringSubmatch(doc[i:])
	if m == nil {
		t.Fatalf("no C block after %q", marker)
	}
	return m[1]
}

// callSequence is the sequence of runtime, library and glue names that a function uses after its declarations.
func callSequence(fn string) []string {
	fn = blockCmt.ReplaceAllString(fn, "")
	if i := strings.Index(fn, "hqqrpc_rd_init"); i >= 0 {
		fn = fn[i:]
	}
	return apiTokens.FindAllString(fn, -1)
}

func normalizeC(s string) string {
	s = blockCmt.ReplaceAllString(s, "")
	return strings.TrimSpace(spaceRuns.ReplaceAllString(s, " "))
}

// docFunctionDifferences lists, per function of 9.6, how the emitted glue deliberately differs in its sequence of
// runtime calls. The list is short; every entry is a documented difference of the emitter.
var docFunctionDifferences = map[string]string{
	// 9.6 copies `value` to the response whatever ret says; 4.6.4 says that a failure body carries only `ret`, so
	// the emitter copies the out value only when the status is in ok. The call sequence is the same.
}

// TestCDocument96 compares the generated microlib glue with the listing of 9.6: the header and everything
// outside the op functions are equal after comments and white space are dropped, and every op function makes the
// same sequence of runtime calls, library calls and status returns (statements, declarations, the order of the
// two arms of an error convention and constants such as HQQRPC_OK are not compared). Differences that remain are the documented ones of the emitter: the ok values are
// numbers instead of the IDL const names (the consts are not C macros), and pointers handed to the library are
// passed untyped or with a cast to (void *).
func TestCDocument96(t *testing.T) {
	root := repoRoot(t)
	docBytes, err := os.ReadFile(filepath.Join(root, "spec", "toolchain", "BINDGEN.md"))
	if err != nil {
		t.Fatal(err)
	}
	doc := string(docBytes)
	files := genC(t, parseSpec(t, "bindgen/testdata/microlib/microlib.idl"), "microlib.idl")

	wantH := docBlock(t, doc, "**`microlib_server.h`** (generated)")
	if got, want := normalizeC(string(files["microlib_server.h"])), normalizeC(wantH); got != want {
		t.Errorf("header differs from 9.6:\n%s", firstDiff(strings.ReplaceAll(want, " ", "\n"), strings.ReplaceAll(got, " ", "\n")))
	}

	wantC := docBlock(t, doc, "**`microlib_server.c`** (generated)")
	wantC = strings.NewReplacer("ML_OK", "0", "ML_EMPTY", "1", "ML_PARTIAL", "2").Replace(wantC)
	gotC := string(files["microlib_server.c"])

	docOps := map[string]string{}
	for _, m := range opFunction.FindAllStringSubmatch(wantC, -1) {
		docOps[m[1]] = m[0]
	}
	genOps := map[string]string{}
	for _, m := range opFunction.FindAllStringSubmatch(gotC, -1) {
		genOps[m[1]] = m[0]
	}
	if len(docOps) != 11 || len(genOps) != 11 {
		t.Fatalf("op functions: %d in 9.6, %d generated, want 11", len(docOps), len(genOps))
	}
	for name, want := range docOps {
		got, ok := genOps[name]
		if !ok {
			t.Errorf("%s is in 9.6 and not generated", name)
			continue
		}
		w, g := callSequence(want), callSequence(got)
		if strings.Join(w, " ") != strings.Join(g, " ") {
			if why, ok := docFunctionDifferences[name]; ok {
				t.Logf("%s differs from 9.6: %s", name, why)
				continue
			}
			t.Errorf("%s: runtime call sequence differs from 9.6:\n  doc: %s\n  gen: %s", name, strings.Join(w, " "), strings.Join(g, " "))
		}
	}
	rest := func(s string) string { return normalizeC(opFunction.ReplaceAllString(s, "")) }
	if g, w := rest(gotC), rest(wantC); g != w {
		t.Errorf("the source outside the op functions differs from 9.6:\n%s", firstDiff(strings.ReplaceAll(w, " ", "\n"), strings.ReplaceAll(g, " ", "\n")))
	}
}
