package store

import (
	"bytes"
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

// An account's own save merges its configuration key by key, upserts and
// removes secrets, makes no event and keeps updated_at; an account from the
// environment keeps its saved state the same way.
func TestSaveAccountState(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	a := newDBAccount(t, s, "threads", "123", "access_token", "old")
	err := s.SaveAccountState(ctx, DefaultOwner, a.ID, map[string]any{"token_expires_at": "2027-01-01T00:00:00Z", "instance_url": nil},
		[]SealedSecret{sealedFor(DefaultOwner, a.ID, "access_token", "REFRESHED")}, []string{"old"})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := s.Account(ctx, DefaultOwner, a.ID)
	if !b.UpdatedAt.Equal(a.UpdatedAt) || b.Config != `{"token_expires_at":"2027-01-01T00:00:00Z"}` {
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
	env, err := s.SeedEnvAccounts(ctx, DefaultOwner, []EnvAccount{{Type: "threads", Label: "Threads"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SaveAccountState(ctx, DefaultOwner, env[0].ID, map[string]any{"state_seed": "x"},
		[]SealedSecret{sealedFor(DefaultOwner, env[0].ID, "access_token", "T")}, nil); err != nil {
		t.Fatalf("env account: %v", err)
	}
	if got, _ := s.Secrets(ctx, DefaultOwner, SecretScopeAccount, env[0].ID); len(got) != 1 {
		t.Errorf("env secrets %+v", got)
	}
	// A secret bound to another account is refused.
	if err := s.SaveAccountState(ctx, DefaultOwner, a.ID, nil, []SealedSecret{sealedFor(DefaultOwner, env[0].ID, "x", "y")}, nil); err == nil {
		t.Error("a secret of another account was saved")
	}
}

// The 2.x Threads token moves into the account's sealed state in one step,
// and after the checkpoint neither the database nor its WAL holds it.
func TestMoveThreadsToken(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "p.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	const plain = "THAAplaintexttoken0123456789"
	if err := s.SaveThreadsToken(plain, time.Now().Add(time.Hour), "seed", time.Now()); err != nil {
		t.Fatal(err)
	}
	env, _ := s.SeedEnvAccounts(ctx, DefaultOwner, []EnvAccount{{Type: "threads", Label: "Threads"}})
	// A secret bound to another account is refused, and the token stays.
	if err := s.MoveThreadsToken(ctx, DefaultOwner, env[0].ID, nil, []SealedSecret{sealedFor(DefaultOwner, "acc_other", "access_token", "S")}); err == nil {
		t.Error("a secret of another account was saved")
	}
	if tok, _ := s.GetThreadsToken(); tok == nil {
		t.Error("a refused move removed the token")
	}
	sealed := sealedFor(DefaultOwner, env[0].ID, "access_token", "SEALED")
	if err := s.MoveThreadsToken(ctx, DefaultOwner, env[0].ID, map[string]any{"expires_at": "2027-01-01T00:00:00Z"}, []SealedSecret{sealed}); err != nil {
		t.Fatal(err)
	}
	if tok, err := s.GetThreadsToken(); err != nil || tok != nil {
		t.Errorf("the old row is still there: %v %v", tok, err)
	}
	if got, _ := s.Secrets(ctx, DefaultOwner, SecretScopeAccount, env[0].ID); len(got) != 1 {
		t.Errorf("secrets %+v", got)
	}
	if err := s.Checkpoint(ctx); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{path, path + "-wal"} {
		b, err := os.ReadFile(f)
		if err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		if bytes.Contains(b, []byte(plain)) {
			t.Errorf("%s still holds the token", filepath.Base(f))
		}
	}
	// With no account to give it to, the old row is only removed.
	_ = s.SaveThreadsToken(plain, time.Now(), "seed", time.Now())
	if err := s.MoveThreadsToken(ctx, DefaultOwner, "", nil, nil); err != nil {
		t.Fatal(err)
	}
	if tok, _ := s.GetThreadsToken(); tok != nil {
		t.Error("not removed")
	}
}

// A checkpoint waits for readers to let go of the log, and says so when
// they don't before its context ends.
func TestCheckpointWaitsForReaders(t *testing.T) {
	path := filepath.Join(t.TempDir(), "p.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.sql.SetMaxOpenConns(1) // the checkpoint's connection is the one used after it
	ctx := context.Background()
	if _, err := s.SeedEnvAccounts(ctx, DefaultOwner, []EnvAccount{{Type: "threads", Label: "Threads"}}); err != nil {
		t.Fatal(err)
	}
	reader, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	conn, err := reader.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	// A read transaction holds a snapshot of the log.
	if _, err := conn.ExecContext(ctx, "BEGIN"); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := conn.QueryRowContext(ctx, "SELECT count(*) FROM accounts").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SeedEnvAccounts(ctx, DefaultOwner, []EnvAccount{{Type: "mastodon", Label: "Mastodon"}}); err != nil {
		t.Fatal(err)
	}
	short, cancel := context.WithTimeout(ctx, 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	if err := s.Checkpoint(short); err == nil || !strings.Contains(err.Error(), "busy") {
		t.Errorf("checkpoint with a reader: %v", err)
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Errorf("the checkpoint held %v past its context", d)
	}
	// The pool's connections wait out locks as before.
	var timeout int
	if err := s.sql.QueryRowContext(ctx, "PRAGMA busy_timeout").Scan(&timeout); err != nil || timeout != busyTimeoutMS {
		t.Errorf("busy_timeout = %d %v", timeout, err)
	}
	go func() {
		time.Sleep(200 * time.Millisecond)
		_, _ = conn.ExecContext(ctx, "COMMIT")
	}()
	long, cancel2 := context.WithTimeout(ctx, 10*time.Second)
	defer cancel2()
	if err := s.Checkpoint(long); err != nil {
		t.Errorf("checkpoint once the reader is done: %v", err)
	}
	if fi, err := os.Stat(path + "-wal"); err == nil && fi.Size() != 0 {
		t.Errorf("the log holds %d bytes", fi.Size())
	}
}

// Two live accounts that are the same network account: the second keeps
// its handle, no remote id, and the error names the first.
func TestSetAccountIdentityDuplicate(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	a := newDBAccount(t, s, "mastodon", "https://m.example/@me", "access_token")
	env, _ := s.SeedEnvAccounts(ctx, DefaultOwner, []EnvAccount{{Type: "mastodon", Label: "Mastodon"}})
	err := s.SetAccountIdentity(ctx, env[0].ID, "@me@m.example", "https://m.example/@me")
	var dup *DuplicateAccountError
	if !errors.As(err, &dup) || dup.ID != a.ID {
		t.Fatalf("err = %v", err)
	}
	got, _ := s.Account(ctx, DefaultOwner, env[0].ID)
	if got.Handle != "@me@m.example" || got.RemoteID != "" {
		t.Errorf("row %+v", got)
	}
	if err := s.SetAccountIdentity(ctx, env[0].ID, "@me@m.example", "https://m.example/@other"); err != nil {
		t.Errorf("a different remote id: %v", err)
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
