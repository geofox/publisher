package dispatch

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/geofox/publisher/internal/store"
	"github.com/geofox/publisher/internal/unfurl"
)

// fakeUnfurler scripts the Unfurler interface for dispatch tests.
type fakeUnfurler struct {
	card   *unfurl.Card
	err    error
	thumbs int
}

func (f *fakeUnfurler) Unfurl(_ context.Context, _ string) (*unfurl.Card, error) {
	return f.card, f.err
}
func (f *fakeUnfurler) Thumb(_ context.Context, _ string) ([]byte, string, error) {
	f.thumbs++
	return []byte("img"), "image/jpeg", nil
}

func TestPostAttachesCardAndStripsTrailingURL(t *testing.T) {
	f := &fakeBsky{failAt: -1}
	uf := &fakeUnfurler{card: &unfurl.Card{URI: "https://x.com/a", Title: "T", Description: "D"}}
	d := &Dispatcher{Bluesky: f, Unfurler: uf}
	rec := d.Post(context.Background(), PostSpec{
		MasterText: "hello https://x.com/a", Platforms: []string{"bluesky"},
	})
	if len(f.calls) != 1 || f.calls[0].text != "hello" {
		t.Fatalf("calls: %+v", f.calls)
	}
	if f.calls[0].card == nil || f.calls[0].card.URI != "https://x.com/a" {
		t.Fatalf("card not forwarded: %+v", f.calls[0].card)
	}
	tg := rec.Targets[0]
	if tg.FinalText != "hello" {
		t.Fatalf("FinalText must be the stripped text: %q", tg.FinalText)
	}
	var fields struct {
		LinkCard *unfurl.Card `json:"link_card"`
	}
	if err := json.Unmarshal([]byte(tg.FieldsJSON), &fields); err != nil {
		t.Fatal(err)
	}
	if fields.LinkCard == nil || fields.LinkCard.Segment != 0 {
		t.Fatalf("card not persisted in fields_json: %s", tg.FieldsJSON)
	}
}

func TestPostUnfurlFailureFallsBackToPlainLink(t *testing.T) {
	f := &fakeBsky{failAt: -1}
	uf := &fakeUnfurler{err: context.DeadlineExceeded}
	d := &Dispatcher{Bluesky: f, Unfurler: uf}
	rec := d.Post(context.Background(), PostSpec{
		MasterText: "hello https://x.com/a", Platforms: []string{"bluesky"},
	})
	if f.calls[0].text != "hello https://x.com/a" || f.calls[0].card != nil {
		t.Fatalf("must post unchanged on unfurl failure: %+v", f.calls[0])
	}
	if !strings.Contains(rec.Targets[0].FieldsJSON, `"link_card":null`) {
		t.Fatalf("no card must persist: %s", rec.Targets[0].FieldsJSON)
	}
}

func TestPostImagesWinOverCard(t *testing.T) {
	f := &fakeBsky{failAt: -1}
	uf := &fakeUnfurler{card: &unfurl.Card{URI: "https://x.com/a", Title: "T"}}
	d := &Dispatcher{Bluesky: f, Unfurler: uf}
	d.Post(context.Background(), PostSpec{
		MasterText: "hello https://x.com/a", Platforms: []string{"bluesky"},
		Images: []Img{{Bytes: []byte("i"), Mime: "image/png"}},
	})
	if f.calls[0].card != nil {
		t.Fatal("images own the embed slot — no card")
	}
	if f.calls[0].text != "hello https://x.com/a" {
		t.Fatalf("revert must keep the URL: %q", f.calls[0].text)
	}
}

func TestThreadedCardOnLastSegmentOnly(t *testing.T) {
	f := &fakeBsky{failAt: -1}
	uf := &fakeUnfurler{card: &unfurl.Card{URI: "https://x.com/a", Title: "T"}}
	d := &Dispatcher{Bluesky: f, Unfurler: uf}
	long := strings.Repeat("word ", 120) + "\nhttps://x.com/a"
	d.Post(context.Background(), PostSpec{MasterText: long, Platforms: []string{"bluesky"}, Number: true})
	if len(f.calls) < 2 {
		t.Fatalf("expected a chain, got %d calls", len(f.calls))
	}
	for i, c := range f.calls {
		wantCard := i == len(f.calls)-1
		if (c.card != nil) != wantCard {
			t.Fatalf("segment %d: card=%v, want present=%v", i, c.card, wantCard)
		}
	}
}

