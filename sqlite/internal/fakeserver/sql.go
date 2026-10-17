package fakeserver

import (
	"fmt"
	"strconv"
	"strings"

	"gosuda.org/hqq/sqlite/internal/wire"
)

// This file is the SQL front end of the fake engine: a lexer and a parser for
// the subset of SQL the tests use. It accepts:
//
//	CREATE TABLE [IF NOT EXISTS] t (col [type] [PRIMARY KEY|NOT NULL|UNIQUE], ...)
//	DROP TABLE [IF EXISTS] t
//	INSERT INTO t [(col, ...)] VALUES (expr, ...)[, (expr, ...)]...
//	SELECT *|expr [AS a], ... [FROM t | FROM hqq_series(a, b)] [WHERE col = expr [AND ...]] [LIMIT n]
//	UPDATE t SET col = expr[, ...] [WHERE ...]
//	DELETE FROM t [WHERE ...]
//	BEGIN [DEFERRED|IMMEDIATE|EXCLUSIVE] [TRANSACTION], COMMIT|END [TRANSACTION], ROLLBACK [TRANSACTION]
//	PRAGMA name [= value]
//
// Expressions are literals (numbers, 'text', X'blob', NULL), parameters (?, ?NNN,
// :name, @name, $name), column references, and the two test functions
// hqq_sleep(ms) and hqq_series(a, b). Parameters are numbered as SQLite numbers
// them. The engine is not SQLite and is not meant to be: it exists so that the
// protocol and the driver can be tested end to end without cgo.

type tokKind uint8

const (
	tEOF tokKind = iota
	tIdent
	tNumber
	tString
	tBlob
	tParam
	tPunct
)

type token struct {
	kind  tokKind
	text  string // identifier (unquoted), punctuation, or parameter text
	pos   int    // byte offset of the first character
	end   int    // byte offset after the last character
	isInt bool
	ival  int64
	fval  float64
	bytes []byte // string or blob payload
}

// lex splits sql into tokens. It returns a syntax error for characters it does
// not know.
func lex(sql string) ([]token, *sqlError) {
	var toks []token
	i := 0
	for i < len(sql) {
		c := sql[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\f':
			i++
		case c == '-' && i+1 < len(sql) && sql[i+1] == '-':
			for i < len(sql) && sql[i] != '\n' {
				i++
			}
		case c == '/' && i+1 < len(sql) && sql[i+1] == '*':
			end := strings.Index(sql[i+2:], "*/")
			if end < 0 {
				return nil, syntaxError(sql, i)
			}
			i += end + 4
		case (c == 'x' || c == 'X') && i+1 < len(sql) && sql[i+1] == '\'':
			start := i
			j := i + 2
			for j < len(sql) && sql[j] != '\'' {
				j++
			}
			if j >= len(sql) {
				return nil, syntaxError(sql, start)
			}
			raw, err := hexDecode(sql[i+2 : j])
			if err != nil {
				return nil, syntaxError(sql, start)
			}
			toks = append(toks, token{kind: tBlob, pos: start, end: j + 1, bytes: raw})
			i = j + 1
		case c == '\'':
			start := i
			var b []byte
			j := i + 1
			for {
				if j >= len(sql) {
					return nil, syntaxError(sql, start)
				}
				if sql[j] == '\'' {
					if j+1 < len(sql) && sql[j+1] == '\'' {
						b = append(b, '\'')
						j += 2
						continue
					}
					break
				}
				b = append(b, sql[j])
				j++
			}
			toks = append(toks, token{kind: tString, pos: start, end: j + 1, bytes: b})
			i = j + 1
		case c == '"' || c == '`' || c == '[':
			closer := map[byte]byte{'"': '"', '`': '`', '[': ']'}[c]
			start := i
			j := i + 1
			for j < len(sql) && sql[j] != closer {
				j++
			}
			if j >= len(sql) {
				return nil, syntaxError(sql, start)
			}
			toks = append(toks, token{kind: tIdent, text: sql[i+1 : j], pos: start, end: j + 1})
			i = j + 1
		case isIdentStart(c):
			start := i
			for i < len(sql) && isIdentPart(sql[i]) {
				i++
			}
			toks = append(toks, token{kind: tIdent, text: sql[start:i], pos: start, end: i})
		case c >= '0' && c <= '9' || (c == '.' && i+1 < len(sql) && sql[i+1] >= '0' && sql[i+1] <= '9'):
			start := i
			for i < len(sql) && (sql[i] >= '0' && sql[i] <= '9' || sql[i] == '.' || sql[i] == 'e' || sql[i] == 'E' ||
				((sql[i] == '+' || sql[i] == '-') && (sql[i-1] == 'e' || sql[i-1] == 'E'))) {
				i++
			}
			tok := token{kind: tNumber, pos: start, end: i, text: sql[start:i]}
			if n, err := strconv.ParseInt(tok.text, 10, 64); err == nil {
				tok.isInt, tok.ival = true, n
			} else if f, err := strconv.ParseFloat(tok.text, 64); err == nil {
				tok.fval = f
			} else {
				return nil, syntaxError(sql, start)
			}
			toks = append(toks, tok)
		case c == '?':
			start := i
			i++
			for i < len(sql) && sql[i] >= '0' && sql[i] <= '9' {
				i++
			}
			toks = append(toks, token{kind: tParam, text: sql[start:i], pos: start, end: i})
		case c == ':' || c == '@' || c == '$':
			start := i
			i++
			for i < len(sql) && isIdentPart(sql[i]) {
				i++
			}
			if i == start+1 {
				return nil, syntaxError(sql, start)
			}
			toks = append(toks, token{kind: tParam, text: sql[start:i], pos: start, end: i})
		case (c == '<' || c == '>' || c == '!') && i+1 < len(sql) && (sql[i+1] == '=' || c == '<' && sql[i+1] == '>'):
			toks = append(toks, token{kind: tPunct, text: sql[i : i+2], pos: i, end: i + 2})
			i += 2
		case strings.IndexByte("(),;*=.-<>", c) >= 0:
			toks = append(toks, token{kind: tPunct, text: string(c), pos: i, end: i + 1})
			i++
		default:
			return nil, syntaxError(sql, i)
		}
	}
	toks = append(toks, token{kind: tEOF, pos: len(sql), end: len(sql)})
	return toks, nil
}

