package resolve

import (
	"context"

	"github.com/geofox/publisher/internal/destination"
)

// AccountAdapter resolves through an account (destination.SourceResolver)
// and gives the result the shape /v1/resolve has always had. The drivers
// map their networks' posts; this only renames fields.
type AccountAdapter struct{ R destination.SourceResolver }

func (a AccountAdapter) ResolveSource(ctx context.Context, input string) (*SourceRef, error) {
	rs, err := a.R.ResolveSource(ctx, input)
	if err != nil {
		return nil, err
	}
	return FromResolved(rs), nil
}

// FromResolved converts a driver's ResolvedSource to a SourceRef.
func FromResolved(rs destination.ResolvedSource) *SourceRef {
	r := rs.Ref
	media := make([]Media, 0, len(rs.Preview.Media))
	for _, m := range rs.Preview.Media {
		media = append(media, Media{URL: m.URL, Alt: m.Alt, Video: m.Video})
	}
	allowed := func(a destination.ActionAllowed) Cap { return Cap{Allowed: a.Allowed, Reason: a.Reason} }
	return &SourceRef{
		Platform: r.Type,
		Ref: PlatformRef{
			URI: r.URI, CID: r.CID, ReplyRootURI: r.ReplyRootURI, ReplyRootCID: r.ReplyRootCID,
			LocalID: r.LocalID,
			EventID: r.EventID, Author: r.Author, RelayHints: r.RelayHints, Kind: r.EventKind,
			ThreadRootID: r.ThreadRootID, Mentions: r.Mentions,
		},
		Preview: Preview{
			AuthorName: rs.Preview.AuthorName, AuthorHandle: rs.Preview.AuthorHandle,
			Text: rs.Preview.Text, Media: media, CreatedAt: rs.Preview.CreatedAt, WebURL: rs.Preview.WebURL,
		},
		Caps: Caps{Reply: allowed(rs.Actions.Reply), Quote: allowed(rs.Actions.Quote), Repost: allowed(rs.Actions.Repost)},
	}
}
