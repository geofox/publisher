package dispatch

import (
	"context"
	"encoding/json"
	"strings"
	"unicode"

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
	Action         string // reply|repost|quote
	SourcePlatform string
	Ref            InteractRef
	SourceURL      string
	SourceAuthor   string
	Text           string
	Overrides      map[string]Overrides // keyed by platform
	Fanout         []string             // quote only: other platforms for link-quotes
	Force          bool
	Images         []Img
	MediaRecords   []store.Media
	Number         bool // k/n counters on threaded segments
	// Layout is the source account's breaks and media positions in its
	// commentary (a link-quote copy is split automatically).
	Layout             thread.Layout
	SourcePreview      SourcePreview // for fan-out reproduction
	SourceImages       []Img         // original's media, re-hosted (fan-out only)
	SourceMediaRecords []store.Media // archival records for the re-hosted source media
	// Source is where the post came from ("web" when empty).
	Source string
	// Accounts names the account each platform posts as (platform →
	// account id); a platform left out gets its env account.
	Accounts map[string]string
}

// assembleReproduction builds a fan-out post body: commentary, an attributed copy
// of the original's text, and the source URL — each separated by a blank line.
func assembleReproduction(commentary string, sp SourcePreview, sourceURL string) string {
	var parts []string
	if c := strings.TrimSpace(commentary); c != "" {
		parts = append(parts, c)
	}
	if t := quotedText(sp.Text); t != "" {
		parts = append(parts, "— "+quotedAuthor(sp.Author)+":\n"+t)
	}
	if sourceURL != "" {
		parts = append(parts, sourceURL)
	}
	return strings.Join(parts, "\n\n")
}

// maxQuotedRunes caps how much of the original a link-quote copies; the link
// leads to the rest.
const maxQuotedRunes = 500

// quotedText is the original's text as a link-quote copies it. The text
// comes from someone else, so it is made inert: control and bidi characters
// are dropped, an @ that would start a mention gets a word joiner after it
// (so the copy doesn't notify the people the original names), and the text
// is cut at maxQuotedRunes.
func quotedText(s string) string {
	var b strings.Builder
	n := 0
	rs := []rune(strings.TrimSpace(s))
	for i, r := range rs {
		if isInvisibleControl(r) {
			continue
		}
		if n == maxQuotedRunes {
			return strings.TrimSpace(b.String()) + "…"
		}
		b.WriteRune(r)
		n++
		if r == '@' && i+1 < len(rs) && (unicode.IsLetter(rs[i+1]) || unicode.IsDigit(rs[i+1]) || rs[i+1] == '_') {
			b.WriteRune('\u2060')
		}
	}
	return strings.TrimSpace(b.String())
}

// quotedAuthor is the original's author as a link-quote names it: control and
// bidi characters dropped, at most 100 characters. The handle's @ is kept,
// so the author is credited as on the original network.
func quotedAuthor(s string) string {
	var b strings.Builder
	n := 0
	for _, r := range strings.TrimSpace(s) {
		if isInvisibleControl(r) || r == '\n' || r == '\t' {
			continue
		}
		if n == 100 {
			b.WriteRune('…')
			break
		}
		b.WriteRune(r)
		n++
	}
	return b.String()
}

// isInvisibleControl reports C0/C1 controls (but not newline or tab) and the
// bidi embedding, override and isolate characters, which can make copied
// text display differently from what it says.
func isInvisibleControl(r rune) bool {
	switch {
	case r == '\n' || r == '\t':
		return false
	case r < 0x20, r >= 0x7f && r < 0xa0, r >= 0x202a && r <= 0x202e, r >= 0x2066 && r <= 0x2069:
		return true
	}
	return false
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
