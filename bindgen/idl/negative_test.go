package idl_test

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"gosuda.org/hqq/bindgen/idl"
)

const hdr = "idl 1;\nservice s;\n"

// marker is placed immediately before the token at which a diagnostic is expected.
const marker = "§"

// stripMarker removes the marker from src and returns the position of the byte that followed it.
func stripMarker(t *testing.T, src string) (string, idl.Pos, bool) {
	t.Helper()
	i := strings.Index(src, marker)
	if i < 0 {
		return src, idl.Pos{}, false
	}
	before := src[:i]
	line := strings.Count(before, "\n") + 1
	col := i - (strings.LastIndex(before, "\n") + 1) + 1
	return before + src[i+len(marker):], idl.Pos{Line: line, Col: col}, true
}

type negCase struct {
	name string
	rule string   // the rule the case violates
	also []string // further rules that the same defect necessarily violates
	src  string   // IDL text after hdr, with one marker at the expected position
	opts idl.Options
	raw  bool // src is the whole file, hdr is not prepended
}

func diagnosticsOf(t *testing.T, err error) idl.Diagnostics {
	t.Helper()
	var ds idl.Diagnostics
	if !errors.As(err, &ds) {
		t.Fatalf("error %v is not Diagnostics", err)
	}
	return ds
}

func runNegative(t *testing.T, cases []negCase) {
	t.Helper()
	for _, c := range cases {
		t.Run(c.rule+"/"+c.name, func(t *testing.T) {
			src := c.src
			if !c.raw {
				src = hdr + src
			}
			src, pos, hasPos := stripMarker(t, src)
			spec, err := idl.Parse("neg.idl", []byte(src), c.opts)
			if err == nil {
				t.Fatalf("accepted:\n%s", src)
			}
			if spec != nil {
				t.Error("Parse returned a Spec together with an error")
			}
			ds := diagnosticsOf(t, err)
			want := append([]string{c.rule}, c.also...)
			slices.Sort(want)
			got := slices.Clone(ds.Rules())
			slices.Sort(got)
			if !slices.Equal(got, want) {
				t.Fatalf("rules %v, want %v:\n%v\nsource:\n%s", got, want, err, src)
			}
			for _, d := range ds {
				if d.Msg == "" || d.File != "neg.idl" || d.Line < 1 || d.Col < 1 {
					t.Errorf("incomplete diagnostic %+v", d)
				}
			}
			if !hasPos {
				return
			}
			for _, d := range ds {
				if d.Rule == c.rule && d.Line == pos.Line && d.Col == pos.Col {
					return
				}
			}
			t.Errorf("no %s diagnostic at %d:%d:\n%v", c.rule, pos.Line, pos.Col, err)
		})
	}
}

// ruleCoverage lists every rule id of 2.5; TestEveryRuleHasANegativeCase fails if one has no case.
var ruleCoverage = []string{
	"V1", "V2", "V3", "V4", "V5", "V6", "V7", "V8", "V9", "V10", "V11", "V12", "V13", "V14", "V15", "V16",
	"V18", "V19", "V20", "V21", "V22", "V23", "V26", "V27", "V28", "V29", "V30", "V31", "V35", "V36", "V37",
	"V38", "V39", "V40", "V41", "syntax",
}

func allNegativeCases() [][]negCase {
	return [][]negCase{namesCases, functionCases, errorCases, sizeCases, stringCases, syntaxCases}
}

func TestEveryRuleHasANegativeCase(t *testing.T) {
	have := map[string]int{}
	for _, group := range allNegativeCases() {
		for _, c := range group {
			have[c.rule]++
			for _, r := range c.also {
				have[r]++
			}
		}
	}
	for _, r := range ruleCoverage {
		if have[r] == 0 {
			t.Errorf("rule %s has no negative test", r)
		}
	}
}

func TestNegativeNamesAndStructure(t *testing.T)        { runNegative(t, namesCases) }
func TestNegativeFunctions(t *testing.T)                { runNegative(t, functionCases) }
func TestNegativeErrorConvention(t *testing.T)          { runNegative(t, errorCases) }
func TestNegativeSizesAndBounds(t *testing.T)           { runNegative(t, sizeCases) }
func TestNegativeStringsAndReachableNames(t *testing.T) { runNegative(t, stringCases) }

