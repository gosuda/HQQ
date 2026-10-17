package idl

import (
	"math/big"
	"sort"
)

// fnCheck holds the validation state of one function.
type fnCheck struct {
	c      *checker
	f      *Func
	rf     *rawFunc
	decl   annSet
	ret    annSet
	pset   []annSet
	server bool // @allow(server)
	bad    bool // a diagnostic was reported about this function: no plan is built

	maxOut    *big.Int
	maxOutPos Pos
}

// phase runs one stage of the validation of the function and marks it bad if the stage reported anything.
func (fc *fnCheck) phase(run func()) {
	before := len(fc.c.sink.list)
	run()
	if len(fc.c.sink.list) != before {
		fc.bad = true
	}
}

func (fc *fnCheck) err(pos Pos, rule, format string, args ...any) {
	fc.c.sink.add(pos, rule, format, args...)
}

func (c *checker) declareFuncs() {
	for i := range c.raw.funcs {
		rf := &c.raw.funcs[i]
		f := &Func{Name: rf.name.text, Shim: rf.shim, Pos: rf.name.pos}
		fc := &fnCheck{c: c, f: f, rf: rf}
		c.fcs[f] = fc
		c.spec.Funcs = append(c.spec.Funcs, f)
		if _, dup := c.funcs[f.Name]; dup {
			c.sink.add(rf.name.pos, "V1", "function %q is declared twice", f.Name)
		} else {
			c.funcs[f.Name] = f
		}
		fc.phase(fc.build)
	}
}

func (fc *fnCheck) build() {
	c, f, rf := fc.c, fc.f, fc.rf
	c.checkName(rf.name.pos, rf.name.text, "function name")

	fc.decl = c.takeAnns(rf.anns, "a function", "error", "intercept", "allow", "retain_on_fail")
	if a := fc.decl["allow"]; a != nil {
		if v, ok := c.oneIdent(a, false); ok {
			switch v {
			case "wire":
			case "server":
				fc.server = true
			default:
				fc.err(a.args[0].pos, RuleSyntax, "@allow takes wire or server, not %q", v)
			}
		}
	}
	if a := fc.decl["intercept"]; a != nil {
		c.noArgs(a)
		f.Intercept = true
		if fc.server {
			fc.err(a.pos, "V29", "@intercept is only valid on @allow(wire) functions")
			fc.err(rf.name.pos, "V31", "an @allow(server) function is not @intercept")
		}
	}
	if a := fc.decl["retain_on_fail"]; a != nil {
		c.noArgs(a)
	}

	for i := range rf.params {
		fc.buildParam(&rf.params[i])
	}
	if rf.ret != nil {
		t := c.resolveType(rf.ret, true)
		if t == nil {
			t = &Type{Kind: KU8}
		}
		f.Ret = t
		fc.ret = c.takeAnns(rf.retAnns, "a return type", "len_of", "nullable", "free", "max_out", "c")
		f.RetAttrs = fc.attrs(fc.ret)
		fc.parseMaxOut()
	}

	if len(f.Params) > 64 {
		fc.err(rf.name.pos, "V9", "function %s has %d parameters, the limit is 64", f.Name, len(f.Params))
	}
	fc.checkParams()
	fc.checkReturn()
	fc.checkLenOf()
	fc.checkOwnership()
}

func (fc *fnCheck) buildParam(rp *rawParam) {
	c, f := fc.c, fc.f
	c.checkName(rp.name.pos, rp.name.text, "parameter name")
	t := c.resolveType(rp.typ, true)
	if t == nil {
		t = &Type{Kind: KU8}
	}
	p := &Param{Name: rp.name.text, Dir: rp.dir, Type: t, Pos: rp.name.pos}
	set := c.takeAnns(rp.anns, "a parameter", "len_of", "offset_in", "nullable", "borrowed", "free", "c")
	fc.pset = append(fc.pset, set)
	p.Attrs = fc.attrs(set)
	p.Derived = p.Attrs.LenOf != ""
	f.Params = append(f.Params, p)
}

