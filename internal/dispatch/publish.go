package dispatch

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/geofox/publisher/internal/destination"
	"github.com/geofox/publisher/internal/metrics"
	"github.com/geofox/publisher/internal/store"
)

// headSpec makes a chain's head item a reply to, or a quote of, an external
// post instead of a plain post. nil → plain head. Tail items always thread as
// plain replies under the head.
type headSpec struct {
	reply *InteractRef
	quote *InteractRef
}

// item is one thing to publish to one account: a post, or one segment of a
// thread.
type item struct {
	text   string
	ov     Overrides
	imgs   []Img
	thread *destination.ThreadRef // segment k+1 replies to segment k
	head   *headSpec              // only on a thread's first item
	// postID and ordinal make the idempotency key: stable across every
	// attempt to publish this item, distinct between items.
	postID  string
	ordinal int
}

// account returns the account a post names, or a classified error.
func (d *Dispatcher) account(name string) (destination.Account, error) {
	if a, ok := d.Accounts[name]; ok && a != nil {
		return a, nil
	}
	return nil, &destination.Error{Class: destination.ClassAuth, Err: fmt.Errorf("%s not configured", name)}
}

// Caps returns the effective capabilities for a target name: the account's
// when one is connected, else the registered driver's defaults (so the
// composer can preview a platform before it is connected). ok=false when the
// name is unknown.
func (d *Dispatcher) Caps(ctx context.Context, name string) (destination.Capabilities, bool) {
	if a, ok := d.Accounts[name]; ok && a != nil {
		return a.Capabilities(ctx), true
	}
	if drv, ok := destination.Lookup(name); ok {
		return drv.Manifest().Defaults, true
	}
	return destination.Capabilities{}, false
}

// displayName is the destination's human name for messages ("Bluesky").
func (d *Dispatcher) displayName(name string) string {
	if a, ok := d.Accounts[name]; ok && a != nil {
		if n := a.Manifest().Name; n != "" {
			return n
		}
	}
	if drv, ok := destination.Lookup(name); ok {
		if n := drv.Manifest().Name; n != "" {
			return n
		}
	}
	return name
}

// publishItem publishes one item to one account and returns the normalized
// result. Nothing is sent when the media can't be made to fit the account.
func (d *Dispatcher) publishItem(ctx context.Context, name string, it item) TargetResult {
	start := time.Now()
	acct, err := d.account(name)
	var res destination.PublishResult
	if err == nil {
		var req destination.PublishRequest
		req, err = d.request(ctx, acct, name, it)
		if err == nil {
			res, err = acct.Publish(ctx, req)
		}
	}
	r := normalize(name, acct, res, err)
	r.LatencyMS = int(time.Since(start).Milliseconds())
	metrics.RecordPublish(name, r.Status, time.Since(start))
	return r
}

// repost natively reposts an external post.
func (d *Dispatcher) repost(ctx context.Context, name string, ref InteractRef) TargetResult {
	start := time.Now()
	acct, err := d.account(name)
	var res destination.PublishResult
	if err == nil {
		rp, ok := acct.(destination.Reposter)
		if !ok {
			err = destination.Invalidf("%s does not support reposts", d.displayName(name))
		} else {
			res, err = rp.Repost(ctx, sourceRef(name, ref))
		}
	}
	r := normalize(name, acct, res, err)
	r.LatencyMS = int(time.Since(start).Milliseconds())
	metrics.RecordPublish(name, r.Status, time.Since(start))
	return r
}

