package idl_test

import (
	"fmt"
	"strings"
	"testing"

	"gosuda.org/hqq/bindgen/idl"
)

// dumpLayout renders a layout as "name[:cap|:ret]@off:type ... size=N pad=N | name:type ...".
func dumpLayout(l idl.Layout) string {
	var parts []string
	for _, f := range l.Fixed {
		name := f.Name
		switch f.Role {
		case idl.RoleCap:
			name += ".cap"
		case idl.RoleRet:
			name = "ret"
		}
		parts = append(parts, fmt.Sprintf("%s@%d:%s", name, f.Off, f.Type))
	}
	parts = append(parts, fmt.Sprintf("size=%d pad=%d", l.FixedSize, l.FixedPad))
	if len(l.Var) > 0 {
		parts = append(parts, "|")
		for _, f := range l.Var {
			if f.Off != -1 {
				parts = append(parts, "BAD-OFFSET")
			}
			parts = append(parts, fmt.Sprintf("%s:%s", f.Name, f.Type))
		}
	}
	return strings.Join(parts, " ")
}

type planWant struct {
	req, resp string
	g2pad     int
	bound     uint64 // StaticBound
}

// checkPlans compares the plans of the named functions with the layouts that BINDGEN.md 4.14 and 2.7 give.
func checkPlans(t *testing.T, spec *idl.Spec, want map[string]planWant) {
	t.Helper()
	for name, w := range want {
		t.Run(name, func(t *testing.T) {
			f := spec.Func(name)
			if f == nil {
				t.Fatal("no such function")
			}
			if f.Plan == nil {
				t.Fatal("no plan")
			}
			if got := dumpLayout(f.Plan.Req); got != w.req {
				t.Errorf("request layout\n got %s\nwant %s", got, w.req)
			}
			if got := dumpLayout(f.Plan.Resp); got != w.resp {
				t.Errorf("response layout\n got %s\nwant %s", got, w.resp)
			}
			if got := f.Plan.G2Pad(); got != w.g2pad {
				t.Errorf("G2Pad %d, want %d", got, w.g2pad)
			}
			got, err := f.Plan.StaticBound(f)
			if err != nil {
				t.Fatal(err)
			}
			if got != w.bound {
				t.Errorf("StaticBound %d, want %d", got, w.bound)
			}
		})
	}
}

func TestPlanMicrolib(t *testing.T) {
	spec := parseFile(t, "microlib/microlib.idl", idl.Options{GoStrip: "ml_"})
	checkPlans(t, spec, map[string]planWant{
		// 4.14 request 3: two i32, response ret = 42 and nothing after it.
		"ml_add": {"a@0:i32 b@4:i32 size=8 pad=8", "ret@0:i32 size=4 pad=8", 4, 4},
		// Request 1: the single fixed field start: i64.
		"ml_counter_new": {"start@0:i64 size=8 pad=8", "ret@0:Counter size=4 pad=8", 4, 4},
		// Request 2: G1 is c at 0 and delta at 8 (padding 4 to 7), G2 is ret at 0 and total at 8, 16 bytes;
		// the detail string D follows at 16 (vector R2_RESP). RESP_BOUND = max(16, 16 + 1032).
		"ml_counter_add": {"c@0:Counter delta@8:i64 size=16 pad=16", "ret@0:i32 total@8:i64 size=16 pad=16", 16, 1048},
		// Request 13: void function, empty response.
		"ml_counter_free": {"c@0:Counter size=4 pad=8", "size=0 pad=0", 0, 0},
		"ml_cursor_open":  {"c@0:Counter step@4:i32 size=8 pad=8", "ret@0:Cursor size=4 pad=8", 4, 4},
		// ml_cursor_next: value is an out i64 after the i32 status.
		"ml_cursor_next": {"cur@0:Cursor size=4 pad=8", "ret@0:i32 value@8:i64 size=16 pad=16", 16, 16},
		// Request 10: a failed destroy has a 4-byte G2 and no detail.
		"ml_cursor_free": {"cur@0:Cursor size=4 pad=8", "ret@0:i32 size=4 pad=8", 4, 4},
		// Request 4: G1 is key at 0 and the capacity of dst at 4, V1 is src; G2 is ret (u64) at 0, V2 is dst.
		// The cap is not bounded, so the static bound leaves the term out: G2pad = 8.
		"ml_xor":  {"key@0:u8 dst.cap@4:u32 size=8 pad=8 | src:bytes<=65536", "ret@0:size size=8 pad=8 | dst:bytes", 8, 8},
		"ml_hash": {"size=0 pad=0 | s:str<=255", "ret@0:u64 size=8 pad=8", 8, 8},
		// Request 8: no G2, the body starts with the array count; the term is max_out.
		"ml_cells": {"c@0:Counter max@4:i32 size=8 pad=8", "size=0 pad=0 | ret:Cell[]<=256", 0, 8192},
		// Request 12: a 4-byte G2 with detail pads to 8 (G2pad), RESP_BOUND = 8 + 1032.
		"ml_counter_check": {"c@0:Counter limit@4:i32 size=8 pad=8", "ret@0:i32 size=4 pad=8", 8, 1040},
	})
}

