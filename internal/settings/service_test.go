package settings

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/geofox/publisher/internal/destination"
	"github.com/geofox/publisher/internal/netguard"
	"github.com/geofox/publisher/internal/secrets"
	"github.com/geofox/publisher/internal/store"
)

func testKeys(t *testing.T) secrets.Keyring {
	t.Helper()
	k, err := secrets.NewKey([]byte("settings-test-master-key-32-byte"))
	if err != nil {
		t.Fatal(err)
	}
	return secrets.Keyring{Current: k}
}

func openStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "p.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func ownerURL(raw string, rule destination.URLRule) (string, error) {
	return netguard.Policy{}.OwnerURL(raw, netguard.URLOptions{Secure: rule.Secure, Plain: rule.Plain, Path: rule.Path, Query: rule.Query})
}

func newService(t *testing.T, st *store.Store, env map[string]string) *Service {
	t.Helper()
	s, err := New(context.Background(), Options{Store: st, Keys: testKeys(t), Env: env, URL: ownerURL})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestPrecedenceEnvSavedDefault(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	s := newService(t, st, nil)
	cur := s.Current()
	if v, _ := cur.Get(PublishingMaxAttempts); v.Source != SourceDefault || cur.Int(PublishingMaxAttempts) != 6 {
		t.Fatalf("default: %+v", v)
	}
	if !cur.Bool(PublishingAutoRetry) || cur.Duration(PublishingGrace).String() != "2h0m0s" {
		t.Fatalf("defaults: retry %v grace %v", cur.Bool(PublishingAutoRetry), cur.Duration(PublishingGrace))
	}
	if _, err := s.Apply(ctx, Change{Scope: Owner, Values: map[string]any{PublishingMaxAttempts: 3, ComposingLanguages: []any{"en", "fr"}}}); err != nil {
		t.Fatal(err)
	}
	cur = s.Current()
	if v, _ := cur.Get(PublishingMaxAttempts); v.Source != SourceSaved || cur.Int(PublishingMaxAttempts) != 3 {
		t.Fatalf("saved: %+v", v)
	}
	if got := cur.Strings(ComposingLanguages); strings.Join(got, ",") != "en,fr" {
		t.Fatalf("languages %v", got)
	}
	// A restart reads the saved values; the environment wins over them.
	s2 := newService(t, st, map[string]string{"AUTO_RETRY_MAX_ATTEMPTS": "9", "USER_LANGUAGES": "de"})
	cur = s2.Current()
	if v, _ := cur.Get(PublishingMaxAttempts); v.Source != SourceEnv || !v.Locked || cur.Int(PublishingMaxAttempts) != 9 {
		t.Fatalf("env: %+v", v)
	}
	if got := cur.Strings(ComposingLanguages); strings.Join(got, ",") != "de" {
		t.Fatalf("env languages %v", got)
	}
	// Writing a locked key is refused, even back to the default.
	_, err := s2.Apply(ctx, Change{Scope: Owner, Values: map[string]any{PublishingMaxAttempts: nil}})
	var le *LockedError
	if !errors.As(err, &le) || le.Env != "AUTO_RETRY_MAX_ATTEMPTS" {
		t.Fatalf("locked: %v", err)
	}
	// null reverts a saved value to its default.
	if _, err := s.Apply(ctx, Change{Scope: Owner, Values: map[string]any{PublishingMaxAttempts: nil}}); err != nil {
		t.Fatal(err)
	}
	if v, _ := s.Current().Get(PublishingMaxAttempts); v.Source != SourceDefault {
		t.Fatalf("after null: %+v", v)
	}
}

func TestValidation(t *testing.T) {
	s := newService(t, openStore(t), nil)
	_, err := s.Apply(context.Background(), Change{Scope: Owner, Values: map[string]any{
		PublishingMaxAttempts: 50, PublishingGrace: "30s", ComposingLanguages: []any{"english"},
		"nope.key": 1, MediaPublic: true, // an instance key in an owner write
	}})
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("got %v", err)
	}
	got := map[string]string{}
	for _, f := range ve.Fields {
		got[f.Key] = f.Code
	}
	want := map[string]string{PublishingMaxAttempts: "out_of_range", PublishingGrace: "out_of_range",
		ComposingLanguages: "invalid", "nope.key": "unknown_field", MediaPublic: "unknown_field"}
	for k, c := range want {
		if got[k] != c {
			t.Errorf("%s: code %q, want %q (%v)", k, got[k], c, ve.Fields)
		}
	}
	if v, _ := s.Current().Get(PublishingMaxAttempts); v.Source != SourceDefault {
		t.Fatal("a refused write changed something")
	}
}

