package dispatch

import "time"

// backoff returns the wait between the attempt-th attempt and the next one:
// base * 2^(attempt-1), clamped to max. attempt is the number of attempts
// already made (post_targets.attempt_count). No jitter — a single-operator
// deployment has no thundering-herd to spread.
func backoff(attempt int, base, max time.Duration) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	d := base
	for i := 1; i < attempt; i++ {
		d *= 2
		if d >= max {
			return max
		}
	}
	if d > max {
		return max
	}
	return d
}
