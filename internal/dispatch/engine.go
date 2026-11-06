package dispatch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"
	"sync"
	"time"

	"github.com/geofox/publisher/internal/accounts"
	"github.com/geofox/publisher/internal/destination"
	"github.com/geofox/publisher/internal/logging"
	"github.com/geofox/publisher/internal/media"
	"github.com/geofox/publisher/internal/metrics"
	"github.com/geofox/publisher/internal/store"
	"github.com/geofox/publisher/internal/thread"
	"github.com/geofox/publisher/internal/unfurl"
)

// The engine. A post is written whole, every item planned, before anything
// is sent. Items are then claimed one at a time (store.ClaimItem), published,
// and their results recorded with the retry decision (store.RecordItemResult).
// Every item (a new post, a scheduled one, a retry, what a stopped process
// left in flight) is claimed by the background workers Run starts; the
// caller follows the outcome through the post and its events.

// RetryPolicy decides when a failed item is tried again.
type RetryPolicy struct {
	Enabled     bool // false: failed items wait for a retry by hand
	MaxAttempts int  // attempts per item, the first included
	Base, Max   time.Duration
}

// span is how long the policy keeps retrying one item at most (twice the
// sum of its waits, at least a day): a failure older than that was not
// left for automatic retries.
func (p RetryPolicy) span() time.Duration {
	n := p.MaxAttempts
	if n <= 0 {
		n = 10
	}
	var total time.Duration
	for i := 1; i < n; i++ {
		total += backoff(i, p.Base, p.Max)
	}
	return max(2*total, 24*time.Hour)
}

const (
	defaultWorkers = 4
	defaultPoll    = 2 * time.Second
	passInterval   = 30 * time.Second
)

// engine is the Dispatcher's run state. The zero value is ready.
type engine struct {
	mu       sync.Mutex
	inline   map[string]int // posts a test drives inline (inline_test.go), by drive count
	recover  sync.Once      // startup recovery
	stopping bool
	active   sync.WaitGroup // workers (and a test's inline drives)
	kickOnce sync.Once
	kickCh   chan struct{}
}

func (d *Dispatcher) now() time.Time {
	if d.Clock != nil {
		return d.Clock().UTC()
	}
	return time.Now().UTC()
}

// kick wakes an idle worker: something became due.
func (d *Dispatcher) kick() {
	d.eng.kickOnce.Do(func() { d.eng.kickCh = make(chan struct{}, 1) })
	select {
	case d.eng.kickCh <- struct{}{}:
	default:
	}
}

func (d *Dispatcher) kicks() <-chan struct{} {
	d.eng.kickOnce.Do(func() { d.eng.kickCh = make(chan struct{}, 1) })
	return d.eng.kickCh
}

func (d *Dispatcher) inlinePosts() []string {
	d.eng.mu.Lock()
	defer d.eng.mu.Unlock()
	out := make([]string, 0, len(d.eng.inline))
	for id := range d.eng.inline {
		out = append(out, id)
	}
	return out
}

func (d *Dispatcher) isInline(postID string) bool {
	d.eng.mu.Lock()
	defer d.eng.mu.Unlock()
	return d.eng.inline[postID] > 0
}

func (d *Dispatcher) isStopping() bool {
	d.eng.mu.Lock()
	defer d.eng.mu.Unlock()
	return d.eng.stopping
}

// Run recovers what a stopped process left, then claims due items with
// Workers workers (default 4) until ctx is done. Workers finish the item in
// hand; Shutdown waits for them.
func (d *Dispatcher) Run(ctx context.Context) {
	if d.Store == nil {
		slog.Error("engine: no store; nothing will be published in the background")
		return
	}
	d.Recover(ctx)
	d.passes(ctx)
	n := d.Workers
	if n <= 0 {
		n = defaultWorkers
	}
	for i := 0; i < n; i++ {
		d.eng.mu.Lock()
		if d.eng.stopping {
			d.eng.mu.Unlock()
			break
		}
		d.eng.active.Add(1)
		d.eng.mu.Unlock()
		go d.worker(ctx)
	}
	t := time.NewTicker(passInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			d.passes(ctx)
		}
	}
}

