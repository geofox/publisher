package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/geofox/publisher/internal/destination"
	"github.com/geofox/publisher/internal/dispatch"
	"github.com/geofox/publisher/internal/store"
	"github.com/geofox/publisher/internal/transcode"
	"github.com/geofox/publisher/internal/unfurl"
)

// POST /v1/posts and POST /v1/posts:preview take one body: shared content,
// the accounts to post to (each with its own options and content
// overrides), threading and an optional schedule. Both run the same
// validation and planner, so a request that previews clean publishes
// clean. Replies, quotes, reposts and explicit per-account thread items
// come later (spec §6.4).

// Bounds on a post. Destinations have their own (smaller) limits, which the
// planner applies by splitting into a thread; these keep one request from
// planning something unbounded.
const (
	maxPostTextRunes = 50000
	maxCWRunes       = 1000
	maxPostTargets   = 20
	maxPostLanguages = 10
	maxThreadParts   = 100
	maxPreferWait    = 60 // seconds
	maxOptionRunes   = 500
)

type v1CreatePostReq struct {
	Content   v1CreateContent    `json:"content"`
	Targets   []v1CreateTarget   `json:"targets"`
	Threading *v1CreateThreading `json:"threading"`
	Schedule  *v1CreateSchedule  `json:"schedule"`
}

type v1CreateContent struct {
	Text           *string         `json:"text"`
	Media          []v1CreateMedia `json:"media"`
	ContentWarning *string         `json:"content_warning"`
	Sensitive      *bool           `json:"sensitive"`
	Languages      []string        `json:"languages"`
}

type v1CreateMedia struct {
	ID  string  `json:"id"`
	Alt *string `json:"alt"` // null or absent: the media object's own alt
}

// v1TargetContent overrides the shared content for one account. A field
// left out (or null) inherits; languages: [] sends none.
type v1TargetContent struct {
	Text           *string  `json:"text"`
	ContentWarning *string  `json:"content_warning"`
	Sensitive      *bool    `json:"sensitive"`
	Languages      []string `json:"languages"`
}

type v1CreateTarget struct {
	Account string           `json:"account"`
	Options map[string]any   `json:"options"`
	Content *v1TargetContent `json:"content"`
}

type v1CreateThreading struct {
	Numbering bool `json:"numbering"`
}

type v1CreateSchedule struct {
	At *string `json:"at"`
}

// createPlan is a validated request, ready to save or preview.
type createPlan struct {
	spec    dispatch.PostSpec
	at      *time.Time
	targets []plannedTarget
	media   []*store.MediaObject // the post's media, in order
	// mediaReq is each media's index in the request.
	mediaReq []int
}

type plannedTarget struct {
	account store.Account
	caps    destination.Capabilities
	text    string
	textPtr string // where text came from, for errors
	ov      dispatch.Overrides
	// warnings are notes about request fields the account ignores.
	warnings []string
}

// optionFields maps a destination option key onto the override field the
// engine reads. Every option a registered driver declares must be here
// (TestV1EveryOptionMapped), or a request using it is refused rather than
// having the option silently dropped.
var optionFields = map[string]func(ov *dispatch.Overrides, v any){
	"pow": func(ov *dispatch.Overrides, v any) {
		n, _ := destination.OptInt(map[string]any{"v": v}, "v")
		ov.POW = &n
	},
	"topic_tag":      func(ov *dispatch.Overrides, v any) { ov.TopicTag, _ = v.(string) },
	"reply_control":  func(ov *dispatch.Overrides, v any) { ov.ThreadsReplyControl, _ = v.(string) },
	"reply_gate":     func(ov *dispatch.Overrides, v any) { ov.BlueskyReply, _ = v.(string) },
	"disable_quotes": func(ov *dispatch.Overrides, v any) { ov.BlueskyDisableQuotes, _ = v.(bool) },
}

// langTag is a loose BCP 47 shape check: "en", "pt-BR", "zh-Hant".
var langTag = regexp.MustCompile(`^[A-Za-z]{2,3}(-[A-Za-z0-9]{1,8}){0,3}$`)

