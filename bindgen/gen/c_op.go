package gen

import (
	"fmt"
	"regexp"

	"gosuda.org/hqq/bindgen/idl"
)

// pclass classifies a parameter by how the glue handles it.
type pclass int

const (
	cInScalar    pclass = iota // non-derived in scalar or bool
	cInHandle                  // in handle
	cInStr                     // in str
	cInBytes                   // in bytes
	cInArray                   // in T[] with fixed elements
	cInFixedArr                // in T[N]
	cInOut                     // inout scalar
	cOutScalar                 // out scalar or bool
	cOutHandle                 // out handle
	cOutStr                    // out str (borrowed)
	cOutBorrowed               // out bytes or T[], @borrowed
	cOutBuf                    // out bytes or T[], caller-supplied storage
	cOutFixedArr               // out T[N]
	cOutStruct                 // out fixed @c struct
	cOutOffset                 // out u32 with @offset_in
	cLenIn                     // derived in scalar (@len_of)
	cLenOut                    // derived out scalar (@len_of)
)

func classify(p *idl.Param) pclass {
	t := p.Type
	if p.Derived {
		if p.Dir == idl.DirIn {
			return cLenIn
		}
		return cLenOut
	}
	if p.Attrs.OffsetIn != "" {
		return cOutOffset
	}
	switch p.Dir {
	case idl.DirIn:
		switch t.Kind {
		case idl.KHandle:
			return cInHandle
		case idl.KStr:
			return cInStr
		case idl.KBytes:
			return cInBytes
		case idl.KArray:
			return cInArray
		case idl.KFixedArray:
			return cInFixedArr
		}
		return cInScalar
	case idl.DirInOut:
		return cInOut
	}
	switch t.Kind {
	case idl.KHandle:
		return cOutHandle
	case idl.KStr:
		return cOutStr
	case idl.KBytes, idl.KArray:
		if p.Attrs.Borrowed {
			return cOutBorrowed
		}
		return cOutBuf
	case idl.KFixedArray:
		return cOutFixedArr
	case idl.KStruct:
		return cOutStruct
	}
	return cOutScalar
}

// pv is the glue state of one parameter.
type pv struct {
	p      *idl.Param
	i      int
	cls    pclass
	val    string // value local: scalar, pointer, array or struct
	n      string // in buffer: length local (uint32_t)
	capL   string // caller-supplied out buffer: capacity local (uint32_t)
	hid    string // handle id local
	hptr   string // in handle: the looked-up pointer
	cpy    string // private copy of an in buffer
	nout   string // out buffer: produced length (uint32_t)
	off32  string // @offset_in: the computed offset
	buf    string // caller-supplied buffer not in place: scratch pointer
	lenIn  *pv    // the derived in scalar bound to this buffer
	lenOut *pv    // the derived out scalar bound to this buffer
	copy   bool   // the in buffer is copied into scratch (7.2)
	g1off  int    // in T[N]: offset in G1
}

// prodHandle is a handle that the call produces (the return or an out parameter).
type prodHandle struct {
	h      *idl.Handle
	ptr    string // typed pointer local (ret or the out parameter)
	id     string // uint32_t id local
	parent string // id local of the parent handle parameter, or "0"
	isRet  bool
}

// opGen generates the glue function of one wire function.
type opGen struct {
	c     *cgen
	f     *idl.Func
	plan  *idl.Plan
	nm    *namer
	glue  map[string]string
	decls []decl
	b     code

	pv        []*pv
	intercept bool
	lockOn    bool
	locked    bool

	conv    *idl.ErrorConv
	detail  bool
	retLen  *pv
	inPlace *pv
	// G2 geometry
	g2size, g2pad, g2align int
	v2                     bool
	handle                 []*prodHandle
	frees                  []string // statements that free borrowed memory after the call
	// handle parameters
	nInHandles int
	destroyed  *pv // the handle parameter of a destroy function

	writeInLock bool
	preclaimed  bool
	pre         []string // step 4 conditions (handle lookups, reserve)
	hookArgs    []hookMember

	usesFlags  bool
	retHandle  *prodHandle
	retLocal   bool // a local "ret" holds the C return value
	needFailed bool
}

type hookMember struct {
	ctype, name string
	pv          *pv
}

var usesSession = regexp.MustCompile(`(^|[^.\w])s\b`)

func newOp(c *cgen, f *idl.Func) *opGen {
	return &opGen{
		c: c, f: f, plan: f.Plan, nm: newNamer(c.reserved), glue: map[string]string{},
		intercept: f.Intercept, lockOn: c.s.Serialize != idl.SerializeNone, conv: f.Error,
		b: code{depth: 1},
	}
}

// gl claims a glue local lazily, so that an IDL-derived local wins a name and the glue adapts.
func (o *opGen) gl(base string) string {
	if n, ok := o.glue[base]; ok {
		return n
	}
	n := o.nm.claim(base)
	o.glue[base] = n
	return n
}