func isIdentStart(c byte) bool {
	return c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= 0x80
}

func isIdentPart(c byte) bool {
	return isIdentStart(c) || c >= '0' && c <= '9'
}

func hexDecode(s string) ([]byte, error) {
	if len(s)%2 != 0 {
		return nil, fmt.Errorf("odd length")
	}
	out := make([]byte, len(s)/2)
	for i := range out {
		v, err := strconv.ParseUint(s[2*i:2*i+2], 16, 8)
		if err != nil {
			return nil, err
		}
		out[i] = byte(v)
	}
	return out, nil
}

func syntaxError(sql string, at int) *sqlError {
	near := sql[min(at, len(sql)):]
	if len(near) > 20 {
		near = near[:20]
	}
	return &sqlError{rc: rcError, msg: fmt.Sprintf("near %q: syntax error", near)}
}

type stmtKind uint8

const (
	kCreateTable stmtKind = iota + 1
	kDropTable
	kInsert
	kSelect
	kUpdate
	kDelete
	kBegin
	kCommit
	kRollback
	kPragma
)

// statement is a parsed statement.
type statement struct {
	kind     stmtKind
	table    string
	ifExists bool // IF NOT EXISTS (create) or IF EXISTS (drop)
	cols     []colDef
	insCols  []string
	rows     [][]expr
	items    []selItem
	from     *fromSpec
	where    []cond
	limit    expr
	setCols  []string
	setVals  []expr
	mode     wire.TxMode
	pragma   string
	pragmaV  expr // nil if the pragma has no value
}

type colDef struct {
	name       string
	decl       string
	notNull    bool
	primaryKey bool
	unique     bool
}

type fromSpec struct {
	table  string
	series bool
	args   []expr
}

type selItem struct {
	star bool
	e    expr
	name string
}

// cond is one WHERE condition: column op value.
type cond struct {
	col string
	op  string // =, !=, <>, <, <=, >, >=
	val expr
}

// expr is one of litExpr, paramExpr, colExpr, callExpr.
type expr interface{}

type litExpr struct{ v wire.Value }
type paramExpr struct{ idx int }
type colExpr struct{ name string }
type callExpr struct {
	fn   string
	args []expr
}

func isCompareOp(op string) bool {
	switch op {
	case "=", "==", "!=", "<>", "<", "<=", ">", ">=":
		return true
	}
	return false
}

// parser is a recursive-descent parser over the tokens of one statement.
type parser struct {
	sql      string
	toks     []token
	i        int
	maxParam int
	named    map[string]int
}

