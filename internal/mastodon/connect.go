package mastodon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/geofox/publisher/internal/destination"
	"github.com/geofox/publisher/internal/redact"
)

// Connecting by signing in at the owner's server (spec §6.2): Publisher
// finds the server's OAuth endpoints (RFC 8414 metadata since Mastodon
// 4.3, the standard paths before), registers itself there once per
// redirect URI, and sends the owner to sign in; the callback's code is
// exchanged for a token, which doesn't expire.

// MethodOAuth connects by signing in at the server.
const MethodOAuth = "oauth"

var _ destination.Connector = Driver{}

// oauthScopes are what the driver uses: the owner's account, search (to
// find a post to reply to), statuses and media. A server asks that what
// a sign-in requests be within what the app registered.
const oauthScopes = "read:accounts read:search read:statuses write:media write:statuses"

// connectTimeout bounds each request of a sign-in.
const connectTimeout = 15 * time.Second

// connectData is what the callback needs from the first step.
type connectData struct {
	Issuer        string `json:"issuer"`
	TokenEndpoint string `json:"token_endpoint"`
	// ClientID and ClientSecret: the app this flow's sign-in is for,
	// kept with the flow (sealed by the host), so a flow started meanwhile
	// that registered another app doesn't break it.
	ClientID     string `json:"client_id"`
	ClientSecret string `json:"client_secret"`
	// PKCE: the server lists S256, so the challenge was sent.
	PKCE bool `json:"pkce,omitempty"`
	// ISS: the server says it adds iss to its answer (RFC 9207), so the
	// answer must carry it.
	ISS bool `json:"iss,omitempty"`
}

// oauthMeta is the server's authorization metadata (RFC 8414).
type oauthMeta struct {
	Issuer                string   `json:"issuer"`
	AuthorizationEndpoint string   `json:"authorization_endpoint"`
	TokenEndpoint         string   `json:"token_endpoint"`
	CodeChallengeMethods  []string `json:"code_challenge_methods_supported"`
	ISS                   bool     `json:"authorization_response_iss_parameter_supported"`
}

func connectFailed(code, format string, a ...any) error {
	return &destination.ConnectError{Code: code, Err: fmt.Errorf(format, a...)}
}

// BeginConnect implements destination.Connector.
func (Driver) BeginConnect(ctx context.Context, req destination.ConnectRequest) (destination.ConnectStart, error) {
	base, err := destination.ConfigString(req.Config, ConfigInstanceURL)
	if err != nil || base == "" {
		return destination.ConnectStart{}, destination.InvalidField(ConfigInstanceURL, "required")
	}
	origin := strings.TrimRight(base, "/")
	if req.Clients == nil {
		return destination.ConnectStart{}, errors.New("mastodon: connect needs a place to keep the app it registers")
	}
	hc := req.Net.Client(connectTimeout)
	meta, err := discover(ctx, hc, origin)
	if err != nil {
		return destination.ConnectStart{}, err
	}
	client, ok, err := req.Clients.Client(ctx, meta.Issuer)
	if err != nil {
		return destination.ConnectStart{}, err
	}
	if !ok || !covers(client.Scopes, oauthScopes) {
		if client, err = register(ctx, hc, origin, req.RedirectURI); err != nil {
			return destination.ConnectStart{}, err
		}
		client.Issuer = meta.Issuer
		if err := req.Clients.SaveClient(ctx, client); err != nil {
			return destination.ConnectStart{}, err
		}
	}
	pkce := slices.Contains(meta.CodeChallengeMethods, "S256")
	q := url.Values{"response_type": {"code"}, "client_id": {client.ClientID}, "redirect_uri": {req.RedirectURI},
		"scope": {oauthScopes}, "state": {req.State}}
	if pkce {
		q.Set("code_challenge", req.CodeChallenge)
		q.Set("code_challenge_method", "S256")
	}
	data, err := json.Marshal(connectData{Issuer: meta.Issuer, TokenEndpoint: meta.TokenEndpoint, ClientID: client.ClientID,
		ClientSecret: client.ClientSecret, PKCE: pkce, ISS: meta.ISS})
	if err != nil {
		return destination.ConnectStart{}, err
	}
	sep := "?"
	if strings.Contains(meta.AuthorizationEndpoint, "?") {
		sep = "&"
	}
	return destination.ConnectStart{AuthorizeURL: meta.AuthorizationEndpoint + sep + q.Encode(), Data: data}, nil
}

