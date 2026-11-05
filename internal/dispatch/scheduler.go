package dispatch

import "context"

// Notifier sends an operational alert (implemented by *notify.Webhook).
type Notifier interface {
	Alert(ctx context.Context, summary, body string) error
}
