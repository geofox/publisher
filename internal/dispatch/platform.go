package dispatch

import (
	"context"
	"errors"
	"fmt"
	"time"

	gonostr "fiatjaf.com/nostr"
	"github.com/geofox/publisher/internal/metrics"
)

// runPlatform executes one platform and returns a normalized TargetResult.
func (d *Dispatcher) runPlatform(ctx context.Context, plat, text string, ov Overrides, imgs []Img, imetas []gonostr.Tag, replyTo *ReplyRef) TargetResult {
	start := time.Now()
	var r TargetResult
	var err error
	switch plat {
	case "nostr":
		if d.Nostr != nil {
			r, err = d.Nostr.PublishText(ctx, text, ov.POW, imetas, replyTo)
		} else {
			err = errors.New("nostr not configured")
		}
	case "mastodon":
		if d.Mastodon != nil {
			r, err = d.Mastodon.PostText(ctx, text, ov, imgs, replyTo)
		} else {
			err = errors.New("mastodon not configured")
		}
	case "bluesky":
		if d.Bluesky != nil {
			r, err = d.Bluesky.PostBsky(ctx, text, ov, imgs, replyTo)
		} else {
			err = errors.New("bluesky not configured")
		}
	case "threads":
		if d.Threads != nil {
			r, err = d.Threads.PostThreads(ctx, text, ov, imgs, replyTo)
		} else {
			err = errors.New("threads not configured")
		}
	default:
		err = errors.New("unsupported platform")
	}
	if r.Platform == "" {
		r.Platform = plat
	}
	// Normalize: an adapter that returns an error (or a zero TargetResult)
	// must still produce a "failed" target with a message — never an
	// empty status that silently aggregates wrong.
	if r.Status == "" {
		r.Status = "failed"
	}
	if r.Status == "failed" && r.Error == "" && err != nil {
		r.Error = err.Error()
	}
	if r.LatencyMS == 0 {
		r.LatencyMS = int(time.Since(start).Milliseconds())
	}
	metrics.RecordPublish(r.Platform, r.Status, time.Since(start))
	return r
}

// runAction executes one native interaction (repost/quote) on one platform and
// returns a normalized TargetResult, mirroring runPlatform's normalization.
func (d *Dispatcher) runAction(ctx context.Context, action, plat, text string, ov Overrides, imgs []Img, imetas []gonostr.Tag, ref InteractRef) TargetResult {
	start := time.Now()
	var r TargetResult
	var err error
	switch {
	case action == actionRepost && plat == "bluesky":
		if d.Bluesky != nil {
			r, err = d.Bluesky.RepostBsky(ctx, ref.URI, ref.CID)
		} else {
			err = errors.New("bluesky not configured")
		}
	case action == actionQuote && plat == "bluesky":
		if d.Bluesky != nil {
			r, err = d.Bluesky.QuoteBsky(ctx, text, ov, imgs, ref.URI, ref.CID)
		} else {
			err = errors.New("bluesky not configured")
		}
	case action == actionRepost && plat == "mastodon":
		if d.Mastodon != nil {
			r, err = d.Mastodon.Reblog(ctx, ref.LocalID)
		} else {
			err = errors.New("mastodon not configured")
		}
	case action == actionQuote && plat == "mastodon":
		if d.Mastodon != nil {
			r, err = d.Mastodon.QuoteStatus(ctx, text, ref.LocalID, imgs)
		} else {
			err = errors.New("mastodon not configured")
		}
	case action == actionRepost && plat == "nostr":
		if d.Nostr != nil {
			r, err = d.Nostr.Repost(ctx, ref.EventID, ref.Author, ref.Kind, relayHint(ref.RelayHints))
		} else {
			err = errors.New("nostr not configured")
		}
	case action == actionQuote && plat == "nostr":
		if d.Nostr != nil {
			r, err = d.Nostr.Quote(ctx, text, ref.EventID, ref.Author, relayHint(ref.RelayHints), imetas)
		} else {
			err = errors.New("nostr not configured")
		}
	default:
		err = fmt.Errorf("unsupported action %q for %q", action, plat)
	}
	r.Platform = plat
	r.LatencyMS = int(time.Since(start).Milliseconds())
	if err != nil {
		r.Status, r.Error = "failed", err.Error()
	} else if r.Status == "" {
		r.Status = "failed"
		if r.Error == "" {
			r.Error = "adapter returned empty status"
		}
	}
	metrics.RecordPublish(r.Platform, r.Status, time.Since(start))
	return r
}

// relayHint returns the first relay hint, or "" when none are present.
func relayHint(hints []string) string {
	if len(hints) > 0 {
		return hints[0]
	}
	return ""
}
