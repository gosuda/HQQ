// Package header extracts an initial HQQ bindgen IDL schema from a C header file (.h).
//
// The extractor is a deterministic bootstrap tool: it scans preprocessor integer macros, enum
// constants, opaque struct typedefs, fixed-layout C structs, and function prototypes, and emits
// a valid `idl 1` specification that passes idl.Parse without manual syntax fixes. Declarations
// that require human or agent policy decisions (such as variadic functions, function-pointer
// callbacks, or unbounded output buffers) are either given safe default bounds or emitted as
// documented comments explaining how to wrap them with a `shim fn`.
package header

import (
	"bytes"
	"fmt"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"unicode"
)

// Options configures C-header-to-IDL extraction.
type Options struct {
	// Service is the IDL service name. When empty, it is derived from the header filename.
	Service string
	// Include is the header path emitted in `include "...";`. When empty, filepath.Base(path) is used.
	Include string
	// Prefix filters functions and constants to those starting with Prefix (case-sensitive).
	// When empty, all parseable declarations in the header are considered.
	Prefix string
	// SerializeNone emits `@serialize(none)` on the service declaration.
	SerializeNone bool
	// DefaultMaxStr is the default byte bound for `str<=N` parameters and returns (default 4096).
	DefaultMaxStr int
	// DefaultMaxBytes is the default byte bound for `bytes<=N` and `T[]<=N` buffers (default 16777216).
	DefaultMaxBytes int
}

type parsedConst struct {
	name  string
	typ   string
	value int64
	uval  uint64
	isU64 bool
}

type parsedField struct {
	name string
	typ  string
}

type parsedStruct struct {
	cName   string // e.g. "ml_pair" or "struct foo"
	idlName string // e.g. "Pair"
	fields  []parsedField
}

type parsedHandle struct {
	cType     string // e.g. "ZSTD_CCtx *" or "sqlite3 *"
	baseName  string // e.g. "ZSTD_CCtx" or "sqlite3"
	idlName   string // e.g. "CCtx" or "Db"
	destroyFn string // IDL function name that destroys this handle
	cancelFn  string // C function name for @cancel(...)
	ownedBy   string // IDL handle name of parent, if inferred
	synthShim bool   // true if destroyFn is a synthesized shim
}

type rawCParam struct {
	name      string
	cType     string
	isConst   bool
	ptrDepth  int
	baseType  string
	arraySize int // >0 if fixed array T[N]
}

type parsedFunc struct {
	cName    string
	retType  string
	retPtr   int
	retConst bool
	params   []rawCParam
}

// Extract parses the C header src (named path) and returns a formatted `idl 1` specification.
func Extract(path string, src []byte, opts Options) ([]byte, error) {
	if opts.DefaultMaxStr <= 0 {
		opts.DefaultMaxStr = 4096
	}
	if opts.DefaultMaxBytes <= 0 {
		opts.DefaultMaxBytes = 16777216
	}
	base := filepath.Base(path)
	inc := opts.Include
	if inc == "" {
		inc = sanitizeInclude(base)
	}
	svc := opts.Service
	if svc == "" {
		svc = deriveServiceName(base)
	}

	e := &extractor{
		path:          path,
		inc:           inc,
		svc:           svc,
		prefix:        opts.Prefix,
		serializeNone: opts.SerializeNone,
		maxStr:        opts.DefaultMaxStr,
		maxBytes:      opts.DefaultMaxBytes,
		opaqueTypes:   map[string]string{}, // C type name -> C pointer spelling
		pointerTypes:  map[string]string{}, // typedef pointer name -> C pointer spelling
		intTypedefs:   map[string]string{}, // typedef name -> IDL scalar type
		structDefs:    map[string]*parsedStruct{},
		constVals:     map[string]int64{},
		usedNames:     map[string]bool{},
		usedGoNames: map[string]bool{
			"Client": true,
			"Dial":   true,
			"Close":  true,
			"Kill":   true,
			"Conn":   true,
		},
	}
	return e.run(string(src))
}

type extractor struct {
	path          string
	inc           string
	svc           string
	prefix        string
	serializeNone bool
	maxStr        int
	maxBytes      int

	consts       []parsedConst
	constVals    map[string]int64
	opaqueTypes  map[string]string
	pointerTypes map[string]string
	intTypedefs  map[string]string
	structDefs   map[string]*parsedStruct
	structOrder  []string
	handles      map[string]*parsedHandle // baseName -> handle
	handleOrder  []string
	funcs        []parsedFunc
	usedNames    map[string]bool
	usedGoNames  map[string]bool
}

func (e *extractor) run(src string) ([]byte, error) {
	noComments := stripComments(src)
	cleanC := e.extractDefinesAndStripPP(noComments)
	cleanC = unwrapExternC(cleanC)
	decls := splitTopLevelDecls(cleanC)

	for _, d := range decls {
		e.classifyDecl(d)
	}
	e.buildHandles()
	return e.emitIDL(), nil
}

// stripComments removes /* ... */ and // ... comments from C source, replacing them with spaces.
func stripComments(s string) string {
	var out strings.Builder
	out.Grow(len(s))
	i := 0
	n := len(s)
	for i < n {
		if s[i] == '"' || s[i] == '\'' {
			quote := s[i]
			out.WriteByte(quote)
			i++
			for i < n {
				out.WriteByte(s[i])
				if s[i] == '\\' && i+1 < n {
					i++
					out.WriteByte(s[i])
				} else if s[i] == quote {
					i++
					break
				}
				i++
			}
			continue
		}
		if i+1 < n && s[i] == '/' && s[i+1] == '*' {
			out.WriteByte(' ')
			i += 2
			for i+1 < n && !(s[i] == '*' && s[i+1] == '/') {
				if s[i] == '\n' {
					out.WriteByte('\n')
				}
				i++
			}
			if i+1 < n {
				i += 2
			}
			continue
		}
		if i+1 < n && s[i] == '/' && s[i+1] == '/' {
			i += 2
			for i < n && s[i] != '\n' {
				i++
			}
			continue
		}
		out.WriteByte(s[i])
		i++
	}
	return out.String()
}

var reDefine = regexp.MustCompile(`^\s*#\s*define\s+([A-Za-z_][A-Za-z0-9_]*)\s+(.+)$`)

func (e *extractor) extractDefinesAndStripPP(s string) string {
	var out strings.Builder
	lines := strings.Split(s, "\n")
	for i := 0; i < len(lines); i++ {
		line := lines[i]
		for strings.HasSuffix(strings.TrimRight(line, "\r "), "\\") && i+1 < len(lines) {
			line = strings.TrimSuffix(strings.TrimRight(line, "\r "), "\\") + " " + lines[i+1]
			i++
		}
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "#") {
			if m := reDefine.FindStringSubmatch(trimmed); m != nil {
				name, expr := m[1], strings.TrimSpace(m[2])
				if e.matchesPrefix(name) && !strings.HasSuffix(name, "_H") && !strings.HasSuffix(name, "_H_") && !strings.HasSuffix(name, "_INCLUDED") {
					if val, uval, isU64, typ, ok := e.evalIntExpr(expr); ok {
						e.addConst(name, typ, val, uval, isU64)
					}
				}
			}
			out.WriteByte('\n')
			continue
		}
		out.WriteString(line)
		out.WriteByte('\n')
	}
	return out.String()
}

