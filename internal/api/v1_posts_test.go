package api

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/geofox/publisher/internal/store"
)

// seedPost saves a post with one target per platform, all in status (the
// store's words), created at the time it is seeded.
func (e *v1Env) seedPost(id, status string, platforms ...string) *store.Post {
	e.t.Helper()
	return e.seedPostAt(id, status, time.Now().UTC(), platforms...)
}

func (e *v1Env) seedPostAt(id, status string, at time.Time, platforms ...string) *store.Post {
	e.t.Helper()
	p := &store.Post{ID: id, CreatedAt: at, MasterText: "text of " + id, Platforms: platforms, Source: "test", Status: status}
	for _, pl := range platforms {
		tg := store.Target{Platform: pl, Status: status, FinalText: "text of " + id}
		if status == "success" {
			tg.RemoteID, tg.RemoteURL = "r-"+id, "https://example.com/"+id
			tg.Attempts = []store.Attempt{{AttemptNo: 1, Status: "success", AttemptedAt: at}}
		}
		if status == "failed" {
			tg.ErrorClass = "transient"
			tg.Attempts = []store.Attempt{{AttemptNo: 1, Status: "failed", Error: "503", ErrorClass: "transient", AttemptedAt: at}}
		}
		p.Targets = append(p.Targets, tg)
	}
	if err := e.st.SavePost(p); err != nil {
		e.t.Fatal(err)
	}
	got, err := e.st.GetPost(id)
	if err != nil {
		e.t.Fatal(err)
	}
	return got
}

func (e *v1Env) seedScheduled(id string, at time.Time, platforms ...string) *store.Post {
	e.t.Helper()
	p := &store.Post{ID: id, CreatedAt: time.Now().UTC(), MasterText: "later", Platforms: platforms, Source: "test",
		Status: "scheduled", ScheduledAt: &at}
	for _, pl := range platforms {
		p.Targets = append(p.Targets, store.Target{Platform: pl, Status: "scheduled", FinalText: "later"})
	}
	if err := e.st.SavePost(p); err != nil {
		e.t.Fatal(err)
	}
	got, err := e.st.GetPost(id)
	if err != nil {
		e.t.Fatal(err)
	}
	return got
}

type postList struct {
	Data       []v1PostBody `json:"data"`
	NextCursor *string      `json:"next_cursor"`
}

func listIDs(l postList) []string {
	var ids []string
	for _, p := range l.Data {
		ids = append(ids, p.ID)
	}
	return ids
}

func TestV1ListPosts(t *testing.T) {
	e := newV1Env(t, true)
	base := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	e.seedPostAt("a", "success", base, "bluesky")
	e.seedPostAt("b", "failed", base.Add(time.Hour), "mastodon")
	e.seedPostAt("c", "partial", base.Add(2*time.Hour), "bluesky", "mastodon")
	e.seedPostAt("d", "success", base.Add(3*time.Hour), "bluesky")
	if err := e.st.SetPostHidden("d", true); err != nil {
		t.Fatal(err)
	}
	e.seedScheduled("s", time.Now().Add(time.Hour), "bluesky")

	var l postList
	r := e.do(req{method: "GET", path: "/v1/posts"})
	wantStatus(t, r, 200)
	r.json(&l)
	if got := strings.Join(listIDs(l), ","); got != "pst_s,pst_c,pst_b,pst_a" || l.NextCursor != nil {
		t.Fatalf("posts = %s, next %v", got, l.NextCursor)
	}
	if c := l.Data[1]; c.Status != "partial" || len(c.Targets) != 2 || c.Targets[0].Items != nil || !strings.HasPrefix(c.Targets[0].ID, "tgt_") ||
		!strings.HasPrefix(c.Targets[0].Account, "acc_") || c.Content.Text != "text of c" {
		t.Fatalf("post c = %+v", c)
	}
	if l.Data[0].Status != "scheduled" || l.Data[0].ScheduledAt == nil || l.Data[3].Status != "published" {
		t.Fatalf("statuses = %s, %s", l.Data[0].Status, l.Data[3].Status)
	}

	cases := []struct{ query, want string }{
		{"status=published", "pst_a"},
		{"status=failed,partial", "pst_c,pst_b"},
		{"status=scheduled", "pst_s"},
		{"hidden=true", "pst_d"},
		{"hidden=false", "pst_s,pst_c,pst_b,pst_a"},
		{"q=of+b", "pst_b"},
		{"since=2026-11-01T13:00:00Z", "pst_s,pst_c,pst_b"},
		{"until=2026-11-01T13:00:00z", "pst_a"},
		{"account=" + l.Data[1].Targets[1].Account, "pst_c,pst_b"},
		{"status=needs_attention", ""},
	}
	for _, c := range cases {
		var got postList
		r := e.do(req{method: "GET", path: "/v1/posts?" + c.query})
		wantStatus(t, r, 200)
		r.json(&got)
		if ids := strings.Join(listIDs(got), ","); ids != c.want {
			t.Errorf("?%s = %s, want %s", c.query, ids, c.want)
		}
	}

	// Pages follow each other without gaps or repeats.
	var seen []string
	path := "/v1/posts?limit=1"
	for i := 0; i < 10; i++ {
		var page postList
		r := e.do(req{method: "GET", path: path})
		wantStatus(t, r, 200)
		r.json(&page)
		seen = append(seen, listIDs(page)...)
		if page.NextCursor == nil {
			break
		}
		path = "/v1/posts?limit=1&cursor=" + *page.NextCursor
	}
	if got := strings.Join(seen, ","); got != "pst_s,pst_c,pst_b,pst_a" {
		t.Fatalf("paged = %s", got)
	}

	for _, bad := range []struct{ query, param, code string }{
		{"status=success", "status", codeInvalidParameter},
		{"account=bluesky", "account", codeInvalidParameter},
		{"since=yesterday", "since", codeInvalidParameter},
		{"hidden=maybe", "hidden", codeInvalidParameter},
		{"cursor=bm9wZQ", "cursor", codeInvalidCursor},
	} {
		r := e.do(req{method: "GET", path: "/v1/posts?" + bad.query, badRequest: true})
		p := wantProblem(t, r, 400, bad.code)
		if len(p.Errors) != 1 || p.Errors[0].Parameter != bad.param {
			t.Errorf("?%s: errors = %+v", bad.query, p.Errors)
		}
	}
}

