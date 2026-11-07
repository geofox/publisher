package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	gonostr "fiatjaf.com/nostr"
	"github.com/geofox/publisher/internal/api"
	"github.com/geofox/publisher/internal/destination"
	"github.com/geofox/publisher/internal/media"
	"github.com/geofox/publisher/internal/netguard"
	pubnostr "github.com/geofox/publisher/internal/nostr"
	"github.com/geofox/publisher/internal/secrets"
	"github.com/geofox/publisher/internal/settings"
	"github.com/geofox/publisher/internal/store"
)

// The media store follows the settings: none; the environment's Blossom
// server signed with its Nostr key, as before; then the store's own key,
// named in each authorization; a server set in the app reached through
// the owner-set guard.
func TestLiveMediaStore(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "p.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	k, err := secrets.NewKey([]byte("media-store-test-master-key-32-b"))
	if err != nil {
		t.Fatal(err)
	}
	newSvc := func(env map[string]string) *settings.Service {
		svc, err := settings.New(ctx, settings.Options{Store: st, Keys: secrets.Keyring{Current: k}, Env: env,
			URL: func(raw string, rule destination.URLRule) (string, error) {
				return netguard.Policy{}.OwnerURL(raw, netguard.URLOptions{Secure: rule.Secure, Plain: rule.Plain, Path: rule.Path, Query: rule.Query})
			}})
		if err != nil {
			t.Fatal(err)
		}
		return svc
	}
	envKey := gonostr.Generate()
	disk := &media.Local{Dir: t.TempDir()}

	// Nothing set: no store.
	svc := newSvc(nil)
	if _, err := liveMediaStore(svc, guardedStreamClient(netguard.Policy{}), envKey, true, disk)(); !errors.Is(err, media.ErrNoStore) {
		t.Fatalf("none: %v", err)
	}

	// BLOSSOM_URL, signed with the environment's key, without the tag.
	svc = newSvc(map[string]string{"BLOSSOM_URL": "https://blossom.example/"})
	get := liveMediaStore(svc, guardedStreamClient(netguard.Policy{}), envKey, true, disk)
	s, err := get()
	b, ok := s.(*media.Blossom)
	if err != nil || !ok || b.URL != "https://blossom.example" || b.Own || b.Confined || b.Key != envKey {
		t.Fatalf("env: %+v, %v", s, err)
	}
	trusted := b.HTTP
	// Without the environment's key, an upload says to make one.
	var ue *media.UnusableError
	if _, err := liveMediaStore(svc, guardedStreamClient(netguard.Policy{}), gonostr.SecretKey{}, false, disk)(); !errors.As(err, &ue) {
		t.Fatalf("no key: %v", err)
	}

	// The store's own key, from the next upload on.
	hex, npub := pubnostr.NewSigningKey()
	if _, err := svc.Apply(ctx, settings.Change{Scope: settings.Instance, Values: map[string]any{settings.MediaBlossomKey: hex}}); err != nil {
		t.Fatal(err)
	}
	s, err = get()
	b, _ = s.(*media.Blossom)
	if err != nil || b == nil || !b.Own || pubnostr.Npub(hex) != npub || gonostr.GetPublicKey(b.Key) != gonostr.GetPublicKey(mustKey(t, hex)) {
		t.Fatalf("own key: %+v, %v", s, err)
	}

	// A server set in the app goes through the guard.
	svc2 := newSvc(nil)
	if _, err := svc2.Apply(ctx, settings.Change{Scope: settings.Instance, Values: map[string]any{
		settings.MediaStore: settings.StoreBlossom, settings.MediaBlossomURL: "https://media.example"}}); err != nil {
		t.Fatal(err)
	}
	s, err = liveMediaStore(svc2, guardedStreamClient(netguard.Policy{}), envKey, true, disk)()
	b, _ = s.(*media.Blossom)
	if err != nil || b == nil || b.URL != "https://media.example" || b.HTTP == trusted || b.HTTP.CheckRedirect == nil {
		t.Fatalf("saved: %+v, %v", s, err)
	}
	if !b.Own || !b.Confined {
		t.Fatalf("saved, own key: own %v confined %v", b.Own, b.Confined)
	}
	// Signed with the environment's key, its answers still stay on its
	// own origin.
	st2, err := store.Open(filepath.Join(t.TempDir(), "p2.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st2.Close()
	svc3, err := settings.New(ctx, settings.Options{Store: st2, Keys: secrets.Keyring{Current: k},
		URL: func(raw string, rule destination.URLRule) (string, error) {
			return netguard.Policy{}.OwnerURL(raw, netguard.URLOptions{Secure: rule.Secure, Plain: rule.Plain, Path: rule.Path})
		}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc3.Apply(ctx, settings.Change{Scope: settings.Instance, Values: map[string]any{
		settings.MediaStore: settings.StoreBlossom, settings.MediaBlossomURL: "https://media.example"}}); err != nil {
		t.Fatal(err)
	}
	s, err = liveMediaStore(svc3, guardedStreamClient(netguard.Policy{}), envKey, true, disk)()
	if b, _ := s.(*media.Blossom); err != nil || b == nil || b.Own || !b.Confined || b.Key != envKey {
		t.Fatalf("saved, env key: %+v, %v", s, err)
	}
	// This server's disk.
	if _, err := svc2.Apply(ctx, settings.Change{Scope: settings.Instance, Values: map[string]any{settings.MediaStore: settings.StoreLocal}}); err != nil {
		t.Fatal(err)
	}
	if s, err := liveMediaStore(svc2, guardedStreamClient(netguard.Policy{}), envKey, true, disk)(); err != nil || s != media.Store(disk) {
		t.Fatalf("local: %+v, %v", s, err)
	}
	// "Nowhere" again: no store.
	if _, err := svc2.Apply(ctx, settings.Change{Scope: settings.Instance, Values: map[string]any{settings.MediaStore: settings.StoreNone}}); err != nil {
		t.Fatal(err)
	}
	if _, err := liveMediaStore(svc2, guardedStreamClient(netguard.Policy{}), envKey, true, disk)(); !errors.Is(err, media.ErrNoStore) {
		t.Fatalf("none again: %v", err)
	}
}

func mustKey(t *testing.T, s string) gonostr.SecretKey {
	t.Helper()
	k, err := pubnostr.ParseKey(s)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

// Bytes stored before keep their address, unless it is on this server's
// disk and the store is another now, its file is gone, or it was given
// before PUBLIC_URL was set.
func TestReuseStored(t *testing.T) {
	ctx := context.Background()
	disk := &media.Local{Dir: t.TempDir(), BaseURL: "https://pub.example",
		Free: func(string) (uint64, uint64, error) { return 50 << 30, 100 << 30, nil }}
	body := []byte("stored before")
	sum := sha256.Sum256(body)
	sha := hex.EncodeToString(sum[:])
	u, err := disk.Upload(ctx, bytes.NewReader(body), int64(len(body)), sha, "")
	if err != nil {
		t.Fatal(err)
	}
	known := u
	local := true
	lookup := reuseStored(func(string) (string, bool, error) { return known, known != "", nil }, disk, func() bool { return local })
	check := func(why string, want bool) {
		t.Helper()
		if got, ok, err := lookup(sha); err != nil || ok != want || (ok && got != known) {
			t.Errorf("%s: %q %v %v, want %v", why, got, ok, err, want)
		}
	}
	check("on disk, the store", true)
	known = "/media/" + sha + ".bin"
	check("relative, PUBLIC_URL set since", false)
	known = u
	local = false
	check("on disk, another store now", false)
	known = "https://blossom.example/" + sha
	check("on a Blossom server", true)
	local, known = true, u
	if err := disk.Remove(sha + ".bin"); err != nil {
		t.Fatal(err)
	}
	check("file gone", false)
	known = ""
	check("never stored", false)
}

// The media test asks the store set: a Blossom server's BUD-06 answer (one
// without it is reachable), this server's disk written, or fetched at
// PUBLIC_URL when served publicly.
func TestTestMediaStore(t *testing.T) {
	ctx := context.Background()
	var cur media.Store
	mp := media.New(func() (media.Store, error) {
		if cur == nil {
			return nil, media.ErrNoStore
		}
		return cur, nil
	})
	if _, set, _ := testMediaStore(ctx, mp); set {
		t.Error("nothing set, tested")
	}
	status := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodHead || r.URL.Path != "/upload" || r.Header.Get("X-SHA-256") == "" {
			t.Errorf("check = %s %s", r.Method, r.URL.Path)
		}
		w.WriteHeader(status)
	}))
	defer srv.Close()
	cur = &media.Blossom{URL: srv.URL, Key: gonostr.Generate(), HTTP: srv.Client()}
	for answered, want := range map[int]int{200: 200, 401: 401, 404: 204, 405: 204} {
		status = answered
		if got, set, err := testMediaStore(ctx, mp); got != want || !set || err != nil {
			t.Errorf("blossom %d: %d %v %v", answered, got, set, err)
		}
	}
	public := false
	disk := &media.Local{Dir: t.TempDir(), Free: func(string) (uint64, uint64, error) { return 50 << 30, 100 << 30, nil },
		IsPublic: func() bool { return public }}
	cur = disk
	if got, set, err := testMediaStore(ctx, mp); got != 204 || !set || err != nil {
		t.Errorf("disk: %d %v %v", got, set, err)
	}
	front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusUnauthorized) }))
	defer front.Close()
	disk.BaseURL, public = front.URL, true
	if got, set, err := testMediaStore(ctx, mp); got != 401 || !set || err != nil {
		t.Errorf("public behind a sign-in: %d %v %v", got, set, err)
	}
}

