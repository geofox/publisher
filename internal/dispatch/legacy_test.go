package dispatch

import (
	"context"
	"encoding/json"
	"errors"

	gonostr "fiatjaf.com/nostr"

	"github.com/geofox/publisher/internal/bluesky"
	"github.com/geofox/publisher/internal/destination"
	"github.com/geofox/publisher/internal/mastodon"
	"github.com/geofox/publisher/internal/nostr"
	"github.com/geofox/publisher/internal/store"
	"github.com/geofox/publisher/internal/threads"
	"github.com/geofox/publisher/internal/unfurl"
)

// The dispatcher's unit tests predate the destination contract: their fakes
// implement the per-platform interfaces the dispatcher used to call. This
// file adapts those fakes to destination.Account, so the tests drive the real
// contract path (request building, media preparation, normalization, thread
// refs) with their original assertions. New tests should implement
// destination.Account directly (see contract_test.go).

// ReplyRef is the old thread/reply reference the fakes receive.
type ReplyRef struct {
	RootID, RootCID, ParentID, ParentCID string
	AuthorPubkey                         string
}

// legacyResult is the old adapter result the fakes return.
type legacyResult struct {
	Platform        string
	Status          string // success|partial|failed
	Error           string
	RemoteID        string
	RemoteURL       string
	CID             string
	LatencyMS       int
	RequestJSON     string
	ResponseJSON    string
	Relays          []store.RelayState
	SignedEventJSON string
	PendingRelays   []string
}

type NostrPoster interface {
	PublishText(ctx context.Context, text string, pow *int, imetas []gonostr.Tag, replyTo *ReplyRef) (legacyResult, error)
	RebroadcastToRelay(ctx context.Context, signedEventJSON, relayURL string) (ok bool, message string)
	Repost(ctx context.Context, eventID, author string, kind int, relayHint string) (legacyResult, error)
	Quote(ctx context.Context, text, eventID, author, relayHint string, imetas []gonostr.Tag) (legacyResult, error)
}

type MastodonPoster interface {
	PostText(ctx context.Context, text string, o Overrides, imgs []Img, replyTo *ReplyRef) (legacyResult, error)
	Reblog(ctx context.Context, statusID string) (legacyResult, error)
	QuoteStatus(ctx context.Context, text, quotedID string, imgs []Img) (legacyResult, error)
}

type BlueskyPoster interface {
	PostBsky(ctx context.Context, text string, o Overrides, imgs []Img, replyTo *ReplyRef) (legacyResult, error)
	RepostBsky(ctx context.Context, subjectURI, subjectCID string) (legacyResult, error)
	QuoteBsky(ctx context.Context, text string, o Overrides, imgs []Img, quoteURI, quoteCID string) (legacyResult, error)
}

type ThreadsPoster interface {
	PostThreads(ctx context.Context, text string, o Overrides, imgs []Img, replyTo *ReplyRef) (legacyResult, error)
}

// legacy names the fakes a test wires, by platform.
type legacy struct {
	Nostr    NostrPoster
	Mastodon MastodonPoster
	Bluesky  BlueskyPoster
	Threads  ThreadsPoster
}

// legacyAccounts wraps the set fakes as accounts that speak the real drivers'
// manifests, each its type's test account.
func legacyAccounts(l legacy) *testRegistry {
	out := map[string]destination.Account{}
	if l.Nostr != nil {
		out[nostr.Type] = &legacyNostr{l.Nostr}
	}
	if l.Mastodon != nil {
		out[mastodon.Type] = &legacyMastodon{l.Mastodon}
	}
	if l.Bluesky != nil {
		out[bluesky.Type] = &legacyBluesky{l.Bluesky}
	}
	if l.Threads != nil {
		out[threads.Type] = &legacyThreads{l.Threads}
	}
	return registryOf(out)
}

// toResult converts a fake's result the way the old dispatcher read it.
func toResult(r legacyResult, err error) (destination.PublishResult, error) {
	out := destination.PublishResult{
		Ref:     destination.ItemRef{ID: r.RemoteID, URL: r.RemoteURL},
		Pending: r.PendingRelays,
		Trace:   destination.Trace{Request: r.RequestJSON, Response: r.ResponseJSON},
	}
	switch {
	case r.SignedEventJSON != "":
		out.Ref.Data = json.RawMessage(r.SignedEventJSON)
	case r.CID != "":
		out.Ref.Data = bluesky.RefData(r.CID)
	}
	for _, rl := range r.Relays {
		out.Deliveries = append(out.Deliveries, destination.Delivery{Endpoint: rl.URL, Status: rl.Status, Message: rl.Message})
	}
	switch r.Status {
	case "success":
		out.Status = destination.StatusPublished
	case "partial":
		out.Status = destination.StatusPartial
	default:
		out.Status = destination.StatusFailed
	}
	if err == nil && r.Error != "" {
		err = errors.New(r.Error)
	}
	return out, err
}

