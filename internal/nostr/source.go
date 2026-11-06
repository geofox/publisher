package nostr

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"strings"
	"time"

	gonostr "fiatjaf.com/nostr"
	"fiatjaf.com/nostr/nip19"

	"github.com/geofox/publisher/internal/netguard"
)

// SourceEvent is a fetched external event plus decoded pointer metadata. It is
// the read counterpart to PublishResult: enough to render a preview and act on
// (reply/quote/repost) the referenced event later.
type SourceEvent struct {
	IDHex      string
	Author     string // hex pubkey
	Kind       int
	Content    string
	CreatedAt  time.Time
	RelayHints []string
	Protected  bool   // NIP-70 ["-"] tag present
	AuthorName string // best-effort, from kind-0 profile metadata
	// ThreadRootID is the root of the thread a kind-1 event replies into
	// (its NIP-10 "root" e-tag, or the first e-tag in the older positional
	// form); empty when the event starts a thread. Mentions are its p-tags.
	ThreadRootID string
	Mentions     []string
}

// maxReplyMentions caps the p-tags a reply copies from the event it answers.
const maxReplyMentions = 50

// threadContext reads a kind-1 event's NIP-10 thread root and p-tags.
func threadContext(tags gonostr.Tags) (root string, mentions []string) {
	var firstE string
	marked := false
	seen := map[string]bool{}
	for _, t := range tags {
		if len(t) < 2 {
			continue
		}
		switch t[0] {
		case "e":
			if !isHex64(t[1]) {
				continue
			}
			if len(t) > 3 && t[3] == "root" {
				root = t[1]
			}
			if len(t) > 3 && (t[3] == "root" || t[3] == "reply") {
				marked = true
			}
			if firstE == "" {
				firstE = t[1]
			}
		case "p":
			if isHex64(t[1]) && !seen[t[1]] && len(mentions) < maxReplyMentions {
				seen[t[1]] = true
				mentions = append(mentions, t[1])
			}
		}
	}
	if root == "" && !marked {
		root = firstE // deprecated positional form: the first e-tag is the root
	}
	return root, mentions
}

