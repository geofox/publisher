package dispatch

import (
	"strings"

	"github.com/geofox/publisher/internal/thread"
	"github.com/geofox/publisher/internal/unfurl"
)

// CardPlan is a bluesky chain layout with an optional link-card placement.
type CardPlan struct {
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

// PlanBlueskyCard computes the bluesky split with an optional link card.
// Deterministic strip-then-revert: tentatively strip a trailing card URL,
// split, locate the card's segment (last segment for trailing, the
// URL-bearing segment otherwise), then revert entirely — no card, no strip —
// when that segment already holds images, so the link can never silently
// disappear from a post that can't carry the card. Shared by runChain,
// Schedule and the thread-preview endpoint so preview and dispatch cannot
// diverge.
func PlanBlueskyCard(text string, card *unfurl.Card, imgParts []int, number bool) CardPlan {
	limit, imgCap := thread.LimitFor("bluesky"), thread.MaxImagesFor("bluesky")
	plain := func() CardPlan {
		segs, plan, warns := thread.SplitPlace(text, limit, imgParts, imgCap, thread.Opts{Number: number})
		return CardPlan{Segs: segs, Plan: plan, Warnings: warns, Text: text}
	}
	if card == nil {
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
	segs, plan, warns := thread.SplitPlace(eff, limit, imgParts, imgCap, thread.Opts{Number: number})
	target := -1
	if trailing {
		target = len(segs) - 1
	} else {
		for i, s := range segs {
			if strings.Contains(s, card.URI) {
				target = i
				break
			}
		}
	}
	// target == -1 means the URL got hard-split across segments — no card.
	// len(plan[target]) > 0 means images own that segment's embed slot — revert.
	if target < 0 || len(plan[target]) > 0 {
		return plain()
	}
	c := *card
	c.Segment = target
	return CardPlan{Segs: segs, Plan: plan, Warnings: warns, Text: eff, Card: &c}
}
