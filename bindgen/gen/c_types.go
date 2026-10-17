package gen

import (
	"fmt"
	"strings"

	"gosuda.org/hqq/bindgen/idl"
)

// cgen is the state shared by every part of one C emission.
type cgen struct {
	s      *idl.Spec
	src    string
	svc    string // service name, the prefix of generated names
	hmacro map[*idl.Handle]string
	// reserved holds every identifier that a generated local must not take: function names, the words of
	// the @c strings and the C names the glue itself uses.
	reserved map[string]bool
	// children[h] is true when some handle type is owned by h.
	children map[*idl.Handle]bool
	cstrLen  bool // some function needs hqq_cstr_len
	hdrBool  bool // the header includes <stdbool.h>
}

var scalarCType = map[idl.Kind]string{
	idl.KBool: "bool", idl.KI8: "int8_t", idl.KI16: "int16_t", idl.KI32: "int32_t", idl.KI64: "int64_t",
	idl.KU8: "uint8_t", idl.KU16: "uint16_t", idl.KU32: "uint32_t", idl.KU64: "uint64_t",
	idl.KSize: "size_t", idl.KF32: "float", idl.KF64: "double",
}

// glueNames are the C names that generated code uses and that IDL-derived locals must not shadow.
var glueNames = []string{
	"memcpy", "memset", "memchr", "NULL", "offsetof", "size_t", "uintptr_t", "bool", "true", "false",
	"int8_t", "int16_t", "int32_t", "int64_t", "uint8_t", "uint16_t", "uint32_t", "uint64_t",
	"float", "double", "void", "char", "int", "INT32_MAX", "UINT32_MAX",
	"c", "s", "r", "w", "st", "ret",
}

func handleCType(h *idl.Handle) string {
	if h.CType != "" {
		return h.CType
	}
	return "void *"
}

// valueC is the C type of a scalar, handle or fixed struct value, honoring @c on the parameter or return.
func valueC(t *idl.Type, at idl.Attrs) string {
	if at.CType != "" {
		return at.CType
	}
	switch t.Kind {
	case idl.KHandle:
		return handleCType(t.H)
	case idl.KStruct:
		return t.St.CType
	}
	return scalarCType[t.Kind]
}

// elemC is the C element type of an array, or "void" if the IDL names none.
func elemC(t *idl.Type) string {
	switch t.Kind {
	case idl.KStruct:
		if t.St.CType != "" {
			return t.St.CType
		}
		return "void"
	case idl.KFixedArray, idl.KArray, idl.KStr, idl.KBytes, idl.KUnion, idl.KHandle:
		return "void"
	}
	return scalarCType[t.Kind]
}

// inPtrC is the C type under which glue and hooks see an in buffer.
func inPtrC(t *idl.Type) string {
	switch t.Kind {
	case idl.KStr:
		return "const char *"
	case idl.KBytes:
		return "const void *"
	case idl.KArray, idl.KFixedArray:
		return "const " + elemC(t.Elem) + " *"
	}
	return "const void *"
}

func isFixedT(t *idl.Type) bool {
	_, _, fixed := idl.SizeOf(t)
	return fixed
}

func sizeT(t *idl.Type) int64 {
	size, _, _ := idl.SizeOf(t)
	return size
}

func alignT(t *idl.Type) int64 {
	_, align, _ := idl.SizeOf(t)
	return align
}

// isVarBuf reports whether t is a str, bytes or variable array.
func isVarBuf(t *idl.Type) bool {
	return t.Kind == idl.KStr || t.Kind == idl.KBytes || t.Kind == idl.KArray
}

// callerSupplied reports whether p is an out buffer whose storage the caller provides (4.6.2).
func callerSupplied(p *idl.Param) bool {
	return p.Dir == idl.DirOut && (p.Type.Kind == idl.KBytes || p.Type.Kind == idl.KArray) && !p.Attrs.Borrowed
}

// bound returns the "<=" bound of t as a C literal, or UINT32_MAX when there is none.
func boundLit(t *idl.Type) string {
	if t.Max > 0 {
		return fmt.Sprint(t.Max)
	}
	return "UINT32_MAX"
}

