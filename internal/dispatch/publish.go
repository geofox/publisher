package dispatch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/geofox/publisher/internal/accounts"
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
	// postID and ordinal name the item; idemKey is its idempotency key
	// (store.AccountItemKey when unset), stable across every attempt.
	postID  string
	ordinal int
	idemKey string
	// prepared is what a Preparer built for the item on an earlier attempt;
	// itemID is the claimed item that a new preparation is persisted to
	// before the first send.
	prepared json.RawMessage
	itemID   int64
}

// errLocked is why an item of a locked account waits instead of failing.
type errLocked struct{ reason string }

func (e errLocked) Error() string {
	if e.reason == "" {
		return "the account is locked"
	}
	return "the account is locked: " + e.reason
}

// acquire leases account id for a send. An account that can't be used is
// a classified failure (auth), except a locked one, whose items wait
// (errLocked).
func (d *Dispatcher) acquire(id, typ string) (destination.Account, func(), error) {
	if d.Registry == nil || id == "" {
		return nil, nil, &destination.Error{Class: destination.ClassAuth, Err: fmt.Errorf("%s not configured", typ)}
	}
	acct, release, err := d.Registry.Acquire(id)
	if err == nil {
		return acct, release, nil
	}
	if u, ok := accounts.IsUnavailable(err); ok {
		if u.Status == accounts.Locked {
			return nil, nil, errLocked{reason: u.Reason}
		}
		return nil, nil, &destination.Error{Class: destination.ClassAuth, Err: u}
	}
	if errors.Is(err, accounts.ErrClosed) {
		return nil, nil, &destination.Error{Class: destination.ClassTransient, Err: err}
	}
	return nil, nil, &destination.Error{Class: destination.ClassAuth, Err: err}
}

// Caps returns account id's capabilities: the open account's, else its
// type's defaults (so recovery and the composer's preview work for an
// account that isn't open).
func (d *Dispatcher) Caps(ctx context.Context, id, typ string) destination.Capabilities {
	if d.Registry != nil && id != "" {
		return d.Registry.Caps(ctx, id, typ)
	}
	if drv, ok := destination.Lookup(typ); ok {
		return drv.Manifest().Defaults
	}
	return destination.Capabilities{}
}

// CanLocate reports whether account id can look its own posts up by URL
// (destination.Locator).
func (d *Dispatcher) CanLocate(id string) bool {
	if d.Registry == nil {
		return false
	}
	acct, release, err := d.Registry.Acquire(id)
	if err != nil {
		return false
	}
	defer release()
	_, ok := acct.(destination.Locator)
	return ok
}

// Locate finds one of account id's own posts by its URL.
func (d *Dispatcher) Locate(ctx context.Context, id, url string) (destination.ItemRef, error) {
	if d.Registry == nil {
		return destination.ItemRef{}, errors.New("dispatch: no accounts")
	}
	acct, release, err := d.Registry.Acquire(id)
	if err != nil {
		return destination.ItemRef{}, err
	}
	defer release()
	l, ok := acct.(destination.Locator)
	if !ok {
		return destination.ItemRef{}, errors.New("dispatch: this account can't look posts up")
	}
	return l.Locate(ctx, url)
}

// displayName names an account in messages: its type's name with its
// handle, or its label when that says more ("Bluesky (@me.bsky.social)"),
// or the type's name alone for an account the registry doesn't know. A
// type without a driver is named by the account's label.
func (d *Dispatcher) displayName(id, typ string) string {
	name, known := typ, false
	if drv, ok := destination.Lookup(typ); ok && drv.Manifest().Name != "" {
		name, known = drv.Manifest().Name, true
	}
	if d.Registry == nil || id == "" {
		return name
	}
	e, ok := d.Registry.Get(id)
	if !ok {
		return name
	}
	label := e.Row.Label
	if !known && label != "" {
		name, label = label, ""
	}
	switch {
	case e.Row.Handle != "":
		return name + " (" + e.Row.Handle + ")"
	case label == "" || strings.EqualFold(label, name): // a row labeled by its type in lower case says nothing more
		return name
	default:
		return name + " (" + label + ")"
	}
}

// publishItem publishes one item from one account and returns the
// normalized result. Nothing is sent when the media can't be made to fit
// the account. A refusal of the credentials asks the registry to check
// them.
func (d *Dispatcher) publishItem(ctx context.Context, id, typ string, it item) TargetResult {
	start := time.Now()
	acct, release, err := d.acquire(id, typ)
	var res destination.PublishResult
	if err == nil {
		defer release()
		var req destination.PublishRequest
		req, err = d.request(ctx, acct, id, typ, it)
		if err == nil {
			req.Prepared, err = d.prepare(ctx, acct, req, it)
		}
		if err == nil {
			res, err = acct.Publish(ctx, req)
		}
		d.reportAuth(id, err)
	}
	return d.finish(typ, acct, res, err, start)
}

