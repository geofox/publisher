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
