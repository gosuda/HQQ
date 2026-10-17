package idl_test

import (
	"testing"

	"gosuda.org/hqq/bindgen/idl"
)

// This file checks SizeOf, MinSize and WorstCase against a reference encoder that implements the value
// encoding of BINDGEN.md 4.5 directly, with no use of the idl size functions.

type mode int

const (
	modeMin mode = iota // the shortest encoding of a value: empty strings, no elements, the cheapest union case
	modeMax             // the longest encoding the type admits (needs finite bounds)
)

func alignUp(n, a int64) int64 { return (n + a - 1) / a * a }

func scalarBytes(k idl.Kind) int64 {
	switch k {
	case idl.KBool, idl.KI8, idl.KU8:
		return 1
	case idl.KI16, idl.KU16:
		return 2
	case idl.KI32, idl.KU32, idl.KF32, idl.KHandle:
		return 4
	}
	return 8
}

// refFixed reports whether the encoded size of t is independent of the value.
func refFixed(t *idl.Type) bool {
	switch {
	case t.Kind.IsScalar(), t.Kind == idl.KHandle:
		return true
	case t.Kind == idl.KFixedArray:
		return refFixed(t.Elem)
	case t.Kind == idl.KStruct:
		for _, m := range t.St.Members {
			if !refFixed(m.Type) {
				return false
			}
		}
		return true
	}
	return false
}

// refAlign is the alignment of t as 2.3 defines it.
func refAlign(t *idl.Type) int64 {
	switch {
	case t.Kind.IsScalar(), t.Kind == idl.KHandle:
		return scalarBytes(t.Kind)
	case t.Kind == idl.KFixedArray:
		return refAlign(t.Elem)
	case t.Kind == idl.KStruct:
		a := int64(1)
		for _, m := range t.St.Members {
			a = max(a, refAlign(m.Type))
		}
		return a
	}
	return 4
}

// refEnd returns the position at which the encoding of the chosen value of t ends when it starts at
// position start.
func refEnd(t *idl.Type, start int64, m mode) int64 {
	n := func(bound int64) int64 { // length or count of a string, byte string or array
		if m == modeMax {
			return bound
		}
		return 0
	}
	switch {
	case t.Kind.IsScalar(), t.Kind == idl.KHandle:
		s := scalarBytes(t.Kind)
		return alignUp(start, s) + s
	case t.Kind == idl.KFixedArray:
		p := alignUp(start, refAlign(t.Elem))
		return p + t.Len*refStride(t.Elem)
	case t.Kind == idl.KStr:
		return alignUp(start, 4) + 4 + n(t.Max) + 1
	case t.Kind == idl.KBytes:
		return alignUp(start, 4) + 4 + n(t.Max)
	case t.Kind == idl.KArray:
		p := alignUp(start, 4) + 4
		for range n(t.Max) {
			p = refEnd(t.Elem, p, m)
		}
		return p
	case t.Kind == idl.KStruct:
		fixed := refFixed(t)
		p := start
		if fixed {
			p = alignUp(start, refAlign(t))
		}
		first := p
		for _, mem := range t.St.Members {
			p = refEnd(mem.Type, p, m)
		}
		if fixed {
			p = first + alignUp(p-first, refAlign(t))
		}
		return p
	case t.Kind == idl.KUnion:
		p := alignUp(start, 4) + 4
		best := int64(-1)
		for _, c := range t.Un.Cases {
			end := p
			if c.Type != nil {
				end = refEnd(c.Type, p, m)
			}
			switch {
			case best < 0:
				best = end
			case m == modeMin && end < best, m == modeMax && end > best:
				best = end
			}
		}
		return best
	}
	panic("refEnd: unknown kind")
}

// refStride is the size of a fixed type rounded up to its alignment.
func refStride(t *idl.Type) int64 {
	return alignUp(refEnd(t, 0, modeMin), refAlign(t))
}

func namedStruct(t *testing.T, spec *idl.Spec, name string) *idl.Type {
	t.Helper()
	for _, s := range spec.Structs {
		if s.Name == name {
			return &idl.Type{Kind: idl.KStruct, Name: name, St: s}
		}
	}
	t.Fatalf("no struct %s", name)
	return nil
}

func namedUnion(t *testing.T, spec *idl.Spec, name string) *idl.Type {
	t.Helper()
	for _, u := range spec.Unions {
		if u.Name == name {
			return &idl.Type{Kind: idl.KUnion, Name: name, Un: u}
		}
	}
	t.Fatalf("no union %s", name)
	return nil
}

