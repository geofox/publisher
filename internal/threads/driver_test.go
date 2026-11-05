package threads

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/geofox/publisher/internal/destination"
)

// fakeGraph answers container creates, status polls and publishes, recording
// each container-create query.
func fakeGraph(t *testing.T) (*Client, *[]url.Values) {
	t.Helper()
	var mu sync.Mutex
	var creates []url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		switch {
		case strings.Contains(r.URL.Path, "/threads_publish"):
			_, _ = w.Write([]byte(`{"id":"m1"}`))
		case strings.HasSuffix(r.URL.Path, "/threads"):
			mu.Lock()
			creates = append(creates, r.Form)
			mu.Unlock()
			_, _ = w.Write([]byte(`{"id":"c1"}`))
		default: // status poll + permalink
			_, _ = w.Write([]byte(`{"status":"FINISHED","permalink":"https://t/p"}`))
		}
	}))
	t.Cleanup(srv.Close)
	c := New("tok", "me")
	c.BaseURL = srv.URL
	c.PollInterval = time.Millisecond
	return c, &creates
}

// A video becomes the VIDEO container (URL + alt) and never an image.
func TestPublishVideoContainer(t *testing.T) {
	c, creates := fakeGraph(t)
	a := &Account{C: c}
	_, err := a.Publish(context.Background(), destination.PublishRequest{Text: "hi",
		Media: []destination.MediaItem{{Mime: "video/mp4", URL: "https://b/v", Alt: "clip"}}})
	if err != nil {
		t.Fatal(err)
	}
	q := (*creates)[0]
	if q.Get("media_type") != "VIDEO" || q.Get("video_url") != "https://b/v" || q.Get("alt_text") != "clip" || q.Get("image_url") != "" {
		t.Fatalf("create query = %v", q)
	}
}

func TestPublishReplyAndOptions(t *testing.T) {
	c, creates := fakeGraph(t)
	a := &Account{C: c}
	_, err := a.Publish(context.Background(), destination.PublishRequest{Text: "two",
		InReplyTo: &destination.ThreadRef{Parent: destination.ItemRef{ID: "p1"}},
		Options:   map[string]any{OptTopicTag: "golang", OptReplyControl: "mentioned_only"}})
	if err != nil {
		t.Fatal(err)
	}
	q := (*creates)[0]
	if q.Get("reply_to_id") != "p1" || q.Get("topic_tag") != "golang" || q.Get("reply_control") != "mentioned_only" {
		t.Fatalf("create query = %v", q)
	}
}
