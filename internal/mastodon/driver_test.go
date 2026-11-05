package mastodon

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"
	"time"

	"github.com/geofox/publisher/internal/destination"
)

func TestPublishSendsIdempotencyKeyAndReply(t *testing.T) {
	var key, replyTo, spoiler string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/api/v1/statuses" {
			_ = r.ParseForm()
			key, replyTo, spoiler = r.Header.Get("Idempotency-Key"), r.FormValue("in_reply_to_id"), r.FormValue("spoiler_text")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"222","url":"https://m.example/@a/222"}`))
	}))
	defer srv.Close()

	a := NewAccount(New(srv.URL, "token"))
	res, err := a.Publish(context.Background(), destination.PublishRequest{
		Text: "reply", ContentWarning: "cw", IdempotencyKey: "k1",
		InReplyTo: &destination.ThreadRef{Parent: destination.ItemRef{ID: "111"}},
	})
	if err != nil || res.Status != destination.StatusPublished || res.Ref.ID != "222" {
		t.Fatalf("publish: %+v %v", res, err)
	}
	if key != "k1" || replyTo != "111" || spoiler != "cw" {
		t.Fatalf("Idempotency-Key %q, in_reply_to_id %q, spoiler %q", key, replyTo, spoiler)
	}
}

func TestQuoteSendsIdempotencyKey(t *testing.T) {
	var key, quoted string
	var form url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		key, quoted, form = r.Header.Get("Idempotency-Key"), r.FormValue("quoted_status_id"), r.Form
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"9","url":"https://m.example/@a/9"}`))
	}))
	defer srv.Close()
	a := NewAccount(New(srv.URL, "token"))
	if _, err := a.Publish(context.Background(), destination.PublishRequest{Text: "q", IdempotencyKey: "k2",
		ContentWarning: "spoilers", Sensitive: true, Visibility: "unlisted", Languages: []string{"fr"},
		Quote: &destination.SourceRef{LocalID: "42"}}); err != nil {
		t.Fatal(err)
	}
	if key != "k2" || quoted != "42" {
		t.Fatalf("Idempotency-Key %q, quoted_status_id %q", key, quoted)
	}
	// A quote keeps the post's content warning, sensitivity, visibility and
	// language like any other status.
	if form.Get("spoiler_text") != "spoilers" || form.Get("sensitive") != "true" ||
		form.Get("visibility") != "unlisted" || form.Get("language") != "fr" {
		t.Fatalf("quote form = %v", form)
	}
}

func TestPublishClassifiesStatus(t *testing.T) {
	for code, want := range map[int]destination.ErrorClass{
		http.StatusUnauthorized: destination.ClassAuth, http.StatusUnprocessableEntity: destination.ClassInvalid,
		http.StatusInternalServerError: destination.ClassTransient,
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(code)
			_, _ = w.Write([]byte(`{"error":"nope"}`))
		}))
		_, err := NewAccount(New(srv.URL, "token")).Publish(context.Background(), destination.PublishRequest{Text: "x"})
		srv.Close()
		if got := destination.ClassOf(err); got != want {
			t.Errorf("HTTP %d: class %q, want %q (%v)", code, got, want, err)
		}
	}
}

func TestCapabilitiesUseInstanceLimits(t *testing.T) {
	var fetches atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v2/instance" {
			http.NotFound(w, r)
			return
		}
		fetches.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"configuration":{
			"statuses":{"max_characters":5000,"max_media_attachments":8,"characters_reserved_per_url":23},
			"media_attachments":{"image_size_limit":16777216,"image_matrix_limit":33177600,"video_size_limit":41943040,"description_limit":1500}}}`))
	}))
	defer srv.Close()
	a := NewAccount(New(srv.URL, "token"))
	c := a.Capabilities(context.Background())
	if c.Text.MaxLength != 5000 || c.Media.Images.MaxCount != 8 || c.Media.Images.MaxBytes != 16777216 ||
		c.Media.Images.MaxPixels != 33177600 || c.Media.Video.MaxBytes != 41943040 {
		t.Fatalf("instance limits not applied: %+v", c)
	}
	a.Capabilities(context.Background())
	if n := fetches.Load(); n != 1 {
		t.Fatalf("limits must be cached, fetched %d times", n)
	}
}

func TestCapabilitiesFallBackToDefaults(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	c := NewAccount(New(srv.URL, "token")).Capabilities(context.Background())
	if c.Text.MaxLength != 500 || c.Media.Images.MaxCount != 4 {
		t.Fatalf("a failed fetch must return the manifest defaults: %+v", c)
	}
}

// A caller that gives up while the limits fetch runs doesn't cancel it or
// mark it failed: the fetch finishes on its own budget, concurrent callers
// share it, and the next caller gets the instance's limits.
func TestCapabilitiesFetchSurvivesCallerCancel(t *testing.T) {
	var fetches atomic.Int32
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fetches.Add(1)
		<-release
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"configuration":{"statuses":{"max_characters":5000}}}`))
	}))
	defer srv.Close()
	a := NewAccount(New(srv.URL, "token"))

	ctx, cancel := context.WithCancel(context.Background())
	got := make(chan destination.Capabilities, 2)
	go func() { got <- a.Capabilities(ctx) }()
	go func() { got <- a.Capabilities(ctx) }()
	for fetches.Load() == 0 {
		time.Sleep(time.Millisecond)
	}
	cancel() // both callers leave before the instance answers
	for i := 0; i < 2; i++ {
		if c := <-got; c.Text.MaxLength != 500 {
			t.Fatalf("a caller that left early gets the defaults, got %d", c.Text.MaxLength)
		}
	}
	close(release)
	deadline := time.Now().Add(2 * time.Second)
	for {
		if c := a.Capabilities(context.Background()); c.Text.MaxLength == 5000 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the fetch a cancelled caller started never filled the cache")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if n := fetches.Load(); n != 1 {
		t.Fatalf("concurrent callers must share one fetch, got %d", n)
	}
}

// A timeout on a media upload posted nothing and is safe to retry; a timeout
// on the status create may have published (the Idempotency-Key covers it).
func TestPublishTimeoutClassByStep(t *testing.T) {
	hang := func(_ http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		<-r.Context().Done()
	}
	cases := []struct {
		name          string
		media, status http.HandlerFunc
		want          destination.ErrorClass
	}{
		{"media upload", hang, nil, destination.ClassTransient},
		{"status create", nil, hang, destination.ClassUnknownOutcome},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mux := http.NewServeMux()
			mux.HandleFunc("/api/v2/media", func(w http.ResponseWriter, r *http.Request) {
				if tc.media != nil {
					tc.media(w, r)
					return
				}
				_, _ = w.Write([]byte(`{"id":"m1"}`))
			})
			mux.HandleFunc("/api/v1/statuses", func(w http.ResponseWriter, r *http.Request) {
				if tc.status != nil {
					tc.status(w, r)
					return
				}
				_, _ = w.Write([]byte(`{"id":"1"}`))
			})
			srv := httptest.NewServer(mux)
			t.Cleanup(srv.Close)
			c := New(srv.URL, "token")
			c.c.Timeout = 50 * time.Millisecond
			_, err := NewAccount(c).Publish(context.Background(), destination.PublishRequest{Text: "hi",
				Media: []destination.MediaItem{{Mime: "image/png", Bytes: []byte("png")}}})
			if got := destination.ClassOf(err); got != tc.want {
				t.Fatalf("class = %q, want %q (err %v)", got, tc.want, err)
			}
		})
	}
}
