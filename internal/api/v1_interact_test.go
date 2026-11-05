package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/geofox/publisher/internal/destination"
	"github.com/geofox/publisher/internal/resolve"
	"github.com/geofox/publisher/internal/store"
)

const (
	bskySource  = "https://bsky.app/profile/ann.example/post/3k"
	mastoSource = "https://masto.example/@bob/1"
)

// sourcesResolver knows two posts: one on Bluesky (everything allowed) and
// one on Mastodon that can't be reposted.
type sourcesResolver struct{}

func (sourcesResolver) Resolve(_ context.Context, in string) (*resolve.SourceRef, error) {
	yes := resolve.Cap{Allowed: true}
	switch in {
	case bskySource:
		return &resolve.SourceRef{Platform: "bluesky",
			Ref: resolve.PlatformRef{URI: "at://did:plc:ann/app.bsky.feed.post/3k", CID: "bafy"},
			Preview: resolve.Preview{AuthorName: "Ann", AuthorHandle: "ann.example", Text: "the original",
				WebURL: bskySource, CreatedAt: time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC),
				Media: []resolve.Media{{URL: "https://cdn.example/a.jpg", Alt: "a cat"}, {URL: "https://cdn.example/v.mp4", Video: true}}},
			Caps: resolve.Caps{Reply: yes, Quote: yes, Repost: yes}}, nil
	case mastoSource:
		return &resolve.SourceRef{Platform: "mastodon", Ref: resolve.PlatformRef{LocalID: "1"},
			Preview: resolve.Preview{AuthorHandle: "bob@masto.example", Text: "toot", WebURL: mastoSource},
			Caps: resolve.Caps{Reply: yes, Quote: resolve.Cap{Allowed: true, Reason: "needs the author's approval (lands pending)"},
				Repost: resolve.Cap{Reason: "private posts can't be boosted"}}}, nil
	case "https://slow.example/1":
		return nil, context.DeadlineExceeded
	case "https://down.example/1":
		return nil, &destination.HTTPError{Op: "getPosts", StatusCode: 503, Body: "secret upstream detail"}
	case "https://gone.example/1":
		return nil, &destination.HTTPError{Op: "getPosts", StatusCode: 404, Body: "secret upstream detail"}
	}
	return nil, resolve.ErrUnrecognized
}

func TestV1CreateInteractions(t *testing.T) {
	e := newV1Env(t, true)
	e.withEngine()
	e.api.Resolve = sourcesResolver{}
	png := pngBytes(t, 4, 4)
	e.api.fetchMedia = func(_ context.Context, u string) ([]byte, string, error) {
		if u != "https://cdn.example/a.jpg" {
			t.Errorf("fetched %s: only the original's image should be copied", u)
		}
		return png, "image/png", nil
	}
	bsky, masto := e.acct("bluesky"), e.acct("mastodon")
	post := func(body string) *store.Post {
		t.Helper()
		r := e.do(req{method: "POST", path: "/v1/posts", body: body})
		wantStatus(t, r, 202)
		var p v1PostBody
		r.json(&p)
		got, err := e.st.GetPost(strings.TrimPrefix(p.ID, prefixPost))
		if err != nil {
			t.Fatal(err)
		}
		if p.Interaction == nil || got.Source != "api" {
			t.Fatalf("post = %+v", p)
		}
		return got
	}

	p := post(`{"content":{"text":"nice"},"targets":[{"account":"` + bsky + `"}],"reply_to":{"url":"` + bskySource + `"}}`)
	ix := p.Interaction
	if ix.Action != "reply" || ix.SourcePlatform != "bluesky" || ix.SourceAuthor != "ann.example" ||
		!strings.Contains(string(ix.Ref), "at://did:plc:ann") {
		t.Fatalf("interaction = %+v", ix)
	}
	if len(p.Targets) != 1 || p.Targets[0].AccountID != bsky || p.Targets[0].FinalText != "nice" {
		t.Fatalf("targets = %+v", p.Targets)
	}

	// A quote with a link-quote on Mastodon.
	p = post(`{"content":{"text":"look"},"targets":[{"account":"` + bsky + `"},{"account":"` + masto + `"}],"quote":{"url":"` + bskySource + `"}}`)
	if len(p.Targets) != 2 || p.Targets[1].AccountID != masto {
		t.Fatalf("targets = %+v", p.Targets)
	}
	if lq := p.Targets[1].FinalText; !strings.Contains(lq, "look") || !strings.Contains(lq, "— ann.example:\nthe original") ||
		!strings.Contains(lq, bskySource) {
		t.Fatalf("link-quote = %q", lq)
	}
	// The link-quote carries the original's image (not its video).
	if it := p.Targets[1].Items; len(it) != 1 || len(it[0].Attach) != 1 || it[0].Attach[0].Alt != "a cat" {
		t.Fatalf("link-quote items = %+v", it)
	}
	if len(p.Targets[0].Items[0].Attach) != 0 {
		t.Fatalf("the native quote must not copy the original's media: %+v", p.Targets[0].Items)
	}

	p = post(`{"content":{},"targets":[{"account":"` + bsky + `"}],"repost":{"url":"` + bskySource + `"}}`)
	if p.Interaction.Action != "repost" || len(p.Targets) != 1 || len(p.Targets[0].Items) != 1 {
		t.Fatalf("repost = %+v", p)
	}
}

