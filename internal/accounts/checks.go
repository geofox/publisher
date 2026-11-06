package accounts

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"net/url"
	"time"

	"github.com/geofox/publisher/internal/destination"
	"github.com/geofox/publisher/internal/health"
)

const (
	// reportGap: at most one check per account per this long, however many
	// sends report an auth failure.
	reportGap = 10 * time.Minute
	// checkEvery: a connected account is checked daily, one that needs
	// reconnecting every 15 minutes (so fixing it on the network side is
	// noticed without a restart).
	checkEvery        = 24 * time.Hour
	checkEveryRefused = 15 * time.Minute
	checkTimeout      = 30 * time.Second
	expiryAlertWindow = 7 * 24 * time.Hour
	alertEvery        = 24 * time.Hour
	loopEvery         = time.Minute
)

// ReportAuthFailure tells the registry that the network answered a send
// with an auth error. That alone never parks the account (a reply the
// account isn't allowed to make is an auth error too): it schedules an
// authenticated check, at most one per account per 10 minutes, and only a
// check that sees the credentials refused sets needs_reconnect.
func (r *Registry) ReportAuthFailure(id string) {
	now := r.now()
	r.mu.Lock()
	e, ok := r.entries[id]
	if !ok || (!e.lastReport.IsZero() && now.Sub(e.lastReport) < reportGap) {
		r.mu.Unlock()
		return
	}
	e.lastReport = now
	r.mu.Unlock()
	r.background(func(ctx context.Context) { r.CheckNow(ctx, id) })
}

// CheckNow runs an authenticated check of an open account and applies it:
// AuthOK false sets needs_reconnect, true clears it, anything else (an
// outage, a rate limit) changes nothing. ok is false when the account
// isn't open or can't check itself.
func (r *Registry) CheckNow(ctx context.Context, id string) (h destination.Health, ok bool) {
	l, release := r.leaseAny(id)
	if l == nil {
		return h, false
	}
	defer release()
	c, isChecker := l.acct.(destination.Checker)
	if !isChecker {
		return h, false
	}
	cctx, cancel := context.WithTimeout(ctx, checkTimeout)
	h = c.Check(cctx, destination.CheckRequest{Authenticated: true})
	cancel()
	r.applyHealth(ctx, id, l, h)
	return h, true
}

// Probe runs an unauthenticated check of an open account: whether its
// network answers, with no credential sent (the readiness probe). It never
// changes the account's status. ok is false when the account isn't open,
// the owner turned it off, or it can't check itself.
func (r *Registry) Probe(ctx context.Context, id string) (h destination.Health, ok bool) {
	r.mu.RLock()
	e, found := r.entries[id]
	enabled := found && e.row.Enabled
	r.mu.RUnlock()
	if !enabled {
		return h, false
	}
	l, release := r.leaseAny(id)
	if l == nil {
		return h, false
	}
	defer release()
	c, isChecker := l.acct.(destination.Checker)
	if !isChecker {
		return h, false
	}
	return c.Check(ctx, destination.CheckRequest{}), true
}

// Probeable lists the owner's accounts Probe can check: turned on and open
// (connected, or open but refused by its network), in registry order.
func (r *Registry) Probeable(owner string) []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var out []string
	for _, id := range r.order {
		e := r.entries[id]
		if e.row.OwnerID != owner || !e.row.Enabled || e.live == nil {
			continue
		}
		if _, ok := e.live.acct.(destination.Checker); ok {
			out = append(out, id)
		}
	}
	return out
}

// probeTimeout bounds one readiness probe.
const probeTimeout = 5 * time.Second

