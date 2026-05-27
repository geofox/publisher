package store

import (
	"strings"
	"time"
)

// Draft is a saved work-in-progress Compose state. See
// docs/superpowers/specs/2026-05-27-drafts-design.md for the full shape.
type Draft struct {
	ID         string    `json:"id"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
	Title      string    `json:"title"`
	MasterText string    `json:"master_text"`
	Tags       []string  `json:"tags"`
	Spec       string    `json:"spec"` // raw spec_json — opaque to the store
	Media      []Media   `json:"media,omitempty"`
}

// DraftListItem is the lightweight projection used by ListDraftsFiltered for
// the sidebar — full spec is deliberately omitted.
type DraftListItem struct {
	ID            string    `json:"id"`
	Title         string    `json:"title"`
	Preview       string    `json:"preview"`
	Tags          []string  `json:"tags"`
	UpdatedAt     time.Time `json:"updated_at"`
	FirstMediaURL string    `json:"first_media_url,omitempty"`
}

// NormalizeTags applies the server-side tag rules: lowercase, trim, strip a
// single leading '#', cap at 32 chars (truncate), drop empties, dedup,
// preserving first-occurrence order. Always returns a non-nil slice.
func NormalizeTags(in []string) []string {
	out := make([]string, 0, len(in))
	seen := make(map[string]struct{}, len(in))
	for _, raw := range in {
		t := strings.TrimSpace(raw)
		t = strings.ToLower(t)
		if strings.HasPrefix(t, "#") {
			t = t[1:]
		}
		if len(t) > 32 {
			t = t[:32]
		}
		if t == "" {
			continue
		}
		if _, dup := seen[t]; dup {
			continue
		}
		seen[t] = struct{}{}
		out = append(out, t)
	}
	return out
}

// DeriveTitle returns the first non-empty line of master_text, trimmed and
// capped at 80 chars. Used when the caller didn't supply an explicit title.
func DeriveTitle(masterText string) string {
	for _, line := range strings.Split(masterText, "\n") {
		t := strings.TrimSpace(line)
		if t == "" {
			continue
		}
		if len(t) > 80 {
			t = t[:80]
		}
		return t
	}
	return ""
}
