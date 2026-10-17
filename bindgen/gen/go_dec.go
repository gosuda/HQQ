package gen

import (
	"fmt"
	"strconv"
	"strings"

	"gosuda.org/hqq/bindgen/idl"
)

// goItemRes is what the decoder of one variable response item leaves behind.
type goItemRes struct {
	expr    string // Go expression of the result
	length  string // Go int expression of the produced length, for the @len_of(ret) check
	end     string // variable that holds the end offset
	pending bool   // the helper call's err has not been checked yet
}

// dstFor returns the destination slice of the response item of the parameter at index i (-1: the return).
func (g *goFunc) dstFor(i int) *goParam {
	for _, d := range g.dsts {
		if d.idx == i {
			return d
		}
	}
	return nil
}

// leave closes a block scope without printing a brace (a case clause).
func (g *goFunc) leave() {
	for n, d := range g.declared {
		if d == g.depth {
			delete(g.declared, n)
		}
	}
	g.depth--
}

func (g *goFunc) emitWait() {
	g.reset()
	f, plan := g.f, g.plan
	resp := &plan.Resp
	conv := f.Error

	g.line("func (f %s) Wait() %s {", g.fut, g.resultSig())
	// The zero future of a failed Async call has no client to ask (8.3, rule 4).
	g.line("if f.c == nil {")
	g.line("return %s", g.retWith("rt.ErrFutureUsed"))
	g.line("}")
	g.line("d, err := f.c.c.Wait(f.call, f.id)")
	g.declare("d", "err")
	g.line("if err != nil {")
	g.line("return %s", g.retWith("err"))
	g.line("}")
	g.line("defer d.Release()")

	size := resp.FixedSize
	hasVar := len(resp.Var) > 0
	g.bv = "d.Body"
	if g.needsBodyVar() {
		g.line("b := d.Body")
		g.declare("b")
		g.bv = "b"
	}
	g.line("// %s", g.responseNote())

	// Length and flags, before anything is read.
	var conds []string
	switch {
	case !hasVar && !plan.Detail:
		conds = append(conds, fmt.Sprintf("len(%s) != %d", g.bv, size))
	case size > 0:
		conds = append(conds, fmt.Sprintf("len(%s) < %d", g.bv, size))
	}
	for _, fl := range resp.Fixed {
		if fl.Type.Kind == idl.KBool {
			conds = append(conds, g.boolCheck(goPos{k: int64(fl.Off)}))
		}
	}
	switch {
	case conv == nil:
		conds = append(conds, "d.Fail")
		if hasVar {
			conds = append(conds, "d.Detail")
		}
	case !plan.Detail:
		conds = append(conds, "d.Detail")
	}
	if len(conds) > 0 {
		g.ifMalformed(strings.Join(conds, " || "))
	}

	local, localExpr := g.emitRetLocal()
	g.emitConvention(local)

	exprs := map[int]string{}
	g.emitFixedResults(local, localExpr, exprs)
	if hasVar {
		g.emitVarResults(local, exprs)
	}

	g.emitReturn(exprs)
	g.line("}")
	g.out.blank()
}

// needsBodyVar reports whether Wait names the response body b. A body that is one fixed value without a
// status check is read as d.Body directly.
func (g *goFunc) needsBodyVar() bool {
	resp, conv := &g.plan.Resp, g.f.Error
	checksStatus := conv != nil && conv.Kind != idl.ErrNull
	return len(resp.Var) > 0 || len(resp.Fixed) > 1 || checksStatus
}

// responseNote describes G2 and V2 of the response for the reader of the generated code.
func (g *goFunc) responseNote() string {
	resp := &g.plan.Resp
	var parts []string
	for _, fl := range resp.Fixed {
		parts = append(parts, fmt.Sprintf("%s %s @%d", fl.Name, goWireName(fl.Type), fl.Off))
	}
	note := "G2: none"
	if len(parts) > 0 {
		note = fmt.Sprintf("G2: %s (size %d)", strings.Join(parts, ", "), resp.FixedSize)
	}
	if len(resp.Var) > 0 {
		names := make([]string, len(resp.Var))
		for i, v := range resp.Var {
			names[i] = v.Name
		}
		note += fmt.Sprintf("; V2: %s from %d", strings.Join(names, ", "), resp.FixedPad)
	}
	if g.plan.Detail {
		note += fmt.Sprintf("; a failure carries a detail string at %d", g.plan.G2Pad())
	}
	return note + "."
}

// retField returns the G2 field of the return value, or nil.
func (g *goFunc) retField() *idl.Field {
	for i := range g.plan.Resp.Fixed {
		if g.plan.Resp.Fixed[i].Role == idl.RoleRet {
			return &g.plan.Resp.Fixed[i]
		}
	}
	return nil
}

