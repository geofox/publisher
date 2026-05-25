package mastodon

import (
	"context"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

type SourceStatus struct {
	LocalID          string
	AuthorName       string
	AuthorAcct       string
	TextPlain        string
	Media            []SourceMedia
	CreatedAt        time.Time
	WebURL           string
	Visibility       string // public|unlisted|private|direct
	QuoteCurrentUser string // automatic|manual|denied|unknown|"" (no quote info)
}

type SourceMedia struct{ URL, Alt string }

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
			return nil, fmt.Errorf("the Mastodon token is missing the read:search scope (needed to resolve posts)")
		}
		return nil, fmt.Errorf("search: %w", err)
	}
	if len(search.Statuses) == 0 {
		return nil, fmt.Errorf("post not found or not federated to this instance yet")
	}
	id := search.Statuses[0].ID

	var st struct {
		ID         string    `json:"id"`
		Content    string    `json:"content"`
		Visibility string    `json:"visibility"`
		URL        string    `json:"url"`
		CreatedAt  time.Time `json:"created_at"`
		Account    struct {
			DisplayName string `json:"display_name"`
			Acct        string `json:"acct"`
		} `json:"account"`
		MediaAttachments []struct {
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
		LocalID: st.ID, AuthorName: st.Account.DisplayName, AuthorAcct: st.Account.Acct,
		TextPlain: deHTML(st.Content), CreatedAt: st.CreatedAt, WebURL: st.URL,
		Visibility: st.Visibility, QuoteCurrentUser: st.QuoteApproval.CurrentUser,
	}
	for _, m := range st.MediaAttachments {
		out.Media = append(out.Media, SourceMedia{URL: m.URL, Alt: m.Description})
	}
	return out, nil
}

func deHTML(s string) string {
	s = strings.ReplaceAll(s, "</p><p>", "\n\n")
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
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		rb, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return fmt.Errorf("%s: %s: %s", path, resp.Status, string(rb))
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// Reblog boosts a status. Returns the reblog wrapper (its `reblog` is the original).
func (c *Client) Reblog(ctx context.Context, id string) (Result, error) {
	var st struct {
		ID  string `json:"id"`
		URL string `json:"url"`
	}
	if err := c.postForm(ctx, "/api/v1/statuses/"+id+"/reblog", nil, &st); err != nil {
		return Result{}, fmt.Errorf("reblog: %w", err)
	}
	return Result{RemoteID: st.ID, RemoteURL: st.URL}, nil
}

// QuotePost creates a native quote post (server 4.5+). text is the commentary;
// quotedID is the LOCAL status id to quote.
func (c *Client) QuotePost(ctx context.Context, text, quotedID string) (Result, error) {
	var st struct {
		ID  string `json:"id"`
		URL string `json:"url"`
	}
	form := url.Values{"status": {text}, "quoted_status_id": {quotedID}}
	if err := c.postForm(ctx, "/api/v1/statuses", form, &st); err != nil {
		return Result{}, fmt.Errorf("quote: %w", err)
	}
	return Result{RemoteID: st.ID, RemoteURL: st.URL}, nil
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
		rb, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return fmt.Errorf("%s: %s: %s", path, resp.Status, string(rb))
	}
	return json.NewDecoder(resp.Body).Decode(out)
}
