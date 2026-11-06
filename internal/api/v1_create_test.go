package api

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/png"
	"mime/multipart"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/geofox/publisher/internal/accounts"
	"github.com/geofox/publisher/internal/destination"
	"github.com/geofox/publisher/internal/dispatch"
	"github.com/geofox/publisher/internal/media"
	"github.com/geofox/publisher/internal/store"
)

// withEngine gives the env a real dispatcher over its store (no workers
// run, so posts stay as saved) and a media pipeline that stores nothing.
func (e *v1Env) withEngine() *dispatch.Dispatcher {
	e.t.Helper()
	d := &dispatch.Dispatcher{Store: e.st}
	e.api.Dispatch = d
	e.api.media = &media.Pipeline{Lookup: func(sha string) (string, bool, error) {
		return "https://blossom.test/" + sha, true, nil
	}}
	return d
}

// seedAccounts adds accounts of more types to the env.
func (e *v1Env) seedAccounts(types ...string) {
	e.t.Helper()
	envs := []store.EnvAccount{{Type: "bluesky", Label: "Bluesky"}, {Type: "mastodon", Label: "Mastodon"}}
	for _, typ := range types {
		envs = append(envs, store.EnvAccount{Type: typ, Label: typ})
	}
	if _, err := e.st.SeedEnvAccounts(context.Background(), store.DefaultOwner, envs); err != nil {
		e.t.Fatal(err)
	}
}

// disable turns an account off, as the owner would.
func (e *v1Env) disable(id string) {
	e.t.Helper()
	off := false
	if _, err := e.st.UpdateAccountMeta(context.Background(), store.DefaultOwner, id, store.AccountMeta{Enabled: &off}); err != nil {
		e.t.Fatal(err)
	}
	e.syncRegistry(id)
}

// savePost saves p, each target without an account sent as the env's
// account of its type (seeded when missing).
func (e *v1Env) savePost(p *store.Post) error {
	e.t.Helper()
	for i := range p.Targets {
		tg := &p.Targets[i]
		if tg.AccountID != "" {
			continue
		}
		if _, err := e.st.SeedEnvAccounts(context.Background(), store.DefaultOwner,
			[]store.EnvAccount{{Type: tg.Platform, Label: tg.Platform}}); err != nil {
			return err
		}
		tg.AccountID = e.acct(tg.Platform)
	}
	return e.st.SavePost(p)
}

// acct returns the env's account id of a type.
func (e *v1Env) acct(typ string) string {
	e.t.Helper()
	accts, err := e.st.Accounts(context.Background(), store.DefaultOwner)
	if err != nil {
		e.t.Fatal(err)
	}
	for _, a := range accts {
		if a.Type == typ {
			return a.ID
		}
	}
	e.t.Fatalf("no %s account", typ)
	return ""
}

func (e *v1Env) countPosts() int {
	e.t.Helper()
	ps, err := e.st.ListPostsPage(context.Background(), store.PostQuery{OwnerID: store.DefaultOwner, Limit: 50})
	if err != nil {
		e.t.Fatal(err)
	}
	return len(ps)
}

func pngBytes(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for x := 0; x < w; x++ {
		img.Set(x, 0, color.RGBA{R: uint8(x), A: 255})
	}
	var b bytes.Buffer
	if err := png.Encode(&b, img); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// upload posts an image as a raw body and returns its media id.
func (e *v1Env) upload(alt string) string {
	e.t.Helper()
	r := e.do(req{method: "POST", path: "/v1/media?alt=" + alt, body: string(pngBytes(e.t, 8, 4)),
		header: map[string]string{"Content-Type": "image/png"}})
	wantStatus(e.t, r, 201)
	var m v1MediaObjectBody
	r.json(&m)
	return m.ID
}

func TestV1CreatePost(t *testing.T) {
	e := newV1Env(t, true)
	e.withEngine()
	bsky, masto := e.acct("bluesky"), e.acct("mastodon")
	med := e.upload("a%20square")
	body := `{"content":{"text":"hello","media":[{"id":"` + med + `"}],"languages":["en"]},
		"targets":[{"account":"` + bsky + `","options":{"reply_gate":"following"}},
		{"account":"` + masto + `","options":{"visibility":"unlisted"},"content":{"text":"hi masto","content_warning":"cw"}}]}`
	r := e.do(req{method: "POST", path: "/v1/posts", body: body})
	wantStatus(t, r, 202)
	var p v1PostBody
	r.json(&p)
	if r.Header().Get("Location") != "/v1/posts/"+p.ID || p.Status != "publishing" || p.Source != "api" {
		t.Fatalf("post = %+v, Location %q", p, r.Header().Get("Location"))
	}
	if len(p.Content.Media) != 1 || p.Content.Media[0].Alt != "a square" {
		t.Fatalf("media = %+v", p.Content.Media)
	}
	got, err := e.st.GetPost(strings.TrimPrefix(p.ID, prefixPost))
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Targets) != 2 || got.Targets[0].AccountID != bsky || got.Targets[1].AccountID != masto {
		t.Fatalf("targets = %+v", got.Targets)
	}
	ms := got.Targets[1]
	if ms.FinalText != "hi masto" || !strings.Contains(ms.FieldsJSON, `"visibility":"unlisted"`) ||
		!strings.Contains(ms.FieldsJSON, `"content_warning":"cw"`) || !strings.Contains(ms.FieldsJSON, `"langs":["en"]`) {
		t.Fatalf("mastodon target = %+v", ms)
	}
	if !strings.Contains(got.Targets[0].FieldsJSON, `"bluesky_reply":"following"`) {
		t.Fatalf("bluesky fields = %s", got.Targets[0].FieldsJSON)
	}
	if n := len(got.Targets[0].Items); n != 1 || got.Targets[0].Items[0].Status != store.ItemPlanned {
		t.Fatalf("items = %+v", got.Targets[0].Items)
	}
}

