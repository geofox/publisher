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
					httpx.WriteError(w, http.StatusForbidden, "cross-origin request blocked")
					return
				}
			}
		}
		next.ServeHTTP(w, r)
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

// withGates applies session/token gates per path. When auth is disabled it is a
// pass-through, preserving the pre-OIDC behavior (and keeping the existing test
// suite green).
func (a *API) withGates(next http.Handler) http.Handler {
	if !a.authEnabled() {
		return next
	}
	session := a.requireSession(next)
	token := a.requireAPIToken(next)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Clean the path before gate selection: net/http does not normalize
		// r.URL.Path before the handler runs, so a raw "/auth/../api/tokens"
		// would otherwise satisfy the "/auth/" prefix and dodge the session gate.
		p := path.Clean(r.URL.Path)
		switch {
		case p == "/healthz" || p == "/metrics" ||
			strings.HasPrefix(p, "/auth/") ||
			p == "/api/public/feed":
			next.ServeHTTP(w, r) // always-open / separately-gated
		case p == "/publish" || p == "/upload-media":
			token.ServeHTTP(w, r) // machine endpoints
		default:
			session.ServeHTTP(w, r) // SPA + browser /api/*
		}
	})
}
