package drivers

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The core works from manifests and capabilities, never from destination
// names: a destination added later (a third-party plug-in) must not need a
// core change. This fails when a core package's non-test Go code names a
// built-in destination type in a string literal.
func TestCoreNamesNoDestination(t *testing.T) {
	Register()
	core := []string{"../dispatch", "../api", "../store"}
	lit := regexp.MustCompile(`"(bluesky|mastodon|nostr|threads)"`)
	for _, dir := range core {
		files, err := filepath.Glob(filepath.Join(dir, "*.go"))
		if err != nil {
			t.Fatal(err)
		}
		for _, f := range files {
			if strings.HasSuffix(f, "_test.go") {
				continue
			}
			b, err := os.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}
			for i, line := range strings.Split(string(b), "\n") {
				if trimmed := strings.TrimSpace(line); strings.HasPrefix(trimmed, "//") {
					continue
				}
				if m := lit.FindString(line); m != "" {
					t.Errorf("%s:%d names destination %s; use manifests/capabilities instead", f, i+1, m)
				}
			}
		}
	}
}
