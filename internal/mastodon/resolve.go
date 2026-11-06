package mastodon

import (
	"context"
	"errors"
	"fmt"

	"github.com/geofox/publisher/internal/destination"
)

var _ destination.SourceResolver = (*Account)(nil)

// ResolveSource implements destination.SourceResolver: the instance's
// search finds the post, so the id it returns is valid for this account.
func (a *Account) ResolveSource(ctx context.Context, input string) (destination.ResolvedSource, error) {
	st, err := a.C.ResolveStatus(ctx, input)
	if err != nil {
		if errors.Is(err, ErrStatusNotFound) {
			return destination.ResolvedSource{}, fmt.Errorf("%w: %w", destination.ErrSourceNotFound, err)
		}
		return destination.ResolvedSource{}, err
	}
	return resolvedFromStatus(st), nil
}

// resolvedFromStatus maps a resolved status: quote_approval.current_user
// decides a quote; a private or direct post can't be boosted.
func resolvedFromStatus(st *SourceStatus) destination.ResolvedSource {
	var quote destination.ActionAllowed
	switch st.QuoteCurrentUser {
	case "automatic":
		quote = destination.ActionAllowed{Allowed: true}
	case "manual":
		quote = destination.ActionAllowed{Allowed: true, Reason: "needs the author's approval (lands pending)"}
	case "denied":
		quote = destination.ActionAllowed{Reason: "the author doesn't allow quotes of this post"}
	default:
		quote = destination.ActionAllowed{Reason: "this post can't be quoted from your server (native quotes need Mastodon 4.5 or later); share its link instead"}
	}
	repost := destination.ActionAllowed{Allowed: true}
	if st.Visibility == "private" || st.Visibility == "direct" {
		repost = destination.ActionAllowed{Reason: "this post's visibility can't be boosted"}
	}
	media := make([]destination.SourceMedia, 0, len(st.Media))
	for _, m := range st.Media {
		media = append(media, destination.SourceMedia{URL: m.URL, Alt: m.Alt, Video: m.Video})
	}
	name := st.AuthorName
	if name == "" {
		name = st.AuthorAcct
	}
	return destination.ResolvedSource{
		Ref: destination.SourceRef{Type: Type, LocalID: st.LocalID},
		Preview: destination.SourcePreview{
			AuthorName: name, AuthorHandle: "@" + st.AuthorAcct,
			Text: st.TextPlain, Media: media, CreatedAt: st.CreatedAt, WebURL: st.WebURL,
		},
		Actions: destination.SourceActions{Reply: destination.ActionAllowed{Allowed: true}, Quote: quote, Repost: repost},
	}
}
