package gen

import (
	"bytes"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

const goDesignDoc = "../../spec/toolchain/BINDGEN.md"

// listingBlocks returns the ```go blocks of section 9.4 of the design document.
func listingBlocks(t *testing.T) []string {
	t.Helper()
	raw, err := os.ReadFile(filepath.FromSlash(goDesignDoc))
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	start := strings.Index(text, "\n### 9.4 ")
	end := strings.Index(text, "\n### 9.5 ")
	if start < 0 || end < start {
		t.Fatal("section 9.4 not found in the design document")
	}
	var blocks []string
	var cur []string
	in := false
	for _, l := range strings.Split(text[start:end], "\n") {
		switch {
		case !in && strings.HasPrefix(l, "```go"):
			in, cur = true, nil
		case in && strings.HasPrefix(l, "```"):
			in = false
			blocks = append(blocks, strings.Join(cur, "\n")+"\n")
		case in:
			cur = append(cur, l)
		}
	}
	return blocks
}

// declKey names a top-level declaration: "func Client.Add", "type Cell", "const ML_OK", "var hqqIface".
func declKey(d ast.Decl) string {
	switch d := d.(type) {
	case *ast.FuncDecl:
		if d.Recv == nil {
			return "func " + d.Name.Name
		}
		t := d.Recv.List[0].Type
		if s, ok := t.(*ast.StarExpr); ok {
			t = s.X
		}
		return "func " + t.(*ast.Ident).Name + "." + d.Name.Name
	case *ast.GenDecl:
		if d.Tok == token.IMPORT || len(d.Specs) == 0 {
			return ""
		}
		switch s := d.Specs[0].(type) {
		case *ast.TypeSpec:
			return "type " + s.Name.Name
		case *ast.ValueSpec:
			return d.Tok.String() + " " + s.Names[0].Name
		}
	}
	return ""
}

// stripComments removes the doc and trailing comments from a declaration, so that the comparison covers
// code only: the comments of the listing are explanations, the generated ones are derived from the IDL.
func stripComments(d ast.Decl) {
	ast.Inspect(d, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.FuncDecl:
			n.Doc = nil
		case *ast.GenDecl:
			n.Doc = nil
		case *ast.Field:
			n.Doc, n.Comment = nil, nil
		case *ast.ValueSpec:
			n.Doc, n.Comment = nil, nil
		case *ast.TypeSpec:
			n.Doc, n.Comment = nil, nil
		}
		return true
	})
}

func printDecl(t *testing.T, fset *token.FileSet, d ast.Decl) string {
	t.Helper()
	var buf bytes.Buffer
	cfg := printer.Config{Mode: printer.UseSpaces | printer.TabIndent, Tabwidth: 8}
	if err := cfg.Fprint(&buf, fset, d); err != nil {
		t.Fatal(err)
	}
	// Removing a comment leaves a gap in the positions, which the printer renders as a blank line.
	lines := strings.Split(buf.String(), "\n")
	kept := lines[:0]
	for _, l := range lines {
		if strings.TrimSpace(l) != "" {
			kept = append(kept, l)
		}
	}
	return strings.Join(kept, "\n")
}

func parseGoSource(t *testing.T, name, src string) (*token.FileSet, *ast.File, bool) {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, name, src, parser.ParseComments)
	if err != nil {
		return nil, nil, false
	}
	return fset, f, true
}

// TestGoMicrolibMatchesListing is the acceptance test of 11.2: every declaration that section 9.4 of the
// design document shows with a body is byte for byte the declaration the emitter produces for
// microlib.idl, and every declaration it shows without a body (the method set) has the generated
// signature. Comments are not compared.
func TestGoMicrolibMatchesListing(t *testing.T) {
	spec := loadSpec(t, "microlib/microlib.idl", "ml_")
	out := generateGo(t, spec, "ml_", "microlib.idl")

	gfset, gfile, ok := parseGoSource(t, "microlib_client.go", string(out))
	if !ok {
		t.Fatal("generated file does not parse")
	}
	generated := map[string]ast.Decl{}
	for _, d := range gfile.Decls {
		if k := declKey(d); k != "" {
			generated[k] = d
		}
	}

	compared, bodyless := 0, 0
	for i, block := range listingBlocks(t) {
		src := block
		if !strings.Contains(block, "package ") {
			src = "package listing\n" + block
		}
		fset, f, ok := parseGoSource(t, "listing", src)
		if !ok {
			continue // the usage example is not a declaration list
		}
		for _, d := range f.Decls {
			key := declKey(d)
			if key == "" {
				continue
			}
			want, have := d, generated[key]
			if have == nil {
				t.Errorf("block %d: listing shows %s, which the emitter does not produce", i, key)
				continue
			}
			stripComments(want)
			stripComments(have)
			if fd, ok := want.(*ast.FuncDecl); ok && fd.Body == nil {
				hd := *have.(*ast.FuncDecl)
				hd.Body = nil
				have = &hd
				bodyless++
			}
			if w, g := printDecl(t, fset, want), printDecl(t, gfset, have); w != g {
				t.Errorf("block %d, %s differs\n--- listing\n%s\n--- generated\n%s", i, key, w, g)
			}
			compared++
		}
	}
	if compared < 60 || bodyless < 30 {
		t.Errorf("compared %d declarations (%d without body); the listing parser lost most of section 9.4", compared, bodyless)
	}
	keys := make([]string, 0, len(generated))
	for k := range generated {
		keys = append(keys, k)
	}
	sort.Strings(keys)
}
