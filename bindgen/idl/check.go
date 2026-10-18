package idl

import (
	"math/big"
	"strings"
	"unicode"
)

// Options carries what validation needs besides the text: GoStrip is the -go-strip prefix, which rule V37
// applies when it checks the Go names (9.2).
type Options struct{ GoStrip string }

// Parse lexes, parses and validates src against every rule of 2.5. On failure the returned Spec is nil
// and the error is a Diagnostics value; a lexical or grammatical error is reported alone, validation errors
// are all reported. The result does not depend on anything but filename, src and opts.
func Parse(filename string, src []byte, opts Options) (*Spec, error) {
	sink := &diagSink{file: filename}
	toks, serr := lex(src)
	if serr != nil {
		sink.add(serr.pos, RuleSyntax, "%s", serr.msg)
		return nil, sink.sorted()
	}
	raw, serr := parseFile(toks, sink)
	if serr != nil {
		sink.add(serr.pos, RuleSyntax, "%s", serr.msg)
		return nil, sink.sorted()
	}
	c := &checker{raw: raw, sink: sink, opts: opts}
	spec := c.run()
	if len(sink.list) > 0 {
		return nil, sink.sorted()
	}
	return spec, nil
}

// Func returns the declared function with the given C name, or nil.
func (s *Spec) Func(name string) *Func {
	for _, f := range s.Funcs {
		if f.Name == name {
			return f
		}
	}
	return nil
}

var cKeywords = map[string]bool{
	"auto": true, "break": true, "case": true, "char": true, "const": true, "continue": true, "default": true,
	"do": true, "double": true, "else": true, "enum": true, "extern": true, "float": true, "for": true,
	"goto": true, "if": true, "inline": true, "int": true, "long": true, "register": true, "restrict": true,
	"return": true, "short": true, "signed": true, "sizeof": true, "static": true, "struct": true,
	"switch": true, "typedef": true, "union": true, "unsigned": true, "void": true, "volatile": true,
	"while": true, "_Alignas": true, "_Alignof": true, "_Atomic": true, "_Bool": true, "_Complex": true,
	"_Generic": true, "_Imaginary": true, "_Noreturn": true, "_Static_assert": true, "_Thread_local": true,
}

// typeEnt is an entry of the type namespace.
type typeEnt struct {
	st *Struct
	un *Union
	h  *Handle
}

type checker struct {
	raw  *rawFile
	sink *diagSink
	opts Options
	spec *Spec

	consts map[string]*Const
	types  map[string]typeEnt
	funcs  map[string]*Func
	fcs    map[*Func]*fnCheck

	deferred []func() // checks that need the sizes of every type
}

func (c *checker) run() *Spec {
	f := c.raw
	c.spec = &Spec{Service: f.service.text}
	c.consts = map[string]*Const{}
	c.types = map[string]typeEnt{}
	c.funcs = map[string]*Func{}
	c.fcs = map[*Func]*fnCheck{}

	if f.idlVersion.Cmp(big.NewInt(1)) != 0 {
		c.sink.add(f.idlPos, "V2", "idl %s: only version 1 exists", f.idlVersion)
	}
	c.checkName(f.service.pos, f.service.text, "service name")
	c.serviceAnnotations()
	for _, in := range f.includes {
		c.spec.Includes = append(c.spec.Includes, in.text)
		c.checkInclude(in)
	}
	c.declareConsts()
	c.declareTypes()
	c.resolveHandleParents()
	c.resolveMembers()
	c.checkCycles()
	c.checkStructsAndUnions()
	c.declareFuncs()
	c.resolveDestroy()
	for _, fn := range c.spec.Funcs {
		c.fcs[fn].resolveError()
	}
	c.checkDestroyFuncs()
	for _, d := range c.deferred {
		d()
	}
	for _, fn := range c.spec.Funcs {
		c.fcs[fn].finish()
	}
	c.assignOpcodes()
	c.checkGoNames()
	return c.spec
}

