package api

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/geofox/publisher/internal/accounts"
	"github.com/geofox/publisher/internal/destination"
	"github.com/geofox/publisher/internal/export"
	"github.com/geofox/publisher/internal/netguard"
	"github.com/geofox/publisher/internal/settings"
	"github.com/geofox/publisher/internal/store"
)

// portEnv is a writeEnv with the settings service: a server to export
// from, or to import into.
type portEnv struct {
	*writeEnv
	svc *settings.Service
}

func newPortEnv(t *testing.T) *portEnv {
	t.Helper()
	w := newWriteEnv(t, true, false)
	svc, err := settings.New(context.Background(), settings.Options{Store: w.st, Keys: w.kr,
		URL: func(raw string, rule destination.URLRule) (string, error) {
			return netguard.Policy{}.OwnerURL(raw, netguard.URLOptions{Secure: rule.Secure, Plain: rule.Plain, Path: rule.Path, Query: rule.Query})
		}})
	if err != nil {
		t.Fatal(err)
	}
	w.api.Settings = svc
	return &portEnv{writeEnv: w, svc: svc}
}

func (e *portEnv) export() v1ExportBody {
	e.t.Helper()
	r := e.do(req{method: "POST", path: "/v1/export", header: e.reauth(opExport)})
	wantStatus(e.t, r, 200)
	var b v1ExportBody
	r.json(&b)
	return b
}

func (e *portEnv) read(file, pass string) v1Resp {
	e.t.Helper()
	body, _ := json.Marshal(v1ImportIn{File: file, Passphrase: pass})
	return e.do(req{method: "POST", path: "/v1/import", body: string(body)})
}

func (e *portEnv) confirm(id string) v1Resp {
	e.t.Helper()
	return e.do(req{method: "POST", path: "/v1/import:confirm", body: `{"confirmation":"` + id + `"}`, header: e.reauth(opImport)})
}

// seed gives the server what an owner sets up: two accounts added in the
// app (Bluesky, a generated Nostr key), the environment's Mastodon
// account relabelled, a set, and settings of both scopes.
func (e *portEnv) seed() (bsky, nostr string) {
	e.t.Helper()
	var a v1AccountBody
	r := e.create("ada.example", "canary-app-password-41f2", nil)
	wantStatus(e.t, r, 201)
	r.json(&a)
	bsky = a.ID
	r = e.do(req{method: "POST", path: "/v1/accounts", body: `{"type":"nostr","method":"generate","label":"Notes","config":{"bootstrap_relay":"wss://relay.example"}}`})
	wantStatus(e.t, r, 201)
	r.json(&a)
	nostr = a.ID
	wantStatus(e.t, e.do(req{method: "PATCH", path: "/v1/accounts/" + bsky, header: map[string]string{"If-Match": "*"},
		body: `{"label":"Ada","default_selected":false,"public_feed":true}`}), 200)
	wantStatus(e.t, e.do(req{method: "PATCH", path: "/v1/accounts/" + e.acct("mastodon"), header: map[string]string{"If-Match": "*"},
		body: `{"label":"Masto here","enabled":false}`}), 200)
	wantStatus(e.t, e.do(req{method: "POST", path: "/v1/account-sets",
		body: `{"name":"Everywhere","accounts":["` + bsky + `","` + nostr + `","` + e.acct("mastodon") + `"],"is_default":true}`}), 201)
	ctx := context.Background()
	if _, err := e.svc.Apply(ctx, settings.Change{Scope: settings.Owner, Values: map[string]any{
		settings.PublishingMaxAttempts: 3, settings.PublishingGrace: "3h",
		settings.AlertsWebhookURL: "https://alerts.example/hook?k=canary-hook-77a1", settings.AlertsWebhookPass: "canary-pass-5c3d"}}); err != nil {
		e.t.Fatal(err)
	}
	if _, err := e.svc.Apply(ctx, settings.Change{Scope: settings.Instance, Values: map[string]any{settings.MediaPublic: false}}); err != nil {
		e.t.Fatal(err)
	}
	return bsky, nostr
}

