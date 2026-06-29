package dispatch

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	gonostr "fiatjaf.com/nostr"
	"github.com/geofox/publisher/internal/logging"
	"github.com/geofox/publisher/internal/media"
	"github.com/geofox/publisher/internal/metrics"
	"github.com/geofox/publisher/internal/progress"
	"github.com/geofox/publisher/internal/store"
	"github.com/geofox/publisher/internal/thread"
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

// runPlatform executes one platform and returns a normalized TargetResult.
func (d *Dispatcher) runPlatform(ctx context.Context, plat, text string, ov Overrides, imgs []Img, imetas []gonostr.Tag, replyTo *ReplyRef) TargetResult {
	start := time.Now()
	var r TargetResult
	var err error
	switch plat {
	case "nostr":
		if d.Nostr != nil {
			r, err = d.Nostr.PublishText(ctx, text, ov.POW, imetas, replyTo)
		} else {
			err = errors.New("nostr not configured")
		}
	case "mastodon":
		if d.Mastodon != nil {
			r, err = d.Mastodon.PostText(ctx, text, ov, imgs, replyTo)
		} else {
			err = errors.New("mastodon not configured")
		}
	case "bluesky":
		if d.Bluesky != nil {
			r, err = d.Bluesky.PostBsky(ctx, text, ov, imgs, replyTo)
		} else {
			err = errors.New("bluesky not configured")
		}
	case "threads":
		if d.Threads != nil {
			r, err = d.Threads.PostThreads(ctx, text, ov, imgs, replyTo)
		} else {
			err = errors.New("threads not configured")
		}
	default:
		err = errors.New("unsupported platform")
	}
	if r.Platform == "" {
		r.Platform = plat
	}
	// Normalize: an adapter that returns an error (or a zero TargetResult)
	// must still produce a "failed" target with a message — never an
	// empty status that silently aggregates wrong.
	if r.Status == "" {
		r.Status = "failed"
	}
	if r.Status == "failed" && r.Error == "" && err != nil {
		r.Error = err.Error()
	}
	if r.LatencyMS == 0 {
		r.LatencyMS = int(time.Since(start).Milliseconds())
	}
	metrics.RecordPublish(r.Platform, r.Status, time.Since(start))
	return r
}

// runAction executes one native interaction (repost/quote) on one platform and
// returns a normalized TargetResult, mirroring runPlatform's normalization.
func (d *Dispatcher) runAction(ctx context.Context, action, plat, text string, ov Overrides, imgs []Img, imetas []gonostr.Tag, ref InteractRef) TargetResult {
	start := time.Now()
	var r TargetResult
	var err error
	switch {
	case action == actionRepost && plat == "bluesky":
		if d.Bluesky != nil {
			r, err = d.Bluesky.RepostBsky(ctx, ref.URI, ref.CID)
		} else {
			err = errors.New("bluesky not configured")
		}
	case action == actionQuote && plat == "bluesky":
		if d.Bluesky != nil {
			r, err = d.Bluesky.QuoteBsky(ctx, text, ov, imgs, ref.URI, ref.CID)
		} else {
			err = errors.New("bluesky not configured")
		}
	case action == actionRepost && plat == "mastodon":
		if d.Mastodon != nil {
			r, err = d.Mastodon.Reblog(ctx, ref.LocalID)
		} else {
			err = errors.New("mastodon not configured")
		}
	case action == actionQuote && plat == "mastodon":
		if d.Mastodon != nil {
			r, err = d.Mastodon.QuoteStatus(ctx, text, ref.LocalID, imgs)
		} else {
			err = errors.New("mastodon not configured")
		}
	case action == actionRepost && plat == "nostr":
		if d.Nostr != nil {
			r, err = d.Nostr.Repost(ctx, ref.EventID, ref.Author, ref.Kind, relayHint(ref.RelayHints))
		} else {
			err = errors.New("nostr not configured")
		}
	case action == actionQuote && plat == "nostr":
		if d.Nostr != nil {
			r, err = d.Nostr.Quote(ctx, text, ref.EventID, ref.Author, relayHint(ref.RelayHints), imetas)
		} else {
			err = errors.New("nostr not configured")
		}
	default:
		err = fmt.Errorf("unsupported action %q for %q", action, plat)
	}
	r.Platform = plat
	r.LatencyMS = int(time.Since(start).Milliseconds())
	if err != nil {
		r.Status, r.Error = "failed", err.Error()
	} else if r.Status == "" {
		r.Status = "failed"
		if r.Error == "" {
			r.Error = "adapter returned empty status"
		}
	}
	metrics.RecordPublish(r.Platform, r.Status, time.Since(start))
	return r
}

// relayHint returns the first relay hint, or "" when none are present.
func relayHint(hints []string) string {
	if len(hints) > 0 {
		return hints[0]
	}
	return ""
}

// chainOutcome is the result of posting one platform's (possibly single-segment)
// chain. Segments is empty for a single post (preserving non-threaded behavior).
type chainOutcome struct {
	Platform                    string
	Status                      string
	Error                       string
	FinalText                   string       // what this target actually sent (head text)
	LinkCard                    *unfurl.Card // bluesky link card as actually attached (nil = none)
	HeadRemoteID, HeadRemoteURL string
	LatencyMS                   int
	Relays                      []store.RelayState
	SignedEventJSON             string
	RequestJSON, ResponseJSON   string
	Segments                    []store.Segment
}

