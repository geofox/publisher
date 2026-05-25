package mastodon

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestReblog(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/statuses/99/reblog" || r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		w.Write([]byte(`{"id":"100","url":"https://x/@me/100","reblog":{"id":"99","url":"https://x/@a/99"}}`))
	}))
	defer srv.Close()
	c := New(srv.URL, "tok")
	res, err := c.Reblog(context.Background(), "99")
	if err != nil {
		t.Fatal(err)
	}
	if res.RemoteID != "100" {
		t.Errorf("reblog wrapper id wrong: %+v", res)
	}
}

func TestQuotePostSendsQuotedStatusID(t *testing.T) {
	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/statuses" || r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Write([]byte(`{"id":"200","url":"https://x/@me/200"}`))
	}))
	defer srv.Close()
	c := New(srv.URL, "tok")
	res, err := c.QuotePost(context.Background(), "my take", "99", nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.RemoteID != "200" {
		t.Errorf("quote id wrong: %+v", res)
	}
	if !strings.Contains(gotBody, "quoted_status_id=99") || !strings.Contains(gotBody, "status=my+take") {
		t.Errorf("quote form missing fields: %q", gotBody)
	}
}

func TestQuotePostSendsMediaIDs(t *testing.T) {
	var gotMediaIDs []string
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v2/media", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "media1"})
	})
	mux.HandleFunc("/api/v1/statuses", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		gotMediaIDs = r.Form["media_ids[]"]
		if r.FormValue("quoted_status_id") != "99" || r.FormValue("status") != "take" {
			t.Errorf("quote form missing fields: %v", r.Form)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "200", "url": "https://x/@me/200"})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := New(srv.URL, "tok")
	res, err := c.QuotePost(context.Background(), "take", "99", []Image{{Bytes: []byte("img"), Alt: "a cat"}})
	if err != nil {
		t.Fatal(err)
	}
	if res.RemoteID != "200" {
		t.Errorf("quote id wrong: %+v", res)
	}
	if len(gotMediaIDs) != 1 || gotMediaIDs[0] != "media1" {
		t.Errorf("media_ids[] not sent: %v", gotMediaIDs)
	}
}
