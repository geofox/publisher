package api

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/geofox/publisher/internal/destination"
	"github.com/geofox/publisher/internal/dispatch"
	"github.com/geofox/publisher/internal/store"
	"github.com/geofox/publisher/internal/translate"
)

// /v1/drafts: posts in the making. A draft holds a POST /v1/posts body (as
// it stands: nothing is required yet), with a title and tags. Each write
// bumps the draft's version, its ETag; a write with If-Match on a stale
// version is refused (412), so two tabs can't silently overwrite each
// other. POST /v1/posts with "draft" uses the draft up.

const (
	prefixDraft   = "drf_"
	maxDraftTitle = 200
	maxDraftTags  = 20
)

func draftAPIID(storeID string) string { return prefixDraft + storeID }

type v1DraftIn struct {
	Title *string          `json:"title"`
	Tags  *[]string        `json:"tags"`
	Post  *v1CreatePostReq `json:"post"`
}

type v1DraftBody struct {
	ID    string          `json:"id"`
	Title string          `json:"title"`
	Tags  []string        `json:"tags"`
	Post  json.RawMessage `json:"post"`
	// Media are the post's media objects, in its order, so a client shows
	// them without reading each.
	Media     []v1MediaObjectBody `json:"media"`
	CreatedAt string              `json:"created_at"`
	UpdatedAt string              `json:"updated_at"`
}

type v1DraftListItem struct {
	ID        string   `json:"id"`
	Title     string   `json:"title"`
	Preview   string   `json:"preview"`
	Tags      []string `json:"tags"`
	MediaURL  *string  `json:"media_url"`
	CreatedAt string   `json:"created_at"`
	UpdatedAt string   `json:"updated_at"`
}

func draftETag(version int64) string { return `"` + strconv.FormatInt(version, 10) + `"` }

// ifMatch reads If-Match: 0 when absent or "*" (any version), the version
// it names, or ok=false for a value this server never sent (a weak tag or
// a list included).
func ifMatch(r *http.Request) (version int64, ok bool) {
	v := strings.TrimSpace(r.Header.Get("If-Match"))
	if v == "" || v == "*" {
		return 0, true
	}
	// If-Match compares strongly (RFC 9110 §13.1.1): a weak tag never matches.
	n, err := strconv.ParseInt(strings.Trim(v, `"`), 10, 64)
	if err != nil || n < 1 || !strings.HasPrefix(v, `"`) || !strings.HasSuffix(v, `"`) {
		return 0, false
	}
	return n, true
}

// draftTitle is the draft's title: the one given, else the text's first
// line.
func draftTitle(title, text string) string {
	if t := strings.TrimSpace(title); t != "" {
		return t
	}
	return store.DeriveTitle(text)
}

func postText(p *v1CreatePostReq) string {
	if p == nil || p.Content.Text == nil {
		return ""
	}
	return *p.Content.Text
}

