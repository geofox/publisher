package dispatch

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	gonostr "fiatjaf.com/nostr"
	"fiatjaf.com/nostr/nip19"
	"github.com/geofox/publisher/internal/bluesky"
	"github.com/geofox/publisher/internal/mastodon"
	pubnostr "github.com/geofox/publisher/internal/nostr"
	"github.com/geofox/publisher/internal/store"
	"github.com/geofox/publisher/internal/threads"
	"github.com/geofox/publisher/internal/transcode"
)

// gateVideo enforces the per-platform video ceilings before any network call.
// Returns "" when all attached videos pass (or none are videos).
func gateVideo(plat string, imgs []Img) string {
	for i, im := range imgs {
		if !im.IsVideo() {
			continue
		}
		size := im.SizeBytes
		if size == 0 {
			size = int64(len(im.Bytes))
		}
		info := transcode.VideoInfo{SizeBytes: size, DurationSecs: im.DurationSecs}
		if fail, _ := transcode.VideoGate(plat, info); fail != "" {
			return fmt.Sprintf("video %d: %s", i, fail)
		}
		if len(im.Bytes) == 0 && (plat == "bluesky" || plat == "mastodon") {
			return fmt.Sprintf("video %d: bytes unavailable for %s (over its size cap or fetch failed)", i, plat)
		}
		if im.BlossomURL == "" && plat == "threads" {
			return fmt.Sprintf("video %d: no canonical URL for threads (ingests by URL)", i)
		}
	}
	return ""
}

// splitVideo returns the first video attachment (nil if none) and the
// remaining image attachments. v1: the composer enforces video XOR images and
// max one video, so images is empty whenever v != nil — the split is defensive.
// Extra videos beyond the first are dropped (composer enforces max 1).
func splitVideo(imgs []Img) (v *Img, images []Img) {
	for i := range imgs {
		if imgs[i].IsVideo() {
			if v == nil {
				v = &imgs[i]
			}
			// extra videos dropped — composer enforces max 1
			continue
		}
		images = append(images, imgs[i])
	}
	return v, images
}

// bskyVideo maps a video attachment to the bluesky client's shape, parsing
// the canonical "WxH" dim into the aspectRatio hint (0,0 when unknown — the
// client then omits aspectRatio rather than sending a bogus one).
func bskyVideo(v *Img) *bluesky.Video {
	w, h := transcode.ParseDim(v.Dim)
	return &bluesky.Video{Bytes: v.Bytes, Alt: v.Alt, W: w, H: h}
}

// Compile-time checks that the adapters satisfy the dispatcher interfaces.
var (
	_ NostrPoster    = NostrAdapter{}
	_ MastodonPoster = MastodonAdapter{}
	_ BlueskyPoster  = BlueskyAdapter{}
	_ ThreadsPoster  = ThreadsAdapter{}
)

type NostrAdapter struct{ P *pubnostr.Publisher }

func (a NostrAdapter) PublishText(ctx context.Context, text string, pow *int, imetas []gonostr.Tag, replyTo *ReplyRef) (TargetResult, error) {
	in := pubnostr.PublishInput{Text: text, POW: pow, Imetas: imetas}
	if replyTo != nil {
		// RelayHint is intentionally left empty: ReplyRef carries no relay hint
		// (NIP-10 hints are optional). Revisit if ReplyRef gains a RelayHint field.
		in.ReplyTo = &pubnostr.NostrReply{RootID: replyTo.RootID, ParentID: replyTo.ParentID, AuthorPubkey: replyTo.AuthorPubkey}
	}
	r := TargetResult{Platform: "nostr"}
	reqB, _ := json.Marshal(map[string]any{"text": text, "pow": pow, "imeta": imetas})
	r.RequestJSON = string(reqB)

	res, err := a.P.Publish(ctx, in)
	out, err := nostrResult(res, err)
	out.RequestJSON = r.RequestJSON
	return out, err
}