// jsonPointerToken escapes a key for a JSON Pointer (RFC 6901).
func jsonPointerToken(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, "~", "~0"), "/", "~1")
}

// planCreate validates a create body against the caller's accounts and
// media. Field problems come back as errs (422); err is a server failure.
func (a *API) planCreate(ctx context.Context, owner string, req *v1CreatePostReq) (*createPlan, []fieldError, error) {
	var errs []fieldError
	add := func(ptr, code, detail string) {
		errs = append(errs, fieldError{Pointer: ptr, Code: code, Detail: detail})
	}
	plan := &createPlan{}

	// Shared content.
	text := ""
	if req.Content.Text != nil {
		text = *req.Content.Text
		if utf8.RuneCountInString(text) > maxPostTextRunes {
			add("/content/text", "too_long", "at most 50000 characters")
		}
	}
	checkCW := func(ptr string, cw *string) {
		if cw != nil && utf8.RuneCountInString(*cw) > maxCWRunes {
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
	checkCW("/content/content_warning", req.Content.ContentWarning)
	checkLangs("/content/languages", req.Content.Languages)

	// Media, by id.
	images, videos := 0, 0
	if len(req.Content.Media) > maxImagesPerRequest {
		add("/content/media", "too_many", "at most 40 media")
	} else {
		seen := map[string]bool{}
		for j, cm := range req.Content.Media {
			ptr := "/content/media/" + strconv.Itoa(j)
			if seen[cm.ID] {
				add(ptr+"/id", "duplicate", "this media is already in the post")
				continue
			}
			seen[cm.ID] = true
			m, err := a.Store.MediaObject(ctx, owner, cm.ID)
			if errors.Is(err, store.ErrNoMedia) || !strings.HasPrefix(cm.ID, prefixMedia) {
				add(ptr+"/id", "unknown_media", "no media "+cm.ID+"; upload it with POST /v1/media")
				continue
			}
			if err == nil {
				err = a.refreshMedia(ctx, m)
			}
			if err != nil {
				return nil, nil, err
			}
			switch m.Status {
			case store.MediaProcessing:
				add(ptr+"/id", "media_processing", "the video is still being processed; poll GET /v1/media/"+m.ID+" until it is ready")
				continue
			case store.MediaFailed:
				add(ptr+"/id", "media_failed", "this media failed to process: "+m.Error)
				continue
			}
			if cm.Alt != nil {
				if msg := altProblem(*cm.Alt); msg != "" {
					add(ptr+"/alt", "invalid", msg)
				}
			}
			alt := m.Alt
			if cm.Alt != nil {
				alt = *cm.Alt
			}
			if strings.HasPrefix(m.Mime, "video/") {
				videos++
			} else {
				images++
			}
			plan.media = append(plan.media, m)
			plan.mediaReq = append(plan.mediaReq, j)
			plan.spec.MediaRecords = append(plan.spec.MediaRecords, mediaRecord(m, len(plan.spec.MediaRecords), alt))
		}
		switch {
		case videos > 1:
			add("/content/media", "too_many_videos", "a post carries at most one video")
		case videos == 1 && images > 0:
			add("/content/media", "mixed_media", "a post carries images or one video, not both")
		}
	}

	// Targets.
	if len(req.Targets) == 0 {
		add("/targets", "required", "name at least one account to post to")
	}
	if len(req.Targets) > maxPostTargets {
		add("/targets", "too_many", "at most 20 accounts")
		return nil, errs, nil
	}
	accts, err := a.Store.Accounts(ctx, owner)
	if err != nil {
		return nil, nil, err
	}
	byID := make(map[string]store.Account, len(accts))
	for _, ac := range accts {
		byID[ac.ID] = ac
	}
	plan.spec.Overrides = map[string]dispatch.Overrides{}
	plan.spec.Accounts = map[string]string{}
	seenAcct, seenType := map[string]bool{}, map[string]bool{}
	sharedEmpty := false // reported once, not per target
	for i, t := range req.Targets {
		ptr := "/targets/" + strconv.Itoa(i)
		ac, ok := byID[t.Account]
		if !ok {
			add(ptr+"/account", "unknown_account", "no account "+t.Account+"; list them with GET /v1/accounts")
			continue
		}
		drv, registered := destination.Lookup(ac.Type)
		if !ac.Enabled || !registered {
			add(ptr+"/account", "account_disabled", "this account is disabled (not configured on this server)")
			continue
		}
		if seenAcct[ac.ID] {
			add(ptr+"/account", "duplicate", "this account is already a target")
			continue
		}
		if seenType[ac.Type] {
			// The engine sends through one account per type until accounts
			// can be added (step 4).
			add(ptr+"/account", "duplicate_type", "one account per destination type for now")
			continue
		}
		seenAcct[ac.ID], seenType[ac.Type] = true, true
		caps := a.caps(ctx, ac.Type)
		name := drv.Manifest().Name
		pt := plannedTarget{account: ac, caps: caps, text: text, textPtr: "/content/text"}
		ov := dispatch.Overrides{}
		cw, sensitive, langs := req.Content.ContentWarning, req.Content.Sensitive, req.Content.Languages
		if c := t.Content; c != nil {
			if c.Text != nil {
				// The engine reads an empty override as "inherit", so a
				// blank one would preview empty and publish the shared text.
				pt.text, ov.Text, pt.textPtr = *c.Text, *c.Text, ptr+"/content/text"
				switch {
				case strings.TrimSpace(*c.Text) == "":
					add(ptr+"/content/text", "empty", "leave text out (or null) to post the shared text")
				case utf8.RuneCountInString(*c.Text) > maxPostTextRunes:
					add(ptr+"/content/text", "too_long", "at most 50000 characters")
				}
			}
			checkCW(ptr+"/content/content_warning", c.ContentWarning)
			checkLangs(ptr+"/content/languages", c.Languages)
			if c.ContentWarning != nil {
				cw = c.ContentWarning
			}
			if c.Sensitive != nil {
				sensitive = c.Sensitive
			}
			if c.Languages != nil {
				langs = c.Languages
			}
		}
		if cw != nil && *cw != "" {
			ov.ContentWarning = *cw
			if !caps.ContentWarning {
				pt.warnings = append(pt.warnings, name+" has no content warnings; it is left out")
			}
		}
		if sensitive != nil && *sensitive {
			ov.Sensitive = true
			if !caps.Sensitive {
				pt.warnings = append(pt.warnings, name+" can't mark media sensitive; it is left out")
			}
		}
		if len(langs) > 0 {
			ov.Langs = langs
			switch {
			case caps.Languages == 0:
				pt.warnings = append(pt.warnings, name+" takes no languages; they are left out")
			case len(langs) > caps.Languages:
				pt.warnings = append(pt.warnings, name+" takes "+strconv.Itoa(caps.Languages)+" language(s); the rest are left out")
			}
		}
		a.planOptions(drv.Manifest(), caps, t.Options, ptr+"/options", &ov, add)
		if strings.TrimSpace(pt.text) == "" && len(req.Content.Media) == 0 && ov.Text == "" && !sharedEmpty {
			sharedEmpty = true
			add("/content/text", "required", "a post needs text or media")
		}
		for j, m := range plan.media {
			mptr := "/content/media/" + strconv.Itoa(plan.mediaReq[j])
			if strings.HasPrefix(m.Mime, "video/") {
				if fail, warns := dispatch.VideoCheck(name, caps.Media, m.SizeBytes, m.DurationSecs); fail != "" {
					add(mptr, "media_unsupported", "for "+ac.ID+": "+fail)
				} else {
					pt.warnings = append(pt.warnings, warns...)
				}
			} else if !caps.Media.Images.Supported {
				add(mptr, "media_unsupported", "for "+ac.ID+": "+name+" does not take images")
				break
			}
		}
		pt.ov = ov
		plan.targets = append(plan.targets, pt)
		plan.spec.Platforms = append(plan.spec.Platforms, ac.Type)
		plan.spec.Overrides[ac.Type] = ov
		plan.spec.Accounts[ac.Type] = ac.ID
	}

	// Threading and schedule.
	if req.Threading != nil {
		plan.spec.Number = req.Threading.Numbering
	}
	if s := req.Schedule; s != nil {
		if s.At == nil {
			add("/schedule/at", "required", "an RFC 3339 time, such as 2027-01-01T09:00:00Z")
		} else if t, err := time.Parse(time.RFC3339, strings.ToUpper(*s.At)); err != nil {
			add("/schedule/at", "invalid_time", "an RFC 3339 time, such as 2027-01-01T09:00:00Z")
		} else if !t.After(time.Now()) {
			add("/schedule/at", "in_past", "the time must be in the future; leave schedule out to post now")
		} else {
			t = t.UTC()
			plan.at = &t
		}
	}

	// The planner's split must stay a reasonable thread.
	// (Planned without the link card: a card only shortens the text.)
	for _, pt := range plan.targets {
		cp := dispatch.PlanTarget(pt.caps, pt.text, nil, make([]int, len(plan.media)), plan.spec.Number)
		if len(cp.Segs) > maxThreadParts {
			add(pt.textPtr, "too_many_parts", "the text makes "+strconv.Itoa(len(cp.Segs))+
				" thread parts on "+pt.account.ID+"; at most 100")
		}
	}
	plan.spec.MasterText = text
	plan.spec.Source = "api"
	return plan, errs, nil
}

// planOptions validates one target's options against its destination's
// manifest (plus visibility, a common field the account's capabilities
// list) and sets them on ov.
func (a *API) planOptions(m destination.Manifest, caps destination.Capabilities, opts map[string]any, ptr string,
	ov *dispatch.Overrides, add func(ptr, code, detail string)) {
	keys := make([]string, 0, len(opts))
	for k := range opts {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		v, p := opts[k], ptr+"/"+jsonPointerToken(k)
		if v == nil {
			continue // null: the default
		}
		if k == "visibility" && len(caps.Visibility) > 0 {
			s, ok := v.(string)
			if !ok || (s != "" && !slices.Contains(caps.Visibility, s)) {
				add(p, "invalid_option", "one of "+strings.Join(caps.Visibility, ", "))
				continue
			}
			ov.Visibility = s
			continue
		}
		spec, ok := m.Option(k)
		if !ok {
			add(p, "unknown_option", m.Name+" has no option "+k+"; see GET /v1/destinations")
			continue
		}
		if errs := destination.ValidateOptions([]destination.OptionSpec{spec}, map[string]any{k: v}); len(errs) > 0 {
			add(p, "invalid_option", strings.TrimPrefix(errs[0], k+": "))
			continue
		}
		if str, ok := v.(string); ok && utf8.RuneCountInString(str) > maxOptionRunes {
			add(p, "invalid_option", "at most 500 characters")
			continue
		}
		if k == "reply_gate" {
			if msg := replyGateProblem(v.(string)); msg != "" {
				add(p, "invalid_option", msg)
				continue
			}
		}
		set, ok := optionFields[k]
		if !ok {
			add(p, "unsupported_option", "this server can't send "+k+" yet")
			continue
		}
		set(ov, v)
	}
}

// replyGateProblem checks a Bluesky reply gate strictly: the driver reads
// an unknown word as "anyone may reply", which an API caller should hear
// about instead.
func replyGateProblem(s string) string {
	s = strings.TrimSpace(s)
	if s == "" || strings.EqualFold(s, "nobody") {
		return ""
	}
	for _, tok := range strings.Split(s, ",") {
		switch strings.ToLower(strings.TrimSpace(tok)) {
		case "mention", "following", "follower":
		default:
			return `"" (anyone), "nobody", or a comma list of mention, following, follower`
		}
	}
	return ""
}

// enqueuer saves a post for the background workers (*dispatch.Dispatcher).
type enqueuer interface {
	Enqueue(ctx context.Context, spec dispatch.PostSpec, at *time.Time) (*store.Post, error)
}

// ─── POST /v1/posts ──────────────────────────────────────────────────────

func (a *API) v1CreatePost(w http.ResponseWriter, r *http.Request) {
	var req v1CreatePostReq
	if !decodeJSON(w, r, &req) {
		return
	}
	if a.Store == nil {
		problem(w, r, http.StatusServiceUnavailable, codeUnavailable, "no database")
		return
	}
	enq, ok := a.Dispatch.(enqueuer)
	if !ok {
		problem(w, r, http.StatusServiceUnavailable, codeUnavailable, "publishing is not available")
		return
	}
	pr, _ := principalFrom(r.Context())
	plan, errs, err := a.planCreate(r.Context(), pr.OwnerID, &req)
	if err != nil {
		slog.ErrorContext(r.Context(), "v1: plan post", "err", err)
		problem(w, r, http.StatusInternalServerError, codeInternal, "planning the post failed")
		return
	}
	if len(errs) > 0 {
		invalid(w, r, errs)
		return
	}
	// Saving plans the post, which may fetch a link card: the post must
	// not be lost to a client that hangs up meanwhile.
	p, err := enq.Enqueue(context.WithoutCancel(r.Context()), plan.spec, plan.at)
	if err != nil {
		slog.ErrorContext(r.Context(), "v1: save post", "err", err)
		problem(w, r, http.StatusInternalServerError, codeInternal, "saving the post failed")
		return
	}
	w.Header().Set("Location", "/v1/posts/"+postID(p.ID))
	wait, asked := preferWait(r.Header.Values("Prefer"))
	status := http.StatusAccepted
	if asked && plan.at == nil {
		w.Header().Set("Preference-Applied", "wait="+strconv.Itoa(wait))
		if a.waitSettled(r.Context(), p.ID, time.Duration(wait)*time.Second) {
			status = http.StatusCreated
		}
	}
	// The post is saved: whatever happens now, the answer must not be a
	// 5xx, or the client would retry its Idempotency-Key and post twice.
	if cur, err := a.Store.GetPost(p.ID); err == nil {
		p = cur
	} else {
		slog.WarnContext(r.Context(), "v1: reload created post", "post_id", p.ID, "err", err)
	}
	writeV1(w, status, v1Post(*p, true))
}

// preferWait reads a wait preference (RFC 7240 §4.3) from Prefer headers,
// capped at maxPreferWait seconds.
func preferWait(values []string) (int, bool) {
	for _, v := range values {
		for _, pref := range strings.Split(v, ",") {
			tok, _, _ := strings.Cut(pref, ";")
			name, val, ok := strings.Cut(strings.TrimSpace(tok), "=")
			if !ok || !strings.EqualFold(strings.TrimSpace(name), "wait") {
				continue
			}
			n, err := strconv.Atoi(strings.Trim(strings.TrimSpace(val), `"`))
			if err != nil || n < 0 {
				continue
			}
			return min(n, maxPreferWait), true
		}
	}
	return 0, false
}

// waitSettled waits up to d for a post to settle: nothing left to send and
// no automatic retry pending. False when the wait ran out, the client went
// away or the server is stopping.
func (a *API) waitSettled(ctx context.Context, id string, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	for {
		changed := a.Store.EventsChanged() // before reading, so no change is missed
		if _, settled, err := a.Store.PostState(ctx, id); err != nil || settled {
			return err == nil
		}
		select {
		case <-changed:
		case <-timer.C:
			return false
		case <-ctx.Done():
			return false
		case <-a.Stopping:
			return false
		}
	}
}

// ─── POST /v1/posts:preview ──────────────────────────────────────────────

type v1PreviewBody struct {
	Targets []v1PreviewTarget `json:"targets"`
}

type v1PreviewTarget struct {
	Account  string          `json:"account"`
	Type     string          `json:"type"`
	Items    []v1PreviewItem `json:"items"`
	LinkCard *v1PreviewCard  `json:"link_card"`
	Warnings []string        `json:"warnings"`
	Media    []v1PreviewNote `json:"media_notes"`
}

type v1PreviewItem struct {
	Text  string `json:"text"`
	Media []int  `json:"media"`
}

type v1PreviewCard struct {
	Item        int    `json:"item"`
	URL         string `json:"url"`
	Title       string `json:"title"`
	Description string `json:"description"`
	ThumbURL    string `json:"thumb_url,omitempty"`
}

type v1PreviewNote struct {
	Media int    `json:"media"`
	Note  string `json:"note"`
}

func (a *API) v1PreviewPost(w http.ResponseWriter, r *http.Request) {
	var req v1CreatePostReq
	if !decodeJSON(w, r, &req) {
		return
	}
	if a.Store == nil {
		problem(w, r, http.StatusServiceUnavailable, codeUnavailable, "no database")
		return
	}
	pr, _ := principalFrom(r.Context())
	plan, errs, err := a.planCreate(r.Context(), pr.OwnerID, &req)
	if err != nil {
		slog.ErrorContext(r.Context(), "v1: plan post", "err", err)
		problem(w, r, http.StatusInternalServerError, codeInternal, "planning the post failed")
		return
	}
	if len(errs) > 0 {
		invalid(w, r, errs)
		return
	}
	metas := make([]transcode.Meta, len(plan.media))
	for i, m := range plan.media {
		mw, mh := transcode.ParseDim(m.Dim)
		metas[i] = transcode.Meta{SizeBytes: m.SizeBytes, Mime: m.Mime, W: mw, H: mh}
	}
	cards := map[string]*unfurl.Card{} // one fetch per link
	out := v1PreviewBody{Targets: []v1PreviewTarget{}}
	for _, pt := range plan.targets {
		// The unfurl gets a tight budget: a slow page previews without a
		// card rather than holding the composer.
		var card *unfurl.Card
		if pt.caps.LinkPreview == destination.LinkPreviewClient && a.Unfurl != nil {
			if u, _, ok := unfurl.CardURL(pt.text); ok {
				c, seen := cards[u]
				if !seen {
					uctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
					c, _ = a.Unfurl.Unfurl(uctx, u)
					cancel()
					cards[u] = c
				}
				card = c
			}
		}
		cp := dispatch.PlanTarget(pt.caps, pt.text, card, make([]int, len(plan.media)), plan.spec.Number)
		vt := v1PreviewTarget{Account: pt.account.ID, Type: pt.account.Type, Items: []v1PreviewItem{},
			Warnings: append(append([]string{}, pt.warnings...), cp.Warnings...), Media: []v1PreviewNote{}}
		if len(cp.Segs) <= 1 {
			all := make([]int, len(plan.media))
			for i := range all {
				all[i] = i
			}
			vt.Items = append(vt.Items, v1PreviewItem{Text: cp.Text, Media: all})
		} else {
			for i, seg := range cp.Segs {
				it := v1PreviewItem{Text: seg, Media: []int{}}
				if i < len(cp.Plan) && cp.Plan[i] != nil {
					it.Media = cp.Plan[i]
				}
				vt.Items = append(vt.Items, it)
			}
		}
		if c := cp.Card; c != nil {
			vt.LinkCard = &v1PreviewCard{Item: c.Segment, URL: c.URI, Title: c.Title, Description: c.Description,
				ThumbURL: absoluteHTTPURL(c.ThumbURL)}
		}
		for _, n := range dispatch.PlanMediaFit(displayName(pt.account.Type), pt.caps.Media, metas) {
			vt.Media = append(vt.Media, v1PreviewNote{Media: n.Ordinal, Note: n.Note})
		}
		out.Targets = append(out.Targets, vt)
	}
	writeV1(w, http.StatusOK, out)
}