// checkName applies the name rules of V1 (C keywords, "__") and V37 (the "hqq" prefix). Keywords of the IDL
// are rejected by the parser.
func (c *checker) checkName(pos Pos, name, what string) {
	if cKeywords[name] {
		c.sink.add(pos, "V1", "%s %q is a C keyword", what, name)
	}
	if strings.Contains(name, "__") {
		c.sink.add(pos, "V1", "%s %q contains \"__\"", what, name)
	}
	if len(name) >= 3 && strings.EqualFold(name[:3], "hqq") {
		c.sink.add(pos, "V37", "%s %q begins with \"hqq\", which is reserved for generated names", what, name)
	}
}

func (c *checker) checkInclude(in rawInclude) {
	s := in.text
	bad := func(format string, args ...any) { c.sink.add(in.pos, "V35", format, args...) }
	if len(s) < 1 || len(s) > 256 {
		bad("include string has length %d, want 1 to 256", len(s))
		return
	}
	for i := range len(s) {
		ch := s[i]
		ok := ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z' || ch >= '0' && ch <= '9' ||
			strings.IndexByte("_./+-", ch) >= 0
		if !ok {
			bad("include string contains %q; allowed are letters, digits and _ . / + -", ch)
			return
		}
	}
	if s[0] == '/' {
		bad("include string %q is absolute", s)
		return
	}
	for _, comp := range strings.Split(s, "/") {
		if comp == ".." {
			bad("include string %q contains a \"..\" component", s)
			return
		}
	}
}

// checkCType applies V36 to a @c string.
func (c *checker) checkCType(pos Pos, s string) {
	if len(s) < 1 || len(s) > 64 {
		c.sink.add(pos, "V36", "@c string has length %d, want 1 to 64", len(s))
		return
	}
	if !(isIdentStart(s[0])) {
		c.sink.add(pos, "V36", "@c string %q must start with a letter or _", s)
		return
	}
	for i := range len(s) {
		ch := s[i]
		if !isIdentPart(ch) && ch != ' ' && ch != '*' {
			c.sink.add(pos, "V36", "@c string %q contains %q; allowed are letters, digits, _, space and *", s, ch)
			return
		}
	}
}

// checkFreeSym applies V36 to an @free symbol.
func (c *checker) checkFreeSym(pos Pos, s string) {
	if len(s) < 1 || len(s) > 64 {
		c.sink.add(pos, "V36", "@free symbol has length %d, want 1 to 64", len(s))
		return
	}
	if !isIdentStart(s[0]) {
		c.sink.add(pos, "V36", "@free symbol %q must start with a letter or _", s)
		return
	}
	for i := range len(s) {
		if !isIdentPart(s[i]) {
			c.sink.add(pos, "V36", "@free symbol %q must be a single identifier", s)
			return
		}
	}
}

// checkCancelSym applies V1/V36/V37 to a @cancel symbol.
func (c *checker) checkCancelSym(pos Pos, s string) {
	if len(s) < 1 || len(s) > 64 {
		c.sink.add(pos, "V36", "@cancel symbol has length %d, want 1 to 64", len(s))
		return
	}
	if !isIdentStart(s[0]) {
		c.sink.add(pos, "V36", "@cancel symbol %q must start with a letter or _", s)
		return
	}
	for i := range len(s) {
		if !isIdentPart(s[i]) {
			c.sink.add(pos, "V36", "@cancel symbol %q must be a single identifier", s)
			return
		}
	}
	c.checkName(pos, s, "@cancel symbol")
}

// annSet is the annotations of one place by name.
type annSet map[string]*rawAnn

// takeAnns sorts the annotations of one place. An annotation that does not belong there, a repeated one and
// an unknown one are syntax errors; @serialize outside the service is V3.
func (c *checker) takeAnns(anns []rawAnn, place string, allowed ...string) annSet {
	set := annSet{}
	for i := range anns {
		a := &anns[i]
		ok := false
		for _, n := range allowed {
			if n == a.name {
				ok = true
			}
		}
		switch {
		case a.name == "serialize" && (place != "the service" || set[a.name] != nil):
			c.sink.add(a.pos, "V3", "@serialize appears at most once, on the service declaration, not on %s", place)
		case !ok:
			c.sink.add(a.pos, RuleSyntax, "annotation @%s is not valid on %s", a.name, place)
		case set[a.name] != nil:
			c.sink.add(a.pos, RuleSyntax, "annotation @%s appears twice on %s", a.name, place)
		default:
			set[a.name] = a
		}
	}
	return set
}

