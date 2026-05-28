// Package feed builds the public homepage feed and decides which published
// posts are eligible to appear in it. The same Eligible predicate gates the
// publish webhook, so the read API and the webhook can never disagree about
// what is "public".
package feed

import (
	"encoding/json"
	"time"

	"github.com/geofox/publisher/internal/store"
)

type Response struct {
	Version     int       `json:"version"`
	GeneratedAt time.Time `json:"generated_at"`
	Posts       []Item    `json:"posts"`
}

type Item struct {
	ID          string       `json:"id"`
	PublishedAt time.Time    `json:"published_at"`
	Text        string       `json:"text"`
	Media       []MediaItem  `json:"media,omitempty"`
	Interaction *Interaction `json:"interaction,omitempty"`
	Links       []Link       `json:"links"`
}

type MediaItem struct {
	URL      string `json:"url"`
	Mime     string `json:"mime"`
	Alt      string `json:"alt,omitempty"`
	Dim      string `json:"dim,omitempty"`
	Blurhash string `json:"blurhash,omitempty"`
}

type Interaction struct {
	Action         string `json:"action"`
	SourcePlatform string `json:"source_platform"`
	SourceURL      string `json:"source_url"`
	SourceAuthor   string `json:"source_author"`
}

type Link struct {
	Platform string `json:"platform"`
	URL      string `json:"url"`
}

const defaultLimit, maxLimit = 20, 100

func isReply(p store.Post) bool {
	return p.Interaction != nil && p.Interaction.Action == "reply"
}

// publicVisible reports whether a target is publicly visible. Only Mastodon has
// a per-post visibility setting; an unset value means the account default,
// which we treat as public. A malformed fields_json is treated as non-public.
func publicVisible(platform, fieldsJSON string) bool {
	if platform != "mastodon" || fieldsJSON == "" {
		return true
	}
	var f struct {
		Visibility string `json:"visibility"`
	}
	if err := json.Unmarshal([]byte(fieldsJSON), &f); err != nil {
		return false
	}
	// Absent/empty visibility = Mastodon account default, treated as public.
	// Non-public values (dropped): unlisted, private, direct.
	return f.Visibility == "" || f.Visibility == "public"
}

// targetLink returns the public link for a target, or false if it should not
// appear: it must have succeeded, carry a URL, and be publicly visible.
func targetLink(t store.Target) (Link, bool) {
	if t.Status != "success" || t.RemoteURL == "" || !publicVisible(t.Platform, t.FieldsJSON) {
		return Link{}, false
	}
	return Link{Platform: t.Platform, URL: t.RemoteURL}, true
}

// Eligible reports whether a post may appear in the feed: it is not a reply and
// has at least one public, successful platform link.
func Eligible(p store.Post) bool {
	if isReply(p) {
		return false
	}
	for _, t := range p.Targets {
		if _, ok := targetLink(t); ok {
			return true
		}
	}
	return false
}

func publishedAt(p store.Post) time.Time {
	if p.FirstSuccessAt != nil {
		return *p.FirstSuccessAt
	}
	return p.CreatedAt
}

// Build reshapes hydrated store posts into the feed response, applying the
// link filter, dropping ineligible/link-less posts, and trimming to limit.
func Build(posts []store.Post, limit int) Response {
	if limit <= 0 {
		limit = defaultLimit
	}
	if limit > maxLimit {
		limit = maxLimit
	}
	out := Response{Version: 1, GeneratedAt: time.Now().UTC(), Posts: []Item{}}
	for _, p := range posts {
		if len(out.Posts) >= limit {
			break
		}
		if isReply(p) {
			continue
		}
		links := make([]Link, 0, len(p.Targets))
		for _, t := range p.Targets {
			if l, ok := targetLink(t); ok {
				links = append(links, l)
			}
		}
		if len(links) == 0 {
			continue
		}
		item := Item{
			ID:          p.ID,
			PublishedAt: publishedAt(p),
			Text:        p.MasterText,
			Links:       links,
			Media:       mediaItems(p.Media),
		}
		if p.Interaction != nil {
			item.Interaction = &Interaction{
				Action:         p.Interaction.Action,
				SourcePlatform: p.Interaction.SourcePlatform,
				SourceURL:      p.Interaction.SourceURL,
				SourceAuthor:   p.Interaction.SourceAuthor,
			}
		}
		out.Posts = append(out.Posts, item)
	}
	return out
}

func mediaItems(ms []store.Media) []MediaItem {
	if len(ms) == 0 {
		return nil
	}
	out := make([]MediaItem, 0, len(ms))
	for _, m := range ms {
		out = append(out, MediaItem{
			URL: m.BlossomURL, Mime: m.Mime, Alt: m.Alt, Dim: m.Dim, Blurhash: m.Blurhash,
		})
	}
	return out
}
