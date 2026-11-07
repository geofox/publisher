package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/rivo/uniseg"

	"github.com/geofox/publisher/internal/destination"
	"github.com/geofox/publisher/internal/dispatch"
	"github.com/geofox/publisher/internal/resolve"
	"github.com/geofox/publisher/internal/store"
	"github.com/geofox/publisher/internal/thread"
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
	// At most one: reply to, quote or repost the post at a URL.
	ReplyTo *v1SourceIn `json:"reply_to"`
	Quote   *v1SourceIn `json:"quote"`
	Repost  *v1SourceIn `json:"repost"`
	// Draft, when set, is deleted once the post is saved (posting a draft).
	Draft *string `json:"draft"`
}

type v1CreateContent struct {
	Text *string `json:"text"`
	// Breaks are hand-made thread breaks: code point positions in text,
	// ascending, each starting a new post on every account that posts the
	// shared text (and has no breaks of its own).
	Breaks         []int           `json:"breaks"`
	Media          []v1CreateMedia `json:"media"`
	ContentWarning *string         `json:"content_warning"`
	Sensitive      *bool           `json:"sensitive"`
	Languages      []string        `json:"languages"`
}

type v1CreateMedia struct {
	ID  string  `json:"id"`
	Alt *string `json:"alt"` // null or absent: the media object's own alt
	// At is a code point position in the text: the media goes with the
	// post that holds it. Default 0 (the first post).
	At *int `json:"at"`
	// Part was the "---" part the media went with; it is refused (with a
	// pointer to at) rather than ignored.
	Part *int `json:"part"`
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
	Thread  *v1TargetThread  `json:"thread"`
}

// v1TargetThread is how one account's thread breaks, against the text it
// posts (its own, or the shared text).
type v1TargetThread struct {
	// Breaks replace the shared breaks for this account: [] posts the
	// text unbroken (still split to the account's limit); null or absent
	// takes the shared breaks, or none when the account has its own text.
	Breaks *[]int `json:"breaks"`
	// MediaAt places the media in this account's text, one position per
	// media in content.media. Absent: the shared positions, or the first
	// post when the account has its own text.
	MediaAt []int `json:"media_at"`
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
	draft   *store.V1Draft // the draft the post uses up, as read
	targets []plannedTarget
	media   []*store.MediaObject // the post's media, in order
	// mediaReq is each media's index in the request.
	mediaReq []int
	// mediaAt is each media's position in the shared text.
	mediaAt []int
	// ix is the reply, quote or repost, with the post it acts on.
	ix     *dispatch.InteractSpec
	source *resolve.SourceRef
	// upstream is set when the source's network couldn't be reached: the
	// request may be retried (502).
	upstream bool
}

// layoutOf is a target's layout for the planner, with a media position
// for each of the post's media.
func (p *createPlan) layoutOf(pt plannedTarget) thread.Layout {
	lay := pt.layout
	lay.MediaAt = append([]int(nil), lay.MediaAt...)
	return lay
}

