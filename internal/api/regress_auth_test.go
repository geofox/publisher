package api

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
	"golang.org/x/oauth2"

	"github.com/geofox/publisher/internal/auth"
)

// qaMockOIDC is newMockOIDC with the ID token's claims given by the test.
func qaMockOIDC(t *testing.T, extra map[string]any) string {
	t.Helper()
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	signer, _ := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: key},
		(&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", "test"))
	var srv *httptest.Server
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"issuer": srv.URL, "authorization_endpoint": srv.URL + "/auth",
			"token_endpoint": srv.URL + "/token", "jwks_uri": srv.URL + "/jwks"})
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: key.Public(), KeyID: "test", Algorithm: "RS256", Use: "sig"}}})
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		c := map[string]any{"iss": srv.URL, "aud": "client-1", "nonce": "n1",
			"exp": time.Now().Add(time.Hour).Unix(), "iat": time.Now().Unix()}
		for k, v := range extra {
			c[k] = v
		}
		w.Header().Set("Content-Type", "application/json")
		tok, _ := jwt.Signed(signer).Claims(c).Serialize()
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "a", "token_type": "Bearer", "id_token": tok})
	})
	srv = httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv.URL
}

// An identity whose email the IdP says is NOT verified must not pass an
// email allowlist entry: anyone who can register that address at the IdP
// (self sign-up, social login linking) would get the owner's session.
func TestUnverifiedEmailPassesAllowlist(t *testing.T) {
	issuer := qaMockOIDC(t, map[string]any{"sub": "attacker-sub", "email": "owner@example.com", "email_verified": false})
	au, err := auth.New(context.Background(), auth.Config{Issuer: issuer, ClientID: "client-1", ClientSecret: "s",
		RedirectURL: "https://app/auth/callback"})
	if err != nil {
		t.Fatal(err)
	}
	db, err := storeOpen(t)
	if err != nil {
		t.Fatal(err)
	}
	a := &API{Store: db, Auth: au, Allowlist: auth.NewAllowlist(nil, []string{"owner@example.com"}), SessionTTL: time.Hour}
	ls, _ := json.Marshal(loginState{State: "st", Nonce: "n1", PKCEVerifier: oauth2.GenerateVerifier()})
	r := httptest.NewRequest("GET", "/auth/callback?state=st&code=x", nil)
	r.AddCookie(&http.Cookie{Name: loginStateCookie, Value: base64.RawURLEncoding.EncodeToString(ls)})
	w := httptest.NewRecorder()
	a.handleAuthCallback(w, r)
	t.Logf("status %d body %s", w.Code, w.Body.String())
	for _, c := range w.Result().Cookies() {
		if c.Name == sessionCookie && c.Value != "" {
			t.Fatalf("email_verified=false identity got a session (status %d)", w.Code)
		}
	}
}

// Removing an identity from OIDC_ALLOWED_* (and restarting) should end its
// access; its existing session (SESSION_TTL, 168h by default) keeps working.
func TestSessionOutlivesAllowlistRemoval(t *testing.T) {
	e := newV1Env(t, true) // session for sub-1, allowed
	e.api.Allowlist = auth.NewAllowlist([]string{"someone-else"}, nil)
	e.h = e.api.Routes()
	r := e.do(req{method: "GET", path: "/v1/tokens"})
	if r.Code == 200 {
		t.Fatalf("session of a de-allowlisted identity still works: GET /v1/tokens = %d", r.Code)
	}
}
