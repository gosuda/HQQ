package idl

import (
	"fmt"
	"math/big"
)

// The raw syntax tree. It records what the text says; check.go resolves names and applies the rules of 2.5.

type rawIntRef struct {
	pos    Pos
	isName bool
	name   string
	val    *big.Int
}

type rawType struct {
	pos Pos
	// elem is a type keyword ("i32", "str", "bytes", ...) or, when named is true, a type name.
	elem     string
	named    bool
	hasArr   bool
	arrLen   *rawIntRef // nil for T[]
	bound    *rawIntRef // nil without "<="
	boundPos Pos
}

type rawArg struct {
	pos       Pos
	kind      argKind
	val       *big.Int
	text      string // string content or identifier
	hasParens bool
	args      []rawArg
}

type argKind int

const (
	aInt argKind = iota
	aStr
	aRet
	aIdent
)

type rawAnn struct {
	pos       Pos
	name      string
	hasParens bool
	args      []rawArg
}

type rawName struct {
	pos  Pos
	text string
}

type rawConst struct {
	pos  Pos
	name rawName
	typ  string
	val  *big.Int
	vpos Pos
	tpos Pos
}

type rawHandle struct {
	pos     Pos
	anns    []rawAnn
	name    rawName
	ownedBy *rawName
	destroy rawName
}

type rawMember struct {
	pos  Pos
	name rawName
	typ  *rawType
}

type rawStruct struct {
	pos     Pos
	anns    []rawAnn
	name    rawName
	members []rawMember
}

type rawCase struct {
	pos  Pos
	tag  *big.Int
	tpos Pos
	name rawName
	typ  *rawType
}

type rawUnion struct {
	pos   Pos
	anns  []rawAnn
	name  rawName
	cases []rawCase
}

type rawParam struct {
	pos  Pos
	dir  Dir
	name rawName
	typ  *rawType
	anns []rawAnn
}

type rawFunc struct {
	pos     Pos
	anns    []rawAnn
	shim    bool
	name    rawName
	params  []rawParam
	ret     *rawType
	retAnns []rawAnn
	retPos  Pos
}

type rawInclude struct {
	pos  Pos
	text string
}

type rawFile struct {
	idlVersion  *big.Int
	idlPos      Pos
	serviceAnns []rawAnn
	service     rawName
	servicePos  Pos
	includes    []rawInclude
	consts      []rawConst
	handles     []rawHandle
	structs     []rawStruct
	unions      []rawUnion
	funcs       []rawFunc
}

type parser struct {
	toks  []token
	i     int
	depth int
	file  *rawFile
	sink  *diagSink
}

// parseFile parses the token stream. A syntax error stops the parse and is returned; violations of
// semantic rules that the grammar can see (V1, V2, V3, V5) are added to the sink and the parse goes on.
func parseFile(toks []token, sink *diagSink) (f *rawFile, err *syntaxError) {
	p := &parser{toks: toks, file: &rawFile{}, sink: sink}
	defer func() {
		if r := recover(); r != nil {
			se, ok := r.(*syntaxError)
			if !ok {
				panic(r)
			}
			f, err = nil, se
		}
	}()
	p.file0()
	return p.file, nil
}

func (p *parser) fail(pos Pos, format string, args ...any) {
	panic(&syntaxError{pos, fmt.Sprintf(format, args...)})
}

func (p *parser) cur() token { return p.toks[p.i] }

func (p *parser) peek(n int) token {
	if p.i+n < len(p.toks) {
		return p.toks[p.i+n]
	}
	return p.toks[len(p.toks)-1]
}

func (p *parser) advance() token {
	t := p.toks[p.i]
	if t.kind != tEOF {
		p.i++
	}
	return t
}

func (p *parser) isPunct(s string) bool { t := p.cur(); return t.kind == tPunct && t.text == s }

func (p *parser) isKw(s string) bool { t := p.cur(); return t.kind == tKw && t.text == s }

func (p *parser) expectPunct(s string) token {
	if !p.isPunct(s) {
		p.fail(p.cur().pos, "expected %q, found %s", s, p.cur().describe())
	}
	return p.advance()
}

func (p *parser) expectKw(s string) token {
	if !p.isKw(s) {
		p.fail(p.cur().pos, "expected %q, found %s", s, p.cur().describe())
	}
	return p.advance()
}

// expectIdent reads an identifier that refers to something (a type, a function, a parameter).
func (p *parser) expectIdent(what string) rawName {
	t := p.cur()
	if t.kind != tIdent {
		p.fail(t.pos, "expected %s, found %s", what, t.describe())
	}
	p.advance()
	return rawName{t.pos, t.text}
}

// expectDeclName reads the name of a declaration. A keyword is accepted and reported as V1, so that the
// rest of the file is still checked.
func (p *parser) expectDeclName(what string) rawName {
	t := p.cur()
	if t.kind == tKw {
		p.sink.add(t.pos, "V1", "%s: %q is a keyword of the IDL", what, t.text)
		p.advance()
		return rawName{t.pos, t.text}
	}
	return p.expectIdent(what)
}