// headSpec makes a chain's head segment a reply or a quote instead of a plain
// post. nil → plain post head (normal Post + fan-out reproduction). The tail
// segments always thread as plain replies under the head.
type headSpec struct {
	reply *ReplyRef    // head replies to this
	quote *InteractRef // head quotes this (native quote)
}

// runHead posts the head segment per the headSpec (reply / quote / plain).
func (d *Dispatcher) runHead(ctx context.Context, plat, text string, ov Overrides, imgs []Img, imetas []gonostr.Tag, head *headSpec) TargetResult {
	if head != nil && head.quote != nil {
		return d.runAction(ctx, actionQuote, plat, text, ov, imgs, imetas, *head.quote)
	}
	var replyTo *ReplyRef
	if head != nil {
		replyTo = head.reply
	}
	return d.runPlatform(ctx, plat, text, ov, imgs, imetas, replyTo)
}

// runChain splits text to the platform's limit and posts the segments as a
// reply-chain (segment k+1 replies to segment k). Images are distributed per
// thread.PlanMedia: head first, capped per platform, with image-only segments
// appended when images outrun the text (Mastodon's 4-attachment cap). A
// single segment posts exactly as before, with no Segments recorded. An
// optional head action (reply/quote via headSpec; nil = plain post) lets the
// chain's head segment thread under or quote a source post.
func (d *Dispatcher) runChain(ctx context.Context, plat, text string, ov Overrides, imgs []Img, imetas []gonostr.Tag, number bool, imgParts []int, head *headSpec) chainOutcome {
	// imgParts must be parallel to imgs. A nil/short slice (interaction paths,
	// or an assembleImages skip) pads with 0 (front-load); a long one truncates.
	// SplitPlace derives nImages from len(imgParts), so it MUST equal len(imgs).
	if len(imgParts) != len(imgs) {
		np := make([]int, len(imgs))
		copy(np, imgParts)
		imgParts = np
	}
	var card *unfurl.Card
	var segTexts []string
	var plan [][]int
	if plat == "bluesky" {
		cp := PlanBlueskyCard(text, ov.LinkCard, imgParts, number)
		segTexts, plan, text, card = cp.Segs, cp.Plan, cp.Text, cp.Card
	} else {
		segTexts, plan, _ = thread.SplitPlace(text, thread.LimitFor(plat), imgParts, thread.MaxImagesFor(plat), thread.Opts{Number: number})
	}
	// Only the card's own segment carries it; re-attached per segment below.
	ov.LinkCard = nil
	if len(segTexts) <= 1 {
		headOv := ov
		if card != nil {
			headOv.LinkCard = card
		}
		r := d.runHead(ctx, plat, text, headOv, imgs, imetas, head)
		return chainOutcome{
			Platform: plat, Status: r.Status, Error: r.Error, FinalText: text, LinkCard: card,
			HeadRemoteID: r.RemoteID, HeadRemoteURL: r.RemoteURL, LatencyMS: r.LatencyMS,
			Relays: r.Relays, SignedEventJSON: r.SignedEventJSON,
			RequestJSON: r.RequestJSON, ResponseJSON: r.ResponseJSON,
		}
	}
	out := chainOutcome{Platform: plat}
	// Record every planned segment up front (pending) so a mid-chain stop still
	// preserves the not-yet-sent tail for resume; each is updated in place as it
	// posts.
	for i, st := range segTexts {
		out.Segments = append(out.Segments, store.Segment{Ordinal: i, Text: st, Status: "pending", Images: plan[i]})
	}
	var rootID, rootCID, parentID, parentCID string
	for i, st := range segTexts {
		var replyTo *ReplyRef
		if i > 0 {
			replyTo = &ReplyRef{RootID: rootID, RootCID: rootCID, ParentID: parentID, ParentCID: parentCID}
		}
		segImgs := pick(imgs, plan[i])
		var segImetas []gonostr.Tag
		if i == 0 {
			segImetas = imetas // imeta stays head-only in v1 (buildImetas skips
			// empty-Blossom records, so it's not index-parallel to imgs)
		}
		segOv := ov
		if card != nil && i == card.Segment {
			segOv.LinkCard = card
		}
		var r TargetResult
		if i == 0 {
			r = d.runHead(ctx, plat, st, segOv, segImgs, segImetas, head)
		} else {
			r = d.runPlatform(ctx, plat, st, segOv, segImgs, segImetas, replyTo)
		}
		out.Segments[i] = store.Segment{
			Ordinal: i, Text: st, RemoteID: r.RemoteID, RemoteURL: r.RemoteURL, CID: r.CID,
			Status: r.Status, Error: r.Error, Images: plan[i],
		}
		// live thread counter: successes so far / total planned
		done := 0
		for _, sg := range out.Segments {
			if sg.Status == "success" {
				done++
			}
		}
		progress.SinkFrom(ctx).Platform(plat, progress.StatusRunning, "thread "+strconv.Itoa(done)+"/"+strconv.Itoa(len(segTexts)), "")
		if i == 0 {
			rootID, rootCID = r.RemoteID, r.CID
			out.HeadRemoteID, out.HeadRemoteURL = r.RemoteID, r.RemoteURL
			out.Relays, out.SignedEventJSON = r.Relays, r.SignedEventJSON
			out.RequestJSON, out.ResponseJSON, out.LatencyMS = r.RequestJSON, r.ResponseJSON, r.LatencyMS
		}
		parentID, parentCID = r.RemoteID, r.CID
		out.Error = r.Error
		if r.RemoteID == "" { // no id to reply to → cannot continue the chain
			break
		}
	}
	out.Status = chainStatus(out.Segments, len(segTexts))
	out.FinalText = text
	out.LinkCard = card
	return out
}

