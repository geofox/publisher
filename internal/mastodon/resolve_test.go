package mastodon

import (
	"testing"
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
