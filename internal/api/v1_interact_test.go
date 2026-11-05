package api

import (
	"context"
	"strings"
	"testing"
	"time"

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
				Media: []resolve.Media{{URL: "https://cdn.example/a.jpg", Alt: "a cat"}}},
			Caps: resolve.Caps{Reply: yes, Quote: yes, Repost: yes}}, nil
	case mastoSource:
		return &resolve.SourceRef{Platform: "mastodon", Ref: resolve.PlatformRef{LocalID: "1"},
			Preview: resolve.Preview{AuthorHandle: "bob@masto.example", Text: "toot", WebURL: mastoSource},
			Caps: resolve.Caps{Reply: yes, Quote: yes, Repost: resolve.Cap{Reason: "private posts can't be boosted"}}}, nil
	}
	return nil, resolve.ErrUnrecognized
}

func TestV1CreateInteractions(t *testing.T) {
	e := newV1Env(t, true)
	e.withEngine()
	e.api.Resolve = sourcesResolver{}
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
			[]string{"/targets/0/content/text:not_allowed"}},
		{"not allowed", `{"content":{},"targets":[{"account":"` + masto + `"}],"repost":{"url":"` + mastoSource + `"}}`,
			[]string{"/repost/url:not_allowed"}},
		{"scheduled", `{"content":{"text":"x"},"targets":[{"account":"` + bsky + `"}],"reply_to":{"url":"` + bskySource + `"},"schedule":{"at":"` + at + `"}}`,
			[]string{"/schedule:not_supported"}},
		{"two actions", `{"content":{"text":"x"},"targets":[{"account":"` + bsky + `"}],"reply_to":{"url":"` + bskySource + `"},"quote":{"url":"` + bskySource + `"}}`,
			[]string{":conflict"}},
		{"unresolvable", `{"content":{"text":"x"},"targets":[{"account":"` + bsky + `"}],"reply_to":{"url":"https://nowhere.example/x"}}`,
			[]string{"/reply_to/url:unresolvable"}},
		{"reply needs text", `{"content":{},"targets":[{"account":"` + bsky + `"}],"reply_to":{"url":"` + bskySource + `"}}`,
			[]string{"/content/text:required"}},
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
	if !slicesHasPrefix(pv.Targets[1].Warnings, "the original's images are copied") {
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
	if p.Errors[0].Code != "unresolvable" {
		t.Fatalf("errors = %+v", p.Errors)
	}
	// No account on the post's network: nothing is allowed.
	if _, err := e.st.SeedEnvAccounts(context.Background(), store.DefaultOwner, []store.EnvAccount{{Type: "mastodon", Label: "Mastodon"}}); err != nil {
		t.Fatal(err)
	}
	r = e.do(req{method: "POST", path: "/v1/resolve", body: `{"url":"` + bskySource + `"}`})
	wantStatus(t, r, 200)
	r.json(&got)
	if got.Account != nil || got.Actions.Reply.Allowed || got.CreatedAt == nil {
		t.Fatalf("resolve = %+v", got)
	}
}
