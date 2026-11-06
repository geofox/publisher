package destination

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"sync"
)

// Driver is a destination type: it describes the type and opens its
// accounts.
type Driver interface {
	Manifest() Manifest
	// Open builds an account from what the host stored for it, or what
	// the environment configured. It must not do network I/O: an
	// unreachable network must never stop an account from loading (Checker
	// and Identity find out later). A missing or malformed field is an
	// *Error of class invalid naming it.
	Open(ctx context.Context, req OpenRequest) (Account, error)
}

var (
	regMu    sync.RWMutex
	registry = map[string]Driver{}
	order    []string // registration order
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
	order = append(order, m.Type)
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

// TypesInOrder returns the registered types in registration order: the
// order accounts are listed and composed in. Built-in drivers register
// first, in the order the composer has always shown them.
func TypesInOrder() []string {
	regMu.RLock()
	defer regMu.RUnlock()
	return append([]string(nil), order...)
}

// Unregister removes a type. For tests.
func Unregister(typ string) {
	regMu.Lock()
	defer regMu.Unlock()
	delete(registry, typ)
	for i, t := range order {
		if t == typ {
			order = append(order[:i:i], order[i+1:]...)
			break
		}
	}
}
