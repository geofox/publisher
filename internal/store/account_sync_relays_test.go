package store

import (
	"context"
	"reflect"
	"testing"
	"time"
)

func TestAccountSyncRelays(t *testing.T) {
	s := openEngineStore(t)
	ctx := context.Background()
	a, b := seedTestAccount(t, s, "nostr", "a"), seedTestAccount(t, s, "nostr", "b")
	if err := s.SeedAccountSyncRelaysIfEmpty(ctx, a, []string{"wss://1", "", "wss://2"}); err != nil {
		t.Fatal(err)
	}
	// A list that has relays is never re-seeded.
	if err := s.SeedAccountSyncRelaysIfEmpty(ctx, a, []string{"wss://9"}); err != nil {
		t.Fatal(err)
	}
	got, err := s.AccountSyncRelays(ctx, a)
	if want := []string{"wss://1", "wss://2"}; err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("a = %v, %v; want %v", got, err, want)
	}
	_ = s.SetAccountSyncRelays(ctx, b, []string{"wss://x"})
	if err := s.SetAccountSyncRelays(ctx, a, []string{"wss://3", "wss://4"}); err != nil {
		t.Fatal(err)
	}
	got, _ = s.AccountSyncRelays(ctx, a)
	if want := []string{"wss://3", "wss://4"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("after set: %v, want %v", got, want)
	}
	if got, _ := s.AccountSyncRelays(ctx, b); !reflect.DeepEqual(got, []string{"wss://x"}) {
		t.Fatalf("b = %v: lists must not mix", got)
	}
	if err := s.SetAccountSyncRelays(ctx, b, nil); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.AccountSyncRelays(ctx, b); len(got) != 0 {
		t.Fatalf("b after clearing = %v", got)
	}
}

// The global list from before v11 moves to the first account that adopts
// it, in order, once.
func TestAdoptLegacySyncRelays(t *testing.T) {
	s := openEngineStore(t)
	ctx := context.Background()
	setLegacySyncRelays(t, s, "wss://old1", "wss://old2")
	a := seedTestAccount(t, s, "nostr", "a")
	if ok, err := s.AdoptLegacySyncRelays(ctx, a); err != nil || !ok {
		t.Fatalf("adopt = %v, %v", ok, err)
	}
	if ok, err := s.AdoptLegacySyncRelays(ctx, a); err != nil || ok {
		t.Fatalf("second adopt = %v, %v; want a no-op", ok, err)
	}
	got, _ := s.AccountSyncRelays(ctx, a)
	if want := []string{"wss://old1", "wss://old2"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("adopted %v, want %v", got, want)
	}
	// With nothing to adopt, the seed applies.
	b := seedTestAccount(t, s, "nostr", "b")
	setLegacySyncRelays(t, s)
	if ok, _ := s.AdoptLegacySyncRelays(ctx, b); ok {
		t.Fatal("adopted an empty list")
	}
}

// seedTestAccount inserts an enabled env account of typ with env key key.
func seedTestAccount(t *testing.T, s *Store, typ, key string) string {
	t.Helper()
	id := newAccountID()
	if _, err := s.sql.Exec(`INSERT INTO accounts (id, owner_id, type, label, source, env_key, enabled, created_at, updated_at)
		VALUES (?, ?, ?, ?, 'env', ?, 1, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)`, id, DefaultOwner, typ, typ, key); err != nil {
		t.Fatal(err)
	}
	return id
}

// setLegacySyncRelays sets the global list as 2.x kept it, in order.
func setLegacySyncRelays(t *testing.T, s *Store, urls ...string) {
	t.Helper()
	if _, err := s.sql.Exec(`DELETE FROM sync_relays`); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	for i, u := range urls {
		if _, err := s.sql.Exec(`INSERT INTO sync_relays (url, added_at) VALUES (?, ?)`, u, now.Add(time.Duration(i)*time.Second)); err != nil {
			t.Fatal(err)
		}
	}
}
