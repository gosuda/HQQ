package gen

// The C server emitter turns an idl.Spec into <service>_server.h and <service>_server.c, the glue between
// the C runtime (c/include/hqq/hqq_rpc.h, spec/toolchain/BINDGEN.md 9.5) and the library (9.6).
//
// Contract
//
//   - Output is a pure function of the Spec and COptions.Source (only its base name is used). It has no
//     timestamps and no host paths; declarations follow the IDL order, and tables follow handle type ids
//     and opcodes. Indentation is 4 spaces. The first line of both files is the generated-code comment with
//     the interface digest, in the /* */ form.
//   - Wire layouts (offsets, sizes, padding, response bound terms) come from idl.Plan, member offsets from
//     idl.MemberOffsets. The emitter never re-derives them.
//   - Each wire function becomes `static hqqrpc_status op_<fn>(hqqrpc_call *)` that follows the server
//     order of 4.7.1: decode and validate the body with one fetch per field (step 3); look up handles and
//     reserve room for new ones (4); copy the str in-arguments that rely on their NUL and, for @intercept
//     functions, every str, bytes and array in-argument into scratch (5); reserve the response (6) with
//     RESP_BOUND in uint64_t; take the execution lock unless the service is @serialize(none), where the
//     lock calls are not emitted at all (7); call the interceptor (8); destroy descendants of a destroyed
//     handle (9); call the C function or shim (10); evaluate the error convention before any length check
//     (11); check produced lengths and @offset_in pointers, copy borrowed results and call @free (12);
//     release the lock (13); commit (14). Every return after the call releases the lock. A failure after
//     the call is HQQRPC_E_INTERNAL (4.6.6). Handles are registered under the lock, right after the call,
//     because a failed registration must destroy the object through its adapter, which calls the library.
//   - Results that are read from library memory (the detail string, borrowed outputs, returned str, bytes
//     and arrays, @free) are written before the lock is released; every other result is written after it.
//   - The first variable item of a response, when it is a caller-supplied out buffer, is written by the
//     library straight into the reserved response slot (wr_span, with the padding and the alignment of 4.5
//     inside the span); every other caller-supplied buffer goes through scratch. A buffer without a length
//     scalar is taken as produced in full (its capacity). A fixed out T[N] and a @c struct are zeroed and
//     written into G2 field by field, never as a C object, so C padding cannot reach the wire; a T[N] above
//     1024 bytes lives in scratch.
//   - On a failure body only `ret` is written; every other field of G2 stays zero (4.6.4). A produced handle
//     of a failed call is destroyed through its adapter after the detail string was read and is sent as 0.
//     A pointer that a borrowed out parameter or the return hands over for @free is freed on every path
//     once the call has returned; the local that receives it is non-const exactly when @free is present.
//     A NULL out str is the empty string, a NULL returned str is E_INTERNAL unless it is nullable or the
//     convention is @error(null).
//   - Ok values of @error(status) are written as numbers: IDL consts are not C macros.
//   - `size` in the interface is decoded through uint64_t and checked against size_t; encoding widens.
//     A derived length scalar that is narrower than the largest body is range-checked (E_BAD_REQUEST for an
//     in buffer, E_LIMIT for a capacity). @c overrides of scalars are _Static_assert-ed against the wire
//     size, and every @c struct that the glue touches against its size and member offsets.
//   - A bool inside an in array is validated to be 0 or 1 in place (the exposure of 7.2 applies).
//   - The service header declares the interceptor hooks, the shim prototypes and a writer per union case.
//     A hook or shim that is missing is a link error (7.3). A union case writer takes the payload when it is
//     a scalar, str, bytes, a scalar or @c struct array, a fixed array or a @c struct; for any other payload
//     it writes the tag only and the caller writes the payload with the hqqrpc_wr_* functions.
//   - A shim returns its value as the IDL return, except for a composite return: a variable composite gets
//     `hqqrpc_wr *ret` as its last parameter, a fixed struct `<ctype> *ret`, and the C return is then the
//     response status (D16). A fixed struct that appears in a shim prototype, in an out parameter or in a
//     returned array needs @c; C reports it as an error otherwise.
//   - Generated static names are op_<fn>, destroy_<Handle>, <service>_dispatch, <service>_handle_types,
//     <service>_op_names and the hqq_cstr_len helper; exported names begin with the service name. C reports
//     an IDL function that has one of these names. Locals take the IDL parameter names; a name that the glue
//     or the library uses is followed by "__", which no IDL name contains.
//   - The glue assumes a little-endian host, like the wire (SPEC 0). <service>_server.c does not contain
//     main: the consumer writes it with hqqrpc_main (9.6).

