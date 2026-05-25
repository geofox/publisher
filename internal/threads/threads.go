package threads

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Image represents a single image to attach to a Threads post.
type Image struct {
	URL string // public URL (Meta fetches it server-side)
	Alt string
}

// Post describes the content of a single Threads post.
type Post struct {
	Text     string
	TopicTag string
	Images   []Image
	// ReplyControl restricts who can reply. Set on the published container; one
	// of the threadsReplyControls values, otherwise omitted (Threads defaults to
	// everyone).
	ReplyControl string
	ReplyToID    string // when set, posts as a reply to this media id (threading)
}

// threadsReplyControls is the set of accepted reply_control values (Threads API).
var threadsReplyControls = map[string]bool{
	"everyone":                true,
	"accounts_you_follow":     true,
	"mentioned_only":          true,
	"parent_post_author_only": true,
	"followers_only":          true,
}

// Result holds the identifiers returned after a successful publish.
type Result struct {
	RemoteID  string
	RemoteURL string
}

// Client is a hand-rolled Threads Graph API client.
// BaseURL, RefreshURL, PollInterval, and PollTimeout are exported for test overrides.
type Client struct {
	BaseURL      string
	RefreshURL   string
	UserID       string
	token        string
	mu           sync.RWMutex
	HTTP         *http.Client
	PollInterval time.Duration
	PollTimeout  time.Duration
}

// New creates a Client with production defaults.
// Pass userID="" to use the literal "me" alias.
func New(token, userID string) *Client {
	if userID == "" {
		userID = "me"
	}
	return &Client{
		BaseURL:      "https://graph.threads.net/v1.0",
		RefreshURL:   "https://graph.threads.net/refresh_access_token",
		UserID:       userID,
		token:        token,
		HTTP:         &http.Client{Timeout: 30 * time.Second},
		PollInterval: 3 * time.Second,
		PollTimeout:  90 * time.Second,
	}
}

// Post creates and publishes a Threads post, returning its remote ID and permalink.
func (c *Client) Post(ctx context.Context, p Post) (Result, error) {
	deadline := time.Now().Add(c.PollTimeout)
	creationID, err := c.createMain(ctx, p, deadline)
	if err != nil {
		return Result{}, err
	}
	if err := c.pollReadyUntil(ctx, creationID, deadline); err != nil {
		return Result{}, err
	}
	mediaID, err := c.publish(ctx, creationID)
	if err != nil {
		return Result{}, err
	}
	link, _ := c.permalink(ctx, mediaID) // best-effort; don't fail the post if this errors
	return Result{RemoteID: mediaID, RemoteURL: link}, nil
}

// createMain builds the appropriate container(s) based on image count.
func (c *Client) createMain(ctx context.Context, p Post, deadline time.Time) (string, error) {
	switch len(p.Images) {
	case 0:
		v := url.Values{"media_type": {"TEXT"}, "text": {p.Text}}
		c.addTopic(v, p.TopicTag)
		c.addReplyControl(v, p.ReplyControl)
		c.addReplyTo(v, p.ReplyToID)
		return c.createContainer(ctx, v)

	case 1:
		v := url.Values{
			"media_type": {"IMAGE"},
			"image_url":  {p.Images[0].URL},
			"text":       {p.Text},
		}
		if p.Images[0].Alt != "" {
			v.Set("alt_text", p.Images[0].Alt)
		}
		c.addTopic(v, p.TopicTag)
		c.addReplyControl(v, p.ReplyControl)
		c.addReplyTo(v, p.ReplyToID)
		return c.createContainer(ctx, v)

	default:
		// Carousel: create + poll each child, then create the parent container.
		children := make([]string, 0, len(p.Images))
		for _, img := range p.Images {
			cv := url.Values{
				"media_type":       {"IMAGE"},
				"image_url":        {img.URL},
				"is_carousel_item": {"true"},
			}
			if img.Alt != "" {
				cv.Set("alt_text", img.Alt)
			}
			id, err := c.createContainer(ctx, cv)
			if err != nil {
				return "", err
			}
			if err := c.pollReadyUntil(ctx, id, deadline); err != nil {
				return "", err
			}
			children = append(children, id)
		}
		pv := url.Values{
			"media_type": {"CAROUSEL"},
			"children":   {strings.Join(children, ",")},
			"text":       {p.Text},
		}
		c.addTopic(pv, p.TopicTag)
		c.addReplyControl(pv, p.ReplyControl)
		c.addReplyTo(pv, p.ReplyToID)
		return c.createContainer(ctx, pv)
	}
}

func (c *Client) addTopic(v url.Values, tag string) {
	if tag != "" {
		v.Set("topic_tag", tag)
	}
}

// addReplyControl sets reply_control on the published container when rc is a
// recognized value; unknown/empty values are omitted so Threads defaults to
// everyone (a typo fails open rather than erroring the post).
func (c *Client) addReplyControl(v url.Values, rc string) {
	if threadsReplyControls[rc] {
		v.Set("reply_control", rc)
	}
}

