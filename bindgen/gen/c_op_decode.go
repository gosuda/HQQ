package gen

import (
	"fmt"

	"gosuda.org/hqq/bindgen/idl"
)

// fixedOutLimit is the largest out T[N] (bytes) that the glue keeps on the stack; larger ones use scratch.
const fixedOutLimit = 1024

const (
	retBad      = "HQQRPC_E_BAD_REQUEST"
	retLimit    = "HQQRPC_E_LIMIT"
	retInternal = "HQQRPC_E_INTERNAL"
)

// declareLocals declares the locals of the parameters and the return, in parameter order.
func (o *opGen) declareLocals() {
	for _, q := range o.pv {
		par := q.p
		t := par.Type
		switch q.cls {
		case cInScalar, cInOut:
			o.local(valueC(t, par.Attrs), q.val, "")
		case cInHandle:
			o.local("uint32_t", q.hid, "")
			o.local("void *", q.hptr, "")
		case cInStr:
			o.local("const char *", q.val, "")
			o.local("uint32_t", q.n, "")
		case cInBytes, cInArray:
			o.local("const void *", q.val, "")
			o.local("uint32_t", q.n, "")
		case cOutScalar:
			o.local(valueC(t, par.Attrs), q.val, "0")
		case cLenOut:
			o.local(valueC(t, par.Attrs), q.val, "0")
		case cOutHandle:
			o.local(handleCType(t.H), q.val, "NULL")
			o.local("uint32_t", q.hid, "0")
		case cOutStr:
			o.local(borrowedC(q.p, "char"), q.val, "NULL")
		case cOutBorrowed:
			if t.Kind == idl.KBytes {
				o.local(borrowedC(q.p, "void"), q.val, "NULL")
			} else {
				o.local(borrowedC(q.p, elemC(t.Elem)), q.val, "NULL")
			}
		case cOutBuf:
			o.local("uint32_t", q.capL, "")
		case cOutOffset:
			o.local("const char *", q.val, "NULL")
			o.local("uint32_t", q.off32, "0")
		case cOutStruct:
			o.local(valueC(t, par.Attrs), q.val, "")
		case cOutFixedArr:
			if size := sizeT(t); size <= fixedOutLimit {
				o.local(elemC(t.Elem), fmt.Sprintf("%s[%d]", q.val, t.Len), "")
			} else {
				o.local(elemC(t.Elem)+" *", q.val, "")
			}
		}
		if q.copy {
			switch q.cls {
			case cInStr:
				o.local("char *", q.cpy, "")
			default:
				o.local("void *", q.cpy, "")
			}
		}
		if q.cls == cOutBuf && q != o.inPlace {
			o.local("void *", q.buf, "")
		}
	}
	if o.f.Ret != nil {
		switch {
		case varWriterRet(o.f):
		default:
			o.local(retC(o.f), "ret", "")
			o.retLocal = true
		}
	}
	if o.retHandle != nil {
		o.local("uint32_t", o.retHandle.id, "0")
	}
}

// decode writes step 3 of 4.7.1: read and validate every field of the body.
func (o *opGen) decode() {
	c := &o.b
	req := &o.plan.Req
	c.l("hqqrpc_rd_init(&r, c);")

	var vexprs []string
	for _, fl := range req.Var {
		q := o.pv[fl.Param]
		t := q.p.Type
		null := b2i(q.p.Attrs.Nullable)
		switch t.Kind {
		case idl.KStr:
			vexprs = append(vexprs, fmt.Sprintf("(st = hqqrpc_rd_str(&r, %d, %s, &%s, &%s)) != HQQRPC_OK", null, boundLit(t), q.val, q.n))
		case idl.KBytes:
			vexprs = append(vexprs, fmt.Sprintf("(st = hqqrpc_rd_bytes(&r, %d, %s, &%s, &%s)) != HQQRPC_OK", null, boundLit(t), q.val, q.n))
		case idl.KArray:
			vexprs = append(vexprs, fmt.Sprintf("(st = hqqrpc_rd_array(&r, %d, %d, %d, %s, &%s, &%s)) != HQQRPC_OK",
				idl.Stride(t.Elem), alignT(t.Elem), null, boundLit(t), q.val, q.n))
		}
	}
	end := "(st = hqqrpc_rd_end(&r)) != HQQRPC_OK"

	if len(req.Fixed) > 0 {
		align := 1
		for _, fl := range req.Fixed {
			align = max(align, int(alignT(fl.Type)))
		}
		g := o.gv()
		take := fmt.Sprintf("(st = hqqrpc_rd_take(&r, %d, %d, &%s)) != HQQRPC_OK", align, req.VarStart(), g)
		if len(vexprs) > 0 {
			c.chain([]string{take}, "st")
			o.extract()
			c.chain(append(vexprs, end), "st")
		} else {
			c.chain([]string{take, end}, "st")
			o.extract()
		}
	} else {
		c.chain(append(vexprs, end), "st")
	}
	o.checkDecoded()
}

