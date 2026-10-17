package gen

import (
	"fmt"
	"strings"

	"gosuda.org/hqq/bindgen/idl"
)

// arms writes the success and failure arms of the error convention. Without a convention only the success
// body is written, unconditionally. A nil function means an empty arm.
func (o *opGen) arms(succ, fail func(c *code)) {
	c := &o.b
	if o.conv == nil {
		if succ != nil {
			succ(c)
		}
		return
	}
	var s, f code
	if succ != nil {
		succ(&s)
	}
	if fail != nil {
		fail(&f)
	}
	simple := func(x *code) bool {
		if len(x.lines) != 1 {
			return false
		}
		l := x.lines[0]
		return !strings.HasPrefix(l, "if ") && !strings.HasPrefix(l, "for ") && !strings.HasPrefix(l, "{")
	}
	put := func(x *code) {
		c.in()
		c.add(x.lines...)
		c.out()
	}
	switch {
	case len(s.lines) > 0 && len(f.lines) > 0:
		if simple(&s) && simple(&f) {
			c.l("if (%s)", o.okCond())
			put(&s)
			c.l("else")
			put(&f)
		} else {
			c.l("if (%s) {", o.okCond())
			put(&s)
			c.l("} else {")
			put(&f)
			c.l("}")
		}
	case len(s.lines) > 0:
		if simple(&s) {
			c.l("if (%s)", o.okCond())
			put(&s)
		} else {
			c.l("if (%s) {", o.okCond())
			put(&s)
			c.l("}")
		}
	case len(f.lines) > 0:
		if simple(&f) {
			c.l("if (%s)", o.failCond())
			put(&f)
		} else {
			c.l("if (%s) {", o.failCond())
			put(&f)
			c.l("}")
		}
	}
}

// armsInLock writes the arms of a function whose results are read from library memory: everything
// happens before the lock is released.
func (o *opGen) armsInLock() {
	o.locked = true
	o.arms(func(c *code) {
		o.putHandles(c)
		o.succBody(c)
	}, func(c *code) {
		o.failBody(c, true)
	})
}

// lockArms writes the part of the arms that must run under the lock when the response is written after it:
// registering produced handles on success, destroying them on failure. It reports whether it sets flags.
func (o *opGen) lockArms() bool {
	if len(o.handle) == 0 {
		return false
	}
	o.locked = true
	o.arms(func(c *code) { o.putHandles(c) }, func(c *code) { o.failBody(c, false) })
	return o.conv != nil
}

// armsAfterLock writes the success-only stores of a function whose response is written after the lock.
func (o *opGen) armsAfterLock() {
	o.locked = false
	o.arms(func(c *code) { o.succBody(c) }, nil)
}

// failBody is the failure arm: detail text, borrowed memory, produced handles that must not be registered.
func (o *opGen) failBody(c *code, detail bool) {
	flags := o.gl("flags")
	if detail && o.detail {
		c.l("%s = HQQRPC_F_FAIL | hqqrpc_wr_put_detail(&w, %s);", flags, o.detailCall())
	} else {
		c.l("%s = HQQRPC_F_FAIL;", flags)
	}
	for _, s := range o.frees {
		c.l("%s", s)
	}
	for _, ph := range o.handle {
		if ph.isRet && o.conv != nil && o.conv.Kind == idl.ErrNull {
			continue // NULL by definition
		}
		c.l("if (%s != NULL)", ph.ptr)
		c.in()
		c.l("destroy_%s(%s);", ph.h.Name, ph.ptr)
		c.out()
	}
}

// detailCall is the call of the detail function with the arguments named in the IDL (4.6.4).
func (o *opGen) detailCall() string {
	d := o.conv.Detail
	var args []string
	for _, a := range d.Args {
		if a == "ret" {
			args = append(args, "ret")
			continue
		}
		q := o.pv[o.paramIndex(a)]
		args = append(args, o.valueExpr(q))
	}
	return fmt.Sprintf("%s(%s)", d.Func, strings.Join(args, ", "))
}

// nonNull reports whether a produced handle is known to be non-NULL on success.
func (o *opGen) nonNull(ph *prodHandle) bool {
	return ph.isRet && o.conv != nil && o.conv.Kind == idl.ErrNull
}

// putHandles registers the produced handles (4.6.5). A failed registration destroys the handles that were not
// registered yet; the runtime rolls back the registered ones when the call ends with E_INTERNAL.
func (o *opGen) putHandles(c *code) {
	for i, ph := range o.handle {
		var conds []string
		if !o.nonNull(ph) {
			conds = append(conds, ph.ptr+" != NULL")
		}
		conds = append(conds, fmt.Sprintf("hqqrpc_handle_put(s, %s, %s, %s, &%s) != HQQRPC_OK", o.c.hmacro[ph.h], ph.ptr, ph.parent, ph.id))
		c.block("if ("+strings.Join(conds, " && ")+")", func() {
			for j, rest := range o.handle[i:] {
				if j == 0 || o.nonNull(rest) {
					c.l("destroy_%s(%s);", rest.h.Name, rest.ptr)
					continue
				}
				c.l("if (%s != NULL)", rest.ptr)
				c.in()
				c.l("destroy_%s(%s);", rest.h.Name, rest.ptr)
				c.out()
			}
			o.failRet(c, retInternal, true)
		})
	}
}

