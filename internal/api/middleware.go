package api

import (
	"net"
	"net/http"
	"net/url"
	"path"
	"strings"

	"github.com/geofox/publisher/internal/httpx"
)

// contentSecurityPolicy locks the SPA to same-origin code. Every script, style,
// and font is a same-origin asset and no markup uses inline scripts/styles, so
// the policy needs no 'unsafe-inline' — which is what makes it an effective
// stored/reflected-XSS backstop (e.g. a hostile platform remote_url). Images
// additionally allow https/blob/data for Blossom thumbnails and local previews.
const contentSecurityPolicy = "default-src 'self'; " +
	"img-src 'self' https: data: blob:; media-src 'self' https:; style-src 'self'; script-src 'self'; " +
	"font-src 'self'; connect-src 'self'; base-uri 'none'; form-action 'self'; " +
	"frame-ancestors 'none'; object-src 'none'"

// withSecurityHeaders adds defense-in-depth response headers to every reply.
func withSecurityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", contentSecurityPolicy)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		next.ServeHTTP(w, r)
	})
}

// withCSRFGuard rejects a state-changing request whose Origin header is present
// but points at a different host — the signature of a cross-site attack riding
// the owner's authenticated (Authelia) session. Same-origin SPA calls and
// server-to-server callers like n8n (no Origin header) pass through, so CSRF is
// blocked without an app-level token scheme.
//
// The Origin is accepted when it matches the request Host OR the configured
// public origin (AppBaseURL). Behind a reverse proxy r.Host is whatever the
// proxy forwards upstream — often an internal host that never matches the
// browser's public Origin — so comparing against the server-configured public
// origin is what keeps the SPA's own same-origin requests (sign-out included)
// from being wrongly blocked. AppBaseURL comes from env, not the client, so it
// is not spoofable.
func (a *API) withCSRFGuard(next http.Handler) http.Handler {
	allowedHost := ""
	if a.AppBaseURL != "" {
		if u, err := url.Parse(a.AppBaseURL); err == nil {
			allowedHost = hostOnly(u.Host)
		}
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
			// safe methods don't mutate state
		default:
			if origin := r.Header.Get("Origin"); origin != "" {
				u, err := url.Parse(origin)
				okHost := err == nil && (sameHost(u.Host, r.Host) ||
					(allowedHost != "" && hostOnly(u.Host) == allowedHost))
				if !okHost {
					if p := path.Clean(r.URL.Path); p == "/v1" || strings.HasPrefix(p, "/v1/") {
						w.Header().Set("Cache-Control", "no-store")
						problem(w, r, http.StatusForbidden, codeCrossOrigin, "cross-origin request blocked")
						return
					}
					httpx.WriteError(w, http.StatusForbidden, "cross-origin request blocked")
					return
				}
			}
		}
		next.ServeHTTP(w, r)
	})
}

// withHostGuard refuses requests for a Host that isn't this server's, when
// AllowedHosts is set (ALLOWED_HOSTS). With OIDC off the reverse proxy is
// the gate, and a page that rebinds its own name to the server's address
// (127.0.0.1 in the documented compose) would otherwise be same-origin with
// the app: the CSRF guard compares Origin with that same Host. Loopback
// names and the health probes always pass.
func (a *API) withHostGuard(next http.Handler) http.Handler {
	if len(a.AllowedHosts) == 0 {
		return next
	}
	allowed := map[string]bool{"localhost": true, "127.0.0.1": true, "::1": true}
	for _, h := range a.AllowedHosts {
		allowed[strings.ToLower(hostOnly(strings.TrimSpace(h)))] = true
	}
	if a.AppBaseURL != "" {
		if u, err := url.Parse(a.AppBaseURL); err == nil {
			allowed[strings.ToLower(hostOnly(u.Host))] = true
		}
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if allowed[strings.ToLower(hostOnly(r.Host))] || r.URL.Path == "/healthz" || r.URL.Path == "/readyz" {
			next.ServeHTTP(w, r)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		problem(w, r, http.StatusMisdirectedRequest, codeMisdirected, "this server does not serve that host name (see ALLOWED_HOSTS)")
	})
}

// sameHost compares two host[:port] strings by hostname, ignoring the port (the
// request Host often carries no port behind the TLS-terminating proxy while the
// Origin may, or vice-versa). Empty hosts never match.
func sameHost(a, b string) bool {
	ha, hb := hostOnly(a), hostOnly(b)
	return ha != "" && ha == hb
}

func hostOnly(h string) string {
	if host, _, err := net.SplitHostPort(h); err == nil {
		return host
	}
	return h
}

// publicAssets are the web app files a browser fetches without the session
// cookie: Chrome and Edge load the manifest (and the icons it lists) in
// credentials-omitted mode, and the OS fetches the install and touch icons on
// its own, so gating them made installing the app fail outside Safari. They
// hold nothing private. The list is exact on purpose: the rest of the web app
// stays behind the session.
var publicAssets = map[string]bool{
	"/manifest.json": true,
	"/icon-32.png":   true,
	"/icon-180.png":  true,
	"/icon-192.png":  true,
	"/icon-512.png":  true,
}

// signInAssets are the files the sign-in and setup pages load before
// anyone has signed in (local mode). They hold nothing private.
var signInAssets = map[string]bool{
	"/signin.js":   true,
	"/setup.js":    true,
	"/webauthn.js": true,
	"/app.css":     true,
}

// withGates applies session/token gates per path. Behind a proxy (ModeProxy)
// it is a pass-through: the reverse proxy in front is the gate. In every
// other mode the web app needs a session, whatever state the instance is
// in (setup pending, PUBLIC_URL missing).
func (a *API) withGates(next http.Handler) http.Handler {
	if a.mode() == ModeProxy {
		return next
	}
	local := a.mode() == ModeLocal
	session := a.requireSession(next)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Clean the path before gate selection: net/http does not normalize
		// r.URL.Path before the handler runs, so a raw "/auth/../v1/tokens"
		// would otherwise satisfy the "/auth/" prefix and dodge a gate.
		// The mux matches the escaped path ("/media%2Fx" is one segment, so
		// the web app's), so a path opens only when both readings agree.
		p, ep := path.Clean(r.URL.Path), path.Clean(r.URL.EscapedPath())
		open := func(p string) bool {
			return p == "/healthz" || p == "/readyz" || p == "/metrics" ||
				strings.HasPrefix(p, "/auth/") ||
				p == "/v1" || strings.HasPrefix(p, "/v1/") || isOldAPIPath(p) ||
				publicAssets[p] ||
				// Media on this server's disk, like a Blossom server's (the mux
				// sends all of /media/ to serveMedia, never the web app).
				p == "/media" || strings.HasPrefix(p, "/media/")
		}
		signIn := func(p string) bool {
			return p == "/setup" || strings.HasPrefix(p, "/setup/") || signInAssets[p] || strings.HasPrefix(p, "/fonts/")
		}
		switch {
		case open(p) && open(ep):
			next.ServeHTTP(w, r) // always-open / separately-gated (/v1 authenticates per route; old paths answer 410)
		case local && signIn(p) && signIn(ep):
			next.ServeHTTP(w, r) // the sign-in and setup pages and what they load
		default:
			session.ServeHTTP(w, r) // the web app
		}
	})
}
