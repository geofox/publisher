package auth

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/geofox/publisher/internal/config"
)

// Auth0 (and others) publish an issuer that ends in "/". config.Load trims
// it, and go-oidc requires the configured issuer to equal the discovered one
// exactly, so the server refuses to start.
func TestIssuerTrailingSlash(t *testing.T) {
	var srv *httptest.Server
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"issuer": srv.URL + "/", "authorization_endpoint": srv.URL + "/auth",
			"token_endpoint": srv.URL + "/token", "jwks_uri": srv.URL + "/jwks"})
	})
	srv = httptest.NewServer(mux)
	defer srv.Close()
	for k, v := range map[string]string{
		"NSEC_HEX":     "0000000000000000000000000000000000000000000000000000000000000001",
		"OWNER_PUBKEY": "79be667ef9dcbbac55a06295ce870b07029bfcdb2dce28d959f2815b16f81798",
		"BLOSSOM_URL":  "https://b.test", "OIDC_ISSUER": srv.URL + "/", "OIDC_CLIENT_ID": "c",
		"OIDC_CLIENT_SECRET": "s", "OIDC_REDIRECT_URL": "https://app/auth/callback", "OIDC_ALLOWED_SUBJECTS": "x",
	} {
		t.Setenv(k, v)
	}
	_ = os.Getenv
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := New(context.Background(), Config{Issuer: cfg.OIDCIssuer, ClientID: "c", ClientSecret: "s",
		RedirectURL: cfg.OIDCRedirectURL}); err != nil {
		t.Fatalf("issuer %q from OIDC_ISSUER=%q: %v", cfg.OIDCIssuer, srv.URL+"/", err)
	}
}
