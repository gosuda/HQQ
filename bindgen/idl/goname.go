package idl

import (
	"strings"
	"unicode"
)

// GoName is the method name of a function (9.2 rule 1): the -go-strip prefix is removed, then every
// "_"-separated part is capitalized and the parts are joined. "ml_counter_new" with strip "ml_" is
// "CounterNew"; "sqlite3_open_v2" is "Sqlite3OpenV2". The prefix is removed only when the name starts with
// it. The result can be empty or not exported; the validator (V37) rejects such names.
func GoName(strip, cname string) string {
	name := strings.TrimPrefix(cname, strip)
	var sb strings.Builder
	for _, part := range strings.Split(name, "_") {
		sb.WriteString(capitalize(part))
	}
	return sb.String()
}

// reservedGo are the exported names the generated package surface takes (9.2 rule 6).
var reservedGo = map[string]bool{"Client": true, "Dial": true, "Close": true, "Kill": true, "Conn": true}

// exportedGo reports whether name is an exported Go identifier made of ASCII letters, digits and
// underscores.
func exportedGo(name string) bool {
	return name != "" && unicode.IsUpper(rune(name[0]))
}

// checkGoNames applies the second sentence of V37: the exported Go names that the generator emits are
// valid, pairwise distinct and none is a reserved name.
func (c *checker) checkGoNames() {
	owner := map[string]string{}
	claim := func(pos Pos, goName, what string) {
		switch {
		case !exportedGo(goName):
			c.sink.add(pos, "V37", "%s becomes the Go name %q, which is not an exported identifier", what, goName)
		case reservedGo[goName]:
			c.sink.add(pos, "V37", "%s becomes the Go name %q, which the generated package surface uses", what, goName)
		default:
			if prev, dup := owner[goName]; dup {
				c.sink.add(pos, "V37", "%s and %s both become the Go name %q", what, prev, goName)
				return
			}
			owner[goName] = what
		}
	}

	for _, k := range c.spec.Consts {
		if c.consts[k.Name] == k {
			claim(k.Pos, capitalize(k.Name), "const "+k.Name)
		}
	}
	for _, h := range c.spec.Handles {
		if c.types[h.Name].h == h {
			claim(h.Pos, capitalize(h.Name), "handle "+h.Name)
		}
	}
	for _, s := range c.spec.Structs {
		if c.types[s.Name].st != s {
			continue
		}
		claim(s.Pos, capitalize(s.Name), "struct "+s.Name)
		seen := map[string]string{}
		for _, m := range s.Members {
			g := capitalize(m.Name)
			if prev, dup := seen[g]; dup && prev != m.Name {
				c.sink.add(m.Pos, "V37", "fields %s and %s of struct %s both become the Go field %q", prev, m.Name, s.Name, g)
			}
			seen[g] = m.Name
		}
	}
	for _, u := range c.spec.Unions {
		if c.types[u.Name].un != u {
			continue
		}
		un := capitalize(u.Name)
		claim(u.Pos, un, "union "+u.Name)
		claim(u.Pos, un+"Tag", "the tag type of union "+u.Name)
		seen := map[string]string{}
		for _, cs := range u.Cases {
			g := capitalize(cs.Name)
			if g == "Tag" {
				c.sink.add(cs.Pos, "V37", "union %s has a case %q, which would collide with the Tag field", u.Name, cs.Name)
			}
			prev, dup := seen[g]
			if dup && prev == cs.Name {
				continue // the same name twice is V5
			}
			if dup {
				c.sink.add(cs.Pos, "V37", "cases %s and %s of union %s both become the Go field %q", prev, cs.Name, u.Name, g)
			}
			seen[g] = cs.Name
			claim(cs.Pos, un+g, "case "+cs.Name+" of union "+u.Name)
		}
	}
	for _, f := range c.spec.Funcs {
		f.GoName = GoName(c.opts.GoStrip, f.Name)
		if !f.Wire() || c.funcs[f.Name] != f {
			continue
		}
		claim(f.Pos, f.GoName, "function "+f.Name)
		claim(f.Pos, f.GoName+"Async", "the async method of "+f.Name)
		claim(f.Pos, f.GoName+"Future", "the future type of "+f.Name)
	}
}
