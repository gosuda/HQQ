package gen

import (
	"fmt"
	"strings"

	"gosuda.org/hqq/bindgen/idl"
)

// emit generates the glue function of f: its comment line and the function.
func (o *opGen) emit() []string {
	o.analyze()
	o.declareLocals()
	o.decode()
	o.lookups()
	o.respBegin()
	o.hookFill()
	o.lockBegin()
	o.hook()
	o.cascade()
	o.preclaim()
	o.call()
	o.post()

	var out code
	for _, l := range wrapComment(o.comment(), 100) {
		out.l("%s", l)
	}
	out.l("static hqqrpc_status op_%s(hqqrpc_call *c) {", o.f.Name)
	out.in()
	body := joinLines(o.b.lines)
	core := []decl{}
	if usesSession.MatchString(body) {
		core = append(core, decl{prio: 0, rank: rkS, typ: "hqqrpc_session *", name: "s", init: "c->session"})
	}
	core = append(core,
		decl{prio: 0, rank: rkR, typ: "hqqrpc_rd", name: "r"},
		decl{prio: 0, rank: rkSt, typ: "hqqrpc_status", name: "st"},
		decl{prio: 0, rank: rkW, typ: "hqqrpc_wr", name: "w"})
	var ds []decl
	for _, d := range append(core, o.decls...) {
		ds = append(ds, d)
	}
	sortDecls(ds)
	writeDecls(&out, ds)
	out.l("")
	out.lines = append(out.lines, o.b.lines...)
	out.out()
	out.l("}")
	return out.lines
}

func sortDecls(ds []decl) {
	// stable insertion by (prio, rank); prio 1 keeps creation order
	for i := 1; i < len(ds); i++ {
		for j := i; j > 0 && less(ds[j], ds[j-1]); j-- {
			ds[j], ds[j-1] = ds[j-1], ds[j]
		}
	}
}

func less(a, b decl) bool {
	if a.prio != b.prio {
		return a.prio < b.prio
	}
	return a.prio == 0 && a.rank < b.rank
}

// comment is the one-line description above a glue function.
func (o *opGen) comment() string {
	f := o.f
	var parts []string
	parts = append(parts, fmt.Sprintf("op %d: %s.", f.Opcode, f.Name))
	var tags []string
	if f.Shim {
		tags = append(tags, "shim")
	}
	if f.Intercept {
		tags = append(tags, "@intercept")
	}
	if f.Destroys != nil {
		tags = append(tags, "destroy of "+f.Destroys.Name)
		if f.Destroys.RetainOnFail {
			tags = append(tags, "@retain_on_fail")
		}
	}
	if f.Error != nil {
		tags = append(tags, "@error("+f.Error.Kind.String()+")")
	}
	if o.detail {
		tags = append(tags, "detail")
	}
	if len(tags) > 0 {
		parts = append(parts, strings.Join(tags, ", ")+".")
	}
	if g1 := o.layoutText(&o.plan.Req, "G1", "V1", true); g1 != "" {
		parts = append(parts, g1)
	}
	if g2 := o.layoutText(&o.plan.Resp, "G2", "V2", false); g2 != "" {
		parts = append(parts, g2)
	}
	parts = append(parts, fmt.Sprintf("RESP_BOUND %s.", boundComment(o)))
	return sanitizeComment(strings.Join(parts, " "))
}

// wrapComment formats text as a C comment of lines of at most width columns.
func wrapComment(text string, width int) []string {
	words := strings.Fields(text)
	var lines []string
	cur := "/*"
	for _, w := range words {
		if len(cur)+1+len(w) > width && cur != "/*" && cur != " *" {
			lines = append(lines, cur)
			cur = " *"
		}
		cur += " " + w
	}
	lines = append(lines, cur+" */")
	return lines
}

func boundComment(o *opGen) string {
	if b, err := o.plan.StaticBound(o.f); err == nil {
		return fmt.Sprint(b)
	}
	return "?"
}

func (o *opGen) layoutText(l *idl.Layout, g, v string, req bool) string {
	var fixed []string
	for _, fl := range l.Fixed {
		name := fl.Name
		typ := fl.Type.String()
		if fl.Role == idl.RoleCap {
			name += ".cap"
			typ = "u32"
		}
		if fl.Type.Kind == idl.KHandle {
			typ = "u32"
		}
		fixed = append(fixed, fmt.Sprintf("%s %s @%d", name, typ, fl.Off))
	}
	var vars []string
	for _, fl := range l.Var {
		vars = append(vars, fl.Name)
	}
	var s []string
	if len(fixed) > 0 {
		s = append(s, fmt.Sprintf("%s: %s (%d).", g, strings.Join(fixed, ", "), l.FixedSize))
	}
	if len(vars) > 0 {
		s = append(s, fmt.Sprintf("%s: %s.", v, strings.Join(vars, ", ")))
	}
	return strings.Join(s, " ")
}

