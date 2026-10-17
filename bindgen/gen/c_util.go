package gen

import (
	"fmt"
	"strings"
	"unicode"

	"gosuda.org/hqq/bindgen/idl"
)

// cLineWidth is the column at which the emitter wraps a chain of "||" conditions.
const cLineWidth = 118

// code is an indented line buffer. Lines are 4-space indented (9.2).
type code struct {
	lines []string
	depth int
}

// l appends one formatted line at the current depth. An empty result appends an empty line.
func (c *code) l(format string, args ...any) {
	s := format
	if len(args) > 0 {
		s = fmt.Sprintf(format, args...)
	}
	if s == "" {
		c.lines = append(c.lines, "")
		return
	}
	c.lines = append(c.lines, strings.Repeat("    ", c.depth)+s)
}

// add appends already formatted lines at the current depth.
func (c *code) add(lines ...string) {
	for _, s := range lines {
		c.l("%s", s)
	}
}

func (c *code) in()  { c.depth++ }
func (c *code) out() { c.depth-- }

// block writes "open {", the body produced by f one level deeper, and "}".
func (c *code) block(open string, f func()) {
	c.l("%s {", open)
	c.in()
	f()
	c.out()
	c.l("}")
}

// text joins the lines with "\n" and a final newline.
func joinLines(lines []string) string {
	return strings.Join(lines, "\n") + "\n"
}

// namer hands out C identifiers that collide with nothing generated or reserved in one scope.
type namer struct{ used map[string]bool }

func newNamer(seed map[string]bool) *namer {
	n := &namer{used: make(map[string]bool, len(seed)+16)}
	for k := range seed {
		n.used[k] = true
	}
	return n
}

// claim returns base, or base followed by "__" (IDL names contain no "__", V1) as often as needed.
func (n *namer) claim(base string) string {
	name := base
	for n.used[name] {
		name += "__"
	}
	n.used[name] = true
	return name
}

// cIdents returns the C identifier tokens of s.
func cIdents(s string) []string {
	return strings.FieldsFunc(s, func(r rune) bool { return !(r == '_' || unicode.IsLetter(r) || unicode.IsDigit(r)) })
}

// ptrTo returns the C declaration text of a pointer to ct, as "T *" ("T **" for a pointer type).
func ptrTo(ct string) string {
	if strings.HasSuffix(ct, "*") {
		return ct + "*"
	}
	return ct + " *"
}

// declText formats "type name" for a declaration; a type that ends in "*" is followed by the name directly.
func declText(typ, name string) string {
	if strings.HasSuffix(typ, "*") {
		return typ + name
	}
	return typ + " " + name
}

// splitStars splits "const char *" into "const char" and "*".
func splitStars(typ string) (base, stars string) {
	base = strings.TrimRight(typ, "* ")
	stars = strings.Repeat("*", strings.Count(typ[len(base):], "*"))
	return base, stars
}

// decl is one local variable declaration of a generated function.
type decl struct {
	prio int // 0: the fixed core of every glue function, 1: everything else, in creation order
	rank int // order inside prio 0
	typ  string
	name string // may carry an array suffix
	init string
}

// writeDecls emits the declarations, merging neighbours of the same base type and pointer depth.
func writeDecls(c *code, ds []decl) {
	for i := 0; i < len(ds); {
		base, stars := splitStars(ds[i].typ)
		j := i
		var parts []string
		for ; j < len(ds); j++ {
			b, s := splitStars(ds[j].typ)
			if b != base || s != stars {
				break
			}
			p := stars + ds[j].name
			if ds[j].init != "" {
				p += " = " + ds[j].init
			}
			parts = append(parts, p)
		}
		sep := " "
		c.l("%s%s%s;", base, sep, strings.Join(parts, ", "))
		i = j
	}
}

// chain writes `if (e1 || e2 || ...) return ret;`, wrapping the conditions when the line is too long.
func (c *code) chain(exprs []string, ret string) {
	if len(exprs) == 0 {
		return
	}
	one := "if (" + strings.Join(exprs, " || ") + ")"
	if len(strings.Repeat("    ", c.depth))+len(one) <= cLineWidth {
		c.l("%s", one)
	} else {
		for i, e := range exprs {
			switch {
			case i == 0:
				c.l("if (%s ||", e)
			case i == len(exprs)-1:
				c.l("    %s)", e)
			default:
				c.l("    %s ||", e)
			}
		}
	}
	c.in()
	c.l("return %s;", ret)
	c.out()
}

// intMax is the largest value of an integer kind as an unsigned number.
func intMax(k idl.Kind) uint64 {
	switch k {
	case idl.KI8:
		return 1<<7 - 1
	case idl.KU8:
		return 1<<8 - 1
	case idl.KI16:
		return 1<<15 - 1
	case idl.KU16:
		return 1<<16 - 1
	case idl.KI32:
		return 1<<31 - 1
	case idl.KU32:
		return 1<<32 - 1
	case idl.KI64:
		return 1<<63 - 1
	}
	return 1<<64 - 1
}

func roundUp(n, a int64) int64 {
	if a <= 1 {
		return n
	}
	return (n + a - 1) / a * a
}

// sanitizeComment keeps text from ending a C comment.
func sanitizeComment(s string) string {
	return strings.ReplaceAll(s, "*/", "*_/")
}
