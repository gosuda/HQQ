// The Go client emitter of package gen (spec/toolchain/BINDGEN.md, sections
// 9.2 to 9.4 and 11).
//
// # Go client emitter
//
// Go produces one file, <service>_client.go, for any validated idl.Spec. The
// file is a function of the Spec and the GoOptions only: it holds no
// timestamp and no host path, tables follow declaration order, and it is
// already formatted by go/format. Its first line is the header comment of 9.2
// with the interface digest. The file contains, in this order: the consts of
// the IDL, one opcode constant per @allow(wire) function, the rt.Iface, the
// handle, struct and union types, the Client with Dial, Close, Kill and Conn,
// and for every wire function a future type, a synchronous method, the Async
// method and the future's Wait method (9.4).
//
// The wire layout is never recomputed here: field offsets, padding, the order
// of the variable group and the size of the fixed groups come from idl.Plan,
// and the response bound from idl.BoundTerms, so the client cannot disagree
// with the C emitter. Encoding and decoding of the scalars is inline code on
// the request slot (Enc.Body) and the response body (Dec.Body); only strings,
// byte strings and element counts go through the rt helpers.
//
// Go names (9.2): methods are idl.GoName(Strip, name); a wire function X adds
// XAsync and the type XFuture; types and consts have their first letter
// upper-cased; fields are the capitalized IDL names; parameters are p_<name>.
// Locals are lowercase names from a fixed family (c, f, e, d, b, err, body,
// call, off, end, n, v, i, id, ret, rc, cnt, out, r, s), numbered when a
// function needs several of one kind (v1, i1, ...). Package-level unexported names begin
// with hqq. No local can equal a parameter, a type or a const, so no IDL name
// can collide with generated code and no Go keyword can appear as an
// identifier.
//
// Go types: bool, i8..i64 and u8..u64 are the Go integers of that width, size
// is uint64, f32 and f64 are float32 and float64, str is string, bytes is
// []byte, T[] is []T, T[N] is [N]T, a struct is a Go struct with capitalized
// fields, a union is a Go struct with a Tag and one field per payload, and a
// handle type is a struct that embeds rt.Handle. A @nullable str is *string
// (nil is NULL); a @nullable bytes or T[] is a slice whose nil value is NULL
// (an empty non-nil slice is a present empty value); a @nullable handle uses
// the nil handle.
//
// Signature (9.4, rules 1 to 3): the parameters are the non-derived in and
// inout parameters in C order, the caller-supplied out buffers in their C
// position, and, for a shim that returns an array of variable-size elements,
// a trailing destination slice named p_dst (with underscores appended until it
// differs from every other parameter name). The results are the
// return value (omitted when @len_of binds it to a buffer, when it is a status
// code with one ok value, and when it is void), then each non-derived out or
// inout parameter in C order, then error.
//
// Errors: a call-level status is returned by rt.Conn.Wait as *rt.StatusError;
// F_FAIL becomes *rt.LibError through Dec.LibError with the G2 size of the
// plan; anything in the response that contradicts the interface (length,
// flags, a bool other than 0 or 1, a count or string above its bound, an
// unknown union tag, trailing bytes) is Dec.Malformed. The Wait of a zero future
// (the result of a failed Async call) returns rt.ErrFutureUsed. Arguments are checked
// before the request is reserved (handles with CheckHandle, strings with
// CheckStr, bounds, capacities), so a refused argument commits nothing.
//
// The in-order hot path allocates nothing beyond what rt allows: futures are
// plain values, defer is open-coded, and only decoded strings, byte strings and
// slices are copied out of the response.
package gen

import (
	"bytes"
	"cmp"
	"errors"
	"fmt"
	"go/format"
	"go/token"
	"strings"
	"unicode/utf8"

	"gosuda.org/hqq/bindgen/idl"
)

// Go generates the Go client of s: one file, <service>_client.go.
func Go(s *idl.Spec, o GoOptions) ([]File, error) {
	e, err := newGoEmitter(s, o)
	if err != nil {
		return nil, err
	}
	src, err := e.render()
	if err != nil {
		return nil, err
	}
	formatted, err := format.Source(src)
	if err != nil {
		return nil, fmt.Errorf("gen: Go output of %s does not parse (emitter bug): %w\n%s", s.Service, err, src)
	}
	return []File{{Name: s.Service + "_client.go", Data: formatted}}, nil
}

var (
	errGoNilSpec    = errors.New("gen: Go: nil spec")
	errGoSourceUTF8 = errors.New("gen: Go: source name is not valid UTF-8")
)

// goReservedMethods are the exported names that the generated package surface owns besides the future
// methods: Close, Kill and Conn are methods of Client, Client is its type and Dial the constructor.
var goReservedMethods = map[string]bool{"Client": true, "Dial": true, "Close": true, "Kill": true, "Conn": true}

type goEmitter struct {
	spec   *idl.Spec
	opts   GoOptions
	pkg    string
	source string
	names  map[*idl.Func]string // Go method name of every wire function
	math   bool                 // the file uses package math
	out    goBuf
}

