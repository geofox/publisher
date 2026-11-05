package bluesky

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/geofox/publisher/internal/destination"
	"github.com/geofox/publisher/internal/transcode"
)

// Type is the destination type identifier.
const Type = "bluesky"

// Option keys.
const (
	OptReplyGate     = "reply_gate"
	OptDisableQuotes = "disable_quotes"
)

// Driver is the Bluesky destination type.
type Driver struct{}

var _ destination.Driver = Driver{}

// Manifest describes Bluesky.
func (Driver) Manifest() destination.Manifest {
	return destination.Manifest{
		Contract: destination.ContractVersion,
		Type:     Type,
		Name:     "Bluesky",
		Kinds:    []destination.Kind{destination.KindNote},
		Options: []destination.OptionSpec{
			{Key: OptReplyGate, Type: destination.OptionString, Label: "Who can reply",
				Help: `"" (anyone), "nobody", or a comma list of mention, following, follower`},
			{Key: OptDisableQuotes, Type: destination.OptionBool, Label: "Disable quote posts"},
		},
		Defaults: destination.Capabilities{
			Text:      destination.TextCaps{MaxLength: maxGraphemes, Counting: destination.Counting{Unit: destination.UnitGraphemes}},
			Threading: destination.ThreadingCaps{Supported: true},
			Media: destination.MediaCaps{
				Transport: destination.TransportBytes,
				// 10 is the app.bsky.embed.gallery client soft limit (1.123+).
				Images: destination.Limit{Supported: true, MaxCount: 10, MaxBytes: transcode.Bluesky.MaxBytes},
				Video: destination.Limit{Supported: true, MaxCount: 1, MaxBytes: transcode.BlueskyVideoMaxBytes,
					AdvisoryDurationSecs: transcode.BlueskyVideoAdvisorySecs},
				AltText: true,
			},
			LinkPreview:  destination.LinkPreviewClient,
			Languages:    3,
			Interactions: destination.InteractionCaps{Reply: true, Quote: true, Repost: true, QuoteVideo: true},
			Idempotent:   destination.IdempotentNone,
		},
	}
}

// Account is one Bluesky identity.
type Account struct {
	C *Client
}

var (
	_ destination.Account    = (*Account)(nil)
	_ destination.Reposter   = (*Account)(nil)
	_ destination.Identifier = (*Account)(nil)
)

// Manifest implements destination.Account.
func (a *Account) Manifest() destination.Manifest { return Driver{}.Manifest() }

// Capabilities implements destination.Account. Bluesky's limits are fixed by
// the lexicon, so they are the manifest defaults.
func (a *Account) Capabilities(context.Context) destination.Capabilities {
	return Driver{}.Manifest().Defaults
}

// refData is what Bluesky keeps in ItemRef.Data: the record's CID, needed to
// reply to it.
type refData struct {
	CID string `json:"cid"`
}

// RefData encodes a CID as ItemRef.Data. Exported for records written before
// ItemRef.Data existed, which only kept the CID.
func RefData(cid string) json.RawMessage {
	if cid == "" {
		return nil
	}
	b, _ := json.Marshal(refData{CID: cid})
	return b
}

func cidOf(r destination.ItemRef) string {
	var d refData
	_ = json.Unmarshal(r.Data, &d)
	return d.CID
}

