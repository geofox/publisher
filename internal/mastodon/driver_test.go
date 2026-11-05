package mastodon

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

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
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		key, quoted = r.Header.Get("Idempotency-Key"), r.FormValue("quoted_status_id")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"9","url":"https://m.example/@a/9"}`))
	}))
	defer srv.Close()
	a := NewAccount(New(srv.URL, "token"))
	if _, err := a.Publish(context.Background(), destination.PublishRequest{Text: "q", IdempotencyKey: "k2",
		Quote: &destination.SourceRef{LocalID: "42"}}); err != nil {
		t.Fatal(err)
	}
	if key != "k2" || quoted != "42" {
		t.Fatalf("Idempotency-Key %q, quoted_status_id %q", key, quoted)
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