func (o *opGen) local(typ, name, init string) {
	for _, d := range o.decls {
		if d.prio == 1 && d.name == name {
			return
		}
	}
	o.decls = append(o.decls, decl{prio: 1, typ: typ, name: name, init: init})
}

func (o *opGen) core(rank int, typ, name string) {
	for _, d := range o.decls {
		if d.prio == 0 && d.name == name {
			return
		}
	}
	o.decls = append(o.decls, decl{prio: 0, rank: rank, typ: typ, name: name})
}

const (
	rkS = iota
	rkR
	rkG
	rkO
	rkV
	rkSt
	rkW
)

func (o *opGen) paramIndex(name string) int {
	for i, p := range o.f.Params {
		if p.Name == name {
			return i
		}
	}
	return -1
}

// analyze classifies the parameters, binds the length scalars and claims every local name.
func (o *opGen) analyze() {
	f := o.f
	p := o.plan
	o.detail = p.Detail
	o.g2size = p.Resp.FixedSize
	o.g2pad = p.G2Pad()
	o.v2 = len(p.Resp.Var) > 0
	o.g2align = 1
	for _, fl := range p.Resp.Fixed {
		o.g2align = max(o.g2align, int(alignT(fl.Type)))
	}

	o.pv = make([]*pv, len(f.Params))
	for i, par := range f.Params {
		o.pv[i] = &pv{p: par, i: i, cls: classify(par)}
	}
	for _, q := range o.pv {
		switch lo := q.p.Attrs.LenOf; {
		case lo == "":
		case lo == "ret":
			o.retLen = q
		default:
			buf := o.pv[o.paramIndex(lo)]
			if q.cls == cLenIn {
				buf.lenIn = q
			} else {
				buf.lenOut = q
			}
		}
	}
	for _, q := range o.pv {
		if q.cls == cInHandle {
			o.nInHandles++
		}
	}

	// IDL-derived locals first, so that their names are the IDL names where possible.
	for _, q := range o.pv {
		name := q.p.Name
		switch q.cls {
		case cInScalar, cInOut, cOutScalar, cOutStruct, cOutFixedArr, cOutStr, cOutBorrowed, cOutHandle, cOutOffset, cLenOut:
			q.val = o.nm.claim(name)
		case cInStr, cInBytes, cInArray:
			q.val = o.nm.claim(name)
		}
	}
	for _, q := range o.pv {
		name := q.p.Name
		switch q.cls {
		case cInStr, cInBytes, cInArray:
			q.n = o.nm.claim(q.val + "n")
		case cInHandle:
			q.hid = o.nm.claim("h_" + name)
		case cOutHandle:
			q.hid = o.nm.claim("h_" + name)
		case cOutBuf:
			q.capL = o.nm.claim(q.lenIn.p.Name)
		case cOutOffset:
			q.off32 = o.nm.claim(name + "_off")
		}
	}

	// copies (4.7.1 step 5)
	for _, q := range o.pv {
		switch q.cls {
		case cInStr:
			q.copy = o.intercept || q.lenIn == nil
		case cInBytes, cInArray, cInFixedArr:
			q.copy = o.intercept
		}
		if q.copy {
			q.cpy = o.nm.claim(q.valOrName() + "c")
		}
	}

	// handle parameters and the destroyed handle
	for _, q := range o.pv {
		if q.cls == cInHandle {
			if o.nInHandles == 1 {
				q.hptr = o.gl("p")
			} else {
				q.hptr = o.nm.claim("p_" + q.p.Name)
			}
		}
	}
	if f.Destroys != nil {
		for _, q := range o.pv {
			if q.cls == cInHandle {
				o.destroyed = q
			}
		}
	}

	// produced handles: the return first, then out parameters in order
	if f.Ret != nil && f.Ret.Kind == idl.KHandle {
		ph := &prodHandle{h: f.Ret.H, isRet: true, id: o.gl("id"), ptr: "ret"}
		o.handle = append(o.handle, ph)
		o.retHandle = ph
	}
	for _, q := range o.pv {
		if q.cls == cOutHandle {
			o.handle = append(o.handle, &prodHandle{h: q.p.Type.H, ptr: q.val, id: q.hid})
		}
	}
	for _, ph := range o.handle {
		ph.parent = "0"
		if ph.h.Parent != nil {
			for _, q := range o.pv {
				if q.cls == cInHandle && q.p.Type.H == ph.h.Parent {
					ph.parent = q.hid
					break
				}
			}
		}
	}

	// the first variable item of the response may be written in place by the library
	if len(p.Resp.Var) > 0 {
		fl := p.Resp.Var[0]
		if fl.Param >= 0 && o.pv[fl.Param].cls == cOutBuf {
			o.inPlace = o.pv[fl.Param]
		}
	}
	for _, q := range o.pv {
		if q.cls == cOutBuf && q != o.inPlace {
			q.buf = o.nm.claim(q.p.Name + "buf")
		}
	}

	for _, q := range o.pv {
		if (q.cls == cOutStr || q.cls == cOutBorrowed) && q.p.Attrs.Free != "" {
			o.frees = append(o.frees, "if ("+q.val+" != NULL)", "    "+q.p.Attrs.Free+"((void *)"+q.val+");")
		}
	}
	if f.Ret != nil && f.RetAttrs.Free != "" {
		o.frees = append(o.frees, "if (ret != NULL)", "    "+f.RetAttrs.Free+"((void *)ret);")
	}

	o.writeInLock = o.detail || o.hasLibMemoryResults()
	o.hookArgs = o.hookMembers()
}

