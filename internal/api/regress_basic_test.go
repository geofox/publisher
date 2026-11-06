package api

import "testing"

// README: with OIDC off, put publisher behind "Authelia, oauth2-proxy, Basic
// Auth". nginx auth_basic and Caddy basicauth forward the browser's
// Authorization: Basic header upstream, and /v1 answers every such request
// 401, so the web app (which runs on /v1) cannot load anything.
func TestProxyBasicAuthBreaksV1(t *testing.T) {
	e := newV1Env(t, false)
	r := e.do(req{method: "GET", path: "/v1/me", auth: "none", badRequest: true,
		header: map[string]string{"Authorization": "Basic b3duZXI6cHc="}})
	if r.Code != 200 {
		t.Fatalf("GET /v1/me behind a Basic Auth proxy: %d %s", r.Code, r.Body.String())
	}
}
