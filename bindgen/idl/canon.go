package idl

import (
	"crypto/sha256"
	"sort"
	"strconv"
	"strings"
)

// Canonical returns the canonical text of the interface (2.6): the lines that determine the wire bytes and
// the client-visible behavior, produced from the validated model. Its SHA-256 is the interface digest.
func (s *Spec) Canonical() []byte {
	var sb strings.Builder
	line := func(parts ...string) {
		sb.WriteString(strings.Join(parts, " "))
		sb.WriteByte('\n')
	}
	line("HQQ-IDL", "1")
	line("service", s.Service)

	handles := append([]*Handle(nil), s.Handles...)
	sort.Slice(handles, func(i, j int) bool { return handles[i].Name < handles[j].Name })
	for _, h := range handles {
		parent := "-"
		if h.Parent != nil {
			parent = h.Parent.Name
		}
		destroy := ""
		if h.Destroy != nil {
			destroy = h.Destroy.Name
		}
		retain := "0"
		if h.RetainOnFail {
			retain = "1"
		}
		line("handle", h.Name, "parent="+parent, "destroy="+destroy, "retain="+retain)
	}

	type typeLine struct {
		name string
		text []string
	}
	var types []typeLine
	for _, st := range s.Structs {
		parts := []string{"struct", st.Name}
		for _, m := range st.Members {
			parts = append(parts, m.Name+":"+m.Type.String())
		}
		types = append(types, typeLine{st.Name, parts})
	}
	for _, u := range s.Unions {
		cases := append([]*Case(nil), u.Cases...)
		sort.Slice(cases, func(i, j int) bool { return cases[i].Tag < cases[j].Tag })
		parts := []string{"union", u.Name}
		for _, c := range cases {
			typ := "-"
			if c.Type != nil {
				typ = c.Type.String()
			}
			parts = append(parts, strconv.FormatUint(uint64(c.Tag), 10)+":"+c.Name+":"+typ)
		}
		types = append(types, typeLine{u.Name, parts})
	}
	sort.Slice(types, func(i, j int) bool { return types[i].name < types[j].name })
	for _, t := range types {
		line(t.text...)
	}

	for _, f := range s.Wire {
		parts := []string{"fn", strconv.Itoa(f.Opcode), f.Name}
		for _, p := range f.Params {
			parts = append(parts, "p="+p.Dir.String()+":"+p.Name+":"+p.Type.String()+p.Attrs.canonical())
		}
		ret := "-"
		if f.Ret != nil {
			ret = f.Ret.String() + f.RetAttrs.canonical()
		}
		parts = append(parts, "ret="+ret)
		errKind, ok, detail := "none", "-", "0"
		if e := f.Error; e != nil {
			errKind = e.Kind.String()
			if e.Kind == ErrStatus {
				vals := make([]string, len(e.Ok))
				for i, v := range e.Ok {
					vals[i] = strconv.FormatInt(v, 10)
				}
				ok = strings.Join(vals, ",")
			}
			if e.Detail != nil {
				detail = "1"
			}
		}
		maxOut := "-"
		if f.MaxOut > 0 {
			maxOut = strconv.FormatInt(f.MaxOut, 10)
		}
		parts = append(parts, "err="+errKind, "ok="+ok, "detail="+detail, "max_out="+maxOut)
		line(parts...)
	}
	return []byte(sb.String())
}

// canonical renders the attributes that appear in the canonical text, each with a leading colon, in the
// fixed order len_of, offset_in, nullable, borrowed.
func (a Attrs) canonical() string {
	var sb strings.Builder
	if a.LenOf != "" {
		sb.WriteString(":len_of=" + a.LenOf)
	}
	if a.OffsetIn != "" {
		sb.WriteString(":offset_in=" + a.OffsetIn)
	}
	if a.Nullable {
		sb.WriteString(":nullable")
	}
	if a.Borrowed {
		sb.WriteString(":borrowed")
	}
	return sb.String()
}

// Digest is the SHA-256 of Canonical().
func (s *Spec) Digest() [32]byte { return sha256.Sum256(s.Canonical()) }
