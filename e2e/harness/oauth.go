package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/geofox/publisher/internal/destination"
)

// fakeSignIn is the networks' side of signing in, for connect.spec.ts: a
// Mastodon server at any https://<name>.e2e.invalid (4.3 and later;
// a name starting "old-" is from before 4.3, without metadata or PKCE),
// and Meta's Graph API at graph.e2e.invalid. The real drivers reach it
// over TLS (a certificate for *.e2e.invalid the harness makes and
// trusts). The browser's side, the sign-in pages, is the spec's own:
// Playwright answers https://*.e2e.invalid/oauth/authorize by sending the
// browser back to the callback with a code.
//
// A Mastodon code is the token it is exchanged for, "<user>:<anything>"
// (the fake accounts' identity); a code starting "bad" is refused. A
// Graph code becomes "<code>~short", then the long-lived "<code>".
type fakeSignIn struct {
	srv   *httptest.Server
	roots *x509.CertPool
	mu    sync.Mutex
	log   []signInCall
	apps  int
}

// signInCall is one request the fake received.
type signInCall struct {
	Host   string     `json:"host"`
	Path   string     `json:"path"`
	Values url.Values `json:"values"` // the form, or the query
}

func newFakeSignIn() (*fakeSignIn, error) {
	cert, err := e2eCertificate()
	if err != nil {
		return nil, err
	}
	f := &fakeSignIn{roots: x509.NewCertPool()}
	f.roots.AddCert(cert.Leaf)
	f.srv = httptest.NewUnstartedServer(http.HandlerFunc(f.serve))
	f.srv.TLS = &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
	f.srv.StartTLS()
	return f, nil
}

// e2eCertificate is a self-signed certificate for *.e2e.invalid.
func e2eCertificate() (tls.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, err
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "e2e.invalid"},
		DNSNames:              []string{"*.e2e.invalid"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(7 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, err
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		return tls.Certificate{}, err
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}, nil
}

// connectNet is how the harness's connect flows reach the network: every
// *.e2e.invalid name on port 443 is the fake; anything else is refused.
func (f *fakeSignIn) connectNet(string) destination.Net {
	addr := f.srv.Listener.Addr().String()
	d := &net.Dialer{Timeout: 5 * time.Second}
	return destination.Net{
		DialContext: func(ctx context.Context, network, a string) (net.Conn, error) {
			host, port, err := net.SplitHostPort(a)
			if err != nil || port != "443" || !strings.HasSuffix(host, ".e2e.invalid") {
				return nil, fmt.Errorf("e2e: %s isn't a fake sign-in server: %w", a, destination.ErrBlockedAddress)
			}
			return d.DialContext(ctx, network, addr)
		},
		CheckRedirect:   destination.SafeRedirect,
		TLSClientConfig: &tls.Config{RootCAs: f.roots, MinVersion: tls.VersionTLS12},
	}
}

func (f *fakeSignIn) record(r *http.Request, v url.Values) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.log = append(f.log, signInCall{Host: hostOnly(r.Host), Path: r.URL.Path, Values: v})
}

func hostOnly(h string) string {
	if x, _, err := net.SplitHostPort(h); err == nil {
		return x
	}
	return h
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (f *fakeSignIn) serve(w http.ResponseWriter, r *http.Request) {
	host := hostOnly(r.Host)
	origin := "https://" + host
	_ = r.ParseForm()
	switch {
	case host == "graph.e2e.invalid" && r.Method == http.MethodPost && r.URL.Path == "/oauth/access_token":
		f.record(r, r.PostForm)
		code := r.PostForm.Get("code")
		if strings.HasPrefix(code, "bad") || r.PostForm.Get("grant_type") != "authorization_code" {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": map[string]any{"type": "OAuthException", "code": 100,
				"message": "Invalid code " + code}})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"access_token": code + "~short", "user_id": 17841400000, "token_type": "bearer"})
	case host == "graph.e2e.invalid" && r.Method == http.MethodGet && r.URL.Path == "/access_token":
		q := r.URL.Query()
		f.record(r, q)
		short := q.Get("access_token")
		if q.Get("grant_type") != "th_exchange_token" || !strings.HasSuffix(short, "~short") || q.Get("client_secret") == "" {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": map[string]any{"type": "OAuthException", "code": 190}})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"access_token": strings.TrimSuffix(short, "~short"), "token_type": "bearer",
			"expires_in": 5184000})
	case r.Method == http.MethodGet && r.URL.Path == "/.well-known/oauth-authorization-server":
		if strings.HasPrefix(host, "old-") {
			http.NotFound(w, r)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"issuer": origin + "/", "authorization_endpoint": origin + "/oauth/authorize",
			"token_endpoint": origin + "/oauth/token", "code_challenge_methods_supported": []string{"S256"},
			"scopes_supported": []string{"read", "write", "read:accounts", "read:search", "read:statuses", "write:media", "write:statuses"}})
	case r.Method == http.MethodPost && r.URL.Path == "/api/v1/apps":
		f.record(r, r.PostForm)
		f.mu.Lock()
		f.apps++
		n := f.apps
		f.mu.Unlock()
		writeJSON(w, http.StatusOK, map[string]any{"id": fmt.Sprint(n), "client_id": fmt.Sprintf("cid-%d", n),
			"client_secret": fmt.Sprintf("csec-%d", n), "redirect_uri": r.PostForm.Get("redirect_uris")})
	case r.Method == http.MethodPost && r.URL.Path == "/oauth/token":
		f.record(r, r.PostForm)
		code := r.PostForm.Get("code")
		// 4.3 and later need the verifier; a server from before ignores it.
		pkce := !strings.HasPrefix(host, "old-")
		if strings.HasPrefix(code, "bad") || (pkce && r.PostForm.Get("code_verifier") == "") || r.PostForm.Get("client_secret") == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_grant", "error_description": "no " + code})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"access_token": code, "token_type": "Bearer", "scope": r.PostForm.Get("scope"),
			"created_at": time.Now().Unix()})
	default:
		http.NotFound(w, r)
	}
}

// calls is every request so far, oldest first.
func (f *fakeSignIn) calls() []signInCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]signInCall(nil), f.log...)
}