func (e *extractor) matchesPrefix(name string) bool {
	if e.prefix == "" {
		return true
	}
	return strings.HasPrefix(strings.ToLower(name), strings.ToLower(e.prefix))
}

func (e *extractor) addConst(name, typ string, val int64, uval uint64, isU64 bool) {
	id := sanitizeIdent(name, false)
	if id == "" || e.usedNames[id] || e.usedGoNames[capitalizeFirst(id)] {
		return
	}
	e.usedNames[id] = true
	e.usedGoNames[capitalizeFirst(id)] = true
	e.constVals[name] = val
	e.consts = append(e.consts, parsedConst{
		name:  id,
		typ:   typ,
		value: val,
		uval:  uval,
		isU64: isU64,
	})
}

var (
	reShiftExpr = regexp.MustCompile(`^\(?\s*(\d+)[uUlL]*\s*<<\s*(\d+)[uUlL]*\s*\)?$`)
	reIntLit    = regexp.MustCompile(`^\(?\s*([+-]?(?:0[xX][0-9a-fA-F]+|\d+))([uUlL]*)\s*\)?$`)
)

func (e *extractor) evalIntExpr(expr string) (val int64, uval uint64, isU64 bool, typ string, ok bool) {
	expr = strings.TrimSpace(expr)
	if m := reShiftExpr.FindStringSubmatch(expr); m != nil {
		base, err1 := strconv.ParseUint(m[1], 10, 64)
		shift, err2 := strconv.ParseUint(m[2], 10, 64)
		if err1 == nil && err2 == nil && shift < 63 {
			v := int64(base << shift)
			t := "i32"
			if v > (1<<31)-1 {
				t = "i64"
			}
			return v, uint64(v), false, t, true
		}
	}
	if m := reIntLit.FindStringSubmatch(expr); m != nil {
		lit, suffix := m[1], strings.ToUpper(m[2])
		unsigned := strings.Contains(suffix, "U")
		if unsigned && !strings.HasPrefix(lit, "-") {
			uv, err := strconv.ParseUint(strings.TrimPrefix(lit, "+"), 0, 64)
			if err != nil {
				return 0, 0, false, "", false
			}
			if uv <= (1<<32)-1 && !strings.Contains(suffix, "LL") {
				return int64(uv), uv, false, "u32", true
			}
			return int64(uv), uv, true, "u64", true
		}
		sv, err := strconv.ParseInt(lit, 0, 64)
		if err != nil {
			if uv, errU := strconv.ParseUint(strings.TrimPrefix(lit, "+"), 0, 64); errU == nil {
				return int64(uv), uv, true, "u64", true
			}
			return 0, 0, false, "", false
		}
		if sv >= -(1<<31) && sv <= (1<<31)-1 && !strings.Contains(suffix, "LL") {
			return sv, uint64(sv), false, "i32", true
		}
		return sv, uint64(sv), false, "i64", true
	}
	if v, exists := e.constVals[expr]; exists {
		t := "i32"
		if v < -(1<<31) || v > (1<<31)-1 {
			t = "i64"
		}
		return v, uint64(v), false, t, true
	}
	return 0, 0, false, "", false
}

// unwrapExternC replaces `extern "C" { ... }` wrappers with their inner declarations.
func unwrapExternC(s string) string {
	re := regexp.MustCompile(`extern\s+"C"\s*\{`)
	for {
		loc := re.FindStringIndex(s)
		if loc == nil {
			break
		}
		depth := 1
		i := loc[1]
		for i < len(s) && depth > 0 {
			switch s[i] {
			case '{':
				depth++
			case '}':
				depth--
			}
			i++
		}
		if depth == 0 {
			s = s[:loc[0]] + s[loc[1]:i-1] + s[i:]
		} else {
			s = s[:loc[0]] + s[loc[1]:]
		}
	}
	return s
}

// splitTopLevelDecls splits C code into top-level statements ending in `;` or function bodies ending in `}`.
func splitTopLevelDecls(s string) []string {
	var decls []string
	var cur strings.Builder
	braceDepth := 0
	parenDepth := 0
	for i := 0; i < len(s); i++ {
		ch := s[i]
		cur.WriteByte(ch)
		switch ch {
		case '(':
			parenDepth++
		case ')':
			if parenDepth > 0 {
				parenDepth--
			}
		case '{':
			braceDepth++
		case '}':
			if braceDepth > 0 {
				braceDepth--
			}
			if braceDepth == 0 && parenDepth == 0 {
				rest := strings.TrimLeft(s[i+1:], " \t\r\n")
				if strings.HasPrefix(rest, ";") {
					idx := strings.IndexByte(s[i+1:], ';')
					i += idx + 1
					cur.WriteByte(';')
					if d := strings.TrimSpace(cur.String()); d != "" {
						decls = append(decls, d)
					}
					cur.Reset()
				} else if !startsIdentOrStar(rest) {
					cur.Reset()
				}
			}
		case ';':
			if braceDepth == 0 && parenDepth == 0 {
				if d := strings.TrimSpace(cur.String()); d != "" {
					decls = append(decls, d)
				}
				cur.Reset()
			}
		}
	}
	return decls
}

