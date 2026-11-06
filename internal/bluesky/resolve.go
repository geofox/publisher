package bluesky

import (
	"context"
	"fmt"

	"github.com/geofox/publisher/internal/destination"
)

var _ destination.SourceResolver = (*Account)(nil)

// ResolveSource implements destination.SourceResolver for a bsky.app post
// URL, read through the account's own session (the viewer flags say what
// this account may do).
func (a *Account) ResolveSource(ctx context.Context, input string) (destination.ResolvedSource, error) {
	p, err := a.C.GetPost(ctx, input)
	if err != nil {
		return destination.ResolvedSource{}, err
	}
	return resolvedFromPost(p)
}

// resolvedFromPost maps a fetched post: viewer.replyDisabled blocks a
// reply, viewer.embeddingDisabled a quote; a repost is always allowed.
func resolvedFromPost(p *SourcePost) (destination.ResolvedSource, error) {
	if p.NotFoundOrBlocked {
		return destination.ResolvedSource{}, fmt.Errorf("%w: not found or blocked", destination.ErrSourceNotFound)
	}
	replyReason := ""
	if p.ReplyDisabled {
		replyReason = "replies are restricted by the author"
		if p.ThreadgateReason != "" {
			replyReason = p.ThreadgateReason
		}
	}
	quoteReason := ""
	if p.EmbeddingDisabled {
		quoteReason = "the author disabled quotes for this post"
	}
	media := make([]destination.SourceMedia, 0, len(p.Media))
	for _, m := range p.Media {
		media = append(media, destination.SourceMedia{URL: m.URL, Alt: m.Alt})
	}
	name := p.AuthorName
	if name == "" {
		name = p.AuthorHandle
	}
	return destination.ResolvedSource{
		Ref: destination.SourceRef{Type: Type, URI: p.URI, CID: p.CID,
			ReplyRootURI: p.ReplyRootURI, ReplyRootCID: p.ReplyRootCID},
		Preview: destination.SourcePreview{
			AuthorName: name, AuthorHandle: "@" + p.AuthorHandle,
			Text: p.Text, Media: media, CreatedAt: p.CreatedAt, WebURL: p.WebURL,
		},
		Actions: destination.SourceActions{
			Reply:  destination.ActionAllowed{Allowed: !p.ReplyDisabled, Reason: replyReason},
			Quote:  destination.ActionAllowed{Allowed: !p.EmbeddingDisabled, Reason: quoteReason},
			Repost: destination.ActionAllowed{Allowed: true},
		},
	}, nil
}
