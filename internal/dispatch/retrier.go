package dispatch

import "time"

// backoff returns the delay before the next retry: base * 2^(attempt-1),
// clamped to max. attempt is the number of attempts already made
// (post_targets.attempt_count). No jitter — a single-operator deployment
// has no thundering-herd to spread.
func backoff(attempt int, base, max time.Duration) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	d := base
	for i := 1; i < attempt; i++ {
		next := d * 2
		if next <= 0 || next >= max { // next <= 0 catches int64 overflow
			return max
		}
		d = next
	}
	if d > max {
		return max
	}
	return d
}