// noArgs reports a syntax error when a flag annotation carries arguments.
func (c *checker) noArgs(a *rawAnn) {
	if a != nil && len(a.args) > 0 {
		c.sink.add(a.pos, RuleSyntax, "annotation @%s takes no arguments", a.name)
	}
}

// oneString returns the single string argument of an annotation.
func (c *checker) oneString(a *rawAnn) (string, bool) {
	if len(a.args) != 1 || a.args[0].kind != aStr || a.args[0].hasParens {
		c.sink.add(a.pos, RuleSyntax, "annotation @%s takes one string argument", a.name)
		return "", false
	}
	return a.args[0].text, true
}

// oneIdent returns the single identifier argument of an annotation; ret is accepted when allowRet.
func (c *checker) oneIdent(a *rawAnn, allowRet bool) (string, bool) {
	if len(a.args) == 1 && !a.args[0].hasParens {
		switch a.args[0].kind {
		case aIdent:
			return a.args[0].text, true
		case aRet:
			if allowRet {
				return "ret", true
			}
		}
	}
	c.sink.add(a.pos, RuleSyntax, "annotation @%s takes one name argument", a.name)
	return "", false
}

func (c *checker) serviceAnnotations() {
	set := c.takeAnns(c.raw.serviceAnns, "the service", "serialize")
	a := set["serialize"]
	if a == nil {
		return
	}
	v, ok := c.oneIdent(a, false)
	if !ok {
		return
	}
	switch v {
	case "session":
		c.spec.Serialize = SerializeSession
	case "none":
		c.spec.Serialize = SerializeNone
	default:
		c.sink.add(a.args[0].pos, RuleSyntax, "@serialize takes session or none, not %q", v)
	}
}

// Integer ranges of the integer types.
var kindRange = map[Kind][2]*big.Int{}

func init() {
	r := func(lo, hi *big.Int) [2]*big.Int { return [2]*big.Int{lo, hi} }
	pow := func(n uint) *big.Int { return new(big.Int).Lsh(big.NewInt(1), n) }
	signed := func(bitsN uint) [2]*big.Int {
		return r(new(big.Int).Neg(pow(bitsN-1)), new(big.Int).Sub(pow(bitsN-1), big.NewInt(1)))
	}
	unsigned := func(bitsN uint) [2]*big.Int { return r(big.NewInt(0), new(big.Int).Sub(pow(bitsN), big.NewInt(1))) }
	kindRange[KI8], kindRange[KI16], kindRange[KI32], kindRange[KI64] = signed(8), signed(16), signed(32), signed(64)
	kindRange[KU8], kindRange[KU16], kindRange[KU32], kindRange[KU64] = unsigned(8), unsigned(16), unsigned(32), unsigned(64)
	kindRange[KSize] = unsigned(64)
}

func fitsKind(v *big.Int, k Kind) bool {
	r, ok := kindRange[k]
	return ok && v.Cmp(r[0]) >= 0 && v.Cmp(r[1]) <= 0
}

var kindByName = map[string]Kind{
	"bool": KBool, "i8": KI8, "i16": KI16, "i32": KI32, "i64": KI64, "u8": KU8, "u16": KU16, "u32": KU32,
	"u64": KU64, "size": KSize, "f32": KF32, "f64": KF64, "str": KStr, "bytes": KBytes,
}

