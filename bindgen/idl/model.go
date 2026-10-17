package idl

import (
	"math/big"
	"strconv"
	"strings"
)

// Constants of the wire format that the model and the plan use (BINDGEN.md 4.2 and 2.5).
const (
	VarSlack      = 11      // VAR_SLACK: length prefix plus alignment padding per variable item of a response bound
	DetailReserve = 1032    // DETAIL_RESERVE
	DetailMax     = 1024    // DETAIL_MAX
	MaxStatic     = 1 << 30 // V40: largest fixed type, fixed group, static response bound and @max_out
	MaxFixedArray = 1 << 20 // V6: largest N of T[N]
	MaxBound      = 1 << 30 // V6: largest N of "<=N"
)

// Serialize is the threading contract of the service (8.1).
type Serialize int

const (
	SerializeSession Serialize = iota // default
	SerializeNone
)

func (s Serialize) String() string {
	if s == SerializeNone {
		return "none"
	}
	return "session"
}

// Dir is the data direction of a parameter.
type Dir int

const (
	DirIn Dir = iota // default
	DirOut
	DirInOut
)

func (d Dir) String() string {
	switch d {
	case DirOut:
		return "out"
	case DirInOut:
		return "inout"
	}
	return "in"
}

// Kind is the kind of a Type.
type Kind int

const (
	KBool Kind = iota + 1
	KI8
	KI16
	KI32
	KI64
	KU8
	KU16
	KU32
	KU64
	KSize
	KF32
	KF64
	KStr
	KBytes
	KStruct
	KUnion
	KHandle
	KFixedArray // Elem[Len]
	KArray      // Elem[], optionally bounded by Max
)

var kindNames = [...]string{
	KBool: "bool", KI8: "i8", KI16: "i16", KI32: "i32", KI64: "i64", KU8: "u8", KU16: "u16", KU32: "u32",
	KU64: "u64", KSize: "size", KF32: "f32", KF64: "f64", KStr: "str", KBytes: "bytes",
}

func (k Kind) String() string {
	if k > 0 && int(k) < len(kindNames) && kindNames[k] != "" {
		return kindNames[k]
	}
	switch k {
	case KStruct:
		return "struct"
	case KUnion:
		return "union"
	case KHandle:
		return "handle"
	case KFixedArray:
		return "array"
	case KArray:
		return "array"
	}
	return "kind(" + strconv.Itoa(int(k)) + ")"
}

// IsInt reports whether k is one of the integer types i8..u64 and size.
func (k Kind) IsInt() bool { return k >= KI8 && k <= KSize }

// IsScalar reports whether k is an integer, bool, f32 or f64.
func (k Kind) IsScalar() bool { return k >= KBool && k <= KF64 }

// Type is a wire type. Kind selects which of the other fields are meaningful.
type Type struct {
	Kind Kind
	Name string  // struct, union or handle name
	Elem *Type   // KFixedArray, KArray
	Len  int64   // KFixedArray length
	Max  int64   // "<=" bound of KStr, KBytes and KArray; 0: none
	St   *Struct // KStruct: the declaration
	Un   *Union  // KUnion: the declaration
	H    *Handle // KHandle: the declaration
}

// String returns the type in the notation of the canonical text (2.6): the scalar name, str, bytes, a
// declared name, then [N] or [] and <=N.
func (t *Type) String() string {
	var sb strings.Builder
	t.write(&sb)
	return sb.String()
}

func (t *Type) write(sb *strings.Builder) {
	switch t.Kind {
	case KStruct, KUnion, KHandle:
		sb.WriteString(t.Name)
	case KFixedArray:
		t.Elem.write(sb)
		sb.WriteByte('[')
		sb.WriteString(strconv.FormatInt(t.Len, 10))
		sb.WriteByte(']')
	case KArray:
		t.Elem.write(sb)
		sb.WriteString("[]")
	default:
		sb.WriteString(t.Kind.String())
	}
	if t.Max > 0 {
		sb.WriteString("<=")
		sb.WriteString(strconv.FormatInt(t.Max, 10))
	}
}

// Const is a declared integer constant.
type Const struct {
	Name  string
	Type  *Type // an integer type
	Value *big.Int
	Pos   Pos
}

// Handle is a handle type (4.9). Its type id in the session table is its index in Spec.Handles plus 1.
type Handle struct {
	Name, CType  string
	Parent       *Handle // owned_by
	Destroy      *Func   // the destroy function (V7)
	RetainOnFail bool    // @retain_on_fail on Destroy (V39)
	ID           int     // index in Spec.Handles plus 1
	Pos          Pos
}

// Struct is a struct type. CType is the @c text, empty if absent.
type Struct struct {
	Name     string
	CType    string
	Members  []*Member
	Pos      Pos
	info     *tinfo
	infoBusy bool
}

// Member is a field of a struct, in declaration (wire) order.
type Member struct {
	Name string
	Type *Type
	Pos  Pos
}

// Union is a tagged union; Cases are in declaration order.
type Union struct {
	Name     string
	Cases    []*Case
	Pos      Pos
	info     *tinfo
	infoBusy bool
}

// Case is a union case. Type is nil for a case without a payload.
type Case struct {
	Tag  uint32
	Name string
	Type *Type
	Pos  Pos
}

// Spec is a parsed and validated IDL file.
type Spec struct {
	Service   string
	Serialize Serialize // SerializeSession (default) or SerializeNone
	Includes  []string
	Consts    []*Const  // declaration order
	Handles   []*Handle // declaration order; handle type id = index + 1
	Structs   []*Struct
	Unions    []*Union
	Funcs     []*Func // every function, declaration order
	Wire      []*Func // @allow(wire) functions; Wire[i].Opcode == i+1
}

// Func is a declared function.
type Func struct {
	Name      string
	GoName    string // 9.2 rule 1
	Opcode    int    // 0 for @allow(server)
	Shim      bool
	Intercept bool
	Params    []*Param
	Ret       *Type // nil: void
	RetAttrs  Attrs
	Error     *ErrorConv // nil: none
	MaxOut    int64      // 0: none
	Destroys  *Handle    // the handle type whose destroy function this is, or nil
	Plan      *Plan      // set for wire functions
	Pos       Pos
}

// Wire reports whether the function has an opcode and a client method (@allow(wire)).
func (f *Func) Wire() bool { return f.Opcode != 0 }

// Param is a function parameter.
type Param struct {
	Name    string
	Dir     Dir // DirIn, DirOut, DirInOut
	Type    *Type
	Attrs   Attrs
	Derived bool // has @len_of (4.6.1)
	Pos     Pos
}

// Attrs are the annotations of a parameter or a return.
type Attrs struct {
	LenOf, OffsetIn, Free, CType string
	Nullable, Borrowed           bool
}

// ErrKind is the failure convention of a function (4.6.4).
type ErrKind int

const (
	ErrStatus ErrKind = iota + 1
	ErrNull
	ErrPredicate
)

func (k ErrKind) String() string {
	switch k {
	case ErrStatus:
		return "status"
	case ErrNull:
		return "null"
	case ErrPredicate:
		return "predicate"
	}
	return "none"
}

// ErrorConv is the @error annotation.
type ErrorConv struct {
	Kind      ErrKind // ErrStatus, ErrNull, ErrPredicate
	Ok        []int64 // ErrStatus, sorted ascending
	Predicate string  // ErrPredicate: the C function
	Detail    *Detail // nil: none
}

// Detail is the detail(f, a1, ...) clause of @error.
type Detail struct {
	Func string
	Args []string // param names or "ret"
}
