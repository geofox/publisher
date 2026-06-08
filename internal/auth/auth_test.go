package auth

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	jose "github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
)

func TestAllowlist(t *testing.T) {
	a := NewAllowlist([]string{"sub-1"}, []string{"ok@e.com"})
	if err := a.Check(Claims{Subject: "sub-1"}); err != nil {
		t.Fatalf("subject match should pass: %v", err)
	}
	if err := a.Check(Claims{Subject: "x", Email: "ok@e.com"}); err != nil {
		t.Fatalf("email match should pass: %v", err)
	}
	if err := a.Check(Claims{Subject: "x", Email: "no@e.com"}); !errors.Is(err, ErrNotAllowed) {
		t.Fatalf("want ErrNotAllowed, got %v", err)
	}
	empty := NewAllowlist(nil, nil)
	if err := empty.Check(Claims{Subject: "sub-1"}); !errors.Is(err, ErrNotAllowed) {
		t.Fatalf("empty allowlist must fail closed")
	}
}

func newMockProvider(t *testing.T) (issuer string, signer jose.Signer) {
	t.Helper()
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	var srv *httptest.Server
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer":                 srv.URL,
			"authorization_endpoint": srv.URL + "/auth",
			"token_endpoint":         srv.URL + "/token",
			"jwks_uri":               srv.URL + "/jwks",
		})
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{
			{Key: key.Public(), KeyID: "test", Algorithm: "RS256", Use: "sig"},
		}})
	})
	srv = httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	signer, _ = jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: key},
		(&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", "test"))
	return srv.URL, signer
}

func signIDToken(t *testing.T, signer jose.Signer, issuer, aud, sub, nonce string, exp time.Time) string {
	t.Helper()
	claims := map[string]any{
		"iss": issuer, "aud": aud, "sub": sub, "nonce": nonce,
		"exp": exp.Unix(), "iat": time.Now().Unix(), "email": "a@e.com", "name": "A",
	}
	raw, err := jwt.Signed(signer).Claims(claims).Serialize()
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestVerifierAcceptsGoodToken(t *testing.T) {
	issuer, signer := newMockProvider(t)
	provider, err := oidc.NewProvider(context.Background(), issuer)
	if err != nil {
		t.Fatal(err)
	}
	verifier := provider.Verifier(&oidc.Config{ClientID: "client-1"})
	raw := signIDToken(t, signer, issuer, "client-1", "sub-1", "n1", time.Now().Add(time.Hour))
	idt, err := verifier.Verify(context.Background(), raw)
	if err != nil {
		t.Fatalf("good token rejected: %v", err)
	}
	if idt.Nonce != "n1" {
		t.Fatalf("nonce not carried")
	}
}

func TestVerifierRejectsExpiredAndWrongAud(t *testing.T) {
	issuer, signer := newMockProvider(t)
	provider, _ := oidc.NewProvider(context.Background(), issuer)
	verifier := provider.Verifier(&oidc.Config{ClientID: "client-1"})
	expired := signIDToken(t, signer, issuer, "client-1", "sub-1", "n1", time.Now().Add(-time.Hour))
	if _, err := verifier.Verify(context.Background(), expired); err == nil {
		t.Fatal("expired token accepted")
	}
	wrongAud := signIDToken(t, signer, issuer, "other", "sub-1", "n1", time.Now().Add(time.Hour))
	if _, err := verifier.Verify(context.Background(), wrongAud); err == nil {
		t.Fatal("wrong-aud token accepted")
	}
}
