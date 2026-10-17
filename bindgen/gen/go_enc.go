package gen

import (
	"fmt"
	"strconv"
	"strings"

	"gosuda.org/hqq/bindgen/idl"
)

// reset clears the per-method scope state; the three methods of a function do not share locals.
func (g *goFunc) reset() {
	g.declared = map[string]int{}
	g.counts = map[string]int{}
	g.depth = 0
}

func (g *goFunc) emitAsync() {
	g.reset()
	zero := g.fut + "{}"
	g.line("func (c *Client) %sAsync(%s) (%s, error) {", g.name, g.paramSig(), g.fut)
	for _, p := range g.params {
		g.validate(p, zero)
	}
	req := &g.plan.Req
	g.line("// %s", g.requestNote())
	bodyLen := g.bodyLen()
	respMax, respNote := g.respMax()
	g.line("// %s", respNote)
	g.line("e, err := c.c.BeginRequest(hqqOp%s, %s, %s)", g.name, bodyLen, respMax)
	g.line("if err != nil {")
	g.line("return %s, err", zero)
	g.line("}")
	if req.FixedSize > 0 || len(req.Var) > 0 {
		g.line("body := e.Body()")
	}
	g.fill()
	g.line("call, err := c.c.Submit(e)")
	g.line("if err != nil {")
	g.line("return %s, err", zero)
	g.line("}")
	args := "c, call, call.ID()"
	for _, d := range g.dsts {
		args += ", " + d.name
	}
	g.line("return %s{%s}, nil", g.fut, args)
	g.line("}")
	g.out.blank()
}

// validate refuses an argument before anything is reserved.
func (g *goFunc) validate(p *goParam, zero string) {
	if p.idx < 0 {
		return
	}
	t, par := p.t, g.f.Params[p.idx]
	switch {
	case t.Kind == idl.KHandle:
		g.line("if err := c.c.CheckHandle(%s.Handle, %t); err != nil {", p.name, p.null)
		g.line("return %s, err", zero)
		g.line("}")
	case t.Kind == idl.KStr && p.null:
		g.line("if %s != nil {", p.name)
		g.checkStr("*"+p.name, t.Max, zero)
		g.line("}")
	case t.Kind == idl.KStr:
		g.checkStr(p.name, t.Max, zero)
	case (t.Kind == idl.KBytes || t.Kind == idl.KArray) && t.Max > 0:
		g.line("if len(%s) > %d { // %s: %s", p.name, t.Max, par.Name, t)
		g.line("return %s, rt.ErrInvalidArg", zero)
		g.line("}")
	}
}

func (g *goFunc) checkStr(expr string, max int64, zero string) {
	g.line("if err := rt.CheckStr(%s, %d); err != nil {", expr, max)
	g.line("return %s, err", zero)
	g.line("}")
}

// pname is the Go parameter name of the IDL parameter at index i.
func (g *goFunc) pname(i int) string { return "p_" + g.f.Params[i].Name }

// goWireName names a wire type in the layout comments.
func goWireName(t *idl.Type) string {
	if t.Kind == idl.KHandle {
		return "u32"
	}
	return t.String()
}

// requestNote describes G1 and V1 of the request for the reader of the generated code.
func (g *goFunc) requestNote() string {
	req := &g.plan.Req
	var parts []string
	for _, fl := range req.Fixed {
		switch fl.Role {
		case idl.RoleCap:
			parts = append(parts, fmt.Sprintf("%s.cap u32 @%d", fl.Name, fl.Off))
		default:
			parts = append(parts, fmt.Sprintf("%s %s @%d", fl.Name, goWireName(fl.Type), fl.Off))
		}
	}
	note := "G1: none"
	if len(parts) > 0 {
		note = "G1: " + strings.Join(parts, ", ")
	}
	if len(req.Var) == 0 {
		return note + "; no variable group."
	}
	names := make([]string, len(req.Var))
	for i, v := range req.Var {
		names[i] = v.Name
	}
	return fmt.Sprintf("%s; V1: %s from %d.", note, strings.Join(names, ", "), req.FixedPad)
}

// simpleVar reports whether a variable request item is a non-nullable str or bytes, whose size is one
// helper call.
func (g *goFunc) simpleVar(fl idl.Field) bool {
	par := g.f.Params[fl.Param]
	return (fl.Type.Kind == idl.KStr || fl.Type.Kind == idl.KBytes) && !par.Attrs.Nullable
}