type plannedTarget struct {
	account store.Account
	caps    destination.Capabilities
	text    string
	textPtr string // where text came from, for errors
	own     bool   // text is the target's own, not the shared text
	reqPtr  string // the target in the request: /targets/i
	ov      dispatch.Overrides
	// layout is the target's breaks and media positions in text.
	layout thread.Layout
	// warnings are notes about request fields the account ignores, and
	// the account's defaults applied.
	warnings []string
	// defaults are the account's defaults the target took.
	defaults v1AccountDefaults
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

	if req.Draft != nil {
		pr, _ := principalFrom(ctx)
		sid, ok := parseID(*req.Draft, prefixDraft)
		switch {
		case !pr.can(scopeDraftsWrite):
			add("/draft", "not_permitted", "posting a draft needs the drafts:write scope")
		case !ok:
			add("/draft", "unknown_draft", "no such draft")
		default:
			d, err := a.Store.GetV1Draft(ctx, owner, sid)
			if errors.Is(err, store.ErrDraftNotFound) {
				add("/draft", "unknown_draft", "no such draft")
			} else if err != nil {
				return nil, nil, err
			}
			plan.draft = d
		}
	}

	// Shared content.
	text := ""
	if req.Content.Text != nil {
		text = *req.Content.Text
		if utf8.RuneCountInString(text) > maxPostTextRunes {
			add("/content/text", "too_long", "at most 50000 characters")
		}
	}
	rawText := text
	checkBreaks(rawText, req.Content.Breaks, "/content/breaks", add)
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
			if cm.Part != nil {
				add(ptr+"/part", "replaced", "parts are gone: place the media with at, a position in the text")
			}
			at := 0
			if cm.At != nil {
				at = *cm.At
				checkAt(utf8.RuneCountInString(rawText), at, ptr+"/at", add)
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
			plan.mediaAt = append(plan.mediaAt, at)
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
	seenAcct := map[string]bool{}
	sharedEmpty := false // reported once, not per target
	for i, t := range req.Targets {
		ptr := "/targets/" + strconv.Itoa(i)
		ac, ok := byID[t.Account]
		if !ok {
			add(ptr+"/account", "unknown_account", "no account "+t.Account+"; list them with GET /v1/accounts")
			continue
		}
		drv, _ := destination.Lookup(ac.Type)
		if why := a.accountUnusable(ac); why != "" {
			add(ptr+"/account", "account_disabled", why)
			continue
		}
		if seenAcct[ac.ID] {
			add(ptr+"/account", "duplicate", "this account is already a target")
			continue
		}
		seenAcct[ac.ID] = true
		caps := a.caps(ctx, ac.ID, ac.Type)
		name := drv.Manifest().Name
		pt := plannedTarget{account: ac, caps: caps, text: rawText, textPtr: "/content/text", reqPtr: ptr}
		own := false
		ov := dispatch.Overrides{}
		cw, sensitive, langs := req.Content.ContentWarning, req.Content.Sensitive, req.Content.Languages
		if c := t.Content; c != nil {
			if c.Text != nil {
				// The engine reads an empty override as "inherit", so a
				// blank one would preview empty and publish the shared text.
				pt.text, pt.textPtr, own = *c.Text, ptr+"/content/text", true
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
		opts := t.Options
		if action, _, _ := req.interaction(); action != "repost" { // a repost carries nothing of its own
			opts, pt.defaults = a.applyDefaults(drv.Manifest(), caps, accountDefaults(ac), &cw, &langs, t.Options, &pt)
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
		a.planOptions(drv.Manifest(), caps, opts, ptr+"/options", &ov, add)
		if action, _, _ := req.interaction(); strings.TrimSpace(pt.text) == "" && len(req.Content.Media) == 0 &&
			!own && !sharedEmpty && action != "quote" && action != "repost" {
			sharedEmpty = true
			add("/content/text", "required", "a post needs text or media")
		}
		// A network that fetches media by address (Threads, Nostr) can't
		// reach this server's disk unless it is served publicly.
		if caps.Media.Transport == destination.TransportURL && a.DiskMedia != nil {
			for j, m := range plan.media {
				if _, local := a.DiskMedia.Owns(m.URL); local && !a.DiskMedia.Reachable(m.URL) {
					add("/content/media/"+strconv.Itoa(plan.mediaReq[j]), "media_unsupported", "for "+ac.ID+": "+name+
						" fetches media by address, and this server's disk doesn't serve them publicly (Settings, Media)")
				}
			}
		}
		for j, m := range plan.media {
			mptr := "/content/media/" + strconv.Itoa(plan.mediaReq[j])
			if max := caps.Media.AltMaxLength; max > 0 && utf8.RuneCountInString(plan.spec.MediaRecords[j].Alt) > max {
				pt.warnings = append(pt.warnings, fmt.Sprintf("%s takes alt text up to %d characters: media %d's is cut to that",
					name, max, plan.mediaReq[j]+1))
			}
			if !strings.HasPrefix(m.Mime, "video/") {
				mw, mh := transcode.ParseDim(m.Dim)
				if why := dispatch.ImageRejects(name, caps.Media, transcode.Meta{SizeBytes: m.SizeBytes, Mime: m.Mime, W: mw, H: mh}); why != "" {
					add(mptr, "media_unsupported", "for "+ac.ID+": "+name+" can't take this image: "+why)
				}
			}
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
		pt.layout = targetLayout(req, t, own, pt.text, plan, ptr, add)
		pt.text = thread.NormalizeNewlines(pt.text, pt.layout.Breaks, pt.layout.MediaAt)
		pt.ov, pt.own = ov, own
		plan.targets = append(plan.targets, pt)
		plan.spec.Targets = append(plan.spec.Targets, dispatch.TargetSpec{AccountID: ac.ID, Type: ac.Type,
			Text: pt.text, Overrides: ov, Layout: pt.layout})
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

	plan.spec.MasterText = thread.NormalizeNewlines(rawText)
	plan.spec.Source = "api"
	if _, _, n := req.interaction(); n > 0 {
		a.checkInteraction(req, plan, add)
		// Resolving costs a request to the source's network: only for an
		// otherwise valid request.
		if len(errs) == 0 {
			a.planInteraction(ctx, req, plan, add)
		}
	}

	// The planner's split must stay a reasonable thread. Planning costs
	// time on long texts: only for an otherwise valid request.
	// (Planned without the link card: a card only shortens the text.)
	if len(errs) > 0 {
		return plan, errs, nil
	}
	for _, pt := range plan.targets {
		cp := dispatch.PlanTarget(pt.caps, dispatch.TargetInput{Text: pt.text, Layout: plan.layoutOf(pt),
			Number: plan.spec.Number, ContentWarning: pt.ov.ContentWarning})
		if len(cp.Segs) > maxThreadParts {
			add(pt.textPtr, "too_many_parts", "the text makes "+strconv.Itoa(len(cp.Segs))+
				" thread parts on "+pt.account.ID+"; at most 100")
		}
		for i, it := range cp.Items {
			for _, pb := range it.Problems {
				add(pt.textPtr, "refused_by_destination", "post "+strconv.Itoa(i+1)+" on "+pt.account.ID+" "+pb)
			}
		}
	}
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
	if plan.upstream {
		problem(w, r, http.StatusBadGateway, codeUpstream, "the post's network could not be reached; try again")
		return
	}
	ienq, _ := a.Dispatch.(interactionEnqueuer)
	if plan.ix != nil && ienq == nil {
		problem(w, r, http.StatusServiceUnavailable, codeUnavailable, "publishing replies, quotes and reposts is not available")
		return
	}
	// The draft is taken before the post is saved, so two requests posting
	// it (a double click, two tabs) make one post; a draft saved again since
	// it was read is not lost. If saving fails, the draft is put back.
	if d := plan.draft; d != nil {
		switch err := a.Store.DeleteV1Draft(r.Context(), d.OwnerID, d.ID, d.Version); {
		case errors.Is(err, store.ErrDraftNotFound):
			invalid(w, r, []fieldError{{Pointer: "/draft", Code: "unknown_draft", Detail: "the draft was posted or deleted meanwhile"}})
			return
		case errors.Is(err, store.ErrDraftConflict):
			invalid(w, r, []fieldError{{Pointer: "/draft", Code: "draft_changed", Detail: "the draft was saved again meanwhile; post what it holds now"}})
			return
		case err != nil:
			slog.ErrorContext(r.Context(), "v1: take posted draft", "err", err)
			problem(w, r, http.StatusInternalServerError, codeInternal, "saving the post failed")
			return
		}
	}
	// Saving plans the post, which may fetch a link card: the post must
	// not be lost to a client that hangs up meanwhile.
	var p *store.Post
	if plan.ix != nil {
		ctx := context.WithoutCancel(r.Context())
		if want, n := sourceImagesWanted(plan), a.rehostSourceMedia(ctx, plan); n < want {
			slog.WarnContext(ctx, "v1: quote: some of the original's images were not copied", "want", want, "copied", n)
		}
		p, err = ienq.EnqueueInteraction(ctx, *plan.ix)
	} else {
		p, err = enq.Enqueue(context.WithoutCancel(r.Context()), plan.spec, plan.at)
	}
	if err != nil {
		slog.ErrorContext(r.Context(), "v1: save post", "err", err)
		if d := plan.draft; d != nil {
			if rerr := a.Store.RestoreV1Draft(context.WithoutCancel(r.Context()), d); rerr != nil {
				slog.ErrorContext(r.Context(), "v1: put back draft after a failed post", "draft_id", d.ID, "err", rerr)
			}
		}
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
	body := v1Post(*p, true)
	// What each account ignores of the request, and the defaults it
	// took: said once, in this answer.
	for i := range body.Targets {
		for _, pt := range plan.targets {
			if pt.account.ID == body.Targets[i].Account && len(pt.warnings) > 0 {
				body.Targets[i].Warnings = pt.warnings
			}
		}
	}
	writeV1(w, status, body)
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
	Account string `json:"account"`
	Type    string `json:"type"`
	// Role is what the account gets: post, reply, quote, repost, or
	// link_quote (a quote's copy on another network).
	Role string `json:"role"`
	// Text is the text the items' ranges are in: the account's own or the
	// shared text (a Bluesky link card's URL taken off its end), or the
	// copy a link-quote posts.
	Text string `json:"text"`
	// Limit is the most one post may count on this account (0: no limit),
	// after a content warning the destination counts in.
	Limit    int             `json:"limit"`
	Items    []v1PreviewItem `json:"items"`
	LinkCard *v1PreviewCard  `json:"link_card"`
	Warnings []string        `json:"warnings"`
	Media    []v1PreviewNote `json:"media_notes"`
	// DefaultsApplied are the account's defaults the post takes (those
	// it didn't set itself).
	DefaultsApplied v1AccountDefaults `json:"defaults_applied"`
}

type v1PreviewItem struct {
	Text  string `json:"text"`
	Media []int  `json:"media"`
	// Range is the code points of the target's text the post carries
	// (counter excluded); a media-only post's is empty at the end.
	Range [2]int `json:"range"`
	// Break says what starts the post: "manual" (a hand-made break),
	// "auto" (split to the limit) or "none" (the first post).
	Break string `json:"break"`
	// Length is the post as the destination counts it.
	Length int `json:"length"`
	// Problems are what the destination would refuse about the post.
	Problems []string `json:"problems"`
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
	if plan.upstream {
		problem(w, r, http.StatusBadGateway, codeUpstream, "the post's network could not be reached; try again")
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
		// Interactions carry no link cards (the engine plans none).
		var card *unfurl.Card
		if pt.caps.LinkPreview == destination.LinkPreviewClient && a.Unfurl != nil && plan.ix == nil {
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
		cp := dispatch.PlanTarget(pt.caps, dispatch.TargetInput{Text: pt.text, Card: card, Layout: plan.layoutOf(pt),
			Number: plan.spec.Number, ContentWarning: pt.ov.ContentWarning})
		vt := v1PreviewTarget{Account: pt.account.ID, Type: pt.account.Type, Role: "post", Items: []v1PreviewItem{},
			Text: cp.Text, Limit: dispatch.Rules(pt.caps, plan.spec.Number, pt.ov.ContentWarning).Limit,
			Warnings: append(append([]string{}, pt.warnings...), cp.Warnings...), Media: []v1PreviewNote{},
			DefaultsApplied: pt.defaults}
		if ix := plan.ix; ix != nil {
			vt.Role = ix.Action
			if pt.account.Type != ix.SourcePlatform {
				vt.Role = "link_quote"
				if n := sourceImagesWanted(plan); n > 0 && a.media.Enabled() && pt.caps.Media.Images.Supported {
					if c := pt.caps.Media.Images.MaxCount; c > 0 {
						n = max(0, min(n, c-len(plan.media)))
					}
					switch {
					case n > 0 && pt.caps.Media.Transport == destination.TransportURL && !a.media.Public():
						// As the post will (withoutUnreachableCopies): the
						// copies land on a disk the network can't fetch from.
						vt.Warnings = append(vt.Warnings, "the original's images aren't attached: "+displayName(pt.account.Type)+
							" fetches media by address, and this server's disk doesn't serve them publicly (Settings, Media)")
					case n > 0:
						vt.Warnings = append(vt.Warnings, "up to "+strconv.Itoa(n)+" of the original's images are copied to media storage and attached after yours when posting")
					}
				}
			}
			if ix.Action == "repost" {
				vt.Text = ""
				vt.Items = append(vt.Items, v1PreviewItem{Text: "", Media: []int{}, Break: "none", Problems: []string{}})
				out.Targets = append(out.Targets, vt)
				continue
			}
		}
		for i, it := range cp.Items {
			brk := "auto"
			switch {
			case i == 0:
				brk = "none"
			case it.Manual:
				brk = "manual"
			}
			vi := v1PreviewItem{Text: it.Text, Media: it.Media, Range: [2]int{it.From, it.To}, Break: brk,
				Length: it.Length, Problems: it.Problems}
			if vi.Media == nil {
				vi.Media = []int{}
			}
			if vi.Problems == nil {
				vi.Problems = []string{}
			}
			vt.Items = append(vt.Items, vi)
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

// ─── Thread layout ───────────────────────────────────────────────────────

// urlInText finds the URLs a break must not fall inside.
var urlInText = regexp.MustCompile(`https?://[^\s<>"]+`)

// breakable answers, for each code point position in text, whether a break
// may go there: between graphemes and not inside a URL.
func breakable(text string) []bool {
	n := utf8.RuneCountInString(text)
	ok := make([]bool, n+1)
	pos := 0
	g := uniseg.NewGraphemes(text)
	for g.Next() {
		ok[pos] = true
		pos += utf8.RuneCountInString(g.Str())
	}
	ok[n] = true
	for _, loc := range urlInText.FindAllStringIndex(text, -1) {
		a := utf8.RuneCountInString(text[:loc[0]])
		b := a + utf8.RuneCountInString(text[loc[0]:loc[1]])
		for i := a + 1; i < b; i++ {
			ok[i] = false
		}
	}
	return ok
}

// checkBreaks validates hand-made breaks in text: strictly ascending,
// inside the text, between graphemes and outside URLs, at most one fewer
// than the parts a thread may have.
func checkBreaks(text string, breaks []int, ptr string, add func(ptr, code, detail string)) {
	if len(breaks) == 0 {
		return
	}
	if len(breaks) >= maxThreadParts {
		add(ptr, "too_many", "at most 99 breaks")
		return
	}
	ok := breakable(text)
	n := len(ok) - 1
	prev := 0
	for i, b := range breaks {
		p := ptr + "/" + strconv.Itoa(i)
		switch {
		case b <= 0 || b >= n:
			add(p, "out_of_range", "a break is a code point position inside the text, from 1 to "+strconv.Itoa(max(n-1, 1)))
		case b <= prev:
			add(p, "not_ascending", "breaks must be in ascending order, each once")
		case !ok[b]:
			add(p, "inside_grapheme", "a break can't split a character or a link")
		}
		prev = max(prev, b)
	}
}

// checkAt validates a media position in text: from 0 to the text's length.
func checkAt(n int, at int, ptr string, add func(ptr, code, detail string)) {
	if at < 0 || at > n {
		add(ptr, "out_of_range", "a code point position in the text, from 0 to "+strconv.Itoa(n))
	}
}

// targetLayout is one target's breaks and media positions in the text it
// posts (pt.text, not yet normalized): its own thread fields where it has
// them, else the shared ones when it posts the shared text, else none. The
// slices are the target's own.
func targetLayout(req *v1CreatePostReq, t v1CreateTarget, own bool, text string, plan *createPlan,
	ptr string, add func(ptr, code, detail string)) thread.Layout {
	lay := thread.Layout{Breaks: []int{}}
	if !own {
		lay.Breaks = append(lay.Breaks, req.Content.Breaks...)
	}
	lay.MediaAt = make([]int, len(plan.media))
	if !own {
		copy(lay.MediaAt, plan.mediaAt)
	}
	th := t.Thread
	if th == nil {
		return lay
	}
	if th.Breaks != nil {
		checkBreaks(text, *th.Breaks, ptr+"/thread/breaks", add)
		lay.Breaks = append([]int{}, *th.Breaks...)
	}
	if th.MediaAt != nil {
		if len(th.MediaAt) != len(req.Content.Media) {
			add(ptr+"/thread/media_at", "length_mismatch", "one position per media in content.media ("+
				strconv.Itoa(len(req.Content.Media))+")")
			return lay
		}
		n := utf8.RuneCountInString(text)
		for j, k := range plan.mediaReq {
			checkAt(n, th.MediaAt[k], ptr+"/thread/media_at/"+strconv.Itoa(k), add)
			lay.MediaAt[j] = th.MediaAt[k]
		}
	}
	return lay
}