// checkDraft validates a draft write. A draft is a post being written:
// nothing is required, but what is there must be a post's fields within a
// post's bounds, naming the owner's media and accounts.
func (a *API) checkDraft(ctx context.Context, owner string, in *v1DraftIn) ([]fieldError, error) {
	var errs []fieldError
	add := func(ptr, code, detail string) {
		errs = append(errs, fieldError{Pointer: ptr, Code: code, Detail: detail})
	}
	if in.Title != nil && utf8.RuneCountInString(*in.Title) > maxDraftTitle {
		add("/title", "too_long", "at most 200 characters")
	}
	if in.Tags != nil {
		if len(*in.Tags) > maxDraftTags {
			add("/tags", "too_many", "at most 20 tags")
		}
		for i, t := range *in.Tags {
			if utf8.RuneCountInString(t) > 64 {
				add("/tags/"+strconv.Itoa(i), "too_long", "at most 64 characters (tags are cut to 32)")
			}
		}
	}
	p := in.Post
	if p == nil {
		return errs, nil
	}
	checkText := func(ptr string, s *string) {
		if s != nil && utf8.RuneCountInString(*s) > maxPostTextRunes {
			add(ptr, "too_long", "at most 50000 characters")
		}
	}
	checkCW := func(ptr string, s *string) {
		if s != nil && utf8.RuneCountInString(*s) > maxCWRunes {
			add(ptr, "too_long", "at most 1000 characters")
		}
	}
	checkLangs := func(ptr string, langs []string) {
		if len(langs) > maxPostLanguages {
			add(ptr, "too_many", "at most 10 languages")
			return
		}
		for i, l := range langs {
			if !langTag.MatchString(l) {
				add(ptr+"/"+strconv.Itoa(i), "invalid_language", "a language tag such as en or pt-BR")
			}
		}
	}
	checkText("/post/content/text", p.Content.Text)
	sharedText := ""
	if p.Content.Text != nil {
		sharedText = *p.Content.Text
	}
	checkBreaks(sharedText, p.Content.Breaks, "/post/content/breaks", add)
	checkCW("/post/content/content_warning", p.Content.ContentWarning)
	checkLangs("/post/content/languages", p.Content.Languages)
	if p.Draft != nil {
		add("/post/draft", "not_allowed", "a draft's post can't name a draft")
	}
	if len(p.Content.Media) > maxImagesPerRequest {
		add("/post/content/media", "too_many", "at most 40 media")
	} else {
		seen := map[string]bool{}
		for j, cm := range p.Content.Media {
			ptr := "/post/content/media/" + strconv.Itoa(j)
			if seen[cm.ID] {
				add(ptr+"/id", "duplicate", "this media is already in the post")
				continue
			}
			seen[cm.ID] = true
			if _, err := a.Store.MediaObject(ctx, owner, cm.ID); errors.Is(err, store.ErrNoMedia) || !strings.HasPrefix(cm.ID, prefixMedia) {
				add(ptr+"/id", "unknown_media", "no media "+cm.ID+"; upload it with POST /v1/media")
			} else if err != nil {
				return nil, err
			}
			if cm.Alt != nil {
				if msg := altProblem(*cm.Alt); msg != "" {
					add(ptr+"/alt", "invalid", msg)
				}
			}
			if cm.Part != nil {
				add(ptr+"/part", "replaced", "parts are gone: place the media with at, a position in the text")
			}
			if cm.At != nil {
				checkAt(utf8.RuneCountInString(sharedText), *cm.At, ptr+"/at", add)
			}
		}
	}
	if len(p.Targets) > maxPostTargets {
		add("/post/targets", "too_many", "at most 20 accounts")
	} else {
		accts, err := a.Store.Accounts(ctx, owner)
		if err != nil {
			return nil, err
		}
		known := map[string]store.Account{}
		for _, ac := range accts {
			known[ac.ID] = ac
		}
		seen := map[string]bool{}
		for i, t := range p.Targets {
			ptr := "/post/targets/" + strconv.Itoa(i)
			ac, ok := known[t.Account]
			switch {
			case !ok:
				add(ptr+"/account", "unknown_account", "no account "+t.Account+"; list them with GET /v1/accounts")
			case seen[t.Account]:
				add(ptr+"/account", "duplicate", "this account is already a target")
			}
			seen[t.Account] = true
			// Options as the post checks them, against the account's
			// destination; without one, only their shape.
			if drv, registered := destination.Lookup(ac.Type); ok && registered {
				a.planOptions(drv.Manifest(), a.caps(ctx, ac.ID, ac.Type), t.Options, ptr+"/options", &dispatch.Overrides{}, add)
			} else {
				for k, v := range t.Options {
					op := ptr + "/options/" + jsonPointerToken(k)
					switch val := v.(type) {
					case nil, bool, float64:
					case string:
						if utf8.RuneCountInString(val) > maxOptionRunes {
							add(op, "invalid_option", "at most 500 characters")
						}
					default:
						add(op, "invalid_option", "a string, number, boolean or null")
					}
				}
			}
			if c := t.Content; c != nil {
				checkText(ptr+"/content/text", c.Text)
				if c.Text != nil && strings.TrimSpace(*c.Text) == "" {
					add(ptr+"/content/text", "empty", "leave text out to use the shared text")
				}
				checkCW(ptr+"/content/content_warning", c.ContentWarning)
				checkLangs(ptr+"/content/languages", c.Languages)
			}
			if th := t.Thread; th != nil {
				text := sharedText
				if t.Content != nil && t.Content.Text != nil {
					text = *t.Content.Text
				}
				if th.Breaks != nil {
					checkBreaks(text, *th.Breaks, ptr+"/thread/breaks", add)
				}
				if th.MediaAt != nil && len(th.MediaAt) != len(p.Content.Media) {
					add(ptr+"/thread/media_at", "length_mismatch", "one position per media in content.media")
				} else if len(th.MediaAt) > 0 {
					n := utf8.RuneCountInString(text)
					for k, at := range th.MediaAt {
						checkAt(n, at, ptr+"/thread/media_at/"+strconv.Itoa(k), add)
					}
				}
			}
		}
	}
	if _, src, n := p.interaction(); n > 1 {
		add("/post", "conflict", "set one of reply_to, quote and repost")
	} else if src != nil && len(src.URL) > maxSourceURL {
		add("/post", "too_long", "the source URL is at most 2048 characters")
	}
	if s := p.Schedule; s != nil && s.At == nil {
		add("/post/schedule/at", "required", "a time; leave schedule out for none")
	} else if s != nil {
		if _, err := time.Parse(time.RFC3339, strings.ToUpper(*s.At)); err != nil {
			add("/post/schedule/at", "invalid_time", "an RFC 3339 time, such as 2027-01-01T09:00:00Z")
		}
	}
	return errs, nil
}