// extract copies the fixed group of the request into locals, once (single fetch, 7.2).
func (o *opGen) extract() {
	c := &o.b
	g := o.gv()
	for _, fl := range o.plan.Req.Fixed {
		q := o.pv[fl.Param]
		if fl.Role == idl.RoleCap {
			c.l("memcpy(&%s, %s + %d, 4);", q.capL, g, fl.Off)
			continue
		}
		t := q.p.Type
		switch {
		case q.cls == cInFixedArr:
			q.g1off = fl.Off
		case t.Kind == idl.KBool:
			bv := o.gl("bv")
			o.local("uint8_t", bv, "")
			c.l("%s = %s[%d];", bv, g, fl.Off)
			c.l("if (%s > 1u)", bv)
			c.in()
			c.l("return %s;", retBad)
			c.out()
			c.l("%s = %s != 0;", q.val, bv)
		case t.Kind == idl.KSize:
			sz := o.gl("sz")
			o.local("uint64_t", sz, "")
			c.l("memcpy(&%s, %s + %d, 8);", sz, g, fl.Off)
			c.l("if ((uint64_t)(size_t)%s != %s)", sz, sz)
			c.in()
			c.l("return %s;", retBad)
			c.out()
			c.l("%s = (size_t)%s;", q.val, sz)
		case t.Kind == idl.KHandle:
			c.l("memcpy(&%s, %s + %d, 4);", q.hid, g, fl.Off)
		case t.Kind == idl.KU8:
			c.l("%s = %s[%d];", q.val, g, fl.Off)
		default:
			c.l("memcpy(&%s, %s + %d, %d);", q.val, g, fl.Off, sizeT(t))
		}
	}
	// A bool inside an in T[N] must be 0 or 1 (4.5).
	for _, fl := range o.plan.Req.Fixed {
		q := o.pv[fl.Param]
		if fl.Role == idl.RoleValue && q.cls == cInFixedArr && hasBool(q.p.Type) {
			o.boolCheck(fmt.Sprintf("%s + %d", g, fl.Off), q.p.Type, 0)
		}
	}
}

// boolCheck rejects a bool leaf other than 0 or 1 in the value of type t at base (a const uint8_t * expression).
func (o *opGen) boolCheck(base string, t *idl.Type, depth int) {
	c := &o.b
	switch t.Kind {
	case idl.KBool:
		c.l("if (*(%s) > 1u)", base)
		c.in()
		c.l("return %s;", retBad)
		c.out()
	case idl.KFixedArray, idl.KArray:
		if !hasBool(t.Elem) {
			return
		}
		iv := o.gl([]string{"i", "j", "k"}[min(depth, 2)])
		o.local("uint32_t", iv, "")
		count := fmt.Sprint(t.Len)
		o.loop(c, iv, count, func() {
			o.boolCheck(fmt.Sprintf("%s + %s * %d", base, iv, idl.Stride(t.Elem)), t.Elem, depth+1)
		})
	case idl.KStruct:
		offs := idl.MemberOffsets(t.St)
		for i, m := range t.St.Members {
			if hasBool(m.Type) {
				o.boolCheck(fmt.Sprintf("%s + %d", base, offs[i]), m.Type, depth)
			}
		}
	}
}

// loop writes `for (iv = 0; iv < count; iv++) { body }`.
func (o *opGen) loop(c *code, iv, count string, body func()) {
	c.block(fmt.Sprintf("for (%s = 0; %s < %s; %s++)", iv, iv, count, iv), body)
}