// Shutdown stops new claims and inline drives and waits, until ctx is done,
// for the items in flight. What it can't wait for stays sending and is
// recovered on the next start.
func (d *Dispatcher) Shutdown(ctx context.Context) error {
	d.eng.mu.Lock()
	d.eng.stopping = true
	d.eng.mu.Unlock()
	done := make(chan struct{})
	go func() {
		d.eng.active.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (d *Dispatcher) worker(ctx context.Context) {
	defer d.eng.active.Done()
	poll := d.PollInterval
	if poll <= 0 {
		poll = defaultPoll
	}
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		if ctx.Err() != nil || d.isStopping() {
			return
		}
		now := d.now()
		c, err := d.Store.ClaimItem(ctx, now, store.ClaimFilter{Exclude: d.inlinePosts(), MissedBefore: d.missedBefore(now)})
		if err != nil && ctx.Err() == nil {
			slog.ErrorContext(ctx, "engine: claim failed", "err", err)
		}
		if c != nil {
			// The item in hand finishes even when shutdown starts: cutting
			// the request would leave its outcome unknown. A claim that
			// raced the owner's own retry of the post is the owner's.
			d.runClaim(context.WithoutCancel(ctx), c, nil, !d.isInline(c.PostID))
			continue
		}
		timer.Reset(poll)
		select {
		case <-ctx.Done():
			return
		case <-d.kicks():
		case <-timer.C:
		}
	}
}

func (d *Dispatcher) missedBefore(now time.Time) time.Time {
	if d.ScheduleGrace <= 0 {
		return time.Time{}
	}
	return now.Add(-d.ScheduleGrace)
}

// passes are the periodic jobs around the claim loop: scheduled posts missed
// beyond the grace window, failures recorded without a retry decision, and
// the attention gauge.
func (d *Dispatcher) passes(ctx context.Context) {
	now := d.now()
	if d.ScheduleGrace > 0 {
		missed, err := d.Store.MarkOverdueMissed(ctx, now.Add(-d.ScheduleGrace))
		if err != nil {
			slog.ErrorContext(ctx, "engine: missed pass failed", "err", err)
		}
		for _, m := range missed {
			slog.WarnContext(ctx, "scheduled post missed (beyond grace)", "post_id", m.ID, "scheduled_at", m.ScheduledAt)
			d.alert(ctx, "Scheduled post missed", "post "+m.ID+" was due "+m.ScheduledAt.Format(time.RFC3339)+
				" but is beyond the grace window; not published")
		}
	}
	d.scheduleUnscheduled(ctx, now)
	if n, err := d.Store.AttentionCount(); err == nil {
		metrics.SetAttentionBacklog(float64(n))
	}
}

// scheduleUnscheduled applies the retry policy to failures recorded before
// the engine decided retries (the v4 migration's rows).
func (d *Dispatcher) scheduleUnscheduled(ctx context.Context, now time.Time) {
	if !d.Policy.Enabled {
		return
	}
	us, err := d.Store.UnscheduledFailures(ctx)
	if err != nil {
		slog.ErrorContext(ctx, "engine: unscheduled failures query failed", "err", err)
		return
	}
	stale := d.Policy.span()
	for _, u := range us {
		// A failure older than the policy's whole retry span was recorded
		// while auto-retry was off, and the owner has had it in front of
		// them since: sending it now, possibly weeks later, would surprise
		// them. It stays failed, for a retry by hand.
		if !u.LastAttempt.IsZero() && now.Sub(u.LastAttempt) > stale {
			if _, err := d.Store.MarkTargetGaveUp(u.TargetID, now); err != nil {
				slog.ErrorContext(ctx, "engine: mark gave-up failed", "post_id", u.PostID, "err", err)
			} else {
				slog.InfoContext(ctx, "engine: old failure left for a retry by hand", "post_id", u.PostID,
					"platform", u.Platform, "last_attempt", u.LastAttempt)
			}
			continue
		}
		dec := d.decide(ctx, u.Claim, destination.ErrorClass(u.ErrorClass), 0, u.Attempts, now)
		if dec.giveUp != "" {
			if set, err := d.Store.MarkTargetGaveUp(u.TargetID, now); err != nil {
				slog.ErrorContext(ctx, "engine: mark gave-up failed", "post_id", u.PostID, "err", err)
			} else if set {
				metrics.IncRetryExhausted(u.Platform)
				d.alertGaveUp(ctx, u.PostID, u.AccountID, u.Platform, dec.giveUp)
			}
			continue
		}
		next := u.LastAttempt.Add(backoff(u.Attempts, d.Policy.Base, d.Policy.Max))
		if next.Before(now) {
			next = now
		}
		if err := d.Store.ScheduleItemRetry(ctx, u.ItemID, next); err != nil {
			slog.ErrorContext(ctx, "engine: schedule retry failed", "post_id", u.PostID, "err", err)
		}
	}
}

// Recover settles what a stopped process left sending, once per process.
// Run calls it; call it first to settle before serving requests.
func (d *Dispatcher) Recover(ctx context.Context) {
	if d.Store == nil {
		return
	}
	d.eng.recover.Do(func() { d.recoverSending(ctx) })
}

// recoverSending settles items a stopped process left sending. The send may
// have published. An idempotent destination drops a repeat, so the item is
// retried; any other is left unknown for the owner to check.
func (d *Dispatcher) recoverSending(ctx context.Context) {
	items, err := d.Store.SendingItems(ctx)
	if err != nil {
		slog.ErrorContext(ctx, "engine: recovery query failed", "err", err)
		return
	}
	now := d.now()
	for _, si := range items {
		// The attempt is dated when its send started, so the key window
		// is measured from the send, not from this restart.
		at := now
		if !si.SentAt.IsZero() {
			at = si.SentAt
		}
		caps := d.Caps(ctx, si.AccountID, si.Platform)
		if caps.CommitMarks && !si.Committing {
			// The driver marks the item right before its publishing request,
			// and there is no mark: it stopped while preparing (media,
			// containers, uploads). Nothing was posted, so send it again.
			// NotSent: no attempt is counted (nor read as maybe-posted).
			res := store.ItemResult{Claim: si.Claim, Status: store.ItemFailed, At: at, NextAttemptAt: &now, NotSent: true,
				ErrorClass: string(destination.ClassTransient),
				Error:      "the publisher stopped before this was sent; sending it again"}
			if _, err := d.Store.RecordItemResult(ctx, res); err != nil {
				slog.ErrorContext(ctx, "engine: recover item failed", "post_id", si.PostID, "item_id", si.ItemID, "err", err)
				continue
			}
			pctx := logging.With(ctx, "post_id", si.PostID)
			logItemResult(pctx, &si.Claim, res)
			slog.InfoContext(pctx, "engine: re-queued an item stopped before sending", "platform", si.Platform, "ordinal", si.Ordinal)
			continue
		}
		res := store.ItemResult{Claim: si.Claim, Status: store.ItemUnknown, At: at,
			ErrorClass: string(destination.ClassUnknownOutcome),
			Error:      "the publisher stopped while this was being sent; it may have been posted"}
		mode := d.idempotency(ctx, si.Claim, caps)
		retry := mode == destination.IdempotentKey || mode == destination.IdempotentNatural
		if retry && mode == destination.IdempotentKey && caps.IdempotencyKeyTTLSecs > 0 && !si.SentAt.IsZero() &&
			now.Sub(si.SentAt) > time.Duration(caps.IdempotencyKeyTTLSecs)*time.Second-keyMargin {
			retry = false // the server has forgotten the key
		}
		if retry {
			res.NextAttemptAt = &now
		} else {
			res.GiveUp = true
		}
		if _, err := d.Store.RecordItemResult(ctx, res); err != nil {
			slog.ErrorContext(ctx, "engine: recover item failed", "post_id", si.PostID, "item_id", si.ItemID, "err", err)
			continue
		}
		pctx := logging.With(ctx, "post_id", si.PostID)
		logItemResult(pctx, &si.Claim, res)
		if retry {
			slog.InfoContext(pctx, "engine: re-queued an item left sending", "platform", si.Platform, "ordinal", si.Ordinal)
			continue
		}
		slog.WarnContext(pctx, "engine: item left sending may have been posted", "platform", si.Platform, "ordinal", si.Ordinal)
		d.alert(ctx, "Publisher: post may have been published",
			"post "+si.PostID+" was being sent to "+d.displayName(si.AccountID, si.Platform)+" when the publisher stopped. "+
				"It may have been posted: check "+d.displayName(si.AccountID, si.Platform)+", then retry it if it wasn't.")
	}
	if n, err := d.Store.ResetUnstarted(ctx); err != nil {
		slog.ErrorContext(ctx, "engine: reset unstarted failed", "err", err)
	} else if n > 0 {
		slog.InfoContext(ctx, "engine: returned claimed but unsent scheduled posts to scheduled", "n", n)
	}
}

// memMedia is media a caller already holds in memory (the request's
// uploads), so an inline drive doesn't fetch it back from Blossom.
type memMedia struct {
	post   []Img                   // by post media index
	source []Img                   // a quote's re-hosted source media, by source index
	thumbs map[string]*unfurl.Card // link cards fetched at planning, by thumbnail URL
}

// runClaim publishes a claimed item and records the result. auto marks a
// claim the engine made on its own (not a publish or retry the owner asked
// for), which is released unsent when sending could duplicate a post.
func (d *Dispatcher) runClaim(ctx context.Context, c *store.Claim, mm *memMedia, auto bool) (status string) {
	ctx = logging.With(ctx, "post_id", c.PostID)
	now := d.now()
	defer func() {
		// A bug that panics on one item must not take the process down, nor
		// (recovered as sending at the next start) do so again on every start.
		if p := recover(); p != nil {
			slog.ErrorContext(ctx, "engine: publish panicked; the target stops for the owner",
				"platform", c.Platform, "ordinal", c.Ordinal, "panic", p, "stack", string(debug.Stack()))
			res := store.ItemResult{Claim: *c, Status: store.ItemUnknown, GiveUp: true, At: d.now(),
				ErrorClass: string(destination.ClassUnknownOutcome),
				Error:      "internal error while sending; it may have been posted: check, then retry by hand"}
			if out, err := d.Store.RecordItemResult(context.WithoutCancel(ctx), res); err == nil {
				logItemResult(ctx, c, res)
				d.settled(ctx, c, out, "internal error")
			}
			status = store.ItemUnknown
		}
	}()
	if auto && c.Attempts > 0 {
		caps := d.Caps(ctx, c.AccountID, c.Platform)
		if d.keyForgotten(ctx, *c, "", caps, now) {
			d.release(ctx, c, now)
			return store.ItemUnknown
		}
	}
	var r TargetResult
	post, err := d.Store.GetPost(c.PostID)
	var tg *store.Target
	var it *store.Item
	if err == nil {
		tg, it = findItem(post, c)
	}
	if tg == nil || it == nil {
		if err == nil {
			err = errors.New("claimed item not found")
		}
		r = TargetResult{Platform: c.Platform, Status: "failed", Error: "load post: " + err.Error(),
			ErrorClass: string(destination.ClassTransient)}
	} else if why, locked := d.lockedReason(c.AccountID); locked {
		r = TargetResult{Platform: c.Platform, waiting: true, Error: why}
	} else {
		r = d.publishClaimed(ctx, post, *tg, *it, mm)
	}
	if r.waiting {
		d.wait(ctx, c, post, r.Error, now)
		return store.ItemPlanned
	}
	if c.Attempts > 0 {
		metrics.RecordRetry(c.Platform)
	}
	res := store.ItemResult{Claim: *c, Status: itemStatus(r.Status), RemoteID: r.RemoteID, RemoteURL: r.RemoteURL,
		Ref: r.RefData, Error: r.Error, ErrorClass: r.ErrorClass, LatencyMS: r.LatencyMS,
		RequestJSON: r.RequestJSON, ResponseJSON: r.ResponseJSON, Deliveries: r.Relays, At: d.now()}
	var dec decision
	if res.Status == store.ItemFailed {
		dec = d.decide(ctx, *c, destination.ErrorClass(r.ErrorClass), r.RetryAfter, c.Attempts+1, now)
		res.NextAttemptAt, res.GiveUp = dec.next, dec.giveUp != ""
	}
	out, err := d.recordResult(ctx, res)
	if err != nil {
		slog.ErrorContext(ctx, "engine: record result failed", "platform", c.Platform, "ordinal", c.Ordinal, "err", err)
		return res.Status
	}
	logItemResult(ctx, c, res)
	d.enqueueFanout(ctx, c.PostID, c.ItemID, r.Status, r.SignedEventJSON, r.PendingRelays)
	if out.PostBefore == "scheduled" {
		metrics.IncSchedulerFire()
	}
	d.settled(ctx, c, out, dec.giveUp)
	return res.Status
}

// recordResult records a send's result, retrying a few times on a database
// error: once something was published, losing its result leaves the item
// sending until a restart, which then can only call it "may have been
// posted". A lost claim is final.
func (d *Dispatcher) recordResult(ctx context.Context, res store.ItemResult) (store.Outcome, error) {
	ctx = context.WithoutCancel(ctx)
	var out store.Outcome
	var err error
	for i, wait := range []time.Duration{0, 250 * time.Millisecond, time.Second, 3 * time.Second} {
		if i > 0 {
			time.Sleep(wait)
		}
		if out, err = d.Store.RecordItemResult(ctx, res); err == nil || errors.Is(err, store.ErrClaimLost) {
			return out, err
		}
	}
	return out, err
}

// lockedReason reports whether account id is locked, and why, so its
// item waits before any media is fetched for it.
func (d *Dispatcher) lockedReason(id string) (string, bool) {
	if d.Registry == nil {
		return "", false
	}
	e, ok := d.Registry.Get(id)
	if !ok || e.Status != accounts.Locked {
		return "", false
	}
	return errLocked{reason: e.Reason}.Error(), true
}

// lockedWaitMax caps how long an item of a locked account waits between
// looks at its account.
const lockedWaitMax = 15 * time.Minute

// wait puts back an item whose account is locked: nothing was sent, so no
// attempt is counted and nothing gives up, and it is due again after a wait
// that grows with how long the post has waited (one minute at first, at
// most lockedWaitMax), whether or not automatic retries are on. It stays
// planned, so its post stays sending and no failure is reported, and an
// item.waiting event gives the reason. If the post was canceled meanwhile,
// the item ends canceled instead (the store decides, in the transaction).
func (d *Dispatcher) wait(ctx context.Context, c *store.Claim, post *store.Post, why string, now time.Time) {
	waited := time.Duration(0)
	if post != nil {
		due := post.CreatedAt
		if post.ScheduledAt != nil {
			due = *post.ScheduledAt
		}
		waited = now.Sub(due)
	}
	next := now.Add(min(max(waited, time.Minute), lockedWaitMax))
	res := store.ItemResult{Claim: *c, Status: store.ItemPlanned, NotSent: true, NextAttemptAt: &next, At: now,
		ErrorClass: string(destination.ClassTransient), Error: why + "; it is sent once the account opens"}
	out, err := d.recordResult(ctx, res)
	if err != nil {
		slog.ErrorContext(ctx, "engine: record wait failed", "platform", c.Platform, "account", c.AccountID, "err", err)
		return
	}
	if out.ItemStatus != store.ItemPlanned {
		slog.InfoContext(ctx, "engine: item canceled while its account was locked", "platform", c.Platform,
			"account", c.AccountID, "ordinal", c.Ordinal)
		d.settled(ctx, c, out, "")
		return
	}
	slog.WarnContext(ctx, "engine: item waits for its account", "platform", c.Platform, "account", c.AccountID,
		"ordinal", c.Ordinal, "next_attempt_at", next, "reason", why)
}

// settled follows up a recorded result: the post's end-of-run alert and
// feed notice when this result ended its run, and an alert for a target
// that gave up unless the end-of-run alert already names it.
func (d *Dispatcher) settled(ctx context.Context, c *store.Claim, out store.Outcome, why string) {
	alerted := false
	if out.PostBefore == "sending" && out.PostAfter != "sending" {
		alerted = d.finished(ctx, c.PostID)
	}
	if out.GaveUp {
		metrics.IncRetryExhausted(c.Platform)
		if !alerted {
			d.alertGaveUp(ctx, c.PostID, c.AccountID, c.Platform, why)
		}
	}
}

// idempotency is how the destination dedupes a repeat of the claimed item.
// A Nostr repost is signed anew on each attempt (it has no prepared event),
// so a repeat of one is a second repost.
func (d *Dispatcher) idempotency(ctx context.Context, c store.Claim, caps destination.Capabilities) destination.IdempotencyMode {
	if caps.Idempotent != destination.IdempotentNatural || c.Ordinal != 0 {
		return caps.Idempotent
	}
	post, err := d.Store.GetPost(c.PostID)
	if err != nil {
		return destination.IdempotentNone
	}
	for _, tg := range post.Targets {
		if tg.ID == c.TargetID {
			if _, repost, _, _ := interactionAction(post, tg); repost != nil {
				return destination.IdempotentNone
			}
		}
	}
	return caps.Idempotent
}

// release gives a claim back unsent because an earlier attempt may have
// published and the destination no longer remembers its idempotency key:
// the target stops for the owner to check.
func (d *Dispatcher) release(ctx context.Context, c *store.Claim, now time.Time) {
	res := store.ItemResult{Claim: *c, Status: store.ItemUnknown, NotSent: true, GiveUp: true, At: now,
		ErrorClass: string(destination.ClassUnknownOutcome),
		Error: "not retried: an earlier attempt may have been posted and " + d.displayName(c.AccountID, c.Platform) +
			" no longer remembers its idempotency key; check, then retry by hand"}
	out, err := d.Store.RecordItemResult(ctx, res)
	if err != nil {
		slog.ErrorContext(ctx, "engine: release claim failed", "err", err)
		return
	}
	logItemResult(ctx, c, res)
	d.settled(ctx, c, out, "idempotency key expired")
}

// finished runs when a post stops sending: the feed is told, and a first
// run that didn't fully succeed raises an alert naming the targets that
// gave up (after that, a target's give-up is alerted on its own). It
// reports whether it alerted.
func (d *Dispatcher) finished(ctx context.Context, postID string) bool {
	post, err := d.Store.GetPost(postID)
	if err != nil {
		return false
	}
	d.notify(ctx, post)
	for _, t := range post.Targets {
		if t.AttemptCount > 1 {
			return false
		}
	}
	return d.alertFailure(ctx, post)
}

func findItem(post *store.Post, c *store.Claim) (*store.Target, *store.Item) {
	for i := range post.Targets {
		if post.Targets[i].ID != c.TargetID {
			continue
		}
		for j := range post.Targets[i].Items {
			if post.Targets[i].Items[j].ID == c.ItemID {
				return &post.Targets[i], &post.Targets[i].Items[j]
			}
		}
	}
	return nil, nil
}

func itemStatus(s string) string {
	switch s {
	case "success":
		return store.ItemPublished
	case "partial":
		return store.ItemPartial
	}
	return store.ItemFailed
}

// publishClaimed publishes one item of a target: its text and media, the
// target's options, threaded under the items before it (or, for the first
// item of an interaction's source target, as the reply, quote or repost).
func (d *Dispatcher) publishClaimed(ctx context.Context, post *store.Post, tg store.Target, it store.Item, mm *memMedia) TargetResult {
	// The driver marks the item just before its publishing request, so a
	// restart can tell it never reached the network (recoverSending).
	var marked bool
	ctx = destination.WithCommitHook(ctx, func() error {
		if marked {
			return nil
		}
		if err := d.Store.MarkCommitting(context.WithoutCancel(ctx), it.ID, d.now()); err != nil {
			return fmt.Errorf("record send start: %w", err)
		}
		marked = true
		return nil
	})
	var ov Overrides
	if tg.FieldsJSON != "" {
		if err := json.Unmarshal([]byte(tg.FieldsJSON), &ov); err != nil {
			slog.WarnContext(ctx, "engine: bad fields_json, using zero overrides", "target_id", tg.ID, "err", err)
		}
	}
	card := ov.LinkCard
	ov.LinkCard = nil
	if card != nil && card.Segment == it.Ordinal {
		if mm != nil && len(card.ThumbData) == 0 {
			if c := mm.thumbs[card.ThumbURL]; c != nil {
				card.ThumbData, card.ThumbMime = c.ThumbData, c.ThumbMime
			}
		}
		d.refetchThumb(ctx, card)
		ov.LinkCard = card
	}
	head, repost, isAction, ok := interactionAction(post, tg)
	if isAction && !ok {
		return TargetResult{Platform: tg.Platform, Status: "failed", Error: errNoInteractionRef,
			ErrorClass: string(destination.ClassInvalid)}
	}
	if repost != nil && it.Ordinal == 0 {
		return d.repost(ctx, tg.AccountID, tg.Platform, *repost)
	}
	if legacyPlacement(post, tg) {
		caps := d.Caps(ctx, tg.AccountID, tg.Platform)
		it.Media = legacyMedia(thread.PlanMedia(len(post.Media), len(tg.Items), caps.Media.Images.MaxCount), it.Ordinal)
	}
	x := item{text: it.Text, ov: ov, imgs: d.itemImages(ctx, post, it, mm), postID: post.ID, ordinal: it.Ordinal,
		idemKey: it.IdemKey, prepared: it.Ref, itemID: it.ID}
	if it.Ordinal == 0 {
		x.head = head
	} else {
		x.thread = &destination.ThreadRef{Root: itemRef(tg.Items[0]), Parent: itemRef(prevItem(tg, it))}
		// A part confirmed live by hand without its reference can't be
		// replied to; sending would post this part detached.
		if x.thread.Root.ID == "" || x.thread.Parent.ID == "" {
			return TargetResult{Platform: tg.Platform, Status: "failed", ErrorClass: string(destination.ClassInvalid),
				Error: "an earlier part of this thread has no reference to reply to (it was marked posted by hand without one); post the rest yourself"}
		}
	}
	return d.publishItem(ctx, tg.AccountID, tg.Platform, x)
}

// legacyPlacement reports whether a thread was planned before media
// placement was recorded: the post has media and no item names any. Its
// media is then placed as it was then, front-filled at the account's cap.
func legacyPlacement(post *store.Post, tg store.Target) bool {
	if len(post.Media) == 0 || len(tg.Items) < 2 {
		return false
	}
	for _, it := range tg.Items {
		if it.Media != nil || it.Attach != nil {
			return false
		}
	}
	return true
}

// legacyMedia is item ordinal's media under a front-fill plan (counts per
// item, in order).
func legacyMedia(plan []int, ordinal int) []int {
	start := 0
	for j := 0; j < ordinal && j < len(plan); j++ {
		start += plan[j]
	}
	if ordinal >= len(plan) {
		return nil
	}
	out := make([]int, plan[ordinal])
	for i := range out {
		out[i] = start + i
	}
	return out
}

func prevItem(tg store.Target, it store.Item) store.Item {
	for i := range tg.Items {
		if tg.Items[i].Ordinal == it.Ordinal-1 {
			return tg.Items[i]
		}
	}
	return store.Item{}
}

func itemRef(it store.Item) destination.ItemRef {
	return destination.ItemRef{ID: it.RemoteID, URL: it.RemoteURL, Data: it.Ref}
}

// refetchThumb downloads a persisted link card's thumbnail (stored cards
// have no bytes); best-effort.
func (d *Dispatcher) refetchThumb(ctx context.Context, card *unfurl.Card) {
	if card.ThumbURL == "" || d.Unfurler == nil || len(card.ThumbData) > 0 {
		return
	}
	tctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	data, mime, err := d.Unfurler.Thumb(tctx, card.ThumbURL)
	cancel()
	if err != nil {
		slog.WarnContext(ctx, "link card thumb re-fetch failed; card posts without thumb", "url", card.ThumbURL, "err", err)
		return
	}
	card.ThumbData, card.ThumbMime = data, mime
}

// itemImages assembles an item's media: the post's media it names, then its
// own (a quote reproduction's source media), from memory when the caller
// holds the bytes, else fetched from Blossom.
func (d *Dispatcher) itemImages(ctx context.Context, post *store.Post, it store.Item, mm *memMedia) []Img {
	var out []Img
	for _, i := range it.Media {
		if mm != nil && i >= 0 && i < len(mm.post) {
			out = append(out, mm.post[i])
			continue
		}
		if i >= 0 && i < len(post.Media) {
			out = append(out, d.fetchImg(ctx, post.Media[i]))
		}
	}
	for _, m := range it.Attach {
		if mm != nil && m.Ordinal >= 0 && m.Ordinal < len(mm.source) {
			out = append(out, mm.source[m.Ordinal])
			continue
		}
		out = append(out, d.fetchImg(ctx, m))
	}
	return out
}

// fetchImg builds an Img from a media record, fetching its bytes when a
// byte-upload destination could use them. Videos over media.FetchCap ride
// as metadata (URL destinations need nothing more; byte ones gate them
// out). A failed fetch leaves Bytes nil, and the media gate fails the item
// as transient.
func (d *Dispatcher) fetchImg(ctx context.Context, m store.Media) Img {
	img := Img{Mime: m.Mime, Alt: m.Alt, BlossomURL: m.BlossomURL, Dim: m.Dim, DurationSecs: m.DurationSecs,
		SizeBytes: m.SizeBytes, SHA256: m.SHA256, Blurhash: m.Blurhash, PosterURL: m.PosterURL}
	if d.Fetcher != nil && (!img.IsVideo() || (m.SizeBytes > 0 && m.SizeBytes <= media.FetchCap)) {
		if body, fmime, err := d.Fetcher.Fetch(ctx, m.BlossomURL); err == nil {
			img.Bytes = body
			if img.Mime == "" {
				img.Mime = fmime
			}
		}
	}
	return img
}

// decision is the retry policy's verdict on a failed attempt.
type decision struct {
	next   *time.Time
	giveUp string // why nothing automatic will try again ("" = it will, or the policy is off)
}

// decide applies the retry policy to an item's failure. attempts counts
// this one. Without auto-retry nothing is scheduled and nothing given up.
func (d *Dispatcher) decide(ctx context.Context, c store.Claim, cls destination.ErrorClass, retryAfter time.Duration, attempts int, now time.Time) decision {
	if !d.Policy.Enabled {
		return decision{}
	}
	caps := d.Caps(ctx, c.AccountID, c.Platform)
	if !cls.Retryable(d.idempotency(ctx, c, caps)) {
		return decision{giveUp: string(cls)}
	}
	if d.keyForgotten(ctx, c, cls, caps, now) {
		return decision{giveUp: "idempotency key expired"}
	}
	if d.Policy.MaxAttempts > 0 && attempts >= d.Policy.MaxAttempts {
		return decision{giveUp: "attempts exhausted"}
	}
	wait := backoff(attempts, d.Policy.Base, d.Policy.Max)
	if cls == destination.ClassRateLimited && retryAfter > wait {
		// A server's wait is honored up to a day: past that it is more
		// likely a bug than a plan, and the owner can still retry.
		wait = min(retryAfter, maxRetryAfter)
	}
	next := now.Add(wait)
	// A keyed destination forgets its key after a window measured from the
	// first uncertain attempt; a retry after that is released unsent. Pull
	// the retry in to the end of the window rather than lose it.
	if caps.Idempotent == destination.IdempotentKey && caps.IdempotencyKeyTTLSecs > 0 {
		if first, err := d.Store.FirstUncertainAttempt(ctx, c.ItemID); err == nil && first != nil {
			last := first.Add(time.Duration(caps.IdempotencyKeyTTLSecs)*time.Second - keyMargin - time.Minute)
			if next.After(last) && last.After(now) {
				next = last
			}
		}
	}
	return decision{next: &next}
}

// keyForgotten reports whether an attempt at the item may have published
// and the destination has since forgotten its idempotency key, so a retry
// could post twice.
func (d *Dispatcher) keyForgotten(ctx context.Context, c store.Claim, cls destination.ErrorClass, caps destination.Capabilities, now time.Time) bool {
	if caps.Idempotent != destination.IdempotentKey || caps.IdempotencyKeyTTLSecs <= 0 {
		return false
	}
	window := time.Duration(caps.IdempotencyKeyTTLSecs)*time.Second - keyMargin
	first, err := d.Store.FirstUncertainAttempt(ctx, c.ItemID)
	if err != nil {
		return true // can't tell: don't risk a duplicate
	}
	if first == nil && cls == destination.ClassUnknownOutcome {
		return false // this attempt is the first in doubt
	}
	return first != nil && now.Sub(*first) > window
}

func (d *Dispatcher) alert(ctx context.Context, summary, body string) {
	if d.Alerter == nil {
		return
	}
	if err := d.Alerter.Alert(ctx, summary, body); err != nil {
		slog.ErrorContext(ctx, "alert failed", "err", err)
	}
}

// alertGaveUp tells the owner the engine stopped retrying a target.
func (d *Dispatcher) alertGaveUp(ctx context.Context, postID, accountID, platform, why string) {
	body := "post " + postID + " will not be retried automatically on " + d.displayName(accountID, platform) + " (" + why + "). "
	if why == "attempts exhausted" {
		body += "Manual retry is still available."
		d.alert(ctx, "Publisher: auto-retry exhausted", body)
		return
	}
	body += "The failure would repeat (auth, invalid, rejected) or the post may already be live (unknown_outcome). " +
		"Check the target, fix the cause, then retry manually."
	d.alert(ctx, "Publisher: delivery needs attention", body)
}

// logItemResult writes one line per send attempt (post_id comes from ctx), so
// a log alert can watch for failures without reading the database. Endpoint
// counts are there for destinations that deliver to several (Nostr relays).
func logItemResult(ctx context.Context, c *store.Claim, res store.ItemResult) {
	attrs := []any{"platform", c.Platform, "account", c.AccountID, "ordinal", c.Ordinal, "attempt", c.Attempts + 1,
		"status", res.Status, "latency_ms", res.LatencyMS}
	if res.RemoteID != "" {
		attrs = append(attrs, "remote_id", res.RemoteID)
	}
	if len(res.Deliveries) > 0 {
		ok := 0
		for _, rs := range res.Deliveries {
			if rs.Status == "ok" {
				ok++
			}
		}
		attrs = append(attrs, "endpoints_ok", ok, "endpoints_failed", len(res.Deliveries)-ok)
	}
	if res.Status == store.ItemFailed || res.Status == store.ItemUnknown {
		// Scrubbed as the store scrubs it: an upstream body may echo a token.
		msg := store.Scrub(res.Error)
		if r := []rune(msg); len(r) > 300 {
			msg = string(r[:300]) + "…"
		}
		attrs = append(attrs, "error_class", res.ErrorClass, "error", msg, "give_up", res.GiveUp)
		slog.WarnContext(ctx, "engine: item not sent", attrs...)
		return
	}
	slog.InfoContext(ctx, "engine: item sent", attrs...)
}
