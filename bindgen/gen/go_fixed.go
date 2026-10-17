package gen

import (
	"fmt"
	"strconv"

	"gosuda.org/hqq/bindgen/idl"
)

// goPos is a byte position in a body: an optional variable part plus a constant. String renders it as the
// Go expression that indexes the body.
type goPos struct {
	expr string
	k    int64
}

func (p goPos) String() string {
	switch {
	case p.expr == "":
		return strconv.FormatInt(p.k, 10)
	case p.k == 0:
		return p.expr
	}
	return p.expr + "+" + strconv.FormatInt(p.k, 10)
}

func (p goPos) add(k int64) goPos { return goPos{p.expr, p.k + k} }

// idx is the position of element i of an array of the given stride that starts at p.
func (p goPos) idx(i string, stride int64) goPos {
	if stride == 1 {
		return goPos{p.String() + "+" + i, 0}
	}
	return goPos{p.String() + "+" + i + "*" + strconv.FormatInt(stride, 10), 0}
}

// goMember is a field of a fixed struct at its wire offset.
type goMember struct {
	field string
	t     *idl.Type
	off   int64
}

// goStructLayout places the members of a fixed struct like a C struct (4.5) and reports whether the struct
// has padding bytes, which a writer must zero.
func goStructLayout(st *idl.Struct) (members []goMember, padded bool) {
	var off, sum int64
	for _, m := range st.Members {
		size, align, _ := idl.SizeOf(m.Type)
		if align > 1 {
			off = (off + align - 1) / align * align
		}
		members = append(members, goMember{goCapitalize(m.Name), m.Type, off})
		off += size
		sum += size
	}
	total, _, _ := idl.SizeOf(&idl.Type{Kind: idl.KStruct, St: st})
	return members, total != sum
}

// scalarToWire converts a Go scalar value to the unsigned integer that the Put helper of its width takes.
func (e *goEmitter) scalarToWire(k idl.Kind, val string) (put, conv string) {
	switch k {
	case idl.KU16:
		return "PutUint16", val
	case idl.KI16:
		return "PutUint16", "uint16(" + val + ")"
	case idl.KU32:
		return "PutUint32", val
	case idl.KI32:
		return "PutUint32", "uint32(" + val + ")"
	case idl.KF32:
		e.math = true
		return "PutUint32", "math.Float32bits(" + val + ")"
	case idl.KU64, idl.KSize:
		return "PutUint64", val
	case idl.KI64:
		return "PutUint64", "uint64(" + val + ")"
	case idl.KF64:
		e.math = true
		return "PutUint64", "math.Float64bits(" + val + ")"
	}
	panic(fmt.Sprintf("gen: not a multi-byte scalar kind %v", k))
}

// store emits the statements that write the fixed value val of type t at pos of body. It writes every
// byte of the value, padding included, because the request slot is not zeroed.
func (g *goFunc) store(t *idl.Type, val string, pos goPos) {
	switch t.Kind {
	case idl.KBool:
		g.line("body[%s] = 0", pos)
		g.line("if %s {", val)
		g.line("body[%s] = 1", pos)
		g.line("}")
	case idl.KU8:
		g.line("body[%s] = %s", pos, val)
	case idl.KI8:
		g.line("body[%s] = uint8(%s)", pos, val)
	case idl.KHandle:
		g.line("hqqLE.PutUint32(body[%s:], %s.ID())", pos, val)
	case idl.KU16, idl.KI16, idl.KU32, idl.KI32, idl.KF32, idl.KU64, idl.KI64, idl.KSize, idl.KF64:
		put, conv := g.e.scalarToWire(t.Kind, val)
		g.line("hqqLE.%s(body[%s:], %s)", put, pos, conv)
	case idl.KFixedArray:
		g.storeArray(t, val, pos)
	case idl.KStruct:
		members, padded := goStructLayout(t.St)
		if size, _, _ := idl.SizeOf(t); padded {
			g.line("clear(body[%s:%s])", pos, pos.add(size))
		}
		for _, m := range members {
			g.store(m.t, val+"."+m.field, pos.add(m.off))
		}
	default:
		panic(fmt.Sprintf("gen: %s is not a fixed value", t))
	}
}