// attrs reads the annotations of a parameter or a return.
func (fc *fnCheck) attrs(set annSet) Attrs {
	c := fc.c
	var at Attrs
	if a := set["len_of"]; a != nil {
		if v, ok := c.oneIdent(a, true); ok {
			at.LenOf = v
		}
	}
	if a := set["offset_in"]; a != nil {
		if v, ok := c.oneIdent(a, false); ok {
			at.OffsetIn = v
		}
	}
	if a := set["nullable"]; a != nil {
		c.noArgs(a)
		at.Nullable = true
	}
	if a := set["borrowed"]; a != nil {
		c.noArgs(a)
		at.Borrowed = true
	}
	if a := set["free"]; a != nil {
		if v, ok := c.oneString(a); ok {
			c.checkFreeSym(a.args[0].pos, v)
			at.Free = v
		}
	}
	if a := set["c"]; a != nil {
		if v, ok := c.oneString(a); ok {
			c.checkCType(a.args[0].pos, v)
			at.CType = v
		}
	}
	return at
}

func (fc *fnCheck) parseMaxOut() {
	a := fc.ret["max_out"]
	if a == nil {
		return
	}
	fc.maxOutPos = a.pos
	if len(a.args) != 1 {
		fc.err(a.pos, RuleSyntax, "annotation @max_out takes one integer or const argument")
		return
	}
	if v, ok := fc.c.argInt(a.args[0]); ok {
		fc.maxOut = v
	}
}

func isVarBuf(t *Type) bool { return t.Kind == KStr || t.Kind == KBytes || t.Kind == KArray }

func isFixed(t *Type) bool {
	_, _, fixed := SizeOf(t)
	return fixed
}

// callerSupplied reports whether p is an out buffer whose storage the caller provides (V10, 4.6.2).
func callerSupplied(p *Param) bool {
	return p.Dir == DirOut && (p.Type.Kind == KBytes || p.Type.Kind == KArray) && !p.Attrs.Borrowed
}

// borrowedOut reports whether p is an out parameter whose memory the library owns.
func borrowedOut(p *Param) bool {
	return p.Dir == DirOut && (p.Type.Kind == KStr || (p.Type.Kind == KBytes || p.Type.Kind == KArray) && p.Attrs.Borrowed)
}

func (fc *fnCheck) annPos(i int, name string) Pos {
	if i < 0 {
		if a := fc.ret[name]; a != nil {
			return a.pos
		}
		return fc.rf.retPos
	}
	if a := fc.pset[i][name]; a != nil {
		return a.pos
	}
	return fc.f.Params[i].Pos
}

// checkParams applies V9, V10, V12 to V15, V27 and V38 to the parameters.
func (fc *fnCheck) checkParams() {
	f := fc.f
	seen := map[string]bool{}
	for i, p := range f.Params {
		if seen[p.Name] {
			fc.err(p.Pos, "V9", "parameter %q appears twice in %s", p.Name, f.Name)
		}
		seen[p.Name] = true
		t := p.Type

		if p.Dir == DirInOut {
			switch t.Kind {
			case KHandle, KStr, KBytes, KArray, KFixedArray:
				fc.err(p.Pos, "V10", "inout is not allowed on %s parameter %q", t, p.Name)
			}
		}
		if p.Dir != DirOut {
			switch {
			case t.Kind.IsScalar(), t.Kind == KHandle, t.Kind == KStr, t.Kind == KBytes, t.Kind == KFixedArray:
			case t.Kind == KArray && isFixed(t.Elem):
			default:
				fc.err(p.Pos, "V38", "composite %s parameter %q is not supported as an in or inout argument", t, p.Name)
			}
		} else {
			fc.checkOutType(i, p)
		}

		if a := fc.pset[i]["offset_in"]; a != nil {
			fc.checkOffsetIn(i, p, a)
		}
		if a := fc.pset[i]["nullable"]; a != nil {
			okType := t.Kind == KHandle || t.Kind == KStr || t.Kind == KBytes || t.Kind == KArray
			if p.Dir != DirIn || !okType {
				fc.err(a.pos, "V13", "@nullable is only valid on in handle, str, bytes or T[] parameters, not on %s %s", p.Dir, t)
			}
		}
		if a := fc.pset[i]["borrowed"]; a != nil {
			if p.Dir != DirOut || t.Kind != KBytes && t.Kind != KArray {
				fc.err(a.pos, "V14", "@borrowed is only valid on out bytes or out T[] parameters, not on %s %s", p.Dir, t)
			}
		}
		if a := fc.pset[i]["free"]; a != nil && !borrowedOut(p) {
			fc.err(a.pos, "V15", "@free is only valid on a borrowed out parameter or an out str, not on %s %s", p.Dir, t)
		}
	}
}

