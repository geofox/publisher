// Package resolve turns a pasted post URL or Nostr identifier into a SourceRef:
// the platform-native handle, a preview, and what the owner may do with it. It is
// the inbound counterpart to internal/dispatch and wraps the platform clients via
// adapters (see adapters.go).
package resolve

import (
	"context"
	"errors"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// Sentinel errors surfaced to the UI.
var (
	ErrThreadsUnsupported = errors.New("threads posts can't be resolved: the API has no URL→id lookup")
	ErrUnrecognized       = errors.New("unrecognized post URL or identifier")
	ErrNotConfigured      = errors.New("that platform is not configured")
)

// SourceRef is a resolved external post.
type SourceRef struct {
	Platform string      `json:"platform"`
	Ref      PlatformRef `json:"ref"`
	Preview  Preview     `json:"preview"`
	Caps     Caps        `json:"caps"`
}

// PlatformRef is the platform-native identity needed to act later (Plan B). It is
// a flat, JSON-serializable union; only the fields relevant to Platform are set.
type PlatformRef struct {
	// Bluesky
	URI          string `json:"uri,omitempty"`
	CID          string `json:"cid,omitempty"`
	ReplyRootURI string `json:"reply_root_uri,omitempty"`
	ReplyRootCID string `json:"reply_root_cid,omitempty"`
	// Mastodon
	LocalID string `json:"local_id,omitempty"`
	// Nostr
	EventID    string   `json:"event_id,omitempty"` // hex
	Author     string   `json:"author,omitempty"`   // hex pubkey
	RelayHints []string `json:"relay_hints,omitempty"`
	Kind       int      `json:"kind,omitempty"`
}

type Preview struct {
	AuthorName   string    `json:"author_name"`
	AuthorHandle string    `json:"author_handle"`
	Text         string    `json:"text"`
	Media        []Media   `json:"media,omitempty"`
	CreatedAt    time.Time `json:"created_at"`
	WebURL       string    `json:"web_url"`
}

type Media struct {
	URL string `json:"url"`
	Alt string `json:"alt,omitempty"`
}

// Caps reports per-action availability; Reason explains a false Allowed.
type Caps struct {
	Reply  Cap `json:"reply"`
	Quote  Cap `json:"quote"`
	Repost Cap `json:"repost"`
}

type Cap struct {
	Allowed bool   `json:"allowed"`
	Reason  string `json:"reason,omitempty"`
}

// Source resolves one platform's input into a SourceRef.
type Source interface {
	ResolveSource(ctx context.Context, input string) (*SourceRef, error)
}

// Service detects the platform and delegates. Each field is nil when that
// platform isn't configured.
type Service struct {
	Bluesky  Source
	Mastodon Source
	Nostr    Source
}

func (s *Service) Resolve(ctx context.Context, input string) (*SourceRef, error) {
	input = strings.TrimSpace(input)
	switch detect(input) {
	case "bluesky":
		if s.Bluesky == nil {
			return nil, ErrNotConfigured
		}
		return s.Bluesky.ResolveSource(ctx, input)
	case "mastodon":
		if s.Mastodon == nil {
			return nil, ErrNotConfigured
		}
		return s.Mastodon.ResolveSource(ctx, input)
	case "nostr":
		if s.Nostr == nil {
			return nil, ErrNotConfigured
		}
		return s.Nostr.ResolveSource(ctx, input)
	case "threads":
		return nil, ErrThreadsUnsupported
	default:
		return nil, ErrUnrecognized
	}
}

var (
	reNostrID = regexp.MustCompile(`^(?:nostr:|web\+nostr:)?(?:nevent|note|naddr)1[0-9a-zA-Z]+$`)
	reHex64   = regexp.MustCompile(`^[0-9a-fA-F]{64}$`)
)

// detect classifies an input as "bluesky" | "mastodon" | "nostr" | "threads" |
// "" (unrecognized). Mastodon is the fallthrough for any non-Bluesky/Threads
// http(s) URL, since Mastodon instances use arbitrary domains.
func detect(input string) string {
	if reNostrID.MatchString(input) || reHex64.MatchString(input) {
		return "nostr"
	}
	u, err := url.Parse(input)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return ""
	}
	host := strings.ToLower(strings.TrimPrefix(u.Hostname(), "www.")) // Hostname() drops any :port
	switch {
	case host == "bsky.app":
		return "bluesky"
	case host == "threads.net" || host == "threads.com":
		return "threads"
	default:
		return "mastodon"
	}
}
