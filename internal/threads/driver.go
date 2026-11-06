package threads

import (
	"context"
	"encoding/json"

	"github.com/geofox/publisher/internal/destination"
	"github.com/geofox/publisher/internal/transcode"
)

// Type is the destination type identifier.
const Type = "threads"

// Option keys.
const (
	OptTopicTag     = "topic_tag"
	OptReplyControl = "reply_control"
)

// Driver is the Threads destination type.
type Driver struct{}

var _ destination.Driver = Driver{}

// Manifest describes Threads.
func (Driver) Manifest() destination.Manifest {
	return destination.Manifest{
		Contract: destination.ContractVersion,
		Type:     Type,
		Name:     "Threads",
		Kinds:    []destination.Kind{destination.KindNote},
		Options: []destination.OptionSpec{
			{Key: OptTopicTag, Type: destination.OptionString, Label: "Topic tag"},
			{Key: OptReplyControl, Type: destination.OptionEnum, Label: "Who can reply", Enum: []destination.EnumValue{
				{Value: "", Label: "Everyone"},
				{Value: "everyone", Label: "Everyone"},
				{Value: "accounts_you_follow", Label: "Accounts you follow"},
				{Value: "mentioned_only", Label: "Mentioned only"},
				{Value: "parent_post_author_only", Label: "Parent post author only"},
				{Value: "followers_only", Label: "Followers only"},
			}},
		},
		Defaults: destination.Capabilities{
			// 500 characters, an emoji counting as its UTF-8 bytes, and at
			// most 5 distinct links (refused since 2025-12-22): see
			// https://developers.facebook.com/docs/threads/posts
			Text: destination.TextCaps{MaxLength: 500, MaxLinks: 5,
				Counting: destination.Counting{Unit: destination.UnitGraphemes, EmojiUTF8Bytes: true}},
			Threading: destination.ThreadingCaps{Supported: true},
			Media: destination.MediaCaps{
				// Meta fetches media by URL, and accepts only JPEG/PNG up to
				// 8 MB; anything else is re-encoded and re-hosted first.
				Transport: destination.TransportURL,
				// Carousels take up to 20 items; 10 keeps posts readable.
				Images: destination.Limit{Supported: true, MaxCount: 10,
					MaxBytes: transcode.Threads.MaxBytes, Mimes: transcode.Threads.Mimes},
				Video: destination.Limit{Supported: true, MaxCount: 1, MaxBytes: transcode.ThreadsVideoMaxBytes,
					MaxDurationSecs: transcode.ThreadsVideoMaxSecs},
				AltText: true,
			},
			LinkPreview: destination.LinkPreviewServer,
			// Replies to the account's own posts only: the API has no lookup
			// from a URL to a media id, so external posts can't be targeted.
			Interactions: destination.InteractionCaps{},
			Idempotent:   destination.IdempotentNone,
		},
	}
}

// Account is one Threads identity.
type Account struct {
	C *Client
}

var (
	_ destination.Account    = (*Account)(nil)
	_ destination.Identifier = (*Account)(nil)
)

// Manifest implements destination.Account.
func (a *Account) Manifest() destination.Manifest { return Driver{}.Manifest() }

// Capabilities implements destination.Account.
func (a *Account) Capabilities(context.Context) destination.Capabilities {
	return Driver{}.Manifest().Defaults
}

// Publish implements destination.Account.
func (a *Account) Publish(ctx context.Context, req destination.PublishRequest) (destination.PublishResult, error) {
	topic := destination.OptString(req.Options, OptTopicTag)
	control := destination.OptString(req.Options, OptReplyControl)
	trace, _ := json.Marshal(map[string]any{
		"text": req.Text, "topic_tag": topic, "images": len(req.Media), "reply_control": control,
	})
	out := destination.PublishResult{Status: destination.StatusFailed, Trace: destination.Trace{Request: string(trace)}}

	p := Post{Text: req.Text, TopicTag: topic, ReplyControl: control}
	for _, m := range req.Media {
		if m.IsVideo() {
			if p.Video == nil {
				p.Video = &Video{URL: m.URL, Alt: m.Alt}
			}
			continue
		}
		p.Images = append(p.Images, Image{URL: m.URL, Alt: m.Alt})
	}
	if req.InReplyTo != nil {
		p.ReplyToID = req.InReplyTo.Parent.ID
	}
	res, err := a.C.Post(ctx, p)
	if err != nil {
		return out, classify(err)
	}
	b, _ := json.Marshal(res)
	out.Status = destination.StatusPublished
	out.Ref = destination.ItemRef{ID: res.RemoteID, URL: res.RemoteURL}
	out.Trace.Response = string(b)
	return out, nil
}

// Identity implements destination.Identifier.
func (a *Account) Identity(ctx context.Context) (destination.Identity, error) {
	handle, name, avatar, err := a.C.Profile(ctx)
	if err != nil {
		return destination.Identity{}, err
	}
	return destination.Identity{Handle: handle, DisplayName: name, AvatarURL: avatar}, nil
}