// emitRetLocal reads the fixed return value into a local when the convention, the @len_of binding or the
// result needs it more than once. It returns the local's name and the Go expression of the result.
func (g *goFunc) emitRetLocal() (local, expr string) {
	f := g.f
	fl := g.retField()
	if fl == nil {
		return "", ""
	}
	needed := f.Error != nil || f.RetAttrs.LenOf != ""
	if !needed {
		return "", ""
	}
	t := fl.Type
	if !t.Kind.IsScalar() && t.Kind != idl.KHandle {
		return "", ""
	}
	pos := goPos{k: int64(fl.Off)}
	switch {
	case t.Kind == idl.KHandle:
		local, expr = "id", goCapitalize(t.Name)+"{f.c.c.NewHandle(id)}"
		g.line("id := hqqLE.Uint32(%s[%s:])", g.bv, pos)
	case f.Error != nil && f.Error.Kind == idl.ErrStatus:
		local, expr = "rc", "rc"
		g.line("rc := %s", g.loadExpr(t, pos))
	default:
		local, expr = "ret", "ret"
		g.line("ret := %s", g.loadExpr(t, pos))
	}
	g.declare(local)
	return local, expr
}

// emitConvention checks the failure flag against the return value and returns the *rt.LibError (4.6.4).
func (g *goFunc) emitConvention(local string) {
	f, plan := g.f, g.plan
	conv := f.Error
	size := plan.Resp.FixedSize
	if conv == nil {
		return
	}
	code := "0"
	switch conv.Kind {
	case idl.ErrStatus:
		oks := make([]string, len(conv.Ok))
		for i, v := range conv.Ok {
			oks[i] = fmt.Sprintf("%s == %d", local, v)
		}
		g.line("if (%s) == d.Fail { // ok(%s) <=> not F_FAIL", strings.Join(oks, " || "), goJoinInts(conv.Ok))
		g.malformed()
		g.line("}")
		code = "int64(rc)"
	case idl.ErrNull:
		if local != "" {
			g.line("if (%s == 0) != d.Fail { // @error(null): F_FAIL <=> NULL", local)
			g.malformed()
			g.line("}")
		}
	case idl.ErrPredicate:
		code = g.predicateCode(local)
	}
	g.line("if d.Fail {")
	g.line("return %s", g.retWith(fmt.Sprintf("d.LibError(%q, %s, %d)", f.Name, code, size)))
	g.line("}")
	if plan.Detail {
		cond := "d.Detail"
		if len(plan.Resp.Var) == 0 {
			cond = fmt.Sprintf("len(%s) != %d || d.Detail", g.bv, size)
		}
		g.ifMalformed(cond)
	}
}

func goJoinInts(v []int64) string {
	parts := make([]string, len(v))
	for i, x := range v {
		parts[i] = strconv.FormatInt(x, 10)
	}
	return strings.Join(parts, ", ")
}

// predicateCode is the Code of the *rt.LibError of a predicate failure: the library's return value when
// it is an integer or a handle, else 0 (a bool, a float or a value in the variable group).
func (g *goFunc) predicateCode(local string) string {
	fl := g.retField()
	if fl == nil || local == "" {
		return "0"
	}
	switch k := fl.Type.Kind; {
	case k.IsInt(), k == idl.KHandle:
		return "int64(" + local + ")"
	case k == idl.KBool:
		return fmt.Sprintf("int64(%s[%d])", g.bv, fl.Off)
	}
	return "0"
}

// emitFixedResults reads the fixed group into result expressions.
func (g *goFunc) emitFixedResults(local, localExpr string, exprs map[int]string) {
	for _, fl := range g.plan.Resp.Fixed {
		t := fl.Type
		pos := goPos{k: int64(fl.Off)}
		switch {
		case fl.Role == idl.RoleRet && local != "":
			exprs[-1] = localExpr
		case fl.Role == idl.RoleRet && !g.retIsResult():
		case t.Kind == idl.KFixedArray || t.Kind == idl.KStruct:
			r := g.alloc("r")
			g.line("var %s %s", r, goType(t, false))
			g.declare(r)
			g.load(t, r, pos)
			exprs[fl.Param] = r
		default:
			exprs[fl.Param] = g.loadExpr(t, pos)
		}
	}
}

