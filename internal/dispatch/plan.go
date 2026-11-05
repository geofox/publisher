package dispatch

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"time"

	"github.com/geofox/publisher/internal/logging"
	"github.com/geofox/publisher/internal/store"
)

// Creating a post plans it whole: every target's items (one per thread
// part) with their text, media and idempotency key, written in one
// transaction before anything is sent.

// planTarget plans one target's items from its text, overrides and media:
// split to the account's limits with media placed (PlanTarget), the link
// card on the part that carries its URL. imgs maps the media list the
// splitter sees onto the post's media (post index) or the item's own
// (attach). Returns the target with Items set, planned and due at due (nil:
// not until a retry).
func (d *Dispatcher) planTarget(ctx context.Context, postID, plat, text string, ov Overrides, imgs []planMedia,
	imgParts []int, number bool, due *time.Time, status string) store.Target {
	parts := make([]int, len(imgs))
	copy(parts, imgParts)
	caps, _ := d.Caps(ctx, plat)
	cp := PlanTarget(caps, text, ov.LinkCard, parts, number)
	ov.LinkCard = cp.Card // persist the card only as placed
	fields, _ := json.Marshal(ov2fields(ov))
	tg := store.Target{Platform: plat, FinalText: cp.Text, FieldsJSON: string(fields), Status: status}
	add := func(ordinal int, text string, idx []int) {
		it := store.Item{Ordinal: ordinal, Text: text, Status: store.ItemPlanned,
			IdemKey: store.ItemKey(postID, plat, ordinal), NextAttemptAt: due}
		for _, i := range idx {
			if i < 0 || i >= len(imgs) {
				continue
			}
			if m := imgs[i]; m.attach != nil {
				it.Attach = append(it.Attach, *m.attach)
			} else {
				it.Media = append(it.Media, m.post)
			}
		}
		tg.Items = append(tg.Items, it)
	}
	if len(cp.Segs) <= 1 {
		all := make([]int, len(imgs))
		for i := range all {
			all[i] = i
		}
		add(0, cp.Text, all) // a single post carries every attachment
		return tg
	}
	for i, seg := range cp.Segs {
		var idx []int
		if i < len(cp.Plan) {
			idx = cp.Plan[i]
		}
		add(i, seg, idx)
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

// notSaved is the record returned when a post couldn't be saved: nothing
// was sent, and every target says why.
func notSaved(rec *store.Post, err error) *store.Post {
	rec.Status = "failed"
	for i := range rec.Targets {
		rec.Targets[i].Status = "failed"
		rec.Targets[i].Attempts = []store.Attempt{{AttemptNo: 1, Status: "failed",
			Error: "not sent: the post could not be saved: " + err.Error(), AttemptedAt: time.Now().UTC()}}
	}
	return rec
}

func (d *Dispatcher) Post(ctx context.Context, spec PostSpec) *store.Post {
	return d.PostWithID(ctx, newID(), spec)
}

// PostWithID publishes a post now: it is planned and saved, then its items
// are sent inline and the post is returned as recorded.
func (d *Dispatcher) PostWithID(ctx context.Context, id string, spec PostSpec) *store.Post {
	ctx = logging.With(ctx, "post_id", id)
	now := d.now()
	rec := d.planPost(ctx, id, spec, &now, "sending")
	if err := d.drive(ctx, id, &memMedia{post: spec.Images}, func() error { return d.create(ctx, rec) }); err != nil {
		return notSaved(rec, err)
	}
	return d.recorded(ctx, rec)
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
	platforms := dedupStrings(spec.Platforms)
	rec := &store.Post{
		ID: id, CreatedAt: time.Now().UTC(), MasterText: spec.MasterText,
		Platforms: platforms, DelaySeconds: spec.DelaySeconds, Source: spec.Source,
		Media: spec.MediaRecords, Status: status,
	}
	scrubLinkCards(spec.Overrides)
	d.attachLinkCard(ctx, &spec)
	n := mediaCount(spec.Images, spec.MediaRecords)
	for _, plat := range platforms {
		rec.Targets = append(rec.Targets, d.planTarget(ctx, id, plat, finalText(spec, plat), spec.Overrides[plat],
			postMedia(n), spec.ImgParts, spec.Number, due, targetStatusFor(status)))
	}
	return rec
}

func targetStatusFor(postStatus string) string {
	if postStatus == "scheduled" {
		return "scheduled"
	}
	return "sending"
}

// Schedule persists a post for a future time without sending it; the
// engine claims its items when they are due.
func (d *Dispatcher) Schedule(ctx context.Context, spec PostSpec, at time.Time) (*store.Post, error) {
	if d.Store == nil {
		return nil, errors.New("schedule requires a store")
	}
	atUTC := at.UTC()
	rec := d.planPost(ctx, newID(), spec, &atUTC, "scheduled")
	rec.ScheduledAt = &atUTC
	if err := d.Store.SavePost(rec); err != nil {
		return nil, err
	}
	d.kick()
	return rec, nil
}

// Fire sends a scheduled post now, inline.
func (d *Dispatcher) Fire(ctx context.Context, postID string) (*store.Post, error) {
	return d.retryWhere(ctx, postID, func(t store.Target) bool { return t.Status == "scheduled" })
}

// Retry re-sends the unfinished targets of a post: failed, missed, and
// threads stopped part-way (from their first unsent part). If platforms is
// non-empty, only those. A single post that is partial (a Nostr note some
// relays missed) is retried per relay (RetryRelay), not re-sent.
func (d *Dispatcher) Retry(ctx context.Context, postID string, platforms []string) (*store.Post, error) {
	want := map[string]bool{}
	for _, p := range platforms {
		want[p] = true
	}
	return d.retryWhere(ctx, postID, func(t store.Target) bool {
		retryable := t.Status == "failed" || t.Status == "missed" || (t.Status == "partial" && len(t.Items) > 1)
		return retryable && (len(platforms) == 0 || want[t.Platform])
	})
}

func (d *Dispatcher) retryWhere(ctx context.Context, postID string, want func(store.Target) bool) (*store.Post, error) {
	if d.Store == nil {
		return nil, errors.New("retry requires a store")
	}
	ctx = logging.With(ctx, "post_id", postID)
	post, err := d.Store.GetPost(postID)
	if err != nil {
		return nil, err
	}
	var ids []int64
	for _, t := range post.Targets {
		if want(t) {
			ids = append(ids, t.ID)
		}
	}
	if len(ids) == 0 {
		return post, nil
	}
	if err := d.drive(ctx, postID, nil, func() error {
		_, err := d.Store.MarkDue(ctx, postID, ids, d.now())
		return err
	}); err != nil {
		return nil, err
	}
	return d.Store.GetPost(postID)
}

// Interact performs reply/repost/quote and records it as a store.Post carrying an
// interaction descriptor. Reply/quote thread the source action as the chain head on
// the source platform; quote also fans out an assembled reproduction (commentary +
// original text + source URL + re-hosted source media) to each Fanout platform.
func (d *Dispatcher) Interact(ctx context.Context, spec InteractSpec) *store.Post {
	return d.InteractWithID(ctx, newID(), spec)
}

func (d *Dispatcher) InteractWithID(ctx context.Context, id string, spec InteractSpec) *store.Post {
	ctx = logging.With(ctx, "post_id", id)
	now := d.now()
	rec := &store.Post{
		ID: id, CreatedAt: time.Now().UTC(), MasterText: spec.Text,
		Source: "web", Media: spec.MediaRecords, Status: "sending",
		Interaction: &store.Interaction{
			Action: spec.Action, SourcePlatform: spec.SourcePlatform,
			SourceURL: spec.SourceURL, SourceAuthor: spec.SourceAuthor,
			Ref: marshalRef(spec.Ref),
		},
	}
	scrubLinkCards(spec.Overrides)
	nUser := mediaCount(spec.Images, spec.MediaRecords)
	switch spec.Action {
	case actionRepost:
		rec.Platforms = []string{spec.SourcePlatform}
		fields, _ := json.Marshal(ov2fields(spec.Overrides[spec.SourcePlatform]))
		rec.Targets = append(rec.Targets, store.Target{Platform: spec.SourcePlatform, FieldsJSON: string(fields), Status: "sending",
			Items: []store.Item{{Ordinal: 0, Status: store.ItemPlanned, NextAttemptAt: &now,
				IdemKey: store.ItemKey(id, spec.SourcePlatform, 0)}}})
	case actionReply, actionQuote:
		rec.Platforms = append(rec.Platforms, spec.SourcePlatform)
		rec.Targets = append(rec.Targets, d.planTarget(ctx, id, spec.SourcePlatform, interactText(spec, spec.SourcePlatform),
			spec.Overrides[spec.SourcePlatform], postMedia(nUser), nil, spec.Number, &now, "sending"))
		for _, p := range dedupStrings(spec.Fanout) {
			if p == spec.SourcePlatform {
				continue
			}
			rec.Platforms = append(rec.Platforms, p)
			text := assembleReproduction(interactText(spec, p), spec.SourcePreview, spec.SourceURL)
			rec.Targets = append(rec.Targets, d.planTarget(ctx, id, p, text, spec.Overrides[p],
				reproductionMedia(nUser, spec, d.mediaMax(ctx, p)), nil, spec.Number, &now, "sending"))
		}
	}
	if err := d.drive(ctx, id, &memMedia{post: spec.Images, source: spec.SourceImages},
		func() error { return d.create(ctx, rec) }); err != nil {
		return notSaved(rec, err)
	}
	return d.recorded(ctx, rec)
}

// reproductionMedia is a quote reproduction's media: the user's, then the
// source's re-hosted media, capped at limit (0 = no cap). User media comes
// first.
func reproductionMedia(nUser int, spec InteractSpec, limit int) []planMedia {
	out := postMedia(nUser)
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