func (q *pv) valOrName() string {
	if q.val != "" {
		return q.val
	}
	return q.p.Name
}

// hasLibMemoryResults reports whether results are read from memory the library owns after the call.
func (o *opGen) hasLibMemoryResults() bool {
	for _, q := range o.pv {
		switch q.cls {
		case cOutStr, cOutBorrowed:
			return true
		}
	}
	if t := o.f.Ret; t != nil && isVarBuf(t) && !compositeRet(o.f) {
		return true
	}
	return false
}

// hookMembers lists the members of the interceptor args struct (7.3): one per in or inout C parameter, in C order.
func (o *opGen) hookMembers() []hookMember {
	var ms []hookMember
	for _, q := range o.pv {
		par := q.p
		switch q.cls {
		case cInScalar, cInOut, cLenIn:
			ms = append(ms, hookMember{ctype: valueC(par.Type, par.Attrs), name: par.Name, pv: q})
		case cInHandle:
			ms = append(ms, hookMember{ctype: valueC(par.Type, par.Attrs), name: par.Name, pv: q})
		case cInStr, cInBytes, cInArray, cInFixedArr:
			ms = append(ms, hookMember{ctype: inPtrC(par.Type), name: par.Name, pv: q})
		}
	}
	return ms
}

// argStructName is the name of the interceptor argument type.
func (c *cgen) argStructName(f *idl.Func) string { return c.svc + "_args_" + f.Name }

// hookName is the name of the interceptor of f.
func (c *cgen) hookName(f *idl.Func) string { return c.svc + "_intercept_" + f.Name }

// ---- expressions ---------------------------------------------------------------------------------

func (o *opGen) aMember(q *pv) string { return o.gl("a") + "." + q.p.Name }

// valueExpr is the C value that the library call receives for an in parameter (or a derived in scalar).
func (o *opGen) valueExpr(q *pv) string {
	if o.intercept {
		switch q.cls {
		case cInScalar, cInHandle, cInStr, cInBytes, cInArray, cInFixedArr, cLenIn:
			return o.aMember(q)
		}
	}
	par := q.p
	switch q.cls {
	case cInScalar:
		return q.val
	case cInOut, cOutScalar, cOutStruct, cOutFixedArr, cOutStr, cOutBorrowed, cOutHandle, cOutOffset, cLenOut:
		return q.val
	case cInHandle:
		return "(" + valueC(par.Type, par.Attrs) + ")" + q.hptr
	case cInStr, cInBytes, cInArray:
		if q.copy {
			return q.cpy
		}
		return q.val
	case cInFixedArr:
		if q.copy {
			return q.cpy
		}
		return fmt.Sprintf("(const void *)(%s + %d)", o.gv(), q.g1off)
	case cLenIn:
		ct := valueC(par.Type, par.Attrs)
		if buf := o.boundBuffer(q); buf != nil {
			if buf.cls == cOutBuf {
				return "(" + ct + ")" + buf.capL
			}
			return "(" + ct + ")" + buf.n
		}
	}
	return "0"
}

// boundBuffer is the buffer that the derived scalar q is bound to.
func (o *opGen) boundBuffer(q *pv) *pv {
	i := o.paramIndex(q.p.Attrs.LenOf)
	if i < 0 {
		return nil
	}
	return o.pv[i]
}

// failRet writes the lines of an early return after the library call: free borrowed memory, release the lock.
func (o *opGen) failRet(c *code, status string, withFrees bool) {
	if withFrees {
		for _, s := range o.frees {
			c.l("%s", s)
		}
	}
	if o.lockOn && o.locked {
		c.l("hqqrpc_exec_end(s);")
	}
	c.l("return %s;", status)
}

// failBlock writes `if (cond) { cleanup; return status; }` for a condition that depends on library results.
func (o *opGen) failBlock(c *code, cond, status string) {
	c.block("if ("+cond+")", func() { o.failRet(c, status, true) })
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

func (o *opGen) gv() string {
	n := o.gl("g")
	o.core(rkG, "const uint8_t *", n)
	return n
}

func (o *opGen) ov() string {
	n := o.gl("o")
	o.core(rkO, "uint8_t *", n)
	return n
}

func (o *opGen) vv() string {
	n := o.gl("v")
	o.core(rkV, "uint8_t *", n)
	return n
}