// discover reads the server's metadata: its issuer must be the server's
// own origin and every endpoint on it. A server from before 4.3 has none
// (404): the standard paths, without PKCE.
func discover(ctx context.Context, hc *http.Client, origin string) (oauthMeta, error) {
	fallback := oauthMeta{Issuer: origin, AuthorizationEndpoint: origin + "/oauth/authorize", TokenEndpoint: origin + "/oauth/token"}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, origin+"/.well-known/oauth-authorization-server", nil)
	if err != nil {
		return oauthMeta{}, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := hc.Do(req)
	if err != nil {
		if errors.Is(err, destination.ErrBlockedAddress) {
			return oauthMeta{}, destination.ErrBlockedAddress
		}
		return oauthMeta{}, connectFailed(destination.ConnectExchangeFailed, "the server's sign-in metadata: %v", redact.URLError(err))
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return fallback, nil
	case resp.StatusCode != http.StatusOK:
		return oauthMeta{}, connectFailed(destination.ConnectExchangeFailed, "the server's sign-in metadata answered %d", resp.StatusCode)
	}
	var m oauthMeta
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&m); err != nil {
		return oauthMeta{}, connectFailed(destination.ConnectExchangeFailed, "the server's sign-in metadata isn't JSON: %v", err)
	}
	if !strings.EqualFold(strings.TrimRight(m.Issuer, "/"), origin) {
		return oauthMeta{}, connectFailed(destination.ConnectIssuerMismatch, "the metadata names issuer %q, not %s", redact.Scrub(m.Issuer), origin)
	}
	m.Issuer = origin
	for _, e := range []string{m.AuthorizationEndpoint, m.TokenEndpoint} {
		if !onOrigin(e, origin) {
			return oauthMeta{}, connectFailed(destination.ConnectIssuerMismatch, "an endpoint isn't on %s", origin)
		}
	}
	return m, nil
}

// onOrigin: raw is an absolute address on origin (scheme, host and port).
func onOrigin(raw, origin string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.User != nil || u.Host == "" {
		return false
	}
	o, err := url.Parse(origin)
	if err != nil {
		return false
	}
	return strings.EqualFold(u.Scheme, o.Scheme) && strings.EqualFold(u.Host, o.Host)
}

// covers: the scopes have says (space-separated) include each of want's.
func covers(have, want string) bool {
	h := strings.Fields(have)
	for _, s := range strings.Fields(want) {
		if !slices.Contains(h, s) {
			return false
		}
	}
	return true
}

