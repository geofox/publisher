package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
