package feed

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/geofox/publisher/internal/store"
)

// Webhook POSTs a signal-only ping to an external consumer (the homepage build)
// when a feed-eligible post is published. The payload carries no post content;
// the receiver re-fetches GET /api/public/feed. Delivery is best-effort and
// never blocks the caller. An empty URL makes it a no-op.
type Webhook struct {
	URL   string
	Token string
	HTTP  *http.Client
}

func NewWebhook(url, token string) *Webhook {
	return &Webhook{URL: url, Token: token, HTTP: &http.Client{Timeout: 10 * time.Second}}
}

// PostPublished pings the consumer if the post is feed-eligible. It returns
// immediately; delivery (with retries) runs in a background goroutine.
func (w *Webhook) PostPublished(_ context.Context, p *store.Post) {
	if w == nil || w.URL == "" || p == nil || !Eligible(*p) {
		return
	}
	body, err := json.Marshal(map[string]string{
		"event":        "post.published",
		"id":           p.ID,
		"published_at": publishedAt(*p).Format(time.RFC3339),
	})
	if err != nil {
		return
	}
	go w.deliver(body, p.ID)
}

// deliver POSTs the payload with up to 3 attempts and a short linear backoff.
// It uses a detached context so the originating request finishing cannot cancel
// the ping. On exhaustion it logs a warning — a missed ping just leaves the
// homepage stale until the next post, never wrong.
func (w *Webhook) deliver(body []byte, postID string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var lastErr error
	for attempt := 1; attempt <= 3; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, w.URL, bytes.NewReader(body))
		if err != nil {
			lastErr = err
			break
		}
		req.Header.Set("Content-Type", "application/json")
		if w.Token != "" {
			req.Header.Set("Authorization", "Bearer "+w.Token)
		}
		resp, err := w.HTTP.Do(req)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode/100 == 2 {
				return
			}
			lastErr = fmt.Errorf("status %d", resp.StatusCode)
		} else {
			lastErr = err
		}
		if attempt < 3 {
			time.Sleep(time.Duration(attempt) * 500 * time.Millisecond)
		}
	}
	slog.Warn("feed webhook delivery failed", "post_id", postID, "err", lastErr)
}
