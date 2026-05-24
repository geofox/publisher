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
)

// Webhook POSTs an Alertmanager-format webhook to the alertmanager-pushover
// relay. An empty URL makes Alert a no-op (logs once) so the feature degrades to
// log-only rather than failing.
type Webhook struct {
	URL      string
	User     string
	Pass     string
	HTTP     *http.Client
	warnOnce sync.Once
}

func NewWebhook(url, user, pass string) *Webhook {
	return &Webhook{URL: url, User: user, Pass: pass, HTTP: &http.Client{Timeout: 10 * time.Second}}
}

func (w *Webhook) Alert(ctx context.Context, summary, body string) error {
	if w.URL == "" {
		w.warnOnce.Do(func() {
			slog.Warn("alert webhook not configured; alerts go to logs only")
		})
		return nil
	}
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
			GeneratorURL: "https://post.geoffrey.one",
		}},
	}
	b, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, w.URL, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if w.User != "" || w.Pass != "" {
		req.SetBasicAuth(w.User, w.Pass)
	}
	resp, err := w.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
		return fmt.Errorf("alert webhook: status %d: %s", resp.StatusCode, string(b))
	}
	return nil
}
