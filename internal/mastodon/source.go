package mastodon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/geofox/publisher/internal/destination"
	"html"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	gomast "github.com/mattn/go-mastodon"
)

type SourceStatus struct {
	LocalID          string
	AuthorID         string // the author's account id on this instance
	AuthorName       string
	AuthorAcct       string
	TextPlain        string
	Media            []SourceMedia
	CreatedAt        time.Time
	WebURL           string
	Visibility       string // public|unlisted|private|direct
	QuoteCurrentUser string // automatic|manual|denied|unknown|"" (no quote info)
}

// SourceMedia is one attachment of a resolved status. Video is set for
// anything but a still image (video, gifv, audio).
type SourceMedia struct {
	URL, Alt string
	Video    bool
}

// ErrSearchScope: the token can't search, which looking posts up needs.
var ErrSearchScope = errors.New("the Mastodon token is missing the read:search scope (needed to resolve posts)")

// ErrStatusNotFound: the instance's search found no status at the URL.
var ErrStatusNotFound = errors.New("post not found or not federated to this instance yet")

var reTag = regexp.MustCompile(`<[^>]+>`)

// ResolveStatus resolves any instance's post URL to a local status (via search
// resolve) and returns its preview + capability fields.
func (c *Client) ResolveStatus(ctx context.Context, postURL string) (*SourceStatus, error) {
	var search struct {
		Statuses []struct {
			ID string `json:"id"`
		} `json:"statuses"`
	}
	if err := c.getJSON(ctx, "/api/v2/search", url.Values{
		"q": {postURL}, "type": {"statuses"}, "resolve": {"true"}, "limit": {"1"},
	}, &search); err != nil {
		if strings.Contains(err.Error(), "outside the authorized scopes") {
			return nil, ErrSearchScope
		}
		return nil, fmt.Errorf("search: %w", err)
	}
	if len(search.Statuses) == 0 {
		return nil, ErrStatusNotFound
	}
	id := search.Statuses[0].ID

	var st struct {
		ID         string    `json:"id"`
		Content    string    `json:"content"`
		Visibility string    `json:"visibility"`
		URL        string    `json:"url"`
		CreatedAt  time.Time `json:"created_at"`
		Account    struct {
			ID          string `json:"id"`
			DisplayName string `json:"display_name"`
			Acct        string `json:"acct"`
		} `json:"account"`
		MediaAttachments []struct {
			Type        string `json:"type"`
			URL         string `json:"url"`
			Description string `json:"description"`
		} `json:"media_attachments"`
		QuoteApproval struct {
			CurrentUser string `json:"current_user"`
		} `json:"quote_approval"`
	}
	if err := c.getJSON(ctx, "/api/v1/statuses/"+id, nil, &st); err != nil {
		return nil, fmt.Errorf("get status: %w", err)
	}
	out := &SourceStatus{
		LocalID: st.ID, AuthorID: st.Account.ID, AuthorName: st.Account.DisplayName, AuthorAcct: st.Account.Acct,
		TextPlain: deHTML(st.Content), CreatedAt: st.CreatedAt, WebURL: st.URL,
		Visibility: st.Visibility, QuoteCurrentUser: st.QuoteApproval.CurrentUser,
	}
	for _, m := range st.MediaAttachments {
		out.Media = append(out.Media, SourceMedia{URL: m.URL, Alt: m.Description, Video: m.Type != "" && m.Type != "image"})
	}
	return out, nil
}

// reBreak and reParagraph match the HTML Mastodon uses for line and
// paragraph breaks in a status's content.
var (
	reBreak     = regexp.MustCompile(`(?i)<br\s*/?>`)
	reParagraph = regexp.MustCompile(`(?i)</p>\s*<p[^>]*>`)
)

func deHTML(s string) string {
	s = reParagraph.ReplaceAllString(s, "\n\n")
	s = reBreak.ReplaceAllString(s, "\n")
	s = reTag.ReplaceAllString(s, "")
	return strings.TrimSpace(html.UnescapeString(s))
}