// TestPlanMicrolibXorBound evaluates the symbolic terms for the caps of the vectors R4 (cap 8) and C_RESP (cap 110).
func TestPlanMicrolibXorBound(t *testing.T) {
	spec := parseFile(t, "microlib/microlib.idl", idl.Options{})
	f := spec.Func("ml_xor")
	terms := f.Plan.BoundTerms(f)
	if len(terms) != 1 {
		t.Fatalf("%d terms, want 1", len(terms))
	}
	term := terms[0]
	if term.Name != "dst" || term.Param != 3 || term.Const != idl.VarSlack || term.PerCap != 1 || term.CapMax != 0 {
		t.Errorf("term %+v", term)
	}
	bound := func(cap int64) int64 { return int64(f.Plan.G2Pad()) + term.Const + term.PerCap*cap }
	if got := bound(8); got != 27 {
		t.Errorf("RESP_BOUND at cap 8 is %d, the document says 27", got)
	}
	if got := bound(110); got != 129 {
		t.Errorf("RESP_BOUND at cap 110 is %d, the document says 129", got)
	}
	// Field roles and parameter indices.
	req := f.Plan.Req
	if req.Fixed[1].Role != idl.RoleCap || req.Fixed[1].Param != 3 {
		t.Errorf("cap field %+v", req.Fixed[1])
	}
	if f.Plan.Resp.Fixed[0].Role != idl.RoleRet || f.Plan.Resp.Fixed[0].Param != -1 {
		t.Errorf("ret field %+v", f.Plan.Resp.Fixed[0])
	}
	if f.Plan.Req.VarStart() != 8 || f.Plan.Resp.VarStart() != 8 {
		t.Errorf("VarStart %d and %d, want 8 and 8", f.Plan.Req.VarStart(), f.Plan.Resp.VarStart())
	}
	for _, p := range f.Params {
		wantDerived := p.Name == "n" || p.Name == "cap"
		if p.Derived != wantDerived {
			t.Errorf("param %s Derived = %v", p.Name, p.Derived)
		}
	}
}

func TestPlanMicrolibCells(t *testing.T) {
	spec := parseFile(t, "microlib/microlib.idl", idl.Options{})
	f := spec.Func("ml_cells")
	terms := f.Plan.BoundTerms(f)
	if len(terms) != 1 || terms[0].Const != 8192 || terms[0].PerCap != 0 || terms[0].Param != -1 {
		t.Errorf("terms %+v", terms)
	}
	if f.MaxOut != 8192 {
		t.Errorf("MaxOut %d", f.MaxOut)
	}
}

