package api

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/geofox/publisher/internal/store"
)

// /v1/posts: reading and managing posts. Creating them (POST /v1/posts,
// :preview) and media come in the next PR.

const (
	prefixPost   = "pst_"
	prefixTarget = "tgt_"
)

func postID(storeID string) string { return prefixPost + storeID }
func targetID(id int64) string     { return prefixTarget + strconv.FormatInt(id, 10) }
func parseTargetID(s string) (int64, bool) {
	rest, ok := parseID(s, prefixTarget)
	if !ok {
		return 0, false
	}
	n, err := strconv.ParseInt(rest, 10, 64)
	return n, err == nil && n > 0
}

// Status vocabulary (spec §6.5). The store keeps the /api words until the
// old routes go; the API speaks the new ones.

// v1PostStatuses maps each /v1 post status to the store statuses it covers.
var v1PostStatuses = map[string][]string{
	"scheduled":       {"scheduled"},
	"publishing":      {"sending"},
	"published":       {"success"},
	"partial":         {"partial"},
	"failed":          {"failed"},
	"canceled":        {"canceled", "cancelling"},
	"missed":          {"missed"},
	"needs_attention": nil, // computed: see store.attentionExpr
}

// v1PostStatusNames is v1PostStatuses' keys in documentation order.
var v1PostStatusNames = []string{"scheduled", "publishing", "published", "partial", "failed", "canceled", "missed", "needs_attention"}

// v1Status maps a store post or target status to /v1's.
func v1Status(s string) string {
	switch s {
	case "success":
		return "published"
	case "sending", "pending":
		return "publishing"
	case "cancelling":
		return "canceled"
	}
	return s
}

func v1PostStatus(p store.Post) string {
	if p.Attention {
		return "needs_attention"
	}
	return v1Status(p.Status)
}

// ─── resources ───────────────────────────────────────────────────────────

type v1MediaBody struct {
	URL          string `json:"url"`
	Mime         string `json:"mime"`
	Alt          string `json:"alt,omitempty"`
	SHA256       string `json:"sha256,omitempty"`
	SizeBytes    int64  `json:"size_bytes,omitempty"`
	Dim          string `json:"dim,omitempty"`
	Blurhash     string `json:"blurhash,omitempty"`
	DurationSecs int64  `json:"duration_secs,omitempty"`
	PosterURL    string `json:"poster_url,omitempty"`
}

type v1ContentBody struct {
	Text  string        `json:"text"`
	Media []v1MediaBody `json:"media"`
}

type v1InteractionBody struct {
	Action   string `json:"action"`
	URL      string `json:"url"`
	Platform string `json:"platform,omitempty"`
	Author   string `json:"author,omitempty"`
}

type v1DeliveryBody struct {
	Endpoint    string  `json:"endpoint"`
	Status      string  `json:"status"`
	Message     string  `json:"message,omitempty"`
	Retries     int     `json:"retries"`
	AttemptedAt *string `json:"attempted_at"`
	GaveUpAt    *string `json:"gave_up_at"`
}

type v1ItemBody struct {
	Ordinal       int              `json:"ordinal"`
	Status        string           `json:"status"`
	Text          string           `json:"text"`
	Media         []int            `json:"media"`
	URL           string           `json:"url,omitempty"`
	RemoteID      string           `json:"remote_id,omitempty"`
	Error         string           `json:"error,omitempty"`
	ErrorClass    string           `json:"error_class,omitempty"`
	Attempts      int              `json:"attempts"`
	NextAttemptAt *string          `json:"next_attempt_at"`
	PublishedAt   *string          `json:"published_at"`
	Deliveries    []v1DeliveryBody `json:"deliveries"`
}

type v1AttemptBody struct {
	Number      int    `json:"number"`
	Status      string `json:"status"`
	Error       string `json:"error,omitempty"`
	ErrorClass  string `json:"error_class,omitempty"`
	LatencyMS   int    `json:"latency_ms"`
	AttemptedAt string `json:"attempted_at"`
}

type v1TargetBody struct {
	ID         string          `json:"id"`
	Account    string          `json:"account"`
	Type       string          `json:"type"`
	Status     string          `json:"status"`
	ErrorClass string          `json:"error_class,omitempty"`
	GaveUpAt   *string         `json:"gave_up_at"`
	Items      []v1ItemBody    `json:"items,omitempty"`
	Attempts   []v1AttemptBody `json:"attempts,omitempty"`
}

