package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The e2e harness points connect flows at fakes through test hooks; the
// server never sets them: its flows reach the real networks through the
// guarded dialer, and their redirect URI comes from PUBLIC_URL.
func TestServerSetsNoTestHooks(t *testing.T) {
	hooks := []string{"ConnectNet", "ConnectBaseURL", "TLSClientConfig", "threads.AuthorizeURL", "threads.TokenURL",
		"threads.ExchangeURL", "threads.DebugURL"}
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, h := range hooks {
			if strings.Contains(string(src), h) {
				t.Errorf("%s uses the test hook %s", f, h)
			}
		}
	}
}