// checkDecoded writes the value checks that follow the decoding: bool elements of variable arrays, the
// range of derived lengths and the bounds of caller-chosen capacities (4.5, 4.6.2).
func (o *opGen) checkDecoded() {
	c := &o.b
	for _, q := range o.pv {
		t := q.p.Type
		if q.cls == cInArray && hasBool(t.Elem) {
			iv := o.gl("i")
			o.local("uint32_t", iv, "")
			o.loop(c, iv, q.n, func() {
				o.boolCheck(fmt.Sprintf("(const uint8_t *)%s + %s * %d", q.val, iv, idl.Stride(t.Elem)), t.Elem, 1)
			})
		}
		if q.lenIn == nil {
			continue
		}
		lk := q.lenIn.p.Type.Kind
		if q.cls == cOutBuf {
			if t.Max > 0 {
				c.l("if (%s > %d)", q.capL, t.Max)
				c.in()
				c.l("return %s;", retLimit)
				c.out()
			}
			if m := intMax(lk); m < 1<<32-1 {
				c.l("if (%s > %d)", q.capL, m)
				c.in()
				c.l("return %s;", retLimit)
				c.out()
			}
			continue
		}
		limit := uint64(1 << 30)
		if t.Max > 0 && uint64(t.Max) < limit {
			limit = uint64(t.Max)
		}
		if m := intMax(lk); m < limit {
			c.l("if (%s > %d)", q.n, m)
			c.in()
			c.l("return %s;", retBad)
			c.out()
		}
	}
}

// lookups collects the conditions of step 4: handle lookups and the reservation for new handles.
func (o *opGen) lookups() {
	for _, q := range o.pv {
		if q.cls != cInHandle {
			continue
		}
		o.pre = append(o.pre, fmt.Sprintf("(st = hqqrpc_handle_get(s, %s, %s, %d, &%s)) != HQQRPC_OK",
			q.hid, o.c.hmacro[q.p.Type.H], b2i(q.p.Attrs.Nullable), q.hptr))
	}
	if n := len(o.handle); n > 0 {
		o.pre = append(o.pre, fmt.Sprintf("(st = hqqrpc_handle_reserve(s, %d)) != HQQRPC_OK", n))
	}
}

// copies writes step 5: private copies of the arguments the library or a hook must see stable (7.2).
func (o *opGen) copies() bool {
	c := &o.b
	any := false
	for _, q := range o.pv {
		if !q.copy {
			continue
		}
		any = true
		t := q.p.Type
		nullable := q.p.Attrs.Nullable
		switch q.cls {
		case cInStr:
			alloc := func() {
				c.l("if ((%s = (char *)hqqrpc_scratch(s, (size_t)%s + 1)) == NULL)", q.cpy, q.n)
				c.in()
				c.l("return %s;", retInternal)
				c.out()
				c.l("memcpy(%s, %s, %s);", q.cpy, q.val, q.n)
				c.l("%s[%s] = '\\0';", q.cpy, q.n)
			}
			if nullable {
				c.l("%s = NULL;", q.cpy)
				c.block(fmt.Sprintf("if (%s != NULL)", q.val), alloc)
			} else {
				alloc()
			}
		case cInBytes, cInArray:
			stride := int64(1)
			if t.Kind == idl.KArray {
				stride = idl.Stride(t.Elem)
			}
			size := fmt.Sprintf("(size_t)%s * %d", q.n, stride)
			if stride == 1 {
				size = fmt.Sprintf("(size_t)%s", q.n)
			}
			alloc := func() {
				c.l("if ((%s = hqqrpc_scratch(s, %s)) == NULL)", q.cpy, size)
				c.in()
				c.l("return %s;", retInternal)
				c.out()
				c.l("if (%s != 0)", q.n)
				c.in()
				c.l("memcpy(%s, %s, %s);", q.cpy, q.val, size)
				c.out()
			}
			if nullable {
				c.l("%s = NULL;", q.cpy)
				c.block(fmt.Sprintf("if (%s != NULL)", q.val), alloc)
			} else {
				alloc()
			}
		case cInFixedArr:
			size := sizeT(t)
			c.l("if ((%s = hqqrpc_scratch(s, %d)) == NULL)", q.cpy, size)
			c.in()
			c.l("return %s;", retInternal)
			c.out()
			c.l("memcpy(%s, %s + %d, %d);", q.cpy, o.gv(), q.g1off, size)
		}
	}
	return any
}