// draftOutBody renders a draft, with its media objects.
func (a *API) draftOutBody(ctx context.Context, d *store.V1Draft) v1DraftBody {
	out := v1DraftBody{ID: draftAPIID(d.ID), Title: d.Title, Tags: d.Tags, Post: json.RawMessage(d.Body),
		Media: []v1MediaObjectBody{}, CreatedAt: rfc3339(d.CreatedAt), UpdatedAt: rfc3339(d.UpdatedAt)}
	var p v1CreatePostReq
	_ = json.Unmarshal([]byte(d.Body), &p)
	for _, cm := range p.Content.Media {
		m, err := a.Store.MediaObject(ctx, d.OwnerID, cm.ID)
		if err != nil {
			continue // gone: the post would refuse it, and say so
		}
		if err := a.refreshMedia(ctx, m); err != nil {
			slog.WarnContext(ctx, "v1: draft media", "media_id", m.ID, "err", err)
		}
		out.Media = append(out.Media, a.v1MediaBody(m))
	}
	return out
}

func (a *API) writeDraftV1(w http.ResponseWriter, r *http.Request, status int, d *store.V1Draft) {
	w.Header().Set("ETag", draftETag(d.Version))
	if status == http.StatusCreated {
		w.Header().Set("Location", "/v1/drafts/"+draftAPIID(d.ID))
	}
	writeV1(w, status, a.draftOutBody(r.Context(), d))
}

func emptyPostBody() string { return `{"content":{},"targets":[]}` }

// marshalPost stores a draft's post as it was sent (every field known: the
// decoder refuses others), leaving out the fields not set, so the stored
// body is one POST /v1/posts takes as it is.
func marshalPost(p *v1CreatePostReq) string {
	if p == nil {
		return emptyPostBody()
	}
	b, _ := json.Marshal(p)
	var v any
	if json.Unmarshal(b, &v) != nil {
		return emptyPostBody()
	}
	b, _ = json.Marshal(dropNulls(v))
	return string(b)
}

// dropNulls removes the object members whose value is null, at any depth.
// Target options keep theirs: null there means "the default", on purpose.
func dropNulls(v any) any {
	switch t := v.(type) {
	case map[string]any:
		for k, x := range t {
			if x == nil {
				delete(t, k)
				continue
			}
			if k == "options" {
				continue
			}
			t[k] = dropNulls(x)
		}
	case []any:
		for i, x := range t {
			t[i] = dropNulls(x)
		}
	}
	return v
}

