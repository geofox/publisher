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