// A post the engine gave up on with an item unsent needs attention, and
// the filter finds it.
func TestV1PostsNeedingAttention(t *testing.T) {
	e := newV1Env(t, true)
	ctx := context.Background()
	now := time.Now().UTC()
	p := &store.Post{ID: "x", CreatedAt: now, Platforms: []string{"bluesky"}, Source: "test", Status: "sending",
		Targets: []store.Target{{Platform: "bluesky", Status: "sending", Items: []store.Item{
			{Ordinal: 0, Text: "x", Status: store.ItemPlanned, IdemKey: store.ItemKey("x", "bluesky", 0), NextAttemptAt: &now}}}}}
	if err := e.st.SavePost(p); err != nil {
		t.Fatal(err)
	}
	c, err := e.st.ClaimItem(ctx, now, store.ClaimFilter{})
	if err != nil || c == nil {
		t.Fatalf("claim = %v, %v", c, err)
	}
	if _, err := e.st.RecordItemResult(ctx, store.ItemResult{Claim: *c, Status: store.ItemFailed, ErrorClass: "permanent", GiveUp: true}); err != nil {
		t.Fatal(err)
	}
	e.seedPost("ok", "success", "bluesky")

	var l postList
	r := e.do(req{method: "GET", path: "/v1/posts?status=needs_attention"})
	wantStatus(t, r, 200)
	r.json(&l)
	if ids := strings.Join(listIDs(l), ","); ids != "pst_x" || l.Data[0].Status != "needs_attention" {
		t.Fatalf("needs_attention = %s (%+v)", ids, l.Data)
	}
	r = e.do(req{method: "GET", path: "/v1/posts?status=failed"})
	r.json(&l)
	if len(l.Data) != 0 {
		t.Fatalf("a post needing attention is also listed as failed: %+v", l.Data)
	}

	var got v1PostBody
	r = e.do(req{method: "GET", path: "/v1/posts/pst_x"})
	wantStatus(t, r, 200)
	r.json(&got)
	tg := got.Targets[0]
	if got.Status != "needs_attention" || tg.Status != "failed" || tg.GaveUpAt == nil || len((*tg.Items)) != 1 ||
		(*tg.Items)[0].Status != "failed" || (*tg.Items)[0].ErrorClass != "permanent" || len((*tg.Attempts)) != 1 {
		t.Fatalf("post = %+v", got)
	}
}