// ---- interceptor, lock, cascade ------------------------------------------------------------------

// hookFill sets the members of the interceptor args from the decoded locals.
func (o *opGen) hookFill() {
	if !o.intercept {
		return
	}
	c := &o.b
	a := o.gl("a")
	o.local(o.c.argStructName(o.f), a, "")
	for _, m := range o.hookArgs {
		q := m.pv
		var v string
		switch q.cls {
		case cInScalar, cInOut:
			v = q.val
		case cInHandle:
			v = "(" + m.ctype + ")" + q.hptr
		case cInStr, cInBytes, cInArray:
			v = q.val
			if q.copy {
				v = q.cpy
			}
		case cInFixedArr:
			v = fmt.Sprintf("(%s)(%s + %d)", m.ctype, o.gv(), q.g1off)
			if q.copy {
				v = q.cpy
			}
		case cLenIn:
			ct := valueC(q.p.Type, q.p.Attrs)
			if buf := o.boundBuffer(q); buf.cls == cOutBuf {
				v = "(" + ct + ")" + buf.capL
			} else {
				v = "(" + ct + ")" + buf.n
			}
		}
		c.l("%s.%s = %s;", a, m.name, v)
	}
}

func (o *opGen) lockBegin() {
	if o.lockOn {
		o.b.l("hqqrpc_exec_begin(s);")
	}
	o.locked = true
}

func (o *opGen) unlock() {
	if o.lockOn {
		o.b.l("hqqrpc_exec_end(s);")
	}
	o.locked = false
}

func (o *opGen) hook() {
	if !o.intercept {
		return
	}
	c := &o.b
	a := o.gl("a")
	c.block(fmt.Sprintf("if ((st = %s(s, &%s)) != HQQRPC_OK)", o.c.hookName(o.f), a), func() {
		if o.lockOn {
			c.l("hqqrpc_exec_end(s);")
		}
		c.l("return hqqrpc_hook_status(st);")
	})
	for _, q := range o.pv {
		if q.cls == cInOut {
			c.l("%s = %s.%s;", q.val, a, q.p.Name)
		}
	}
}

func (o *opGen) cascade() {
	if o.destroyed == nil || !o.c.children[o.f.Destroys] {
		return
	}
	c := &o.b
	q := o.destroyed
	stmt := fmt.Sprintf("hqqrpc_handle_destroy_children(s, %s); /* the lock is held; %s descendants go first */", q.hid, o.f.Destroys.Name)
	if q.p.Attrs.Nullable {
		c.l("if (%s != 0)", q.hid)
		c.in()
		c.l("%s", stmt)
		c.out()
		return
	}
	c.l("%s", stmt)
}

// preclaim claims the response slot before the call when the library writes into it (4.7.3).
func (o *opGen) preclaim() {
	needV := o.inPlace != nil
	needPad := varWriterRet(o.f) && o.g2size%8 != 0 && o.v2
	if !needV && !needPad {
		return
	}
	c := &o.b
	oo := ""
	var conds []string
	if o.g2size > 0 {
		oo = o.ov()
		c.l("%s = hqqrpc_wr_fixed(&w, %d, %d);", oo, o.g2align, o.g2size)
		conds = append(conds, oo+" == NULL")
		o.preclaimed = true
	}
	if needV {
		v := o.vv()
		q := o.inPlace
		pad, rel, _, _ := o.spanGeometry(q)
		size := fmt.Sprintf("(size_t)%d + %s", rel, o.capTimes(q))
		align := 4
		if pad > 0 {
			align = 1
		}
		c.l("%s = hqqrpc_wr_span(&w, %d, %s);", v, align, size)
		conds = append(conds, v+" == NULL")
	}
	if len(conds) > 0 {
		c.block("if ("+strings.Join(conds, " || ")+")", func() { o.failRet(c, "w.err", false) })
	}
	if needPad {
		c.block("if (hqqrpc_wr_pad(&w, 8) != HQQRPC_OK)", func() { o.failRet(c, "w.err", false) })
	}
}

