package api

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/geofox/publisher/internal/store"
)

func (e *v1Env) newDraft(body string) (v1DraftBody, string) {
	e.t.Helper()
	r := e.do(req{method: "POST", path: "/v1/drafts", body: body})
	wantStatus(e.t, r, 201)
	var d v1DraftBody
	r.json(&d)
	if loc := r.Header().Get("Location"); loc != "/v1/drafts/"+d.ID {
		e.t.Fatalf("Location = %q", loc)
	}
	return d, r.Header().Get("ETag")
}

func TestV1DraftLifecycle(t *testing.T) {
	e := newV1Env(t, true)
	e.withEngine()
	med := e.upload("dot")
	bsky := e.acct("bluesky")
	post := `{"content":{"text":"First line\n\nmore","media":[{"id":"` + med + `","part":1}]},` +
		`"targets":[{"account":"` + bsky + `","options":{"reply_gate":"following"}}],"threading":{"numbering":false}}`
	d, etag := e.newDraft(`{"tags":["#Work","work"],"post":` + post + `}`)
	if !strings.HasPrefix(d.ID, "drf_") || etag != `"1"` || d.Title != "First line" || len(d.Tags) != 1 || d.Tags[0] != "work" {
		t.Fatalf("created %+v etag %s", d, etag)
	}
	if len(d.Media) != 1 || d.Media[0].ID != med {
		t.Fatalf("media = %+v", d.Media)
	}
	// The post comes back as sent, without fields that weren't.
	var got map[string]any
	_ = json.Unmarshal(d.Post, &got)
	if _, ok := got["schedule"]; ok {
		t.Fatalf("post has unset fields: %s", d.Post)
	}
	if !strings.Contains(string(d.Post), `"part":1`) || !strings.Contains(string(d.Post), `"reply_gate":"following"`) {
		t.Fatalf("post = %s", d.Post)
	}

	// Read, then change with If-Match.
	r := e.do(req{method: "GET", path: "/v1/drafts/" + d.ID})
	wantStatus(t, r, 200)
	if r.Header().Get("ETag") != `"1"` {
		t.Fatalf("GET ETag = %q", r.Header().Get("ETag"))
	}
	r = e.do(req{method: "PATCH", path: "/v1/drafts/" + d.ID, header: map[string]string{"If-Match": `"1"`},
		body: `{"post":{"content":{"text":"Second take"}}}`})
	wantStatus(t, r, 200)
	var d2 v1DraftBody
	r.json(&d2)
	if r.Header().Get("ETag") != `"2"` || d2.Title != "Second take" || len(d2.Media) != 0 || d2.Tags[0] != "work" {
		t.Fatalf("patched %+v etag %s", d2, r.Header().Get("ETag"))
	}
	// Another tab still at "1" is refused, told the current version, and changes nothing.
	r = e.do(req{method: "PATCH", path: "/v1/drafts/" + d.ID, header: map[string]string{"If-Match": `"1"`},
		body: `{"title":"stale"}`})
	wantProblem(t, r, 412, codePreconditionFailed)
	if r.Header().Get("ETag") != `"2"` {
		t.Fatalf("412 ETag = %q", r.Header().Get("ETag"))
	}
	r = e.do(req{method: "DELETE", path: "/v1/drafts/" + d.ID, header: map[string]string{"If-Match": `"1"`}})
	wantProblem(t, r, 412, codePreconditionFailed)
	r = e.do(req{method: "PATCH", path: "/v1/drafts/" + d.ID, header: map[string]string{"If-Match": "nonsense"},
		body: `{"title":"x"}`, badRequest: true})
	wantProblem(t, r, 400, codeInvalidParameter)
	r = e.do(req{method: "PATCH", path: "/v1/drafts/" + d.ID, header: map[string]string{"If-Match": `W/"2"`},
		body: `{"title":"x"}`})
	wantProblem(t, r, 400, codeInvalidParameter) // If-Match compares strongly

	// A title given is kept when the text changes; "" goes back to the text's.
	r = e.do(req{method: "PATCH", path: "/v1/drafts/" + d.ID, body: `{"title":"Mine"}`})
	wantStatus(t, r, 200)
	r = e.do(req{method: "PATCH", path: "/v1/drafts/" + d.ID, body: `{"post":{"content":{"text":"Third"}}}`})
	r.json(&d2)
	if d2.Title != "Mine" {
		t.Fatalf("title = %q, want Mine", d2.Title)
	}
	r = e.do(req{method: "PATCH", path: "/v1/drafts/" + d.ID, body: `{"title":""}`})
	r.json(&d2)
	if d2.Title != "Third" {
		t.Fatalf("title = %q, want Third", d2.Title)
	}

	r = e.do(req{method: "DELETE", path: "/v1/drafts/" + d.ID, header: map[string]string{"If-Match": r.Header().Get("ETag")}})
	wantStatus(t, r, 204)
	wantProblem(t, e.do(req{method: "GET", path: "/v1/drafts/" + d.ID}), 404, codeNotFound)
	wantProblem(t, e.do(req{method: "GET", path: "/v1/drafts/nope"}), 404, codeNotFound)
}

