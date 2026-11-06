package nostr

import (
	"slices"
	"strings"
	"testing"

	gonostr "fiatjaf.com/nostr"

	"github.com/geofox/publisher/internal/destination"
)

// Replying to an external note that is itself a reply: NIP-10 wants the
// "root" e-tag to name the thread root, the "reply" e-tag the note, and the
// p-tags to carry the note's p-tags plus its author.
func TestReplyToExternalReplyKeepsThreadRoot(t *testing.T) {
	root := strings.Repeat("a", 64)
	parent := strings.Repeat("b", 64)
	author := strings.Repeat("c", 64)
	other := strings.Repeat("e", 64)
	owner := strings.Repeat("d", 64)
	gotRoot, mentions := threadContext(gonostr.Tags{{"e", root, "", "root"}, {"e", parent[:10], "", "reply"}, {"p", other}, {"p", owner}})
	if gotRoot != root || !slices.Equal(mentions, []string{other, owner}) {
		t.Fatalf("threadContext = %q %v", gotRoot, mentions)
	}
	in := noteInput(destination.PublishRequest{Text: "me too", ReplyTo: &destination.SourceRef{
		Type: "nostr", EventID: parent, Author: author, ThreadRootID: gotRoot, Mentions: mentions,
	}})
	tags := replyTags(in.ReplyTo, owner)
	want := []gonostr.Tag{{"e", root, "", "root"}, {"e", parent, "", "reply", author}, {"p", author}, {"p", other}}
	if !slices.EqualFunc(tags, want, func(a, b gonostr.Tag) bool { return slices.Equal(a, b) }) {
		t.Errorf("tags = %v, want %v", tags, want)
	}
}

// The deprecated positional form: the first e-tag is the root.
func TestThreadContextPositional(t *testing.T) {
	root := strings.Repeat("a", 64)
	if got, _ := threadContext(gonostr.Tags{{"e", root}, {"e", strings.Repeat("b", 64)}}); got != root {
		t.Errorf("root = %q, want %q", got, root)
	}
	if got, _ := threadContext(gonostr.Tags{{"p", root}}); got != "" {
		t.Errorf("root of a root note = %q, want empty", got)
	}
}