// checkOutType checks that an out parameter has a type whose response size is known (4.6.3, V27) and
// that a struct passed to C has its C type (V26).
func (fc *fnCheck) checkOutType(i int, p *Param) {
	t := p.Type
	switch {
	case t.Kind.IsScalar(), t.Kind == KHandle, t.Kind == KFixedArray:
	case t.Kind == KStruct:
		switch {
		case !isFixed(t):
			fc.err(p.Pos, "V27", "out struct %q has no static response bound; return it from a shim with @max_out", p.Name)
		case !fc.f.Shim && t.St.CType == "":
			fc.err(p.Pos, "V26", "out struct %q of a non-shim function needs a struct with @c", p.Name)
		}
	case t.Kind == KUnion:
		fc.err(p.Pos, "V27", "out union %q has no static response bound; return it from a shim with @max_out", p.Name)
	case t.Kind == KStr:
		if t.Max == 0 {
			fc.err(p.Pos, "V27", "out str %q needs a \"<=\" bound, so that the response bound is known before the call", p.Name)
		}
	case t.Kind == KBytes || t.Kind == KArray:
		if t.Kind == KArray && !isFixed(t.Elem) {
			fc.err(p.Pos, "V27", "out %s %q has no static response bound (variable-size elements)", t, p.Name)
			return
		}
		if p.Attrs.Borrowed && t.Max == 0 {
			fc.err(p.Pos, "V27", "borrowed out %q needs a \"<=\" bound, so that the response bound is known before the call", p.Name)
		}
	}
}

func (fc *fnCheck) checkOffsetIn(i int, p *Param, a *rawAnn) {
	if p.Attrs.OffsetIn == "" {
		return
	}
	if p.Dir != DirOut || p.Type.Kind != KU32 || p.Attrs.LenOf != "" {
		fc.err(a.pos, "V12", "@offset_in needs an out u32 parameter, %q is %s %s", p.Name, p.Dir, p.Type)
		return
	}
	j := fc.paramIndex(p.Attrs.OffsetIn)
	if j < 0 || j == i || fc.f.Params[j].Dir != DirIn || fc.f.Params[j].Type.Kind != KStr {
		fc.err(a.pos, "V12", "@offset_in(%s) must name an in str parameter", p.Attrs.OffsetIn)
	}
}

func (fc *fnCheck) paramIndex(name string) int {
	for i, p := range fc.f.Params {
		if p.Name == name {
			return i
		}
	}
	return -1
}

// composite reports whether a return type is composite in the sense of V26.
func composite(t *Type) bool {
	switch t.Kind {
	case KStruct, KUnion:
		return true
	case KArray:
		return !isFixed(t.Elem)
	}
	return false
}

// checkReturn applies V13, V15, V26, V27, V28 and V41 to the return.
func (fc *fnCheck) checkReturn() {
	f := fc.f
	t := f.Ret
	if t == nil {
		return
	}
	pos := fc.rf.retPos
	if a := fc.ret["nullable"]; a != nil {
		if t.Kind != KHandle && !isVarBuf(t) {
			fc.err(a.pos, "V13", "@nullable is only valid on a handle, str, bytes or T[] return, not on %s", t)
		}
	}
	if a := fc.ret["free"]; a != nil && !isVarBuf(t) {
		fc.err(a.pos, "V15", "@free is only valid on a str, bytes or T[] return, not on %s", t)
	}
	if isVarBuf(t) && t.Max == 0 {
		fc.err(pos, "V27", "a %s return needs a \"<=\" bound, so that the response bound is known before the call", t)
	}

	isComposite := composite(t)
	switch {
	case isComposite && !f.Shim:
		if !(t.Kind == KStruct && t.St.CType != "" && isFixed(t)) {
			fc.err(pos, "V26", "a composite return of type %s must be a shim function", t)
		}
	case isComposite && f.Shim && fc.maxOut == nil && fc.ret["max_out"] == nil:
		fc.err(pos, "V26", "a composite return of type %s needs @max_out on its return", t)
	case !isComposite && !f.Shim:
		okType := t.Kind.IsScalar() || t.Kind == KHandle || t.Kind == KStr || t.Kind == KBytes ||
			t.Kind == KArray && t.Elem.Kind.IsScalar()
		if !okType {
			fc.err(pos, "V28", "a non-shim function cannot return %s", t)
		}
	}
	if a := fc.ret["max_out"]; a != nil {
		if !f.Shim || !isComposite {
			fc.err(a.pos, "V26", "@max_out is only valid on the composite return of a shim")
		} else if fc.maxOut != nil {
			fc.checkMaxOut()
		}
	}
}

