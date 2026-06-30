// Package nostr provides a high-level Nostr event publisher that handles
// POW mining, NIP-65 outbox relay resolution, relay-list caching, and
// fan-out publishing via a connection pool.
package nostr

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"sync"
	"time"

	gonostr "fiatjaf.com/nostr"
	"fiatjaf.com/nostr/nip13"
	"fiatjaf.com/nostr/nip19"

	"github.com/geofox/publisher/internal/progress"
)

// ErrInvalidInput is returned by Publish when the caller supplies an empty text field.
var ErrInvalidInput = errors.New("text is required")

// requiresText reports whether an event of this kind must have non-empty content.
// NIP-18 reposts (kind 6 / generic 16) legitimately carry empty content, so they
// are exempt; everything else (text notes etc.) requires text.
func requiresText(kind int) bool { return kind != 6 && kind != 16 }

// RelayResult captures the outcome of a single relay publish attempt.
type RelayResult struct {
	URL     string `json:"url"`
	OK      bool   `json:"ok"`
	Skipped bool   `json:"skipped,omitempty"` // overlay relay (.onion/.i2p): not dialed
	Message string `json:"message,omitempty"`
}

// NostrReply carries NIP-10 reply context for a threaded event. RootID is the
// chain's first event; ParentID is the immediately-preceding one (equal to
// RootID when replying directly to the root). RelayHint is an optional relay URL.
type NostrReply struct {
	RootID, ParentID, RelayHint string
	AuthorPubkey                string // replied-to author (hex); empty → self-thread, p-tag the owner
}

// PublishInput carries everything needed to build and publish a Nostr event.
type PublishInput struct {
	Text    string
	Kind    int
	Imetas  []gonostr.Tag // optional NIP-92, one tag per attached media
	POW     *int          // nil → use Config.POWDifficultyDefault
	ReplyTo *NostrReply   // when set, adds NIP-10 e/p tags (threading)
	Tags    []gonostr.Tag // extra tags appended before signing (e.g. NIP-18 q/e/p/k)
}

// PublishResult summarises the event that was broadcast.
type PublishResult struct {
	EventID     string
	Nevent      string // NIP-19 nevent (id + relay hints + author) for viewer links
	SignedEvent string // marshaled signed event, for per-relay rebroadcast
	Kind        int
	POW         int
	MinedMS     int64
	Relays        []RelayResult
	PendingRelays []string // secondary relays deferred to async fan-out (fan-out mode only)
}

// Config holds all tunables for a Publisher instance.
type Config struct {
	NSEC                 gonostr.SecretKey
	OwnerPubkey          gonostr.PubKey
	NIP65BootstrapRelay  string
	FallbackRelays       []string
	POWDifficultyDefault int
	POWDifficultyMax     int
	POWTimeout           time.Duration
	RelayCacheTTL        time.Duration
	PublishTimeout       time.Duration
	PrimaryFanout bool     // when true, publish synchronously to PrimaryRelays only
	PrimaryRelays []string // the primary relay set (e.g. the owner's own relay)
}

// publishPool is the subset of *gonostr.Pool that Publisher needs; an
// interface so tests can inject fake relay results.
type publishPool interface {
	PublishMany(ctx context.Context, urls []string, evt gonostr.Event) chan gonostr.PublishResult
	QuerySingle(ctx context.Context, urls []string, filter gonostr.Filter, opts gonostr.SubscriptionOptions) *gonostr.RelayEvent
}

// Publisher is a stateful Nostr broadcast client. Create one with New and
// reuse it across requests — it holds a persistent relay pool and a relay
// list cache keyed by pubkey.
type Publisher struct {
	cfg   Config
	pool  publishPool
	mu    sync.RWMutex
	cache map[gonostr.PubKey]relayCacheEntry
}

type relayCacheEntry struct {
	write []string
	read  []string
	exp   time.Time
}

// New constructs a Publisher with a fresh connection pool.
func New(cfg Config) *Publisher {
	return &Publisher{
		cfg:   cfg,
		pool:  gonostr.NewPool(),
		cache: map[gonostr.PubKey]relayCacheEntry{},
	}
}

