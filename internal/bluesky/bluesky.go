package bluesky

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/rivo/uniseg"
)

const maxGraphemes = 300

// isoMillis is RFC 3339 with always-three fractional digits. Bluesky's
// getAuthorFeed dedupes records sharing the same (author, createdAt), so
// second-precision timestamps make rapid chain segments collide — only the
// highest-rkey record per second survives, hiding the chain head from the
// "Posts" tab.
const isoMillis = "2006-01-02T15:04:05.000Z07:00"

type Image struct {
	Bytes []byte
	Mime  string
	Alt   string
}

type Post struct {
	Text   string
	Langs  []string
	Images []Image
	// ReplyGate, when non-nil, writes an app.bsky.feed.threadgate restricting who
	// can reply. A nil gate means anyone can reply; an all-false gate (e.g. from
	// "nobody") writes an empty allow list, which means nobody can reply.
	ReplyGate *ReplyGate
	// DisableQuotes writes an app.bsky.feed.postgate#disableRule, blocking
	// quote-posts of this post.
	DisableQuotes bool
	// Reply, when non-nil, makes this post a reply (used for threading).
	Reply *ReplyRef
	Quote *QuoteRef // when set, embeds the quoted post
}

// ReplyRef threads a post into an existing conversation. For a self-thread the
// root is the chain's first post and the parent is the immediately-preceding one.
type ReplyRef struct {
	RootURI, RootCID, ParentURI, ParentCID string
}

// QuoteRef, when set on a Post, embeds a quoted post (app.bsky.embed.record).
type QuoteRef struct{ URI, CID string }

// ReplyGate selects the app.bsky.feed.threadgate allow rules. All-false = nobody.
type ReplyGate struct {
	AllowMention   bool
	AllowFollowing bool
	AllowFollower  bool
}

// ParseReplyGate maps a config string to a threadgate spec:
//   - ""             → nil (anyone can reply; no threadgate written)
//   - "nobody"       → empty gate (nobody can reply)
//   - comma list of {mention, following, follower} → those allow rules
//
// A string with no recognized tokens returns nil so a typo fails open (anyone)
// rather than silently locking the thread; recognized tokens still win even when
// mixed with unrecognized ones.
func ParseReplyGate(s string) *ReplyGate {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	if strings.EqualFold(s, "nobody") {
		return &ReplyGate{}
	}
	g := &ReplyGate{}
	matched := false
	for _, tok := range strings.Split(s, ",") {
		switch strings.ToLower(strings.TrimSpace(tok)) {
		case "mention", "mentioned":
			g.AllowMention, matched = true, true
		case "following":
			g.AllowFollowing, matched = true, true
		case "follower", "followers":
			g.AllowFollower, matched = true, true
		}
	}
	if !matched {
		return nil
	}
	return g
}

// allowRules builds the threadgate allow array. Empty (all-false) → [] = nobody.
func (g *ReplyGate) allowRules() []map[string]any {
	rules := []map[string]any{}
	if g.AllowMention {
		rules = append(rules, map[string]any{"$type": "app.bsky.feed.threadgate#mentionRule"})
	}
	if g.AllowFollowing {
		rules = append(rules, map[string]any{"$type": "app.bsky.feed.threadgate#followingRule"})
	}
	if g.AllowFollower {
		rules = append(rules, map[string]any{"$type": "app.bsky.feed.threadgate#followerRule"})
	}
	return rules
}

type Result struct {
	RemoteID  string // at:// uri
	RemoteURL string // https://bsky.app/...
	CID       string // content hash of the record; needed for later lookups/deletions
}

type Client struct {
	PDS         string
	Identifier  string
	AppPassword string
	HTTP        *http.Client
}

func New(pds, identifier, appPassword string) *Client {
	return &Client{
		PDS: strings.TrimRight(pds, "/"), Identifier: identifier, AppPassword: appPassword,
		HTTP: &http.Client{Timeout: 30 * time.Second},
	}
}

type session struct {
	AccessJwt  string `json:"accessJwt"`
	RefreshJwt string `json:"refreshJwt"`
	Did        string `json:"did"`
	Handle     string `json:"handle"`
}

// buildPostRecord assembles the text/langs/facets/reply portion of an
// app.bsky.feed.post record. The image embed is added by the caller (it needs
// uploaded blobs), keeping this helper pure and network-free.
func buildPostRecord(p Post) map[string]any {
	record := map[string]any{
		"$type":     "app.bsky.feed.post",
		"text":      p.Text,
		"createdAt": time.Now().UTC().Format(isoMillis),
	}
	if len(p.Langs) > 0 {
		record["langs"] = p.Langs
	}
	if f := parseFacets(p.Text); len(f) > 0 {
		record["facets"] = f
	}
	if p.Reply != nil {
		record["reply"] = map[string]any{
			"root":   map[string]any{"uri": p.Reply.RootURI, "cid": p.Reply.RootCID},
			"parent": map[string]any{"uri": p.Reply.ParentURI, "cid": p.Reply.ParentCID},
		}
	}
	if p.Quote != nil {
		record["embed"] = map[string]any{
			"$type":  "app.bsky.embed.record",
			"record": map[string]any{"uri": p.Quote.URI, "cid": p.Quote.CID},
		}
	}
	return record
}