// bodyLen returns the Go expression of the request body length, emitting the statements it needs.
func (g *goFunc) bodyLen() string {
	req := &g.plan.Req
	if len(req.Var) == 0 {
		return strconv.Itoa(req.FixedSize)
	}
	pad := req.FixedPad
	if len(req.Var) == 1 && g.simpleVar(req.Var[0]) {
		w := g.wireSize(req.Var[0])
		if pad == 0 {
			return w
		}
		return fmt.Sprintf("%d+%s", pad, w)
	}
	n := g.alloc("n")
	g.line("%s := %d", n, pad)
	g.declare(n)
	for k, fl := range req.Var {
		if k > 0 {
			g.line("%s = rt.Align(%s, 4)", n, n)
		}
		g.addSize(n, fl)
	}
	return n
}

// wireSize is the encoded size of a non-nullable str or bytes without alignment.
func (g *goFunc) wireSize(fl idl.Field) string {
	if fl.Type.Kind == idl.KStr {
		return fmt.Sprintf("rt.StrWire(len(%s))", g.pname(fl.Param))
	}
	return fmt.Sprintf("rt.BytesWire(len(%s))", g.pname(fl.Param))
}

// addSize emits n += (the encoded size of the variable item fl).
func (g *goFunc) addSize(n string, fl idl.Field) {
	par := g.f.Params[fl.Param]
	name := g.pname(fl.Param)
	switch fl.Type.Kind {
	case idl.KStr:
		if par.Attrs.Nullable {
			g.line("if %s == nil {", name)
			g.line("%s += 4", n)
			g.line("} else {")
			g.line("%s += rt.StrWire(len(*%s))", n, name)
			g.line("}")
			return
		}
		g.line("%s += %s", n, g.wireSize(fl))
	case idl.KBytes:
		g.line("%s += %s", n, g.wireSize(fl))
	case idl.KArray:
		_, align, _ := idl.SizeOf(fl.Type.Elem)
		stride := idl.Stride(fl.Type.Elem)
		g.line("%s += 4", n)
		if align > 4 {
			g.line("if len(%s) > 0 {", name)
			g.line("%s = rt.Align(%s, %d) + len(%s)*%d", n, n, align, name, stride)
			g.line("}")
			return
		}
		g.line("%s += len(%s) * %d", n, name, stride)
	}
}

// respMax returns the Go expression of RESP_BOUND (4.6.3), computed in 64 bits, and a comment that
// names its terms. Constants are folded; only the caller-chosen capacities are symbolic.
func (g *goFunc) respMax() (expr, note string) {
	p := g.plan
	g2pad := int64(p.G2Pad())
	sum := g2pad
	var caps, desc []string
	desc = append(desc, fmt.Sprintf("G2pad %d", g2pad))
	for _, t := range p.BoundTerms(g.f) {
		if t.PerCap == 0 {
			sum += t.Const
			desc = append(desc, fmt.Sprintf("%s %d", t.Name, t.Const))
			continue
		}
		piece := "rt.VarSlack+int64(len(" + g.pname(t.Param) + "))"
		if t.PerCap != 1 {
			piece += "*" + strconv.FormatInt(t.PerCap, 10)
		}
		caps = append(caps, piece)
		desc = append(desc, "VarSlack + cap of "+t.Name)
	}
	var parts []string
	if sum != 0 || len(caps) == 0 {
		parts = append(parts, strconv.FormatInt(sum, 10))
	}
	expr = strings.Join(append(parts, caps...), "+")
	note = "RESP_BOUND = " + strings.Join(desc, " + ") + " (4.6.3), in 64 bits."
	if !p.Detail {
		return expr, note
	}
	det := "rt.DetailReserve"
	if g2pad != 0 {
		det = strconv.FormatInt(g2pad, 10) + "+rt.DetailReserve"
	}
	note = "RESP_BOUND = max(" + strings.Join(desc, " + ") + ", G2pad + DETAIL_RESERVE) (4.6.3), in 64 bits."
	switch {
	case len(caps) > 0:
		return "max(" + expr + ", " + det + ")", note
	case sum <= g2pad+idl.DetailReserve:
		return det, note
	}
	return expr, note
}

