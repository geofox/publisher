package dispatch

import (
	"context"
	"encoding/json"
	"strconv"

	"github.com/geofox/publisher/internal/destination"
	"github.com/geofox/publisher/internal/progress"
	"github.com/geofox/publisher/internal/store"
	"github.com/geofox/publisher/internal/thread"
	"github.com/geofox/publisher/internal/unfurl"
)

// chainOutcome is the result of posting one platform's (possibly single-segment)
// chain. Segments is empty for a single post (preserving non-threaded behavior).
type chainOutcome struct {
	Platform                    string
	Status                      string
	Error                       string
	ErrorClass                  string
	FinalText                   string       // what this target actually sent (head text)
	LinkCard                    *unfurl.Card // bluesky link card as actually attached (nil = none)
	HeadRemoteID, HeadRemoteURL string
	LatencyMS                   int
	Relays                      []store.RelayState
	SignedEventJSON             string
	RequestJSON, ResponseJSON   string
	Segments                    []store.Segment
}

// segRef is the reference a later segment replies to.
func segRef(sg store.Segment) destination.ItemRef {
	ref := destination.ItemRef{ID: sg.RemoteID, URL: sg.RemoteURL, Data: sg.Ref}
	if len(ref.Data) == 0 && sg.CID != "" {
		// Segments written before Ref existed kept only the Bluesky CID.
		// This is the Bluesky driver's Ref.Data shape, {"cid": ...}.
		b, _ := json.Marshal(map[string]string{"cid": sg.CID})
		ref.Data = b
	}
	return ref
}

