package dispatch

import (
	"testing"
	"time"
)

func TestBackoff(t *testing.T) {
	base, max := 2*time.Minute, 1*time.Hour
	cases := []struct {
		attempt int
		want    time.Duration
	}{
		{0, 2 * time.Minute},  // clamped up to attempt 1
		{1, 2 * time.Minute},  // base
		{2, 4 * time.Minute},
		{3, 8 * time.Minute},
		{4, 16 * time.Minute},
		{5, 32 * time.Minute},
		{6, 1 * time.Hour},    // 64m clamped to max
		{20, 1 * time.Hour},    // far past cap, no overflow
		{100, 1 * time.Hour},   // int64 overflow boundary — must still return max, not negative
	}
	for _, c := range cases {
		if got := backoff(c.attempt, base, max); got != c.want {
			t.Errorf("backoff(%d) = %v, want %v", c.attempt, got, c.want)
		}
	}
}
