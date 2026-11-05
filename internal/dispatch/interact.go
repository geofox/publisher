package dispatch

import (
	"context"
	"encoding/json"
	"strings"

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
