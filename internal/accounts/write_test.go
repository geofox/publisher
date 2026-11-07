package accounts

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/geofox/publisher/internal/destination"
	"github.com/geofox/publisher/internal/drivers"
	"github.com/geofox/publisher/internal/secrets"
	"github.com/geofox/publisher/internal/store"
)

// credAcct is an account a trial opens: who it is follows from its
// credential, the way a network's answer would.
//
//   - "bad…": refused (401), the network echoing what was sent;
//   - "down…": unreachable;
//   - "blocked…": its server is on a private address;
//   - "same…": always the same network account;
//   - "…save…": saves state while opening;
//   - anything else: the network account "remote-<value>".
type credAcct struct {
	fakeAcct
	secret string
}

func (a *credAcct) Identity(context.Context) (destination.Identity, error) {
	switch {
	case strings.HasPrefix(a.secret, "bad"):
		return destination.Identity{}, &destination.HTTPError{Op: "createSession", StatusCode: http.StatusUnauthorized,
			Body: `{"error":"AuthenticationRequired","message":"Invalid identifier or password ` + a.secret + `"}`}
	case strings.HasPrefix(a.secret, "down"):
		return destination.Identity{}, &destination.HTTPError{Op: "createSession", StatusCode: http.StatusServiceUnavailable, Body: "busy"}
	case strings.HasPrefix(a.secret, "blocked"):
		return destination.Identity{}, fmt.Errorf("dial: %w", destination.ErrBlockedAddress)
	case strings.HasPrefix(a.secret, "same"):
		return destination.Identity{RemoteID: "remote-same", Handle: "same.example"}, nil
	}
	return destination.Identity{RemoteID: "remote-" + a.secret, Handle: a.secret + ".example"}, nil
}

// credOpener opens credAccts, recording each request.
type credOpener struct {
	mu   sync.Mutex
	reqs []destination.OpenRequest
}

func (o *credOpener) open(ctx context.Context, typ string, req destination.OpenRequest) (destination.Account, error) {
	o.mu.Lock()
	o.reqs = append(o.reqs, req)
	o.mu.Unlock()
	var secret string
	for _, k := range slices.Sorted(maps.Keys(req.Secrets)) {
		if !strings.HasPrefix(k, "_") {
			secret = req.Secrets[k]
			break
		}
	}
	if strings.Contains(secret, "save") {
		if err := req.Host.Save(ctx, destination.StateUpdate{Config: map[string]any{"expires_at": "2027-01-01T00:00:00Z"}}); err != nil {
			return nil, err
		}
	}
	return &credAcct{fakeAcct: fakeAcct{id: req.AccountID, typ: typ, authOK: &yes}, secret: secret}, nil
}

func (o *credOpener) last() destination.OpenRequest {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.reqs[len(o.reqs)-1]
}

func (o *credOpener) count() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return len(o.reqs)
}

type writeEnv struct {
	t    *testing.T
	st   *store.Store
	path string
	kr   secrets.Keyring
	o    *credOpener
	r    *Registry
	// seeded are the accounts Seed was called for.
	seeded []string
}

// newWriteEnv is a registry that can add accounts, over a fresh store,
// with a master key (unless noKey) and the environment's accounts env.
func newWriteEnv(t *testing.T, noKey bool, env ...EnvAccount) *writeEnv {
	t.Helper()
	drivers.Register()
	st, path := openStore(t)
	w := &writeEnv{t: t, st: st, path: path, o: &credOpener{}}
	if !noKey {
		w.kr = secrets.Keyring{Current: testKey(t, 7)}
	}
	var seed []store.EnvAccount
	for _, e := range env {
		seed = append(seed, store.EnvAccount{Type: e.Type, Key: e.Key, Label: "Env"})
	}
	if _, err := st.SeedEnvAccounts(context.Background(), store.DefaultOwner, seed); err != nil {
		t.Fatal(err)
	}
	ks, err := PrepareKeys(context.Background(), st, w.kr)
	if err != nil {
		t.Fatal(err)
	}
	w.r = New(Options{Store: st, Secrets: st, Writes: st, Flows: st, Keys: w.kr, KeyState: ks, Open: w.o.open, Env: env,
		Getenv: func(string) string { return "" },
		Seed: func(_ context.Context, row store.Account) error {
			w.seeded = append(w.seeded, row.ID)
			return nil
		}})
	if err := w.r.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(w.r.Close)
	return w
}

func bsky(password string) NewAccount {
	return NewAccount{Owner: store.DefaultOwner, Type: "bluesky", Method: "app_password", Actor: "session",
		Config:      map[string]any{"pds_url": "https://pds.example/"},
		Credentials: map[string]any{"identifier": "@Ada.Example", "app_password": password}}
}