// nostrResult maps a nostr PublishResult/err into a normalized TargetResult,
// deriving status from the per-relay outcomes (success/partial/failed). Shared by
// PublishText, Repost and Quote so they agree on relay/signed-event/status mapping.
func nostrResult(res pubnostr.PublishResult, err error) (TargetResult, error) {
	r := TargetResult{Platform: "nostr"}
	if err != nil {
		r.Status, r.Error = "failed", err.Error()
		return r, err
	}
	respB, _ := json.Marshal(res) // result types are JSON-safe; marshal cannot fail
	r.ResponseJSON = string(respB)
	r.RemoteID = res.EventID
	r.SignedEventJSON = res.SignedEvent
	if res.Nevent != "" {
		r.RemoteURL = "https://njump.me/" + res.Nevent
	}

	for _, rr := range res.Relays {
		st := "ok"
		switch {
		case rr.Skipped:
			st = "skipped"
		case !rr.OK:
			st = "failed"
		}
		r.Relays = append(r.Relays, store.RelayState{URL: rr.URL, Status: st, Message: rr.Message})
	}
	r.Status = nostrStatusFromRelays(r.Relays)
	if r.Status == "failed" {
		r.Error = "no relay accepted the event"
	}
	return r, nil
}

func (a NostrAdapter) RebroadcastToRelay(ctx context.Context, signedEventJSON, relayURL string) (bool, string) {
	rr := a.P.RebroadcastToRelay(ctx, signedEventJSON, relayURL)
	return rr.OK, rr.Message
}

type MastodonAdapter struct{ C *mastodon.Client }

func (a MastodonAdapter) PostText(ctx context.Context, text string, o Overrides, imgs []Img, replyTo *ReplyRef) (TargetResult, error) {
	spoiler := firstNonEmpty(o.SpoilerText, o.ContentWarning)
	r := TargetResult{Platform: "mastodon"}
	reqB, _ := json.Marshal(map[string]any{
		"text": text, "visibility": o.Visibility, "language": o.Language, "spoiler_text": spoiler,
	})
	r.RequestJSON = string(reqB)

	if reason := gateVideo("mastodon", imgs); reason != "" {
		r.Status, r.Error = "failed", reason
		return r, fmt.Errorf("%s", reason)
	}
	v, imgRest := splitVideo(imgs)
	fitted, err := fitImgs("mastodon", imgRest)
	if err != nil {
		r.Status, r.Error = "failed", err.Error()
		return r, err
	}
	var mi []mastodon.Image
	for _, im := range fitted {
		mi = append(mi, mastodon.Image{Bytes: im.Bytes, Alt: im.Alt})
	}
	p := mastodon.Post{
		Text: text, SpoilerText: spoiler,
		Sensitive: o.Sensitive, Visibility: o.Visibility, Language: o.Language, Images: mi,
	}
	if v != nil {
		p.Video = &mastodon.Video{Bytes: v.Bytes, Alt: v.Alt}
	}
	if replyTo != nil {
		p.InReplyToID = replyTo.ParentID
	}
	res, err := a.C.Post(ctx, p)
	if err != nil {
		r.Status, r.Error = "failed", err.Error()
		return r, err
	}
	respB, _ := json.Marshal(res) // result types are JSON-safe; marshal cannot fail
	r.Status, r.RemoteID, r.RemoteURL, r.ResponseJSON = "success", res.RemoteID, res.RemoteURL, string(respB)
	return r, nil
}

type BlueskyAdapter struct{ C *bluesky.Client }

