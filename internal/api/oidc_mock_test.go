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

	"golang.org/x/oauth2"

	"github.com/geofox/publisher/internal/auth"
	jose "github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
)

// newMockOIDC starts a test OIDC provider that:
//   - serves /.well-known/openid-configuration
//   - serves /jwks with the RSA public key
//   - serves /token (POST) returning a freshly-signed id_token whose nonce,
//     aud, sub and email are baked in for the happy-path test
//
// It returns the issuer base URL (== srv.URL).
func newMockOIDC(t *testing.T) string {
	t.Helper()

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}

	signer, err := jose.NewSigner(
		jose.SigningKey{Algorithm: jose.RS256, Key: key},
		(&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", "test"),
	)
	if err != nil {
		t.Fatal(err)
	}

	var srv *httptest.Server
	mux := http.NewServeMux()

	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer":                 srv.URL,
			"authorization_endpoint": srv.URL + "/auth",
			"token_endpoint":         srv.URL + "/token",
			"jwks_uri":               srv.URL + "/jwks",
		})
	})

	mux.HandleFunc("/jwks", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{
			{Key: key.Public(), KeyID: "test", Algorithm: "RS256", Use: "sig"},
		}})
	})

	// /token: accept any code and return a signed id_token with the test claims.
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		claims := map[string]any{
			"iss":   srv.URL,
			"aud":   "client-1",
			"sub":   "sub-1",
			"nonce": "n1",
			"email": "test@example.com",
			"name":  "Test User",
			"exp":   time.Now().Add(time.Hour).Unix(),
			"iat":   time.Now().Unix(),
		}
		idToken, err := jwt.Signed(signer).Claims(claims).Serialize()
		if err != nil {
			http.Error(w, "sign error: "+err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "mock-access-token",
			"token_type":   "Bearer",
			"id_token":     idToken,
		})
	})

	srv = httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv.URL
}

// TestCallbackHappyPath exercises the full handleAuthCallback chain:
// login-state cookie → auth.Exchange → allowlist → UpsertUser → CreateSession → cookie.
func TestCallbackHappyPath(t *testing.T) {
	issuer := newMockOIDC(t)

	au, err := auth.New(context.Background(), auth.Config{
		Issuer:       issuer,
		ClientID:     "client-1",
		ClientSecret: "s",
		RedirectURL:  "https://app/auth/callback",
	})
	if err != nil {
		t.Fatal(err)
	}

	db, err := storeOpen(t)
	if err != nil {
		t.Fatal(err)
	}

	a := &API{
		Store:      db,
		Auth:       au,
		Allowlist:  auth.NewAllowlist([]string{"sub-1"}, nil),
		SessionTTL: time.Hour,
	}

	ls := loginState{
		State:        "st",
		Nonce:        "n1",
		PKCEVerifier: oauth2.GenerateVerifier(),
	}
	rawLS, _ := json.Marshal(ls)

	req := httptest.NewRequest(http.MethodGet, "/auth/callback?state=st&code=abc", nil)
	req.AddCookie(&http.Cookie{
		Name:  loginStateCookie,
		Value: base64.RawURLEncoding.EncodeToString(rawLS),
	})
	rec := httptest.NewRecorder()

	a.handleAuthCallback(rec, req)

	if rec.Code != http.StatusFound {
		t.Fatalf("callback code=%d want 302; body=%s", rec.Code, rec.Body.String())
	}

	var hasSession bool
	for _, c := range rec.Result().Cookies() {
		if c.Name == sessionCookie && c.Value != "" {
			hasSession = true
		}
	}
	if !hasSession {
		t.Fatal("no session cookie set")
	}

	if _, err := db.UserBySubject("sub-1"); err != nil {
		t.Fatalf("user not upserted: %v", err)
	}
}

// TestRoutesGatesOnWhenAuthEnabled exercises the gate-on path through the real
// Routes() handler (not the middleware in isolation): an unauthenticated
// browser navigation is redirected to login, an unauthenticated machine call to
// /publish is 401, and the always-open /healthz stays reachable.
func TestRoutesGatesOnWhenAuthEnabled(t *testing.T) {
	issuer := newMockOIDC(t)
	au, err := auth.New(context.Background(), auth.Config{
		Issuer: issuer, ClientID: "client-1", ClientSecret: "s",
		RedirectURL: "https://app/auth/callback",
	})
	if err != nil {
		t.Fatal(err)
	}
	db, err := storeOpen(t)
	if err != nil {
		t.Fatal(err)
	}
	a := &API{Store: db, Auth: au, Allowlist: auth.NewAllowlist([]string{"sub-1"}, nil), SessionTTL: time.Hour}
	mux := a.Routes()

	// Browser navigation to a gated route without a session → 302 /auth/login.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/posts", nil)
	req.Header.Set("Sec-Fetch-Mode", "navigate")
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/auth/login" {
		t.Fatalf("gated page: code=%d loc=%s want 302 /auth/login", rec.Code, rec.Header().Get("Location"))
	}

	// Machine endpoint without a bearer token → 401.
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/publish", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("gated /publish: code=%d want 401", rec.Code)
	}

	// Always-open health check stays reachable with auth on.
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("/healthz: code=%d want 200", rec.Code)
	}
}

// TestCallbackRejectsStateMismatch verifies that a mismatched state parameter
// produces a 400 without touching the provider at all.
func TestCallbackRejectsStateMismatch(t *testing.T) {
	db, err := storeOpen(t)
	if err != nil {
		t.Fatal(err)
	}
	// Auth nil is safe here: handleAuthCallback validates the state cookie and
	// rejects the mismatch before it ever reaches a.Auth.Exchange.
	a := &API{Store: db, Auth: nil, Allowlist: nil, SessionTTL: time.Hour}

	ls := loginState{State: "good-state", Nonce: "n1", PKCEVerifier: "v"}
	rawLS, _ := json.Marshal(ls)

	// state in query doesn't match ls.State in cookie.
	req := httptest.NewRequest(http.MethodGet, "/auth/callback?state=wrong-state&code=abc", nil)
	req.AddCookie(&http.Cookie{
		Name:  loginStateCookie,
		Value: base64.RawURLEncoding.EncodeToString(rawLS),
	})
	rec := httptest.NewRecorder()

	a.handleAuthCallback(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("state mismatch: code=%d want 400; body=%s", rec.Code, rec.Body.String())
	}
}

// TestCallbackMissingLoginState verifies that a request without the
// login-state cookie produces a 400.
func TestCallbackMissingLoginState(t *testing.T) {
	db, err := storeOpen(t)
	if err != nil {
		t.Fatal(err)
	}
	a := &API{Store: db, Auth: nil, Allowlist: nil, SessionTTL: time.Hour}

	req := httptest.NewRequest(http.MethodGet, "/auth/callback?state=st&code=abc", nil)
	rec := httptest.NewRecorder()

	a.handleAuthCallback(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("missing cookie: code=%d want 400; body=%s", rec.Code, rec.Body.String())
	}
}
