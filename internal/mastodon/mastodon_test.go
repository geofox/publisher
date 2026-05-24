package mastodon

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestPostWithMediaAndFields(t *testing.T) {
	var gotStatus map[string]any
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v2/media", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "media1"})
	})
	mux.HandleFunc("/api/v1/statuses", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		gotStatus = map[string]any{
			"status": r.FormValue("status"), "spoiler_text": r.FormValue("spoiler_text"),
			"visibility": r.FormValue("visibility"), "language": r.FormValue("language"),
			"sensitive": r.FormValue("sensitive"), "media_ids": r.Form["media_ids[]"],
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "status1", "url": "https://m/@me/1"})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := New(srv.URL, "tok")
	res, err := c.Post(context.Background(), Post{
		Text: "hello", SpoilerText: "cw", Sensitive: true, Visibility: "unlisted", Language: "fr",
		Images: []Image{{Bytes: []byte("img"), Alt: "a cat"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.RemoteURL != "https://m/@me/1" || res.RemoteID != "status1" {
		t.Errorf("bad result %+v", res)
	}
	if gotStatus["status"] != "hello" || gotStatus["visibility"] != "unlisted" || gotStatus["language"] != "fr" {
		t.Errorf("fields not sent: %v", gotStatus)
	}
	if gotStatus["spoiler_text"] != "cw" || gotStatus["sensitive"] != "true" {
		t.Errorf("cw/sensitive not sent: %v", gotStatus)
	}
}