// spanGeometry is the layout of the in-place buffer span: padding bytes before the length prefix, the
// offset of the data from the span start, the element stride and the element alignment.
func (o *opGen) spanGeometry(q *pv) (pad, rel, stride, ealign int64) {
	pad = int64(o.g2pad - o.g2size)
	stride, ealign = 1, 1
	if q.p.Type.Kind == idl.KArray {
		stride = idl.Stride(q.p.Type.Elem)
		ealign = alignT(q.p.Type.Elem)
	}
	rel = pad + roundUp(4, max(ealign, 1))
	return
}

// capTimes is cap * stride as a size_t expression.
func (o *opGen) capTimes(q *pv) string {
	_, _, stride, _ := o.spanGeometry(q)
	if stride == 1 {
		return q.capL
	}
	return fmt.Sprintf("(size_t)%s * %d", q.capL, stride)
}

// ---- the call ------------------------------------------------------------------------------------

func (o *opGen) call() {
	c := &o.b
	var args []string
	for _, q := range o.pv {
		switch q.cls {
		case cInOut, cOutScalar, cOutStruct, cOutStr, cOutBorrowed, cOutHandle, cOutOffset, cLenOut:
			args = append(args, "&"+q.val)
		case cOutFixedArr:
			args = append(args, q.val)
		case cOutBuf:
			if q == o.inPlace {
				_, rel, _, _ := o.spanGeometry(q)
				args = append(args, fmt.Sprintf("(void *)(%s + %d)", o.vv(), rel))
			} else {
				args = append(args, "(void *)"+q.buf)
			}
		default:
			args = append(args, o.valueExpr(q))
		}
	}
	switch {
	case varWriterRet(o.f):
		args = append(args, "&w")
	case fixedStructRet(o.f):
		args = append(args, "&ret")
	}
	expr := fmt.Sprintf("%s(%s)", o.f.Name, strings.Join(args, ", "))
	switch {
	case compositeRet(o.f):
		c.l("st = %s;", expr)
	case o.f.Ret != nil:
		c.l("ret = %s;", expr)
	default:
		c.l("%s;", expr)
	}
}

// ---- conventions ---------------------------------------------------------------------------------

func (o *opGen) failedVar() string { return o.gl("failed") }

// okCond and failCond are the success and failure tests of the error convention (4.6.4).
func (o *opGen) okCond() string {
	switch o.conv.Kind {
	case idl.ErrStatus:
		var parts []string
		for _, v := range o.conv.Ok {
			parts = append(parts, fmt.Sprintf("ret == %d", v))
		}
		return strings.Join(parts, " || ")
	case idl.ErrNull:
		return "ret != NULL"
	}
	return "!" + o.failedVar()
}

func (o *opGen) failCond() string {
	switch o.conv.Kind {
	case idl.ErrStatus:
		var parts []string
		for _, v := range o.conv.Ok {
			parts = append(parts, fmt.Sprintf("ret != %d", v))
		}
		return strings.Join(parts, " && ")
	case idl.ErrNull:
		return "ret == NULL"
	}
	return o.failedVar()
}

// ---- after the call ------------------------------------------------------------------------------

func (o *opGen) post() {
	c := &o.b
	f := o.f
	if o.conv != nil {
		o.usesFlags = true
		o.local("uint8_t", o.gl("flags"), "0")
		if o.conv.Kind == idl.ErrPredicate {
			o.local("int", o.failedVar(), "")
			c.l("%s = %s(ret);", o.failedVar(), o.conv.Predicate)
		}
	}
	if f.Destroys != nil {
		o.consume()
	}
	shimStatus := varWriterRet(f) || fixedStructRet(f)
	if o.writeInLock {
		if shimStatus {
			c.block("if ((st = hqqrpc_shim_status(st)) != HQQRPC_OK)", func() { o.failRet(c, "st", true) })
		}
		o.claimG2()
		o.storeRet(false)
		o.armsInLock()
		if o.retHandle != nil {
			o.storeRet(true)
		}
		o.unlock()
		c.l("return hqqrpc_resp_commit(c, &w, %s);", o.flagsExpr())
		return
	}
	lockSetsFlags := o.lockArms()
	o.unlock()
	if shimStatus {
		c.l("if ((st = hqqrpc_shim_status(st)) != HQQRPC_OK)")
		c.in()
		c.l("return st;")
		c.out()
	}
	if o.conv != nil && !lockSetsFlags {
		c.l("if (%s)", o.failCond())
		c.in()
		c.l("%s = HQQRPC_F_FAIL;", o.gl("flags"))
		c.out()
	}
	o.claimG2()
	o.storeRet(false)
	if o.retHandle != nil {
		o.storeRet(true)
	}
	o.armsAfterLock()
	c.l("return hqqrpc_resp_commit(c, &w, %s);", o.flagsExpr())
}

