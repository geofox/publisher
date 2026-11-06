package drivers

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/geofox/publisher/internal/destination"
	"github.com/geofox/publisher/internal/store"
	"github.com/geofox/publisher/internal/threads"
)

type tokenStore struct {
	tk     *store.ThreadsToken
	err    error
	writes int
}

func (s *tokenStore) GetThreadsToken() (*store.ThreadsToken, error) { return s.tk, s.err }
func (s *tokenStore) SaveThreadsToken(tok string, exp time.Time, hash string, ref time.Time) error {
	s.writes++
	s.tk = &store.ThreadsToken{Token: tok, ExpiresAt: exp.UTC(), SeedHash: hash, RefreshedAt: ref.UTC()}
	return nil
}

var threadsRow = store.Account{ID: "acc_t", Type: threads.Type, Source: "env", EnvKey: "default"}

func restore(t *testing.T, l LegacyState, envToken string) (map[string]string, map[string]any) {
	t.Helper()
	s, c, err := l.Restore(context.Background(), threadsRow,
		map[string]string{threads.SecretAccessToken: envToken}, map[string]any{threads.ConfigUserID: "me"})
	if err != nil {
		t.Fatal(err)
	}
	return s, c
}

func times(t *testing.T, c map[string]any) (exp, ref time.Time) {
	t.Helper()
	exp, _, err1 := destination.ConfigTime(c, threads.ConfigExpiresAt)
	ref, _, err2 := destination.ConfigTime(c, threads.ConfigRefreshedAt)
	if err := errors.Join(err1, err2); err != nil {
		t.Fatal(err)
	}
	return exp, ref
}

// Ported from TestNeedsSeed and the token manager's seeding: the first
// start adopts the environment's token with a provisional expiry and
// saves it; a restart keeps that schedule.
func TestLegacyStateSeeds(t *testing.T) {
	now := time.Unix(6_000_000, 0).UTC()
	st := &tokenStore{}
	l := LegacyState{Store: st, Now: func() time.Time { return now }}
	s, c := restore(t, l, "envtok")
	exp, ref := times(t, c)
	if s[threads.SecretAccessToken] != "envtok" || !exp.Equal(now.Add(threads.SeedProvisional)) || !ref.Equal(now) || c[threads.ConfigUserID] != "me" {
		t.Errorf("restored %v, %v", s, c)
	}
	if st.tk == nil || st.tk.SeedHash != threads.SeedHash("envtok") {
		t.Fatalf("seed not saved: %+v", st.tk)
	}
	later := LegacyState{Store: st, Now: func() time.Time { return now.Add(time.Hour) }}
	_, c = restore(t, later, "envtok")
	if _, ref := times(t, c); !ref.Equal(now) || st.writes != 1 {
		t.Errorf("a restart moved the schedule: refreshed %v, %d writes", ref, st.writes)
	}
}

// A token refreshed since is what the account opens with, while the
// environment still holds the token it descends from.
func TestLegacyStateKeepsRefreshedToken(t *testing.T) {
	now := time.Unix(6_000_000, 0).UTC()
	st := &tokenStore{tk: &store.ThreadsToken{Token: "fresh", ExpiresAt: now.Add(50 * 24 * time.Hour),
		SeedHash: threads.SeedHash("envtok"), RefreshedAt: now.Add(-10 * 24 * time.Hour)}}
	s, c := restore(t, LegacyState{Store: st, Now: func() time.Time { return now }}, "envtok")
	exp, _ := times(t, c)
	if s[threads.SecretAccessToken] != "fresh" || !exp.Equal(now.Add(50*24*time.Hour)) || st.writes != 0 {
		t.Errorf("restored %v, %v (%d writes)", s, c, st.writes)
	}
}

// Ported from TestEnsureSeedAdoptsRotatedEnv: a new token in the
// environment replaces the saved one.
func TestLegacyStateAdoptsRotatedToken(t *testing.T) {
	now := time.Unix(6_000_000, 0).UTC()
	st := &tokenStore{tk: &store.ThreadsToken{Token: "oldtok", ExpiresAt: now.Add(40 * 24 * time.Hour),
		SeedHash: threads.SeedHash("oldenv"), RefreshedAt: now.Add(-5 * 24 * time.Hour)}}
	s, _ := restore(t, LegacyState{Store: st, Now: func() time.Time { return now }}, "newenv")
	if s[threads.SecretAccessToken] != "newenv" || st.tk.SeedHash != threads.SeedHash("newenv") {
		t.Errorf("rotated token not adopted: %v, %+v", s, st.tk)
	}
}

// A saved state that can't be read keeps the account shut instead of
// overwriting a refreshed token with the environment's.
func TestLegacyStateReadErrorWritesNothing(t *testing.T) {
	st := &tokenStore{err: errors.New("database is locked")}
	_, _, err := LegacyState{Store: st}.Restore(context.Background(), threadsRow,
		map[string]string{threads.SecretAccessToken: "envtok"}, nil)
	if err == nil || st.writes != 0 {
		t.Errorf("err = %v, %d writes", err, st.writes)
	}
}

// Save merges into the row: values left out keep theirs, the seed hash
// always stays.
func TestLegacyStateSaveMerges(t *testing.T) {
	now := time.Unix(6_000_000, 0).UTC()
	st := &tokenStore{tk: &store.ThreadsToken{Token: "cur", ExpiresAt: now, SeedHash: "h", RefreshedAt: now.Add(-time.Hour)}}
	l := LegacyState{Store: st}
	err := l.Save(context.Background(), threadsRow, destination.StateUpdate{
		Secrets: map[string]string{threads.SecretAccessToken: "fresh"},
		Config:  map[string]any{threads.ConfigExpiresAt: now.Add(60 * 24 * time.Hour).Format(time.RFC3339)},
	})
	if err != nil {
		t.Fatal(err)
	}
	if st.tk.Token != "fresh" || !st.tk.ExpiresAt.Equal(now.Add(60*24*time.Hour)) || !st.tk.RefreshedAt.Equal(now.Add(-time.Hour)) || st.tk.SeedHash != "h" {
		t.Errorf("saved %+v", st.tk)
	}
	if err := l.Save(context.Background(), threadsRow, destination.StateUpdate{Secrets: map[string]string{threads.SecretAccessToken: ""}}); err == nil {
		t.Error("the token was dropped")
	}
}

// Other accounts have no state yet.
func TestLegacyStateOtherTypes(t *testing.T) {
	row := store.Account{ID: "acc_m", Type: "mastodon", Source: "env"}
	l := LegacyState{Store: &tokenStore{}}
	s, c, err := l.Restore(context.Background(), row, map[string]string{"access_token": "x"}, map[string]any{"instance_url": "u"})
	if err != nil || s["access_token"] != "x" || c["instance_url"] != "u" {
		t.Errorf("restore = %v, %v, %v", s, c, err)
	}
	if err := l.Save(context.Background(), row, destination.StateUpdate{}); !errors.Is(err, ErrStateUnsupported) {
		t.Errorf("save = %v", err)
	}
}