var namesCases = []negCase{
	// V1
	{name: "duplicate function", rule: "V1", src: "fn a();\nfn §a();\n"},
	{name: "duplicate const", rule: "V1", src: "const K: i32 = 1;\nconst §K: i32 = 2;\n"},
	{name: "struct and union share a namespace", rule: "V1",
		src: "struct A { x: u8; }\nunion §A { case 1 X; }\n"},
	{name: "struct and handle share a namespace", rule: "V1",
		src: "handle A destroy da;\nfn da(h: A);\nstruct §A { x: u8; }\n"},
	{name: "C keyword as function name", rule: "V1", src: "fn §int();\n"},
	{name: "C keyword as field name", rule: "V1", src: "struct A { §register: u8; }\n"},
	{name: "C11 keyword _Bool as parameter", rule: "V1", src: "fn f(§_Bool: i32);\n"},
	{name: "double underscore", rule: "V1", src: "fn §a__b();\n"},
	{name: "IDL keyword as parameter name", rule: "V1", src: "fn f(§str: i32);\n"},
	{name: "IDL keyword ret as field name", rule: "V1", src: "struct A { §ret: u8; }\n"},
	{name: "IDL keyword as service name", rule: "V1", raw: true, src: "idl 1;\nservice §fn;\n"},
	// V2
	{name: "version 2", rule: "V2", raw: true, src: "idl §2;\nservice s;\n"},
	{name: "version 0", rule: "V2", raw: true, src: "idl §0;\nservice s;\n"},
	{name: "negative version", rule: "V2", raw: true, src: "idl §-1;\nservice s;\n"},
	// V3
	{name: "second service", rule: "V3", src: "§service t;\n"},
	{name: "two serialize annotations", rule: "V3", raw: true,
		src: "idl 1;\n@serialize(none)\n§@serialize(session)\nservice s;\n", also: []string{}},
	{name: "serialize on a function", rule: "V3", src: "§@serialize(none)\nfn f();\n"},
	{name: "serialize on a struct", rule: "V3", src: "§@serialize(none)\nstruct A { x: u8; }\n"},
	// V4
	{name: "unknown type", rule: "V4", src: "fn f(a: §Nope);\n"},
	{name: "unknown type in struct", rule: "V4", src: "struct A { x: §Nope; }\n"},
	{name: "unknown return type", rule: "V4", src: "fn f() -> §Nope;\n"},
	{name: "struct contains itself", rule: "V4", src: "struct A { §a: A; }\n"},
	{name: "struct contains itself through an array", rule: "V4", src: "struct A { §a: A[]; }\n"},
	{name: "mutual recursion", rule: "V4", src: "struct A { b: B; }\nstruct B { §a: A; }\n"},
	{name: "recursion through a union", rule: "V4",
		src: "union U { case 1 §X: S; }\nstruct S { u: U[]; }\n"},
	// V5
	{name: "empty struct", rule: "V5", src: "struct §S { }\n"},
	{name: "duplicate field", rule: "V5", src: "struct S { a: u8; §a: u16; }\n"},
	{name: "empty union", rule: "V5", src: "union §U { }\n"},
	{name: "duplicate case name", rule: "V5", src: "union U { case 1 A; case 2 §A; }\n"},
	{name: "duplicate case number", rule: "V5", src: "union U { case 1 A; case 1 §B; }\n"},
	{name: "case number too large", rule: "V5", src: "union U { case §4294967296 A; }\n"},
	{name: "negative case number", rule: "V5", src: "union U { case §-1 A; }\n"},
	// V6
	{name: "fixed array of a variable type", rule: "V6", src: "struct S { a: §str[3]; }\n"},
	{name: "fixed array of length 0", rule: "V6", src: "struct S { a: u8[§0]; }\n"},
	{name: "fixed array above 2^20", rule: "V6", src: "struct S { a: u8[§1048577]; }\n"},
	{name: "bound after a scalar", rule: "V6", src: "fn f(a: i32 §<= 5);\n"},
	{name: "bound after a fixed array", rule: "V6", src: "struct S { a: u8[4] §<= 4; }\n"},
	{name: "bound after a struct", rule: "V6", src: "struct S { a: u8; }\nfn f(a: S §<= 4);\n", also: []string{"V38"}},
	{name: "bound zero", rule: "V6", src: "fn f(a: str<=§0);\n"},
	{name: "bound above 2^30", rule: "V6", src: "fn f(a: bytes<=§1073741825, n: size @len_of(a));\n"},
	{name: "negative bound", rule: "V6", src: "fn f(a: str<=§-3);\n"},
	// V7
	{name: "destroy names no function", rule: "V7", src: "handle H destroy §nope;\n"},
	{name: "destroy with two parameters", rule: "V7",
		src: "handle H destroy §d;\nfn d(h: H, x: i32);\n"},
	{name: "destroy with an out parameter", rule: "V7",
		src: "handle H destroy §d;\nfn d(out h: H);\n"},
	{name: "destroy of another handle type", rule: "V7",
		src: "handle H destroy d;\nhandle G destroy §d;\nfn d(h: H);\n"},
	{name: "destroy returns an integer without @error", rule: "V7",
		src: "handle H destroy d;\nfn §d(h: H) -> i32;\n"},
	{name: "destroy returns a string", rule: "V7",
		src: "handle H destroy d;\nfn §d(h: H) -> str<=4;\n"},
	{name: "owned_by an unknown name", rule: "V7", src: "handle H owned_by §X destroy d;\nfn d(h: H);\n"},
	{name: "owned_by a struct", rule: "V7",
		src: "struct S { x: u8; }\nhandle H owned_by §S destroy d;\nfn d(h: H);\n"},
	{name: "owned_by itself", rule: "V7", src: "handle H owned_by §H destroy d;\nfn d(h: H);\n"},
	{name: "ownership cycle", rule: "V7",
		src:  "handle §A owned_by B destroy da;\nhandle B owned_by A destroy db;\nfn da(h: A);\nfn db(h: B);\n",
		also: []string{}},
	// V8
	{name: "handle as a struct field", rule: "V8", src: "handle H destroy d;\nfn d(h: H);\nstruct S { a: §H; }\n"},
	{name: "handle as a union payload", rule: "V8",
		src: "handle H destroy d;\nfn d(h: H);\nunion U { case 1 A: §H; }\n"},
	{name: "array of handles as a parameter", rule: "V8",
		src: "handle H destroy d;\nfn d(h: H);\nfn f(a: §H[], n: i32 @len_of(a));\n"},
	// V9
	{name: "duplicate parameter", rule: "V9", src: "fn f(a: i32, §a: i32);\n"},
	{name: "65 parameters", rule: "V9", src: "fn §f(" + params(65) + ");\n"},
	{name: "65536 wire functions", rule: "V9", src: manyFuncs(65536)},
}

