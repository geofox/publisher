package dispatch

import (
	"context"
	"errors"

	"github.com/geofox/publisher/internal/destination"
	"github.com/geofox/publisher/internal/store"
)

// RetryTargetRelay re-delivers an archived item of one target of the post
// (0: the first target with that endpoint) to one failed endpoint (a Nostr
// relay: the same signed event, so no re-mine and no duplicate note) and
// updates that endpoint's status. Only destinations that deliver to
// several endpoints (destination.Redeliverer) have endpoint rows. The
// event is already signed, so another account of the type delivers it
// when the target's own can't be used.
func (d *Dispatcher) RetryTargetRelay(ctx context.Context, postID string, targetID int64, relayURL string) (*store.Post, error) {
	if d.Store == nil {
		return nil, errors.New("retry requires a store")
	}
	post, err := d.Store.GetPost(postID)
	if err != nil {
		return nil, err
	}
	var target *store.Target
	var relay *store.RelayState
	for i := range post.Targets {
		if targetID != 0 && post.Targets[i].ID != targetID {
			continue
		}
		for j := range post.Targets[i].Relays {
			if post.Targets[i].Relays[j].URL == relayURL {
				target, relay = &post.Targets[i], &post.Targets[i].Relays[j]
				break
			}
		}
		if target != nil {
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
	rd, release, err := d.redeliverer(target.AccountID)
	if err != nil {
		return nil, err
	}
	defer release()
	dl := rd.Redeliver(ctx, destination.ItemRef{ID: target.RemoteID, URL: target.RemoteURL,
		Data: []byte(target.SignedEventJSON)}, relayURL)
	if dl.Status != "ok" && ctx.Err() != nil {
		return nil, ctx.Err() // stopped or canceled, not the relay's failure: no retry is spent
	}
	if err := d.Store.UpdateRelayStatus(target.ID, relayURL, dl.Status, dl.Message); err != nil {
		return nil, err
	}
	// No notify() here: RetryRelay only re-sends an already-signed event to
	// one relay; it creates no new post/URL. The narrow case where a
	// partial→success relay flip newly makes a post feed-eligible is
	// intentionally not pinged (the feed still surfaces it on next fetch).
	return d.Store.GetPost(postID)
}

// ErrBadRelayRetry signals an unknown/ineligible relay-retry request (maps to 400).
var ErrBadRelayRetry = errors.New("unknown or non-retryable relay")
