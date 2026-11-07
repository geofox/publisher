package main

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/geofox/publisher/internal/secrets"
	"github.com/geofox/publisher/internal/store"
)

// A fresh database signs in locally: pages answer the sign-in page, the
// API wants a session, and the setup link is written to SETUP_TOKEN_FILE
// (0600), not the log.
func TestRunFreshDatabaseSignsInLocally(t *testing.T) {
	file := filepath.Join(t.TempDir(), "setup-url")
	url, _ := startRun(t, map[string]string{"PUBLIC_URL": "http://localhost", "SETUP_TOKEN_FILE": file})
	if code := get(t, url+"/v1/instance", nil); code != http.StatusUnauthorized {
		t.Fatalf("/v1/instance = %d, want 401", code)
	}
	var m struct {
		Mode  string `json:"mode"`
		Setup bool   `json:"setup"`
	}
	if code := get(t, url+"/auth/methods", &m); code != 200 || m.Mode != "local" || !m.Setup {
		t.Fatalf("/auth/methods = %d %+v", code, m)
	}
	rq, _ := http.NewRequest("GET", url+"/", nil)
	rq.Header.Set("Sec-Fetch-Mode", "navigate")
	resp, err := http.DefaultClient.Do(rq)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(string(b), "signin.js") {
		t.Fatalf("GET / = %d, want the sign-in page: %.200s", resp.StatusCode, b)
	}
	fi, err := os.Stat(file)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("setup file mode %v, want 0600", fi.Mode().Perm())
	}
	link, _ := os.ReadFile(file)
	if !regexp.MustCompile(`^http://localhost/setup#[A-Za-z0-9_-]{40,}\n$`).Match(link) {
		t.Errorf("setup file = %q", link)
	}
}

// Local sign-in refuses an IP address for PUBLIC_URL (passkeys can't use
// one) at startup.
func TestRunLocalRefusesAnIPPublicURL(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	env := map[string]string{"AUTH_MODE": "local", "PUBLIC_URL": "https://192.0.2.1", "PORT": "0",
		"DB_PATH": filepath.Join(t.TempDir(), "p.db")}
	if code := run(ctx, runOptions{Getenv: func(k string) string { return env[k] }}); code != 2 {
		t.Fatalf("exit code %d, want 2 (configuration error)", code)
	}
}

type cliEnv map[string]string

func (e cliEnv) run(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	code, ok := runCLI(context.Background(), args, func(k string) string { return e[k] }, &out, &errb)
	if !ok {
		t.Fatalf("%v: not a command", args)
	}
	return code, out.String(), errb.String()
}

var linkRE = regexp.MustCompile(`https://post\.example/setup#([A-Za-z0-9_-]+)`)

func TestCLISetupURL(t *testing.T) {
	ctx := context.Background()
	env := cliEnv{"DB_PATH": filepath.Join(t.TempDir(), "p.db"), "PUBLIC_URL": "https://post.example"}
	code, out, errs := env.run(t, "setup-url")
	if code != 0 || !linkRE.MatchString(out) {
		t.Fatalf("setup-url = %d %q %q", code, out, errs)
	}
	first := linkRE.FindStringSubmatch(out)[1]
	code, out, _ = env.run(t, "setup-url")
	second := linkRE.FindStringSubmatch(out)[1]
	if code != 0 || second == first {
		t.Fatalf("second setup-url = %d %q", code, out)
	}
	st, err := store.Open(env["DB_PATH"])
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.ConsumeSetupToken(ctx, first); err == nil {
		t.Error("the older setup link still works")
	}
	if err := st.ConsumeSetupToken(ctx, second); err != nil {
		t.Errorf("the new setup link: %v", err)
	}
	if _, err := st.CreateFirstPassword(ctx, "post.example", "$argon2id$v=19$m=65536,t=3,p=4$c2FsdHNhbHRzYWx0c2FsdA$aGFzaGhhc2hoYXNoaGFzaGhhc2hoYXNoaGFzaGhhc2g"); err != nil {
		t.Fatal(err)
	}
	if code, _, errs := env.run(t, "setup-url"); code != 1 || !strings.Contains(errs, "reset-login") {
		t.Errorf("setup-url after setup = %d %q", code, errs)
	}
}

func TestCLIRefusesOutsideLocalMode(t *testing.T) {
	for _, mode := range []string{"proxy", "oidc"} {
		env := cliEnv{"DB_PATH": filepath.Join(t.TempDir(), "p.db"), "AUTH_MODE": mode}
		if mode == "oidc" {
			env["OIDC_ISSUER"], env["OIDC_CLIENT_ID"], env["OIDC_CLIENT_SECRET"] = "https://id.example", "c", "s"
			env["OIDC_REDIRECT_URL"], env["OIDC_ALLOWED_SUBJECTS"] = "https://post.example/auth/callback", "me"
		}
		for _, args := range [][]string{{"setup-url"}, {"owner", "reset-login"}} {
			if code, _, errs := env.run(t, args...); code != 2 || !strings.Contains(errs, "AUTH_MODE="+mode) {
				t.Errorf("%s %v = %d %q", mode, args, code, errs)
			}
		}
	}
}