// Publish builds, optionally mines, signs, and broadcasts a Nostr event.
// It returns PublishResult even on partial relay failure — the caller
// decides how to interpret the relay results slice.
func (p *Publisher) Publish(ctx context.Context, in PublishInput) (PublishResult, error) {
	if strings.TrimSpace(in.Text) == "" && requiresText(in.Kind) {
		return PublishResult{}, ErrInvalidInput
	}
	kind := in.Kind
	if kind == 0 {
		kind = 1
	}
	text := in.Text
	for _, im := range in.Imetas {
		if url := extractImetaURL(im); url != "" && !strings.Contains(text, url) {
			text = text + "\n\n" + url
		}
	}
	pow := p.cfg.POWDifficultyDefault
	if in.POW != nil {
		pow = *in.POW
	}
	if pow < 0 {
		pow = 0
	}
	if pow > p.cfg.POWDifficultyMax {
		pow = p.cfg.POWDifficultyMax
	}

	event := gonostr.Event{
		PubKey:    p.cfg.OwnerPubkey,
		CreatedAt: gonostr.Timestamp(time.Now().Unix()),
		Kind:      gonostr.Kind(kind),
		Tags:      gonostr.Tags{},
		Content:   text,
	}
	for _, im := range in.Imetas {
		if len(im) > 0 {
			event.Tags = append(event.Tags, im)
		}
	}
	for _, tg := range replyTags(in.ReplyTo, p.cfg.OwnerPubkey.Hex()) {
		event.Tags = append(event.Tags, tg)
	}
	for _, tg := range in.Tags {
		event.Tags = append(event.Tags, tg)
	}

	var minedMS int64
	if pow > 0 {
		wctx, cancel := context.WithTimeout(ctx, p.cfg.POWTimeout)
		defer cancel()
		start := time.Now()
		nonce, err := nip13.DoWork(wctx, event, pow)
		if err != nil {
			return PublishResult{}, fmt.Errorf("mining: %w", err)
		}
		event.Tags = append(event.Tags, nonce)
		minedMS = time.Since(start).Milliseconds()
	}
	if err := event.Sign(p.cfg.NSEC); err != nil {
		return PublishResult{}, fmt.Errorf("sign: %w", err)
	}

	relays, err := p.resolveRelays(ctx, p.cfg.OwnerPubkey)
	if err != nil || len(relays) == 0 {
		slog.Warn("relay resolution fell back", "err", err)
		relays = p.cfg.FallbackRelays
	}
	relays = dedup(append(relays, p.cfg.NIP65BootstrapRelay))

	var pending []string
	var attempted, skipped []string
	if p.cfg.PrimaryFanout {
		attempted, pending = selectPrimary(relays, p.cfg.PrimaryRelays)
		// overlay relays already excluded by selectPrimary; record none as skipped
	} else {
		// Partition: clearnet relays are dialed; overlay relays (.onion/.i2p) are
		// recorded as skipped (no Tor/I2P egress) and excluded from the tally.
		for _, u := range relays {
			if IsOverlayRelay(u) {
				skipped = append(skipped, u)
			} else {
				attempted = append(attempted, u)
			}
		}
	}

	results := p.publishToRelays(ctx, event, attempted)
	for _, u := range skipped {
		results = append(results, RelayResult{URL: u, Skipped: true})
	}

	// Relay hints for the nevent: prefer relays that accepted, capped; else attempted.
	var hints []string
	for _, rr := range results {
		if rr.OK {
			hints = append(hints, rr.URL)
		}
	}
	if len(hints) == 0 {
		hints = attempted
	}
	if len(hints) > 3 {
		hints = hints[:3]
	}

	signed, _ := json.Marshal(event) // event is signed above; Event is JSON-safe
	return PublishResult{
		EventID:       event.ID.Hex(),
		Nevent:        nip19.EncodeNevent(event.ID, hints, p.cfg.OwnerPubkey),
		SignedEvent:   string(signed),
		Kind:          kind,
		POW:           pow,
		MinedMS:       minedMS,
		Relays:        results,
		PendingRelays: pending,
	}, nil
}

