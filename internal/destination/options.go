package destination

import (
	"fmt"
	"math"
	"sort"
	"strings"
)

// ValidateOptions checks opts against the manifest's option specs and returns
// one message per bad key, sorted by key. An unknown key is an error: a typo
// silently ignored is worse than a rejected request.
func ValidateOptions(specs []OptionSpec, opts map[string]any) []string {
	byKey := make(map[string]OptionSpec, len(specs))
	for _, s := range specs {
		byKey[s.Key] = s
	}
	keys := make([]string, 0, len(opts))
	for k := range opts {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var errs []string
	for _, k := range keys {
		spec, ok := byKey[k]
		if !ok {
			errs = append(errs, fmt.Sprintf("%s: unknown option", k))
			continue
		}
		if msg := spec.check(opts[k]); msg != "" {
			errs = append(errs, k+": "+msg)
		}
	}
	return errs
}

func (s OptionSpec) check(v any) string {
	if v == nil {
		return "" // null = default
	}
	switch s.Type {
	case OptionBool:
		if _, ok := v.(bool); !ok {
			return "must be true or false"
		}
	case OptionString:
		if _, ok := v.(string); !ok {
			return "must be a string"
		}
	case OptionInt:
		n, ok := asInt(v)
		if !ok {
			return "must be an integer"
		}
		if s.Min != nil && n < *s.Min {
			return fmt.Sprintf("must be at least %d", *s.Min)
		}
		if s.Max != nil && n > *s.Max {
			return fmt.Sprintf("must be at most %d", *s.Max)
		}
	case OptionEnum:
		str, ok := v.(string)
		if !ok || !s.allows(str) {
			return "must be one of " + s.choices()
		}
	case OptionEnumList:
		l, ok := stringList(v)
		if !ok {
			return "must be a list of strings"
		}
		for _, item := range l {
			if !s.allows(item) {
				return "each value must be one of " + s.choices()
			}
		}
	}
	return ""
}

func (s OptionSpec) allows(v string) bool {
	for _, e := range s.Enum {
		if e.Value == v {
			return true
		}
	}
	return false
}

func (s OptionSpec) choices() string {
	vals := make([]string, 0, len(s.Enum))
	for _, e := range s.Enum {
		vals = append(vals, fmt.Sprintf("%q", e.Value))
	}
	return strings.Join(vals, ", ")
}

// OptString returns opts[key] as a string ("" if absent or not a string).
func OptString(opts map[string]any, key string) string {
	s, _ := opts[key].(string)
	return s
}

// OptBool returns opts[key] as a bool (false if absent).
func OptBool(opts map[string]any, key string) bool {
	b, _ := opts[key].(bool)
	return b
}

// OptInt returns opts[key] as an int and whether it was set.
func OptInt(opts map[string]any, key string) (int, bool) {
	return asInt(opts[key])
}

// OptStrings returns opts[key] as a string list.
func OptStrings(opts map[string]any, key string) []string {
	l, _ := stringList(opts[key])
	return l
}

func asInt(v any) (int, bool) {
	switch n := v.(type) {
	case int:
		return n, true
	case int64:
		return int(n), true
	case float64: // JSON numbers
		if n == math.Trunc(n) && math.Abs(n) < 1<<31 {
			return int(n), true
		}
	}
	return 0, false
}

func stringList(v any) ([]string, bool) {
	switch l := v.(type) {
	case []string:
		return l, true
	case []any:
		out := make([]string, 0, len(l))
		for _, x := range l {
			s, ok := x.(string)
			if !ok {
				return nil, false
			}
			out = append(out, s)
		}
		return out, true
	}
	return nil, false
}