func startsIdentOrStar(s string) bool {
	if s == "" {
		return false
	}
	ch := s[0]
	return ch == '*' || ch == '_' || (ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z')
}

var (
	reOpaqueTypedefStruct = regexp.MustCompile(`^typedef\s+struct\s+([A-Za-z_][A-Za-z0-9_]*)\s*(\*?)\s*([A-Za-z_][A-Za-z0-9_]*)\s*;$`)
	reForwardStruct       = regexp.MustCompile(`^struct\s+([A-Za-z_][A-Za-z0-9_]*)\s*;$`)
	reSimpleTypedef       = regexp.MustCompile(`^typedef\s+(.+?)\s+(\*?)\s*([A-Za-z_][A-Za-z0-9_]*)\s*;$`)
)

func (e *extractor) classifyDecl(d string) {
	d = stripAttributes(strings.TrimSpace(d))
	if d == "" || d == ";" {
		return
	}
	if strings.Contains(d, "{") && !strings.HasPrefix(d, "typedef") && !strings.HasPrefix(d, "struct ") && !strings.HasPrefix(d, "enum ") && !strings.HasPrefix(d, "union ") {
		return
	}
	if strings.HasPrefix(d, "enum ") || (strings.HasPrefix(d, "typedef") && strings.Contains(d, "enum") && strings.Contains(d, "{")) {
		e.parseEnumDecl(d)
		return
	}
	if m := reOpaqueTypedefStruct.FindStringSubmatch(d); m != nil {
		structTag, star, alias := m[1], m[2], m[3]
		if star == "*" {
			e.pointerTypes[alias] = alias
		} else {
			e.opaqueTypes[alias] = alias + " *"
			e.opaqueTypes["struct "+structTag] = alias + " *"
		}
		return
	}
	if m := reForwardStruct.FindStringSubmatch(d); m != nil {
		tag := m[1]
		if _, exists := e.opaqueTypes["struct "+tag]; !exists {
			e.opaqueTypes["struct "+tag] = "struct " + tag + " *"
		}
		return
	}
	if (strings.HasPrefix(d, "typedef") || strings.HasPrefix(d, "struct ")) && strings.Contains(d, "{") && strings.Contains(d, "}") {
		e.parseStructDecl(d)
		return
	}
	if strings.HasPrefix(d, "typedef ") && !strings.Contains(d, "(") {
		if m := reSimpleTypedef.FindStringSubmatch(d); m != nil {
			target, star, alias := strings.TrimSpace(m[1]), m[2], m[3]
			if star == "*" || strings.HasSuffix(target, "*") {
				e.pointerTypes[alias] = alias
			} else if idlScalar, ok := mapCScalarType(target); ok {
				e.intTypedefs[alias] = idlScalar
			}
		}
		return
	}
	if strings.Contains(d, "(") && strings.HasSuffix(d, ");") && !strings.HasPrefix(d, "typedef") {
		e.parseFuncDecl(d)
	}
}

func stripAttributes(s string) string {
	for _, kw := range []string{"__attribute__", "__declspec", "ZSTD_DEPRECATED", "DEPRECATED"} {
		for {
			idx := strings.Index(s, kw)
			if idx < 0 {
				break
			}
			rest := s[idx+len(kw):]
			openIdx := strings.IndexByte(rest, '(')
			if openIdx < 0 || strings.TrimSpace(rest[:openIdx]) != "" {
				break
			}
			depth := 1
			j := idx + len(kw) + openIdx + 1
			for j < len(s) && depth > 0 {
				if s[j] == '(' {
					depth++
				} else if s[j] == ')' {
					depth--
				}
				j++
			}
			s = s[:idx] + " " + s[j:]
		}
	}
	return s
}

func (e *extractor) parseEnumDecl(d string) {
	openBrace := strings.IndexByte(d, '{')
	closeBrace := strings.LastIndexByte(d, '}')
	if openBrace < 0 || closeBrace <= openBrace {
		return
	}
	after := strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(d[closeBrace+1:]), ";"))
	if after != "" && isCIdent(after) {
		e.intTypedefs[after] = "i32"
	}
	header := strings.TrimSpace(d[:openBrace])
	parts := strings.Fields(header)
	for i := 0; i+1 < len(parts); i++ {
		if parts[i] == "enum" && isCIdent(parts[i+1]) {
			e.intTypedefs["enum "+parts[i+1]] = "i32"
			e.intTypedefs[parts[i+1]] = "i32"
		}
	}

	body := d[openBrace+1 : closeBrace]
	var nextVal int64
	for _, item := range strings.Split(body, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		name := item
		if eq := strings.IndexByte(item, '='); eq >= 0 {
			name = strings.TrimSpace(item[:eq])
			expr := strings.TrimSpace(item[eq+1:])
			if v, _, _, _, ok := e.evalIntExpr(expr); ok {
				nextVal = v
			}
		}
		if isCIdent(name) && e.matchesPrefix(name) {
			e.addConst(name, "i32", nextVal, uint64(nextVal), false)
		}
		nextVal++
	}
}

func (e *extractor) parseStructDecl(d string) {
	openBrace := strings.IndexByte(d, '{')
	closeBrace := strings.LastIndexByte(d, '}')
	if openBrace < 0 || closeBrace <= openBrace {
		return
	}
	header := strings.TrimSpace(d[:openBrace])
	after := strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(d[closeBrace+1:]), ";"))

	var cTypeName string
	var baseName string
	if strings.HasPrefix(header, "typedef") && isCIdent(after) {
		cTypeName = after
		baseName = after
		hfields := strings.Fields(header)
		for i := 0; i+1 < len(hfields); i++ {
			if hfields[i] == "struct" && isCIdent(hfields[i+1]) {
				delete(e.opaqueTypes, "struct "+hfields[i+1])
			}
		}
		delete(e.opaqueTypes, after)
	} else {
		hfields := strings.Fields(header)
		for i := 0; i+1 < len(hfields); i++ {
			if hfields[i] == "struct" && isCIdent(hfields[i+1]) {
				cTypeName = "struct " + hfields[i+1]
				baseName = hfields[i+1]
				delete(e.opaqueTypes, cTypeName)
				break
			}
		}
	}
	if cTypeName == "" {
		return
	}

	body := d[openBrace+1 : closeBrace]
	var fields []parsedField
	allFixed := true
	for _, stmt := range strings.Split(body, ";") {
		stmt = strings.TrimSpace(stmt)
		if stmt == "" {
			continue
		}
		p, ok := e.parseOneCParam(stmt)
		if !ok || p.name == "" || p.ptrDepth > 0 {
			allFixed = false
			break
		}
		idlType, ok := e.mapScalarOrFixedField(p)
		if !ok {
			allFixed = false
			break
		}
		fname := sanitizeIdent(p.name, false)
		if fname == "" {
			allFixed = false
			break
		}
		fields = append(fields, parsedField{name: fname, typ: idlType})
	}
	if !allFixed || len(fields) == 0 {
		e.opaqueTypes[baseName] = cTypeName + " *"
		e.opaqueTypes[cTypeName] = cTypeName + " *"
		return
	}

	stripped := strings.TrimSuffix(strings.TrimSuffix(e.stripPrefix(baseName), "_t"), "_s")
	if stripped == "" {
		stripped = baseName
	}
	idlName := e.uniqueTypeName(toCamelCase(stripped))
	st := &parsedStruct{
		cName:   cTypeName,
		idlName: idlName,
		fields:  fields,
	}
	e.structDefs[baseName] = st
	e.structDefs[cTypeName] = st
	e.structOrder = append(e.structOrder, cTypeName)
}

func (e *extractor) mapScalarOrFixedField(p rawCParam) (string, bool) {
	scalar, ok := e.resolveScalar(p.baseType)
	if !ok {
		if st, exists := e.structDefs[p.baseType]; exists {
			scalar = st.idlName
		} else {
			return "", false
		}
	}
	if p.arraySize > 0 {
		return fmt.Sprintf("%s[%d]", scalar, p.arraySize), true
	}
	return scalar, true
}

func (e *extractor) parseFuncDecl(d string) {
	d = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(d), ";"))
	openParen := strings.IndexByte(d, '(')
	closeParen := strings.LastIndexByte(d, ')')
	if openParen <= 0 || closeParen < openParen {
		return
	}
	prefixPart := strings.TrimSpace(d[:openParen])
	paramPart := strings.TrimSpace(d[openParen+1 : closeParen])

	fnName, retBase, retPtr, retConst, ok := parseFuncHead(prefixPart)
	if !ok || fnName == "" {
		return
	}
	if strings.HasPrefix(fnName, "_") {
		return
	}

	var params []rawCParam
	if paramPart != "" && paramPart != "void" {
		rawParams := splitParams(paramPart)
		for idx, rp := range rawParams {
			rp = strings.TrimSpace(rp)
			if rp == "..." {
				params = append(params, rawCParam{name: "varargs", baseType: "..."})
				continue
			}
			if strings.Contains(rp, "(") {
				params = append(params, rawCParam{name: fmt.Sprintf("arg%d", idx+1), baseType: "fnptr"})
				continue
			}
			p, ok := e.parseOneCParam(rp)
			if !ok {
				params = append(params, rawCParam{name: fmt.Sprintf("arg%d", idx+1), baseType: "unparsed"})
				continue
			}
			if p.name == "" {
				p.name = fmt.Sprintf("arg%d", idx+1)
			}
			params = append(params, p)
		}
	}

	e.funcs = append(e.funcs, parsedFunc{
		cName:    fnName,
		retType:  retBase,
		retPtr:   retPtr,
		retConst: retConst,
		params:   params,
	})
}