func (c *checker) declareConsts() {
	for _, rc := range c.raw.consts {
		c.checkName(rc.name.pos, rc.name.text, "const name")
		k := kindByName[rc.typ]
		ct := &Const{Name: rc.name.text, Type: &Type{Kind: k}, Value: rc.val, Pos: rc.name.pos}
		if _, dup := c.consts[ct.Name]; dup {
			c.sink.add(rc.name.pos, "V1", "const %q is declared twice", ct.Name)
		} else {
			c.consts[ct.Name] = ct
		}
		c.spec.Consts = append(c.spec.Consts, ct)
		if !fitsKind(rc.val, k) {
			c.sink.add(rc.vpos, "V30", "const %s = %s does not fit %s", ct.Name, rc.val, rc.typ)
		}
	}
}

// intRef resolves an IntRef to its value. An unknown const name is V30.
func (c *checker) intRef(r rawIntRef) (*big.Int, bool) {
	if !r.isName {
		return r.val, true
	}
	k, ok := c.consts[r.name]
	if !ok {
		c.sink.add(r.pos, "V30", "%q is not a const", r.name)
		return nil, false
	}
	return k.Value, true
}

// argInt resolves an annotation argument that is an integer literal or a const name.
func (c *checker) argInt(a rawArg) (*big.Int, bool) {
	switch {
	case a.kind == aInt:
		return a.val, true
	case a.kind == aIdent && !a.hasParens:
		return c.intRef(rawIntRef{pos: a.pos, isName: true, name: a.text})
	}
	c.sink.add(a.pos, RuleSyntax, "expected an integer or a const name")
	return nil, false
}

func (c *checker) declareTypes() {
	f := c.raw
	add := func(name rawName, ent typeEnt) {
		c.checkName(name.pos, name.text, "type name")
		if _, dup := c.types[name.text]; dup {
			c.sink.add(name.pos, "V1", "type %q is declared twice", name.text)
			return
		}
		c.types[name.text] = ent
	}
	for i := range f.handles {
		rh := &f.handles[i]
		set := c.takeAnns(rh.anns, "a handle", "c", "cancel")
		h := &Handle{Name: rh.name.text, ID: i + 1, Pos: rh.name.pos}
		if a := set["c"]; a != nil {
			if s, ok := c.oneString(a); ok {
				c.checkCType(a.args[0].pos, s)
				h.CType = s
			}
		}
		if a := set["cancel"]; a != nil {
			if len(a.args) == 1 && !a.args[0].hasParens && (a.args[0].kind == aIdent || a.args[0].kind == aStr) {
				s := a.args[0].text
				c.checkCancelSym(a.args[0].pos, s)
				h.Cancel = s
			} else {
				c.sink.add(a.pos, RuleSyntax, "annotation @cancel takes one function name argument")
			}
		}
		c.spec.Handles = append(c.spec.Handles, h)
		add(rh.name, typeEnt{h: h})
	}
	for i := range f.structs {
		rs := &f.structs[i]
		set := c.takeAnns(rs.anns, "a struct", "c")
		s := &Struct{Name: rs.name.text, Pos: rs.name.pos}
		if a := set["c"]; a != nil {
			if v, ok := c.oneString(a); ok {
				c.checkCType(a.args[0].pos, v)
				s.CType = v
			}
		}
		c.spec.Structs = append(c.spec.Structs, s)
		add(rs.name, typeEnt{st: s})
	}
	for i := range f.unions {
		ru := &f.unions[i]
		c.takeAnns(ru.anns, "a union")
		u := &Union{Name: ru.name.text, Pos: ru.name.pos}
		c.spec.Unions = append(c.spec.Unions, u)
		add(ru.name, typeEnt{un: u})
	}
}

// resolveHandleParents checks V7 for owned_by. The destroy functions are checked after the functions exist.
func (c *checker) resolveHandleParents() {
	for i, rh := range c.raw.handles {
		h := c.spec.Handles[i]
		if rh.ownedBy == nil {
			continue
		}
		ent, ok := c.types[rh.ownedBy.text]
		switch {
		case !ok || ent.h == nil:
			c.sink.add(rh.ownedBy.pos, "V7", "owned_by %q does not name a handle type", rh.ownedBy.text)
		case ent.h == h:
			c.sink.add(rh.ownedBy.pos, "V7", "handle %s is owned by itself", h.Name)
		default:
			h.Parent = ent.h
		}
	}
	for _, h := range c.spec.Handles {
		for p, steps := h.Parent, 0; p != nil; p, steps = p.Parent, steps+1 {
			if p == h || steps > len(c.spec.Handles) {
				c.sink.add(h.Pos, "V7", "the owned_by relation of handle %s is cyclic", h.Name)
				h.Parent = nil
				break
			}
		}
	}
}

