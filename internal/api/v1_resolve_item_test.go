package api

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/geofox/publisher/internal/destination"
	"github.com/geofox/publisher/internal/store"
)

type v1FakeLocator struct {
	can bool
	ref destination.ItemRef
	err error
	got string
}

func (f *v1FakeLocator) CanLocate(string) bool { return f.can }
func (f *v1FakeLocator) Locate(_ context.Context, _, url string) (destination.ItemRef, error) {
	f.got = url
	return f.ref, f.err
}

// seedUnknown saves a three-item thread on one account: the first item
// live, the second unknown (its target given up), the third planned.
func (e *v1Env) seedUnknown(id, platform string) (postPath, target string) {
	e.t.Helper()
	now := time.Now().UTC()
	tg := store.Target{Platform: platform, Status: "partial", GaveUpAt: &now, Items: []store.Item{
		{Ordinal: 0, Text: "one", Status: store.ItemPublished, RemoteID: "r0", RemoteURL: "https://example.com/0", PublishedAt: &now},
		{Ordinal: 1, Text: "two", Status: store.ItemUnknown, ErrorClass: "unknown_outcome", Error: "may have been posted"},
		{Ordinal: 2, Text: "three", Status: store.ItemPlanned},
	}}
	p := &store.Post{ID: id, CreatedAt: now, MasterText: "one two three", Platforms: []string{platform}, Source: "test",
		Status: "partial", Targets: []store.Target{tg}}
	if err := e.savePost(p); err != nil {
		e.t.Fatal(err)
	}
	got, err := e.st.GetPost(id)
	if err != nil {
		e.t.Fatal(err)
	}
	return "/v1/posts/" + postID(id), targetID(got.Targets[0].ID)
}

