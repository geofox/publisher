package dispatch

import (
	"context"
	"strings"
	"testing"

	"github.com/geofox/publisher/internal/bluesky"
	"github.com/geofox/publisher/internal/destination"
	"github.com/geofox/publisher/internal/nostr"
	"github.com/geofox/publisher/internal/threads"
	"github.com/geofox/publisher/internal/transcode"
)

func TestPlanMediaFit(t *testing.T) {
	metas := []transcode.Meta{
		{SizeBytes: 500, Mime: "image/jpeg"},     // fits everywhere
		{SizeBytes: 3 << 20, Mime: "image/jpeg"}, // over bluesky's ~2 MB
		{SizeBytes: 500, Mime: "image/webp"},     // threads format violation
	}
	plan := func(d destination.Driver) []FitNote {
		name, mc := capsOf(d)
		return PlanMediaFit(name, mc, metas)
	}
	bs := plan(bluesky.Driver{})
	if len(bs) != 1 || bs[0].Ordinal != 1 || !strings.Contains(bs[0].Note, "JPEG") {
		t.Fatalf("bluesky notes = %+v, want one note for ordinal 1", bs)
	}
	th := plan(threads.Driver{})
	if len(th) != 1 || th[0].Ordinal != 2 || !strings.Contains(th[0].Note, "webp") {
		t.Fatalf("threads notes = %+v, want one note for ordinal 2", th)
	}
	if n := plan(nostr.Driver{}); n != nil {
		t.Fatalf("nostr must plan no fits, got %+v", n)
	}
}

// Parity: anything the planner flags, the executor changes; anything it
// passes, the executor leaves alone. Same Profile predicate on both sides —
// this test pins that the wiring keeps it that way.
func TestPlanMediaFitParityWithExecutors(t *testing.T) {
	png := tinyPNG(t)
	heic := readHEICFixture(t)
	imgs := []Img{
		{Bytes: png, Mime: "image/png", BlossomURL: "https://b/png"},
		{Bytes: heic, Mime: "image/heic", BlossomURL: "https://b/heic"}, // threads format violation
	}
	metas := []transcode.Meta{
		{SizeBytes: int64(len(png)), Mime: "image/png"},
		{SizeBytes: int64(len(heic)), Mime: "image/heic"},
	}
	name, mc := capsOf(threads.Driver{})
	notes := PlanMediaFit(name, mc, metas)
	planned := map[int]bool{}
	for _, n := range notes {
		planned[n.Ordinal] = true
	}
	items, err := prepareMedia(context.Background(), name, mc, imgs,
		func(context.Context, []byte, string) (string, error) { return "https://b/v", nil })
	if err != nil {
		t.Fatal(err)
	}
	for i := range imgs {
		hosted := items[i].URL == "https://b/v"
		if hosted != planned[i] {
			t.Fatalf("image %d: planned=%v but executor hosted=%v", i, planned[i], hosted)
		}
	}
}
