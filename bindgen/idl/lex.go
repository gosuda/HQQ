package idl

import (
	"fmt"
	"math/big"
	"unicode/utf8"
)

type tokKind int

const (
	tEOF   tokKind = iota
	tIdent         // identifier that is not a keyword
	tKw            // keyword, including the type names
	tInt           // integer literal, value in token.val
	tStr           // string literal, decoded content in token.text
	tPunct         // punctuation, text is the punctuation
	tAnn           // "@name", text is the name without the "@"
)

type token struct {
	kind tokKind
	text string
	val  *big.Int
	pos  Pos
}

func (t token) describe() string {
	switch t.kind {
	case tEOF:
		return "end of file"
	case tIdent:
		return fmt.Sprintf("identifier %q", t.text)
	case tKw:
		return fmt.Sprintf("keyword %q", t.text)
	case tInt:
		return fmt.Sprintf("integer %s", t.val)
	case tStr:
		return fmt.Sprintf("string %q", t.text)
	case tAnn:
		return fmt.Sprintf("annotation @%s", t.text)
	}
	return fmt.Sprintf("%q", t.text)
}

// Lexical limits of section 2.1.
const (
	maxIdentLen  = 128
	maxStringLen = 256
)

var keywords = map[string]bool{
	"idl": true, "service": true, "include": true, "const": true, "handle": true, "struct": true,
	"union": true, "case": true, "fn": true, "shim": true, "in": true, "out": true, "inout": true,
	"destroy": true, "owned_by": true, "ret": true,
	"bool": true, "i8": true, "i16": true, "i32": true, "i64": true, "u8": true, "u16": true,
	"u32": true, "u64": true, "f32": true, "f64": true, "size": true, "str": true, "bytes": true,
}

var (
	minInt = new(big.Int).Neg(new(big.Int).Lsh(big.NewInt(1), 63))
	maxInt = new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 64), big.NewInt(1))
)

// syntaxError is the single diagnostic that stops lexing or parsing.
type syntaxError struct {
	pos Pos
	msg string
}

type lexer struct {
	src  []byte
	i    int
	line int
	col  int
}

