// Package metrics defines the publisher's Prometheus metric set and exposes a
// /metrics handler. Recorder helpers are infallible and non-blocking — a metrics
// call must never alter or block a publish. All metrics live on a private
// registry (plus the standard Go/process collectors) so the endpoint reflects
// this app only, not collectors pulled in transitively by SDK dependencies.
package metrics

import (
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

var (
	reg = prometheus.NewRegistry()

	publishTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "publisher_publish_total",
		Help: "Platform publish operations by platform and outcome.",
	}, []string{"platform", "outcome"})

	publishDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "publisher_publish_duration_seconds",
		Help:    "Latency of a single platform publish operation.",
		Buckets: prometheus.DefBuckets,
	}, []string{"platform"})

	retryTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "publisher_retry_total",
		Help: "Auto-retry passes fired, by platform.",
	}, []string{"platform"})

	retryExhausted = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "publisher_retry_exhausted_total",
		Help: "Targets that exhausted their retry budget, by platform.",
	}, []string{"platform"})

	schedulerFires = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "publisher_scheduler_fires_total",
		Help: "Scheduled posts fired by the scheduler.",
	})

	attentionBacklog = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "publisher_attention_backlog",
		Help: "Posts currently needing attention (delivery failed or partial).",
	})

	tokenExpiry = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "publisher_token_expiry_seconds",
		Help: "Seconds until a platform access token expires.",
	}, []string{"platform"})

	buildInfo = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "publisher_build_info",
		Help: "Build metadata; constant 1.",
	}, []string{"version", "commit"})
)

func init() {
	reg.MustRegister(
		publishTotal, publishDuration, retryTotal, retryExhausted,
		schedulerFires, attentionBacklog, tokenExpiry, buildInfo,
		collectors.NewGoCollector(),
		// No ProcessCollector: it reads /proc, which the FROM scratch image
		// lacks, so it would silently emit nothing. GoCollector is pure-Go.
	)
}

// Handler serves the Prometheus exposition format for this app's registry.
func Handler() http.Handler {
	return promhttp.HandlerFor(reg, promhttp.HandlerOpts{})
}

// RecordPublish counts one platform send and observes its latency. outcome is
// the normalized TargetResult.Status: "success" | "partial" | "failed".
func RecordPublish(platform, outcome string, d time.Duration) {
	publishTotal.WithLabelValues(platform, outcome).Inc()
	publishDuration.WithLabelValues(platform).Observe(d.Seconds())
}

// RecordRetry counts one auto-retry pass fired for a platform.
func RecordRetry(platform string) { retryTotal.WithLabelValues(platform).Inc() }

// IncRetryExhausted counts one target that just hit its retry cap.
func IncRetryExhausted(platform string) { retryExhausted.WithLabelValues(platform).Inc() }

// IncSchedulerFire counts one scheduled post fired.
func IncSchedulerFire() { schedulerFires.Inc() }

// SetAttentionBacklog sets the current failed+partial backlog gauge.
func SetAttentionBacklog(n float64) { attentionBacklog.Set(n) }

// SetTokenExpiry sets seconds-until-expiry for a platform's access token.
func SetTokenExpiry(platform string, secondsUntil float64) {
	tokenExpiry.WithLabelValues(platform).Set(secondsUntil)
}

// SetBuildInfo records build metadata as a constant-1 gauge.
func SetBuildInfo(version, commit string) {
	buildInfo.WithLabelValues(version, commit).Set(1)
}
