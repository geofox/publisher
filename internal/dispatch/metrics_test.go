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

func TestSchedulerMetricsHelpers(t *testing.T) {
	reFires := regexp.MustCompile(`publisher_scheduler_fires_total (\d+)`)
	bf := metricValue(t, reFires)
	metrics.IncSchedulerFire()
	if metricValue(t, reFires) < bf+1 {
		t.Fatalf("scheduler_fires_total not incremented")
	}

	reBacklog := regexp.MustCompile(`publisher_attention_backlog (\d+)`)
	metrics.SetAttentionBacklog(7)
	if metricValue(t, reBacklog) != 7 {
		t.Fatalf("attention_backlog gauge = %v, want 7", metricValue(t, reBacklog))
	}
}

// runAction (native repost/quote) is also a platform send and must be counted
// in publish_total. fakeMastodon.Reblog returns an empty TargetResult, which
// runAction normalizes to status "failed" — so this asserts the failed series.
func TestRunActionRecordsPublish(t *testing.T) {
	d := &Dispatcher{Mastodon: fakeMastodon{}}
	re := regexp.MustCompile(`publisher_publish_total\{outcome="failed",platform="mastodon"\} (\d+)`)
	before := metricValue(t, re)

	_ = d.runAction(context.Background(), actionRepost, "mastodon", "", Overrides{}, nil, nil, InteractRef{})

	after := metricValue(t, re)
	if after < before+1 {
		t.Fatalf("publish_total mastodon/failed via runAction: before=%v after=%v, want +1", before, after)
	}
}
