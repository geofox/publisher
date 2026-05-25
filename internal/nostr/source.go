package nostr

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	gonostr "fiatjaf.com/nostr"
	"fiatjaf.com/nostr/nip19"
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
	urls := p.sourceRelays(ctx, ptr.RelayHints)
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
		RelayHints: ptr.RelayHints,
		Protected:  hasProtectedTag(ev.Tags),
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

// sourceRelays unions pointer hints with the owner's NIP-65 write relays (and
// the configured fallbacks), dropping overlay relays the container can't reach
// and de-duping.
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
