package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	pubnostr "github.com/geofox/publisher/internal/nostr"
)

func TestMetricsRoute(t *testing.T) {
	a := New(&pubnostr.Publisher{}, nil)
	srv := httptest.NewServer(a.Routes())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	buf := make([]byte, 4096)
	n, _ := resp.Body.Read(buf)
	if !strings.Contains(string(buf[:n]), "publisher_build_info") &&
		!strings.Contains(string(buf[:n]), "go_goroutines") {
		t.Fatalf("metrics body missing expected series")
	}
}
