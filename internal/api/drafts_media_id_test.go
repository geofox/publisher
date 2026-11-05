package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/geofox/publisher/internal/store"
)

// A draft's attachments come back with the id of a media object for the
// same file, so the composer can post them through /v1. One object per
// file: reading the draft again finds it. References that don't look like
// media storage get none.
func TestDraftMediaIDs(t *testing.T) {
	a := newDraftAPI(t)
	sha := strings.Repeat("ab", 32)
	ref := func(url, sha string) map[string]any {
		return map[string]any{"ordinal": 0, "blossom_url": url, "sha256": sha, "mime": "image/png", "dim": "8x4",
			"size_bytes": 99, "alt": "a dot", "id": "c1"}
	}
	rec := postMultipart(t, a, "/api/drafts", map[string]any{
		"master_text": "x", "platforms": []string{"bluesky"}, "overrides": map[string]any{},
		"images": []any{
			ref("https://blossom.test/"+sha+".png", sha),
			ref("https://elsewhere.test/cat.png", sha),        // not named by its hash
			ref("javascript:alert(1)//"+sha, sha),             // not http(s)
			ref("https://blossom.test/"+sha+".png", "nothex"), // no hash
		},
	}, nil)
	if rec.Code != 200 {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	var created struct {
		ID    string          `json:"id"`
		Media []draftMediaOut `json:"media"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if len(created.Media) != 4 || !strings.HasPrefix(created.Media[0].MediaID, "med_") {
		t.Fatalf("media = %+v", created.Media)
	}
	for i, m := range created.Media[1:] {
		if m.MediaID != "" {
			t.Errorf("media %d: id %q for %q", i+1, m.MediaID, m.BlossomURL)
		}
	}
	obj, err := a.Store.MediaObject(context.Background(), store.DefaultOwner, created.Media[0].MediaID)
	if err != nil {
		t.Fatal(err)
	}
	if obj.Status != store.MediaReady || obj.SHA256 != sha || obj.Mime != "image/png" || obj.Dim != "8x4" || obj.SizeBytes != 99 {
		t.Fatalf("object = %+v", obj)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/drafts/"+created.ID, nil)
	rec = httptest.NewRecorder()
	a.Routes().ServeHTTP(rec, req)
	var got struct {
		Media []draftMediaOut `json:"media"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if len(got.Media) != 4 || got.Media[0].MediaID != created.Media[0].MediaID {
		t.Fatalf("read again: %+v, want id %s", got.Media, created.Media[0].MediaID)
	}
}