// secretsOf opens an account's stored credentials for audience.
func (w *writeEnv) secretsOf(id, audience string) map[string]string {
	w.t.Helper()
	sealed, err := w.st.Secrets(context.Background(), store.DefaultOwner, store.SecretScopeAccount, id)
	if err != nil {
		w.t.Fatal(err)
	}
	out := map[string]string{}
	for _, s := range sealed {
		b := Binding(s)
		b.Audience = audience
		v, err := w.kr.Open(b, s.Sealed)
		if err != nil {
			w.t.Fatalf("%s doesn't open for %q: %v", s.Name, audience, err)
		}
		out[s.Name] = v
	}
	return out
}

func (w *writeEnv) count() int {
	w.t.Helper()
	all, err := w.st.AccountsWithDeleted(context.Background(), store.DefaultOwner)
	if err != nil {
		w.t.Fatal(err)
	}
	return len(all)
}

// An account added in the app is opened with what was typed, asked who
// it is, and stored with its credentials sealed for its server; it opens
// at once, with its network's identity.
func TestCreate(t *testing.T) {
	w := newWriteEnv(t, false)
	ctx := context.Background()
	en, err := w.r.Create(ctx, bsky("abcd-efgh-ijkl-mnop"))
	if err != nil {
		t.Fatal(err)
	}
	row := en.Row
	if row.Source != store.SourceDB || row.AuthMethod != "app_password" || row.Label != "Bluesky" ||
		row.RemoteID != "remote-abcd-efgh-ijkl-mnop" || row.Handle != "abcd-efgh-ijkl-mnop.example" || row.DefaultSelected {
		t.Fatalf("row = %+v", row)
	}
	var cfg map[string]any
	if err := json.Unmarshal([]byte(row.Config), &cfg); err != nil {
		t.Fatal(err)
	}
	// The identifier is normalized; the address kept as its origin.
	if cfg["identifier"] != "ada.example" || cfg["pds_url"] != "https://pds.example" || len(cfg) != 2 {
		t.Fatalf("config = %v", cfg)
	}
	if got := w.secretsOf(row.ID, "https://pds.example"); !maps.Equal(got, map[string]string{"app_password": "abcd-efgh-ijkl-mnop"}) {
		t.Fatalf("secrets = %v", got)
	}
	if en.Status != Connected {
		t.Fatalf("status = %s %q", en.Status, en.Reason)
	}
	if got, ok := w.r.Get(row.ID); !ok || got.Status != Connected {
		t.Fatalf("registry: %+v %v", got, ok)
	}
	// The trial and the registry each opened it with the same values.
	if n := w.o.count(); n != 2 {
		t.Fatalf("opened %d times", n)
	}
	if req := w.o.last(); req.Secrets["app_password"] != "abcd-efgh-ijkl-mnop" || req.Method != "app_password" {
		t.Fatalf("opened with %+v", req)
	}
	if !slices.Equal(w.seeded, []string{row.ID}) {
		t.Fatalf("seeded = %v", w.seeded)
	}
	// The label typed is kept.
	n := bsky("other")
	n.Label = "  Work  "
	if en, err := w.r.Create(ctx, n); err != nil || en.Row.Label != "Work" {
		t.Fatalf("label: %q %v", en.Row.Label, err)
	}
}

// Nothing is stored when the fields, the key, or the network say no.
func TestCreateRefused(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name  string
		noKey bool
		n     func() NewAccount
		check func(t *testing.T, err error)
	}{
		{"unknown type", false, func() NewAccount { n := bsky("x"); n.Type = "myspace"; return n }, pointers("/type")},
		{"unknown method", false, func() NewAccount { n := bsky("x"); n.Method = "oauth"; return n }, pointers("/method")},
		{"fields", false, func() NewAccount {
			n := bsky("x")
			delete(n.Credentials, "app_password")
			n.Credentials["extra"] = "y"
			n.Config["pds_url"] = "ftp://pds.example"
			return n
		}, pointers("/config/pds_url", "/credentials/app_password", "/credentials/extra")},
		{"private address", false, func() NewAccount { n := bsky("x"); n.Config["pds_url"] = "https://127.0.0.1"; return n }, pointers("/config/pds_url")},
		{"no key", true, func() NewAccount { return bsky("x") }, func(t *testing.T, err error) {
			if !errors.Is(err, ErrMasterKeyRequired) {
				t.Fatalf("err = %v", err)
			}
		}},
		{"rejected", false, func() NewAccount { return bsky("bad-secret-value") }, func(t *testing.T, err error) {
			var rej *RejectedError
			if !errors.As(err, &rej) {
				t.Fatalf("err = %T %v", err, err)
			}
			// The network's words, without what was sent.
			if rej.Detail != "Invalid identifier or password [redacted]" {
				t.Fatalf("detail = %q", rej.Detail)
			}
		}},
		{"unreachable", false, func() NewAccount { return bsky("down") }, func(t *testing.T, err error) {
			var up *UpstreamError
			if !errors.As(err, &up) || strings.Contains(up.Detail, "busy") {
				t.Fatalf("err = %T %v", err, err)
			}
		}},
		{"blocked at dial", false, func() NewAccount { return bsky("blocked") }, pointers("/config/pds_url")},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w := newWriteEnv(t, c.noKey)
			_, err := w.r.Create(ctx, c.n())
			c.check(t, err)
			if n := w.count(); n != 0 {
				t.Fatalf("%d accounts stored", n)
			}
			// Without a key, the network isn't even asked.
			if c.noKey && w.o.count() != 0 {
				t.Fatal("opened without a master key")
			}
			if len(w.seeded) != 0 {
				t.Fatal("seeded")
			}
		})
	}
}