func params(n int) string {
	var parts []string
	for i := range n {
		parts = append(parts, "p"+itoa(i)+": i32")
	}
	return strings.Join(parts, ", ")
}

func manyFuncs(n int) string {
	var sb strings.Builder
	for i := range n {
		if i == 65535 {
			sb.WriteString("fn §")
			sb.WriteString("g" + itoa(i))
			sb.WriteString("();\n")
			continue
		}
		sb.WriteString("fn g" + itoa(i) + "();\n")
	}
	return sb.String()
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for ; n > 0; n /= 10 {
		b = append([]byte{byte('0' + n%10)}, b...)
	}
	return string(b)
}

var functionCases = []negCase{
	// V10
	{name: "inout str", rule: "V10", src: "fn f(inout §s: str);\n"},
	{name: "inout bytes", rule: "V10", src: "fn f(inout §b: bytes);\n"},
	{name: "inout array", rule: "V10", src: "fn f(inout §a: i32[4]);\n"},
	{name: "inout handle", rule: "V10",
		src: "handle H destroy d;\nfn d(h: H);\nfn f(inout §h: H);\n"},
	// V11
	{name: "in bytes without a length", rule: "V11", src: "fn f(§b: bytes);\n"},
	{name: "in array without a length", rule: "V11", src: "fn f(§a: i32[]);\n"},
	{name: "two lengths for one buffer", rule: "V11",
		src: "fn f(§b: bytes, n: i32 @len_of(b), m: i32 @len_of(b));\n"},
	{name: "len_of names no parameter", rule: "V11", src: "fn f(n: i32 §@len_of(zz));\n"},
	{name: "len_of names itself", rule: "V11", src: "fn f(n: i32 §@len_of(n));\n"},
	{name: "len_of on a non-integer", rule: "V11", src: "fn f(b: bytes, n: bool §@len_of(b));\n", also: []string{}},
	{name: "len_of names a scalar", rule: "V11", src: "fn f(a: i32, n: i32 §@len_of(a));\n"},
	{name: "len_of on inout", rule: "V11", src: "fn f(b: bytes, inout n: i32 §@len_of(b));\n", also: []string{}},
	{name: "out buffer without a capacity", rule: "V11", src: "fn f(out §d: bytes);\n"},
	{name: "borrowed out buffer without a length", rule: "V11", src: "fn f(out §d: bytes<=8 @borrowed);\n"},
	{name: "out scalar bound to an in buffer", rule: "V11",
		src: "fn f(b: bytes, n: i32 @len_of(b), out m: i32 §@len_of(b));\n"},
	{name: "len_of ret on a function that returns i32", rule: "V11", src: "fn f(out n: i32 §@len_of(ret)) -> i32;\n"},
	{name: "in scalar bound to a borrowed buffer", rule: "V11",
		src: "fn f(out d: bytes<=8 @borrowed, out k: i32 @len_of(d), c: i32 §@len_of(d));\n"},
	{name: "return bound to the return", rule: "V11", src: "fn f(out d: bytes) -> i32 §@len_of(ret);\n",
		also: []string{}},
	{name: "two out scalars for one buffer", rule: "V11",
		src: "fn f(out §d: bytes, cap: i32 @len_of(d), out a: i32 @len_of(d), out b: i32 @len_of(d));\n"},
	// V12
	{name: "offset_in on an in parameter", rule: "V12", src: "fn f(s: str, t: u32 §@offset_in(s));\n"},
	{name: "offset_in on an out i32", rule: "V12", src: "fn f(s: str, out t: i32 §@offset_in(s));\n"},
	{name: "offset_in names an unknown parameter", rule: "V12", src: "fn f(out t: u32 §@offset_in(zz));\n"},
	{name: "offset_in names an out str", rule: "V12",
		src: "fn f(out s: str<=4, out t: u32 §@offset_in(s));\n"},
	{name: "offset_in names bytes", rule: "V12",
		src: "fn f(s: bytes, n: i32 @len_of(s), out t: u32 §@offset_in(s));\n"},
	// V13
	{name: "nullable on an out handle", rule: "V13",
		src: "handle H destroy d;\nfn d(h: H);\nfn f(out h: H §@nullable);\n"},
	{name: "nullable on an i32", rule: "V13", src: "fn f(a: i32 §@nullable);\n"},
	{name: "nullable on a fixed array", rule: "V13", src: "fn f(a: u8[4] §@nullable);\n"},
	{name: "nullable on a scalar return", rule: "V13", src: "fn f() -> i32 §@nullable;\n"},
	// V14
	{name: "borrowed on an in parameter", rule: "V14", src: "fn f(b: bytes §@borrowed, n: i32 @len_of(b));\n"},
	{name: "borrowed on an out str", rule: "V14", src: "fn f(out s: str<=4 §@borrowed);\n"},
	{name: "borrowed on an out scalar", rule: "V14", src: "fn f(out a: i32 §@borrowed);\n"},
	// V15
	{name: "free on an in parameter", rule: "V15", src: "fn f(a: str §@free(\"x\"));\n"},
	{name: "free on a caller-supplied out buffer", rule: "V15",
		src: "fn f(out d: bytes §@free(\"x\"), n: size @len_of(d));\n"},
	{name: "free on a scalar return", rule: "V15", src: "fn f() -> i32 §@free(\"x\");\n"},
	// V16
	{name: "child handle without a parent argument", rule: "V16",
		src: "handle P destroy dp;\nfn dp(p: P);\nhandle C owned_by P destroy dc;\nfn dc(c: C);\n" +
			"@error(null)\nfn mk(x: i32) -> §C;\n"},
	{name: "child handle with two parent arguments", rule: "V16",
		src: "handle P destroy dp;\nfn dp(p: P);\nhandle C owned_by P destroy dc;\nfn dc(c: C);\n" +
			"fn mk(a: P, b: P, out §c: C);\n"},
	{name: "child out handle without a parent argument", rule: "V16",
		src: "handle P destroy dp;\nfn dp(p: P);\nhandle C owned_by P destroy dc;\nfn dc(c: C);\n" +
			"fn mk(out §c: C);\n"},
	// V26
	{name: "struct return of a non-shim without @c", rule: "V26",
		src: "struct S { a: i32; }\nfn f() -> §S;\n"},
	{name: "union return of a non-shim", rule: "V26",
		src: "union U { case 1 A; }\nfn f() -> §U;\n"},
	{name: "shim composite return without @max_out", rule: "V26",
		src: "union U { case 1 A; }\nshim fn f() -> §U;\n"},
	{name: "max_out on a non-shim return", rule: "V26", src: "fn f() -> i32 §@max_out(8);\n"},
	{name: "max_out on a shim scalar return", rule: "V26", src: "shim fn f() -> i32 §@max_out(8);\n"},
	{name: "non-fixed struct return of a non-shim", rule: "V26",
		src: "@c(\"S\")\nstruct S { a: str; }\nfn f() -> §S;\n"},
	{name: "out struct of a non-shim without @c", rule: "V26",
		src: "struct S { a: i32; }\nfn f(out §s: S);\n"},
	// V27
	{name: "str return without a bound", rule: "V27", src: "fn f() -> §str;\n"},
	{name: "bytes return without a bound", rule: "V27", src: "fn f(out n: i32 @len_of(ret)) -> §bytes;\n"},
	{name: "out str without a bound", rule: "V27", src: "fn f(out §s: str);\n"},
	{name: "borrowed out without a bound", rule: "V27",
		src: "fn f(out §d: bytes @borrowed, out n: i32 @len_of(d));\n"},
	{name: "out union", rule: "V27", src: "union U { case 1 A; }\nshim fn f(out §u: U);\n"},
	{name: "out array of variable elements", rule: "V27",
		src: "shim fn f(out §a: str[], cap: i32 @len_of(a));\n"},
	// V28
	{name: "fixed array return of a non-shim", rule: "V28", src: "fn f() -> §u8[4];\n"},
	{name: "array of fixed structs returned by a non-shim", rule: "V28",
		src: "struct S { a: i32; }\nfn f(out n: i32 @len_of(ret)) -> §S[]<=4;\n"},
	{name: "returned bytes without a length binding", rule: "V28", src: "fn f() -> §bytes<=4;\n"},
	// V29 and V31
	{name: "intercept on a server function", rule: "V29", also: []string{"V31"},
		src: "§@intercept\n@allow(server)\nfn f();\n"},
	// V38
	{name: "struct in-parameter", rule: "V38",
		src: "struct S { a: i32; }\nfn f(§a: S);\n"},
	{name: "union in-parameter", rule: "V38", src: "union U { case 1 A; }\nfn f(§a: U);\n"},
	{name: "array of variable elements in-parameter", rule: "V38",
		src: "fn f(§a: str[], n: i32 @len_of(a));\n"},
	{name: "array of unions in-parameter", rule: "V38",
		src: "union U { case 1 A; }\nfn f(§a: U[], n: i32 @len_of(a));\n"},
	{name: "inout struct", rule: "V38", src: "struct S { a: i32; }\nfn f(inout §a: S);\n"},
}