func TestV1CreateInteractionValidation(t *testing.T) {
	e := newV1Env(t, true)
	e.withEngine()
	e.api.Resolve = sourcesResolver{}
	bsky, masto := e.acct("bluesky"), e.acct("mastodon")
	vidObj := &store.MediaObject{Status: store.MediaReady, Mime: "video/mp4", URL: "https://b/v", SizeBytes: 10, DurationSecs: 5}
	if err := e.st.CreateMediaObject(context.Background(), vidObj); err != nil {
		t.Fatal(err)
	}
	vid := vidObj.ID
	at := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	cases := []struct {
		name, body string
		want       []string
	}{
		{"reply elsewhere", `{"content":{"text":"x"},"targets":[{"account":"` + bsky + `"},{"account":"` + masto + `"}],"reply_to":{"url":"` + bskySource + `"}}`,
			[]string{"/targets/1/account:not_allowed"}},
		{"no source account", `{"content":{"text":"x"},"targets":[{"account":"` + masto + `"}],"quote":{"url":"` + bskySource + `"}}`,
			[]string{"/targets:missing_source_account"}},
		{"repost with text", `{"content":{"text":"x"},"targets":[{"account":"` + bsky + `"}],"repost":{"url":"` + bskySource + `"}}`,
			[]string{"/content:not_allowed"}},
		{"repost override", `{"content":{},"targets":[{"account":"` + bsky + `","content":{"text":"y"}}],"repost":{"url":"` + bskySource + `"}}`,
			[]string{"/targets/0/content:not_allowed"}},
		{"repost options", `{"content":{"content_warning":"cw"},"targets":[{"account":"` + bsky + `","options":{"reply_gate":"nobody"}}],"repost":{"url":"` + bskySource + `"}}`,
			[]string{"/content:not_allowed", "/targets/0/options:not_allowed"}},
		{"masto quote video", `{"content":{"media":[{"id":"` + vid + `"}]},"targets":[{"account":"` + masto + `"}],"quote":{"url":"` + mastoSource + `"}}`,
			[]string{"/content/media/0:media_unsupported"}},
		{"static checks with others", `{"content":{"text":"x"},"targets":[{"account":"nope"}],"reply_to":{"url":"` + bskySource + `"},"schedule":{"at":"` + at + `"}}`,
			[]string{"/targets/0/account:unknown_account", "/schedule:not_supported"}},
		{"unrecognized", `{"content":{"text":"x"},"targets":[{"account":"` + bsky + `"}],"reply_to":{"url":"nope"}}`,
			[]string{"/reply_to/url:unrecognized"}},
		{"long url", `{"content":{"text":"x"},"targets":[{"account":"` + bsky + `"}],"reply_to":{"url":"https://x.example/` + strings.Repeat("a", 2048) + `"}}`,
			[]string{"/reply_to/url:too_long"}},
		{"not allowed", `{"content":{},"targets":[{"account":"` + masto + `"}],"repost":{"url":"` + mastoSource + `"}}`,
			[]string{"/repost/url:not_allowed"}},
		{"scheduled", `{"content":{"text":"x"},"targets":[{"account":"` + bsky + `"}],"reply_to":{"url":"` + bskySource + `"},"schedule":{"at":"` + at + `"}}`,
			[]string{"/schedule:not_supported"}},
		{"two actions", `{"content":{"text":"x"},"targets":[{"account":"` + bsky + `"}],"reply_to":{"url":"` + bskySource + `"},"quote":{"url":"` + bskySource + `"}}`,
			[]string{":conflict"}},
		{"unresolvable", `{"content":{"text":"x"},"targets":[{"account":"` + bsky + `"}],"reply_to":{"url":"https://gone.example/1"}}`,
			[]string{"/reply_to/url:unresolvable"}},
		{"reply needs text", `{"content":{},"targets":[{"account":"` + bsky + `"}],"reply_to":{"url":"` + bskySource + `"}}`,
			[]string{"/content/text:required"}},
		{"media part", `{"content":{"text":"x","media":[{"id":"` + vid + `","part":1}]},"targets":[{"account":"` + bsky + `"}],"reply_to":{"url":"` + bskySource + `"}}`,
			[]string{"/content/media/0/part:not_supported"}},
	}
	for _, c := range cases {
		for _, path := range []string{"/v1/posts", "/v1/posts:preview"} {
			p := wantProblem(t, e.do(req{method: "POST", path: path, body: c.body, badRequest: true}), 422, codeValidation)
			var got []string
			for _, fe := range p.Errors {
				got = append(got, fe.Pointer+":"+fe.Code)
				if strings.Contains(fe.Detail, "secret") {
					t.Errorf("%s: upstream detail leaked: %q", c.name, fe.Detail)
				}
			}
			if strings.Join(got, " ") != strings.Join(c.want, " ") {
				t.Errorf("%s %s: errors = %v, want %v", c.name, path, got, c.want)
			}
		}
	}
	// The source's network being down is not the client's fault: 502, to
	// retry.
	for _, u := range []string{"https://slow.example/1", "https://down.example/1"} {
		for _, path := range []string{"/v1/posts", "/v1/posts:preview"} {
			p := wantProblem(t, e.do(req{method: "POST", path: path, badRequest: true,
				body: `{"content":{"text":"x"},"targets":[{"account":"` + bsky + `"}],"reply_to":{"url":"` + u + `"}}`}), 502, codeUpstream)
			if strings.Contains(p.Detail, "secret") {
				t.Errorf("upstream detail leaked: %q", p.Detail)
			}
		}
	}
	if n := e.countPosts(); n != 0 {
		t.Fatalf("an invalid request saved %d posts", n)
	}
	// A quote may have no text of its own.
	wantStatus(t, e.do(req{method: "POST", path: "/v1/posts",
		body: `{"content":{},"targets":[{"account":"` + bsky + `"}],"quote":{"url":"` + bskySource + `"}}`}), 202)
}