func TestResumeReattachesPersistedCard(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	c := &unfurl.Card{URI: "https://x.com/a", Title: "T", ThumbURL: "https://x.com/t.jpg", Segment: 1}
	fields, _ := json.Marshal(map[string]any{"link_card": c})
	post := &store.Post{
		ID: "p1", CreatedAt: time.Now().UTC(), Platforms: []string{"bluesky"},
		Source: "web", Status: "partial", MasterText: "irrelevant",
		Targets: []store.Target{{
			Platform: "bluesky", Status: "partial", FinalText: "irrelevant",
			FieldsJSON: string(fields),
			Segments: []store.Segment{
				{Ordinal: 0, Text: "seg0", RemoteID: "at://post0", CID: "cid0", Status: "success"},
				{Ordinal: 1, Text: "seg1", Status: "failed"},
			},
		}},
	}
	if err := db.SavePost(post); err != nil {
		t.Fatal(err)
	}
	f := &fakeBsky{failAt: -1}
	uf := &fakeUnfurler{}
	d := &Dispatcher{Bluesky: f, Store: db, Unfurler: uf}
	if _, err := d.Retry(context.Background(), "p1", nil); err != nil {
		t.Fatal(err)
	}
	if len(f.calls) != 1 || f.calls[0].text != "seg1" {
		t.Fatalf("resume calls: %+v", f.calls)
	}
	if f.calls[0].card == nil || f.calls[0].card.URI != "https://x.com/a" {
		t.Fatalf("persisted card not re-attached: %+v", f.calls[0].card)
	}
	if string(f.calls[0].card.ThumbData) != "img" || uf.thumbs != 1 {
		t.Fatalf("thumb must be re-downloaded on resume: %+v (thumbs=%d)", f.calls[0].card, uf.thumbs)
	}
}

func TestPostScrubsInjectedLinkCard(t *testing.T) {
	f := &fakeBsky{failAt: -1}
	// No Unfurler wired: attachLinkCard is a no-op, so an injected card would
	// survive to the adapter without the boundary scrub.
	d := &Dispatcher{Bluesky: f}
	rec := d.Post(context.Background(), PostSpec{
		MasterText: "hello https://x.com/a", Platforms: []string{"bluesky"},
		Overrides: map[string]Overrides{"bluesky": {LinkCard: &unfurl.Card{URI: "https://x.com/a", Title: "Crafted"}}},
	})
	if f.calls[0].card != nil {
		t.Fatalf("injected link_card must be scrubbed: %+v", f.calls[0].card)
	}
	if !strings.Contains(rec.Targets[0].FieldsJSON, `"link_card":null`) {
		t.Fatalf("injected card must not persist: %s", rec.Targets[0].FieldsJSON)
	}
}

func TestInteractScrubsInjectedLinkCard(t *testing.T) {
	f := &fakeBsky{failAt: -1}
	d := &Dispatcher{Bluesky: f}
	rec := d.Interact(context.Background(), InteractSpec{
		Action: "reply", SourcePlatform: "bluesky",
		Ref:       InteractRef{URI: "at://src", CID: "c1"},
		Text:      "nice post https://x.com/a",
		Overrides: map[string]Overrides{"bluesky": {LinkCard: &unfurl.Card{URI: "https://x.com/a", Title: "Crafted"}}},
	})
	if len(f.calls) == 0 || f.calls[0].card != nil {
		t.Fatalf("interact must never carry a card: %+v", f.calls)
	}
	if !strings.Contains(rec.Targets[0].FieldsJSON, `"link_card":null`) {
		t.Fatalf("injected card must not persist on interact: %s", rec.Targets[0].FieldsJSON)
	}
}

func TestScheduleFreezesCardAndFiresWithIt(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeBsky{failAt: -1}
	uf := &fakeUnfurler{card: &unfurl.Card{URI: "https://x.com/a", Title: "T"}}
	d := &Dispatcher{Bluesky: f, Store: db, Unfurler: uf}
	rec, err := d.Schedule(context.Background(), PostSpec{
		MasterText: "hello https://x.com/a", Platforms: []string{"bluesky"},
	}, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if rec.Targets[0].FinalText != "hello" {
		t.Fatalf("scheduled FinalText must be stripped: %q", rec.Targets[0].FinalText)
	}
	if _, err := d.Fire(context.Background(), rec.ID); err != nil {
		t.Fatal(err)
	}
	if len(f.calls) != 1 || f.calls[0].text != "hello" || f.calls[0].card == nil {
		t.Fatalf("fire must post the stripped text with the card: %+v", f.calls)
	}
}
