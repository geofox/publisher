package dispatch

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/geofox/publisher/internal/threads"
)

// TestPostThreadsWiresVideoContainer pins the adapter→client seam: the video
// Img becomes the VIDEO container (URL + alt) and never enters the image list.
func TestPostThreadsWiresVideoContainer(t *testing.T) {
	var createQuery url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/threads_publish"):
			w.Write([]byte(`{"id":"m1"}`))
		case strings.HasSuffix(r.URL.Path, "/threads"):
			createQuery = r.URL.Query()
			w.Write([]byte(`{"id":"c1"}`))
		default: // status poll + permalink
			w.Write([]byte(`{"status":"FINISHED","permalink":"https://t/p"}`))
		}
	}))
	defer srv.Close()
	tc := threads.New("tok", "me")
	tc.BaseURL = srv.URL
	tc.PollInterval = time.Millisecond
	a := ThreadsAdapter{C: tc}
	imgs := []Img{{Mime: "video/mp4", BlossomURL: "https://b/v", Alt: "clip", DurationSecs: 30, SizeBytes: 1000}}
	res, err := a.PostThreads(context.Background(), "hi", Overrides{}, imgs, nil)
	if err != nil {
		t.Fatalf("%v (%+v)", err, res)
	}
	if createQuery.Get("media_type") != "VIDEO" || createQuery.Get("video_url") != "https://b/v" {
		t.Fatalf("create query = %v", createQuery)
	}
	if createQuery.Get("alt_text") != "clip" {
		t.Fatalf("alt_text missing: %v", createQuery)
	}
	if createQuery.Get("image_url") != "" {
		t.Fatal("video must not set image_url")
	}
}