// boundExpr is RESP_BOUND (4.6.3) as a C expression in uint64_t, and any statements that compute it.
func (o *opGen) boundExpr() (expr string, stmts []string) {
	terms := o.plan.BoundTerms(o.f)
	base := int64(o.g2pad)
	var syms []string
	for _, bt := range terms {
		if bt.PerCap == 0 {
			base += bt.Const
			continue
		}
		q := o.pv[bt.Param]
		syms = append(syms, "HQQRPC_VAR_SLACK")
		if bt.PerCap == 1 {
			syms = append(syms, q.capL)
		} else {
			syms = append(syms, fmt.Sprintf("(uint64_t)%s * %d", q.capL, bt.PerCap))
		}
	}
	detailMin := fmt.Sprintf("(uint64_t)%d + HQQRPC_DETAIL_RESERVE", o.g2pad)
	if o.g2pad == 0 {
		detailMin = "HQQRPC_DETAIL_RESERVE"
	}
	if len(syms) == 0 {
		if o.detail && int64(o.g2pad)+idl.DetailReserve >= base {
			return detailMin, nil
		}
		return fmt.Sprint(base), nil
	}
	first := fmt.Sprintf("(uint64_t)%d", base)
	rest := syms
	if base == 0 {
		first = "(uint64_t)" + syms[0]
		rest = syms[1:]
	}
	expr = first
	for _, s := range rest {
		expr += " + " + s
	}
	if o.detail {
		bound := o.gl("bound")
		o.local("uint64_t", bound, "")
		stmts = []string{
			fmt.Sprintf("%s = %s;", bound, expr),
			fmt.Sprintf("if (%s < %s)", bound, detailMin),
			fmt.Sprintf("    %s = %s;", bound, detailMin),
		}
		return bound, stmts
	}
	return expr, nil
}

// respBegin writes steps 4 to 6: lookups, private copies, the response reservation, and the zeroed
// out locals and scratch buffers that the call needs.
func (o *opGen) respBegin() {
	c := &o.b
	expr, stmts := o.boundExpr()
	begin := fmt.Sprintf("(st = hqqrpc_resp_begin(c, %s, &w)) != HQQRPC_OK", expr)
	hasCopies := false
	for _, q := range o.pv {
		if q.copy {
			hasCopies = true
		}
	}
	for _, s := range stmts {
		c.l("%s", s)
	}
	if hasCopies {
		c.chain(o.pre, "st")
		o.copies()
		c.chain([]string{begin}, "st")
	} else {
		c.chain(append(append([]string{}, o.pre...), begin), "st")
	}
	o.initOuts()
}

// initOuts allocates scratch for out buffers that are not written in place and zeroes out locals (4.6.1).
func (o *opGen) initOuts() {
	c := &o.b
	for _, q := range o.pv {
		t := q.p.Type
		switch q.cls {
		case cOutBuf:
			if q == o.inPlace {
				continue
			}
			stride := int64(1)
			if t.Kind == idl.KArray {
				stride = idl.Stride(t.Elem)
			}
			size := fmt.Sprintf("(size_t)%s", q.capL)
			if stride != 1 {
				size = fmt.Sprintf("(size_t)%s * %d", q.capL, stride)
			}
			c.l("if ((%s = hqqrpc_scratch(s, %s)) == NULL)", q.buf, size)
			c.in()
			c.l("return %s;", retInternal)
			c.out()
		case cOutStruct:
			c.l("memset(&%s, 0, sizeof %s);", q.val, q.val)
		case cOutFixedArr:
			if size := sizeT(t); size <= fixedOutLimit {
				c.l("memset(%s, 0, sizeof %s);", q.val, q.val)
			} else {
				c.l("if ((%s = (%s *)hqqrpc_scratch(s, %d)) == NULL)", q.val, elemC(t.Elem), size)
				c.in()
				c.l("return %s;", retInternal)
				c.out()
				c.l("memset(%s, 0, %d);", q.val, size)
			}
		}
	}
	if o.f.Ret != nil && fixedStructRet(o.f) || o.f.Ret != nil && o.f.Ret.Kind == idl.KStruct && !o.f.Shim {
		// the struct return of a shim is an out pointer; zero it
		if fixedStructRet(o.f) {
			c.l("memset(&ret, 0, sizeof ret);")
		}
	}
}