func isIdentStart(c byte) bool {
	return c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

func isIdentPart(c byte) bool { return isIdentStart(c) || isDigit(c) }

func isHexDigit(c byte) bool {
	return isDigit(c) || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F'
}

// lex splits src into tokens. It stops at the first lexical error.
func lex(src []byte) ([]token, *syntaxError) {
	if len(src) >= 3 && src[0] == 0xEF && src[1] == 0xBB && src[2] == 0xBF {
		return nil, &syntaxError{Pos{1, 1}, "byte order mark: the source is UTF-8 without BOM"}
	}
	lx := &lexer{src: src, line: 1, col: 1}
	var toks []token
	for {
		t, err := lx.next()
		if err != nil {
			return nil, err
		}
		toks = append(toks, t)
		if t.kind == tEOF {
			return toks, nil
		}
	}
}

func (lx *lexer) pos() Pos { return Pos{lx.line, lx.col} }

func (lx *lexer) peekAt(n int) byte {
	if lx.i+n < len(lx.src) {
		return lx.src[lx.i+n]
	}
	return 0
}

// advance moves over n bytes of the current line (no newline among them).
func (lx *lexer) advance(n int) {
	lx.i += n
	lx.col += n
}

func (lx *lexer) newline() {
	lx.i++
	lx.line++
	lx.col = 1
}

// skipSpace skips whitespace and comments.
func (lx *lexer) skipSpace() *syntaxError {
	for lx.i < len(lx.src) {
		c := lx.src[lx.i]
		switch {
		case c == '\n':
			lx.newline()
		case c == ' ' || c == '\t' || c == '\r':
			lx.advance(1)
		case c == '/' && lx.peekAt(1) == '/':
			for lx.i < len(lx.src) && lx.src[lx.i] != '\n' {
				if err := lx.skipRune(); err != nil {
					return err
				}
			}
		case c == '/' && lx.peekAt(1) == '*':
			start := lx.pos()
			lx.advance(2)
			closed := false
			for lx.i < len(lx.src) {
				if lx.src[lx.i] == '*' && lx.peekAt(1) == '/' {
					lx.advance(2)
					closed = true
					break
				}
				if lx.src[lx.i] == '\n' {
					lx.newline()
					continue
				}
				if err := lx.skipRune(); err != nil {
					return err
				}
			}
			if !closed {
				return &syntaxError{start, "unterminated comment"}
			}
		default:
			return nil
		}
	}
	return nil
}

// skipRune consumes one UTF-8 sequence inside a comment and rejects invalid UTF-8.
func (lx *lexer) skipRune() *syntaxError {
	r, n := utf8.DecodeRune(lx.src[lx.i:])
	if r == utf8.RuneError && n <= 1 {
		return &syntaxError{lx.pos(), "invalid UTF-8"}
	}
	lx.i += n
	lx.col += n
	return nil
}

func (lx *lexer) next() (token, *syntaxError) {
	if err := lx.skipSpace(); err != nil {
		return token{}, err
	}
	pos := lx.pos()
	if lx.i >= len(lx.src) {
		return token{kind: tEOF, pos: pos}, nil
	}
	c := lx.src[lx.i]
	switch {
	case isIdentStart(c):
		return lx.ident(pos)
	case isDigit(c):
		return lx.number(pos)
	case c == '-' && isDigit(lx.peekAt(1)):
		return lx.number(pos)
	case c == '"':
		return lx.str(pos)
	case c == '@':
		if !isIdentStart(lx.peekAt(1)) {
			return token{}, &syntaxError{pos, "'@' must be followed immediately by an annotation name"}
		}
		lx.advance(1)
		t, err := lx.ident(lx.pos())
		if err != nil {
			return token{}, err
		}
		return token{kind: tAnn, text: t.text, pos: pos}, nil
	}
	two := ""
	if lx.i+1 < len(lx.src) {
		two = string(lx.src[lx.i : lx.i+2])
	}
	if two == "<=" || two == "->" {
		lx.advance(2)
		return token{kind: tPunct, text: two, pos: pos}, nil
	}
	switch c {
	case '{', '}', '(', ')', '[', ']', '<', '>', ',', ';', ':', '=':
		lx.advance(1)
		return token{kind: tPunct, text: string(c), pos: pos}, nil
	}
	if c >= utf8.RuneSelf {
		return token{}, &syntaxError{pos, "non-ASCII character outside a comment"}
	}
	return token{}, &syntaxError{pos, fmt.Sprintf("unexpected character %q", c)}
}

func (lx *lexer) ident(pos Pos) (token, *syntaxError) {
	start := lx.i
	for lx.i < len(lx.src) && isIdentPart(lx.src[lx.i]) {
		lx.i++
		lx.col++
	}
	text := string(lx.src[start:lx.i])
	if len(text) > maxIdentLen {
		return token{}, &syntaxError{pos, fmt.Sprintf("identifier longer than %d bytes", maxIdentLen)}
	}
	if keywords[text] {
		return token{kind: tKw, text: text, pos: pos}, nil
	}
	return token{kind: tIdent, text: text, pos: pos}, nil
}

func (lx *lexer) number(pos Pos) (token, *syntaxError) {
	start := lx.i
	neg := false
	if lx.src[lx.i] == '-' {
		neg = true
		lx.advance(1)
	}
	base := 10
	digits := lx.i
	if lx.src[lx.i] == '0' && (lx.peekAt(1) == 'x') {
		base = 16
		lx.advance(2)
		digits = lx.i
		for lx.i < len(lx.src) && isHexDigit(lx.src[lx.i]) {
			lx.advance(1)
		}
		if lx.i == digits {
			return token{}, &syntaxError{pos, "hexadecimal literal without digits"}
		}
	} else {
		for lx.i < len(lx.src) && isDigit(lx.src[lx.i]) {
			lx.advance(1)
		}
	}
	if lx.i < len(lx.src) && isIdentPart(lx.src[lx.i]) {
		return token{}, &syntaxError{pos, "malformed integer literal (no suffixes, no octal)"}
	}
	v, ok := new(big.Int).SetString(string(lx.src[digits:lx.i]), base)
	if !ok {
		return token{}, &syntaxError{pos, "malformed integer literal"}
	}
	if neg {
		v.Neg(v)
	}
	if v.Cmp(minInt) < 0 || v.Cmp(maxInt) > 0 {
		return token{}, &syntaxError{pos, fmt.Sprintf("integer %s out of range [-2^63, 2^64-1]", string(lx.src[start:lx.i]))}
	}
	return token{kind: tInt, val: v, text: string(lx.src[start:lx.i]), pos: pos}, nil
}

func (lx *lexer) str(pos Pos) (token, *syntaxError) {
	lx.advance(1)
	var out []byte
	for {
		if lx.i >= len(lx.src) {
			return token{}, &syntaxError{pos, "unterminated string literal"}
		}
		c := lx.src[lx.i]
		switch {
		case c == '"':
			lx.advance(1)
			return token{kind: tStr, text: string(out), pos: pos}, nil
		case c == '\\':
			e := lx.peekAt(1)
			if e != '\\' && e != '"' {
				return token{}, &syntaxError{lx.pos(), "invalid escape in string literal (only \\\\ and \\\" exist)"}
			}
			out = append(out, e)
			lx.advance(2)
		case c < 0x20 || c > 0x7E:
			return token{}, &syntaxError{lx.pos(), "string literals contain ASCII 0x20 to 0x7E only"}
		default:
			out = append(out, c)
			lx.advance(1)
		}
		if len(out) > maxStringLen {
			return token{}, &syntaxError{pos, fmt.Sprintf("string literal longer than %d bytes", maxStringLen)}
		}
	}
}
