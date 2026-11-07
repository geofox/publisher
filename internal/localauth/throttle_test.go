package localauth

import (
	"context"
	"net/netip"
	"sync"
	"testing"
	"time"
)

type memFailures struct {
	mu sync.Mutex
	m  map[string][]time.Time
}

func (f *memFailures) RecordLoginFailure(_ context.Context, addr string, at time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.m == nil {
		f.m = map[string][]time.Time{}
	}
	f.m[addr] = append(f.m[addr], at)
	return nil
}

func (f *memFailures) LoginFailures(_ context.Context, addr string, since time.Time) ([]time.Time, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []time.Time
	for a, ts := range f.m {
		if addr != "" && a != addr {
			continue
		}
		for _, t := range ts {
			if t.After(since) {
				out = append(out, t)
			}
		}
	}
	// oldest first
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j].Before(out[j-1]); j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out, nil
}

func (f *memFailures) ClearLoginFailures(_ context.Context, addr string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.m, addr)
	return nil
}

func TestAddrKey(t *testing.T) {
	for in, want := range map[string]string{
		"192.0.2.7":            "192.0.2.7",
		"::ffff:192.0.2.7":     "192.0.2.7",
		"2001:db8:1:2:3:4:5:6": "2001:db8:1:2::/64",
		"2001:db8:1:2:ffff::1": "2001:db8:1:2::/64",
		"2001:db8:1:3::1":      "2001:db8:1:3::/64",
		"fe80::1%eth0":         "fe80::/64",
	} {
		if got := AddrKey(netip.MustParseAddr(in)); got != want {
			t.Errorf("%s: %s, want %s", in, got, want)
		}
	}
	if AddrKey(netip.Addr{}) != "unknown" {
		t.Error("zero address")
	}
}

func TestThrottlePerAddress(t *testing.T) {
	now := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	th := &Throttle{Store: &memFailures{}, Now: func() time.Time { return now }}
	ctx := context.Background()
	a := netip.MustParseAddr("2001:db8::1")
	sameNet := netip.MustParseAddr("2001:db8::ffff")
	other := netip.MustParseAddr("192.0.2.1")
	for i := range 4 {
		if w, _, _ := th.Wait(ctx, a, false); w != 0 {
			t.Fatalf("attempt %d waits %v", i+1, w)
		}
		_ = th.Failed(ctx, a)
	}
	if w, _, _ := th.Wait(ctx, a, false); w != 0 {
		t.Fatalf("5th attempt waits %v", w)
	}
	_ = th.Failed(ctx, sameNet) // the 5th failure, from the same /64
	for i, want := range []time.Duration{time.Minute, 2 * time.Minute, 4 * time.Minute, 8 * time.Minute, 15 * time.Minute, 15 * time.Minute} {
		w, _, _ := th.Wait(ctx, a, false)
		if w < want || w > want+time.Second {
			t.Fatalf("after %d failures: wait %v, want %v", 5+i, w, want)
		}
		if w2, _, _ := th.Wait(ctx, other, false); w2 != 0 {
			t.Fatalf("another address waits %v", w2)
		}
		now = now.Add(w)
		if w, _, _ := th.Wait(ctx, a, false); w != 0 {
			t.Fatalf("still waiting %v after the delay", w)
		}
		_ = th.Failed(ctx, a)
	}
	// A pause ends the run.
	now = now.Add(ThrottleWindow + 15*time.Minute + time.Second)
	if w, _, _ := th.Wait(ctx, a, false); w != 0 {
		t.Fatalf("after a pause: %v", w)
	}
	for range 5 {
		_ = th.Failed(ctx, a)
	}
	if w, _, _ := th.Wait(ctx, a, false); w < time.Minute {
		t.Fatalf("a new run: %v", w)
	}
	// A success forgets the address's failures.
	_ = th.Succeeded(ctx, a)
	if w, _, _ := th.Wait(ctx, a, false); w != 0 {
		t.Fatalf("after success: %v", w)
	}
}

// From many addresses: a short delay for everyone, never a block.
func TestThrottlePerOwner(t *testing.T) {
	now := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	th := &Throttle{Store: &memFailures{}, Now: func() time.Time { return now }}
	ctx := context.Background()
	for i := range 20 {
		_ = th.Failed(ctx, netip.AddrFrom4([4]byte{198, 51, 100, byte(i)}))
	}
	owner := netip.MustParseAddr("203.0.113.5")
	if w, _, _ := th.Wait(ctx, owner, false); w < time.Second || w > 2*time.Second {
		t.Fatalf("after 20: %v", w)
	}
	for i := range 20 {
		_ = th.Failed(ctx, netip.AddrFrom4([4]byte{198, 51, 101, byte(i)}))
	}
	w, _, _ := th.Wait(ctx, owner, false)
	if w < ownerMaxDelay || w > ownerMaxDelay+time.Second {
		t.Fatalf("after 40: %v, want at most %v", w, ownerMaxDelay)
	}
	now = now.Add(w)
	if w, _, _ := th.Wait(ctx, owner, false); w != 0 {
		t.Fatalf("the owner is blocked: %v", w)
	}
	// A pause longer than the window ends the run.
	now = now.Add(ThrottleWindow + time.Second)
	if w, _, _ := th.Wait(ctx, netip.MustParseAddr("198.51.100.1"), false); w != 0 {
		t.Fatalf("after the window: %v", w)
	}
}

// An attacker rotating addresses keeps the owner-wide delay going for
// good, but a browser the owner signed in with before isn't held by it:
// only its own address's limit applies.
func TestThrottleKnownBrowserSkipsTheOwnerWideDelay(t *testing.T) {
	now := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	th := &Throttle{Store: &memFailures{}, Now: func() time.Time { return now }}
	ctx := context.Background()
	for i := range 30 {
		_ = th.Failed(ctx, netip.AddrFrom4([4]byte{198, 51, 100, byte(i)}))
	}
	owner := netip.MustParseAddr("203.0.113.5")
	w, everyone, _ := th.Wait(ctx, owner, false)
	if w == 0 || !everyone {
		t.Fatalf("an unknown browser: wait %v, owner-wide %v", w, everyone)
	}
	if w, everyone, _ := th.Wait(ctx, owner, true); w != 0 || everyone {
		t.Fatalf("a known browser: wait %v, owner-wide %v", w, everyone)
	}
	// Its own address's failures still count.
	for range 5 {
		_ = th.Failed(ctx, owner)
	}
	if w, everyone, _ := th.Wait(ctx, owner, true); w < time.Minute || everyone {
		t.Fatalf("a known browser at a failing address: wait %v, owner-wide %v", w, everyone)
	}
}

func TestLimiter(t *testing.T) {
	now := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	l := &Limiter{N: 3, Window: time.Minute, Now: func() time.Time { return now }}
	for i := range 3 {
		if w := l.Take("a"); w != 0 {
			t.Fatalf("event %d waits %v", i+1, w)
		}
		now = now.Add(10 * time.Second)
	}
	if w := l.Take("a"); w != 31*time.Second {
		t.Fatalf("4th: %v, want 31s (the first leaves the window at 60s)", w)
	}
	if w := l.Take("b"); w != 0 {
		t.Fatalf("another key: %v", w)
	}
	now = now.Add(31 * time.Second)
	if w := l.Wait("a"); w != 0 {
		t.Fatalf("after the window: %v", w)
	}
	l.Note("a")
	if w := l.Wait("a"); w == 0 {
		t.Fatal("Note doesn't count")
	}
}
