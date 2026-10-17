package idl_test

import (
	"crypto/sha256"
	"encoding/hex"
	"math/big"
	"slices"
	"strings"
	"testing"

	"gosuda.org/hqq/bindgen/idl"
)

func mustParse(t *testing.T, src string, opts idl.Options) *idl.Spec {
	t.Helper()
	spec, err := idl.Parse("test.idl", []byte(src), opts)
	if err != nil {
		t.Fatalf("parse failed:\n%v\nsource:\n%s", err, src)
	}
	return spec
}

// everything uses every production of the grammar of 2.2 and every annotation place of 2.4.
const everything = `// leading comment
idl 1;

@serialize(none)
service everything;

include "a/b.h";
include "c-d_e+f.h";

const A: i8 = -128;
const B: u8 = 255;
const C: i16 = 0x7fff;
const D: u16 = 65535;
const E: i32 = -2147483648;
const F: u32 = 4294967295;
const G: i64 = -9223372036854775808;
const H: u64 = 18446744073709551615;
const I: size = 0x1F;
const LEN: u32 = 007;
const BIG: u32 = 1024;

@c("thing *")
handle Thing destroy thing_free;
@c("sub *")
handle Sub owned_by Thing destroy sub_free;
handle Plain destroy plain_free;

@c("pt_t")
struct Point { x: i32; y: i32; }
struct Grid { cells: Point[LEN]; tag: u8; flags: bool[2]; ratio: f32; wide: f64; }
struct Item { name: str<=BIG; data: bytes; many: i16[]<=10; one: Point; }
union Shape {
    case 0 None;
    case 4294967295 Dot: Point;
    case 2 Names: str[]<=3;
    case 3 Blob: bytes<=0x10;
}

@retain_on_fail
@error(status, ok(0, 1, A))
fn thing_free(t: Thing) -> i32;
@error(status)
fn sub_free(s: Sub) -> i32;
fn plain_free(p: Plain);

@error(null)
fn thing_new(seed: i64 @c("long")) -> Thing @c("thing *");
@error(null)
fn sub_new(t: Thing, out extra: Plain, step: u8) -> Sub;
@error(status, ok(I), detail(thing_msg, t))
fn thing_ctl(t: Thing, op: u32, out result: i64 @c("int64_t")) -> i32;
fn all_scalars(a: bool, b: i8, c: i16, d: i32, e: i64, f: u8, g: u16, h: u32, i: u64, j: size, k: f32, l: f64);
fn inouts(inout a: i32, inout b: f64, out c: bool);
fn buffers(src: bytes<=BIG, n: size @len_of(src), tag: str, nums: i32[], cnt: u32 @len_of(nums), key: u8[16],
           out dst: bytes, cap: size @len_of(dst), out ints: i16[], icap: u32 @len_of(ints), out sum: u8[4]);
fn nullable(t: Thing @nullable, s: str @nullable, b: bytes @nullable, bn: u32 @len_of(b), a: i32[] @nullable,
            an: u32 @len_of(a)) -> str<=8 @nullable;
fn borrowed(out p: bytes<=64 @borrowed, out n: size @len_of(p), out q: i32[]<=4 @borrowed @free("release"),
            out m: u32 @len_of(q), out msg: str<=32 @free("sqlite3_free"));
fn scan(sql: str, nbyte: i32 @len_of(sql), out tail: u32 @offset_in(sql));
@error(predicate(failed))
fn produce(out dst: bytes<=64, cap: size @len_of(dst)) -> size @len_of(dst);
fn produce_buf(out n: size @len_of(ret)) -> bytes<=128 @free("release");
fn produce_len(out n: u32 @len_of(ret)) -> i16[]<=8;
fn corners() -> Point;
@intercept
@allow(wire)
shim fn cells(t: Thing, max: i32) -> Shape @max_out(4096);
shim fn items(t: Thing) -> Item[]<=4 @max_out(0x4000);
shim fn grid_ret() -> Grid @max_out(0x1000);

@allow(server) fn thing_msg(t: Thing) -> str<=1024;
@allow(server) fn failed(r: size) -> bool;
@allow(server) shim fn other(x: i32) -> i32;
/* block
   comment */
`

