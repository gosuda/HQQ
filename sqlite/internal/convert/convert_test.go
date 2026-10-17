package convert

import (
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"
)

type namedInt int16
type namedStr string

type valuer struct{ v driver.Value }

func (x valuer) Value() (driver.Value, error) { return x.v, nil }

func TestArgValue(t *testing.T) {
	kst := time.FixedZone("KST", 9*3600)
	cases := []struct {
		name string
		in   any
		want driver.Value
	}{
		{"nil", nil, nil},
		{"int", -3, int64(-3)},
		{"uint32", uint32(math.MaxUint32), int64(math.MaxUint32)},
		{"uint64 max int64", uint64(math.MaxInt64), int64(math.MaxInt64)},
		{"float32", float32(0.5), 0.5},
		{"+Inf", math.Inf(1), math.Inf(1)},
		{"bool", true, int64(1)},
		{"named int", namedInt(7), int64(7)},
		{"named string", namedStr("s"), "s"},
		{"raw json", json.RawMessage(`{}`), []byte(`{}`)},
		{"nil pointer", (*int)(nil), nil},
		{"pointer", new(int), int64(0)},
		{"time", time.Date(2024, 3, 5, 10, 20, 30, 5e8, kst), "2024-03-05 10:20:30.5+09:00"},
		{"valuer", valuer{int64(9)}, int64(9)},
		{"NullString null", sql.NullString{}, nil},
	}
	for _, tc := range cases {
		got, err := ArgValue(tc.in)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: got %#v, want %#v", tc.name, got, tc.want)
		}
	}
	for name, in := range map[string]any{
		"NaN":              math.NaN(),
		"uint64 too large": uint64(math.MaxUint64),
		"struct":           struct{}{},
		"map":              map[string]int{},
		"nested valuer":    valuer{valuer{1}},
	} {
		if _, err := ArgValue(in); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

func TestParamName(t *testing.T) {
	for in, want := range map[string]string{"": "", "a": "a", ":a": "a", "@a": "a", "$a": "a"} {
		got, err := ParamName(in, 8)
		if err != nil || got != want {
			t.Errorf("ParamName(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	if _, err := ParamName(":", 8); err == nil {
		t.Error("empty name after prefix accepted")
	}
	if _, err := ParamName(strings.Repeat("x", 9), 8); err == nil {
		t.Error("too long name accepted")
	}
}

func TestParseTime(t *testing.T) {
	for _, s := range []string{
		"2024-03-05 10:20:30.123456789+09:00", "2024-03-05T10:20:30Z", "2024-03-05 10:20:30",
		"2024-03-05T10:20:30.5", "2024-03-05 10:20", "2024-03-05T10:20", "2024-03-05",
	} {
		if _, ok := ParseTime(s); !ok {
			t.Errorf("%q not parsed", s)
		}
	}
	for _, s := range []string{"", "yesterday", "2024-13-05"} {
		if _, ok := ParseTime(s); ok {
			t.Errorf("%q parsed", s)
		}
	}
}

func TestNewColumn(t *testing.T) {
	for decl, want := range map[string]reflect.Type{
		"INTEGER":      reflect.TypeFor[int64](),
		"bigint":       reflect.TypeFor[int64](),
		"VARCHAR(20)":  reflect.TypeFor[string](),
		"text":         reflect.TypeFor[string](),
		"DOUBLE":       reflect.TypeFor[float64](),
		"BLOB":         reflect.TypeFor[[]byte](),
		"DATETIME":     reflect.TypeFor[time.Time](),
		"timestamp(3)": reflect.TypeFor[time.Time](),
		"WIDGET":       reflect.TypeFor[any](),
		"":             reflect.TypeFor[any](),
	} {
		if got := NewColumn(decl); got.Scan != want || got.Decl != strings.ToUpper(decl) {
			t.Errorf("%q: %+v, want scan %v", decl, got, want)
		}
	}
	dt := NewColumn("DATETIME")
	if v := dt.TextValue([]byte("2024-03-05")); !reflect.DeepEqual(v, time.Date(2024, 3, 5, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("time column text = %#v", v)
	}
	if v := dt.TextValue([]byte("soon")); v != "soon" {
		t.Errorf("unparsable time text = %#v", v)
	}
	if v := NewColumn("TEXT").TextValue([]byte("2024-03-05")); v != "2024-03-05" {
		t.Errorf("TEXT column parsed as time: %#v", v)
	}
}