type v1PostBody struct {
	ID          string             `json:"id"`
	Status      string             `json:"status"`
	CreatedAt   string             `json:"created_at"`
	ScheduledAt *string            `json:"scheduled_at"`
	Hidden      bool               `json:"hidden"`
	Source      string             `json:"source"`
	Content     v1ContentBody      `json:"content"`
	Interaction *v1InteractionBody `json:"interaction"`
	Targets     []v1TargetBody     `json:"targets"`
}

func rfc3339ptr(t *time.Time) *string {
	if t == nil {
		return nil
	}
	return rfc3339p(*t)
}

// v1Post renders a post. full adds items and attempts (GetPost loads
// them; ListPostsPage does not).
func v1Post(p store.Post, full bool) v1PostBody {
	b := v1PostBody{
		ID: postID(p.ID), Status: v1PostStatus(p), CreatedAt: rfc3339(p.CreatedAt), ScheduledAt: rfc3339ptr(p.ScheduledAt),
		Hidden: p.Hidden, Source: p.Source, Content: v1ContentBody{Text: p.MasterText, Media: []v1MediaBody{}},
		Targets: []v1TargetBody{},
	}
	for _, m := range p.Media {
		b.Content.Media = append(b.Content.Media, v1MediaBody{
			URL: m.BlossomURL, Mime: m.Mime, Alt: m.Alt, SHA256: m.SHA256, SizeBytes: m.SizeBytes, Dim: m.Dim,
			Blurhash: m.Blurhash, DurationSecs: m.DurationSecs, PosterURL: m.PosterURL,
		})
	}
	if ix := p.Interaction; ix != nil {
		b.Interaction = &v1InteractionBody{Action: ix.Action, URL: ix.SourceURL, Platform: ix.SourcePlatform, Author: ix.SourceAuthor}
	}
	for _, t := range p.Targets {
		tb := v1TargetBody{
			ID: targetID(t.ID), Account: t.AccountID, Type: t.Platform, Status: v1Status(t.Status),
			ErrorClass: t.ErrorClass, GaveUpAt: rfc3339ptr(t.GaveUpAt),
		}
		if full {
			tb.Items, tb.Attempts = []v1ItemBody{}, []v1AttemptBody{}
			for _, it := range t.Items {
				ib := v1ItemBody{
					Ordinal: it.Ordinal, Status: it.Status, Text: it.Text, Media: it.Media, URL: it.RemoteURL,
					RemoteID: it.RemoteID, Error: it.Error, ErrorClass: it.ErrorClass, Attempts: it.AttemptCount,
					NextAttemptAt: rfc3339ptr(it.NextAttemptAt), PublishedAt: rfc3339ptr(it.PublishedAt),
					Deliveries: []v1DeliveryBody{},
				}
				if ib.Media == nil {
					ib.Media = []int{}
				}
				if it.Status == store.ItemSending {
					ib.NextAttemptAt = nil // holds the send's start time, not a schedule
				}
				for _, d := range it.Deliveries {
					ib.Deliveries = append(ib.Deliveries, v1DeliveryBody{
						Endpoint: d.URL, Status: d.Status, Message: d.Message, Retries: d.RetryCount,
						AttemptedAt: rfc3339p(d.AttemptedAt), GaveUpAt: rfc3339ptr(d.GaveUpAt),
					})
				}
				tb.Items = append(tb.Items, ib)
			}
			for _, at := range t.Attempts {
				tb.Attempts = append(tb.Attempts, v1AttemptBody{
					Number: at.AttemptNo, Status: v1Status(at.Status), Error: at.Error, ErrorClass: at.ErrorClass,
					LatencyMS: at.LatencyMS, AttemptedAt: rfc3339(at.AttemptedAt),
				})
			}
		}
		b.Targets = append(b.Targets, tb)
	}
	return b
}