func (o *opGen) flagsExpr() string {
	if o.usesFlags {
		return o.gl("flags")
	}
	return "0"
}

// consume writes the handle-table update of a destroy function (5.1).
func (o *opGen) consume() {
	c := &o.b
	q := o.destroyed
	stmt := fmt.Sprintf("hqqrpc_handle_consume(s, %s);", q.hid)
	guard := ""
	if q.p.Attrs.Nullable {
		guard = fmt.Sprintf("%s != 0", q.hid)
	}
	switch {
	case o.f.Ret == nil:
		if guard != "" {
			c.l("if (%s)", guard)
			c.in()
			c.l("%s          /* void destroy: always consumed */", stmt)
			c.out()
		} else {
			c.l("%s          /* void destroy: always consumed */", stmt)
		}
	case o.f.Destroys.RetainOnFail:
		cond := o.okCond()
		if guard != "" {
			cond = guard + " && (" + cond + ")"
		}
		c.l("if (%s)", cond)
		c.in()
		c.l("%s /* @retain_on_fail: a failed destroy keeps the entry */", stmt)
		c.out()
	default:
		if guard != "" {
			c.l("if (%s)", guard)
			c.in()
			c.l("%s /* always: the object is gone whatever ret says (5.1) */", stmt)
			c.out()
		} else {
			c.l("%s /* always: the object is gone whatever ret says (5.1) */", stmt)
		}
	}
}

// claimG2 claims the fixed response group unless it was claimed before the call.
func (o *opGen) claimG2() {
	if o.preclaimed || o.g2size == 0 {
		return
	}
	c := &o.b
	oo := o.ov()
	cond := fmt.Sprintf("(%s = hqqrpc_wr_fixed(&w, %d, %d)) == NULL", oo, o.g2align, o.g2size)
	o.ifFail(c, cond, "w.err", false)
}

// ifFail writes `if (cond) return status;`, with the cleanup of a failure after the call when needed.
func (o *opGen) ifFail(c *code, cond, status string, frees bool) {
	if (o.lockOn && o.locked) || (frees && len(o.frees) > 0) {
		o.failBlock(c, cond, status)
		return
	}
	c.l("if (%s)", cond)
	c.in()
	c.l("return %s;", status)
	c.out()
}

// store writes the wire form of the fixed value src of type t at dst + off (4.5, 4.6.1).
func (o *opGen) store(c *code, dst string, off int64, t *idl.Type, src string, depth int) {
	at := dst
	if off != 0 {
		at = fmt.Sprintf("%s + %d", dst, off)
	}
	switch t.Kind {
	case idl.KBool:
		c.l("%s[%d] = %s ? 1u : 0u;", dst, off, src)
	case idl.KSize:
		sz := o.gl("sz")
		o.local("uint64_t", sz, "")
		c.l("%s = (uint64_t)%s;", sz, src)
		c.l("memcpy(%s, &%s, 8);", at, sz)
	case idl.KHandle:
		c.l("memcpy(%s, &%s, 4);", at, src)
	case idl.KFixedArray:
		stride := idl.Stride(t.Elem)
		if t.Elem.Kind.IsScalar() && t.Elem.Kind != idl.KSize {
			c.l("memcpy(%s, %s, %d);", at, src, t.Len*stride)
			return
		}
		iv := o.gl([]string{"i", "j", "k"}[min(depth, 2)])
		o.local("uint32_t", iv, "")
		o.loop(c, iv, fmt.Sprint(t.Len), func() {
			o.store(c, fmt.Sprintf("(%s + %d + %s * %d)", dst, off, iv, stride), 0, t.Elem, fmt.Sprintf("%s[%s]", src, iv), depth+1)
		})
	case idl.KStruct:
		offs := idl.MemberOffsets(t.St)
		for i, m := range t.St.Members {
			o.store(c, dst, off+offs[i], m.Type, src+"."+m.Name, depth)
		}
	default:
		c.l("memcpy(%s, &%s, %d);", at, src, sizeT(t))
	}
}

// storeRet writes the return value into G2. A handle return is written after the arms that register it.
func (o *opGen) storeRet(handle bool) {
	f := o.f
	if f.Ret == nil || compositeRet(f) && !fixedStructRet(f) {
		return
	}
	for _, fl := range o.plan.Resp.Fixed {
		if fl.Role != idl.RoleRet {
			continue
		}
		isH := f.Ret.Kind == idl.KHandle
		if isH != handle {
			return
		}
		src := "ret"
		if isH {
			src = o.retHandle.id
		}
		o.store(&o.b, o.ov(), int64(fl.Off), fl.Type, src, 0)
	}
}