// runChain splits text to the platform's limit and posts the segments as a
// reply-chain (segment k+1 replies to segment k). Images are distributed per
// thread.PlanMedia: head first, capped per platform, with image-only segments
// appended when images outrun the text (Mastodon's 4-attachment cap). A
// single segment posts exactly as before, with no Segments recorded. An
// optional head action (reply/quote via headSpec; nil = plain post) lets the
// chain's head segment thread under or quote a source post.
func (d *Dispatcher) runChain(ctx context.Context, plat, text string, ov Overrides, imgs []Img, number bool, imgParts []int, head *headSpec, postID string) chainOutcome {
	// imgParts must be parallel to imgs. A nil/short slice (interaction paths,
	// or an assembleImages skip) pads with 0 (front-load); a long one truncates.
	// SplitPlace derives nImages from len(imgParts), so it MUST equal len(imgs).
	if len(imgParts) != len(imgs) {
		np := make([]int, len(imgs))
		copy(np, imgParts)
		imgParts = np
	}
	caps, _ := d.Caps(ctx, plat)
	cp := PlanTarget(caps, text, ov.LinkCard, imgParts, number)
	segTexts, plan, text, card := cp.Segs, cp.Plan, cp.Text, cp.Card
	// Only the card's own segment carries it; re-attached per segment below.
	ov.LinkCard = nil
	if len(segTexts) <= 1 {
		headOv := ov
		if card != nil {
			headOv.LinkCard = card
		}
		r := d.publishItem(ctx, plat, item{text: text, ov: headOv, imgs: imgs, head: head, postID: postID})
		d.enqueueFanout(ctx, postID, r.Status, r.SignedEventJSON, r.PendingRelays)
		return chainOutcome{
			Platform: plat, Status: r.Status, Error: r.Error, ErrorClass: r.ErrorClass, FinalText: text, LinkCard: card,
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
	var root, parent destination.ItemRef
	for i, st := range segTexts {
		it := item{text: st, ov: ov, imgs: pick(imgs, plan[i]), postID: postID, ordinal: i}
		if card != nil && i == card.Segment {
			it.ov.LinkCard = card
		}
		if i == 0 {
			it.head = head
		} else {
			it.thread = &destination.ThreadRef{Root: root, Parent: parent}
		}
		r := d.publishItem(ctx, plat, it)
		out.Segments[i] = store.Segment{
			Ordinal: i, Text: st, RemoteID: r.RemoteID, RemoteURL: r.RemoteURL, Ref: r.RefData,
			Status: r.Status, Error: r.Error, Images: plan[i],
		}
		d.enqueueFanout(ctx, postID, r.Status, r.SignedEventJSON, r.PendingRelays)
		// live thread counter: successes so far / total planned
		done := 0
		for _, sg := range out.Segments {
			if sg.Status == "success" {
				done++
			}
		}
		progress.SinkFrom(ctx).Platform(plat, progress.StatusRunning, "thread "+strconv.Itoa(done)+"/"+strconv.Itoa(len(segTexts)), "")
		if i == 0 {
			root = segRef(out.Segments[0])
			out.HeadRemoteID, out.HeadRemoteURL = r.RemoteID, r.RemoteURL
			out.Relays, out.SignedEventJSON = r.Relays, r.SignedEventJSON
			out.RequestJSON, out.ResponseJSON, out.LatencyMS = r.RequestJSON, r.ResponseJSON, r.LatencyMS
		}
		parent = segRef(out.Segments[i])
		out.Error, out.ErrorClass = r.Error, r.ErrorClass
		if r.RemoteID == "" { // no id to reply to → cannot continue the chain
			break
		}
	}
	out.Status = chainStatus(out.Segments, len(segTexts))
	out.FinalText = text
	out.LinkCard = card
	return out
}

// legacyStart returns the start offset of segment i in a flat image slice
// described by the contiguous legacy plan (prefix-sum of plan[0..i-1]).
func legacyStart(plan []int, i int) int {
	offset := 0
	for j := 0; j < i && j < len(plan); j++ {
		offset += plan[j]
	}
	return offset
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
func (d *Dispatcher) resumeSegments(ctx context.Context, tg store.Target, ov Overrides, imgs []Img, postID string) chainOutcome {
	return d.resumeSegmentsHead(ctx, tg, ov, imgs, postID, nil)
}

// resumeSegmentsHead is resumeSegments with the chain's head action: when the
// head (segment 0) has to be re-posted, it is posted per head (reply/quote of
// the interaction's source) rather than as a plain post. nil = plain head.
func (d *Dispatcher) resumeSegmentsHead(ctx context.Context, tg store.Target, ov Overrides, imgs []Img, postID string, head *headSpec) chainOutcome {
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
	var root, parent destination.ItemRef
	if start > 0 {
		root, parent = segRef(segs[0]), segRef(segs[start-1])
	}

	// Back-compat: segments saved before media-placement have nil Images. Only
	// re-derive the front-fill plan when the persisted placement is entirely absent.
	legacy := true
	for i := range segs {
		if segs[i].Images != nil {
			legacy = false
			break
		}
	}
	var legacyPlan []int
	if legacy {
		caps, _ := d.Caps(ctx, tg.Platform)
		legacyPlan = thread.PlanMedia(len(imgs), len(segs), caps.Media.Images.MaxCount)
	}

	for i := start; i < len(segs); i++ {
		var segImgs []Img
		if legacy {
			if i < len(legacyPlan) {
				segImgs = imgs[legacyStart(legacyPlan, i) : legacyStart(legacyPlan, i)+legacyPlan[i]]
			}
		} else {
			segImgs = pick(imgs, segs[i].Images)
		}
		it := item{text: segs[i].Text, ov: ov, imgs: segImgs, postID: postID, ordinal: i}
		if card != nil && i == card.Segment {
			it.ov.LinkCard = card
		}
		if i == 0 {
			it.head = head
		} else {
			it.thread = &destination.ThreadRef{Root: root, Parent: parent}
		}
		r := d.publishItem(ctx, tg.Platform, it)
		segs[i] = store.Segment{Ordinal: i, Text: segs[i].Text, RemoteID: r.RemoteID, RemoteURL: r.RemoteURL, Ref: r.RefData, Status: r.Status, Error: r.Error, Images: segs[i].Images}
		d.enqueueFanout(ctx, postID, r.Status, r.SignedEventJSON, r.PendingRelays)
		if i == 0 {
			root = segRef(segs[0])
			out.Relays, out.SignedEventJSON, out.LatencyMS = r.Relays, r.SignedEventJSON, r.LatencyMS
		}
		parent = segRef(segs[i])
		out.Error, out.ErrorClass = r.Error, r.ErrorClass
		if r.RemoteID == "" {
			break
		}
	}
	out.Status = chainStatus(segs, len(segs))
	out.HeadRemoteID, out.HeadRemoteURL = segs[0].RemoteID, segs[0].RemoteURL
	return out
}

// resumeChain resumes a partial threaded target and persists the result.
func (d *Dispatcher) resumeChain(ctx context.Context, tg store.Target, ov Overrides, imgs []Img, postID string, head *headSpec) error {
	out := d.resumeSegmentsHead(ctx, tg, ov, imgs, postID, head)
	return d.Store.UpdateTargetSegments(tg.ID, out.Segments, out.Status, out.HeadRemoteID, out.HeadRemoteURL, out.LatencyMS, out.Error, out.ErrorClass)
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
