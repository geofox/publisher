package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/geofox/publisher/internal/unfurl"
)

type fakeUnfurl struct{ card *unfurl.Card }

func (f *fakeUnfurl) Unfurl(_ context.Context, _ string) (*unfurl.Card, error) {
	if f.card == nil {
		return nil, context.DeadlineExceeded
	}
	return f.card, nil
}

func postPreviewAPI(t *testing.T, a *API, body string) map[string]any {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/thread-preview", bytes.NewBufferString(body))
	rec := httptest.NewRecorder()
	a.handleThreadPreview(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestThreadPreviewBlueskyCard(t *testing.T) {
	a := &API{Unfurl: &fakeUnfurl{card: &unfurl.Card{
		URI: "https://x.com/a", Title: "T", Description: "D", ThumbURL: "https://x.com/t.jpg",
	}}}
	out := postPreviewAPI(t, a, `{"text":"hello https://x.com/a","platforms":["bluesky"],"number":true,"images":0}`)
	pv := out["previews"].([]any)[0].(map[string]any)
	if pv["segments"].([]any)[0].(string) != "hello" {
		t.Fatalf("preview must show the stripped text: %v", pv["segments"])
	}
	card := pv["card"].(map[string]any)
	if card["title"] != "T" || card["segment"].(float64) != 0 || card["thumb_url"] != "https://x.com/t.jpg" {
		t.Fatalf("card: %+v", card)
	}
}

func TestThreadPreviewCardRevertsWithImages(t *testing.T) {
	a := &API{Unfurl: &fakeUnfurl{card: &unfurl.Card{URI: "https://x.com/a", Title: "T"}}}
	out := postPreviewAPI(t, a, `{"text":"hello https://x.com/a","platforms":["bluesky"],"number":true,"images":2}`)
	pv := out["previews"].([]any)[0].(map[string]any)
	if _, ok := pv["card"]; ok {
		t.Fatal("images own the embed slot — preview must show no card")
	}
	if pv["segments"].([]any)[0].(string) != "hello https://x.com/a" {
		t.Fatalf("revert must keep the URL: %v", pv["segments"])
	}
}

func TestThreadPreviewNoUnfurlerUnchanged(t *testing.T) {
	a := &API{} // Unfurl nil — behaves exactly as before this feature
	out := postPreviewAPI(t, a, `{"text":"hello https://x.com/a","platforms":["bluesky"],"number":true,"images":0}`)
	pv := out["previews"].([]any)[0].(map[string]any)
	if _, ok := pv["card"]; ok {
		t.Fatal("nil unfurler must yield no card")
	}
}

func TestThreadPreviewInteractionSkipsCard(t *testing.T) {
	a := &API{Unfurl: &fakeUnfurl{card: &unfurl.Card{URI: "https://x.com/a", Title: "T"}}}
	out := postPreviewAPI(t, a, `{"text":"nice https://x.com/a","platforms":["bluesky"],"number":true,"images":0,"interaction":true}`)
	pv := out["previews"].([]any)[0].(map[string]any)
	if _, ok := pv["card"]; ok {
		t.Fatal("interaction previews must not plan a card")
	}
	if pv["segments"].([]any)[0].(string) != "nice https://x.com/a" {
		t.Fatalf("interaction preview must keep the URL: %v", pv["segments"])
	}
}

func TestThreadPreviewBlueskyCardOnLastSegment(t *testing.T) {
	long := strings.Repeat("word ", 120) + "\nhttps://x.com/a"
	a := &API{Unfurl: &fakeUnfurl{card: &unfurl.Card{URI: "https://x.com/a", Title: "T"}}}
	out := postPreviewAPI(t, a, fmt.Sprintf(
		`{"text":%q,"platforms":["bluesky"],"number":true,"images":0}`, long))
	pv := out["previews"].([]any)[0].(map[string]any)
	segs := pv["segments"].([]any)
	card := pv["card"].(map[string]any)
	if len(segs) < 2 {
		t.Fatalf("expected a threaded preview, got %d segments", len(segs))
	}
	if want := float64(len(segs) - 1); card["segment"].(float64) != want {
		t.Fatalf("card.segment = %v, want %v (last segment)", card["segment"], want)
	}
}

func TestThreadPreviewVideoNotes(t *testing.T) {
	a := &API{}
	body := `{"text":"clip","platforms":["bluesky","threads","nostr"],
	  "media":[{"size_bytes":150000000,"mime":"video/mp4","dim":"1920x1080","duration_secs":400}]}`
	out := postPreviewAPI(t, a, body)

	previews := out["previews"].([]any)
	byp := map[string]map[string]any{}
	for _, raw := range previews {
		pv := raw.(map[string]any)
		byp[pv["platform"].(string)] = pv
	}

	// bluesky: exactly 2 notes — one "✗ … over 100 MB …" (hard) and one "⚠ … 3 min …" (advisory)
	bsPV := byp["bluesky"]
	bsNotes, ok := bsPV["fit_notes"].([]any)
	if !ok || len(bsNotes) != 2 {
		t.Fatalf("bluesky fit_notes = %v, want exactly 2", bsPV["fit_notes"])
	}
	hardNote := bsNotes[0].(map[string]any)["note"].(string)
	advisoryNote := bsNotes[1].(map[string]any)["note"].(string)
	if !strings.HasPrefix(hardNote, "✗") || !strings.Contains(hardNote, "over 100 MB") {
		t.Fatalf("bluesky fit_notes[0] = %q, want ✗ prefix and 'over 100 MB'", hardNote)
	}
	if !strings.HasPrefix(advisoryNote, "⚠") || !strings.Contains(advisoryNote, "3 min") {
		t.Fatalf("bluesky fit_notes[1] = %q, want ⚠ prefix and '3 min'", advisoryNote)
	}

	// threads: at least 1 note containing "over 5 min" prefixed "✗"
	thPV := byp["threads"]
	thNotes, ok := thPV["fit_notes"].([]any)
	if !ok || len(thNotes) < 1 {
		t.Fatalf("threads fit_notes = %v, want at least 1", thPV["fit_notes"])
	}
	var found5min bool
	for _, raw := range thNotes {
		n := raw.(map[string]any)["note"].(string)
		if strings.HasPrefix(n, "✗") && strings.Contains(n, "over 5 min") {
			found5min = true
		}
	}
	if !found5min {
		t.Fatalf("threads fit_notes = %v, want a ✗ note containing 'over 5 min'", thNotes)
	}

	// nostr: no fit_notes key (nil)
	nostrPV := byp["nostr"]
	if fn, exists := nostrPV["fit_notes"]; exists {
		t.Fatalf("nostr fit_notes = %v, want none", fn)
	}

	// no preview may contain a "→ JPEG" note (PlanMediaFit must keep skipping video metas)
	for _, raw := range previews {
		pv := raw.(map[string]any)
		plat := pv["platform"].(string)
		fnSlice, _ := pv["fit_notes"].([]any)
		for _, fnRaw := range fnSlice {
			n, _ := fnRaw.(map[string]any)["note"].(string)
			if strings.Contains(n, "→ JPEG") {
				t.Fatalf("platform %s: got image fit note %q for a video meta — PlanMediaFit must skip it", plat, n)
			}
		}
	}
}

func TestThreadPreviewFitNotes(t *testing.T) {
	a := &API{}
	// 3 MB JPEG: over bluesky's ~1.9 MB cap → bluesky gets 1 fit note.
	// nostr has no profile → no fit notes.
	body := `{"text":"hello","platforms":["bluesky","nostr"],"media":[{"size_bytes":3000000,"mime":"image/jpeg","dim":"4000x3000"}]}`
	out := postPreviewAPI(t, a, body)

	previews := out["previews"].([]any)
	byp := map[string]map[string]any{}
	for _, raw := range previews {
		pv := raw.(map[string]any)
		byp[pv["platform"].(string)] = pv
	}

	// bluesky: exactly 1 fit_note, ordinal 0, note contains "JPEG"
	bsPV := byp["bluesky"]
	bsNotes, ok := bsPV["fit_notes"].([]any)
	if !ok || len(bsNotes) != 1 {
		t.Fatalf("bluesky fit_notes = %v, want exactly 1", bsPV["fit_notes"])
	}
	note := bsNotes[0].(map[string]any)
	if note["ordinal"].(float64) != 0 {
		t.Fatalf("bluesky fit_note ordinal = %v, want 0", note["ordinal"])
	}
	if n, _ := note["note"].(string); !strings.Contains(n, "JPEG") {
		t.Fatalf("bluesky fit_note = %q, want it to contain JPEG", n)
	}

	// nostr: no fit_notes key (omitempty)
	nostrPV := byp["nostr"]
	if fn, exists := nostrPV["fit_notes"]; exists {
		t.Fatalf("nostr fit_notes = %v, want none (no profile)", fn)
	}

	// media[] is authoritative: plan must contain exactly 1 image segment
	bsImgs := bsPV["imgs"].([]any)
	if len(bsImgs) != 1 {
		t.Fatalf("bluesky imgs len = %v, want 1 (media[] authoritative)", bsImgs)
	}
	if got := bsImgs[0].([]any); len(got) != 1 {
		t.Fatalf("bluesky imgs[0] = %v, want 1 index (single image placed in segment 0)", got)
	}

	// only-raise rule: images:3 + media:[1 entry] must keep the 3-image plan
	// (count not shrunk) while still returning the 1 fit note for the one
	// media entry whose metadata was provided.
	t.Run("count not shrunk when media has fewer entries than images", func(t *testing.T) {
		body2 := `{"text":"hello","platforms":["bluesky"],"images":3,"media":[{"size_bytes":3000000,"mime":"image/jpeg","dim":"4000x3000"}]}`
		out2 := postPreviewAPI(t, a, body2)
		pv2 := out2["previews"].([]any)[0].(map[string]any)

		// plan must reflect 3 images (count not shrunk to 1)
		imgs2 := pv2["imgs"].([]any)
		total2 := 0
		for _, v := range imgs2 {
			total2 += len(v.([]any))
		}
		if total2 != 3 {
			t.Fatalf("bluesky imgs total = %d, want 3 (count must not be shrunk by media[] with fewer entries)", total2)
		}

		// still exactly 1 fit note (for the 1 media entry provided)
		notes2, ok2 := pv2["fit_notes"].([]any)
		if !ok2 || len(notes2) != 1 {
			t.Fatalf("bluesky fit_notes = %v, want exactly 1", pv2["fit_notes"])
		}
	})
}