func TestV1GetPost(t *testing.T) {
	e := newV1Env(t, true)
	e.seedPost("a", "success", "bluesky")

	var got v1PostBody
	r := e.do(req{method: "GET", path: "/v1/posts/pst_a"})
	wantStatus(t, r, 200)
	r.json(&got)
	tg := got.Targets[0]
	if got.ID != "pst_a" || got.Status != "published" || got.Hidden || tg.Type != "bluesky" || tg.Status != "published" {
		t.Fatalf("post = %+v", got)
	}
	if len((*tg.Items)) != 1 || (*tg.Items)[0].Status != "published" || (*tg.Items)[0].URL != "https://example.com/a" ||
		(*tg.Items)[0].PublishedAt == nil || len((*tg.Attempts)) != 1 || (*tg.Attempts)[0].Status != "published" {
		t.Fatalf("target = %+v", tg)
	}

	for _, path := range []string{"/v1/posts/pst_nope", "/v1/posts/a", "/v1/posts/pst_"} {
		wantProblem(t, e.do(req{method: "GET", path: path}), 404, codeNotFound)
	}
}

func TestV1PostScopes(t *testing.T) {
	e := newV1Env(t, true)
	e.seedPost("a", "success", "bluesky")
	read := e.token("posts:read")
	wantStatus(t, e.do(req{method: "GET", path: "/v1/posts", auth: read}), 200)
	wantStatus(t, e.do(req{method: "GET", path: "/v1/posts/pst_a", auth: read}), 200)
	wantProblem(t, e.do(req{method: "PATCH", path: "/v1/posts/pst_a", auth: read, body: `{"hidden":true}`}), 403, codeInsufficientScope)
	wantProblem(t, e.do(req{method: "POST", path: "/v1/posts/pst_a:cancel", auth: read}), 403, codeInsufficientScope)
	wantProblem(t, e.do(req{method: "POST", path: "/v1/posts/pst_a:retry", auth: read}), 403, codeInsufficientScope)

	media := e.token("media:write")
	wantProblem(t, e.do(req{method: "GET", path: "/v1/posts", auth: media}), 403, codeInsufficientScope)
	wantProblem(t, e.do(req{method: "GET", path: "/v1/events", auth: media}), 403, codeInsufficientScope)
	wantProblem(t, e.do(req{method: "GET", path: "/v1/posts/pst_a/events", auth: media}), 403, codeInsufficientScope)

	write := e.token("posts:write")
	wantStatus(t, e.do(req{method: "PATCH", path: "/v1/posts/pst_a", auth: write, body: `{"hidden":true}`}), 200)
}

func TestV1PatchPost(t *testing.T) {
	e := newV1Env(t, true)
	e.seedPost("a", "success", "bluesky")
	e.seedScheduled("s", time.Now().Add(time.Hour), "bluesky")

	var got v1PostBody
	r := e.do(req{method: "PATCH", path: "/v1/posts/pst_s", body: `{"schedule":{"at":"2099-01-01T09:00:00+01:00"}}`})
	wantStatus(t, r, 200)
	r.json(&got)
	if got.ScheduledAt == nil || *got.ScheduledAt != "2099-01-01T08:00:00Z" || got.Status != "scheduled" ||
		(*got.Targets[0].Items)[0].NextAttemptAt == nil || *(*got.Targets[0].Items)[0].NextAttemptAt != "2099-01-01T08:00:00Z" {
		t.Fatalf("rescheduled = %+v", got)
	}

	// A merge patch is welcome too.
	r = e.do(req{method: "PATCH", path: "/v1/posts/pst_a", body: `{"hidden":true}`,
		header: map[string]string{"Content-Type": "application/merge-patch+json"}})
	wantStatus(t, r, 200)
	r.json(&got)
	if !got.Hidden {
		t.Fatalf("not hidden: %+v", got)
	}
	r = e.do(req{method: "PATCH", path: "/v1/posts/pst_a", body: `{"hidden":false}`})
	wantStatus(t, r, 200)
	r.json(&got)
	if got.Hidden {
		t.Fatalf("still hidden: %+v", got)
	}

	for _, c := range []struct{ body, pointer, code string }{
		{`{}`, "", "empty"},
		{`{"schedule":{"at":"tomorrow"}}`, "/schedule/at", "invalid_time"},
		{`{"schedule":{"at":"2001-01-01T00:00:00Z"}}`, "/schedule/at", "in_past"},
	} {
		r := e.do(req{method: "PATCH", path: "/v1/posts/pst_s", body: c.body, badRequest: true})
		p := wantProblem(t, r, 422, codeValidation)
		if len(p.Errors) != 1 || p.Errors[0].Pointer != c.pointer || p.Errors[0].Code != c.code {
			t.Errorf("%s: errors = %+v", c.body, p.Errors)
		}
	}
	wantProblem(t, e.do(req{method: "PATCH", path: "/v1/posts/pst_s", body: `{"hidden":"yes"}`, badRequest: true}), 400, codeInvalidJSON)
	wantProblem(t, e.do(req{method: "PATCH", path: "/v1/posts/pst_a", body: `{"schedule":{"at":"2099-01-01T09:00:00Z"}}`}), 409, codeInvalidState)
	wantProblem(t, e.do(req{method: "PATCH", path: "/v1/posts/pst_s", body: `{"hidden":true}`}), 409, codeInvalidState)
	wantProblem(t, e.do(req{method: "PATCH", path: "/v1/posts/pst_nope", body: `{"hidden":true}`}), 404, codeNotFound)
}

