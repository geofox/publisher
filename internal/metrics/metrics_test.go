package metrics

import (
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestRecordPublish(t *testing.T) {
	RecordPublish("mastodon", "success", 1500*time.Millisecond)
	want := `
# HELP publisher_publish_total Platform publish operations by platform and outcome.
# TYPE publisher_publish_total counter
publisher_publish_total{outcome="success",platform="mastodon"} 1
`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(want), "publisher_publish_total"); err != nil {
		t.Fatal(err)
	}
}

func TestSetBuildInfo(t *testing.T) {
	SetBuildInfo("v1.3.0", "abc123")
	want := `
# HELP publisher_build_info Build metadata; constant 1.
# TYPE publisher_build_info gauge
publisher_build_info{commit="abc123",version="v1.3.0"} 1
`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(want), "publisher_build_info"); err != nil {
		t.Fatal(err)
	}
}
