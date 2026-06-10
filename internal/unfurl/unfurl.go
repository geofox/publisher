package unfurl

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/geofox/publisher/internal/verify"
)

const (
	userAgent       = "publisher-link-preview/1.0"
	maxHTMLBytes    = 2 << 20  // 2MB of HTML is plenty for <head> metadata
	maxThumbBytes   = 10 << 20 // pre-resize cap; bluesky fitBlob shrinks to ≤1MB
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
		return fmt.Errorf("GET %s returned %d", url, resp.StatusCode)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(out)
}