func splitParams(s string) []string {
	var out []string
	var cur strings.Builder
	depth := 0
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '(':
			depth++
		case ')':
			if depth > 0 {
				depth--
			}
		case ',':
			if depth == 0 {
				out = append(out, cur.String())
				cur.Reset()
				continue
			}
		}
		cur.WriteByte(s[i])
	}
	if cur.Len() > 0 {
		out = append(out, cur.String())
	}
	return out
}

var ignoredQualifiers = map[string]bool{
	"extern":              true,
	"static":              true,
	"inline":              true,
	"__inline":            true,
	"__inline__":          true,
	"_Noreturn":           true,
	"__cdecl":             true,
	"__stdcall":           true,
	"WINAPI":              true,
	"APIENTRY":            true,
	"SQLITE_API":          true,
	"SQLITE_EXTERN":       true,
	"SQLITE_STDCALL":      true,
	"SQLITE_CDECL":        true,
	"SQLITE_EXPERIMENTAL": true,
	"ZSTDLIB_API":         true,
	"ZSTDLIB_VISIBLE":     true,
	"ZSTDERRORLIB_API":    true,
	"HQQ_API":             true,
	"restrict":            true,
	"__restrict":          true,
	"__restrict__":        true,
	"volatile":            true,
}

func parseFuncHead(head string) (fnName, baseType string, ptrDepth int, isConst bool, ok bool) {
	norm := strings.ReplaceAll(head, "*", " * ")
	tokens := strings.Fields(norm)
	if len(tokens) < 2 {
		return "", "", 0, false, false
	}
	for _, t := range tokens {
		if t == "static" || t == "inline" || t == "__inline" || t == "__inline__" {
			return "", "", 0, false, false
		}
	}
	fnName = tokens[len(tokens)-1]
	if !isCIdent(fnName) {
		return "", "", 0, false, false
	}
	typeTokens := tokens[:len(tokens)-1]
	baseType, ptrDepth, isConst = normalizeCTypeTokens(typeTokens)
	if baseType == "" {
		return "", "", 0, false, false
	}
	return fnName, baseType, ptrDepth, isConst, true
}

func (e *extractor) parseOneCParam(decl string) (rawCParam, bool) {
	var arraySize int
	if openBr := strings.IndexByte(decl, '['); openBr >= 0 {
		closeBr := strings.IndexByte(decl[openBr:], ']')
		if closeBr < 0 {
			return rawCParam{}, false
		}
		inside := strings.TrimSpace(decl[openBr+1 : openBr+closeBr])
		decl = decl[:openBr] + decl[openBr+closeBr+1:]
		if inside == "" {
			decl += " *"
		} else if n, err := strconv.Atoi(inside); err == nil && n > 0 {
			arraySize = n
		} else if v, exists := e.constVals[inside]; exists && v > 0 {
			arraySize = int(v)
		} else {
			return rawCParam{}, false
		}
	}

	norm := strings.ReplaceAll(decl, "*", " * ")
	tokens := strings.Fields(norm)
	if len(tokens) == 0 {
		return rawCParam{}, false
	}

	last := tokens[len(tokens)-1]
	var paramName string
	typeTokens := tokens
	if last != "*" && !isCTypeKeyword(last) && len(tokens) >= 2 {
		prev := tokens[len(tokens)-2]
		if prev != "struct" && prev != "enum" && prev != "union" {
			paramName = last
			typeTokens = tokens[:len(tokens)-1]
		}
	}
	baseType, ptrDepth, isConst := normalizeCTypeTokens(typeTokens)
	if baseType == "" {
		return rawCParam{}, false
	}
	return rawCParam{
		name:      paramName,
		cType:     decl,
		isConst:   isConst,
		ptrDepth:  ptrDepth,
		baseType:  baseType,
		arraySize: arraySize,
	}, true
}

func isCTypeKeyword(t string) bool {
	switch t {
	case "void", "char", "short", "int", "long", "float", "double", "signed", "unsigned",
		"bool", "_Bool", "size_t", "ssize_t", "intptr_t", "uintptr_t", "ptrdiff_t",
		"int8_t", "int16_t", "int32_t", "int64_t",
		"uint8_t", "uint16_t", "uint32_t", "uint64_t",
		"const", "volatile", "struct", "enum", "union":
		return true
	}
	return false
}

func normalizeCTypeTokens(tokens []string) (baseType string, ptrDepth int, isConst bool) {
	var clean []string
	for _, t := range tokens {
		if t == "*" {
			ptrDepth++
			continue
		}
		if t == "const" {
			isConst = true
			continue
		}
		if ignoredQualifiers[t] {
			continue
		}
		clean = append(clean, t)
	}
	for len(clean) > 1 && isAllCapsMacro(clean[0]) && !isCTypeKeyword(clean[0]) {
		clean = clean[1:]
	}
	for len(clean) > 1 && isAllCapsMacro(clean[len(clean)-1]) && !isCTypeKeyword(clean[len(clean)-1]) {
		clean = clean[:len(clean)-1]
	}
	return strings.Join(clean, " "), ptrDepth, isConst
}

func isAllCapsMacro(s string) bool {
	if len(s) < 2 {
		return false
	}
	hasUpper := false
	for i := 0; i < len(s); i++ {
		ch := s[i]
		if ch >= 'A' && ch <= 'Z' {
			hasUpper = true
		} else if !(ch == '_' || (ch >= '0' && ch <= '9')) {
			return false
		}
	}
	return hasUpper && (strings.HasSuffix(s, "_API") || strings.HasSuffix(s, "_EXPORT") || strings.HasSuffix(s, "_CALL") || strings.HasSuffix(s, "EXTERN") || strings.HasSuffix(s, "VISIBLE"))
}

func (e *extractor) resolveScalar(cType string) (string, bool) {
	if s, ok := mapCScalarType(cType); ok {
		return s, true
	}
	if s, ok := e.intTypedefs[cType]; ok {
		return s, true
	}
	return "", false
}

func mapCScalarType(cType string) (string, bool) {
	switch cType {
	case "bool", "_Bool":
		return "bool", true
	case "int8_t", "signed char":
		return "i8", true
	case "uint8_t", "unsigned char":
		return "u8", true
	case "int16_t", "short", "short int", "signed short", "signed short int":
		return "i16", true
	case "uint16_t", "unsigned short", "unsigned short int":
		return "u16", true
	case "int32_t", "int", "signed", "signed int":
		return "i32", true
	case "uint32_t", "unsigned", "unsigned int":
		return "u32", true
	case "int64_t", "long", "long int", "long long", "long long int", "signed long", "signed long long", "ssize_t", "ptrdiff_t", "intptr_t", "sqlite3_int64", "sqlite_int64":
		return "i64", true
	case "uint64_t", "unsigned long", "unsigned long int", "unsigned long long", "unsigned long long int", "uintptr_t", "sqlite3_uint64", "sqlite_uint64":
		return "u64", true
	case "size_t":
		return "size", true
	case "float":
		return "f32", true
	case "double":
		return "f64", true
	}
	return "", false
}

