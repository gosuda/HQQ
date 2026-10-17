package gen

import (
	"fmt"
	"sort"
	"strings"

	"gosuda.org/hqq/bindgen/idl"
)

// goScalar is the Go type of a scalar kind. size is uint64 on the wire and in Go.
func goScalar(k idl.Kind) string {
	switch k {
	case idl.KBool:
		return "bool"
	case idl.KI8:
		return "int8"
	case idl.KI16:
		return "int16"
	case idl.KI32:
		return "int32"
	case idl.KI64:
		return "int64"
	case idl.KU8:
		return "uint8"
	case idl.KU16:
		return "uint16"
	case idl.KU32:
		return "uint32"
	case idl.KU64, idl.KSize:
		return "uint64"
	case idl.KF32:
		return "float32"
	case idl.KF64:
		return "float64"
	}
	return ""
}

// goType is the Go type of an IDL type. nullable selects the nullable form of a str (*string).
func goType(t *idl.Type, nullable bool) string {
	switch t.Kind {
	case idl.KStr:
		if nullable {
			return "*string"
		}
		return "string"
	case idl.KBytes:
		return "[]byte"
	case idl.KStruct, idl.KUnion, idl.KHandle:
		return goCapitalize(t.Name)
	case idl.KFixedArray:
		return fmt.Sprintf("[%d]%s", t.Len, goType(t.Elem, false))
	case idl.KArray:
		return "[]" + goType(t.Elem, false)
	}
	return goScalar(t.Kind)
}

// goZero is the Go expression of the zero value of goType(t, nullable).
func goZero(t *idl.Type, nullable bool) string {
	switch t.Kind {
	case idl.KBool:
		return "false"
	case idl.KStr:
		if nullable {
			return "nil"
		}
		return `""`
	case idl.KBytes, idl.KArray:
		return "nil"
	case idl.KStruct, idl.KUnion, idl.KHandle, idl.KFixedArray:
		return goType(t, false) + "{}"
	}
	return "0"
}

// goDecl is one top-level type declaration, ordered by position in the IDL.
type goDecl struct {
	pos  idl.Pos
	emit func(*goEmitter)
}

func (e *goEmitter) emitConsts() {
	if len(e.spec.Consts) == 0 {
		return
	}
	e.out.line("const (")
	for _, k := range e.spec.Consts {
		e.out.line("%s %s = %s", goCapitalize(k.Name), goScalar(k.Type.Kind), k.Value.String())
	}
	e.out.line(")")
	e.out.blank()
}

func (e *goEmitter) emitOpcodes() {
	if len(e.spec.Wire) == 0 {
		return
	}
	e.out.line("const (")
	for i, f := range e.spec.Wire {
		if i == 0 {
			e.out.line("hqqOp%s uint16 = iota + 1", e.names[f])
			continue
		}
		e.out.line("hqqOp%s", e.names[f])
	}
	e.out.line(")")
	e.out.blank()
}

// goOpsWidth is the column limit at which the opcode name list of the rt.Iface wraps; a tab counts as four.
const goOpsWidth = 100

func (e *goEmitter) emitIface() {
	digest := e.spec.Digest()
	e.out.line("var hqqIface = &rt.Iface{")
	e.out.line("Service: %q,", e.spec.Service)
	e.out.line("Digest: [32]byte{")
	for i := 0; i < len(digest); i += 8 {
		parts := make([]string, 8)
		for j := range parts {
			parts[j] = fmt.Sprintf("0x%02x", digest[i+j])
		}
		e.out.line("%s,", strings.Join(parts, ", "))
	}
	e.out.line("},")
	e.out.line("%s", goOpsList(e.spec.Wire))
	e.out.line("}")
	e.out.blank()
}

