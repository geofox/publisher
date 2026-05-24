package dispatch

import (
	"context"
	"log/slog"
	"time"
)

const schedulerTick = 30 * time.Second

// Notifier sends an operational alert (implemented by *notify.Webhook).
type Notifier interface {
	Alert(ctx context.Context, summary, body string) error
}

// Scheduler fires due scheduled posts and marks overdue ones missed.
type Scheduler struct {
	disp     *Dispatcher
	notifier Notifier
	grace    time.Duration
	now      func() time.Time
}

func NewScheduler(d *Dispatcher, n Notifier, grace time.Duration) *Scheduler {
	return &Scheduler{disp: d, notifier: n, grace: grace, now: time.Now}
}

// overdue reports whether a post due at scheduledAt is past the grace window.
func overdue(now, scheduledAt time.Time, grace time.Duration) bool {
	return now.Sub(scheduledAt) > grace
}

// Start reconciles crash-leftover 'sending' rows, then ticks until ctx is done.
func (s *Scheduler) Start(ctx context.Context) {
	if n, err := s.disp.Store.ResetSendingToScheduled(); err != nil {
		slog.Error("scheduler: reset sending failed", "err", err)
	} else if n > 0 {
		slog.Info("scheduler: reset crash-leftover sending posts", "n", n)
	}
	s.runDue(ctx)
	t := time.NewTicker(schedulerTick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.runDue(ctx)
		}
	}
}

func (s *Scheduler) runDue(ctx context.Context) {
	now := s.now()
	ids, err := s.disp.Store.DueScheduledPosts(now)
	if err != nil {
		slog.Error("scheduler: due query failed", "err", err)
		return
	}
	for _, id := range ids {
		won, err := s.disp.Store.ClaimScheduled(id)
		if err != nil {
			slog.Error("scheduler: claim failed", "post_id", id, "err", err)
			continue
		}
		if !won {
			continue
		}
		post, err := s.disp.Store.GetPost(id)
		if err != nil {
			slog.Error("scheduler: load failed", "post_id", id, "err", err)
			continue
		}
		if post.ScheduledAt != nil && overdue(now, *post.ScheduledAt, s.grace) {
			if err := s.disp.Store.MarkMissed(id); err != nil {
				slog.Error("scheduler: mark missed failed", "post_id", id, "err", err)
				continue
			}
			slog.Warn("scheduler: post missed (beyond grace)", "post_id", id, "scheduled_at", post.ScheduledAt)
			if err := s.notifier.Alert(ctx, "Scheduled post missed",
				"post "+id+" was due "+post.ScheduledAt.Format(time.RFC3339)+" but is beyond the grace window; not published"); err != nil {
				slog.Error("scheduler: missed alert failed", "err", err)
			}
			continue
		}
		if _, err := s.disp.Fire(ctx, id); err != nil {
			slog.Error("scheduler: fire failed", "post_id", id, "err", err)
		} else {
			slog.Info("scheduler: fired scheduled post", "post_id", id)
		}
	}
}
