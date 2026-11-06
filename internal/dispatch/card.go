package dispatch

import (
	"strings"

	"github.com/geofox/publisher/internal/destination"
	"github.com/geofox/publisher/internal/thread"
	"github.com/geofox/publisher/internal/unfurl"
)

// CardPlan is a target's chain layout with an optional link-card placement.
type CardPlan struct {
	// Items are the planned posts, with their ranges in Text.
	Items []thread.Item
	// Segs and Plan are the items' texts and media, for callers that only
	// need those.
	Segs     []string
	Plan     [][]int
	Warnings []string
	// Text is the effective draft: the trailing card URL is stripped when the
	// card attaches there; the input text unchanged otherwise.
	Text string
	// Card is nil when no card attaches (none given, text edited away from
	// the card's URL, or the target segment's embed slot is taken by images).
	// Non-nil ⇒ Card.Segment is the chain ordinal that carries it.
	Card *unfurl.Card
}

// TargetInput is what a target is planned from.
type TargetInput struct {
	Text string
	// Card is the link card to place, if any.
	Card *unfurl.Card
	// Layout is the target's breaks and media positions, in Text.
	Layout thread.Layout
	Number bool
	// ContentWarning goes with every post; a destination that counts it
	// toward the limit (Mastodon) has that much less room.
	ContentWarning string
}

// Rules are the planner's rules for a destination's capabilities and a
// post's content warning.
func Rules(caps destination.Capabilities, number bool, cw string) thread.Rules {
	limit := caps.Text.MaxLength
	if limit > 0 && cw != "" && caps.Text.Counting.ContentWarningCounts {
		// The warning is plain text to the destination: no link or mention
		// discount (Mastodon counts spoiler_text as it is).
		plain := destination.Counting{Unit: caps.Text.Counting.Unit, EmojiUTF8Bytes: caps.Text.Counting.EmojiUTF8Bytes}
		limit = max(1, limit-plain.Count(cw))
	}
	return thread.Rules{Limit: limit, Counting: caps.Text.Counting, ImgCap: caps.Media.Images.MaxCount,
		Number: number, MaxLinks: caps.Text.MaxLinks}
}

// PlanTarget plans a target's chain for a destination with the given
// capabilities: posts within its text limit as it counts them, media placed
// by their positions within its per-post cap and, for a destination that
// builds link previews itself (LinkPreview "client"), the link card placed.
// Shared by the engine and the API's preview and validation, so preview and
// dispatch cannot diverge.
//
// Card placement is a deterministic strip-then-revert: tentatively strip a trailing card URL,
// split, locate the card's segment (last segment for trailing, the
// URL-bearing segment otherwise), then revert entirely — no card, no strip —
// when that segment already holds images, so the link can never silently
// disappear from a post that can't carry the card. Stripping only shortens
// the text's end, so the layout's positions stay valid.
func PlanTarget(caps destination.Capabilities, in TargetInput) CardPlan {
	rules := Rules(caps, in.Number, in.ContentWarning)
	planned := func(text string) CardPlan {
		items, warns := thread.Plan(text, in.Layout, rules)
		cp := CardPlan{Items: items, Warnings: warns, Text: text}
		for _, it := range items {
			cp.Segs = append(cp.Segs, it.Text)
			cp.Plan = append(cp.Plan, it.Media)
		}
		return cp
	}
	text, card := in.Text, in.Card
	plain := func() CardPlan { return planned(text) }
	if card == nil || caps.LinkPreview != destination.LinkPreviewClient {
		return plain()
	}
	u, trailing, ok := unfurl.CardURL(text)
	if !ok || u != card.URI {
		return plain() // text no longer matches the card (edited) — drop it
	}
	eff := text
	if trailing {
		eff = unfurl.StripTrailing(text, card.URI)
		if strings.TrimSpace(eff) == "" {
			// URL-only post: keep the URL in the text, still attach the card.
			eff, trailing = text, false
		}
	}
	cp := planned(eff)
	target := -1
	if trailing {
		target = len(cp.Segs) - 1
	} else {
		for i, s := range cp.Segs {
			if strings.Contains(s, card.URI) {
				target = i
				break
			}
		}
	}
	// target == -1 means the URL got hard-split across segments — no card.
	// len(plan[target]) > 0 means images own that segment's embed slot — revert.
	if target < 0 || len(cp.Plan[target]) > 0 {
		return plain()
	}
	c := *card
	c.Segment = target
	cp.Card = &c
	return cp
}