// v1OwnDraft loads the draft named by the path.
func (a *API) v1OwnDraft(w http.ResponseWriter, r *http.Request) (*store.V1Draft, bool) {
	if a.Store == nil {
		problem(w, r, http.StatusServiceUnavailable, codeUnavailable, "no database")
		return nil, false
	}
	id := r.PathValue("id")
	sid, ok := parseID(id, prefixDraft)
	if !ok {
		problem(w, r, http.StatusNotFound, codeNotFound, "no draft "+id)
		return nil, false
	}
	pr, _ := principalFrom(r.Context())
	d, err := a.Store.GetV1Draft(r.Context(), pr.OwnerID, sid)
	if errors.Is(err, store.ErrDraftNotFound) {
		problem(w, r, http.StatusNotFound, codeNotFound, "no draft "+id)
		return nil, false
	}
	if err != nil {
		slog.ErrorContext(r.Context(), "v1: read draft", "err", err)
		problem(w, r, http.StatusInternalServerError, codeInternal, "reading the draft failed")
		return nil, false
	}
	return d, true
}

// ─── GET /v1/drafts ──────────────────────────────────────────────────────

func (a *API) v1ListDrafts(w http.ResponseWriter, r *http.Request) {
	if a.Store == nil {
		problem(w, r, http.StatusServiceUnavailable, codeUnavailable, "no database")
		return
	}
	limit, after, ok := pageParams(w, r)
	if !ok {
		return
	}
	pr, _ := principalFrom(r.Context())
	q := r.URL.Query()
	if len(q["tag"]) > maxDraftTags {
		problem(w, r, http.StatusBadRequest, codeInvalidParameter, "at most 20 tags",
			fieldError{Parameter: "tag", Code: "too_many", Detail: "at most 20 tags"})
		return
	}
	dq := store.V1DraftQuery{OwnerID: pr.OwnerID, Text: q.Get("q"), Tags: q["tag"], Limit: limit + 1}
	if after != nil {
		sid, ok := parseID(after.ID, prefixDraft)
		if !ok {
			problem(w, r, http.StatusBadRequest, codeInvalidCursor, "cursor is not one this server issued",
				fieldError{Parameter: "cursor", Code: "invalid", Detail: "pass next_cursor from the previous page unchanged"})
			return
		}
		dq.BeforeTime, dq.BeforeID = &after.Time, sid
	}
	ds, err := a.Store.ListV1Drafts(r.Context(), dq)
	if err != nil {
		slog.ErrorContext(r.Context(), "v1: list drafts", "err", err)
		problem(w, r, http.StatusInternalServerError, codeInternal, "listing drafts failed")
		return
	}
	var next *cursor
	if len(ds) > limit {
		ds = ds[:limit]
		last := ds[limit-1]
		next = &cursor{Time: last.UpdatedAt, ID: draftAPIID(last.ID)}
	}
	items := make([]v1DraftListItem, 0, len(ds))
	for _, d := range ds {
		it := v1DraftListItem{ID: draftAPIID(d.ID), Title: d.Title, Tags: d.Tags, CreatedAt: rfc3339(d.CreatedAt),
			UpdatedAt: rfc3339(d.UpdatedAt)}
		preview := strings.Join(strings.Fields(d.Text), " ")
		if r := []rune(preview); len(r) > 120 {
			preview = string(r[:120])
		}
		it.Preview = preview
		if d.FirstMediaID != "" {
			if m, err := a.Store.MediaObject(r.Context(), pr.OwnerID, d.FirstMediaID); err == nil {
				u := m.URL
				if strings.HasPrefix(m.Mime, "video/") {
					u = m.PosterURL
				}
				if u != "" {
					it.MediaURL = &u
				}
			}
		}
		items = append(items, it)
	}
	writeV1(w, http.StatusOK, newList(items, next))
}

