package dispatch

import (
	"context"
	"log/slog"
	"time"

	"github.com/geofox/publisher/internal/destination"
	"github.com/geofox/publisher/internal/unfurl"
)

// scrubLinkCards drops any client-supplied link_card from the targets.
// LinkCard is dispatcher-computed only (attachLinkCard + PlanTarget); a
// request-supplied card must never reach an adapter or fields_json.
func scrubLinkCards(targets []TargetSpec) {
	for i := range targets {
		targets[i].Overrides.LinkCard = nil
	}
}

// attachLinkCard unfurls a link card for every target whose account builds
// link previews itself (LinkPreview "client"): the card URL (trailing,
// else first) of that target's text goes into its LinkCard. Best-effort:
// on any unfurl error the post proceeds without a card (a bare faceted
// link). Placement and trailing-strip happen in PlanTarget.
func (d *Dispatcher) attachLinkCard(ctx context.Context, spec *PostSpec) {
	if d.Unfurler == nil {
		return
	}
	cards := map[string]*unfurl.Card{} // by URL: one fetch per link
	for i, t := range spec.Targets {
		if caps := d.Caps(ctx, t.AccountID, t.Type); caps.LinkPreview != destination.LinkPreviewClient {
			continue
		}
		u, _, ok := unfurl.CardURL(spec.text(t))
		if !ok {
			continue
		}
		card, seen := cards[u]
		if !seen {
			uctx, cancel := context.WithTimeout(ctx, 15*time.Second)
			c, err := d.Unfurler.Unfurl(uctx, u)
			cancel()
			if err != nil {
				slog.WarnContext(ctx, "unfurl failed; posting without link card", "url", u, "err", err)
			}
			card, cards[u] = c, c
		}
		if card == nil {
			continue
		}
		cp := *card
		spec.Targets[i].Overrides.LinkCard = &cp
	}
}

func ov2fields(o Overrides) map[string]any {
	return map[string]any{
		"language": o.Language, "visibility": o.Visibility, "sensitive": o.Sensitive,
		"spoiler_text": o.SpoilerText, "content_warning": o.ContentWarning, "pow": o.POW,
		"langs": o.Langs, "topic_tag": o.TopicTag,
		"threads_reply_control": o.ThreadsReplyControl,
		"bluesky_reply":         o.BlueskyReply, "bluesky_disable_quotes": o.BlueskyDisableQuotes,
		"link_card": o.LinkCard,
	}
}
