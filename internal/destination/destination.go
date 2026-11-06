// Package destination is the contract between Publisher's core and the
// networks it publishes to (Bluesky, Mastodon, Nostr, Threads, and later
// plug-ins).
//
// A destination type is described by a Manifest: what it can publish, how it
// counts text, what media it takes, which options it understands. An Account
// is one connected identity of a type; the core asks it for its effective
// Capabilities and hands it one item at a time to Publish. The core, not the
// driver, runs threads (item k+1 replies to item k through ThreadRef), retries,
// scheduling, media fitting and persistence, so a driver stays a "publish one
// thing" adapter.
//
// The package has no dependency on the rest of Publisher, and every type that
// crosses the boundary is JSON-serializable, so the same contract can later be
// spoken by an out-of-process plug-in. ContractVersion changes when a change
// would break an existing driver; adding optional fields does not.
//
// Design: docs/superpowers/specs/2026-11-04-destinations-and-v1-api-design.md.
package destination

import (
	"context"
	"encoding/json"
	"errors"
)

// ContractVersion is the version of this contract. Manifests record the
// version their driver was written against.
const ContractVersion = 2

// Kind is what a published item is.
type Kind string

const (
	// KindNote is a short post: plain text, media, optionally threaded.
	KindNote Kind = "note"
	// KindArticle is long-form: a title and a formatted body.
	KindArticle Kind = "article"
)

// Account is one connected identity on a destination type. Implementations
// must be safe for concurrent use.
//
// An account that holds connections or goroutines (a relay pool) also
// implements io.Closer. The host calls Close once nothing uses the account
// any more: it was reopened with new state, disabled or removed, or the
// server is stopping. No other method is called after Close.
type Account interface {
	// Manifest describes the account's destination type.
	Manifest() Manifest
	// Capabilities returns this account's effective capabilities: the
	// manifest defaults, refined by what the server reports (a Mastodon
	// instance's character limit). It never fails: when the server can't be
	// asked it returns the last known value or the manifest defaults.
	Capabilities(ctx context.Context) Capabilities
	// Publish publishes one item. On failure it returns a non-nil error,
	// ideally an *Error so the core can tell a retryable failure from a
	// permanent one. A result with a Ref and an error means the item is live
	// but a side effect failed (Status partial).
	Publish(ctx context.Context, req PublishRequest) (PublishResult, error)
}

// Identifier is implemented by accounts that can describe their own profile.
type Identifier interface {
	Identity(ctx context.Context) (Identity, error)
}

// Reposter is implemented by accounts that can natively repost (boost) an
// external post. Replies to and quotes of external posts go through Publish
// (PublishRequest.ReplyTo / Quote).
type Reposter interface {
	Repost(ctx context.Context, src SourceRef) (PublishResult, error)
}

// Redeliverer is implemented by destinations that send one item to several
// endpoints (Nostr relays): it re-sends an already published item to one
// endpoint, using the item's Ref.Data.
type Redeliverer interface {
	Redeliver(ctx context.Context, ref ItemRef, endpoint string) Delivery
}

// Locator is implemented by destinations that can find one of the
// account's own published posts by its URL. When an item's outcome is
// unknown and the owner finds it was posted, the core looks it up so a
// thread can go on under it (the next item needs the post's reference,
// not just its URL). Locate returns ErrPostNotFound when there is no post
// at the URL, and ErrNotOwnPost when the post is another account's.
type Locator interface {
	Locate(ctx context.Context, url string) (ItemRef, error)
}

var (
	ErrPostNotFound = errors.New("destination: no post at that URL")
	ErrNotOwnPost   = errors.New("destination: that post is another account's")
)

// Preparer is implemented by destinations whose items can be built before
// they are sent, so every attempt sends the same thing (Nostr: the signed
// event, which relays dedupe by id). The core persists what Prepare returns
// before the first send and passes it back as PublishRequest.Prepared on
// every attempt; such a destination's Capabilities.Idempotent is "natural".
type Preparer interface {
	Prepare(ctx context.Context, req PublishRequest) (json.RawMessage, error)
}

// PublishRequest is one item to publish.
type PublishRequest struct {
	Kind Kind `json:"kind"`
	// Text is a note's body: plain text. Drivers add their own rich-text
	// annotations (Bluesky facets, Nostr mentions).
	Text string `json:"text"`
	// Title and Body are for articles.
	Title string `json:"title,omitempty"`
	Body  *Body  `json:"body,omitempty"`
	// Media has already been fitted to this account's MediaCaps by the core.
	Media          []MediaItem `json:"media,omitempty"`
	ContentWarning string      `json:"content_warning,omitempty"`
	Sensitive      bool        `json:"sensitive,omitempty"`
	// Languages are BCP 47 tags, most significant first.
	Languages []string `json:"languages,omitempty"`
	// Visibility is one of Capabilities.Visibility; "" = the account default.
	Visibility string `json:"visibility,omitempty"`
	// Options are destination-specific settings, keyed by OptionSpec.Key.
	Options map[string]any `json:"options,omitempty"`
	// LinkCard is set only when Capabilities.LinkPreview is "client".
	LinkCard *LinkCard `json:"link_card,omitempty"`
	// InReplyTo threads this item under earlier items of the same target.
	InReplyTo *ThreadRef `json:"in_reply_to,omitempty"`
	// ReplyTo / Quote make the item a native reply to, or quote of, an
	// external post. Only on a thread's first item.
	ReplyTo *SourceRef `json:"reply_to,omitempty"`
	Quote   *SourceRef `json:"quote,omitempty"`
	// IdempotencyKey is stable across every attempt to publish this item.
	// Drivers whose Capabilities.Idempotent is "key" send it so the server
	// can drop a repeat.
	IdempotencyKey string `json:"idempotency_key,omitempty"`
	// Prepared is what the account's Prepare returned for this item, when
	// it is a Preparer. Publish sends it instead of building the item again.
	Prepared json.RawMessage `json:"prepared,omitempty"`
}

