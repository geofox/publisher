// Package notify sends operational alerts via the alertmanager-pushover relay
// (which holds the Pushover credentials + encryption key and delivers them).
package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/geofox/publisher/internal/redact"
)

// Webhook POSTs an Alertmanager-format webhook to the alertmanager-pushover
// relay. An empty URL makes Alert a no-op (logs once) so the feature degrades to
// log-only rather than failing.
type Webhook struct {
	URL  string
	User string
	Pass string
	// Source is this server's address (PUBLIC_URL), sent as the alert's
	// generatorURL ("" when unset).
	Source   string
	HTTP     *http.Client
	warnOnce sync.Once
}

func NewWebhook(url, user, pass string) *Webhook {
	return &Webhook{URL: url, User: user, Pass: pass, HTTP: &http.Client{Timeout: 10 * time.Second}}
}

// Alert sends one alert. Its summary and body are scrubbed of credentials
// first: they often carry an upstream error.
func (w *Webhook) Alert(ctx context.Context, summary, body string) error {
	if w.URL == "" {
		w.warnOnce.Do(func() {
			slog.Warn("alert webhook not configured; alerts go to logs only")
		})
		return nil
	}
	status, answer, err := w.send(ctx, redact.Scrub(summary), redact.Scrub(body))
	if err != nil {
		return err
	}
	if status/100 != 2 {
		return fmt.Errorf("alert webhook: status %d: %s", status, redact.Upstream(answer))
	}
	return nil
}

// Test sends a test alert (Settings, Integrations) and returns the
// status the webhook answered, or the transport's error.
func (w *Webhook) Test(ctx context.Context) (int, error) {
	status, _, err := w.send(ctx, "Publisher test alert",
		"Sent from Publisher's settings to check that alerts reach you. Nothing is wrong.")
	return status, err
}

// send POSTs one alert, labelled as every alert is (a receiver routes a
// test as it would a real one), and returns the status and up to 64 KiB
// of the answer.
func (w *Webhook) send(ctx context.Context, summary, body string) (int, string, error) {
	type alert struct {
		Status       string            `json:"status"`
		Labels       map[string]string `json:"labels"`
		Annotations  map[string]string `json:"annotations"`
		GeneratorURL string            `json:"generatorURL"`
	}
	payload := struct {
		Status string  `json:"status"`
		Alerts []alert `json:"alerts"`
	}{
		Status: "firing",
		Alerts: []alert{{
			Status:       "firing",
			Labels:       map[string]string{"alertname": "publisher-threads-token", "severity": "warning", "host": "publisher"},
			Annotations:  map[string]string{"summary": summary, "description": body},
			GeneratorURL: w.Source,
		}},
	}
	b, err := json.Marshal(payload)
	if err != nil {
		return 0, "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, w.URL, bytes.NewReader(b))
	if err != nil {
		return 0, "", redact.URLError(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if w.User != "" || w.Pass != "" {
		req.SetBasicAuth(w.User, w.Pass)
	}
	resp, err := w.HTTP.Do(req)
	if err != nil {
		return 0, "", redact.URLError(err) // the address may hold a credential
	}
	defer resp.Body.Close()
	answer, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	return resp.StatusCode, string(answer), nil
}
