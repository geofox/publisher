package accounts

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/geofox/publisher/internal/destination"
	"github.com/geofox/publisher/internal/drivers"
	"github.com/geofox/publisher/internal/secrets"
	"github.com/geofox/publisher/internal/store"
	"github.com/geofox/publisher/internal/threads"
)

func testKey(t *testing.T, b byte) *secrets.Key {
	t.Helper()
	k, err := secrets.NewKey(bytes.Repeat([]byte{b}, 32))
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func openStore(t *testing.T) (*store.Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "p.db")
	st, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st, path
}

// sealFor seals credentials for an account as the account API does.
func sealFor(t *testing.T, kr secrets.Keyring, owner, id, audience string, plain map[string]string) []store.SealedSecret {
	t.Helper()
	put, _, err := sealUpdate(kr, KeyState{Status: KeyOK}, owner, id, audience, plain)
	if err != nil {
		t.Fatal(err)
	}
	return put
}

// addDB stores an account added in the app, sealed with kr.
func addDB(t *testing.T, st *store.Store, kr secrets.Keyring, typ string, config map[string]any, plain map[string]string) store.Account {
	t.Helper()
	drivers.Register()
	d, _ := destination.Lookup(typ)
	id := store.NewAccountID()
	a, err := st.CreateAccountWithSecrets(context.Background(), store.NewAccount{ID: id, OwnerID: store.DefaultOwner, Type: typ,
		Label: "Work", AuthMethod: "token", Config: config, Actor: "test",
		Secrets: sealFor(t, kr, store.DefaultOwner, id, destination.AudienceOf(d.Manifest(), config), plain)})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// opened records what each account was opened with.
type opened struct {
	mu   sync.Mutex
	reqs map[string]destination.OpenRequest
}

func (o *opened) opener(_ context.Context, typ string, req destination.OpenRequest) (destination.Account, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.reqs == nil {
		o.reqs = map[string]destination.OpenRequest{}
	}
	o.reqs[req.AccountID] = req
	return &fakeAcct{id: req.AccountID, typ: typ, authOK: &yes}, nil
}

func (o *opened) req(id string) destination.OpenRequest {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.reqs[id]
}

func loadRegistry(t *testing.T, st *store.Store, kr secrets.Keyring, o *opened, state StateStore, env ...EnvAccount) *Registry {
	t.Helper()
	ks, err := PrepareKeys(context.Background(), st, kr)
	if err != nil {
		t.Fatal(err)
	}
	r := New(Options{Store: st, Secrets: st, Keys: kr, KeyState: ks, Open: o.opener, State: state, Env: env})
	if err := r.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(r.Close)
	return r
}

// The key's status follows what is stored; a rotation moves everything
// under the previous key to the current one, or changes nothing.
func TestPrepareKeys(t *testing.T) {
	ctx := context.Background()
	a, b, c := testKey(t, 1), testKey(t, 2), testKey(t, 3)
	st, _ := openStore(t)
	if ks, _ := PrepareKeys(ctx, st, secrets.Keyring{}); ks.Status != KeyNone || ks.WriteError() != ErrMasterKeyRequired {
		t.Errorf("empty, no key: %+v", ks)
	}
	if ks, _ := PrepareKeys(ctx, st, secrets.Keyring{Current: a}); ks.Status != KeyOK || ks.WriteError() != nil {
		t.Errorf("empty, a key: %+v", ks)
	}
	acct := addDB(t, st, secrets.Keyring{Current: a}, "mastodon", map[string]any{"instance_url": "https://m.example"}, map[string]string{"access_token": "tok"})
	for name, c := range map[string]struct {
		kr   secrets.Keyring
		want KeyStatus
		err  error
	}{
		"no key":    {secrets.Keyring{}, KeyMissing, ErrMasterKeyRequired},
		"other key": {secrets.Keyring{Current: b}, KeyMismatch, ErrMasterKeyMismatch},
		"same key":  {secrets.Keyring{Current: a}, KeyOK, nil},
	} {
		ks, err := PrepareKeys(ctx, st, c.kr)
		if err != nil || ks.Status != c.want || ks.WriteError() != c.err {
			t.Errorf("%s: %+v %v", name, ks, err)
		}
		if strings.Contains(ks.Detail, "tok") {
			t.Errorf("%s: detail %q", name, ks.Detail)
		}
	}
	// A rotation that can't open a value (under neither key) changes
	// nothing, and says so.
	ks, err := PrepareKeys(ctx, st, secrets.Keyring{Current: b, Previous: c})
	if err != nil || ks.Status != KeyMismatch || !strings.Contains(ks.Detail, "nothing was changed") {
		t.Errorf("failed rotation: %+v %v", ks, err)
	}
	if ids, _ := st.SecretKeyIDs(ctx); ids[a.ID()] != 1 {
		t.Errorf("after a failed rotation: %v", ids)
	}
	// A rotation from a to b.
	ks, err = PrepareKeys(ctx, st, secrets.Keyring{Current: b, Previous: a})
	if err != nil || ks.Status != KeyOK || ks.Rotated != 1 {
		t.Errorf("rotation: %+v %v", ks, err)
	}
	o := &opened{}
	r := loadRegistry(t, st, secrets.Keyring{Current: b}, o, nil)
	if e, _ := r.Get(acct.ID); e.Status != Connected || o.req(acct.ID).Secrets["access_token"] != "tok" {
		t.Errorf("after rotation: %+v %v", e, o.req(acct.ID).Secrets)
	}
}

// An account added in the app opens with its credentials; without the key
// that sealed them it is locked, saying why, and opens once the key is set
// again. Its configuration names the server its credentials are bound to:
// an address edited in the database locks it.
func TestDBAccountOpens(t *testing.T) {
	a, b := testKey(t, 1), testKey(t, 2)
	st, path := openStore(t)
	acct := addDB(t, st, secrets.Keyring{Current: a}, "mastodon", map[string]any{"instance_url": "https://m.example"},
		map[string]string{"access_token": "tok"})
	for _, c := range []struct {
		kr     secrets.Keyring
		status Status
		reason string
	}{
		{secrets.Keyring{}, Locked, "PUBLISHER_MASTER_KEY"},
		{secrets.Keyring{Current: b}, Locked, "another master key"},
		{secrets.Keyring{Current: a}, Connected, ""},
	} {
		o := &opened{}
		r := loadRegistry(t, st, c.kr, o, nil)
		e, _ := r.Get(acct.ID)
		if e.Status != c.status || !strings.Contains(e.Reason, c.reason) {
			t.Errorf("key %v: %s %q", c.kr.Current != nil, e.Status, e.Reason)
		}
		if _, _, err := r.Acquire(acct.ID); (err == nil) != (c.status == Connected) {
			t.Errorf("acquire: %v", err)
		}
		if c.status == Connected {
			req := o.req(acct.ID)
			if req.Secrets["access_token"] != "tok" || req.Config["instance_url"] != "https://m.example" || req.Method != "token" {
				t.Errorf("opened with %+v", req)
			}
		}
	}
	// The address edited in the database file: the credentials don't open.
	edit(t, path, `UPDATE accounts SET config_json = '{"instance_url":"https://evil.example"}' WHERE id = ?`, acct.ID)
	r := loadRegistry(t, st, secrets.Keyring{Current: a}, &opened{}, nil)
	if e, _ := r.Get(acct.ID); e.Status != Locked || !strings.Contains(e.Reason, "connect it again") {
		t.Errorf("edited address: %s %q", e.Status, e.Reason)
	}
}

// edit changes the database behind the store's back, as someone with the
// file could.
func edit(t *testing.T, path, q string, args ...any) {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(q, args...); err != nil {
		t.Fatal(err)
	}
}

// What an account added in the app saves (a refreshed token) is sealed
// for its server and opens at the next start; it can't move its
// credentials to another server, and nothing is saved under a key that
// doesn't match.
func TestDBAccountSaves(t *testing.T) {
	ctx := context.Background()
	a := testKey(t, 1)
	st, _ := openStore(t)
	acct := addDB(t, st, secrets.Keyring{Current: a}, "mastodon", map[string]any{"instance_url": "https://m.example"},
		map[string]string{"access_token": "tok"})
	o := &opened{}
	r := loadRegistry(t, st, secrets.Keyring{Current: a}, o, nil)
	host := o.req(acct.ID).Host
	if err := host.Save(ctx, destination.StateUpdate{Secrets: map[string]string{"access_token": "tok2"}, Config: map[string]any{"expires_at": "x"}}); err != nil {
		t.Fatal(err)
	}
	if err := host.Save(ctx, destination.StateUpdate{Config: map[string]any{"instance_url": "https://evil.example"}}); !errors.Is(err, errMoveAudience) {
		t.Errorf("moving the audience: %v", err)
	}
	if err := r.Reopen(ctx, acct.ID); err != nil {
		t.Fatal(err)
	}
	o2 := &opened{}
	loadRegistry(t, st, secrets.Keyring{Current: a}, o2, nil)
	if req := o2.req(acct.ID); req.Secrets["access_token"] != "tok2" || req.Config["expires_at"] != "x" || req.Config["instance_url"] != "https://m.example" {
		t.Errorf("reopened with %+v %+v", req.Secrets, req.Config)
	}
	// Under a key that doesn't match the stored values, nothing is sealed.
	b := testKey(t, 2)
	r3 := New(Options{Store: st, Secrets: st, Keys: secrets.Keyring{Current: b, Previous: a}, KeyState: KeyState{Status: KeyMismatch}, Open: o.opener})
	if err := r3.Load(ctx); err != nil {
		t.Fatal(err)
	}
	defer r3.Close()
	if err := o.req(acct.ID).Host.Save(ctx, destination.StateUpdate{Secrets: map[string]string{"access_token": "tok3"}}); !errors.Is(err, ErrMasterKeyMismatch) {
		t.Errorf("save under a mismatched key: %v", err)
	}
}

// An account from the environment keeps what it saves sealed beside it,
// while the environment's credentials are the ones it came from; nothing
// is written until it saves.
func TestSealedStateForEnvAccounts(t *testing.T) {
	ctx := context.Background()
	drivers.Register()
	a := testKey(t, 1)
	st, _ := openStore(t)
	rows, err := st.SeedEnvAccounts(ctx, store.DefaultOwner, []store.EnvAccount{{Type: "threads", Key: "default", Label: "Threads"}})
	if err != nil {
		t.Fatal(err)
	}
	id := rows[0].ID
	envAcct := func(tok string) EnvAccount {
		return EnvAccount{Type: "threads", EnvAccount: destination.EnvAccount{Key: "default", Config: map[string]any{"user_id": "me"},
			Secrets: map[string]string{"access_token": tok}}}
	}
	kr := secrets.Keyring{Current: a}
	state := func() *SealedState { return &SealedState{Store: st, Keys: kr, KeyState: KeyState{Status: KeyOK}} }
	o := &opened{}
	loadRegistry(t, st, kr, o, state(), envAcct("envtok"))
	if got, _ := st.Secrets(ctx, store.DefaultOwner, store.SecretScopeAccount, id); len(got) != 0 {
		t.Errorf("opening wrote %d secrets", len(got))
	}
	if err := o.req(id).Host.Save(ctx, destination.StateUpdate{Secrets: map[string]string{"access_token": "refreshed"},
		Config: map[string]any{"expires_at": "2027-01-01T00:00:00Z"}}); err != nil {
		t.Fatal(err)
	}
	// The next start opens with the saved token.
	o = &opened{}
	loadRegistry(t, st, kr, o, state(), envAcct("envtok"))
	if req := o.req(id); req.Secrets["access_token"] != "refreshed" || req.Config["expires_at"] != "2027-01-01T00:00:00Z" || req.Config["user_id"] != "me" {
		t.Errorf("restored %+v %+v", req.Secrets, req.Config)
	}
	if _, ok := o.req(id).Secrets[StateSeedSecret]; ok {
		t.Error("the seed reached the account")
	}
	// Under another key, the account stays shut rather than use the
	// environment's (older) token.
	o = &opened{}
	b := testKey(t, 2)
	r := loadRegistry(t, st, secrets.Keyring{Current: b}, o, &SealedState{Store: st, Keys: secrets.Keyring{Current: b}, KeyState: KeyState{Status: KeyMismatch}}, envAcct("envtok"))
	if e, _ := r.Get(id); e.Status != Locked {
		t.Errorf("under another key: %s %q", e.Status, e.Reason)
	}
	if got, _ := st.Secrets(ctx, store.DefaultOwner, store.SecretScopeAccount, id); len(got) != 2 {
		t.Errorf("under another key, %d secrets are left", len(got))
	}
	// The operator sets a new token: what was saved for the old one is
	// forgotten.
	o = &opened{}
	loadRegistry(t, st, kr, o, state(), envAcct("newtok"))
	if req := o.req(id); req.Secrets["access_token"] != "newtok" || req.Config["expires_at"] != nil {
		t.Errorf("after a new token: %+v %+v", req.Secrets, req.Config)
	}
	if got, _ := st.Secrets(ctx, store.DefaultOwner, store.SecretScopeAccount, id); len(got) != 0 {
		t.Errorf("%d secrets kept for the old token", len(got))
	}
}

// 2.x's plaintext Threads token moves into the sealed state of the account
// it belongs to, the old row goes, and neither the database nor its WAL
// holds it afterwards; the account then opens with it.
func TestMoveThreadsToken(t *testing.T) {
	ctx := context.Background()
	drivers.Register()
	a := testKey(t, 1)
	st, path := openStore(t)
	rows, _ := st.SeedEnvAccounts(ctx, store.DefaultOwner, []store.EnvAccount{{Type: "threads", Key: "default", Label: "Threads"}})
	const envTok, live = "THAAenvironmenttoken", "THAArefreshedlivetoken0123456789"
	exp := time.Now().Add(40 * 24 * time.Hour).UTC().Truncate(time.Second)
	if err := st.SaveThreadsToken(live, exp, threads.SeedHash(envTok), time.Now()); err != nil {
		t.Fatal(err)
	}
	env := []drivers.EnvAccount{{Type: "threads", EnvAccount: destination.EnvAccount{Key: "default",
		Config: map[string]any{"user_id": "me"}, Secrets: map[string]string{"access_token": envTok}}}}
	kr := secrets.Keyring{Current: a}
	sealed := &SealedState{Store: st, Keys: kr, KeyState: KeyState{Status: KeyOK}}
	to, done, err := drivers.MoveThreadsToken(ctx, st, sealed.Prepare, rows, env)
	if err != nil || !done || to != rows[0].ID {
		t.Fatalf("move: %q %v %v", to, done, err)
	}
	if tok, _ := st.GetThreadsToken(); tok != nil {
		t.Error("the old row is still there")
	}
	for _, f := range []string{path, path + "-wal"} {
		b, err := os.ReadFile(f)
		if err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		if bytes.Contains(b, []byte(live)) {
			t.Errorf("%s still holds the token", filepath.Base(f))
		}
	}
	o := &opened{}
	loadRegistry(t, st, kr, o, sealed, EnvAccount{Type: "threads", EnvAccount: env[0].EnvAccount})
	req := o.req(rows[0].ID)
	if req.Secrets["access_token"] != live || req.Config["expires_at"] != exp.Format(time.RFC3339Nano) {
		t.Errorf("opened with %+v %+v", req.Secrets, req.Config)
	}
	// A token from another environment token belongs to no account: it is
	// only removed.
	_ = st.SaveThreadsToken(live, exp, threads.SeedHash("someone else's"), time.Now())
	to, done, err = drivers.MoveThreadsToken(ctx, st, sealed.Prepare, rows, env)
	if err != nil || !done || to != "" {
		t.Errorf("stale row: %q %v %v", to, done, err)
	}
	if tok, _ := st.GetThreadsToken(); tok != nil {
		t.Error("the stale row is still there")
	}
}

// Saved state is forgotten, and the environment's credentials used, when
// any of it fails to open (a value moved in the database), and when the
// operator points the account at another server with the same token. Under
// a key that doesn't match the database, nothing is deleted.
func TestSealedStateForgets(t *testing.T) {
	ctx := context.Background()
	drivers.Register()
	kr := secrets.Keyring{Current: testKey(t, 1)}
	mastodon := func(server, tok string) EnvAccount {
		return EnvAccount{Type: "mastodon", EnvAccount: destination.EnvAccount{Key: "default",
			Config: map[string]any{"instance_url": server}, Secrets: map[string]string{"access_token": tok}}}
	}
	setup := func(t *testing.T) (*store.Store, string, string) {
		st, path := openStore(t)
		rows, err := st.SeedEnvAccounts(ctx, store.DefaultOwner, []store.EnvAccount{{Type: "mastodon", Key: "default", Label: "M"}})
		if err != nil {
			t.Fatal(err)
		}
		o := &opened{}
		loadRegistry(t, st, kr, o, &SealedState{Store: st, Keys: kr, KeyState: KeyState{Status: KeyOK}}, mastodon("https://m.example", "envtok"))
		if err := o.req(rows[0].ID).Host.Save(ctx, destination.StateUpdate{Secrets: map[string]string{"access_token": "saved"},
			Config: map[string]any{"note": "x"}}); err != nil {
			t.Fatal(err)
		}
		return st, path, rows[0].ID
	}
	restored := func(t *testing.T, st *store.Store, ks KeyState, env EnvAccount, id string) destination.OpenRequest {
		t.Helper()
		o := &opened{}
		r := New(Options{Store: st, Secrets: st, Keys: kr, KeyState: ks, Open: o.opener,
			State: &SealedState{Store: st, Keys: kr, KeyState: ks}, Env: []EnvAccount{env}})
		if err := r.Load(ctx); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(r.Close)
		return o.req(id)
	}
	ok := KeyState{Status: KeyOK}

	t.Run("kept", func(t *testing.T) {
		st, _, id := setup(t)
		if req := restored(t, st, ok, mastodon("https://m.example", "envtok"), id); req.Secrets["access_token"] != "saved" || req.Config["note"] != "x" {
			t.Errorf("restored %+v %+v", req.Secrets, req.Config)
		}
	})
	t.Run("a value moved", func(t *testing.T) {
		st, path, id := setup(t)
		edit(t, path, `UPDATE secrets SET sealed = (SELECT sealed FROM secrets WHERE name = '_state_seed' AND ref = ?) WHERE name = 'access_token' AND ref = ?`, id, id)
		if req := restored(t, st, ok, mastodon("https://m.example", "envtok"), id); req.Secrets["access_token"] != "envtok" || req.Config["note"] != nil {
			t.Errorf("restored %+v %+v", req.Secrets, req.Config)
		}
		if got, _ := st.Secrets(ctx, store.DefaultOwner, store.SecretScopeAccount, id); len(got) != 0 {
			t.Errorf("%d secrets kept", len(got))
		}
	})
	t.Run("another server", func(t *testing.T) {
		st, _, id := setup(t)
		if req := restored(t, st, ok, mastodon("https://other.example", "envtok"), id); req.Secrets["access_token"] != "envtok" || req.Config["note"] != nil {
			t.Errorf("restored %+v %+v", req.Secrets, req.Config)
		}
	})
	t.Run("a key that doesn't match", func(t *testing.T) {
		st, _, id := setup(t)
		mismatch := KeyState{Status: KeyMismatch}
		if req := restored(t, st, mismatch, mastodon("https://m.example", "newtok"), id); req.Secrets["access_token"] != "newtok" || req.Config["note"] != nil {
			t.Errorf("restored %+v %+v", req.Secrets, req.Config)
		}
		if got, _ := st.Secrets(ctx, store.DefaultOwner, store.SecretScopeAccount, id); len(got) != 2 {
			t.Errorf("%d secrets left, want both kept", len(got))
		}
	})
}

// The seed names the credentials and the server, whatever order the
// environment lists them in.
func TestEnvStateSeed(t *testing.T) {
	a := EnvStateSeed(map[string]string{"a": "1", "b": "2"}, "https://m.example")
	if a != EnvStateSeed(map[string]string{"b": "2", "a": "1"}, "https://m.example") {
		t.Error("order changed the seed")
	}
	for name, other := range map[string]string{
		"server": EnvStateSeed(map[string]string{"a": "1", "b": "2"}, "https://other.example"),
		"value":  EnvStateSeed(map[string]string{"a": "1", "b": "3"}, "https://m.example"),
		"split":  EnvStateSeed(map[string]string{"a": "1\x00b\x002"}, "https://m.example"),
	} {
		if other == a {
			t.Errorf("another %s, the same seed", name)
		}
	}
}
