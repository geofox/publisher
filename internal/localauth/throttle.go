package localauth

import (
	"context"
	"net/netip"
	"time"
)

// Throttling of password attempts (a passkey is never throttled by
// them). Failures form a run while each comes within 30 minutes of the one
// before (twice the longest delay, so trying again as soon as allowed
// keeps the run going). Per client address (an IPv6 address by its /64):
// after a run of 5, each further attempt waits a doubling delay from the
// last failure (1, 2, 4, 8, then 15 minutes, for as long as the run goes
// on). For the owner, from any address: after a run of 20, a short
// doubling delay (at most 30 seconds). Someone with many addresses could
// keep that delay going for good, so it doesn't apply to a browser the
// owner signed in with before (a known browser, see DeviceToken): only
// its address's own limit does.
const (
	ThrottleWindow    = 30 * time.Minute
	addrFreeFailures  = 5
	addrMaxDelay      = 15 * time.Minute
	ownerFreeFailures = 20
	ownerMaxDelay     = 30 * time.Second
)

// FailureStore keeps failed password attempts (store.Store).
type FailureStore interface {
	RecordLoginFailure(ctx context.Context, addr string, at time.Time) error
	LoginFailures(ctx context.Context, addr string, since time.Time) ([]time.Time, error)
	ClearLoginFailures(ctx context.Context, addr string) error
}

// Throttle decides when a password may be tried.
type Throttle struct {
	Store FailureStore
	Now   func() time.Time // nil: time.Now
}

func (t *Throttle) now() time.Time {
	if t.Now != nil {
		return t.Now()
	}
	return time.Now()
}

// AddrKey is how failures from an address are grouped: an IPv4 address
// alone, an IPv6 address by its /64 (one host usually holds a whole /64).
func AddrKey(a netip.Addr) string {
	a = a.Unmap()
	switch {
	case !a.IsValid():
		return "unknown"
	case a.Is6():
		p, _ := a.Prefix(64)
		return p.String()
	}
	return a.String()
}

// Wait is how long addr must wait before trying a password (0: now), and
// whether that is the owner-wide delay (failures from everywhere) rather
// than addr's own. A known browser skips the owner-wide delay.
func (t *Throttle) Wait(ctx context.Context, addr netip.Addr, known bool) (time.Duration, bool, error) {
	now := t.now()
	since := now.Add(-24 * time.Hour)
	mine, err := t.Store.LoginFailures(ctx, AddrKey(addr), since)
	if err != nil {
		return 0, false, err
	}
	wait := delayAfter(mine, addrFreeFailures, time.Minute, addrMaxDelay, now)
	if known {
		return wait, false, nil
	}
	all, err := t.Store.LoginFailures(ctx, "", since)
	if err != nil {
		return 0, false, err
	}
	if w := delayAfter(all, ownerFreeFailures, time.Second, ownerMaxDelay, now); w > wait {
		return w, true, nil
	}
	return wait, false, nil
}

// delayAfter: with a run of n failures (oldest first) and free of them
// allowed, the time left of base·2^(n-free) (at most max) after the last.
func delayAfter(fails []time.Time, free int, base, max time.Duration, now time.Time) time.Duration {
	n := runLength(fails)
	if n < free {
		return 0
	}
	d := max
	if shift := n - free; shift < 30 {
		if dd := base << shift; dd < max {
			d = dd
		}
	}
	left := fails[len(fails)-1].Add(d).Sub(now)
	if left < 0 {
		return 0
	}
	// Whole seconds, rounded up, as Retry-After says them.
	return left.Truncate(time.Second) + time.Second
}

// runLength counts the failures, from the newest back, that each came
// within ThrottleWindow of the next.
func runLength(fails []time.Time) int {
	if len(fails) == 0 {
		return 0
	}
	n := 1
	for i := len(fails) - 1; i > 0 && fails[i].Sub(fails[i-1]) <= ThrottleWindow; i-- {
		n++
	}
	return n
}

// Failed records a wrong password from addr.
func (t *Throttle) Failed(ctx context.Context, addr netip.Addr) error {
	return t.Store.RecordLoginFailure(ctx, AddrKey(addr), t.now())
}

// Succeeded forgets addr's failures once it signed in.
func (t *Throttle) Succeeded(ctx context.Context, addr netip.Addr) error {
	return t.Store.ClearLoginFailures(ctx, AddrKey(addr))
}
