package nostr

import (
	"context"
	"strings"
	"testing"

	gonostr "fiatjaf.com/nostr"

	"github.com/geofox/publisher/internal/destination"
)

func TestImetas(t *testing.T) {
	got := Imetas([]destination.MediaItem{
		{URL: "https://b/x", Mime: "image/png", SHA256: "aa", Width: 10, Height: 10, Blurhash: "LEH", Alt: "A blue\n  gradient "},
		{Mime: "image/png"}, // no URL: nothing to point at
		{URL: "https://b/v", Mime: "video/mp4", SHA256: "bb", PosterURL: "https://b/p"},
	})
	if len(got) != 2 {
		t.Fatalf("want one tag per item with a URL, got %d", len(got))
	}
	if s := strings.Join(got[0], " "); s != "imeta url https://b/x m image/png x aa dim 10x10 blurhash LEH alt A blue gradient" {
		t.Errorf("imeta[0] = %q", s)
	}
	for _, f := range got[1] {
		if strings.HasPrefix(f, "alt") {
			t.Errorf("an item without alt text has an alt field: %v", got[1])
		}
	}
	if s := strings.Join(got[1], " "); !strings.Contains(s, "image https://b/p") {
		t.Errorf("video imeta should carry its poster: %q", s)
	}
}

func TestStatusFromDeliveries(t *testing.T) {
	d := func(ss ...string) []destination.Delivery {
		var out []destination.Delivery
		for _, s := range ss {
			out = append(out, destination.Delivery{Status: s})
		}
		return out
	}
	cases := []struct {
		in   []destination.Delivery
		want destination.ItemStatus
	}{
		{d("ok", "ok"), destination.StatusPublished},
		{d("ok", "failed"), destination.StatusPartial},
		{d("failed", "failed"), destination.StatusFailed},
		{d("ok", "ok", "skipped"), destination.StatusPublished}, // skipped relays don't count
		{d("failed", "skipped"), destination.StatusFailed},
		{d("skipped"), destination.StatusFailed}, // nothing attempted
	}
	for _, c := range cases {
		if got := StatusFromDeliveries(c.in); got != c.want {
			t.Errorf("%v → %s, want %s", c.in, got, c.want)
		}
	}
}

func hasTag(tags []gonostr.Tag, want ...string) bool {
	for _, t := range tags {
		if strings.Join(t, "\x00") == strings.Join(want, "\x00") {
			return true
		}
	}
	return false
}

// A content warning is sent as the NIP-36 tag on notes, replies and quotes.
func TestNoteInputContentWarning(t *testing.T) {
	for name, req := range map[string]destination.PublishRequest{
		"note":  {Text: "spoilers", ContentWarning: "film\nending"},
		"reply": {Text: "r", ContentWarning: "film ending", InReplyTo: &destination.ThreadRef{Root: destination.ItemRef{ID: "a"}, Parent: destination.ItemRef{ID: "b"}}},
		"quote": {Text: "q", ContentWarning: "film ending", Quote: &destination.SourceRef{EventID: strings.Repeat("ab", 32)}},
	} {
		in := noteInput(req)
		if !hasTag(in.Tags, "content-warning", "film ending") {
			t.Errorf("%s: tags %v lack the content warning", name, in.Tags)
		}
	}
	if in := noteInput(destination.PublishRequest{Text: "plain"}); len(in.Tags) != 0 {
		t.Errorf("no warning, no tag: %v", in.Tags)
	}
}

func TestNoteInputQuoteAndReply(t *testing.T) {
	id := strings.Repeat("ab", 32)
	q := noteInput(destination.PublishRequest{Text: "my take", Quote: &destination.SourceRef{EventID: id, RelayHints: []string{"wss://r"}}})
	if !hasTag(q.Tags, "q", id, "wss://r", "") || !strings.HasPrefix(q.Text, "my take\nnostr:nevent1") {
		t.Errorf("quote = %q %v", q.Text, q.Tags)
	}
	r := noteInput(destination.PublishRequest{Text: "hi", ReplyTo: &destination.SourceRef{EventID: id, Author: "pk"}})
	if r.ReplyTo == nil || r.ReplyTo.ParentID != id || r.ReplyTo.AuthorPubkey != "pk" {
		t.Errorf("external reply = %+v", r.ReplyTo)
	}
	pow := noteInput(destination.PublishRequest{Text: "x", Options: map[string]any{OptPOW: 12}})
	if pow.POW == nil || *pow.POW != 12 {
		t.Errorf("pow option not applied: %v", pow.POW)
	}
}

// An empty note fails the same way every time: invalid, not retried.
func TestPublishEmptyTextIsInvalid(t *testing.T) {
	a := &Account{P: New(Config{})}
	_, err := a.Publish(context.Background(), destination.PublishRequest{Text: "  "})
	if got := destination.ClassOf(err); got != destination.ClassInvalid {
		t.Fatalf("class = %q, want invalid (err %v)", got, err)
	}
}