// borrowedC is the C type of the local that receives a pointer from the library through an out parameter: pointer
// to const, unless the library hands over the memory to be released with @free.
func borrowedC(p *idl.Param, elem string) string {
	if p.Attrs.Free != "" {
		return elem + " *"
	}
	return "const " + elem + " *"
}

// protoParamC is the C type of a parameter in a shim prototype (3.2).
func protoParamC(p *idl.Param) string {
	t := p.Type
	switch p.Dir {
	case idl.DirIn:
		switch t.Kind {
		case idl.KStr:
			return "const char *"
		case idl.KBytes:
			return "const void *"
		case idl.KArray, idl.KFixedArray:
			return "const " + elemC(t.Elem) + " *"
		}
		return valueC(t, p.Attrs)
	case idl.DirInOut:
		return ptrTo(valueC(t, p.Attrs))
	}
	switch {
	case p.Attrs.OffsetIn != "":
		return "const char **"
	case t.Kind == idl.KStr:
		return ptrTo(borrowedC(p, "char"))
	case t.Kind == idl.KBytes && p.Attrs.Borrowed:
		return ptrTo(borrowedC(p, "void"))
	case t.Kind == idl.KArray && p.Attrs.Borrowed:
		return ptrTo(borrowedC(p, elemC(t.Elem)))
	case t.Kind == idl.KBytes:
		return "void *"
	case t.Kind == idl.KArray:
		return elemC(t.Elem) + " *"
	case t.Kind == idl.KFixedArray:
		return elemC(t.Elem) + " *"
	}
	return ptrTo(valueC(t, p.Attrs))
}

// composite reports whether the return of a shim is a composite written through the writer or a fixed struct.
func compositeRet(f *idl.Func) bool {
	if f.Ret == nil || !f.Shim {
		return false
	}
	switch f.Ret.Kind {
	case idl.KStruct, idl.KUnion:
		return true
	case idl.KArray:
		return !isFixedT(f.Ret.Elem)
	}
	return false
}

// varWriterRet reports whether the shim writes its return through hqqrpc_wr (a variable composite).
func varWriterRet(f *idl.Func) bool {
	return compositeRet(f) && !isFixedT(f.Ret)
}

// fixedStructRet reports whether the shim returns a fixed struct through a pointer.
func fixedStructRet(f *idl.Func) bool {
	return compositeRet(f) && isFixedT(f.Ret)
}

// protoText is the C prototype of a shim, with a trailing semicolon.
func protoText(f *idl.Func) string {
	var args []string
	for _, p := range f.Params {
		args = append(args, declText(protoParamC(p), p.Name))
	}
	ret := "void"
	switch {
	case varWriterRet(f):
		args = append(args, "hqqrpc_wr *ret")
		ret = "hqqrpc_status"
	case fixedStructRet(f):
		args = append(args, declText(ptrTo(valueC(f.Ret, f.RetAttrs)), "ret"))
		ret = "hqqrpc_status"
	case f.Ret != nil:
		ret = retC(f)
	}
	if len(args) == 0 {
		args = []string{"void"}
	}
	return declText(ret, f.Name) + "(" + strings.Join(args, ", ") + ");"
}

// retC is the C type of the value a function returns (not for composite shim returns).
func retC(f *idl.Func) string {
	t := f.Ret
	switch t.Kind {
	case idl.KStr:
		return "const char *"
	case idl.KBytes:
		return "const void *"
	case idl.KArray:
		if f.RetAttrs.CType != "" {
			return f.RetAttrs.CType
		}
		return "const " + elemC(t.Elem) + " *"
	}
	return valueC(t, f.RetAttrs)
}

// hasBool reports whether a fixed value of type t contains a bool.
func hasBool(t *idl.Type) bool {
	switch t.Kind {
	case idl.KBool:
		return true
	case idl.KFixedArray, idl.KArray:
		return hasBool(t.Elem)
	case idl.KStruct:
		for _, m := range t.St.Members {
			if hasBool(m.Type) {
				return true
			}
		}
	}
	return false
}
