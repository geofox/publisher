package dispatch

import (
	"context"
	"io"
	"net/http/httptest"
	"regexp"
	"strconv"
	"testing"

	gonostr "fiatjaf.com/nostr"
	"github.com/geofox/publisher/internal/metrics"
)

// fakeMastodon implements MastodonPoster; PostText always succeeds.
type fakeMastodon struct{}

func (fakeMastodon) PostText(ctx context.Context, text string, o Overrides, imgs []Img, replyTo *ReplyRef) (TargetResult, error) {
	return TargetResult{Platform: "mastodon", Status: "success", RemoteID: "1"}, nil
}
func (fakeMastodon) Reblog(ctx context.Context, statusID string) (TargetResult, error) {
	return TargetResult{}, nil
}
func (fakeMastodon) QuoteStatus(ctx context.Context, text, quotedID string, imgs []Img) (TargetResult, error) {
	return TargetResult{}, nil
}

func metricValue(t *testing.T, re *regexp.Regexp) float64 {
	t.Helper()
	srv := httptest.NewServer(metrics.Handler())
	defer srv.Close()
	resp, err := srv.Client().Get(srv.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	m := re.FindSubmatch(body)
	if m == nil {
		return 0
	}
	v, _ := strconv.ParseFloat(string(m[1]), 64)
	return v
}

func TestRunPlatformRecordsPublish(t *testing.T) {
	d := &Dispatcher{Mastodon: fakeMastodon{}}
	re := regexp.MustCompile(`publisher_publish_total\{outcome="success",platform="mastodon"\} (\d+)`)
	before := metricValue(t, re)

	_ = d.runPlatform(context.Background(), "mastodon", "hi", Overrides{}, nil, []gonostr.Tag(nil), nil)

	after := metricValue(t, re)
	if after < before+1 {
		t.Fatalf("publish_total mastodon/success: before=%v after=%v, want +1", before, after)
	}
}

func TestMetricsHelpersForRetry(t *testing.T) {
	reExhausted := regexp.MustCompile(`publisher_retry_exhausted_total\{platform="bluesky"\} (\d+)`)
	reRetry := regexp.MustCompile(`publisher_retry_total\{platform="bluesky"\} (\d+)`)
	be, br := metricValue(t, reExhausted), metricValue(t, reRetry)

	metrics.IncRetryExhausted("bluesky")
	metrics.RecordRetry("bluesky")

	if metricValue(t, reExhausted) < be+1 {
		t.Fatalf("retry_exhausted_total not incremented")
	}
	if metricValue(t, reRetry) < br+1 {
		t.Fatalf("retry_total not incremented")
	}
}
