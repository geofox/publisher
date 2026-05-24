package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/geofox/publisher/internal/dispatch"
	"github.com/geofox/publisher/internal/store"
)

type retryFakeDispatcher struct{ db *store.Store }

func (retryFakeDispatcher) Post(ctx context.Context, spec dispatch.PostSpec) *store.Post {
	return &store.Post{ID: "x"}
}
func (f retryFakeDispatcher) Retry(ctx context.Context, id string, platforms []string) (*store.Post, error) {
	return f.db.GetPost(id)
}
func (retryFakeDispatcher) RetryRelay(ctx context.Context, id, relay string) (*store.Post, error) {
	return &store.Post{ID: id, Status: "success"}, nil
}
func (retryFakeDispatcher) Schedule(ctx context.Context, spec dispatch.PostSpec, at time.Time) (*store.Post, error) {
	return &store.Post{ID: "sch", Status: "scheduled"}, nil
}

func TestAPIRetry(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	_ = db.SavePost(&store.Post{ID: "p1", Platforms: []string{"mastodon"}, Status: "failed",
		Targets: []store.Target{{Platform: "mastodon", Status: "failed"}}})

	a := &API{Store: db, Dispatch: retryFakeDispatcher{db: db}}
	rec := httptest.NewRecorder()
	a.Routes().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/posts/p1/retry", nil))
	if rec.Code != 200 {
		t.Fatalf("retry code %d body %s", rec.Code, rec.Body.String())
	}
}