// cidOf reads the Bluesky CID back out of ItemRef.Data.
func cidOf(data json.RawMessage) string {
	var d struct {
		CID string `json:"cid"`
	}
	_ = json.Unmarshal(data, &d)
	return d.CID
}

func legacyReply(req destination.PublishRequest) *ReplyRef {
	switch {
	case req.InReplyTo != nil:
		tr := req.InReplyTo
		return &ReplyRef{RootID: tr.Root.ID, RootCID: cidOf(tr.Root.Data), ParentID: tr.Parent.ID, ParentCID: cidOf(tr.Parent.Data)}
	case req.ReplyTo != nil:
		s := req.ReplyTo
		switch s.Type {
		case bluesky.Type:
			root, rootCID := s.ReplyRootURI, s.ReplyRootCID
			if root == "" {
				root, rootCID = s.URI, s.CID
			}
			return &ReplyRef{RootID: root, RootCID: rootCID, ParentID: s.URI, ParentCID: s.CID}
		case mastodon.Type:
			return &ReplyRef{ParentID: s.LocalID}
		case nostr.Type:
			return &ReplyRef{RootID: s.EventID, ParentID: s.EventID, AuthorPubkey: s.Author}
		}
	}
	return nil
}

func legacyImgs(items []destination.MediaItem) []Img {
	var out []Img
	for _, m := range items {
		out = append(out, Img{Bytes: m.Bytes, Mime: m.Mime, Alt: m.Alt, BlossomURL: m.URL, SHA256: m.SHA256,
			DurationSecs: m.DurationSecs, SizeBytes: m.SizeBytes, Blurhash: m.Blurhash, PosterURL: m.PosterURL})
	}
	return out
}

func legacyOverrides(req destination.PublishRequest) Overrides {
	ov := Overrides{
		Visibility: req.Visibility, Langs: req.Languages, Sensitive: req.Sensitive,
		SpoilerText: req.ContentWarning, ContentWarning: req.ContentWarning,
		TopicTag:             destination.OptString(req.Options, "topic_tag"),
		ThreadsReplyControl:  destination.OptString(req.Options, "reply_control"),
		BlueskyReply:         destination.OptString(req.Options, "reply_gate"),
		BlueskyDisableQuotes: destination.OptBool(req.Options, "disable_quotes"),
	}
	if len(req.Languages) > 0 {
		ov.Language = req.Languages[0]
	}
	if n, ok := destination.OptInt(req.Options, "pow"); ok {
		ov.POW = &n
	}
	if c := req.LinkCard; c != nil {
		card := &unfurl.Card{URI: c.URL, Title: c.Title, Description: c.Description, ThumbData: c.Thumb, ThumbMime: c.ThumbMime}
		for _, r := range c.Records {
			card.Refs = append(card.Refs, unfurl.StrongRef{URI: r.URI, CID: r.CID})
		}
		ov.LinkCard = card
	}
	return ov
}

type legacyNostr struct{ p NostrPoster }

func (a *legacyNostr) Manifest() destination.Manifest { return nostr.Driver{}.Manifest() }
func (a *legacyNostr) Capabilities(context.Context) destination.Capabilities {
	return nostr.Driver{}.Manifest().Defaults
}
func (a *legacyNostr) Publish(ctx context.Context, req destination.PublishRequest) (destination.PublishResult, error) {
	imetas := nostr.Imetas(req.Media)
	if q := req.Quote; q != nil {
		return toResult(a.p.Quote(ctx, req.Text, q.EventID, q.Author, firstOf(q.RelayHints), imetas))
	}
	var pow *int
	if n, ok := destination.OptInt(req.Options, "pow"); ok {
		pow = &n
	}
	return toResult(a.p.PublishText(ctx, req.Text, pow, imetas, legacyReply(req)))
}
func (a *legacyNostr) Repost(ctx context.Context, s destination.SourceRef) (destination.PublishResult, error) {
	return toResult(a.p.Repost(ctx, s.EventID, s.Author, s.EventKind, firstOf(s.RelayHints)))
}
func (a *legacyNostr) Redeliver(ctx context.Context, ref destination.ItemRef, endpoint string) destination.Delivery {
	ok, msg := a.p.RebroadcastToRelay(ctx, string(ref.Data), endpoint)
	d := destination.Delivery{Endpoint: endpoint, Status: "failed", Message: msg}
	if ok {
		d.Status = "ok"
	}
	return d
}