// n32 returns the uint32_t local that holds the produced length of q, declaring it once.
func (o *opGen) n32(q *pv) string {
	if q.nout == "" {
		q.nout = o.nm.claim(q.p.Name + "n")
		o.local("uint32_t", q.nout, "")
	}
	return q.nout
}

// succBody writes the success path: the values of G2, the padding to V2 and the variable items, and frees.
func (o *opGen) succBody(c *code) {
	for _, fl := range o.plan.Resp.Fixed {
		if fl.Role != idl.RoleValue {
			continue
		}
		q := o.pv[fl.Param]
		src := q.val
		switch q.cls {
		case cOutHandle:
			src = q.hid
		case cOutOffset:
			o.offsetCompute(c, q)
			src = q.off32
		}
		o.store(c, o.ov(), int64(fl.Off), fl.Type, src, 0)
	}
	if o.v2 && o.inPlace == nil && !varWriterRet(o.f) && o.g2size%8 != 0 {
		o.failIf(c, "hqqrpc_wr_pad(&w, 8) != HQQRPC_OK", "w.err")
	}
	for _, fl := range o.plan.Resp.Var {
		o.putVar(c, fl)
	}
	for _, s := range o.frees {
		c.l("%s", s)
	}
}

// failIf writes `if (cond) { cleanup; return status; }` after the call.
func (o *opGen) failIf(c *code, cond, status string) {
	o.ifFail(c, cond, status, true)
}

// offsetCompute turns the pointer written through an @offset_in parameter into its byte offset (4.6.1).
func (o *opGen) offsetCompute(c *code, q *pv) {
	sq := o.pv[o.paramIndex(q.p.Attrs.OffsetIn)]
	base := o.valueExpr(sq)
	c.block(fmt.Sprintf("if (%s != NULL)", q.val), func() {
		o.failIf(c, fmt.Sprintf("(uintptr_t)%s < (uintptr_t)%s || (uintptr_t)%s - (uintptr_t)%s > %s", q.val, base, q.val, base, sq.n), retInternal)
		c.l("%s = (uint32_t)((uintptr_t)%s - (uintptr_t)%s);", q.off32, q.val, base)
	})
}

// lenSource is the C expression for the produced length of the out buffer q, or "" when the capacity is it.
func (o *opGen) lenSource(q *pv) string {
	if q.lenOut != nil {
		return q.lenOut.val
	}
	if o.f.RetAttrs.LenOf == q.p.Name {
		return "ret"
	}
	return ""
}

// putVar writes one variable item of the response.
func (o *opGen) putVar(c *code, fl idl.Field) {
	if fl.Role == idl.RoleRet {
		if compositeRet(o.f) {
			return
		}
		t := o.f.Ret
		lenSrc := ""
		if o.retLen != nil {
			lenSrc = o.retLen.val
		}
		o.putValue(c, "ret", t, lenSrc, o.f.RetAttrs.Nullable, false, o.gl("retn"), true)
		return
	}
	q := o.pv[fl.Param]
	t := q.p.Type
	switch q.cls {
	case cOutStr:
		o.putValue(c, q.val, t, "", false, true, o.n32(q), false)
	case cOutBorrowed:
		o.putValue(c, q.val, t, q.lenOut.val, false, false, o.n32(q), false)
	case cOutBuf:
		o.putBuf(c, q)
	}
}

// putValue writes a str, bytes or array result that the library produced (ptr, plus a length where the
// type has one) with its IDL bound. emptyIfNil makes a NULL out str the empty string; ret says the value is
// the return, which is declared here.
func (o *opGen) putValue(c *code, ptr string, t *idl.Type, lenSrc string, nullable, emptyIfNil bool, n string, ret bool) {
	nonNull := ret && o.conv != nil && o.conv.Kind == idl.ErrNull // success of @error(null): the pointer is not NULL
	if ret {
		o.local("uint32_t", n, "")
	}
	max := boundLit(t)
	fail := func(cond, status string) { o.failIf(c, cond, status) }
	write := func() {
		switch t.Kind {
		case idl.KStr:
			o.c.cstrLen = true
			switch {
			case emptyIfNil:
				c.l("%s = 0;", n)
				fail(fmt.Sprintf("%s != NULL && !hqq_cstr_len(%s, %s, &%s)", ptr, ptr, max, n), retInternal)
			case nullable || nonNull:
				fail(fmt.Sprintf("!hqq_cstr_len(%s, %s, &%s)", ptr, max, n), retInternal)
			default:
				fail(fmt.Sprintf("%s == NULL || !hqq_cstr_len(%s, %s, &%s)", ptr, ptr, max, n), retInternal)
			}
			fail(fmt.Sprintf("hqqrpc_wr_put_str(&w, %s, %s, %s) != HQQRPC_OK", ptr, n, max), "w.err")
		case idl.KBytes:
			if t.Max > 0 {
				fail(fmt.Sprintf("(uint64_t)%s > %d", lenSrc, t.Max), retInternal)
			}
			c.l("%s = (uint32_t)%s;", n, lenSrc)
			fail(fmt.Sprintf("hqqrpc_wr_put_bytes(&w, %s, %s, %s) != HQQRPC_OK", ptr, n, max), "w.err")
		case idl.KArray:
			if t.Max > 0 {
				fail(fmt.Sprintf("(uint64_t)%s > %d", lenSrc, t.Max), retInternal)
			}
			c.l("%s = (uint32_t)%s;", n, lenSrc)
			o.putArray(c, ptr, n, t, max)
		}
	}
	if !nullable {
		write()
		return
	}
	c.l("if (%s == NULL) {", ptr)
	c.in()
	fail("hqqrpc_wr_put_null(&w) != HQQRPC_OK", "w.err")
	c.out()
	c.l("} else {")
	c.in()
	write()
	c.out()
	c.l("}")
}

