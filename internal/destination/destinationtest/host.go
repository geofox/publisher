// Package destinationtest has helpers for testing destination drivers.
package destinationtest

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"sync"
	"testing"

	"github.com/geofox/publisher/internal/destination"
)

// Host is an in-memory destination.AccountHost. It keeps what the account
// saves, merged as a real host merges it, and refuses every network
// connection unless the test allows it with AllowDial, so a test can prove
// that Open does no I/O. A refused dial fails the test when it ends (dials
// happen on transport goroutines, where t.Errorf can't be called safely).
type Host struct {
	mu       sync.Mutex
	saved    []destination.StateUpdate
	secrets  map[string]string
	config   map[string]any
	allow    bool
	refused  []string
	policies []destination.NetPolicy
}

// NewHost returns a host that fails the test on any dial.
func NewHost(t testing.TB) *Host {
	h := &Host{}
	t.Cleanup(func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		for _, addr := range h.refused {
			t.Errorf("unexpected network connection to %s", addr)
		}
	})
	return h
}

// AllowDial lets the account connect (to httptest servers).
func (h *Host) AllowDial() *Host {
	h.mu.Lock()
	h.allow = true
	h.mu.Unlock()
	return h
}

// WithState sets the stored state Save merges into (the account's
// secrets and config as opened).
func (h *Host) WithState(secrets map[string]string, config map[string]any) *Host {
	h.mu.Lock()
	h.secrets, h.config = secrets, config
	h.mu.Unlock()
	return h
}

// Saved returns each update the account saved, in order.
func (h *Host) Saved() []destination.StateUpdate {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]destination.StateUpdate(nil), h.saved...)
}

// State returns the stored state with every update merged in.
func (h *Host) State() (map[string]string, map[string]any) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return destination.MergeState(h.secrets, h.config, destination.StateUpdate{})
}

// Policies returns the network kinds the account asked for.
func (h *Host) Policies() []destination.NetPolicy {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]destination.NetPolicy(nil), h.policies...)
}

// Save implements destination.AccountHost.
func (h *Host) Save(_ context.Context, u destination.StateUpdate) error {
	h.mu.Lock()
	h.saved = append(h.saved, u)
	h.secrets, h.config = destination.MergeState(h.secrets, h.config, u)
	h.mu.Unlock()
	return nil
}

// Net implements destination.AccountHost.
func (h *Host) Net(p destination.NetPolicy) destination.Net {
	h.mu.Lock()
	h.policies = append(h.policies, p)
	h.mu.Unlock()
	d := &net.Dialer{}
	return destination.Net{DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
		h.mu.Lock()
		allow := h.allow
		if !allow {
			h.refused = append(h.refused, addr)
		}
		h.mu.Unlock()
		if !allow {
			return nil, errors.New("destinationtest: network refused")
		}
		return d.DialContext(ctx, network, addr)
	}}
}

// Logger implements destination.AccountHost.
func (h *Host) Logger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }
