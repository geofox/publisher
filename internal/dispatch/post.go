package dispatch

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"
	"time"

	"github.com/geofox/publisher/internal/destination"
	"github.com/geofox/publisher/internal/logging"
	"github.com/geofox/publisher/internal/media"
	"github.com/geofox/publisher/internal/progress"
	"github.com/geofox/publisher/internal/store"
	"github.com/geofox/publisher/internal/unfurl"
)

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

// attachLinkCard unfurls a link card for every targeted destination that
// builds link previews itself (LinkPreview "client"): the card URL (trailing,
// else first) of that target's effective text goes into its override's
// LinkCard. Best-effort: on any unfurl error the post proceeds without a card
// (a bare faceted link). Placement and trailing-strip happen in PlanTarget.
func (d *Dispatcher) attachLinkCard(ctx context.Context, spec *PostSpec) {
	if d.Unfurler == nil {
		return
	}
	cards := map[string]*unfurl.Card{} // by URL: one fetch per link
	for _, p := range dedupStrings(spec.Platforms) {
		if caps, _ := d.Caps(ctx, p); caps.LinkPreview != destination.LinkPreviewClient {
			continue
		}
		ov := spec.Overrides[p]
		text := spec.MasterText
		if ov.Text != "" {
			text = ov.Text
		}
		u, _, ok := unfurl.CardURL(text)
		if !ok {
			continue
		}
		card, seen := cards[u]
		if !seen {
			uctx, cancel := context.WithTimeout(ctx, 15*time.Second)
			c, err := d.Unfurler.Unfurl(uctx, u)
			cancel()
			if err != nil {
				slog.WarnContext(ctx, "unfurl failed; posting without link card", "url", u, "err", err)
			}
			card, cards[u] = c, c
		}
		if card == nil {
			continue
		}
		cp := *card
		ov.LinkCard = &cp
		if spec.Overrides == nil {
			spec.Overrides = map[string]Overrides{}
		}
		spec.Overrides[p] = ov
	}
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
			o := d.runChain(ctx, plat, text, ov, spec.Images, spec.Number, spec.ImgParts, nil, rec.ID)
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
			Status: o.Status, ErrorClass: o.ErrorClass, RemoteID: o.HeadRemoteID, RemoteURL: o.HeadRemoteURL, LatencyMS: o.LatencyMS,
			Relays: o.Relays, SignedEventJSON: o.SignedEventJSON, Segments: o.Segments,
			Attempts: []store.Attempt{{
				AttemptNo: 1, Status: o.Status, Error: o.Error, ErrorClass: o.ErrorClass, LatencyMS: o.LatencyMS, RemoteID: o.HeadRemoteID,
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
			Dim: m.Dim, DurationSecs: m.DurationSecs, SizeBytes: m.SizeBytes,
			SHA256: m.SHA256, Blurhash: m.Blurhash, PosterURL: m.PosterURL}
		// Videos: fetch only when a byte platform could use them (mirror
		// assembleImages' policy); URL platforms ride metadata-only. A failed
		// best-effort fetch leaves Bytes nil and the media gate fails loudly.
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
		// The source-platform target of a reply/quote/repost must repeat its
		// action; re-posting its text plainly would publish a top-level post
		// (or, for a repost, an empty one).
		head, repost, isAction, ok := interactionAction(post, tg)
		if isAction && !ok {
			if err := d.Store.RecordAttempt(tg.ID, store.AttemptResult{Status: "failed", Error: errNoInteractionRef,
				ErrorClass: string(destination.ClassInvalid)}); err != nil {
				return err
			}
			continue
		}
		if len(tg.Segments) > 1 { // threaded target → resume the chain
			if err := d.resumeChain(ctx, tg, ov, imgs, post.ID, head); err != nil {
				return err
			}
			continue
		}
		var r TargetResult
		if repost != nil {
			r = d.repost(ctx, tg.Platform, *repost)
		} else {
			r = d.publishItem(ctx, tg.Platform, item{text: tg.FinalText, ov: ov, imgs: imgs, head: head, postID: post.ID})
		}
		d.enqueueFanout(ctx, post.ID, r.Status, r.SignedEventJSON, r.PendingRelays)
		if err := d.Store.RecordAttempt(tg.ID, store.AttemptResult{
			Status: r.Status, Error: r.Error, ErrorClass: r.ErrorClass, RemoteID: r.RemoteID, RemoteURL: r.RemoteURL,
			LatencyMS: r.LatencyMS, RequestJSON: r.RequestJSON, ResponseJSON: r.ResponseJSON,
			Relays: r.Relays, SignedEventJSON: r.SignedEventJSON,
		}); err != nil {
			return err
		}
	}
	return nil
}
