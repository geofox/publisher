package mastodon

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/geofox/publisher/internal/destination"
)

// Moved from internal/resolve (MastodonAdapter).
func TestResolvedFromStatusActions(t *testing.T) {
	rs := resolvedFromStatus(&SourceStatus{LocalID: "9", AuthorAcct: "a@x", Visibility: "private", QuoteCurrentUser: "denied"})
	if rs.Actions.Repost.Allowed || rs.Actions.Quote.Allowed {
		t.Errorf("private+denied should block boost and native quote: %+v", rs.Actions)
	}
	if !rs.Actions.Reply.Allowed {
		t.Error("reply should be allowed")
	}
	if rs.Ref.LocalID != "9" || rs.Ref.Type != Type || rs.Preview.AuthorHandle != "@a@x" {
		t.Errorf("ref/preview = %+v %+v", rs.Ref, rs.Preview)
	}
	rs = resolvedFromStatus(&SourceStatus{LocalID: "9", Visibility: "public", QuoteCurrentUser: "automatic"})
	if !rs.Actions.Quote.Allowed || !rs.Actions.Repost.Allowed {
		t.Errorf("public+automatic should allow quote+boost: %+v", rs.Actions)
	}
	for _, q := range []string{"manual", "unknown", ""} {
		rs = resolvedFromStatus(&SourceStatus{QuoteCurrentUser: q})
		if q == "manual" != rs.Actions.Quote.Allowed || rs.Actions.Quote.Reason == "" {
			t.Errorf("quote_approval %q → %+v", q, rs.Actions.Quote)
		}
	}
}

// Through the account: a post the instance can't find is ErrSourceNotFound
// (the API answers "unresolvable"), and a found one maps to the instance's
// local id.
func TestResolveSourceThroughAccount(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/v2/search" && strings.Contains(r.URL.Query().Get("q"), "missing"):
			_, _ = w.Write([]byte(`{"statuses":[]}`))
		case r.URL.Path == "/api/v2/search":
			_, _ = w.Write([]byte(`{"statuses":[{"id":"42"}]}`))
		case r.URL.Path == "/api/v1/statuses/42":
			_, _ = w.Write([]byte(`{"id":"42","content":"<p>hi</p>","visibility":"public","url":"https://masto.example/@a/42",
				"account":{"id":"7","acct":"a"},"quote_approval":{"current_user":"automatic"}}`))
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	acct := NewAccount(New(srv.URL, "tok"))
	_, err := acct.ResolveSource(context.Background(), "https://elsewhere.example/@b/missing")
	if !errors.Is(err, destination.ErrSourceNotFound) || !errors.Is(err, ErrStatusNotFound) {
		t.Errorf("not found: err = %v", err)
	}
	rs, err := acct.ResolveSource(context.Background(), "https://elsewhere.example/@a/1")
	if err != nil {
		t.Fatal(err)
	}
	if rs.Ref.LocalID != "42" || rs.Preview.Text != "hi" || !rs.Actions.Quote.Allowed {
		t.Errorf("resolved = %+v", rs)
	}
}
