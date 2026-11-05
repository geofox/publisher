package dispatch

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/geofox/publisher/internal/store"
)

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
		caps, _ := d.Caps(ctx, plat)
		cp := PlanTarget(caps, text, ov.LinkCard, imgParts, spec.Number)
		text, segTexts, segPlan := cp.Text, cp.Segs, cp.Plan
		ov.LinkCard = cp.Card // persist only as actually placed
		// ov2fields returns a map of JSON-safe values only, so Marshal can't fail.
		fields, _ := json.Marshal(ov2fields(ov))
		tg := store.Target{
			Platform: plat, FinalText: text, FieldsJSON: string(fields), Status: "scheduled",
		}
		// Pre-split exactly as runChain does at dispatch time, so a scheduled
		// over-limit post fires as a reply-chain (the fire path threads any target
		// with >1 segment) instead of one over-limit post the platform rejects.
		// Media overflow counts too: 10 images on Mastodon is a 3-post chain.
		// Record Segment.Images from the placement plan so Fire (resumeSegments)
		// reads the persisted placement rather than re-deriving a front-fill.
		if len(segTexts) > 1 {
			for i, st := range segTexts {
				var imgs []int
				if i < len(segPlan) {
					imgs = segPlan[i]
				}
				tg.Segments = append(tg.Segments, store.Segment{Ordinal: i, Text: st, Status: "pending", Images: imgs})
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
