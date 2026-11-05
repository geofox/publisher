package api

import (
	"context"
	"strings"
	"testing"

	"github.com/geofox/publisher/internal/store"
	"github.com/geofox/publisher/internal/unfurl"
)

type fakeUnfurl struct{ card *unfurl.Card }

func (f *fakeUnfurl) Unfurl(_ context.Context, _ string) (*unfurl.Card, error) {
	if f.card == nil {
		return nil, context.DeadlineExceeded
	}
	return f.card, nil
}

func (e *v1Env) preview(body string) v1PreviewBody {
	e.t.Helper()
	r := e.do(req{method: "POST", path: "/v1/posts:preview", body: body})
	wantStatus(e.t, r, 200)
	var pv v1PreviewBody
	r.json(&pv)
	return pv
}

// mediaObject stores a ready media object with the given description.
func (e *v1Env) mediaObject(mime, dim string, size, secs int64) string {
	e.t.Helper()
	m := &store.MediaObject{OwnerID: store.DefaultOwner, Status: store.MediaReady, Mime: mime, Dim: dim,
		SizeBytes: size, DurationSecs: secs, URL: "https://blossom.test/" + strings.Repeat("c", 64)}
	if err := e.st.CreateMediaObject(context.Background(), m); err != nil {
		e.t.Fatal(err)
	}
	return m.ID
}

// A link card on a client-side link-preview destination (Bluesky): the
// preview shows the text without the URL and the card on its item; with
// media, or for a reply, no card.
func TestV1PreviewLinkCard(t *testing.T) {
	e := newV1Env(t, true)
	e.withEngine()
	bsky := e.acct("bluesky")
	post := func(content string) string {
		return `{"content":` + content + `,"targets":[{"account":"` + bsky + `"}]}`
	}
	// Without an unfurler, no card.
	if pv := e.preview(post(`{"text":"hello https://x.com/a"}`)); pv.Targets[0].LinkCard != nil {
		t.Fatalf("card without an unfurler: %+v", pv.Targets[0].LinkCard)
	}
	e.api.Unfurl = &fakeUnfurl{card: &unfurl.Card{URI: "https://x.com/a", Title: "T", Description: "D", ThumbURL: "https://x.com/t.jpg"}}

	pv := e.preview(post(`{"text":"hello https://x.com/a"}`))
	bt := pv.Targets[0]
	if bt.LinkCard == nil || bt.LinkCard.Title != "T" || bt.LinkCard.Item != 0 || bt.LinkCard.ThumbURL != "https://x.com/t.jpg" {
		t.Fatalf("card = %+v", bt.LinkCard)
	}
	if bt.Items[0].Text != "hello" {
		t.Fatalf("text = %q, want the URL taken out", bt.Items[0].Text)
	}

	// A thread: the card goes on the last item.
	long := strings.Repeat("word ", 120) + "\\nhttps://x.com/a"
	bt = e.preview(post(`{"text":"` + long + `"}`)).Targets[0]
	if len(bt.Items) < 2 || bt.LinkCard == nil || bt.LinkCard.Item != len(bt.Items)-1 {
		t.Fatalf("thread card = %+v over %d items", bt.LinkCard, len(bt.Items))
	}

	// Media take the embed: no card, the URL stays.
	img := e.upload("x")
	bt = e.preview(post(`{"text":"hello https://x.com/a","media":[{"id":"` + img + `"}]}`)).Targets[0]
	if bt.LinkCard != nil || bt.Items[0].Text != "hello https://x.com/a" {
		t.Fatalf("with media: card %+v, text %q", bt.LinkCard, bt.Items[0].Text)
	}
}

// Media notes: an image over a destination's size is noted (to be
// re-encoded); a video's limits are checked per account.
func TestV1PreviewMediaNotes(t *testing.T) {
	e := newV1Env(t, true)
	e.withEngine()
	nostr := e.withNostr()
	big := e.mediaObject("image/jpeg", "4000x3000", 3_000_000, 0)
	pv := e.preview(`{"content":{"text":"hello","media":[{"id":"` + big + `"}]},"targets":[{"account":"` +
		e.acct("bluesky") + `"},{"account":"` + nostr + `"}]}`)
	bs, ns := pv.Targets[0], pv.Targets[1]
	if len(bs.Media) != 1 || bs.Media[0].Media != 0 || !strings.Contains(bs.Media[0].Note, "JPEG") {
		t.Fatalf("bluesky notes = %+v", bs.Media)
	}
	if len(ns.Media) != 0 {
		t.Fatalf("nostr notes = %+v, want none", ns.Media)
	}

	// A video over Bluesky's size is refused for that account; a long one
	// within it gets a warning.
	huge := e.mediaObject("video/mp4", "1920x1080", 150_000_000, 400)
	r := e.do(req{method: "POST", path: "/v1/posts:preview", body: `{"content":{"text":"clip","media":[{"id":"` + huge +
		`"}]},"targets":[{"account":"` + e.acct("bluesky") + `"}]}`})
	p := wantProblem(t, r, 422, codeValidation)
	if p.Errors[0].Code != "media_unsupported" || !strings.Contains(p.Errors[0].Detail, "100 MB") {
		t.Fatalf("errors = %+v", p.Errors)
	}
	long := e.mediaObject("video/mp4", "1920x1080", 20_000_000, 400)
	bs = e.preview(`{"content":{"text":"clip","media":[{"id":"` + long + `"}]},"targets":[{"account":"` + e.acct("bluesky") + `"}]}`).Targets[0]
	if !slicesHasPrefix(bs.Warnings, "") || !strings.Contains(strings.Join(bs.Warnings, " "), "3 min") {
		t.Fatalf("bluesky warnings = %v", bs.Warnings)
	}
}