func TestPlanSQLite(t *testing.T) {
	spec := parseFile(t, "sqlite_lowlevel.idl", idl.Options{})
	checkPlans(t, spec, map[string]planWant{
		"sqlite3_open_v2": {
			"flags@0:i32 size=4 pad=8 | filename:str<=4096 zvfs:str<=64",
			"ret@0:i32 db@4:Db size=8 pad=8", 8, 1040,
		},
		"sqlite3_prepare_v2": {
			// nbyte is derived (the length of sql) and absent; tail is an offset_in result.
			"db@0:Db size=4 pad=8 | sql:str<=1048576",
			"ret@0:i32 stmt@4:Stmt tail@8:u32 size=12 pad=16", 16, 1048,
		},
		"sqlite3_step":         {"stmt@0:Stmt size=4 pad=8", "ret@0:i32 size=4 pad=8", 8, 1040},
		"sqlite3_bind_int64":   {"stmt@0:Stmt index@4:i32 value@8:i64 size=16 pad=16", "ret@0:i32 size=4 pad=8", 8, 1040},
		"sqlite3_column_count": {"stmt@0:Stmt size=4 pad=8", "ret@0:i32 size=4 pad=8", 4, 4},
		"bind_text": {
			"stmt@0:Stmt index@4:i32 size=8 pad=8 | text:str<=1048576",
			"ret@0:i32 size=4 pad=8", 8, 1040,
		},
		// fetch_rows: no G2, the term is max_out 65536.
		"fetch_rows": {"stmt@0:Stmt max@4:i32 size=8 pad=8", "size=0 pad=0 | ret:Rows", 0, 65536},
	})
	f := spec.Func("sqlite3_prepare_v2")
	if !f.Plan.Detail {
		t.Error("prepare has a detail function")
	}
	if spec.Func("sqlite3_errmsg").Plan != nil || spec.Func("stmt_errmsg").Plan != nil {
		t.Error("@allow(server) functions have no plan")
	}
}

// TestPlanLoweringShapes covers the parameter shapes of section 3.2 that microlib and the sqlite excerpt
// do not use.
func TestPlanLoweringShapes(t *testing.T) {
	src := hdr + `
@c("ZSTD_bounds")
struct Bounds { lo: i32; hi: i32; }
struct Mixed { a: u8; b: u64; c: u16; }

fn in_out(x: i32, inout y: i64, out z: i32);
fn order(a: u8, out d: bytes, c: u32 @len_of(d), b: u64);
fn fixed_arrays(key: u8[32], out sum: u8[32], n: i16[3]);
fn borrowed_out(out p: bytes<=100 @borrowed, out n: size @len_of(p));
fn free_str(out msg: str<=50 @free("sqlite3_free"));
fn ret_buf(out n: size @len_of(ret)) -> bytes<=64;
fn ret_arr(out n: size @len_of(ret)) -> i32[]<=10;
fn out_arr(out a: i32[], cap: u32 @len_of(a), out used: u32 @len_of(a));
fn offset(sql: str, out tail: u32 @offset_in(sql));
fn ret_struct() -> Bounds;
shim fn mixed_ret() -> Mixed @max_out(64);
shim fn bounded_out(out a: bytes<=32, cap: u32 @len_of(a));
fn nothing();
fn str_ret() -> str<=7 @nullable;
`
	spec, err := idl.Parse("shapes.idl", []byte(src), idl.Options{})
	if err != nil {
		t.Fatal(err)
	}
	checkPlans(t, spec, map[string]planWant{
		"in_out": {"x@0:i32 y@8:i64 size=16 pad=16", "y@0:i64 z@8:i32 size=12 pad=16", 12, 12},
		// G1 lists wire-in values and caps in declaration order: a, the capacity of d, b.
		"order":        {"a@0:u8 d.cap@4:u32 b@8:u64 size=16 pad=16", "size=0 pad=0 | d:bytes", 0, 0},
		"fixed_arrays": {"key@0:u8[32] n@32:i16[3] size=38 pad=40", "sum@0:u8[32] size=32 pad=32", 32, 32},
		"borrowed_out": {"size=0 pad=0", "size=0 pad=0 | p:bytes<=100", 0, 111},
		"free_str":     {"size=0 pad=0", "size=0 pad=0 | msg:str<=50", 0, 62},
		"ret_buf":      {"size=0 pad=0", "size=0 pad=0 | ret:bytes<=64", 0, 75},
		"ret_arr":      {"size=0 pad=0", "size=0 pad=0 | ret:i32[]<=10", 0, 51},
		"out_arr":      {"a.cap@0:u32 size=4 pad=8", "size=0 pad=0 | a:i32[]", 0, 0},
		"offset":       {"size=0 pad=0 | sql:str", "tail@0:u32 size=4 pad=8", 4, 4},
		"ret_struct":   {"size=0 pad=0", "ret@0:Bounds size=8 pad=8", 8, 8},
		// A fixed struct is part of G2 even when a shim returns it; its size already counts.
		"mixed_ret":   {"size=0 pad=0", "ret@0:Mixed size=24 pad=24", 24, 24},
		"bounded_out": {"a.cap@0:u32 size=4 pad=8", "size=0 pad=0 | a:bytes<=32", 0, 43},
		"nothing":     {"size=0 pad=0", "size=0 pad=0", 0, 0},
		"str_ret":     {"size=0 pad=0", "size=0 pad=0 | ret:str<=7", 0, 19},
	})

	terms := func(name string) []idl.BoundTerm {
		f := spec.Func(name)
		return f.Plan.BoundTerms(f)
	}
	for _, c := range []struct {
		fn   string
		want idl.BoundTerm
	}{
		{"order", idl.BoundTerm{Name: "d", Param: 1, Const: 11, PerCap: 1}},
		{"out_arr", idl.BoundTerm{Name: "a", Param: 0, Const: 11, PerCap: 4}},
		{"bounded_out", idl.BoundTerm{Name: "a", Param: 0, Const: 11, PerCap: 1, CapMax: 32}},
		{"borrowed_out", idl.BoundTerm{Name: "p", Param: 0, Const: 111}},
		{"free_str", idl.BoundTerm{Name: "msg", Param: 0, Const: 62}},
		{"ret_buf", idl.BoundTerm{Name: "ret", Param: -1, Const: 75}},
		{"ret_arr", idl.BoundTerm{Name: "ret", Param: -1, Const: 51}},
	} {
		got := terms(c.fn)
		if len(got) != 1 || got[0] != c.want {
			t.Errorf("%s: terms %+v, want [%+v]", c.fn, got, c.want)
		}
	}
	if got := terms("nothing"); len(got) != 0 {
		t.Errorf("nothing: terms %+v", got)
	}

	// Derived parameters are in no layout.
	f := spec.Func("order")
	for _, p := range f.Params {
		if p.Derived != (p.Name == "c") {
			t.Errorf("order: param %s Derived = %v", p.Name, p.Derived)
		}
	}
	if !spec.Func("borrowed_out").Params[1].Derived {
		t.Error("the out length scalar of a borrowed buffer is derived")
	}
}