func TestV1CreatePostScheduled(t *testing.T) {
	e := newV1Env(t, true)
	e.withEngine()
	at := time.Now().Add(time.Hour).UTC().Truncate(time.Second).Format(time.RFC3339)
	r := e.do(req{method: "POST", path: "/v1/posts", header: map[string]string{"Prefer": "wait=5"},
		body: `{"content":{"text":"later"},"targets":[{"account":"` + e.acct("bluesky") + `"}],"schedule":{"at":"` + at + `"}}`})
	wantStatus(t, r, 202) // no wait for a scheduled post
	if r.Header().Get("Preference-Applied") != "" {
		t.Fatalf("Preference-Applied = %q", r.Header().Get("Preference-Applied"))
	}
	var p v1PostBody
	r.json(&p)
	if p.Status != "scheduled" || p.ScheduledAt == nil || *p.ScheduledAt != at {
		t.Fatalf("post = %+v", p)
	}
}

func TestV1CreatePostValidation(t *testing.T) {
	e := newV1Env(t, true)
	e.withEngine()
	bsky, masto := e.acct("bluesky"), e.acct("mastodon")
	e.disable(masto)
	long := strings.Repeat("a", maxPostTextRunes+1)
	manyParts := strings.Repeat("word ", 9000) // 45000 chars: over 100 Bluesky posts
	img := e.upload("x")
	cases := []struct {
		name, body string
		want       []string // pointer:code
	}{
		{"no targets", `{"content":{"text":"x"},"targets":[]}`, []string{"/targets:required"}},
		{"no text", `{"content":{},"targets":[{"account":"` + bsky + `"}]}`, []string{"/content/text:required"}},
		{"blank override", `{"content":{"text":"x"},"targets":[{"account":"` + bsky + `","content":{"text":" "}}]}`,
			[]string{"/targets/0/content/text:empty"}},
		// With media too: the engine would have sent the shared text.
		{"blank override with media", `{"content":{"text":"x","media":[{"id":"` + img + `"}]},"targets":[{"account":"` + bsky + `","content":{"text":""}}]}`,
			[]string{"/targets/0/content/text:empty"}},
		{"long option", `{"content":{"text":"x"},"targets":[{"account":"` + bsky + `","options":{"reply_gate":"` + strings.Repeat("a", 501) + `"}}]}`,
			[]string{"/targets/0/options/reply_gate:invalid_option"}},
		// Pointers name the request's own fields, past a target that failed.
		{"thread parts", `{"content":{"text":"x"},"targets":[{"account":"` + bsky + `","content":{"text":"` + manyParts + `"}}]}`,
			[]string{"/targets/0/content/text:too_many_parts"}},
		// Planning (costly on long texts) is only for an otherwise valid request.
		{"not planned when invalid", `{"content":{"text":"x"},"targets":[{"account":"acc_nope"},{"account":"` + bsky + `","content":{"text":"` + manyParts + `"}}]}`,
			[]string{"/targets/0/account:unknown_account"}},
		{"unknown account", `{"content":{"text":"x"},"targets":[{"account":"acc_nope"}]}`, []string{"/targets/0/account:unknown_account"}},
		{"disabled account", `{"content":{"text":"x"},"targets":[{"account":"` + masto + `"}]}`, []string{"/targets/0/account:account_disabled"}},
		{"duplicate", `{"content":{"text":"x"},"targets":[{"account":"` + bsky + `"},{"account":"` + bsky + `"}]}`,
			[]string{"/targets/1/account:duplicate"}},
		{"unknown option", `{"content":{"text":"x"},"targets":[{"account":"` + bsky + `","options":{"pow":3}}]}`,
			[]string{"/targets/0/options/pow:unknown_option"}},
		{"bad option", `{"content":{"text":"x"},"targets":[{"account":"` + bsky + `","options":{"disable_quotes":"yes","reply_gate":"folowing"}}]}`,
			[]string{"/targets/0/options/disable_quotes:invalid_option", "/targets/0/options/reply_gate:invalid_option"}},
		{"visibility where none", `{"content":{"text":"x"},"targets":[{"account":"` + bsky + `","options":{"visibility":"public"}}]}`,
			[]string{"/targets/0/options/visibility:unknown_option"}},
		{"unknown media", `{"content":{"text":"x","media":[{"id":"med_nope"}]},"targets":[{"account":"` + bsky + `"}]}`,
			[]string{"/content/media/0/id:unknown_media"}},
		{"bad language", `{"content":{"text":"x","languages":["english!"]},"targets":[{"account":"` + bsky + `"}]}`,
			[]string{"/content/languages/0:invalid_language"}},
		{"too long", `{"content":{"text":"` + long + `"},"targets":[{"account":"` + bsky + `"}]}`,
			[]string{"/content/text:too_long"}},
		{"past", `{"content":{"text":"x"},"targets":[{"account":"` + bsky + `"}],"schedule":{"at":"2001-01-01T00:00:00Z"}}`,
			[]string{"/schedule/at:in_past"}},
		{"bad time", `{"content":{"text":"x"},"targets":[{"account":"` + bsky + `"}],"schedule":{"at":"soon"}}`,
			[]string{"/schedule/at:invalid_time"}},
		{"part is replaced", `{"content":{"text":"x","media":[{"id":"` + img + `","part":1}]},"targets":[{"account":"` + bsky + `"}]}`,
			[]string{"/content/media/0/part:replaced"}},
		{"bad breaks", `{"content":{"text":"abc def https://example.com/x 👨‍👩‍👧 end","breaks":[0,4,4,20,33,99]},"targets":[{"account":"` + bsky + `"}]}`,
			[]string{"/content/breaks/0:out_of_range", "/content/breaks/2:not_ascending", "/content/breaks/3:inside_grapheme",
				"/content/breaks/4:inside_grapheme", "/content/breaks/5:out_of_range"}},
		{"bad media position", `{"content":{"text":"abc","media":[{"id":"` + img + `","at":4}]},"targets":[{"account":"` + bsky + `"}]}`,
			[]string{"/content/media/0/at:out_of_range"}},
		{"bad target thread", `{"content":{"text":"abc def","media":[{"id":"` + img + `"}]},"targets":[{"account":"` + bsky +
			`","content":{"text":"xy"},"thread":{"breaks":[5],"media_at":[0,1]}}]}`,
			[]string{"/targets/0/thread/breaks/0:out_of_range", "/targets/0/thread/media_at:length_mismatch"}},
	}
	for _, c := range cases {
		for _, path := range []string{"/v1/posts", "/v1/posts:preview"} {
			p := wantProblem(t, e.do(req{method: "POST", path: path, body: c.body, badRequest: true}), 422, codeValidation)
			var got []string
			for _, fe := range p.Errors {
				got = append(got, fe.Pointer+":"+fe.Code)
			}
			if strings.Join(got, " ") != strings.Join(c.want, " ") {
				t.Errorf("%s %s: errors = %v, want %v", c.name, path, got, c.want)
			}
		}
	}
	if n := e.countPosts(); n != 0 {
		t.Fatalf("an invalid request saved %d posts", n)
	}
}

