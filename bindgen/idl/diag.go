// Package idl parses, validates and lowers the interface description language of hqq-bindgen
// (spec/toolchain/BINDGEN.md section 2): the lexer and grammar of 2.1 and 2.2, the validation
// rules V1 to V41 of 2.5, the canonical text and digest of 2.6, the type sizes of 2.3 and 4.5, and the
// wire plan of 4.6 that both code emitters consume.
//
// The package depends on the standard library only.
package idl

import (
	"fmt"
	"sort"
	"strings"
)

// RuleSyntax is the Diagnostic.Rule of a lexical or grammatical error. Every other diagnostic carries the
// id of the validation rule of section 2.5 that it violates ("V11").
const RuleSyntax = "syntax"

// Pos is a source position. Line and Col are 1-based; Col counts bytes.
type Pos struct{ Line, Col int }

func (p Pos) String() string { return fmt.Sprintf("%d:%d", p.Line, p.Col) }

// Diagnostic is one violation, with the rule it breaks and where.
type Diagnostic struct {
	File      string
	Line, Col int
	Rule      string // "V11", or "syntax"
	Msg       string
}

func (d Diagnostic) Error() string {
	return fmt.Sprintf("%s:%d:%d: %s: %s", d.File, d.Line, d.Col, d.Rule, d.Msg)
}

// Diagnostics is the error that Parse returns: every violation found, ordered by position.
type Diagnostics []Diagnostic

func (ds Diagnostics) Error() string {
	var sb strings.Builder
	for i, d := range ds {
		if i > 0 {
			sb.WriteByte('\n')
		}
		sb.WriteString(d.Error())
	}
	return sb.String()
}

// Rules returns the rule ids of the diagnostics, in order and without repeats.
func (ds Diagnostics) Rules() []string {
	var out []string
	seen := map[string]bool{}
	for _, d := range ds {
		if !seen[d.Rule] {
			seen[d.Rule] = true
			out = append(out, d.Rule)
		}
	}
	return out
}

// diagSink collects diagnostics for one file.
type diagSink struct {
	file string
	list Diagnostics
}

func (s *diagSink) add(p Pos, rule, format string, args ...any) {
	s.list = append(s.list, Diagnostic{File: s.file, Line: p.Line, Col: p.Col, Rule: rule, Msg: fmt.Sprintf(format, args...)})
}

func (s *diagSink) sorted() Diagnostics {
	out := append(Diagnostics(nil), s.list...)
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Line != b.Line {
			return a.Line < b.Line
		}
		if a.Col != b.Col {
			return a.Col < b.Col
		}
		return a.Rule < b.Rule
	})
	return out
}