// Readiness is the external readiness checks for the owner's accounts:
// one per account Probe can check, named account:<id>, never critical. A
// failure is reported by class only ("unreachable", "timeout"); the
// network's message goes to the log.
func (r *Registry) Readiness(owner string) []health.Check {
	var out []health.Check
	for _, id := range r.Probeable(owner) {
		out = append(out, health.Check{Name: "account:" + id, Timeout: probeTimeout, Run: func(ctx context.Context) error {
			h, ok := r.Probe(ctx, id)
			switch {
			case !ok || h.OK:
				// Turned off or closed since the list was made, or fine.
				return nil
			case ctx.Err() != nil:
				return health.Fail("timeout", errors.New(h.Message))
			case h.Reachable:
				return health.Fail("not ok", errors.New(h.Message))
			default:
				return health.Fail("unreachable", errors.New(h.Message))
			}
		}})
	}
	return out
}

// applyHealth records a check's outcome on the account it was made with
// (a check that raced a Reopen says nothing about the new credentials).
func (r *Registry) applyHealth(ctx context.Context, id string, l *lease, h destination.Health) {
	now := r.now()
	r.mu.Lock()
	e, ok := r.entries[id]
	if !ok || e.live != l {
		r.mu.Unlock()
		return
	}
	before, _ := e.status()
	e.health, e.checkedAt = &h, now
	if h.AuthOK != nil {
		e.refused = !*h.AuthOK
		e.refusedReason = ""
		if e.refused {
			e.refusedReason = h.Message
		}
	}
	e.nextCheck = now.Add(checkEvery)
	if e.refused {
		e.nextCheck = now.Add(checkEveryRefused)
	}
	after, reason := e.status()
	snap := e.snapshot()
	var alert string
	_, expirer := l.acct.(destination.Expirer)
	if !expirer && h.ExpiresAt != nil && h.ExpiresAt.Sub(now) <= expiryAlertWindow && now.Sub(e.lastHealthAlert) >= alertEvery {
		e.lastHealthAlert = now
		alert = "expire " + h.ExpiresAt.UTC().Format(time.RFC3339)
	}
	r.mu.Unlock()
	if alert != "" {
		r.alert(ctx, snap.Row.Label+": credentials expire soon", "The "+snap.Row.Type+" account's credentials "+alert+"; connect it again before then.")
	}
	if before != after {
		r.log.Warn("account status changed", "account", id, "type", snap.Row.Type, "from", before, "to", after, "reason", reason)
		r.emit(Change{ID: id, Kind: ChangeStatus, Entry: snap})
	}
}

// RunChecks checks every open account when its check is due (daily, every
// 15 minutes while it needs reconnecting), one at a time, until ctx ends.
func (r *Registry) RunChecks(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-r.bg.Done():
			return
		case <-time.After(jitter(loopEvery)):
		}
		r.checkDue(ctx)
	}
}

func (r *Registry) checkDue(ctx context.Context) {
	now := r.now()
	for _, id := range r.ids() {
		r.mu.RLock()
		e, ok := r.entries[id]
		due := ok && e.live != nil && e.row.Enabled && !now.Before(e.nextCheck)
		r.mu.RUnlock()
		if !due {
			continue
		}
		if ctx.Err() != nil {
			return
		}
		if _, checked := r.CheckNow(ctx, id); !checked {
			r.mu.Lock()
			if e, ok := r.entries[id]; ok {
				e.nextCheck = now.Add(checkEvery)
			}
			r.mu.Unlock()
		}
	}
}

// jitter is d ± 10 %, so loops of several servers don't line up.
func jitter(d time.Duration) time.Duration {
	return d - d/10 + time.Duration(rand.Int64N(int64(d/5)+1))
}

func (r *Registry) alert(ctx context.Context, summary, body string) {
	if r.opt.Alerter == nil {
		return
	}
	if err := r.opt.Alerter.Alert(ctx, summary, body); err != nil {
		r.log.Error("alert webhook failed", "err", err)
	}
}

// describe is an error as an alert or log may show it: a failure to reach
// a server is described, never quoted (its URL can carry a credential).
func describe(err error) string {
	var ue *url.Error
	if errors.As(err, &ue) {
		return destination.NetProblem(err)
	}
	var he *destination.HTTPError
	if errors.As(err, &he) {
		return fmt.Sprintf("%s: the server answered %d", he.Op, he.StatusCode)
	}
	return err.Error()
}
