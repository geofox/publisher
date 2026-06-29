package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"mime/multipart"
	"path/filepath"
	"testing"

	"github.com/geofox/publisher/internal/media"
	"github.com/geofox/publisher/internal/store"
)

func newDraftAPI(t *testing.T) *API {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "drafts.db")
	s, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	mp := &media.Pipeline{Lookup: s.LookupMediaURL}
	return &API{Store: s, media: mp}
}

func postMultipart(t *testing.T, a *API, path string, spec map[string]any, files map[string][]byte) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	b, _ := json.Marshal(spec)
	_ = mw.WriteField("spec", string(b))
	for name, body := range files {
		fw, _ := mw.CreateFormFile(name, name+".bin")
		_, _ = fw.Write(body)
	}
	mw.Close()
	req := httptest.NewRequest(http.MethodPost, path, &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	rec := httptest.NewRecorder()
	a.Routes().ServeHTTP(rec, req)
	return rec
}

func TestGetDraftHandler(t *testing.T) {
	a := newDraftAPI(t)
	rec := postMultipart(t, a, "/api/drafts", map[string]any{
		"master_text": "x", "platforms": []string{"nostr"},
		"overrides": map[string]any{}, "images": []any{},
	}, nil)
	if rec.Code != 200 {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	var created store.Draft
	_ = json.Unmarshal(rec.Body.Bytes(), &created)

	req := httptest.NewRequest(http.MethodGet, "/api/drafts/"+created.ID, nil)
	rec2 := httptest.NewRecorder()
	a.Routes().ServeHTTP(rec2, req)
	if rec2.Code != 200 {
		t.Fatalf("get: %d %s", rec2.Code, rec2.Body.String())
	}
	var got store.Draft
	_ = json.Unmarshal(rec2.Body.Bytes(), &got)
	if got.ID != created.ID {
		t.Errorf("got=%q want=%q", got.ID, created.ID)
	}
}

func TestUpdateDraftHandler(t *testing.T) {
	a := newDraftAPI(t)
	rec := postMultipart(t, a, "/api/drafts", map[string]any{
		"master_text": "v1", "platforms": []string{"nostr"},
		"overrides": map[string]any{}, "images": []any{},
	}, nil)
	var created store.Draft
	_ = json.Unmarshal(rec.Body.Bytes(), &created)

	// PUT requires a different verb than postMultipart provides, so build inline:
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	b, _ := json.Marshal(map[string]any{
		"master_text": "v2", "platforms": []string{"nostr"},
		"overrides": map[string]any{}, "images": []any{},
	})
	_ = mw.WriteField("spec", string(b))
	mw.Close()
	req := httptest.NewRequest(http.MethodPut, "/api/drafts/"+created.ID, &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	rec3 := httptest.NewRecorder()
	a.Routes().ServeHTTP(rec3, req)
	if rec3.Code != 200 {
		t.Fatalf("put: %d %s", rec3.Code, rec3.Body.String())
	}

	got, _ := a.Store.GetDraft(created.ID)
	if got.MasterText != "v2" {
		t.Errorf("master not updated: %q", got.MasterText)
	}
}

func TestDeleteDraftHandler(t *testing.T) {
	a := newDraftAPI(t)
	rec := postMultipart(t, a, "/api/drafts", map[string]any{
		"master_text": "bye", "platforms": []string{"nostr"},
		"overrides": map[string]any{}, "images": []any{},
	}, nil)
	var created store.Draft
	_ = json.Unmarshal(rec.Body.Bytes(), &created)

	req := httptest.NewRequest(http.MethodDelete, "/api/drafts/"+created.ID, nil)
	rec2 := httptest.NewRecorder()
	a.Routes().ServeHTTP(rec2, req)
	if rec2.Code != http.StatusNoContent {
		t.Fatalf("delete: %d %s", rec2.Code, rec2.Body.String())
	}

	if _, err := a.Store.GetDraft(created.ID); err == nil {
		t.Error("draft still exists after delete")
	}
}

func TestCreateAndListDraft(t *testing.T) {
	a := newDraftAPI(t)
	spec := map[string]any{
		"master_text": "hello drafts",
		"platforms":   []string{"nostr"},
		"tags":        []string{"essay", "test"},
		"overrides":   map[string]any{},
		"images":      []any{}, // no media in this test
	}
	rec := postMultipart(t, a, "/api/drafts", spec, nil)
	if rec.Code != 200 {
		t.Fatalf("create: code=%d body=%s", rec.Code, rec.Body.String())
	}
	var got store.Draft
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode created draft: %v", err)
	}
	if got.ID == "" || got.MasterText != "hello drafts" {
		t.Errorf("bad created draft: %+v", got)
	}

	// list
	req := httptest.NewRequest(http.MethodGet, "/api/drafts", nil)
	rec2 := httptest.NewRecorder()
	a.Routes().ServeHTTP(rec2, req)
	if rec2.Code != 200 {
		t.Fatalf("list: code=%d body=%s", rec2.Code, rec2.Body.String())
	}
	var items []store.DraftListItem
	if err := json.Unmarshal(rec2.Body.Bytes(), &items); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if len(items) != 1 || items[0].ID != got.ID {
		t.Errorf("list mismatch: %+v", items)
	}
}

func TestTranslateDraftHandler(t *testing.T) {
	a := newDraftAPI(t)
	// fakeTranslator is declared in api_test.go; use it here with canned output.
	a.Translator = &fakeTranslator{out: "[de] hello", src: "en"}
	rec := postMultipart(t, a, "/api/drafts", map[string]any{
		"master_text": "hello", "platforms": []string{"nostr"},
		"tags": []string{"essay"},
		"overrides": map[string]any{}, "images": []any{},
	}, nil)
	var created store.Draft
	_ = json.Unmarshal(rec.Body.Bytes(), &created)

	body := bytes.NewBufferString(`{"target":"de"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/drafts/"+created.ID+"/translate", body)
	req.Header.Set("Content-Type", "application/json")
	rec2 := httptest.NewRecorder()
	a.Routes().ServeHTTP(rec2, req)
	if rec2.Code != 200 {
		t.Fatalf("translate: %d %s", rec2.Code, rec2.Body.String())
	}
	var got store.Draft
	_ = json.Unmarshal(rec2.Body.Bytes(), &got)
	if got.ID == created.ID {
		t.Errorf("new draft should have different id")
	}
	if got.MasterText != "[de] hello" {
		t.Errorf("master not translated: %q", got.MasterText)
	}
	if len(got.Tags) != 1 || got.Tags[0] != "essay" {
		t.Errorf("tags not copied: %v", got.Tags)
	}
}

func TestDraftRoundTripsAnchorsAndClientID(t *testing.T) {
	a := newDraftAPI(t)
	spec := map[string]any{
		"master_text": "a\n---\nb",
		"platforms":   []string{"bluesky"},
		"overrides":   map[string]any{},
		"tags":        []string{},
		"anchors":     map[string]any{"img-x": 1},
		"images": []any{
			map[string]any{
				"id":          "img-x",
				"blossom_url": "https://b/1",
				"sha256":      "s",
			},
		},
	}
	rec := postMultipart(t, a, "/api/drafts", spec, nil)
	if rec.Code != 200 {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	var created store.Draft
	_ = json.Unmarshal(rec.Body.Bytes(), &created)

	// load it back
	req := httptest.NewRequest(http.MethodGet, "/api/drafts/"+created.ID, nil)
	rec2 := httptest.NewRecorder()
	a.Routes().ServeHTTP(rec2, req)
	if rec2.Code != 200 {
		t.Fatalf("get: %d %s", rec2.Code, rec2.Body.String())
	}
	var got store.Draft
	_ = json.Unmarshal(rec2.Body.Bytes(), &got)

	// assert client_id on the media row
	if len(got.Media) == 0 {
		t.Fatalf("no media rows")
	}
	if got.Media[0].ClientID != "img-x" {
		t.Fatalf("client_id=%q, want img-x", got.Media[0].ClientID)
	}

	// assert anchors in spec_json
	var stored struct {
		Anchors map[string]int `json:"anchors"`
	}
	if err := json.Unmarshal([]byte(got.Spec), &stored); err != nil {
		t.Fatalf("decode spec: %v", err)
	}
	if stored.Anchors["img-x"] != 1 {
		t.Fatalf("anchors=%v, want img-x→1", stored.Anchors)
	}
}

func TestTranslateDraftPreservesAnchors(t *testing.T) {
	a := newDraftAPI(t)
	a.Translator = &fakeTranslator{out: "[de] hello", src: "en"}

	spec := map[string]any{
		"master_text": "hello",
		"platforms":   []string{"bluesky"},
		"overrides":   map[string]any{},
		"tags":        []string{"essay"},
		"anchors":     map[string]any{"img-x": 2},
		"images": []any{
			map[string]any{
				"id":          "img-x",
				"blossom_url": "https://b/1",
				"sha256":      "s",
			},
		},
	}
	rec := postMultipart(t, a, "/api/drafts", spec, nil)
	if rec.Code != 200 {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	var created store.Draft
	_ = json.Unmarshal(rec.Body.Bytes(), &created)

	// call translate
	body := bytes.NewBufferString(`{"target":"de"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/drafts/"+created.ID+"/translate", body)
	req.Header.Set("Content-Type", "application/json")
	rec2 := httptest.NewRecorder()
	a.Routes().ServeHTTP(rec2, req)
	if rec2.Code != 200 {
		t.Fatalf("translate: %d %s", rec2.Code, rec2.Body.String())
	}
	var got store.Draft
	_ = json.Unmarshal(rec2.Body.Bytes(), &got)

	// assert the new draft's spec_json still carries anchors
	var stored struct {
		Anchors map[string]int `json:"anchors"`
	}
	if err := json.Unmarshal([]byte(got.Spec), &stored); err != nil {
		t.Fatalf("decode spec: %v", err)
	}
	if stored.Anchors["img-x"] != 2 {
		t.Fatalf("anchors=%v, want img-x→2 in translated draft", stored.Anchors)
	}
}
