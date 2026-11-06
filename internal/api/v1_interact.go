package api

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	neturl "net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/geofox/publisher/internal/accounts"
	"github.com/geofox/publisher/internal/destination"
	"github.com/geofox/publisher/internal/dispatch"
	"github.com/geofox/publisher/internal/resolve"
	"github.com/geofox/publisher/internal/store"
	"github.com/geofox/publisher/internal/thread"
)

// Replies, quotes and reposts. A client names the source post by its URL
// (or Nostr id); the server resolves it, so platform-native ids never pass
// through clients. The action happens on the account of the source's
// network; a quote can also go to other accounts as a link-quote: the
// commentary, the original's text with its author, and its URL.

// v1SourceIn names a post to act on.
type v1SourceIn struct {
	URL string `json:"url"`
}

// interaction returns the request's action and source, if any, and how
// many of reply_to, quote and repost it sets.
func (req *v1CreatePostReq) interaction() (action string, src *v1SourceIn, n int) {
	for _, c := range []struct {
		action string
		src    *v1SourceIn
	}{{"reply", req.ReplyTo}, {"quote", req.Quote}, {"repost", req.Repost}} {
		if c.src != nil {
			action, src, n = c.action, c.src, n+1
		}
	}
	return action, src, n
}

// maxSourceURL bounds a source URL (or Nostr id).
const maxSourceURL = 2048

// lookUp resolves a pasted post with one of the owner's connected accounts
// on its network: prefer when it is one (the account that will act on
// the post), else the first in registry order that can look posts up. It
// returns the account used.
func (a *API) lookUp(ctx context.Context, input, prefer string) (*resolve.SourceRef, string, error) {
	typ := resolve.Detect(input)
	if typ == "" {
		return nil, "", resolve.ErrUnrecognized
	}
	if a.Accounts == nil {
		return nil, "", resolve.ErrNotConfigured
	}
	p, _ := principalFrom(ctx)
	var ids []string
	for _, e := range a.Accounts.List(p.OwnerID) {
		if e.Row.Type != typ || e.Status != accounts.Connected {
			continue
		}
		if e.Row.ID == prefer {
			ids = append([]string{e.Row.ID}, ids...)
		} else {
			ids = append(ids, e.Row.ID)
		}
	}
	for _, id := range ids {
		acct, release, err := a.Accounts.Acquire(id)
		if err != nil {
			continue
		}
		r, ok := acct.(destination.SourceResolver)
		if !ok {
			release()
			return nil, "", resolve.ErrUnsupported
		}
		rs, err := r.ResolveSource(ctx, strings.TrimSpace(input))
		release()
		if err != nil {
			return nil, id, err
		}
		return resolve.FromResolved(rs), id, nil
	}
	return nil, "", resolve.ErrNotConfigured
}

// resolveSource resolves a URL for an action, with prefer's account when
// it is on the post's network; problems are field errors at ptr. upstream
// is set instead when the post's network couldn't be reached (a timeout, a
// dropped connection, a 5xx or rate limit): the request was fine and may
// be retried. Details stay in the log; clients get fixed text.
func (a *API) resolveSource(ctx context.Context, url, prefer, ptr string, add func(ptr, code, detail string)) (src *resolve.SourceRef, account string, upstream bool) {
	switch {
	case strings.TrimSpace(url) == "":
		add(ptr, "required", "the post's URL (or a Nostr nevent/note id)")
		return nil, "", false
	case len(url) > maxSourceURL:
		add(ptr, "too_long", "at most 2048 characters")
		return nil, "", false
	}
	rctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	src, account, err := a.lookUp(rctx, url, prefer)
	if err == nil {
		return src, account, false
	}
	slog.InfoContext(ctx, "v1: resolve", "url", url, "account", account, "err", store.Scrub(err.Error()))
	var he *destination.HTTPError
	var ne net.Error
	switch {
	case errors.Is(err, resolve.ErrUnrecognized):
		add(ptr, "unrecognized", "not a Bluesky or Mastodon post URL, or a Nostr nevent/note id")
	case errors.Is(err, resolve.ErrUnsupported):
		add(ptr, "not_supported", displayName(resolve.Detect(url))+" posts can't be looked up (its API has no URL lookup)")
	case errors.Is(err, resolve.ErrNotConfigured):
		add(ptr, "not_configured", "none of your accounts on that post's network is connected")
	case errors.Is(err, context.DeadlineExceeded), errors.As(err, &ne),
		errors.As(err, &he) && (he.StatusCode >= 500 || he.StatusCode == http.StatusTooManyRequests):
		return nil, account, true
	default:
		add(ptr, "unresolvable", "the post couldn't be found or read: it may be deleted, private, "+
			"or not yet known to your server; the server log has details")
	}
	return nil, account, false
}

