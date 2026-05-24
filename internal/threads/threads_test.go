package threads

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestPostSingleImage(t *testing.T) {
	var createQuery string
	mux := http.NewServeMux()
	mux.HandleFunc("/me/threads", func(w http.ResponseWriter, r *http.Request) {
		createQuery = r.URL.RawQuery
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "cre1"})
	})
	mux.HandleFunc("/cre1", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "FINISHED"})
	})
	mux.HandleFunc("/me/threads_publish", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "med1"})
	})
	mux.HandleFunc("/med1", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"permalink": "https://www.threads.net/@me/post/abc"})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := New("tok", "me")
	c.BaseURL = srv.URL
	c.PollInterval = time.Millisecond

	res, err := c.Post(context.Background(), Post{
		Text: "hello", TopicTag: "golang",
		Images: []Image{{URL: "https://blossom/x.jpg", Alt: "a cat"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.RemoteID != "med1" || res.RemoteURL != "https://www.threads.net/@me/post/abc" {
		t.Errorf("result = %+v", res)
	}
	for _, want := range []string{"media_type=IMAGE", "image_url=", "alt_text=", "topic_tag=golang", "text=hello"} {
		if !strings.Contains(createQuery, want) {
			t.Errorf("create query %q missing %q", createQuery, want)
		}
	}
}

func TestPostTextOnly(t *testing.T) {
	var q string
	mux := http.NewServeMux()
	mux.HandleFunc("/me/threads", func(w http.ResponseWriter, r *http.Request) {
		q = r.URL.RawQuery
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "c"})
	})
	mux.HandleFunc("/c", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "FINISHED"})
	})
	mux.HandleFunc("/me/threads_publish", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "m"})
	})
	mux.HandleFunc("/m", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"permalink": "https://t/p"})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := New("tok", "me")
	c.BaseURL = srv.URL
	c.PollInterval = time.Millisecond
	_, err := c.Post(context.Background(), Post{Text: "just text"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(q, "media_type=TEXT") || strings.Contains(q, "alt_text") {
		t.Errorf("text post query wrong: %q", q)
	}
}

func TestPostErrorStatus(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/me/threads", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "c"})
	})
	mux.HandleFunc("/c", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "ERROR", "error_message": "boom"})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := New("tok", "me")
	c.BaseURL = srv.URL
	c.PollInterval = time.Millisecond
	c.PollTimeout = time.Second
	if _, err := c.Post(context.Background(), Post{Text: "x"}); err == nil {
		t.Errorf("expected error on ERROR status")
	}
}

func TestPostCarousel(t *testing.T) {
	var createCount int
	var parentQuery string
	mux := http.NewServeMux()
	mux.HandleFunc("/me/threads", func(w http.ResponseWriter, r *http.Request) {
		createCount++
		id := fmt.Sprintf("child%d", createCount)
		if strings.Contains(r.URL.RawQuery, "media_type=CAROUSEL") {
			id = "parent"
			parentQuery = r.URL.RawQuery
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"id": id})
	})
	mux.HandleFunc("/me/threads_publish", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "med"})
	})
	// catch-all handles container status polls and the permalink fetch.
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.RawQuery, "fields=permalink") {
			_ = json.NewEncoder(w).Encode(map[string]any{"permalink": "https://t/p"})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "FINISHED"})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := New("tok", "me")
	c.BaseURL = srv.URL
	c.PollInterval = time.Millisecond
	res, err := c.Post(context.Background(), Post{
		Text: "hi", Images: []Image{{URL: "u1", Alt: "a"}, {URL: "u2", Alt: "b"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.RemoteID != "med" {
		t.Errorf("RemoteID = %q, want med", res.RemoteID)
	}
	if !strings.Contains(parentQuery, "media_type=CAROUSEL") {
		t.Errorf("parent not a carousel: %q", parentQuery)
	}
	// children are comma-joined; url.Values.Encode percent-encodes the comma as %2C
	if !strings.Contains(parentQuery, "children=child1%2Cchild2") {
		t.Errorf("parent children wrong: %q", parentQuery)
	}
}

func TestPostReplyControl(t *testing.T) {
	var q string
	mux := http.NewServeMux()
	mux.HandleFunc("/me/threads", func(w http.ResponseWriter, r *http.Request) {
		q = r.URL.RawQuery
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "c"})
	})
	mux.HandleFunc("/c", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "FINISHED"})
	})
	mux.HandleFunc("/me/threads_publish", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "m"})
	})
	mux.HandleFunc("/m", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"permalink": "https://t/p"})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := New("tok", "me")
	c.BaseURL = srv.URL
	c.PollInterval = time.Millisecond

	if _, err := c.Post(context.Background(), Post{Text: "hi", ReplyControl: "mentioned_only"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(q, "reply_control=mentioned_only") {
		t.Errorf("valid reply_control missing: %q", q)
	}

	if _, err := c.Post(context.Background(), Post{Text: "hi", ReplyControl: "garbage"}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(q, "reply_control") {
		t.Errorf("invalid reply_control should be omitted (fail open): %q", q)
	}

	if _, err := c.Post(context.Background(), Post{Text: "hi"}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(q, "reply_control") {
		t.Errorf("empty reply_control should be omitted: %q", q)
	}
}
