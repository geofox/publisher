package threads

import (
	"io"
	"net/http/httptest"
	"regexp"
	"strconv"
	"testing"

	"github.com/geofox/publisher/internal/metrics"
)

func TestTokenExpiryGaugeHelper(t *testing.T) {
	srv := httptest.NewServer(metrics.Handler())
	defer srv.Close()
	metrics.SetTokenExpiry("threads", 86400)

	resp, err := srv.Client().Get(srv.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	re := regexp.MustCompile(`publisher_token_expiry_seconds\{platform="threads"\} (\d+)`)
	m := re.FindSubmatch(body)
	if m == nil {
		t.Fatal("token_expiry_seconds{threads} not present")
	}
	if v, _ := strconv.ParseFloat(string(m[1]), 64); v != 86400 {
		t.Fatalf("token_expiry_seconds = %v, want 86400", v)
	}
}