// Media fetched again for a send: an address that isn't the environment's
// Blossom server goes through the owner-set guard (a server set in the app
// could answer one inside this server's network); the environment's is
// reached as before.
func TestMediaFetchGuard(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("blob")) }))
	defer srv.Close()
	ctx := context.Background()
	f := &media.Fetcher{HTTP: guardedStreamClient(netguard.Policy{}), TrustedHTTP: media.NewStreamClient()}
	if _, _, err := f.Fetch(ctx, srv.URL+"/x"); !errors.Is(err, destination.ErrBlockedAddress) {
		t.Fatalf("loopback fetched through the guard: %v", err)
	}
	f.TrustedOrigin = media.Origin(srv.URL)
	if b, _, err := f.Fetch(ctx, srv.URL+"/x"); err != nil || string(b) != "blob" {
		t.Fatalf("the environment's server: %q, %v", b, err)
	}
}

// System shows the credential variables the server started with, though
// they are removed from its environment at once.
func TestSystemReadBeforeUnset(t *testing.T) {
	env := map[string]string{"OIDC_CLIENT_SECRET": "s", "PUBLISHER_MASTER_KEY_FILE": "/run/secrets/mk"}
	got := api.SystemValues(func(k string) string { return env[k] }, systemVars)
	var secret, file bool
	for _, v := range got {
		secret = secret || (v.Env == "OIDC_CLIENT_SECRET" && v.Set && v.Value == "")
		file = file || (v.Env == "PUBLISHER_MASTER_KEY_FILE" && v.Value == "/run/secrets/mk")
	}
	if !secret || !file {
		t.Fatalf("system: %+v", got)
	}
}