// checkMaxOut applies V41.
func (fc *fnCheck) checkMaxOut() {
	n := fc.maxOut
	if n.Cmp(big.NewInt(1)) < 0 || n.Cmp(big.NewInt(MaxStatic)) > 0 {
		fc.err(fc.maxOutPos, "V41", "@max_out(%s) is outside [1, 2^30]", n)
		return
	}
	fc.f.MaxOut = n.Int64()
	if wc, finite := WorstCase(fc.f.Ret); finite && fc.f.MaxOut < wc {
		fc.err(fc.maxOutPos, "V41", "@max_out(%d) is below wc(%s) = %d", fc.f.MaxOut, fc.f.Ret, wc)
	}
}

// checkLenOf applies V11 and the returned-buffer part of V28.
func (fc *fnCheck) checkLenOf() {
	f := fc.f
	n := len(f.Params)
	inBound := make([]int, n)  // in scalars bound to each buffer
	outBound := make([]int, n) // out scalars and the return bound to each buffer
	retBound := 0              // out scalars bound to the returned buffer

	check := func(idx int, name string, at Attrs, t *Type, dir Dir) {
		if at.LenOf == "" {
			return
		}
		pos := fc.annPos(idx, "len_of")
		if !t.Kind.IsInt() {
			fc.err(pos, "V11", "@len_of needs an integer type, %q is %s", name, t)
			return
		}
		if dir == DirInOut {
			fc.err(pos, "V11", "@len_of cannot annotate the inout parameter %q", name)
			return
		}
		if at.LenOf == "ret" {
			switch {
			case idx < 0:
				fc.err(pos, "V11", "the return cannot be bound to itself")
			case dir != DirOut:
				fc.err(pos, "V11", "@len_of(ret) is only valid on an out scalar, %q is %s", name, dir)
			case f.Ret == nil || f.Ret.Kind != KBytes && f.Ret.Kind != KArray:
				fc.err(pos, "V11", "@len_of(ret) needs a function that returns bytes or T[]")
			default:
				retBound++
			}
			return
		}
		j := fc.paramIndex(at.LenOf)
		if j < 0 || j == idx {
			fc.err(pos, "V11", "@len_of(%s) must name another parameter of %s", at.LenOf, f.Name)
			return
		}
		target := f.Params[j]
		if !isVarBuf(target.Type) {
			fc.err(pos, "V11", "@len_of(%s): %s is %s, not a str, bytes or T[]", at.LenOf, target.Name, target.Type)
			return
		}
		if idx >= 0 && dir == DirIn {
			if target.Dir == DirIn || callerSupplied(target) {
				inBound[j]++
			} else {
				fc.err(pos, "V11", "@len_of(%s): a borrowed out buffer has no caller-chosen capacity", at.LenOf)
			}
			return
		}
		// An out scalar or the return receives the produced length of an out buffer.
		if target.Dir != DirOut {
			fc.err(pos, "V11", "@len_of(%s): an out scalar or a return can only be bound to an out parameter", at.LenOf)
			return
		}
		outBound[j]++
	}
	for i, p := range f.Params {
		check(i, p.Name, p.Attrs, p.Type, p.Dir)
	}
	if f.Ret != nil {
		check(-1, "ret", f.RetAttrs, f.Ret, DirOut)
	}

	for j, p := range f.Params {
		if !isVarBuf(p.Type) {
			continue
		}
		pos := p.Pos
		switch {
		case p.Dir == DirIn:
			if inBound[j] == 0 && p.Type.Kind != KStr {
				fc.err(pos, "V11", "in buffer %q has no in scalar bound to it by @len_of", p.Name)
			}
			if inBound[j] > 1 {
				fc.err(pos, "V11", "in buffer %q has %d in scalars bound to it", p.Name, inBound[j])
			}
		case callerSupplied(p):
			if inBound[j] != 1 {
				fc.err(pos, "V11", "out buffer %q needs exactly one in scalar (its capacity) bound by @len_of, found %d", p.Name, inBound[j])
			}
			if outBound[j] > 1 {
				fc.err(pos, "V11", "out buffer %q has %d out scalars bound to it", p.Name, outBound[j])
			}
		case p.Dir == DirOut && p.Type.Kind != KStr:
			if outBound[j] != 1 {
				fc.err(pos, "V11", "borrowed out buffer %q needs exactly one out scalar bound by @len_of, found %d", p.Name, outBound[j])
			}
		default:
			if outBound[j] > 1 {
				fc.err(pos, "V11", "out str %q has %d out scalars bound to it", p.Name, outBound[j])
			}
		}
	}
	if retBound > 1 {
		fc.err(fc.rf.retPos, "V11", "the returned buffer has %d out scalars bound to it", retBound)
	}
	if t := f.Ret; t != nil && !f.Shim && (t.Kind == KBytes || t.Kind == KArray) && retBound != 1 {
		fc.err(fc.rf.retPos, "V28", "a non-shim function that returns %s needs one out scalar with @len_of(ret)", t)
	}
}