func TestV1DraftValidation(t *testing.T) {
	e := newV1Env(t, true)
	e.withEngine()
	// Empty is a draft.
	d, _ := e.newDraft(`{}`)
	if string(d.Post) != `{"content":{},"targets":[]}` || d.Title != "" {
		t.Fatalf("empty draft = %+v %s", d, d.Post)
	}
	e.newDraft(`{"post":{"content":{"text":""},"targets":[],"schedule":{"at":"2001-01-01T00:00:00Z"}}}`)

	r := e.do(req{method: "POST", path: "/v1/drafts", badRequest: true, body: `{"title":"` + strings.Repeat("t", 201) + `",` +
		`"tags":["` + strings.Repeat("x", 65) + `"],` +
		`"post":{"content":{"media":[{"id":"med_nope"}]},"targets":[{"account":"acc_nope"}],` +
		`"reply_to":{"url":"https://bsky.app/profile/a/post/b"},"quote":{"url":"https://bsky.app/profile/a/post/b"},` +
		`"schedule":{"at":"tomorrow"},"draft":"drf_x"}}`})
	p := wantProblem(t, r, 422, codeValidation)
	for _, want := range []string{"/title", "/tags/0", "/post/content/media/0/id", "/post/targets/0/account", "/post",
		"/post/schedule/at", "/post/draft"} {
		found := false
		for _, fe := range p.Errors {
			found = found || fe.Pointer == want
		}
		if !found {
			t.Errorf("no error at %s: %+v", want, p.Errors)
		}
	}
	// What the post would refuse anyway is refused now.
	bsky := e.acct("bluesky")
	r = e.do(req{method: "POST", path: "/v1/drafts", badRequest: true, body: `{"post":{"targets":[{"account":"` + bsky +
		`","options":{"bogus":1,"disable_quotes":"yes"},"content":{"text":"  "}}],"schedule":{}}}`})
	p = wantProblem(t, r, 422, codeValidation)
	for _, want := range []string{"/post/targets/0/options/bogus", "/post/targets/0/options/disable_quotes",
		"/post/targets/0/content/text", "/post/schedule/at"} {
		found := false
		for _, fe := range p.Errors {
			found = found || fe.Pointer == want
		}
		if !found {
			t.Errorf("no error at %s: %+v", want, p.Errors)
		}
	}
	// Unknown fields are refused, nested too.
	wantProblem(t, e.do(req{method: "POST", path: "/v1/drafts", badRequest: true, body: `{"post":{"contents":{}}}`}), 400, codeInvalidJSON)
}