// parse parses exactly one statement. It returns errMultiple if text follows
// the statement and errEmpty if there is no statement.
func parse(sql string) (*statement, *parser, error) {
	toks, lexErr := lex(sql)
	if lexErr != nil {
		return nil, nil, lexErr
	}
	p := &parser{sql: sql, toks: toks, named: map[string]int{}}
	if p.peek().kind == tEOF {
		return nil, nil, errEmpty
	}
	st, err := p.statement()
	if err != nil {
		return nil, nil, err
	}
	p.acceptPunct(";")
	if p.peek().kind != tEOF {
		return nil, nil, errMultiple
	}
	return st, p, nil
}

func (p *parser) peek() token { return p.toks[p.i] }

func (p *parser) next() token {
	t := p.toks[p.i]
	if t.kind != tEOF {
		p.i++
	}
	return t
}

// keyword reports whether the next token is the keyword kw (case-insensitive).
func (p *parser) keyword(kw string) bool {
	t := p.peek()
	return t.kind == tIdent && strings.EqualFold(t.text, kw)
}

func (p *parser) acceptKeyword(kw string) bool {
	if p.keyword(kw) {
		p.i++
		return true
	}
	return false
}

func (p *parser) expectKeyword(kw string) *sqlError {
	if !p.acceptKeyword(kw) {
		return syntaxError(p.sql, p.peek().pos)
	}
	return nil
}

func (p *parser) acceptPunct(s string) bool {
	t := p.peek()
	if t.kind == tPunct && t.text == s {
		p.i++
		return true
	}
	return false
}

func (p *parser) expectPunct(s string) *sqlError {
	if !p.acceptPunct(s) {
		return syntaxError(p.sql, p.peek().pos)
	}
	return nil
}

func (p *parser) ident() (string, *sqlError) {
	t := p.peek()
	if t.kind != tIdent {
		return "", syntaxError(p.sql, t.pos)
	}
	p.i++
	return t.text, nil
}

func (p *parser) statement() (*statement, error) {
	switch {
	case p.acceptKeyword("CREATE"):
		return p.createTable()
	case p.acceptKeyword("DROP"):
		return p.dropTable()
	case p.acceptKeyword("INSERT"):
		return p.insert()
	case p.acceptKeyword("SELECT"):
		return p.selectStmt()
	case p.acceptKeyword("UPDATE"):
		return p.update()
	case p.acceptKeyword("DELETE"):
		return p.deleteStmt()
	case p.acceptKeyword("BEGIN"):
		st := &statement{kind: kBegin}
		switch {
		case p.acceptKeyword("IMMEDIATE"):
			st.mode = wire.TxImmediate
		case p.acceptKeyword("EXCLUSIVE"):
			st.mode = wire.TxExclusive
		case p.acceptKeyword("DEFERRED"):
		}
		p.acceptKeyword("TRANSACTION")
		return st, nil
	case p.acceptKeyword("COMMIT"), p.acceptKeyword("END"):
		p.acceptKeyword("TRANSACTION")
		return &statement{kind: kCommit}, nil
	case p.acceptKeyword("ROLLBACK"):
		p.acceptKeyword("TRANSACTION")
		return &statement{kind: kRollback}, nil
	case p.acceptKeyword("PRAGMA"):
		return p.pragma()
	}
	return nil, syntaxError(p.sql, p.peek().pos)
}

func (p *parser) createTable() (*statement, error) {
	if err := p.expectKeyword("TABLE"); err != nil {
		return nil, err
	}
	st := &statement{kind: kCreateTable}
	if p.acceptKeyword("IF") {
		if err := p.expectKeyword("NOT"); err != nil {
			return nil, err
		}
		if err := p.expectKeyword("EXISTS"); err != nil {
			return nil, err
		}
		st.ifExists = true
	}
	name, err := p.ident()
	if err != nil {
		return nil, err
	}
	st.table = name
	if err := p.expectPunct("("); err != nil {
		return nil, err
	}
	for {
		col, err := p.columnDef()
		if err != nil {
			return nil, err
		}
		st.cols = append(st.cols, col)
		if p.acceptPunct(",") {
			continue
		}
		if err := p.expectPunct(")"); err != nil {
			return nil, err
		}
		break
	}
	return st, nil
}

