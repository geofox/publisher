package dispatch

import (
	"context"
	"errors"

	"github.com/geofox/publisher/internal/store"
)

// Retry re-runs the failed (or missed) targets of an archived post. If platforms
// is non-empty, only those platforms are retried. Successful targets are skipped.
func (d *Dispatcher) Retry(ctx context.Context, postID string, platforms []string) (*store.Post, error) {
	if d.Store == nil {
		return nil, errors.New("retry requires a store")
	}
	post, err := d.Store.GetPost(postID)
	if err != nil {
		return nil, err
	}
	want := map[string]bool{}
	for _, p := range platforms {
		want[p] = true
	}
	if err := d.dispatchTargets(ctx, post, func(t store.Target) bool {
		// 'missed' targets (scheduled posts past the grace window) are re-postable
		// through the same retry path as failed ones. Partial threaded targets
		// (len(Segments) > 1) can also be resumed; single-post partials (e.g. nostr
		// with some relays down) use RetryRelay to avoid duplicating the note.
		retryable := t.Status == "failed" || t.Status == "missed" || (t.Status == "partial" && len(t.Segments) > 1)
		return retryable && (len(platforms) == 0 || want[t.Platform])
	}); err != nil {
		return nil, err
	}
	post, err = d.Store.GetPost(postID)
	if err != nil {
		return nil, err
	}
	d.notify(ctx, post)
	return post, nil
}

// nostrStatusFromRelays derives a Nostr target status from its relay states,
// ignoring skipped (overlay) relays: success = all attempted ok, failed = none
// ok, partial = some ok.
func nostrStatusFromRelays(relays []store.RelayState) string {
	attempted, ok := 0, 0
	for _, r := range relays {
		if r.Status == "skipped" {
			continue
		}
		attempted++
		if r.Status == "ok" {
			ok++
		}
	}
	switch {
	case ok == 0:
		return "failed"
	case ok < attempted:
		return "partial"
	default:
		return "success"
	}
}

// RetryRelay rebroadcasts an archived nostr event to a single failed relay
// (same signed event — no re-mine, no duplicate note) and updates that relay's
// status. Platforms other than nostr have no relay rows and are rejected.
func (d *Dispatcher) RetryRelay(ctx context.Context, postID, relayURL string) (*store.Post, error) {
	if d.Store == nil {
		return nil, errors.New("retry requires a store")
	}
	post, err := d.Store.GetPost(postID)
	if err != nil {
		return nil, err
	}
	var target *store.Target
	for i := range post.Targets {
		if post.Targets[i].Platform == "nostr" {
			target = &post.Targets[i]
			break
		}
	}
	if target == nil {
		return nil, ErrBadRelayRetry
	}
	var relay *store.RelayState
	for i := range target.Relays {
		if target.Relays[i].URL == relayURL {
			relay = &target.Relays[i]
			break
		}
	}
	// Only failed relays are retryable: skipped relays are unreachable, and an
	// ok relay already has the event (the UI only offers retry on failures).
	if relay == nil || relay.Status != "failed" {
		return nil, ErrBadRelayRetry
	}
	if target.SignedEventJSON == "" {
		return nil, errors.New("no stored event to rebroadcast")
	}
	if d.Nostr == nil {
		return nil, errors.New("nostr not configured")
	}
	ok, msg := d.Nostr.RebroadcastToRelay(ctx, target.SignedEventJSON, relayURL)
	status := "failed"
	if ok {
		status = "ok"
	}
	if err := d.Store.UpdateRelayStatus(target.ID, relayURL, status, msg); err != nil {
		return nil, err
	}
	// No notify() here: RetryRelay only rebroadcasts an already-signed Nostr
	// event to one relay; it creates no new post/URL. The narrow case where a
	// partial→success relay flip newly makes a post feed-eligible is
	// intentionally not pinged (the feed still surfaces it on next fetch).
	return d.Store.GetPost(postID)
}

// ErrBadRelayRetry signals an unknown/ineligible relay-retry request (maps to 400).
var ErrBadRelayRetry = errors.New("unknown or non-retryable relay")
