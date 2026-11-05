package dispatch

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"strings"
	"time"

	"github.com/geofox/publisher/internal/destination"
	"github.com/geofox/publisher/internal/progress"
	"github.com/geofox/publisher/internal/store"
	"github.com/geofox/publisher/internal/unfurl"
)

// TargetResult is the normalized outcome of publishing one item (one post or
// one thread segment) to one platform, in the shape the store records.
type TargetResult struct {
	Platform string
	Status   string // success|partial|failed
	Error    string
	// ErrorClass is the destination.ErrorClass of a failure ("" on success):
	// it tells the retrier whether repeating the attempt can help.
	ErrorClass string
	// RetryAfter is the wait a rate-limited destination asked for.
	RetryAfter   time.Duration
	RemoteID     string
	RemoteURL    string
	RefData      json.RawMessage // destination.ItemRef.Data (Bluesky CID, Nostr signed event)
	LatencyMS    int
	RequestJSON  string
	ResponseJSON string
	Relays       []store.RelayState
	// SignedEventJSON is RefData for destinations that redeliver (Nostr):
	// what the fan-out and relay retry re-send.
	SignedEventJSON string
	PendingRelays   []string // secondary endpoints to deliver to asynchronously
}

type Overrides struct {
	Text           string   `json:"text"`
	SpoilerText    string   `json:"spoiler_text"`
	Sensitive      bool     `json:"sensitive"`
	Visibility     string   `json:"visibility"`
	Language       string   `json:"language"`
	Langs          []string `json:"langs"`
	POW            *int     `json:"pow"`
	ContentWarning string   `json:"content_warning"`
	TopicTag       string   `json:"topic_tag"`
	// ThreadsReplyControl restricts Threads replies: "" (everyone) | everyone |
	// accounts_you_follow | mentioned_only | parent_post_author_only |
	// followers_only.
	ThreadsReplyControl string `json:"threads_reply_control"`
	// Bluesky-only interaction gating. BlueskyReply: "" (anyone) | "nobody" |
	// comma list of {mention,following,follower}. BlueskyDisableQuotes blocks
	// quote-posts (postgate).
	BlueskyReply         string `json:"bluesky_reply"`
	BlueskyDisableQuotes bool   `json:"bluesky_disable_quotes"`
	// LinkCard, when non-nil on the bluesky override, attaches an
	// app.bsky.embed.external card. It is computed by the dispatcher (never
	// user input) and persisted via fields_json exactly as attached, so
	// retry/resume re-attach the same card. Only the chain segment matching
	// LinkCard.Segment carries it.
	LinkCard *unfurl.Card `json:"link_card,omitempty"`
}

type Img struct {
	Bytes        []byte
	Mime         string
	Alt          string
	BlossomURL   string
	Dim          string // "WxH" of the canonical object ("" if unknown)
	DurationSecs int64  // video only; 0 for images
	SizeBytes    int64  // canonical object size; 0 if unknown
	SHA256       string // canonical object hash (NIP-92 x)
	Blurhash     string
	PosterURL    string // video poster frame
}

// IsVideo reports whether this attachment is a video.
func (im Img) IsVideo() bool { return strings.HasPrefix(im.Mime, "video/") }

// Unfurler builds link cards. Satisfied by *unfurl.Service; an interface so
// dispatch tests can fake it. May be nil — links then post without cards.
type Unfurler interface {
	Unfurl(ctx context.Context, url string) (*unfurl.Card, error)
	Thumb(ctx context.Context, url string) (data []byte, mime string, err error)
}

// Interaction action kinds.
const (
	actionReply  = "reply"
	actionRepost = "repost"
	actionQuote  = "quote"
)

// InteractRef is the platform-native identity of the source post (mirrors
// resolve.PlatformRef; json tags MATCH it so the UI can pass /api/resolve's `ref`
// straight back). reply_root_* are bluesky-only (thread root for replies).
type InteractRef struct {
	URI          string   `json:"uri,omitempty"`
	CID          string   `json:"cid,omitempty"`
	ReplyRootURI string   `json:"reply_root_uri,omitempty"`
	ReplyRootCID string   `json:"reply_root_cid,omitempty"`
	LocalID      string   `json:"local_id,omitempty"`
	EventID      string   `json:"event_id,omitempty"`
	Author       string   `json:"author,omitempty"`
	RelayHints   []string `json:"relay_hints,omitempty"`
	Kind         int      `json:"kind,omitempty"`
}

type PostSpec struct {
	MasterText   string
	Platforms    []string
	DelaySeconds int
	Source       string
	Overrides    map[string]Overrides // keyed by platform
	Images       []Img
	ImgParts     []int         // per-image part anchor, parallel to Images; nil ⇒ all front-load
	MediaRecords []store.Media // already uploaded to Blossom, for archival
	Number       bool          // append k/n counters to threaded segments
}

type Fetcher interface {
	Fetch(ctx context.Context, url string) (data []byte, mime string, err error)
}