// buildHandles identifies opaque handle types used by functions, pairs them with destructors and
// @cancel functions, and infers `owned_by` parent relationships.
func (e *extractor) buildHandles() {
	e.handles = map[string]*parsedHandle{}

	ensureHandle := func(baseType, cTypeSpelling string) *parsedHandle {
		if h, ok := e.handles[baseType]; ok {
			return h
		}
		rawName := strings.TrimPrefix(baseType, "struct ")
		stripped := e.stripPrefix(rawName)
		stripped = strings.TrimSuffix(strings.TrimSuffix(stripped, "_t"), "_s")
		if stripped == "" {
			stripped = rawName
		}
		idlName := toCamelCase(stripped)
		if strings.EqualFold(idlName, e.svc) && strings.Contains(strings.ToLower(e.svc), "sqlite") {
			idlName = "Db"
		}
		idlName = e.uniqueTypeName(idlName)
		h := &parsedHandle{
			cType:    cTypeSpelling,
			baseName: baseType,
			idlName:  idlName,
		}
		e.handles[baseType] = h
		e.handleOrder = append(e.handleOrder, baseType)
		return h
	}

	for _, fn := range e.funcs {
		if !e.matchesPrefix(fn.cName) {
			continue
		}
		if fn.retPtr == 1 {
			if cSpell, ok := e.opaqueTypes[fn.retType]; ok {
				ensureHandle(fn.retType, cSpell)
			}
		} else if fn.retPtr == 0 {
			if cSpell, ok := e.pointerTypes[fn.retType]; ok {
				ensureHandle(fn.retType, cSpell)
			}
		}
		for _, p := range fn.params {
			if p.ptrDepth == 1 || p.ptrDepth == 2 {
				if cSpell, ok := e.opaqueTypes[p.baseType]; ok {
					ensureHandle(p.baseType, cSpell)
				} else if strings.HasPrefix(p.baseType, "struct ") {
					if _, isStruct := e.structDefs[p.baseType]; !isStruct {
						ensureHandle(p.baseType, p.baseType+" *")
					}
				}
			} else if p.ptrDepth == 0 || p.ptrDepth == 1 {
				if cSpell, ok := e.pointerTypes[p.baseType]; ok {
					ensureHandle(p.baseType, cSpell)
				}
			}
		}
	}

	usedDestroy := map[string]bool{}
	for _, base := range e.handleOrder {
		h := e.handles[base]
		var bestDestroy string
		var bestScore int
		for _, fn := range e.funcs {
			if !e.matchesPrefix(fn.cName) {
				continue
			}
			if len(fn.params) != 1 || !e.isHandleParam(fn.params[0], base) {
				continue
			}
			lower := strings.ToLower(fn.cName)
			if fn.retType == "void" && fn.retPtr == 0 && (strings.HasSuffix(lower, "interrupt") || strings.HasSuffix(lower, "cancel")) {
				if h.cancelFn == "" {
					h.cancelFn = fn.cName
				}
				continue
			}
			if usedDestroy[fn.cName] {
				continue
			}
			retOK := (fn.retType == "void" && fn.retPtr == 0)
			if !retOK && fn.retPtr == 0 {
				if sc, ok := e.resolveScalar(fn.retType); ok && isIDLInt(sc) {
					retOK = true
				}
			}
			if !retOK {
				continue
			}
			score := destroyMatchScore(fn.cName, h.idlName, base)
			if score > bestScore {
				bestScore = score
				bestDestroy = fn.cName
			}
		}
		if bestDestroy != "" {
			h.destroyFn = sanitizeIdent(bestDestroy, false)
			usedDestroy[bestDestroy] = true
		} else {
			shimName := sanitizeIdent("destroy_"+strings.ToLower(h.idlName), false)
			for e.usedNames[shimName] {
				shimName += "_h"
			}
			e.usedNames[shimName] = true
			h.destroyFn = shimName
			h.synthShim = true
		}
	}

	// Infer `owned_by` (Rule V16).
	for _, base := range e.handleOrder {
		h := e.handles[base]
		var parentCandidate string
		producers := 0
		consistent := true
		for _, fn := range e.funcs {
			if !e.matchesPrefix(fn.cName) {
				continue
			}
			producesH := (fn.retPtr == 1 && fn.retType == base) || (fn.retPtr == 0 && e.pointerTypes[fn.retType] != "" && fn.retType == base)
			for _, p := range fn.params {
				if e.isOutHandleParam(p, base) {
					producesH = true
				}
			}
			if !producesH {
				continue
			}
			producers++
			var inHandles []string
			for _, p := range fn.params {
				if hb, ok := e.paramInHandleBase(p); ok && hb != base {
					inHandles = append(inHandles, hb)
				}
			}
			if len(inHandles) != 1 {
				consistent = false
				break
			}
			if parentCandidate == "" {
				parentCandidate = inHandles[0]
			} else if parentCandidate != inHandles[0] {
				consistent = false
				break
			}
		}
		if producers > 0 && consistent && parentCandidate != "" {
			parentH := e.handles[parentCandidate]
			if parentH != nil && parentH.ownedBy != h.idlName {
				h.ownedBy = parentH.idlName
			}
		}
	}
}

func (e *extractor) isHandleParam(p rawCParam, base string) bool {
	if _, isPtrTypedef := e.pointerTypes[base]; isPtrTypedef {
		return p.baseType == base && p.ptrDepth == 0
	}
	return p.baseType == base && p.ptrDepth == 1
}

func (e *extractor) isOutHandleParam(p rawCParam, base string) bool {
	if _, isPtrTypedef := e.pointerTypes[base]; isPtrTypedef {
		return p.baseType == base && p.ptrDepth == 1 && !p.isConst
	}
	return p.baseType == base && p.ptrDepth == 2 && !p.isConst
}

func (e *extractor) paramInHandleBase(p rawCParam) (string, bool) {
	if p.ptrDepth == 1 {
		if _, ok := e.handles[p.baseType]; ok {
			if _, isPtr := e.pointerTypes[p.baseType]; !isPtr {
				return p.baseType, true
			}
		}
	} else if p.ptrDepth == 0 {
		if _, ok := e.handles[p.baseType]; ok {
			if _, isPtr := e.pointerTypes[p.baseType]; isPtr {
				return p.baseType, true
			}
		}
	}
	return "", false
}

func destroyMatchScore(fnName, idlHandleName, baseName string) int {
	lower := strings.ToLower(fnName)
	isDestroyVerb := false
	for _, kw := range []string{"free", "close", "destroy", "finalize", "dispose", "release", "delete"} {
		if strings.Contains(lower, kw) {
			isDestroyVerb = true
			break
		}
	}
	if !isDestroyVerb {
		return 0
	}
	score := 10
	if strings.HasSuffix(lower, "_v2") || strings.HasSuffix(lower, "v2") {
		score -= 3
	}
	hLower := strings.ToLower(idlHandleName)
	if strings.Contains(lower, hLower) {
		score += 20
	}
	bLower := strings.ToLower(strings.TrimPrefix(baseName, "struct "))
	if strings.Contains(lower, bLower) {
		score += 10
	}
	return score
}

