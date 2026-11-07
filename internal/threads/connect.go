package threads

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/geofox/publisher/internal/destination"
	"github.com/geofox/publisher/internal/redact"
)

// Connecting by signing in at Threads (spec §6.3) with the server's Meta
// app: the owner allows the app at threads.com, which sends them back
// with a code; the code becomes a short-lived token (an hour), and that a
// long-lived one (60 days), which the refresher then renews. Meta's
// documentation: get started → access tokens and permissions, long-lived
// tokens (verified 2026-11-07, the plan's E1).

var _ destination.Connector = Driver{}

// The sign-in's addresses, as Meta documents them. Tests and the e2e
// harness point them at fakes before connecting.
var (
	AuthorizeURL = "https://threads.com/oauth/authorize"
	TokenURL     = "https://graph.threads.com/oauth/access_token"
	ExchangeURL  = "https://graph.threads.com/access_token"
	// DebugURL tells when Meta's grant ends (Client.GrantEnd).
	DebugURL = "https://graph.threads.com/v1.0/debug_token"
)

// oauthScopes: the profile, posting, and replying (reply_to_id needs
// threads_manage_replies). Meta separates them with commas.
const oauthScopes = "threads_basic,threads_content_publish,threads_manage_replies"

// connectTimeout bounds each request of a sign-in.
const connectTimeout = 15 * time.Second

// connectData is what the callback needs from the first step: the app
// the owner allowed.
type connectData struct {
	AppID string `json:"app_id"`
}

func connectFailed(format string, a ...any) error {
	return &destination.ConnectError{Code: destination.ConnectExchangeFailed, Err: fmt.Errorf(format, a...)}
}

// app is the server's Meta app, from the instance settings.
func app(req destination.ConnectRequest) (id, secret string, ok bool) {
	id, _ = req.InstanceConfig[InstanceAppID].(string)
	secret = req.InstanceSecrets[InstanceAppSecret]
	return id, secret, id != "" && secret != ""
}

// BeginConnect implements destination.Connector.
func (Driver) BeginConnect(_ context.Context, req destination.ConnectRequest) (destination.ConnectStart, error) {
	id, _, ok := app(req)
	if !ok {
		return destination.ConnectStart{}, &destination.FieldError{Key: "method", Code: "not_allowed",
			Detail: "set the server's Threads app first (Settings, Integrations)"}
	}
	if !metaRedirect(req.RedirectURI) {
		return destination.ConnectStart{}, &destination.FieldError{Key: "method", Code: "not_allowed",
			Detail: "Meta sends you back only to an https address other than localhost: set PUBLIC_URL to Publisher's https address"}
	}
	q := url.Values{"client_id": {id}, "redirect_uri": {req.RedirectURI}, "scope": {oauthScopes},
		"response_type": {"code"}, "state": {req.State}}
	data, err := json.Marshal(connectData{AppID: id})
	if err != nil {
		return destination.ConnectStart{}, err
	}
	return destination.ConnectStart{AuthorizeURL: AuthorizeURL + "?" + q.Encode(), Data: data}, nil
}

// metaRedirect: Meta takes the redirect URI (an https address, never
// localhost or a loopback address).
func metaRedirect(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil {
		return false
	}
	h := strings.ToLower(strings.TrimSuffix(u.Hostname(), "."))
	if h == "localhost" || strings.HasSuffix(h, ".localhost") {
		return false
	}
	ip := net.ParseIP(h)
	return ip == nil || !ip.IsLoopback()
}

// CompleteConnect implements destination.Connector.
func (Driver) CompleteConnect(ctx context.Context, req destination.ConnectComplete) (destination.ConnectResult, error) {
	var d connectData
	if err := json.Unmarshal(req.Data, &d); err != nil || d.AppID == "" {
		return destination.ConnectResult{}, connectFailed("the flow's data is unreadable")
	}
	id, secret, ok := app(req.ConnectRequest)
	if !ok || id != d.AppID {
		return destination.ConnectResult{}, connectFailed("the server's Threads app changed meanwhile")
	}
	// Meta may end the redirect with "#_"; a browser keeps a fragment to
	// itself, but a pasted address may not.
	code := strings.TrimSuffix(req.Callback.Get("code"), "#_")
	if code == "" {
		return destination.ConnectResult{}, connectFailed("the answer has no code")
	}
	hc := req.Net.Client(connectTimeout)
	short, err := exchangeCode(ctx, hc, id, secret, code, req.RedirectURI)
	if err != nil {
		return destination.ConnectResult{}, err
	}
	long, ttl, err := longLived(ctx, hc, secret, short)
	if err != nil {
		return destination.ConnectResult{}, err
	}
	// The refresher takes over from here: a day from now at the soonest,
	// within 10 days of the expiry.
	now := time.Now().UTC()
	return destination.ConnectResult{
		Config: map[string]any{
			ConfigExpiresAt:   now.Add(ttl).Format(time.RFC3339Nano),
			ConfigRefreshedAt: now.Format(time.RFC3339Nano),
		},
		Secrets: map[string]string{SecretAccessToken: long},
	}, nil
}