// ─── POST /v1/drafts ─────────────────────────────────────────────────────

func (a *API) v1CreateDraft(w http.ResponseWriter, r *http.Request) {
	var in v1DraftIn
	if !decodeJSON(w, r, &in) {
		return
	}
	if a.Store == nil {
		problem(w, r, http.StatusServiceUnavailable, codeUnavailable, "no database")
		return
	}
	pr, _ := principalFrom(r.Context())
	errs, err := a.checkDraft(r.Context(), pr.OwnerID, &in)
	if err != nil {
		slog.ErrorContext(r.Context(), "v1: check draft", "err", err)
		problem(w, r, http.StatusInternalServerError, codeInternal, "checking the draft failed")
		return
	}
	if len(errs) > 0 {
		invalid(w, r, errs)
		return
	}
	d := &store.V1Draft{OwnerID: pr.OwnerID, Text: postText(in.Post), Body: marshalPost(in.Post)}
	if in.Tags != nil {
		d.Tags = *in.Tags
	}
	title := ""
	if in.Title != nil {
		title = *in.Title
	}
	d.Title = draftTitle(title, d.Text)
	if err := a.Store.CreateV1Draft(r.Context(), d); err != nil {
		slog.ErrorContext(r.Context(), "v1: save draft", "err", err)
		problem(w, r, http.StatusInternalServerError, codeInternal, "saving the draft failed")
		return
	}
	a.writeDraftV1(w, r, http.StatusCreated, d)
}

// ─── GET/PATCH/DELETE /v1/drafts/{id} ────────────────────────────────────

func (a *API) v1GetDraft(w http.ResponseWriter, r *http.Request) {
	d, ok := a.v1OwnDraft(w, r)
	if !ok {
		return
	}
	a.writeDraftV1(w, r, http.StatusOK, d)
}

// draftPrecondition answers a stale or unreadable If-Match; false when it
// did.
func draftPrecondition(w http.ResponseWriter, r *http.Request, d *store.V1Draft) (int64, bool) {
	want, ok := ifMatch(r)
	if !ok {
		problem(w, r, http.StatusBadRequest, codeInvalidParameter, `If-Match must be an ETag this server sent ("3") or *`,
			fieldError{Parameter: "If-Match", Code: "invalid", Detail: "the draft's ETag, unchanged"})
		return 0, false
	}
	if want > 0 && want != d.Version {
		w.Header().Set("ETag", draftETag(d.Version))
		problem(w, r, http.StatusPreconditionFailed, codePreconditionFailed,
			"the draft changed since you read it (another tab or client saved it); read it again, then decide")
		return 0, false
	}
	return want, true
}

func (a *API) v1PatchDraft(w http.ResponseWriter, r *http.Request) {
	var in v1DraftIn
	if !decodeJSON(w, r, &in) {
		return
	}
	d, ok := a.v1OwnDraft(w, r)
	if !ok {
		return
	}
	want, ok := draftPrecondition(w, r, d)
	if !ok {
		return
	}
	errs, err := a.checkDraft(r.Context(), d.OwnerID, &in)
	if err != nil {
		slog.ErrorContext(r.Context(), "v1: check draft", "err", err)
		problem(w, r, http.StatusInternalServerError, codeInternal, "checking the draft failed")
		return
	}
	if len(errs) > 0 {
		invalid(w, r, errs)
		return
	}
	// A title derived from the text follows the text; one given is kept.
	derived := d.Title == store.DeriveTitle(d.Text)
	if in.Post != nil {
		d.Body, d.Text = marshalPost(in.Post), postText(in.Post)
	}
	if in.Tags != nil {
		d.Tags = *in.Tags
	}
	switch {
	case in.Title != nil:
		d.Title = draftTitle(*in.Title, d.Text)
	case derived:
		d.Title = store.DeriveTitle(d.Text)
	}
	// Without If-Match the write goes on the version just read, so a
	// write landing in between is still not lost silently.
	if want == 0 {
		want = d.Version
	}
	switch err := a.Store.UpdateV1Draft(r.Context(), d, want); {
	case errors.Is(err, store.ErrDraftConflict):
		a.setCurrentETag(r.Context(), w, d.OwnerID, d.ID)
		problem(w, r, http.StatusPreconditionFailed, codePreconditionFailed, "the draft changed while saving; read it again")
		return
	case errors.Is(err, store.ErrDraftNotFound):
		problem(w, r, http.StatusNotFound, codeNotFound, "no draft "+r.PathValue("id"))
		return
	case err != nil:
		slog.ErrorContext(r.Context(), "v1: update draft", "err", err)
		problem(w, r, http.StatusInternalServerError, codeInternal, "saving the draft failed")
		return
	}
	a.writeDraftV1(w, r, http.StatusOK, d)
}

