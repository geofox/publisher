package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func postPreview(t *testing.T, body string) *httptest.ResponseRecorder {
	t.Helper()
	a := &API{}
	req := httptest.NewRequest(http.MethodPost, "/api/thread-preview", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	a.Routes().ServeHTTP(rec, req)
	return rec
}

func TestThreadPreviewSplitsPerPlatform(t *testing.T) {
	long := strings.TrimSpace(strings.Repeat("word ", 120)) // ~600 chars
	body, _ := json.Marshal(map[string]any{
		"text": long, "platforms": []string{"bluesky", "nostr"}, "number": true,
	})
	rec := postPreview(t, string(body))
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	var out struct {
		Previews []struct {
			Platform string   `json:"platform"`
			Count    int      `json:"count"`
			Segments []string `json:"segments"`
		} `json:"previews"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	byp := map[string]int{}
	for _, p := range out.Previews {
		byp[p.Platform] = p.Count
	}
	if byp["bluesky"] < 2 {
		t.Errorf("bluesky should thread: count=%d", byp["bluesky"])
	}
	if byp["nostr"] != 1 {
		t.Errorf("nostr should be single: count=%d", byp["nostr"])
	}
}

func TestThreadPreviewEmptyTextIs400(t *testing.T) {
	body, _ := json.Marshal(map[string]any{"text": "  ", "platforms": []string{"bluesky"}})
	if rec := postPreview(t, string(body)); rec.Code != http.StatusBadRequest {
		t.Fatalf("empty text should be 400, got %d", rec.Code)
	}
}

func TestThreadPreviewBadJSONIs400(t *testing.T) {
	if rec := postPreview(t, "{not json"); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad json should be 400, got %d", rec.Code)
	}
}

func TestThreadPreviewMediaOverflowSplits(t *testing.T) {
	body, _ := json.Marshal(map[string]any{
		"text": "hello", "platforms": []string{"mastodon", "bluesky"}, "images": 10,
	})
	rec := postPreview(t, string(body))
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	var out struct {
		Previews []struct {
			Platform string   `json:"platform"`
			Count    int      `json:"count"`
			Segments []string `json:"segments"`
			Imgs     [][]int  `json:"imgs"`
		} `json:"previews"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	byp := map[string]struct {
		count int
		imgs  [][]int
	}{}
	for _, p := range out.Previews {
		byp[p.Platform] = struct {
			count int
			imgs  [][]int
		}{p.Count, p.Imgs}
	}
	if m := byp["mastodon"]; m.count != 3 || len(m.imgs) != 3 || len(m.imgs[0]) != 4 || len(m.imgs[1]) != 4 || len(m.imgs[2]) != 2 {
		t.Errorf("mastodon: %+v", m)
	}
	if b := byp["bluesky"]; b.count != 1 || len(b.imgs) != 1 || len(b.imgs[0]) != 10 {
		t.Errorf("bluesky: %+v", b)
	}
}
