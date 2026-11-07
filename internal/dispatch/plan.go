package dispatch

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"time"

	"github.com/geofox/publisher/internal/logging"
	"github.com/geofox/publisher/internal/store"
	"github.com/geofox/publisher/internal/thread"
	"github.com/geofox/publisher/internal/unfurl"
)

// Creating a post plans it whole: every target's items (one per thread
// part) with their text, media and idempotency key, written in one
// transaction before anything is sent.

// planTarget plans one target's items from its text, overrides, layout and
// media: split to its account's limits with media placed by position
// (PlanTarget), the link card on the part that carries its URL. t.Text is
// the text it posts. imgs maps the media list the splitter sees onto the
// post's media (post index) or the item's own (attach); a media past the
// layout's positions goes with the first post. Returns the target with
// Items set, planned and due at due (nil: not until a retry).
func (d *Dispatcher) planTarget(ctx context.Context, postID string, t TargetSpec, imgs []planMedia,
	number bool, due *time.Time, status string) store.Target {
	lay, ov := t.Layout, t.Overrides
	at := make([]int, len(imgs))
	copy(at, lay.MediaAt)
	lay.MediaAt = at
	caps := d.Caps(ctx, t.AccountID, t.Type)
	// As publishing picks it (SpoilerText first).
	cw := ov.SpoilerText
	if cw == "" {
		cw = ov.ContentWarning
	}
	cp := PlanTarget(caps, TargetInput{Text: t.Text, Card: ov.LinkCard, Layout: lay, Number: number, ContentWarning: cw})
	ov.LinkCard = cp.Card // persist the card only as placed
	fields, _ := json.Marshal(ov2fields(ov))
	tg := store.Target{AccountID: t.AccountID, Platform: t.Type, FinalText: cp.Text, FieldsJSON: string(fields), Status: status}
	for i, pi := range cp.Items {
		it := store.Item{Ordinal: i, Text: pi.Text, Status: store.ItemPlanned,
			IdemKey: store.AccountItemKey(postID, t.AccountID, i), NextAttemptAt: due, Range: &[2]int{pi.From, pi.To}}
		for _, k := range pi.Media {
			if k < 0 || k >= len(imgs) {
				continue
			}
			if m := imgs[k]; m.attach != nil {
				it.Attach = append(it.Attach, *m.attach)
			} else {
				it.Media = append(it.Media, m.post)
			}
		}
		tg.Items = append(tg.Items, it)
	}
	return tg
}

// planMedia is one attachment as the splitter sees it: the post's media at
// index post, or (attach set) media the item carries itself.
type planMedia struct {
	post   int
	attach *store.Media
}

func postMedia(n int) []planMedia {
	out := make([]planMedia, n)
	for i := range out {
		out[i] = planMedia{post: i}
	}
	return out
}

func mediaCount(imgs []Img, recs []store.Media) int {
	return max(len(imgs), len(recs))
}

// create persists a planned post. Nothing is sent when it fails. A post
// with no targets has nothing to send: it is recorded failed.
func (d *Dispatcher) create(ctx context.Context, rec *store.Post) error {
	if d.Store == nil {
		return errors.New("publishing requires a store")
	}
	if len(rec.Targets) == 0 {
		rec.Status = "failed"
	}
	if err := d.Store.SavePost(rec); err != nil {
		slog.ErrorContext(ctx, "savepost failed; nothing was sent", "err", err)
		return err
	}
	return nil
}

// recorded is the post as stored, or rec when it can't be read back.
func (d *Dispatcher) recorded(ctx context.Context, rec *store.Post) *store.Post {
	p, err := d.Store.GetPost(rec.ID)
	if err != nil {
		slog.ErrorContext(ctx, "reload post failed", "post_id", rec.ID, "err", err)
		return rec
	}
	return p
}

