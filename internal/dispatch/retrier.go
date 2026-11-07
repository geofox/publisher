package dispatch

import (
	"context"
	"errors"
	"log/slog"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/geofox/publisher/internal/logging"
	"github.com/geofox/publisher/internal/store"
)

// backoff returns the delay before the next retry: base * 2^(attempt-1),
// clamped to max. attempt is the number of attempts already made
// (post_targets.attempt_count). No jitter — a single-operator deployment
// has no thundering-herd to spread.
func backoff(attempt int, base, max time.Duration) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	d := base
	for i := 1; i < attempt; i++ {
		next := d * 2
		if next <= 0 || next >= max { // next <= 0 catches int64 overflow
			return max
		}
		d = next
	}
	if d > max {
		return max
	}
	return d
}

// Retrier re-sends failed relay deliveries of live Nostr notes with
// exponential backoff, and gives up after a cap. (Failed items themselves
// are retried by the engine; see RetryPolicy.) A single in-process actor;
// the running guard prevents overlapping ticks.
type Retrier struct {
	disp        *Dispatcher
	notifier    Notifier
	enabled     bool
	maxAttempts int
	base, max   time.Duration
	tick        time.Duration
	now         func() time.Time
	running     atomic.Bool
	noAccount   lastingWarn // relay retries that wait for an account
	// Live, when set, is read at each tick: whether retries are on and
	// how many (the owner's settings), in place of enabled and maxAttempts.
	Live func() (enabled bool, maxAttempts int)
}

func (r *Retrier) maxTries() int { _, n := r.settings(); return n }

// settings are the retrier's switch and tries now.
func (r *Retrier) settings() (bool, int) {
	if r.Live != nil {
		return r.Live()
	}
	return r.enabled, r.maxAttempts
}

func NewRetrier(d *Dispatcher, n Notifier, enabled bool, maxAttempts int, base, max, tick time.Duration) *Retrier {
	return &Retrier{disp: d, notifier: n, enabled: enabled, maxAttempts: maxAttempts,
		base: base, max: max, tick: tick, now: time.Now}
}

// Start ticks until ctx is done. Without Live, it returns at once when
// retries are off; with it, a tick while they are off does nothing.
func (r *Retrier) Start(ctx context.Context) {
	if r.Live == nil && !r.enabled {
		slog.Info("retrier disabled (AUTO_RETRY_ENABLED=false)")
		return
	}
	t := time.NewTicker(r.tick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if on, _ := r.settings(); on {
				r.runDue(ctx)
			}
		}
	}
}

// runDue processes one pass. The running guard drops a tick if the previous
// pass is still in flight (a slow network retry must not let ticks pile up).
func (r *Retrier) runDue(ctx context.Context) {
	if !r.running.CompareAndSwap(false, true) {
		return
	}
	defer r.running.Store(false)

	ids, err := r.disp.Store.PostsNeedingRetry()
	if err != nil {
		slog.Error("retrier: candidate query failed", "err", err)
		return
	}
	waiting := 0
	for _, id := range ids {
		if ctx.Err() != nil {
			return // shutting down
		}
		waiting += r.processPost(ctx, id)
	}
	switch now := r.now(); {
	case waiting > 0 && r.noAccount.on(now):
		slog.Warn("retrier: no account can deliver to relays now; relay retries wait", "relays", waiting)
	case waiting == 0 && r.noAccount.off(now, 0):
		slog.Info("retrier: no relay retry waits for an account any more")
	}
}

// processPost retries the post's due relays. It returns how many wait
// because no account can deliver to relays now (nothing is counted
// against them).
func (r *Retrier) processPost(ctx context.Context, id string) (waiting int) {
	ctx = logging.With(ctx, "post_id", id)
	post, err := r.disp.Store.GetPost(id)
	if err != nil {
		slog.ErrorContext(ctx, "retrier: load failed", "err", err)
		return 0
	}
	now := r.now()
	var exhausted []string
	for _, t := range post.Targets {
		// Only single-post partials with endpoint rows (Nostr relays) use
		// endpoint-level retry; threads and failed items are the engine's.
		if len(t.Relays) == 0 || t.Status != "partial" || len(t.Items) > 1 {
			continue
		}
		for _, rl := range t.Relays {
			if rl.Status != "failed" || rl.GaveUpAt != nil {
				continue
			}
			// retry_count counts retries only (the original publish is attempt 0), so a relay gets up to maxAttempts rebroadcasts before giving up.
			if _, most := r.settings(); rl.RetryCount >= most {
				if set, err := r.disp.Store.MarkRelayGaveUp(t.ID, rl.URL, now); err != nil {
					slog.ErrorContext(ctx, "retrier: mark relay gave-up failed", "relay", rl.URL, "err", err)
				} else if set {
					exhausted = append(exhausted, rl.URL)
				}
				continue
			}
			if !r.relayDue(rl, now) || ctx.Err() != nil {
				continue
			}
			if _, err := r.disp.RetryTargetRelay(ctx, id, t.ID, rl.URL); errors.Is(err, ErrNoRedeliverer) {
				waiting++
			} else if err != nil {
				slog.ErrorContext(ctx, "retrier: relay retry failed", "relay", rl.URL, "err", err)
			} else {
				slog.InfoContext(ctx, "retrier: rebroadcast to relay", "relay", rl.URL)
			}
		}
	}
	if len(exhausted) > 0 {
		r.alertGaveUp(ctx, id, exhausted)
	}
	return waiting
}

// keyMargin is how long before the server forgets an idempotency key the
// engine stops relying on it (clock skew, a slow request).
const keyMargin = 5 * time.Minute

// maxRetryAfter caps a server's requested wait before an automatic retry.
const maxRetryAfter = 24 * time.Hour

// relayDue checks whether the relay's next retry is due. Uses AttemptedAt +
// backoff(RetryCount+1) so the first retry is after base*2^(RetryCount) wait.
func (r *Retrier) relayDue(rl store.RelayState, now time.Time) bool {
	if rl.AttemptedAt.IsZero() {
		return true
	}
	return !now.Before(rl.AttemptedAt.Add(backoff(rl.RetryCount+1, r.base, r.max)))
}

// alertGaveUp fires one operational alert summarizing newly given-up relays.
func (r *Retrier) alertGaveUp(ctx context.Context, postID string, relays []string) {
	if r.notifier == nil {
		return
	}
	body := "post " + postID + " gave up on " + strings.Join(relays, ", ") + " relays after " +
		strconv.Itoa(r.maxTries()) + " attempts; manual retry still available"
	if err := r.notifier.Alert(ctx, "Publisher: auto-retry exhausted", body); err != nil {
		slog.ErrorContext(ctx, "retrier: give-up alert failed", "err", err)
	}
}
