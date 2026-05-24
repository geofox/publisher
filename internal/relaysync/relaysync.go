// Package relaysync reconciles the owner's Nostr events between the home relay
// and a set of target relays (REQ-diff: fetch both sides, diff by id, publish
// the deltas). Scan is read-only; Apply is the only write path.
package relaysync

import (
	"context"
	"errors"
	"fmt"
	"strings"

	gonostr "fiatjaf.com/nostr"
	pubnostr "github.com/geofox/publisher/internal/nostr"
)

type Target struct {
	URL   string `json:"url"`
	Group string `json:"group"` // "nip65" | "secondary"
}

type RelayDiff struct {
	URL            string `json:"url"`
	Group          string `json:"group"`
	MissingAtHome  int    `json:"missing_at_home"`
	MissingAtRelay int    `json:"missing_at_relay"`
	Status         string `json:"status"` // ok | unreachable | auth | error
	Message        string `json:"message,omitempty"`
}

type ApplyResult struct {
	URL       string `json:"url"`
	Direction string `json:"direction"` // pull | push
	Published int    `json:"published"`
	Failed    int    `json:"failed"`
	Status    string `json:"status"` // ok | unreachable | auth | error
	Message   string `json:"message,omitempty"`
}

// Sentinels let the concrete relayIO classify failures; the engine maps them to
// a per-relay status without string matching.
var (
	ErrRelayUnreachable = errors.New("relay unreachable")
	ErrRelayAuth        = errors.New("relay requires auth")
)

// relayIO abstracts per-relay reads/writes so Scan/Apply are testable.
type relayIO interface {
	Fetch(ctx context.Context, relayURL string, pubkey gonostr.PubKey) (map[gonostr.ID]gonostr.Event, error)
	// detail summarizes per-event failure reasons (e.g. "restricted: … ×3") for
	// the caller to surface; empty when nothing failed.
	Publish(ctx context.Context, relayURL string, events []gonostr.Event) (published, failed int, detail string, err error)
}

type Sync struct {
	io    relayIO
	home  string
	owner gonostr.PubKey
}

func New(io relayIO, home string, owner gonostr.PubKey) *Sync {
	return &Sync{io: io, home: home, owner: owner}
}

// replKey returns the replaceable identity of an event — (kind,pubkey) for
// replaceable kinds (0, 3, 10000–19999) and (kind,pubkey,d) for addressable
// kinds (30000–39999) — and ok=false for regular events. Two events sharing a
// replKey are versions of the same logical event; relays keep only the newest.
func replKey(ev gonostr.Event) (string, bool) {
	k := int(ev.Kind)
	switch {
	case k == 0 || k == 3 || (k >= 10000 && k < 20000):
		return fmt.Sprintf("%d:%s", k, ev.PubKey.Hex()), true
	case k >= 30000 && k < 40000:
		return fmt.Sprintf("%d:%s:%s", k, ev.PubKey.Hex(), dTag(ev)), true
	default:
		return "", false
	}
}

func dTag(ev gonostr.Event) string {
	for _, t := range ev.Tags {
		if len(t) >= 2 && t[0] == "d" {
			return t[1]
		}
	}
	return ""
}

// isEphemeral reports whether a kind is ephemeral (20000–29999). Per NIP-01
// relays are not meant to store these, so they're excluded from sync entirely —
// a stale ephemeral on home is rejected everywhere ("invalid: ephemeral event
// expired") and would otherwise fail on every push.
func isEphemeral(kind gonostr.Kind) bool {
	k := int(kind)
	return k >= 20000 && k < 30000
}

// diff returns events present in target but not home (missingAtHome) and events
// present in home but not target (missingAtRelay). Ephemeral kinds are excluded
// from both directions (relays don't store them). missingAtHome is
// replaceable-aware: a target replaceable/addressable event is NOT a pull
// candidate when home already holds a same-or-newer version of it (home would
// just drop the superseded copy — a permanent phantom otherwise). Push
// (missingAtRelay) stays a plain id diff: pushing home's newer version
// supersedes a relay's stale copy.
func diff(home, target map[gonostr.ID]gonostr.Event) (missingAtHome, missingAtRelay []gonostr.Event) {
	homeRepl := map[string]gonostr.Timestamp{} // newest created_at per replaceable identity on home
	for _, ev := range home {
		if key, ok := replKey(ev); ok {
			if ts, seen := homeRepl[key]; !seen || ev.CreatedAt > ts {
				homeRepl[key] = ev.CreatedAt
			}
		}
	}
	for id, ev := range target {
		if _, ok := home[id]; ok {
			continue
		}
		if isEphemeral(ev.Kind) {
			continue // ephemeral events aren't stored/propagated
		}
		if key, ok := replKey(ev); ok {
			if ts, seen := homeRepl[key]; seen && ts >= ev.CreatedAt {
				continue // home already has a same-or-newer version — skip the stale one
			}
		}
		missingAtHome = append(missingAtHome, ev)
	}
	for id, ev := range home {
		if isEphemeral(ev.Kind) {
			continue // ephemeral events aren't stored/propagated
		}
		if _, ok := target[id]; !ok {
			missingAtRelay = append(missingAtRelay, ev)
		}
	}
	return
}

