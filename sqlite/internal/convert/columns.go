package convert

import (
	"reflect"
	"strings"
	"time"
)

// Column is the metadata derived from the declared type of a result column.
type Column struct {
	Decl   string       // declared type, uppercased; "" when none
	Scan   reflect.Type // type a Scan into this column expects
	IsTime bool         // DATE, DATETIME, or TIMESTAMP: TEXT values are parsed as time
}

var (
	anyType     = reflect.TypeFor[any]()
	bytesType   = reflect.TypeFor[[]byte]()
	timeType    = reflect.TypeFor[time.Time]()
	int64Type   = reflect.TypeFor[int64]()
	stringType  = reflect.TypeFor[string]()
	float64Type = reflect.TypeFor[float64]()
)

// NewColumn derives the metadata of a column from its declared type. The scan
// type follows SQLite's type affinity rules (sqlite.org/datatype3.html, section
// 3.1): INTEGER, TEXT, REAL, and BLOB declarations scan into int64, string,
// float64, and []byte. A column with no declared type, and one with NUMERIC
// affinity, scans into any. SQLite is dynamically typed, so a value may differ
// from its column's scan type.
func NewColumn(decl string) Column {
	ct := Column{Decl: strings.ToUpper(decl), Scan: anyType}
	base, _, _ := strings.Cut(strings.TrimSpace(ct.Decl), "(")
	switch strings.TrimSpace(base) {
	case "DATE", "DATETIME", "TIMESTAMP":
		ct.IsTime = true
		ct.Scan = timeType
		return ct
	}
	switch {
	case ct.Decl == "":
		// No declared type: the value keeps its storage class.
	case strings.Contains(ct.Decl, "BLOB"):
		ct.Scan = bytesType
	case strings.Contains(ct.Decl, "INT"):
		ct.Scan = int64Type
	case strings.Contains(ct.Decl, "CHAR"), strings.Contains(ct.Decl, "CLOB"), strings.Contains(ct.Decl, "TEXT"):
		ct.Scan = stringType
	case strings.Contains(ct.Decl, "REAL"), strings.Contains(ct.Decl, "FLOA"), strings.Contains(ct.Decl, "DOUB"):
		ct.Scan = float64Type
	}
	return ct
}

// Storage class names, as reported for columns without a declared type.
const (
	ClassInteger = "INTEGER"
	ClassReal    = "REAL"
	ClassText    = "TEXT"
	ClassBlob    = "BLOB"
	ClassNull    = "NULL"
)

// TextValue converts the TEXT of a column to a driver value: a time.Time for a
// DATE, DATETIME, or TIMESTAMP column whose text parses (see ParseTime), the
// string otherwise.
func (c Column) TextValue(b []byte) any {
	if c.IsTime {
		if t, ok := ParseTime(string(b)); ok {
			return t
		}
	}
	return string(b)
}

// StringValue is TextValue for text that is already a string.
func (c Column) StringValue(s string) any {
	if c.IsTime {
		if t, ok := ParseTime(s); ok {
			return t
		}
	}
	return s
}