func TestV1CancelPost(t *testing.T) {
	e := newV1Env(t, true)
	e.seedScheduled("s", time.Now().Add(time.Hour), "bluesky", "mastodon")
	e.seedPost("a", "success", "bluesky")

	var got v1PostBody
	r := e.do(req{method: "POST", path: "/v1/posts/pst_s:cancel"})
	wantStatus(t, r, 200)
	r.json(&got)
	if got.Status != "canceled" || got.Targets[0].Status != "canceled" || (*got.Targets[0].Items)[0].Status != "canceled" {
		t.Fatalf("canceled = %+v", got)
	}
	// It stays in the history, unlike the old /cancel.
	wantStatus(t, e.do(req{method: "GET", path: "/v1/posts/pst_s"}), 200)
	wantProblem(t, e.do(req{method: "POST", path: "/v1/posts/pst_s:cancel"}), 409, codeInvalidState)
	wantProblem(t, e.do(req{method: "POST", path: "/v1/posts/pst_a:cancel"}), 409, codeInvalidState)
	wantProblem(t, e.do(req{method: "POST", path: "/v1/posts/pst_nope:cancel"}), 404, codeNotFound)
	// A canceled post can be hidden.
	wantStatus(t, e.do(req{method: "PATCH", path: "/v1/posts/pst_s", body: `{"hidden":true}`}), 200)
}

func TestV1PostActionRouting(t *testing.T) {
	e := newV1Env(t, true)
	e.seedPost("a", "success", "bluesky")
	r := e.do(req{method: "POST", path: "/v1/posts/pst_a", undocumented: true})
	wantProblem(t, r, 405, codeMethodNotAllowed)
	if allow := r.Header().Get("Allow"); allow != "GET, HEAD, PATCH" {
		t.Errorf("Allow = %q", allow)
	}
	wantProblem(t, e.do(req{method: "POST", path: "/v1/posts/pst_a:publish", undocumented: true}), 404, codeNotFound)
	wantProblem(t, e.do(req{method: "POST", path: "/v1/posts/pst_a:", undocumented: true}), 404, codeNotFound)
	wantProblem(t, e.do(req{method: "GET", path: "/v1/posts/pst_a:cancel", undocumented: true}), 404, codeNotFound)
}

