package dispatch

import (
	"context"
	"encoding/json"

	gonostr "fiatjaf.com/nostr"
	"github.com/geofox/publisher/internal/bluesky"
	"github.com/geofox/publisher/internal/mastodon"
	pubnostr "github.com/geofox/publisher/internal/nostr"
	"github.com/geofox/publisher/internal/store"
	"github.com/geofox/publisher/internal/threads"
)

// Compile-time checks that the adapters satisfy the dispatcher interfaces.
var (
	_ NostrPoster    = NostrAdapter{}
	_ MastodonPoster = MastodonAdapter{}
	_ BlueskyPoster  = BlueskyAdapter{}
	_ ThreadsPoster  = ThreadsAdapter{}
)

type NostrAdapter struct{ P *pubnostr.Publisher }

func (a NostrAdapter) PublishText(ctx context.Context, text string, pow *int, imetas []gonostr.Tag) (TargetResult, error) {
	in := pubnostr.PublishInput{Text: text, POW: pow, Imetas: imetas}
	r := TargetResult{Platform: "nostr"}
	reqB, _ := json.Marshal(map[string]any{"text": text, "pow": pow, "imeta": imetas})
	r.RequestJSON = string(reqB)

	res, err := a.P.Publish(ctx, in)
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

func (a MastodonAdapter) PostText(ctx context.Context, text string, o Overrides, imgs []Img) (TargetResult, error) {
	var mi []mastodon.Image
	for _, im := range imgs {
		mi = append(mi, mastodon.Image{Bytes: im.Bytes, Alt: im.Alt})
	}
	p := mastodon.Post{
		Text: text, SpoilerText: firstNonEmpty(o.SpoilerText, o.ContentWarning),
		Sensitive: o.Sensitive, Visibility: o.Visibility, Language: o.Language, Images: mi,
	}
	r := TargetResult{Platform: "mastodon"}
	reqB, _ := json.Marshal(map[string]any{
		"text": text, "visibility": o.Visibility, "language": o.Language, "spoiler_text": p.SpoilerText,
	})
	r.RequestJSON = string(reqB)

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

func (a BlueskyAdapter) PostBsky(ctx context.Context, text string, o Overrides, imgs []Img) (TargetResult, error) {
	var bi []bluesky.Image
	for _, im := range imgs {
		bi = append(bi, bluesky.Image{Bytes: im.Bytes, Mime: im.Mime, Alt: im.Alt})
	}
	r := TargetResult{Platform: "bluesky"}
	reqB, _ := json.Marshal(map[string]any{
		"text": text, "langs": o.Langs,
		"bluesky_reply": o.BlueskyReply, "bluesky_disable_quotes": o.BlueskyDisableQuotes,
	})
	r.RequestJSON = string(reqB)

	res, err := a.C.Post(ctx, bluesky.Post{
		Text: text, Langs: o.Langs, Images: bi,
		ReplyGate:     bluesky.ParseReplyGate(o.BlueskyReply),
		DisableQuotes: o.BlueskyDisableQuotes,
	})
	// Post returns the published-post Result even when a gate write fails, so
	// record the link regardless.
	r.RemoteID, r.RemoteURL = res.RemoteID, res.RemoteURL
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

type ThreadsAdapter struct{ C *threads.Client }

func (a ThreadsAdapter) PostThreads(ctx context.Context, text string, o Overrides, imgs []Img) (TargetResult, error) {
	var ti []threads.Image
	for _, im := range imgs {
		ti = append(ti, threads.Image{URL: im.BlossomURL, Alt: im.Alt})
	}
	r := TargetResult{Platform: "threads"}
	reqB, _ := json.Marshal(map[string]any{
		"text": text, "topic_tag": o.TopicTag, "images": len(ti), "reply_control": o.ThreadsReplyControl,
	})
	r.RequestJSON = string(reqB)

	res, err := a.C.Post(ctx, threads.Post{Text: text, TopicTag: o.TopicTag, Images: ti, ReplyControl: o.ThreadsReplyControl})
	if err != nil {
		r.Status, r.Error = "failed", err.Error()
		return r, err
	}
	respB, _ := json.Marshal(res) // Result is JSON-safe
	r.Status, r.RemoteID, r.RemoteURL, r.ResponseJSON = "success", res.RemoteID, res.RemoteURL, string(respB)
	return r, nil
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
