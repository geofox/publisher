package mastodon

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"time"

	gomast "github.com/mattn/go-mastodon"

	"github.com/geofox/publisher/internal/destination"
	"github.com/geofox/publisher/internal/transcode"
)

// Type is the destination type identifier.
const Type = "mastodon"

// Driver is the Mastodon destination type.
type Driver struct{}

var _ destination.Driver = Driver{}

// Manifest describes Mastodon. The defaults are vanilla Mastodon's; each
// account replaces them with what its instance announces.
func (Driver) Manifest() destination.Manifest {
	return destination.Manifest{
		Contract: destination.ContractVersion,
		Type:     Type,
		Name:     "Mastodon",
		Kinds:    []destination.Kind{destination.KindNote},
		Defaults: destination.Capabilities{
			Text: destination.TextCaps{MaxLength: 500, Counting: destination.Counting{Unit: destination.UnitGraphemes}},
			// Mastodon counts every URL as 23 characters and a remote mention
			// as its local part. The splitter and the composer's counter
			// don't apply that rule yet, so it is not declared until they do
			// (the counts stay conservative: literal URLs are longer).
			Threading: destination.ThreadingCaps{Supported: true},
			Media: destination.MediaCaps{
				Transport: destination.TransportBytes,
				Images: destination.Limit{Supported: true, MaxCount: 4,
					MaxBytes: transcode.Mastodon.MaxBytes, MaxPixels: transcode.Mastodon.MaxPixels},
				Video:        destination.Limit{Supported: true, MaxCount: 1, MaxBytes: transcode.MastodonVideoMaxBytes},
				AltText:      true,
				AltMaxLength: 1500,
			},
			LinkPreview:    destination.LinkPreviewServer,
			ContentWarning: true,
			Sensitive:      true,
			Languages:      1,
			Visibility:     []string{"public", "unlisted", "private", "direct"},
			Interactions:   destination.InteractionCaps{Reply: true, Quote: true, Repost: true}, // a quote carries text and images only
			Idempotent:     destination.IdempotentKey,
			// Mastodon keeps an Idempotency-Key for an hour.
			IdempotencyKeyTTLSecs: 3600,
		},
	}
}

// capsTTL is how long announced instance limits are trusted.
const capsTTL = time.Hour

// Account is one Mastodon identity.
type Account struct {
	C *Client

	mu        sync.Mutex
	caps      destination.Capabilities
	fetchedAt time.Time
	failedAt  time.Time
	inflight  chan struct{} // closed when the running limits fetch ends
}

var (
	_ destination.Account    = (*Account)(nil)
	_ destination.Reposter   = (*Account)(nil)
	_ destination.Identifier = (*Account)(nil)
	_ destination.Locator    = (*Account)(nil)
)

// NewAccount wraps a client.
func NewAccount(c *Client) *Account { return &Account{C: c} }

// Manifest implements destination.Account.
func (a *Account) Manifest() destination.Manifest { return Driver{}.Manifest() }