// exchangeCode turns the code into a short-lived token (POST, the secret
// in the body).
func exchangeCode(ctx context.Context, hc *http.Client, id, secret, code, redirect string) (string, error) {
	form := url.Values{"client_id": {id}, "client_secret": {secret}, "code": {code},
		"grant_type": {"authorization_code"}, "redirect_uri": {redirect}}
	hr, err := http.NewRequestWithContext(ctx, http.MethodPost, TokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	hr.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	hr.Header.Set("Accept", "application/json")
	var out struct {
		AccessToken string `json:"access_token"`
	}
	if err := graphJSON(hc, hr, "the code exchange", &out, secret, code); err != nil {
		return "", err
	}
	if out.AccessToken == "" {
		return "", connectFailed("the code exchange answered no token")
	}
	return out.AccessToken, nil
}

// longLived exchanges the short-lived token for a long-lived one. Meta
// takes the app secret and the token in the query string here: neither
// reaches a log or an error.
func longLived(ctx context.Context, hc *http.Client, secret, short string) (string, time.Duration, error) {
	u, err := url.Parse(ExchangeURL)
	if err != nil {
		return "", 0, err
	}
	q := u.Query()
	q.Set("grant_type", "th_exchange_token")
	q.Set("client_secret", secret)
	q.Set("access_token", short)
	u.RawQuery = q.Encode()
	hr, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return "", 0, err
	}
	hr.Header.Set("Accept", "application/json")
	var out struct {
		AccessToken string `json:"access_token"`
		TokenType   string `json:"token_type"`
		ExpiresIn   int64  `json:"expires_in"`
	}
	if err := graphJSON(hc, hr, "the long-lived token exchange", &out, secret, short); err != nil {
		return "", 0, err
	}
	if out.AccessToken == "" || (out.TokenType != "" && !strings.EqualFold(out.TokenType, "bearer")) {
		return "", 0, connectFailed("the long-lived token exchange answered no bearer token")
	}
	if out.ExpiresIn <= 0 {
		return "", 0, connectFailed("the long-lived token exchange answered no lifetime")
	}
	return out.AccessToken, time.Duration(out.ExpiresIn) * time.Second, nil
}

// graphJSON sends a sign-in request and decodes its answer. A failure
// names the step, the status and the Graph error's type and code, never
// the address (it may hold secrets) or Meta's words.
func graphJSON(hc *http.Client, hr *http.Request, step string, out any, secrets ...string) error {
	resp, err := hc.Do(hr)
	if err != nil {
		if errors.Is(err, destination.ErrBlockedAddress) {
			return connectFailed("%s: the address is refused", step)
		}
		msg := redact.URLError(err).Error()
		for _, s := range secrets {
			msg = scrubToken(msg, s)
		}
		return connectFailed("%s: %s", step, msg)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode != http.StatusOK {
		return connectFailed("%s answered %d%s", step, resp.StatusCode, graphErrorWords(body))
	}
	if err := json.Unmarshal(body, out); err != nil {
		return connectFailed("%s answered something other than JSON", step)
	}
	return nil
}

// graphErrorWords is a Graph error's type and code (" (OAuthException
// 190)"), never its message, which may quote what was sent.
func graphErrorWords(body []byte) string {
	var g struct {
		Error struct {
			Type string `json:"type"`
			Code int    `json:"code"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &g) != nil || (g.Error.Type == "" && g.Error.Code == 0) {
		return ""
	}
	typ := g.Error.Type
	if len(typ) > 40 {
		typ = typ[:40]
	}
	for _, c := range typ {
		if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') {
			typ = ""
			break
		}
	}
	parts := []string{}
	if typ != "" {
		parts = append(parts, typ)
	}
	if g.Error.Code != 0 {
		parts = append(parts, strconv.Itoa(g.Error.Code))
	}
	if len(parts) == 0 {
		return ""
	}
	return " (" + strings.Join(parts, " ") + ")"
}

// GrantEnd asks when Meta's grant to the app ends (data_access_expires_at
// of debug_token: 90 days after the owner allowed it, moved on for a
// public profile by each renewal). Zero: never, or Meta didn't say. Meta
// takes the token in the query string here; it reaches no log or error.
func (c *Client) GrantEnd(ctx context.Context) (time.Time, error) {
	u, err := url.Parse(c.DebugURL)
	if err != nil {
		return time.Time{}, err
	}
	tok := c.tok()
	q := u.Query()
	q.Set("input_token", tok)
	q.Set("access_token", tok) // the owner is a tester of their own app
	u.RawQuery = q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return time.Time{}, err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return time.Time{}, fmt.Errorf("threads debug_token: %s", scrubToken(redact.URLError(err).Error(), tok))
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode != http.StatusOK {
		return time.Time{}, fmt.Errorf("threads debug_token: status %d%s", resp.StatusCode, graphErrorWords(body))
	}
	var out struct {
		Data struct {
			DataAccessExpiresAt int64 `json:"data_access_expires_at"`
			IsValid             bool  `json:"is_valid"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return time.Time{}, errors.New("threads debug_token: not JSON")
	}
	if !out.Data.IsValid || out.Data.DataAccessExpiresAt <= 0 {
		return time.Time{}, nil
	}
	return time.Unix(out.Data.DataAccessExpiresAt, 0).UTC(), nil
}