// emitVarResults decodes the variable group in order (4.6.3) and checks that it ends the body.
func (g *goFunc) emitVarResults(local string, exprs map[int]string) {
	f, resp := g.f, &g.plan.Resp
	start := strconv.Itoa(resp.FixedPad)
	var last goItemRes
	var checks []string
	for k, fl := range resp.Var {
		res := g.decodeItem(fl, start, k == len(resp.Var)-1)
		start = res.end
		last = res
		exprs[fl.Param] = res.expr
		if fl.Param >= 0 && f.RetAttrs.LenOf == f.Params[fl.Param].Name && local != "" {
			cmp := "uint64(" + res.length + ") != " + local
			if t := g.retField().Type; t.Kind != idl.KU64 && t.Kind != idl.KSize {
				cmp = "uint64(" + res.length + ") != uint64(" + local + ")"
			}
			checks = append(checks, cmp)
		}
	}
	conds := []string{}
	if last.pending {
		conds = append(conds, "err != nil")
	}
	conds = append(conds, fmt.Sprintf("%s != len(b)", last.end))
	g.ifMalformed(strings.Join(append(conds, checks...), " || "))
}

// emitReturn writes the final return statement.
func (g *goFunc) emitReturn(exprs map[int]string) {
	vals := make([]string, 0, len(g.results)+1)
	for _, r := range g.results {
		e, ok := exprs[r.key]
		if !ok {
			g.fail(fmt.Errorf("gen: Go: %s: no decoder for result %d", g.f.Name, r.key))
			return
		}
		vals = append(vals, e)
	}
	g.line("return %s", strings.Join(append(vals, "nil"), ", "))
}

// decodeItem decodes one variable response item that starts at start (a literal for the first item, then
// the name of the offset variable).
func (g *goFunc) decodeItem(fl idl.Field, start string, last bool) goItemRes {
	t := fl.Type
	dst := g.dstFor(fl.Param)
	nullable := fl.Param < 0 && g.f.RetAttrs.Nullable
	if nullable {
		return g.decodeNullable(t, start)
	}
	endName := "off"
	if last {
		endName = "end"
	}
	switch {
	case t.Kind == idl.KBytes && dst != nil:
		n := g.alloc("n")
		g.assign(fmt.Sprintf("rt.GetBytesInto(f.%s, b, %s)", dst.field, start), goLHS{n, "int"}, goLHS{endName, "int"}, goLHS{"err", "error"})
		if !last {
			g.ifMalformed("err != nil")
		}
		return goItemRes{expr: fmt.Sprintf("f.%s[:%s]", dst.field, n), length: n, end: endName, pending: last}
	case t.Kind == idl.KStr:
		r := g.alloc("r")
		g.assign(fmt.Sprintf("rt.GetStr(b, %s, %d)", start, t.Max), goLHS{r, "string"}, goLHS{endName, "int"}, goLHS{"err", "error"})
		if !last {
			g.ifMalformed("err != nil")
		}
		return goItemRes{expr: r, length: "len(" + r + ")", end: endName, pending: last}
	case t.Kind == idl.KBytes:
		r := g.bytesValue(start, t.Max, false)
		return goItemRes{expr: r, length: "len(" + r + ")", end: "off"}
	case t.Kind == idl.KArray:
		out := g.arrayValue(t, start, dst)
		return goItemRes{expr: out, length: "len(" + out + ")", end: "off"}
	}
	start = g.ensureOff(start)
	r := g.alloc("r")
	g.line("var %s %s", r, goType(t, false))
	g.declare(r)
	g.decodeValue(t, r)
	return goItemRes{expr: r, end: "off"}
}

// decodeNullable decodes a nullable str, bytes or T[] return: the length word NULL_LEN is NULL (4.5).
func (g *goFunc) decodeNullable(t *idl.Type, start string) goItemRes {
	g.ensureOff(start)
	g.line("off = rt.Align(off, 4)")
	g.ifMalformed("off+4 > len(b)")
	r := g.alloc("r")
	typ := goType(t, true)
	g.line("var %s %s", r, typ)
	g.declare(r)
	g.open("if hqqLE.Uint32(b[off:]) == rt.NullLen {")
	g.line("off += 4")
	g.leave()
	g.line("} else {")
	g.depth++
	switch t.Kind {
	case idl.KStr:
		s := g.alloc("s")
		g.line("var %s string", s)
		g.declare(s)
		g.line("if %s, off, err = rt.GetStr(b, off, %d); err != nil {", s, t.Max)
		g.malformed()
		g.line("}")
		g.line("%s = &%s", r, s)
	case idl.KBytes:
		g.line("%s = %s", r, g.bytesValue("off", t.Max, true))
	default:
		out := g.arrayValue(t, "off", nil)
		g.line("%s = %s", r, out)
	}
	g.closeBlock()
	return goItemRes{expr: r, length: "len(" + r + ")", end: "off"}
}