func (p *parser) columnDef() (colDef, error) {
	var c colDef
	name, err := p.ident()
	if err != nil {
		return c, err
	}
	c.name = name
	var decl []string
	for p.peek().kind == tIdent && !p.isConstraintKeyword() {
		decl = append(decl, p.next().text)
	}
	if p.acceptPunct("(") { // size arguments, for example VARCHAR(10)
		for !p.acceptPunct(")") {
			if p.peek().kind == tEOF {
				return c, syntaxError(p.sql, p.peek().pos)
			}
			p.next()
		}
	}
	c.decl = strings.Join(decl, " ")
	for {
		switch {
		case p.acceptKeyword("PRIMARY"):
			if err := p.expectKeyword("KEY"); err != nil {
				return c, err
			}
			p.acceptKeyword("ASC")
			p.acceptKeyword("DESC")
			c.primaryKey = true
		case p.acceptKeyword("NOT"):
			if err := p.expectKeyword("NULL"); err != nil {
				return c, err
			}
			c.notNull = true
		case p.acceptKeyword("UNIQUE"):
			c.unique = true
		default:
			return c, nil
		}
	}
}

func (p *parser) isConstraintKeyword() bool {
	return p.keyword("PRIMARY") || p.keyword("NOT") || p.keyword("UNIQUE")
}

func (p *parser) dropTable() (*statement, error) {
	if err := p.expectKeyword("TABLE"); err != nil {
		return nil, err
	}
	st := &statement{kind: kDropTable}
	if p.acceptKeyword("IF") {
		if err := p.expectKeyword("EXISTS"); err != nil {
			return nil, err
		}
		st.ifExists = true
	}
	name, err := p.ident()
	if err != nil {
		return nil, err
	}
	st.table = name
	return st, nil
}

func (p *parser) insert() (*statement, error) {
	if err := p.expectKeyword("INTO"); err != nil {
		return nil, err
	}
	name, err := p.ident()
	if err != nil {
		return nil, err
	}
	st := &statement{kind: kInsert, table: name}
	if p.acceptPunct("(") {
		for {
			c, err := p.ident()
			if err != nil {
				return nil, err
			}
			st.insCols = append(st.insCols, c)
			if p.acceptPunct(",") {
				continue
			}
			if err := p.expectPunct(")"); err != nil {
				return nil, err
			}
			break
		}
	}
	if err := p.expectKeyword("VALUES"); err != nil {
		return nil, err
	}
	for {
		if err := p.expectPunct("("); err != nil {
			return nil, err
		}
		var row []expr
		for {
			e, err := p.expr()
			if err != nil {
				return nil, err
			}
			row = append(row, e)
			if p.acceptPunct(",") {
				continue
			}
			if err := p.expectPunct(")"); err != nil {
				return nil, err
			}
			break
		}
		st.rows = append(st.rows, row)
		if !p.acceptPunct(",") {
			break
		}
	}
	return st, nil
}

func (p *parser) selectStmt() (*statement, error) {
	st := &statement{kind: kSelect}
	for {
		if p.acceptPunct("*") {
			st.items = append(st.items, selItem{star: true})
		} else {
			start := p.peek().pos
			e, err := p.expr()
			if err != nil {
				return nil, err
			}
			end := p.toks[p.i-1].end
			item := selItem{e: e, name: strings.TrimSpace(p.sql[start:end])}
			if c, ok := e.(colExpr); ok {
				item.name = c.name
			}
			if p.acceptKeyword("AS") {
				alias, err := p.ident()
				if err != nil {
					return nil, err
				}
				item.name = alias
			}
			st.items = append(st.items, item)
		}
		if !p.acceptPunct(",") {
			break
		}
	}
	if p.acceptKeyword("FROM") {
		name, err := p.ident()
		if err != nil {
			return nil, err
		}
		from := &fromSpec{table: name}
		if p.acceptPunct("(") {
			if !strings.EqualFold(name, "hqq_series") {
				return nil, &sqlError{rc: rcError, msg: "no such table-valued function: " + name}
			}
			from.series = true
			for {
				e, err := p.expr()
				if err != nil {
					return nil, err
				}
				from.args = append(from.args, e)
				if p.acceptPunct(",") {
					continue
				}
				if err := p.expectPunct(")"); err != nil {
					return nil, err
				}
				break
			}
			if len(from.args) != 2 {
				return nil, &sqlError{rc: rcError, msg: "hqq_series takes two arguments"}
			}
		}
		st.from = from
	}
	if p.acceptKeyword("WHERE") {
		conds, err := p.where()
		if err != nil {
			return nil, err
		}
		st.where = conds
	}
	if p.acceptKeyword("LIMIT") {
		e, err := p.expr()
		if err != nil {
			return nil, err
		}
		st.limit = e
	}
	return st, nil
}