func TestV1ResolveItem(t *testing.T) {
	e := newV1Env(t, true)
	path, tgt := e.seedUnknown("u1", "bluesky")
	resolve := func(body string) v1Resp {
		return e.do(req{method: "POST", path: path + ":resolve", body: body, badRequest: true})
	}
	// The post needs the owner.
	var got v1PostBody
	r := e.do(req{method: "GET", path: path})
	r.json(&got)
	if got.Status != "needs_attention" {
		t.Fatalf("status = %s", got.Status)
	}

	// Field checks, all at once.
	p := wantProblem(t, resolve(`{"target":"nope","ordinal":-1,"outcome":"maybe"}`), 422, codeValidation)
	if c := fieldCodes(p); c != "/target:invalid /ordinal:invalid /outcome:invalid" {
		t.Fatalf("errors = %s", c)
	}
	p = wantProblem(t, resolve(`{"target":"`+tgt+`","ordinal":1,"outcome":"not_published","url":"https://x.example/1"}`), 422, codeValidation)
	if c := fieldCodes(p); c != "/url:not_allowed" {
		t.Fatalf("errors = %s", c)
	}
	p = wantProblem(t, resolve(`{"target":"`+tgt+`","ordinal":1,"outcome":"published","url":"javascript:alert(1)"}`), 422, codeValidation)
	if c := fieldCodes(p); c != "/url:invalid" {
		t.Fatalf("errors = %s", c)
	}
	// Plain http and user info are refused too: the URL may be stored and
	// shown as the post's link.
	for _, u := range []string{"http://x.example/1", "https://u:p@x.example/1"} {
		p = wantProblem(t, resolve(`{"target":"`+tgt+`","ordinal":1,"outcome":"published","url":"`+u+`"}`), 422, codeValidation)
		if c := fieldCodes(p); c != "/url:invalid" {
			t.Fatalf("%s: errors = %s", u, c)
		}
	}
	p = wantProblem(t, resolve(`{"target":"tgt_999999","ordinal":1,"outcome":"published"}`), 422, codeValidation)
	if c := fieldCodes(p); c != "/target:unknown_target" {
		t.Fatalf("errors = %s", c)
	}
	p = wantProblem(t, resolve(`{"target":"`+tgt+`","ordinal":7,"outcome":"published"}`), 422, codeValidation)
	if c := fieldCodes(p); c != "/ordinal:unknown_item" {
		t.Fatalf("errors = %s", c)
	}
	wantProblem(t, resolve(`{"target":"`+tgt+`","ordinal":0,"outcome":"published"}`), 409, codeInvalidState)

	// Lookups: a thread with more to send needs the URL; the URL must be
	// the account's own post; the network may be down.
	loc := &v1FakeLocator{can: true}
	e.api.Locator = loc
	p = wantProblem(t, resolve(`{"target":"`+tgt+`","ordinal":1,"outcome":"published"}`), 422, codeValidation)
	if c := fieldCodes(p); c != "/url:required" {
		t.Fatalf("errors = %s", c)
	}
	for _, c := range []struct {
		err    error
		status int
		code   string
	}{
		{destination.ErrPostNotFound, 422, "/url:not_found"},
		{destination.ErrNotOwnPost, 422, "/url:not_own"},
		{destination.Invalid(errors.New("not a post URL")), 422, "/url:invalid"},
		{&destination.Error{Class: destination.ClassAuth, Err: errors.New("missing read:search")}, 422, "/url:not_permitted"},
		{errors.New("dial tcp: i/o timeout"), 502, ""},
	} {
		loc.err = c.err
		r := resolve(`{"target":"` + tgt + `","ordinal":1,"outcome":"published","url":"https://bsky.app/profile/me/post/2"}`)
		if c.status == 502 {
			wantProblem(t, r, 502, codeUpstream)
			continue
		}
		if got := fieldCodes(wantProblem(t, r, c.status, codeValidation)); got != c.code {
			t.Fatalf("%v: errors = %s", c.err, got)
		}
	}

	// Another part of the same thread.
	loc.err, loc.ref = nil, destination.ItemRef{ID: "r0", URL: "https://example.com/0"}
	p = wantProblem(t, resolve(`{"target":"`+tgt+`","ordinal":1,"outcome":"published","url":"https://example.com/0"}`), 422, codeValidation)
	if c := fieldCodes(p); c != "/url:duplicate" {
		t.Fatalf("errors = %s", c)
	}

	// Found: the item is live with the post's reference, and the thread
	// goes on.
	loc.err = nil
	loc.ref = destination.ItemRef{ID: "at://me/2", URL: "https://bsky.app/profile/me.example/post/2", Data: []byte(`{"cid":"c2"}`)}
	r = resolve(`{"target":"` + tgt + `","ordinal":1,"outcome":"published","url":" https://bsky.app/profile/me/post/2 "}`)
	wantStatus(t, r, 200)
	r.json(&got)
	items := *got.Targets[0].Items
	if loc.got != "https://bsky.app/profile/me/post/2" || items[1].Status != "published" ||
		items[1].URL != loc.ref.URL || items[1].RemoteID != "at://me/2" || got.Status != "publishing" {
		t.Fatalf("post = %+v, items = %+v", got, items)
	}
	sp, _ := e.st.GetPost("u1")
	if string(sp.Targets[0].Items[1].Ref) != `{"cid":"c2"}` || sp.Targets[0].Items[2].NextAttemptAt == nil {
		t.Fatalf("stored items = %+v", sp.Targets[0].Items)
	}
	wantProblem(t, resolve(`{"target":"`+tgt+`","ordinal":1,"outcome":"published"}`), 409, codeInvalidState)
}

func TestV1ResolveItemWithoutLookups(t *testing.T) {
	e := newV1Env(t, true)
	// A network whose posts can't be looked up keeps the URL as given; the
	// rest of the thread waits for a retry by hand.
	path, tgt := e.seedUnknown("u2", "bluesky")
	e.api.Locator = &v1FakeLocator{can: false}
	r := e.do(req{method: "POST", path: path + ":resolve", body: `{"target":"` + tgt + `","ordinal":1,"outcome":"published","url":"https://x.example/2"}`})
	wantStatus(t, r, 200)
	var got v1PostBody
	r.json(&got)
	if items := *got.Targets[0].Items; items[1].URL != "https://x.example/2" || got.Status != "partial" || items[2].Status != "canceled" {
		t.Fatalf("post = %+v", got)
	}

	// Not published: sent again.
	path, tgt = e.seedUnknown("u3", "bluesky")
	r = e.do(req{method: "POST", path: path + ":resolve", body: `{"target":"` + tgt + `","ordinal":1,"outcome":"not_published"}`})
	wantStatus(t, r, 200)
	r.json(&got)
	if items := *got.Targets[0].Items; items[1].Status != "failed" || got.Status != "publishing" {
		t.Fatalf("post = %+v", got)
	}

	// Needs posts:write.
	_, raw, err := e.st.CreateScopedToken("r", []string{"posts:read"}, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	wantProblem(t, e.do(req{method: "POST", path: path + ":resolve", auth: raw,
		body: `{"target":"` + tgt + `","ordinal":1,"outcome":"not_published"}`}), 403, codeInsufficientScope)
}