// fill emits the statements that write the request body: the fixed group with its padding, then the
// variable group.
func (g *goFunc) fill() {
	req := &g.plan.Req
	var end int64
	for _, fl := range req.Fixed {
		off := int64(fl.Off)
		if off > end {
			g.line("clear(body[%d:%d])", end, off)
		}
		size, _, _ := idl.SizeOf(fl.Type)
		if fl.Role == idl.RoleCap {
			g.line("hqqLE.PutUint32(body[%d:], uint32(len(%s)))", off, g.pname(fl.Param))
		} else {
			g.store(fl.Type, g.pname(fl.Param), goPos{k: off})
		}
		end = off + size
	}
	if len(req.Var) == 0 {
		return
	}
	if pad := int64(req.FixedPad); pad > end {
		g.line("clear(body[%d:%d])", end, pad)
	}
	start := strconv.Itoa(req.FixedPad)
	for k, fl := range req.Var {
		start = g.putVar(fl, start, k == len(req.Var)-1, len(req.Var) == 1)
	}
}

// putVar writes one variable item at start (a literal for the first item, then the variable off) and
// returns where the next item starts. The result of the last item is not needed and is dropped.
func (g *goFunc) putVar(fl idl.Field, start string, last, only bool) string {
	par := g.f.Params[fl.Param]
	name := g.pname(fl.Param)
	t := fl.Type
	if start == "off" {
		g.line("clear(body[off:rt.Align(off, 4)])")
		g.line("off = rt.Align(off, 4)")
	}
	call := func(helper, arg string) {
		rhs := fmt.Sprintf("rt.%s(body, %s, %s)", helper, start, arg)
		if last {
			g.line("%s", rhs)
			return
		}
		g.assign(rhs, goLHS{"off", "int"})
	}
	switch {
	case t.Kind == idl.KStr && par.Attrs.Nullable:
		start = g.ensureOff(start)
		g.line("if %s == nil {", name)
		g.line("hqqLE.PutUint32(body[off:], rt.NullLen)")
		g.line("off += 4")
		g.line("} else {")
		g.line("off = rt.PutStr(body, off, *%s)", name)
		g.line("}")
	case t.Kind == idl.KStr:
		call("PutStr", name)
	case t.Kind == idl.KBytes && par.Attrs.Nullable:
		start = g.ensureOff(start)
		g.line("if %s == nil {", name)
		g.line("hqqLE.PutUint32(body[off:], rt.NullLen)")
		g.line("off += 4")
		g.line("} else {")
		g.line("off = rt.PutBytes(body, off, %s)", name)
		g.line("}")
	case t.Kind == idl.KBytes:
		call("PutBytes", name)
	case t.Kind == idl.KArray:
		start = g.ensureOff(start)
		g.putArray(t, name, par.Attrs.Nullable)
	}
	if only && g.simpleVar(fl) {
		return start
	}
	return "off"
}

// ensureOff makes sure the variable off exists and holds start.
func (g *goFunc) ensureOff(start string) string {
	if start != "off" {
		g.line("off := %s", start)
		g.declare("off")
	}
	return "off"
}

// putArray writes a T[] of fixed elements at off: the count, the padding up to the element alignment (only
// when there is an element, 4.5) and the elements at their stride.
func (g *goFunc) putArray(t *idl.Type, name string, nullable bool) {
	_, align, _ := idl.SizeOf(t.Elem)
	stride := idl.Stride(t.Elem)
	if nullable {
		g.line("if %s == nil {", name)
		g.line("hqqLE.PutUint32(body[off:], rt.NullLen)")
		g.line("} else {")
		g.line("hqqLE.PutUint32(body[off:], uint32(len(%s)))", name)
		g.line("}")
	} else {
		g.line("hqqLE.PutUint32(body[off:], uint32(len(%s)))", name)
	}
	g.line("off += 4")
	g.line("if len(%s) > 0 {", name)
	if align > 4 {
		g.line("clear(body[off:rt.Align(off, %d)])", align)
		g.line("off = rt.Align(off, %d)", align)
	}
	i := g.alloc("i")
	g.line("for %s := range %s {", i, name)
	g.store(t.Elem, name+"["+i+"]", goPos{expr: "off"}.idx(i, stride))
	g.line("}")
	g.line("off += len(%s) * %d", name, stride)
	g.line("}")
}