// putArray writes count and elements of a T[] with fixed elements: array_begin, the elements, array_end.
func (o *opGen) putArray(c *code, ptr, n string, t *idl.Type, max string) {
	mark := o.gl("mark")
	o.local("size_t", mark, "")
	stride := idl.Stride(t.Elem)
	ealign := alignT(t.Elem)
	o.failIf(c, fmt.Sprintf("hqqrpc_wr_array_begin(&w, &%s) != HQQRPC_OK", mark), "w.err")
	c.block(fmt.Sprintf("if (%s != 0)", n), func() {
		v := o.vv()
		size := fmt.Sprintf("(size_t)%s * %d", n, stride)
		if stride == 1 {
			size = fmt.Sprintf("(size_t)%s", n)
		}
		o.failIf(c, fmt.Sprintf("(%s = hqqrpc_wr_fixed(&w, %d, %s)) == NULL", v, ealign, size), "w.err")
		if t.Elem.Kind.IsScalar() && t.Elem.Kind != idl.KSize {
			c.l("memcpy(%s, %s, %s);", v, ptr, size)
			return
		}
		iv := o.gl("i")
		o.local("uint32_t", iv, "")
		o.loop(c, iv, n, func() {
			o.store(c, fmt.Sprintf("(%s + %s * %d)", v, iv, stride), 0, t.Elem, fmt.Sprintf("%s[%s]", ptr, iv), 1)
		})
	})
	o.failIf(c, fmt.Sprintf("hqqrpc_wr_array_end(&w, %s, %s, %s) != HQQRPC_OK", mark, n, max), "w.err")
}

// putBuf writes a caller-supplied out buffer: in place, or from scratch (4.7.3).
func (o *opGen) putBuf(c *code, q *pv) {
	t := q.p.Type
	n := o.n32(q)
	if src := o.lenSource(q); src != "" {
		o.failIf(c, fmt.Sprintf("(uint64_t)%s > %s", src, q.capL), retInternal)
		c.l("%s = (uint32_t)%s;", n, src)
	} else {
		c.l("%s = %s;", n, q.capL)
	}
	max := boundLit(t)
	if q != o.inPlace {
		if t.Kind == idl.KBytes {
			o.failIf(c, fmt.Sprintf("hqqrpc_wr_put_bytes(&w, %s, %s, %s) != HQQRPC_OK", q.buf, n, max), "w.err")
		} else {
			o.putArray(c, fmt.Sprintf("(const %s *)%s", elemC(t.Elem), q.buf), n, t, max)
		}
		return
	}
	// in place: the library wrote the data into the span; write the padding and the count, then advance
	v := o.vv()
	pad, rel, stride, ealign := o.spanGeometry(q)
	if pad > 0 {
		c.l("memset(%s, 0, %d);", v, pad)
	}
	if pad == 0 {
		c.l("memcpy(%s, &%s, 4);", v, n)
	} else {
		c.l("memcpy(%s + %d, &%s, 4);", v, pad, n)
	}
	if gap := rel - pad - 4; gap > 0 { // the padding between the count and elements that are aligned to 8
		c.l("memset(%s + %d, 0, %d);", v, pad+4, gap)
	}
	var adv string
	switch {
	case stride == 1:
		adv = fmt.Sprintf("(size_t)%d + %s", pad+4, n)
	case ealign <= 4:
		adv = fmt.Sprintf("(size_t)%d + (size_t)%s * %d", pad+4, n, stride)
	default:
		adv = fmt.Sprintf("(size_t)%d + (%s != 0 ? (size_t)%d + (size_t)%s * %d : 0)", pad+4, n, rel-pad-4, n, stride)
	}
	o.failIf(c, fmt.Sprintf("hqqrpc_wr_advance(&w, %s) != HQQRPC_OK", adv), "w.err")
}
