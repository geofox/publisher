package bluesky

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// SourcePost is a fetched external post for preview + capability detection.
type SourcePost struct {
	URI, CID          string
	AuthorHandle      string
	AuthorName        string
	Text              string
	Media             []SourceMedia
	CreatedAt         time.Time
	WebURL            string
	ReplyDisabled     bool
	EmbeddingDisabled bool
	ThreadgateReason  string
	NotFoundOrBlocked bool
	ReplyRootURI      string
	ReplyRootCID      string
}

type SourceMedia struct{ URL, Alt string }

// parsePostURL splits a bsky.app post URL into the actor (handle or DID) and rkey.
func parsePostURL(raw string) (actor, rkey string, err error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", "", err
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) != 4 || parts[0] != "profile" || parts[2] != "post" {
		return "", "", fmt.Errorf("not a bluesky post URL: %s", raw)
	}
	return parts[1], parts[3], nil
}

// get performs an authed XRPC GET query and decodes JSON into out (the existing
// do() is POST-only). Mirrors do().
func (c *Client) get(ctx context.Context, path string, params url.Values, accessJwt string, out any) error {
	full := c.PDS + path
	if len(params) > 0 {
		full += "?" + params.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, full, nil)
	if err != nil {
		return err
	}
	if accessJwt != "" {
		req.Header.Set("Authorization", "Bearer "+accessJwt)
	}
	resp, err := c.HTTP.Do(req)
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

// GetPost resolves a bsky.app URL to a SourcePost using an authed session (viewer
// flags are only meaningful when authed).
func (c *Client) GetPost(ctx context.Context, postURL string) (*SourcePost, error) {
	actor, rkey, err := parsePostURL(postURL)
	if err != nil {
		return nil, err
	}
	s, err := c.createSession(ctx)
	if err != nil {
		return nil, err
	}
	did := actor
	if !strings.HasPrefix(actor, "did:") {
		var rh struct {
			Did string `json:"did"`
		}
		if err := c.get(ctx, "/xrpc/com.atproto.identity.resolveHandle",
			url.Values{"handle": {actor}}, s.AccessJwt, &rh); err != nil {
			return nil, fmt.Errorf("resolveHandle: %w", err)
		}
		did = rh.Did
	}
	uri := "at://" + did + "/app.bsky.feed.post/" + rkey

	var out struct {
		Posts []struct {
			URI    string `json:"uri"`
			Cid    string `json:"cid"`
			Author struct {
				Handle      string `json:"handle"`
				DisplayName string `json:"displayName"`
			} `json:"author"`
			Record struct {
				Text      string    `json:"text"`
				CreatedAt time.Time `json:"createdAt"`
				Reply     *struct {
					Root struct {
						URI string `json:"uri"`
						Cid string `json:"cid"`
					} `json:"root"`
				} `json:"reply"`
			} `json:"record"`
			Embed  json.RawMessage `json:"embed"`
			Viewer struct {
				ReplyDisabled     bool `json:"replyDisabled"`
				EmbeddingDisabled bool `json:"embeddingDisabled"`
			} `json:"viewer"`
			Threadgate *struct{} `json:"threadgate"`
		} `json:"posts"`
	}
	if err := c.get(ctx, "/xrpc/app.bsky.feed.getPosts",
		url.Values{"uris": {uri}}, s.AccessJwt, &out); err != nil {
		return nil, fmt.Errorf("getPosts: %w", err)
	}
	if len(out.Posts) == 0 {
		return &SourcePost{URI: uri, NotFoundOrBlocked: true}, nil
	}
	p := out.Posts[0]
	sp := &SourcePost{
		URI:               p.URI,
		CID:               p.Cid,
		AuthorHandle:      p.Author.Handle,
		AuthorName:        p.Author.DisplayName,
		Text:              p.Record.Text,
		CreatedAt:         p.Record.CreatedAt,
		WebURL:            webURL(p.Author.Handle, p.URI),
		ReplyDisabled:     p.Viewer.ReplyDisabled,
		EmbeddingDisabled: p.Viewer.EmbeddingDisabled,
	}
	if p.Threadgate != nil {
		sp.ThreadgateReason = "replies are limited by the author"
	}
	if p.Record.Reply != nil {
		sp.ReplyRootURI = p.Record.Reply.Root.URI
		sp.ReplyRootCID = p.Record.Reply.Root.Cid
	}
	sp.Media = extractImages(p.Embed)
	return sp, nil
}

// extractImages pulls image URLs+alts from the hydrated embed view (best-effort).
func extractImages(embed json.RawMessage) []SourceMedia {
	if len(embed) == 0 {
		return nil
	}
	var e struct {
		Type   string `json:"$type"`
		Images []struct {
			Thumb    string `json:"thumb"`
			Fullsize string `json:"fullsize"`
			Alt      string `json:"alt"`
		} `json:"images"`
	}
	if json.Unmarshal(embed, &e) != nil {
		return nil
	}
	var out []SourceMedia
	for _, im := range e.Images {
		u := im.Fullsize
		if u == "" {
			u = im.Thumb
		}
		if u != "" {
			out = append(out, SourceMedia{URL: u, Alt: im.Alt})
		}
	}
	return out
}