// addReplyTo sets reply_to_id when id is non-empty, making the container a reply.
func (c *Client) addReplyTo(v url.Values, id string) {
	if id != "" {
		v.Set("reply_to_id", id)
	}
}

// createContainer calls POST /{userID}/threads with the given query parameters
// and returns the creation_id.
func (c *Client) createContainer(ctx context.Context, v url.Values) (string, error) {
	var resp struct {
		ID string `json:"id"`
	}
	if err := c.do(ctx, http.MethodPost, "/"+c.UserID+"/threads?"+v.Encode(), &resp); err != nil {
		return "", fmt.Errorf("create container: %w", err)
	}
	if resp.ID == "" {
		return "", fmt.Errorf("create container: empty id")
	}
	return resp.ID, nil
}

// pollReadyUntil polls GET /{id}?fields=status,error_message until the container
// reaches FINISHED (or fails with ERROR/EXPIRED), respecting the shared deadline.
func (c *Client) pollReadyUntil(ctx context.Context, id string, deadline time.Time) error {
	for {
		var resp struct {
			Status string `json:"status"`
			ErrMsg string `json:"error_message"`
		}
		if err := c.do(ctx, http.MethodGet, "/"+id+"?fields=status,error_message", &resp); err != nil {
			return fmt.Errorf("poll status: %w", err)
		}
		switch resp.Status {
		case "FINISHED", "PUBLISHED":
			return nil
		case "ERROR", "EXPIRED":
			return fmt.Errorf("container %s: %s", resp.Status, resp.ErrMsg)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("container %s not ready by deadline (last status %q)", id, resp.Status)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(c.PollInterval):
		}
	}
}

// publish calls POST /{userID}/threads_publish and returns the media ID.
func (c *Client) publish(ctx context.Context, creationID string) (string, error) {
	v := url.Values{"creation_id": {creationID}}
	var resp struct {
		ID string `json:"id"`
	}
	if err := c.do(ctx, http.MethodPost, "/"+c.UserID+"/threads_publish?"+v.Encode(), &resp); err != nil {
		return "", fmt.Errorf("publish: %w", err)
	}
	if resp.ID == "" {
		return "", fmt.Errorf("publish: empty media id")
	}
	return resp.ID, nil
}

// permalink fetches the public URL of a published post (best-effort).
func (c *Client) permalink(ctx context.Context, mediaID string) (string, error) {
	var resp struct {
		Permalink string `json:"permalink"`
	}
	if err := c.do(ctx, http.MethodGet, "/"+mediaID+"?fields=permalink", &resp); err != nil {
		return "", err
	}
	return resp.Permalink, nil
}

// SetToken atomically swaps the bearer token (used by the refresher).
func (c *Client) SetToken(s string) {
	c.mu.Lock()
	c.token = s
	c.mu.Unlock()
}

func (c *Client) tok() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.token
}

// RefreshToken exchanges the current long-lived token for a fresh ~60-day one.
// The token is a query param here (Threads' documented contract for this
// endpoint) — never log the URL. The manager only calls this on tokens ≥24h
// old, so the "token too new" rejection does not arise.
func (c *Client) RefreshToken(ctx context.Context, current string) (string, time.Duration, error) {
	u, err := url.Parse(c.RefreshURL)
	if err != nil {
		return "", 0, err
	}
	q := u.Query()
	q.Set("grant_type", "th_refresh_token")
	q.Set("access_token", current)
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return "", 0, err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		// A transport error (*url.Error) embeds the full request URL — which
		// carries access_token=<token> in its query. This error is logged and
		// sent in alerts, so scrub the token before it can escape.
		return "", 0, fmt.Errorf("%s", strings.ReplaceAll(err.Error(), current, "REDACTED"))
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		// Threads can echo the access_token (query param) back in error bodies;
		// this error is logged + sent in alerts, so scrub the token first.
		msg := strings.ReplaceAll(strings.TrimSpace(string(body)), current, "REDACTED")
		return "", 0, fmt.Errorf("threads refresh: status %d: %s", resp.StatusCode, msg)
	}
	var out struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int64  `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return "", 0, err
	}
	if out.AccessToken == "" {
		return "", 0, fmt.Errorf("threads refresh: empty access_token")
	}
	if out.ExpiresIn <= 0 {
		return "", 0, fmt.Errorf("threads refresh: invalid expires_in %d", out.ExpiresIn)
	}
	return out.AccessToken, time.Duration(out.ExpiresIn) * time.Second, nil
}

// do executes an authenticated HTTP request and JSON-decodes the response into out.
// The token is placed in the Authorization header, never in the URL.
func (c *Client) do(ctx context.Context, method, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.tok())
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		rb, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return fmt.Errorf("%s returned %d: %s", path, resp.StatusCode, string(rb))
	}
	return json.NewDecoder(resp.Body).Decode(out)
}
