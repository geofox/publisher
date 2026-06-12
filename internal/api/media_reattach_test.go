package api

import (
	"bytes"
	"context"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
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

// TestAPIPostVideoReferenceOverFetchCapSkipsFetch guards that a video reference
// whose size_bytes exceeds media.FetchCap is forwarded to dispatch as
// metadata-only (Bytes nil) and that the fake-Blossom server is never called.
func TestAPIPostVideoReferenceOverFetchCapSkipsFetch(t *testing.T) {
	uploadCalled := false
	blossom := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		uploadCalled = true
		w.WriteHeader(http.StatusInternalServerError) // must never be reached
	}))
	defer blossom.Close()

	sk := nostr.Generate()
	cap := &capturingSpecDispatcher{}
	fetchCalled := false
	a := &API{
		Dispatch: cap,
		media:    media.New(blossom.URL, sk, nostr.GetPublicKey(sk)),
		fetchMedia: func(_ context.Context, rawURL string) ([]byte, string, error) {
			fetchCalled = true
			return nil, "", nil
		},
	}

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	spec, _ := json.Marshal(map[string]any{
		"master_text": "hi", "platforms": []string{"nostr"},
		"images": []map[string]any{{
			"ordinal":       0,
			"blossom_url":   "https://blossom.example/big.mp4",
			"sha256":        "deadbeef",
			"mime":          "video/mp4",
			"size_bytes":    media.FetchCap + 1,
			"duration_secs": 120,
			"dim":           "1920x1080",
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
	if fetchCalled {
		t.Error("fetchMedia must NOT be called for video over FetchCap")
	}
	if uploadCalled {
		t.Error("blossom must NOT be called for video reference (no re-processing)")
	}
	if len(cap.spec.Images) != 1 {
		t.Fatalf("expected 1 image forwarded to dispatch, got %d", len(cap.spec.Images))
	}
	img := cap.spec.Images[0]
	if img.Bytes != nil {
		t.Error("Bytes must be nil for over-cap video reference")
	}
	if img.Mime != "video/mp4" {
		t.Errorf("Mime = %q, want video/mp4", img.Mime)
	}
	if img.SizeBytes != media.FetchCap+1 {
		t.Errorf("SizeBytes = %d, want %d", img.SizeBytes, media.FetchCap+1)
	}
	if img.DurationSecs != 120 {
		t.Errorf("DurationSecs = %d, want 120", img.DurationSecs)
	}
}

// TestAPIPostFreshVideoRejected guards that uploading a video file directly
// through the image multipart field returns 400 and mentions the async pipeline.
func TestAPIPostFreshVideoRejected(t *testing.T) {
	a := &API{Dispatch: fakeDispatcher{}}

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	spec, _ := json.Marshal(map[string]any{
		"master_text": "hi", "platforms": []string{"nostr"},
	})
	_ = mw.WriteField("spec", string(spec))
	fw, _ := mw.CreateFormFile("image", "clip.mp4")
	fw.Write([]byte("fake-video-bytes"))
	mw.Close()

	req := httptest.NewRequest(http.MethodPost, "/api/post", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	// Set Content-Type for the part itself so the handler's header-check fires.
	// The multipart writer already set the part header; we supplement via a
	// custom reader that wraps the part with the right Content-Type — but the
	// simplest approach is to rely on MIME sniff via a real MP4 magic number.
	// Instead, send with explicit Content-Type on the file part by using a raw
	// multipart write.
	rec := httptest.NewRecorder()

	// Re-build with an explicit video/mp4 Content-Type on the file part.
	var buf2 bytes.Buffer
	mw2 := multipart.NewWriter(&buf2)
	spec2, _ := json.Marshal(map[string]any{
		"master_text": "hi", "platforms": []string{"nostr"},
	})
	_ = mw2.WriteField("spec", string(spec2))
	h := make(map[string][]string)
	h["Content-Disposition"] = []string{`form-data; name="image"; filename="clip.mp4"`}
	h["Content-Type"] = []string{"video/mp4"}
	pw, _ := mw2.CreatePart(h)
	pw.Write([]byte("fake-video-bytes"))
	mw2.Close()

	req2 := httptest.NewRequest(http.MethodPost, "/api/post", &buf2)
	req2.Header.Set("Content-Type", mw2.FormDataContentType())
	rec2 := httptest.NewRecorder()
	a.Routes().ServeHTTP(rec2, req2)

	if rec2.Code != http.StatusBadRequest {
		t.Fatalf("code=%d body=%s, want 400 for video upload via image field", rec2.Code, rec2.Body.String())
	}
	if !strings.Contains(rec2.Body.String(), "/api/media/video") {
		t.Errorf("error body %q must mention /api/media/video", rec2.Body.String())
	}
	_ = rec
}