// Body is an article's formatted body.
type Body struct {
	Format string `json:"format"` // "markdown" (canonical) or "html"
	Source string `json:"source"`
}

// MediaItem is one attachment. Bytes are set for "bytes" transport, URL for
// "url" transport; the core fills both when it has them.
type MediaItem struct {
	Mime         string `json:"mime"`
	Alt          string `json:"alt,omitempty"`
	Bytes        []byte `json:"-"`
	URL          string `json:"url,omitempty"`
	SHA256       string `json:"sha256,omitempty"`
	Width        int    `json:"width,omitempty"`
	Height       int    `json:"height,omitempty"`
	SizeBytes    int64  `json:"size_bytes,omitempty"`
	DurationSecs int64  `json:"duration_secs,omitempty"`
	Blurhash     string `json:"blurhash,omitempty"`
	PosterURL    string `json:"poster_url,omitempty"`
}

// IsVideo reports whether the item is a video.
func (m MediaItem) IsVideo() bool { return len(m.Mime) >= 6 && m.Mime[:6] == "video/" }

// LinkCard is a link preview the client must build itself.
type LinkCard struct {
	URL         string `json:"url"`
	Title       string `json:"title"`
	Description string `json:"description"`
	Thumb       []byte `json:"-"`
	ThumbMime   string `json:"thumb_mime,omitempty"`
	// Records are records backing the link on the destination's own network
	// (atproto strong refs for a bsky.app link), when the core resolved them.
	Records []RecordRef `json:"records,omitempty"`
}

// RecordRef identifies a record by URI and content hash.
type RecordRef struct {
	URI string `json:"uri"`
	CID string `json:"cid,omitempty"`
}

// ItemRef identifies a published item. Data is driver-private: the core
// stores it as-is and hands it back (ThreadRef, Redeliver). Bluesky keeps the
// record CID there, Nostr the signed event.
type ItemRef struct {
	ID   string          `json:"id"`
	URL  string          `json:"url,omitempty"`
	Data json.RawMessage `json:"data,omitempty"`
}

// ThreadRef places an item in a thread: Root is the thread's first item,
// Parent the item it replies to.
type ThreadRef struct {
	Root   ItemRef `json:"root"`
	Parent ItemRef `json:"parent"`
}

// SourceRef is an external post, as resolved from its URL. Fields are the
// union of what each network needs; only the source network's are set.
type SourceRef struct {
	Type string `json:"type"` // destination type of the source network
	// Bluesky
	URI          string `json:"uri,omitempty"`
	CID          string `json:"cid,omitempty"`
	ReplyRootURI string `json:"reply_root_uri,omitempty"`
	ReplyRootCID string `json:"reply_root_cid,omitempty"`
	// Mastodon (the status id on the account's own instance)
	LocalID string `json:"local_id,omitempty"`
	// Nostr
	EventID    string   `json:"event_id,omitempty"`
	Author     string   `json:"author,omitempty"`
	RelayHints []string `json:"relay_hints,omitempty"`
	EventKind  int      `json:"event_kind,omitempty"`
	// ThreadRootID is the root of the thread the event itself replies into
	// (empty when it is a root); Mentions are its p-tags. A reply needs both
	// to stay in the conversation and notify its participants (NIP-10).
	ThreadRootID string   `json:"thread_root_id,omitempty"`
	Mentions     []string `json:"mentions,omitempty"`
}

// ItemStatus is the outcome of a publish.
type ItemStatus string

const (
	StatusPublished ItemStatus = "published"
	// StatusPartial: the item is live but something secondary failed (a
	// Bluesky reply gate, some Nostr relays).
	StatusPartial ItemStatus = "partial"
	StatusFailed  ItemStatus = "failed"
)

// PublishResult is what a publish produced.
type PublishResult struct {
	Status ItemStatus `json:"status"`
	Ref    ItemRef    `json:"ref"`
	// Deliveries are per-endpoint outcomes for multi-endpoint destinations.
	Deliveries []Delivery `json:"deliveries,omitempty"`
	// Pending are endpoints the core should deliver to later with
	// Redeliverer (Nostr secondary relays in fan-out mode).
	Pending  []string `json:"pending,omitempty"`
	Warnings []string `json:"warnings,omitempty"`
	Trace    Trace    `json:"trace"`
}

// Delivery is one endpoint's outcome.
type Delivery struct {
	Endpoint string `json:"endpoint"`
	Status   string `json:"status"` // ok | failed | skipped
	Message  string `json:"message,omitempty"`
}

// Trace is the request and response of an attempt, for the attempt log.
// Drivers must leave secrets out.
type Trace struct {
	Request  string `json:"request,omitempty"`
	Response string `json:"response,omitempty"`
}

// Identity is an account's public profile.
type Identity struct {
	// RemoteID is the account's stable id on its network (a DID, an
	// account URL, a user id, a public key): it tells two accounts apart
	// when handles change.
	RemoteID    string `json:"remote_id,omitempty"`
	Handle      string `json:"handle"`
	DisplayName string `json:"display_name,omitempty"`
	AvatarURL   string `json:"avatar_url,omitempty"`
	ProfileURL  string `json:"profile_url,omitempty"`
}