// checkOwnership applies V16.
func (fc *fnCheck) checkOwnership() {
	f := fc.f
	check := func(pos Pos, t *Type) {
		if t == nil || t.Kind != KHandle || t.H == nil || t.H.Parent == nil {
			return
		}
		count := 0
		for _, p := range f.Params {
			if p.Dir == DirIn && p.Type.Kind == KHandle && p.Type.H == t.H.Parent {
				count++
			}
		}
		if count != 1 {
			fc.err(pos, "V16", "%s creates a %s, which is owned by %s: it needs exactly one in parameter of type %s, found %d",
				f.Name, t.H.Name, t.H.Parent.Name, t.H.Parent.Name, count)
		}
	}
	for _, p := range f.Params {
		if p.Dir == DirOut {
			check(p.Pos, p.Type)
		}
	}
	check(fc.rf.retPos, f.Ret)
}

// resolveDestroy links every handle to its destroy function (V7).
func (c *checker) resolveDestroy() {
	for i, rh := range c.raw.handles {
		h := c.spec.Handles[i]
		fn := c.funcs[rh.destroy.text]
		if fn == nil {
			c.sink.add(rh.destroy.pos, "V7", "destroy %q names no declared function", rh.destroy.text)
			continue
		}
		okShape := len(fn.Params) == 1 && fn.Params[0].Dir == DirIn &&
			fn.Params[0].Type.Kind == KHandle && fn.Params[0].Type.H == h
		if !okShape {
			c.sink.add(rh.destroy.pos, "V7", "destroy function %s must have exactly one in parameter of type %s", fn.Name, h.Name)
			continue
		}
		if fn.Destroys != nil {
			c.sink.add(rh.destroy.pos, "V7", "function %s already destroys %s", fn.Name, fn.Destroys.Name)
			continue
		}
		h.Destroy, fn.Destroys = fn, h
		h.RetainOnFail = c.fcs[fn].decl["retain_on_fail"] != nil
	}
}

// checkDestroyFuncs applies the return rule of V7 and V39, once the error conventions are resolved.
func (c *checker) checkDestroyFuncs() {
	for _, h := range c.spec.Handles {
		fn := h.Destroy
		if fn == nil || fn.Ret == nil {
			continue
		}
		if !fn.Ret.Kind.IsInt() || fn.Error == nil || fn.Error.Kind != ErrStatus {
			c.sink.add(fn.Pos, "V7", "destroy function %s must return void or an integer with @error(status)", fn.Name)
		}
	}
	for _, fn := range c.spec.Funcs {
		a := c.fcs[fn].decl["retain_on_fail"]
		if a == nil {
			continue
		}
		if fn.Destroys == nil || fn.Error == nil || fn.Error.Kind != ErrStatus {
			c.sink.add(a.pos, "V39", "@retain_on_fail is only valid on the destroy function of a handle that has @error(status)")
		}
	}
}

