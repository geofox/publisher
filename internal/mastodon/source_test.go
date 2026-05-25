package mastodon

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestResolveStatusMapsFields(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v2/search":
			w.Write([]byte(`{"statuses":[{"id":"local99"}]}`))
		case "/api/v1/statuses/local99":
			w.Write([]byte(`{
				"id":"local99","content":"<p>hello</p>","visibility":"public","url":"https://x/@a/9",
				"created_at":"2026-05-25T10:00:00Z",
				"account":{"display_name":"Alice","acct":"alice@x"},
				"media_attachments":[{"type":"image","url":"https://x/i.png","description":"alt"}],
				"quote_approval":{"current_user":"automatic"}
			}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	c := New(srv.URL, "tok")
	st, err := c.ResolveStatus(context.Background(), "https://x/@a/9")
	if err != nil {
		t.Fatal(err)
	}
	if st.LocalID != "local99" || st.AuthorName != "Alice" || st.Visibility != "public" {
		t.Fatalf("status mapped wrong: %+v", st)
	}
	if st.QuoteCurrentUser != "automatic" || len(st.Media) != 1 {
		t.Fatalf("quote/media mapped wrong: %+v", st)
	}
	if st.TextPlain != "hello" {
		t.Errorf("content should be de-HTMLed: %q", st.TextPlain)
	}
}
