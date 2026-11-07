package store

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

const testKID = "0123456789abcdef"

func sealedFor(owner, id, name, value string) SealedSecret {
	return SealedSecret{Owner: owner, Scope: SecretScopeAccount, Ref: id, Name: name, Sealed: "v1." + testKID + "." + value, KeyID: testKID}
}

func newDBAccount(t *testing.T, s *Store, typ, remote string, secrets ...string) Account {
	t.Helper()
	id := NewAccountID()
	var ss []SealedSecret
	for _, n := range secrets {
		ss = append(ss, sealedFor(DefaultOwner, id, n, "SEALED-"+n))
	}
	a, err := s.CreateAccountWithSecrets(context.Background(), NewAccount{ID: id, OwnerID: DefaultOwner, Type: typ,
		Label: "Work", Handle: "@me", RemoteID: remote, AuthMethod: "token", Config: map[string]any{"instance_url": "https://m.example"},
		Secrets: ss, Actor: "session"})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// v13 on a v12 database: the columns arrive with their defaults, the
// secrets table and both indexes exist, and an env account keeps working.
func TestMigrationV13FromV12(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v12.db")
	db, err := openWith(t, path, migrations[:12])
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	mustExec(t, db, `INSERT INTO accounts (id, owner_id, type, label, source, env_key, enabled, created_at, updated_at, remote_id) VALUES
		('acc_m', 'own_default', 'mastodon', 'Mastodon', 'env', 'default', 1, ?, ?, 'https://m.example/@me')`, now, now)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	a, err := s.Account(ctx, DefaultOwner, "acc_m")
	if err != nil {
		t.Fatal(err)
	}
	if a.AuthMethod != "" || a.Config != "{}" || !a.DeletedAt.IsZero() || a.Source != SourceEnv {
		t.Errorf("migrated account %+v", a)
	}
	for _, idx := range []string{"idx_secrets_key", "idx_accounts_remote"} {
		var n int
		if err := s.sql.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'index' AND name = ?`, idx).Scan(&n); err != nil || n != 1 {
			t.Errorf("index %s: %d %v", idx, n, err)
		}
	}
	// A backup was written before the migration.
	if _, err := os.Stat(path + ".pre-v13.bak"); err != nil {
		t.Errorf("no backup: %v", err)
	}
}

func TestSecureDeleteIsOn(t *testing.T) {
	s := openTestStore(t)
	var on int
	if err := s.sql.QueryRow(`PRAGMA secure_delete`).Scan(&on); err != nil || on != 1 {
		t.Fatalf("secure_delete = %d %v", on, err)
	}
}

func TestCreateAccountWithSecrets(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	a := newDBAccount(t, s, "mastodon", "https://m.example/@me", "access_token")
	if a.Source != SourceDB || a.AuthMethod != "token" || !a.Enabled || a.DefaultSelected || !a.PublicFeed || a.RemoteID != "https://m.example/@me" {
		t.Errorf("account %+v", a)
	}
	if !strings.Contains(a.Config, `"instance_url":"https://m.example"`) {
		t.Errorf("config %s", a.Config)
	}
	got, err := s.Secrets(ctx, DefaultOwner, SecretScopeAccount, a.ID)
	if err != nil || len(got) != 1 || got[0].Name != "access_token" || got[0].Sealed != "v1."+testKID+".SEALED-access_token" || got[0].KeyID != testKID {
		t.Fatalf("secrets %+v %v", got, err)
	}
	// The event names the secret, never its value.
	var data string
	if err := s.sql.QueryRow(`SELECT data FROM events WHERE type = 'account.created'`).Scan(&data); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(data, `"access_token"`) || strings.Contains(data, "SEALED") {
		t.Errorf("event %s", data)
	}

	// The same account again is refused, naming the first.
	_, err = s.CreateAccountWithSecrets(ctx, NewAccount{ID: NewAccountID(), OwnerID: DefaultOwner, Type: "mastodon", RemoteID: "https://m.example/@me"})
	var dup *DuplicateAccountError
	if !errors.As(err, &dup) || dup.ID != a.ID {
		t.Errorf("duplicate: %v", err)
	}
	// Another type with that remote id is another account.
	newDBAccount(t, s, "bluesky", "https://m.example/@me")

	// A secret bound to another account is refused before anything is
	// written.
	id := NewAccountID()
	_, err = s.CreateAccountWithSecrets(ctx, NewAccount{ID: id, OwnerID: DefaultOwner, Type: "threads",
		Secrets: []SealedSecret{sealedFor(DefaultOwner, "acc_other", "access_token", "x")}})
	if err == nil {
		t.Fatal("misbound secret accepted")
	}
	if _, err := s.Account(ctx, DefaultOwner, id); !errors.Is(err, ErrAccountNotFound) {
		t.Errorf("account written anyway: %v", err)
	}
	// A value not sealed under the key it names is refused.
	bad := sealedFor(DefaultOwner, id, "access_token", "x")
	bad.KeyID = "fedcba9876543210"
	if _, err := s.CreateAccountWithSecrets(ctx, NewAccount{ID: id, OwnerID: DefaultOwner, Type: "threads", Secrets: []SealedSecret{bad}}); err == nil {
		t.Error("key id mismatch accepted")
	}
}

// The partial unique index closes the race between two creates that both
// passed the check.
func TestDuplicateRaceIsClosedByTheIndex(t *testing.T) {
	s := openTestStore(t)
	newDBAccount(t, s, "nostr", "pub1")
	_, err := s.sql.Exec(`INSERT INTO accounts (id, owner_id, type, label, source, enabled, created_at, updated_at, remote_id)
		VALUES ('acc_race', 'own_default', 'nostr', 'x', 'db', 1, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP, 'pub1')`)
	if !isUniqueRemote(err) {
		t.Fatalf("second live row: %v", err)
	}
	// A deleted one doesn't count.
	if _, err := s.sql.Exec(`INSERT INTO accounts (id, owner_id, type, label, source, enabled, created_at, updated_at, remote_id, deleted_at)
		VALUES ('acc_gone', 'own_default', 'nostr', 'x', 'db', 0, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP, 'pub1', CURRENT_TIMESTAMP)`); err != nil {
		t.Errorf("deleted row: %v", err)
	}
}

func TestReplaceAccountCredentials(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	a := newDBAccount(t, s, "bluesky", "did:plc:me", "app_password", "session")
	c := CredentialChange{AuthMethod: "app_password", Config: map[string]any{"pds_url": "https://pds2.example"},
		Secrets: []SealedSecret{sealedFor(DefaultOwner, a.ID, "app_password", "NEW")}, RemoteID: "did:plc:me",
		IfUpdatedAt: a.UpdatedAt, Fields: []string{"pds_url", "app_password"}, Actor: "session"}
	b, err := s.ReplaceAccountCredentials(ctx, DefaultOwner, a.ID, c)
	if err != nil {
		t.Fatal(err)
	}
	if !b.UpdatedAt.After(a.UpdatedAt) || !strings.Contains(b.Config, "pds2") {
		t.Errorf("after %+v", b)
	}
	got, _ := s.Secrets(ctx, DefaultOwner, SecretScopeAccount, a.ID)
	if len(got) != 1 || got[0].Name != "app_password" || !strings.HasSuffix(got[0].Sealed, "NEW") {
		t.Errorf("secrets after %+v (every old one goes)", got)
	}
	// A stale ETag is refused.
	if _, err := s.ReplaceAccountCredentials(ctx, DefaultOwner, a.ID, c); !errors.Is(err, ErrAccountConflict) {
		t.Errorf("stale: %v", err)
	}
	// The new credentials can't be another account the owner has.
	other := newDBAccount(t, s, "bluesky", "did:plc:other")
	c.IfUpdatedAt, c.RemoteID = time.Time{}, "did:plc:other"
	var dup *DuplicateAccountError
	if _, err := s.ReplaceAccountCredentials(ctx, DefaultOwner, a.ID, c); !errors.As(err, &dup) || dup.ID != other.ID {
		t.Errorf("identity of another account: %v", err)
	}
	// An env account's credentials are the environment's.
	if _, err := testAccount(s, "threads"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReplaceAccountCredentials(ctx, DefaultOwner, "acc_threads", CredentialChange{}); !errors.Is(err, ErrNotDBAccount) {
		t.Errorf("env account: %v", err)
	}
}

func TestAdoptAccount(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	if _, err := testAccount(s, "mastodon"); err != nil {
		t.Fatal(err)
	}
	a, err := s.AdoptAccount(ctx, DefaultOwner, "acc_mastodon", CredentialChange{AuthMethod: "token",
		Config: map[string]any{"instance_url": "https://m.example"}, Secrets: []SealedSecret{sealedFor(DefaultOwner, "acc_mastodon", "access_token", "T")},
		Fields: []string{"instance_url", "access_token"}, Actor: "session"})
	if err != nil {
		t.Fatal(err)
	}
	if a.Source != SourceDB || a.EnvKey != "test-mastodon" || a.ID != "acc_mastodon" {
		t.Errorf("adopted %+v", a)
	}
	if _, err := s.AdoptAccount(ctx, DefaultOwner, "acc_mastodon", CredentialChange{}); err == nil {
		t.Error("adopted twice")
	}
	// Seeding the same env account again doesn't make a second one.
	rows, err := s.SeedEnvAccounts(ctx, DefaultOwner, []EnvAccount{{Type: "mastodon", Key: "test-mastodon", Label: "Mastodon"}})
	if err != nil || len(rows) != 0 {
		t.Errorf("seed after adoption: %v %v", rows, err)
	}
	var n int
	_ = s.sql.QueryRow(`SELECT COUNT(*) FROM events WHERE type = 'account.adopted'`).Scan(&n)
	if n != 1 {
		t.Errorf("%d adopted events", n)
	}
}

func TestSaveAccountState(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	a := newDBAccount(t, s, "threads", "123", "access_token", "old")
	err := s.SaveAccountState(ctx, DefaultOwner, a.ID, map[string]any{"token_expires_at": "2027-01-01T00:00:00Z"},
		[]SealedSecret{sealedFor(DefaultOwner, a.ID, "access_token", "REFRESHED")}, []string{"old"})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := s.Account(ctx, DefaultOwner, a.ID)
	if !b.UpdatedAt.Equal(a.UpdatedAt) || !strings.Contains(b.Config, "2027") {
		t.Errorf("after %+v", b)
	}
	got, _ := s.Secrets(ctx, DefaultOwner, SecretScopeAccount, a.ID)
	if len(got) != 1 || !strings.HasSuffix(got[0].Sealed, "REFRESHED") {
		t.Errorf("secrets %+v", got)
	}
	var n int
	_ = s.sql.QueryRow(`SELECT COUNT(*) FROM events WHERE type = 'account.updated'`).Scan(&n)
	if n != 0 {
		t.Error("an account's own save made an event")
	}
}

// Deleting an account: soft, secrets and relay lists gone, what it still
// had to send canceled with the reason; another account's target and the
// account's sent items stay.
func TestDeleteAccount(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	a := newDBAccount(t, s, "mastodon", "https://m.example/@me", "access_token")
	mustExec(t, s.sql, `INSERT INTO nostr_sync_relays (account_id, url, added_at) VALUES (?, 'wss://r', CURRENT_TIMESTAMP)`, a.ID)
	due := time.Now().Add(time.Hour)
	at := due
	p := &Post{ID: "p1", CreatedAt: time.Now().UTC(), Platforms: []string{"mastodon", "bluesky"}, Source: "test", Status: "scheduled", ScheduledAt: &at}
	p.Targets = []Target{
		{Platform: "mastodon", AccountID: a.ID, Status: "sending", Items: []Item{
			{Ordinal: 0, Text: "a", Status: ItemPublished, IdemKey: "k0"},
			{Ordinal: 1, Text: "b", Status: ItemPlanned, IdemKey: "k1", NextAttemptAt: &due}}},
		{Platform: "bluesky", Status: "sending", Items: []Item{{Ordinal: 0, Text: "c", Status: ItemPlanned, IdemKey: "k2", NextAttemptAt: &due}}},
	}
	if err := saveWithAccounts(s, p); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteAccount(ctx, DefaultOwner, a.ID, "session", ""); err != nil {
		t.Fatal(err)
	}
	got, err := s.Account(ctx, DefaultOwner, a.ID)
	if err != nil || got.DeletedAt.IsZero() || got.Enabled || got.DefaultSelected {
		t.Errorf("deleted account %+v %v", got, err)
	}
	if ss, _ := s.Secrets(ctx, DefaultOwner, SecretScopeAccount, a.ID); len(ss) != 0 {
		t.Errorf("secrets left: %v", ss)
	}
	var n int
	_ = s.sql.QueryRow(`SELECT COUNT(*) FROM nostr_sync_relays WHERE account_id = ?`, a.ID).Scan(&n)
	if n != 0 {
		t.Error("sync relays left")
	}
	all, _ := s.Accounts(ctx, DefaultOwner)
	if slices.ContainsFunc(all, func(x Account) bool { return x.ID == a.ID }) {
		t.Error("a deleted account is listed")
	}
	with, _ := s.AccountsWithDeleted(ctx, DefaultOwner)
	if !slices.ContainsFunc(with, func(x Account) bool { return x.ID == a.ID }) {
		t.Error("include=deleted misses it")
	}
	rows, err := s.sql.Query(`SELECT pt.account_id, ti.ordinal, ti.status, COALESCE(ti.error, ''), ti.next_attempt_at IS NULL
		FROM target_items ti JOIN post_targets pt ON pt.id = ti.target_id ORDER BY pt.id, ti.ordinal`)
	if err != nil {
		t.Fatal(err)
	}
	type it struct {
		acct, status, err string
		ord               int
		noNext            bool
	}
	var items []it
	for rows.Next() {
		var x it
		if err := rows.Scan(&x.acct, &x.ord, &x.status, &x.err, &x.noNext); err != nil {
			t.Fatal(err)
		}
		items = append(items, x)
	}
	rows.Close()
	want := []it{
		{acct: a.ID, ord: 0, status: ItemPublished, noNext: true},
		{acct: a.ID, ord: 1, status: ItemCanceled, err: AccountDeleteReason, noNext: true},
		{acct: "acc_bluesky", ord: 0, status: ItemPlanned},
	}
	if !slices.Equal(items, want) {
		t.Errorf("items\n got %+v\nwant %+v", items, want)
	}
	var gaveUp sql.NullTime
	var st string
	_ = s.sql.QueryRow(`SELECT status, gave_up_at FROM post_targets WHERE account_id = ?`, a.ID).Scan(&st, &gaveUp)
	if st != "partial" || !gaveUp.Valid {
		t.Errorf("target %s gave_up %v", st, gaveUp)
	}
	_ = s.sql.QueryRow(`SELECT COUNT(*) FROM events WHERE type = 'item.canceled' AND data LIKE '%account was deleted%'`).Scan(&n)
	if n != 1 {
		t.Errorf("%d item.canceled events with the reason", n)
	}
	// Twice: it is gone. Changes to it are refused.
	if err := s.DeleteAccount(ctx, DefaultOwner, a.ID, "session", ""); !errors.Is(err, ErrAccountDeleted) {
		t.Errorf("second delete: %v", err)
	}
	label := "x"
	if _, err := s.UpdateAccountMeta(ctx, DefaultOwner, a.ID, AccountMeta{Label: &label}); !errors.Is(err, ErrAccountNotFound) {
		t.Errorf("meta on a deleted account: %v", err)
	}
	// The same account can be added again.
	newDBAccount(t, s, "mastodon", "https://m.example/@me")
	// An env account can't be deleted.
	if err := s.DeleteAccount(ctx, DefaultOwner, "acc_bluesky", "session", ""); !errors.Is(err, ErrNotDBAccount) {
		t.Errorf("env account: %v", err)
	}
}

// A rotation writes every changed value or none.
func TestResealSecretsIsAllOrNothing(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	a := newDBAccount(t, s, "bluesky", "did:a", "app_password")
	newDBAccount(t, s, "mastodon", "https://m/@b", "access_token")
	ids, _ := s.SecretKeyIDs(ctx)
	if ids[testKID] != 2 || len(ids) != 1 {
		t.Fatalf("key ids %v", ids)
	}
	const newKID = "fedcba9876543210"
	_, err := s.ResealSecrets(ctx, func(x SealedSecret) (string, string, bool, error) {
		if x.Ref != a.ID {
			return "", "", false, errors.New("won't open")
		}
		return "v1." + newKID + ".NEW", newKID, true, nil
	})
	if err == nil {
		t.Fatal("a failing reseal succeeded")
	}
	if ids, _ := s.SecretKeyIDs(ctx); ids[testKID] != 2 {
		t.Fatalf("partial reseal: %v", ids)
	}
	n, err := s.ResealSecrets(ctx, func(x SealedSecret) (string, string, bool, error) {
		return "v1." + newKID + ".NEW", newKID, true, nil
	})
	if err != nil || n != 2 {
		t.Fatalf("reseal %d %v", n, err)
	}
	if ids, _ := s.SecretKeyIDs(ctx); ids[newKID] != 2 || len(ids) != 1 {
		t.Errorf("after %v", ids)
	}
}