func TestGrammarEveryProduction(t *testing.T) {
	spec := mustParse(t, everything, idl.Options{GoStrip: "thing_"})

	if spec.Service != "everything" || spec.Serialize != idl.SerializeNone {
		t.Errorf("service %q serialize %v", spec.Service, spec.Serialize)
	}
	if !slices.Equal(spec.Includes, []string{"a/b.h", "c-d_e+f.h"}) {
		t.Errorf("includes %q", spec.Includes)
	}

	wantConsts := map[string]string{
		"A": "-128", "B": "255", "C": "32767", "D": "65535", "E": "-2147483648", "F": "4294967295",
		"G": "-9223372036854775808", "H": "18446744073709551615", "I": "31", "LEN": "7", "BIG": "1024",
	}
	if len(spec.Consts) != len(wantConsts) {
		t.Errorf("%d consts", len(spec.Consts))
	}
	for _, k := range spec.Consts {
		if want := wantConsts[k.Name]; k.Value.String() != want {
			t.Errorf("const %s = %s, want %s", k.Name, k.Value, want)
		}
	}
	if k := spec.Consts[7]; k.Name != "H" || k.Type.Kind != idl.KU64 || k.Value.Cmp(new(big.Int).SetUint64(1<<64-1)) != 0 {
		t.Errorf("const H = %+v", k)
	}

	if len(spec.Handles) != 3 {
		t.Fatalf("%d handles", len(spec.Handles))
	}
	thing, sub, plain := spec.Handles[0], spec.Handles[1], spec.Handles[2]
	if thing.CType != "thing *" || thing.ID != 1 || sub.ID != 2 || plain.ID != 3 {
		t.Errorf("handles %+v %+v %+v", thing, sub, plain)
	}
	if sub.Parent != thing || thing.Parent != nil {
		t.Error("owned_by is not resolved")
	}
	if thing.Destroy == nil || thing.Destroy.Name != "thing_free" || !thing.RetainOnFail || sub.RetainOnFail {
		t.Errorf("destroy and retain_on_fail: %+v %+v", thing, sub)
	}
	if thing.Destroy.Destroys != thing {
		t.Error("Func.Destroys is not set")
	}
	if plain.Destroy.Ret != nil || plain.Destroy.Error != nil {
		t.Error("a void destroy function has no return and no error convention")
	}

	if len(spec.Structs) != 3 || spec.Structs[0].CType != "pt_t" || spec.Structs[1].CType != "" {
		t.Errorf("structs %+v", spec.Structs)
	}
	grid := spec.Structs[1]
	if cells := grid.Members[0].Type; cells.Kind != idl.KFixedArray || cells.Len != 7 || cells.Elem.Name != "Point" {
		t.Errorf("Grid.cells = %v", cells)
	}
	if item := spec.Structs[2]; item.Members[0].Type.Max != 1024 || item.Members[2].Type.Max != 10 ||
		item.Members[2].Type.Kind != idl.KArray {
		t.Errorf("Item members %v %v", item.Members[0].Type, item.Members[2].Type)
	}
	shape := spec.Unions[0]
	var tags []uint32
	for _, c := range shape.Cases {
		tags = append(tags, c.Tag)
	}
	if !slices.Equal(tags, []uint32{0, 4294967295, 2, 3}) {
		t.Errorf("union tags %v", tags)
	}
	if shape.Cases[0].Type != nil || shape.Cases[1].Type == nil || shape.Cases[3].Type.Max != 16 {
		t.Error("union payloads")
	}

	// Functions: declaration order, opcodes for wire functions, none for @allow(server).
	var wire, server []string
	for _, f := range spec.Funcs {
		if f.Opcode == 0 {
			server = append(server, f.Name)
		} else {
			wire = append(wire, f.Name)
			if spec.Wire[f.Opcode-1] != f {
				t.Errorf("Wire[%d] is not %s", f.Opcode-1, f.Name)
			}
		}
	}
	if len(wire) != 19 || wire[0] != "thing_free" || wire[3] != "thing_new" {
		t.Errorf("wire functions %v", wire)
	}
	if !slices.Equal(server, []string{"thing_msg", "failed", "other"}) {
		t.Errorf("server functions %v", server)
	}
	if len(spec.Wire) != 19 {
		t.Errorf("%d wire functions", len(spec.Wire))
	}

	fn := spec.Func("thing_new")
	if fn.GoName != "New" || fn.Params[0].Attrs.CType != "long" || fn.RetAttrs.CType != "thing *" ||
		fn.Error.Kind != idl.ErrNull {
		t.Errorf("thing_new: %+v %+v", fn, fn.Params[0])
	}
	if fn := spec.Func("thing_ctl"); fn.Error.Kind != idl.ErrStatus || !slices.Equal(fn.Error.Ok, []int64{31}) ||
		fn.Error.Detail == nil || fn.Error.Detail.Func != "thing_msg" || !slices.Equal(fn.Error.Detail.Args, []string{"t"}) ||
		fn.Params[2].Attrs.CType != "int64_t" || fn.Params[2].Dir != idl.DirOut {
		t.Errorf("thing_ctl: %+v", fn.Error)
	}
	if fn := spec.Func("thing_free"); !slices.Equal(fn.Error.Ok, []int64{-128, 0, 1}) {
		t.Errorf("ok list not sorted and resolved: %v", fn.Error.Ok)
	}
	if fn := spec.Func("inouts"); fn.Params[0].Dir != idl.DirInOut || fn.Params[1].Dir != idl.DirInOut || fn.Params[2].Dir != idl.DirOut {
		t.Error("directions")
	}
	if fn := spec.Func("buffers"); !fn.Params[1].Derived || fn.Params[1].Attrs.LenOf != "src" || fn.Params[0].Derived ||
		!fn.Params[5].Type.Elem.Kind.IsInt() || fn.Params[5].Type.Len != 16 {
		t.Errorf("buffers params: %+v", fn.Params[1])
	}
	if fn := spec.Func("nullable"); !fn.Params[0].Attrs.Nullable || !fn.RetAttrs.Nullable || !fn.Params[2].Attrs.Nullable {
		t.Error("nullable")
	}
	if fn := spec.Func("borrowed"); !fn.Params[0].Attrs.Borrowed || fn.Params[2].Attrs.Free != "release" ||
		fn.Params[4].Attrs.Free != "sqlite3_free" {
		t.Error("borrowed and free")
	}
	if fn := spec.Func("scan"); fn.Params[2].Attrs.OffsetIn != "sql" {
		t.Error("offset_in")
	}
	if fn := spec.Func("produce"); fn.RetAttrs.LenOf != "dst" || fn.Error.Kind != idl.ErrPredicate || fn.Error.Predicate != "failed" ||
		fn.Params[0].Derived || !fn.Params[1].Derived {
		t.Error("produce")
	}
	if fn := spec.Func("produce_buf"); fn.RetAttrs.Free != "release" || !fn.Params[0].Derived {
		t.Error("produce_buf")
	}
	if fn := spec.Func("cells"); !fn.Shim || !fn.Intercept || fn.MaxOut != 4096 || fn.Opcode == 0 {
		t.Errorf("cells: %+v", fn)
	}
	if fn := spec.Func("items"); fn.MaxOut != 0x4000 {
		t.Errorf("items MaxOut %d", fn.MaxOut)
	}
	if fn := spec.Func("other"); !fn.Shim || fn.Opcode != 0 || fn.Plan != nil {
		t.Errorf("other: %+v", fn)
	}
	for _, f := range spec.Wire {
		if f.Plan == nil {
			t.Errorf("%s has no plan", f.Name)
		}
	}
}