// An address set in the app never gets the environment's secret.
func TestPairedSecret(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "p.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	k, _ := secrets.NewKey([]byte("media-store-test-master-key-32-b"))
	mk := func(env map[string]string) *settings.Service {
		svc, err := settings.New(ctx, settings.Options{Store: st, Keys: secrets.Keyring{Current: k}, Env: env,
			URL: func(raw string, rule destination.URLRule) (string, error) { return raw, nil }})
		if err != nil {
			t.Fatal(err)
		}
		return svc
	}
	// The address saved first, then ALERT_WEBHOOK_PASS set.
	if _, err := mk(nil).Apply(ctx, settings.Change{Scope: settings.Owner, Values: map[string]any{
		settings.AlertsWebhookURL: "https://elsewhere.example/hook"}}); err != nil {
		t.Fatal(err)
	}
	s := mk(map[string]string{"ALERT_WEBHOOK_PASS": "operator-pw"}).Current()
	if got := pairedSecret(s, settings.AlertsWebhookPass, settings.AlertsWebhookURL); got != "" {
		t.Fatalf("the operator's password went to an app address: %q", got)
	}
	s = mk(map[string]string{"ALERT_WEBHOOK_PASS": "operator-pw", "ALERT_WEBHOOK_URL": "https://am.example/hook"}).Current()
	if got := pairedSecret(s, settings.AlertsWebhookPass, settings.AlertsWebhookURL); got != "operator-pw" {
		t.Fatalf("env with env: %q", got)
	}
}
