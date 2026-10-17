package idl

import "math/bits"

// satCap is the saturation value of every size computation. Sums and products of two values at most satCap
// fit in int64, and any value above MaxStatic is already a V40 violation, so saturating loses nothing.
const satCap = int64(1) << 61

func satAdd(a, b int64) int64 {
	if s := a + b; s < satCap {
		return s
	}
	return satCap
}

func satMul(a, b int64) int64 {
	if a == 0 || b == 0 {
		return 0
	}
	hi, lo := bits.Mul64(uint64(a), uint64(b))
	if hi != 0 || lo >= uint64(satCap) {
		return satCap
	}
	return int64(lo)
}

func roundUp(n, a int64) int64 {
	if a <= 1 {
		return n
	}
	return (n + a - 1) / a * a
}

// tinfo is everything the wire rules derive from a type alone.
type tinfo struct {
	size     int64 // wire size when fixed (the stride for a struct), else 0
	align    int64
	fixed    bool
	min      int64 // MinSize
	wc       int64 // worst-case size when wcFinite
	wcFinite bool
}

func scalarSize(k Kind) int64 {
	switch k {
	case KBool, KI8, KU8:
		return 1
	case KI16, KU16:
		return 2
	case KI32, KU32, KF32, KHandle:
		return 4
	case KI64, KU64, KSize, KF64:
		return 8
	}
	return 0
}

// infoOf computes the tinfo of t. The results for structs and unions are stored in their declarations,
// which makes repeated queries on nested types linear. A struct or union that is part of a cycle (rejected
// by V4) yields a non-fixed type with no finite worst case instead of recursing forever.
func infoOf(t *Type) tinfo {
	switch t.Kind {
	case KBool, KI8, KI16, KI32, KI64, KU8, KU16, KU32, KU64, KSize, KF32, KF64:
		s := scalarSize(t.Kind)
		return tinfo{size: s, align: s, fixed: true, min: s, wc: s + s - 1, wcFinite: true}
	case KHandle:
		return tinfo{size: 4, align: 4, fixed: true, min: 4, wc: 7, wcFinite: true}
	case KStr:
		ti := tinfo{align: 4, min: 5}
		if t.Max > 0 {
			ti.wc, ti.wcFinite = satAdd(8, t.Max), true
		}
		return ti
	case KBytes:
		ti := tinfo{align: 4, min: 4}
		if t.Max > 0 {
			ti.wc, ti.wcFinite = satAdd(7, t.Max), true
		}
		return ti
	case KFixedArray:
		e := infoOf(t.Elem)
		if !e.fixed || t.Len < 1 {
			return tinfo{align: max(e.align, 1), min: 4}
		}
		stride := roundUp(e.size, e.align)
		body := satMul(t.Len, stride)
		return tinfo{
			size: body, align: e.align, fixed: true, min: body,
			wc: satAdd(e.align-1, body), wcFinite: true,
		}
	case KArray:
		e := infoOf(t.Elem)
		ti := tinfo{align: 4, min: 4}
		if t.Max > 0 && e.wcFinite {
			ti.wc, ti.wcFinite = satAdd(7, satMul(t.Max, e.wc)), true
		}
		return ti
	case KStruct:
		return structInfo(t.St)
	case KUnion:
		return unionInfo(t.Un)
	}
	return tinfo{align: 1}
}

func structInfo(s *Struct) tinfo {
	if s == nil {
		return tinfo{align: 1}
	}
	if s.info != nil {
		return *s.info
	}
	if s.infoBusy {
		return tinfo{align: 1}
	}
	s.infoBusy = true
	defer func() { s.infoBusy = false }()

	ti := tinfo{align: 1, fixed: true, wcFinite: true}
	var off int64
	for _, m := range s.Members {
		mi := infoOf(m.Type)
		ti.align = max(ti.align, mi.align)
		ti.min = satAdd(ti.min, mi.min)
		if mi.wcFinite {
			ti.wc = satAdd(ti.wc, mi.wc)
		} else {
			ti.wcFinite = false
		}
		if mi.fixed {
			off = satAdd(roundUp(off, mi.align), mi.size)
		} else {
			ti.fixed = false
		}
	}
	if ti.fixed {
		ti.size = roundUp(off, ti.align)
		// A fixed struct starts at its alignment and ends with trailing padding, which the sum of the
		// fields' wc does not cover (a struct {u8; u16; u8} encodes in 6 bytes, the sum is 5). Use the
		// rule of T[N] with N = 1: align - 1 + size.
		ti.wc, ti.wcFinite = satAdd(ti.align-1, ti.size), true
	}
	if !ti.wcFinite {
		ti.wc = 0
	}
	s.info = &ti
	return ti
}

func unionInfo(u *Union) tinfo {
	if u == nil {
		return tinfo{align: 4, min: 4}
	}
	if u.info != nil {
		return *u.info
	}
	if u.infoBusy {
		return tinfo{align: 4, min: 4}
	}
	u.infoBusy = true
	defer func() { u.infoBusy = false }()

	ti := tinfo{align: 4, min: 4, wcFinite: true}
	var payload int64
	for _, c := range u.Cases {
		if c.Type == nil {
			continue
		}
		ci := infoOf(c.Type)
		if !ci.wcFinite {
			ti.wcFinite = false
			continue
		}
		payload = max(payload, ci.wc)
	}
	if ti.wcFinite {
		ti.wc = satAdd(7, payload)
	}
	u.info = &ti
	return ti
}

// SizeOf returns the wire size and alignment of t (2.3 and 4.5) and whether t is fixed, that is, whether
// its encoded size is known from the type alone. For a fixed struct the size is the stride, rounded up to
// the alignment. For a type that is not fixed the size is 0 and only align is meaningful. Arithmetic is
// 64-bit and saturates at 2^61; a fixed size above MaxStatic violates V40.
func SizeOf(t *Type) (size, align int64, fixed bool) {
	ti := infoOf(t)
	return ti.size, ti.align, ti.fixed
}

// Stride is the distance between two elements of a fixed type in a fixed array: the size rounded up to the
// alignment (2.3). It is 0 for a type that is not fixed.
func Stride(t *Type) int64 {
	size, align, fixed := SizeOf(t)
	if !fixed {
		return 0
	}
	return roundUp(size, align)
}

// MinSize is the smallest possible encoding of t, the quantity of the plausibility check of 4.5: a
// scalar its size, a handle 4, str 5, bytes 4, T[] 4, T[N] N*stride, a struct the sum of its fields,
// a union 4.
func MinSize(t *Type) int64 { return infoOf(t).min }

// WorstCase returns wc(t) of 2.3: the largest number of body bytes one encoding of t can consume when it
// starts at any position, counting alignment padding. finite is false for a type without a bound.
func WorstCase(t *Type) (wc int64, finite bool) {
	ti := infoOf(t)
	return ti.wc, ti.wcFinite
}

// MemberOffsets returns the wire offset of each member of a fixed struct, relative to the start of the
// struct, which sits at a multiple of its alignment (4.5): every member is aligned to its own type. It
// returns nil for a struct that is not fixed, whose members align themselves at run time.
func MemberOffsets(s *Struct) []int64 {
	if s == nil || !structInfo(s).fixed {
		return nil
	}
	offs := make([]int64, len(s.Members))
	var off int64
	for i, m := range s.Members {
		size, align, _ := SizeOf(m.Type)
		off = roundUp(off, align)
		offs[i] = off
		off = satAdd(off, size)
	}
	return offs
}