// register registers Publisher at the server for the redirect URI
// (POST /api/v1/apps).
func register(ctx context.Context, hc *http.Client, origin, redirectURI string) (destination.OAuthClient, error) {
	name, site := "Publisher", ""
	if u, err := url.Parse(redirectURI); err == nil && u.Host != "" {
		name, site = "Publisher ("+u.Hostname()+")", u.Scheme+"://"+u.Host
	}
	form := url.Values{"client_name": {name}, "redirect_uris": {redirectURI}, "scopes": {oauthScopes}}
	if site != "" {
		form.Set("website", site)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, origin+"/api/v1/apps", strings.NewReader(form.Encode()))
	if err != nil {
		return destination.OAuthClient{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := hc.Do(req)
	if err != nil {
		if errors.Is(err, destination.ErrBlockedAddress) {
			return destination.OAuthClient{}, destination.ErrBlockedAddress
		}
		return destination.OAuthClient{}, connectFailed(destination.ConnectExchangeFailed, "registering the app: %v", redact.URLError(err))
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return destination.OAuthClient{}, connectFailed(destination.ConnectExchangeFailed, "registering the app answered %d%s",
			resp.StatusCode, oauthErrorWord(resp.Body))
	}
	var app struct {
		ClientID     string `json:"client_id"`
		ClientSecret string `json:"client_secret"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&app); err != nil || app.ClientID == "" || app.ClientSecret == "" {
		return destination.OAuthClient{}, connectFailed(destination.ConnectExchangeFailed, "registering the app: no client in the answer")
	}
	return destination.OAuthClient{ClientID: app.ClientID, ClientSecret: app.ClientSecret, Scopes: oauthScopes}, nil
}

// oauthErrorWord is an OAuth error answer's code (RFC 6749 §5.2), never
// its description: ": invalid_client", or "".
func oauthErrorWord(body io.Reader) string {
	var e struct {
		Error string `json:"error"`
	}
	if json.NewDecoder(io.LimitReader(body, 16<<10)).Decode(&e) != nil || e.Error == "" {
		return ""
	}
	w := e.Error
	if len(w) > 64 {
		w = w[:64]
	}
	for _, c := range w {
		if c != '_' && (c < 'a' || c > 'z') {
			return ""
		}
	}
	return ": " + w
}

// CompleteConnect implements destination.Connector.
func (Driver) CompleteConnect(ctx context.Context, req destination.ConnectComplete) (destination.ConnectResult, error) {
	var d connectData
	if err := json.Unmarshal(req.Data, &d); err != nil || d.Issuer == "" || d.TokenEndpoint == "" || d.ClientID == "" || d.ClientSecret == "" {
		return destination.ConnectResult{}, connectFailed(destination.ConnectExchangeFailed, "the flow's data is unreadable")
	}
	// RFC 9207: an answer's issuer is the server's; one the server says
	// it adds must be there.
	if iss, ok := req.Callback["iss"]; ok {
		if len(iss) != 1 || !strings.EqualFold(strings.TrimRight(iss[0], "/"), d.Issuer) {
			return destination.ConnectResult{}, connectFailed(destination.ConnectIssuerMismatch, "the answer came from another issuer")
		}
	} else if d.ISS {
		return destination.ConnectResult{}, connectFailed(destination.ConnectIssuerMismatch, "the answer lacks the issuer the server promised")
	}
	code := req.Callback.Get("code")
	if code == "" {
		return destination.ConnectResult{}, connectFailed(destination.ConnectExchangeFailed, "the answer has no code")
	}
	form := url.Values{"grant_type": {"authorization_code"}, "code": {code}, "client_id": {d.ClientID},
		"client_secret": {d.ClientSecret}, "redirect_uri": {req.RedirectURI}}
	if d.PKCE {
		form.Set("code_verifier", req.CodeVerifier)
	}
	hr, err := http.NewRequestWithContext(ctx, http.MethodPost, d.TokenEndpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return destination.ConnectResult{}, err
	}
	hr.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	hr.Header.Set("Accept", "application/json")
	resp, err := req.Net.Client(connectTimeout).Do(hr)
	if err != nil {
		return destination.ConnectResult{}, connectFailed(destination.ConnectExchangeFailed, "the token exchange: %v", redact.URLError(err))
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		word := oauthErrorWord(resp.Body)
		if word == ": invalid_client" {
			// The server forgot the app (or it was revoked there): the
			// next sign-in registers again.
			if req.Clients != nil {
				if err := req.Clients.ForgetClient(ctx, d.Issuer, d.ClientID); err != nil {
					return destination.ConnectResult{}, err
				}
			}
		}
		return destination.ConnectResult{}, connectFailed(destination.ConnectExchangeFailed, "the token exchange answered %d%s", resp.StatusCode, word)
	}
	var tok struct {
		AccessToken string `json:"access_token"`
		TokenType   string `json:"token_type"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&tok); err != nil || tok.AccessToken == "" ||
		(tok.TokenType != "" && !strings.EqualFold(tok.TokenType, "bearer")) {
		return destination.ConnectResult{}, connectFailed(destination.ConnectExchangeFailed, "the token exchange answered no bearer token")
	}
	return destination.ConnectResult{Config: map[string]any{ConfigInstanceURL: d.Issuer},
		Secrets: map[string]string{SecretAccessToken: tok.AccessToken}}, nil
}
