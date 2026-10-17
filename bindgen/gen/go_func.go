package gen

import (
	"strconv"
	"strings"

	"gosuda.org/hqq/bindgen/idl"
)

// goParam is one parameter of the generated Go signature, or one destination slice of a future.
type goParam struct {
	idx   int // index into Func.Params; -1 for the destination of a composite return
	name  string
	typ   string
	t     *idl.Type
	null  bool   // @nullable
	buf   bool   // caller-supplied storage: out buffer or composite-return destination
	field string // future field that keeps the storage (buf only)
}

// goResult is one non-error result of the generated Go methods.
type goResult struct {
	key  int // Func.Params index, -1 for the return value
	typ  string
	zero string
}

// goFunc generates the methods of one wire function.
type goFunc struct {
	e       *goEmitter
	f       *idl.Func
	plan    *idl.Plan
	name    string
	fut     string
	params  []*goParam // signature, C order, destination of a composite return last
	dsts    []*goParam // future fields after id
	results []goResult

	out goBuf

	// Scope tracking for the generated statements: which names exist and at which block depth.
	declared map[string]int
	depth    int
	counts   map[string]int
	zeros    []string // the zero values of the non-error results
	bv       string   // the Go expression of the response body inside Wait
	err      error    // first emitter failure
}

func goIsFixed(t *idl.Type) bool {
	_, _, fixed := idl.SizeOf(t)
	return fixed
}

// goCallerSupplied mirrors V10: an out bytes or T[] without @borrowed is storage the caller provides.
func goCallerSupplied(p *idl.Param) bool {
	return p.Dir == idl.DirOut && (p.Type.Kind == idl.KBytes || p.Type.Kind == idl.KArray) && !p.Attrs.Borrowed
}

// goCompositeDst reports whether t is a returned array of variable-size elements, which the caller may give
// a destination slice for.
func goCompositeDst(t *idl.Type) bool {
	return t != nil && t.Kind == idl.KArray && !goIsFixed(t.Elem)
}

func (e *goEmitter) newFunc(f *idl.Func) *goFunc {
	g := &goFunc{
		e:        e,
		f:        f,
		plan:     f.Plan,
		name:     e.names[f],
		declared: map[string]int{},
		counts:   map[string]int{},
	}
	g.fut = g.name + "Future"

	taken := map[string]bool{}
	for _, p := range f.Params {
		taken["p_"+p.Name] = true
	}
	for i, p := range f.Params {
		if p.Derived || p.Dir == idl.DirOut && !goCallerSupplied(p) {
			continue
		}
		gp := &goParam{idx: i, name: "p_" + p.Name, typ: goType(p.Type, p.Attrs.Nullable), t: p.Type, null: p.Attrs.Nullable}
		if goCallerSupplied(p) {
			gp.buf = true
			g.dsts = append(g.dsts, gp)
		}
		g.params = append(g.params, gp)
	}
	if goCompositeDst(f.Ret) {
		name := "p_dst"
		for taken[name] {
			name += "_"
		}
		gp := &goParam{idx: -1, name: name, typ: goType(f.Ret, false), t: f.Ret, buf: true}
		g.dsts = append(g.dsts, gp)
		g.params = append(g.params, gp)
	}
	for _, d := range g.dsts {
		switch {
		case d.idx < 0 || len(g.dsts) == 1:
			d.field = "dst"
		default:
			d.field = "dst_" + f.Params[d.idx].Name
		}
	}

	if g.retIsResult() {
		g.results = append(g.results, goResult{-1, goType(f.Ret, f.RetAttrs.Nullable), goZero(f.Ret, f.RetAttrs.Nullable)})
	}
	for i, p := range f.Params {
		if p.Derived || p.Dir == idl.DirIn {
			continue
		}
		g.results = append(g.results, goResult{i, goType(p.Type, false), goZero(p.Type, false)})
	}
	for _, r := range g.results {
		g.zeros = append(g.zeros, r.zero)
	}
	return g
}