// pick returns the images at the given indices, preserving order. Out-of-range
// indices are skipped (a Blossom skip can leave the plan referencing a dropped
// image), so the returned slice never panics on a short imgs.
func pick(imgs []Img, idx []int) []Img {
	out := make([]Img, 0, len(idx))
	for _, i := range idx {
		if i >= 0 && i < len(imgs) {
			out = append(out, imgs[i])
		}
	}
	return out
}

// resumeSegments re-posts a partial chain's segments from the first non-success
// segment, threading from the last successful one (root stays segment 0). If the
// head (segment 0) isn't success, the whole chain is re-posted from scratch.
// No store writes — returns the updated outcome.
func (d *Dispatcher) resumeSegments(ctx context.Context, tg store.Target, ov Overrides, imgs []Img, imetas []gonostr.Tag) chainOutcome {
	segs := append([]store.Segment(nil), tg.Segments...)
	card := ov.LinkCard
	ov.LinkCard = nil
	start := 0
	// A segment with a RemoteID is already live on-platform (success, or partial —
	// e.g. nostr posted but a relay flapped, or bluesky posted but a gate write
	// failed). Never re-post a live segment; resume from the first without an id.
	for start < len(segs) && segs[start].RemoteID != "" {
		start++
	}
	out := chainOutcome{Platform: tg.Platform, Segments: segs}
	if start == len(segs) { // already complete
		out.Status = chainStatus(segs, len(segs))
		if len(segs) > 0 {
			out.HeadRemoteID, out.HeadRemoteURL = segs[0].RemoteID, segs[0].RemoteURL
		}
		return out
	}
	var rootID, rootCID, parentID, parentCID string
	if start > 0 {
		rootID, rootCID = segs[0].RemoteID, segs[0].CID
		parentID, parentCID = segs[start-1].RemoteID, segs[start-1].CID
	}

	// Re-derive the media plan deterministically (same inputs as runChain).
	// Per-segment media assignments are not persisted (the plan is recomputed
	// from counts), so if a Blossom re-fetch skipped an image the surviving
	// images re-index and later segments can shift content by one position.
	// That's the accepted trade-off for a schema-free resume; skips are rare
	// and best-effort by design.
	plan := thread.PlanMedia(len(imgs), len(segs), thread.MaxImagesFor(tg.Platform))
	starts := make([]int, len(plan))
	offset := 0
	for i, c := range plan {
		starts[i] = offset
		offset += c
	}
	// More images than the recorded segments can carry (legacy or hand-edited
	// data): plan entries beyond len(segs) would never post — say so.
	if len(plan) > len(segs) {
		dropped := 0
		for _, c := range plan[len(segs):] {
			dropped += c
		}
		slog.WarnContext(ctx, "resume: trailing images exceed recorded segments, dropped",
			"platform", tg.Platform, "images", len(imgs), "dropped", dropped)
	}

	for i := start; i < len(segs); i++ {
		var replyTo *ReplyRef
		if i > 0 {
			replyTo = &ReplyRef{RootID: rootID, RootCID: rootCID, ParentID: parentID, ParentCID: parentCID}
		}
		var segImgs []Img
		if i < len(plan) {
			segImgs = imgs[starts[i] : starts[i]+plan[i]]
		}
		var segImetas []gonostr.Tag
		if i == 0 {
			segImetas = imetas // nostr-only; nostr never splits media
		}
		segOv := ov
		if card != nil && i == card.Segment {
			segOv.LinkCard = card
		}
		r := d.runPlatform(ctx, tg.Platform, segs[i].Text, segOv, segImgs, segImetas, replyTo)
		segs[i] = store.Segment{Ordinal: i, Text: segs[i].Text, RemoteID: r.RemoteID, RemoteURL: r.RemoteURL, CID: r.CID, Status: r.Status, Error: r.Error}
		if i == 0 {
			rootID, rootCID = r.RemoteID, r.CID
			out.Relays, out.SignedEventJSON, out.LatencyMS = r.Relays, r.SignedEventJSON, r.LatencyMS
		}
		parentID, parentCID = r.RemoteID, r.CID
		out.Error = r.Error
		if r.RemoteID == "" {
			break
		}
	}
	out.Status = chainStatus(segs, len(segs))
	out.HeadRemoteID, out.HeadRemoteURL = segs[0].RemoteID, segs[0].RemoteURL
	return out
}

// resumeChain resumes a partial threaded target and persists the result.
func (d *Dispatcher) resumeChain(ctx context.Context, tg store.Target, ov Overrides, imgs []Img, imetas []gonostr.Tag) error {
	out := d.resumeSegments(ctx, tg, ov, imgs, imetas)
	return d.Store.UpdateTargetSegments(tg.ID, out.Segments, out.Status, out.HeadRemoteID, out.HeadRemoteURL, out.LatencyMS, out.Error)
}

