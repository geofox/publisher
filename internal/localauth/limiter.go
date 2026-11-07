package localauth

import (
	"sync"
	"time"
)

// Limiter allows each key (an AddrKey) N events in a sliding Window. It
// is safe for concurrent use; the zero value with N and Window set works.
type Limiter struct {
	N      int
	Window time.Duration
	Now    func() time.Time // nil: time.Now

	mu sync.Mutex
	m  map[string][]time.Time
}

// maxTrackedKeys: past this many keys, the ones idle for a window are
// forgotten before another is added.
const maxTrackedKeys = 4096

func (l *Limiter) now() time.Time {
	if l.Now != nil {
		return l.Now()
	}
	return time.Now()
}

// recent is key's events still in the window (l.mu held).
func (l *Limiter) recent(key string, now time.Time) []time.Time {
	ts := l.m[key]
	kept := ts[:0]
	for _, t := range ts {
		if now.Sub(t) < l.Window {
			kept = append(kept, t)
		}
	}
	if len(kept) == 0 {
		delete(l.m, key)
		return nil
	}
	l.m[key] = kept
	return kept
}

// wait is how long key must wait for another event (l.mu held).
func (l *Limiter) wait(key string, now time.Time) time.Duration {
	ts := l.recent(key, now)
	if len(ts) < l.N {
		return 0
	}
	return ts[len(ts)-l.N].Add(l.Window).Sub(now).Truncate(time.Second) + time.Second
}

// Wait is how long key must wait for another event (0: now).
func (l *Limiter) Wait(key string) time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.wait(key, l.now())
}

// Note records an event for key.
func (l *Limiter) Note(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.note(key, l.now())
}

func (l *Limiter) note(key string, now time.Time) {
	if l.m == nil {
		l.m = map[string][]time.Time{}
	}
	if _, ok := l.m[key]; !ok && len(l.m) >= maxTrackedKeys {
		for k := range l.m {
			l.recent(k, now)
		}
	}
	l.m[key] = append(l.m[key], now)
}

// Take records an event for key when one is allowed now, and otherwise
// says how long to wait.
func (l *Limiter) Take(key string) time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	if w := l.wait(key, now); w > 0 {
		return w
	}
	l.note(key, now)
	return 0
}