type legacyMastodon struct{ p MastodonPoster }

func (a *legacyMastodon) Manifest() destination.Manifest { return mastodon.Driver{}.Manifest() }
func (a *legacyMastodon) Capabilities(context.Context) destination.Capabilities {
	return mastodon.Driver{}.Manifest().Defaults
}
func (a *legacyMastodon) Publish(ctx context.Context, req destination.PublishRequest) (destination.PublishResult, error) {
	if q := req.Quote; q != nil {
		return toResult(a.p.QuoteStatus(ctx, req.Text, q.LocalID, legacyImgs(req.Media)))
	}
	return toResult(a.p.PostText(ctx, req.Text, legacyOverrides(req), legacyImgs(req.Media), legacyReply(req)))
}
func (a *legacyMastodon) Repost(ctx context.Context, s destination.SourceRef) (destination.PublishResult, error) {
	return toResult(a.p.Reblog(ctx, s.LocalID))
}

type legacyBluesky struct{ p BlueskyPoster }

func (a *legacyBluesky) Manifest() destination.Manifest { return bluesky.Driver{}.Manifest() }
func (a *legacyBluesky) Capabilities(context.Context) destination.Capabilities {
	return bluesky.Driver{}.Manifest().Defaults
}
func (a *legacyBluesky) Publish(ctx context.Context, req destination.PublishRequest) (destination.PublishResult, error) {
	if q := req.Quote; q != nil {
		return toResult(a.p.QuoteBsky(ctx, req.Text, legacyOverrides(req), legacyImgs(req.Media), q.URI, q.CID))
	}
	return toResult(a.p.PostBsky(ctx, req.Text, legacyOverrides(req), legacyImgs(req.Media), legacyReply(req)))
}
func (a *legacyBluesky) Repost(ctx context.Context, s destination.SourceRef) (destination.PublishResult, error) {
	return toResult(a.p.RepostBsky(ctx, s.URI, s.CID))
}

type legacyThreads struct{ p ThreadsPoster }

func (a *legacyThreads) Manifest() destination.Manifest { return threads.Driver{}.Manifest() }
func (a *legacyThreads) Capabilities(context.Context) destination.Capabilities {
	return threads.Driver{}.Manifest().Defaults
}
func (a *legacyThreads) Publish(ctx context.Context, req destination.PublishRequest) (destination.PublishResult, error) {
	return toResult(a.p.PostThreads(ctx, req.Text, legacyOverrides(req), legacyImgs(req.Media), legacyReply(req)))
}

func firstOf(s []string) string {
	if len(s) > 0 {
		return s[0]
	}
	return ""
}

// runPlatform publishes one item the way the old dispatcher's runPlatform
// did: a plain post, or a reply threaded by an old-style ReplyRef.
func (d *Dispatcher) runPlatform(ctx context.Context, plat, text string, ov Overrides, imgs []Img, _ []gonostr.Tag, replyTo *ReplyRef) TargetResult {
	it := item{text: text, ov: ov, imgs: imgs}
	if replyTo != nil {
		if replyTo.AuthorPubkey != "" { // an external reply
			it.head = &headSpec{reply: &InteractRef{EventID: replyTo.ParentID, Author: replyTo.AuthorPubkey}}
		} else {
			it.thread = &destination.ThreadRef{
				Root:   destination.ItemRef{ID: replyTo.RootID, Data: bluesky.RefData(replyTo.RootCID)},
				Parent: destination.ItemRef{ID: replyTo.ParentID, Data: bluesky.RefData(replyTo.ParentCID)},
			}
		}
	}
	return d.publishItem(ctx, acct(plat), plat, it)
}

// runAction runs a repost or a single-item quote, as the old runAction did.
func (d *Dispatcher) runAction(ctx context.Context, action, plat, text string, ov Overrides, imgs []Img, _ []gonostr.Tag, ref InteractRef) TargetResult {
	if action == actionRepost {
		return d.repost(ctx, acct(plat), plat, ref)
	}
	return d.publishItem(ctx, acct(plat), plat, item{text: text, ov: ov, imgs: imgs, head: &headSpec{quote: &ref}})
}

// testImgs returns n small images with bytes, as the composer would send.
func testImgs(n int) []Img {
	out := make([]Img, n)
	for i := range out {
		out[i] = Img{Bytes: []byte("img" + itoa(i)), Mime: "image/png", BlossomURL: "https://b/" + itoa(i)}
	}
	return out
}
