package dispatch

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"sync"
	"time"

	gonostr "fiatjaf.com/nostr"
	"github.com/geofox/publisher/internal/media"
	"github.com/geofox/publisher/internal/store"
)

type TargetResult struct {
	Platform        string
	Status          string // success|partial|failed
	Error           string
	RemoteID        string
	RemoteURL       string
	LatencyMS       int
	RequestJSON     string
	ResponseJSON    string
	Relays          []store.RelayState
	SignedEventJSON string
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
}

type Img struct {
	Bytes      []byte
	Mime       string
	Alt        string
	BlossomURL string
}

// Adapters should populate Status ("success"|"failed") and Error in the returned
// TargetResult. The dispatcher defensively normalizes an empty status to "failed"
// (using the returned error for the message) so a misbehaving adapter can't be
// silently counted as success.
type NostrPoster interface {
	PublishText(ctx context.Context, text string, pow *int, imetas []gonostr.Tag) (TargetResult, error)
	RebroadcastToRelay(ctx context.Context, signedEventJSON, relayURL string) (ok bool, message string)
}
type MastodonPoster interface {
	PostText(ctx context.Context, text string, o Overrides, imgs []Img) (TargetResult, error)
}
type BlueskyPoster interface {
	PostBsky(ctx context.Context, text string, o Overrides, imgs []Img) (TargetResult, error)
}
type ThreadsPoster interface {
	PostThreads(ctx context.Context, text string, o Overrides, imgs []Img) (TargetResult, error)
}

type PostSpec struct {
	MasterText   string
	Platforms    []string
	DelaySeconds int
	Source       string
	Overrides    map[string]Overrides // keyed by platform
	Images       []Img
	MediaRecords []store.Media // already uploaded to Blossom, for archival
}

type Fetcher interface {
	Fetch(ctx context.Context, url string) (data []byte, mime string, err error)
}

type Dispatcher struct {
	Nostr    NostrPoster
	Mastodon MastodonPoster
	Bluesky  BlueskyPoster
	Threads  ThreadsPoster
	Store    *store.Store // may be nil in unit tests
	Fetcher  Fetcher
}

// runPlatform executes one platform and returns a normalized TargetResult.
func (d *Dispatcher) runPlatform(ctx context.Context, plat, text string, ov Overrides, imgs []Img, imetas []gonostr.Tag) TargetResult {
	start := time.Now()
	var r TargetResult
	var err error
	switch plat {
	case "nostr":
		if d.Nostr != nil {
			r, err = d.Nostr.PublishText(ctx, text, ov.POW, imetas)
		} else {
			err = errors.New("nostr not configured")
		}
	case "mastodon":
		if d.Mastodon != nil {
			r, err = d.Mastodon.PostText(ctx, text, ov, imgs)
		} else {
			err = errors.New("mastodon not configured")
		}
	case "bluesky":
		if d.Bluesky != nil {
			r, err = d.Bluesky.PostBsky(ctx, text, ov, imgs)
		} else {
			err = errors.New("bluesky not configured")
		}
	case "threads":
		if d.Threads != nil {
			r, err = d.Threads.PostThreads(ctx, text, ov, imgs)
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
	return r
}

func newID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// buildImetas rebuilds NIP-92 imeta tags (one per attached image) from the
// archived media records, so a Nostr cross-post embeds the same media the
// upload pipeline produced. Records without a Blossom URL are skipped.
func buildImetas(recs []store.Media) []gonostr.Tag {
	var out []gonostr.Tag
	for _, m := range recs {
		if m.BlossomURL == "" {
			continue
		}
		out = append(out, media.ImetaTag(m.BlossomURL, m.Mime, m.SHA256, m.Dim, m.Blurhash))
	}
	return out
}

func (d *Dispatcher) Post(ctx context.Context, spec PostSpec) *store.Post {
	platforms := dedupStrings(spec.Platforms)
	rec := &store.Post{
		ID: newID(), CreatedAt: time.Now().UTC(), MasterText: spec.MasterText,
		Platforms: platforms, DelaySeconds: spec.DelaySeconds, Source: spec.Source,
		Media: spec.MediaRecords,
	}

	imetas := buildImetas(spec.MediaRecords)

	// One result slot per platform, written by its own goroutine (distinct
	// indices → no mutex, and rec.Targets stays in platforms order).
	results := make([]TargetResult, len(platforms))
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
			results[i] = d.runPlatform(ctx, plat, text, ov, spec.Images, imetas)
		}(i, plat, text, ov)
	}
	wg.Wait()

	succ, failed := 0, 0
	for _, r := range results {
		fields, _ := json.Marshal(ov2fields(spec.Overrides[r.Platform]))
		tg := store.Target{
			Platform: r.Platform, FinalText: finalText(spec, r.Platform), FieldsJSON: string(fields),
			Status: r.Status, RemoteID: r.RemoteID, RemoteURL: r.RemoteURL, LatencyMS: r.LatencyMS,
			Relays: r.Relays, SignedEventJSON: r.SignedEventJSON,
			Attempts: []store.Attempt{{
				AttemptNo: 1, Status: r.Status, Error: r.Error, LatencyMS: r.LatencyMS, RemoteID: r.RemoteID,
				RequestJSON: r.RequestJSON, ResponseJSON: r.ResponseJSON, AttemptedAt: time.Now().UTC(),
			}},
		}
		rec.Targets = append(rec.Targets, tg)
		switch r.Status {
		case "success":
			succ++
		case "failed":
			failed++
		}
	}
	// Partial-aware aggregation, mirroring store.recomputeStatus so the in-memory
	// rec.Status (returned to the API/modal) and the persisted status agree: a
	// partial target (e.g. nostr with some relays down) keeps the post partial.
	total := len(results)
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
			slog.Error("savepost failed", "post_id", rec.ID, "err", err)
		}
	}
	return rec
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
	}
}

// dispatchTargets re-pulls media once, then dispatches every target the want
// predicate accepts (reconstructing overrides from FieldsJSON) and appends each
// attempt. Shared by Retry (failed targets) and Fire (scheduled targets).
// AppendTargetAttempt recomputes the post status per attempt.
func (d *Dispatcher) dispatchTargets(ctx context.Context, post *store.Post, want func(store.Target) bool) error {
	var imgs []Img
	for _, m := range post.Media {
		var data []byte
		var mime string
		if d.Fetcher != nil {
			data, mime, _ = d.Fetcher.Fetch(ctx, m.BlossomURL) // best-effort; Bluesky/Mastodon need bytes
		}
		imgs = append(imgs, Img{Bytes: data, Mime: mime, Alt: m.Alt, BlossomURL: m.BlossomURL})
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
		r := d.runPlatform(ctx, tg.Platform, tg.FinalText, ov, imgs, imetas)
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
	for _, plat := range platforms {
		// ov2fields returns a map of JSON-safe values only, so Marshal can't fail.
		fields, _ := json.Marshal(ov2fields(spec.Overrides[plat]))
		rec.Targets = append(rec.Targets, store.Target{
			Platform: plat, FinalText: finalText(spec, plat), FieldsJSON: string(fields), Status: "scheduled",
		})
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
	return d.Store.GetPost(postID)
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
		// through the same retry path as failed ones.
		return (t.Status == "failed" || t.Status == "missed") && (len(platforms) == 0 || want[t.Platform])
	}); err != nil {
		return nil, err
	}
	return d.Store.GetPost(postID)
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