const typesIDL = hdr + `
struct Pair { a: u8; b: u32; }
struct Wide { p: Pair; q: u64; }
struct Bytes3 { x: u8[3]; }
struct Odd { a: u8; b: u16; c: u8; }
struct Var1 { n: u8; s: str<=9; }
struct Var2 { n: u64; b: bytes<=4; t: u16; }
struct Nest { v: Var1; w: Pair; }
struct Arrs { p: Pair[3]; q: i16[5]; }
union Cell { case 1 Int: i64; case 2 Text: str<=16; case 3 Null; }
union Both { case 1 A: Pair; case 2 B: Var1; case 3 C: u8; }
union NoEmpty { case 1 A: u8; case 2 B: u64; }
union One { case 1 Only; }
`

func TestTypeSizesAgreeWithTheEncoder(t *testing.T) {
	spec, err := idl.Parse("types.idl", []byte(typesIDL), idl.Options{})
	if err != nil {
		t.Fatal(err)
	}
	scalars := []idl.Kind{idl.KBool, idl.KI8, idl.KI16, idl.KI32, idl.KI64, idl.KU8, idl.KU16, idl.KU32,
		idl.KU64, idl.KSize, idl.KF32, idl.KF64}
	var types []*idl.Type
	for _, k := range scalars {
		types = append(types, &idl.Type{Kind: k})
	}
	handle := &idl.Type{Kind: idl.KHandle, Name: "H", H: &idl.Handle{Name: "H"}}
	types = append(types, handle)
	for _, name := range []string{"Pair", "Wide", "Bytes3", "Odd", "Var1", "Var2", "Nest", "Arrs"} {
		types = append(types, namedStruct(t, spec, name))
	}
	for _, name := range []string{"Cell", "Both", "NoEmpty", "One"} {
		types = append(types, namedUnion(t, spec, name))
	}
	u8 := &idl.Type{Kind: idl.KU8}
	u64 := &idl.Type{Kind: idl.KU64}
	i16 := &idl.Type{Kind: idl.KI16}
	pair := namedStruct(t, spec, "Pair")
	cell := namedUnion(t, spec, "Cell")
	types = append(types,
		&idl.Type{Kind: idl.KFixedArray, Elem: u8, Len: 7},
		&idl.Type{Kind: idl.KFixedArray, Elem: u64, Len: 3},
		&idl.Type{Kind: idl.KFixedArray, Elem: i16, Len: 1},
		&idl.Type{Kind: idl.KFixedArray, Elem: pair, Len: 5},
		&idl.Type{Kind: idl.KFixedArray, Elem: handle, Len: 2},
		&idl.Type{Kind: idl.KStr}, &idl.Type{Kind: idl.KStr, Max: 5},
		&idl.Type{Kind: idl.KBytes}, &idl.Type{Kind: idl.KBytes, Max: 9},
		&idl.Type{Kind: idl.KArray, Elem: u8}, &idl.Type{Kind: idl.KArray, Elem: u8, Max: 6},
		&idl.Type{Kind: idl.KArray, Elem: u64, Max: 3},
		&idl.Type{Kind: idl.KArray, Elem: pair, Max: 4},
		&idl.Type{Kind: idl.KArray, Elem: cell, Max: 256},
		&idl.Type{Kind: idl.KArray, Elem: &idl.Type{Kind: idl.KStr, Max: 3}, Max: 2},
	)

	for _, ty := range types {
		t.Run(ty.String(), func(t *testing.T) {
			size, align, fixed := idl.SizeOf(ty)
			if fixed != refFixed(ty) {
				t.Fatalf("fixed = %v, encoder says %v", fixed, refFixed(ty))
			}
			if align != refAlign(ty) {
				t.Errorf("align = %d, encoder says %d", align, refAlign(ty))
			}
			if fixed {
				if want := refStride(ty); size != want {
					t.Errorf("size = %d, encoder says %d", size, want)
				}
				if idl.Stride(ty) != size {
					t.Errorf("Stride = %d, size %d", idl.Stride(ty), size)
				}
			} else if size != 0 {
				t.Errorf("a type that is not fixed reports size %d, want 0", size)
			}

			// MinSize is the plausibility lower bound: no encoding is shorter, and it is exact unless a
			// struct or union has no cheapest member that costs only its minimum.
			minEnd := refEnd(ty, 0, modeMin)
			got := idl.MinSize(ty)
			if got > minEnd {
				t.Errorf("MinSize = %d is above the shortest encoding %d", got, minEnd)
			}
			if exactMin(ty) && got != minEnd {
				t.Errorf("MinSize = %d, the shortest encoding at position 0 is %d", got, minEnd)
			}

			wc, finite := idl.WorstCase(ty)
			if finite != isBounded(ty) {
				t.Fatalf("finite = %v, bounded = %v", finite, isBounded(ty))
			}
			if !finite {
				return
			}
			var worst int64
			for p := range int64(16) {
				worst = max(worst, refEnd(ty, p, modeMax)-p)
			}
			if wc < worst {
				t.Errorf("wc = %d is below the largest encoding %d found at some start position", wc, worst)
			}
			if exactWC(ty) && wc != worst {
				t.Errorf("wc = %d, the largest encoding over all start positions is %d", wc, worst)
			}
		})
	}
}