func isHex64(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, c := range s {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

// eventPointer is the decoded identity of a reference, before the event itself
// is fetched.
type eventPointer struct {
	IDHex      string
	Author     string // hex pubkey, empty unless the reference carried one
	Kind       int
	RelayHints []string
}

// parseEventInput decodes a NIP-19/NIP-21 reference or a raw 64-char hex id.
// naddr (addressable) is out of scope for v1 fetch-by-id and errors.
func parseEventInput(in string) (eventPointer, error) {
	in = strings.TrimSpace(in)
	in = strings.TrimPrefix(in, "web+nostr:")
	in = strings.TrimPrefix(in, "nostr:")
	if len(in) == 64 {
		if b, err := hex.DecodeString(in); err == nil {
			return eventPointer{IDHex: hex.EncodeToString(b)}, nil
		}
	}
	prefix, value, err := nip19.Decode(in)
	if err != nil {
		return eventPointer{}, fmt.Errorf("decode %q: %w", in, err)
	}
	switch prefix {
	case "note", "nevent":
		ep, ok := value.(gonostr.EventPointer)
		if !ok {
			return eventPointer{}, fmt.Errorf("unexpected pointer type for %s", prefix)
		}
		return eventPointer{
			IDHex:      ep.ID.Hex(),
			Author:     pubkeyHexOrEmpty(ep.Author),
			Kind:       int(ep.Kind),
			RelayHints: ep.Relays,
		}, nil
	default:
		return eventPointer{}, fmt.Errorf("unsupported reference %q (paste an nevent/note/hex event id)", prefix)
	}
}

// ResolveSource fetches the referenced event from relays (pointer hints unioned
// with the owner's write relays) and best-effort resolves the author's display
// name from their kind-0 profile metadata.
func (p *Publisher) ResolveSource(ctx context.Context, input string) (*SourceEvent, error) {
	ptr, err := parseEventInput(input)
	if err != nil {
		return nil, err
	}
	hints := publicRelayHints(ctx, net.DefaultResolver, ptr.RelayHints)
	urls := p.sourceRelays(ctx, hints)
	id, err := gonostr.IDFromHex(ptr.IDHex)
	if err != nil {
		return nil, fmt.Errorf("bad event id: %w", err)
	}
	ev := p.fetchOne(ctx, urls, gonostr.Filter{IDs: []gonostr.ID{id}, Limit: 1})
	if ev == nil {
		return nil, fmt.Errorf("event %s not found on %d relays", ptr.IDHex[:8], len(urls))
	}
	src := &SourceEvent{
		IDHex:      ptr.IDHex,
		Author:     ev.PubKey.Hex(),
		Kind:       int(ev.Kind),
		Content:    ev.Content,
		CreatedAt:  ev.CreatedAt.Time(),
		RelayHints: hints,
		Protected:  hasProtectedTag(ev.Tags),
	}
	if ev.Kind == 1 {
		src.ThreadRootID, src.Mentions = threadContext(ev.Tags)
	}
	if prof := p.fetchOne(ctx, urls, gonostr.Filter{
		Authors: []gonostr.PubKey{ev.PubKey},
		Kinds:   []gonostr.Kind{0},
		Limit:   1,
	}); prof != nil {
		var meta struct {
			Name        string `json:"name"`
			DisplayName string `json:"display_name"`
		}
		if json.Unmarshal([]byte(prof.Content), &meta) == nil {
			src.AuthorName = firstNonEmpty(meta.DisplayName, meta.Name)
		}
	}
	return src, nil
}

// OwnerProfile returns the operator's npub (encoded from the configured pubkey)
// plus a best-effort display name + avatar from their kind-0 profile metadata on
// relays. The npub is always returned; name/picture are empty when no kind-0 is
// found.
func (p *Publisher) OwnerProfile(ctx context.Context) (npub, name, picture string, err error) {
	npub = nip19.EncodeNpub(p.cfg.OwnerPubkey)
	urls := p.sourceRelays(ctx, nil)
	prof := p.fetchOne(ctx, urls, gonostr.Filter{
		Authors: []gonostr.PubKey{p.cfg.OwnerPubkey},
		Kinds:   []gonostr.Kind{0},
		Limit:   1,
	})
	if prof == nil {
		return npub, "", "", nil
	}
	var meta struct {
		Name        string `json:"name"`
		DisplayName string `json:"display_name"`
		Picture     string `json:"picture"`
	}
	if json.Unmarshal([]byte(prof.Content), &meta) == nil {
		name = firstNonEmpty(meta.DisplayName, meta.Name)
		picture = meta.Picture
	}
	return npub, name, picture, nil
}

// hasProtectedTag reports whether the event carries a NIP-70 ["-"] tag, which
// asks relays not to accept the event from anyone but its author — so a repost
// (which rebroadcasts the original) can't reliably propagate it.
func hasProtectedTag(tags gonostr.Tags) bool {
	for _, t := range tags {
		if len(t) >= 1 && t[0] == "-" {
			return true
		}
	}
	return false
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func pubkeyHexOrEmpty(pk gonostr.PubKey) string {
	if pk == gonostr.ZeroPK {
		return ""
	}
	return pk.Hex()
}

// maxRelayHints caps how many relays a pasted reference may name; the rest
// are ignored.
const maxRelayHints = 4

// publicRelayHints keeps the hints the server may dial: wss:// only, public
// hosts only, at most maxRelayHints. Hints come from whoever made the
// reference, so without this a pasted nevent could make the server open
// connections to its own network (SSRF) or to hundreds of relays.
func publicRelayHints(ctx context.Context, r netguard.Resolver, hints []string) []string {
	var out []string
	for _, h := range hints {
		if len(out) == maxRelayHints {
			break
		}
		if IsOverlayRelay(h) || netguard.CheckPublicURL(ctx, r, h, "wss") != nil {
			continue
		}
		out = append(out, h)
	}
	return out
}

// sourceRelays unions pointer hints (already checked by publicRelayHints) with the
// owner's NIP-65 write relays (and the configured fallbacks), dropping
// overlay relays the container can't reach and de-duping.
func (p *Publisher) sourceRelays(ctx context.Context, hints []string) []string {
	seen := map[string]bool{}
	var out []string
	add := func(u string) {
		if u == "" || seen[u] || IsOverlayRelay(u) {
			return
		}
		seen[u] = true
		out = append(out, u)
	}
	for _, h := range hints {
		add(h)
	}
	if write, err := p.ResolveWriteRelays(ctx); err == nil {
		for _, u := range write {
			add(u)
		}
	}
	for _, u := range p.cfg.FallbackRelays {
		add(u)
	}
	return out
}

// fetchOne returns the first event matching filter across urls, or nil.
// QuerySingle returns the first event from the first relay to answer and cancels
// the rest; RelayEvent embeds Event, so we copy the embedded value out.
func (p *Publisher) fetchOne(ctx context.Context, urls []string, filter gonostr.Filter) *gonostr.Event {
	if len(urls) == 0 {
		return nil
	}
	if re := p.pool.QuerySingle(ctx, urls, filter, gonostr.SubscriptionOptions{}); re != nil {
		ev := re.Event
		return &ev
	}
	return nil
}