func TestV1RetryPost(t *testing.T) {
	e := newV1Env(t, true)
	e.seedPost("f", "failed", "bluesky", "mastodon")
	e.seedPost("a", "success", "bluesky")
	f, _ := e.st.GetPost("f")

	// Only the named target is retried.
	var got v1PostBody
	r := e.do(req{method: "POST", path: "/v1/posts/pst_f:retry", body: `{"targets":["` + targetID(f.Targets[1].ID) + `"]}`})
	wantStatus(t, r, 202)
	if loc := r.Header().Get("Location"); loc != "/v1/posts/pst_f" {
		t.Errorf("Location = %q", loc)
	}
	r.json(&got)
	if got.Status != "publishing" || got.Targets[0].Status != "failed" || got.Targets[1].Status != "publishing" ||
		(*got.Targets[1].Items)[0].NextAttemptAt == nil {
		t.Fatalf("retried = %+v", got)
	}
	// Without a body, every retryable target.
	r = e.do(req{method: "POST", path: "/v1/posts/pst_f:retry"})
	wantStatus(t, r, 202)
	r.json(&got)
	if got.Targets[0].Status != "publishing" {
		t.Fatalf("retried all = %+v", got)
	}
	wantProblem(t, e.do(req{method: "POST", path: "/v1/posts/pst_f:retry"}), 409, codeInvalidState)
	wantProblem(t, e.do(req{method: "POST", path: "/v1/posts/pst_a:retry"}), 409, codeInvalidState)

	a, _ := e.st.GetPost("a")
	wantProblem(t, e.do(req{method: "POST", path: "/v1/posts/pst_a:retry", body: `{"targets":["` + targetID(a.Targets[0].ID) + `"]}`}), 409, codeInvalidState)
	for _, c := range []struct{ body, pointer string }{
		{`{"targets":["` + targetID(f.Targets[0].ID) + `"]}`, "/targets/0"}, // another post's
		{`{"targets":["nope"]}`, "/targets/0"},
		{`{"deliveries":[{"target":"tgt_999","endpoint":"wss://x"}]}`, "/deliveries/0/target"},
		{`{"deliveries":[{"target":"` + targetID(a.Targets[0].ID) + `","endpoint":"wss://x"}]}`, "/deliveries/0/endpoint"},
	} {
		r := e.do(req{method: "POST", path: "/v1/posts/pst_a:retry", body: c.body, badRequest: true})
		p := wantProblem(t, r, 422, codeValidation)
		if len(p.Errors) != 1 || p.Errors[0].Pointer != c.pointer {
			t.Errorf("%s: errors = %+v", c.body, p.Errors)
		}
	}
}

// sseEvent is one event of a text/event-stream body.
type sseEvent struct {
	ID, Event string
	Data      v1EventBody
}

func parseSSE(t *testing.T, body string) (retry string, events []sseEvent) {
	t.Helper()
	for _, block := range strings.Split(body, "\n\n") {
		if block == "" || strings.HasPrefix(block, ":") {
			continue
		}
		var ev sseEvent
		for _, line := range strings.Split(block, "\n") {
			k, v, _ := strings.Cut(line, ": ")
			switch k {
			case "retry":
				retry = v
			case "id":
				ev.ID = v
			case "event":
				ev.Event = v
			case "data":
				if err := json.Unmarshal([]byte(v), &ev.Data); err != nil {
					t.Fatalf("data %q: %v", v, err)
				}
			default:
				t.Fatalf("unexpected line %q", line)
			}
		}
		if ev.ID != "" {
			events = append(events, ev)
		}
	}
	return retry, events
}

func sseTypes(evs []sseEvent) string {
	var out []string
	for _, e := range evs {
		out = append(out, e.Event)
	}
	return strings.Join(out, ",")
}

func TestV1PostEvents(t *testing.T) {
	e := newV1Env(t, true)
	e.seedScheduled("s", time.Now().Add(time.Hour), "bluesky")
	e.seedPost("other", "success", "bluesky")
	wantStatus(t, e.do(req{method: "POST", path: "/v1/posts/pst_s:cancel"}), 200)

	// A settled post's stream replays its events and ends.
	r := e.do(req{method: "GET", path: "/v1/posts/pst_s/events"})
	wantStatus(t, r, 200)
	if ct := r.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("Content-Type = %q", ct)
	}
	retry, evs := parseSSE(t, r.Body.String())
	if retry != "3000" || sseTypes(evs) != "post.created,item.canceled,post.updated,post.completed" {
		t.Fatalf("retry %q, events %s", retry, sseTypes(evs))
	}
	for _, ev := range evs {
		if ev.ID != strconv64(ev.Data.ID) || ev.Event != ev.Data.Type || ev.Data.Post != "pst_s" || ev.Data.CreatedAt == "" {
			t.Fatalf("event = %+v", ev)
		}
	}
	if d := evs[0].Data; d.Status != "scheduled" || d.ScheduledAt == nil {
		t.Fatalf("created = %+v", d)
	}
	if d := evs[1].Data; d.Status != "canceled" || !strings.HasPrefix(d.Target, "tgt_") || !strings.HasPrefix(d.Account, "acc_") ||
		d.Destination != "bluesky" || d.Ordinal == nil || *d.Ordinal != 0 {
		t.Fatalf("item = %+v", d)
	}
	if evs[3].Data.Status != "canceled" {
		t.Fatalf("completed = %+v", evs[3].Data)
	}

	// Resuming skips what the client has.
	for _, rq := range []req{
		{method: "GET", path: "/v1/posts/pst_s/events", header: map[string]string{"Last-Event-ID": evs[1].ID}},
		{method: "GET", path: "/v1/posts/pst_s/events?after=" + evs[1].ID},
	} {
		r := e.do(rq)
		wantStatus(t, r, 200)
		if _, got := parseSSE(t, r.Body.String()); sseTypes(got) != "post.updated,post.completed" {
			t.Fatalf("resumed = %s", sseTypes(got))
		}
	}
	p := wantProblem(t, e.do(req{method: "GET", path: "/v1/posts/pst_s/events", header: map[string]string{"Last-Event-ID": "x"}, badRequest: true}), 400, codeInvalidParameter)
	if p.Errors[0].Parameter != "Last-Event-ID" {
		t.Fatalf("errors = %+v", p.Errors)
	}
	wantProblem(t, e.do(req{method: "GET", path: "/v1/posts/pst_s/events?after=-1", badRequest: true}), 400, codeInvalidParameter)
	wantProblem(t, e.do(req{method: "GET", path: "/v1/posts/pst_nope/events"}), 404, codeNotFound)
}

