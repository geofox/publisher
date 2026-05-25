package resolve

import (
	"context"
	"fmt"

	gonostr "fiatjaf.com/nostr"
	"fiatjaf.com/nostr/nip19"
	bsky "github.com/geofox/publisher/internal/bluesky"
	mast "github.com/geofox/publisher/internal/mastodon"
	pubnostr "github.com/geofox/publisher/internal/nostr"
)

// NostrSourceClient is the read surface resolve needs from the nostr publisher.
// internal/nostr.Publisher satisfies it via its ResolveSource method.
type NostrSourceClient interface {
	ResolveSource(ctx context.Context, input string) (*pubnostr.SourceEvent, error)
}

// NostrAdapter maps a fetched Nostr SourceEvent to a platform-neutral SourceRef.
// Nostr is an open protocol, so every action is allowed; a NIP-70 protected
// event only annotates (does not disable) the repost capability.
type NostrAdapter struct{ P NostrSourceClient }

func (a NostrAdapter) ResolveSource(ctx context.Context, input string) (*SourceRef, error) {
	ev, err := a.P.ResolveSource(ctx, input)
	if err != nil {
		return nil, err
	}
	repostReason := ""
	if ev.Protected {
		repostReason = "protected event (NIP-70): repost won't embed it — quote instead"
	}
	name := ev.AuthorName
	if name == "" {
		name = "npub:" + shortHex(ev.Author)
	}
	return &SourceRef{
		Platform: "nostr",
		Ref: PlatformRef{
			EventID:    ev.IDHex,
			Author:     ev.Author,
			Kind:       ev.Kind,
			RelayHints: ev.RelayHints,
		},
		Preview: Preview{
			AuthorName: name,
			Text:       ev.Content,
			CreatedAt:  ev.CreatedAt,
			WebURL:     njumpURL(ev.IDHex, ev.Author, ev.RelayHints),
		},
		Caps: Caps{
			Reply:  Cap{Allowed: true},
			Quote:  Cap{Allowed: true},
			Repost: Cap{Allowed: true, Reason: repostReason},
		},
	}, nil
}

func shortHex(s string) string {
	if len(s) > 8 {
		return s[:8]
	}
	return s
}

// njumpURL builds an njump.me web link so non-Nostr platforms get a clickable
// URL. It prefers a self-describing nevent (id + relays + author); on any
// encoding failure it falls back to the bare-hex form, which njump also accepts.
func njumpURL(idHex, authorHex string, relays []string) string {
	if nevent := encodeNeventBestEffort(idHex, authorHex, relays); nevent != "" {
		return "https://njump.me/" + nevent
	}
	return "https://njump.me/" + idHex
}

// encodeNeventBestEffort converts hex id/author into a NIP-19 nevent, or returns
// "" if the id isn't a valid 32-byte hex (e.g. truncated test fixtures). The
// author is optional: a bad/empty author still yields an nevent (encoded with
// the zero pubkey, which nip19 omits).
func encodeNeventBestEffort(idHex, authorHex string, relays []string) string {
	id, err := gonostr.IDFromHex(idHex)
	if err != nil {
		return ""
	}
	var author gonostr.PubKey
	if pk, err := gonostr.PubKeyFromHex(authorHex); err == nil {
		author = pk
	}
	return nip19.EncodeNevent(id, relays, author)
}

// MastodonSourceClient is the read surface resolve needs from the mastodon client.
type MastodonSourceClient interface {
	ResolveStatus(ctx context.Context, url string) (*mast.SourceStatus, error)
}

// MastodonAdapter maps a fetched Mastodon SourceStatus to a platform-neutral SourceRef.
// quote_approval.current_user drives the Quote cap; private/direct posts block Repost.
type MastodonAdapter struct{ C MastodonSourceClient }

func (a MastodonAdapter) ResolveSource(ctx context.Context, input string) (*SourceRef, error) {
	st, err := a.C.ResolveStatus(ctx, input)
	if err != nil {
		return nil, err
	}
	quote := Cap{}
	switch st.QuoteCurrentUser {
	case "automatic":
		quote = Cap{Allowed: true}
	case "manual":
		quote = Cap{Allowed: true, Reason: "needs the author's approval (lands pending)"}
	default:
		quote = Cap{Allowed: false, Reason: "native quote not available — will link instead"}
	}
	repost := Cap{Allowed: true}
	if st.Visibility == "private" || st.Visibility == "direct" {
		repost = Cap{Allowed: false, Reason: "this post's visibility can't be boosted"}
	}
	media := make([]Media, 0, len(st.Media))
	for _, m := range st.Media {
		media = append(media, Media{URL: m.URL, Alt: m.Alt})
	}
	name := st.AuthorName
	if name == "" {
		name = st.AuthorAcct
	}
	return &SourceRef{
		Platform: "mastodon",
		Ref:      PlatformRef{LocalID: st.LocalID},
		Preview: Preview{
			AuthorName: name, AuthorHandle: "@" + st.AuthorAcct,
			Text: st.TextPlain, Media: media, CreatedAt: st.CreatedAt, WebURL: st.WebURL,
		},
		Caps: Caps{Reply: Cap{Allowed: true}, Quote: quote, Repost: repost},
	}, nil
}

// BlueskySourceClient is the read surface resolve needs from the bluesky client.
type BlueskySourceClient interface {
	GetPost(ctx context.Context, url string) (*bsky.SourcePost, error)
}

// BlueskyAdapter maps a fetched Bluesky SourcePost to a platform-neutral SourceRef.
// viewer.embeddingDisabled → quote blocked; viewer.replyDisabled → reply blocked;
// repost is always allowed on Bluesky.
type BlueskyAdapter struct{ C BlueskySourceClient }

func (a BlueskyAdapter) ResolveSource(ctx context.Context, input string) (*SourceRef, error) {
	p, err := a.C.GetPost(ctx, input)
	if err != nil {
		return nil, err
	}
	if p.NotFoundOrBlocked {
		return nil, fmt.Errorf("post not found or blocked")
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
	media := make([]Media, 0, len(p.Media))
	for _, m := range p.Media {
		media = append(media, Media{URL: m.URL, Alt: m.Alt})
	}
	name := p.AuthorName
	if name == "" {
		name = p.AuthorHandle
	}
	return &SourceRef{
		Platform: "bluesky",
		Ref:      PlatformRef{URI: p.URI, CID: p.CID, ReplyRootURI: p.ReplyRootURI, ReplyRootCID: p.ReplyRootCID},
		Preview: Preview{
			AuthorName: name, AuthorHandle: "@" + p.AuthorHandle,
			Text: p.Text, Media: media, CreatedAt: p.CreatedAt, WebURL: p.WebURL,
		},
		Caps: Caps{
			Reply:  Cap{Allowed: !p.ReplyDisabled, Reason: replyReason},
			Quote:  Cap{Allowed: !p.EmbeddingDisabled, Reason: quoteReason},
			Repost: Cap{Allowed: true},
		},
	}, nil
}
