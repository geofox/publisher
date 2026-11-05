package api

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/geofox/publisher/internal/destination"
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
	"publishing":      {"sending", "pending"},
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
	ID         string  `json:"id"`
	Account    string  `json:"account"`
	Type       string  `json:"type"`
	Status     string  `json:"status"`
	ErrorClass string  `json:"error_class,omitempty"`
	GaveUpAt   *string `json:"gave_up_at"`
	// Items and Attempts are in a single post's view only (always set
	// there, [] when empty), not in lists.
	Items    *[]v1ItemBody    `json:"items,omitempty"`
	Attempts *[]v1AttemptBody `json:"attempts,omitempty"`
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
			items, attempts := []v1ItemBody{}, []v1AttemptBody{}
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
				items = append(items, ib)
			}
			for _, at := range t.Attempts {
				attempts = append(attempts, v1AttemptBody{
					Number: at.AttemptNo, Status: v1Status(at.Status), Error: at.Error, ErrorClass: at.ErrorClass,
					LatencyMS: at.LatencyMS, AttemptedAt: rfc3339(at.AttemptedAt),
				})
			}
			tb.Items, tb.Attempts = &items, &attempts
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
	if ss := vals["status"]; len(ss) > 0 {
		for _, st := range strings.Split(strings.Join(ss, ","), ",") {
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
			// An offset's unencoded + arrives as a space.
			t, err := time.Parse(time.RFC3339, strings.ToUpper(strings.ReplaceAll(s, " ", "+")))
			if err != nil {
				badParam(tp.name, tp.name+" is an RFC 3339 time, such as 2026-11-01T00:00:00Z (encode + as %2B)")
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
	Schedule json.RawMessage `json:"schedule"`
	Hidden   json.RawMessage `json:"hidden"`
}

// v1PatchPost reschedules a scheduled post, or hides or shows one. A JSON
// merge patch (RFC 7396) of the fields it names. Every check runs before
// any change, so a refused patch changes nothing.
func (a *API) v1PatchPost(w http.ResponseWriter, r *http.Request) {
	var req v1PatchPostReq
	if !decodeJSON(w, r, &req) {
		return
	}
	var at time.Time
	var hidden *bool
	var errs []fieldError
	isNull := func(m json.RawMessage) bool { return string(bytes.TrimSpace(m)) == "null" }
	wrongType := func(field string) {
		problem(w, r, http.StatusBadRequest, codeInvalidJSON, field+" has the wrong type")
	}
	if req.Schedule == nil && req.Hidden == nil {
		errs = append(errs, fieldError{Pointer: "", Code: "empty", Detail: "change schedule.at or hidden"})
	}
	if req.Schedule != nil {
		var sch struct {
			At *string `json:"at"`
		}
		if !isNull(req.Schedule) && json.Unmarshal(req.Schedule, &sch) != nil {
			wrongType("schedule")
			return
		}
		switch {
		case isNull(req.Schedule):
			errs = append(errs, fieldError{Pointer: "/schedule", Code: "null_not_allowed",
				Detail: "a scheduled post can't be unscheduled; cancel it, or set schedule.at"})
		case sch.At == nil:
			errs = append(errs, fieldError{Pointer: "/schedule/at", Code: "invalid_time", Detail: "an RFC 3339 time, such as 2027-01-01T09:00:00Z"})
		default:
			t, err := time.Parse(time.RFC3339, strings.ToUpper(*sch.At))
			switch {
			case err != nil:
				errs = append(errs, fieldError{Pointer: "/schedule/at", Code: "invalid_time", Detail: "an RFC 3339 time, such as 2027-01-01T09:00:00Z"})
			case !t.After(time.Now()):
				errs = append(errs, fieldError{Pointer: "/schedule/at", Code: "in_past", Detail: "the new time must be in the future"})
			default:
				at = t
			}
		}
	}
	if req.Hidden != nil {
		var h bool
		switch {
		case isNull(req.Hidden):
			errs = append(errs, fieldError{Pointer: "/hidden", Code: "null_not_allowed", Detail: "true or false"})
		case json.Unmarshal(req.Hidden, &h) != nil:
			wrongType("hidden")
			return
		default:
			hidden = &h
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
	if req.Schedule != nil && p.Status != "scheduled" {
		problem(w, r, http.StatusConflict, codeInvalidState, "only a scheduled post can be rescheduled; this one is "+v1PostStatus(*p))
		return
	}
	if hidden != nil && *hidden && !store.PostSettled(p.Status) {
		problem(w, r, http.StatusConflict, codeInvalidState, "a post still to be sent can't be hidden; cancel it first")
		return
	}
	if req.Schedule != nil {
		if err := a.Store.RescheduleScheduled(p.ID, at); err != nil {
			if errors.Is(err, store.ErrNotPending) {
				problem(w, r, http.StatusConflict, codeInvalidState, "only a scheduled post can be rescheduled; it has just started")
				return
			}
			slog.ErrorContext(r.Context(), "v1: reschedule", "err", err)
			problem(w, r, http.StatusInternalServerError, codeInternal, "rescheduling failed")
			return
		}
	}
	if hidden != nil {
		if err := a.Store.SetPostHidden(p.ID, *hidden); err != nil {
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
		problem(w, r, http.StatusMethodNotAllowed, codeMethodNotAllowed, "POST is not allowed here; use GET, HEAD, PATCH, or POST to :cancel, :retry or :resolve")
		return
	}
	r.SetPathValue("id", id)
	switch action {
	case "cancel":
		a.v1CancelPost(w, r)
	case "retry":
		a.v1RetryPost(w, r)
	case "resolve":
		a.v1ResolveItem(w, r)
	default:
		problem(w, r, http.StatusNotFound, codeNotFound, "no such action :"+action+"; posts take :cancel, :retry and :resolve")
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
	// The body is optional: an empty one is no body, whatever the headers
	// say (many clients send Content-Type on every POST).
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 64<<10))
	if err != nil {
		problem(w, r, http.StatusRequestEntityTooLarge, codeTooLarge, "the body is over 64 KiB")
		return
	}
	if len(bytes.TrimSpace(body)) > 0 {
		r.Body = io.NopCloser(bytes.NewReader(body))
		r.ContentLength = int64(len(body))
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
		if slices.Contains(want, id) {
			errs = append(errs, fieldError{Pointer: "/targets/" + strconv.Itoa(i), Code: "duplicate", Detail: "named twice"})
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
		// Each failed delivery once: a request can't queue more retries
		// than the post has failures.
		if slices.ContainsFunc(relays, func(rr relayRetry) bool { return rr.target.ID == t.ID && rr.endpoint == d.Endpoint }) {
			errs = append(errs, fieldError{Pointer: "/deliveries/" + strconv.Itoa(i), Code: "duplicate", Detail: "named twice"})
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
			retryable := t.Status == "failed" || t.Status == "missed" || t.Status == "canceled" ||
				(t.Status == "partial" && len(t.Items) > 1)
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
	if len(relays) > 0 {
		// The retries outlive the request but not the server.
		ctx, cancel := context.WithCancel(context.WithoutCancel(r.Context()))
		go func() {
			defer cancel()
			select {
			case <-a.Stopping:
				cancel()
			case <-ctx.Done():
			}
		}()
		go func() {
			defer cancel()
			for _, rr := range relays {
				if ctx.Err() != nil {
					return
				}
				if _, err := a.Dispatch.RetryTargetRelay(ctx, p.ID, rr.target.ID, rr.endpoint); err != nil {
					slog.WarnContext(ctx, "v1: relay retry", "post_id", p.ID, "relay", rr.endpoint, "err", err)
				}
			}
		}()
	}
	w.Header().Set("Location", "/v1/posts/"+postID(p.ID))
	a.reloadPost(w, r, p.ID, http.StatusAccepted)
}

// ─── POST /v1/posts/{id}:resolve ─────────────────────────────────────────

type v1ResolveReq struct {
	Target  string  `json:"target"`
	Ordinal *int    `json:"ordinal"`
	Outcome string  `json:"outcome"`
	URL     *string `json:"url"`
}

// v1ResolveItem records the owner's word on an item whose outcome is
// unknown: "published" (they found it; its URL lets a thread go on under
// it) or "not_published" (it is sent again).
func (a *API) v1ResolveItem(w http.ResponseWriter, r *http.Request) {
	var req v1ResolveReq
	if !decodeJSON(w, r, &req) {
		return
	}
	var errs []fieldError
	add := func(ptr, code, detail string) {
		errs = append(errs, fieldError{Pointer: ptr, Code: code, Detail: detail})
	}
	tid, ok := parseTargetID(req.Target)
	if !ok {
		add("/target", "invalid", "a target id of this post (tgt_…)")
	}
	switch {
	case req.Ordinal == nil:
		add("/ordinal", "required", "the item's ordinal in its target, from 0")
	case *req.Ordinal < 0 || *req.Ordinal >= maxThreadParts:
		add("/ordinal", "invalid", "the item's ordinal in its target, from 0")
	}
	if req.Outcome != "published" && req.Outcome != "not_published" {
		add("/outcome", "invalid", "published or not_published")
	}
	link := ""
	if req.URL != nil {
		link = strings.TrimSpace(*req.URL)
		switch {
		case req.Outcome == "not_published":
			add("/url", "not_allowed", "only a published item has a URL")
		case len(link) > maxSourceURL:
			add("/url", "too_long", "at most 2048 characters")
		case !isHTTPSURL(link):
			add("/url", "invalid", "the post's https URL")
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
	ti := slices.IndexFunc(p.Targets, func(t store.Target) bool { return t.ID == tid })
	if ti < 0 {
		invalid(w, r, []fieldError{{Pointer: "/target", Code: "unknown_target", Detail: "not a target of this post"}})
		return
	}
	t := p.Targets[ti]
	ii := slices.IndexFunc(t.Items, func(it store.Item) bool { return it.Ordinal == *req.Ordinal })
	if ii < 0 {
		invalid(w, r, []fieldError{{Pointer: "/ordinal", Code: "unknown_item", Detail: "the target has no item at this ordinal"}})
		return
	}
	if t.Items[ii].Status != store.ItemUnknown {
		problem(w, r, http.StatusConflict, codeInvalidState, "only an item whose outcome is unknown can be resolved; this one is "+t.Items[ii].Status)
		return
	}
	res := store.Resolution{PostID: p.ID, TargetID: t.ID, Ordinal: *req.Ordinal, Published: req.Outcome == "published",
		RemoteURL: link, Continue: req.Outcome == "not_published"}
	rest := slices.ContainsFunc(t.Items, func(it store.Item) bool {
		return it.Ordinal > *req.Ordinal && (it.Status == store.ItemPlanned || it.Status == store.ItemFailed)
	})
	if res.Published && a.Locator != nil && a.Locator.CanLocate(t.Platform) {
		switch {
		case link != "":
			lctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
			ref, err := a.Locator.Locate(lctx, t.Platform, link)
			cancel()
			switch {
			case errors.Is(err, destination.ErrPostNotFound):
				invalid(w, r, []fieldError{{Pointer: "/url", Code: "not_found", Detail: "no post at that URL, or it isn't visible to the account"}})
				return
			case errors.Is(err, destination.ErrNotOwnPost):
				invalid(w, r, []fieldError{{Pointer: "/url", Code: "not_own", Detail: "that post is another account's"}})
				return
			case err != nil:
				slog.InfoContext(r.Context(), "v1: locate", "post_id", p.ID, "err", store.Scrub(err.Error()))
				e := destination.Classify(err)
				if e != nil && e.Class == destination.ClassAuth {
					invalid(w, r, []fieldError{{Pointer: "/url", Code: "not_permitted",
						Detail: "the account can't look posts up: check its credentials and scopes (Mastodon needs read:search)"}})
					return
				}
				if e != nil && (e.Class == destination.ClassInvalid || e.Class == destination.ClassRejected) {
					invalid(w, r, []fieldError{{Pointer: "/url", Code: "invalid", Detail: "not a URL of a post on this account's network"}})
					return
				}
				problem(w, r, http.StatusBadGateway, codeUpstream, "the post's network couldn't be reached to look it up; try again")
				return
			case ref.ID != "" && slices.ContainsFunc(t.Items, func(x store.Item) bool { return x.RemoteID == ref.ID }):
				invalid(w, r, []fieldError{{Pointer: "/url", Code: "duplicate", Detail: "that is another part of this thread"}})
				return
			default:
				res.RemoteID, res.Ref, res.Continue = ref.ID, ref.Data, ref.ID != ""
				if ref.URL != "" {
					res.RemoteURL = ref.URL
				}
			}
		case rest:
			invalid(w, r, []fieldError{{Pointer: "/url", Code: "required",
				Detail: "give the post's URL, so the rest of the thread can go on under it"}})
			return
		}
	}
	if err := a.Store.ResolveItem(r.Context(), res); err != nil {
		if errors.Is(err, store.ErrNotUnknown) {
			problem(w, r, http.StatusConflict, codeInvalidState, "only an item whose outcome is unknown can be resolved; it has just changed")
			return
		}
		slog.ErrorContext(r.Context(), "v1: resolve item", "err", err)
		problem(w, r, http.StatusInternalServerError, codeInternal, "recording the outcome failed")
		return
	}
	a.reloadPost(w, r, p.ID, http.StatusOK)
}

// isHTTPSURL reports whether s is an absolute https URL with a host and no
// user info. Where posts can't be looked up the URL is stored as given and
// shown as the post's link (also in the public feed), so plain http is out.
func isHTTPSURL(s string) bool {
	u, err := url.Parse(s)
	return err == nil && u.Scheme == "https" && u.Host != "" && u.User == nil
}
