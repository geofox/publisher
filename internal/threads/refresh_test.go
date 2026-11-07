package threads

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/geofox/publisher/internal/destination"
	"github.com/geofox/publisher/internal/destination/destinationtest"
)

// refreshServer answers the refresh endpoint with a fresh 60-day token,
// or with status when it isn't 200.
func refreshServer(t *testing.T, status int, calls *atomic.Int32) string {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if status != http.StatusOK {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"error":{"message":"bad token ` + r.URL.Query().Get("access_token") + `","code":190}}`))
			return
		}
		_, _ = w.Write([]byte(`{"access_token":"fresh","token_type":"bearer","expires_in":5184000}`))
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// openWithState opens an account the way the registry does, with the
// token state in its config.
func openWithState(t *testing.T, host *destinationtest.Host, refreshURL string, s tokenState, now time.Time) *Account {
	t.Helper()
	cfg := map[string]any{ConfigUserID: "me"}
	if !s.expiresAt.IsZero() {
		cfg[ConfigExpiresAt] = s.expiresAt.Format(time.RFC3339Nano)
		cfg[ConfigRefreshedAt] = s.refreshedAt.Format(time.RFC3339Nano)
	}
	secrets := map[string]string{SecretAccessToken: "cur"}
	host.WithState(secrets, cfg)
	acct, err := Driver{}.Open(context.Background(), destination.OpenRequest{Config: cfg, Secrets: secrets, Host: host})
	if err != nil {
		t.Fatal(err)
	}
	a := acct.(*Account)
	a.C.RefreshURL = refreshURL
	a.now = func() time.Time { return now }
	return a
}

// Ported from the token manager's refreshDue: due once the token is a day
// old and within 10 days of expiring.
func TestNextRefresh(t *testing.T) {
	now := time.Unix(1_000_000, 0).UTC()
	for _, tc := range []struct {
		name               string
		refreshed, expires time.Time
		due                bool
	}{
		{"younger than a day", now.Add(-12 * time.Hour), now.Add(5 * 24 * time.Hour), false},
		{"expiry more than 10 days away", now.Add(-48 * time.Hour), now.Add(30 * 24 * time.Hour), false},
		{"a day old and within 10 days", now.Add(-48 * time.Hour), now.Add(5 * 24 * time.Hour), true},
	} {
		a := &Account{tokens: tokenState{expiresAt: tc.expires, refreshedAt: tc.refreshed}}
		if due := !a.NextRefresh(now).After(now); due != tc.due {
			t.Errorf("%s: next refresh %v, due = %v", tc.name, a.NextRefresh(now), due)
		}
	}
	if !(&Account{}).NextRefresh(now).IsZero() {
		t.Error("an unknown expiry should never schedule a refresh")
	}
}

// Ported from TestSeedThenRefreshLearnsExpiry: a seeded token (provisional
// expiry) is refreshed after a day; the new token and its real expiry are
// saved together, then used.
func TestRefreshLearnsExpiryAndSaves(t *testing.T) {
	t0 := time.Unix(2_000_000, 0).UTC()
	var calls atomic.Int32
	host := destinationtest.NewHost(t).AllowDial()
	a := openWithState(t, host, refreshServer(t, 200, &calls), tokenState{expiresAt: t0.Add(SeedProvisional), refreshedAt: t0}, t0)
	if !a.C.TokenExpiry().IsZero() {
		t.Errorf("a provisional expiry was reported as known: %v", a.C.TokenExpiry())
	}
	now := t0.Add(25 * time.Hour)
	a.now = func() time.Time { return now }
	if a.NextRefresh(now).After(now) {
		t.Fatalf("not due after a day: %v", a.NextRefresh(now))
	}
	if err := a.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	secrets, cfg := host.State()
	if secrets[SecretAccessToken] != "fresh" || cfg[ConfigUserID] != "me" {
		t.Errorf("saved state = %v, %v", secrets, cfg)
	}
	exp, _, _ := destination.ConfigTime(cfg, ConfigExpiresAt)
	if got := exp.Sub(now); got != 60*24*time.Hour {
		t.Errorf("saved expiry is %s away, want 60 days", got)
	}
	if a.C.tok() != "fresh" || !a.C.TokenExpiry().Equal(exp) || !a.CredentialsExpireAt().Equal(exp) {
		t.Errorf("client token %q, expiry %v", a.C.tok(), a.C.TokenExpiry())
	}
	// Ported from TestRefreshResetsSchedule: not due again at once.
	if !a.NextRefresh(now).After(now) {
		t.Error("due again right after a refresh")
	}
	if calls.Load() != 1 {
		t.Errorf("%d refresh calls", calls.Load())
	}
}

// Ported from TestRefreshFailureAlertsAndKeepsToken: a failed refresh
// keeps the token and saves nothing; its error carries no token.
func TestRefreshFailureKeepsToken(t *testing.T) {
	now := time.Unix(4_000_000, 0).UTC()
	var calls atomic.Int32
	host := destinationtest.NewHost(t).AllowDial()
	a := openWithState(t, host, refreshServer(t, 400, &calls), tokenState{expiresAt: now.Add(5 * 24 * time.Hour), refreshedAt: now.Add(-48 * time.Hour)}, now)
	err := a.Refresh(context.Background())
	if err == nil {
		t.Fatal("a refused refresh succeeded")
	}
	if strings.Contains(err.Error(), "cur") {
		t.Errorf("error %q contains the token", err)
	}
	if a.C.tok() != "cur" || len(host.Saved()) != 0 {
		t.Errorf("token %q, saved %v", a.C.tok(), host.Saved())
	}
}

// A refreshed token that can't be saved isn't used: the saved one stays
// the working copy.
func TestRefreshSaveFailureKeepsToken(t *testing.T) {
	now := time.Unix(4_000_000, 0).UTC()
	var calls atomic.Int32
	url := refreshServer(t, 200, &calls)
	a := openWithState(t, destinationtest.NewHost(t).AllowDial(), url, tokenState{expiresAt: now.Add(5 * 24 * time.Hour), refreshedAt: now.Add(-48 * time.Hour)}, now)
	a.host = failingHost{a.host}
	if err := a.Refresh(context.Background()); err == nil {
		t.Fatal("refresh succeeded without saving")
	}
	if a.C.tok() != "cur" {
		t.Errorf("token %q is in use but was never saved", a.C.tok())
	}
}

type failingHost struct{ destination.AccountHost }

func (failingHost) Save(context.Context, destination.StateUpdate) error {
	return errors.New("disk full")
}

// Open refuses a malformed expiry, naming the field.
func TestOpenRefusesBadExpiry(t *testing.T) {
	_, err := Driver{}.Open(context.Background(), destination.OpenRequest{
		Config:  map[string]any{ConfigExpiresAt: "soon"},
		Secrets: map[string]string{SecretAccessToken: "t"},
		Host:    destinationtest.NewHost(t)})
	if err == nil || !strings.HasPrefix(err.Error(), ConfigExpiresAt+":") {
		t.Errorf("err = %v", err)
	}
}

// A token whose expiry isn't known is given a provisional one at Open,
// saved at once so a restart doesn't move the first refresh; a known
// expiry is left alone. An account with nowhere to save says nothing.
func TestOpenSavesProvisionalExpiry(t *testing.T) {
	host := destinationtest.NewHost(t)
	before := time.Now().UTC()
	a := openWithState(t, host, "", tokenState{}, before)
	saved := host.Saved()
	if len(saved) != 1 {
		t.Fatalf("%d saves", len(saved))
	}
	exp, ok, err := destination.ConfigTime(saved[0].Config, ConfigExpiresAt)
	if !ok || err != nil || exp.Before(before.Add(SeedProvisional-time.Minute)) || exp.After(time.Now().Add(SeedProvisional)) {
		t.Errorf("provisional expiry %v %v %v", exp, ok, err)
	}
	if !a.tokens.expiresAt.Equal(exp) || len(saved[0].Secrets) != 0 {
		t.Errorf("tokens %+v, saved %+v", a.tokens, saved[0])
	}
	// Known: no save.
	host = destinationtest.NewHost(t)
	openWithState(t, host, "", tokenState{expiresAt: before.Add(30 * 24 * time.Hour), refreshedAt: before}, before)
	if n := len(host.Saved()); n != 0 {
		t.Errorf("a known expiry was saved again (%d)", n)
	}
	// Nowhere to save: quiet; another failure is logged.
	for _, c := range []struct {
		err  error
		logs bool
	}{{destination.ErrStateUnsupported, false}, {errors.New("disk full"), true}} {
		h := &refusingHost{Host: destinationtest.NewHost(t), err: c.err}
		if _, err := (Driver{}).Open(context.Background(), destination.OpenRequest{Config: map[string]any{ConfigUserID: "me"},
			Secrets: map[string]string{SecretAccessToken: "cur"}, Host: h}); err != nil {
			t.Fatal(err)
		}
		if got := strings.Contains(h.log.String(), "provisional expiry was not saved"); got != c.logs {
			t.Errorf("%v: logged %v: %s", c.err, got, h.log.String())
		}
	}
}

// refusingHost answers every Save with err and keeps what is logged.
type refusingHost struct {
	*destinationtest.Host
	err error
	log strings.Builder
}

func (h *refusingHost) Save(context.Context, destination.StateUpdate) error { return h.err }
func (h *refusingHost) Logger() *slog.Logger                                { return slog.New(slog.NewTextHandler(&h.log, nil)) }
