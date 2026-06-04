// Package progress provides an in-memory, ephemeral live-progress layer for
// in-flight posts: per-platform and (for Nostr) per-relay state, streamed to
// subscribers. It is independent of the durable store; the post record remains
// the source of truth.
package progress

const (
	StatusQueued  = "queued"
	StatusRunning = "running"
	StatusSuccess = "success"
	StatusPartial = "partial"
	StatusFailed  = "failed"

	// relay-only states
	RelayOK      = "ok"
	RelayFailed  = "failed"
	RelaySkipped = "skipped"
)

// RelayState is one Nostr relay row.
type RelayState struct {
	URL     string `json:"url"`
	Status  string `json:"status"` // queued|running|ok|failed|skipped
	Message string `json:"message,omitempty"`
}

// PlatformState is one platform row. Relays is populated only for Nostr.
type PlatformState struct {
	Platform string       `json:"platform"`
	Status   string       `json:"status"` // queued|running|success|partial|failed
	Detail   string       `json:"detail,omitempty"`
	URL      string       `json:"url,omitempty"`
	Native   bool         `json:"native,omitempty"`
	Relays   []RelayState `json:"relays,omitempty"`
}

// Snapshot is the full progress tree for one post. Every SSE message carries a
// complete snapshot (not deltas), so rendering is idempotent.
type Snapshot struct {
	PostID    string          `json:"post_id"`
	Status    string          `json:"status"`
	Platforms []PlatformState `json:"platforms"`
}

// recomputeStatus derives the overall status from the platform rows: running
// until every platform is terminal, then success (all ok) / failed (all failed)
// / partial (anything mixed, or any platform itself partial).
func (s *Snapshot) recomputeStatus() {
	if len(s.Platforms) == 0 {
		s.Status = StatusFailed
		return
	}
	succ, failed, terminal := 0, 0, 0
	for _, p := range s.Platforms {
		switch p.Status {
		case StatusSuccess:
			succ++
			terminal++
		case StatusFailed:
			failed++
			terminal++
		case StatusPartial:
			terminal++
		}
	}
	if terminal < len(s.Platforms) {
		s.Status = StatusRunning
		return
	}
	switch {
	case failed == len(s.Platforms):
		s.Status = StatusFailed
	case succ == len(s.Platforms):
		s.Status = StatusSuccess
	default:
		s.Status = StatusPartial
	}
}

// clone deep-copies a Snapshot so a broadcast value can't be mutated by later
// hub writes while a subscriber is reading it.
func (s Snapshot) clone() Snapshot {
	out := s
	out.Platforms = make([]PlatformState, len(s.Platforms))
	for i, p := range s.Platforms {
		cp := p
		if len(p.Relays) > 0 {
			cp.Relays = make([]RelayState, len(p.Relays))
			copy(cp.Relays, p.Relays)
		}
		out.Platforms[i] = cp
	}
	return out
}
