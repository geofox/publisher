package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// v12 on a v11 database: users are OIDC identities, existing sessions get
// a public id, and the database is recorded as born before local sign-in
// (so AUTH_MODE defaults to proxy for it).
func TestMigrationV12FromV11(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v11.db")
	db, err := openWith(t, path, migrations[:11])
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	mustExec(t, db, `INSERT INTO users (id, subject, created_at) VALUES ('u1', 'sub-1', ?)`, now)
	mustExec(t, db, `INSERT INTO sessions (id, user_id, created_at, expires_at) VALUES ('h1', 'u1', ?, ?), ('h2', 'u1', ?, ?)`,
		now, now.Add(time.Hour), now, now.Add(time.Hour))
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	if v, err := s.CreatedSchema(ctx); err != nil || v != 11 {
		t.Fatalf("created_schema = %d, %v; want 11", v, err)
	}
	u, err := s.UserByID("u1")
	if err != nil || u.Source != UserOIDC {
		t.Fatalf("user %+v, %v", u, err)
	}
	ss, err := s.ListSessions(ctx, "u1")
	if err != nil || len(ss) != 2 || ss[0].PublicID == "" || ss[0].PublicID == ss[1].PublicID || ss[0].Method != "oidc" {
		t.Fatalf("sessions %+v, %v", ss, err)
	}
}

func TestCreatedSchemaOfANewDatabase(t *testing.T) {
	s := openTestStore(t)
	if v, err := s.CreatedSchema(context.Background()); err != nil || v != LatestSchemaVersion() {
		t.Fatalf("created_schema = %d, %v; want %d", v, err, LatestSchemaVersion())
	}
}

// A database from before versioned migrations (tables, no version) is an
// existing one.
func TestCreatedSchemaOfAPreVersionedDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	db := rawDB(t, path)
	mustExec(t, db, `CREATE TABLE posts (id TEXT PRIMARY KEY)`)
	_ = db.Close()
	db = rawDB(t, path)
	// Only the birth record matters here: run v12 alone on top of a fake
	// "v11" history to see what it records for a database holding data.
	mustExec(t, db, createHistoryTable)
	ms := append([]migration{}, migrations...)
	for i := 0; i < 11; i++ {
		ms[i] = sqlMigration(i+1, ms[i].name, `SELECT 1`)
	}
	ms[11] = migration{version: 12, name: "birth_only", up: func(ctx context.Context, tx execer) error {
		if _, err := tx.ExecContext(ctx, `CREATE TABLE instance_meta (key TEXT PRIMARY KEY, value TEXT NOT NULL)`); err != nil {
			return err
		}
		born, _ := bornVersion(ctx)
		_, err := tx.ExecContext(ctx, `INSERT INTO instance_meta VALUES ('created_schema', ?)`, born)
		return err
	}}
	if err := runMigrations(context.Background(), db, path, ms[:12]); err != nil {
		t.Fatal(err)
	}
	var v int
	if err := db.QueryRow(`SELECT value FROM instance_meta WHERE key = 'created_schema'`).Scan(&v); err != nil || v != 0 {
		t.Fatalf("created_schema = %d, %v; want 0", v, err)
	}
}

func testPasskey(id string) WebAuthnCredential {
	return WebAuthnCredential{RPID: "post.example.com", CredentialID: []byte(id), PublicKey: []byte("pk-" + id), Name: "Passkey " + id}
}

func TestFirstCredentialOnlyOnce(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	if ok, err := s.HasLocalCredential(ctx); err != nil || ok {
		t.Fatalf("has = %v, %v", ok, err)
	}
	raw, _, err := s.MintSetupToken(ctx, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make([]error, 8)
	for i := range errs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if i%2 == 0 {
				_, _, errs[i] = s.CreateFirstPasskey(ctx, testPasskey(string(rune('a'+i))))
			} else {
				_, errs[i] = s.CreateFirstPassword(ctx, "$argon2id$x")
			}
		}()
	}
	wg.Wait()
	won := 0
	for _, err := range errs {
		switch {
		case err == nil:
			won++
		case !errors.Is(err, ErrSetupDone):
			t.Fatalf("unexpected error %v", err)
		}
	}
	if won != 1 {
		t.Fatalf("%d first credentials were created", won)
	}
	if n, _ := localMethodsTx(ctx, s.sql); n != 1 {
		t.Fatalf("%d local methods", n)
	}
	if err := s.ConsumeSetupToken(ctx, raw); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("setup token still usable after setup: %v", err)
	}
	u, err := s.LocalUser(ctx)
	if err != nil || u.Source != UserLocal || u.Subject != LocalSubject {
		t.Fatalf("local user %+v, %v", u, err)
	}
	// An identity provider can't sign in as the local owner.
	if _, err := s.UpsertUser(LocalSubject, "", "x"); !errors.Is(err, ErrNotOIDCUser) {
		t.Fatalf("UpsertUser(local subject) = %v", err)
	}
	if u2, _ := s.LocalUser(ctx); u2.Name != u.Name {
		t.Fatalf("the local owner was changed: %+v", u2)
	}
}