// A post still being sent keeps its stream open and sends what happens.
func TestV1PostEventsFollowsAPost(t *testing.T) {
	e := newV1Env(t, true)
	ctx := context.Background()
	now := time.Now().UTC()
	p := &store.Post{ID: "x", CreatedAt: now, Platforms: []string{"bluesky"}, Source: "test", Status: "sending",
		Targets: []store.Target{{Platform: "bluesky", Status: "sending", Items: []store.Item{
			{Ordinal: 0, Text: "x", Status: store.ItemPlanned, IdemKey: store.ItemKey("x", "bluesky", 0), NextAttemptAt: &now}}}}}
	if err := e.st.SavePost(p); err != nil {
		t.Fatal(err)
	}
	go func() {
		time.Sleep(100 * time.Millisecond)
		c, err := e.st.ClaimItem(ctx, now, store.ClaimFilter{})
		if err != nil || c == nil {
			return
		}
		_, _ = e.st.RecordItemResult(ctx, store.ItemResult{Claim: *c, Status: store.ItemPublished, RemoteID: "r", RemoteURL: "https://bsky.app/x"})
	}()
	stop, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	start := time.Now()
	r := e.do(req{method: "GET", path: "/v1/posts/pst_x/events", ctx: stop})
	wantStatus(t, r, 200)
	if time.Since(start) > 4*time.Second {
		t.Fatal("the stream didn't end when the post settled")
	}
	_, evs := parseSSE(t, r.Body.String())
	if sseTypes(evs) != "post.created,item.sending,item.published,post.updated,post.completed" {
		t.Fatalf("events = %s", sseTypes(evs))
	}
	if d := evs[2].Data; d.Status != "published" || d.URL != "https://bsky.app/x" {
		t.Fatalf("published = %+v", d)
	}
	if d := evs[3].Data; d.Status != "published" {
		t.Fatalf("post.updated status = %q, want /v1's word", d.Status)
	}
}

// The global stream starts now, unless asked to resume.
func TestV1Events(t *testing.T) {
	e := newV1Env(t, true)
	e.seedPost("old", "success", "bluesky")
	go func() {
		time.Sleep(100 * time.Millisecond)
		e.seedPost("new", "success", "mastodon")
	}()
	stop, cancel := context.WithTimeout(context.Background(), 600*time.Millisecond)
	defer cancel()
	r := e.do(req{method: "GET", path: "/v1/events", ctx: stop})
	wantStatus(t, r, 200)
	_, evs := parseSSE(t, r.Body.String())
	if sseTypes(evs) != "post.created,post.completed" || evs[0].Data.Post != "pst_new" || evs[1].Data.Status != "published" {
		t.Fatalf("events = %+v", evs)
	}

	stop, cancel = context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	r = e.do(req{method: "GET", path: "/v1/events?after=0", ctx: stop})
	_, evs = parseSSE(t, r.Body.String())
	if len(evs) != 4 || evs[0].Data.Post != "pst_old" {
		t.Fatalf("replayed = %+v", evs)
	}
}

func strconv64(n int64) string {
	b, _ := json.Marshal(n)
	return string(b)
}

// A stopping server ends its streams rather than wait for the clients.
func TestV1EventsEndOnShutdown(t *testing.T) {
	e := newV1Env(t, true)
	stopping := make(chan struct{})
	e.api.Stopping = stopping
	go func() {
		time.Sleep(100 * time.Millisecond)
		close(stopping)
	}()
	stop, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	start := time.Now()
	wantStatus(t, e.do(req{method: "GET", path: "/v1/events", ctx: stop}), 200)
	if time.Since(start) > 4*time.Second {
		t.Fatal("the stream outlived the server")
	}
}

