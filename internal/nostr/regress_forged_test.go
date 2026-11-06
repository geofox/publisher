package nostr

import (
	"testing"

	gonostr "fiatjaf.com/nostr"
)

// A relay answering a by-id query with a validly signed event that claims
// someone else's id must not be taken for that event.
func TestMatchesRefusesForgedID(t *testing.T) {
	sk := gonostr.Generate()
	ev := gonostr.Event{Kind: 1, Content: "forged", CreatedAt: gonostr.Now()}
	if err := ev.Sign(sk); err != nil {
		t.Fatal(err)
	}
	if !matches(&ev, gonostr.Filter{IDs: []gonostr.ID{ev.ID}}) {
		t.Fatal("a genuine event was refused")
	}
	var want gonostr.ID
	want[0] = 0xab
	forged := ev
	forged.ID = want
	if matches(&forged, gonostr.Filter{IDs: []gonostr.ID{want}}) {
		t.Fatal("an event whose id doesn't match its content was accepted")
	}
	other := gonostr.GetPublicKey(gonostr.Generate())
	if matches(&ev, gonostr.Filter{Authors: []gonostr.PubKey{other}}) {
		t.Fatal("an event by another author was accepted")
	}
}