func isIDLInt(t string) bool {
	switch t {
	case "i8", "i16", "i32", "i64", "u8", "u16", "u32", "u64", "size":
		return true
	}
	return false
}

func (e *extractor) emitIDL() []byte {
	var b bytes.Buffer
	fmt.Fprintf(&b, "// Code bootstrapped by hqq-bindgen -from-header from %s.\n", filepath.Base(e.path))
	fmt.Fprintf(&b, "// Review buffer bounds, error conventions (@error), and security interceptors (@intercept)\n")
	fmt.Fprintf(&b, "// before compiling generated server code into production.\n")
	fmt.Fprintf(&b, "idl 1;\n\n")
	if e.serializeNone {
		fmt.Fprintf(&b, "@serialize(none)\n")
	}
	fmt.Fprintf(&b, "service %s;\n\n", e.svc)
	fmt.Fprintf(&b, "include %q;\n\n", e.inc)

	maxStrConst := "MAX_STR_BYTES"
	maxBufConst := "MAX_BUF_BYTES"
	for e.usedNames[maxStrConst] || e.usedGoNames[capitalizeFirst(maxStrConst)] {
		maxStrConst = "DEFAULT_" + maxStrConst
	}
	e.usedNames[maxStrConst] = true
	e.usedGoNames[capitalizeFirst(maxStrConst)] = true
	for e.usedNames[maxBufConst] || e.usedGoNames[capitalizeFirst(maxBufConst)] {
		maxBufConst = "DEFAULT_" + maxBufConst
	}
	e.usedNames[maxBufConst] = true
	e.usedGoNames[capitalizeFirst(maxBufConst)] = true

	fmt.Fprintf(&b, "const %s: u32 = %d;\n", maxStrConst, e.maxStr)
	fmt.Fprintf(&b, "const %s: u32 = %d;\n", maxBufConst, e.maxBytes)

	if len(e.consts) > 0 {
		fmt.Fprintf(&b, "\n// Extracted C constants and enum values.\n")
		for _, c := range e.consts {
			if c.isU64 {
				fmt.Fprintf(&b, "const %s: %s = %d;\n", c.name, c.typ, c.uval)
			} else {
				fmt.Fprintf(&b, "const %s: %s = %d;\n", c.name, c.typ, c.value)
			}
		}
	}

	if len(e.handleOrder) > 0 {
		fmt.Fprintf(&b, "\n// Opaque handle types.\n")
		for _, base := range e.handleOrder {
			h := e.handles[base]
			fmt.Fprintf(&b, "@c(%q)\n", h.cType)
			if h.cancelFn != "" {
				fmt.Fprintf(&b, "@cancel(%s)\n", h.cancelFn)
			}
			if h.ownedBy != "" {
				fmt.Fprintf(&b, "handle %s owned_by %s destroy %s;\n\n", h.idlName, h.ownedBy, h.destroyFn)
			} else {
				fmt.Fprintf(&b, "handle %s destroy %s;\n\n", h.idlName, h.destroyFn)
			}
		}
	}

	if len(e.structOrder) > 0 {
		fmt.Fprintf(&b, "// Fixed-layout structs.\n")
		for _, key := range e.structOrder {
			st := e.structDefs[key]
			fmt.Fprintf(&b, "@c(%q)\n", st.cName)
			fmt.Fprintf(&b, "struct %s {\n", st.idlName)
			for _, f := range st.fields {
				fmt.Fprintf(&b, "    %s: %s;\n", f.name, f.typ)
			}
			fmt.Fprintf(&b, "}\n\n")
		}
	}

	for _, base := range e.handleOrder {
		h := e.handles[base]
		if h.synthShim {
			fmt.Fprintf(&b, "// TODO: implement %s in your C hooks file or replace with the library destructor.\n", h.destroyFn)
			fmt.Fprintf(&b, "shim fn %s(h: %s);\n\n", h.destroyFn, h.idlName)
		}
	}

	destroySet := map[string]*parsedHandle{}
	cancelSet := map[string]bool{}
	for _, base := range e.handleOrder {
		h := e.handles[base]
		if !h.synthShim {
			destroySet[h.destroyFn] = h
		}
		if h.cancelFn != "" {
			cancelSet[h.cancelFn] = true
		}
	}

	fmt.Fprintf(&b, "// Functions.\n")
	for _, fn := range e.funcs {
		if !e.matchesPrefix(fn.cName) {
			continue
		}
		if cancelSet[fn.cName] {
			fmt.Fprintf(&b, "// %s is wired as @cancel above.\n", fn.cName)
			continue
		}
		line, reason := e.formatFunc(fn, destroySet[fn.cName], maxStrConst, maxBufConst)
		if reason != "" {
			fmt.Fprintf(&b, "// skipped %s: %s\n", fn.cName, reason)
			continue
		}
		fmt.Fprintf(&b, "%s\n", line)
	}

	return b.Bytes()
}