// ─── review regressions ─────────────────────────────────────────────────

// A refused patch changes nothing.
func TestV1PatchIsAllOrNothing(t *testing.T) {
	e := newV1Env(t, true)
	s := e.seedScheduled("s", time.Now().Add(time.Hour), "bluesky")
	wantProblem(t, e.do(req{method: "PATCH", path: "/v1/posts/pst_s", body: `{"schedule":{"at":"2099-01-01T09:00:00Z"},"hidden":true}`}),
		409, codeInvalidState)
	got, _ := e.st.GetPost("s")
	if !got.ScheduledAt.Equal(*s.ScheduledAt) {
		t.Fatalf("scheduled_at moved to %v by a refused patch", got.ScheduledAt)
	}
	for _, c := range []struct{ body, pointer string }{
		{`{"schedule":null}`, "/schedule"},
		{`{"hidden":null}`, "/hidden"},
		{`{"schedule":{}}`, "/schedule/at"},
	} {
		p := wantProblem(t, e.do(req{method: "PATCH", path: "/v1/posts/pst_s", body: c.body, badRequest: true}), 422, codeValidation)
		if p.Errors[0].Pointer != c.pointer {
			t.Errorf("%s: errors = %+v", c.body, p.Errors)
		}
	}
	wantProblem(t, e.do(req{method: "PATCH", path: "/v1/posts/pst_s", body: `{"schedule":"soon"}`, badRequest: true}), 400, codeInvalidJSON)
}

// A single post's targets always carry items and attempts, [] when none.
func TestV1PostShowsEmptyItemsAndAttempts(t *testing.T) {
	e := newV1Env(t, true)
	e.seedScheduled("s", time.Now().Add(time.Hour), "bluesky")
	r := e.do(req{method: "GET", path: "/v1/posts/pst_s"})
	if !strings.Contains(r.Body.String(), `"attempts":[]`) || !strings.Contains(r.Body.String(), `"items":[`) {
		t.Fatalf("body = %s", r.Body.String())
	}
}

func TestV1RetryEdgeCases(t *testing.T) {
	e := newV1Env(t, true)
	e.seedPost("f", "failed", "bluesky")
	// An empty body with a JSON Content-Type is no body.
	r := e.do(req{method: "POST", path: "/v1/posts/pst_f:retry", header: map[string]string{"Content-Type": "application/json"}})
	wantStatus(t, r, 202)

	// A canceled thread can be sent by hand.
	ctx := context.Background()
	now := time.Now().UTC()
	p := &store.Post{ID: "t", CreatedAt: now, Platforms: []string{"bluesky"}, Source: "test", Status: "sending",
		Targets: []store.Target{{Platform: "bluesky", Status: "sending"}}}
	for i := 0; i < 2; i++ {
		p.Targets[0].Items = append(p.Targets[0].Items, store.Item{Ordinal: i, Text: "x", Status: store.ItemPlanned,
			IdemKey: store.ItemKey("t", "bluesky", i), NextAttemptAt: &now})
	}
	if err := e.st.SavePost(p); err != nil {
		t.Fatal(err)
	}
	c, _ := e.st.ClaimItem(ctx, now, store.ClaimFilter{})
	if _, err := e.st.RecordItemResult(ctx, store.ItemResult{Claim: *c, Status: store.ItemPublished}); err != nil {
		t.Fatal(err)
	}
	wantStatus(t, e.do(req{method: "POST", path: "/v1/posts/pst_t:cancel"}), 200)
	var got v1PostBody
	r = e.do(req{method: "POST", path: "/v1/posts/pst_t:retry"})
	wantStatus(t, r, 202)
	r.json(&got)
	if items := *got.Targets[0].Items; items[1].Status != "planned" || items[1].NextAttemptAt == nil {
		t.Fatalf("items = %+v", items)
	}
}

// relayDispatch records which target's delivery is retried.
type relayDispatch struct {
	mu    chan struct{}
	calls []string
}

func (d *relayDispatch) RetryTargetRelay(_ context.Context, postID string, target int64, relay string) (*store.Post, error) {
	d.calls = append(d.calls, targetID(target)+" "+relay)
	d.mu <- struct{}{}
	return nil, nil
}

