package destination

import (
	"fmt"
	"regexp"
	"sort"
	"sync"
)

// Driver is a destination type: something that has a manifest. Accounts of
// the type are constructed by the driver's package.
type Driver interface {
	Manifest() Manifest
}

var (
	regMu    sync.RWMutex
	registry = map[string]Driver{}
	typeRe   = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}$`)
)

// Register adds a driver. It panics on an invalid or duplicate type or a
// contract version this build doesn't speak: all are programming errors
// caught at startup.
func Register(d Driver) {
	m := d.Manifest()
	if !typeRe.MatchString(m.Type) {
		panic(fmt.Sprintf("destination: invalid type %q", m.Type))
	}
	if m.Contract != ContractVersion {
		panic(fmt.Sprintf("destination: %s speaks contract %d, this build speaks %d", m.Type, m.Contract, ContractVersion))
	}
	regMu.Lock()
	defer regMu.Unlock()
	if _, dup := registry[m.Type]; dup {
		panic(fmt.Sprintf("destination: %s registered twice", m.Type))
	}
	registry[m.Type] = d
}

// Lookup returns the driver for a type.
func Lookup(typ string) (Driver, bool) {
	regMu.RLock()
	defer regMu.RUnlock()
	d, ok := registry[typ]
	return d, ok
}

// Types returns the registered types, sorted.
func Types() []string {
	regMu.RLock()
	defer regMu.RUnlock()
	out := make([]string, 0, len(registry))
	for t := range registry {
		out = append(out, t)
	}
	sort.Strings(out)
	return out
}

// Unregister removes a type. For tests.
func Unregister(typ string) {
	regMu.Lock()
	defer regMu.Unlock()
	delete(registry, typ)
}
