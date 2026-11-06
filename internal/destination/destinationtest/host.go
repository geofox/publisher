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

// Host is an in-memory destination.AccountHost. It records what the
// account saves, and refuses every network connection unless the test
// allows it with AllowDial, so a test can prove that Open does no I/O.
type Host struct {
	t testing.TB

	mu       sync.Mutex
	saved    []destination.StateUpdate
	allow    bool
	policies []destination.NetPolicy
}

// NewHost returns a host that fails the test on any dial.
func NewHost(t testing.TB) *Host { return &Host{t: t} }

// AllowDial lets the account connect (to httptest servers).
func (h *Host) AllowDial() *Host {
	h.mu.Lock()
	h.allow = true
	h.mu.Unlock()
	return h
}

// Saved returns what the account saved, in order.
func (h *Host) Saved() []destination.StateUpdate {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]destination.StateUpdate(nil), h.saved...)
}

// Policies returns the network policies the account asked for.
func (h *Host) Policies() []destination.NetPolicy {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]destination.NetPolicy(nil), h.policies...)
}

// Save implements destination.AccountHost.
func (h *Host) Save(_ context.Context, u destination.StateUpdate) error {
	h.mu.Lock()
	h.saved = append(h.saved, u)
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
		h.mu.Unlock()
		if !allow {
			h.t.Errorf("unexpected network connection to %s", addr)
			return nil, errors.New("destinationtest: network refused")
		}
		return d.DialContext(ctx, network, addr)
	}}
}

// Logger implements destination.AccountHost.
func (h *Host) Logger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }
