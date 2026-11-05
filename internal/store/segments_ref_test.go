package store

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A segment's Ref is stored (threading needs it) but never serialized for
// API clients: for Nostr it is a ready-to-broadcast signed event.
func TestSegmentRefStoredButNotSerialized(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ref := json.RawMessage(`{"sig":"secret-ish"}`)
	p := &Post{ID: "p1", CreatedAt: time.Now().UTC(), Platforms: []string{"x"}, Source: "web", Status: "success",
		Targets: []Target{{Platform: "x", Status: "success",
			Segments: []Segment{{Ordinal: 0, Text: "a", Status: "success", Ref: ref}, {Ordinal: 1, Text: "b", Status: "success"}}}}}
	if err := st.SavePost(p); err != nil {
		t.Fatal(err)
	}
	got, err := st.GetPost("p1")
	if err != nil {
		t.Fatal(err)
	}
	if s := string(got.Targets[0].Segments[0].Ref); s != string(ref) {
		t.Fatalf("stored ref = %q, want %q", s, ref)
	}
	b, _ := json.Marshal(got)
	if strings.Contains(string(b), "secret-ish") {
		t.Fatalf("API form leaks the segment ref: %s", b)
	}
	// UpdateTargetSegments keeps it too.
	segs := got.Targets[0].Segments
	if err := st.UpdateTargetSegments(got.Targets[0].ID, segs, "success", "", "", 0, "", ""); err != nil {
		t.Fatal(err)
	}
	got, _ = st.GetPost("p1")
	if s := string(got.Targets[0].Segments[0].Ref); s != string(ref) {
		t.Fatalf("ref lost on update: %q", s)
	}
}
