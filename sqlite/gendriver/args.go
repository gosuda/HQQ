package gendriver

import (
	"database/sql/driver"
	"fmt"
	"math"
	"unsafe"

	"gosuda.org/hqq/bindgen/examples/sqlite3"
	"gosuda.org/hqq/sqlite/internal/convert"
	"gosuda.org/hqq/sqlite/internal/wire"
)

// Kinds of the arrays of bind_values and exec_stmt (sqlite3.idl).
const (
	kindInteger  uint8 = 1
	kindFloat    uint8 = 2
	kindText     uint8 = 3
	kindBlob     uint8 = 4
	kindNull     uint8 = 5
	kindZeroblob uint8 = 6
)

// userError marks an error that comes from the caller's input and not from the
// session: the conn stays usable and the error is returned as is.
type userError struct{ err error }

func (e userError) Error() string { return e.err.Error() }
func (e userError) Unwrap() error { return e.err }

// bindArgs are the three arrays that bind_values and exec_stmt take. They alias
// the scratch buffers of the conn, so they are valid until the next call of
// encode, and the generated client copies them into the request when it submits.
type bindArgs struct {
	kinds []uint8
	words []uint64
	data  []byte
}

// empty reports whether no parameter is bound.
func (b bindArgs) empty() bool { return len(b.kinds) == 0 }

// paramIndex returns the 1-based index of the parameter that arg binds. A
// positional argument binds the parameter of its ordinal. A named argument binds
// the parameter with that name, trying the prefixes ':', '@' and '$' in this
// order, as the hand-written server does. names are the parameter names of the
// statement, as SQLite spells them.
func paramIndex(a driver.NamedValue, name string, names []string) (int, error) {
	if name == "" {
		return a.Ordinal, nil
	}
	for _, prefix := range [...]byte{':', '@', '$'} {
		for i, n := range names {
			if len(n) == len(name)+1 && n[0] == prefix && n[1:] == name {
				return i + 1, nil
			}
		}
	}
	return 0, sqliteError(25 /* SQLITE_RANGE */, "no such parameter: "+name)
}

// needsNames reports whether encode must know the parameter names of the
// statement: some argument is named.
func needsNames(args []driver.NamedValue) bool {
	for _, a := range args {
		if a.Name != "" {
			return true
		}
	}
	return false
}

// encode converts database/sql arguments to the arrays of bind_values. names are
// the parameter names of the statement (only read for named arguments).
//
// Parameters that no argument binds are NULL, as in SQLite. When two arguments
// bind the same parameter, the later one wins.
func (cn *conn) encode(args []driver.NamedValue, names []string) (bindArgs, error) {
	if len(args) > wire.MaxParams {
		return bindArgs{}, userError{fmt.Errorf("sqlite: %d arguments exceed the limit of %d", len(args), wire.MaxParams)}
	}
	// First pass: convert every argument and find the parameter it binds.
	cn.vals = cn.vals[:0]
	cn.idx = cn.idx[:0]
	top := 0
	for _, a := range args {
		if a.Ordinal < 1 {
			return bindArgs{}, userError{fmt.Errorf("sqlite: argument ordinal %d is invalid", a.Ordinal)}
		}
		name, err := convert.ParamName(a.Name, wire.MaxNameBytes)
		if err != nil {
			return bindArgs{}, userError{err}
		}
		v, err := convert.ArgValue(a.Value)
		if err != nil {
			return bindArgs{}, userError{fmt.Errorf("sqlite: argument %d: %w", a.Ordinal, err)}
		}
		i, err := paramIndex(a, name, names)
		if err != nil {
			return bindArgs{}, err
		}
		if i > wire.MaxParams {
			return bindArgs{}, userError{fmt.Errorf("sqlite: parameter index %d exceeds the limit of %d", i, wire.MaxParams)}
		}
		cn.vals = append(cn.vals, v)
		cn.idx = append(cn.idx, i)
		top = max(top, i)
	}
	if top == 0 {
		return bindArgs{}, nil
	}
	// Second pass: one slot per parameter; a later argument replaces an earlier one.
	slots := growTo(cn.slots, top)
	clear(slots)
	for k, v := range cn.vals {
		slots[cn.idx[k]-1] = v
	}
	cn.slots = slots
	kinds := growTo(cn.kinds, top)
	words := growTo(cn.words, top)
	var total, nvar int
	for i, v := range slots {
		switch x := v.(type) {
		case int64:
			kinds[i], words[i] = kindInteger, uint64(x)
		case float64:
			kinds[i], words[i] = kindFloat, math.Float64bits(x)
		case string:
			kinds[i], words[i] = kindText, uint64(len(x))
			total += len(x)
			nvar++
		case []byte:
			kinds[i], words[i] = kindBlob, uint64(len(x))
			total += len(x)
			nvar++
		default: // nil, or a parameter that no argument binds
			kinds[i], words[i] = kindNull, 0
		}
	}
	cn.kinds, cn.words = kinds, words
	if total > int(sqlite3.SVC_MAX_BIND_BYTES) {
		return bindArgs{}, userError{fmt.Errorf("sqlite: text and blob arguments of %d bytes exceed the limit of %d", total, sqlite3.SVC_MAX_BIND_BYTES)}
	}
	out := bindArgs{kinds: kinds, words: words}
	switch {
	case total == 0:
	case nvar == 1:
		// A single text or blob value is not copied: the request encoder copies
		// it once, into the request.
		for _, v := range slots {
			if b := valueBytes(v); b != nil {
				out.data = b
			}
		}
	default:
		data := cn.data[:0]
		for _, v := range slots {
			data = append(data, valueBytes(v)...)
		}
		cn.data = data
		out.data = data
	}
	return out, nil
}

// growTo returns s with length n, reusing its storage.
func growTo[T any](s []T, n int) []T {
	if cap(s) < n {
		return make([]T, n)
	}
	return s[:n]
}

// valueBytes returns the bytes of a text or blob value without copying them, and
// nil for any other value.
func valueBytes(v driver.Value) []byte {
	switch x := v.(type) {
	case string:
		return unsafe.Slice(unsafe.StringData(x), len(x))
	case []byte:
		return x
	}
	return nil
}