func TestV1CreatePostMediaStates(t *testing.T) {
	e := newV1Env(t, true)
	e.withEngine()
	bsky := e.acct("bluesky")
	ctx := context.Background()
	proc := &store.MediaObject{Status: store.MediaProcessing, Mime: "video/mp4", JobID: "gone"}
	if err := e.st.CreateMediaObject(ctx, proc); err != nil {
		t.Fatal(err)
	}
	vid := &store.MediaObject{Status: store.MediaReady, Mime: "video/mp4", URL: "https://b/v", SizeBytes: 10, DurationSecs: 5}
	if err := e.st.CreateMediaObject(ctx, vid); err != nil {
		t.Fatal(err)
	}
	img := e.upload("x")
	post := func(media string) problemBody {
		return wantProblem(t, e.do(req{method: "POST", path: "/v1/posts", badRequest: true,
			body: `{"content":{"text":"x","media":[` + media + `]},"targets":[{"account":"` + bsky + `"}]}`}), 422, codeValidation)
	}
	// A processing video whose job is gone (a restart) has failed.
	if p := post(`{"id":"` + proc.ID + `"}`); p.Errors[0].Code != "media_failed" {
		t.Fatalf("errors = %+v", p.Errors)
	}
	if p := post(`{"id":"` + vid.ID + `"},{"id":"` + img + `"}`); p.Errors[0].Code != "mixed_media" {
		t.Fatalf("errors = %+v", p.Errors)
	}
	if p := post(`{"id":"` + img + `"},{"id":"` + img + `"}`); p.Errors[0].Pointer != "/content/media/1/id" || p.Errors[0].Code != "duplicate" {
		t.Fatalf("errors = %+v", p.Errors)
	}
}

