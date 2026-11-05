package dispatch

import (
	"context"
	"log/slog"
	"time"

	"github.com/geofox/publisher/internal/destination"
	"github.com/geofox/publisher/internal/unfurl"
)

// scrubLinkCards drops any client-supplied link_card from incoming overrides.
// LinkCard is dispatcher-computed only (attachLinkCard + PlanBlueskyCard);
// a request-supplied card must never reach an adapter or fields_json.
func scrubLinkCards(overrides map[string]Overrides) {
	for k, ov := range overrides {
		if ov.LinkCard != nil {
			ov.LinkCard = nil
			overrides[k] = ov
		}
	}
}

// attachLinkCard unfurls a link card for every targeted destination that
// builds link previews itself (LinkPreview "client"): the card URL (trailing,
// else first) of that target's effective text goes into its override's
// LinkCard. Best-effort: on any unfurl error the post proceeds without a card
// (a bare faceted link). Placement and trailing-strip happen in PlanTarget.
func (d *Dispatcher) attachLinkCard(ctx context.Context, spec *PostSpec) {
	if d.Unfurler == nil {
		return
	}
	cards := map[string]*unfurl.Card{} // by URL: one fetch per link
	for _, p := range dedupStrings(spec.Platforms) {
		if caps, _ := d.Caps(ctx, p); caps.LinkPreview != destination.LinkPreviewClient {
			continue
		}
		ov := spec.Overrides[p]
		text := spec.MasterText
		if ov.Text != "" {
			text = ov.Text
		}
		u, _, ok := unfurl.CardURL(text)
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
		ov.LinkCard = &cp
		if spec.Overrides == nil {
			spec.Overrides = map[string]Overrides{}
		}
		spec.Overrides[p] = ov
	}
}

func finalText(spec PostSpec, plat string) string {
	if ov, ok := spec.Overrides[plat]; ok && ov.Text != "" {
		return ov.Text
	}
	return spec.MasterText
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