// Capabilities implements destination.Account: the manifest defaults with the
// instance's announced limits applied, cached for an hour. A failed fetch is
// retried after a minute; until then the last good value (or the defaults) is
// used.
//
// One fetch runs at a time, outside the lock and on its own 3 s budget, so a
// caller that gives up (a closed browser tab) neither cancels it nor marks it
// failed; concurrent callers wait for the same fetch, or until their own ctx
// ends, and then read the cache.
func (a *Account) Capabilities(ctx context.Context) destination.Capabilities {
	a.mu.Lock()
	now := time.Now()
	if !a.fetchedAt.IsZero() && now.Sub(a.fetchedAt) < capsTTL {
		c := a.caps
		a.mu.Unlock()
		return c
	}
	if !a.failedAt.IsZero() && now.Sub(a.failedAt) < time.Minute {
		c := a.cachedOrDefaults()
		a.mu.Unlock()
		return c
	}
	done := a.inflight
	if done == nil {
		done = make(chan struct{})
		a.inflight = done
		go a.fetchLimits(done)
	}
	a.mu.Unlock()
	select {
	case <-done:
	case <-ctx.Done():
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.cachedOrDefaults()
}

// fetchLimits reads the instance's limits into the cache and closes done.
func (a *Account) fetchLimits(done chan struct{}) {
	fctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	lim, err := a.C.Instance(fctx)
	a.mu.Lock()
	now := time.Now()
	if err != nil {
		a.failedAt = now
	} else {
		a.caps, a.fetchedAt, a.failedAt = applyLimits(Driver{}.Manifest().Defaults, lim), now, time.Time{}
	}
	a.inflight = nil
	a.mu.Unlock()
	close(done)
}

func (a *Account) cachedOrDefaults() destination.Capabilities {
	if !a.fetchedAt.IsZero() {
		return a.caps
	}
	return Driver{}.Manifest().Defaults
}

// applyLimits overlays announced limits on c; unannounced (zero) ones keep
// the default.
func applyLimits(c destination.Capabilities, l InstanceLimits) destination.Capabilities {
	if l.MaxCharacters > 0 {
		c.Text.MaxLength = l.MaxCharacters
	}
	if l.MaxMediaAttachments > 0 {
		c.Media.Images.MaxCount = l.MaxMediaAttachments
	}
	if l.ImageSizeLimit > 0 {
		c.Media.Images.MaxBytes = l.ImageSizeLimit
	}
	if l.ImageMatrixLimit > 0 {
		c.Media.Images.MaxPixels = l.ImageMatrixLimit
	}
	if l.VideoSizeLimit > 0 {
		c.Media.Video.MaxBytes = l.VideoSizeLimit
	}
	if l.DescriptionLimit > 0 {
		c.Media.AltMaxLength = l.DescriptionLimit
	}
	return c
}

// Publish implements destination.Account.
func (a *Account) Publish(ctx context.Context, req destination.PublishRequest) (destination.PublishResult, error) {
	lang := ""
	if len(req.Languages) > 0 {
		lang = req.Languages[0]
	}
	trace, _ := json.Marshal(map[string]any{
		"text": req.Text, "visibility": req.Visibility, "language": lang, "spoiler_text": req.ContentWarning,
	})
	out := destination.PublishResult{Status: destination.StatusFailed, Trace: destination.Trace{Request: string(trace)}}

	var images []Image
	var video *Video
	for _, m := range req.Media {
		if m.IsVideo() {
			if video == nil {
				video = &Video{Bytes: m.Bytes, Alt: m.Alt}
			}
			continue
		}
		images = append(images, Image{Bytes: m.Bytes, Alt: m.Alt})
	}

	p := Post{
		Text: req.Text, SpoilerText: req.ContentWarning, Sensitive: req.Sensitive,
		Visibility: req.Visibility, Language: lang, Images: images, Video: video,
		IdempotencyKey: req.IdempotencyKey,
	}
	var res Result
	var err error
	if req.Quote != nil {
		// Native quotes (Mastodon 4.5+) carry text and images; the composer
		// doesn't offer video with quotes.
		res, err = a.C.QuotePost(ctx, p, req.Quote.LocalID)
	} else {
		switch {
		case req.InReplyTo != nil:
			p.InReplyToID = req.InReplyTo.Parent.ID
		case req.ReplyTo != nil:
			p.InReplyToID = req.ReplyTo.LocalID
		}
		res, err = a.C.Post(ctx, p)
	}
	if err != nil {
		return out, classify(err)
	}
	b, _ := json.Marshal(res)
	out.Status = destination.StatusPublished
	out.Ref = destination.ItemRef{ID: res.RemoteID, URL: res.RemoteURL}
	out.Trace.Response = string(b)
	return out, nil
}

// Repost implements destination.Reposter (a boost).
func (a *Account) Repost(ctx context.Context, src destination.SourceRef) (destination.PublishResult, error) {
	res, err := a.C.Reblog(ctx, src.LocalID)
	if err != nil {
		return destination.PublishResult{Status: destination.StatusFailed}, classify(err)
	}
	return destination.PublishResult{Status: destination.StatusPublished,
		Ref: destination.ItemRef{ID: res.RemoteID, URL: res.RemoteURL}}, nil
}

// Identity implements destination.Identifier.
func (a *Account) Identity(ctx context.Context) (destination.Identity, error) {
	handle, name, avatar, err := a.C.Profile(ctx)
	if err != nil {
		return destination.Identity{}, err
	}
	return destination.Identity{Handle: handle, DisplayName: name, AvatarURL: avatar}, nil
}

// classify maps go-mastodon's APIError (which carries the status) and the
// rest through destination.Classify.
func classify(err error) error {
	var ae *gomast.APIError
	if errors.As(err, &ae) {
		return &destination.Error{Class: destination.ClassForStatus(ae.StatusCode), Err: err}
	}
	return destination.Classify(err)
}

// Locate implements destination.Locator: the URL of one of the account's
// own statuses (on this instance or as federated elsewhere), as the
// reference a thread's next item replies to.
func (a *Account) Locate(ctx context.Context, postURL string) (destination.ItemRef, error) {
	st, err := a.C.ResolveStatus(ctx, postURL)
	if err != nil {
		if errors.Is(err, ErrStatusNotFound) {
			return destination.ItemRef{}, destination.ErrPostNotFound
		}
		return destination.ItemRef{}, classify(err)
	}
	me, err := a.C.OwnAccountID(ctx)
	if err != nil {
		return destination.ItemRef{}, classify(err)
	}
	if st.AuthorID != me {
		return destination.ItemRef{}, destination.ErrNotOwnPost
	}
	return destination.ItemRef{ID: st.LocalID, URL: st.WebURL}, nil
}