func (e *extractor) formatFunc(fn parsedFunc, destroysHandle *parsedHandle, maxStrConst, maxBufConst string) (string, string) {
	fnID := sanitizeIdent(fn.cName, false)
	if fnID == "" || fnID != fn.cName {
		return "", "function name is not a valid IDL identifier"
	}
	goFnName := toCamelCase(fnID)
	if e.usedNames[fnID] || e.usedGoNames[goFnName] || e.usedGoNames[goFnName+"Async"] || e.usedGoNames[goFnName+"Future"] {
		return "", "name collides with an earlier declaration or reserved Go client method"
	}

	for _, p := range fn.params {
		if p.baseType == "..." {
			return "", "variadic C function; wrap a fixed-arity C shim if needed"
		}
		if p.baseType == "fnptr" {
			return "", "takes a C function-pointer callback; wrap with a C shim if needed"
		}
		if p.baseType == "unparsed" {
			return "", "complex C parameter declaration; wrap with a C shim if needed"
		}
	}

	var retIDL string
	var anns []string
	returnsOutBufLen := false

	switch {
	case fn.retType == "void" && fn.retPtr == 0:
		retIDL = ""
	case fn.retPtr == 0:
		if h, ok := e.handles[fn.retType]; ok {
			if _, isPtr := e.pointerTypes[fn.retType]; isPtr {
				retIDL = h.idlName
				anns = append(anns, "@error(null)")
			}
		}
		if retIDL == "" {
			if sc, ok := e.resolveScalar(fn.retType); ok {
				retIDL = sc
			} else if st, ok := e.structDefs[fn.retType]; ok {
				retIDL = st.idlName
			} else {
				return "", fmt.Sprintf("unmapped return type %q", fn.retType)
			}
		}
	case fn.retPtr == 1:
		if h, ok := e.handles[fn.retType]; ok {
			retIDL = h.idlName
			anns = append(anns, "@error(null)")
		} else if fn.retType == "char" && fn.retConst {
			retIDL = "str<=" + maxStrConst
		} else {
			return "", fmt.Sprintf("raw pointer return `%s *`; declare a handle or wrap with a shim", fn.retType)
		}
	default:
		return "", fmt.Sprintf("multi-level pointer return `%s`", fn.retType)
	}

	type mappedParam struct {
		dir      string // "", "out", "inout"
		name     string
		typ      string
		attr     string
		isOutBuf bool
	}
	var mparams []mappedParam
	usedParamNames := map[string]int{}
	uniqueParamName := func(raw string, idx int) string {
		id := sanitizeIdent(raw, false)
		if id == "" || id == "ret" {
			id = fmt.Sprintf("arg%d", idx+1)
		}
		usedParamNames[id]++
		if usedParamNames[id] > 1 {
			id = fmt.Sprintf("%s_%d", id, usedParamNames[id])
		}
		return id
	}

	pnames := make([]string, len(fn.params))
	for i, p := range fn.params {
		pnames[i] = uniqueParamName(p.name, i)
	}

	var producedHandles []*parsedHandle
	inHandleNames := map[string]int{}
	if fn.retPtr == 1 {
		if h, ok := e.handles[fn.retType]; ok {
			producedHandles = append(producedHandles, h)
		}
	} else if fn.retPtr == 0 {
		if h, ok := e.handles[fn.retType]; ok {
			if _, isPtr := e.pointerTypes[fn.retType]; isPtr {
				producedHandles = append(producedHandles, h)
			}
		}
	}

	firstOutBufName := ""
	lastStrParamName := ""
	i := 0
	for i < len(fn.params) {
		p := fn.params[i]
		pname := pnames[i]

		// 1. In handle parameter?
		if hb, ok := e.paramInHandleBase(p); ok {
			h := e.handles[hb]
			inHandleNames[h.idlName]++
			mparams = append(mparams, mappedParam{name: pname, typ: h.idlName})
			i++
			continue
		}
		// 2. Out handle parameter (`handle_t **out`)?
		if p.ptrDepth == 2 && !p.isConst {
			if h, ok := e.handles[p.baseType]; ok {
				producedHandles = append(producedHandles, h)
				mparams = append(mparams, mappedParam{dir: "out", name: pname, typ: h.idlName})
				i++
				continue
			}
		}
		if p.ptrDepth == 1 && !p.isConst {
			if h, ok := e.handles[p.baseType]; ok {
				if _, isPtr := e.pointerTypes[p.baseType]; isPtr {
					producedHandles = append(producedHandles, h)
					mparams = append(mparams, mappedParam{dir: "out", name: pname, typ: h.idlName})
					i++
					continue
				}
			}
		}
		// 2b. `const char **tail` offset into preceding `const char *` input string?
		if p.ptrDepth == 2 && p.isConst && p.baseType == "char" && lastStrParamName != "" {
			mparams = append(mparams, mappedParam{
				dir:  "out",
				name: pname,
				typ:  "u32",
				attr: fmt.Sprintf("@offset_in(%s)", lastStrParamName),
			})
			i++
			continue
		}

		// 3. `const char *` string parameter?
		if p.ptrDepth == 1 && p.isConst && p.baseType == "char" {
			lastStrParamName = pname
			mparams = append(mparams, mappedParam{name: pname, typ: "str<=" + maxStrConst})
			if i+1 < len(fn.params) && isLengthCompanion(pname, pnames[i+1], fn.params[i+1], e) {
				sc, _ := e.resolveScalar(fn.params[i+1].baseType)
				mparams = append(mparams, mappedParam{
					name: pnames[i+1],
					typ:  sc,
					attr: fmt.Sprintf("@len_of(%s)", pname),
				})
				i += 2
				continue
			}
			i++
			continue
		}

		// 4. Byte buffer `(const void *buf, size_t len)` or `(void *dst, size_t cap)`?
		if p.ptrDepth == 1 && isByteBufferBase(p.baseType, p.isConst) {
			if i+1 < len(fn.params) && fn.params[i+1].ptrDepth == 0 {
				if sc, ok := e.resolveScalar(fn.params[i+1].baseType); ok && isIDLInt(sc) {
					dir := ""
					if !p.isConst {
						dir = "out"
						if firstOutBufName == "" {
							firstOutBufName = pname
						}
					}
					mparams = append(mparams, mappedParam{
						dir:      dir,
						name:     pname,
						typ:      "bytes<=" + maxBufConst,
						isOutBuf: dir == "out",
					})
					mparams = append(mparams, mappedParam{
						name: pnames[i+1],
						typ:  sc,
						attr: fmt.Sprintf("@len_of(%s)", pname),
					})
					i += 2
					continue
				}
			}
			return "", fmt.Sprintf("pointer parameter `%s` (`%s *`) has no paired length scalar", pname, p.baseType)
		}

		// 5. Pointer to scalar (`const T *` with length -> array, or `T *` -> `out` scalar)?
		if p.ptrDepth == 1 {
			if sc, ok := e.resolveScalar(p.baseType); ok {
				if p.isConst {
					if i+1 < len(fn.params) && fn.params[i+1].ptrDepth == 0 {
						if lenSc, okL := e.resolveScalar(fn.params[i+1].baseType); okL && isIDLInt(lenSc) {
							mparams = append(mparams, mappedParam{
								name: pname,
								typ:  fmt.Sprintf("%s[]<=%d", sc, 65536),
							})
							mparams = append(mparams, mappedParam{
								name: pnames[i+1],
								typ:  lenSc,
								attr: fmt.Sprintf("@len_of(%s)", pname),
							})
							i += 2
							continue
						}
					}
					return "", fmt.Sprintf("const pointer parameter `%s` (`const %s *`) has no paired length scalar", pname, p.baseType)
				}
				mparams = append(mparams, mappedParam{dir: "out", name: pname, typ: sc})
				i++
				continue
			}
			if st, ok := e.structDefs[p.baseType]; ok && !p.isConst {
				mparams = append(mparams, mappedParam{dir: "out", name: pname, typ: st.idlName})
				i++
				continue
			}
			return "", fmt.Sprintf("unsupported pointer parameter `%s` (`%s *`)", pname, p.baseType)
		}

		// 6. Value parameter (scalar or fixed array).
		if p.ptrDepth == 0 {
			if sc, ok := e.resolveScalar(p.baseType); ok {
				typ := sc
				if p.arraySize > 0 {
					typ = fmt.Sprintf("%s[%d]", sc, p.arraySize)
				}
				mparams = append(mparams, mappedParam{name: pname, typ: typ})
				i++
				continue
			}
			if _, ok := e.structDefs[p.baseType]; ok {
				return "", fmt.Sprintf("struct value parameter `%s` (`%s`); HQQ IDL rule V38 requires flattening or a C shim for struct in-parameters", pname, p.baseType)
			}
		}

		return "", fmt.Sprintf("unmapped parameter `%s` (`%s`)", pname, p.cType)
	}

	for _, h := range producedHandles {
		if h.ownedBy != "" && inHandleNames[h.ownedBy] != 1 {
			return "", fmt.Sprintf("creates %s (owned_by %s) without a single %s in-parameter", h.idlName, h.ownedBy, h.ownedBy)
		}
	}

	if destroysHandle != nil {
		if len(mparams) != 1 || mparams[0].dir != "" || mparams[0].typ != destroysHandle.idlName {
			return "", fmt.Sprintf("destructor of %s does not match single in-parameter shape", destroysHandle.idlName)
		}
		if retIDL != "" && isIDLInt(retIDL) {
			anns = append(anns, "@error(status, ok(0))")
		}
	} else if firstOutBufName != "" && retIDL == "size" {
		returnsOutBufLen = true
	}

	e.usedNames[fnID] = true
	e.usedGoNames[goFnName] = true
	e.usedGoNames[goFnName+"Async"] = true
	e.usedGoNames[goFnName+"Future"] = true

	var sb strings.Builder
	for _, a := range anns {
		sb.WriteString(a)
		sb.WriteByte('\n')
	}
	sb.WriteString("fn ")
	sb.WriteString(fnID)
	sb.WriteByte('(')
	for idx, mp := range mparams {
		if idx > 0 {
			sb.WriteString(", ")
		}
		if mp.dir != "" {
			sb.WriteString(mp.dir)
			sb.WriteByte(' ')
		}
		sb.WriteString(mp.name)
		sb.WriteString(": ")
		sb.WriteString(mp.typ)
		if mp.attr != "" {
			sb.WriteByte(' ')
			sb.WriteString(mp.attr)
		}
	}
	sb.WriteByte(')')
	if retIDL != "" {
		sb.WriteString(" -> ")
		sb.WriteString(retIDL)
		if returnsOutBufLen {
			fmt.Fprintf(&sb, " @len_of(%s)", firstOutBufName)
		}
	}
	sb.WriteByte(';')
	return sb.String(), ""
}