func TestDeclarationOrderIsFreeAndNamespacesAreSeparate(t *testing.T) {
	// A destroy function, a parent and a type may be used before they are declared. A server function may
	// share a name with a type or a const (V1: three namespaces); a wire function cannot, because the Go
	// names would collide (V37).
	src := hdr + `
fn use(a: Thing, b: Kid);
handle Kid owned_by Thing destroy kid_free;
handle Thing destroy thing_free;
@allow(server) fn Thing(t: Thing);
@allow(server) fn LATER(t: Thing) -> i32;
fn thing_free(t: Thing);
fn kid_free(k: Kid);
const LATER: u32 = 5;
struct Early { items: Late[]; }
struct Late { x: u8[LATER]; }
`
	spec := mustParse(t, src, idl.Options{})
	if got := spec.Handles[0].Name; got != "Kid" {
		t.Errorf("handle order: first is %s", got)
	}
}

func TestSerializeAndAllowDefaults(t *testing.T) {
	spec := mustParse(t, hdr+"fn f();\n@allow(wire) fn g();\n@allow(server) fn h();\n", idl.Options{})
	if spec.Serialize != idl.SerializeSession {
		t.Error("the default of @serialize is session")
	}
	if len(spec.Wire) != 2 || spec.Func("h").Opcode != 0 || spec.Func("g").Opcode != 2 {
		t.Error("opcodes")
	}
	spec = mustParse(t, "idl 1;\n@serialize(session)\nservice s;\n", idl.Options{})
	if spec.Serialize != idl.SerializeSession {
		t.Error("explicit session")
	}
}