// PostNotifier is told when a post reaches a terminal publish state, so an
// implementation (feed.Webhook) can ping an external consumer. Implementations
// must be non-blocking and best-effort — the dispatcher fires this on the hot
// publish path and does not wait for or check the result.
type PostNotifier interface {
	PostPublished(ctx context.Context, p *store.Post)
}

// Dispatcher publishes posts to destination accounts and records the outcome.
type Dispatcher struct {
	// Accounts are the connected destination accounts, keyed by the name a
	// post targets. Until accounts are stored (step 2, PR 2) the name is the
	// destination type.
	Accounts map[string]destination.Account
	// Host re-hosts re-encoded media for URL-transport destinations. Nil
	// keeps canonical URLs.
	Host     MediaHost
	Store    *store.Store // may be nil in unit tests
	Fetcher  Fetcher
	Unfurler Unfurler     // may be nil; attachLinkCard guards it
	Notify   PostNotifier // may be nil; notify() guards it
	Alerter  Notifier     // may be nil; alertFailure guards it

	// Policy decides automatic retries of failed items.
	Policy RetryPolicy
	// Workers is how many items the background engine sends at once
	// (default 4); ScheduleGrace is how late a scheduled post may still go
	// out (0: any lateness); PollInterval is how often an idle worker looks
	// for due items (default 2s). Clock replaces time.Now in tests.
	Workers       int
	ScheduleGrace time.Duration
	PollInterval  time.Duration
	Clock         func() time.Time

	eng engine
}

// enqueueFanout records async relay deliveries for a freshly-published Nostr
// event (no-op without a store, an empty event, no pending relays, or a
// failed primary publish — a failed delivery must not fan out).
func (d *Dispatcher) enqueueFanout(ctx context.Context, postID, status, signedEventJSON string, relays []string) {
	if d.Store == nil || status == "failed" || signedEventJSON == "" || len(relays) == 0 {
		return
	}
	if err := d.Store.EnqueueFanout(postID, signedEventJSON, relays); err != nil {
		slog.WarnContext(ctx, "enqueue fan-out failed", "post_id", postID, "err", err)
	}
}

// notify fires the PostNotifier when configured. Safe with a nil notifier or
// nil post so call sites stay one-liners.
func (d *Dispatcher) notify(ctx context.Context, p *store.Post) {
	if d.Notify != nil && p != nil {
		d.Notify.PostPublished(ctx, p)
	}
}

// alertFailure fires an operational alert when a freshly recorded post did not
// fully succeed. No-op when no alerter is wired or the post fully succeeded.
func (d *Dispatcher) alertFailure(ctx context.Context, p *store.Post) bool {
	if d.Alerter == nil || p == nil || (p.Status != "failed" && p.Status != "partial") {
		return false
	}
	var gaveUp []string
	for _, t := range p.Targets {
		if t.GaveUpAt != nil {
			gaveUp = append(gaveUp, d.displayName(t.Platform))
		}
	}
	const why = ": the failure would repeat (auth, invalid, rejected) " +
		"or the post may already be live (unknown_outcome). Check the target, fix the cause, then retry manually."
	body := "post " + p.ID + " finished with status " + p.Status + "; "
	switch {
	case !d.anyAutoRetryable(ctx, p):
		body += "it will not be retried automatically" + why
	case len(gaveUp) > 0:
		body += "auto-retry will attempt recovery, but not on " + strings.Join(gaveUp, ", ") + why
	default:
		body += "auto-retry will attempt recovery"
	}
	if err := d.Alerter.Alert(ctx, "Publisher: post delivery "+p.Status, body); err != nil {
		slog.ErrorContext(ctx, "alertFailure", "err", err)
	}
	return true
}

// anyAutoRetryable reports whether the engine will try any of the post's
// unfinished targets again: an item has a retry time and its target hasn't
// given up.
func (d *Dispatcher) anyAutoRetryable(_ context.Context, p *store.Post) bool {
	for _, t := range p.Targets {
		if (t.Status != "failed" && t.Status != "partial") || t.GaveUpAt != nil {
			continue
		}
		for _, it := range t.Items {
			if (it.Status == store.ItemFailed || it.Status == store.ItemUnknown) && it.NextAttemptAt != nil {
				return true
			}
		}
	}
	return false
}

func newID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// NewID mints a new unique post ID. Exported so the API layer can pre-mint an
// ID before launching a detached dispatch goroutine.
func NewID() string { return newID() }

func dedupStrings(in []string) []string {
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

// mapStatus maps a chainOutcome/target status to a progress platform status.
// They share the same vocabulary ("success"/"failed"/"partial"); an empty or
// unknown status is treated as failed.
func mapStatus(s string) string {
	switch s {
	case "success":
		return progress.StatusSuccess
	case "partial":
		return progress.StatusPartial
	default:
		return progress.StatusFailed
	}
}