// chainStatus aggregates a chain's segment statuses. expected is the planned
// segment count; fewer attempted (a stop) or any non-success ⇒ partial.
func chainStatus(segs []store.Segment, expected int) string {
	if len(segs) == 0 || segs[0].RemoteID == "" || segs[0].Status == "failed" {
		return "failed"
	}
	complete := len(segs) == expected
	for _, s := range segs {
		if s.Status != "success" {
			complete = false
		}
	}
	if complete {
		return "success"
	}
	return "partial"
}

func newID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// NewID mints a new unique post ID. Exported so the API layer can pre-mint an
// ID before launching a detached dispatch goroutine.
func NewID() string { return newID() }

// buildImetas rebuilds NIP-92 imeta tags (one per attached image) from the
// archived media records, so a Nostr cross-post embeds the same media the
// upload pipeline produced. Records without a Blossom URL are skipped.
func buildImetas(recs []store.Media) []gonostr.Tag {
	var out []gonostr.Tag
	for _, m := range recs {
		if m.BlossomURL == "" {
			continue
		}
		out = append(out, media.ImetaTag(m.BlossomURL, m.Mime, m.SHA256, m.Dim, m.Blurhash, m.PosterURL))
	}
	return out
}

// scrubLinkCards drops any client-supplied link_card from incoming overrides.
// LinkCard is dispatcher-computed only (attachLinkCard + PlanBlueskyCard);
// a request-supplied card must never reach an adapter or fields_json.
func scrubLinkCards(overrides map[string]Overrides) {
	for k, ov := range overrides {
		if ov.LinkCard != nil {
			ov.LinkCard = nil
			overrides[k] = ov
		}
	}
}

// attachLinkCard unfurls the bluesky-effective text's card URL (trailing,
// else first) into spec.Overrides["bluesky"].LinkCard. Best-effort: on any
// unfurl error the post proceeds exactly as before — bare faceted link, no
// card. Placement and trailing-strip happen later in PlanBlueskyCard.
func (d *Dispatcher) attachLinkCard(ctx context.Context, spec *PostSpec) {
	if d.Unfurler == nil {
		return
	}
	targeted := false
	for _, p := range spec.Platforms {
		if p == "bluesky" {
			targeted = true
			break
		}
	}
	if !targeted {
		return
	}
	ov := spec.Overrides["bluesky"]
	text := spec.MasterText
	if ov.Text != "" {
		text = ov.Text
	}
	u, _, ok := unfurl.CardURL(text)
	if !ok {
		return
	}
	uctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	card, err := d.Unfurler.Unfurl(uctx, u)
	if err != nil {
		slog.WarnContext(ctx, "unfurl failed; posting without link card", "url", u, "err", err)
		return
	}
	ov.LinkCard = card
	if spec.Overrides == nil {
		spec.Overrides = map[string]Overrides{}
	}
	spec.Overrides["bluesky"] = ov
}

func (d *Dispatcher) Post(ctx context.Context, spec PostSpec) *store.Post {
	return d.PostWithID(ctx, newID(), spec)
}

func (d *Dispatcher) PostWithID(ctx context.Context, id string, spec PostSpec) *store.Post {
	platforms := dedupStrings(spec.Platforms)
	rec := &store.Post{
		ID: id, CreatedAt: time.Now().UTC(), MasterText: spec.MasterText,
		Platforms: platforms, DelaySeconds: spec.DelaySeconds, Source: spec.Source,
		Media: spec.MediaRecords,
	}
	ctx = logging.With(ctx, "post_id", rec.ID)

	imetas := buildImetas(spec.MediaRecords)
	scrubLinkCards(spec.Overrides)
	d.attachLinkCard(ctx, &spec)

	// One result slot per platform, written by its own goroutine (distinct
	// indices → no mutex, and rec.Targets stays in platforms order).
	outcomes := make([]chainOutcome, len(platforms))
	var wg sync.WaitGroup
	for i, plat := range platforms {
		ov := spec.Overrides[plat]
		text := spec.MasterText
		if ov.Text != "" {
			text = ov.Text
		}
		wg.Add(1)
		go func(i int, plat, text string, ov Overrides) {
			defer wg.Done()
			sink := progress.SinkFrom(ctx)
			sink.Platform(plat, progress.StatusRunning, "", "")
			o := d.runChain(ctx, plat, text, ov, spec.Images, imetas, spec.Number, spec.ImgParts, nil)
			outcomes[i] = o
			sink.Platform(plat, mapStatus(o.Status), platformDetail(o), o.HeadRemoteURL)
		}(i, plat, text, ov)
	}
	wg.Wait()

	succ, failed := 0, 0
	for _, o := range outcomes {
		ovp := spec.Overrides[o.Platform]
		ovp.LinkCard = o.LinkCard // persist the card only as actually attached
		fields, _ := json.Marshal(ov2fields(ovp))
		tg := store.Target{
			Platform: o.Platform, FinalText: o.FinalText, FieldsJSON: string(fields),
			Status: o.Status, RemoteID: o.HeadRemoteID, RemoteURL: o.HeadRemoteURL, LatencyMS: o.LatencyMS,
			Relays: o.Relays, SignedEventJSON: o.SignedEventJSON, Segments: o.Segments,
			Attempts: []store.Attempt{{
				AttemptNo: 1, Status: o.Status, Error: o.Error, LatencyMS: o.LatencyMS, RemoteID: o.HeadRemoteID,
				RequestJSON: o.RequestJSON, ResponseJSON: o.ResponseJSON, AttemptedAt: time.Now().UTC(),
			}},
		}
		rec.Targets = append(rec.Targets, tg)
		switch o.Status {
		case "success":
			succ++
		case "failed":
			failed++
		}
	}
	// Partial-aware aggregation, mirroring store.recomputeStatus so the in-memory
	// rec.Status (returned to the API/modal) and the persisted status agree: a
	// partial target (e.g. nostr with some relays down) keeps the post partial.
	total := len(outcomes)
	switch {
	case total == 0 || failed == total:
		rec.Status = "failed"
	case succ == total:
		rec.Status = "success"
	default:
		rec.Status = "partial"
	}
	if d.Store != nil {
		if err := d.Store.SavePost(rec); err != nil {
			slog.ErrorContext(ctx, "savepost failed", "err", err)
		}
	}
	d.notify(ctx, rec)
	d.alertFailure(ctx, rec)
	return rec
}

