package api

import (
	"bytes"
	"context"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"fiatjaf.com/nostr"

	"github.com/geofox/publisher/internal/dispatch"
	"github.com/geofox/publisher/internal/media"
	"github.com/geofox/publisher/internal/store"
)

// capturingSpecDispatcher records the whole PostSpec so tests can assert on the
// images the handler forwarded to dispatch.
type capturingSpecDispatcher struct{ spec dispatch.PostSpec }

func (c *capturingSpecDispatcher) Post(_ context.Context, spec dispatch.PostSpec) *store.Post {
	c.spec = spec
	return &store.Post{ID: "p1", Status: "success", Platforms: spec.Platforms}
}
func (c *capturingSpecDispatcher) Retry(context.Context, string, []string) (*store.Post, error) {
	return nil, nil
}
func (c *capturingSpecDispatcher) RetryRelay(context.Context, string, string) (*store.Post, error) {
	return nil, nil
}
func (c *capturingSpecDispatcher) Schedule(context.Context, dispatch.PostSpec, time.Time) (*store.Post, error) {
	return nil, nil
}
func (c *capturingSpecDispatcher) Interact(context.Context, dispatch.InteractSpec) *store.Post {
	return nil
}
func (c *capturingSpecDispatcher) PostWithID(_ context.Context, id string, spec dispatch.PostSpec) *store.Post {
	c.spec = spec
	return &store.Post{ID: id, Status: "success", Platforms: spec.Platforms}
}
func (c *capturingSpecDispatcher) InteractWithID(_ context.Context, id string, _ dispatch.InteractSpec) *store.Post {
	return &store.Post{ID: id, Status: "success"}
}

// TestAPIPostReattachesUploadedImageReferences guards the restored-draft bug:
// when a publish carries an already-uploaded image as a Blossom reference
// (blossom_url, no fresh multipart file), the handler must re-fetch the bytes
// and forward the image to dispatch — otherwise media is silently dropped on
// every platform (Nostr included).
func TestAPIPostReattachesUploadedImageReferences(t *testing.T) {
	blossom := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte(`{"url":"https://blossom.example/reup"}`))
	}))
	defer blossom.Close()

	sk := nostr.Generate()
	cap := &capturingSpecDispatcher{}
	a := &API{
		Dispatch: cap,
		media:    media.New(blossom.URL, sk, nostr.GetPublicKey(sk)),
		fetchMedia: func(_ context.Context, rawURL string) ([]byte, string, error) {
			if rawURL != "https://blossom.example/orig.png" {
				t.Errorf("unexpected fetch url %q", rawURL)
			}
			return []byte("imgbytes"), "image/png", nil
		},
	}

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	spec, _ := json.Marshal(map[string]any{
		"master_text": "hi", "platforms": []string{"bluesky"},
		"images": []map[string]any{{
			"ordinal": 0, "blossom_url": "https://blossom.example/orig.png",
			"sha256": "abc", "mime": "image/png", "alt": "a cat",
		}},
	})
	_ = mw.WriteField("spec", string(spec))
	mw.Close()

	req := httptest.NewRequest(http.MethodPost, "/api/post", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	rec := httptest.NewRecorder()
	a.Routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	if len(cap.spec.Images) != 1 {
		t.Fatalf("expected 1 image attached, got %d", len(cap.spec.Images))
	}
	if string(cap.spec.Images[0].Bytes) != "imgbytes" {
		t.Errorf("image bytes not re-fetched, got %q", cap.spec.Images[0].Bytes)
	}
	if cap.spec.Images[0].Alt != "a cat" {
		t.Errorf("alt lost, got %q", cap.spec.Images[0].Alt)
	}
	if len(cap.spec.MediaRecords) != 1 || cap.spec.MediaRecords[0].BlossomURL == "" {
		t.Errorf("media record not built: %+v", cap.spec.MediaRecords)
	}
}
