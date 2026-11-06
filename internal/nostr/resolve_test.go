package nostr

import (
	"strings"
	"testing"
	"time"
)

// Moved from internal/resolve (NostrAdapter).
func TestResolvedFromEventAllowsEverything(t *testing.T) {
	ev := &SourceEvent{
		IDHex: "ab12cd", Author: "f00dbabef00dbabef00dbabef00dbabef00dbabef00dbabef00dbabef00dbabe",
		Kind: 1, Content: "hello", CreatedAt: time.Now(),
	}
	rs := resolvedFromEvent(ev)
	if !rs.Actions.Reply.Allowed || !rs.Actions.Quote.Allowed || !rs.Actions.Repost.Allowed {
		t.Errorf("nostr actions should all be allowed: %+v", rs.Actions)
	}
	if rs.Preview.WebURL == "" || rs.Ref.EventID != "ab12cd" || rs.Ref.Type != Type {
		t.Errorf("ref/web url wrong: %+v", rs)
	}
	if rs.Preview.AuthorName != "npub:f00dbabe" {
		t.Errorf("author name = %q, want the short key", rs.Preview.AuthorName)
	}
}

func TestResolvedFromEventProtectedRepostReason(t *testing.T) {
	rs := resolvedFromEvent(&SourceEvent{IDHex: "ab12cd", Author: "f00d", Kind: 1, Protected: true})
	if rs.Actions.Repost.Reason == "" {
		t.Error("protected event should annotate the repost reason")
	}
}

// A full id gives a self-describing nevent link; a truncated one the bare
// hex form.
func TestNjumpURL(t *testing.T) {
	id := "f00dbabef00dbabef00dbabef00dbabef00dbabef00dbabef00dbabef00dbabe"
	if u := njumpURL(id, "", []string{"wss://r.example"}); !strings.HasPrefix(u, "https://njump.me/nevent1") {
		t.Errorf("njumpURL = %q, want an nevent link", u)
	}
	if u := njumpURL("ab12", "", nil); u != "https://njump.me/ab12" {
		t.Errorf("njumpURL = %q, want the bare id", u)
	}
}