// SourcePreview is the resolved original's content, passed from the frontend so
// fan-out targets can reproduce it without re-resolving.
type SourcePreview struct {
	Author string // @handle / display
	Text   string
}

type InteractSpec struct {
	Action             string // reply|repost|quote
	SourcePlatform     string
	Ref                InteractRef
	SourceURL          string
	SourceAuthor       string
	Text               string
	Overrides          map[string]Overrides // keyed by platform
	Fanout             []string             // quote only: other platforms for link-quotes
	Force              bool
	Images             []Img
	MediaRecords       []store.Media
	Number             bool          // k/n counters on threaded segments
	SourcePreview      SourcePreview // for fan-out reproduction
	SourceImages       []Img         // original's media, re-hosted (fan-out only)
	SourceMediaRecords []store.Media // imeta records for the re-hosted source media
}

// assembleReproduction builds a fan-out post body: commentary, an attributed copy
// of the original's text, and the source URL — each separated by a blank line.
func assembleReproduction(commentary string, sp SourcePreview, sourceURL string) string {
	var parts []string
	if c := strings.TrimSpace(commentary); c != "" {
		parts = append(parts, c)
	}
	if strings.TrimSpace(sp.Text) != "" {
		parts = append(parts, "— "+sp.Author+":\n"+sp.Text)
	}
	if sourceURL != "" {
		parts = append(parts, sourceURL)
	}
	return strings.Join(parts, "\n\n")
}

// capMedia returns the user's images followed by source images, truncated to max
// (max <= 0 means no cap). User images always take priority.
func capMedia(user, source []Img, max int) []Img {
	out := append([]Img(nil), user...)
	for _, m := range source {
		if max > 0 && len(out) >= max {
			break
		}
		out = append(out, m)
	}
	return out
}

// capMediaRecords mirrors capMedia for store.Media (nostr imeta).
func capMediaRecords(user, source []store.Media, max int) []store.Media {
	out := append([]store.Media(nil), user...)
	for _, m := range source {
		if max > 0 && len(out) >= max {
			break
		}
		out = append(out, m)
	}
	return out
}

// mediaMax is the per-platform attachment cap. Single source of truth lives in
// thread.MaxImagesFor so dispatch, preview, and the splitter cannot diverge.
func mediaMax(plat string) int { return thread.MaxImagesFor(plat) }

// fanoutChain posts an assembled reproduction (commentary + original text + url,
// with re-hosted source media capped per platform) as a normal thread.
// interactText is the commentary for one platform: its per-platform text
// override when set (mirrors Post honoring ov.Text), else the master commentary.
func interactText(spec InteractSpec, plat string) string {
	if t := spec.Overrides[plat].Text; t != "" {
		return t
	}
	return spec.Text
}

func (d *Dispatcher) fanoutChain(ctx context.Context, plat string, spec InteractSpec) chainOutcome {
	text := assembleReproduction(interactText(spec, plat), spec.SourcePreview, spec.SourceURL)
	imgs := capMedia(spec.Images, spec.SourceImages, mediaMax(plat))
	recs := capMediaRecords(spec.MediaRecords, spec.SourceMediaRecords, mediaMax(plat))
	ov := spec.Overrides[plat]
	// Interactions carry no per-image part anchors; nil ⇒ runChain front-loads.
	return d.runChain(ctx, plat, text, ov, imgs, buildImetas(recs), spec.Number, nil, nil)
}

// Interact performs reply/repost/quote and records it as a store.Post carrying an
// interaction descriptor. Reply/quote thread the source action as the chain head on
// the source platform; quote also fans out an assembled reproduction (commentary +
// original text + source URL + re-hosted source media) to each Fanout platform.
func (d *Dispatcher) Interact(ctx context.Context, spec InteractSpec) *store.Post {
	return d.InteractWithID(ctx, newID(), spec)
}

