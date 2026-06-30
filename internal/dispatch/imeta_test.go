package dispatch

import (
	"strings"
	"testing"

	"github.com/geofox/publisher/internal/store"
)

func TestBuildImetasIndexParallel(t *testing.T) {
	recs := []store.Media{
		{BlossomURL: "https://b/x", Mime: "image/png", SHA256: "aa", Dim: "10x10"},
		{BlossomURL: ""}, // no Blossom URL → empty placeholder, alignment preserved
		{BlossomURL: "https://b/z", Mime: "image/jpeg", SHA256: "cc"},
	}
	got := buildImetas(recs)
	if len(got) != 3 {
		t.Fatalf("buildImetas len=%d, want 3 (index-parallel)", len(got))
	}
	if len(got[1]) != 0 {
		t.Errorf("rec without Blossom URL should be an empty placeholder, got %v", got[1])
	}
	if s := strings.Join(got[0], " "); !strings.Contains(s, "url https://b/x") {
		t.Errorf("got[0]=%q", s)
	}
}

func TestPickImetasSelectsAndDropsEmpties(t *testing.T) {
	recs := []store.Media{
		{BlossomURL: "https://b/x", Mime: "image/png", SHA256: "aa"},
		{BlossomURL: ""},
		{BlossomURL: "https://b/z", Mime: "image/jpeg", SHA256: "cc"},
	}
	imetas := buildImetas(recs)
	// Select indices 1 (empty) and 2 (real): only the real one survives.
	got := pickImetas(imetas, []int{1, 2})
	if len(got) != 1 {
		t.Fatalf("pickImetas dropped-empty len=%d, want 1", len(got))
	}
	if s := strings.Join(got[0], " "); !strings.Contains(s, "url https://b/z") {
		t.Errorf("got[0]=%q", s)
	}
	// Out-of-range index is skipped, not a panic.
	if n := len(pickImetas(imetas, []int{0, 9})); n != 1 {
		t.Errorf("out-of-range skip len=%d, want 1", n)
	}
}