// Publish implements destination.Account.
func (a *Account) Publish(ctx context.Context, req destination.PublishRequest) (destination.PublishResult, error) {
	trace, _ := json.Marshal(map[string]any{
		"text": req.Text, "langs": req.Languages,
		"bluesky_reply":          destination.OptString(req.Options, OptReplyGate),
		"bluesky_disable_quotes": destination.OptBool(req.Options, OptDisableQuotes),
	})
	out := destination.PublishResult{Trace: destination.Trace{Request: string(trace)}}

	p := Post{
		Text: req.Text, Langs: req.Languages,
		ReplyGate:     ParseReplyGate(destination.OptString(req.Options, OptReplyGate)),
		DisableQuotes: destination.OptBool(req.Options, OptDisableQuotes),
	}
	for _, m := range req.Media {
		if m.IsVideo() {
			if p.Video == nil { // one video per post; the core enforces it
				p.Video = &Video{Bytes: m.Bytes, Alt: m.Alt, W: m.Width, H: m.Height}
			}
			continue
		}
		p.Images = append(p.Images, Image{Bytes: m.Bytes, Mime: m.Mime, Alt: m.Alt})
	}
	switch {
	case req.InReplyTo != nil:
		p.Reply = &ReplyRef{
			RootURI: req.InReplyTo.Root.ID, RootCID: cidOf(req.InReplyTo.Root),
			ParentURI: req.InReplyTo.Parent.ID, ParentCID: cidOf(req.InReplyTo.Parent),
		}
	case req.ReplyTo != nil:
		root, rootCID := req.ReplyTo.ReplyRootURI, req.ReplyTo.ReplyRootCID
		if root == "" { // the source is itself a thread root
			root, rootCID = req.ReplyTo.URI, req.ReplyTo.CID
		}
		p.Reply = &ReplyRef{RootURI: root, RootCID: rootCID, ParentURI: req.ReplyTo.URI, ParentCID: req.ReplyTo.CID}
	}
	if req.Quote != nil {
		p.Quote = &QuoteRef{URI: req.Quote.URI, CID: req.Quote.CID}
	}
	if c := req.LinkCard; c != nil {
		ext := &ExternalCard{URI: c.URL, Title: c.Title, Description: c.Description, Thumb: c.Thumb, ThumbMime: c.ThumbMime}
		for _, r := range c.Records {
			ext.Refs = append(ext.Refs, ExternalRef{URI: r.URI, CID: r.CID})
		}
		p.External = ext
	}

	res, err := a.C.Post(ctx, p)
	// Post returns the published post even when a gate write fails, so keep
	// the link either way.
	out.Ref = destination.ItemRef{ID: res.RemoteID, URL: res.RemoteURL, Data: RefData(res.CID)}
	if b, mErr := json.Marshal(res); mErr == nil {
		out.Trace.Response = string(b)
	}
	if err != nil {
		// Live but ungated (only a threadgate/postgate write failed): partial.
		if res.RemoteID != "" {
			out.Status = destination.StatusPartial
		} else {
			out.Status = destination.StatusFailed
		}
		return out, classify(err)
	}
	out.Status = destination.StatusPublished
	return out, nil
}

// Repost implements destination.Reposter.
func (a *Account) Repost(ctx context.Context, src destination.SourceRef) (destination.PublishResult, error) {
	res, err := a.C.Repost(ctx, src.URI, src.CID)
	if err != nil {
		return destination.PublishResult{Status: destination.StatusFailed}, classify(err)
	}
	return destination.PublishResult{
		Status: destination.StatusPublished,
		Ref:    destination.ItemRef{ID: res.RemoteID, URL: res.RemoteURL, Data: RefData(res.CID)},
	}, nil
}

// Identity implements destination.Identifier.
func (a *Account) Identity(ctx context.Context) (destination.Identity, error) {
	handle, name, avatar, err := a.C.Profile(ctx)
	if err != nil {
		return destination.Identity{}, err
	}
	return destination.Identity{Handle: handle, DisplayName: name, AvatarURL: avatar,
		ProfileURL: "https://bsky.app/profile/" + handle}, nil
}

// errTooLong is the client's own pre-flight length check.
var errTooLong = errors.New("text too long")

func classify(err error) error {
	e := destination.Classify(err)
	if e.Class == destination.ClassTransient && isLocalValidation(err) {
		e = destination.Invalid(err)
	}
	return e
}

// isLocalValidation reports errors the client raises before any request:
// they will fail the same way on every retry.
func isLocalValidation(err error) bool {
	return errors.Is(err, errTooLong) || errors.Is(err, transcode.ErrUnfittable)
}