// resolveType turns a raw type into a Type and applies V4, V6 and V8. inFunc is true for a parameter or
// return type, where a handle is allowed (not as an array element).
func (c *checker) resolveType(rt *rawType, inFunc bool) *Type {
	var t *Type
	if rt.named {
		ent, ok := c.types[rt.elem]
		switch {
		case !ok:
			c.sink.add(rt.pos, "V4", "unknown type %q", rt.elem)
			return nil
		case ent.st != nil:
			t = &Type{Kind: KStruct, Name: rt.elem, St: ent.st}
		case ent.un != nil:
			t = &Type{Kind: KUnion, Name: rt.elem, Un: ent.un}
		default:
			t = &Type{Kind: KHandle, Name: rt.elem, H: ent.h}
			if !inFunc || rt.hasArr {
				c.sink.add(rt.pos, "V8", "handle type %q is only allowed as a function parameter or return type", rt.elem)
			}
		}
	} else {
		t = &Type{Kind: kindByName[rt.elem]}
	}
	if rt.hasArr {
		elem := t
		if rt.arrLen == nil {
			t = &Type{Kind: KArray, Elem: elem}
		} else {
			t = &Type{Kind: KFixedArray, Elem: elem}
			if n, ok := c.intRef(*rt.arrLen); ok {
				if n.Cmp(big.NewInt(1)) < 0 || n.Cmp(big.NewInt(MaxFixedArray)) > 0 {
					c.sink.add(rt.arrLen.pos, "V6", "array length %s is outside [1, 2^20]", n)
				} else {
					t.Len = n.Int64()
				}
			}
			arr := t
			c.deferred = append(c.deferred, func() {
				if _, _, fixed := SizeOf(elem); !fixed {
					c.sink.add(rt.pos, "V6", "T[N] needs a fixed element type, %s is not fixed", elem)
					return
				}
				if size, _, _ := SizeOf(arr); size > MaxStatic {
					c.sink.add(rt.pos, "V40", "fixed type %s is larger than 2^30 bytes", arr)
				}
			})
		}
	}
	if rt.bound != nil {
		if t.Kind != KStr && t.Kind != KBytes && t.Kind != KArray {
			c.sink.add(rt.boundPos, "V6", "\"<=\" is only valid after str, bytes or T[]")
			return t
		}
		if n, ok := c.intRef(*rt.bound); ok {
			if n.Cmp(big.NewInt(1)) < 0 || n.Cmp(big.NewInt(MaxBound)) > 0 {
				c.sink.add(rt.bound.pos, "V6", "bound %s is outside [1, 2^30]", n)
			} else {
				t.Max = n.Int64()
			}
		}
	}
	return t
}

func (c *checker) resolveMembers() {
	for i := range c.raw.structs {
		rs := &c.raw.structs[i]
		s := c.spec.Structs[i]
		for _, rm := range rs.members {
			c.checkName(rm.name.pos, rm.name.text, "field name")
			t := c.resolveType(rm.typ, false)
			if t == nil {
				t = &Type{Kind: KU8} // keeps the member list aligned; V4 is already reported
			}
			s.Members = append(s.Members, &Member{Name: rm.name.text, Type: t, Pos: rm.name.pos})
		}
	}
	for i := range c.raw.unions {
		ru := &c.raw.unions[i]
		u := c.spec.Unions[i]
		for _, rc := range ru.cases {
			c.checkName(rc.name.pos, rc.name.text, "case name")
			cs := &Case{Name: rc.name.text, Pos: rc.name.pos}
			if rc.tag.Sign() >= 0 && rc.tag.Cmp(new(big.Int).Lsh(big.NewInt(1), 32)) < 0 {
				cs.Tag = uint32(rc.tag.Uint64())
			} else {
				c.sink.add(rc.tpos, "V5", "case number %s is outside [0, 2^32-1]", rc.tag)
			}
			if rc.typ != nil {
				cs.Type = c.resolveType(rc.typ, false)
			}
			u.Cases = append(u.Cases, cs)
		}
	}
}

// typeDeps lists the struct and union declarations a type contains directly or through arrays.
func typeDeps(t *Type) (st *Struct, un *Union) {
	for t != nil && (t.Kind == KFixedArray || t.Kind == KArray) {
		t = t.Elem
	}
	if t == nil {
		return nil, nil
	}
	switch t.Kind {
	case KStruct:
		return t.St, nil
	case KUnion:
		return nil, t.Un
	}
	return nil, nil
}

// checkCycles reports V4 for a struct or union that contains itself.
func (c *checker) checkCycles() {
	const (
		white = iota
		grey
		black
	)
	state := map[any]int{}
	var visit func(node any)
	visit = func(node any) {
		state[node] = grey
		each := func(pos Pos, t *Type) {
			if t == nil {
				return
			}
			st, un := typeDeps(t)
			var dep any
			switch {
			case st != nil:
				dep = st
			case un != nil:
				dep = un
			default:
				return
			}
			switch state[dep] {
			case grey:
				c.sink.add(pos, "V4", "type %s contains itself", t)
			case white:
				visit(dep)
			}
		}
		switch n := node.(type) {
		case *Struct:
			for _, m := range n.Members {
				each(m.Pos, m.Type)
			}
		case *Union:
			for _, cs := range n.Cases {
				each(cs.Pos, cs.Type)
			}
		}
		state[node] = black
	}
	for _, s := range c.spec.Structs {
		if state[s] == white {
			visit(s)
		}
	}
	for _, u := range c.spec.Unions {
		if state[u] == white {
			visit(u)
		}
	}
}

func (c *checker) checkStructsAndUnions() {
	for _, s := range c.spec.Structs {
		seen := map[string]bool{}
		for _, m := range s.Members {
			if seen[m.Name] {
				c.sink.add(m.Pos, "V5", "struct %s has two fields named %q", s.Name, m.Name)
			}
			seen[m.Name] = true
		}
		s := s
		c.deferred = append(c.deferred, func() {
			if ti := structInfo(s); ti.fixed && ti.size > MaxStatic {
				c.sink.add(s.Pos, "V40", "fixed struct %s is larger than 2^30 bytes", s.Name)
			}
		})
	}
	for _, u := range c.spec.Unions {
		names := map[string]bool{}
		tags := map[uint32]bool{}
		for _, cs := range u.Cases {
			if names[cs.Name] {
				c.sink.add(cs.Pos, "V5", "union %s has two cases named %q", u.Name, cs.Name)
			}
			names[cs.Name] = true
			if tags[cs.Tag] {
				c.sink.add(cs.Pos, "V5", "union %s has two cases numbered %d", u.Name, cs.Tag)
			}
			tags[cs.Tag] = true
		}
	}
}

// assignOpcodes numbers the @allow(wire) functions in declaration order.
func (c *checker) assignOpcodes() {
	n := 0
	for _, f := range c.spec.Funcs {
		if c.fcs[f].server {
			continue
		}
		n++
		f.Opcode = n
		c.spec.Wire = append(c.spec.Wire, f)
	}
	if n > 65535 {
		for _, f := range c.spec.Wire[65535:] {
			c.sink.add(f.Pos, "V9", "more than 65535 functions have @allow(wire)")
			break
		}
	}
}

// capitalize upper-cases the first letter of a name (9.2 rule 2).
func capitalize(s string) string {
	if s == "" {
		return s
	}
	r := []rune(s)
	r[0] = unicode.ToUpper(r[0])
	return string(r)
}