func (d *Dispatcher) InteractWithID(ctx context.Context, id string, spec InteractSpec) *store.Post {
	rec := &store.Post{
		ID: id, CreatedAt: time.Now().UTC(), MasterText: spec.Text,
		Source: "web", Media: spec.MediaRecords,
		Interaction: &store.Interaction{
			Action: spec.Action, SourcePlatform: spec.SourcePlatform,
			SourceURL: spec.SourceURL, SourceAuthor: spec.SourceAuthor,
		},
	}
	ctx = logging.With(ctx, "post_id", rec.ID)
	scrubLinkCards(spec.Overrides)
	var outcomes []chainOutcome
	switch spec.Action {
	case actionRepost:
		sink := progress.SinkFrom(ctx)
		sink.Platform(spec.SourcePlatform, progress.StatusRunning, "", "")
		r := d.runAction(ctx, actionRepost, spec.SourcePlatform, "", spec.Overrides[spec.SourcePlatform], nil, nil, spec.Ref)
		o := chainOutcome{
			Platform: r.Platform, Status: r.Status, Error: r.Error,
			HeadRemoteID: r.RemoteID, HeadRemoteURL: r.RemoteURL, LatencyMS: r.LatencyMS,
			Relays: r.Relays, SignedEventJSON: r.SignedEventJSON, RequestJSON: r.RequestJSON, ResponseJSON: r.ResponseJSON,
		}
		sink.Platform(spec.SourcePlatform, mapStatus(o.Status), platformDetail(o), o.HeadRemoteURL)
		outcomes = append(outcomes, o)
	case actionReply, actionQuote:
		ov := spec.Overrides[spec.SourcePlatform]
		head := &headSpec{}
		if spec.Action == actionReply {
			head.reply = buildReplyRef(spec)
		} else {
			head.quote = &spec.Ref
		}
		sink := progress.SinkFrom(ctx)
		sink.Platform(spec.SourcePlatform, progress.StatusRunning, "", "")
		o := d.runChain(ctx, spec.SourcePlatform, interactText(spec, spec.SourcePlatform), ov, spec.Images, buildImetas(spec.MediaRecords), spec.Number, nil, head)
		sink.Platform(spec.SourcePlatform, mapStatus(o.Status), platformDetail(o), o.HeadRemoteURL)
		outcomes = append(outcomes, o)
		for _, p := range spec.Fanout {
			if p == spec.SourcePlatform {
				continue
			}
			sink.Platform(p, progress.StatusRunning, "", "")
			fo := d.fanoutChain(ctx, p, spec)
			sink.Platform(p, mapStatus(fo.Status), platformDetail(fo), fo.HeadRemoteURL)
			outcomes = append(outcomes, fo)
		}
	}

	succ, failed := 0, 0
	for _, o := range outcomes {
		fields, _ := json.Marshal(ov2fields(spec.Overrides[o.Platform]))
		rec.Platforms = append(rec.Platforms, o.Platform)
		rec.Targets = append(rec.Targets, store.Target{
			Platform: o.Platform, FinalText: o.FinalText, FieldsJSON: string(fields),
			Status: o.Status, RemoteID: o.HeadRemoteID, RemoteURL: o.HeadRemoteURL, LatencyMS: o.LatencyMS,
			Relays: o.Relays, SignedEventJSON: o.SignedEventJSON, Segments: o.Segments,
			Attempts: []store.Attempt{{AttemptNo: 1, Status: o.Status, Error: o.Error, LatencyMS: o.LatencyMS,
				RemoteID: o.HeadRemoteID, RequestJSON: o.RequestJSON, ResponseJSON: o.ResponseJSON, AttemptedAt: time.Now().UTC()}},
		})
		switch o.Status {
		case "success":
			succ++
		case "failed":
			failed++
		}
	}
	switch total := len(outcomes); {
	case total == 0 || failed == total:
		rec.Status = "failed"
	case succ == total:
		rec.Status = "success"
	default:
		rec.Status = "partial"
	}
	if d.Store != nil {
		if err := d.Store.SavePost(rec); err != nil {
			slog.ErrorContext(ctx, "savepost (interact) failed", "err", err)
		}
	}
	d.notify(ctx, rec)
	d.alertFailure(ctx, rec)
	return rec
}

// buildReplyRef derives the platform reply ref from the source. Bluesky: parent =
// source, root = its thread root (or source if top-level). Mastodon: parent id =
// local id. Nostr: parent = event id with the external author for the p-tag.
func buildReplyRef(spec InteractSpec) *ReplyRef {
	ref := spec.Ref
	switch spec.SourcePlatform {
	case "bluesky":
		root, rootCID := ref.ReplyRootURI, ref.ReplyRootCID
		if root == "" {
			root, rootCID = ref.URI, ref.CID
		}
		return &ReplyRef{RootID: root, RootCID: rootCID, ParentID: ref.URI, ParentCID: ref.CID}
	case "mastodon":
		return &ReplyRef{ParentID: ref.LocalID}
	case "nostr":
		return &ReplyRef{RootID: ref.EventID, ParentID: ref.EventID, AuthorPubkey: ref.Author}
	}
	return nil
}

func finalText(spec PostSpec, plat string) string {
	if ov, ok := spec.Overrides[plat]; ok && ov.Text != "" {
		return ov.Text
	}
	return spec.MasterText
}

func ov2fields(o Overrides) map[string]any {
	return map[string]any{
		"language": o.Language, "visibility": o.Visibility, "sensitive": o.Sensitive,
		"spoiler_text": o.SpoilerText, "content_warning": o.ContentWarning, "pow": o.POW,
		"langs": o.Langs, "topic_tag": o.TopicTag,
		"threads_reply_control": o.ThreadsReplyControl,
		"bluesky_reply":         o.BlueskyReply, "bluesky_disable_quotes": o.BlueskyDisableQuotes,
		"link_card": o.LinkCard,
	}
}