// pointers checks an InvalidError's pointers.
func pointers(want ...string) func(t *testing.T, err error) {
	return func(t *testing.T, err error) {
		t.Helper()
		var inv *InvalidError
		if !errors.As(err, &inv) {
			t.Fatalf("err = %T %v", err, err)
		}
		var got []string
		for _, p := range inv.Problems {
			got = append(got, p.Pointer)
		}
		slices.Sort(got)
		if !slices.Equal(got, want) {
			t.Fatalf("pointers = %v, want %v (%v)", got, want, err)
		}
	}
}

// The same network account can't be added twice.
func TestCreateDuplicate(t *testing.T) {
	w := newWriteEnv(t, false)
	ctx := context.Background()
	first, err := w.r.Create(ctx, bsky("same-1"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = w.r.Create(ctx, bsky("same-2"))
	var dup *store.DuplicateAccountError
	if !errors.As(err, &dup) || dup.ID != first.Row.ID {
		t.Fatalf("err = %v", err)
	}
	if n := w.count(); n != 1 {
		t.Fatalf("%d accounts", n)
	}
}

// A generated Nostr key is stored like a typed one, and shown again as
// nsec1…; a type without such a credential shows nothing.
func TestCreateGenerateAndReveal(t *testing.T) {
	w := newWriteEnv(t, false)
	ctx := context.Background()
	en, err := w.r.Create(ctx, NewAccount{Owner: store.DefaultOwner, Type: "nostr", Method: "generate",
		Config: map[string]any{"bootstrap_relay": "wss://relay.example"}})
	if err != nil {
		t.Fatal(err)
	}
	stored := w.secretsOf(en.Row.ID, "")
	if len(stored["secret_key"]) != 64 {
		t.Fatalf("stored = %v", stored)
	}
	shown, err := w.r.Reveal(ctx, store.DefaultOwner, en.Row.ID)
	if err != nil || !strings.HasPrefix(shown["secret_key"], "nsec1") || len(shown) != 1 {
		t.Fatalf("reveal = %v, %v", shown, err)
	}
	var cfg map[string]any
	_ = json.Unmarshal([]byte(en.Row.Config), &cfg)
	if cfg["pow_default"] == nil || cfg["fallback_relays"] == nil {
		t.Fatalf("defaults missing: %v", cfg)
	}
	// Typing credentials for a generated key is refused.
	_, err = w.r.Create(ctx, NewAccount{Owner: store.DefaultOwner, Type: "nostr", Method: "generate",
		Config: map[string]any{"bootstrap_relay": "wss://relay.example"}, Credentials: map[string]any{"secret_key": "x"}})
	pointers("/credentials/secret_key")(t, err)

	b, err := w.r.Create(ctx, bsky("pw"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.r.Reveal(ctx, store.DefaultOwner, b.Row.ID); !errors.Is(err, ErrNotRevealable) {
		t.Fatalf("bluesky reveal: %v", err)
	}
}

// What an account saves while it is opened with new credentials is
// stored with it.
func TestCreateKeepsTrialState(t *testing.T) {
	w := newWriteEnv(t, false)
	en, err := w.r.Create(context.Background(), NewAccount{Owner: store.DefaultOwner, Type: "threads", Method: "token",
		Credentials: map[string]any{"access_token": "save-token"}})
	if err != nil {
		t.Fatal(err)
	}
	var cfg map[string]any
	_ = json.Unmarshal([]byte(en.Row.Config), &cfg)
	if cfg["expires_at"] != "2027-01-01T00:00:00Z" || cfg["user_id"] != "me" {
		t.Fatalf("config = %v", cfg)
	}
	// Details shows the owner's fields only.
	d, err := w.r.Details(context.Background(), en.Row)
	if err != nil || !maps.Equal(d.Config, map[string]any{"user_id": "me"}) || !d.Credentials["access_token"].Set ||
		d.Method != "token" || len(d.LockedFields) != 0 {
		t.Fatalf("details = %+v, %v", d, err)
	}
}

func mastodon(token, instance string) NewAccount {
	return NewAccount{Owner: store.DefaultOwner, Type: "mastodon", Method: "token",
		Config: map[string]any{"instance_url": instance}, Credentials: map[string]any{"access_token": token}}
}

// Changing an account opens it with the result first; a new server
// address needs the credentials again; the network account can't change.
func TestChangeCredentials(t *testing.T) {
	w := newWriteEnv(t, false)
	ctx := context.Background()
	en, err := w.r.Create(ctx, mastodon("tok1", "https://one.example"))
	if err != nil {
		t.Fatal(err)
	}
	id := en.Row.ID
	change := func(u CredentialUpdate) (Entry, error) {
		u.Owner, u.ID = store.DefaultOwner, id
		return w.r.ChangeCredentials(ctx, u)
	}

	// Another server without the credentials: refused, nothing changes.
	_, err = change(CredentialUpdate{Config: map[string]any{"instance_url": "https://two.example"}})
	pointers("/credentials")(t, err)

	// With them: sealed for the new server only.
	opens := w.o.count()
	en, err = change(CredentialUpdate{Config: map[string]any{"instance_url": "https://two.example"},
		Credentials: map[string]any{"access_token": "tok1"}})
	if err != nil {
		t.Fatal(err)
	}
	if got := w.secretsOf(id, "https://two.example"); got["access_token"] != "tok1" {
		t.Fatalf("secrets = %v", got)
	}
	if w.o.count() != opens+2 || w.o.last().Config["instance_url"] != "https://two.example" {
		t.Fatalf("reopened with %+v", w.o.last())
	}
	if got, _ := w.r.Get(id); got.Row.Config != en.Row.Config || got.Status != Connected {
		t.Fatalf("registry row = %+v", got.Row)
	}

	// Credentials of another network account: refused.
	_, err = change(CredentialUpdate{Credentials: map[string]any{"access_token": "tok2"}})
	if !errors.Is(err, ErrIdentityChanged) {
		t.Fatalf("err = %v", err)
	}
	if got := w.secretsOf(id, "https://two.example"); got["access_token"] != "tok1" {
		t.Fatalf("changed anyway: %v", got)
	}

	// A stale read is refused.
	_, err = change(CredentialUpdate{Credentials: map[string]any{"access_token": "tok1"}, IfUpdatedAt: t0})
	if !errors.Is(err, store.ErrAccountConflict) {
		t.Fatalf("stale: %v", err)
	}

	_, err = change(CredentialUpdate{Method: "nope"})
	pointers("/method")(t, err)

	// A type with one method needs none named; with two it does.
	if _, err := w.r.Create(ctx, NewAccount{Owner: store.DefaultOwner, Type: "mastodon",
		Config: map[string]any{"instance_url": "https://three.example"}, Credentials: map[string]any{"access_token": "tok3"}}); err != nil {
		t.Fatal(err)
	}
	_, err = w.r.Create(ctx, NewAccount{Owner: store.DefaultOwner, Type: "nostr", Config: map[string]any{"bootstrap_relay": "wss://r.example"}})
	pointers("/method")(t, err)

	// A new method needs its credentials; a generated key would be
	// another account.
	key, err := w.r.Create(ctx, NewAccount{Owner: store.DefaultOwner, Type: "nostr", Method: "secret_key",
		Config: map[string]any{"bootstrap_relay": "wss://r.example"}, Credentials: map[string]any{"secret_key": strings.Repeat("1", 64)}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = w.r.ChangeCredentials(ctx, CredentialUpdate{Owner: store.DefaultOwner, ID: key.Row.ID, Method: "generate"})
	pointers("/credentials")(t, err)
	_, err = w.r.ChangeCredentials(ctx, CredentialUpdate{Owner: store.DefaultOwner, ID: key.Row.ID, Method: "generate",
		Credentials: map[string]any{}})
	pointers("/method")(t, err)
}

// New credentials start an account's saved state afresh; a change of
// configuration alone keeps the credentials and the state.
func TestChangeCredentialsState(t *testing.T) {
	w := newWriteEnv(t, false)
	ctx := context.Background()
	en, err := w.r.Create(ctx, NewAccount{Owner: store.DefaultOwner, Type: "threads", Method: "token",
		Credentials: map[string]any{"access_token": "save-a"}})
	if err != nil {
		t.Fatal(err)
	}
	id := en.Row.ID
	en, err = w.r.ChangeCredentials(ctx, CredentialUpdate{Owner: store.DefaultOwner, ID: id,
		Config: map[string]any{"user_id": "12345"}})
	if err != nil {
		t.Fatal(err)
	}
	var cfg map[string]any
	_ = json.Unmarshal([]byte(en.Row.Config), &cfg)
	if cfg["user_id"] != "12345" || cfg["expires_at"] == nil {
		t.Fatalf("config = %v", cfg)
	}
	// Back to the default.
	en, err = w.r.ChangeCredentials(ctx, CredentialUpdate{Owner: store.DefaultOwner, ID: id, Config: map[string]any{"user_id": nil}})
	if err != nil {
		t.Fatal(err)
	}
	_ = json.Unmarshal([]byte(en.Row.Config), &cfg)
	if cfg["user_id"] != "me" {
		t.Fatalf("config = %v", cfg)
	}
	// "same…" is the same network account whatever the token: a new
	// token that saves nothing leaves no expiry behind; the owner's
	// fields stay.
	same, err := w.r.Create(ctx, NewAccount{Owner: store.DefaultOwner, Type: "threads", Method: "token",
		Config: map[string]any{"user_id": "777"}, Credentials: map[string]any{"access_token": "same-save-1"}})
	if err != nil {
		t.Fatal(err)
	}
	en, err = w.r.ChangeCredentials(ctx, CredentialUpdate{Owner: store.DefaultOwner, ID: same.Row.ID,
		Credentials: map[string]any{"access_token": "same-2"}})
	if err != nil {
		t.Fatal(err)
	}
	cfg = map[string]any{}
	_ = json.Unmarshal([]byte(en.Row.Config), &cfg)
	if cfg["expires_at"] != nil || cfg["user_id"] != "777" {
		t.Fatalf("config = %v", cfg)
	}
	if got := w.secretsOf(same.Row.ID, ""); !maps.Equal(got, map[string]string{"access_token": "same-2"}) {
		t.Fatalf("secrets = %v", got)
	}
	// State an account saved itself (a name starting with _) isn't shown
	// as a credential.
	put := sealFor(t, w.kr, store.DefaultOwner, same.Row.ID, "", map[string]string{"_seen": "x"})
	if err := w.st.SaveAccountState(ctx, store.DefaultOwner, same.Row.ID, nil, put, nil); err != nil {
		t.Fatal(err)
	}
	d, err := w.r.Details(ctx, en.Row)
	if err != nil || len(d.Credentials) != 1 || !d.Credentials["access_token"].Set {
		t.Fatalf("details = %+v, %v", d, err)
	}
}

// An account from the environment can't be changed, deleted or shown in
// the app; adopted, it can, with the environment's values sealed.
func TestAdopt(t *testing.T) {
	envAcct := EnvAccount{Type: "bluesky", EnvAccount: destination.EnvAccount{Key: "default",
		Config:  map[string]any{"identifier": "ada.example", "pds_url": "https://pds.example"},
		Secrets: map[string]string{"app_password": "env-password"}}}
	w := newWriteEnv(t, false, envAcct)
	ctx := context.Background()
	rows, err := w.st.Accounts(ctx, store.DefaultOwner)
	if err != nil || len(rows) != 1 {
		t.Fatal(rows, err)
	}
	id := rows[0].ID
	if _, err := w.r.ChangeCredentials(ctx, CredentialUpdate{Owner: store.DefaultOwner, ID: id,
		Credentials: map[string]any{"identifier": "ada.example", "app_password": "x"}}); !errors.Is(err, store.ErrNotDBAccount) {
		t.Fatalf("change: %v", err)
	}
	if _, err := w.r.Reveal(ctx, store.DefaultOwner, id); !errors.Is(err, store.ErrNotDBAccount) {
		t.Fatalf("reveal: %v", err)
	}
	d, err := w.r.Details(ctx, rows[0])
	if err != nil || d.Method != "app_password" || !slices.Equal(d.LockedFields, []string{"app_password", "identifier", "pds_url"}) ||
		d.Config["identifier"] != "ada.example" || !d.Credentials["app_password"].Set {
		t.Fatalf("env details = %+v, %v", d, err)
	}

	en, err := w.r.Adopt(ctx, store.DefaultOwner, id, "session", "")
	if err != nil {
		t.Fatal(err)
	}
	if en.Row.Source != store.SourceDB || en.Row.AuthMethod != "app_password" || en.Row.EnvKey != "default" || en.Status != Connected {
		t.Fatalf("adopted = %+v %s %q", en.Row, en.Status, en.Reason)
	}
	if got := w.secretsOf(id, "https://pds.example"); got["app_password"] != "env-password" {
		t.Fatalf("secrets = %v", got)
	}
	if req := w.o.last(); req.Secrets["app_password"] != "env-password" || req.Method != "app_password" {
		t.Fatalf("reopened with %+v", req)
	}
	if _, err := w.r.Adopt(ctx, store.DefaultOwner, id, "session", ""); !errors.Is(err, ErrAlreadyInApp) {
		t.Fatalf("again: %v", err)
	}
	if err := w.r.Delete(ctx, store.DefaultOwner, id, "session", ""); err != nil {
		t.Fatal(err)
	}
	if _, ok := w.r.Get(id); ok {
		t.Fatal("still listed")
	}
	if _, err := w.r.Reveal(ctx, store.DefaultOwner, id); !errors.Is(err, store.ErrAccountNotFound) {
		t.Fatalf("reveal deleted: %v", err)
	}
}

// Adopting checks the environment's addresses as if typed in the app.
func TestAdoptChecksAddresses(t *testing.T) {
	envAcct := EnvAccount{Type: "mastodon", EnvAccount: destination.EnvAccount{Key: "default",
		Config: map[string]any{"instance_url": "http://10.0.0.5"}, Secrets: map[string]string{"access_token": "t"}}}
	w := newWriteEnv(t, false, envAcct)
	rows, _ := w.st.Accounts(context.Background(), store.DefaultOwner)
	_, err := w.r.Adopt(context.Background(), store.DefaultOwner, rows[0].ID, "session", "")
	pointers("/config/instance_url")(t, err)
	if !strings.Contains(err.Error(), "in the app this address would be refused") {
		t.Fatalf("err = %v", err)
	}
	// Without its variables there is nothing to adopt.
	w2 := newWriteEnv(t, false)
	if err := w2.st.InsertAccount(context.Background(), store.Account{ID: "acc_gone", Type: "mastodon", EnvKey: "default", Label: "Gone"}); err != nil {
		t.Fatal(err)
	}
	if _, err := w2.r.Adopt(context.Background(), store.DefaultOwner, "acc_gone", "session", ""); !errors.Is(err, ErrNothingToAdopt) {
		t.Fatalf("err = %v", err)
	}
}

// The network's words reach the owner without what was sent, scrubbed,
// on one line and short; a page that isn't JSON is named by its status.
func TestCleanDetail(t *testing.T) {
	sent := map[string]string{"app_password": "abcd-efgh-ijkl-mnop", "short": "ab"}
	he := func(body string) error {
		return &destination.HTTPError{Op: "createSession", StatusCode: 401, Body: body}
	}
	long := strings.Repeat("é", 300)
	for _, c := range []struct {
		err  error
		want string
	}{
		{he(`{"error":"AuthenticationRequired","message":"bad abcd-efgh-ijkl-mnop"}`), "bad [redacted]"},
		{he(`{"error":"invalid_token","error_description":"The access token is invalid"}`), "The access token is invalid"},
		{he(`{"error":{"message":"Invalid OAuth access token.","type":"OAuthException"}}`), "Invalid OAuth access token."},
		{he(`{"error":"Unauthorized"}`), "Unauthorized"},
		{he(`<html><body>nope</body></html>`), "the server answered 401"},
		{he(`{"message":"line\nbreak\u0000here"}`), "line break here"},
		{errors.New("dial tcp: abcd-efgh-ijkl-mnop refused"), "dial tcp: [redacted] refused"},
	} {
		if got := cleanDetail(c.err, sent); got != c.want {
			t.Errorf("cleanDetail(%v) = %q, want %q", c.err, got, c.want)
		}
	}
	got := cleanDetail(he(`{"message":"`+long+`"}`), nil)
	if !utf8.ValidString(got) || !strings.HasSuffix(got, "…") || len(got) > maxUpstreamDetail+len("…") {
		t.Fatalf("long = %q (%d bytes)", got, len(got))
	}
}

// Reopening from a saved row takes its connection; the owner's settings of
// a newer row already held stay (the two saves may land in either order).
func TestReopenWithKeepsNewerSettings(t *testing.T) {
	w := newWriteEnv(t, false)
	en, err := w.r.Create(context.Background(), bsky("pw"))
	if err != nil {
		t.Fatal(err)
	}
	older := en.Row
	newer := older
	newer.Label, newer.Enabled, newer.UpdatedAt = "Newer", false, older.UpdatedAt.Add(time.Second)
	if err := w.r.Update(older.ID, newer); err != nil {
		t.Fatal(err)
	}
	older.Config = `{"identifier":"ada.example","pds_url":"https://moved.example"}`
	got, err := w.r.ReopenWith(context.Background(), older)
	if err != nil {
		t.Fatal(err)
	}
	if got.Row.Label != "Newer" || got.Row.Enabled || !got.Row.UpdatedAt.Equal(newer.UpdatedAt) || got.Row.Config != older.Config {
		t.Fatalf("row = %+v", got.Row)
	}
	// A newer saved row wins whole.
	newest := got.Row
	newest.Label, newest.UpdatedAt = "Newest", newer.UpdatedAt.Add(time.Second)
	if got, _ := w.r.ReopenWith(context.Background(), newest); got.Row.Label != "Newest" {
		t.Fatalf("row = %+v", got.Row)
	}
	if _, err := w.r.ReopenWith(context.Background(), store.Account{ID: "acc_none"}); !errors.Is(err, ErrNoAccount) {
		t.Fatalf("unknown: %v", err)
	}
}

// Two adds of the same network account at once: one is stored, the other
// is told it is a duplicate.
func TestCreateDuplicateRace(t *testing.T) {
	w := newWriteEnv(t, false)
	var wg sync.WaitGroup
	errs := make([]error, 6)
	for i := range errs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = w.r.Create(context.Background(), bsky(fmt.Sprintf("same-%d", i)))
		}()
	}
	wg.Wait()
	ok, dups := 0, 0
	for _, err := range errs {
		var dup *store.DuplicateAccountError
		switch {
		case err == nil:
			ok++
		case errors.As(err, &dup):
			dups++
		default:
			t.Errorf("err = %v", err)
		}
	}
	if ok != 1 || dups != len(errs)-1 || w.count() != 1 {
		t.Fatalf("%d stored, %d duplicates, %d rows", ok, dups, w.count())
	}
}

// A new handle with the same app password keeps the saved password while
// the server stays the same; another identity's server is found again
// (an email can't be looked up, so it needs the address), and a new
// server needs the password again.
func TestChangeIdentifierKeepsPasswordForSameServer(t *testing.T) {
	w := newWriteEnv(t, false)
	ctx := context.Background()
	en, err := w.r.Create(ctx, bsky("pw1"))
	if err != nil {
		t.Fatal(err)
	}
	id := en.Row.ID
	change := func(u CredentialUpdate) (Entry, error) {
		u.Owner, u.ID = store.DefaultOwner, id
		return w.r.ChangeCredentials(ctx, u)
	}
	// The old server isn't assumed for another identity.
	_, err = change(CredentialUpdate{Credentials: map[string]any{"identifier": "ada@example.com"}})
	pointers("/config/pds_url")(t, err)
	// Given the same server: the saved password goes with it.
	en, err = change(CredentialUpdate{Config: map[string]any{"pds_url": "https://pds.example"},
		Credentials: map[string]any{"identifier": "ada@example.com"}})
	if err != nil {
		t.Fatal(err)
	}
	if req := w.o.last(); req.Secrets["app_password"] != "pw1" || req.Config["identifier"] != "ada@example.com" {
		t.Fatalf("opened with %+v", req)
	}
	if got := w.secretsOf(id, "https://pds.example"); got["app_password"] != "pw1" {
		t.Fatalf("secrets = %v", got)
	}
	// Another server: the password is needed again, and nothing changes.
	opens := w.o.count()
	_, err = change(CredentialUpdate{Config: map[string]any{"pds_url": "https://other.example"},
		Credentials: map[string]any{"identifier": "ada@example.com"}})
	pointers("/credentials/app_password")(t, err)
	if w.o.count() != opens {
		t.Fatal("opened anyway")
	}
	if got := w.secretsOf(id, "https://pds.example"); got["app_password"] != "pw1" {
		t.Fatalf("secrets = %v", got)
	}
}

// A change that changes nothing is answered without asking the network.
func TestChangeNothingOpensNothing(t *testing.T) {
	w := newWriteEnv(t, false)
	ctx := context.Background()
	en, err := w.r.Create(ctx, mastodon("tok1", "https://one.example"))
	if err != nil {
		t.Fatal(err)
	}
	opens := w.o.count()
	for _, cfg := range []map[string]any{{}, {"instance_url": "https://one.example"}} {
		got, err := w.r.ChangeCredentials(ctx, CredentialUpdate{Owner: store.DefaultOwner, ID: en.Row.ID, Config: cfg})
		if err != nil || got.Row.UpdatedAt != en.Row.UpdatedAt {
			t.Fatalf("%v: %+v, %v", cfg, got.Row, err)
		}
	}
	if w.o.count() != opens {
		t.Fatalf("opened %d times", w.o.count()-opens)
	}
}

// Credentials that don't open say why, as errors the API answers with
// 409: no key, another key, or a sealed value that was changed.
func TestOpenSecretsErrors(t *testing.T) {
	w := newWriteEnv(t, false)
	ctx := context.Background()
	key, err := w.r.Create(ctx, NewAccount{Owner: store.DefaultOwner, Type: "nostr", Method: "secret_key",
		Config: map[string]any{"bootstrap_relay": "wss://r.example"}, Credentials: map[string]any{"secret_key": strings.Repeat("1", 64)}})
	if err != nil {
		t.Fatal(err)
	}
	id := key.Row.ID
	keys := w.r.opt.Keys
	t.Cleanup(func() { w.r.opt.Keys = keys })
	w.r.opt.Keys = secrets.Keyring{}
	if _, err := w.r.Reveal(ctx, store.DefaultOwner, id); !errors.Is(err, ErrMasterKeyRequired) {
		t.Fatalf("no key: %v", err)
	}
	w.r.opt.Keys = secrets.Keyring{Current: testKey(t, 9)}
	if _, err := w.r.Reveal(ctx, store.DefaultOwner, id); !errors.Is(err, ErrMasterKeyMismatch) {
		t.Fatalf("another key: %v", err)
	}
	w.r.opt.Keys = keys
	// Sealed for another account: it doesn't open here.
	put := sealFor(t, keys, store.DefaultOwner, "acc_other", "", map[string]string{"secret_key": strings.Repeat("2", 64)})
	for i := range put {
		put[i].Ref = id
	}
	if err := w.st.SaveAccountState(ctx, store.DefaultOwner, id, nil, put, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := w.r.Reveal(ctx, store.DefaultOwner, id); !errors.Is(err, ErrCredentialsUnreadable) {
		t.Fatalf("moved: %v", err)
	}
}

// An account from the environment can be deleted once its variables are
// gone; set again, they make a new account.
func TestDeleteEnvAccountNoLongerConfigured(t *testing.T) {
	envAcct := EnvAccount{Type: "mastodon", EnvAccount: destination.EnvAccount{Key: "default",
		Config: map[string]any{"instance_url": "https://m.example"}, Secrets: map[string]string{"access_token": "t"}}}
	w := newWriteEnv(t, false, envAcct)
	ctx := context.Background()
	rows, _ := w.st.Accounts(ctx, store.DefaultOwner)
	if err := w.r.Delete(ctx, store.DefaultOwner, rows[0].ID, "session", ""); !errors.Is(err, store.ErrNotDBAccount) {
		t.Fatalf("configured: %v", err)
	}
	if err := w.st.InsertAccount(ctx, store.Account{ID: "acc_gone", Type: "bluesky", EnvKey: "default", Label: "Gone"}); err != nil {
		t.Fatal(err)
	}
	if err := w.r.Load(ctx); err != nil {
		t.Fatal(err)
	}
	if en, _ := w.r.Get("acc_gone"); en.Status != NotConfigured {
		t.Fatalf("status = %s", en.Status)
	}
	if err := w.r.Delete(ctx, store.DefaultOwner, "acc_gone", "session", ""); err != nil {
		t.Fatal(err)
	}
	if _, ok := w.r.Get("acc_gone"); ok {
		t.Fatal("still listed")
	}
	got, err := w.st.Account(ctx, store.DefaultOwner, "acc_gone")
	if err != nil || got.DeletedAt.IsZero() || got.EnvKey != "" {
		t.Fatalf("row = %+v, %v", got, err)
	}
	seeded, err := w.st.SeedEnvAccounts(ctx, store.DefaultOwner, []store.EnvAccount{{Type: "bluesky", Key: "default", Label: "Back"}})
	if err != nil {
		t.Fatal(err)
	}
	var back []string
	for _, r := range seeded {
		if r.Type == "bluesky" {
			back = append(back, r.ID)
		}
	}
	if len(back) != 1 || back[0] == "acc_gone" {
		t.Fatalf("seeded bluesky rows = %v", back)
	}
}

// The operator's defaults for a new account (FALLBACK_RELAYS,
// POW_DIFFICULTY_DEFAULT) are the forms' defaults, and what a field put
// back to its default gets.
func TestOperatorDefaults(t *testing.T) {
	w := newWriteEnv(t, false)
	w.r.opt.Getenv = func(k string) string {
		return map[string]string{"POW_DIFFICULTY_DEFAULT": "12", "FALLBACK_RELAYS": "wss://fb.example"}[k]
	}
	m, ok := w.r.Manifest("nostr")
	if !ok {
		t.Fatal("no nostr manifest")
	}
	defaults := map[string]any{}
	for _, f := range m.Config {
		defaults[f.Key] = f.Default
	}
	if defaults["pow_default"] != 12 || !sameValue(defaults["fallback_relays"], []string{"wss://fb.example"}) {
		t.Fatalf("defaults = %v", defaults)
	}
	if _, ok := w.r.Manifest("nope"); ok {
		t.Fatal("an unknown type has a manifest")
	}
	ctx := context.Background()
	key, err := w.r.Create(ctx, NewAccount{Owner: store.DefaultOwner, Type: "nostr", Method: "secret_key",
		Config: map[string]any{"bootstrap_relay": "wss://r.example", "pow_default": 5}, Credentials: map[string]any{"secret_key": strings.Repeat("1", 64)}})
	if err != nil {
		t.Fatal(err)
	}
	en, err := w.r.ChangeCredentials(ctx, CredentialUpdate{Owner: store.DefaultOwner, ID: key.Row.ID, Config: map[string]any{"pow_default": nil}})
	if err != nil {
		t.Fatal(err)
	}
	var cfg map[string]any
	_ = json.Unmarshal([]byte(en.Row.Config), &cfg)
	if cfg["pow_default"] != float64(12) {
		t.Fatalf("config = %v", cfg)
	}
}