// TestPlanFixedGroupAlignment checks natural alignment inside G1 and G2 with nested fixed structs.
func TestPlanFixedGroupAlignment(t *testing.T) {
	src := hdr + `
struct Pair { a: u8; b: u32; }
struct Wide { p: Pair; q: u64; }
fn f(x: u8, p: Pair[2], w: Wide, y: u16) -> u8;
`
	// Pair is an in-parameter array element: V38 allows a fixed array. Wide is a struct in-parameter, which
	// V38 rejects, so the layout is exercised through the array only.
	_, err := idl.Parse("a.idl", []byte(src), idl.Options{})
	if err == nil {
		t.Fatal("a struct in-parameter must be rejected (V38)")
	}
	src = hdr + `
struct Pair { a: u8; b: u32; }
struct Wide { p: Pair; q: u64; }
fn f(x: u8, p: Pair[2], y: u16, out w: Wide[1], out z: u8);
`
	spec, err := idl.Parse("a.idl", []byte(src), idl.Options{})
	if err != nil {
		t.Fatal(err)
	}
	checkPlans(t, spec, map[string]planWant{
		// x@0, p at the alignment of Pair (4) with stride 8: 4..20, y at 20..22.
		// Wide is {p@0 (8), q@8 (8)}, 16 bytes, aligned to 8: w@0, z@16.
		"f": {"x@0:u8 p@4:Pair[2] y@20:u16 size=22 pad=24", "w@0:Wide[1] z@16:u8 size=17 pad=24", 17, 17},
	})
	for _, n := range []string{"Pair", "Wide"} {
		var st *idl.Struct
		for _, s := range spec.Structs {
			if s.Name == n {
				st = s
			}
		}
		size, align, fixed := idl.SizeOf(&idl.Type{Kind: idl.KStruct, Name: n, St: st})
		t.Logf("%s: size %d align %d fixed %v", n, size, align, fixed)
	}
}