func TestSetupTokens(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	old, _, _ := s.MintSetupToken(ctx, time.Hour)
	raw, exp, err := s.MintSetupToken(ctx, time.Hour)
	if err != nil || time.Until(exp) < 59*time.Minute {
		t.Fatalf("mint: %v %v", exp, err)
	}
	if err := s.ConsumeSetupToken(ctx, old); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("an older token still works: %v", err)
	}
	if err := s.ConsumeSetupToken(ctx, raw); err != nil {
		t.Fatal(err)
	}
	if err := s.ConsumeSetupToken(ctx, raw); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("a token worked twice: %v", err)
	}
	expired, _, _ := s.MintSetupToken(ctx, -time.Second)
	if err := s.ConsumeSetupToken(ctx, expired); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("an expired token worked: %v", err)
	}
	var n int
	_ = s.sql.QueryRow(`SELECT COUNT(*) FROM setup_tokens WHERE token_hash = ?`, raw).Scan(&n)
	if n != 0 {
		t.Fatal("the raw token is stored")
	}
}

func TestPasskeysAndPassword(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	u, first, err := s.CreateFirstPasskey(ctx, testPasskey("one"))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RemoveWebAuthnCredential(ctx, u.ID, first.ID); !errors.Is(err, ErrLastSignInMethod) {
		t.Fatalf("removing the only passkey: %v", err)
	}
	second, err := s.AddWebAuthnCredential(ctx, u.ID, testPasskey("two"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddWebAuthnCredential(ctx, u.ID, testPasskey("two")); err == nil {
		t.Fatal("the same credential id was stored twice")
	}
	got, err := s.WebAuthnCredentialByCredID(ctx, []byte("two"))
	if err != nil || got.ID != second.ID || got.UserID != u.ID || string(got.PublicKey) != "pk-two" {
		t.Fatalf("by cred id %+v, %v", got, err)
	}
	if err := s.RenameWebAuthnCredential(ctx, u.ID, second.ID, "Laptop"); err != nil {
		t.Fatal(err)
	}
	if err := s.RenameWebAuthnCredential(ctx, "someone", second.ID, "x"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("rename another's: %v", err)
	}
	if err := s.RemoveWebAuthnCredential(ctx, u.ID, first.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.SetPassword(ctx, u.ID, "$argon2id$a"); err != nil {
		t.Fatal(err)
	}
	if phc, err := s.PasswordPHC(ctx, u.ID); err != nil || phc != "$argon2id$a" {
		t.Fatalf("phc %q, %v", phc, err)
	}
	if err := s.RemoveWebAuthnCredential(ctx, u.ID, second.ID); err != nil {
		t.Fatalf("a passkey with a password left: %v", err)
	}
	if err := s.RemovePassword(ctx, u.ID); !errors.Is(err, ErrLastSignInMethod) {
		t.Fatalf("removing the last password: %v", err)
	}
	if list, _ := s.WebAuthnCredentials(ctx, u.ID); len(list) != 0 {
		t.Fatalf("passkeys left: %+v", list)
	}
}

func TestSignatureCounter(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	_, c, _ := s.CreateFirstPasskey(ctx, testPasskey("c"))
	if err := s.UseWebAuthnCredential(ctx, c.ID, 0, 1); err != nil {
		t.Fatalf("an always-0 authenticator: %v", err)
	}
	if err := s.UseWebAuthnCredential(ctx, c.ID, 5, 1); err != nil {
		t.Fatal(err)
	}
	for _, n := range []uint32{5, 4, 0} {
		if err := s.UseWebAuthnCredential(ctx, c.ID, n, 1); !errors.Is(err, ErrCounterReplay) {
			t.Errorf("counter %d after 5: %v", n, err)
		}
	}
	got, _ := s.WebAuthnCredentialByCredID(ctx, []byte("c"))
	if got.SignCount != 5 || got.LastUsedAt.IsZero() {
		t.Fatalf("%+v", got)
	}
}

func TestReauthNonces(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	raw, _, err := s.CreateReauthNonce(ctx, "sess-a", "signin.add", "", 5*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.ConsumeReauthNonce(ctx, raw, "sess-a", "signin.add", ""); err != nil {
		t.Fatal(err)
	}
	if err := s.ConsumeReauthNonce(ctx, raw, "sess-a", "signin.add", ""); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("used twice: %v", err)
	}
	for _, tc := range []struct{ sess, op, target string }{
		{"sess-b", "signin.add", ""}, {"sess-a", "password.set", ""}, {"sess-a", "signin.add", "acc_1"},
	} {
		raw, _, _ := s.CreateReauthNonce(ctx, "sess-a", "signin.add", "", 5*time.Minute)
		if err := s.ConsumeReauthNonce(ctx, raw, tc.sess, tc.op, tc.target); !errors.Is(err, ErrInvalidToken) {
			t.Errorf("%+v accepted: %v", tc, err)
		}
		// Spent by the wrong use.
		if err := s.ConsumeReauthNonce(ctx, raw, "sess-a", "signin.add", ""); !errors.Is(err, ErrInvalidToken) {
			t.Errorf("%+v: the nonce survived a wrong use", tc)
		}
	}
	old, _, _ := s.CreateReauthNonce(ctx, "sess-a", "signin.add", "", -time.Second)
	if err := s.ConsumeReauthNonce(ctx, old, "sess-a", "signin.add", ""); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("expired nonce: %v", err)
	}
}

func TestSessionsAndTheirNonces(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	u, _, _ := s.CreateFirstPasskey(ctx, testPasskey("s"))
	a, _ := s.CreateSessionWith(u.ID, time.Hour, SessionMeta{Method: "passkey", UserAgent: "Firefox", Addr: "192.0.2.1"})
	b, _ := s.CreateSessionWith(u.ID, time.Hour, SessionMeta{Method: "password"})
	nb, _, _ := s.CreateReauthNonce(ctx, SessionHash(b), "signin.add", "", time.Minute)
	sa, ua, err := s.SessionByRaw(a)
	if err != nil || ua.ID != u.ID || sa.Method != "passkey" || sa.UserAgent != "Firefox" || sa.Addr != "192.0.2.1" || sa.PublicID == "" {
		t.Fatalf("%+v %+v %v", sa, ua, err)
	}
	if list, _ := s.ListSessions(ctx, u.ID); len(list) != 2 {
		t.Fatalf("%d sessions", len(list))
	}
	if err := s.DeleteSessionByPublicID(ctx, "other", sa.PublicID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("another user's session: %v", err)
	}
	if err := s.DeleteOtherSessions(ctx, u.ID, SessionHash(a)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SessionUser(b); err == nil {
		t.Fatal("the other session lives")
	}
	if err := s.ConsumeReauthNonce(ctx, nb, SessionHash(b), "signin.add", ""); !errors.Is(err, ErrInvalidToken) {
		t.Fatal("the ended session's nonce lives")
	}
	if err := s.DeleteSessionByPublicID(ctx, u.ID, sa.PublicID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SessionUser(a); err == nil {
		t.Fatal("the session lives")
	}
}

func TestLoginFailures(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	_ = s.RecordLoginFailure(ctx, "a", now.Add(-25*time.Hour))
	_ = s.RecordLoginFailure(ctx, "a", now.Add(-time.Minute))
	_ = s.RecordLoginFailure(ctx, "b", now)
	if got, _ := s.LoginFailures(ctx, "a", now.Add(-time.Hour)); len(got) != 1 {
		t.Fatalf("a: %v", got)
	}
	if got, _ := s.LoginFailures(ctx, "", now.Add(-48*time.Hour)); len(got) != 3 {
		t.Fatalf("all: %v", got)
	}
	if err := s.PruneLoginFailures(ctx, now); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.LoginFailures(ctx, "", now.Add(-48*time.Hour)); len(got) != 2 {
		t.Fatalf("after prune: %v", got)
	}
	_ = s.ClearLoginFailures(ctx, "a")
	if got, _ := s.LoginFailures(ctx, "", now.Add(-48*time.Hour)); len(got) != 1 {
		t.Fatalf("after clear: %v", got)
	}
}

func TestResetLocalLogin(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	u, _, _ := s.CreateFirstPasskey(ctx, testPasskey("r"))
	_ = s.SetPassword(ctx, u.ID, "$x")
	raw, _ := s.CreateSessionWith(u.ID, time.Hour, SessionMeta{Method: "passkey"})
	oidc, _ := s.UpsertUser("sub-1", "", "")
	other, _ := s.CreateSession(oidc.ID, time.Hour)
	if err := s.ResetLocalLogin(ctx); err != nil {
		t.Fatal(err)
	}
	if ok, _ := s.HasLocalCredential(ctx); ok {
		t.Fatal("credentials left")
	}
	if _, err := s.SessionUser(raw); err == nil {
		t.Fatal("the local session lives")
	}
	if _, err := s.SessionUser(other); err != nil {
		t.Fatal("an OIDC session was ended")
	}
}