func TestSecretsSealedBoundAndKeptOut(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	s := newService(t, st, nil)
	_, err := s.Apply(ctx, Change{Scope: Owner, Values: map[string]any{
		FeedWebhookURL: "https://hooks.example.com/feed/s3cretpath?k=1", FeedWebhookToken: "tok-123", TranslateDeepLKey: "dl-key:fx",
	}})
	if err != nil {
		t.Fatal(err)
	}
	cur := s.Current()
	if cur.Secret(FeedWebhookURL) != "https://hooks.example.com/feed/s3cretpath?k=1" || cur.Secret(FeedWebhookToken) != "tok-123" {
		t.Fatalf("secrets %q %q", cur.Secret(FeedWebhookURL), cur.Secret(FeedWebhookToken))
	}
	v, _ := cur.Get(FeedWebhookURL)
	if !v.Set || v.Origin != "https://hooks.example.com" || v.Value != nil {
		t.Fatalf("url value %+v", v)
	}
	b, _ := json.Marshal(cur)
	if strings.Contains(string(b), "tok-123") || strings.Contains(string(b), "s3cret") {
		t.Fatal("snapshot JSON holds a secret")
	}
	// Stored sealed: nothing in plaintext.
	rows, sec, err := st.Settings(ctx, store.DefaultOwner)
	if err != nil || len(rows) != 0 || len(sec) != 3 {
		t.Fatalf("rows %v secrets %d err %v", rows, len(sec), err)
	}
	for _, x := range sec {
		if strings.Contains(x.Sealed, "tok-123") || strings.Contains(x.Sealed, "s3cret") {
			t.Fatal("plaintext stored")
		}
		if x.Ref == FeedWebhookToken && x.Audience != "https://hooks.example.com" {
			t.Fatalf("token audience %q", x.Audience)
		}
	}
	// A new address on another origin needs the token again.
	_, err = s.Apply(ctx, Change{Scope: Owner, Values: map[string]any{FeedWebhookURL: "https://other.example.com/feed"}})
	var ve *ValidationError
	if !errors.As(err, &ve) || ve.Fields[0].Key != FeedWebhookToken || ve.Fields[0].Code != "required" {
		t.Fatalf("origin change: %v", err)
	}
	// Same origin, another path: the token stays.
	if _, err := s.Apply(ctx, Change{Scope: Owner, Values: map[string]any{FeedWebhookURL: "https://hooks.example.com/feed/new"}}); err != nil {
		t.Fatal(err)
	}
	if s.Current().Secret(FeedWebhookToken) != "tok-123" {
		t.Fatal("token lost on a same-origin change")
	}
	// Clearing the address takes its token along.
	if _, err := s.Apply(ctx, Change{Scope: Owner, Values: map[string]any{FeedWebhookURL: nil}}); err != nil {
		t.Fatal(err)
	}
	if v, _ := s.Current().Get(FeedWebhookToken); v.Set {
		t.Fatal("token kept without its address")
	}
	// A restart opens what was saved; without the key it can't.
	s2 := newService(t, st, nil)
	if s2.Current().Secret(TranslateDeepLKey) != "dl-key:fx" {
		t.Fatal("restart lost the DeepL key")
	}
	s3, err := New(ctx, Options{Store: st, Env: nil, URL: ownerURL})
	if err != nil {
		t.Fatal(err)
	}
	if v, _ := s3.Current().Get(TranslateDeepLKey); v.Set || v.Broken == "" {
		t.Fatalf("no key: %+v", v)
	}
}

func TestSecretMovedToAnotherRowFailsClosed(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	s := newService(t, st, nil)
	if _, err := s.Apply(ctx, Change{Scope: Owner, Values: map[string]any{AlertsWebhookPass: "pw", AlertsWebhookURL: "https://am.example.com/api/v2/alerts"}}); err != nil {
		t.Fatal(err)
	}
	_, sec, _ := st.Settings(ctx, store.DefaultOwner)
	var pass store.SealedSecret
	for _, x := range sec {
		if x.Ref == AlertsWebhookPass {
			pass = x
		}
	}
	// The password copied over the DeepL key's row doesn't open there.
	pass.Ref = TranslateDeepLKey
	pass.Audience = ""
	if err := st.ApplySettings(ctx, store.SettingsChange{Owner: store.DefaultOwner, PutSecrets: []store.SealedSecret{pass}}); err != nil {
		t.Fatal(err)
	}
	s2 := newService(t, st, nil)
	if v, _ := s2.Current().Get(TranslateDeepLKey); v.Set || v.Broken == "" || s2.Current().Secret(TranslateDeepLKey) != "" {
		t.Fatalf("moved secret opened: %+v", v)
	}
}

