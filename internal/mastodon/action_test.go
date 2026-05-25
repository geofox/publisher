package mastodon

import (
	"context"
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
	res, err := c.QuotePost(context.Background(), "my take", "99")
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
