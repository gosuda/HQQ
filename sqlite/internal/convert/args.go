package convert

import (
	"database/sql/driver"
	"errors"
	"fmt"
	"math"
	"reflect"
	"strings"
	"time"
)

var valuerType = reflect.TypeFor[driver.Valuer]()

// ArgValue converts an argument to a driver value of the protocol: nil,
// int64, float64, string, or []byte. The rules (also in the package
// documentation of the drivers):
//
//   - all signed integers, and unsigned integers up to math.MaxInt64, are
//     INTEGER; a larger uint64 is an error;
//   - float32 and float64 are REAL; NaN is an error, because SQLite stores NaN
//     as NULL and the driver does not guess; ±Inf is stored as is;
//   - bool is INTEGER 0 or 1;
//   - string is TEXT;
//   - []byte and json.RawMessage are BLOB, and a nil one is an empty BLOB
//     (use string(raw) to store JSON text);
//   - time.Time is TEXT in TimeLayout;
//   - a driver.Valuer is replaced by its Value, which must not be a Valuer;
//   - a pointer is dereferenced, and a nil pointer is NULL;
//   - a named type is converted by its underlying kind;
//   - anything else is an error that names the type.
func ArgValue(v any) (driver.Value, error) {
	return convertArg(v, false)
}

// convertArg implements ArgValue. nested is true for the result of a Valuer,
// which must be a plain value.
func convertArg(v any, nested bool) (driver.Value, error) {
	switch x := v.(type) {
	case nil:
		return nil, nil
	case int64:
		return x, nil
	case float64:
		return floatArg(x)
	case string:
		return x, nil
	case bool:
		if x {
			return int64(1), nil
		}
		return int64(0), nil
	case []byte:
		return x, nil
	case time.Time:
		return x.Format(TimeLayout), nil
	}
	if vr, ok := v.(driver.Valuer); ok {
		if nested {
			return nil, fmt.Errorf("sqlite: driver.Valuer %T returned another driver.Valuer", v)
		}
		dv, err := callValuer(vr)
		if err != nil {
			return nil, err
		}
		return convertArg(dv, true)
	}
	return convertReflect(v, nested)
}

// callValuer calls Value. A nil pointer whose Value method has a value receiver
// is NULL, as in database/sql.
func callValuer(vr driver.Valuer) (driver.Value, error) {
	rv := reflect.ValueOf(vr)
	if rv.Kind() == reflect.Pointer && rv.IsNil() && rv.Type().Elem().Implements(valuerType) {
		return nil, nil
	}
	return vr.Value()
}

// convertReflect converts a value of a named or derived type by its kind.
func convertReflect(v any, nested bool) (driver.Value, error) {
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return rv.Int(), nil
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		u := rv.Uint()
		if u > math.MaxInt64 {
			return nil, fmt.Errorf("sqlite: unsigned integer %d does not fit in INTEGER (max %d)", u, int64(math.MaxInt64))
		}
		return int64(u), nil
	case reflect.Float32, reflect.Float64:
		return floatArg(rv.Float())
	case reflect.Bool:
		if rv.Bool() {
			return int64(1), nil
		}
		return int64(0), nil
	case reflect.String:
		return rv.String(), nil
	case reflect.Slice:
		if rv.Type().Elem().Kind() == reflect.Uint8 {
			return rv.Bytes(), nil
		}
	case reflect.Pointer:
		if rv.IsNil() {
			return nil, nil
		}
		return convertArg(rv.Elem().Interface(), nested)
	}
	return nil, fmt.Errorf("sqlite: unsupported argument type %T (use a basic type, []byte, time.Time, or a driver.Valuer)", v)
}

// floatArg checks a REAL argument. NaN has no SQLite representation.
func floatArg(f float64) (driver.Value, error) {
	if math.IsNaN(f) {
		return nil, errors.New("sqlite: NaN cannot be stored: SQLite stores NaN as NULL; pass nil to store NULL")
	}
	return f, nil
}

// ParamName returns the parameter name a protocol carries: the name without
// its prefix character (:, @, or $), or "" for a positional argument. maxBytes
// bounds the name.
func ParamName(name string, maxBytes int) (string, error) {
	if name == "" {
		return "", nil
	}
	if strings.ContainsAny(name[:1], ":@$") {
		name = name[1:]
	}
	if name == "" {
		return "", errors.New("sqlite: parameter name is empty after its prefix")
	}
	if len(name) > maxBytes {
		return "", fmt.Errorf("sqlite: parameter name of %d bytes exceeds %d", len(name), maxBytes)
	}
	return name, nil
}
