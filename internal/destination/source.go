package destination

import (
	"context"
	"errors"
	"time"
)

// SourceResolver is implemented by accounts that can look up an external
// post (to reply to, quote or repost) from a URL or id the owner pasted.
// The account resolves through its own server, so ids it returns (a
// Mastodon status id) are valid for that account.
type SourceResolver interface {
	ResolveSource(ctx context.Context, input string) (ResolvedSource, error)
}

// ResolvedSource is an external post: how to act on it, what it looks like,
// and which actions the network allows on it.
type ResolvedSource struct {
	Ref     SourceRef     `json:"ref"`
	Preview SourcePreview `json:"preview"`
	Actions SourceActions `json:"actions"`
}

// SourcePreview is what the owner sees of an external post.
type SourcePreview struct {
	AuthorName   string        `json:"author_name"`
	AuthorHandle string        `json:"author_handle"`
	Text         string        `json:"text"`
	Media        []SourceMedia `json:"media,omitempty"`
	CreatedAt    time.Time     `json:"created_at"`
	WebURL       string        `json:"web_url"`
}

// SourceMedia is one attachment of an external post.
type SourceMedia struct {
	URL string `json:"url"`
	Alt string `json:"alt,omitempty"`
	// Video marks a moving attachment (video, gifv, audio).
	Video bool `json:"video,omitempty"`
}

// SourceActions says which actions are allowed on an external post.
type SourceActions struct {
	Reply  ActionAllowed `json:"reply"`
	Quote  ActionAllowed `json:"quote"`
	Repost ActionAllowed `json:"repost"`
}

// ActionAllowed is one action's availability; Reason explains a refusal
// (or annotates an allowed action).
type ActionAllowed struct {
	Allowed bool   `json:"allowed"`
	Reason  string `json:"reason,omitempty"`
}

var (
	// ErrSourceNotFound: nothing at that URL or id.
	ErrSourceNotFound = errors.New("destination: post not found")
	// ErrSourceUnsupported: the input isn't something this account can
	// resolve.
	ErrSourceUnsupported = errors.New("destination: can't resolve that input")
)
