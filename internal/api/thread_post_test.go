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

	"github.com/geofox/publisher/internal/dispatch"
	"github.com/geofox/publisher/internal/store"
)

// capturingDispatcher implements the api.Dispatcher interface and records the
// Number flag from the spec.
type capturingDispatcher struct{ gotNumber bool }

func (c *capturingDispatcher) Post(_ context.Context, spec dispatch.PostSpec) *store.Post {
	c.gotNumber = spec.Number
	return &store.Post{
		ID: "p1", Status: "success", Platforms: spec.Platforms,
		Targets: []store.Target{{
			Platform: "bluesky", Status: "success",
			Segments: []store.Segment{
				{Ordinal: 0, Text: "a", Status: "success", RemoteURL: "u0"},
				{Ordinal: 1, Text: "b", Status: "success", RemoteURL: "u1"},
			},
		}},
	}
}
func (c *capturingDispatcher) Retry(context.Context, string, []string) (*store.Post, error) {
	return nil, nil
}
func (c *capturingDispatcher) RetryRelay(context.Context, string, string) (*store.Post, error) {
	return nil, nil
}
func (c *capturingDispatcher) Schedule(context.Context, dispatch.PostSpec, time.Time) (*store.Post, error) {
	return nil, nil
}
func (c *capturingDispatcher) Interact(context.Context, dispatch.InteractSpec) *store.Post {
	return nil
}
func (c *capturingDispatcher) PostWithID(_ context.Context, id string, spec dispatch.PostSpec) *store.Post {
	c.gotNumber = spec.Number
	return &store.Post{ID: id, Status: "success", Platforms: spec.Platforms}
}
func (c *capturingDispatcher) InteractWithID(_ context.Context, id string, _ dispatch.InteractSpec) *store.Post {
	return &store.Post{ID: id, Status: "success"}
}

func TestAPIPostForwardsNumberAndReturnsSegments(t *testing.T) {
	cap := &capturingDispatcher{}
	a := &API{Dispatch: cap}

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	spec, _ := json.Marshal(map[string]any{
		"master_text": "hi", "platforms": []string{"bluesky"}, "number": true,
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
	if !cap.gotNumber {
		t.Errorf("PostSpec.Number not forwarded from request")
	}
	var out struct {
		Targets []struct {
			Platform string          `json:"platform"`
			Segments []store.Segment `json:"segments"`
		} `json:"targets"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if len(out.Targets) != 1 || len(out.Targets[0].Segments) != 2 {
		t.Fatalf("segments not in response: %s", rec.Body.String())
	}
}