func TestV1DraftList(t *testing.T) {
	e := newV1Env(t, true)
	e.withEngine()
	med := e.upload("dot")
	e.newDraft(`{"tags":["a"],"post":{"content":{"text":"apple 100%"}}}`)
	e.newDraft(`{"tags":["a","b"],"post":{"content":{"text":"banana","media":[{"id":"` + med + `"}]}}}`)
	e.newDraft(`{"title":"cherry title","post":{"content":{"text":"x"}}}`)

	list := func(q string) listBody[v1DraftListItem] {
		t.Helper()
		r := e.do(req{method: "GET", path: "/v1/drafts" + q})
		wantStatus(t, r, 200)
		var b listBody[v1DraftListItem]
		r.json(&b)
		return b
	}
	all := list("")
	if len(all.Data) != 3 || all.Data[0].Title != "cherry title" || all.Data[1].MediaURL == nil || all.Data[2].MediaURL != nil {
		t.Fatalf("list = %+v", all.Data)
	}
	if b := list("?q=100%25"); len(b.Data) != 1 || b.Data[0].Preview != "apple 100%" {
		t.Fatalf("q=100%%: %+v", b.Data) // % is literal, not a wildcard
	}
	if b := list("?q=cherry"); len(b.Data) != 1 {
		t.Fatalf("q=cherry (title): %+v", b.Data)
	}
	if b := list("?tag=a&tag=b"); len(b.Data) != 1 || b.Data[0].Preview != "banana" {
		t.Fatalf("tags a+b: %+v", b.Data)
	}
	// Pages.
	p1 := list("?limit=2")
	if len(p1.Data) != 2 || p1.NextCursor == nil {
		t.Fatalf("page 1 = %+v", p1)
	}
	p2 := list("?limit=2&cursor=" + *p1.NextCursor)
	if len(p2.Data) != 1 || p2.NextCursor != nil || p2.Data[0].ID == p1.Data[1].ID {
		t.Fatalf("page 2 = %+v", p2)
	}
	wantProblem(t, e.do(req{method: "GET", path: "/v1/drafts?cursor=bogus"}), 400, codeInvalidCursor)
	wantProblem(t, e.do(req{method: "GET", path: "/v1/drafts" + "?tag=a" + strings.Repeat("&tag=x", 20)}), 400, codeInvalidParameter)
}

func TestV1PostConsumesDraft(t *testing.T) {
	e := newV1Env(t, true)
	e.withEngine()
	bsky := e.acct("bluesky")
	body := `{"content":{"text":"hi"},"targets":[{"account":"` + bsky + `"}]}`
	d, _ := e.newDraft(`{"post":` + body + `}`)

	// A token without drafts:write can't use a draft up.
	tok := e.token("posts:write")
	withDraft := `{"content":{"text":"hi"},"targets":[{"account":"` + bsky + `"}],"draft":"` + d.ID + `"}`
	r := e.do(req{method: "POST", path: "/v1/posts", auth: tok, body: withDraft})
	p := wantProblem(t, r, 422, codeValidation)
	if len(p.Errors) != 1 || p.Errors[0].Pointer != "/draft" || p.Errors[0].Code != "not_permitted" {
		t.Fatalf("errors = %+v", p.Errors)
	}
	r = e.do(req{method: "POST", path: "/v1/posts:preview", body: strings.Replace(withDraft, d.ID, "drf_gone", 1)})
	p = wantProblem(t, r, 422, codeValidation)
	if p.Errors[0].Code != "unknown_draft" {
		t.Fatalf("errors = %+v", p.Errors)
	}
	if e.countPosts() != 0 {
		t.Fatal("a refused post was saved")
	}

	r = e.do(req{method: "POST", path: "/v1/posts", body: withDraft})
	wantStatus(t, r, 202)
	wantProblem(t, e.do(req{method: "GET", path: "/v1/drafts/" + d.ID}), 404, codeNotFound)
	// Posting it again (a double click, another tab) makes no second post.
	r = e.do(req{method: "POST", path: "/v1/posts", body: withDraft})
	p = wantProblem(t, r, 422, codeValidation)
	if p.Errors[0].Code != "unknown_draft" || e.countPosts() != 1 {
		t.Fatalf("second post: %+v, %d posts", p.Errors, e.countPosts())
	}

	// A draft saved again after the post read it is kept, and nothing posts.
	d2, _ := e.newDraft(`{"post":` + body + `}`)
	if err := e.st.UpdateV1Draft(context.Background(), &store.V1Draft{ID: d2.ID[4:], OwnerID: store.DefaultOwner,
		Text: "edited", Body: body}, 0); err != nil {
		t.Fatal(err)
	}
	stale, err := e.st.GetV1Draft(context.Background(), store.DefaultOwner, d2.ID[4:])
	if err != nil {
		t.Fatal(err)
	}
	stale.Version-- // as the request read it
	if err := e.st.DeleteV1Draft(context.Background(), store.DefaultOwner, stale.ID, stale.Version); !errors.Is(err, store.ErrDraftConflict) {
		t.Fatalf("stale take: %v", err)
	}
}

