package accounts

import (
	"context"
	"time"

	"github.com/geofox/publisher/internal/destination"
	"github.com/geofox/publisher/internal/metrics"
)

const (
	refreshTimeout = 30 * time.Second
	// A failed refresh is tried again after an hour, doubling to 12 hours,
	// so a refused token isn't sent to the network every minute.
	refreshRetryFirst = time.Hour
	refreshRetryMax   = 12 * time.Hour
)

// refreshState is an account's refresh bookkeeping (guarded by
// Registry.mu).
type refreshState struct {
	failures     int
	retryAt      time.Time
	lastFailure  time.Time // last refresh-failure alert
	lastExpiring time.Time // last "expires soon" alert
}

// RunRefresh renews credentials before they expire: every minute (±10 %),
// one account at a time, each holding its lease, until ctx ends.
func (r *Registry) RunRefresh(ctx context.Context) {
	for {
		r.RefreshDue(ctx)
		select {
		case <-ctx.Done():
			return
		case <-r.bg.Done():
			return
		case <-time.After(jitter(loopEvery)):
		}
	}
}

// RefreshDue runs one pass: refreshes what is due, alerts on failures and
// on credentials close to expiring, and reports the soonest expiry of each
// type as a gauge.
func (r *Registry) RefreshDue(ctx context.Context) {
	soonest := map[string]time.Time{}
	for _, id := range r.ids() {
		if ctx.Err() != nil {
			return
		}
		r.refreshOne(ctx, id, soonest)
	}
	now := r.now()
	for typ, exp := range soonest {
		metrics.SetTokenExpiry(typ, exp.Sub(now).Seconds())
	}
}

// refreshOne renews one account's credentials when they are due. A
// turned-off account is renewed too (and its expiry reported): turning an
// account off stops posting, and credentials left to expire (a Threads
// token) can't be renewed afterwards. An account that isn't open, or
// whose credentials the network refused, is left alone.
func (r *Registry) refreshOne(ctx context.Context, id string, soonest map[string]time.Time) {
	en, ok := r.Get(id)
	if !ok || en.Status != Connected && en.Status != Disabled {
		return
	}
	l, release := r.leaseAny(id)
	if l == nil {
		return
	}
	defer release()
	now := r.now()
	refreshed := false
	if rf, ok := l.acct.(destination.Refresher); ok {
		r.mu.RLock()
		var retryAt time.Time
		if e, ok := r.entries[id]; ok {
			retryAt = e.refresh.retryAt
		}
		r.mu.RUnlock()
		if next := rf.NextRefresh(now); !next.IsZero() && !now.Before(next) && !now.Before(retryAt) {
			refreshed = r.refresh(ctx, id, en, rf, l)
		}
	}
	ex, ok := l.acct.(destination.Expirer)
	if !ok {
		return
	}
	exp := ex.CredentialsExpireAt()
	if exp.IsZero() {
		return
	}
	if cur, seen := soonest[en.Row.Type]; !seen || exp.Before(cur) {
		soonest[en.Row.Type] = exp
	}
	if refreshed || exp.Sub(now) > expiryAlertWindow {
		return
	}
	r.mu.Lock()
	e := r.entries[id]
	send := e != nil && now.Sub(e.refresh.lastExpiring) >= alertEvery
	if send {
		e.refresh.lastExpiring = now
	}
	r.mu.Unlock()
	if send {
		r.alert(ctx, en.Row.Label+": credentials expire soon",
			"The "+en.Row.Type+" account's credentials expire "+exp.UTC().Format(time.RFC3339)+" and renewing them is not succeeding.")
	}
}

// refresh renews one account's credentials; it reports success.
func (r *Registry) refresh(ctx context.Context, id string, en Entry, rf destination.Refresher, l *lease) bool {
	cctx, cancel := context.WithTimeout(ctx, refreshTimeout)
	err := rf.Refresh(cctx)
	cancel()
	now := r.now()
	r.mu.Lock()
	e, ok := r.entries[id]
	if !ok || e.live != l {
		r.mu.Unlock()
		return err == nil
	}
	if err == nil {
		e.refresh.failures, e.refresh.retryAt = 0, time.Time{}
		r.mu.Unlock()
		r.log.Info("account credentials renewed", "account", id, "type", en.Row.Type)
		return true
	}
	e.refresh.failures++
	wait := refreshRetryFirst << min(e.refresh.failures-1, 4)
	e.refresh.retryAt = now.Add(min(wait, refreshRetryMax))
	send := now.Sub(e.refresh.lastFailure) >= alertEvery
	if send {
		e.refresh.lastFailure = now
	}
	failures := e.refresh.failures
	r.mu.Unlock()
	msg := describe(err)
	r.log.Error("account credentials could not be renewed", "account", id, "type", en.Row.Type, "err", msg, "failures", failures)
	if send {
		r.alert(ctx, en.Row.Label+": renewing credentials failed", "The "+en.Row.Type+" account's credentials could not be renewed: "+msg)
	}
	return false
}
