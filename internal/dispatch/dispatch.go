package dispatch

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"strconv"
	"strings"

	gonostr "fiatjaf.com/nostr"
	"github.com/geofox/publisher/internal/progress"
	"github.com/geofox/publisher/internal/store"
	"github.com/geofox/publisher/internal/unfurl"
)

type TargetResult struct {
	Platform        string
	Status          string // success|partial|failed
	Error           string
	RemoteID        string
	RemoteURL       string
	CID             string // bluesky content-hash of the created record (for threading the next reply)
	LatencyMS       int
	RequestJSON     string
	ResponseJSON    string
	Relays          []store.RelayState
	SignedEventJSON string
	PendingRelays   []string // nostr fan-out: secondary relays to rebroadcast to async
}

// ReplyRef threads one segment onto the previous in a chain. RootID/RootCID
// identify the chain head; ParentID/ParentCID the immediately-preceding segment.
// IDs are platform-native: at:// URIs (+ cids) for Bluesky, status/media ids for
// Mastodon/Threads, event ids for Nostr (cids unused there).
type ReplyRef struct {
	RootID, RootCID, ParentID, ParentCID string
	AuthorPubkey                         string // nostr: replied-to author hex (external replies)
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
}

// IsVideo reports whether this attachment is a video (drives the adapter
// gates and the bytes-vs-URL split).
func (im Img) IsVideo() bool { return strings.HasPrefix(im.Mime, "video/") }

// Adapters should populate Status ("success"|"failed") and Error in the returned
// TargetResult. The dispatcher defensively normalizes an empty status to "failed"
// (using the returned error for the message) so a misbehaving adapter can't be
// silently counted as success.
type NostrPoster interface {
	PublishText(ctx context.Context, text string, pow *int, imetas []gonostr.Tag, replyTo *ReplyRef) (TargetResult, error)
	RebroadcastToRelay(ctx context.Context, signedEventJSON, relayURL string) (ok bool, message string)
	NostrActor
}
type MastodonPoster interface {
	PostText(ctx context.Context, text string, o Overrides, imgs []Img, replyTo *ReplyRef) (TargetResult, error)
	MastodonActor
}
type BlueskyPoster interface {
	PostBsky(ctx context.Context, text string, o Overrides, imgs []Img, replyTo *ReplyRef) (TargetResult, error)
	BlueskyActor
}
type ThreadsPoster interface {
	PostThreads(ctx context.Context, text string, o Overrides, imgs []Img, replyTo *ReplyRef) (TargetResult, error)
}

// Unfurler builds link cards. Satisfied by *unfurl.Service; an interface so
// dispatch tests can fake it. May be nil — links then post without cards.
type Unfurler interface {
	Unfurl(ctx context.Context, url string) (*unfurl.Card, error)
	Thumb(ctx context.Context, url string) (data []byte, mime string, err error)
}

// Interaction action kinds for runAction.
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

// Actor interfaces add native repost/quote of an external post, embedded into the
// per-platform poster interfaces. Threads has no native repost/quote of an
// external post, so ThreadsPoster gains no actor.
type NostrActor interface {
	Repost(ctx context.Context, eventID, author string, kind int, relayHint string) (TargetResult, error)
	Quote(ctx context.Context, text, eventID, author, relayHint string, imetas []gonostr.Tag) (TargetResult, error)
}
type BlueskyActor interface {
	RepostBsky(ctx context.Context, subjectURI, subjectCID string) (TargetResult, error)
	QuoteBsky(ctx context.Context, text string, o Overrides, imgs []Img, quoteURI, quoteCID string) (TargetResult, error)
}
type MastodonActor interface {
	Reblog(ctx context.Context, statusID string) (TargetResult, error)
	QuoteStatus(ctx context.Context, text, quotedID string, imgs []Img) (TargetResult, error)
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

type Dispatcher struct {
	Nostr    NostrPoster
	Mastodon MastodonPoster
	Bluesky  BlueskyPoster
	Threads  ThreadsPoster
	Store    *store.Store // may be nil in unit tests
	Fetcher  Fetcher
	Unfurler Unfurler     // may be nil; attachLinkCard guards it
	Notify   PostNotifier // may be nil; notify() guards it
	Alerter  Notifier     // may be nil; alertFailure guards it
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
func (d *Dispatcher) alertFailure(ctx context.Context, p *store.Post) {
	if d.Alerter == nil || p == nil {
		return
	}
	if p.Status == "failed" || p.Status == "partial" {
		body := "post " + p.ID + " finished with status " + p.Status + "; auto-retry will attempt recovery"
		if err := d.Alerter.Alert(ctx, "Publisher: post delivery "+p.Status, body); err != nil {
			slog.ErrorContext(ctx, "alertFailure", "err", err)
		}
	}
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

// platformDetail renders the per-platform detail line: a thread counter when the
// outcome has multiple segments, else empty.
func platformDetail(o chainOutcome) string {
	if n := len(o.Segments); n > 1 {
		done := 0
		for _, sg := range o.Segments {
			if sg.Status == "success" {
				done++
			}
		}
		return "thread " + strconv.Itoa(done) + "/" + strconv.Itoa(n)
	}
	return ""
}