// planPost plans a composed post's targets, due at due.
func (d *Dispatcher) planPost(ctx context.Context, id string, spec PostSpec, due *time.Time, status string) *store.Post {
	rec, _ := d.planPostCards(ctx, id, spec, due, status)
	return rec
}

// planPostCards is planPost that also returns the link cards it fetched,
// with their thumbnails' bytes (a stored card has none), so the first send
// doesn't download them again. An account named twice is planned once.
func (d *Dispatcher) planPostCards(ctx context.Context, id string, spec PostSpec, due *time.Time, status string) (*store.Post, map[string]*unfurl.Card) {
	spec.Targets = uniqueTargets(spec.Targets)
	rec := &store.Post{
		ID: id, CreatedAt: time.Now().UTC(), MasterText: spec.MasterText,
		Platforms: targetTypes(spec.Targets), DelaySeconds: spec.DelaySeconds, Source: spec.Source,
		Media: spec.MediaRecords, Status: status,
	}
	scrubLinkCards(spec.Targets)
	d.attachLinkCard(ctx, &spec)
	thumbs := map[string]*unfurl.Card{}
	for _, t := range spec.Targets {
		if c := t.Overrides.LinkCard; c != nil && c.ThumbURL != "" && len(c.ThumbData) > 0 {
			thumbs[c.ThumbURL] = c
		}
	}
	n := mediaCount(spec.Images, spec.MediaRecords)
	for _, t := range spec.Targets {
		t.Text = spec.text(t)
		rec.Targets = append(rec.Targets, d.planTarget(ctx, id, t, postMedia(n), spec.Number, due, targetStatusFor(status)))
	}
	return rec, thumbs
}

// uniqueTargets drops a repeat of an account, keeping the first. The
// targets are copied, so planning never changes the caller's spec.
func uniqueTargets(in []TargetSpec) []TargetSpec {
	seen := make(map[string]bool, len(in))
	out := make([]TargetSpec, 0, len(in))
	for _, t := range in {
		if seen[t.AccountID] {
			continue
		}
		seen[t.AccountID] = true
		out = append(out, t)
	}
	return out
}

// targetTypes is the post's destination types, each once, in target order
// (posts.platforms).
func targetTypes(ts []TargetSpec) []string {
	out := make([]string, 0, len(ts))
	for _, t := range ts {
		out = append(out, t.Type)
	}
	return dedupStrings(out)
}

func targetStatusFor(postStatus string) string {
	if postStatus == "scheduled" {
		return "scheduled"
	}
	return "sending"
}

// Enqueue persists a post for the background workers: due now, or at at
// when it is set (a scheduled post). Nothing is sent inline, so it returns
// as soon as the post is saved.
func (d *Dispatcher) Enqueue(ctx context.Context, spec PostSpec, at *time.Time) (*store.Post, error) {
	if d.Store == nil {
		return nil, errors.New("publishing requires a store")
	}
	id := newID()
	ctx = logging.With(ctx, "post_id", id)
	due, status := d.now(), "sending"
	if at != nil {
		due, status = at.UTC(), "scheduled"
	}
	rec := d.planPost(ctx, id, spec, &due, status)
	if at != nil {
		rec.ScheduledAt = &due
	}
	if err := d.create(ctx, rec); err != nil {
		return nil, err
	}
	d.kick()
	return d.recorded(ctx, rec), nil
}

// EnqueueInteraction persists a reply, quote or repost for the background
// workers, due now; nothing is sent inline. Its media must be in media
// storage (MediaRecords, SourceMediaRecords): the workers fetch them.
func (d *Dispatcher) EnqueueInteraction(ctx context.Context, spec InteractSpec) (*store.Post, error) {
	if d.Store == nil {
		return nil, errors.New("publishing requires a store")
	}
	id := newID()
	ctx = logging.With(ctx, "post_id", id)
	rec := d.planInteract(ctx, id, spec)
	if err := d.create(ctx, rec); err != nil {
		return nil, err
	}
	d.kick()
	return d.recorded(ctx, rec), nil
}