import (
	"encoding/hex"
	"fmt"
	"path"
	"regexp"
	"strings"

	"gosuda.org/hqq/bindgen/idl"
)

// C generates <service>_server.h and <service>_server.c for s.
func C(s *idl.Spec, o COptions) ([]File, error) {
	g, err := newCGen(s, o)
	if err != nil {
		return nil, err
	}
	hdr := g.header()
	src := g.source()
	return []File{
		{Name: s.Service + "_server.h", Data: []byte(hdr)},
		{Name: s.Service + "_server.c", Data: []byte(src)},
	}, nil
}

func newCGen(s *idl.Spec, o COptions) (*cgen, error) {
	g := &cgen{
		s:        s,
		src:      path.Base(strings.ReplaceAll(o.Source, "\\", "/")),
		svc:      s.Service,
		hmacro:   map[*idl.Handle]string{},
		reserved: map[string]bool{},
		children: map[*idl.Handle]bool{},
	}
	if o.Source == "" {
		g.src = s.Service + ".idl"
	}
	for _, n := range glueNames {
		g.reserved[n] = true
	}
	for _, f := range s.Funcs {
		g.reserved[f.Name] = true
		for _, t := range cIdents(f.RetAttrs.CType) {
			g.reserved[t] = true
		}
		for _, p := range f.Params {
			for _, t := range cIdents(p.Attrs.CType) {
				g.reserved[t] = true
			}
		}
	}
	for _, h := range s.Handles {
		for _, t := range cIdents(h.CType) {
			g.reserved[t] = true
		}
		if h.Parent != nil {
			g.children[h.Parent] = true
		}
	}
	for _, st := range s.Structs {
		for _, t := range cIdents(st.CType) {
			g.reserved[t] = true
		}
	}
	g.handleMacros()
	if err := g.check(); err != nil {
		return nil, err
	}
	return g, nil
}

// handleMacros names the handle type ids: <SERVICE>_H_<NAME>. Names that differ only in case get their id appended.
func (g *cgen) handleMacros() {
	count := map[string]int{}
	for _, h := range g.s.Handles {
		count[strings.ToUpper(h.Name)]++
	}
	for _, h := range g.s.Handles {
		name := fmt.Sprintf("%s_H_%s", strings.ToUpper(g.svc), strings.ToUpper(h.Name))
		if count[strings.ToUpper(h.Name)] > 1 {
			name += fmt.Sprintf("_%d", h.ID)
		}
		g.hmacro[h] = name
	}
}

