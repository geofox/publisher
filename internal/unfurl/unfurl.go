package unfurl

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/geofox/publisher/internal/verify"
)

const (
	userAgent       = "publisher-link-preview/1.0"
	maxHTMLBytes    = 2 << 20  // 2MB of HTML is plenty for <head> metadata
	maxThumbBytes   = 10 << 20 // pre-resize cap; bluesky fitBlob shrinks to ≤1MB
	maxJSONBytes    = 1 << 20  // DID docs and getRecord responses are small
	cacheTTL        = 15 * time.Minute
	negativeTTL     = time.Minute
	maxCacheEntries = 16 // entries hold thumb bytes — keep the cache tiny
)

// StrongRef pins one atproto record version (com.atproto.repo.strongRef).
type StrongRef struct {
	URI string `json:"uri"`
	CID string `json:"cid"`
}

// Card is everything needed to build an app.bsky.embed.external embed. It is
// persisted (sans thumb bytes) in the bluesky target's fields_json so
// retry/resume re-attach the exact card the original dispatch computed.
type Card struct {
	URI         string      `json:"uri"`
	Title       string      `json:"title"`
	Description string      `json:"description"`
	ThumbURL    string      `json:"thumb_url,omitempty"`
	Refs        []StrongRef `json:"refs,omitempty"`
	// Segment is the chain ordinal carrying the card (set by
	// dispatch.PlanBlueskyCard; 0 for single posts).
	Segment int `json:"segment"`
	// Thumb bytes are never persisted; resume re-downloads from ThumbURL.
	ThumbData []byte `json:"-"`
	ThumbMime string `json:"-"`
}

// deepCopy returns a Card whose slice fields do not alias the receiver, so
// callers can never mutate a cached entry through a returned pointer.
func (c *Card) deepCopy() *Card {
	if c == nil {
		return nil
	}
	cp := *c
	if len(c.Refs) > 0 {
		cp.Refs = append([]StrongRef(nil), c.Refs...)
	}
	if len(c.ThumbData) > 0 {
		cp.ThumbData = append([]byte(nil), c.ThumbData...)
	}
	return &cp
}

type cacheEntry struct {
	card *Card
	err  error
	exp  time.Time
}

// Service fetches link cards. One instance is shared by dispatch and the
// thread-preview endpoint so a previewed card and the posted card come from
// the same (cached) fetch.
type Service struct {
	HTTP         *http.Client // SSRF-guarded in production
	PLCDirectory string

	mu    sync.Mutex
	cache map[string]cacheEntry
}

// New returns a Service with an SSRF-guarded client: card URLs are operator
// input, but the server must never be steered into internal addresses (nor
// follow a redirect there — the guard validates at dial time, every hop).
func New() *Service {
	return &Service{
		HTTP:         verify.NewSafeClient(10 * time.Second),
		PLCDirectory: "https://plc.directory",
		cache:        map[string]cacheEntry{},
	}
}

// getJSON GETs url and decodes the JSON response (≤1MB) into out.
func (s *Service) getJSON(ctx context.Context, url string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", userAgent)
	resp, err := s.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		rb, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
		return fmt.Errorf("GET %s returned %d: %s", url, resp.StatusCode, string(rb))
	}
	return json.NewDecoder(io.LimitReader(resp.Body, maxJSONBytes)).Decode(out)
}

// Unfurl fetches rawURL and builds its link card: OG/twitter/title metadata,
// resolved site.standard strongRefs, and downloaded thumb bytes. Successes
// and failures are both cached (failures briefly), so composer-preview
// keystrokes can't hammer a slow or broken site. A page with no usable title
// yields an error — no card.
//
// Concurrent calls for the same URL may each fetch independently (no
// singleflight); the last writer wins in the cache. At most one redundant
// fetch per TTL window — acceptable for a single-operator app.
func (s *Service) Unfurl(ctx context.Context, rawURL string) (*Card, error) {
	if c, hit, err := s.cached(rawURL); hit {
		return c, err
	}
	card, err := s.unfurl(ctx, rawURL)
	s.store(rawURL, card, err)
	return card, err
}