func (g *goFunc) storeArray(t *idl.Type, val string, pos goPos) {
	if t.Elem.Kind == idl.KU8 {
		g.line("copy(body[%s:%s], %s[:])", pos, pos.add(t.Len), val)
		return
	}
	stride := idl.Stride(t.Elem)
	i := g.alloc("i")
	g.line("for %s := range %s {", i, val)
	g.store(t.Elem, val+"["+i+"]", pos.idx(i, stride))
	g.line("}")
}

// loadExpr is the Go expression that reads the scalar or handle t at pos of the response body. A bool is
// compared with 1; the caller checks that it is 0 or 1 (boolCheck).
func (g *goFunc) loadExpr(t *idl.Type, pos goPos) string {
	b := g.bv
	switch t.Kind {
	case idl.KBool:
		return fmt.Sprintf("%s[%s] == 1", b, pos)
	case idl.KU8:
		return fmt.Sprintf("%s[%s]", b, pos)
	case idl.KI8:
		return fmt.Sprintf("int8(%s[%s])", b, pos)
	case idl.KU16:
		return fmt.Sprintf("hqqLE.Uint16(%s[%s:])", b, pos)
	case idl.KI16:
		return fmt.Sprintf("int16(hqqLE.Uint16(%s[%s:]))", b, pos)
	case idl.KU32:
		return fmt.Sprintf("hqqLE.Uint32(%s[%s:])", b, pos)
	case idl.KI32:
		return fmt.Sprintf("int32(hqqLE.Uint32(%s[%s:]))", b, pos)
	case idl.KF32:
		g.e.math = true
		return fmt.Sprintf("math.Float32frombits(hqqLE.Uint32(%s[%s:]))", b, pos)
	case idl.KU64, idl.KSize:
		return fmt.Sprintf("hqqLE.Uint64(%s[%s:])", b, pos)
	case idl.KI64:
		return fmt.Sprintf("int64(hqqLE.Uint64(%s[%s:]))", b, pos)
	case idl.KF64:
		g.e.math = true
		return fmt.Sprintf("math.Float64frombits(hqqLE.Uint64(%s[%s:]))", b, pos)
	case idl.KHandle:
		return fmt.Sprintf("%s{f.c.c.NewHandle(hqqLE.Uint32(%s[%s:]))}", goCapitalize(t.Name), b, pos)
	}
	panic(fmt.Sprintf("gen: %s is not a scalar", t))
}

// boolCheck is the condition that is true when the byte at pos is not 0 or 1.
func (g *goFunc) boolCheck(pos goPos) string { return fmt.Sprintf("%s[%s] > 1", g.bv, pos) }

// load emits the statements that read the fixed value of type t at pos of the response body into target.
// The caller has checked that the bytes exist.
func (g *goFunc) load(t *idl.Type, target string, pos goPos) {
	switch t.Kind {
	case idl.KBool:
		g.ifMalformed(g.boolCheck(pos))
		g.line("%s = %s", target, g.loadExpr(t, pos))
	case idl.KFixedArray:
		if t.Elem.Kind == idl.KU8 {
			g.line("copy(%s[:], %s[%s:%s])", target, g.bv, pos, pos.add(t.Len))
			return
		}
		i := g.alloc("i")
		g.line("for %s := range %s {", i, target)
		g.load(t.Elem, target+"["+i+"]", pos.idx(i, idl.Stride(t.Elem)))
		g.line("}")
	case idl.KStruct:
		members, _ := goStructLayout(t.St)
		for _, m := range members {
			g.load(m.t, target+"."+m.field, pos.add(m.off))
		}
	default:
		g.line("%s = %s", target, g.loadExpr(t, pos))
	}
}