// checkInteraction runs the checks on an interaction that need no network,
// so they are reported with the request's other problems.
func (a *API) checkInteraction(req *v1CreatePostReq, plan *createPlan, add func(ptr, code, detail string)) {
	action, _, n := req.interaction()
	if n > 1 {
		add("", "conflict", "set one of reply_to, quote and repost")
		return
	}
	if req.Schedule != nil {
		add("/schedule", "not_supported", "replies, quotes and reposts are sent now; leave schedule out")
	}
	if action != "repost" {
		return
	}
	// A repost is the original, shared: nothing of ours goes with it.
	c := req.Content
	if c.Text != nil && *c.Text != "" || len(c.Media) > 0 {
		add("/content", "not_allowed", "a repost carries no text or media; use a quote to add some")
	}
	if c.ContentWarning != nil && *c.ContentWarning != "" || c.Sensitive != nil && *c.Sensitive || len(c.Languages) > 0 {
		add("/content", "not_allowed", "a repost carries no content warning, sensitivity or languages")
	}
	for i, t := range req.Targets {
		ptr := "/targets/" + strconv.Itoa(i)
		if t.Content != nil {
			add(ptr+"/content", "not_allowed", "a repost carries no content of its own; use a quote to add some")
		}
		for _, v := range t.Options {
			if v != nil {
				add(ptr+"/options", "not_allowed", "a repost takes no options")
				break
			}
		}
	}
}

// planInteraction resolves the source and checks the interaction against
// the accounts and content planCreate has validated, and sets plan.ix.
// Every target but the source's account is a link-quote, so only a quote
// may have others.
func (a *API) planInteraction(ctx context.Context, req *v1CreatePostReq, plan *createPlan, add func(ptr, code, detail string)) {
	action, in, _ := req.interaction()
	ptr := map[string]string{"reply": "/reply_to", "quote": "/quote", "repost": "/repost"}[action]
	// The account that will act on the post looks it up: an id (a
	// Mastodon status id) is only valid on its own server.
	prefer := ""
	for _, pt := range plan.targets {
		if pt.account.Type == resolve.Detect(in.URL) {
			prefer = pt.account.ID
		}
	}
	src, _, upstream := a.resolveSource(ctx, in.URL, prefer, ptr+"/url", add)
	if src == nil {
		plan.upstream = upstream
		return
	}
	var allowed resolve.Cap
	switch action {
	case "reply":
		allowed = src.Caps.Reply
	case "quote":
		allowed = src.Caps.Quote
	case "repost":
		allowed = src.Caps.Repost
	}
	if !allowed.Allowed {
		reason := allowed.Reason
		if reason == "" {
			reason = "this post can't be " + map[string]string{"reply": "replied to", "quote": "quoted", "repost": "reposted"}[action]
		}
		add(ptr+"/url", "not_allowed", reason)
	}
	native := -1
	for i, pt := range plan.targets {
		if pt.account.Type == src.Platform {
			native = i
			continue
		}
		if action != "quote" {
			add(pt.reqPtr+"/account", "not_allowed", "a "+action+" goes only to the source's network ("+src.Platform+
				"); use a quote to share it elsewhere")
		}
	}
	if native < 0 {
		add("/targets", "missing_source_account", "include your "+src.Platform+" account: the "+action+" happens there")
		return
	}
	if nt := plan.targets[native]; action == "quote" && !nt.caps.Interactions.QuoteVideo {
		for j, m := range plan.media {
			if strings.HasPrefix(m.Mime, "video/") {
				add("/content/media/"+strconv.Itoa(plan.mediaReq[j]), "media_unsupported",
					"a quote on "+displayName(nt.account.Type)+" can't carry a video")
			}
		}
	}
	author := src.Preview.AuthorHandle
	if author == "" {
		author = src.Preview.AuthorName
	}
	webURL := src.Preview.WebURL
	if webURL == "" {
		webURL = in.URL
	}
	r := src.Ref
	targets := make([]dispatch.TargetSpec, len(plan.targets))
	for i, pt := range plan.targets {
		targets[i] = dispatch.TargetSpec{AccountID: pt.account.ID, Type: pt.account.Type, Overrides: pt.ov, Layout: pt.layout}
		if pt.own {
			targets[i].Text = pt.text
		}
	}
	ix := &dispatch.InteractSpec{
		Action: action, SourcePlatform: src.Platform, SourceAccount: plan.targets[native].account.ID,
		SourceURL: webURL, SourceAuthor: author,
		Ref: dispatch.InteractRef{URI: r.URI, CID: r.CID, ReplyRootURI: r.ReplyRootURI, ReplyRootCID: r.ReplyRootCID,
			LocalID: r.LocalID, EventID: r.EventID, Author: r.Author, RelayHints: r.RelayHints, Kind: r.Kind,
			ThreadRootID: r.ThreadRootID, Mentions: r.Mentions},
		Text: plan.spec.MasterText, Targets: targets, Number: plan.spec.Number,
		MediaRecords:  plan.spec.MediaRecords,
		SourcePreview: dispatch.SourcePreview{Author: author, Text: src.Preview.Text},
		Source:        "api",
	}
	plan.ix, plan.source = ix, src
	if allowed.Allowed && allowed.Reason != "" {
		plan.targets[native].warnings = append(plan.targets[native].warnings, allowed.Reason)
	}
	// Each target's text is what it will post.
	for i := range plan.targets {
		pt := &plan.targets[i]
		switch {
		case action == "repost":
			pt.text = ""
		case i == native:
			pt.text = dispatch.InteractText(*ix, targets[i])
		default:
			// A link-quote copy is split automatically, every media on
			// its first post.
			pt.text = dispatch.ReproductionText(*ix, targets[i])
			pt.layout = thread.Layout{MediaAt: make([]int, len(plan.media))}
		}
	}
}