func TestV1CreatePostWait(t *testing.T) {
	e := newV1Env(t, true)
	e.withEngine()
	ctx := context.Background()
	go func() { // the engine: publish whatever is due
		for i := 0; i < 200; i++ {
			c, _ := e.st.ClaimItem(ctx, time.Now().Add(time.Minute), store.ClaimFilter{})
			if c != nil {
				_, _ = e.st.RecordItemResult(ctx, store.ItemResult{Claim: *c, Status: store.ItemPublished})
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	}()
	r := e.do(req{method: "POST", path: "/v1/posts", header: map[string]string{"Prefer": "respond-async, wait=10"},
		body: `{"content":{"text":"now"},"targets":[{"account":"` + e.acct("bluesky") + `"}]}`})
	wantStatus(t, r, 201)
	if r.Header().Get("Preference-Applied") != "wait=10" {
		t.Fatalf("Preference-Applied = %q", r.Header().Get("Preference-Applied"))
	}
	var p v1PostBody
	r.json(&p)
	if p.Status != "published" {
		t.Fatalf("status = %s", p.Status)
	}

	// Nothing sends it: the wait runs out and the answer is 202.
	start := time.Now()
	r = e.do(req{method: "POST", path: "/v1/posts", header: map[string]string{"Prefer": "wait=1"},
		body: `{"content":{"text":"stuck"},"targets":[{"account":"` + e.acct("bluesky") + `"}]}`})
	wantStatus(t, r, 202)
	if d := time.Since(start); d < time.Second || d > 5*time.Second {
		t.Fatalf("waited %v", d)
	}
}

func TestV1CreatePostIdempotent(t *testing.T) {
	e := newV1Env(t, true)
	e.withEngine()
	body := `{"content":{"text":"once"},"targets":[{"account":"` + e.acct("bluesky") + `"}]}`
	h := map[string]string{"Idempotency-Key": `"k1"`}
	r1 := e.do(req{method: "POST", path: "/v1/posts", body: body, header: h})
	wantStatus(t, r1, 202)
	r2 := e.do(req{method: "POST", path: "/v1/posts", body: body, header: h})
	wantStatus(t, r2, 202)
	if r2.Header().Get("Idempotent-Replayed") != "true" || r1.Header().Get("Location") != r2.Header().Get("Location") {
		t.Fatalf("replay: %v vs %v", r1.Header(), r2.Header())
	}
	if n := e.countPosts(); n != 1 {
		t.Fatalf("%d posts, want 1", n)
	}
}

func TestV1PreviewPost(t *testing.T) {
	e := newV1Env(t, true)
	e.withEngine()
	img := e.upload("x")
	// 495 characters and a 2-character content warning, which Mastodon
	// counts in: a thread on Bluesky, one Mastodon post.
	text := strings.Repeat("word ", 99)
	r := e.do(req{method: "POST", path: "/v1/posts:preview", body: `{"content":{"text":"` + text + `","media":[{"id":"` + img + `"}],
		"content_warning":"cw"},"targets":[{"account":"` + e.acct("bluesky") + `"},{"account":"` + e.acct("mastodon") + `"}],
		"threading":{"numbering":true}}`})
	wantStatus(t, r, 200)
	var pv v1PreviewBody
	r.json(&pv)
	if len(pv.Targets) != 2 {
		t.Fatalf("preview = %+v", pv)
	}
	bs, ms := pv.Targets[0], pv.Targets[1]
	if len(bs.Items) < 2 || !strings.Contains(bs.Items[0].Text, "1/") || len(bs.Items[0].Media) != 1 {
		t.Fatalf("bluesky items = %+v", bs.Items)
	}
	if !slicesHasPrefix(bs.Warnings, "Bluesky has no content warnings") {
		t.Fatalf("bluesky warnings = %v", bs.Warnings)
	}
	if len(ms.Items) != 1 || len(ms.Items[0].Media) != 1 {
		t.Fatalf("mastodon items = %+v", ms.Items)
	}
	if e.countPosts() != 0 {
		t.Fatal("a preview saved a post")
	}
}

func slicesHasPrefix(l []string, prefix string) bool {
	for _, s := range l {
		if strings.HasPrefix(s, prefix) {
			return true
		}
	}
	return false
}

func TestPreferWait(t *testing.T) {
	cases := []struct {
		in []string
		n  int
		ok bool
	}{
		{nil, 0, false},
		{[]string{"respond-async"}, 0, false},
		{[]string{"wait=10"}, 10, true},
		{[]string{"respond-async, wait=5"}, 5, true},
		{[]string{"handling=lenient", "Wait = 3"}, 3, true},
		{[]string{"wait=600"}, maxPreferWait, true},
		{[]string{"wait=-1"}, 0, false},
		{[]string{"wait=abc"}, 0, false},
	}
	for _, c := range cases {
		if n, ok := preferWait(c.in); n != c.n || ok != c.ok {
			t.Errorf("preferWait(%q) = %d, %v; want %d, %v", c.in, n, ok, c.n, c.ok)
		}
	}
}

// Every option a registered driver declares can be sent through /v1.
func TestV1EveryOptionMapped(t *testing.T) {
	for _, typ := range destination.Types() {
		drv, _ := destination.Lookup(typ)
		for _, o := range drv.Manifest().Options {
			if _, ok := optionFields[o.Key]; !ok {
				t.Errorf("%s option %q has no mapping in optionFields", typ, o.Key)
			}
		}
	}
}

// ─── media ──────────────────────────────────────────────────────────────

func TestV1MediaUploadAndAlt(t *testing.T) {
	e := newV1Env(t, true)
	e.withEngine()
	// Multipart, with alt.
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	_ = mw.WriteField("alt", "first")
	fw, _ := mw.CreateFormFile("file", "a.png")
	_, _ = fw.Write(pngBytes(t, 4, 4))
	_ = mw.Close()
	r := e.do(req{method: "POST", path: "/v1/media", body: buf.String(), header: map[string]string{"Content-Type": mw.FormDataContentType()}})
	wantStatus(t, r, 201)
	var m v1MediaObjectBody
	r.json(&m)
	if m.Status != "ready" || m.Alt != "first" || m.Dim != "4x4" || m.URL == nil || !strings.HasPrefix(*m.URL, "https://blossom.test/") {
		t.Fatalf("media = %+v", m)
	}
	r = e.do(req{method: "PATCH", path: "/v1/media/" + m.ID, body: `{"alt":"second"}`})
	wantStatus(t, r, 200)
	r.json(&m)
	if m.Alt != "second" {
		t.Fatalf("alt = %q", m.Alt)
	}
	wantProblem(t, e.do(req{method: "PATCH", path: "/v1/media/" + m.ID, body: `{}`, badRequest: true}), 422, codeValidation)
	wantProblem(t, e.do(req{method: "GET", path: "/v1/media/med_nope"}), 404, codeNotFound)
	// Not an image or video.
	wantProblem(t, e.do(req{method: "POST", path: "/v1/media", body: `{"a":1}`, badRequest: true}), 415, codeUnsupportedMediaType)
	r = e.do(req{method: "POST", path: "/v1/media", body: "plain text, honestly", badRequest: true,
		header: map[string]string{"Content-Type": "application/octet-stream"}})
	wantProblem(t, r, 422, codeValidation)
	// Videos need the video runner.
	wantProblem(t, e.do(req{method: "POST", path: "/v1/media", body: "\x00\x00\x00\x18ftypmp42", badRequest: true,
		header: map[string]string{"Content-Type": "video/mp4"}}), 503, codeUnavailable)
}

func TestV1MediaScope(t *testing.T) {
	e := newV1Env(t, true)
	e.withEngine()
	tok := e.token(scopePostsWrite)
	r := e.do(req{method: "POST", path: "/v1/media", auth: tok, body: string(pngBytes(t, 2, 2)), badRequest: true,
		header: map[string]string{"Content-Type": "image/png"}})
	wantProblem(t, r, 403, codeInsufficientScope)
}

func TestV1CompressMedia(t *testing.T) {
	e := newV1Env(t, true)
	e.withEngine() // a media store
	r := e.do(req{method: "POST", path: "/v1/media:compress?preset=small", body: string(pngBytes(t, 16, 8)),
		header: map[string]string{"Content-Type": "image/png"}})
	wantStatus(t, r, 200)
	if ct := r.Header().Get("Content-Type"); ct != "image/jpeg" {
		t.Fatalf("Content-Type = %q", ct)
	}
	r = e.do(req{method: "POST", path: "/v1/media:compress?preset=huge", body: string(pngBytes(t, 4, 4)), badRequest: true,
		header: map[string]string{"Content-Type": "image/png"}})
	p := wantProblem(t, r, 422, codeValidation)
	if p.Errors[0].Pointer != "/preset" {
		t.Fatalf("errors = %+v", p.Errors)
	}
	// Its own limit, not the upload route's.
	big := strings.Repeat("\x00", int(maxUploadRequestBytes+2<<20))
	p = wantProblem(t, e.do(req{method: "POST", path: "/v1/media:compress?preset=small", body: big, badRequest: true,
		header: map[string]string{"Content-Type": "image/png"}}), 413, codeTooLarge)
	if p.Detail != "images are at most 64 MiB" {
		t.Fatalf("detail = %q", p.Detail)
	}
}

func TestV1MediaUploadPartsAndTypes(t *testing.T) {
	e := newV1Env(t, true)
	e.withEngine()
	form := func(fields ...[2]string) (string, string) {
		var buf bytes.Buffer
		mw := multipart.NewWriter(&buf)
		for _, f := range fields {
			if f[0] == "file" {
				fw, _ := mw.CreateFormFile("file", "a.bin") // curl -F sends octet-stream
				_, _ = fw.Write([]byte(f[1]))
			} else {
				_ = mw.WriteField(f[0], f[1])
			}
		}
		_ = mw.Close()
		return buf.String(), mw.FormDataContentType()
	}
	// alt after the file still counts.
	body, ct := form([2]string{"file", string(pngBytes(t, 4, 4))}, [2]string{"alt", "late alt"})
	r := e.do(req{method: "POST", path: "/v1/media", body: body, header: map[string]string{"Content-Type": ct}})
	wantStatus(t, r, 201)
	var m v1MediaObjectBody
	r.json(&m)
	if m.Alt != "late alt" || m.Mime != "image/png" {
		t.Fatalf("media = %+v", m)
	}
	// A QuickTime file sent as octet-stream is known to be a video (here:
	// no video runner, so 503, not 422).
	mov := "\x00\x00\x00\x14ftypqt  \x00\x00\x00\x00qt  "
	body, ct = form([2]string{"file", mov})
	wantProblem(t, e.do(req{method: "POST", path: "/v1/media", body: body, badRequest: true,
		header: map[string]string{"Content-Type": ct}}), 503, codeUnavailable)
	// The content decides, not the declared type: markup is refused.
	for _, c := range []struct{ ct, body string }{
		{"image/svg+xml", `<svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>`},
		{"image/png", "<html><script>alert(1)</script></html>"},
	} {
		p := wantProblem(t, e.do(req{method: "POST", path: "/v1/media", body: c.body, badRequest: true,
			header: map[string]string{"Content-Type": c.ct}}), 422, codeValidation)
		if p.Errors[0].Code != "unsupported_type" {
			t.Errorf("%s: errors = %+v", c.ct, p.Errors)
		}
	}
	// Two files.
	body, ct = form([2]string{"file", string(pngBytes(t, 2, 2))}, [2]string{"file", string(pngBytes(t, 2, 2))})
	p := wantProblem(t, e.do(req{method: "POST", path: "/v1/media", body: body, badRequest: true,
		header: map[string]string{"Content-Type": ct}}), 422, codeValidation)
	if p.Errors[0].Code != "too_many" {
		t.Fatalf("errors = %+v", p.Errors)
	}
}

func TestSniffType(t *testing.T) {
	ftyp := func(brand string) []byte { return []byte("\x00\x00\x00\x18ftyp" + brand + "\x00\x00\x00\x00") }
	cases := []struct {
		declared string
		head     []byte
		want     string
	}{
		{"application/octet-stream", ftyp("qt  "), "video/quicktime"},
		{"", ftyp("heic"), "image/heic"},
		{"", ftyp("mif1"), "image/heif"},
		{"", ftyp("isom"), "video/mp4"},
		{"image/png", ftyp("isom"), "video/mp4"},
		{"video/x-matroska", []byte{0x1a, 0x45, 0xdf, 0xa3, 0x42, 0x82, 0x88, 'm', 'a', 't', 'r', 'o', 's', 'k', 'a'}, "video/webm"}, // EBML: the sniffer says webm
		{"video/x-unknown", []byte("garbage"), "text/plain"},
		{"image/svg+xml", []byte("<svg></svg>"), "text/plain"},
	}
	for _, c := range cases {
		if got := sniffType(c.declared, c.head); got != c.want {
			t.Errorf("sniffType(%q, %q) = %q, want %q", c.declared, c.head, got, c.want)
		}
	}
}

// A media's part places it on that part of the thread, in the preview and
// in the post as saved.
// TestV1CreatePostBreaksAndPositions: shared breaks apply to every account
// on the shared text; an account's own breaks replace them ([] for none);
// media go with the post holding their position; an account with its own
// text has its own breaks and positions; "---" is text.
func TestV1CreatePostBreaksAndPositions(t *testing.T) {
	e := newV1Env(t, true)
	e.withEngine()
	e.seedAccounts("nostr", "threads")
	a, b := e.upload("a"), e.upload("b")
	text := "one\n\ntwo\n\nthree ---"
	two, three := strings.Index(text, "two"), strings.Index(text, "three")
	body := `{"content":{"text":"one\n\ntwo\n\nthree ---","breaks":[` + strconv.Itoa(two) + `,` + strconv.Itoa(three) + `],
		"media":[{"id":"` + a + `"},{"id":"` + b + `","at":` + strconv.Itoa(three+1) + `}]},
		"targets":[{"account":"` + e.acct("bluesky") + `"},
		  {"account":"` + e.acct("nostr") + `","thread":{"breaks":[]}},
		  {"account":"` + e.acct("threads") + `","thread":{"breaks":[` + strconv.Itoa(three) + `]}},
		  {"account":"` + e.acct("mastodon") + `","content":{"text":"just\none"},"thread":{"breaks":[5],"media_at":[5,0]}}]}`
	r := e.do(req{method: "POST", path: "/v1/posts:preview", body: body})
	wantStatus(t, r, 200)
	var pv v1PreviewBody
	r.json(&pv)
	texts := func(items []v1PreviewItem) (out []string) {
		for _, it := range items {
			out = append(out, it.Text)
		}
		return out
	}
	bs := pv.Targets[0]
	if got := texts(bs.Items); !slices.Equal(got, []string{"one", "two", "three ---"}) {
		t.Fatalf("bluesky = %q", got)
	}
	if bs.Items[0].Break != "none" || bs.Items[1].Break != "manual" || bs.Items[1].Range != [2]int{two, two + 3} ||
		bs.Text != "one\n\ntwo\n\nthree ---" || bs.Limit != 300 {
		t.Fatalf("bluesky = %+v", bs)
	}
	if !slices.Equal(bs.Items[0].Media, []int{0}) || len(bs.Items[1].Media) != 0 || !slices.Equal(bs.Items[2].Media, []int{1}) {
		t.Fatalf("bluesky media = %+v", bs.Items)
	}
	if got := texts(pv.Targets[1].Items); len(got) != 1 || got[0] != "one\n\ntwo\n\nthree ---" {
		t.Fatalf("nostr = %q", got)
	}
	if got := texts(pv.Targets[2].Items); !slices.Equal(got, []string{"one\n\ntwo", "three ---"}) {
		t.Fatalf("threads = %q", got)
	}
	ms := pv.Targets[3]
	if got := texts(ms.Items); !slices.Equal(got, []string{"just", "one"}) || !slices.Equal(ms.Items[0].Media, []int{1}) ||
		!slices.Equal(ms.Items[1].Media, []int{0}) {
		t.Fatalf("mastodon = %+v", ms.Items)
	}

	r = e.do(req{method: "POST", path: "/v1/posts", body: body})
	wantStatus(t, r, 202)
	var p v1PostBody
	r.json(&p)
	got, err := e.st.GetPost(strings.TrimPrefix(p.ID, prefixPost))
	if err != nil {
		t.Fatal(err)
	}
	items := got.Targets[0].Items
	if len(items) != 3 || len(items[0].Media) != 1 || len(items[1].Media) != 0 || len(items[2].Media) != 1 || items[2].Media[0] != 1 {
		t.Fatalf("bluesky items = %+v", items)
	}
	if items[1].Range == nil || *items[1].Range != [2]int{two, two + 3} {
		t.Fatalf("stored range = %v", items[1].Range)
	}
	r = e.do(req{method: "GET", path: "/v1/posts/" + p.ID})
	wantStatus(t, r, 200)
	if !strings.Contains(r.Body.String(), `"range":[`+strconv.Itoa(two)+`,`+strconv.Itoa(two+3)+`]`) {
		t.Fatalf("GET has no range: %s", r.Body.String())
	}
}

// TestV1CreatePostCRLF: positions count a CRLF text as sent; the text is
// posted with LF and the positions follow.
func TestV1CreatePostCRLF(t *testing.T) {
	e := newV1Env(t, true)
	e.withEngine()
	body := `{"content":{"text":"one\r\ntwo\r\nthree","breaks":[10]},"targets":[{"account":"` + e.acct("bluesky") + `"}]}`
	r := e.do(req{method: "POST", path: "/v1/posts:preview", body: body})
	wantStatus(t, r, 200)
	var pv v1PreviewBody
	r.json(&pv)
	it := pv.Targets[0].Items
	if len(it) != 2 || it[0].Text != "one\ntwo" || it[1].Text != "three" || it[1].Range != [2]int{8, 13} {
		t.Fatalf("items = %+v", it)
	}
}

// TestV1CreatePostTooManyLinks: a post the destination would refuse for
// its links is refused before it is saved.
func TestV1CreatePostTooManyLinks(t *testing.T) {
	e := newV1Env(t, true)
	e.withEngine()
	e.seedAccounts("threads")
	links := "https://a.example/1 https://a.example/2 https://a.example/3 https://a.example/4 https://a.example/5 https://a.example/6"
	body := `{"content":{"text":"` + links + `"},"targets":[{"account":"` + e.acct("threads") + `"}]}`
	p := wantProblem(t, e.do(req{method: "POST", path: "/v1/posts", body: body, badRequest: true}), 422, codeValidation)
	if len(p.Errors) != 1 || p.Errors[0].Pointer != "/content/text" || p.Errors[0].Code != "refused_by_destination" {
		t.Fatalf("errors = %+v", p.Errors)
	}
}

// statusOf is an AccountStatus with fixed statuses (connected unless set).
type statusOf map[string]accounts.Status

func (s statusOf) Get(id string) (accounts.Entry, bool) {
	st, ok := s[id]
	if !ok {
		st = accounts.Connected
	}
	reason := ""
	if st == accounts.NotConfigured {
		reason = "set BLUESKY_APP_PASSWORD to use it again"
	}
	return accounts.Entry{Row: store.Account{ID: id}, Status: st, Reason: reason}, true
}

func (statusOf) List(string) []accounts.Entry { return nil }
func (statusOf) Acquire(id string) (destination.Account, func(), error) {
	return nil, nil, accounts.ErrNoAccount
}
func (statusOf) Profile(string) (accounts.Profile, bool) { return accounts.Profile{}, false }
func (statusOf) Update(string, store.Account) error      { return nil }

// TestV1CreatePostUnavailableAccount: an account the registry says can't
// send (here, the environment no longer configures it) is refused, saying
// why, rather than saved to fail later.
func TestV1CreatePostUnavailableAccount(t *testing.T) {
	e := newV1Env(t, true)
	e.withEngine()
	bsky := e.acct("bluesky")
	e.api.Accounts = statusOf{bsky: accounts.NotConfigured}
	body := `{"content":{"text":"x"},"targets":[{"account":"` + bsky + `"}]}`
	for _, path := range []string{"/v1/posts", "/v1/posts:preview"} {
		p := wantProblem(t, e.do(req{method: "POST", path: path, body: body, badRequest: true}), 422, codeValidation)
		if len(p.Errors) != 1 || p.Errors[0].Pointer != "/targets/0/account" || p.Errors[0].Code != "account_disabled" ||
			!strings.Contains(p.Errors[0].Detail, "BLUESKY_APP_PASSWORD") {
			t.Fatalf("%s: errors = %+v", path, p.Errors)
		}
	}
	e.api.Accounts = statusOf{}
	wantStatus(t, e.do(req{method: "POST", path: "/v1/posts", body: body}), 202)
}
