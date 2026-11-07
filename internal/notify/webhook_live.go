package notify

import (
	"context"
	"log/slog"
	"sync"
)

// Live sends each alert to the webhook set at that moment (the owner's
// settings), or logs it when none is.
type Live struct {
	// Webhook returns the webhook now; nil or an empty URL: none.
	Webhook  func() *Webhook
	warnOnce sync.Once
}

// Alert sends one alert (see Webhook.Alert).
func (l *Live) Alert(ctx context.Context, summary, body string) error {
	var w *Webhook
	if l.Webhook != nil {
		w = l.Webhook()
	}
	if w == nil || w.URL == "" {
		l.warnOnce.Do(func() { slog.Warn("alert webhook not configured; alerts go to logs only") })
		return nil
	}
	return w.Alert(ctx, summary, body)
}