// dispatchTargets re-pulls media once, then dispatches every target the want
// predicate accepts (reconstructing overrides from FieldsJSON) and appends each
// attempt. Shared by Retry (failed targets) and Fire (scheduled targets).
// AppendTargetAttempt recomputes the post status per attempt.
func (d *Dispatcher) dispatchTargets(ctx context.Context, post *store.Post, want func(store.Target) bool) error {
	var imgs []Img
	for _, m := range post.Media {
		img := Img{Mime: m.Mime, Alt: m.Alt, BlossomURL: m.BlossomURL,
			Dim: m.Dim, DurationSecs: m.DurationSecs, SizeBytes: m.SizeBytes}
		// Videos: fetch only when a byte platform could use them (mirror
		// assembleImages' policy); URL platforms ride metadata-only. A failed
		// best-effort fetch leaves Bytes nil — adapter gates fail loudly.
		if d.Fetcher != nil && (!img.IsVideo() || (m.SizeBytes > 0 && m.SizeBytes <= media.FetchCap)) {
			if body, fmime, err := d.Fetcher.Fetch(ctx, m.BlossomURL); err == nil {
				img.Bytes = body
				if img.Mime == "" {
					img.Mime = fmime
				}
			}
		}
		imgs = append(imgs, img)
	}
	imetas := buildImetas(post.Media)
	for _, tg := range post.Targets {
		if !want(tg) {
			continue
		}
		var ov Overrides
		if tg.FieldsJSON != "" {
			if err := json.Unmarshal([]byte(tg.FieldsJSON), &ov); err != nil {
				slog.Warn("dispatchTargets: bad fields_json, using zero overrides", "target_id", tg.ID, "err", err)
			}
		}
		// A persisted card has no thumb bytes (json:"-"); re-download
		// best-effort so a retried/fired post still carries its thumbnail.
		if ov.LinkCard != nil && ov.LinkCard.ThumbURL != "" && d.Unfurler != nil {
			tctx, cancel := context.WithTimeout(ctx, 15*time.Second)
			data, mime, err := d.Unfurler.Thumb(tctx, ov.LinkCard.ThumbURL)
			cancel()
			if err != nil {
				slog.WarnContext(ctx, "link card thumb re-fetch failed; card posts without thumb",
					"url", ov.LinkCard.ThumbURL, "err", err)
			} else {
				ov.LinkCard.ThumbData, ov.LinkCard.ThumbMime = data, mime
			}
		}
		if len(tg.Segments) > 1 { // threaded target → resume the chain
			if err := d.resumeChain(ctx, tg, ov, imgs, imetas); err != nil {
				return err
			}
			continue
		}
		r := d.runPlatform(ctx, tg.Platform, tg.FinalText, ov, imgs, imetas, nil)
		if err := d.Store.AppendTargetAttempt(tg.ID, r.Status, r.Error, r.RemoteID, r.RemoteURL, r.LatencyMS, r.RequestJSON, r.ResponseJSON, r.Relays, r.SignedEventJSON); err != nil {
			return err
		}
	}
	return nil
}

// Schedule persists a post (and its per-platform targets) as 'scheduled' for a
// future time, without dispatching. The worker fires it later.
func (d *Dispatcher) Schedule(ctx context.Context, spec PostSpec, at time.Time) (*store.Post, error) {
	if d.Store == nil {
		return nil, errors.New("schedule requires a store")
	}
	platforms := dedupStrings(spec.Platforms)
	atUTC := at.UTC()
	rec := &store.Post{
		ID: newID(), CreatedAt: time.Now().UTC(), MasterText: spec.MasterText,
		Platforms: platforms, DelaySeconds: spec.DelaySeconds, Source: spec.Source,
		Status: "scheduled", ScheduledAt: &atUTC, Media: spec.MediaRecords,
	}
	scrubLinkCards(spec.Overrides)
	d.attachLinkCard(ctx, &spec)
	// Pre-split uses the scheduled image count (MediaRecords); normalize the
	// per-image part anchors to that length so SplitPlace's nImages is right.
	imgParts := make([]int, len(spec.MediaRecords))
	copy(imgParts, spec.ImgParts)
	for _, plat := range platforms {
		ov := spec.Overrides[plat]
		text := finalText(spec, plat)
		var segTexts []string
		if plat == "bluesky" {
			cp := PlanBlueskyCard(text, ov.LinkCard, imgParts, spec.Number)
			text, segTexts = cp.Text, cp.Segs
			ov.LinkCard = cp.Card // persist only as actually placed
		} else {
			segTexts, _, _ = thread.SplitPlace(text, thread.LimitFor(plat), imgParts, thread.MaxImagesFor(plat), thread.Opts{Number: spec.Number})
		}
		// ov2fields returns a map of JSON-safe values only, so Marshal can't fail.
		fields, _ := json.Marshal(ov2fields(ov))
		tg := store.Target{
			Platform: plat, FinalText: text, FieldsJSON: string(fields), Status: "scheduled",
		}
		// Pre-split exactly as runChain does at dispatch time, so a scheduled
		// over-limit post fires as a reply-chain (the fire path threads any target
		// with >1 segment) instead of one over-limit post the platform rejects.
		// Media overflow counts too: 10 images on Mastodon is a 3-post chain.
		if len(segTexts) > 1 {
			for i, st := range segTexts {
				tg.Segments = append(tg.Segments, store.Segment{Ordinal: i, Text: st, Status: "pending"})
			}
		}
		rec.Targets = append(rec.Targets, tg)
	}
	if err := d.Store.SavePost(rec); err != nil {
		return nil, err
	}
	return rec, nil
}