// exactMin reports whether MinSize equals the shortest encoding for t: true unless alignment padding or a
// union that has no payload-free case widens the encoding beyond the sum of the minimum sizes.
func exactMin(t *idl.Type) bool {
	switch {
	case t.Kind.IsScalar(), t.Kind == idl.KHandle, t.Kind == idl.KStr, t.Kind == idl.KBytes, t.Kind == idl.KArray:
		return true
	case t.Kind == idl.KFixedArray:
		return true
	case t.Kind == idl.KUnion:
		for _, c := range t.Un.Cases {
			if c.Type == nil {
				return true
			}
		}
		return false
	}
	return false // a struct: padding between fields counts in the encoder and not in the sum
}

// isBounded reports whether wc(t) is finite.
func isBounded(t *idl.Type) bool {
	switch {
	case t.Kind.IsScalar(), t.Kind == idl.KHandle:
		return true
	case t.Kind == idl.KFixedArray:
		return true
	case t.Kind == idl.KStr, t.Kind == idl.KBytes:
		return t.Max > 0
	case t.Kind == idl.KArray:
		return t.Max > 0 && isBounded(t.Elem)
	case t.Kind == idl.KStruct:
		for _, m := range t.St.Members {
			if !isBounded(m.Type) {
				return false
			}
		}
		return true
	case t.Kind == idl.KUnion:
		for _, c := range t.Un.Cases {
			if c.Type != nil && !isBounded(c.Type) {
				return false
			}
		}
		return true
	}
	return false
}

// exactWC reports whether wc(t) is exactly the largest encoding over all start positions, which holds for
// the types whose only padding is the single alignment before them.
func exactWC(t *idl.Type) bool {
	switch {
	case t.Kind.IsScalar(), t.Kind == idl.KHandle, t.Kind == idl.KFixedArray, t.Kind == idl.KStr, t.Kind == idl.KBytes:
		return true
	case t.Kind == idl.KArray:
		return t.Elem.Kind == idl.KU8 // one byte per element, no padding between elements
	}
	return false
}

// TestDocumentWorstCase pins the number of section 2.7: wc(Cell[]<=256) = 7 + 256 * 31 = 7943.
func TestDocumentWorstCase(t *testing.T) {
	spec, err := idl.Parse("types.idl", []byte(typesIDL), idl.Options{})
	if err != nil {
		t.Fatal(err)
	}
	cell := namedUnion(t, spec, "Cell")
	if wc, ok := idl.WorstCase(cell); !ok || wc != 31 {
		t.Errorf("wc(Cell) = %d, %v; the document says 31", wc, ok)
	}
	arr := &idl.Type{Kind: idl.KArray, Elem: cell, Max: 256}
	if wc, ok := idl.WorstCase(arr); !ok || wc != 7943 {
		t.Errorf("wc(Cell[]<=256) = %d, %v; the document says 7943", wc, ok)
	}
}

// TestSizeOfSaturates checks that nested fixed arrays do not wrap around: V40 depends on it.
func TestSizeOfSaturates(t *testing.T) {
	u8 := &idl.Type{Kind: idl.KU8}
	big := &idl.Type{Kind: idl.KFixedArray, Elem: u8, Len: 1 << 20}
	for range 8 {
		big = &idl.Type{Kind: idl.KFixedArray, Elem: big, Len: 1 << 20}
	}
	size, _, fixed := idl.SizeOf(big)
	if !fixed || size <= idl.MaxStatic {
		t.Errorf("size = %d fixed = %v, want a saturated size above 2^30", size, fixed)
	}
	if wc, ok := idl.WorstCase(big); !ok || wc <= idl.MaxStatic {
		t.Errorf("wc = %d, %v", wc, ok)
	}
}

func TestMemberOffsets(t *testing.T) {
	spec, err := idl.Parse("types.idl", []byte(typesIDL), idl.Options{})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string][]int64{
		"Pair": {0, 4}, "Wide": {0, 8}, "Bytes3": {0}, "Odd": {0, 2, 4}, "Arrs": {0, 24}, "Var1": nil, "Nest": nil,
	}
	for name, w := range want {
		var got []int64
		for _, s := range spec.Structs {
			if s.Name == name {
				got = idl.MemberOffsets(s)
			}
		}
		if len(got) != len(w) {
			t.Errorf("%s: offsets %v, want %v", name, got, w)
			continue
		}
		for i := range w {
			if got[i] != w[i] {
				t.Errorf("%s: offsets %v, want %v", name, got, w)
			}
		}
	}
}
