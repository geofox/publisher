package destination

import (
	"fmt"
	"math"
	"strconv"
	"strings"
)

// Config values reach a driver either as Go values (from FromEnv) or
// decoded from JSON (float64 numbers, []any lists). These accessors read
// both. A missing key is the zero value; a value of the wrong kind is an
// error naming the key.

// ConfigString reads a string.
func ConfigString(cfg map[string]any, key string) (string, error) {
	switch v := cfg[key].(type) {
	case nil:
		return "", nil
	case string:
		return v, nil
	default:
		return "", fmt.Errorf("%s: want a string, got %T", key, v)
	}
}

// ConfigStrings reads a list of strings (a comma-separated string is
// accepted too).
func ConfigStrings(cfg map[string]any, key string) ([]string, error) {
	switch v := cfg[key].(type) {
	case nil:
		return nil, nil
	case []string:
		return append([]string(nil), v...), nil
	case string:
		var out []string
		for _, p := range strings.Split(v, ",") {
			if p = strings.TrimSpace(p); p != "" {
				out = append(out, p)
			}
		}
		return out, nil
	case []any:
		out := make([]string, 0, len(v))
		for i, e := range v {
			s, ok := e.(string)
			if !ok {
				return nil, fmt.Errorf("%s[%d]: want a string, got %T", key, i, e)
			}
			out = append(out, s)
		}
		return out, nil
	default:
		return nil, fmt.Errorf("%s: want a list of strings, got %T", key, v)
	}
}

// ConfigInt reads a whole number; ok is false when the key is absent.
func ConfigInt(cfg map[string]any, key string) (n int, ok bool, err error) {
	switch v := cfg[key].(type) {
	case nil:
		return 0, false, nil
	case int:
		return v, true, nil
	case int64:
		return int(v), true, nil
	case float64:
		if v != math.Trunc(v) || math.Abs(v) > math.MaxInt32 {
			return 0, false, fmt.Errorf("%s: want a whole number, got %v", key, v)
		}
		return int(v), true, nil
	case string:
		i, err := strconv.Atoi(strings.TrimSpace(v))
		if err != nil {
			return 0, false, fmt.Errorf("%s: want a whole number, got %q", key, v)
		}
		return i, true, nil
	default:
		return 0, false, fmt.Errorf("%s: want a whole number, got %T", key, v)
	}
}

// ConfigBool reads a boolean; ok is false when the key is absent.
func ConfigBool(cfg map[string]any, key string) (b bool, ok bool, err error) {
	switch v := cfg[key].(type) {
	case nil:
		return false, false, nil
	case bool:
		return v, true, nil
	default:
		return false, false, fmt.Errorf("%s: want true or false, got %T", key, v)
	}
}

// InvalidField is the error Open returns for a missing or malformed field.
func InvalidField(field, problem string) *Error {
	return Invalidf("%s: %s", field, problem)
}