// v1OwnPost loads the post named by the path, answering 404 when it is
// missing or another owner's.
func (a *API) v1OwnPost(w http.ResponseWriter, r *http.Request) (*store.Post, bool) {
	if a.Store == nil {
		problem(w, r, http.StatusServiceUnavailable, codeUnavailable, "no database")
		return nil, false
	}
	id := r.PathValue("id")
	sid, ok := parseID(id, prefixPost)
	if !ok {
		problem(w, r, http.StatusNotFound, codeNotFound, "no post "+id)
		return nil, false
	}
	p, err := a.Store.GetPost(sid)
	pr, _ := principalFrom(r.Context())
	if errors.Is(err, sql.ErrNoRows) || (err == nil && p.OwnerID != pr.OwnerID) {
		problem(w, r, http.StatusNotFound, codeNotFound, "no post "+id)
		return nil, false
	}
	if err != nil {
		slog.ErrorContext(r.Context(), "v1: get post", "err", err)
		problem(w, r, http.StatusInternalServerError, codeInternal, "reading the post failed")
		return nil, false
	}
	return p, true
}

// reloadPost answers with the post as it is now.
func (a *API) reloadPost(w http.ResponseWriter, r *http.Request, id string, status int) {
	p, err := a.Store.GetPost(id)
	if err != nil {
		slog.ErrorContext(r.Context(), "v1: reload post", "err", err)
		problem(w, r, http.StatusInternalServerError, codeInternal, "reading the post failed")
		return
	}
	writeV1(w, status, v1Post(*p, true))
}

// ─── GET /v1/posts ───────────────────────────────────────────────────────

func (a *API) v1ListPosts(w http.ResponseWriter, r *http.Request) {
	limit, after, ok := pageParams(w, r)
	if !ok {
		return
	}
	pr, _ := principalFrom(r.Context())
	q := store.PostQuery{OwnerID: pr.OwnerID, Limit: limit + 1}
	vals := r.URL.Query()
	badParam := func(name, detail string) {
		problem(w, r, http.StatusBadRequest, codeInvalidParameter, detail,
			fieldError{Parameter: name, Code: "invalid", Detail: detail})
	}
	if s := vals.Get("status"); s != "" {
		for _, st := range strings.Split(s, ",") {
			mapped, known := v1PostStatuses[st]
			if !known {
				badParam("status", "status is a comma-separated list of "+strings.Join(v1PostStatusNames, ", "))
				return
			}
			if st == "needs_attention" {
				q.Attention = true
			}
			q.Statuses = append(q.Statuses, mapped...)
		}
	}
	if s := vals.Get("account"); s != "" {
		if !strings.HasPrefix(s, "acc_") {
			badParam("account", "account is an account id (acc_…)")
			return
		}
		q.AccountID = s
	}
	q.Text = vals.Get("q")
	for _, tp := range []struct {
		name string
		dst  *time.Time
	}{{"since", &q.Since}, {"until", &q.Until}} {
		if s := vals.Get(tp.name); s != "" {
			t, err := time.Parse(time.RFC3339, strings.ToUpper(s))
			if err != nil {
				badParam(tp.name, tp.name+" is an RFC 3339 time, such as 2026-11-01T00:00:00Z")
				return
			}
			*tp.dst = t
		}
	}
	switch vals.Get("hidden") {
	case "", "false":
	case "true":
		t := true
		q.Hidden = &t
	default:
		badParam("hidden", "hidden is true or false")
		return
	}
	if after != nil {
		sid, ok := parseID(after.ID, prefixPost)
		if !ok {
			problem(w, r, http.StatusBadRequest, codeInvalidCursor, "cursor is not one this server issued",
				fieldError{Parameter: "cursor", Code: "invalid", Detail: "pass next_cursor from the previous page unchanged"})
			return
		}
		q.AfterTime, q.AfterID = after.Time, sid
	}
	if a.Store == nil {
		problem(w, r, http.StatusServiceUnavailable, codeUnavailable, "no database")
		return
	}
	posts, err := a.Store.ListPostsPage(r.Context(), q)
	if err != nil {
		slog.ErrorContext(r.Context(), "v1: list posts", "err", err)
		problem(w, r, http.StatusInternalServerError, codeInternal, "listing posts failed")
		return
	}
	var next *cursor
	if len(posts) > limit {
		posts = posts[:limit]
		last := posts[len(posts)-1]
		next = &cursor{Time: last.CreatedAt, ID: postID(last.ID)}
	}
	out := make([]v1PostBody, 0, len(posts))
	for _, p := range posts {
		out = append(out, v1Post(p, false))
	}
	writeV1(w, http.StatusOK, newList(out, next))
}

// ─── GET /v1/posts/{id} ──────────────────────────────────────────────────

