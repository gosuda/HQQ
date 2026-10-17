package gen

import (
	"go/token"
	"go/types"
	"strconv"
	"strings"
	"testing"

	"gosuda.org/hqq/bindgen/idl"
)

// evalBound evaluates the RESP_BOUND expression of the generated code with every capacity at its limit.
func evalBound(t *testing.T, expr string, caps map[string]int64) int64 {
	t.Helper()
	expr = strings.NewReplacer("rt.VarSlack", strconv.Itoa(idl.VarSlack), "rt.DetailReserve", strconv.Itoa(idl.DetailReserve)).Replace(expr)
	for name, n := range caps {
		expr = strings.ReplaceAll(expr, "int64(len("+name+"))", strconv.FormatInt(n, 10))
	}
	tv, err := types.Eval(token.NewFileSet(), nil, token.NoPos, expr)
	if err != nil {
		t.Fatalf("cannot evaluate %q: %v", expr, err)
	}
	n, err := strconv.ParseInt(tv.Value.ExactString(), 10, 64)
	if err != nil {
		t.Fatalf("%q is not an integer: %v", expr, err)
	}
	return n
}

// TestGoRespBoundIsTheStaticBound: the response bound that every generated call passes to BeginRequest
// equals idl.Plan.StaticBound (4.6.3) when each caller-chosen capacity is at its "<=" bound, so the client
// and the server compute the same flow-control quotient.
func TestGoRespBoundIsTheStaticBound(t *testing.T) {
	cases := map[string]struct {
		spec  *idl.Spec
		strip string
	}{
		"microlib": {loadSpec(t, "microlib/microlib.idl", "ml_"), "ml_"},
		"sqlite":   {loadSpec(t, "sqlite_lowlevel.idl", ""), ""},
		"edge":     {loadGenSpec(t, "edge.idl", ""), ""},
	}
	for _, c := range shapeCases() {
		cases[c.name] = struct {
			spec  *idl.Spec
			strip string
		}{parseIDL(t, c.name+".idl", c.idl, c.strip), c.strip}
	}
	checked := 0
	for name, c := range cases {
		e, err := newGoEmitter(c.spec, GoOptions{Strip: c.strip})
		if err != nil {
			t.Fatal(err)
		}
		for _, f := range c.spec.Wire {
			g := e.newFunc(f)
			caps := map[string]int64{}
			bounded := true
			for _, bt := range f.Plan.BoundTerms(f) {
				if bt.PerCap == 0 {
					continue
				}
				if bt.CapMax == 0 {
					bounded = false
					break
				}
				caps[g.pname(bt.Param)] = bt.CapMax
			}
			if !bounded {
				continue
			}
			expr, _ := g.respMax()
			want, err := f.Plan.StaticBound(f)
			if err != nil {
				t.Fatal(err)
			}
			if got := evalBound(t, expr, caps); got != int64(want) {
				t.Errorf("%s.%s: generated bound %q = %d, plan says %d", name, f.Name, expr, got, want)
			}
			checked++
		}
	}
	if checked < 50 {
		t.Errorf("checked only %d functions", checked)
	}
}

// TestGoRespBoundOfUnboundedBuffer: a caller-supplied buffer without a "<=" bound stays symbolic.
func TestGoRespBoundOfUnboundedBuffer(t *testing.T) {
	spec := parseIDL(t, "x.idl", svc("fn f(out a: bytes, n: u32 @len_of(a), out b: i64[], m: u32 @len_of(b));"), "")
	e, err := newGoEmitter(spec, GoOptions{})
	if err != nil {
		t.Fatal(err)
	}
	expr, _ := e.newFunc(spec.Func("f")).respMax()
	if want := "rt.VarSlack+int64(len(p_a))+rt.VarSlack+int64(len(p_b))*8"; expr != want {
		t.Errorf("bound %q, want %q", expr, want)
	}
}

// TestParserRejectsHqqNames pins the premise of D29: IDL names cannot begin with hqq in any letter case, so
// no generated hqq-prefixed identifier collides with one.
func TestParserRejectsHqqNames(t *testing.T) {
	for _, src := range []string{
		"fn hqqx();", "fn HQQx();", "fn Hqq_x();", "fn f(hqq_a: i32);", "fn f(HqQ: i32);",
		"struct hqq_s { a: i32; }", "struct S { hqqa: i32; }", "const hqq_c: u8 = 1;", "const HQQ_C: u8 = 1;",
		"union U { case 1 hqq_a; }", "@c(\"h *\") handle hqq_h destroy hd;\nfn hd(h: hqq_h);",
	} {
		if _, err := idl.Parse("x.idl", []byte(svc(src)), idl.Options{}); err == nil {
			t.Errorf("%q: the parser accepted a name that starts with hqq", src)
		}
	}
}
