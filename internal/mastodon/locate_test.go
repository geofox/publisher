package mastodon

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/geofox/publisher/internal/destination"
)

func TestLocateOwnStatus(t *testing.T) {
	author, found := "7", true
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v2/search":
			if !found {
				w.Write([]byte(`{"statuses":[]}`))
				return
			}
			w.Write([]byte(`{"statuses":[{"id":"99"}]}`))
		case "/api/v1/statuses/99":
			w.Write([]byte(`{"id":"99","content":"<p>hi</p>","url":"https://m.example/@me/99","account":{"id":"` + author + `","acct":"me"}}`))
		case "/api/v1/accounts/verify_credentials":
			w.Write([]byte(`{"id":"7","acct":"me"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	a := NewAccount(New(srv.URL, "tok"))
	ref, err := a.Locate(context.Background(), "https://m.example/@me/99")
	if err != nil || ref.ID != "99" || ref.URL != "https://m.example/@me/99" {
		t.Fatalf("ref = %+v, %v", ref, err)
	}
	author = "8"
	if _, err := a.Locate(context.Background(), "https://m.example/@other/99"); !errors.Is(err, destination.ErrNotOwnPost) {
		t.Fatalf("another's status: %v", err)
	}
	found = false
	if _, err := a.Locate(context.Background(), "https://m.example/@me/100"); !errors.Is(err, destination.ErrPostNotFound) {
		t.Fatalf("missing status: %v", err)
	}
}
