package destination

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"
)

// Config values reach a driver either as Go values (from FromEnv) or
// decoded from JSON (float64 or json.Number numbers, []any lists, RFC 3339
// strings for times). These accessors read both. A missing key is the zero
// value; a value of the wrong kind is an error that describes the problem
// without the key, for InvalidField(key, err.Error()).

// ConfigString reads a string.
func ConfigString(cfg map[string]any, key string) (string, error) {
	switch v := cfg[key].(type) {
	case nil:
		return "", nil
	case string:
		return v, nil
	default:
		return "", fmt.Errorf("want a string, got %s", kindOf(v))
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
				return nil, fmt.Errorf("item %d: want a string, got %s", i+1, kindOf(e))
			}
			out = append(out, s)
		}
		return out, nil
	default:
		return nil, fmt.Errorf("want a list of strings, got %s", kindOf(v))
	}
}

// ConfigInt reads a whole number; ok is false when the key is absent.
func ConfigInt(cfg map[string]any, key string) (n int, ok bool, err error) {
	var f float64
	switch v := cfg[key].(type) {
	case nil:
		return 0, false, nil
	case int:
		return v, true, nil
	case int64:
		f = float64(v)
	case float64:
		f = v
	case json.Number:
		if f, err = v.Float64(); err != nil {
			return 0, false, errors.New("want a whole number")
		}
	default:
		return 0, false, fmt.Errorf("want a whole number, got %s", kindOf(v))
	}
	if f != math.Trunc(f) || math.Abs(f) > math.MaxInt32 {
		return 0, false, fmt.Errorf("want a whole number, got %v", f)
	}
	return int(f), true, nil
}

// ConfigBool reads a boolean; ok is false when the key is absent.
func ConfigBool(cfg map[string]any, key string) (b bool, ok bool, err error) {
	switch v := cfg[key].(type) {
	case nil:
		return false, false, nil
	case bool:
		return v, true, nil
	default:
		return false, false, fmt.Errorf("want true or false, got %s", kindOf(v))
	}
}

// ConfigTime reads a time (a time.Time, or an RFC 3339 string as JSON
// stores it); ok is false when the key is absent.
func ConfigTime(cfg map[string]any, key string) (t time.Time, ok bool, err error) {
	switch v := cfg[key].(type) {
	case nil:
		return time.Time{}, false, nil
	case time.Time:
		return v, true, nil
	case string:
		t, err := time.Parse(time.RFC3339Nano, v)
		if err != nil {
			return time.Time{}, false, errors.New("want a time (RFC 3339)")
		}
		return t, true, nil
	default:
		return time.Time{}, false, fmt.Errorf("want a time, got %s", kindOf(v))
	}
}

// kindOf names a config value's kind in the owner's terms, never its
// value (a misplaced secret must not reach an error message).
func kindOf(v any) string {
	switch v.(type) {
	case string:
		return "text"
	case bool:
		return "true/false"
	case int, int64, float64, json.Number:
		return "a number"
	case []any, []string:
		return "a list"
	case map[string]any:
		return "an object"
	default:
		return fmt.Sprintf("%T", v)
	}
}

// InvalidField is the error Open returns for a missing or malformed field.
func InvalidField(field, problem string) *Error {
	return Invalidf("%s: %s", field, problem)
}