func (a *API) v1GetPost(w http.ResponseWriter, r *http.Request) {
	if p, ok := a.v1OwnPost(w, r); ok {
		writeV1(w, http.StatusOK, v1Post(*p, true))
	}
}

// ─── PATCH /v1/posts/{id} ────────────────────────────────────────────────

type v1PatchPostReq struct {
	Schedule *struct {
		At string `json:"at"`
	} `json:"schedule"`
	Hidden *bool `json:"hidden"`
}

// v1PatchPost reschedules a scheduled post, or hides or shows one. A JSON
// merge patch (RFC 7396) of the fields it names.
func (a *API) v1PatchPost(w http.ResponseWriter, r *http.Request) {
	var req v1PatchPostReq
	if !decodeJSON(w, r, &req) {
		return
	}
	var at time.Time
	var errs []fieldError
	if req.Schedule == nil && req.Hidden == nil {
		errs = append(errs, fieldError{Pointer: "", Code: "empty", Detail: "change schedule.at or hidden"})
	}
	if req.Schedule != nil {
		t, err := time.Parse(time.RFC3339, strings.ToUpper(req.Schedule.At))
		switch {
		case err != nil:
			errs = append(errs, fieldError{Pointer: "/schedule/at", Code: "invalid_time", Detail: "an RFC 3339 time, such as 2027-01-01T09:00:00Z"})
		case !t.After(time.Now()):
			errs = append(errs, fieldError{Pointer: "/schedule/at", Code: "in_past", Detail: "the new time must be in the future"})
		default:
			at = t
		}
	}
	if len(errs) > 0 {
		invalid(w, r, errs)
		return
	}
	p, ok := a.v1OwnPost(w, r)
	if !ok {
		return
	}
	if req.Schedule != nil {
		if err := a.Store.RescheduleScheduled(p.ID, at); err != nil {
			if errors.Is(err, store.ErrNotPending) {
				problem(w, r, http.StatusConflict, codeInvalidState, "only a scheduled post can be rescheduled; this one is "+v1PostStatus(*p))
				return
			}
			slog.ErrorContext(r.Context(), "v1: reschedule", "err", err)
			problem(w, r, http.StatusInternalServerError, codeInternal, "rescheduling failed")
			return
		}
	}
	if req.Hidden != nil {
		if err := a.Store.SetPostHidden(p.ID, *req.Hidden); err != nil {
			if errors.Is(err, store.ErrNotHideable) {
				problem(w, r, http.StatusConflict, codeInvalidState, "a post still to be sent can't be hidden; cancel it first")
				return
			}
			slog.ErrorContext(r.Context(), "v1: hide", "err", err)
			problem(w, r, http.StatusInternalServerError, codeInternal, "hiding failed")
			return
		}
	}
	a.reloadPost(w, r, p.ID, http.StatusOK)
}

// ─── POST /v1/posts/{id}:cancel and :retry ───────────────────────────────

// v1PostAction routes POST /v1/posts/{id}:{action}: ServeMux wildcards are
// whole segments, so the custom methods (AIP-136) are split off here.
func (a *API) v1PostAction(w http.ResponseWriter, r *http.Request) {
	id, action, found := strings.Cut(r.PathValue("id"), ":")
	if !found {
		w.Header().Set("Allow", "GET, HEAD, PATCH")
		problem(w, r, http.StatusMethodNotAllowed, codeMethodNotAllowed, "POST is not allowed here; use GET, HEAD, PATCH, or POST to :cancel or :retry")
		return
	}
	r.SetPathValue("id", id)
	switch action {
	case "cancel":
		a.v1CancelPost(w, r)
	case "retry":
		a.v1RetryPost(w, r)
	default:
		problem(w, r, http.StatusNotFound, codeNotFound, "no such action :"+action+"; posts take :cancel and :retry")
	}
}

// v1CancelPost cancels what a post hasn't sent: all of a scheduled post,
// or the rest of a thread being sent. Repeating it on a post with nothing
// left to send is a 409.
func (a *API) v1CancelPost(w http.ResponseWriter, r *http.Request) {
	p, ok := a.v1OwnPost(w, r)
	if !ok {
		return
	}
	err := a.Store.CancelPost(r.Context(), p.ID, time.Now())
	switch {
	case errors.Is(err, store.ErrNothingToCancel):
		problem(w, r, http.StatusConflict, codeInvalidState, "nothing of this post is waiting to be sent")
		return
	case err != nil:
		slog.ErrorContext(r.Context(), "v1: cancel", "err", err)
		problem(w, r, http.StatusInternalServerError, codeInternal, "canceling failed")
		return
	}
	a.reloadPost(w, r, p.ID, http.StatusOK)
}