func TestCLIResetLogin(t *testing.T) {
	ctx := context.Background()
	env := cliEnv{"DB_PATH": filepath.Join(t.TempDir(), "p.db"), "PUBLIC_URL": "https://post.example"}
	st, err := store.Open(env["DB_PATH"])
	if err != nil {
		t.Fatal(err)
	}
	u, err := st.CreateFirstPassword(ctx, "post.example", "$argon2id$v=19$m=65536,t=3,p=4$c2FsdHNhbHRzYWx0c2FsdA$aGFzaGhhc2hoYXNoaGFzaGhhc2hoYXNoaGFzaGhhc2g")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := st.CreateSession(u.ID, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	strong, _, _ := st.CreateScopedToken("deploy bot", []string{"admin"}, time.Time{})
	weak, _, _ := st.CreateScopedToken("n8n", []string{"posts:write"}, time.Time{})
	_ = st.Close()

	code, out, errs := env.run(t, "owner", "reset-login")
	if code != 0 || !strings.Contains(out, `"deploy bot"`) || !strings.Contains(out, `"n8n"`) || !linkRE.MatchString(out) {
		t.Fatalf("reset-login = %d %q %q", code, out, errs)
	}
	st, err = store.Open(env["DB_PATH"])
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if done, _ := st.HasLocalCredential(ctx, "post.example"); done {
		t.Error("the password survived reset-login")
	}
	if _, _, err := st.SessionByRaw(raw); err == nil {
		t.Error("the session survived reset-login")
	}
	if tok, _ := st.APIToken(strong.ID); tok.Revoked {
		t.Error("reset-login without --revoke-tokens revoked a token")
	}

	code, out, _ = env.run(t, "owner", "reset-login", "--revoke-tokens")
	if code != 0 || !strings.Contains(out, `Revoked token "deploy bot"`) || !strings.Contains(out, `Revoked token "n8n"`) {
		t.Fatalf("reset-login --revoke-tokens = %d %q", code, out)
	}
	// Every token: a posting token outlives the sessions too.
	if tok, _ := st.APIToken(strong.ID); !tok.Revoked {
		t.Error("the admin token wasn't revoked")
	}
	if tok, _ := st.APIToken(weak.ID); !tok.Revoked {
		t.Error("the posting token wasn't revoked")
	}
	evs, _ := st.SignInEvents(ctx, store.DefaultOwner, 10, 10)
	if len(evs) == 0 || evs[0].Data.Method != "reset-login" {
		t.Errorf("events = %+v", evs)
	}
	if code, _, _ := env.run(t, "owner", "reset-login", "--nope"); code != 2 {
		t.Errorf("an unknown flag = %d, want 2", code)
	}
}

// The CLI refuses a PUBLIC_URL the server refuses (an IP address can't be
// a passkey's relying party), rather than print a useless link.
func TestCLIRefusesAnIPPublicURL(t *testing.T) {
	env := cliEnv{"DB_PATH": filepath.Join(t.TempDir(), "p.db"), "PUBLIC_URL": "https://192.0.2.7"}
	if code, out, errs := env.run(t, "setup-url"); code != 2 || out != "" || !strings.Contains(errs, "PUBLIC_URL") {
		t.Errorf("setup-url = %d %q %q", code, out, errs)
	}
}

func TestCLIKeygen(t *testing.T) {
	code, out, _ := cliEnv{}.run(t, "keygen")
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	m := regexp.MustCompile(`(?m)^  ([A-Za-z0-9+/]{43}=)$`).FindStringSubmatch(out)
	if m == nil {
		t.Fatalf("no key in:\n%s", out)
	}
	if _, err := secrets.ParseMasterKey(m[1]); err != nil {
		t.Errorf("key doesn't parse: %v", err)
	}
	if !strings.Contains(out, "PUBLISHER_MASTER_KEY_FILE: /run/secrets/publisher_master_key") {
		t.Errorf("no compose snippet:\n%s", out)
	}
	_, out2, _ := cliEnv{}.run(t, "keygen")
	if out2 == out {
		t.Error("two keygens printed the same key")
	}
}

// The server takes every credential variable out of its environment once
// read, and a malformed master key stops it.
func TestRunUnsetsCredentialVariables(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	env := map[string]string{"PORT": "0", "DB_PATH": filepath.Join(t.TempDir(), "p.db"), "AUTH_MODE": "proxy",
		"PUBLISHER_MASTER_KEY": secrets.Generate(), "MASTODON_TOKEN": "tok", "MASTODON_BASE_URL": "https://m.example",
		"DEEPL_API_KEY": "deepl", "ALERT_WEBHOOK_URL": "https://hooks.example/secret-path"}
	var mu sync.Mutex
	unset := map[string]bool{}
	done := make(chan int)
	go func() {
		done <- run(ctx, runOptions{Getenv: func(k string) string { return env[k] },
			Unsetenv: func(k string) error { mu.Lock(); unset[k] = true; mu.Unlock(); return nil },
			Started:  func(string) { cancel() }})
	}()
	if code := <-done; code != 0 {
		t.Fatalf("exit %d", code)
	}
	for _, k := range []string{"PUBLISHER_MASTER_KEY", "PUBLISHER_MASTER_KEY_FILE", "MASTODON_TOKEN", "DEEPL_API_KEY", "ALERT_WEBHOOK_URL", "NSEC_HEX", "BLUESKY_APP_PASSWORD", "THREADS_ACCESS_TOKEN", "OIDC_CLIENT_SECRET"} {
		if !unset[k] {
			t.Errorf("%s not unset", k)
		}
	}
	if unset["MASTODON_BASE_URL"] || unset["PORT"] {
		t.Error("a non-secret variable was unset")
	}
	bad := map[string]string{"PORT": "0", "DB_PATH": filepath.Join(t.TempDir(), "p.db"), "AUTH_MODE": "proxy", "PUBLISHER_MASTER_KEY": "short"}
	if code := run(context.Background(), runOptions{Getenv: func(k string) string { return bad[k] }}); code != 2 {
		t.Errorf("malformed key: exit %d, want 2", code)
	}
}