func (a BlueskyAdapter) PostBsky(ctx context.Context, text string, o Overrides, imgs []Img, replyTo *ReplyRef) (TargetResult, error) {
	r := TargetResult{Platform: "bluesky"}
	reqB, _ := json.Marshal(map[string]any{
		"text": text, "langs": o.Langs,
		"bluesky_reply": o.BlueskyReply, "bluesky_disable_quotes": o.BlueskyDisableQuotes,
	})
	r.RequestJSON = string(reqB)

	if reason := gateVideo("bluesky", imgs); reason != "" {
		r.Status, r.Error = "failed", reason
		return r, fmt.Errorf("%s", reason)
	}
	v, rest := splitVideo(imgs)
	var bi []bluesky.Image
	for _, im := range rest {
		bi = append(bi, bluesky.Image{Bytes: im.Bytes, Mime: im.Mime, Alt: im.Alt})
	}

	bp := bluesky.Post{
		Text: text, Langs: o.Langs, Images: bi,
		ReplyGate:     bluesky.ParseReplyGate(o.BlueskyReply),
		DisableQuotes: o.BlueskyDisableQuotes,
	}
	if v != nil {
		bp.Video = bskyVideo(v)
	}
	if replyTo != nil {
		bp.Reply = &bluesky.ReplyRef{
			RootURI: replyTo.RootID, RootCID: replyTo.RootCID,
			ParentURI: replyTo.ParentID, ParentCID: replyTo.ParentCID,
		}
	}
	if o.LinkCard != nil {
		ext := &bluesky.ExternalCard{
			URI: o.LinkCard.URI, Title: o.LinkCard.Title, Description: o.LinkCard.Description,
			Thumb: o.LinkCard.ThumbData, ThumbMime: o.LinkCard.ThumbMime,
		}
		for _, ref := range o.LinkCard.Refs {
			ext.Refs = append(ext.Refs, bluesky.ExternalRef{URI: ref.URI, CID: ref.CID})
		}
		bp.External = ext
	}
	res, err := a.C.Post(ctx, bp)
	// Post returns the published-post Result even when a gate write fails, so
	// record the link regardless.
	r.RemoteID, r.RemoteURL, r.CID = res.RemoteID, res.RemoteURL, res.CID
	if respB, mErr := json.Marshal(res); mErr == nil {
		r.ResponseJSON = string(respB)
	}
	if err != nil {
		r.Error = err.Error()
		// If the post itself published (link present) and only a reply/quote
		// gate write failed, the post is live-but-ungated → partial, not failed.
		if res.RemoteID != "" {
			r.Status = "partial"
		} else {
			r.Status = "failed"
		}
		return r, err
	}
	r.Status = "success"
	return r, nil
}

type ThreadsAdapter struct {
	C *threads.Client
	// Host uploads derived variant bytes and returns a public URL (wired to
	// the media pipeline in main). Nil disables fitting (tests, degraded
	// boot) — Threads then receives canonical URLs as before this feature.
	Host func(ctx context.Context, body []byte, mime string) (string, error)
}

func (a ThreadsAdapter) PostThreads(ctx context.Context, text string, o Overrides, imgs []Img, replyTo *ReplyRef) (TargetResult, error) {
	r := TargetResult{Platform: "threads"}
	reqB, _ := json.Marshal(map[string]any{
		"text": text, "topic_tag": o.TopicTag, "images": len(imgs), "reply_control": o.ThreadsReplyControl,
	})
	r.RequestJSON = string(reqB)

	if reason := gateVideo("threads", imgs); reason != "" {
		r.Status, r.Error = "failed", reason
		return r, fmt.Errorf("%s", reason)
	}
	vid, imgRest := splitVideo(imgs)
	ti, err := prepThreadsImgs(ctx, imgRest, a.Host)
	if err != nil {
		r.Status, r.Error = "failed", err.Error()
		return r, err
	}

	tp := threads.Post{Text: text, TopicTag: o.TopicTag, Images: ti, ReplyControl: o.ThreadsReplyControl}
	if vid != nil {
		tp.Video = &threads.Video{URL: vid.BlossomURL, Alt: vid.Alt}
	}
	if replyTo != nil {
		tp.ReplyToID = replyTo.ParentID
	}
	res, err := a.C.Post(ctx, tp)
	if err != nil {
		r.Status, r.Error = "failed", err.Error()
		return r, err
	}
	respB, _ := json.Marshal(res) // Result is JSON-safe
	r.Status, r.RemoteID, r.RemoteURL, r.ResponseJSON = "success", res.RemoteID, res.RemoteURL, string(respB)
	return r, nil
}

