package destination

import (
	"fmt"
	"strconv"
	"strings"
)

// Helpers for EnvSource implementations. getenv is os.Getenv or a test
// map; an empty value means unset.

// EnvString is the variable's value, or def when it is empty.
func EnvString(getenv func(string) string, name, def string) string {
	if v := strings.TrimSpace(getenv(name)); v != "" {
		return v
	}
	return def
}

// EnvList is a comma-separated variable (or def when empty), trimmed, with
// empty entries dropped.
func EnvList(getenv func(string) string, name, def string) []string {
	var out []string
	for _, p := range strings.Split(EnvString(getenv, name, def), ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// EnvInt is a whole-number variable, or def when it is empty.
func EnvInt(getenv func(string) string, name string, def int) (int, error) {
	v := strings.TrimSpace(getenv(name))
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def, fmt.Errorf("%s: %q is not a whole number", name, v)
	}
	return n, nil
}

// EnvBool is a boolean variable, or def when it is empty.
func EnvBool(getenv func(string) string, name string, def bool) (bool, error) {
	return ParseBool(name, getenv(name), def)
}

// ParseBool reads 1/true/yes/on or 0/false/no/off in any case. An empty
// value is the default; anything else is an error naming the variable, so
// a typo can't silently keep the default.
func ParseBool(name, value string, def bool) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "":
		return def, nil
	case "1", "true", "yes", "on":
		return true, nil
	case "0", "false", "no", "off":
		return false, nil
	default:
		return def, fmt.Errorf("%s: %q is not a boolean (use true/false, 1/0, yes/no or on/off)", name, value)
	}
}