func TestGoNames(t *testing.T) {
	for _, c := range []struct{ strip, in, want string }{
		{"ml_", "ml_counter_new", "CounterNew"},
		{"", "sqlite3_open_v2", "Sqlite3OpenV2"},
		{"ml_", "ml_add", "Add"},
		{"ml_", "other_add", "OtherAdd"},
		{"", "x", "X"},
		{"", "_x", "X"},
		{"", "a_b_c", "ABC"},
		{"", "aB_cD", "ABCD"},
		{"x", "x", ""},
		{"", "Mixed_Case", "MixedCase"},
	} {
		if got := idl.GoName(c.strip, c.in); got != c.want {
			t.Errorf("GoName(%q, %q) = %q, want %q", c.strip, c.in, got, c.want)
		}
	}
	spec := mustParse(t, hdr+"fn ml_add();\n@allow(server) fn ml_srv();\n", idl.Options{GoStrip: "ml_"})
	if spec.Func("ml_add").GoName != "Add" || spec.Func("ml_srv").GoName != "Srv" {
		t.Error("Func.GoName")
	}
}

// TestNumericEdgeCases covers the integer and string literal rules of 2.1.
func TestNumericEdgeCases(t *testing.T) {
	for _, c := range []struct {
		decl string
		want string
	}{
		{"const K: i64 = -9223372036854775808;", "-9223372036854775808"},
		{"const K: u64 = 18446744073709551615;", "18446744073709551615"},
		{"const K: u64 = 0xFFFFFFFFFFFFFFFF;", "18446744073709551615"},
		{"const K: i32 = -0x80000000;", "-2147483648"},
		{"const K: i32 = 0;", "0"},
		{"const K: i32 = -0;", "0"},
		{"const K: i32 = 00012;", "12"},
		{"const K: u8 = 0x0;", "0"},
		{"const K: u8 = 0xaB;", "171"},
		{"const K: size = 0;", "0"},
	} {
		spec, err := idl.Parse("n.idl", []byte(hdr+c.decl+"\n"), idl.Options{})
		if err != nil {
			t.Errorf("%s: %v", c.decl, err)
			continue
		}
		if got := spec.Consts[0].Value.String(); got != c.want {
			t.Errorf("%s = %s, want %s", c.decl, got, c.want)
		}
	}
	// 128-byte identifiers and 256-byte strings are the longest allowed.
	long := strings.Repeat("a", 128)
	mustParse(t, hdr+"fn "+long+"();\n", idl.Options{})
	mustParse(t, hdr+"include \""+strings.Repeat("a", 256)+"\";\n", idl.Options{})
	mustParse(t, hdr+"const K: u32 = 1;\nstruct S { a: u8[K]; }\n", idl.Options{})
}

func TestWhitespaceAndComments(t *testing.T) {
	crlf := strings.ReplaceAll(everything, "\n", "\r\n")
	a := mustParse(t, everything, idl.Options{GoStrip: "thing_"})
	b := mustParse(t, crlf, idl.Options{GoStrip: "thing_"})
	if string(a.Canonical()) != string(b.Canonical()) {
		t.Error("CRLF line ends change the canonical text")
	}
	compact := "idl 1;/*x*/service s;//y\nfn/**/f(/* */a:i32,b:i32)/**/->i32;fn g();"
	spec := mustParse(t, compact, idl.Options{})
	if len(spec.Wire) != 2 {
		t.Error("tokens separated by comments only")
	}
	// A block comment does not nest: the first "*/" ends it.
	mustParse(t, hdr+"/* a /* b */ fn f();\n", idl.Options{})
	// Comments may carry any valid UTF-8.
	mustParse(t, hdr+"// héllo wörld ✓\n/* 日本語 */ fn f();\n", idl.Options{})
	// A comment opener inside a string is not a comment.
	mustParse(t, hdr+"include \"a//b.h\";\ninclude \"a/./b.h\";\n", idl.Options{})
}

