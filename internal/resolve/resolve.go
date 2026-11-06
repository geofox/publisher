// Package resolve holds the shape of a pasted post once it is looked up
// (SourceRef: the network's reference, a preview, and what the owner may do
// with it) and Detect, which says which network an input belongs to. The
// lookup itself is an account's (destination.SourceResolver): the API picks
// one of the owner's accounts of the detected type.
package resolve

import (
	"errors"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/geofox/publisher/internal/destination"
)

// Sentinel errors surfaced to the UI.
var (
	// ErrUnsupported: the network's accounts can't look posts up (Threads'
	// API has no URL lookup).
	ErrUnsupported   = errors.New("that network's posts can't be looked up")
	ErrUnrecognized  = errors.New("unrecognized post URL or identifier")
	ErrNotConfigured = errors.New("that platform is not configured")
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
	// ThreadRootID is the root of the thread the event replies into (hex),
	// empty when it starts one; Mentions are its p-tags. A reply to it
	// carries both (NIP-10).
	ThreadRootID string   `json:"thread_root_id,omitempty"`
	Mentions     []string `json:"mentions,omitempty"`
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
	// Video marks a moving attachment (video, gifv, audio); link-quotes
	// copy images only.
	Video bool `json:"video,omitempty"`
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

var (
	reNostrID = regexp.MustCompile(`^(?:nostr:|web\+nostr:)?(?:nevent|note|naddr)1[0-9a-zA-Z]+$`)
	reHex64   = regexp.MustCompile(`^[0-9a-fA-F]{64}$`)
)

// Detect classifies an input as "bluesky" | "mastodon" | "nostr" | "threads" |
// "" (unrecognized). Mastodon is the fallthrough for any non-Bluesky/Threads
// http(s) URL, since Mastodon instances use arbitrary domains. Surrounding
// space is ignored.
func Detect(input string) string {
	input = strings.TrimSpace(input)
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

// FromResolved converts a driver's ResolvedSource to a SourceRef.
func FromResolved(rs destination.ResolvedSource) *SourceRef {
	r := rs.Ref
	media := make([]Media, 0, len(rs.Preview.Media))
	for _, m := range rs.Preview.Media {
		media = append(media, Media{URL: m.URL, Alt: m.Alt, Video: m.Video})
	}
	allowed := func(a destination.ActionAllowed) Cap { return Cap{Allowed: a.Allowed, Reason: a.Reason} }
	return &SourceRef{
		Platform: r.Type,
		Ref: PlatformRef{
			URI: r.URI, CID: r.CID, ReplyRootURI: r.ReplyRootURI, ReplyRootCID: r.ReplyRootCID,
			LocalID: r.LocalID,
			EventID: r.EventID, Author: r.Author, RelayHints: r.RelayHints, Kind: r.EventKind,
			ThreadRootID: r.ThreadRootID, Mentions: r.Mentions,
		},
		Preview: Preview{
			AuthorName: rs.Preview.AuthorName, AuthorHandle: rs.Preview.AuthorHandle,
			Text: rs.Preview.Text, Media: media, CreatedAt: rs.Preview.CreatedAt, WebURL: rs.Preview.WebURL,
		},
		Caps: Caps{Reply: allowed(rs.Actions.Reply), Quote: allowed(rs.Actions.Quote), Repost: allowed(rs.Actions.Repost)},
	}
}