func classify(err error) (status, msg string) {
	switch {
	case err == nil:
		return "ok", ""
	case errors.Is(err, ErrRelayAuth):
		return "auth", err.Error()
	case errors.Is(err, ErrRelayUnreachable):
		return "unreachable", err.Error()
	default:
		return "error", err.Error()
	}
}

func (s *Sync) Scan(ctx context.Context, targets []Target) []RelayDiff {
	out := make([]RelayDiff, 0, len(targets))
	home, homeErr := s.io.Fetch(ctx, s.home, s.owner)
	for _, t := range targets {
		d := RelayDiff{URL: t.URL, Group: t.Group}
		if homeErr != nil {
			d.Status, d.Message = "error", "home fetch failed: "+homeErr.Error()
			out = append(out, d)
			continue
		}
		tev, err := s.io.Fetch(ctx, t.URL, s.owner)
		if err != nil {
			d.Status, d.Message = classify(err)
			out = append(out, d)
			continue
		}
		mh, mr := diff(home, tev)
		d.MissingAtHome, d.MissingAtRelay, d.Status = len(mh), len(mr), "ok"
		out = append(out, d)
	}
	return out
}

func (s *Sync) Apply(ctx context.Context, targets []Target, direction string) []ApplyResult {
	out := make([]ApplyResult, 0, len(targets))
	// Direction is a caller-supplied constant known before any I/O — validate it
	// up front rather than paying a home fetch on a bad request.
	if direction != "pull" && direction != "push" {
		for _, t := range targets {
			out = append(out, ApplyResult{URL: t.URL, Direction: direction, Status: "error", Message: "unknown direction"})
		}
		return out
	}
	home, homeErr := s.io.Fetch(ctx, s.home, s.owner)
	for _, t := range targets {
		r := ApplyResult{URL: t.URL, Direction: direction}
		if homeErr != nil {
			r.Status, r.Message = "error", "home fetch failed: "+homeErr.Error()
			out = append(out, r)
			continue
		}
		tev, err := s.io.Fetch(ctx, t.URL, s.owner)
		if err != nil {
			r.Status, r.Message = classify(err)
			out = append(out, r)
			continue
		}
		mh, mr := diff(home, tev)
		toPublish, dest := mr, t.URL
		if direction == "pull" {
			toPublish, dest = mh, s.home
		}
		if len(toPublish) == 0 { // already in sync — no connection needed
			r.Status = "ok"
			out = append(out, r)
			continue
		}
		pub, fail, detail, perr := s.io.Publish(ctx, dest, toPublish)
		r.Published, r.Failed = pub, fail
		if detail == "" {
			detail = "some events failed to publish"
		}
		switch {
		case perr != nil: // relay-level failure (unreachable / auth)
			r.Status, r.Message = classify(perr)
		case fail > 0 && pub > 0: // some through, some not
			r.Status, r.Message = "partial", detail
		case fail > 0: // nothing through
			r.Status, r.Message = "error", detail
		default:
			r.Status = "ok"
		}
		out = append(out, r)
	}
	return out
}

// ResolveTargets builds the de-duped target set: NIP-65 write relays (group
// "nip65") ∪ secondary list (group "secondary"), excluding the home relay and
// overlay (.onion/.i2p) relays. NIP-65 wins the group label on overlap.
func ResolveTargets(nip65, secondary []string, home string) []Target {
	norm := func(u string) string { return strings.TrimRight(u, "/") }
	h := norm(home)
	seen := map[string]bool{}
	out := make([]Target, 0)
	add := func(u, group string) {
		n := norm(u)
		if n == "" || n == h || seen[n] || pubnostr.IsOverlayRelay(n) {
			return
		}
		seen[n] = true
		out = append(out, Target{URL: u, Group: group})
	}
	for _, u := range nip65 {
		add(u, "nip65")
	}
	for _, u := range secondary {
		add(u, "secondary")
	}
	return out
}