func (a BlueskyAdapter) RepostBsky(ctx context.Context, uri, cid string) (TargetResult, error) {
	res, err := a.C.Repost(ctx, uri, cid)
	if err != nil {
		return TargetResult{Platform: "bluesky"}, err
	}
	return TargetResult{Platform: "bluesky", Status: "success", RemoteID: res.RemoteID, RemoteURL: res.RemoteURL, CID: res.CID}, nil
}

func (a BlueskyAdapter) QuoteBsky(ctx context.Context, text string, o Overrides, imgs []Img, uri, cid string) (TargetResult, error) {
	r := TargetResult{Platform: "bluesky"}
	if reason := gateVideo("bluesky", imgs); reason != "" {
		r.Status, r.Error = "failed", reason
		return r, fmt.Errorf("%s", reason)
	}
	v, rest := splitVideo(imgs)
	var bi []bluesky.Image
	for _, im := range rest {
		bi = append(bi, bluesky.Image{Bytes: im.Bytes, Mime: im.Mime, Alt: im.Alt})
	}
	bp := bluesky.Post{
		Text: text, Langs: o.Langs, Images: bi, Quote: &bluesky.QuoteRef{URI: uri, CID: cid},
		ReplyGate: bluesky.ParseReplyGate(o.BlueskyReply), DisableQuotes: o.BlueskyDisableQuotes,
	}
	if v != nil {
		bp.Video = bskyVideo(v)
	}
	res, err := a.C.Post(ctx, bp)
	if err != nil {
		return TargetResult{Platform: "bluesky"}, err
	}
	return TargetResult{Platform: "bluesky", Status: "success", RemoteID: res.RemoteID, RemoteURL: res.RemoteURL, CID: res.CID}, nil
}

func (a MastodonAdapter) Reblog(ctx context.Context, id string) (TargetResult, error) {
	res, err := a.C.Reblog(ctx, id)
	if err != nil {
		return TargetResult{Platform: "mastodon"}, err
	}
	return TargetResult{Platform: "mastodon", Status: "success", RemoteID: res.RemoteID, RemoteURL: res.RemoteURL}, nil
}

func (a MastodonAdapter) QuoteStatus(ctx context.Context, text, quotedID string, imgs []Img) (TargetResult, error) {
	if reason := gateVideo("mastodon", imgs); reason != "" {
		r := TargetResult{Platform: "mastodon", Status: "failed", Error: reason}
		return r, fmt.Errorf("%s", reason)
	}
	v, imgRest := splitVideo(imgs)
	fitted, err := fitImgs("mastodon", imgRest)
	if err != nil {
		r := TargetResult{Platform: "mastodon", Status: "failed", Error: err.Error()}
		return r, err
	}
	var mi []mastodon.Image
	for _, im := range fitted {
		mi = append(mi, mastodon.Image{Bytes: im.Bytes, Alt: im.Alt})
	}
	// v1: mastodon quotes are text+images only — an attached video is dropped here
	// (bluesky quotes carry it natively); the composer's video XOR images rule makes
	// this unreachable in practice.
	_ = v
	res, err := a.C.QuotePost(ctx, text, quotedID, mi)
	if err != nil {
		return TargetResult{Platform: "mastodon"}, err
	}
	return TargetResult{Platform: "mastodon", Status: "success", RemoteID: res.RemoteID, RemoteURL: res.RemoteURL}, nil
}

// Repost publishes a NIP-18 repost: kind 6 for a kind-1 note, kind 16 (generic
// repost, carrying a "k" tag) for any other kind.
func (a NostrAdapter) Repost(ctx context.Context, eventID, author string, kind int, relayHint string) (TargetResult, error) {
	k := 6
	tags := []gonostr.Tag{{"e", eventID, relayHint}, {"p", author}}
	if kind != 1 {
		k = 16
		tags = append(tags, gonostr.Tag{"k", strconv.Itoa(kind)})
	}
	res, err := a.P.Publish(ctx, pubnostr.PublishInput{Kind: k, Text: "", Tags: tags})
	return nostrResult(res, err)
}