func (p *parser) expectInt(what string) token {
	t := p.cur()
	if t.kind != tInt {
		p.fail(t.pos, "expected %s, found %s", what, t.describe())
	}
	return p.advance()
}

func (p *parser) expectString(what string) token {
	t := p.cur()
	if t.kind != tStr {
		p.fail(t.pos, "expected %s, found %s", what, t.describe())
	}
	return p.advance()
}

// file0 parses File = "idl" Int ";" { Ann } "service" Ident ";" { Decl } EOF.
func (p *parser) file0() {
	f := p.file
	p.expectKw("idl")
	v := p.expectInt("the IDL version")
	f.idlVersion, f.idlPos = v.val, v.pos
	p.expectPunct(";")
	f.serviceAnns = p.annotations()
	f.servicePos = p.expectKw("service").pos
	f.service = p.expectDeclName("the service name")
	p.expectPunct(";")
	for p.cur().kind != tEOF {
		p.decl()
	}
}

func (p *parser) annotations() []rawAnn {
	var out []rawAnn
	for p.cur().kind == tAnn {
		out = append(out, p.annotation())
	}
	return out
}

func (p *parser) annotation() rawAnn {
	t := p.advance()
	a := rawAnn{pos: t.pos, name: t.text}
	if p.isPunct("(") {
		a.hasParens = true
		a.args = p.annArgs()
	}
	return a
}

// maxAnnDepth bounds the nesting of annotation arguments, which the grammar leaves open; the deepest use
// in 2.4 is @error(status, ok(...)), two levels.
const maxAnnDepth = 8

// annArgs parses "(" [ AnnArg { "," AnnArg } ] ")".
func (p *parser) annArgs() []rawArg {
	if p.depth++; p.depth > maxAnnDepth {
		p.fail(p.cur().pos, "annotation arguments nested more than %d levels", maxAnnDepth)
	}
	defer func() { p.depth-- }()
	p.expectPunct("(")
	var args []rawArg
	if !p.isPunct(")") {
		for {
			args = append(args, p.annArg())
			if !p.isPunct(",") {
				break
			}
			p.advance()
		}
	}
	p.expectPunct(")")
	return args
}

func (p *parser) annArg() rawArg {
	t := p.cur()
	switch {
	case t.kind == tInt:
		p.advance()
		return rawArg{pos: t.pos, kind: aInt, val: t.val}
	case t.kind == tStr:
		p.advance()
		return rawArg{pos: t.pos, kind: aStr, text: t.text}
	case t.kind == tKw && t.text == "ret":
		p.advance()
		return rawArg{pos: t.pos, kind: aRet, text: "ret"}
	case t.kind == tIdent:
		p.advance()
		a := rawArg{pos: t.pos, kind: aIdent, text: t.text}
		if p.isPunct("(") {
			a.hasParens = true
			a.args = p.annArgs()
		}
		return a
	}
	p.fail(t.pos, "expected an annotation argument, found %s", t.describe())
	return rawArg{}
}

func (p *parser) decl() {
	t := p.cur()
	if t.kind == tKw && t.text == "include" {
		p.advance()
		s := p.expectString("the include path")
		p.expectPunct(";")
		p.file.includes = append(p.file.includes, rawInclude{s.pos, s.text})
		return
	}
	if t.kind == tKw && t.text == "const" {
		p.constDecl()
		return
	}
	anns := p.annotations()
	t = p.cur()
	if t.kind == tKw && t.text == "service" {
		p.sink.add(t.pos, "V3", "a file has exactly one service declaration")
		p.advance()
		p.expectDeclName("the service name")
		p.expectPunct(";")
		return
	}
	if t.kind != tKw {
		p.fail(t.pos, "expected a declaration, found %s", t.describe())
	}
	switch t.text {
	case "handle":
		p.handleDecl(anns)
	case "struct":
		p.structDecl(anns)
	case "union":
		p.unionDecl(anns)
	case "fn", "shim":
		p.fnDecl(anns)
	default:
		if len(anns) > 0 {
			p.fail(anns[0].pos, "annotations may only precede handle, struct, union or fn, found %s", t.describe())
		}
		p.fail(t.pos, "expected a declaration, found %s", t.describe())
	}
}

var intTypeNames = map[string]bool{
	"i8": true, "i16": true, "i32": true, "i64": true, "u8": true, "u16": true, "u32": true, "u64": true, "size": true,
}

func (p *parser) constDecl() {
	pos := p.advance().pos
	name := p.expectDeclName("the const name")
	p.expectPunct(":")
	tt := p.cur()
	if tt.kind != tKw || !intTypeNames[tt.text] {
		p.fail(tt.pos, "expected an integer type, found %s", tt.describe())
	}
	p.advance()
	p.expectPunct("=")
	v := p.expectInt("the const value")
	p.expectPunct(";")
	p.file.consts = append(p.file.consts, rawConst{pos: pos, name: name, typ: tt.text, tpos: tt.pos, val: v.val, vpos: v.pos})
}