// postForm POSTs application/x-www-form-urlencoded and decodes JSON into out.
func (c *Client) postForm(ctx context.Context, path string, form url.Values, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if k, ok := ctx.Value(idemKey{}).(string); ok {
		req.Header.Set("Idempotency-Key", k)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return destination.NewHTTPError(path, resp)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// Reblog boosts a status. Returns the reblog wrapper (its `reblog` is the original).
func (c *Client) Reblog(ctx context.Context, id string) (Result, error) {
	var st struct {
		ID  string `json:"id"`
		URL string `json:"url"`
	}
	if err := ctx.Err(); err != nil { // nothing sent
		return Result{}, fmt.Errorf("reblog: %w", err)
	}
	if err := c.postForm(ctx, "/api/v1/statuses/"+id+"/reblog", nil, &st); err != nil {
		return Result{}, destination.Committing(fmt.Errorf("reblog: %w", err))
	}
	return Result{RemoteID: st.ID, RemoteURL: st.URL}, nil
}

// QuotePost creates a native quote post (server 4.5+). p.Text is the
// commentary, with p's content warning, sensitivity, visibility, language and
// images; quotedID is the LOCAL status id to quote. p.Video and
// p.InReplyToID are ignored.
func (c *Client) QuotePost(ctx context.Context, p Post, quotedID string) (Result, error) {
	form := url.Values{"status": {p.Text}, "quoted_status_id": {quotedID}}
	if p.SpoilerText != "" {
		form.Set("spoiler_text", p.SpoilerText)
	}
	if p.Sensitive {
		form.Set("sensitive", "true")
	}
	if p.Visibility != "" {
		form.Set("visibility", p.Visibility)
	}
	if p.Language != "" {
		form.Set("language", p.Language)
	}
	for _, img := range p.Images {
		att, err := c.c.UploadMediaFromMedia(ctx, &gomast.Media{
			File: bytes.NewReader(img.Bytes), Description: img.Alt,
		})
		if err != nil {
			return Result{}, fmt.Errorf("quote media: %w", err)
		}
		form.Add("media_ids[]", string(att.ID))
	}
	var st struct {
		ID  string `json:"id"`
		URL string `json:"url"`
	}
	if err := ctx.Err(); err != nil { // nothing sent
		return Result{}, fmt.Errorf("quote: %w", err)
	}
	if err := c.postForm(WithIdempotencyKey(ctx, p.IdempotencyKey), "/api/v1/statuses", form, &st); err != nil {
		return Result{}, destination.Committing(fmt.Errorf("quote: %w", err))
	}
	return Result{RemoteID: st.ID, RemoteURL: st.URL}, nil
}

// Profile fetches the authenticated operator's own account (acct, display name,
// avatar) via /api/v1/accounts/verify_credentials. The acct is bare on the local
// instance, so it is normalized to @user@host using the instance base URL.
func (c *Client) Profile(ctx context.Context) (handle, displayName, avatar string, err error) {
	var a struct {
		Username    string `json:"username"`
		Acct        string `json:"acct"`
		DisplayName string `json:"display_name"`
		Avatar      string `json:"avatar"`
	}
	if err := c.getJSON(ctx, "/api/v1/accounts/verify_credentials", nil, &a); err != nil {
		return "", "", "", err
	}
	h := a.Acct
	if h == "" {
		h = a.Username
	}
	if h != "" && !strings.Contains(h, "@") {
		host := strings.TrimPrefix(strings.TrimPrefix(c.baseURL, "https://"), "http://")
		h = "@" + h + "@" + host
	} else if h != "" && !strings.HasPrefix(h, "@") {
		h = "@" + h
	}
	return h, a.DisplayName, a.Avatar, nil
}

// OwnAccountID is the authenticated account's id on its instance.
func (c *Client) OwnAccountID(ctx context.Context) (string, error) {
	var a struct {
		ID string `json:"id"`
	}
	if err := c.getJSON(ctx, "/api/v1/accounts/verify_credentials", nil, &a); err != nil {
		return "", err
	}
	if a.ID == "" {
		return "", errors.New("verify_credentials: no account id")
	}
	return a.ID, nil
}

func (c *Client) getJSON(ctx context.Context, path string, params url.Values, out any) error {
	full := c.baseURL + path
	if len(params) > 0 {
		full += "?" + params.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, full, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return destination.NewHTTPError(path, resp)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}