// publishToRelays fans out via the pool. PublishMany returns a channel that's
// closed when every goroutine finishes, so a simple `for r := range ch` is
// the canonical iteration. We bound the wait with PublishTimeout.
func (p *Publisher) publishToRelays(ctx context.Context, event gonostr.Event, urls []string) []RelayResult {
	ctx, cancel := context.WithTimeout(ctx, p.cfg.PublishTimeout)
	defer cancel()

	sink := progress.SinkFrom(ctx)
	sink.RelaysQueued("nostr", urls)

	results := make([]RelayResult, 0, len(urls))
	for r := range p.pool.PublishMany(ctx, urls, event) {
		rr := RelayResult{URL: r.RelayURL, OK: r.Error == nil}
		status := progress.RelayOK
		if r.Error != nil {
			rr.Message = r.Error.Error()
			status = progress.RelayFailed
		}
		sink.Relay("nostr", rr.URL, status, rr.Message)
		results = append(results, rr)
	}
	return results
}

// RebroadcastToRelay re-sends an already-signed event (same id, no mining) to a
// single relay. Used by per-relay retry so a partially-delivered note can be
// pushed to a relay that failed, without minting a duplicate event.
func (p *Publisher) RebroadcastToRelay(ctx context.Context, signedEventJSON, relayURL string) RelayResult {
	if IsOverlayRelay(relayURL) {
		return RelayResult{URL: relayURL, Skipped: true}
	}
	var ev gonostr.Event
	if err := json.Unmarshal([]byte(signedEventJSON), &ev); err != nil {
		return RelayResult{URL: relayURL, OK: false, Message: "bad stored event: " + err.Error()}
	}
	res := p.publishToRelays(ctx, ev, []string{relayURL})
	if len(res) == 0 {
		return RelayResult{URL: relayURL, OK: false, Message: "no result from relay"}
	}
	return res[0]
}

// replyTags builds NIP-10 tags: an "e" root marker, an optional "e" reply
// marker, and a "p" tag for the replied-to author. When AuthorPubkey is set
// (replying to an external note), the p-tag carries that author and each e-tag
// carries the author hex as a 5th element (NIP-10 author hint); otherwise this
// is a self-thread and the p-tag is the owner with no 5th element. When
// ParentID equals RootID (replying directly to the root) or ParentID is empty,
// only a single root e-tag is emitted (canonical NIP-10).
func replyTags(r *NostrReply, ownerPubkeyHex string) []gonostr.Tag {
	if r == nil {
		return nil
	}
	pAuthor := r.AuthorPubkey
	if pAuthor == "" {
		pAuthor = ownerPubkeyHex // self-thread: p-tag the owner (self)
	}
	rootE := gonostr.Tag{"e", r.RootID, r.RelayHint, "root"}
	if r.AuthorPubkey != "" {
		rootE = append(rootE, r.AuthorPubkey) // NIP-10 author hint (5th element)
	}
	if r.ParentID == "" || r.ParentID == r.RootID {
		return []gonostr.Tag{rootE, {"p", pAuthor}}
	}
	replyE := gonostr.Tag{"e", r.ParentID, r.RelayHint, "reply"}
	if r.AuthorPubkey != "" {
		replyE = append(replyE, r.AuthorPubkey)
	}
	return []gonostr.Tag{rootE, replyE, {"p", pAuthor}}
}

// extractImetaURL returns the "url ..." value from a NIP-92 imeta tag, or
// empty string if the tag is missing or malformed. imeta layout per spec:
//
//	["imeta", "url <URL>", "m <mime>", "dim WxH", "x <sha256>", "blurhash ..."]
//
// Each field is "key value" with a single space separator. The "url" field
// can appear in any position after the leading "imeta" marker.
func extractImetaURL(imeta gonostr.Tag) string {
	if len(imeta) < 2 {
		return ""
	}
	for _, field := range imeta[1:] {
		if u, ok := strings.CutPrefix(field, "url "); ok {
			return strings.TrimSpace(u)
		}
	}
	return ""
}

