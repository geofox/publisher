package api

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/geofox/publisher/internal/dispatch"
	"github.com/geofox/publisher/internal/resolve"
	"github.com/geofox/publisher/internal/store"
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

// resolveSource resolves a URL for an action; problems are field errors at
// ptr.
func (a *API) resolveSource(ctx context.Context, url, ptr string, add func(ptr, code, detail string)) *resolve.SourceRef {
	if strings.TrimSpace(url) == "" {
		add(ptr, "required", "the post's URL (or a Nostr nevent/note id)")
		return nil
	}
	if a.Resolve == nil {
		add(ptr, "unresolvable", "resolving posts is not configured on this server")
		return nil
	}
	rctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	src, err := a.Resolve.Resolve(rctx, url)
	if err != nil {
		slog.InfoContext(ctx, "v1: resolve", "url", url, "err", err)
		add(ptr, "unresolvable", store.Scrub(err.Error()))
		return nil
	}
	return src
}

// planInteraction checks an interaction against the accounts and content
// planCreate has validated, and sets plan.ix. Every target but the
// source's account is a link-quote, so only a quote may have others.
func (a *API) planInteraction(ctx context.Context, req *v1CreatePostReq, plan *createPlan, add func(ptr, code, detail string)) {
	action, in, n := req.interaction()
	ptr := map[string]string{"reply": "/reply_to", "quote": "/quote", "repost": "/repost"}[action]
	if n > 1 {
		add("", "conflict", "set one of reply_to, quote and repost")
		return
	}
	if req.Schedule != nil {
		add("/schedule", "not_supported", "replies, quotes and reposts are sent now; leave schedule out")
	}
	if action == "repost" {
		if req.Content.Text != nil && *req.Content.Text != "" || len(req.Content.Media) > 0 {
			add("/content", "not_allowed", "a repost carries no text or media; use a quote to add some")
		}
		for _, pt := range plan.targets {
			if pt.ov.Text != "" {
				add(pt.reqPtr+"/content/text", "not_allowed", "a repost carries no text; use a quote to add some")
			}
		}
	}
	src := a.resolveSource(ctx, in.URL, ptr+"/url", add)
	if src == nil {
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
	var fanout []string
	for i, pt := range plan.targets {
		if pt.account.Type == src.Platform {
			native = i
			continue
		}
		if action != "quote" {
			add(pt.reqPtr+"/account", "not_allowed", "a "+action+" goes only to the source's network ("+src.Platform+
				"); use a quote to share it elsewhere")
			continue
		}
		fanout = append(fanout, pt.account.Type)
	}
	if native < 0 {
		add("/targets", "missing_source_account", "include your "+src.Platform+" account: the "+action+" happens there")
		return
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
	ix := &dispatch.InteractSpec{
		Action: action, SourcePlatform: src.Platform, SourceURL: webURL, SourceAuthor: author,
		Ref: dispatch.InteractRef{URI: r.URI, CID: r.CID, ReplyRootURI: r.ReplyRootURI, ReplyRootCID: r.ReplyRootCID,
			LocalID: r.LocalID, EventID: r.EventID, Author: r.Author, RelayHints: r.RelayHints, Kind: r.Kind},
		Text: plan.spec.MasterText, Overrides: plan.spec.Overrides, Fanout: fanout, Number: plan.spec.Number,
		MediaRecords:  plan.spec.MediaRecords,
		SourcePreview: dispatch.SourcePreview{Author: author, Text: src.Preview.Text},
		Source:        "api", Accounts: plan.spec.Accounts,
	}
	plan.ix, plan.source = ix, src
	// Each target's text is what it will post.
	for i := range plan.targets {
		pt := &plan.targets[i]
		switch {
		case action == "repost":
			pt.text = ""
		case i == native:
			pt.text = dispatch.InteractText(*ix, pt.account.Type)
		default:
			pt.text = dispatch.ReproductionText(*ix, pt.account.Type)
		}
	}
}

// rehostSourceMedia copies a quoted post's images to media storage, for
// its link-quotes. Best-effort: an image that won't fetch is left out,
// and videos are (they would be too large to copy here).
func (a *API) rehostSourceMedia(ctx context.Context, ix *dispatch.InteractSpec, src *resolve.SourceRef) {
	if a.media == nil || len(ix.Fanout) == 0 {
		return
	}
	for i, m := range src.Preview.Media {
		if i >= maxImagesPerRequest {
			break
		}
		body, mime, err := a.fetchSourceMedia(ctx, m.URL)
		if err != nil {
			slog.InfoContext(ctx, "v1: source media not copied", "url", m.URL, "err", err)
			continue
		}
		mt := sniffType(mime, body[:min(len(body), 512)])
		if !uploadImageTypes[mt] {
			continue
		}
		res, err := a.media.Process(ctx, body, mt)
		if err != nil {
			slog.InfoContext(ctx, "v1: source media not stored", "url", m.URL, "err", err)
			continue
		}
		ix.SourceMediaRecords = append(ix.SourceMediaRecords, store.Media{Ordinal: len(ix.SourceMediaRecords),
			BlossomURL: res.URL, SHA256: res.SHA256, Mime: res.Mime, Dim: res.Dim, Blurhash: res.Blurhash,
			SizeBytes: res.Size, Alt: m.Alt})
	}
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
	URL string `json:"url"`
	Alt string `json:"alt"`
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
	src := a.resolveSource(r.Context(), req.URL, "/url", add)
	if src == nil {
		invalid(w, r, errs)
		return
	}
	pr, _ := principalFrom(r.Context())
	accts, err := a.Store.Accounts(r.Context(), pr.OwnerID)
	if err != nil {
		slog.ErrorContext(r.Context(), "v1: resolve accounts", "err", err)
		problem(w, r, http.StatusInternalServerError, codeInternal, "reading the accounts failed")
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
		out.Media = append(out.Media, v1ResolveMedia{URL: m.URL, Alt: m.Alt})
	}
	for _, ac := range accts {
		if ac.Enabled && ac.Type == src.Platform {
			id := ac.ID
			out.Account = &id
			break
		}
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
