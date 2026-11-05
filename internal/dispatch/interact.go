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
	SourceMediaRecords []store.Media // archival records for the re-hosted source media
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

// mediaMax is the per-post image cap of the account a target names (0 = no
// cap), the same value the splitter and the preview use.
func (d *Dispatcher) mediaMax(ctx context.Context, plat string) int {
	caps, _ := d.Caps(ctx, plat)
	return caps.Media.Images.MaxCount
}

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
	imgs := capMedia(spec.Images, spec.SourceImages, d.mediaMax(ctx, plat))
	ov := spec.Overrides[plat]
	// Interactions carry no per-image part anchors; nil ⇒ runChain front-loads.
	return d.runChain(ctx, plat, text, ov, imgs, spec.Number, nil, nil, postID)
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
			Ref: marshalRef(spec.Ref),
		},
	}
	ctx = logging.With(ctx, "post_id", rec.ID)
	scrubLinkCards(spec.Overrides)
	var outcomes []chainOutcome
	switch spec.Action {
	case actionRepost:
		sink := progress.SinkFrom(ctx)
		sink.Platform(spec.SourcePlatform, progress.StatusRunning, "", "")
		r := d.repost(ctx, spec.SourcePlatform, spec.Ref)
		d.enqueueFanout(ctx, rec.ID, r.Status, r.SignedEventJSON, r.PendingRelays)
		o := chainOutcome{
			Platform: r.Platform, Status: r.Status, Error: r.Error, ErrorClass: r.ErrorClass,
			HeadRemoteID: r.RemoteID, HeadRemoteURL: r.RemoteURL, LatencyMS: r.LatencyMS,
			Relays: r.Relays, SignedEventJSON: r.SignedEventJSON, RequestJSON: r.RequestJSON, ResponseJSON: r.ResponseJSON,
		}
		sink.Platform(spec.SourcePlatform, mapStatus(o.Status), platformDetail(o), o.HeadRemoteURL)
		outcomes = append(outcomes, o)
	case actionReply, actionQuote:
		ov := spec.Overrides[spec.SourcePlatform]
		head := &headSpec{}
		if spec.Action == actionReply {
			head.reply = &spec.Ref
		} else {
			head.quote = &spec.Ref
		}
		sink := progress.SinkFrom(ctx)
		sink.Platform(spec.SourcePlatform, progress.StatusRunning, "", "")
		o := d.runChain(ctx, spec.SourcePlatform, interactText(spec, spec.SourcePlatform), ov, spec.Images, spec.Number, nil, head, rec.ID)
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
			Status: o.Status, ErrorClass: o.ErrorClass, RemoteID: o.HeadRemoteID, RemoteURL: o.HeadRemoteURL, LatencyMS: o.LatencyMS,
			Relays: o.Relays, SignedEventJSON: o.SignedEventJSON, Segments: o.Segments,
			Attempts: []store.Attempt{{AttemptNo: 1, Status: o.Status, Error: o.Error, ErrorClass: o.ErrorClass, LatencyMS: o.LatencyMS,
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

// marshalRef serializes the source's native ref for Interaction.Ref, so a
// retry can repeat the reply/quote/repost instead of posting plainly.
func marshalRef(ref InteractRef) json.RawMessage {
	b, err := json.Marshal(ref)
	if err != nil {
		return nil
	}
	return b
}

// errNoInteractionRef is recorded on a retry of an interaction target whose
// post predates Interaction.Ref: without the source ref the action can't be
// repeated, and posting the text plainly would publish the wrong thing.
const errNoInteractionRef = "cannot retry: this reply/quote/repost was recorded without its source reference; redo it from the composer"

// interactionAction returns how a retry must re-run target tg of post: the
// head action for the interaction's source-platform target (reply/quote head,
// or a repost), nil/false for every other target (plain posts, including
// quote fan-out reproductions). ok=false with a target means the action
// can't be repeated (missing ref).
func interactionAction(post *store.Post, tg store.Target) (head *headSpec, repost *InteractRef, isAction, ok bool) {
	ix := post.Interaction
	if ix == nil || tg.Platform != ix.SourcePlatform {
		return nil, nil, false, true
	}
	var ref InteractRef
	if len(ix.Ref) == 0 || json.Unmarshal(ix.Ref, &ref) != nil {
		return nil, nil, true, false
	}
	switch ix.Action {
	case actionRepost:
		return nil, &ref, true, true
	case actionQuote:
		return &headSpec{quote: &ref}, nil, true, true
	case actionReply:
		return &headSpec{reply: &ref}, nil, true, true
	}
	return nil, nil, true, false
}