// finish normalizes a send's result and records its metrics.
func (d *Dispatcher) finish(typ string, acct destination.Account, res destination.PublishResult, err error, start time.Time) TargetResult {
	r := normalize(typ, acct, res, err)
	r.LatencyMS = int(time.Since(start).Milliseconds())
	var locked errLocked
	if errors.As(err, &locked) {
		r.waiting = true
		return r
	}
	metrics.RecordPublish(typ, r.Status, time.Since(start))
	return r
}

// reportAuth tells the registry when the network refused the account's
// credentials, so an authenticated check can confirm it.
func (d *Dispatcher) reportAuth(id string, err error) {
	if err != nil && d.Registry != nil && destination.ClassOf(err) == destination.ClassAuth {
		d.Registry.ReportAuthFailure(id)
	}
}

// prepare returns the item as a Preparer built it: from an earlier attempt,
// or built now and persisted to the item before anything is sent, so a
// retry (even after a crash) sends the same thing. Nothing for other
// accounts.
func (d *Dispatcher) prepare(ctx context.Context, acct destination.Account, req destination.PublishRequest, it item) (json.RawMessage, error) {
	p, ok := acct.(destination.Preparer)
	if !ok {
		return nil, nil
	}
	if len(it.prepared) > 0 {
		return it.prepared, nil
	}
	prep, err := p.Prepare(ctx, req)
	if err != nil {
		return nil, err
	}
	if it.itemID != 0 && d.Store != nil {
		if err := d.Store.SetItemRef(ctx, it.itemID, prep); err != nil {
			// Unsaved, a retry would build a different item: don't send.
			return nil, &destination.Error{Class: destination.ClassTransient, Err: fmt.Errorf("save prepared item: %w", err)}
		}
	}
	return prep, nil
}

// repost natively reposts an external post from account id.
func (d *Dispatcher) repost(ctx context.Context, id, typ string, ref InteractRef) TargetResult {
	start := time.Now()
	acct, release, err := d.acquire(id, typ)
	var res destination.PublishResult
	if err == nil {
		defer release()
		rp, ok := acct.(destination.Reposter)
		if !ok {
			err = destination.Invalidf("%s does not support reposts", d.displayName(id, typ))
		} else {
			res, err = rp.Repost(ctx, sourceRef(typ, ref))
			d.reportAuth(id, err)
		}
	}
	return d.finish(typ, acct, res, err, start)
}

// request builds the PublishRequest for an item against the account's
// capabilities: media fitted, and only the fields the account supports.
func (d *Dispatcher) request(ctx context.Context, acct destination.Account, id, typ string, it item) (destination.PublishRequest, error) {
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
			src := sourceRef(typ, *h.quote)
			req.Quote = &src
		case h.reply != nil:
			if !caps.Interactions.Reply {
				return req, destination.Invalidf("%s does not support replies to other posts", m.Name)
			}
			src := sourceRef(typ, *h.reply)
			req.ReplyTo = &src
		}
	}
	if caps.Idempotent == destination.IdempotentKey {
		switch {
		case it.idemKey != "":
			req.IdempotencyKey = it.idemKey
		case it.postID != "":
			req.IdempotencyKey = store.AccountItemKey(it.postID, id, it.ordinal)
		}
	}
	return req, nil
}

// legacyOptions maps the composer's per-platform override fields onto
// destination option keys, keeping only the keys the manifest declares. The
// /v1 API (PR 3) takes options directly and retires this.
//
// Like the old adapters, it fails open: an int out of range is clamped (a
// negative PoW meant none) and a value the option doesn't accept is dropped
// (an unknown Threads reply control meant the default), so a stale override
// never fails a post.
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
		spec, ok := m.Option(k)
		if !ok {
			continue
		}
		if n, isInt := v.(int); isInt && spec.Type == destination.OptionInt {
			if spec.Min != nil && n < *spec.Min {
				v = *spec.Min
			}
			if spec.Max != nil && n > *spec.Max {
				v = *spec.Max
			}
		}
		if errs := destination.ValidateOptions([]destination.OptionSpec{spec}, map[string]any{k: v}); len(errs) > 0 {
			continue
		}
		if out == nil {
			out = map[string]any{}
		}
		out[k] = v
	}
	return out
}

// sourceRef converts the composer's resolved source into the contract's.
func sourceRef(name string, r InteractRef) destination.SourceRef {
	return destination.SourceRef{
		Type: name, URI: r.URI, CID: r.CID, ReplyRootURI: r.ReplyRootURI, ReplyRootCID: r.ReplyRootCID,
		LocalID: r.LocalID, EventID: r.EventID, Author: r.Author, RelayHints: r.RelayHints, EventKind: r.Kind,
		ThreadRootID: r.ThreadRootID, Mentions: r.Mentions,
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
		if e := destination.Classify(err); e != nil {
			r.ErrorClass, r.RetryAfter = string(e.Class), e.RetryAfter
		} else {
			r.ErrorClass = string(destination.ClassOf(err))
		}
	}
	return r
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
