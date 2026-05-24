package store

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestPostJSONTags(t *testing.T) {
	p := Post{ID: "p1", CreatedAt: time.Now(), MasterText: "hi", Platforms: []string{"nostr"},
		Source: "web", Status: "success",
		Targets: []Target{{Platform: "nostr", FinalText: "hi", Status: "success", RemoteID: "ev1",
			Attempts: []Attempt{{AttemptNo: 1, Status: "success", AttemptedAt: time.Now()}}}},
		Media: []Media{{Ordinal: 0, BlossomURL: "https://b/x", SHA256: "d", Alt: "a"}}}
	b, _ := json.Marshal(p)
	s := string(b)
	for _, want := range []string{`"id"`, `"created_at"`, `"master_text"`, `"platforms"`, `"status"`,
		`"final_text"`, `"remote_id"`, `"attempt_no"`, `"blossom_url"`, `"size_bytes"`} {
		if !strings.Contains(s, want) {
			t.Errorf("missing json key %s in %s", want, s)
		}
	}
	for _, bad := range []string{`"MasterText"`, `"FinalText"`, `"BlossomURL"`} {
		if strings.Contains(s, bad) {
			t.Errorf("leaked Go field name %s", bad)
		}
	}
}