func TestV1PreviewQuote(t *testing.T) {
	e := newV1Env(t, true)
	e.withEngine()
	e.api.Resolve = sourcesResolver{}
	r := e.do(req{method: "POST", path: "/v1/posts:preview", body: `{"content":{"text":"look"},"targets":[{"account":"` +
		e.acct("bluesky") + `"},{"account":"` + e.acct("mastodon") + `"}],"quote":{"url":"` + bskySource + `"}}`})
	wantStatus(t, r, 200)
	var pv v1PreviewBody
	r.json(&pv)
	if len(pv.Targets) != 2 || pv.Targets[0].Role != "quote" || pv.Targets[1].Role != "link_quote" {
		t.Fatalf("preview = %+v", pv)
	}
	if pv.Targets[0].Items[0].Text != "look" || !strings.Contains(pv.Targets[1].Items[0].Text, "the original") {
		t.Fatalf("items = %+v / %+v", pv.Targets[0].Items, pv.Targets[1].Items)
	}
	if !slicesHasPrefix(pv.Targets[1].Warnings, "up to 1 of the original's images are copied") {
		t.Fatalf("warnings = %v", pv.Targets[1].Warnings)
	}
}

func TestV1Resolve(t *testing.T) {
	e := newV1Env(t, true)
	e.api.Resolve = sourcesResolver{}
	r := e.do(req{method: "POST", path: "/v1/resolve", body: `{"url":"` + mastoSource + `"}`})
	wantStatus(t, r, 200)
	var got v1ResolveBody
	r.json(&got)
	if got.Platform != "mastodon" || got.Account == nil || *got.Account != e.acct("mastodon") ||
		!got.Actions.Reply.Allowed || got.Actions.Repost.Allowed || got.Actions.Repost.Reason == "" {
		t.Fatalf("resolve = %+v", got)
	}
	p := wantProblem(t, e.do(req{method: "POST", path: "/v1/resolve", body: `{"url":"nope"}`, badRequest: true}), 422, codeValidation)
	if p.Errors[0].Code != "unrecognized" {
		t.Fatalf("errors = %+v", p.Errors)
	}
	p = wantProblem(t, e.do(req{method: "POST", path: "/v1/resolve", body: `{"url":"https://gone.example/1"}`, badRequest: true}), 422, codeValidation)
	if p.Errors[0].Code != "unresolvable" || strings.Contains(p.Errors[0].Detail, "secret") {
		t.Fatalf("errors = %+v", p.Errors)
	}
	wantProblem(t, e.do(req{method: "POST", path: "/v1/resolve", body: `{"url":"https://down.example/1"}`, badRequest: true}), 502, codeUpstream)
	e.api.Resolve = nil
	p = wantProblem(t, e.do(req{method: "POST", path: "/v1/resolve", body: `{"url":"` + bskySource + `"}`, badRequest: true}), 422, codeValidation)
	if p.Errors[0].Code != "not_configured" {
		t.Fatalf("errors = %+v", p.Errors)
	}
	e.api.Resolve = sourcesResolver{}
	// No account on the post's network: nothing is allowed.
	if _, err := e.st.SeedEnvAccounts(context.Background(), store.DefaultOwner, []store.EnvAccount{{Type: "mastodon", Label: "Mastodon"}}); err != nil {
		t.Fatal(err)
	}
	r = e.do(req{method: "POST", path: "/v1/resolve", body: `{"url":"` + bskySource + `"}`})
	wantStatus(t, r, 200)
	r.json(&got)
	if len(got.Media) != 2 || got.Media[0].Type != "image" || got.Media[1].Type != "video" {
		t.Fatalf("media = %+v", got.Media)
	}
	if got.Account != nil || got.Actions.Reply.Allowed || got.CreatedAt == nil {
		t.Fatalf("resolve = %+v", got)
	}
}

// TestFetchSourceImageLimits: an original's "image" that says it is a video,
// or is too large, is refused before (or while) its body is read.
func TestFetchSourceImageLimits(t *testing.T) {
	big := make([]byte, maxSourceImageBytes+1)
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/video":
			w.Header().Set("Content-Type", "video/mp4")
		case "/big":
			w.Header().Set("Content-Type", "image/png")
			w.Header().Set("Content-Length", strconv.Itoa(len(big)))
		case "/chunked":
			w.Header().Set("Content-Type", "image/png")
			w.(http.Flusher).Flush() // no Content-Length
		}
		w.Write(big)
	}))
	defer srv.Close()
	old := sourceMediaClient
	sourceMediaClient = srv.Client()
	defer func() { sourceMediaClient = old }()
	a := &API{}
	for _, path := range []string{"/video", "/big", "/chunked"} {
		if _, _, err := a.fetchSourceImage(context.Background(), srv.URL+path); err == nil {
			t.Errorf("%s: fetched, want refused", path)
		}
	}
	if _, _, err := a.fetchSourceImage(context.Background(), strings.Replace(srv.URL, "https", "http", 1)+"/x"); err == nil {
		t.Error("http: fetched, want refused")
	}
}
