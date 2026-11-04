package dispatch

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"time"

	"github.com/geofox/publisher/internal/logging"
	"github.com/geofox/publisher/internal/progress"
	"github.com/geofox/publisher/internal/store"
	"github.com/geofox/publisher/internal/thread"
)

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

func (d *Dispatcher) fanoutChain(ctx context.Context, plat string, spec InteractSpec, postID string) chainOutcome {
	text := assembleReproduction(interactText(spec, plat), spec.SourcePreview, spec.SourceURL)
	imgs := capMedia(spec.Images, spec.SourceImages, mediaMax(plat))
	recs := capMediaRecords(spec.MediaRecords, spec.SourceMediaRecords, mediaMax(plat))
	ov := spec.Overrides[plat]
	// Interactions carry no per-image part anchors; nil ⇒ runChain front-loads.
	return d.runChain(ctx, plat, text, ov, imgs, buildImetas(recs), spec.Number, nil, nil, postID)
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
		d.enqueueFanout(ctx, rec.ID, r.Status, r.SignedEventJSON, r.PendingRelays)
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
		o := d.runChain(ctx, spec.SourcePlatform, interactText(spec, spec.SourcePlatform), ov, spec.Images, buildImetas(spec.MediaRecords), spec.Number, nil, head, rec.ID)
		sink.Platform(spec.SourcePlatform, mapStatus(o.Status), platformDetail(o), o.HeadRemoteURL)
		outcomes = append(outcomes, o)
		for _, p := range spec.Fanout {
			if p == spec.SourcePlatform {
				continue
			}
			sink.Platform(p, progress.StatusRunning, "", "")
			fo := d.fanoutChain(ctx, p, spec, rec.ID)
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
