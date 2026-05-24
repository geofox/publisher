package threads

import (
	"context"
	"testing"
	"time"

	"github.com/geofox/publisher/internal/store"
)

type fakeStore struct{ tk *store.ThreadsToken }

func (f *fakeStore) GetThreadsToken() (*store.ThreadsToken, error) { return f.tk, nil }
func (f *fakeStore) SaveThreadsToken(token string, exp time.Time, seed string, ref time.Time) error {
	f.tk = &store.ThreadsToken{Token: token, ExpiresAt: exp, SeedHash: seed, RefreshedAt: ref}
	return nil
}

type fakeClient struct {
	tok          string
	ttl          time.Duration
	err          error
	refreshCalls int
	setTokens    []string
}

func (f *fakeClient) RefreshToken(ctx context.Context, current string) (string, time.Duration, error) {
	f.refreshCalls++
	return f.tok, f.ttl, f.err
}
func (f *fakeClient) SetToken(s string) { f.setTokens = append(f.setTokens, s) }

type fakeNotifier struct{ msgs []string }

func (f *fakeNotifier) Alert(ctx context.Context, summary, body string) error {
	f.msgs = append(f.msgs, summary+": "+body)
	return nil
}

func TestNeedsSeed(t *testing.T) {
	if !needsSeed(nil, "h") {
		t.Error("nil persisted → needs seed")
	}
	if !needsSeed(&store.ThreadsToken{SeedHash: "old"}, "new") {
		t.Error("changed env hash → needs seed (adopt)")
	}
	if needsSeed(&store.ThreadsToken{SeedHash: "same"}, "same") {
		t.Error("unchanged hash → keep persisted")
	}
}

func TestRefreshDue(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	if refreshDue(now, now.Add(-12*time.Hour), now.Add(5*24*time.Hour)) {
		t.Error("token <24h old must not be due")
	}
	if refreshDue(now, now.Add(-48*time.Hour), now.Add(30*24*time.Hour)) {
		t.Error("expiry >10d away must not be due")
	}
	if !refreshDue(now, now.Add(-48*time.Hour), now.Add(5*24*time.Hour)) {
		t.Error("≥24h old + ≤10d to expiry must be due")
	}
}

func newMgr(s TokenStore, c tokenClient, n Notifier, seed string, now func() time.Time) *TokenManager {
	m := NewTokenManager(s, c, n, seed)
	m.now = now
	return m
}

func TestSeedThenRefreshLearnsExpiry(t *testing.T) {
	t0 := time.Unix(2_000_000, 0)
	cur := t0
	fs := &fakeStore{}
	fc := &fakeClient{tok: "fresh", ttl: 60 * 24 * time.Hour}
	fn := &fakeNotifier{}
	m := newMgr(fs, fc, fn, "envtoken", func() time.Time { return cur })

	m.ensureSeed(cur)
	if fs.tk == nil || fs.tk.Token != "envtoken" {
		t.Fatalf("seed failed: %+v", fs.tk)
	}
	if len(fc.setTokens) != 1 || fc.setTokens[0] != "envtoken" {
		t.Errorf("seed should set client token to env, got %v", fc.setTokens)
	}

	cur = t0.Add(25 * time.Hour)
	m.tick(context.Background())
	if fc.refreshCalls != 1 {
		t.Fatalf("expected 1 refresh, got %d", fc.refreshCalls)
	}
	if fs.tk.Token != "fresh" {
		t.Errorf("token not updated to refreshed value: %+v", fs.tk)
	}
	if got := fs.tk.ExpiresAt.Sub(cur); got < 59*24*time.Hour {
		t.Errorf("expiry not learned from refresh: %s", got)
	}
	if len(fn.msgs) != 0 {
		t.Errorf("no alert expected on success, got %v", fn.msgs)
	}
}