// retIsResult reports whether the return value is a result of the Go method (9.4, rule 2).
func (g *goFunc) retIsResult() bool {
	f := g.f
	switch {
	case f.Ret == nil, f.RetAttrs.LenOf != "":
		return false
	case f.Error != nil && f.Error.Kind == idl.ErrStatus && len(f.Error.Ok) == 1:
		return false
	}
	return true
}

func (g *goFunc) fail(err error) {
	if g.err == nil {
		g.err = err
	}
}

func (g *goFunc) line(format string, args ...any) { g.out.line(format, args...) }

// retWith is the operand list of a return statement whose error is errExpr.
func (g *goFunc) retWith(errExpr string) string {
	return strings.Join(append(append([]string(nil), g.zeros...), errExpr), ", ")
}

// resultSig is the result list of the synchronous method and of Wait.
func (g *goFunc) resultSig() string {
	if len(g.results) == 0 {
		return "error"
	}
	parts := make([]string, 0, len(g.results)+1)
	for _, r := range g.results {
		parts = append(parts, r.typ)
	}
	return "(" + strings.Join(append(parts, "error"), ", ") + ")"
}

// paramSig is the parameter list, consecutive parameters of one type grouped as in `p_a, p_b int32`.
func (g *goFunc) paramSig() string {
	var sb strings.Builder
	for i := 0; i < len(g.params); {
		j := i
		for j+1 < len(g.params) && g.params[j+1].typ == g.params[i].typ {
			j++
		}
		if i > 0 {
			sb.WriteString(", ")
		}
		for k := i; k <= j; k++ {
			if k > i {
				sb.WriteString(", ")
			}
			sb.WriteString(g.params[k].name)
		}
		sb.WriteString(" " + g.params[i].typ)
		i = j + 1
	}
	return sb.String()
}

func (g *goFunc) argList() string {
	names := make([]string, len(g.params))
	for i, p := range g.params {
		names[i] = p.name
	}
	return strings.Join(names, ", ")
}

// emit writes the future type and the three methods of the function.
func (g *goFunc) emit() {
	g.emitFuture()
	g.emitDoc()
	g.emitSync()
	g.emitAsync()
	g.emitWait()
}

// emitDoc writes the doc comment of the synchronous method: the C signature in IDL terms and the failure
// convention, so that the Go reader does not need the IDL.
func (g *goFunc) emitDoc() {
	f := g.f
	args := make([]string, len(f.Params))
	for i, p := range f.Params {
		if p.Dir == idl.DirIn {
			args[i] = p.Name
			continue
		}
		args[i] = p.Dir.String() + " " + p.Name
	}
	g.line("// %s(%s)", f.Name, strings.Join(args, ", "))
	var notes []string
	if len(g.results) > 0 {
		names := make([]string, 0, len(g.results)+1)
		for _, r := range g.results {
			names = append(names, g.resultName(r))
		}
		notes = append(notes, "Results: ("+strings.Join(append(names, "error"), ", ")+").")
	}
	if f.Destroys != nil {
		notes = append(notes, "Destroy of "+f.Destroys.Name+goRetainNote(f.Destroys)+".")
	}
	if f.Error != nil {
		notes = append(notes, goErrorNote(f))
	}
	if len(g.dsts) > 0 {
		notes = append(notes, g.dstNote())
	}
	for _, n := range notes {
		g.line("//")
		g.line("// %s", n)
	}
}

func goRetainNote(h *idl.Handle) string {
	if h.RetainOnFail {
		return ": a failed call keeps the handle (@retain_on_fail)"
	}
	return ": the handle is consumed whatever the status"
}

func goErrorNote(f *idl.Func) string {
	var sb strings.Builder
	switch f.Error.Kind {
	case idl.ErrStatus:
		oks := make([]string, len(f.Error.Ok))
		for i, v := range f.Error.Ok {
			oks[i] = strconv.FormatInt(v, 10)
		}
		sb.WriteString("@error(status, ok(" + strings.Join(oks, ", ") + ")): any other return value is a *rt.LibError carrying it as Code")
	case idl.ErrNull:
		sb.WriteString("@error(null): a NULL result is a *rt.LibError with Code 0")
	case idl.ErrPredicate:
		sb.WriteString("@error(predicate(" + f.Error.Predicate + ")): a nonzero predicate is a *rt.LibError")
	}
	if f.Error.Detail != nil {
		sb.WriteString("; Detail is the text of " + f.Error.Detail.Func)
	}
	sb.WriteString(".")
	return sb.String()
}