func TestV1TranslateDraft(t *testing.T) {
	e := newV1Env(t, true)
	e.withEngine()
	bsky := e.acct("bluesky")
	d, _ := e.newDraft(`{"tags":["x"],"post":{"content":{"text":"hello","languages":["en"]},` +
		`"targets":[{"account":"` + bsky + `","content":{"text":"own","languages":["en"]},"options":{"reply_gate":"nobody"}}]}}`)

	wantProblem(t, e.do(req{method: "POST", path: "/v1/drafts/" + d.ID + ":translate", body: `{"target_lang":"fr"}`}), 503, codeUnavailable)
	e.api.Translator = v1FakeTranslator{}
	tok := e.token("drafts:write")
	wantProblem(t, e.do(req{method: "POST", path: "/v1/drafts/" + d.ID + ":translate", auth: tok, body: `{"target_lang":"fr"}`}),
		403, codeInsufficientScope)
	// The same answer for a draft that doesn't exist: no probing.
	wantProblem(t, e.do(req{method: "POST", path: "/v1/drafts/drf_none:translate", auth: tok, body: `{"target_lang":"fr"}`}),
		403, codeInsufficientScope)
	wantProblem(t, e.do(req{method: "POST", path: "/v1/drafts/" + d.ID + ":translate", body: `{"target_lang":"xx"}`}), 422, codeValidation)
	wantProblem(t, e.do(req{method: "POST", path: "/v1/drafts/" + d.ID + ":frobnicate", body: `{}`, undocumented: true}), 404, codeNotFound)

	r := e.do(req{method: "POST", path: "/v1/drafts/" + d.ID + ":translate", body: `{"target_lang":"FR"}`})
	wantStatus(t, r, 201)
	var tr v1DraftBody
	r.json(&tr)
	var p v1CreatePostReq
	_ = json.Unmarshal(tr.Post, &p)
	if tr.ID == d.ID || *p.Content.Text != "[fr] hello" || p.Content.Languages[0] != "fr" || tr.Tags[0] != "x" {
		t.Fatalf("translated = %+v %s", tr, tr.Post)
	}
	if c := p.Targets[0].Content; c != nil && (c.Text != nil || c.Languages != nil) {
		t.Fatalf("per-account text kept: %s", tr.Post)
	}
	if p.Targets[0].Options["reply_gate"] != "nobody" {
		t.Fatalf("options lost: %s", tr.Post)
	}
	wantStatus(t, e.do(req{method: "GET", path: "/v1/drafts/" + d.ID}), 200) // the original stays
}