func TestRefreshNotDue(t *testing.T) {
	now := time.Unix(3_000_000, 0)
	fs := &fakeStore{tk: &store.ThreadsToken{Token: "cur", ExpiresAt: now.Add(30 * 24 * time.Hour), SeedHash: "h", RefreshedAt: now.Add(-48 * time.Hour)}}
	fc := &fakeClient{}
	m := newMgr(fs, fc, &fakeNotifier{}, "envtoken", func() time.Time { return now })
	m.tick(context.Background())
	if fc.refreshCalls != 0 {
		t.Errorf("refresh should not fire when not due")
	}
}

func TestRefreshFailureAlertsAndKeepsToken(t *testing.T) {
	now := time.Unix(4_000_000, 0)
	fs := &fakeStore{tk: &store.ThreadsToken{Token: "cur", ExpiresAt: now.Add(5 * 24 * time.Hour), SeedHash: "h", RefreshedAt: now.Add(-48 * time.Hour)}}
	fc := &fakeClient{err: context.DeadlineExceeded}
	fn := &fakeNotifier{}
	m := newMgr(fs, fc, fn, "envtoken", func() time.Time { return now })
	m.tick(context.Background())
	if fc.refreshCalls != 1 {
		t.Errorf("refresh should be attempted")
	}
	if fs.tk.Token != "cur" {
		t.Errorf("token must be unchanged on failure: %+v", fs.tk)
	}
	if len(fn.msgs) != 1 {
		t.Errorf("exactly one alert expected, got %v", fn.msgs)
	}
}

func TestNearExpiryAlertWhenTooYoungToRefresh(t *testing.T) {
	now := time.Unix(5_000_000, 0)
	fs := &fakeStore{tk: &store.ThreadsToken{Token: "cur", ExpiresAt: now.Add(5 * 24 * time.Hour), SeedHash: "h", RefreshedAt: now.Add(-1 * time.Hour)}}
	fc := &fakeClient{}
	fn := &fakeNotifier{}
	m := newMgr(fs, fc, fn, "envtoken", func() time.Time { return now })
	m.tick(context.Background())
	if fc.refreshCalls != 0 {
		t.Errorf("should not refresh a <24h token")
	}
	if len(fn.msgs) != 1 {
		t.Errorf("near-expiry alert expected, got %v", fn.msgs)
	}
}

func TestEnsureSeedAdoptsRotatedEnv(t *testing.T) {
	now := time.Unix(6_000_000, 0)
	// Store holds a token seeded from a DIFFERENT env value (operator rotated it).
	fs := &fakeStore{tk: &store.ThreadsToken{
		Token: "oldtok", ExpiresAt: now.Add(40 * 24 * time.Hour),
		SeedHash: seedHash("oldenv"), RefreshedAt: now.Add(-5 * 24 * time.Hour),
	}}
	fc := &fakeClient{}
	m := newMgr(fs, fc, &fakeNotifier{}, "newenv", func() time.Time { return now })

	m.ensureSeed(now)
	if fs.tk.Token != "newenv" || fs.tk.SeedHash != seedHash("newenv") {
		t.Errorf("rotated env not adopted: %+v", fs.tk)
	}
	if len(fc.setTokens) != 1 || fc.setTokens[0] != "newenv" {
		t.Errorf("client token not set to new env: %v", fc.setTokens)
	}
}

func TestRefreshResetsSchedule(t *testing.T) {
	now := time.Unix(7_000_000, 0)
	fs := &fakeStore{tk: &store.ThreadsToken{Token: "cur", ExpiresAt: now.Add(5 * 24 * time.Hour), SeedHash: "h", RefreshedAt: now.Add(-48 * time.Hour)}}
	fc := &fakeClient{tok: "fresh", ttl: 60 * 24 * time.Hour}
	m := newMgr(fs, fc, &fakeNotifier{}, "envtoken", func() time.Time { return now })

	m.tick(context.Background()) // due → refreshes once
	m.tick(context.Background()) // same clock: refreshed_at=now, expiry 60d → not due
	if fc.refreshCalls != 1 {
		t.Errorf("second tick should not re-refresh, got %d calls", fc.refreshCalls)
	}
}
