package threads

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"time"

	"github.com/geofox/publisher/internal/store"
)

// TokenStore is the slice of *store.Store the manager needs (interface for tests).
type TokenStore interface {
	GetThreadsToken() (*store.ThreadsToken, error)
	SaveThreadsToken(token string, expiresAt time.Time, seedHash string, refreshedAt time.Time) error
}

// tokenClient is the slice of *Client the manager needs (interface for tests).
type tokenClient interface {
	RefreshToken(ctx context.Context, current string) (string, time.Duration, error)
	SetToken(string)
}

// Notifier sends an operational alert (implemented by *notify.Webhook).
type Notifier interface {
	Alert(ctx context.Context, summary, body string) error
}

const (
	minTokenAge     = 24 * time.Hour      // Threads refuses refresh below this
	refreshWindow   = 10 * 24 * time.Hour // refresh once within this of expiry
	alertWindow     = 7 * 24 * time.Hour  // alert when this close to expiry and not refreshed
	seedProvisional = 11 * 24 * time.Hour // short seed expiry → forces a refresh ~24h in
	tickInterval    = 24 * time.Hour
)

type TokenManager struct {
	store    TokenStore
	client   tokenClient
	notifier Notifier
	seed     string // env THREADS_ACCESS_TOKEN
	now      func() time.Time
}

func NewTokenManager(s TokenStore, c tokenClient, n Notifier, seed string) *TokenManager {
	return &TokenManager{store: s, client: c, notifier: n, seed: seed, now: time.Now}
}

func seedHash(token string) string {
	h := sha256.Sum256([]byte(token))
	return hex.EncodeToString(h[:])
}

// needsSeed is true on first run (no persisted token) or when the operator
// rotated the env token (its hash no longer matches the stored seed_hash).
func needsSeed(persisted *store.ThreadsToken, hash string) bool {
	return persisted == nil || persisted.SeedHash != hash
}

// refreshDue gates refresh on token age (≥24h, so Threads won't reject it) AND
// proximity to expiry (≤10d).
func refreshDue(now, refreshedAt, expiresAt time.Time) bool {
	return now.Sub(refreshedAt) >= minTokenAge && expiresAt.Sub(now) <= refreshWindow
}

// ensureSeed adopts the env token when needed (first run or rotation) with a
// short provisional expiry, then points the live client at the working token.
func (m *TokenManager) ensureSeed(now time.Time) {
	hash := seedHash(m.seed)
	cur, err := m.store.GetThreadsToken()
	if err != nil {
		// Treat a load error as "no persisted token" — reseed from env rather
		// than trust a possibly-partial record.
		slog.Error("threads token load failed", "err", err)
		cur = nil
	}
	if needsSeed(cur, hash) {
		if err := m.store.SaveThreadsToken(m.seed, now.Add(seedProvisional), hash, now); err != nil {
			slog.Error("threads token seed failed", "err", err)
			return
		}
		m.client.SetToken(m.seed)
		slog.Info("threads token seeded from env")
		return
	}
	m.client.SetToken(cur.Token)
}

// Start seeds, runs one tick immediately, then ticks daily until ctx is done.
func (m *TokenManager) Start(ctx context.Context) {
	m.ensureSeed(m.now())
	m.tick(ctx)
	t := time.NewTicker(tickInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			m.tick(ctx)
		}
	}
}

func (m *TokenManager) tick(ctx context.Context) {
	now := m.now()
	cur, err := m.store.GetThreadsToken()
	if err != nil {
		slog.Error("threads token tick: load failed", "err", err)
		return
	}
	if cur == nil {
		slog.Warn("threads token tick: no token persisted yet")
		return
	}
	refreshed := false
	if refreshDue(now, cur.RefreshedAt, cur.ExpiresAt) {
		newTok, ttl, rerr := m.client.RefreshToken(ctx, cur.Token)
		if rerr != nil {
			slog.Error("threads token refresh failed", "err", rerr, "expires_at", cur.ExpiresAt)
			m.alert(ctx, "Threads token refresh failing", rerr.Error()+"; expires "+cur.ExpiresAt.Format(time.RFC3339))
			return
		}
		if err := m.store.SaveThreadsToken(newTok, now.Add(ttl), cur.SeedHash, now); err != nil {
			slog.Error("threads token save failed", "err", err)
			return
		}
		m.client.SetToken(newTok)
		slog.Info("threads token refreshed", "expires_at", now.Add(ttl))
		refreshed = true
	}
	// Fires once per tick until a refresh succeeds — intentional nagging so a
	// stuck token surfaces daily rather than silently expiring.
	if !refreshed && cur.ExpiresAt.Sub(now) <= alertWindow {
		m.alert(ctx, "Threads token near expiry", "expires "+cur.ExpiresAt.Format(time.RFC3339)+"; refresh is not succeeding")
	}
}

func (m *TokenManager) alert(ctx context.Context, summary, body string) {
	if err := m.notifier.Alert(ctx, summary, body); err != nil {
		slog.Error("alert webhook failed", "err", err)
	}
}