type v1RetryReq struct {
	Targets    []string `json:"targets"`
	Deliveries []struct {
		Target   string `json:"target"`
		Endpoint string `json:"endpoint"`
	} `json:"deliveries"`
}

// v1RetryPost makes a post's failed items due again: of every retryable
// target, or of the targets named. Deliveries (Nostr relays) are retried
// one by one. It answers 202 at once; the engine sends within seconds and
// /v1/posts/{id}/events reports it.
func (a *API) v1RetryPost(w http.ResponseWriter, r *http.Request) {
	var req v1RetryReq
	if r.ContentLength != 0 || r.Header.Get("Content-Type") != "" {
		if !decodeJSON(w, r, &req) {
			return
		}
	}
	p, ok := a.v1OwnPost(w, r)
	if !ok {
		return
	}
	byID := map[int64]store.Target{}
	for _, t := range p.Targets {
		byID[t.ID] = t
	}
	var errs []fieldError
	var want []int64
	for i, s := range req.Targets {
		id, ok := parseTargetID(s)
		if _, mine := byID[id]; !ok || !mine {
			errs = append(errs, fieldError{Pointer: "/targets/" + strconv.Itoa(i), Code: "unknown_target", Detail: "not a target of this post"})
			continue
		}
		want = append(want, id)
	}
	type relayRetry struct {
		target   store.Target
		endpoint string
	}
	var relays []relayRetry
	for i, d := range req.Deliveries {
		id, ok := parseTargetID(d.Target)
		t, mine := byID[id]
		if !ok || !mine {
			errs = append(errs, fieldError{Pointer: "/deliveries/" + strconv.Itoa(i) + "/target", Code: "unknown_target", Detail: "not a target of this post"})
			continue
		}
		idx := slices.IndexFunc(t.Relays, func(rs store.RelayState) bool { return rs.URL == d.Endpoint })
		if idx < 0 || t.Relays[idx].Status != "failed" {
			errs = append(errs, fieldError{Pointer: "/deliveries/" + strconv.Itoa(i) + "/endpoint", Code: "not_retryable",
				Detail: "not a failed delivery of that target"})
			continue
		}
		relays = append(relays, relayRetry{t, d.Endpoint})
	}
	if len(errs) > 0 {
		invalid(w, r, errs)
		return
	}
	if len(relays) > 0 && a.Dispatch == nil {
		problem(w, r, http.StatusServiceUnavailable, codeUnavailable, "publishing is not running")
		return
	}
	var ids []int64
	if len(req.Deliveries) == 0 || len(req.Targets) > 0 {
		for _, t := range p.Targets {
			retryable := t.Status == "failed" || t.Status == "missed" || (t.Status == "partial" && len(t.Items) > 1)
			if retryable && (len(want) == 0 || slices.Contains(want, t.ID)) {
				ids = append(ids, t.ID)
			}
		}
	}
	due := 0
	if len(ids) > 0 {
		n, err := a.Store.MarkDue(r.Context(), p.ID, ids, time.Now())
		if err != nil {
			slog.ErrorContext(r.Context(), "v1: retry", "err", err)
			problem(w, r, http.StatusInternalServerError, codeInternal, "retrying failed")
			return
		}
		due = n
	}
	if due == 0 && len(relays) == 0 {
		problem(w, r, http.StatusConflict, codeInvalidState, "nothing of this post can be retried")
		return
	}
	if len(relays) > 0 && a.Dispatch != nil {
		ctx := context.WithoutCancel(r.Context())
		go func() {
			for _, rr := range relays {
				if _, err := a.Dispatch.RetryRelay(ctx, p.ID, rr.endpoint); err != nil {
					slog.WarnContext(ctx, "v1: relay retry", "post_id", p.ID, "relay", rr.endpoint, "err", err)
				}
			}
		}()
	}
	w.Header().Set("Location", "/v1/posts/"+postID(p.ID))
	a.reloadPost(w, r, p.ID, http.StatusAccepted)
}