// typeEq compares two types as the C glue sees them: bounds are wire limits and do not count.
func typeEq(a, b *Type) bool {
	if a.Kind != b.Kind {
		return false
	}
	switch a.Kind {
	case KStruct, KUnion, KHandle:
		return a.Name == b.Name
	case KFixedArray:
		return a.Len == b.Len && typeEq(a.Elem, b.Elem)
	case KArray:
		return typeEq(a.Elem, b.Elem)
	}
	return true
}

func isIntOrBool(t *Type) bool { return t.Kind.IsInt() || t.Kind == KBool }

// resolveError interprets @error (V18 to V23).
func (fc *fnCheck) resolveError() {
	fc.phase(func() {
		a := fc.decl["error"]
		if a == nil {
			return
		}
		fc.errorConv(a)
	})
}

func (fc *fnCheck) errorConv(a *rawAnn) {
	f := fc.f
	if f.Ret == nil {
		fc.err(a.pos, "V18", "@error is not allowed on a function that returns void")
		return
	}
	ec := &ErrorConv{}
	var (
		kindSet  bool
		okArg    *rawArg
		detArg   *rawArg
		predName string
		predPos  = a.pos
	)
	setKind := func(pos Pos, k ErrKind) {
		if kindSet {
			fc.err(pos, "V18", "@error has more than one kind")
		}
		kindSet = true
		ec.Kind = k
	}
	for i := range a.args {
		arg := &a.args[i]
		if arg.kind != aIdent {
			fc.err(arg.pos, "V18", "@error: unexpected argument")
			continue
		}
		switch arg.text {
		case "status", "null":
			if arg.hasParens {
				fc.err(arg.pos, "V18", "@error(%s) takes no arguments", arg.text)
			}
			if arg.text == "status" {
				setKind(arg.pos, ErrStatus)
			} else {
				setKind(arg.pos, ErrNull)
			}
		case "predicate":
			setKind(arg.pos, ErrPredicate)
			if len(arg.args) != 1 || arg.args[0].kind != aIdent || arg.args[0].hasParens {
				fc.err(arg.pos, "V18", "predicate takes one function name")
				continue
			}
			predName, predPos = arg.args[0].text, arg.args[0].pos
		case "ok":
			if okArg != nil {
				fc.err(arg.pos, "V19", "@error has more than one ok(...) list")
				continue
			}
			okArg = arg
		case "detail":
			if detArg != nil {
				fc.err(arg.pos, "V22", "@error has more than one detail(...) clause")
				continue
			}
			detArg = arg
		default:
			fc.err(arg.pos, "V18", "@error: unknown kind or clause %q", arg.text)
		}
	}
	if !kindSet {
		fc.err(a.pos, "V18", "@error needs exactly one kind: status, null or predicate(f)")
		return
	}

	switch ec.Kind {
	case ErrStatus:
		fc.statusOk(ec, okArg, a)
	case ErrNull:
		if t := f.Ret; t.Kind != KHandle && t.Kind != KStr && t.Kind != KBytes && t.Kind != KArray {
			fc.err(a.pos, "V20", "@error(null) needs a handle, str, bytes or array return, not %s", t)
		}
		if f.RetAttrs.Nullable {
			fc.err(a.pos, "V20", "@error(null) is not allowed together with a @nullable return: NULL is the failure")
		}
	case ErrPredicate:
		ec.Predicate = predName
		if predName != "" {
			fc.checkPredicate(predName, predPos)
		}
	}
	if ec.Kind != ErrStatus && okArg != nil {
		fc.err(okArg.pos, "V19", "ok(...) is only valid with @error(status)")
	}
	if detArg != nil {
		ec.Detail = fc.detail(detArg)
	}
	f.Error = ec
}

// statusOk resolves ok(...) of @error(status) (V19).
func (fc *fnCheck) statusOk(ec *ErrorConv, okArg *rawArg, a *rawAnn) {
	f := fc.f
	if !f.Ret.Kind.IsInt() {
		fc.err(a.pos, "V19", "@error(status) needs an integer return, not %s", f.Ret)
		return
	}
	if okArg == nil {
		ec.Ok = []int64{0}
		return
	}
	if len(okArg.args) == 0 {
		fc.err(okArg.pos, "V19", "ok(...) needs at least one value")
		return
	}
	seen := map[int64]bool{}
	for _, v := range okArg.args {
		val, ok := fc.c.argInt(v)
		if !ok {
			continue
		}
		if !fitsKind(val, f.Ret.Kind) || !val.IsInt64() {
			fc.err(v.pos, "V19", "ok value %s does not fit the return type %s", val, f.Ret)
			continue
		}
		x := val.Int64()
		if seen[x] {
			fc.err(v.pos, "V19", "ok value %d appears twice", x)
			continue
		}
		seen[x] = true
		ec.Ok = append(ec.Ok, x)
	}
	sort.Slice(ec.Ok, func(i, j int) bool { return ec.Ok[i] < ec.Ok[j] })
}