// request builds the PublishRequest for an item against the account's
// capabilities: media fitted, and only the fields the account supports.
func (d *Dispatcher) request(ctx context.Context, acct destination.Account, name string, it item) (destination.PublishRequest, error) {
	m := acct.Manifest()
	caps := acct.Capabilities(ctx)
	media, err := prepareMedia(ctx, m.Name, caps.Media, it.imgs, d.Host)
	if err != nil {
		return destination.PublishRequest{}, err
	}
	ov := it.ov
	req := destination.PublishRequest{Kind: destination.KindNote, Text: it.text, Media: media, InReplyTo: it.thread}
	if caps.ContentWarning {
		req.ContentWarning = firstNonEmpty(ov.SpoilerText, ov.ContentWarning)
	}
	if caps.Sensitive {
		req.Sensitive = ov.Sensitive
	}
	if caps.Languages > 0 {
		langs := ov.Langs
		if len(langs) == 0 && ov.Language != "" {
			langs = []string{ov.Language}
		}
		if len(langs) > caps.Languages {
			langs = langs[:caps.Languages]
		}
		req.Languages = langs
	}
	if len(caps.Visibility) > 0 {
		req.Visibility = ov.Visibility
	}
	req.Options = legacyOptions(m, ov)
	if errs := destination.ValidateOptions(m.Options, req.Options); len(errs) > 0 {
		return req, destination.Invalidf("%s options: %s", m.Name, strings.Join(errs, "; "))
	}
	if caps.LinkPreview == destination.LinkPreviewClient && ov.LinkCard != nil {
		c := ov.LinkCard
		lc := &destination.LinkCard{URL: c.URI, Title: c.Title, Description: c.Description, Thumb: c.ThumbData, ThumbMime: c.ThumbMime}
		for _, r := range c.Refs {
			lc.Records = append(lc.Records, destination.RecordRef{URI: r.URI, CID: r.CID})
		}
		req.LinkCard = lc
	}
	if h := it.head; h != nil {
		switch {
		case h.quote != nil:
			if !caps.Interactions.Quote {
				return req, destination.Invalidf("%s does not support quotes", m.Name)
			}
			src := sourceRef(name, *h.quote)
			req.Quote = &src
		case h.reply != nil:
			if !caps.Interactions.Reply {
				return req, destination.Invalidf("%s does not support replies to other posts", m.Name)
			}
			src := sourceRef(name, *h.reply)
			req.ReplyTo = &src
		}
	}
	if caps.Idempotent == destination.IdempotentKey && it.postID != "" {
		req.IdempotencyKey = idempotencyKey(it.postID, name, it.ordinal)
	}
	return req, nil
}

// legacyOptions maps the composer's per-platform override fields onto
// destination option keys, keeping only the keys the manifest declares. The
// /v1 API (PR 3) takes options directly and retires this.
func legacyOptions(m destination.Manifest, ov Overrides) map[string]any {
	all := map[string]any{}
	if ov.POW != nil {
		all["pow"] = *ov.POW
	}
	if ov.TopicTag != "" {
		all["topic_tag"] = ov.TopicTag
	}
	if ov.ThreadsReplyControl != "" {
		all["reply_control"] = ov.ThreadsReplyControl
	}
	if ov.BlueskyReply != "" {
		all["reply_gate"] = ov.BlueskyReply
	}
	if ov.BlueskyDisableQuotes {
		all["disable_quotes"] = true
	}
	var out map[string]any
	for k, v := range all {
		if _, ok := m.Option(k); !ok {
			continue
		}
		if out == nil {
			out = map[string]any{}
		}
		out[k] = v
	}
	return out
}

// idempotencyKey is the key sent with every attempt at one item. It is
// derived, not random, so a retry after a crash or a lost response repeats
// the same key and the server drops the duplicate.
func idempotencyKey(postID, name string, ordinal int) string {
	sum := sha256.Sum256([]byte("publisher\x00" + postID + "\x00" + name + "\x00" + strconv.Itoa(ordinal)))
	return hex.EncodeToString(sum[:16])
}

// sourceRef converts the composer's resolved source into the contract's.
func sourceRef(name string, r InteractRef) destination.SourceRef {
	return destination.SourceRef{
		Type: name, URI: r.URI, CID: r.CID, ReplyRootURI: r.ReplyRootURI, ReplyRootCID: r.ReplyRootCID,
		LocalID: r.LocalID, EventID: r.EventID, Author: r.Author, RelayHints: r.RelayHints, EventKind: r.Kind,
	}
}

// normalize maps a driver's result and error to a TargetResult. A failure
// always has a message and a class, so a misbehaving driver can't be counted
// as a success.
func normalize(name string, acct destination.Account, res destination.PublishResult, err error) TargetResult {
	r := TargetResult{
		Platform: name, RemoteID: res.Ref.ID, RemoteURL: res.Ref.URL, RefData: res.Ref.Data,
		RequestJSON: res.Trace.Request, ResponseJSON: res.Trace.Response, PendingRelays: res.Pending,
	}
	for _, dl := range res.Deliveries {
		r.Relays = append(r.Relays, store.RelayState{URL: dl.Endpoint, Status: dl.Status, Message: dl.Message})
	}
	if _, ok := acct.(destination.Redeliverer); ok && len(res.Ref.Data) > 0 {
		r.SignedEventJSON = string(res.Ref.Data)
	}
	switch res.Status {
	case destination.StatusPublished:
		r.Status = "success"
	case destination.StatusPartial:
		r.Status = "partial"
	default:
		r.Status = "failed"
	}
	if err == nil && r.Status == "failed" {
		err = errors.New("destination reported a failure without an error")
	}
	if err != nil {
		if r.Status == "success" { // live but something failed: never "success"
			r.Status = "partial"
		}
		r.Error = err.Error()
		r.ErrorClass = string(destination.ClassOf(err))
	}
	return r
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