// planInteract plans an interaction's targets, due now: the action on the
// source account, and for a reply or quote, a link-quote reproduction on
// each other target.
func (d *Dispatcher) planInteract(ctx context.Context, id string, spec InteractSpec) *store.Post {
	now := d.now()
	source := spec.Source
	if source == "" {
		source = "web"
	}
	src, others := spec.split()
	rec := &store.Post{
		ID: id, CreatedAt: time.Now().UTC(), MasterText: spec.Text,
		Source: source, Media: spec.MediaRecords, Status: "sending",
		Interaction: &store.Interaction{
			Action: spec.Action, SourcePlatform: spec.SourcePlatform, SourceAccount: src.AccountID,
			SourceURL: spec.SourceURL, SourceAuthor: spec.SourceAuthor,
			Ref: marshalRef(spec.Ref),
		},
	}
	scrubLinkCards(others)
	src.Overrides.LinkCard = nil
	nUser := mediaCount(spec.Images, spec.MediaRecords)
	switch spec.Action {
	case actionRepost:
		rec.Platforms = []string{src.Type}
		fields, _ := json.Marshal(ov2fields(src.Overrides))
		rec.Targets = append(rec.Targets, store.Target{AccountID: src.AccountID, Platform: src.Type,
			FieldsJSON: string(fields), Status: "sending",
			Items: []store.Item{{Ordinal: 0, Status: store.ItemPlanned, NextAttemptAt: &now,
				IdemKey: store.AccountItemKey(id, src.AccountID, 0)}}})
	case actionReply, actionQuote:
		src.Text = InteractText(spec, src)
		rec.Targets = append(rec.Targets, d.planTarget(ctx, id, src, postMedia(nUser), spec.Number, &now, "sending"))
		for _, t := range others {
			t.Text, t.Layout = ReproductionText(spec, t), thread.Layout{}
			rec.Targets = append(rec.Targets, d.planTarget(ctx, id, t,
				reproductionMedia(nUser, spec, t, d.mediaMax(ctx, t)), spec.Number, &now, "sending"))
		}
		all := append([]TargetSpec{src}, others...)
		rec.Platforms = targetTypes(all)
	}
	return rec
}

// ReproductionText is a quote's text on a link-quote account: the
// commentary, the original's attributed text and its URL.
func ReproductionText(spec InteractSpec, t TargetSpec) string {
	return assembleReproduction(InteractText(spec, t), spec.SourcePreview, spec.SourceURL)
}

// InteractText is a reply's or quote's commentary on account t: its own
// text, else the shared commentary.
func InteractText(spec InteractSpec, t TargetSpec) string {
	if t.Text != "" {
		return t.Text
	}
	return spec.Text
}

// reproductionMedia is a quote reproduction's media: the user's, then the
// source's re-hosted media (unless t goes without them), capped at limit
// (0 = no cap). User media comes first.
func reproductionMedia(nUser int, spec InteractSpec, t TargetSpec, limit int) []planMedia {
	out := postMedia(nUser)
	if t.NoSourceMedia {
		return out
	}
	nSource := max(len(spec.SourceImages), len(spec.SourceMediaRecords))
	for j := 0; j < nSource; j++ {
		if limit > 0 && len(out) >= limit {
			break
		}
		m := store.Media{Ordinal: j}
		if j < len(spec.SourceMediaRecords) {
			m = spec.SourceMediaRecords[j]
			m.Ordinal = j
		} else if j < len(spec.SourceImages) {
			im := spec.SourceImages[j]
			m = store.Media{Ordinal: j, BlossomURL: im.BlossomURL, SHA256: im.SHA256, Mime: im.Mime, Dim: im.Dim,
				Blurhash: im.Blurhash, SizeBytes: im.SizeBytes, Alt: im.Alt, DurationSecs: im.DurationSecs, PosterURL: im.PosterURL}
		}
		out = append(out, planMedia{attach: &m})
	}
	return out
}