// setCurrentETag sets the draft's ETag as it now is (a 412 tells it).
func (a *API) setCurrentETag(ctx context.Context, w http.ResponseWriter, owner, id string) {
	if cur, err := a.Store.GetV1Draft(ctx, owner, id); err == nil {
		w.Header().Set("ETag", draftETag(cur.Version))
	}
}

func (a *API) v1DeleteDraft(w http.ResponseWriter, r *http.Request) {
	d, ok := a.v1OwnDraft(w, r)
	if !ok {
		return
	}
	want, ok := draftPrecondition(w, r, d)
	if !ok {
		return
	}
	switch err := a.Store.DeleteV1Draft(r.Context(), d.OwnerID, d.ID, want); {
	case errors.Is(err, store.ErrDraftConflict):
		a.setCurrentETag(r.Context(), w, d.OwnerID, d.ID)
		problem(w, r, http.StatusPreconditionFailed, codePreconditionFailed, "the draft changed; read it again")
		return
	case errors.Is(err, store.ErrDraftNotFound):
		problem(w, r, http.StatusNotFound, codeNotFound, "no draft "+r.PathValue("id"))
		return
	case err != nil:
		slog.ErrorContext(r.Context(), "v1: delete draft", "err", err)
		problem(w, r, http.StatusInternalServerError, codeInternal, "deleting the draft failed")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ─── POST /v1/drafts/{id}:translate ──────────────────────────────────────

type v1DraftTranslateReq struct {
	TargetLang string `json:"target_lang"`
}

func (a *API) v1DraftAction(w http.ResponseWriter, r *http.Request) {
	id, action, found := strings.Cut(r.PathValue("id"), ":")
	if !found {
		w.Header().Set("Allow", "DELETE, GET, HEAD, PATCH")
		problem(w, r, http.StatusMethodNotAllowed, codeMethodNotAllowed, "POST is not allowed here; use GET, HEAD, PATCH, DELETE, or POST to :translate")
		return
	}
	r.SetPathValue("id", id)
	if action != "translate" {
		problem(w, r, http.StatusNotFound, codeNotFound, "no such action :"+action+"; drafts take :translate")
		return
	}
	a.v1TranslateDraft(w, r)
}

// v1TranslateDraft makes a new draft with the text translated: the same
// post, in the target language. Per-account texts and languages are left
// out (they were for the original language).
func (a *API) v1TranslateDraft(w http.ResponseWriter, r *http.Request) {
	var req v1DraftTranslateReq
	if !decodeJSON(w, r, &req) {
		return
	}
	// Translating spends the DeepL quota, as POST /v1/translate does.
	if pr, _ := principalFrom(r.Context()); !pr.can(scopePostsWrite) {
		problem(w, r, http.StatusForbidden, codeInsufficientScope, "translating needs the posts:write scope too")
		return
	}
	src, ok := a.v1OwnDraft(w, r)
	if !ok {
		return
	}
	tr := a.translator()
	if tr == nil {
		problem(w, r, http.StatusServiceUnavailable, codeUnavailable, "translation is not configured on this server (DEEPL_API_KEY)")
		return
	}
	var errs []fieldError
	if !translate.IsSupported(req.TargetLang) {
		errs = append(errs, fieldError{Pointer: "/target_lang", Code: "unsupported_language",
			Detail: "an ISO 639-1 code DeepL translates into; GET /v1/instance lists yours as translate_targets"})
	}
	if strings.TrimSpace(src.Text) == "" {
		errs = append(errs, fieldError{Pointer: "", Code: "empty", Detail: "the draft has no text to translate"})
	}
	if len(errs) > 0 {
		invalid(w, r, errs)
		return
	}
	lang := strings.ToLower(req.TargetLang)
	var p v1CreatePostReq
	_ = json.Unmarshal([]byte(src.Body), &p)
	// Each hand-made part is translated on its own, so the thread keeps its
	// breaks (now at the translated parts' starts) and media stay with
	// their posts. Positions in the source text mean nothing in the new one.
	text, breaks, starts, err := translateParts(r.Context(), tr, src.Text, validBreaks(src.Text, p.Content.Breaks), lang)
	if err != nil {
		slog.WarnContext(r.Context(), "v1: translate draft", "err", store.Scrub(err.Error()))
		problem(w, r, http.StatusBadGateway, codeUpstream, "the translation service failed or could not be reached; try again")
		return
	}
	oldStarts := append([]int{0}, validBreaks(src.Text, p.Content.Breaks)...)
	p.Content.Text = &text
	p.Content.Breaks = breaks
	for i := range p.Content.Media {
		if at := p.Content.Media[i].At; at != nil {
			k := 0
			for k+1 < len(oldStarts) && oldStarts[k+1] <= *at {
				k++
			}
			na := starts[k]
			p.Content.Media[i].At = &na
		}
	}
	p.Content.Languages = []string{lang}
	for i := range p.Targets {
		if c := p.Targets[i].Content; c != nil {
			c.Text, c.Languages = nil, nil
		}
		// An account's own breaks and media positions were against its
		// own (now dropped) or the source text: it takes the shared ones.
		p.Targets[i].Thread = nil
	}
	d := &store.V1Draft{OwnerID: src.OwnerID, Title: store.DeriveTitle(text), Tags: src.Tags, Text: text, Body: marshalPost(&p)}
	if err := a.Store.CreateV1Draft(r.Context(), d); err != nil {
		slog.ErrorContext(r.Context(), "v1: save translated draft", "err", err)
		problem(w, r, http.StatusInternalServerError, codeInternal, "saving the translated draft failed")
		return
	}
	a.writeDraftV1(w, r, http.StatusCreated, d)
}

// validBreaks is breaks kept only when ascending and inside text (0 and the
// end excluded), as code point positions.
func validBreaks(text string, breaks []int) []int {
	n := utf8.RuneCountInString(text)
	var out []int
	last := 0
	for _, b := range breaks {
		if b > last && b < n {
			out = append(out, b)
			last = b
		}
	}
	return out
}

// translateParts translates text part by part (cut at breaks, code point
// positions), keeping each part's surrounding whitespace. It returns the
// joined text, the breaks at the translated parts' starts, and every part's
// start (starts[0] = 0).
func translateParts(ctx context.Context, tr Translator, text string, breaks []int, lang string) (string, []int, []int, error) {
	rs := []rune(text)
	cuts := append(append([]int{0}, breaks...), len(rs))
	var b strings.Builder
	var newBreaks []int
	starts := make([]int, 0, len(cuts)-1)
	pos := 0
	for i := 0; i+1 < len(cuts); i++ {
		part := string(rs[cuts[i]:cuts[i+1]])
		core := strings.TrimSpace(part)
		lead := part[:strings.Index(part, core)]
		trail := part[len(lead)+len(core):]
		if core != "" {
			t, _, err := tr.Translate(ctx, core, lang)
			if err != nil {
				return "", nil, nil, err
			}
			core = t
		}
		if i > 0 {
			newBreaks = append(newBreaks, pos)
		}
		starts = append(starts, pos)
		out := lead + core + trail
		b.WriteString(out)
		pos += utf8.RuneCountInString(out)
	}
	return b.String(), newBreaks, starts, nil
}