// check reports a valid IDL that this emitter cannot express in C.
func (g *cgen) check() error {
	names := map[string]string{}
	claim := func(name, what string) error {
		if prev, ok := names[name]; ok {
			return fmt.Errorf("hqq-bindgen: generated C name %s of %s collides with %s", name, what, prev)
		}
		names[name] = what
		return nil
	}
	for _, f := range g.s.Funcs {
		if err := claim(f.Name, "IDL function "+f.Name); err != nil {
			return err
		}
	}
	for _, f := range g.s.Wire {
		if err := claim("op_"+f.Name, "the glue of "+f.Name); err != nil {
			return err
		}
	}
	for _, h := range g.s.Handles {
		if err := claim("destroy_"+h.Name, "the destroy adapter of "+h.Name); err != nil {
			return err
		}
	}
	for _, f := range g.s.Funcs {
		structNeedsC := func(t *idl.Type, what string) error {
			if t.Kind == idl.KStruct && t.St.CType == "" {
				return fmt.Errorf("hqq-bindgen: %s of %s uses struct %s, which needs @c(\"...\") so that C code can name it", what, f.Name, t.St.Name)
			}
			return nil
		}
		for _, p := range f.Params {
			t := p.Type
			switch {
			case p.Dir == idl.DirOut && t.Kind == idl.KStruct && f.Shim:
				if err := structNeedsC(t, "parameter "+p.Name); err != nil {
					return err
				}
			case p.Dir == idl.DirOut && t.Kind == idl.KFixedArray && t.Elem.Kind == idl.KFixedArray:
				return fmt.Errorf("hqq-bindgen: out parameter %s of %s is an array of arrays, which is not supported", p.Name, f.Name)
			case p.Dir == idl.DirOut && (t.Kind == idl.KArray || t.Kind == idl.KFixedArray):
				if err := g.checkElem(f, p.Name, t.Elem); err != nil {
					return err
				}
			}
		}
		if t := f.Ret; t != nil {
			if fixedStructRet(f) {
				if err := structNeedsC(t, "the return"); err != nil {
					return err
				}
			}
			if t.Kind == idl.KArray && !varWriterRet(f) {
				if err := g.checkElem(f, "the return", t.Elem); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func (g *cgen) checkElem(f *idl.Func, what string, e *idl.Type) error {
	switch {
	case e.Kind == idl.KStruct && e.St.CType == "":
		return fmt.Errorf("hqq-bindgen: %s of %s is an array of struct %s, which needs @c(\"...\")", what, f.Name, e.St.Name)
	case e.Kind == idl.KFixedArray:
		return fmt.Errorf("hqq-bindgen: %s of %s is an array of arrays, which is not supported", what, f.Name)
	}
	return nil
}

func (g *cgen) banner() string {
	d := g.s.Digest()
	return fmt.Sprintf("/* Code generated by hqq-bindgen from %s (interface digest %s). DO NOT EDIT. */", sanitizeComment(g.src), hex.EncodeToString(d[:]))
}

var boolWord = regexp.MustCompile(`\bbool\b`)

// ---- header ----------------------------------------------------------------------------------------

func (g *cgen) header() string {
	var body code
	macros := g.s.Handles
	for _, h := range macros {
		body.l("#define %s %du", g.hmacro[h], h.ID)
	}
	if len(macros) > 0 {
		// the comment of the first macro, as in 9.6
		body.lines[0] += "              /* handle type ids = indices into " + g.svc + "_iface.handle_types, 1-based */"
		body.l("")
	}
	body.l("extern const hqqrpc_iface %s_iface;", g.svc)

	var hooks []*idl.Func
	var shims []*idl.Func
	for _, f := range g.s.Wire {
		if f.Intercept {
			hooks = append(hooks, f)
		}
	}
	for _, f := range g.s.Funcs {
		if f.Shim {
			shims = append(shims, f)
		}
	}
	if len(hooks) > 0 {
		body.l("")
		body.l("/* Interceptors, required: each function below is declared @intercept (7.3); a missing definition is a")
		body.l(" * link error. The str, bytes and array members of the args point into private scratch (16-byte aligned);")
		body.l(" * the library call receives the members as the hook leaves them. */")
		for _, f := range hooks {
			g.hookDecl(&body, f)
		}
	}
	if len(shims) > 0 {
		body.l("")
		body.l("/* Shims, required. A shim with a composite return gets the writer as its last parameter and returns")
		body.l(" * a response status (HQQRPC_OK, or an application status in 0x0100..0xFFFF; anything else becomes")
		body.l(" * E_INTERNAL, 4.6.6). */")
		for _, f := range shims {
			body.l("%s", protoText(f))
		}
	}
	protos, _ := g.unionWriters()
	if len(protos) > 0 {
		body.l("")
		body.l("/* Writers for the unions of the IDL (4.5), one per case. A case whose payload has no scalar form writes the")
		body.l(" * tag only; the caller then writes the payload with the hqqrpc_wr_* functions. */")
		for _, p := range protos {
			body.l("%s", p)
		}
	}

	text := joinLines(body.lines)
	var out code
	guard := strings.ToUpper(g.svc) + "_SERVER_H"
	out.l("%s", g.banner())
	out.l("#ifndef %s", guard)
	out.l("#define %s", guard)
	out.l("")
	g.hdrBool = boolWord.MatchString(text)
	if g.hdrBool {
		out.l("#include <stdbool.h>")
	}
	out.l("#include \"hqq/hqq_rpc.h\"")
	for _, inc := range g.s.Includes {
		out.l("#include \"%s\"", inc)
	}
	out.l("")
	return joinLines(out.lines) + text + "\n#endif\n"
}

func (g *cgen) hookDecl(c *code, f *idl.Func) {
	o := newOp(g, f)
	o.analyze()
	name := g.argStructName(f)
	c.l("typedef struct %s {", name)
	c.in()
	if len(o.hookArgs) == 0 {
		c.l("char hqq_unused; /* the function has no in parameter */")
	}
	for _, m := range o.hookArgs {
		c.l("%s;", declText(m.ctype, m.name))
	}
	c.out()
	c.l("} %s;", name)
	c.l("hqqrpc_status %s(hqqrpc_session *s, %s *args);", g.hookName(f), name)
}

// ---- source ----------------------------------------------------------------------------------------

func (g *cgen) source() string {
	var ops []string
	for _, f := range g.s.Wire {
		ops = append(ops, newOp(g, f).emit()...)
		ops = append(ops, "")
	}

	var c code
	c.l("%s", g.banner())
	text := ""
	c.l("#include <string.h>")
	c.l("#include \"%s_server.h\"", g.svc)
	c.l("")
	if asserts := g.staticAsserts(); len(asserts) > 0 {
		for _, a := range asserts {
			c.l("%s", a)
		}
		c.l("")
	}
	if g.cstrLen {
		c.l("/* The length of the NUL-terminated string s, at most max bytes; 0 if there is no NUL within max + 1 bytes. */")
		c.l("static int hqq_cstr_len(const char *s, uint32_t max, uint32_t *n) {")
		c.l("    const char *z = (const char *)memchr(s, 0, (size_t)max + 1u);")
		c.l("")
		c.l("    if (z == NULL)")
		c.l("        return 0;")
		c.l("    *n = (uint32_t)(z - s);")
		c.l("    return 1;")
		c.l("}")
		c.l("")
	}
	for _, h := range g.s.Handles {
		c.l("%s", g.adapter(h))
	}
	if len(g.s.Handles) > 0 {
		c.l("")
	}
	c.lines = append(c.lines, ops...)

	c.l("static hqqrpc_status %s_dispatch(hqqrpc_call *c) {", g.svc)
	c.l("    switch (c->op) {")
	for _, f := range g.s.Wire {
		c.l("    case %d: return op_%s(c);", f.Opcode, f.Name)
	}
	c.l("    default: return HQQRPC_E_UNKNOWN_OP; /* default deny (7.1) */")
	c.l("    }")
	c.l("}")
	c.l("")

	if len(g.s.Handles) > 0 {
		c.l("static const hqqrpc_handle_type %s_handle_types[] = {", g.svc)
		width := 0
		var rows []string
		for _, h := range g.s.Handles {
			parent := "0"
			if h.Parent != nil {
				parent = g.hmacro[h.Parent]
			}
			row := fmt.Sprintf("{ \"%s\", %s, destroy_%s },", h.Name, parent, h.Name)
			rows = append(rows, row)
			width = max(width, len(row))
		}
		for i, h := range g.s.Handles {
			note := g.hmacro[h]
			if h.Parent != nil {
				note += ", owned_by " + h.Parent.Name
			}
			c.l("    %s%s /* %s */", rows[i], strings.Repeat(" ", width-len(rows[i])), note)
		}
		c.l("};")
		c.l("")
	}
	if len(g.s.Wire) > 0 {
		c.l("static const char *const %s_op_names[] = {", g.svc)
		line := "   "
		for _, f := range g.s.Wire {
			item := fmt.Sprintf(" \"%s\",", f.Name)
			if len(line)+len(item) > 100 {
				c.l("%s", line)
				line = "   "
			}
			line += item
		}
		c.l("%s", line)
		c.l("};")
		c.l("")
	}

	d := g.s.Digest()
	c.l("const hqqrpc_iface %s_iface = {", g.svc)
	c.l("    \"%s\",", g.svc)
	c.l("    {")
	for i := 0; i < len(d); i += 8 {
		var b []string
		for _, x := range d[i : i+8] {
			b = append(b, fmt.Sprintf("0x%02x", x))
		}
		c.l("        %s,", strings.Join(b, ", "))
	}
	c.l("    },")
	names, handles := "NULL", "NULL"
	if len(g.s.Wire) > 0 {
		names = g.svc + "_op_names"
	}
	if len(g.s.Handles) > 0 {
		handles = g.svc + "_handle_types"
	}
	c.l("    %d, %s,", len(g.s.Wire), names)
	c.l("    %d, %s,", len(g.s.Handles), handles)
	c.l("    %s_dispatch,", g.svc)
	ser := "HQQRPC_SERIALIZE_SESSION"
	if g.s.Serialize == idl.SerializeNone {
		ser = "HQQRPC_SERIALIZE_NONE"
	}
	c.l("    %s,", ser)
	c.l("};")

	_, defs := g.unionWriters()
	if len(defs) > 0 {
		c.l("")
		c.lines = append(c.lines, defs...)
	}

	text = joinLines(c.lines)
	if boolWord.MatchString(text) && !g.hdrBool {
		// stdbool.h is not in the header: add it before the project header
		text = strings.Replace(text, "#include <string.h>\n", "#include <stdbool.h>\n#include <string.h>\n", 1)
	}
	return text
}

// adapter is the destroy adapter of a handle type: 0 if the object was released without error (5.1).
func (g *cgen) adapter(h *idl.Handle) string {
	f := h.Destroy
	ct := handleCType(h)
	call := fmt.Sprintf("%s((%s)p)", f.Name, ct)
	head := fmt.Sprintf("static int destroy_%s(void *p) {", h.Name)
	switch {
	case f.Ret == nil:
		return fmt.Sprintf("%s %s; return 0; }", head, call)
	case len(f.Error.Ok) == 1:
		return fmt.Sprintf("%s return %s != %d; }", head, call, f.Error.Ok[0])
	}
	var fails []string
	for _, v := range f.Error.Ok {
		fails = append(fails, fmt.Sprintf("ret != %d", v))
	}
	return fmt.Sprintf("%s\n    %s ret = %s;\n\n    return %s;\n}", head, valueC(f.Ret, f.RetAttrs), call, strings.Join(fails, " && "))
}

// ---- static assertions ---------------------------------------------------------------------------------

// staticAsserts checks at compile time that the C types named by @c have the size and layout of the wire
// form (3.1): scalar overrides such as @c("long"), and the offsets of every @c struct that the glue encodes.
func (g *cgen) staticAsserts() []string {
	var out []string
	seen := map[string]bool{}
	scalar := func(at idl.Attrs, t *idl.Type) {
		if at.CType == "" || !t.Kind.IsScalar() {
			return
		}
		key := fmt.Sprintf("%s/%d", at.CType, sizeT(t))
		if seen[key] {
			return
		}
		seen[key] = true
		out = append(out, fmt.Sprintf("_Static_assert(sizeof(%s) == %d, \"hqq-bindgen: @c(\\\"%s\\\") must have the size of %s\");", at.CType, sizeT(t), at.CType, t.Kind))
	}
	used := map[*idl.Struct]bool{}
	var walk func(t *idl.Type)
	walk = func(t *idl.Type) {
		switch t.Kind {
		case idl.KStruct:
			used[t.St] = true
		case idl.KArray, idl.KFixedArray:
			walk(t.Elem)
		}
	}
	for _, f := range g.s.Funcs {
		for _, p := range f.Params {
			scalar(p.Attrs, p.Type)
			walk(p.Type)
		}
		if f.Ret != nil {
			scalar(f.RetAttrs, f.Ret)
			walk(f.Ret)
		}
	}
	for _, u := range g.s.Unions {
		for _, cs := range u.Cases {
			if cs.Type != nil {
				walk(cs.Type)
			}
		}
	}
	for _, st := range g.s.Structs {
		if !used[st] || st.CType == "" || !isFixedT(&idl.Type{Kind: idl.KStruct, Name: st.Name, St: st}) {
			continue
		}
		t := &idl.Type{Kind: idl.KStruct, Name: st.Name, St: st}
		out = append(out, fmt.Sprintf("_Static_assert(sizeof(%s) == %d, \"hqq-bindgen: struct %s: size\");", st.CType, sizeT(t), st.Name))
		offs := idl.MemberOffsets(st)
		for i, m := range st.Members {
			out = append(out, fmt.Sprintf("_Static_assert(offsetof(%s, %s) == %d, \"hqq-bindgen: struct %s: offset of %s\");", st.CType, m.Name, offs[i], st.Name, m.Name))
		}
	}
	return out
}

// ---- union writers ------------------------------------------------------------------------------------

// unionWriters returns the prototypes (for the header) and the definitions (for the source) of the case writers.
func (g *cgen) unionWriters() (protos, defs []string) {
	for _, u := range g.s.Unions {
		defs = append(defs, fmt.Sprintf("/* Writers for union %s: tag u32 (aligned 4), then the payload. */", u.Name))
		for _, cs := range u.Cases {
			p, d := g.caseWriter(u, cs)
			protos = append(protos, p)
			defs = append(defs, d...)
		}
	}
	return protos, defs
}

func (g *cgen) caseWriter(u *idl.Union, cs *idl.Case) (proto string, def []string) {
	name := fmt.Sprintf("%s_%s_put_%s", g.svc, u.Name, cs.Name)
	tag := fmt.Sprintf("%du", cs.Tag)
	t := cs.Type
	head := func(params string) string {
		return fmt.Sprintf("hqqrpc_status %s(hqqrpc_wr *w%s)", name, params)
	}
	if t == nil {
		return head("") + ";", []string{head("") + " { return hqqrpc_wr_put_u32(w, " + tag + "); }"}
	}

	// wg collects the locals that the shared store helper declares
	wg := &opGen{c: g, nm: newNamer(g.reserved), glue: map[string]string{}}
	for _, n := range []string{"w", "v", "p", "s", "n", "st", "o", "mark", "i"} {
		wg.nm.used[n] = true
	}
	var body code
	var params string
	var extra []decl
	fail := func(cond, ret string) {
		body.l("if (%s)", cond)
		body.in()
		body.l("return %s;", ret)
		body.out()
	}
	finish := func(inline bool) (string, []string) {
		var out code
		out.l("%s {", head(params))
		out.in()
		out.l("hqqrpc_status st = hqqrpc_wr_put_u32(w, %s);", tag)
		if len(extra) > 0 || len(wg.decls) > 0 {
			for _, d := range append(extra, wg.decls...) {
				out.l("%s;", declText(d.typ, d.name))
			}
			out.l("")
		}
		out.lines = append(out.lines, body.lines...)
		out.out()
		out.l("}")
		return head(params) + ";", out.lines
	}
	switch {
	case t.Kind == idl.KI32 || t.Kind == idl.KU32 || t.Kind == idl.KI64 || t.Kind == idl.KU64:
		params = ", " + declText(scalarCType[t.Kind], "v")
		put, conv := "hqqrpc_wr_put_u32", "uint32_t"
		if sizeT(t) == 8 {
			put, conv = "hqqrpc_wr_put_u64", "uint64_t"
		}
		body.in()
		body.l("return st != HQQRPC_OK ? st : %s(w, (%s)v);", put, conv)
		return finish(true)
	case t.Kind.IsScalar():
		params = ", " + declText(scalarCType[t.Kind], "v")
		extra = append(extra, decl{typ: "uint8_t *", name: "o"})
		body.in()
		fail("st != HQQRPC_OK", "st")
		fail(fmt.Sprintf("(o = hqqrpc_wr_fixed(w, %d, %d)) == NULL", alignT(t), sizeT(t)), "w->err")
		wg.store(&body, "o", 0, t, "v", 0)
		body.l("return HQQRPC_OK;")
		return finish(false)
	case t.Kind == idl.KStr:
		params = ", const char *s, uint32_t n"
		body.in()
		body.l("return st != HQQRPC_OK ? st : hqqrpc_wr_put_str(w, s, n, %s);", boundOr(t))
		return finish(true)
	case t.Kind == idl.KBytes:
		params = ", const void *p, uint32_t n"
		body.in()
		body.l("return st != HQQRPC_OK ? st : hqqrpc_wr_put_bytes(w, p, n, %s);", boundOr(t))
		return finish(true)
	case t.Kind == idl.KArray && arrayElemOK(t.Elem):
		params = ", " + declText("const "+elemC(t.Elem)+" *", "p") + ", uint32_t n"
		extra = append(extra, decl{typ: "size_t", name: "mark"}, decl{typ: "uint8_t *", name: "o"})
		stride := idl.Stride(t.Elem)
		body.in()
		fail("st != HQQRPC_OK || (st = hqqrpc_wr_array_begin(w, &mark)) != HQQRPC_OK", "st")
		body.block("if (n != 0)", func() {
			size := fmt.Sprintf("(size_t)n * %d", stride)
			if stride == 1 {
				size = "(size_t)n"
			}
			fail(fmt.Sprintf("(o = hqqrpc_wr_fixed(w, %d, %s)) == NULL", alignT(t.Elem), size), "w->err")
			if t.Elem.Kind.IsScalar() && t.Elem.Kind != idl.KSize {
				body.l("memcpy(o, p, %s);", size)
				return
			}
			extra = append(extra, decl{typ: "uint32_t", name: "i"})
			wg.loop(&body, "i", "n", func() {
				wg.store(&body, "(o + i * "+fmt.Sprint(stride)+")", 0, t.Elem, "p[i]", 1)
			})
		})
		body.l("return hqqrpc_wr_array_end(w, mark, n, %s);", boundOr(t))
		return finish(false)
	case t.Kind == idl.KFixedArray && arrayElemOK(t.Elem):
		params = ", " + declText("const "+elemC(t.Elem)+" *", "p")
		extra = append(extra, decl{typ: "uint8_t *", name: "o"})
		body.in()
		fail("st != HQQRPC_OK", "st")
		fail(fmt.Sprintf("(o = hqqrpc_wr_fixed(w, %d, %d)) == NULL", alignT(t), sizeT(t)), "w->err")
		wg.store(&body, "o", 0, t, "p", 0)
		body.l("return HQQRPC_OK;")
		return finish(false)
	case t.Kind == idl.KStruct && t.St.CType != "" && isFixedT(t):
		params = ", " + declText("const "+t.St.CType+" *", "v")
		extra = append(extra, decl{typ: "uint8_t *", name: "o"})
		body.in()
		fail("st != HQQRPC_OK", "st")
		fail(fmt.Sprintf("(o = hqqrpc_wr_fixed(w, %d, %d)) == NULL", alignT(t), sizeT(t)), "w->err")
		wg.store(&body, "o", 0, t, "(*v)", 0)
		body.l("return HQQRPC_OK;")
		return finish(false)
	}
	// no scalar form: the tag only
	return head("") + ";", []string{head("") + " { return hqqrpc_wr_put_u32(w, " + tag + "); }"}
}

func boundOr(t *idl.Type) string {
	if t.Max > 0 {
		return fmt.Sprintf("%du", t.Max)
	}
	return "UINT32_MAX"
}

// arrayElemOK reports whether an array element has a C type the writer can index.
func arrayElemOK(e *idl.Type) bool {
	switch {
	case e.Kind.IsScalar():
		return true
	case e.Kind == idl.KStruct:
		return e.St.CType != "" && isFixedT(e)
	}
	return false
}