func TestCopyFromEnvAndCanSeal(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	env := map[string]string{"PUBLIC_FEED_TOKEN": "feedtok", "SCHEDULE_GRACE": "90m"}
	s := newService(t, st, env)
	for _, k := range []string{FeedToken, PublishingGrace} {
		if _, err := s.CopyFromEnv(ctx, k, "session"); err != nil {
			t.Fatal(k, err)
		}
	}
	// Without the variables, the copies apply.
	s2 := newService(t, st, nil)
	if s2.Current().Secret(FeedToken) != "feedtok" || s2.Current().String(PublishingGrace) != "1h30m" {
		t.Fatalf("copies: %q %q", s2.Current().Secret(FeedToken), s2.Current().String(PublishingGrace))
	}
	if _, err := s2.CopyFromEnv(ctx, FeedToken, "session"); !errors.Is(err, ErrNotFromEnv) {
		t.Fatalf("copied a value not set by env: %v", err)
	}
	// No usable master key: secrets are refused, plain values aren't.
	refuse := errors.New("no key")
	s3, err := New(ctx, Options{Store: st, Keys: testKeys(t), CanSeal: func() error { return refuse }, URL: ownerURL})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s3.Apply(ctx, Change{Scope: Owner, Values: map[string]any{TranslateDeepLKey: "k"}}); !errors.Is(err, refuse) {
		t.Fatalf("secret without key: %v", err)
	}
	if _, err := s3.Apply(ctx, Change{Scope: Owner, Values: map[string]any{PublishingAutoRetry: false}}); err != nil {
		t.Fatal(err)
	}
}

func TestIfMatchAndOnChange(t *testing.T) {
	ctx := context.Background()
	s := newService(t, openStore(t), nil)
	v0 := s.Current().Version(Owner)
	var seen int
	s.OnChange(func(*Snapshot) { seen++ })
	if _, err := s.Apply(ctx, Change{Scope: Owner, IfMatch: v0, Values: map[string]any{PublishingAutoRetry: false}}); err != nil {
		t.Fatal(err)
	}
	if s.Current().Version(Owner) == v0 || seen != 1 {
		t.Fatalf("version %s seen %d", s.Current().Version(Owner), seen)
	}
	if _, err := s.Apply(ctx, Change{Scope: Owner, IfMatch: v0, Values: map[string]any{PublishingAutoRetry: true}}); !errors.Is(err, ErrStale) {
		t.Fatalf("stale: %v", err)
	}
	// An owner change leaves the instance version alone.
	if s.Current().Version(Instance) == "" {
		t.Fatal("no instance version")
	}
}

func TestMediaStoreFollowsBlossomURL(t *testing.T) {
	s := newService(t, openStore(t), map[string]string{"BLOSSOM_URL": "https://blossom.example.com/"})
	cur := s.Current()
	if v, _ := cur.Get(MediaStore); v.Source != SourceEnv || cur.String(MediaStore) != StoreBlossom {
		t.Fatalf("store %+v", v)
	}
	if cur.String(MediaBlossomURL) != "https://blossom.example.com" {
		t.Fatalf("blossom url %q", cur.String(MediaBlossomURL))
	}
	s2 := newService(t, openStore(t), nil)
	if s2.Current().String(MediaStore) != StoreNone {
		t.Fatal("default store")
	}
}

func TestWebhookAddressRules(t *testing.T) {
	s := newService(t, openStore(t), nil)
	for _, bad := range []string{"http://hooks.example.com/x", "https://127.0.0.1/x", "https://169.254.169.254/latest", "https://hooks.example.com/x#frag"} {
		_, err := s.Apply(context.Background(), Change{Scope: Owner, Values: map[string]any{AlertsWebhookURL: bad}})
		var ve *ValidationError
		if !errors.As(err, &ve) {
			t.Errorf("%s accepted", bad)
		}
	}
	// Path (trailing slash) and query kept as given.
	if _, err := s.Apply(context.Background(), Change{Scope: Owner, Values: map[string]any{AlertsWebhookURL: "https://hooks.example.com/a/b/?t=1"}}); err != nil {
		t.Fatal(err)
	}
	if got := s.Current().Secret(AlertsWebhookURL); got != "https://hooks.example.com/a/b/?t=1" {
		t.Fatalf("kept %q", got)
	}
}

