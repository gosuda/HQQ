package idl

import (
	"errors"
	"math"
)

// Role is the part a Field plays in a request or response body.
type Role int

const (
	// RoleValue is the value of a parameter.
	RoleValue Role = iota
	// RoleCap is the u32 capacity (bytes for bytes, elements for T[]) that the request carries for a
	// caller-supplied out buffer (4.6.2). The Field names the buffer parameter; its Type is u32.
	RoleCap
	// RoleRet is the return value (4.6.1).
	RoleRet
)

func (r Role) String() string {
	switch r {
	case RoleCap:
		return "cap"
	case RoleRet:
		return "ret"
	}
	return "value"
}

// Plan is the lowering of 4.6 for one wire function. Derived parameters (@len_of) appear in no layout:
// an in scalar is computed by the server and an out scalar by the client (4.6.1); only a return that
// carries @len_of is sent, as a RoleRet field of G2.
type Plan struct {
	Req, Resp Layout
	Detail    bool // the function has detail(...)
}

// Layout is a body: a fixed group of statically placed fields, followed by variable-size values.
type Layout struct {
	Fixed     []Field // G1 or G2, in order
	FixedSize int     // size(G1) or size(G2), unpadded end offset
	FixedPad  int     // round_up(FixedSize, 8): G1pad, G2pad (4.6.2, 4.6.3)
	Var       []Field // V1 or V2, in order
}

// Field is one value of a body.
type Field struct {
	Name  string // the parameter name, or "ret"
	Role  Role   // RoleValue, RoleCap, RoleRet
	Type  *Type
	Param int // index into Func.Params, -1 for the return
	Off   int // static offset in the fixed group, -1 in the variable group
}

// VarStart is the body offset at which the variable group starts: the fixed group padded to 8 when the
// variable group is not empty (4.6.2, 4.6.3), the end of the fixed group otherwise.
func (l *Layout) VarStart() int {
	if len(l.Var) > 0 {
		return l.FixedPad
	}
	return l.FixedSize
}

// G2Pad is G2pad of 4.6.3: round_up(size(G2), 8) if V2 is statically non-empty or the function has
// detail(...), else size(G2). A failure body that carries a detail string starts it at G2Pad (4.6.4).
func (p *Plan) G2Pad() int {
	if len(p.Resp.Var) > 0 || p.Detail {
		return p.Resp.FixedPad
	}
	return p.Resp.FixedSize
}

// BoundTerm is one term of RESP_BOUND (4.6.3), symbolic in the request cap: its value is
// Const + PerCap*cap when PerCap is not 0, else Const.
type BoundTerm struct {
	Name   string // the parameter name, or "ret"
	Param  int    // index into Func.Params, -1 for the return
	Const  int64  // the part that does not depend on a cap, VarSlack included
	PerCap int64  // bytes per cap unit: 1 for bytes, the stride for T[]; 0 if the term has no cap
	CapMax int64  // the "<=" bound of the buffer, 0 if it has none (the term is then left out of StaticBound)
}

var u32Type = &Type{Kind: KU32}

// buildPlan lowers a validated wire function.
func buildPlan(f *Func) *Plan {
	p := &Plan{Detail: f.Error != nil && f.Error.Detail != nil}
	var reqFixed, reqVar, respFixed, respVar []Field

	for i, par := range f.Params {
		if par.Derived {
			continue
		}
		if par.Dir == DirIn || par.Dir == DirInOut {
			fl := Field{Name: par.Name, Role: RoleValue, Type: par.Type, Param: i, Off: -1}
			if isFixed(par.Type) {
				reqFixed = append(reqFixed, fl)
			} else {
				reqVar = append(reqVar, fl)
			}
		}
		if callerSupplied(par) {
			reqFixed = append(reqFixed, Field{Name: par.Name, Role: RoleCap, Type: u32Type, Param: i, Off: -1})
		}
	}
	if f.Ret != nil {
		fl := Field{Name: "ret", Role: RoleRet, Type: f.Ret, Param: -1, Off: -1}
		if isFixed(f.Ret) {
			respFixed = append(respFixed, fl)
		} else {
			respVar = append(respVar, fl)
		}
	}
	for i, par := range f.Params {
		if par.Derived || par.Dir == DirIn {
			continue
		}
		fl := Field{Name: par.Name, Role: RoleValue, Type: par.Type, Param: i, Off: -1}
		if isFixed(par.Type) {
			respFixed = append(respFixed, fl)
		} else {
			respVar = append(respVar, fl)
		}
	}
	p.Req = layoutOf(reqFixed, reqVar)
	p.Resp = layoutOf(respFixed, respVar)
	return p
}

