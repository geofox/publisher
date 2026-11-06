package config

import (
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func load(env map[string]string) (Config, error) {
	return LoadFrom(func(k string) string { return env[k] })
}

func TestPublicURL(t *testing.T) {
	for in, want := range map[string]string{
		"https://post.example.com":       "https://post.example.com",
		"https://post.example.com/":      "https://post.example.com",
		"https://Post.Example.com:443":   "https://post.example.com",
		"https://post.example.com:8443/": "https://post.example.com:8443",
		"http://localhost:4174":          "http://localhost:4174",
		"http://LOCALHOST":               "http://localhost",
		"https://[2001:db8::1]:8443":     "https://[2001:db8::1]:8443",
	} {
		c, err := load(map[string]string{"PUBLIC_URL": in})
		if err != nil || c.PublicURL != want {
			t.Errorf("PUBLIC_URL=%s: %q, %v; want %q", in, c.PublicURL, err, want)
		}
	}
	for _, bad := range []string{
		"post.example.com", "http://post.example.com", "https://post.example.com/app",
		"https://post.example.com/?x=1", "https://u:p@post.example.com", "ftp://post.example.com",
		"https://post.example.com#x", "https://post.example.com:0", "http://127.0.0.1:8080",
	} {
		if _, err := load(map[string]string{"PUBLIC_URL": bad}); err == nil || !strings.Contains(err.Error(), "PUBLIC_URL") {
			t.Errorf("PUBLIC_URL=%s accepted (err %v)", bad, err)
		}
	}
}

func TestPublicURLIsTheOIDCRedirectBase(t *testing.T) {
	c, err := load(map[string]string{"PUBLIC_URL": "https://post.example.com", "OIDC_ISSUER": "https://auth.example.com",
		"OIDC_CLIENT_ID": "p", "OIDC_CLIENT_SECRET": "s", "OIDC_ALLOWED_SUBJECTS": "me"})
	if err != nil || c.OIDCRedirectURL != "https://post.example.com/auth/callback" {
		t.Fatalf("redirect %q, %v", c.OIDCRedirectURL, err)
	}
	c, err = load(map[string]string{"PUBLIC_URL": "https://post.example.com", "OIDC_ISSUER": "https://auth.example.com",
		"OIDC_CLIENT_ID": "p", "OIDC_CLIENT_SECRET": "s", "OIDC_ALLOWED_SUBJECTS": "me",
		"OIDC_REDIRECT_URL": "https://other.example.com/auth/callback"})
	if err != nil || c.OIDCRedirectURL != "https://other.example.com/auth/callback" {
		t.Fatalf("an explicit redirect must win: %q, %v", c.OIDCRedirectURL, err)
	}
}

func TestAuthMode(t *testing.T) {
	oidc := map[string]string{"OIDC_ISSUER": "https://auth.example.com", "OIDC_CLIENT_ID": "p",
		"OIDC_CLIENT_SECRET": "s", "OIDC_REDIRECT_URL": "https://post.example.com/auth/callback", "OIDC_ALLOWED_SUBJECTS": "me"}
	with := func(base map[string]string, k, v string) map[string]string {
		m := map[string]string{k: v}
		for a, b := range base {
			m[a] = b
		}
		return m
	}
	for _, tc := range []struct {
		env     map[string]string
		created int
		want    string
		err     string
	}{
		{env: nil, created: 11, want: AuthProxy},
		{env: nil, created: 12, want: AuthLocal},
		{env: nil, created: 15, want: AuthLocal},
		{env: oidc, created: 12, want: AuthOIDC},
		{env: map[string]string{"AUTH_MODE": "Local"}, created: 3, want: AuthLocal},
		{env: map[string]string{"AUTH_MODE": "proxy"}, created: 12, want: AuthProxy},
		{env: with(oidc, "AUTH_MODE", "oidc"), created: 11, want: AuthOIDC},
		{env: map[string]string{"AUTH_MODE": "oidc"}, err: "needs OIDC_ISSUER"},
		{env: with(oidc, "AUTH_MODE", "local"), err: "AUTH_MODE=local"},
		{env: with(oidc, "AUTH_MODE", "proxy"), err: "AUTH_MODE=proxy"},
		{env: map[string]string{"AUTH_MODE": "none"}, err: "must be oidc, local or proxy"},
		{env: map[string]string{"AUTH_MODE": "local", "PUBLIC_URL": "https://192.0.2.1"}, err: "domain name"},
		{env: map[string]string{"AUTH_MODE": "local", "PUBLIC_URL": "https://[2001:db8::1]"}, err: "domain name"},
	} {
		c, err := load(tc.env)
		if tc.err != "" {
			if err == nil || !strings.Contains(err.Error(), tc.err) {
				t.Errorf("%v: err %v, want %q", tc.env, err, tc.err)
			}
			continue
		}
		if err != nil {
			t.Errorf("%v: %v", tc.env, err)
			continue
		}
		if got := c.ResolveAuthMode(tc.created); got != tc.want {
			t.Errorf("%v born v%d: mode %s, want %s", tc.env, tc.created, got, tc.want)
		}
	}
	// An IP-literal PUBLIC_URL is fine for proxy and OIDC; CheckLocal
	// refuses it when the mode only resolves to local at start.
	c, err := load(map[string]string{"PUBLIC_URL": "https://192.0.2.1:8443"})
	if err != nil || c.CheckLocal() == nil {
		t.Fatalf("CheckLocal with an IP: %v, %v", err, c.CheckLocal())
	}
}

func TestTrustedProxies(t *testing.T) {
	c, err := load(map[string]string{"TRUSTED_PROXIES": "172.16.0.0/12, 10.0.0.7 ,fd00::/8, ::ffff:192.0.2.9, 10.1.2.3/8"})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"172.16.0.0/12", "10.0.0.7/32", "fd00::/8", "192.0.2.9/32", "10.0.0.0/8"}
	if len(c.TrustedProxies) != len(want) {
		t.Fatalf("got %v", c.TrustedProxies)
	}
	for i, p := range c.TrustedProxies {
		if p != netip.MustParsePrefix(want[i]) {
			t.Errorf("%d: %v, want %s", i, p, want[i])
		}
	}
	if _, err := load(map[string]string{"TRUSTED_PROXIES": "10.0.0.0/8,proxy.local"}); err == nil || !strings.Contains(err.Error(), "TRUSTED_PROXIES") {
		t.Fatalf("a name was accepted: %v", err)
	}
}

func TestSetupTokenFile(t *testing.T) {
	dir := t.TempDir()
	c, err := load(map[string]string{"SETUP_TOKEN_FILE": filepath.Join(dir, "setup-url")})
	if err != nil || c.SetupTokenFile != filepath.Join(dir, "setup-url") {
		t.Fatalf("%q, %v", c.SetupTokenFile, err)
	}
	if _, err := load(map[string]string{"SETUP_TOKEN_FILE": filepath.Join(dir, "missing", "setup-url")}); err == nil {
		t.Fatal("a file in a missing directory was accepted")
	}
	f := filepath.Join(dir, "file")
	_ = os.WriteFile(f, nil, 0o600)
	if _, err := load(map[string]string{"SETUP_TOKEN_FILE": filepath.Join(f, "setup-url")}); err == nil {
		t.Fatal("a file under a file was accepted")
	}
}