// resolveRelays looks up the NIP-65 relay list (kind:10002) for pubkey via
// the bootstrap relay and caches the result for RelayCacheTTL.
func (p *Publisher) resolveRelays(ctx context.Context, pubkey gonostr.PubKey) ([]string, error) {
	now := time.Now()
	p.mu.RLock()
	c, ok := p.cache[pubkey]
	p.mu.RUnlock()
	if ok && c.exp.After(now) {
		return append([]string{}, c.write...), nil
	}

	rctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	relay, err := gonostr.RelayConnect(rctx, p.cfg.NIP65BootstrapRelay, gonostr.RelayOptions{})
	if err != nil {
		return nil, fmt.Errorf("bootstrap connect: %w", err)
	}
	defer relay.Close()

	filter := gonostr.Filter{
		Kinds:   []gonostr.Kind{10002},
		Authors: []gonostr.PubKey{pubkey},
		Limit:   1,
	}

	var write, read []string
	for evt := range relay.QueryEvents(filter) {
		for _, t := range evt.Tags {
			if len(t) < 2 || t[0] != "r" || t[1] == "" {
				continue
			}
			switch {
			case len(t) == 2:
				write = append(write, t[1])
				read = append(read, t[1])
			case len(t) >= 3 && t[2] == "write":
				write = append(write, t[1])
			case len(t) >= 3 && t[2] == "read":
				read = append(read, t[1])
			}
		}
		break // Limit:1 should be enforced by relay; safety net here
	}
	if len(write) == 0 && len(read) == 0 {
		return nil, errors.New("no kind:10002 event found or no usable r tags")
	}
	p.mu.Lock()
	p.cache[pubkey] = relayCacheEntry{write: write, read: read, exp: now.Add(p.cfg.RelayCacheTTL)}
	p.mu.Unlock()
	return append([]string{}, write...), nil
}

// ResolveWriteRelays returns the owner's NIP-65 (kind:10002) write relays.
func (p *Publisher) ResolveWriteRelays(ctx context.Context) ([]string, error) {
	return p.resolveRelays(ctx, p.cfg.OwnerPubkey)
}

// IsOverlayRelay reports whether the relay lives on an overlay network the
// container can't reach (Tor .onion / I2P .i2p). Those relays are skipped
// rather than dialed (the publisher has no Tor/I2P egress).
func IsOverlayRelay(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	h := strings.ToLower(u.Hostname())
	return strings.HasSuffix(h, ".onion") || strings.HasSuffix(h, ".i2p")
}

// selectPrimary partitions resolved write relays for primary-fanout mode. attempt
// is the clearnet subset that appears in primary; if none match, it falls back to
// the first clearnet resolved relay. pending is every other clearnet relay. Overlay
// relays (.onion/.i2p) are excluded from both (no egress to dial or rebroadcast).
func selectPrimary(resolved, primary []string) (attempt, pending []string) {
	isPrimary := make(map[string]bool, len(primary))
	for _, p := range primary {
		isPrimary[strings.TrimRight(p, "/")] = true
	}
	for _, u := range resolved {
		if IsOverlayRelay(u) {
			continue
		}
		if isPrimary[strings.TrimRight(u, "/")] {
			attempt = append(attempt, u)
		} else {
			pending = append(pending, u)
		}
	}
	if len(attempt) == 0 && len(pending) > 0 {
		attempt, pending = pending[:1], pending[1:] // fall back to the first clearnet relay
	}
	return attempt, pending
}

// dedup returns urls with empty strings and duplicates removed. Trailing
// slashes are normalised away before deduplication so that "wss://r.io" and
// "wss://r.io/" are treated as the same relay.
func dedup(urls []string) []string {
	seen := make(map[string]bool, len(urls))
	out := make([]string, 0, len(urls))
	for _, u := range urls {
		u = strings.TrimRight(u, "/")
		if u == "" || seen[u] {
			continue
		}
		seen[u] = true
		out = append(out, u)
	}
	return out
}