func TestV1RetryDeliveries(t *testing.T) {
	e := newV1Env(t, true)
	d := &relayDispatch{mu: make(chan struct{}, 10)}
	e.api.Dispatch = d
	if err := e.st.SavePost(&store.Post{
		ID: "n", CreatedAt: time.Now().UTC(), Platforms: []string{"nostr"}, Source: "web", Status: "partial",
		Targets: []store.Target{{Platform: "nostr", Status: "partial", FinalText: "hi",
			Relays: []store.RelayState{{URL: "wss://a", Status: "ok"}, {URL: "wss://b", Status: "failed"}}}},
	}); err != nil {
		t.Fatal(err)
	}
	n, _ := e.st.GetPost("n")
	tg := targetID(n.Targets[0].ID)
	one := `{"target":"` + tg + `","endpoint":"wss://b"}`
	p := wantProblem(t, e.do(req{method: "POST", path: "/v1/posts/pst_n:retry", body: `{"deliveries":[` + one + `,` + one + `]}`, badRequest: true}),
		422, codeValidation)
	if p.Errors[0].Pointer != "/deliveries/1" || p.Errors[0].Code != "duplicate" {
		t.Fatalf("errors = %+v", p.Errors)
	}
	wantStatus(t, e.do(req{method: "POST", path: "/v1/posts/pst_n:retry", body: `{"deliveries":[` + one + `]}`}), 202)
	select {
	case <-d.mu:
	case <-time.After(5 * time.Second):
		t.Fatal("the delivery was not retried")
	}
	if len(d.calls) != 1 || d.calls[0] != tg+" wss://b" {
		t.Fatalf("calls = %v", d.calls)
	}
}

func TestV1PostListEdgeCases(t *testing.T) {
	e := newV1Env(t, true)
	base := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	e.seedPostAt("a", "success", base, "bluesky")
	e.seedPostAt("b", "failed", base.Add(2*time.Hour), "bluesky")
	var l postList
	r := e.do(req{method: "GET", path: "/v1/posts?status=published&status=failed"})
	wantStatus(t, r, 200)
	r.json(&l)
	if ids := strings.Join(listIDs(l), ","); ids != "pst_b,pst_a" {
		t.Fatalf("repeated status = %s", ids)
	}
	// An unencoded + in an offset arrives as a space.
	r = e.do(req{method: "GET", path: "/v1/posts?since=2026-11-01T14:00:00+01:00", badRequest: true})
	wantStatus(t, r, 200)
	r.json(&l)
	if ids := strings.Join(listIDs(l), ","); ids != "pst_b" {
		t.Fatalf("since with + = %s", ids)
	}
}

func TestV1AllowLeavesOutActionMethods(t *testing.T) {
	e := newV1Env(t, true)
	r := e.do(req{method: "DELETE", path: "/v1/posts/pst_a", undocumented: true})
	wantProblem(t, r, 405, codeMethodNotAllowed)
	if allow := r.Header().Get("Allow"); allow != "GET, HEAD, PATCH" {
		t.Fatalf("Allow = %q", allow)
	}
}

func TestV1StreamsHeadAndLimits(t *testing.T) {
	e := newV1Env(t, true)
	e.seedPost("a", "success", "bluesky")
	start := time.Now()
	for _, path := range []string{"/v1/events", "/v1/posts/pst_a/events?after=0"} {
		r := e.do(req{method: "HEAD", path: path, undocumented: true})
		wantStatus(t, r, 200)
		if r.Body.Len() != 0 {
			t.Errorf("HEAD %s body = %q", path, r.Body.String())
		}
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("HEAD held the connection")
	}

	// A settled post's stream, resumed after its last event, is a 204.
	r := e.do(req{method: "GET", path: "/v1/posts/pst_a/events"})
	_, evs := parseSSE(t, r.Body.String())
	r = e.do(req{method: "GET", path: "/v1/posts/pst_a/events", header: map[string]string{"Last-Event-ID": evs[len(evs)-1].ID}})
	wantStatus(t, r, 204)

	// Both resume points are checked.
	wantProblem(t, e.do(req{method: "GET", path: "/v1/events?after=x", header: map[string]string{"Last-Event-ID": "1"}, badRequest: true}),
		400, codeInvalidParameter)

	e.api.streams.Store(sseMaxStreams)
	r = e.do(req{method: "GET", path: "/v1/events"})
	wantProblem(t, r, 503, codeUnavailable)
	if r.Header().Get("Retry-After") == "" {
		t.Error("no Retry-After")
	}
}