func isByteBufferBase(baseType string, isConst bool) bool {
	switch baseType {
	case "void", "uint8_t", "unsigned char":
		return true
	case "char":
		return !isConst
	}
	return false
}

func isLengthCompanion(strParamName, nextParamName string, next rawCParam, e *extractor) bool {
	if next.ptrDepth != 0 {
		return false
	}
	sc, ok := e.resolveScalar(next.baseType)
	if !ok || !isIDLInt(sc) {
		return false
	}
	low := strings.ToLower(nextParamName)
	sLow := strings.ToLower(strParamName)
	for _, kw := range []string{"len", "size", "nbyte", "bytes", "length", "cap", "count"} {
		if low == kw || strings.HasSuffix(low, "_"+kw) || strings.HasPrefix(low, kw+"_") || low == sLow+kw || low == "n"+sLow {
			return true
		}
	}
	return low == "n"
}

func (e *extractor) stripPrefix(s string) string {
	if e.prefix != "" && len(s) > len(e.prefix) && strings.EqualFold(s[:len(e.prefix)], e.prefix) {
		return strings.TrimLeft(s[len(e.prefix):], "_")
	}
	if len(s) > len(e.svc) && strings.EqualFold(s[:len(e.svc)], e.svc) && s[len(e.svc)] == '_' {
		return strings.TrimLeft(s[len(e.svc)+1:], "_")
	}
	return s
}

func (e *extractor) uniqueTypeName(base string) string {
	id := sanitizeIdent(base, true)
	if id == "" {
		id = "Type"
	}
	cand := id
	for i := 2; e.usedNames[cand] || e.usedGoNames[capitalizeFirst(cand)]; i++ {
		cand = fmt.Sprintf("%s%d", id, i)
	}
	e.usedNames[cand] = true
	e.usedGoNames[capitalizeFirst(cand)] = true
	return cand
}

func capitalizeFirst(s string) string {
	if s == "" {
		return ""
	}
	r := []rune(s)
	r[0] = unicode.ToUpper(r[0])
	return string(r)
}

func deriveServiceName(filename string) string {
	ext := filepath.Ext(filename)
	stem := strings.TrimSuffix(filename, ext)
	stem = strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			return r
		}
		if r >= 'A' && r <= 'Z' {
			return unicode.ToLower(r)
		}
		return '_'
	}, stem)
	id := sanitizeIdent(stem, false)
	if id == "" {
		return "service_api"
	}
	return id
}

func sanitizeInclude(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		ch := s[i]
		if (ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z') || (ch >= '0' && ch <= '9') || strings.IndexByte("_./+-", ch) >= 0 {
			b.WriteByte(ch)
		} else {
			b.WriteByte('_')
		}
	}
	res := strings.TrimLeft(b.String(), "/")
	if res == "" {
		return "library.h"
	}
	return res
}

var idlReserved = map[string]bool{
	"idl": true, "service": true, "include": true, "const": true, "struct": true, "union": true,
	"case": true, "handle": true, "owned_by": true, "destroy": true, "fn": true, "shim": true,
	"in": true, "out": true, "inout": true, "ret": true, "bool": true, "i8": true, "i16": true,
	"i32": true, "i64": true, "u8": true, "u16": true, "u32": true, "u64": true, "size": true,
	"f32": true, "f64": true, "str": true, "bytes": true,
	"auto": true, "break": true, "char": true, "continue": true, "default": true, "do": true,
	"double": true, "else": true, "enum": true, "extern": true, "float": true, "for": true,
	"goto": true, "if": true, "inline": true, "int": true, "long": true, "register": true,
	"restrict": true, "return": true, "short": true, "signed": true, "sizeof": true, "static": true,
	"switch": true, "typedef": true, "unsigned": true, "void": true, "volatile": true, "while": true,
	"_Alignas": true, "_Alignof": true, "_Atomic": true, "_Bool": true, "_Complex": true,
	"_Generic": true, "_Imaginary": true, "_Noreturn": true, "_Static_assert": true, "_Thread_local": true,
	"alignas": true, "alignof": true, "false": true, "static_assert": true, "thread_local": true, "true": true,
}

func sanitizeIdent(s string, camel bool) string {
	s = strings.TrimSpace(s)
	for strings.Contains(s, "__") {
		s = strings.ReplaceAll(s, "__", "_")
	}
	s = strings.Trim(s, "_")
	if s == "" {
		return ""
	}
	if !(s[0] == '_' || (s[0] >= 'a' && s[0] <= 'z') || (s[0] >= 'A' && s[0] <= 'Z')) {
		s = "n_" + s
	}
	if len(s) >= 3 && strings.EqualFold(s[:3], "hqq") {
		if camel {
			s = "Lib" + s
		} else {
			s = "lib_" + s
		}
	}
	if idlReserved[s] {
		s += "_val"
	}
	if len(s) > 64 {
		s = s[:64]
	}
	return s
}

func toCamelCase(s string) string {
	parts := strings.FieldsFunc(s, func(r rune) bool { return r == '_' || r == '-' || r == ' ' })
	if len(parts) == 0 {
		return "Handle"
	}
	var b strings.Builder
	for _, p := range parts {
		if p == "" {
			continue
		}
		r := []rune(p)
		r[0] = unicode.ToUpper(r[0])
		b.WriteString(string(r))
	}
	return b.String()
}

func isCIdent(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		ch := s[i]
		if i == 0 {
			if !(ch == '_' || (ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z')) {
				return false
			}
		} else {
			if !(ch == '_' || (ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z') || (ch >= '0' && ch <= '9')) {
				return false
			}
		}
	}
	return true
}
