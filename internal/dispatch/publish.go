package dispatch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	// postID and ordinal name the item; idemKey is its idempotency key
	// (store.LegacyItemKey when unset), stable across every attempt.
	postID  string
	ordinal int
	idemKey string
	// prepared is what a Preparer built for the item on an earlier attempt;
	// itemID is the claimed item that a new preparation is persisted to
	// before the first send.
	prepared json.RawMessage
	itemID   int64
}

// account returns the account a post names, or a classified error.
func (d *Dispatcher) account(name string) (destination.Account, error) {
	if a, ok := d.Accounts[name]; ok && a != nil {
		return a, nil
	}
	return nil, &destination.Error{Class: destination.ClassAuth, Err: fmt.Errorf("%s not configured", name)}
}

// SendsAs reports whether the connected account of type typ is the stored
// account id, so a post naming id is sent with that account's credentials.
// True when the dispatcher wasn't told the ids.
func (d *Dispatcher) SendsAs(typ, id string) bool {
	if d.AccountIDs == nil || id == "" {
		return true
	}
	connected, ok := d.AccountIDs[typ]
	// No connected account of the type: d.account answers "not configured".
	return !ok || connected == id
}

// errOtherAccount fails a target whose account isn't the connected one.
func errOtherAccount(typ string) *destination.Error {
	return &destination.Error{Class: destination.ClassAuth,
		Err: fmt.Errorf("this %s account isn't the one connected on this server", typ)}
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

// CanLocate reports whether the named account can look its own posts up
// by URL (destination.Locator).
func (d *Dispatcher) CanLocate(name string) bool {
	_, ok := d.Accounts[name].(destination.Locator)
	return ok
}

// Locate finds one of the named account's own posts by its URL.
func (d *Dispatcher) Locate(ctx context.Context, name, url string) (destination.ItemRef, error) {
	l, ok := d.Accounts[name].(destination.Locator)
	if !ok {
		return destination.ItemRef{}, errors.New("dispatch: " + name + " can't look posts up")
	}
	return l.Locate(ctx, url)
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
			req.Prepared, err = d.prepare(ctx, acct, req, it)
		}
		if err == nil {
			res, err = acct.Publish(ctx, req)
		}
	}
	r := normalize(name, acct, res, err)
	r.LatencyMS = int(time.Since(start).Milliseconds())
	metrics.RecordPublish(name, r.Status, time.Since(start))
	return r
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
	if caps.Idempotent == destination.IdempotentKey {
		switch {
		case it.idemKey != "":
			req.IdempotencyKey = it.idemKey
		case it.postID != "":
			req.IdempotencyKey = store.LegacyItemKey(it.postID, name, it.ordinal)
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

// ConnectedIDs maps each type to the stored account its connected account
// sends as, for Dispatcher.AccountIDs, from store.SeedEnvAccounts' answer.
func ConnectedIDs(seeded map[string]store.Account) map[string]string {
	out := make(map[string]string, len(seeded))
	for typ, a := range seeded {
		out[typ] = a.ID
	}
	return out
}