// Fire dispatches a scheduled post's targets now (used by the worker). The
// per-attempt AppendTargetAttempt recomputes the post status to its aggregate.
func (d *Dispatcher) Fire(ctx context.Context, postID string) (*store.Post, error) {
	if d.Store == nil {
		return nil, errors.New("fire requires a store")
	}
	post, err := d.Store.GetPost(postID)
	if err != nil {
		return nil, err
	}
	if err := d.dispatchTargets(ctx, post, func(t store.Target) bool {
		return t.Status == "scheduled"
	}); err != nil {
		return nil, err
	}
	post, err = d.Store.GetPost(postID)
	if err != nil {
		return nil, err
	}
	d.notify(ctx, post)
	d.alertFailure(ctx, post)
	return post, nil
}

// Retry re-runs the failed (or missed) targets of an archived post. If platforms
// is non-empty, only those platforms are retried. Successful targets are skipped.
func (d *Dispatcher) Retry(ctx context.Context, postID string, platforms []string) (*store.Post, error) {
	if d.Store == nil {
		return nil, errors.New("retry requires a store")
	}
	post, err := d.Store.GetPost(postID)
	if err != nil {
		return nil, err
	}
	want := map[string]bool{}
	for _, p := range platforms {
		want[p] = true
	}
	if err := d.dispatchTargets(ctx, post, func(t store.Target) bool {
		// 'missed' targets (scheduled posts past the grace window) are re-postable
		// through the same retry path as failed ones. Partial threaded targets
		// (len(Segments) > 1) can also be resumed; single-post partials (e.g. nostr
		// with some relays down) use RetryRelay to avoid duplicating the note.
		retryable := t.Status == "failed" || t.Status == "missed" || (t.Status == "partial" && len(t.Segments) > 1)
		return retryable && (len(platforms) == 0 || want[t.Platform])
	}); err != nil {
		return nil, err
	}
	post, err = d.Store.GetPost(postID)
	if err != nil {
		return nil, err
	}
	d.notify(ctx, post)
	return post, nil
}

// nostrStatusFromRelays derives a Nostr target status from its relay states,
// ignoring skipped (overlay) relays: success = all attempted ok, failed = none
// ok, partial = some ok.
func nostrStatusFromRelays(relays []store.RelayState) string {
	attempted, ok := 0, 0
	for _, r := range relays {
		if r.Status == "skipped" {
			continue
		}
		attempted++
		if r.Status == "ok" {
			ok++
		}
	}
	switch {
	case ok == 0:
		return "failed"
	case ok < attempted:
		return "partial"
	default:
		return "success"
	}
}

// RetryRelay rebroadcasts an archived nostr event to a single failed relay
// (same signed event — no re-mine, no duplicate note) and updates that relay's
// status. Platforms other than nostr have no relay rows and are rejected.
func (d *Dispatcher) RetryRelay(ctx context.Context, postID, relayURL string) (*store.Post, error) {
	if d.Store == nil {
		return nil, errors.New("retry requires a store")
	}
	post, err := d.Store.GetPost(postID)
	if err != nil {
		return nil, err
	}
	var target *store.Target
	for i := range post.Targets {
		if post.Targets[i].Platform == "nostr" {
			target = &post.Targets[i]
			break
		}
	}
	if target == nil {
		return nil, ErrBadRelayRetry
	}
	var relay *store.RelayState
	for i := range target.Relays {
		if target.Relays[i].URL == relayURL {
			relay = &target.Relays[i]
			break
		}
	}
	// Only failed relays are retryable: skipped relays are unreachable, and an
	// ok relay already has the event (the UI only offers retry on failures).
	if relay == nil || relay.Status != "failed" {
		return nil, ErrBadRelayRetry
	}
	if target.SignedEventJSON == "" {
		return nil, errors.New("no stored event to rebroadcast")
	}
	if d.Nostr == nil {
		return nil, errors.New("nostr not configured")
	}
	ok, msg := d.Nostr.RebroadcastToRelay(ctx, target.SignedEventJSON, relayURL)
	status := "failed"
	if ok {
		status = "ok"
	}
	if err := d.Store.UpdateRelayStatus(target.ID, relayURL, status, msg); err != nil {
		return nil, err
	}
	// No notify() here: RetryRelay only rebroadcasts an already-signed Nostr
	// event to one relay; it creates no new post/URL. The narrow case where a
	// partial→success relay flip newly makes a post feed-eligible is
	// intentionally not pinged (the feed still surfaces it on next fetch).
	return d.Store.GetPost(postID)
}

// ErrBadRelayRetry signals an unknown/ineligible relay-retry request (maps to 400).
var ErrBadRelayRetry = errors.New("unknown or non-retryable relay")

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
