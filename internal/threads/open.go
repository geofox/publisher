package threads

import (
	"context"
	"errors"
	"time"

	"github.com/geofox/publisher/internal/destination"
)

// Account configuration and secret names.
const (
	ConfigUserID      = "user_id" // default "me"
	SecretAccessToken = "access_token"
)

// Open implements destination.Driver.
func (Driver) Open(ctx context.Context, req destination.OpenRequest) (destination.Account, error) {
	userID, err := destination.ConfigString(req.Config, ConfigUserID)
	if err != nil {
		return nil, destination.InvalidField(ConfigUserID, err.Error())
	}
	token := req.Secrets[SecretAccessToken]
	if token == "" {
		return nil, destination.InvalidField(SecretAccessToken, "required")
	}
	tokens, err := openTokenState(req.Config)
	if err != nil {
		return nil, err
	}
	if tokens.expiresAt.IsZero() {
		// A token whose expiry isn't known (just pasted, or new in the
		// environment): record a provisional one at once, so the first
		// refresh, a day from now, learns the real expiry and a restart
		// doesn't move it.
		now := time.Now().UTC()
		tokens = tokenState{expiresAt: now.Add(SeedProvisional), refreshedAt: now}
		if err := req.Host.Save(ctx, destination.StateUpdate{Config: map[string]any{
			ConfigExpiresAt:   tokens.expiresAt.Format(time.RFC3339Nano),
			ConfigRefreshedAt: tokens.refreshedAt.Format(time.RFC3339Nano),
		}}); err != nil && !errors.Is(err, destination.ErrStateUnsupported) {
			req.Host.Logger().Warn("threads: the token's provisional expiry was not saved; it is set again at the next start", "err", err)
		}
	}
	a := &Account{C: NewWithNet(token, userID, req.Host.Net(destination.NetAccount)), host: req.Host, tokens: tokens}
	a.C.SetTokenExpiry(tokens.known())
	return a, nil
}