// Quote publishes a NIP-18 quote (kind 1 with a "q" tag), appending an
// nostr:nevent mention of the quoted event to the commentary.
func (a NostrAdapter) Quote(ctx context.Context, text, eventID, author, relayHint string, imetas []gonostr.Tag) (TargetResult, error) {
	content := strings.TrimSpace(text)
	if mention := neventMention(eventID, author, relayHint); mention != "" {
		if content == "" {
			content = mention
		} else {
			content = content + "\n" + mention
		}
	}
	tags := []gonostr.Tag{{"q", eventID, relayHint, author}}
	res, err := a.P.Publish(ctx, pubnostr.PublishInput{Kind: 1, Text: content, Tags: tags, Imetas: imetas})
	return nostrResult(res, err)
}

// neventMention builds a "nostr:nevent…" mention for the quoted event, or "" on
// error. nip19.EncodeNevent returns "" on failure (it has no error return), so a
// bad event id simply yields no mention.
func neventMention(eventID, author, relayHint string) string {
	id, err := gonostr.IDFromHex(eventID)
	if err != nil {
		return ""
	}
	var relays []string
	if relayHint != "" {
		relays = []string{relayHint}
	}
	pk, _ := gonostr.PubKeyFromHex(author) // zero pk is acceptable to EncodeNevent
	nevent := nip19.EncodeNevent(id, relays, pk)
	if nevent == "" {
		return ""
	}
	return "nostr:" + nevent
}

// fitImgs re-encodes any image violating plat's transcode profile, leaving
// fitting images (and platforms without a profile) untouched. Bluesky fits
// inside bluesky.Post (fitBlob) and Threads hosts a variant instead — see
// prepThreadsImgs. An image that cannot be fitted fails the whole target:
// better a recorded per-target error the retrier can redrive than a
// guaranteed platform-side rejection.
func fitImgs(plat string, imgs []Img) ([]Img, error) {
	prof, ok := transcode.ProfileFor(plat)
	if !ok {
		return imgs, nil
	}
	out := make([]Img, len(imgs))
	for i, im := range imgs {
		if im.IsVideo() {
			out[i] = im
			continue
		}
		out[i] = im
		r, err := prof.Fit(im.Bytes, im.Mime)
		if err != nil {
			return nil, fmt.Errorf("fit image %d for %s: %w", i, plat, err)
		}
		if r.Changed {
			out[i].Bytes, out[i].Mime = r.Bytes, r.Mime
		}
	}
	return out, nil
}

// prepThreadsImgs maps dispatch images to the URL+Alt pairs Threads consumes.
// Threads ingests by URL (Meta fetches it), so an image violating the Threads
// profile is re-encoded and re-hosted via host, and Meta gets the variant URL.
// Blossom is content-addressed (sha256 keys): the deterministic re-encode
// yields the same bytes → same URL on every (re)dispatch, so retries converge
// on one stable variant URL. The work itself is NOT skipped (variant shas have
// no media row, so the pipeline's Lookup can't short-circuit) — each redrive
// re-encodes and re-PUTs; bounded by the retrier's attempt cap. nil host keeps
// the pre-feature behavior (canonical URLs, no fitting).
func prepThreadsImgs(ctx context.Context, imgs []Img, host func(ctx context.Context, body []byte, mime string) (string, error)) ([]threads.Image, error) {
	var ti []threads.Image
	for i, im := range imgs {
		if im.IsVideo() {
			continue // videos consumed separately by the video container; not in the image list
		}
		url := im.BlossomURL
		if host != nil {
			r, err := transcode.Threads.Fit(im.Bytes, im.Mime)
			if err != nil {
				return nil, fmt.Errorf("fit image %d for threads: %w", i, err)
			}
			if r.Changed {
				u, herr := host(ctx, r.Bytes, r.Mime)
				if herr != nil {
					return nil, fmt.Errorf("host threads variant %d: %w", i, herr)
				}
				url = u
			}
		}
		ti = append(ti, threads.Image{URL: url, Alt: im.Alt})
	}
	return ti, nil
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
