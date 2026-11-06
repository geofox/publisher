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
		if w, _ := th.Wait(ctx, a); w != 0 {
			t.Fatalf("attempt %d waits %v", i+1, w)
		}
		_ = th.Failed(ctx, a)
	}
	if w, _ := th.Wait(ctx, a); w != 0 {
		t.Fatalf("5th attempt waits %v", w)
	}
	_ = th.Failed(ctx, sameNet) // the 5th failure, from the same /64
	for i, want := range []time.Duration{time.Minute, 2 * time.Minute, 4 * time.Minute, 8 * time.Minute, 15 * time.Minute, 15 * time.Minute} {
		w, _ := th.Wait(ctx, a)
		if w < want || w > want+time.Second {
			t.Fatalf("after %d failures: wait %v, want %v", 5+i, w, want)
		}
		if w2, _ := th.Wait(ctx, other); w2 != 0 {
			t.Fatalf("another address waits %v", w2)
		}
		now = now.Add(w)
		if w, _ := th.Wait(ctx, a); w != 0 {
			t.Fatalf("still waiting %v after the delay", w)
		}
		_ = th.Failed(ctx, a)
	}
	// A pause ends the run.
	now = now.Add(ThrottleWindow + 15*time.Minute + time.Second)
	if w, _ := th.Wait(ctx, a); w != 0 {
		t.Fatalf("after a pause: %v", w)
	}
	for range 5 {
		_ = th.Failed(ctx, a)
	}
	if w, _ := th.Wait(ctx, a); w < time.Minute {
		t.Fatalf("a new run: %v", w)
	}
	// A success forgets the address's failures.
	_ = th.Succeeded(ctx, a)
	if w, _ := th.Wait(ctx, a); w != 0 {
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
	if w, _ := th.Wait(ctx, owner); w < time.Second || w > 2*time.Second {
		t.Fatalf("after 20: %v", w)
	}
	for i := range 20 {
		_ = th.Failed(ctx, netip.AddrFrom4([4]byte{198, 51, 101, byte(i)}))
	}
	w, _ := th.Wait(ctx, owner)
	if w < ownerMaxDelay || w > ownerMaxDelay+time.Second {
		t.Fatalf("after 40: %v, want at most %v", w, ownerMaxDelay)
	}
	now = now.Add(w)
	if w, _ := th.Wait(ctx, owner); w != 0 {
		t.Fatalf("the owner is blocked: %v", w)
	}
	// A pause longer than the window ends the run.
	now = now.Add(ThrottleWindow + time.Second)
	if w, _ := th.Wait(ctx, netip.MustParseAddr("198.51.100.1")); w != 0 {
		t.Fatalf("after the window: %v", w)
	}
}