// TestCanonicalIgnoresServerSideDetails: reordering consts, comments, the shim keyword, @c, @free,
// @intercept, @serialize, include and @allow(server) functions do not change the digest (2.6).
func TestCanonicalIgnoresServerSideDetails(t *testing.T) {
	base := `idl 1;
service s;
const A: u8 = 4;
const B: u8 = 8;
handle H destroy hd;
fn hd(h: H);
@error(null)
fn mk() -> H;
fn use(h: H, s: str<=A, out buf: bytes<=B @borrowed, out n: size @len_of(buf), out m: str<=B);
`
	variant := `// reordered, commented, annotated
idl 1;
@serialize(none)
service s;
include "x.h";
@c("my_h *")
handle H destroy hd;
const B: u8 = 0x8;   /* eight */
const A: u8 = 4;
fn hd(h: H);
@intercept
@error(null)
shim fn mk() -> H @c("my_h *");
fn use(h: H @c("my_h *"), s: str<=4, out buf: bytes<=8 @borrowed @free("release"), out n: size @len_of(buf),
       out m: str<=B @free("xfree"));
@allow(server) fn extra(h: H) -> str<=4;
@allow(server) fn hd2(h: H);
`
	a := mustParse(t, base, idl.Options{})
	b := mustParse(t, variant, idl.Options{})
	if string(a.Canonical()) != string(b.Canonical()) {
		t.Errorf("canonical text differs:\n%s\n%s", a.Canonical(), b.Canonical())
	}
	if a.Digest() != b.Digest() {
		t.Error("digest differs")
	}
}

func TestCanonicalChangesWithTheInterface(t *testing.T) {
	base := hdr + `handle H destroy hd;
@error(status)
fn hd(h: H) -> i32;
@error(status, ok(0), detail(dm, h))
fn f(h: H, a: i32) -> i32;
@allow(server) fn dm(h: H) -> str<=8;
`
	d0 := mustParse(t, base, idl.Options{}).Digest()
	for name, mutated := range map[string]string{
		"parameter type":       strings.Replace(base, "a: i32", "a: i64", 1),
		"parameter name":       strings.Replace(base, "a: i32", "b: i32", 1),
		"direction":            strings.Replace(base, "fn f(h: H, a: i32)", "fn f(h: H, out a: i32)", 1),
		"function name":        strings.Replace(base, "fn f(", "fn g(", 1),
		"ok list":              strings.Replace(base, "ok(0)", "ok(0, 1)", 1),
		"detail removed":       strings.Replace(base, ", detail(dm, h)", "", 1),
		"retain_on_fail added": strings.Replace(base, "@error(status)\nfn hd", "@retain_on_fail\n@error(status)\nfn hd", 1),
		"service name":         strings.Replace(base, "service s;", "service t;", 1),
		"function order":       strings.Replace(base, "@error(status)\nfn hd(h: H) -> i32;\n@error(status, ok(0), detail(dm, h))\nfn f(h: H, a: i32) -> i32;", "@error(status, ok(0), detail(dm, h))\nfn f(h: H, a: i32) -> i32;\n@error(status)\nfn hd(h: H) -> i32;", 1),
	} {
		if mutated == base {
			t.Errorf("%s: the mutation changed nothing", name)
			continue
		}
		spec, err := idl.Parse("m.idl", []byte(mutated), idl.Options{})
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if spec.Digest() == d0 {
			t.Errorf("%s: the digest did not change", name)
		}
	}
}