// Copying a quoted post's images for its link-quotes: at most
// maxSourceImages (every network takes four per post), each at most
// maxSourceImageBytes, all within sourceMediaBudget.
const (
	maxSourceImages           = 4
	maxSourceImageBytes int64 = 20 << 20
	sourceMediaBudget         = 30 * time.Second
)

// sourceImagesWanted is how many of the original's images the link-quotes
// will attach: the engine adds them after the user's media, up to each
// account's image cap, so the most any link-quote takes.
func sourceImagesWanted(plan *createPlan) int {
	n := 0
	for _, m := range plan.source.Preview.Media {
		if !m.Video {
			n++
		}
	}
	room := 0
	for _, pt := range plan.targets {
		if pt.account.ID == plan.ix.SourceAccount || !pt.caps.Media.Images.Supported {
			continue
		}
		r := maxSourceImages
		if c := pt.caps.Media.Images.MaxCount; c > 0 {
			r = min(r, c-len(plan.media))
		}
		room = max(room, r)
	}
	return max(0, min(n, room))
}

// rehostSourceMedia copies a quoted post's images to media storage, for
// its link-quotes, and returns how many it copied. Best-effort: an image
// that won't fetch is left out, and videos are (they would be too large
// to copy here).
func (a *API) rehostSourceMedia(ctx context.Context, plan *createPlan) int {
	ix, want := plan.ix, sourceImagesWanted(plan)
	if a.media == nil || len(ix.Targets) < 2 || want == 0 {
		return 0
	}
	ctx, cancel := context.WithTimeout(ctx, sourceMediaBudget)
	defer cancel()
	type got struct {
		rec store.Media
		ok  bool
	}
	var urls []resolve.Media
	for _, m := range plan.source.Preview.Media {
		if !m.Video && len(urls) < want {
			urls = append(urls, m)
		}
	}
	res := make([]got, len(urls))
	var wg sync.WaitGroup
	for i, m := range urls {
		wg.Add(1)
		go func() {
			defer wg.Done()
			fetch := a.fetchMedia
			if fetch == nil {
				fetch = a.fetchSourceImage
			}
			body, mime, err := fetch(ctx, m.URL)
			if err != nil {
				slog.InfoContext(ctx, "v1: source media not copied", "url", m.URL, "err", err)
				return
			}
			mt := sniffType(mime, body[:min(len(body), 512)])
			if !uploadImageTypes[mt] {
				return
			}
			r, err := a.media.Process(ctx, body, mt)
			if err != nil {
				slog.InfoContext(ctx, "v1: source media not stored", "url", m.URL, "err", err)
				return
			}
			res[i] = got{ok: true, rec: store.Media{BlossomURL: r.URL, SHA256: r.SHA256, Mime: r.Mime, Dim: r.Dim,
				Blurhash: r.Blurhash, SizeBytes: r.Size, Alt: m.Alt}}
		}()
	}
	wg.Wait()
	for _, g := range res {
		if g.ok {
			g.rec.Ordinal = len(ix.SourceMediaRecords)
			ix.SourceMediaRecords = append(ix.SourceMediaRecords, g.rec)
		}
	}
	return len(ix.SourceMediaRecords)
}