// Drafts saved before /v1 are converted once: their media become media
// objects (only files in this server's storage), their platforms the
// accounts, their overrides options and content.
func TestConvertOldDrafts(t *testing.T) {
	e := newV1Env(t, true)
	e.withEngine()
	e.api.media.BlossomURL = "https://blossom.test"
	sha := strings.Repeat("ab", 32)
	spec := map[string]any{
		"master_text": "one\n---\ntwo",
		"platforms":   []string{"bluesky", "mastodon", "threads"}, // no threads account
		"overrides": map[string]any{
			"bluesky":  map[string]any{"langs": []string{"en", "fr"}, "bluesky_reply": "following", "bluesky_disable_quotes": true},
			"mastodon": map[string]any{"text": "masto text", "spoiler_text": "cw", "sensitive": true, "visibility": "unlisted", "language": "de"},
			"nostr":    "not an object",
		},
		"anchors":      map[string]int{"c2": 1},
		"number":       false,
		"scheduled_at": "2030-01-01T09:00:00Z",
	}
	raw, _ := json.Marshal(spec)
	old := &store.Draft{ID: "old1", Title: "Old one", MasterText: "one\n---\ntwo", Tags: []string{"t"}, Spec: string(raw),
		CreatedAt: time.Now(), UpdatedAt: time.Now(), Media: []store.Media{
			{Ordinal: 0, BlossomURL: "https://blossom.test/" + sha + ".png", SHA256: sha, Mime: "image/png", Alt: "a", ClientID: "c1"},
			{Ordinal: 1, BlossomURL: "https://elsewhere.test/" + sha, SHA256: sha, Mime: "image/png", ClientID: "cx"},
			{Ordinal: 2, BlossomURL: "https://blossom.test/" + sha + ".png", SHA256: sha, Mime: "image/png", Alt: "b", ClientID: "c2",
				PosterURL: "https://tracker.example/x.jpg", Dim: "<b>", Blurhash: "bad hash"},
			{Ordinal: 3, BlossomURL: "https://blossom.test/" + sha, SHA256: sha, Mime: "text/html", ClientID: "c3"},
		}}
	if err := e.st.CreateDraft(old); err != nil {
		t.Fatal(err)
	}
	reply, _ := json.Marshal(map[string]any{"master_text": "re", "platforms": []string{"bluesky"},
		"interaction": map[string]any{"action": "reply", "sourceURL": "https://bsky.app/profile/a/post/b", "sourceInput": "at://x"}})
	if err := e.st.CreateDraft(&store.Draft{ID: "old2", MasterText: "re", Spec: string(reply), CreatedAt: time.Now(), UpdatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := e.st.CreateDraft(&store.Draft{ID: "old3", MasterText: "broken", Spec: "{not json", CreatedAt: time.Now(), UpdatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	// Not visible on /v1 until converted.
	wantProblem(t, e.do(req{method: "GET", path: "/v1/drafts/drf_old1"}), 404, codeNotFound)

	if n, failed := e.api.ConvertOldDrafts(context.Background()); n != 3 || failed != 0 {
		t.Fatalf("converted %d, failed %d", n, failed)
	}
	if n, _ := e.api.ConvertOldDrafts(context.Background()); n != 0 {
		t.Fatalf("converted again: %d", n)
	}

	r := e.do(req{method: "GET", path: "/v1/drafts/drf_old1"})
	wantStatus(t, r, 200)
	var d v1DraftBody
	r.json(&d)
	var p v1CreatePostReq
	if err := json.Unmarshal(d.Post, &p); err != nil {
		t.Fatal(err)
	}
	if d.Title != "one" || d.Tags[0] != "t" || *p.Content.Text != "one\n---\ntwo" {
		t.Fatalf("draft = %+v %s", d, d.Post)
	}
	if len(p.Content.Media) != 2 || p.Content.Media[0].ID == p.Content.Media[1].ID || p.Content.Media[0].Part != nil ||
		p.Content.Media[1].Part == nil || *p.Content.Media[1].Part != 1 || *p.Content.Media[1].Alt != "b" {
		t.Fatalf("media = %s", d.Post)
	}
	// What the old routes took from the client is checked: no outside
	// poster, no made-up dimensions; an unknown type is dropped.
	mo, err := e.st.MediaObject(context.Background(), store.DefaultOwner, p.Content.Media[1].ID)
	if err != nil || mo.PosterURL != "" || mo.Dim != "" || mo.Blurhash != "" {
		t.Fatalf("adopted object = %+v, %v", mo, err)
	}
	if len(p.Targets) != 2 || p.Targets[0].Account != e.acct("bluesky") || p.Targets[1].Account != e.acct("mastodon") {
		t.Fatalf("targets = %s", d.Post)
	}
	bs, ms := p.Targets[0], p.Targets[1]
	if bs.Options["reply_gate"] != "following" || bs.Options["disable_quotes"] != true || strings.Join(bs.Content.Languages, ",") != "en,fr" {
		t.Fatalf("bluesky target = %s", d.Post)
	}
	if *ms.Content.Text != "masto text" || *ms.Content.ContentWarning != "cw" || !*ms.Content.Sensitive ||
		ms.Content.Languages[0] != "de" || ms.Options["visibility"] != "unlisted" {
		t.Fatalf("mastodon target = %s", d.Post)
	}
	if p.Threading == nil || p.Threading.Numbering || p.Schedule == nil || *p.Schedule.At != "2030-01-01T09:00:00Z" {
		t.Fatalf("threading/schedule = %s", d.Post)
	}
	// The converted body is one POST /v1/posts takes as it is.
	wantStatus(t, e.do(req{method: "POST", path: "/v1/posts:preview", body: string(d.Post)}), 200)

	r = e.do(req{method: "GET", path: "/v1/drafts/drf_old2"})
	r.json(&d)
	if !strings.Contains(string(d.Post), `"reply_to":{"url":"at://x"}`) || strings.Contains(string(d.Post), "schedule") {
		t.Fatalf("reply draft = %s", d.Post)
	}
	r = e.do(req{method: "GET", path: "/v1/drafts/drf_old3"})
	r.json(&d)
	if !strings.Contains(string(d.Post), `"text":"broken"`) {
		t.Fatalf("broken spec draft = %s", d.Post)
	}
}
