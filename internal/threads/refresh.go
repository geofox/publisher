package threads

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"time"

	"github.com/geofox/publisher/internal/destination"
)

// Token state an account keeps in its config, next to the token secret.
const (
	// ConfigExpiresAt is when the token stops working (RFC 3339).
	ConfigExpiresAt = "expires_at"
	// ConfigRefreshedAt is when the token was issued or last refreshed
	// (RFC 3339): Threads refuses to refresh a token less than a day old.
	ConfigRefreshedAt = "refreshed_at"
)

// SeedProvisional is the expiry recorded for a token whose real expiry is
// unknown (one pasted or set in the environment): short enough that the
// first refresh, a day later, learns the real one.
const SeedProvisional = 11 * 24 * time.Hour

const (
	minTokenAge   = 24 * time.Hour      // Threads refuses to refresh a younger token
	refreshWindow = 10 * 24 * time.Hour // refresh once within this of expiry
)

// SeedHash identifies an environment token without keeping it: the stored
// token's seed hash says which environment token it descends from.
func SeedHash(token string) string {
	h := sha256.Sum256([]byte(token))
	return hex.EncodeToString(h[:])
}

var (
	_ destination.Refresher = (*Account)(nil)
	_ destination.Expirer   = (*Account)(nil)
)

// tokenState is when the account's token expires and was last refreshed.
type tokenState struct {
	expiresAt, refreshedAt time.Time
}

// openTokenState reads the token state from the account's config.
func openTokenState(cfg map[string]any) (tokenState, error) {
	exp, _, err := destination.ConfigTime(cfg, ConfigExpiresAt)
	if err != nil {
		return tokenState{}, destination.InvalidField(ConfigExpiresAt, err.Error())
	}
	ref, _, err := destination.ConfigTime(cfg, ConfigRefreshedAt)
	if err != nil {
		return tokenState{}, destination.InvalidField(ConfigRefreshedAt, err.Error())
	}
	return tokenState{expiresAt: exp, refreshedAt: ref}, nil
}

// known is the expiry the account can report: none while it is only the
// provisional one.
func (s tokenState) known() time.Time {
	if s.expiresAt.Equal(s.refreshedAt.Add(SeedProvisional)) {
		return time.Time{}
	}
	return s.expiresAt
}

func (a *Account) state() tokenState {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.tokens
}

func (a *Account) clock() time.Time {
	if a.now != nil {
		return a.now()
	}
	return time.Now()
}

// NextRefresh implements destination.Refresher: once the token is a day
// old and within 10 days of expiring (Threads refuses younger tokens; a
// long-lived token lasts 60 days). Never when the expiry is unknown.
func (a *Account) NextRefresh(time.Time) time.Time {
	s := a.state()
	if s.expiresAt.IsZero() {
		return time.Time{}
	}
	at := s.expiresAt.Add(-refreshWindow)
	if young := s.refreshedAt.Add(minTokenAge); young.After(at) {
		at = young
	}
	return at
}

// CredentialsExpireAt implements destination.Expirer. The provisional
// expiry counts: it is when the token must have been refreshed by.
func (a *Account) CredentialsExpireAt() time.Time { return a.state().expiresAt }

var errNoHost = errors.New("this account can't save a refreshed token")

// Refresh implements destination.Refresher: it exchanges the token for a
// fresh one and saves the token and its expiry together before using it,
// so a token that couldn't be saved is never the only copy.
func (a *Account) Refresh(ctx context.Context) error {
	if a.host == nil {
		return errNoHost
	}
	tok, ttl, err := a.C.RefreshToken(ctx, a.C.tok())
	if err != nil {
		return err // RefreshToken scrubs the token from its errors
	}
	now := a.clock().UTC()
	s := tokenState{expiresAt: now.Add(ttl), refreshedAt: now}
	if err := a.host.Save(ctx, destination.StateUpdate{
		Secrets: map[string]string{SecretAccessToken: tok},
		Config: map[string]any{
			ConfigExpiresAt:   s.expiresAt.Format(time.RFC3339Nano),
			ConfigRefreshedAt: s.refreshedAt.Format(time.RFC3339Nano),
		},
	}); err != nil {
		return err
	}
	a.mu.Lock()
	a.tokens = s
	a.mu.Unlock()
	a.C.SetToken(tok)
	a.C.SetTokenExpiry(s.known())
	return nil
}