func (s *Service) unfurl(ctx context.Context, rawURL string) (*Card, error) {
	u, err := url.Parse(rawURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return nil, fmt.Errorf("not a fetchable url: %q", rawURL)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "text/html")
	resp, err := s.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("GET %s returned %d", rawURL, resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "" && !strings.Contains(ct, "html") {
		return nil, fmt.Errorf("not an html page: %s", ct)
	}
	// Redirects may have moved us: resolve relative og:image against the
	// final URL, but keep the card's URI exactly as the operator wrote it.
	m := parseHTML(io.LimitReader(resp.Body, maxHTMLBytes), resp.Request.URL)
	if m.Title == "" {
		return nil, fmt.Errorf("no usable title at %s", rawURL)
	}
	card := &Card{URI: rawURL, Title: m.Title, Description: m.Description, ThumbURL: m.Image}
	// site.standard refs are an enhancement — failures degrade to a plain card.
	for _, at := range []string{m.DocumentURI, m.PublicationURI} {
		if at == "" {
			continue
		}
		ref, err := s.resolveRef(ctx, at)
		if err != nil {
			slog.Debug("unfurl: site.standard ref skipped", "uri", at, "err", err)
			continue
		}
		card.Refs = append(card.Refs, ref)
	}
	if card.ThumbURL != "" {
		data, mime, err := s.Thumb(ctx, card.ThumbURL)
		if err != nil {
			slog.Debug("unfurl: thumb skipped", "url", card.ThumbURL, "err", err)
			card.ThumbURL = "" // don't advertise a thumb we couldn't fetch
		} else {
			card.ThumbData, card.ThumbMime = data, mime
		}
	}
	return card, nil
}

// Thumb downloads a card thumbnail (content-type image/*, ≤maxThumbBytes).
// Also called by dispatch on resume, when the persisted card has a ThumbURL
// but no bytes (thumb bytes are never persisted).
func (s *Service) Thumb(ctx context.Context, thumbURL string) ([]byte, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, thumbURL, nil)
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("User-Agent", userAgent)
	resp, err := s.HTTP.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, "", fmt.Errorf("GET %s returned %d", thumbURL, resp.StatusCode)
	}
	mime := resp.Header.Get("Content-Type")
	if i := strings.IndexByte(mime, ';'); i >= 0 {
		mime = strings.TrimSpace(mime[:i])
	}
	if !strings.HasPrefix(mime, "image/") {
		return nil, "", fmt.Errorf("thumb is not an image: %s", mime)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxThumbBytes+1))
	if err != nil {
		return nil, "", err
	}
	if len(data) > maxThumbBytes {
		return nil, "", fmt.Errorf("thumb exceeds %d bytes", maxThumbBytes)
	}
	return data, mime, nil
}

func (s *Service) cached(url string) (c *Card, hit bool, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.cache[url]
	if !ok || time.Now().After(e.exp) {
		return nil, false, nil
	}
	return e.card.deepCopy(), true, e.err
}

func (s *Service) store(url string, card *Card, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.cache) >= maxCacheEntries {
		// Entries carry thumb bytes, so the cache stays tiny: drop expired
		// entries, and if still full just reset — a miss only costs a refetch.
		// No LRU: thumb bytes make a priority structure pointless at 16
		// entries; a full reset only costs refetches.
		now := time.Now()
		for k, e := range s.cache {
			if now.After(e.exp) {
				delete(s.cache, k)
			}
		}
		if len(s.cache) >= maxCacheEntries {
			s.cache = map[string]cacheEntry{}
		}
	}
	ttl := cacheTTL
	if err != nil {
		ttl = negativeTTL
	}
	s.cache[url] = cacheEntry{card: card.deepCopy(), err: err, exp: time.Now().Add(ttl)}
}