func (e *portEnv) reveal(id string) string {
	e.t.Helper()
	r := e.do(req{method: "POST", path: "/v1/accounts/" + id + ":reveal", header: e.reauthFor(opAccountReveal, id)})
	wantStatus(e.t, r, 200)
	var b struct{ Credentials map[string]string }
	r.json(&b)
	return b.Credentials["secret_key"]
}

func exportConformance(t *testing.T) map[string]bool {
	t.Helper()
	from, to := newPortEnv(t), newPortEnv(t)
	from.seed()
	seen := map[string]bool{}
	f := from.export()
	seen["POST /v1/export"] = true
	r := to.read(f.File, f.Passphrase)
	wantStatus(t, r, 200)
	seen["POST /v1/import"] = true
	var sum v1ImportSummary
	r.json(&sum)
	wantStatus(t, to.confirm(sum.Confirmation), 200)
	seen["POST /v1/import:confirm"] = true
	return seen
}

// An owner's setup moves to a new server: the accounts added in the app
// with their credentials, what they chose for the environment's, the
// sets, and the settings of both scopes, secrets included.
func TestV1ExportImportRoundTrip(t *testing.T) {
	var logs bytes.Buffer
	var logMu sync.Mutex
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(writerFunc(func(p []byte) (int, error) {
		logMu.Lock()
		defer logMu.Unlock()
		return logs.Write(p)
	}), &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	from, to := newPortEnv(t), newPortEnv(t)
	_, nostr := from.seed()
	nsec := from.reveal(nostr)
	f := from.export()
	if words := strings.Fields(f.Passphrase); len(words) != export.Words {
		t.Fatalf("passphrase %q", f.Passphrase)
	}
	if !strings.HasPrefix(f.File, `{"format":"publisher-export","version":1,`) || !strings.HasSuffix(f.Filename, ".export") ||
		f.Accounts != 4 || f.AccountSets != 1 || f.Settings != 5 {
		t.Fatalf("export = %+v", f)
	}
	for _, c := range []string{"canary-app-password-41f2", "canary-hook-77a1", "canary-pass-5c3d", nsec} {
		if strings.Contains(f.File, c) {
			t.Fatalf("the file holds %q in the clear", c)
		}
	}

	r := to.read(f.File, "  "+strings.ToUpper(strings.ReplaceAll(f.Passphrase, " ", "\n "))+"\n")
	wantStatus(t, r, 200)
	var sum v1ImportSummary
	r.json(&sum)
	if !strings.HasPrefix(sum.Confirmation, "imp_") || sum.ExpiresAt == nil || sum.CreatedAt.IsZero() {
		t.Fatalf("summary = %+v", sum)
	}
	acts := map[string]v1ImportAccount{}
	for _, a := range sum.Accounts {
		acts[a.Type+"/"+a.Label] = a
	}
	if a := acts["bluesky/Ada"]; a.Action != importAdd || a.Server != "https://pds.example" {
		t.Fatalf("bluesky: %+v (all %+v)", a, sum.Accounts)
	}
	if a := acts["nostr/Notes"]; a.Action != importAdd {
		t.Fatalf("nostr: %+v", a)
	}
	if a := acts["mastodon/Masto here"]; a.Action != importUpdate || a.ID != to.acct("mastodon") {
		t.Fatalf("mastodon: %+v", a)
	}
	if a := acts["bluesky/Bluesky"]; a.Action != importUpdate || a.ID != to.acct("bluesky") {
		t.Fatalf("env bluesky: %+v", a)
	}
	if len(sum.AccountSets) != 1 || sum.AccountSets[0].Action != importAdd || sum.AccountSets[0].Accounts != 3 {
		t.Fatalf("sets: %+v", sum.AccountSets)
	}
	sets := map[string]v1ImportSetting{}
	for _, s := range sum.Settings {
		sets[s.Key] = s
	}
	if s := sets[settings.AlertsWebhookURL]; !s.Secret || s.Value != "https://alerts.example" || s.Action != importSet {
		t.Fatalf("webhook: %+v", s)
	}
	if s := sets[settings.AlertsWebhookPass]; !s.Secret || s.Value != nil {
		t.Fatalf("webhook password: %+v", s)
	}
	if s := sets[settings.PublishingMaxAttempts]; s.Value != float64(3) || s.Scope != "owner" {
		t.Fatalf("max attempts: %+v", s)
	}
	if s := sets[settings.MediaPublic]; s.Value != false || s.Scope != "instance" {
		t.Fatalf("media public: %+v", s)
	}
	if strings.Contains(r.Body.String(), "canary") {
		t.Fatalf("the summary shows a secret: %s", r.Body.String())
	}
	// Reading changed nothing.
	if n := len(to.reg.List(store.DefaultOwner)); n != 2 {
		t.Fatalf("accounts after reading: %d", n)
	}

	r = to.confirm(sum.Confirmation)
	wantStatus(t, r, 200)
	var done v1ImportSummary
	r.json(&done)
	got := map[string]v1ImportAccount{}
	for _, a := range done.Accounts {
		got[a.Type+"/"+a.Label] = a
	}
	if a := got["bluesky/Ada"]; a.Status != accounts.ImportConnected || a.ID == "" || a.Handle != "ada.example" {
		t.Fatalf("bluesky: %+v", a)
	}
	if a := got["nostr/Notes"]; a.Status != accounts.ImportConnected || a.ID == "" {
		t.Fatalf("nostr: %+v", a)
	}
	if a := got["mastodon/Masto here"]; a.Status != "updated" {
		t.Fatalf("mastodon: %+v", a)
	}
	if strings.Contains(r.Body.String(), "canary") {
		t.Fatalf("the answer shows a secret: %s", r.Body.String())
	}

	ctx := context.Background()
	b, err := to.st.Account(ctx, store.DefaultOwner, got["bluesky/Ada"].ID)
	if err != nil || b.Label != "Ada" || b.DefaultSelected || !b.PublicFeed || b.Source != store.SourceDB || b.RemoteID != "did:plc:ada.example" {
		t.Fatalf("bluesky here: %+v, %v", b, err)
	}
	m, err := to.st.Account(ctx, store.DefaultOwner, to.acct("mastodon"))
	if err != nil || m.Label != "Masto here" || m.Enabled || m.Source == store.SourceDB {
		t.Fatalf("mastodon here: %+v, %v", m, err)
	}
	if got := to.reveal(got["nostr/Notes"].ID); got != nsec {
		t.Fatal("the Nostr key differs after the import")
	}
	// The Bluesky account was opened with the file's password.
	var opened bool
	to.mu.Lock()
	for _, o := range to.opens {
		if o.Secrets["app_password"] == "canary-app-password-41f2" {
			opened = true
		}
	}
	to.mu.Unlock()
	if !opened {
		t.Fatal("the Bluesky account wasn't opened with its password")
	}
	ss, err := to.st.AccountSets(ctx, store.DefaultOwner)
	if err != nil || len(ss) != 1 || ss[0].Name != "Everywhere" || !ss[0].IsDefault || len(ss[0].Accounts) != 3 {
		t.Fatalf("sets here: %+v, %v", ss, err)
	}
	for _, id := range ss[0].Accounts {
		if _, ok := to.reg.Get(id); !ok {
			t.Fatalf("the set names %s, not an account here", id)
		}
	}
	cur := to.svc.Current()
	if cur.Int(settings.PublishingMaxAttempts) != 3 || cur.Duration(settings.PublishingGrace) != 3*time.Hour ||
		cur.Secret(settings.AlertsWebhookURL) != "https://alerts.example/hook?k=canary-hook-77a1" ||
		cur.Secret(settings.AlertsWebhookPass) != "canary-pass-5c3d" || cur.Bool(settings.MediaPublic) {
		t.Fatalf("settings here: attempts %d grace %v public %v", cur.Int(settings.PublishingMaxAttempts),
			cur.Duration(settings.PublishingGrace), cur.Bool(settings.MediaPublic))
	}

	// Done once: the confirmation is spent.
	wantProblem(t, to.confirm(sum.Confirmation), http.StatusNotFound, codeNotFound)
	// And this server has accounts added in the app now.
	wantProblem(t, to.read(f.File, f.Passphrase), http.StatusConflict, codeInvalidState)

	logMu.Lock()
	defer logMu.Unlock()
	for _, c := range []string{"canary", nsec, f.Passphrase} {
		if strings.Contains(logs.String(), c) {
			t.Fatalf("a log line holds %q", c)
		}
	}
}

// sealed makes an export file of c, as another server would have.
func sealed(t *testing.T, c export.Contents) (file, pass string) {
	t.Helper()
	plain, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	pass, err = export.NewPassphrase()
	if err != nil {
		t.Fatal(err)
	}
	f, err := export.Seal(plain, pass, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return string(f), pass
}

func bskyInFile(id, ident, password string) export.Account {
	return export.Account{ID: id, Source: export.SourceApp, Type: "bluesky", Label: ident, Handle: ident,
		RemoteID: "did:plc:" + ident, Method: "app_password", Enabled: true,
		Config:  map[string]any{"identifier": ident, "pds_url": "https://pds.example"},
		Secrets: map[string]string{"app_password": password}}
}

// Credentials the network refuses, or a network that doesn't answer,
// still add the account, as needing a look; an account the environment
// sets here, one in the file twice, one of a network or way of connecting
// this server lacks, and one whose address isn't allowed are skipped.
func TestV1ImportOutcomes(t *testing.T) {
	to := newPortEnv(t)
	ctx := context.Background()
	if err := to.st.SetAccountIdentity(ctx, to.acct("bluesky"), "env.example", "did:plc:env.example"); err != nil {
		t.Fatal(err)
	}
	if err := to.reg.Load(ctx); err != nil {
		t.Fatal(err)
	}
	private := bskyInFile("a6", "lan.example", "fine-6")
	private.Config["pds_url"] = "https://10.0.0.5"
	file, pass := sealed(t, export.Contents{Version: export.ContentsVersion,
		Accounts: []export.Account{
			bskyInFile("a1", "ok.example", "fine-1"),
			bskyInFile("a2", "no.example", "bad-2"),
			bskyInFile("a3", "slow.example", "down-3"),
			bskyInFile("a4", "env.example", "fine-4"),
			bskyInFile("a5", "ok.example", "fine-5"),
			private,
			{ID: "a7", Source: export.SourceApp, Type: "myspace", Method: "password", Label: "Old"},
			{ID: "a8", Source: export.SourceApp, Type: "bluesky", Method: "carrier-pigeon", Label: "Odd"},
			{ID: "a9", Source: export.SourceEnv, EnvKey: "2", Type: "mastodon", Label: "Elsewhere"},
		},
		Sets: []export.Set{
			{Name: "Some", Accounts: []string{"a1", "a2", "a7"}},
			{Name: "None", Accounts: []string{"a7", "a9"}},
			{Name: "bad\x00name", Accounts: []string{"a1"}},
		},
		Owner: map[string]any{"no.such.setting": 1, settings.PublishingMaxAttempts: 99}})

	r := to.read(file, pass)
	wantStatus(t, r, 200)
	var sum v1ImportSummary
	r.json(&sum)
	plan := map[string]v1ImportAccount{}
	for _, a := range sum.Accounts {
		plan[a.FileID] = a
	}
	for id, want := range map[string]string{"a1": importAdd, "a2": importAdd, "a3": importAdd, "a4": importSkip, "a5": importSkip,
		"a6": importSkip, "a7": importSkip, "a8": importSkip, "a9": importSkip} {
		if plan[id].Action != want {
			t.Errorf("%s: %+v, want %s", id, plan[id], want)
		}
	}
	if !strings.Contains(plan["a4"].Why, "environment") || !strings.Contains(plan["a5"].Why, "twice") ||
		!strings.HasPrefix(plan["a6"].Why, "pds_url: ") || !strings.Contains(plan["a8"].Why, "way of connecting") {
		t.Errorf("why: %q, %q, %q, %q", plan["a4"].Why, plan["a5"].Why, plan["a6"].Why, plan["a8"].Why)
	}

	r = to.confirm(sum.Confirmation)
	wantStatus(t, r, 200)
	var done v1ImportSummary
	r.json(&done)
	res := map[string]v1ImportAccount{}
	for _, a := range done.Accounts {
		res[a.FileID] = a
	}
	for id, want := range map[string]string{"a1": accounts.ImportConnected, "a2": accounts.ImportRefused, "a3": accounts.ImportUnchecked,
		"a4": accounts.ImportSkipped, "a6": accounts.ImportSkipped, "a7": accounts.ImportSkipped} {
		if res[id].Status != want {
			t.Errorf("%s: %+v, want %s", id, res[id], want)
		}
	}
	for _, id := range []string{"a1", "a2", "a3"} {
		if res[id].ID == "" {
			t.Errorf("%s wasn't added: %+v", id, res[id])
		}
	}
	if res["a6"].ID != "" {
		t.Errorf("the private address: %+v", res["a6"])
	}
	if strings.Contains(r.Body.String(), "bad-2") || strings.Contains(r.Body.String(), "down-3") {
		t.Fatalf("a credential in the answer: %s", r.Body.String())
	}
	row, err := to.st.Account(ctx, store.DefaultOwner, res["a2"].ID)
	if err != nil || row.RemoteID != "did:plc:no.example" || row.Handle != "no.example" {
		t.Fatalf("the refused account: %+v, %v", row, err)
	}
	sets := map[string]v1ImportSet{}
	for _, s := range done.AccountSets {
		sets[s.Name] = s
	}
	if sets["Some"].Action != importAdd || sets["Some"].Accounts != 2 || sets["None"].Action != importSkip ||
		sets["bad\x00name"].Action != importSkip {
		t.Fatalf("sets: %+v", done.AccountSets)
	}
	var unknown, refused bool
	for _, s := range done.Settings {
		switch {
		case s.Key == "no.such.setting" && s.Action == importSkip:
			unknown = true
		case s.Key == settings.PublishingMaxAttempts && s.Action == importSkip && s.Why != "":
			refused = true
		}
	}
	if !unknown || !refused {
		t.Fatalf("settings: %+v", done.Settings)
	}
}

// Each refusal comes before anything changes, and none echoes the file
// or the passphrase.
func TestV1ImportRefusals(t *testing.T) {
	from, to := newPortEnv(t), newPortEnv(t)
	from.seed()
	f := from.export()
	other, _ := export.NewPassphrase()
	at := func(r v1Resp, ptr string) {
		t.Helper()
		p := wantProblem(t, r, http.StatusUnprocessableEntity, codeValidation)
		if len(p.Errors) == 0 || p.Errors[0].Pointer != ptr {
			t.Fatalf("problem %+v, want at %s", p, ptr)
		}
		if strings.Contains(r.Body.String(), f.Passphrase) || strings.Contains(r.Body.String(), f.File[:40]) {
			t.Fatal("the problem echoes the file or the passphrase")
		}
	}
	at(to.read(f.File, other), "/passphrase")
	at(to.read(f.File, "one two three"), "/passphrase")
	at(to.read(f.File, strings.Replace(f.Passphrase, strings.Fields(f.Passphrase)[0], "zzzz", 1)), "/passphrase")
	at(to.read("hello\nworld\n", f.Passphrase), "/file")
	at(to.read("", f.Passphrase), "/file")
	head, body, _ := strings.Cut(f.File, "\n")
	// The header can't be changed: it is the contents' additional data.
	at(to.read(strings.Replace(head, `"created_at":"`, `"created_at":"1`, 1)+"\n"+body, f.Passphrase), "/file")
	changed := strings.Replace(head, `"version":1`, `"version":1 `, 1)
	at(to.read(changed+"\n"+body, f.Passphrase), "/passphrase")
	// Pinned parameters, and a newer version, are refused before any work.
	for _, h := range []string{strings.Replace(head, `"t":3`, `"t":1`, 1), strings.Replace(head, `"m":65536`, `"m":4194304`, 1),
		strings.Replace(head, `"version":1`, `"version":2`, 1), strings.Replace(head, `"kdf":"argon2id"`, `"kdf":"scrypt"`, 1)} {
		at(to.read(h+"\n"+body, f.Passphrase), "/file")
	}
	// A changed byte of the contents: the passphrase doesn't open it.
	b := []byte(body)
	if b[10] == 'A' {
		b[10] = 'B'
	} else {
		b[10] = 'A'
	}
	at(to.read(head+"\n"+string(b), f.Passphrase), "/passphrase")
	if n := len(to.reg.List(store.DefaultOwner)); n != 2 {
		t.Fatalf("accounts after refusals: %d", n)
	}

	// Only a signed-in browser.
	tok := to.token("admin")
	wantProblem(t, to.do(req{method: "POST", path: "/v1/import", auth: tok, body: `{"file":"x","passphrase":"y"}`}),
		http.StatusForbidden, codeSessionRequired)
	wantProblem(t, to.do(req{method: "POST", path: "/v1/export", auth: tok}), http.StatusForbidden, codeSessionRequired)
	// Re-authenticated, for each.
	p := wantProblem(t, from.do(req{method: "POST", path: "/v1/export"}), http.StatusForbidden, codeReauthRequired)
	if p.Operation != opExport {
		t.Fatalf("operation %q", p.Operation)
	}
	r := to.read(f.File, f.Passphrase)
	wantStatus(t, r, 200)
	var sum v1ImportSummary
	r.json(&sum)
	p = wantProblem(t, to.do(req{method: "POST", path: "/v1/import:confirm", body: `{"confirmation":"` + sum.Confirmation + `"}`}),
		http.StatusForbidden, codeReauthRequired)
	if p.Operation != opImport {
		t.Fatalf("operation %q", p.Operation)
	}
	// Another session's confirmation isn't this one's.
	u, err := to.st.UpsertUser("sub-1", "ann@example.com", "Ann")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := to.st.CreateSession(u.ID, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	mine := to.cookie
	to.cookie = &http.Cookie{Name: sessionCookie, Value: raw}
	wantProblem(t, to.confirm(sum.Confirmation), http.StatusNotFound, codeNotFound)
	to.cookie = mine
	// A confirmation lasts 10 minutes.
	po := to.api.port()
	po.mu.Lock()
	po.pending[sum.Confirmation].expires = time.Now().Add(-time.Second)
	po.mu.Unlock()
	wantProblem(t, to.confirm(sum.Confirmation), http.StatusNotFound, codeNotFound)
	wantProblem(t, to.confirm("imp_nothing"), http.StatusNotFound, codeNotFound)
	if n := len(to.reg.List(store.DefaultOwner)); n != 2 {
		t.Fatalf("accounts after refused confirmations: %d", n)
	}
}

// Reading a file again in the same session replaces the one waiting.
func TestV1ImportOnePerSession(t *testing.T) {
	from, to := newPortEnv(t), newPortEnv(t)
	from.seed()
	f := from.export()
	var first, second v1ImportSummary
	to.read(f.File, f.Passphrase).json(&first)
	to.read(f.File, f.Passphrase).json(&second)
	if first.Confirmation == second.Confirmation {
		t.Fatal("the same confirmation twice")
	}
	wantProblem(t, to.confirm(first.Confirmation), http.StatusNotFound, codeNotFound)
	wantStatus(t, to.confirm(second.Confirmation), 200)
}

// Exports and files read share a limit: 5 a minute.
func TestV1ExportLimit(t *testing.T) {
	e := newPortEnv(t)
	for range exportsPerMinute {
		e.api.limits().exports.Take(store.DefaultOwner)
	}
	wantProblem(t, e.do(req{method: "POST", path: "/v1/export", header: e.reauth(opExport)}), http.StatusTooManyRequests, codeRateLimited)
	f, pass := sealed(t, export.Contents{Version: export.ContentsVersion})
	wantProblem(t, e.read(f, pass), http.StatusTooManyRequests, codeRateLimited)
}

// Without a master key nothing can be imported (credentials can't be
// kept); a server with an account added in the app refuses an import.
func TestV1ImportNeedsKeyAndEmptyServer(t *testing.T) {
	f, pass := sealed(t, export.Contents{Version: export.ContentsVersion})
	noKey := newWriteEnv(t, true, true)
	body, _ := json.Marshal(v1ImportIn{File: f, Passphrase: pass})
	wantProblem(t, noKey.do(req{method: "POST", path: "/v1/import", body: string(body)}), http.StatusConflict, codeMasterKeyRequired)
	e := newPortEnv(t)
	wantStatus(t, e.create("ada.example", "abcd-efgh-ijkl-mnop", nil), 201)
	wantProblem(t, e.read(f, pass), http.StatusConflict, codeInvalidState)
}