// layoutOf places the fixed fields like a C struct with natural alignment and no trailing padding.
func layoutOf(fixed, variable []Field) Layout {
	var off int64
	for i := range fixed {
		size, align, _ := SizeOf(fixed[i].Type)
		off = roundUp(off, align)
		fixed[i].Off = int(off)
		off = satAdd(off, size)
	}
	return Layout{
		Fixed:     fixed,
		FixedSize: int(min(off, math.MaxInt32)),
		FixedPad:  int(min(roundUp(off, 8), math.MaxInt32)),
		Var:       variable,
	}
}

// BoundTerms returns the terms of RESP_BOUND (4.6.3) of f, one for each variable item of the response, in
// the order of the response body. f must be the function this plan belongs to.
func (p *Plan) BoundTerms(f *Func) []BoundTerm {
	var terms []BoundTerm
	for _, fl := range p.Resp.Var {
		t := fl.Type
		bt := BoundTerm{Name: fl.Name, Param: fl.Param}
		switch {
		case fl.Param >= 0 && callerSupplied(f.Params[fl.Param]):
			bt.Const = VarSlack
			bt.PerCap = 1
			if t.Kind == KArray {
				bt.PerCap = Stride(t.Elem)
			}
			bt.CapMax = t.Max
		case t.Kind == KStr:
			bt.Const = satAdd(VarSlack+1, t.Max)
		case t.Kind == KBytes:
			bt.Const = satAdd(VarSlack, t.Max)
		case t.Kind == KArray && isFixed(t.Elem):
			bt.Const = satAdd(VarSlack, satMul(t.Max, Stride(t.Elem)))
		default: // composite return
			bt.Const = f.MaxOut
		}
		terms = append(terms, bt)
	}
	return terms
}

var errNotWire = errors.New("idl: the function has no wire plan")

// StaticBound is RESP_BOUND with every caller-chosen cap set to its "<=" bound; a caller-supplied buffer
// without a bound is left out (V40). The sum saturates at 2^61, which is above every limit.
func (p *Plan) StaticBound(f *Func) (uint64, error) {
	if p == nil {
		return 0, errNotWire
	}
	g2 := int64(p.G2Pad())
	sum := g2
	for _, bt := range p.BoundTerms(f) {
		if bt.PerCap != 0 {
			if bt.CapMax == 0 {
				continue
			}
			sum = satAdd(sum, satAdd(bt.Const, satMul(bt.PerCap, bt.CapMax)))
			continue
		}
		sum = satAdd(sum, bt.Const)
	}
	if p.Detail {
		sum = max(sum, g2+DetailReserve)
	}
	return uint64(sum), nil
}

// finish builds the plan of a wire function that has no diagnostics and applies V40 to its sizes.
func (fc *fnCheck) finish() {
	if fc.server || fc.bad {
		return
	}
	f := fc.f
	p := buildPlan(f)
	f.Plan = p
	if p.Req.FixedSize > MaxStatic {
		fc.err(f.Pos, "V40", "the fixed request group G1 of %s is %d bytes, the limit is 2^30", f.Name, p.Req.FixedSize)
	}
	if p.Resp.FixedSize > MaxStatic {
		fc.err(f.Pos, "V40", "the fixed response group G2 of %s is %d bytes, the limit is 2^30", f.Name, p.Resp.FixedSize)
	}
	if b, _ := p.StaticBound(f); b > MaxStatic {
		fc.err(f.Pos, "V40", "the static response bound of %s is %d bytes, the limit is 2^30", f.Name, b)
	}
}