func newGoEmitter(s *idl.Spec, o GoOptions) (*goEmitter, error) {
	if s == nil {
		return nil, errGoNilSpec
	}
	pkg := cmp.Or(o.Package, s.Service)
	if !token.IsIdentifier(pkg) || pkg == "_" {
		return nil, fmt.Errorf("gen: Go: %q is not a valid package name", pkg)
	}
	if !token.IsIdentifier(s.Service) {
		return nil, fmt.Errorf("gen: Go: service name %q cannot start a file name of a Go package", s.Service)
	}
	source, err := goSourceName(o.Source, s.Service)
	if err != nil {
		return nil, err
	}
	e := &goEmitter{spec: s, opts: o, pkg: pkg, source: source, names: map[*idl.Func]string{}}
	if err := e.checkNames(); err != nil {
		return nil, err
	}
	for _, f := range s.Wire {
		if f.Plan == nil {
			return nil, fmt.Errorf("gen: Go: wire function %s has no plan", f.Name)
		}
	}
	return e, nil
}

// goSourceName is the file name shown in the header: the last path element, so that no host path reaches
// the output. The name must not be able to end the header comment.
func goSourceName(source, service string) (string, error) {
	if source == "" {
		return service + ".idl", nil
	}
	if !utf8.ValidString(source) {
		return "", errGoSourceUTF8
	}
	for _, r := range source {
		if r < 0x20 || r == 0x7f || r == '\u2028' || r == '\u2029' || r == '\u0085' {
			return "", fmt.Errorf("gen: Go: source name %q has a control character", source)
		}
	}
	if i := strings.LastIndexAny(source, `/\`); i >= 0 {
		source = source[i+1:]
	}
	if source == "" {
		return service + ".idl", nil
	}
	return source, nil
}

// checkNames applies the second sentence of V37 to the names this emitter derives from GoOptions.Strip:
// every exported name is valid, none is reserved and all are pairwise distinct. The parser checks the same
// rules for the strip it was given; this check makes the emitter safe when the two differ.
func (e *goEmitter) checkNames() error {
	owner := map[string]string{}
	claim := func(name, what string) error {
		switch {
		case name == "" || name[0] < 'A' || name[0] > 'Z':
			return fmt.Errorf("gen: Go: %s becomes the Go name %q, which is not an exported identifier", what, name)
		case goReservedMethods[name]:
			return fmt.Errorf("gen: Go: %s becomes the Go name %q, which the generated package surface uses", what, name)
		}
		if prev, dup := owner[name]; dup {
			return fmt.Errorf("gen: Go: %s and %s both become the Go name %q", what, prev, name)
		}
		owner[name] = what
		return nil
	}
	for _, k := range e.spec.Consts {
		if err := claim(goCapitalize(k.Name), "const "+k.Name); err != nil {
			return err
		}
	}
	for _, h := range e.spec.Handles {
		if err := claim(goCapitalize(h.Name), "handle "+h.Name); err != nil {
			return err
		}
	}
	for _, st := range e.spec.Structs {
		if err := claim(goCapitalize(st.Name), "struct "+st.Name); err != nil {
			return err
		}
	}
	for _, u := range e.spec.Unions {
		un := goCapitalize(u.Name)
		if err := claim(un, "union "+u.Name); err != nil {
			return err
		}
		if err := claim(un+"Tag", "the tag type of union "+u.Name); err != nil {
			return err
		}
		for _, c := range u.Cases {
			if err := claim(un+goCapitalize(c.Name), "case "+c.Name+" of union "+u.Name); err != nil {
				return err
			}
		}
	}
	for _, f := range e.spec.Wire {
		name := idl.GoName(e.opts.Strip, f.Name)
		e.names[f] = name
		for _, n := range []struct{ name, what string }{
			{name, "function " + f.Name},
			{name + "Async", "the async method of " + f.Name},
			{name + "Future", "the future type of " + f.Name},
		} {
			if err := claim(n.name, n.what); err != nil {
				return err
			}
		}
	}
	return nil
}

func goCapitalize(s string) string {
	if s != "" && s[0] >= 'a' && s[0] <= 'z' {
		return string(s[0]-'a'+'A') + s[1:]
	}
	return s
}

// goBuf collects generated source lines. Indentation is left to go/format.
type goBuf struct{ bytes.Buffer }

func (b *goBuf) line(format string, args ...any) {
	fmt.Fprintf(&b.Buffer, format, args...)
	b.WriteByte('\n')
}

func (b *goBuf) blank() { b.WriteByte('\n') }

// render assembles the file. The function bodies are generated first because they decide whether the
// file imports package math.
func (e *goEmitter) render() ([]byte, error) {
	var funcs goBuf
	for _, f := range e.spec.Wire {
		g := e.newFunc(f)
		g.emit()
		if g.err != nil {
			return nil, g.err
		}
		funcs.Write(g.out.Bytes())
	}

	digest := e.spec.Digest()
	e.out.line("// Code generated by hqq-bindgen from %s (interface digest %x). DO NOT EDIT.", e.source, digest)
	e.out.blank()
	e.out.line("package %s", e.pkg)
	e.out.blank()
	e.out.line("import (")
	e.out.line(`"context"`)
	e.out.line(`"encoding/binary"`)
	if e.math {
		e.out.line(`"math"`)
	}
	e.out.blank()
	e.out.line(`"gosuda.org/hqq/bindgen/rt"`)
	e.out.line(")")
	e.out.blank()
	e.emitConsts()
	e.emitOpcodes()
	e.emitIface()
	e.out.line("var hqqLE = binary.LittleEndian")
	e.out.blank()
	e.emitHandles()
	e.emitTypes()
	e.emitClient()
	e.out.Write(funcs.Bytes())
	return e.out.Bytes(), nil
}
