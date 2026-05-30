// Package identity aggregates the operator's own cross-platform profile (handle,
// display name, avatar) so the composer can show real account data instead of
// placeholders. Each platform is fetched concurrently with a per-call timeout and
// the result is cached; any nil profiler or failing fetch is simply omitted, so
// the UI degrades gracefully to its built-in fallbacks.
package identity

import (
	"context"
	"strings"
	"sync"
	"time"
)

// Account is one platform's resolved profile.
type Account struct {
	Platform    string `json:"platform"`
	Handle      string `json:"handle,omitempty"`
	DisplayName string `json:"display_name,omitempty"`
	Avatar      string `json:"avatar,omitempty"`
}

// Identity is the operator's aggregated cross-platform identity. Name/Avatar are
// the best single choice for the composer header; Accounts holds the per-platform
// handles for the target rows.
type Identity struct {
	Name     string             `json:"name,omitempty"`
	Avatar   string             `json:"avatar,omitempty"`
	Monogram string             `json:"monogram,omitempty"`
	Accounts map[string]Account `json:"accounts"`
}

// Profiler fetches one platform's authenticated profile. Implemented by the
// bluesky / mastodon / threads clients.
type Profiler interface {
	Profile(ctx context.Context) (handle, displayName, avatar string, err error)
}

// NostrProfiler returns the operator's npub plus best-effort name/avatar.
type NostrProfiler interface {
	OwnerProfile(ctx context.Context) (npub, name, avatar string, err error)
}

// Service aggregates per-platform profiles. A nil profiler field means that
// platform is unconfigured and is skipped.
type Service struct {
	Bluesky  Profiler
	Mastodon Profiler
	Threads  Profiler
	Nostr    NostrProfiler

	TTL     time.Duration // cache lifetime (default 10m)
	Timeout time.Duration // per-platform fetch timeout (default 6s)

	mu     sync.Mutex
	cached *Identity
	at     time.Time
}

func (s *Service) ttl() time.Duration {
	if s.TTL > 0 {
		return s.TTL
	}
	return 10 * time.Minute
}

func (s *Service) timeout() time.Duration {
	if s.Timeout > 0 {
		return s.Timeout
	}
	return 6 * time.Second
}

// order is the preference order for choosing the primary name/avatar and for the
// handle fallback.
var order = []string{"bluesky", "mastodon", "threads", "nostr"}

// Get returns the aggregated identity, served from cache within the TTL. Safe for
// concurrent use.
func (s *Service) Get(ctx context.Context) *Identity {
	s.mu.Lock()
	if s.cached != nil && time.Since(s.at) < s.ttl() {
		c := s.cached
		s.mu.Unlock()
		return c
	}
	s.mu.Unlock()

	id := &Identity{Accounts: map[string]Account{}}
	var amu sync.Mutex
	var wg sync.WaitGroup

	fetch := func(platform string, run func(context.Context) (string, string, string, error)) {
		defer wg.Done()
		cctx, cancel := context.WithTimeout(ctx, s.timeout())
		defer cancel()
		handle, name, avatar, err := run(cctx)
		if err != nil && handle == "" && name == "" && avatar == "" {
			return // nothing usable from this platform
		}
		amu.Lock()
		id.Accounts[platform] = Account{Platform: platform, Handle: handle, DisplayName: name, Avatar: avatar}
		amu.Unlock()
	}

	if s.Bluesky != nil {
		wg.Add(1)
		go fetch("bluesky", s.Bluesky.Profile)
	}
	if s.Mastodon != nil {
		wg.Add(1)
		go fetch("mastodon", s.Mastodon.Profile)
	}
	if s.Threads != nil {
		wg.Add(1)
		go fetch("threads", s.Threads.Profile)
	}
	if s.Nostr != nil {
		wg.Add(1)
		go fetch("nostr", s.Nostr.OwnerProfile)
	}
	wg.Wait()

	// Primary name + avatar: prefer the most human-friendly source, stable order.
	for _, p := range order {
		a := id.Accounts[p]
		if id.Name == "" && a.DisplayName != "" {
			id.Name = a.DisplayName
		}
		if id.Avatar == "" && a.Avatar != "" {
			id.Avatar = a.Avatar
		}
	}
	if id.Name == "" {
		for _, p := range order {
			if h := id.Accounts[p].Handle; h != "" {
				id.Name = h
				break
			}
		}
	}
	id.Monogram = monogram(id.Name)

	s.mu.Lock()
	s.cached = id
	s.at = time.Now()
	s.mu.Unlock()
	return id
}

// monogram returns the first letter of name (skipping a leading @), uppercased.
func monogram(name string) string {
	for _, r := range name {
		if r == '@' || r == ' ' {
			continue
		}
		return strings.ToUpper(string(r))
	}
	return ""
}