func (fc *fnCheck) serverFunc(name string) *Func {
	g := fc.c.funcs[name]
	if g == nil {
		return nil
	}
	if gc := fc.c.fcs[g]; gc == nil || !gc.server {
		return nil
	}
	return g
}

// checkPredicate applies V21.
func (fc *fnCheck) checkPredicate(name string, pos Pos) {
	f := fc.f
	g := fc.serverFunc(name)
	switch {
	case g == nil:
		fc.err(pos, "V21", "predicate %q is not a declared @allow(server) function", name)
	case len(g.Params) != 1 || g.Params[0].Dir != DirIn || !typeEq(g.Params[0].Type, f.Ret):
		fc.err(pos, "V21", "predicate %s needs one in parameter of the return type %s", name, f.Ret)
	case g.Ret == nil || !isIntOrBool(g.Ret):
		fc.err(pos, "V21", "predicate %s must return an integer or bool", name)
	}
}

// detail resolves detail(f, a1, ...) (V22, V23).
func (fc *fnCheck) detail(d *rawArg) *Detail {
	f := fc.f
	if !d.hasParens || len(d.args) == 0 || d.args[0].kind != aIdent || d.args[0].hasParens {
		fc.err(d.pos, "V22", "detail takes a function name and argument names")
		return nil
	}
	det := &Detail{Func: d.args[0].text}
	g := fc.serverFunc(det.Func)
	if g == nil {
		fc.err(d.args[0].pos, "V22", "detail function %q is not a declared @allow(server) function", det.Func)
		return det
	}
	if g.Ret == nil || g.Ret.Kind != KStr || g.Ret.Max == 0 {
		fc.err(d.args[0].pos, "V22", "detail function %s must return str<=N", g.Name)
	}
	for _, p := range g.Params {
		if p.Dir != DirIn {
			fc.err(d.args[0].pos, "V22", "detail function %s has a non-in parameter %q", g.Name, p.Name)
			break
		}
	}
	args := d.args[1:]
	if len(args) != len(g.Params) {
		fc.err(d.pos, "V22", "detail function %s takes %d arguments, the clause gives %d", g.Name, len(g.Params), len(args))
	}
	for i, arg := range args {
		var at *Type
		switch {
		case arg.kind == aRet:
			det.Args = append(det.Args, "ret")
			if f.Ret == nil {
				fc.err(arg.pos, "V23", "ret names a void return")
				continue
			}
			at = f.Ret
		case arg.kind == aIdent && !arg.hasParens:
			det.Args = append(det.Args, arg.text)
			j := fc.paramIndex(arg.text)
			if j < 0 {
				fc.err(arg.pos, "V22", "detail argument %q is not a parameter of %s", arg.text, f.Name)
				continue
			}
			p := f.Params[j]
			if !(p.Dir == DirIn || p.Dir == DirOut && p.Type.Kind == KHandle) {
				fc.err(arg.pos, "V22", "detail argument %q must be an in parameter or an out handle", arg.text)
				continue
			}
			if f.Destroys != nil && p.Type.Kind == KHandle && p.Type.H == f.Destroys {
				fc.err(arg.pos, "V22", "the detail of the destroy function %s cannot name the destroyed handle %q", f.Name, arg.text)
			}
			at = p.Type
		default:
			fc.err(arg.pos, "V22", "detail argument must be a parameter name or ret")
			continue
		}
		if i < len(g.Params) && !typeEq(at, g.Params[i].Type) {
			rule := "V22"
			if arg.kind == aRet {
				rule = "V23"
			}
			fc.err(arg.pos, rule, "detail argument %d has type %s, %s expects %s", i+1, at, g.Name, g.Params[i].Type)
		}
	}
	return det
}