// A value the app would refuse isn't copied (it would be dropped at the
// next start); a copy is marked behind the variable, and changes the
// version.
func TestCopyFromEnvChecksAndMarks(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	env := map[string]string{"SCHEDULE_GRACE": "30s", "AUTO_RETRY_MAX_ATTEMPTS": "50",
		"ALERT_WEBHOOK_URL": "http://alertmanager-pushover:5001/alert", "AUTO_RETRY_ENABLED": "false"}
	s := newService(t, st, env)
	for key, envName := range map[string]string{PublishingGrace: "SCHEDULE_GRACE", PublishingMaxAttempts: "AUTO_RETRY_MAX_ATTEMPTS",
		AlertsWebhookURL: "ALERT_WEBHOOK_URL"} {
		_, err := s.CopyFromEnv(ctx, key, "session")
		var ve *ValidationError
		if !errors.As(err, &ve) || ve.Fields[0].Key != key || !strings.Contains(ve.Fields[0].Detail, envName) {
			t.Errorf("%s copied: %v", key, err)
		}
	}
	if rows, sec, _ := st.Settings(ctx, store.DefaultOwner); len(rows) != 0 || len(sec) != 0 {
		t.Fatalf("a refused copy saved %d values, %d secrets", len(rows), len(sec))
	}
	v0 := s.Current().Version(Owner)
	if v, _ := s.Current().Get(PublishingAutoRetry); v.Behind {
		t.Fatal("behind before a copy")
	}
	if _, err := s.CopyFromEnv(ctx, PublishingAutoRetry, "session"); err != nil {
		t.Fatal(err)
	}
	if v, _ := s.Current().Get(PublishingAutoRetry); !v.Behind || v.Source != SourceEnv || s.Current().Version(Owner) == v0 {
		t.Fatalf("after the copy: %+v, version changed %v", v, s.Current().Version(Owner) != v0)
	}
}

// What one variable sets together is copied together, and so are an
// address and its secret.
func TestCopyFromEnvTakesCompanions(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	s := newService(t, st, map[string]string{"BLOSSOM_URL": "https://blossom.example.com/",
		"ALERT_WEBHOOK_URL": "https://am.example.com/hook", "ALERT_WEBHOOK_PASS": "pw"})
	if _, err := s.CopyFromEnv(ctx, MediaBlossomURL, "session"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CopyFromEnv(ctx, AlertsWebhookURL, "session"); err != nil {
		t.Fatal(err)
	}
	if v, _ := s.Current().Get(MediaStore); !v.Behind || v.Env != "BLOSSOM_URL" {
		t.Fatalf("store not copied along: %+v", v)
	}
	s2 := newService(t, st, nil)
	cur := s2.Current()
	if cur.String(MediaStore) != StoreBlossom || cur.String(MediaBlossomURL) != "https://blossom.example.com" {
		t.Fatalf("without BLOSSOM_URL: %q %q", cur.String(MediaStore), cur.String(MediaBlossomURL))
	}
	if cur.Secret(AlertsWebhookURL) != "https://am.example.com/hook" || cur.Secret(AlertsWebhookPass) != "pw" {
		t.Fatalf("webhook: %q %q", cur.Secret(AlertsWebhookURL), cur.Secret(AlertsWebhookPass))
	}
}

// The operator's secret goes only to the operator's address: an address
// typed in the app while the environment sets its secret is refused.
func TestEnvSecretStaysWithEnvAddress(t *testing.T) {
	s := newService(t, openStore(t), map[string]string{"ALERT_WEBHOOK_PASS": "operator-pw"})
	_, err := s.Apply(context.Background(), Change{Scope: Owner, Values: map[string]any{AlertsWebhookURL: "https://elsewhere.example.com/hook"}})
	var ve *ValidationError
	if !errors.As(err, &ve) || ve.Fields[0].Key != AlertsWebhookURL || ve.Fields[0].Code != "locked" ||
		!strings.Contains(ve.Fields[0].Detail, "ALERT_WEBHOOK_URL") {
		t.Fatalf("address set beside the operator's password: %v", err)
	}
}