func (p *parser) where() ([]cond, *sqlError) {
	var conds []cond
	for {
		col, err := p.ident()
		if err != nil {
			return nil, err
		}
		op := p.peek()
		if op.kind != tPunct || !isCompareOp(op.text) {
			return nil, syntaxError(p.sql, op.pos)
		}
		p.next()
		val, err := p.expr()
		if err != nil {
			return nil, err
		}
		conds = append(conds, cond{col: col, op: op.text, val: val})
		if !p.acceptKeyword("AND") {
			return conds, nil
		}
	}
}

func (p *parser) update() (*statement, error) {
	name, err := p.ident()
	if err != nil {
		return nil, err
	}
	st := &statement{kind: kUpdate, table: name}
	if err := p.expectKeyword("SET"); err != nil {
		return nil, err
	}
	for {
		col, err := p.ident()
		if err != nil {
			return nil, err
		}
		if err := p.expectPunct("="); err != nil {
			return nil, err
		}
		val, err := p.expr()
		if err != nil {
			return nil, err
		}
		st.setCols = append(st.setCols, col)
		st.setVals = append(st.setVals, val)
		if !p.acceptPunct(",") {
			break
		}
	}
	if p.acceptKeyword("WHERE") {
		conds, err := p.where()
		if err != nil {
			return nil, err
		}
		st.where = conds
	}
	return st, nil
}

func (p *parser) deleteStmt() (*statement, error) {
	if err := p.expectKeyword("FROM"); err != nil {
		return nil, err
	}
	name, err := p.ident()
	if err != nil {
		return nil, err
	}
	st := &statement{kind: kDelete, table: name}
	if p.acceptKeyword("WHERE") {
		conds, err := p.where()
		if err != nil {
			return nil, err
		}
		st.where = conds
	}
	return st, nil
}

func (p *parser) pragma() (*statement, error) {
	name, err := p.ident()
	if err != nil {
		return nil, err
	}
	st := &statement{kind: kPragma, pragma: strings.ToLower(name)}
	if p.acceptPunct("=") {
		e, err := p.expr()
		if err != nil {
			return nil, err
		}
		st.pragmaV = e
	}
	return st, nil
}

// expr parses one expression.
func (p *parser) expr() (expr, *sqlError) {
	t := p.peek()
	switch t.kind {
	case tNumber:
		p.next()
		if t.isInt {
			return litExpr{wire.Integer(t.ival)}, nil
		}
		return litExpr{wire.Float(t.fval)}, nil
	case tPunct:
		if t.text == "-" {
			p.next()
			n := p.peek()
			if n.kind != tNumber {
				return nil, syntaxError(p.sql, n.pos)
			}
			p.next()
			if n.isInt {
				return litExpr{wire.Integer(-n.ival)}, nil
			}
			return litExpr{wire.Float(-n.fval)}, nil
		}
	case tString:
		p.next()
		return litExpr{wire.Value{Type: wire.TypeText, Bytes: t.bytes}}, nil
	case tBlob:
		p.next()
		return litExpr{wire.Blob(t.bytes)}, nil
	case tParam:
		p.next()
		return p.param(t.text), nil
	case tIdent:
		if strings.EqualFold(t.text, "NULL") {
			p.next()
			return litExpr{wire.Null()}, nil
		}
		p.next()
		if p.acceptPunct("(") {
			call := callExpr{fn: strings.ToLower(t.text)}
			if !p.acceptPunct(")") {
				for {
					e, err := p.expr()
					if err != nil {
						return nil, err
					}
					call.args = append(call.args, e)
					if p.acceptPunct(",") {
						continue
					}
					if err := p.expectPunct(")"); err != nil {
						return nil, err
					}
					break
				}
			}
			return call, nil
		}
		return colExpr{name: t.text}, nil
	}
	return nil, syntaxError(p.sql, t.pos)
}

// param numbers a parameter the way SQLite does: "?" takes the next number
// after the largest so far, "?NNN" takes NNN, and a named parameter keeps the
// number of its first occurrence.
func (p *parser) param(text string) expr {
	switch {
	case text == "?":
		p.maxParam++
		return paramExpr{idx: p.maxParam}
	case text[0] == '?':
		n, _ := strconv.Atoi(text[1:])
		if n > p.maxParam {
			p.maxParam = n
		}
		return paramExpr{idx: n}
	}
	if idx, ok := p.named[text]; ok {
		return paramExpr{idx: idx}
	}
	p.maxParam++
	p.named[text] = p.maxParam
	return paramExpr{idx: p.maxParam}
}
