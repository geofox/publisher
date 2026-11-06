package nostr

import (
	"context"

	gonostr "fiatjaf.com/nostr"
	"fiatjaf.com/nostr/nip19"

	"github.com/geofox/publisher/internal/destination"
)

var _ destination.SourceResolver = (*Account)(nil)

// ResolveSource implements destination.SourceResolver for a nevent, note
// or naddr id (or its nostr: link), fetched from the relays it names and
// the account's own.
func (a *Account) ResolveSource(ctx context.Context, input string) (destination.ResolvedSource, error) {
	ev, err := a.P.ResolveSource(ctx, input)
	if err != nil {
		return destination.ResolvedSource{}, err
	}
	return resolvedFromEvent(ev), nil
}

// resolvedFromEvent maps a fetched event. Nostr is an open protocol, so
// every action is allowed; a NIP-70 protected event only annotates the
// repost.
func resolvedFromEvent(ev *SourceEvent) destination.ResolvedSource {
	repostReason := ""
	if ev.Protected {
		repostReason = "protected event (NIP-70): repost won't embed it — quote instead"
	}
	name := ev.AuthorName
	if name == "" {
		name = "npub:" + shortHex(ev.Author)
	}
	return destination.ResolvedSource{
		Ref: destination.SourceRef{
			Type:         Type,
			EventID:      ev.IDHex,
			Author:       ev.Author,
			EventKind:    ev.Kind,
			RelayHints:   ev.RelayHints,
			ThreadRootID: ev.ThreadRootID,
			Mentions:     ev.Mentions,
		},
		Preview: destination.SourcePreview{
			AuthorName: name,
			Text:       ev.Content,
			CreatedAt:  ev.CreatedAt,
			WebURL:     njumpURL(ev.IDHex, ev.Author, ev.RelayHints),
		},
		Actions: destination.SourceActions{
			Reply:  destination.ActionAllowed{Allowed: true},
			Quote:  destination.ActionAllowed{Allowed: true},
			Repost: destination.ActionAllowed{Allowed: true, Reason: repostReason},
		},
	}
}

func shortHex(s string) string {
	if len(s) > 8 {
		return s[:8]
	}
	return s
}

// njumpURL builds an njump.me web link so non-Nostr platforms get a
// clickable URL. It prefers a self-describing nevent (id + relays +
// author); when the id can't be encoded it falls back to the bare hex
// form, which njump also accepts.
func njumpURL(idHex, authorHex string, relays []string) string {
	if nevent := encodeNeventBestEffort(idHex, authorHex, relays); nevent != "" {
		return "https://njump.me/" + nevent
	}
	return "https://njump.me/" + idHex
}

// encodeNeventBestEffort converts a hex id and author into a NIP-19
// nevent, or "" when the id isn't 32 bytes of hex. The author is optional:
// a bad or empty one still yields an nevent (encoded with the zero key,
// which nip19 omits).
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