// fetchSourceImage downloads one of a quoted post's images (https only,
// SSRF-guarded, at most maxSourceImageBytes). A response that says it is
// not an image, or is too large, is refused before its body is read.
func (a *API) fetchSourceImage(ctx context.Context, rawURL string) ([]byte, string, error) {
	u, err := neturl.Parse(rawURL)
	if err != nil || u.Scheme != "https" {
		return nil, "", fmt.Errorf("bad media url")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, "", err
	}
	resp, err := sourceMediaClient.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	ct := resp.Header.Get("Content-Type")
	switch {
	case resp.StatusCode < 200 || resp.StatusCode >= 300:
		return nil, "", fmt.Errorf("media fetch %s", resp.Status)
	case ct != "" && !strings.HasPrefix(strings.ToLower(ct), "image/") && !strings.HasPrefix(strings.ToLower(ct), "application/octet-stream"):
		return nil, "", fmt.Errorf("not an image: %s", ct)
	case resp.ContentLength > maxSourceImageBytes:
		return nil, "", fmt.Errorf("image too large: %d bytes", resp.ContentLength)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxSourceImageBytes+1))
	if err != nil {
		return nil, "", err
	}
	if int64(len(body)) > maxSourceImageBytes {
		return nil, "", fmt.Errorf("image too large")
	}
	return body, ct, nil
}

// interactionEnqueuer saves an interaction for the workers
// (*dispatch.Dispatcher).
type interactionEnqueuer interface {
	EnqueueInteraction(ctx context.Context, spec dispatch.InteractSpec) (*store.Post, error)
}

// ─── POST /v1/resolve ────────────────────────────────────────────────────

type v1ResolveBody struct {
	Platform  string           `json:"platform"`
	URL       string           `json:"url"`
	Author    v1ResolveAuthor  `json:"author"`
	Text      string           `json:"text"`
	Media     []v1ResolveMedia `json:"media"`
	CreatedAt *string          `json:"created_at"`
	Account   *string          `json:"account"`
	Actions   v1ResolveActions `json:"actions"`
}

type v1ResolveAuthor struct {
	Name   string `json:"name"`
	Handle string `json:"handle"`
}

type v1ResolveMedia struct {
	URL  string `json:"url"`
	Alt  string `json:"alt"`
	Type string `json:"type"` // image or video
}

type v1ResolveActions struct {
	Reply  v1ResolveAction `json:"reply"`
	Quote  v1ResolveAction `json:"quote"`
	Repost v1ResolveAction `json:"repost"`
}

type v1ResolveAction struct {
	Allowed bool   `json:"allowed"`
	Reason  string `json:"reason,omitempty"`
}

func (a *API) v1Resolve(w http.ResponseWriter, r *http.Request) {
	var req v1SourceIn
	if !decodeJSON(w, r, &req) {
		return
	}
	if a.Store == nil {
		problem(w, r, http.StatusServiceUnavailable, codeUnavailable, "no database")
		return
	}
	var errs []fieldError
	add := func(ptr, code, detail string) {
		errs = append(errs, fieldError{Pointer: ptr, Code: code, Detail: detail})
	}
	src, account, upstream := a.resolveSource(r.Context(), req.URL, "", "/url", add)
	if upstream {
		problem(w, r, http.StatusBadGateway, codeUpstream, "the post's network could not be reached; try again")
		return
	}
	if src == nil {
		invalid(w, r, errs)
		return
	}
	out := v1ResolveBody{Platform: src.Platform, URL: src.Preview.WebURL, Text: src.Preview.Text,
		Author: v1ResolveAuthor{Name: src.Preview.AuthorName, Handle: src.Preview.AuthorHandle}, Media: []v1ResolveMedia{}}
	if out.URL == "" {
		out.URL = req.URL
	}
	if !src.Preview.CreatedAt.IsZero() {
		out.CreatedAt = rfc3339p(src.Preview.CreatedAt)
	}
	for _, m := range src.Preview.Media {
		kind := "image"
		if m.Video {
			kind = "video"
		}
		out.Media = append(out.Media, v1ResolveMedia{URL: m.URL, Alt: m.Alt, Type: kind})
	}
	// The account that looked the post up is connected, so a post naming
	// it is accepted.
	if account != "" {
		out.Account = &account
	}
	act := func(c resolve.Cap) v1ResolveAction {
		if out.Account == nil {
			return v1ResolveAction{Reason: "no enabled " + src.Platform + " account"}
		}
		return v1ResolveAction{Allowed: c.Allowed, Reason: c.Reason}
	}
	out.Actions = v1ResolveActions{Reply: act(src.Caps.Reply), Quote: act(src.Caps.Quote), Repost: act(src.Caps.Repost)}
	writeV1(w, http.StatusOK, out)
}
