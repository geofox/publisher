package accounts

import (
	"context"
	"errors"
	"log/slog"

	"github.com/geofox/publisher/internal/destination"
	"github.com/geofox/publisher/internal/netguard"
	"github.com/geofox/publisher/internal/store"
)

// errNoState: the registry has nowhere to keep what accounts save.
var errNoState = errors.New("this account can't save state in this version")

// host is the registry's side of one open account.
type host struct {
	r   *Registry
	id  string
	row store.Account // as opened; type and source never change
}

// current is the account's row as the registry has it now.
func (h *host) current() store.Account {
	h.r.mu.RLock()
	defer h.r.mu.RUnlock()
	if e, ok := h.r.entries[h.id]; ok {
		return e.row
	}
	return h.row
}

// Save implements destination.AccountHost.
func (h *host) Save(ctx context.Context, u destination.StateUpdate) error {
	if h.r.opt.State == nil {
		return errNoState
	}
	return h.r.opt.State.Save(ctx, h.current(), u)
}

// Net implements destination.AccountHost. The operator set every address
// of an account from the environment, so its own addresses are reached
// as Publisher always has; an account added in the app (part C), and any
// address from content, goes through the network guard.
func (h *host) Net(kind destination.NetPolicy) destination.Net {
	if kind == destination.NetAccount && h.row.Source == "env" {
		return destination.TrustedNet()
	}
	return destination.ContentNet(netguard.IsBlockedIP)
}

// Logger implements destination.AccountHost.
func (h *host) Logger() *slog.Logger {
	return h.r.log.With("account", h.id, "account_type", h.row.Type)
}