func (p *parser) handleDecl(anns []rawAnn) {
	pos := p.advance().pos
	h := rawHandle{pos: pos, anns: anns}
	h.name = p.expectDeclName("the handle name")
	if p.isKw("owned_by") {
		p.advance()
		n := p.expectIdent("the parent handle name")
		h.ownedBy = &n
	}
	p.expectKw("destroy")
	h.destroy = p.expectIdent("the destroy function name")
	p.expectPunct(";")
	p.file.handles = append(p.file.handles, h)
}

func (p *parser) structDecl(anns []rawAnn) {
	pos := p.advance().pos
	s := rawStruct{pos: pos, anns: anns}
	s.name = p.expectDeclName("the struct name")
	p.expectPunct("{")
	for !p.isPunct("}") {
		if p.cur().kind == tEOF {
			p.fail(p.cur().pos, "unterminated struct %s", s.name.text)
		}
		m := rawMember{pos: p.cur().pos}
		m.name = p.expectDeclName("a field name")
		p.expectPunct(":")
		m.typ = p.typ()
		p.expectPunct(";")
		s.members = append(s.members, m)
	}
	p.advance()
	if len(s.members) == 0 {
		p.sink.add(s.name.pos, "V5", "struct %s has no fields", s.name.text)
	}
	p.file.structs = append(p.file.structs, s)
}

func (p *parser) unionDecl(anns []rawAnn) {
	pos := p.advance().pos
	u := rawUnion{pos: pos, anns: anns}
	u.name = p.expectDeclName("the union name")
	p.expectPunct("{")
	for !p.isPunct("}") {
		if p.cur().kind == tEOF {
			p.fail(p.cur().pos, "unterminated union %s", u.name.text)
		}
		c := rawCase{pos: p.cur().pos}
		p.expectKw("case")
		tag := p.expectInt("the case number")
		c.tag, c.tpos = tag.val, tag.pos
		c.name = p.expectDeclName("the case name")
		if p.isPunct(":") {
			p.advance()
			c.typ = p.typ()
		}
		p.expectPunct(";")
		u.cases = append(u.cases, c)
	}
	p.advance()
	if len(u.cases) == 0 {
		p.sink.add(u.name.pos, "V5", "union %s has no cases", u.name.text)
	}
	p.file.unions = append(p.file.unions, u)
}

func (p *parser) fnDecl(anns []rawAnn) {
	fn := rawFunc{anns: anns, pos: p.cur().pos}
	if p.isKw("shim") {
		p.advance()
		fn.shim = true
	}
	p.expectKw("fn")
	fn.name = p.expectDeclName("the function name")
	p.expectPunct("(")
	if !p.isPunct(")") {
		for {
			fn.params = append(fn.params, p.param())
			if !p.isPunct(",") {
				break
			}
			p.advance()
		}
	}
	p.expectPunct(")")
	if p.isPunct("->") {
		p.advance()
		fn.retPos = p.cur().pos
		fn.ret = p.typ()
		fn.retAnns = p.annotations()
	}
	p.expectPunct(";")
	p.file.funcs = append(p.file.funcs, fn)
}

func (p *parser) param() rawParam {
	rp := rawParam{pos: p.cur().pos, dir: DirIn}
	switch {
	case p.isKw("in"):
		p.advance()
	case p.isKw("out"):
		p.advance()
		rp.dir = DirOut
	case p.isKw("inout"):
		p.advance()
		rp.dir = DirInOut
	}
	rp.name = p.expectDeclName("a parameter name")
	p.expectPunct(":")
	rp.typ = p.typ()
	rp.anns = p.annotations()
	return rp
}

// typ parses Type = Elem [ "[" [ IntRef ] "]" ] [ "<=" IntRef ].
func (p *parser) typ() *rawType {
	t := p.cur()
	rt := &rawType{pos: t.pos}
	switch {
	case t.kind == tKw && (intTypeNames[t.text] || t.text == "bool" || t.text == "f32" || t.text == "f64" ||
		t.text == "str" || t.text == "bytes"):
		rt.elem = t.text
		p.advance()
	case t.kind == tIdent:
		rt.elem, rt.named = t.text, true
		p.advance()
	default:
		p.fail(t.pos, "expected a type, found %s", t.describe())
	}
	if p.isPunct("[") {
		p.advance()
		rt.hasArr = true
		if !p.isPunct("]") {
			r := p.intRef()
			rt.arrLen = &r
		}
		p.expectPunct("]")
	}
	if p.isPunct("<=") {
		rt.boundPos = p.advance().pos
		r := p.intRef()
		rt.bound = &r
	}
	return rt
}

func (p *parser) intRef() rawIntRef {
	t := p.cur()
	switch t.kind {
	case tInt:
		p.advance()
		return rawIntRef{pos: t.pos, val: t.val}
	case tIdent:
		p.advance()
		return rawIntRef{pos: t.pos, isName: true, name: t.text}
	}
	p.fail(t.pos, "expected an integer or a const name, found %s", t.describe())
	return rawIntRef{}
}