// bytesValue decodes a non-nullable bytes value at start into a fresh slice and returns its name. With
// nonNil an empty value is an empty non-nil slice, which a nullable result needs to tell it from NULL.
func (g *goFunc) bytesValue(start string, max int64, nonNil bool) string {
	cnt := g.alloc("cnt")
	g.assign(fmt.Sprintf("rt.GetCount(b, %s, %d, 1)", start, max), goLHS{cnt, "uint32"}, goLHS{"off", "int"}, goLHS{"err", "error"})
	g.ifMalformed("err != nil")
	r := g.alloc("r")
	base := "[]byte(nil)"
	if nonNil {
		base = "[]byte{}"
	}
	g.line("%s := append(%s, b[off:off+int(%s)]...)", r, base, cnt)
	g.declare(r)
	g.line("off += int(%s)", cnt)
	return r
}

// arrayValue decodes a non-nullable T[] at start and returns the name of the slice. With a destination
// the elements are appended to dst[:0]; a caller-supplied buffer is limited to its length, which the
// request carried as the capacity.
func (g *goFunc) arrayValue(t *idl.Type, start string, dst *goParam) string {
	elem := t.Elem
	limit := strconv.FormatInt(t.Max, 10)
	if dst != nil && dst.idx >= 0 {
		limit = "len(f." + dst.field + ")"
	}
	cnt := g.alloc("cnt")
	g.assign(fmt.Sprintf("rt.GetCount(b, %s, %s, %d) // %s, minsize(%s) = %d", start, limit, idl.MinSize(elem), t, elem, idl.MinSize(elem)),
		goLHS{cnt, "uint32"}, goLHS{"off", "int"}, goLHS{"err", "error"})
	g.ifMalformed("err != nil")
	out := g.alloc("out")
	if dst != nil {
		g.line("%s := f.%s[:0]", out, dst.field)
	} else {
		g.line("%s := make([]%s, 0, %s)", out, goType(elem, false), cnt)
	}
	g.declare(out)
	i, v := g.alloc("i"), g.alloc("v")
	g.open("for %s := uint32(0); %s < %s; %s++ {", i, i, cnt, i)
	g.line("var %s %s", v, goType(elem, false))
	g.declare(v)
	g.decodeValue(elem, v)
	g.line("%s = append(%s, %s)", out, out, v)
	g.closeBlock()
	return out
}

// decodeValue decodes a non-nullable value of type t at off into target, an existing assignable Go
// expression. It always works on the variables off and err.
func (g *goFunc) decodeValue(t *idl.Type, target string) {
	switch t.Kind {
	case idl.KStr:
		g.line("if %s, off, err = rt.GetStr(b, off, %d); err != nil {", target, t.Max)
		g.malformed()
		g.line("}")
	case idl.KBytes:
		g.line("%s = %s", target, g.bytesValue("off", t.Max, false))
	case idl.KArray:
		g.line("%s = %s", target, g.arrayValue(t, "off", nil))
	case idl.KUnion:
		g.decodeUnion(t.Un, target)
	case idl.KStruct:
		if goIsFixed(t) {
			g.readFixed(t, target)
			return
		}
		for _, m := range t.St.Members {
			g.decodeValue(m.Type, target+"."+goCapitalize(m.Name))
		}
	default:
		g.readFixed(t, target)
	}
}

// readFixed aligns off, checks that the bytes of the fixed value t exist, reads it into target and
// advances off.
func (g *goFunc) readFixed(t *idl.Type, target string) {
	size, align, _ := idl.SizeOf(t)
	if align > 1 {
		g.line("off = rt.Align(off, %d)", align)
	}
	g.ifMalformed(fmt.Sprintf("off+%d > len(b)", size))
	g.load(t, target, goPos{expr: "off"})
	g.line("off += %d", size)
}

func (g *goFunc) decodeUnion(u *idl.Union, target string) {
	name := goCapitalize(u.Name)
	g.line("off = rt.Align(off, 4)")
	g.ifMalformed("off+4 > len(b)")
	g.line("%s.Tag = %sTag(hqqLE.Uint32(b[off:]))", target, name)
	g.line("off += 4")
	g.line("switch %s.Tag {", target)
	for _, c := range u.Cases {
		g.line("case %s%s:", name, goCapitalize(c.Name))
		g.depth++
		if c.Type != nil {
			g.decodeValue(c.Type, target+"."+goCapitalize(c.Name))
		}
		g.leave()
	}
	g.line("default:")
	g.depth++
	g.malformed()
	g.leave()
	g.line("}")
}
