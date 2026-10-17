package sqlite

import (
	"database/sql/driver"
	"fmt"

	"gosuda.org/hqq/sqlite/internal/wire"
)

// wireValue converts a converted argument (see convert.ArgValue) to a protocol
// value.
func wireValue(v driver.Value) (wire.Value, error) {
	switch x := v.(type) {
	case nil:
		return wire.Null(), nil
	case int64:
		return wire.Integer(x), nil
	case float64:
		return wire.Float(x), nil
	case string:
		return wire.Text(x), nil
	case []byte:
		return wire.Blob(x), nil
	}
	return wire.Value{}, fmt.Errorf("unsupported argument type %T", v)
}