// goOpsList renders `Ops: []string{...},` wrapped at goOpsWidth columns.
func goOpsList(wire []*idl.Func) string {
	var lines []string
	cur, indent := "Ops: []string{", 4
	for i, f := range wire {
		item := fmt.Sprintf("%q", f.Name)
		if i < len(wire)-1 {
			item += ","
		}
		switch {
		case i == 0:
			cur += item
		case indent+len(cur)+1+len(item) > goOpsWidth:
			lines = append(lines, cur)
			cur, indent = item, 8
		default:
			cur += " " + item
		}
	}
	lines = append(lines, cur+"},")
	return strings.Join(lines, "\n")
}

func (e *goEmitter) emitHandles() {
	if len(e.spec.Handles) == 0 {
		return
	}
	e.out.line("// Handles. The zero value is the nil handle.")
	for _, h := range e.spec.Handles {
		e.out.line("type %s struct{ rt.Handle }", goCapitalize(h.Name))
	}
	e.out.blank()
}

// emitTypes writes the structs and unions in IDL declaration order.
func (e *goEmitter) emitTypes() {
	var decls []goDecl
	for _, st := range e.spec.Structs {
		decls = append(decls, goDecl{st.Pos, func(e *goEmitter) { e.emitStruct(st) }})
	}
	for _, u := range e.spec.Unions {
		decls = append(decls, goDecl{u.Pos, func(e *goEmitter) { e.emitUnion(u) }})
	}
	sort.SliceStable(decls, func(i, j int) bool {
		a, b := decls[i].pos, decls[j].pos
		return a.Line < b.Line || a.Line == b.Line && a.Col < b.Col
	})
	for _, d := range decls {
		d.emit(e)
		e.out.blank()
	}
}

func (e *goEmitter) emitStruct(st *idl.Struct) {
	e.out.line("type %s struct {", goCapitalize(st.Name))
	for _, m := range st.Members {
		e.out.line("%s %s", goCapitalize(m.Name), goType(m.Type, false))
	}
	e.out.line("}")
}

func (e *goEmitter) emitUnion(u *idl.Union) {
	name := goCapitalize(u.Name)
	e.out.line("// %s is the union %s: Tag selects the field that is set.", name, u.Name)
	e.out.line("type %sTag uint32", name)
	e.out.blank()
	e.out.line("const (")
	for _, c := range u.Cases {
		e.out.line("%s%s %sTag = %d", name, goCapitalize(c.Name), name, c.Tag)
	}
	e.out.line(")")
	e.out.blank()
	e.out.line("type %s struct {", name)
	e.out.line("Tag %sTag", name)
	for _, c := range u.Cases {
		if c.Type == nil {
			continue
		}
		e.out.line("%s %s // %s%s%s", goCapitalize(c.Name), goType(c.Type, false), name, goCapitalize(c.Name), goBoundNote(c.Type))
	}
	e.out.line("}")
}

// goBoundNote is the ", at most N ..." suffix of a field comment for a bounded str, bytes or array.
func goBoundNote(t *idl.Type) string {
	switch {
	case t.Max == 0:
		return ""
	case t.Kind == idl.KArray:
		return fmt.Sprintf(", at most %d elements", t.Max)
	case t.Kind == idl.KStr || t.Kind == idl.KBytes:
		return fmt.Sprintf(", at most %d bytes", t.Max)
	}
	return ""
}

func (e *goEmitter) emitClient() {
	e.out.line("// Client is one session of the service. Not safe for concurrent use; see rt.Pool.")
	e.out.line("type Client struct{ c *rt.Conn }")
	e.out.blank()
	e.out.line("// Dial connects to the service listening on listener and checks the interface digest.")
	e.out.line("func Dial(ctx context.Context, listener string, opts rt.Options) (*Client, error) {")
	e.out.line("c, err := rt.Dial(ctx, listener, hqqIface, opts)")
	e.out.line("if err != nil {")
	e.out.line("return nil, err")
	e.out.line("}")
	e.out.line("return &Client{c: c}, nil")
	e.out.line("}")
	e.out.blank()
	e.out.line("func (c *Client) Close() error { return c.c.Close() }")
	e.out.line("func (c *Client) Kill() { c.c.Kill() }")
	e.out.line("func (c *Client) Conn() *rt.Conn { return c.c }")
	e.out.blank()
}