// TestCanonicalDigestTable is the table of digests of small IDL files that 10.3 asks for.
func TestCanonicalDigestTable(t *testing.T) {
	cases := []struct {
		name      string
		src       string
		canonical string
		digest    string
	}{
		{"empty service", "idl 1;\nservice t;\n",
			"HQQ-IDL 1\nservice t\n",
			"55aece679ba4adef480044cc7fc97a94834a82ab113d4d2385994e26f8f0cf83"},
		{"one function", "idl 1;\nservice t;\nfn add(a: i32, b: i32) -> i32;\n",
			"HQQ-IDL 1\nservice t\nfn 1 add p=in:a:i32 p=in:b:i32 ret=i32 err=none ok=- detail=0 max_out=-\n",
			"276555241e93bab65efb2f213a944a94bc174b94d0f95951f5f0aa21066f1f3d"},
		{"handles sort by name and carry retain", `idl 1;
service t;
handle B owned_by A destroy db;
handle A destroy da;
@retain_on_fail
@error(status, ok(2, 0))
fn da(h: A) -> i32;
@error(status)
fn db(h: B) -> i32;
@error(null)
fn mk(a: A) -> B;
`,
			"HQQ-IDL 1\nservice t\nhandle A parent=- destroy=da retain=1\nhandle B parent=A destroy=db retain=0\n" +
				"fn 1 da p=in:h:A ret=i32 err=status ok=0,2 detail=0 max_out=-\n" +
				"fn 2 db p=in:h:B ret=i32 err=status ok=0 detail=0 max_out=-\n" +
				"fn 3 mk p=in:a:A ret=B err=null ok=- detail=0 max_out=-\n",
			"a2532013b18abd5609cd38d33b9bb1aa495be3253294beb87592594985595254"},
		{"structs and unions merge by name, cases by tag", `idl 1;
service t;
union Beta { case 2 Y: u8; case 1 X; }
struct Zed { z: bytes; }
struct Alpha { a: u8[4]; b: str<=3; c: Beta[]<=2; }
`,
			"HQQ-IDL 1\nservice t\nstruct Alpha a:u8[4] b:str<=3 c:Beta[]<=2\nunion Beta 1:X:- 2:Y:u8\nstruct Zed z:bytes\n",
			"158d039d8304d6bc3b180194622da2b2e2a82304d6bfaf176ed31225b14496bb"},
		{"parameter attributes in the fixed order", `idl 1;
service t;
fn f(s: str, n: i32 @len_of(s), out t: u32 @offset_in(s), p: str @nullable,
     out d: bytes<=4 @borrowed, out k: i32 @len_of(d)) -> i32;
`,
			"HQQ-IDL 1\nservice t\nfn 1 f p=in:s:str p=in:n:i32:len_of=s p=out:t:u32:offset_in=s p=in:p:str:nullable " +
				"p=out:d:bytes<=4:borrowed p=out:k:i32:len_of=d ret=i32 err=none ok=- detail=0 max_out=-\n",
			"14dc23ab8238f38eb0f4bc0fed39017a857820ff9c8a1dd1e8b92a7c3cca6058"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			spec := mustParse(t, c.src, idl.Options{})
			if got := string(spec.Canonical()); got != c.canonical {
				t.Errorf("canonical text:\n got %q\nwant %q", got, c.canonical)
			}
			sum := sha256.Sum256([]byte(c.canonical))
			if got := hex.EncodeToString(sum[:]); got != c.digest {
				t.Errorf("table digest is wrong: the text hashes to %s", got)
			}
			d := spec.Digest()
			if got := hex.EncodeToString(d[:]); got != c.digest {
				t.Errorf("digest %s, want %s", got, c.digest)
			}
		})
	}
}

// TestDeepNestingIsLinear: a chain of structs that each contain two copies of the previous one has 2^N
// leaves. Parsing must not walk them.
func TestDeepNestingIsLinear(t *testing.T) {
	var fixed, variable strings.Builder
	fixed.WriteString(hdr + "struct F0 { x: u8; }\n")
	variable.WriteString(hdr + "struct V0 { x: str; }\n")
	for i := 1; i <= 60; i++ {
		n, p := itoa(i), itoa(i-1)
		fixed.WriteString("struct F" + n + " { a: F" + p + "; b: F" + p + "; }\n")
		variable.WriteString("struct V" + n + " { a: V" + p + "; b: V" + p + "; }\n")
	}
	// The fixed chain overflows 2^30 at F31 (V40); the variable chain has no size and is valid.
	_, err := idl.Parse("f.idl", []byte(fixed.String()), idl.Options{})
	if err == nil || !slices.Contains(err.(idl.Diagnostics).Rules(), "V40") {
		t.Errorf("fixed chain: %v", err)
	}
	spec, err := idl.Parse("v.idl", []byte(variable.String()), idl.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(spec.Structs) != 61 {
		t.Errorf("%d structs", len(spec.Structs))
	}
}

// TestDeeplyNestedAnnotationArguments: the grammar allows unbounded nesting; the parser stops at 8 levels
// instead of recursing without bound.
func TestDeeplyNestedAnnotationArguments(t *testing.T) {
	deep := hdr + "@error(status, ok(0))\nfn f() -> i32;\n"
	mustParse(t, deep, idl.Options{})
	src := hdr + "@error(" + strings.Repeat("a(", 1000) + strings.Repeat(")", 1000) + ")\nfn f() -> i32;\n"
	_, err := idl.Parse("d.idl", []byte(src), idl.Options{})
	if err == nil || err.(idl.Diagnostics).Rules()[0] != idl.RuleSyntax {
		t.Errorf("got %v", err)
	}
}