func (g *goFunc) resultName(r goResult) string {
	if r.key < 0 {
		if g.f.Error != nil && g.f.Error.Kind == idl.ErrStatus {
			return "rc"
		}
		return "ret"
	}
	return g.f.Params[r.key].Name
}

func (g *goFunc) dstNote() string {
	var parts []string
	for _, d := range g.dsts {
		if d.idx < 0 {
			parts = append(parts, "the elements of the result are appended to "+d.name+"[:0]")
			continue
		}
		parts = append(parts, d.name+" is the output storage, its length is the capacity sent to the server, and the result is "+d.name+"[:n]")
	}
	return strings.Join(parts, "; ") + ". The slices must stay untouched until Wait returns."
}

func (g *goFunc) emitFuture() {
	g.line("type %s struct {", g.fut)
	g.line("c *Client")
	g.line("call *rt.Call")
	g.line("id uint32")
	for _, d := range g.dsts {
		g.line("%s %s", d.field, d.typ)
	}
	g.line("}")
	g.out.blank()
}

func (g *goFunc) emitSync() {
	g.line("func (c *Client) %s(%s) %s {", g.name, g.paramSig(), g.resultSig())
	g.line("f, err := c.%sAsync(%s)", g.name, g.argList())
	g.line("if err != nil {")
	g.line("return %s", g.retWith("err"))
	g.line("}")
	g.line("return f.Wait()")
	g.line("}")
	g.out.blank()
}

// alloc returns a local name of the family base: base itself first, then base1, base2, ...
func (g *goFunc) alloc(base string) string {
	n := g.counts[base]
	g.counts[base] = n + 1
	if n == 0 {
		return base
	}
	return base + strconv.Itoa(n)
}

// declare records names as existing at the current block depth.
func (g *goFunc) declare(names ...string) {
	for _, n := range names {
		g.declared[n] = g.depth
	}
}

// open and closeBlock bracket a Go block; names declared inside are forgotten at its end.
func (g *goFunc) open(format string, args ...any) {
	g.line(format, args...)
	g.depth++
}

func (g *goFunc) closeBlock() {
	for n, d := range g.declared {
		if d == g.depth {
			delete(g.declared, n)
		}
	}
	g.depth--
	g.line("}")
}

// goLHS is one assigned name with the Go type it has if it must be declared.
type goLHS struct{ name, typ string }

// assign emits `a, b := rhs` or `a, b = rhs`. Names that do not exist yet are declared; when some existing
// name was declared in an outer block, `:=` would shadow it, so the new names get a `var` line first.
func (g *goFunc) assign(rhs string, ls ...goLHS) {
	names := make([]string, len(ls))
	var fresh []goLHS
	sameBlock := true
	for i, l := range ls {
		names[i] = l.name
		d, ok := g.declared[l.name]
		switch {
		case l.name == "_":
		case !ok:
			fresh = append(fresh, l)
		case d != g.depth:
			sameBlock = false
		}
	}
	list := strings.Join(names, ", ")
	switch {
	case len(fresh) == 0:
		g.line("%s = %s", list, rhs)
	case sameBlock:
		g.line("%s := %s", list, rhs)
	default:
		for _, l := range fresh {
			g.line("var %s %s", l.name, l.typ)
		}
		g.line("%s = %s", list, rhs)
	}
	for _, l := range fresh {
		g.declared[l.name] = g.depth
	}
}

// malformed emits the return of the Malformed error.
func (g *goFunc) malformed() { g.line("return %s", g.retWith("d.Malformed()")) }

// ifMalformed emits `if cond { return ..., d.Malformed() }`.
func (g *goFunc) ifMalformed(cond string) {
	g.line("if %s {", cond)
	g.malformed()
	g.line("}")
}