func (c *Client) Post(ctx context.Context, p Post) (Result, error) {
	if uniseg.GraphemeClusterCount(p.Text) > maxGraphemes {
		return Result{}, fmt.Errorf("text exceeds %d graphemes", maxGraphemes)
	}
	s, err := c.createSession(ctx)
	if err != nil {
		return Result{}, err
	}

	var images []map[string]any
	for _, img := range p.Images {
		out, mime, w, h, err := fitBlob(img.Bytes, img.Mime)
		if err != nil {
			return Result{}, err
		}
		blob, err := c.uploadBlob(ctx, s.AccessJwt, out, mime)
		if err != nil {
			return Result{}, err
		}
		entry := map[string]any{"alt": img.Alt, "image": blob}
		if w > 0 && h > 0 {
			entry["aspectRatio"] = map[string]int{"width": w, "height": h}
		}
		images = append(images, entry)
	}

	record := buildPostRecord(p)
	if len(images) > 0 {
		imageEmbed := map[string]any{"$type": "app.bsky.embed.images", "images": images}
		if p.Quote != nil {
			record["embed"] = map[string]any{
				"$type":  "app.bsky.embed.recordWithMedia",
				"record": map[string]any{"$type": "app.bsky.embed.record", "record": map[string]any{"uri": p.Quote.URI, "cid": p.Quote.CID}},
				"media":  imageEmbed,
			}
		} else {
			record["embed"] = imageEmbed
		}
	}

	uri, cid, err := c.createRecord(ctx, s, "app.bsky.feed.post", "", record)
	if err != nil {
		return Result{}, err
	}
	res := Result{RemoteID: uri, RemoteURL: webURL(s.Handle, uri), CID: cid}

	// Reply/quote gates are sidecar records sharing the post's rkey, in the same
	// repo. The post is already published, so on a gate error return the post
	// result alongside the error — the caller records the link and surfaces the
	// gate failure rather than losing the post.
	rkey := rkeyOf(uri)
	if p.ReplyGate != nil {
		tg := map[string]any{
			"$type":     "app.bsky.feed.threadgate",
			"post":      uri,
			"allow":     p.ReplyGate.allowRules(),
			"createdAt": time.Now().UTC().Format(isoMillis),
		}
		if _, _, err := c.createRecord(ctx, s, "app.bsky.feed.threadgate", rkey, tg); err != nil {
			return res, fmt.Errorf("threadgate: %w", err)
		}
	}
	if p.DisableQuotes {
		pg := map[string]any{
			"$type":          "app.bsky.feed.postgate",
			"post":           uri,
			"embeddingRules": []map[string]any{{"$type": "app.bsky.feed.postgate#disableRule"}},
			"createdAt":      time.Now().UTC().Format(isoMillis),
		}
		if _, _, err := c.createRecord(ctx, s, "app.bsky.feed.postgate", rkey, pg); err != nil {
			return res, fmt.Errorf("postgate: %w", err)
		}
	}
	return res, nil
}

// repostRecord builds an app.bsky.feed.repost record for the given subject.
func repostRecord(subjectURI, subjectCID string) map[string]any {
	return map[string]any{
		"$type":     "app.bsky.feed.repost",
		"subject":   map[string]any{"uri": subjectURI, "cid": subjectCID},
		"createdAt": time.Now().UTC().Format(isoMillis),
	}
}

// Repost creates an app.bsky.feed.repost of the subject post.
func (c *Client) Repost(ctx context.Context, subjectURI, subjectCID string) (Result, error) {
	s, err := c.createSession(ctx)
	if err != nil {
		return Result{}, err
	}
	uri, cid, err := c.createRecord(ctx, s, "app.bsky.feed.repost", "", repostRecord(subjectURI, subjectCID))
	if err != nil {
		return Result{}, fmt.Errorf("repost: %w", err)
	}
	return Result{RemoteID: uri, RemoteURL: webURL(s.Handle, uri), CID: cid}, nil
}

func (c *Client) createSession(ctx context.Context) (session, error) {
	body, _ := json.Marshal(map[string]string{"identifier": c.Identifier, "password": c.AppPassword})
	var s session
	if err := c.do(ctx, "/xrpc/com.atproto.server.createSession", "application/json", "", body, &s); err != nil {
		return session{}, fmt.Errorf("createSession: %w", err)
	}
	return s, nil
}

func (c *Client) uploadBlob(ctx context.Context, accessJwt string, data []byte, mime string) (json.RawMessage, error) {
	var resp struct {
		Blob json.RawMessage `json:"blob"`
	}
	if err := c.do(ctx, "/xrpc/com.atproto.repo.uploadBlob", mime, accessJwt, data, &resp); err != nil {
		return nil, fmt.Errorf("uploadBlob: %w", err)
	}
	return resp.Blob, nil
}

func (c *Client) createRecord(ctx context.Context, s session, collection, rkey string, record map[string]any) (uri, cid string, err error) {
	payload := map[string]any{"repo": s.Did, "collection": collection, "record": record}
	if rkey != "" {
		payload["rkey"] = rkey // gates must share the post's rkey
	}
	body, _ := json.Marshal(payload)
	var resp struct {
		URI string `json:"uri"`
		CID string `json:"cid"`
	}
	if err := c.do(ctx, "/xrpc/com.atproto.repo.createRecord", "application/json", s.AccessJwt, body, &resp); err != nil {
		return "", "", fmt.Errorf("createRecord: %w", err)
	}
	return resp.URI, resp.CID, nil
}

// do POSTs body to the PDS path and decodes a JSON response into out. A bearer
// token is attached when accessJwt is non-empty.
func (c *Client) do(ctx context.Context, path, contentType, accessJwt string, body []byte, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.PDS+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", contentType)
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
		return fmt.Errorf("%s returned %d: %s", path, resp.StatusCode, string(rb))
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// rkeyOf returns the record key (last path segment) of an at:// URI.
func rkeyOf(atURI string) string {
	if i := strings.LastIndex(atURI, "/"); i >= 0 {
		return atURI[i+1:]
	}
	return atURI
}

func webURL(handle, atURI string) string {
	return "https://bsky.app/profile/" + handle + "/post/" + rkeyOf(atURI)
}
