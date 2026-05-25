package mastodon

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestPostSendsInReplyToID(t *testing.T) {
	var gotReplyTo string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		gotReplyTo = r.FormValue("in_reply_to_id")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"222","url":"https://m.example/@a/222"}`))
	}))
	defer srv.Close()

	cl := New(srv.URL, "token")
	res, err := cl.Post(context.Background(), Post{Text: "reply body", InReplyToID: "111"})
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	if gotReplyTo != "111" {
		t.Errorf("in_reply_to_id = %q, want 111", gotReplyTo)
	}
	if res.RemoteID != "222" {
		t.Errorf("remote id = %q", res.RemoteID)
	}
}
