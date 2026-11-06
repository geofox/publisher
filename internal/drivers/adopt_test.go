package drivers

import (
	"context"
	"database/sql"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/geofox/publisher/internal/destination"
	"github.com/geofox/publisher/internal/nostr"
	"github.com/geofox/publisher/internal/store"
)

// The 2.x relay-sync list goes to the first env Nostr account; every env
// Nostr account without a list starts with its own SYNC_RELAYS; a Nostr
// account added from the web is left alone; and running it again (every
// start) never brings back a relay the owner removed.
func TestAdoptLegacySyncRelays(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "p.db")
	st, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	legacy := []string{"wss://old-a.example", "wss://old-b.example"}
	setLegacySyncRelays(t, path, legacy...)
	rows, err := st.SeedEnvAccounts(ctx, store.DefaultOwner, []store.EnvAccount{
		{Type: nostr.Type, Key: destination.EnvKeyDefault, Label: "Nostr"},
		{Type: nostr.Type, Key: "2", Label: "Nostr 2"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.InsertAccount(ctx, store.Account{ID: "acc_web", Type: nostr.Type, Label: "Web", Source: "web", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	seed := func(urls ...string) map[string]any { return map[string]any{nostr.ConfigSyncRelaysSeed: urls} }
	env := []EnvAccount{
		{Type: nostr.Type, EnvAccount: destination.EnvAccount{Key: destination.EnvKeyDefault, Config: seed("wss://env-1.example")}},
		{Type: nostr.Type, EnvAccount: destination.EnvAccount{Key: "2", Config: seed("wss://env-2.example")}},
	}
	var first, second string
	for _, r := range rows {
		if r.EnvKey == destination.EnvKeyDefault {
			first = r.ID
		} else {
			second = r.ID
		}
	}
	// The numbered account comes first here: the 2.x list still goes to
	// the account of the unnumbered variables, 2.x's one account.
	all := []store.Account{{ID: second, Type: nostr.Type, Source: "env", EnvKey: "2"},
		{ID: first, Type: nostr.Type, Source: "env", EnvKey: destination.EnvKeyDefault},
		{ID: "acc_web", Type: nostr.Type, Source: "web"}}
	if err := AdoptLegacy(ctx, st, all, env); err != nil {
		t.Fatal(err)
	}
	list := func(id string) []string {
		t.Helper()
		got, err := st.AccountSyncRelays(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	if got := list(first); !reflect.DeepEqual(got, legacy) {
		t.Errorf("first env account = %v, want the 2.x list %v", got, legacy)
	}
	if got := list(second); !reflect.DeepEqual(got, []string{"wss://env-2.example"}) {
		t.Errorf("second env account = %v, want its SYNC_RELAYS", got)
	}
	if got := list("acc_web"); len(got) != 0 {
		t.Errorf("web account = %v, want no list", got)
	}

	// The owner trims the first list; the next start keeps the trim.
	if err := st.SetAccountSyncRelays(ctx, first, legacy[:1]); err != nil {
		t.Fatal(err)
	}
	if err := AdoptLegacy(ctx, st, all, env); err != nil {
		t.Fatal(err)
	}
	if got := list(first); !reflect.DeepEqual(got, legacy[:1]) {
		t.Errorf("after a restart the first list = %v, want the owner's %v", got, legacy[:1])
	}
	if got := list(second); !reflect.DeepEqual(got, []string{"wss://env-2.example"}) {
		t.Errorf("after a restart the second list = %v", got)
	}
	// The owner empties both lists; they stay empty after a restart.
	for _, id := range []string{first, second} {
		if err := st.SetAccountSyncRelays(ctx, id, nil); err != nil {
			t.Fatal(err)
		}
	}
	if err := AdoptLegacy(ctx, st, all, env); err != nil {
		t.Fatal(err)
	}
	if a, b := list(first), list(second); len(a)+len(b) != 0 {
		t.Errorf("emptied lists came back after a restart: %v, %v", a, b)
	}
}

// setLegacySyncRelays writes the 2.x global relay-sync list (table
// sync_relays, which nothing writes any more) through a second connection.
func setLegacySyncRelays(t *testing.T, path string, urls ...string) {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(15000)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	now := time.Now().UTC()
	for i, u := range urls {
		if _, err := db.Exec(`INSERT INTO sync_relays (url, added_at) VALUES (?, ?)`, u, now.Add(time.Duration(i)*time.Second)); err != nil {
			t.Fatal(err)
		}
	}
}
