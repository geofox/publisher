# Automated failure recovery (Retrier) + observability — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add a background `Retrier` that automatically re-drives failed/partial post deliveries with exponential backoff and gives up cleanly after a cap, plus surface and alert on failures that need attention.

**Architecture:** A new `Retrier` background actor in `internal/dispatch` mirrors the existing `Scheduler` (tick → find eligible → act → alert) but scoped to recovery. It reuses `Dispatcher.Retry` (platform-level) and `Dispatcher.RetryRelay` (Nostr relay-level) rather than reimplementing delivery. State is minimal: `gave_up_at` (post_targets + target_relays) and `retry_count` (target_relays). Backoff timing is computed from existing `attempt_count`/`last_attempt_at`. Surfacing reuses the `GET /api/posts?status=` filter plus a cheap count endpoint; alerting reuses `notify.Webhook.Alert`.

**Tech Stack:** Go 1.x, SQLite (`database/sql`), `log/slog`, vanilla ES modules for the web UI. Tests are Go `testing` with an injectable `now func() time.Time` clock (same pattern as `Scheduler` tests).

---

## Spec deviations (decided during planning — confirm if undesired)

1. **Three new columns, not one.** Relay-level backoff/cap needs a per-relay counter, so `target_relays` gets `retry_count` in addition to `gave_up_at`. (`post_targets` gets only `gave_up_at`.)
2. **`gave_up_at` is sticky.** Set once by the Retrier when a target/relay hits the attempt cap; cleared only when a later attempt *succeeds*. Manual retry remains available on a given-up target as a one-shot override but does not re-arm automatic retries (it would already be over the cap). This avoids re-alerting on every tick.
3. **`missed` excluded from auto-retry** (already in the spec) and from the new `attention` filter — `missed` stays under the existing `failed` chip.

## File map

- **Create** `internal/dispatch/retrier.go` — the `Retrier` worker, `backoff()`, eligibility/due helpers.
- **Create** `internal/dispatch/retrier_test.go` — backoff, eligibility, loop, give-up, alert-once tests.
- **Modify** `internal/store/store.go` — three `addColumnIfMissing` migrations.
- **Modify** `internal/store/models.go` — `Target.GaveUpAt`, load `Target.LastAttempt`; `RelayState.GaveUpAt`/`RetryCount`; clear-on-success in write methods; bump `retry_count` in `UpdateRelayStatus`; `PostsNeedingRetry`, `MarkTargetGaveUp`, `MarkRelayGaveUp`, `AttentionCount`; `statusClause` `attention` case.
- **Modify** `internal/store/retry_test.go` — store-method tests.
- **Modify** `internal/dispatch/dispatch.go` — `Dispatcher.Alerter` field + immediate-failure alert in `Post`/`Interact`/`Fire`.
- **Modify** `internal/dispatch/dispatch_test.go` (or `notify_test.go`) — immediate-failure alert test.
- **Modify** `internal/config/config.go` + `config_test.go` — five `AUTO_RETRY_*`/`RETRIER_TICK` vars.
- **Modify** `internal/api/api.go` + `internal/api/api_test.go` — `GET /api/posts/attention/count`.
- **Modify** `cmd/publisher/main.go` — construct + start the `Retrier`, wire `d.Alerter`.
- **Modify** `internal/web/assets/index.html`, `history.js`, `app.css` — attention chip + badge + per-target retry status line.
- **Modify** `README.md` — document env vars, endpoints, behavior.

---

## Task 1: Backoff function

**Files:**
- Create: `internal/dispatch/retrier.go`
- Test: `internal/dispatch/retrier_test.go`

- [ ] **Step 1: Write the failing test**

```go
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
		{20, 1 * time.Hour},   // far past cap, no overflow
	}
	for _, c := range cases {
		if got := backoff(c.attempt, base, max); got != c.want {
			t.Errorf("backoff(%d) = %v, want %v", c.attempt, got, c.want)
		}
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/dispatch/ -run TestBackoff -v`
Expected: FAIL — `undefined: backoff`.

- [ ] **Step 3: Write minimal implementation**

Create `internal/dispatch/retrier.go`:

```go
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
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/dispatch/ -run TestBackoff -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/dispatch/retrier.go internal/dispatch/retrier_test.go
git commit -m "dispatch: exponential backoff helper for auto-retry"
```

---

## Task 2: Config — AUTO_RETRY_* env vars

**Files:**
- Modify: `internal/config/config.go`
- Test: `internal/config/config_test.go`

- [ ] **Step 1: Write the failing test**

Add to `internal/config/config_test.go`:

```go
// These are the exact valid test keys already used elsewhere in config_test.go.
const (
	tNSEC = "0000000000000000000000000000000000000000000000000000000000000001"
	tPUB  = "79be667ef9dcbbac55a06295ce870b07029bfcdb2dce28d959f2815b16f81798"
)

func TestAutoRetryDefaults(t *testing.T) {
	t.Setenv("NSEC_HEX", tNSEC)
	t.Setenv("OWNER_PUBKEY", tPUB)
	t.Setenv("BLOSSOM_URL", "https://b.example.com")
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !c.AutoRetryEnabled {
		t.Error("AutoRetryEnabled should default true")
	}
	if c.AutoRetryMaxAttempts != 6 {
		t.Errorf("AutoRetryMaxAttempts = %d, want 6", c.AutoRetryMaxAttempts)
	}
	if c.AutoRetryBaseDelay != 2*time.Minute {
		t.Errorf("AutoRetryBaseDelay = %v, want 2m", c.AutoRetryBaseDelay)
	}
	if c.AutoRetryMaxDelay != time.Hour {
		t.Errorf("AutoRetryMaxDelay = %v, want 1h", c.AutoRetryMaxDelay)
	}
	if c.RetrierTick != time.Minute {
		t.Errorf("RetrierTick = %v, want 1m", c.RetrierTick)
	}
}

func TestAutoRetryDisabled(t *testing.T) {
	t.Setenv("NSEC_HEX", tNSEC)
	t.Setenv("OWNER_PUBKEY", tPUB)
	t.Setenv("BLOSSOM_URL", "https://b.example.com")
	t.Setenv("AUTO_RETRY_ENABLED", "false")
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.AutoRetryEnabled {
		t.Error("AUTO_RETRY_ENABLED=false should disable")
	}
}
```

> NOTE: if `config_test.go` already declares constants for these keys, reuse those names instead of re-declaring `tNSEC`/`tPUB` to avoid a duplicate-declaration compile error. Also confirm `"time"` is imported in the test file.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/config/ -run TestAutoRetry -v`
Expected: FAIL — `c.AutoRetryEnabled undefined`.

- [ ] **Step 3: Add fields + parsing**

In `internal/config/config.go`, add to the `Config` struct (near `ScheduleGrace`):

```go
	AutoRetryEnabled     bool
	AutoRetryMaxAttempts int
	AutoRetryBaseDelay   time.Duration
	AutoRetryMaxDelay    time.Duration
	RetrierTick          time.Duration
```

Add a `getBool` helper near `getEnv`:

```go
func getBool(k string, d bool) bool {
	v := os.Getenv(k)
	if v == "" {
		return d
	}
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	default:
		return d
	}
}
```

In `Load()`, after the `ScheduleGrace` block, add:

```go
	c.AutoRetryEnabled = getBool("AUTO_RETRY_ENABLED", true)
	if c.AutoRetryMaxAttempts, err = strconv.Atoi(getEnv("AUTO_RETRY_MAX_ATTEMPTS", "6")); err != nil {
		return c, fmt.Errorf("AUTO_RETRY_MAX_ATTEMPTS: %w", err)
	}
	if c.AutoRetryBaseDelay, err = time.ParseDuration(getEnv("AUTO_RETRY_BASE_DELAY", "2m")); err != nil {
		return c, fmt.Errorf("AUTO_RETRY_BASE_DELAY: %w", err)
	}
	if c.AutoRetryMaxDelay, err = time.ParseDuration(getEnv("AUTO_RETRY_MAX_DELAY", "1h")); err != nil {
		return c, fmt.Errorf("AUTO_RETRY_MAX_DELAY: %w", err)
	}
	if c.RetrierTick, err = time.ParseDuration(getEnv("RETRIER_TICK", "60s")); err != nil {
		return c, fmt.Errorf("RETRIER_TICK: %w", err)
	}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/config/ -run TestAutoRetry -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/config/config.go internal/config/config_test.go
git commit -m "config: AUTO_RETRY_* and RETRIER_TICK settings (auto-retry on by default)"
```

---

## Task 3: Store migrations + model fields

**Files:**
- Modify: `internal/store/store.go` (migrate)
- Modify: `internal/store/models.go` (struct fields + GetPost load)
- Test: `internal/store/retry_test.go`

- [ ] **Step 1: Write the failing test**

Add to `internal/store/retry_test.go`:

```go
func TestGaveUpColumnsAndLastAttemptLoad(t *testing.T) {
	st := openTestStore(t) // existing same-package helper (store_test.go-style)
	p := &Post{
		ID: "p-gaveup", CreatedAt: time.Now().UTC(), MasterText: "x",
		Platforms: []string{"bluesky"}, Source: "test", Status: "failed",
		Targets: []Target{{Platform: "bluesky", Status: "failed"}},
	}
	if err := st.SavePost(p); err != nil {
		t.Fatalf("SavePost: %v", err)
	}
	// Record an attempt so last_attempt_at is set.
	got, _ := st.GetPost("p-gaveup")
	tid := got.Targets[0].ID
	if err := st.AppendTargetAttempt(tid, "failed", "boom", "", "", 5, "", "", nil, ""); err != nil {
		t.Fatalf("AppendTargetAttempt: %v", err)
	}
	got, err := st.GetPost("p-gaveup")
	if err != nil {
		t.Fatalf("GetPost: %v", err)
	}
	if got.Targets[0].LastAttempt.IsZero() {
		t.Error("LastAttempt should be loaded, got zero")
	}
	if got.Targets[0].GaveUpAt != nil {
		t.Error("GaveUpAt should be nil initially")
	}
}
```

> NOTE: `openTestStore(t) *Store` already exists in this package's test files; it wraps `Open(filepath.Join(t.TempDir(), ...))`. Reuse it verbatim.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/store/ -run TestGaveUpColumnsAndLastAttemptLoad -v`
Expected: FAIL — `got.Targets[0].GaveUpAt undefined` and/or `LastAttempt` zero.

- [ ] **Step 3: Add migrations**

In `internal/store/store.go`, in `migrate()` before the final `return`, add:

```go
	if err := s.addColumnIfMissing("post_targets", "gave_up_at", "TIMESTAMP"); err != nil {
		return err
	}
	if err := s.addColumnIfMissing("target_relays", "gave_up_at", "TIMESTAMP"); err != nil {
		return err
	}
	if err := s.addColumnIfMissing("target_relays", "retry_count", "INTEGER NOT NULL DEFAULT 0"); err != nil {
		return err
	}
```

- [ ] **Step 4: Add struct fields**

In `internal/store/models.go`, add to `Target` (after `LastAttempt`):

```go
	GaveUpAt *time.Time `json:"gave_up_at,omitempty"`
```

Add to `RelayState`:

```go
	GaveUpAt   *time.Time `json:"gave_up_at,omitempty"`
	RetryCount int        `json:"retry_count,omitempty"`
```

- [ ] **Step 5: Load the new columns in GetPost**

In `internal/store/models.go`, the targets query in `GetPost` (currently selecting `...attempt_count,signed_event_json,segments_json`) must also load `last_attempt_at` and `gave_up_at`. Change the SELECT to:

```go
	trows, err := s.sql.Query(`SELECT id,platform,final_text,fields_json,status,remote_id,remote_url,latency_ms,attempt_count,last_attempt_at,gave_up_at,signed_event_json,segments_json FROM post_targets WHERE post_id=? ORDER BY id`, id)
```

Update the scan (add two `sql.NullTime` locals `la, gu` before the scan, and place them in column order):

```go
		var la, gu sql.NullTime
		if err := trows.Scan(&tg.ID, &tg.Platform, &tg.FinalText, &fields, &tg.Status, &rid, &rurl, &tg.LatencyMS, &tg.AttemptCount, &la, &gu, &sej, &segs); err != nil {
			return nil, err
		}
		if la.Valid {
			tg.LastAttempt = la.Time.UTC()
		}
		if gu.Valid {
			t := gu.Time.UTC()
			tg.GaveUpAt = &t
		}
```

For the relay query in `GetPost`, change the SELECT to include the new columns:

```go
		rrows, err := s.sql.Query(`SELECT relay_url,status,message,gave_up_at,retry_count FROM target_relays WHERE target_id=? ORDER BY id`, p.Targets[i].ID)
```

Update the relay scan:

```go
			var rs RelayState
			var msg sql.NullString
			var rgu sql.NullTime
			if err := rrows.Scan(&rs.URL, &rs.Status, &msg, &rgu, &rs.RetryCount); err != nil {
				rrows.Close()
				return nil, err
			}
			rs.Message = msg.String
			if rgu.Valid {
				t := rgu.Time.UTC()
				rs.GaveUpAt = &t
			}
```

- [ ] **Step 6: Run test to verify it passes**

Run: `go test ./internal/store/ -run TestGaveUpColumnsAndLastAttemptLoad -v`
Expected: PASS.

- [ ] **Step 7: Run the full store package to catch scan-order regressions**

Run: `go test ./internal/store/ -v`
Expected: PASS (all existing tests still green).

- [ ] **Step 8: Commit**

```bash
git add internal/store/store.go internal/store/models.go internal/store/retry_test.go
git commit -m "store: gave_up_at + relay retry_count columns; load last_attempt_at in GetPost"
```

---

## Task 4: Store — PostsNeedingRetry + give-up markers

**Files:**
- Modify: `internal/store/models.go`
- Test: `internal/store/retry_test.go`

- [ ] **Step 1: Write the failing test**

Add to `internal/store/retry_test.go`:

```go
func TestPostsNeedingRetryAndMarkGaveUp(t *testing.T) {
	st := openTestStore(t)
	mk := func(id, status string) {
		p := &Post{ID: id, CreatedAt: time.Now().UTC(), MasterText: "x",
			Platforms: []string{"bluesky"}, Source: "test", Status: status,
			Targets: []Target{{Platform: "bluesky", Status: status}}}
		if err := st.SavePost(p); err != nil {
			t.Fatalf("SavePost %s: %v", id, err)
		}
	}
	mk("p-failed", "failed")
	mk("p-partial", "partial")
	mk("p-success", "success")
	mk("p-missed", "missed")

	ids, err := st.PostsNeedingRetry()
	if err != nil {
		t.Fatalf("PostsNeedingRetry: %v", err)
	}
	set := map[string]bool{}
	for _, id := range ids {
		set[id] = true
	}
	if !set["p-failed"] || !set["p-partial"] {
		t.Errorf("failed/partial should be candidates: %v", ids)
	}
	if set["p-success"] || set["p-missed"] {
		t.Errorf("success/missed must not be candidates: %v", ids)
	}

	// Mark the failed target given up → it drops out of the candidate set.
	gp, _ := st.GetPost("p-failed")
	tid := gp.Targets[0].ID
	at := time.Now().UTC()
	set1, err := st.MarkTargetGaveUp(tid, at)
	if err != nil {
		t.Fatalf("MarkTargetGaveUp: %v", err)
	}
	if !set1 {
		t.Error("first MarkTargetGaveUp should report it set the flag")
	}
	set2, _ := st.MarkTargetGaveUp(tid, at) // idempotent guard
	if set2 {
		t.Error("second MarkTargetGaveUp should report no-op (already set)")
	}
	ids, _ = st.PostsNeedingRetry()
	for _, id := range ids {
		if id == "p-failed" {
			t.Error("given-up post should no longer be a candidate")
		}
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/store/ -run TestPostsNeedingRetryAndMarkGaveUp -v`
Expected: FAIL — `st.PostsNeedingRetry undefined`.

- [ ] **Step 3: Implement the store methods**

Add to `internal/store/models.go`:

```go
// PostsNeedingRetry returns IDs of posts that have at least one target in a
// recoverable state (failed or partial) that has not yet given up. 'missed'
// (scheduling miss) and 'success' are excluded. The caller (Retrier) loads
// each post and applies backoff/cap per target.
func (s *Store) PostsNeedingRetry() ([]string, error) {
	rows, err := s.sql.Query(
		`SELECT DISTINCT post_id FROM post_targets
		  WHERE status IN ('failed','partial') AND gave_up_at IS NULL`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// MarkTargetGaveUp stamps gave_up_at on a platform target, but only if it is
// still NULL. Returns true iff this call set it (so the caller alerts once).
func (s *Store) MarkTargetGaveUp(targetID int64, at time.Time) (bool, error) {
	res, err := s.sql.Exec(
		`UPDATE post_targets SET gave_up_at=? WHERE id=? AND gave_up_at IS NULL`,
		at.UTC(), targetID)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

// MarkRelayGaveUp stamps gave_up_at on a single relay row, only if still NULL.
// Returns true iff this call set it.
func (s *Store) MarkRelayGaveUp(targetID int64, relayURL string, at time.Time) (bool, error) {
	res, err := s.sql.Exec(
		`UPDATE target_relays SET gave_up_at=? WHERE target_id=? AND relay_url=? AND gave_up_at IS NULL`,
		at.UTC(), targetID, relayURL)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/store/ -run TestPostsNeedingRetryAndMarkGaveUp -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/store/models.go internal/store/retry_test.go
git commit -m "store: PostsNeedingRetry + MarkTargetGaveUp/MarkRelayGaveUp"
```

---

## Task 5: Store — clear gave_up on success + bump relay retry_count

**Files:**
- Modify: `internal/store/models.go` (`AppendTargetAttempt`, `UpdateTargetSegments`, `UpdateRelayStatus`)
- Test: `internal/store/retry_test.go`

- [ ] **Step 1: Write the failing test**

Add to `internal/store/retry_test.go`:

```go
func TestSuccessClearsGaveUpAndRelayRetryCount(t *testing.T) {
	st := openTestStore(t)
	p := &Post{ID: "p-clear", CreatedAt: time.Now().UTC(), MasterText: "x",
		Platforms: []string{"bluesky"}, Source: "test", Status: "failed",
		Targets: []Target{{Platform: "bluesky", Status: "failed"}}}
	if err := st.SavePost(p); err != nil {
		t.Fatalf("SavePost: %v", err)
	}
	gp, _ := st.GetPost("p-clear")
	tid := gp.Targets[0].ID
	if _, err := st.MarkTargetGaveUp(tid, time.Now().UTC()); err != nil {
		t.Fatalf("MarkTargetGaveUp: %v", err)
	}
	// A successful later attempt clears the give-up flag.
	if err := st.AppendTargetAttempt(tid, "success", "", "rid", "https://x/1", 10, "", "", nil, ""); err != nil {
		t.Fatalf("AppendTargetAttempt: %v", err)
	}
	gp, _ = st.GetPost("p-clear")
	if gp.Targets[0].GaveUpAt != nil {
		t.Error("a successful attempt must clear gave_up_at")
	}

	// Relay retry_count bumps on UpdateRelayStatus.
	if err := st.AppendTargetAttempt(tid, "failed", "x", "", "", 1, "", "",
		[]RelayState{{URL: "wss://r1", Status: "failed"}}, "{}"); err != nil {
		t.Fatalf("seed relay: %v", err)
	}
	if err := st.UpdateRelayStatus(tid, "wss://r1", "failed", "still down"); err != nil {
		t.Fatalf("UpdateRelayStatus: %v", err)
	}
	gp, _ = st.GetPost("p-clear")
	var rc int
	for _, r := range gp.Targets[0].Relays {
		if r.URL == "wss://r1" {
			rc = r.RetryCount
		}
	}
	if rc != 1 {
		t.Errorf("relay retry_count = %d, want 1 after one UpdateRelayStatus", rc)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/store/ -run TestSuccessClearsGaveUpAndRelayRetryCount -v`
Expected: FAIL — `gave_up_at` not cleared / `retry_count` stays 0.

- [ ] **Step 3: Edit the three write methods**

In `AppendTargetAttempt`, change the `UPDATE post_targets SET ...` so it clears `gave_up_at` on success. Replace that statement with a conditional expression that nulls the column when status is success and leaves it otherwise:

```go
	if _, err := tx.Exec(
		`UPDATE post_targets
		    SET status=?, remote_id=?, remote_url=?, latency_ms=?, attempt_count=?, last_attempt_at=?,
		        gave_up_at = CASE WHEN ?='success' THEN NULL ELSE gave_up_at END
		  WHERE id=?`,
		status, remoteID, remoteURL, latencyMS, n, now, status, targetID,
	); err != nil {
		return err
	}
```

In `UpdateTargetSegments`, make the same change to its `UPDATE post_targets SET ...` statement — append `, gave_up_at = CASE WHEN ?='success' THEN NULL ELSE gave_up_at END` to the SET list and add the extra `status` arg in the right position (immediately before the `WHERE id=?` arg).

In `UpdateRelayStatus`, change the relay UPDATE to bump `retry_count` and clear the relay's `gave_up_at` when it comes back `ok`:

```go
	if _, err := tx.Exec(
		`UPDATE target_relays
		    SET status=?, message=?, attempted_at=?, retry_count = retry_count + 1,
		        gave_up_at = CASE WHEN ?='ok' THEN NULL ELSE gave_up_at END
		  WHERE target_id=? AND relay_url=?`,
		status, message, time.Now().UTC(), status, targetID, relayURL,
	); err != nil {
		return err
	}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/store/ -run TestSuccessClearsGaveUpAndRelayRetryCount -v`
Expected: PASS.

- [ ] **Step 5: Run the full store package**

Run: `go test ./internal/store/ -v`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/store/models.go internal/store/retry_test.go
git commit -m "store: clear gave_up_at on success; bump relay retry_count on rebroadcast"
```

---

## Task 6: Store — attention filter + count

**Files:**
- Modify: `internal/store/models.go` (`statusClause`, new `AttentionCount`)
- Test: `internal/store/posts_filter_test.go` (or `retry_test.go`)

- [ ] **Step 1: Write the failing test**

Add to `internal/store/retry_test.go`:

```go
func TestAttentionFilterAndCount(t *testing.T) {
	st := openTestStore(t)
	mk := func(id, status string) {
		p := &Post{ID: id, CreatedAt: time.Now().UTC(), MasterText: "x",
			Platforms: []string{"bluesky"}, Source: "test", Status: status,
			Targets: []Target{{Platform: "bluesky", Status: status}}}
		if err := st.SavePost(p); err != nil {
			t.Fatalf("SavePost: %v", err)
		}
	}
	mk("a1", "failed")
	mk("a2", "partial")
	mk("a3", "success")
	mk("a4", "missed")

	got, err := st.ListPostsFiltered(PostFilter{Status: "attention", Limit: 50})
	if err != nil {
		t.Fatalf("ListPostsFiltered: %v", err)
	}
	if len(got) != 2 {
		t.Errorf("attention filter returned %d, want 2 (failed+partial)", len(got))
	}
	n, err := st.AttentionCount()
	if err != nil {
		t.Fatalf("AttentionCount: %v", err)
	}
	if n != 2 {
		t.Errorf("AttentionCount = %d, want 2", n)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/store/ -run TestAttentionFilterAndCount -v`
Expected: FAIL — `attention` not handled (returns all) and `AttentionCount` undefined.

- [ ] **Step 3: Implement**

In `statusClause`, add a case before `default`:

```go
	case "attention":
		return "p.status IN ('failed','partial')"
```

Add `AttentionCount` to `internal/store/models.go`:

```go
// AttentionCount returns the number of non-hidden posts needing attention
// (delivery failed or partial). Drives the History "attention" badge.
func (s *Store) AttentionCount() (int, error) {
	var n int
	err := s.sql.QueryRow(
		`SELECT COUNT(*) FROM posts WHERE hidden=0 AND status IN ('failed','partial')`).Scan(&n)
	return n, err
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/store/ -run TestAttentionFilterAndCount -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/store/models.go internal/store/retry_test.go
git commit -m "store: status=attention filter + AttentionCount"
```

---

## Task 7: Retrier engine — platform-level loop

**Files:**
- Modify: `internal/dispatch/retrier.go`
- Test: `internal/dispatch/retrier_test.go`

This task uses a fake clock and a fake retry-target. Reuse the dispatch test helpers if present; otherwise the test below defines its own minimal store via the real `*store.Store` against an in-memory DB.

- [ ] **Step 1: Write the failing test**

Add to `internal/dispatch/retrier_test.go`:

```go
import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/geofox/publisher/internal/store" // module path confirmed: github.com/geofox/publisher
)

// openDispatchStore opens a real temp-file store for dispatch-package tests
// (the store package has no in-memory constructor; external packages use Open).
func openDispatchStore(t *testing.T) *store.Store {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// fakeAlerter records Alert calls.
type fakeAlerter struct{ calls []string }

func (f *fakeAlerter) Alert(_ context.Context, summary, _ string) error {
	f.calls = append(f.calls, summary)
	return nil
}

func TestRetrierRetriesDueFailedTarget(t *testing.T) {
	st := openDispatchStore(t)
	// A failed bluesky target, one attempt made 10 minutes ago → due (base 2m).
	p := &store.Post{ID: "r1", CreatedAt: time.Now().UTC(), MasterText: "hi",
		Platforms: []string{"bluesky"}, Source: "test", Status: "failed",
		Targets: []store.Target{{Platform: "bluesky", Status: "failed"}}}
	if err := st.SavePost(p); err != nil {
		t.Fatalf("SavePost: %v", err)
	}
	gp, _ := st.GetPost("r1")
	tid := gp.Targets[0].ID
	_ = st.AppendTargetAttempt(tid, "failed", "boom", "", "", 1, "", "", nil, "")

	d := &Dispatcher{Store: st} // no platform adapters → runPlatform returns failed
	now := time.Now().UTC().Add(10 * time.Minute)
	r := &Retrier{disp: d, notifier: &fakeAlerter{}, enabled: true,
		maxAttempts: 6, base: 2 * time.Minute, max: time.Hour,
		now: func() time.Time { return now }}

	r.runDue(context.Background())

	gp, _ = st.GetPost("r1")
	if gp.Targets[0].AttemptCount < 2 {
		t.Errorf("expected a retry attempt (count>=2), got %d", gp.Targets[0].AttemptCount)
	}
}

func TestRetrierSkipsNotDue(t *testing.T) {
	st := openDispatchStore(t)
	p := &store.Post{ID: "r2", CreatedAt: time.Now().UTC(), MasterText: "hi",
		Platforms: []string{"bluesky"}, Source: "test", Status: "failed",
		Targets: []store.Target{{Platform: "bluesky", Status: "failed"}}}
	_ = st.SavePost(p)
	gp, _ := st.GetPost("r2")
	tid := gp.Targets[0].ID
	_ = st.AppendTargetAttempt(tid, "failed", "boom", "", "", 1, "", "", nil, "")

	d := &Dispatcher{Store: st}
	now := time.Now().UTC().Add(30 * time.Second) // < base 2m → not due
	r := &Retrier{disp: d, notifier: &fakeAlerter{}, enabled: true,
		maxAttempts: 6, base: 2 * time.Minute, max: time.Hour,
		now: func() time.Time { return now }}
	r.runDue(context.Background())
	gp, _ = st.GetPost("r2")
	if gp.Targets[0].AttemptCount != 1 {
		t.Errorf("not-due target must not be retried, count=%d", gp.Targets[0].AttemptCount)
	}
}
```

> NOTE: `openDispatchStore(t)` is defined once at the top of `retrier_test.go` (Step 1 import block above) and used by every dispatch test in this plan. It opens a real temp-file DB via `store.Open` — the store package has no in-memory constructor.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/dispatch/ -run TestRetrier -v`
Expected: FAIL — `Retrier`/fields undefined.

- [ ] **Step 3: Implement the Retrier core**

Append to `internal/dispatch/retrier.go`:

```go
import (
	"context"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/geofox/publisher/internal/store" // confirm module path
)

// Retrier re-drives failed/partial deliveries with exponential backoff and
// gives up after a cap. It reuses Dispatcher.Retry / RetryRelay. A single
// in-process actor; the running guard prevents overlapping ticks.
type Retrier struct {
	disp        *Dispatcher
	notifier    Notifier // reuses the scheduler's Notifier interface (Alert)
	enabled     bool
	maxAttempts int
	base, max   time.Duration
	tick        time.Duration
	now         func() time.Time
	running     atomic.Bool
}

func NewRetrier(d *Dispatcher, n Notifier, enabled bool, maxAttempts int, base, max, tick time.Duration) *Retrier {
	return &Retrier{disp: d, notifier: n, enabled: enabled, maxAttempts: maxAttempts,
		base: base, max: max, tick: tick, now: time.Now}
}

// Start ticks until ctx is done. No-op (returns immediately) when disabled.
func (r *Retrier) Start(ctx context.Context) {
	if !r.enabled {
		slog.Info("retrier disabled (AUTO_RETRY_ENABLED=false)")
		return
	}
	t := time.NewTicker(r.tick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			r.runDue(ctx)
		}
	}
}

// runDue processes one pass. The running guard drops a tick if the previous
// pass is still in flight (a slow network retry must not let ticks pile up).
func (r *Retrier) runDue(ctx context.Context) {
	if !r.running.CompareAndSwap(false, true) {
		return
	}
	defer r.running.Store(false)

	ids, err := r.disp.Store.PostsNeedingRetry()
	if err != nil {
		slog.Error("retrier: candidate query failed", "err", err)
		return
	}
	for _, id := range ids {
		r.processPost(ctx, id)
	}
}

// retryablePlatform mirrors Dispatcher.Retry's predicate minus 'missed':
// failed targets (any platform) and threaded partials. Nostr single-post
// partials are handled at the relay level (Task 9), not here.
func retryablePlatform(t store.Target) bool {
	return t.Status == "failed" || (t.Status == "partial" && len(t.Segments) > 1)
}

func (r *Retrier) targetDue(t store.Target, now time.Time) bool {
	if t.LastAttempt.IsZero() {
		return true
	}
	return !now.Before(t.LastAttempt.Add(backoff(t.AttemptCount, r.base, r.max)))
}

func (r *Retrier) processPost(ctx context.Context, id string) {
	post, err := r.disp.Store.GetPost(id)
	if err != nil {
		slog.Error("retrier: load failed", "post_id", id, "err", err)
		return
	}
	now := r.now()
	var due []string          // platforms to retry this pass
	var exhausted []string    // platforms that just hit the cap
	for _, t := range post.Targets {
		if !retryablePlatform(t) || t.GaveUpAt != nil {
			continue
		}
		if t.AttemptCount >= r.maxAttempts {
			if set, err := r.disp.Store.MarkTargetGaveUp(t.ID, now); err == nil && set {
				exhausted = append(exhausted, t.Platform)
			}
			continue
		}
		if r.targetDue(t, now) {
			due = append(due, t.Platform)
		}
	}
	if len(due) > 0 {
		if _, err := r.disp.Retry(ctx, id, due); err != nil {
			slog.Error("retrier: retry failed", "post_id", id, "platforms", due, "err", err)
		} else {
			slog.Info("retrier: re-drove targets", "post_id", id, "platforms", due)
		}
	}
	if len(exhausted) > 0 {
		r.alertGaveUp(ctx, id, exhausted, nil)
	}
}

// alertGaveUp fires one operational alert summarizing newly given-up targets.
func (r *Retrier) alertGaveUp(ctx context.Context, postID string, platforms, relays []string) {
	parts := ""
	if len(platforms) > 0 {
		parts += "platforms " + joinComma(platforms)
	}
	if len(relays) > 0 {
		if parts != "" {
			parts += "; "
		}
		parts += joinComma(relays) + " relays"
	}
	body := "post " + postID + " gave up on " + parts + " after " +
		itoa(r.maxAttempts) + " attempts; manual retry still available"
	if err := r.notifier.Alert(ctx, "Publisher: auto-retry exhausted", body); err != nil {
		slog.Error("retrier: give-up alert failed", "post_id", postID, "err", err)
	}
}

func joinComma(ss []string) string {
	out := ""
	for i, s := range ss {
		if i > 0 {
			out += ", "
		}
		out += s
	}
	return out
}

func itoa(n int) string { return strconv.Itoa(n) }
```

Add `"strconv"` to the import block.

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/dispatch/ -run TestRetrier -v`
Expected: PASS (both retry-due and skip-not-due cases).

- [ ] **Step 5: Commit**

```bash
git add internal/dispatch/retrier.go internal/dispatch/retrier_test.go
git commit -m "dispatch: Retrier worker — platform-level auto-retry with backoff"
```

---

## Task 8: Retrier — platform-level give-up + alert-once

**Files:**
- Test: `internal/dispatch/retrier_test.go`

(The give-up logic is already implemented in Task 7; this task locks it under test.)

- [ ] **Step 1: Write the failing test**

Add to `internal/dispatch/retrier_test.go`:

```go
func TestRetrierGivesUpAndAlertsOnce(t *testing.T) {
	st := openDispatchStore(t)
	p := &store.Post{ID: "g1", CreatedAt: time.Now().UTC(), MasterText: "hi",
		Platforms: []string{"bluesky"}, Source: "test", Status: "failed",
		Targets: []store.Target{{Platform: "bluesky", Status: "failed"}}}
	_ = st.SavePost(p)
	gp, _ := st.GetPost("g1")
	tid := gp.Targets[0].ID
	// Drive attempt_count up to the cap (6) — all failures.
	for i := 0; i < 6; i++ {
		_ = st.AppendTargetAttempt(tid, "failed", "boom", "", "", 1, "", "", nil, "")
	}
	al := &fakeAlerter{}
	d := &Dispatcher{Store: st}
	now := time.Now().UTC().Add(2 * time.Hour) // well past any backoff
	r := &Retrier{disp: d, notifier: al, enabled: true, maxAttempts: 6,
		base: 2 * time.Minute, max: time.Hour, now: func() time.Time { return now }}

	r.runDue(context.Background()) // crosses the cap → gives up + alerts
	gp, _ = st.GetPost("g1")
	if gp.Targets[0].GaveUpAt == nil {
		t.Fatal("target should be marked given up at the cap")
	}
	if len(al.calls) != 1 {
		t.Fatalf("expected exactly 1 give-up alert, got %d", len(al.calls))
	}
	r.runDue(context.Background()) // second pass must not re-alert
	if len(al.calls) != 1 {
		t.Errorf("give-up alert must fire only once, got %d", len(al.calls))
	}
}
```

- [ ] **Step 2: Run test to verify it fails or passes**

Run: `go test ./internal/dispatch/ -run TestRetrierGivesUpAndAlertsOnce -v`
Expected: PASS (logic from Task 7). If it FAILS, fix `processPost`/`MarkTargetGaveUp` until green — do not weaken the test.

- [ ] **Step 3: Commit**

```bash
git add internal/dispatch/retrier_test.go
git commit -m "dispatch: test Retrier give-up + alert-once"
```

---

## Task 9: Retrier — Nostr relay-level auto-retry + give-up

**Files:**
- Modify: `internal/dispatch/retrier.go` (`processPost`)
- Test: `internal/dispatch/retrier_test.go`

- [ ] **Step 1: Write the failing test**

Add to `internal/dispatch/retrier_test.go`:

```go
func TestRetrierRelayLevelRetry(t *testing.T) {
	st := openDispatchStore(t)
	p := &store.Post{ID: "rl1", CreatedAt: time.Now().UTC(), MasterText: "hi",
		Platforms: []string{"nostr"}, Source: "test", Status: "partial",
		Targets: []store.Target{{Platform: "nostr", Status: "partial"}}}
	_ = st.SavePost(p)
	gp, _ := st.GetPost("rl1")
	tid := gp.Targets[0].ID
	// One relay ok, one failed → target partial; store a signed event so
	// RetryRelay has something to rebroadcast.
	_ = st.AppendTargetAttempt(tid, "partial", "", "", "", 1, "", "",
		[]store.RelayState{
			{URL: "wss://ok", Status: "ok"},
			{URL: "wss://down", Status: "failed"},
		}, `{"id":"abc"}`)

	// fakeNostr lets RetryRelay "succeed" without a network.
	d := &Dispatcher{Store: st, Nostr: fakeRebroadcaster{ok: true}}
	now := time.Now().UTC().Add(10 * time.Minute)
	r := &Retrier{disp: d, notifier: &fakeAlerter{}, enabled: true, maxAttempts: 6,
		base: 2 * time.Minute, max: time.Hour, now: func() time.Time { return now }}
	r.runDue(context.Background())

	gp, _ = st.GetPost("rl1")
	for _, rl := range gp.Targets[0].Relays {
		if rl.URL == "wss://down" && rl.Status != "ok" {
			t.Errorf("down relay should have been rebroadcast to ok, got %q", rl.Status)
		}
	}
}
```

Add a `fakeRebroadcaster` satisfying the `NostrPoster` interface's `RebroadcastToRelay(ctx, signedEventJSON, relayURL) (bool, string)` method. Check the exact `NostrPoster` interface in `internal/dispatch/adapters.go`/`dispatch.go` and implement only what compiles (embed the interface and override the one method if the interface is large):

```go
type fakeRebroadcaster struct {
	NostrPoster // embed; nil — only RebroadcastToRelay is called in this test
	ok          bool
}

func (f fakeRebroadcaster) RebroadcastToRelay(_ context.Context, _ , _ string) (bool, string) {
	return f.ok, ""
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/dispatch/ -run TestRetrierRelayLevelRetry -v`
Expected: FAIL — down relay still `failed` (relay logic not implemented).

- [ ] **Step 3: Extend processPost with relay-level handling**

In `processPost`, after the platform loop and before the `if len(due) > 0` block, add relay-level handling for Nostr partials. Add a `relayDue` helper and an `exhaustedRelays []string` accumulator:

```go
	var exhaustedRelays []string
	for ti := range post.Targets {
		t := post.Targets[ti]
		// Only single-post nostr partials use relay-level retry; threaded /
		// failed nostr targets are covered by the platform path above.
		if t.Platform != "nostr" || t.Status != "partial" || len(t.Segments) > 1 {
			continue
		}
		for _, rl := range t.Relays {
			if rl.Status != "failed" || rl.GaveUpAt != nil {
				continue
			}
			if rl.RetryCount >= r.maxAttempts {
				if set, err := r.disp.Store.MarkRelayGaveUp(t.ID, rl.URL, now); err == nil && set {
					exhaustedRelays = append(exhaustedRelays, rl.URL)
				}
				continue
			}
			if !r.relayDue(rl, now) {
				continue
			}
			if _, err := r.disp.RetryRelay(ctx, id, rl.URL); err != nil {
				slog.Error("retrier: relay retry failed", "post_id", id, "relay", rl.URL, "err", err)
			} else {
				slog.Info("retrier: rebroadcast to relay", "post_id", id, "relay", rl.URL)
			}
		}
	}
```

Change the give-up alert call at the end of `processPost` to include relays:

```go
	if len(exhausted) > 0 || len(exhaustedRelays) > 0 {
		r.alertGaveUp(ctx, id, exhausted, exhaustedRelays)
	}
```

Add the `relayDue` helper (relay backoff uses `RetryCount` + `attempted_at`; `attempted_at` is loaded into the relay row — if `RelayState` does not carry `attempted_at`, add it to the struct + `GetPost` relay scan the same way `gave_up_at` was added, and use it here):

```go
func (r *Retrier) relayDue(rl store.RelayState, now time.Time) bool {
	if rl.AttemptedAt.IsZero() {
		return true
	}
	return !now.Before(rl.AttemptedAt.Add(backoff(rl.RetryCount+1, r.base, r.max)))
}
```

> NOTE: `RelayState` currently has no `AttemptedAt` field and `GetPost` does not load it. Add `AttemptedAt time.Time` to `RelayState`, include `attempted_at` in the `GetPost` relay SELECT/scan (as a `sql.NullTime`), exactly mirroring the `gave_up_at` addition in Task 3. Make this the first edit of Step 3.

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/dispatch/ -run TestRetrierRelayLevelRetry -v`
Expected: PASS.

- [ ] **Step 5: Run the whole dispatch package**

Run: `go test ./internal/dispatch/ -v`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/dispatch/retrier.go internal/dispatch/retrier_test.go internal/store/models.go
git commit -m "dispatch: Retrier relay-level auto-retry + give-up; load relay attempted_at"
```

---

## Task 10: Immediate-post failure alert

**Files:**
- Modify: `internal/dispatch/dispatch.go` (`Dispatcher.Alerter` field + helper; call in `Post`, `Interact`, `Fire`)
- Test: `internal/dispatch/dispatch_test.go`

- [ ] **Step 1: Write the failing test**

Add to `internal/dispatch/dispatch_test.go` (or `notify_test.go`):

```go
func TestPostAlertsOnFailure(t *testing.T) {
	st := openDispatchStore(t)
	al := &fakeAlerter{} // defined in retrier_test.go (same package)
	d := &Dispatcher{Store: st, Alerter: al} // no adapters → bluesky post fails
	rec := d.Post(context.Background(), PostSpec{
		MasterText: "hello", Platforms: []string{"bluesky"},
	})
	if rec.Status == "success" {
		t.Fatal("expected a non-success post in this no-adapter test")
	}
	if len(al.calls) != 1 {
		t.Errorf("expected one immediate-failure alert, got %d", len(al.calls))
	}
}
```

> NOTE: Confirm `PostSpec`'s field names from `dispatch.go` and adjust the literal accordingly (e.g. it may require additional zero-value fields).

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/dispatch/ -run TestPostAlertsOnFailure -v`
Expected: FAIL — `Dispatcher.Alerter undefined`.

- [ ] **Step 3: Implement**

In `internal/dispatch/dispatch.go`, add to the `Dispatcher` struct:

```go
	Alerter  Notifier     // may be nil; alertFailure guards it
```

Add a helper near `notify`:

```go
// alertFailure fires an operational alert when a freshly recorded post did not
// fully succeed. No-op when no alerter is wired or the post fully succeeded.
func (d *Dispatcher) alertFailure(ctx context.Context, p *store.Post) {
	if d.Alerter == nil || p == nil {
		return
	}
	if p.Status == "failed" || p.Status == "partial" {
		body := "post " + p.ID + " finished with status " + p.Status + "; auto-retry will attempt recovery"
		if err := d.Alerter.Alert(ctx, "Publisher: post delivery "+p.Status, body); err != nil {
			slog.Error("alertFailure", "post_id", p.ID, "err", err)
		}
	}
}
```

Call `d.alertFailure(ctx, rec)` immediately after each existing `d.notify(ctx, rec)` in `Post` (line ~550) and `Interact` (line ~720), and after `d.notify(ctx, post)` in `Fire` (line ~855). Do **not** add it to `Retry`/`RetryRelay` (those have their own give-up alert path).

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/dispatch/ -run TestPostAlertsOnFailure -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/dispatch/dispatch.go internal/dispatch/dispatch_test.go
git commit -m "dispatch: alert on immediate post delivery failure/partial"
```

---

## Task 11: API — attention count endpoint

**Files:**
- Modify: `internal/api/api.go`
- Test: `internal/api/api_test.go` (or `list_filter_test.go`)

- [ ] **Step 1: Write the failing test**

Add to `internal/api/api_test.go`:

```go
func TestAttentionCountEndpoint(t *testing.T) {
	// Same setup pattern as list_filter_test.go: real temp store + &API{Store: db}.
	db, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mk := func(id, status string) {
		_ = db.SavePost(&store.Post{ID: id, CreatedAt: time.Now().UTC(),
			Platforms: []string{"bluesky"}, Source: "test", Status: status,
			Targets: []store.Target{{Platform: "bluesky", Status: status}}})
	}
	mk("c1", "failed")
	mk("c2", "partial")
	mk("c3", "success")

	a := &API{Store: db}
	req := httptest.NewRequest("GET", "/api/posts/attention/count", nil)
	w := httptest.NewRecorder()
	a.Routes().ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("status %d", w.Code)
	}
	var body struct {
		Count int `json:"count"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	if body.Count != 2 {
		t.Errorf("count = %d, want 2", body.Count)
	}
}
```

> NOTE: `list_filter_test.go` already imports `path/filepath`, `time`, `net/http/httptest`, `encoding/json`, and `store` — add this test to that file (or ensure those imports are present).

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/api/ -run TestAttentionCountEndpoint -v`
Expected: FAIL — route returns 404.

- [ ] **Step 3: Implement**

In `internal/api/api.go` `Routes()`, register near the other `/api/posts` routes:

```go
	mux.HandleFunc("GET /api/posts/attention/count", a.handleAttentionCount)
```

Add the handler:

```go
// ─── GET /api/posts/attention/count ─────────────────────────────────────────
func (a *API) handleAttentionCount(w http.ResponseWriter, r *http.Request) {
	n, err := a.Store.AttentionCount()
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, err.Error())
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]int{"count": n})
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/api/ -run TestAttentionCountEndpoint -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/api/api.go internal/api/api_test.go
git commit -m "api: GET /api/posts/attention/count for the attention badge"
```

---

## Task 12: Wire the Retrier in main.go

**Files:**
- Modify: `cmd/publisher/main.go`

- [ ] **Step 1: Wire the alerter into the Dispatcher**

In `cmd/publisher/main.go`, the `notifier := notify.NewWebhook(...)` line currently sits *after* the dispatcher `d` is constructed. Set the dispatcher's alerter after `notifier` exists (add immediately after the `notifier :=` line):

```go
	d.Alerter = notifier
```

- [ ] **Step 2: Start the Retrier next to the Scheduler**

Immediately after the existing `go dispatch.NewScheduler(d, notifier, cfg.ScheduleGrace).Start(context.Background())` line, add:

```go
	go dispatch.NewRetrier(d, notifier,
		cfg.AutoRetryEnabled, cfg.AutoRetryMaxAttempts,
		cfg.AutoRetryBaseDelay, cfg.AutoRetryMaxDelay, cfg.RetrierTick,
	).Start(context.Background())
```

- [ ] **Step 3: Build the whole project**

Run: `go build ./...`
Expected: builds clean.

- [ ] **Step 4: Run the full test suite**

Run: `go test ./...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add cmd/publisher/main.go
git commit -m "main: start the Retrier and wire the dispatcher alerter"
```

---

## Task 13: Web UI — attention chip + badge

**Files:**
- Modify: `internal/web/assets/index.html`
- Modify: `internal/web/assets/history.js`
- Modify: `internal/web/assets/app.css`

There is no JS test runner in this repo (web is verified by building + manual check). Verification is a build + a described manual check.

- [ ] **Step 1: Add the chip to the History filter bar**

In `internal/web/assets/index.html`, in the `#hseg` group, add an attention chip after the `failed` button:

```html
          <button            data-status="attention" type="button">attention <span class="attn-badge" hidden></span></button>
```

- [ ] **Step 2: Fetch + render the badge count in history.js**

In `internal/web/assets/history.js`, add a function and call it from `loadHistory` (after `renderList()`):

```js
async function refreshAttentionBadge() {
  const badge = document.querySelector("#hseg .attn-badge");
  if (!badge) return;
  try {
    const { count } = await api("/api/posts/attention/count");
    if (count > 0) { badge.textContent = String(count); badge.hidden = false; }
    else { badge.hidden = true; }
  } catch { badge.hidden = true; }
}
```

In `loadHistory`, after `renderList();`, add: `refreshAttentionBadge();`

- [ ] **Step 3: Style the badge**

In `internal/web/assets/app.css`, add (match the existing chip/badge palette in this file — find an existing `.badge`/pill rule and mirror its radius/colors):

```css
.attn-badge {
  display: inline-block;
  min-width: 1.2em;
  padding: 0 0.35em;
  margin-left: 0.3em;
  border-radius: 999px;
  background: var(--danger, #d9534f);
  color: #fff;
  font-size: 0.75em;
  line-height: 1.4;
  text-align: center;
}
```

- [ ] **Step 4: Build + manual verify**

Run: `go build ./... && go run ./cmd/publisher` (with a dev env; or rebuild the container).
Manual check: open the UI → History tab shows an **attention** chip; with a failed/partial post present, a red count badge appears; clicking the chip filters to those posts (the existing `#hseg` click handler already sets `hfilter` from `data-status` and reloads — `attention` flows straight through to `?status=attention`).

- [ ] **Step 5: Commit**

```bash
git add internal/web/assets/index.html internal/web/assets/history.js internal/web/assets/app.css
git commit -m "web: History attention filter chip + live count badge"
```

---

## Task 14: Web UI — per-target retry status line

**Files:**
- Modify: `internal/web/assets/history.js`

- [ ] **Step 1: Add a retry-status renderer**

In `internal/web/assets/history.js`, add a helper that renders a one-line status from a target's data:

```js
// retryStatusLine renders the auto-retry state for a target in the detail pane.
function retryStatusLine(t, maxAttempts = 6) {
  if (t.gave_up_at) {
    return el("div", { class: "retry-status gaveup",
      text: `⚠ gave up after ${t.attempt_count || maxAttempts} attempts` });
  }
  if ((t.status === "failed" || t.status === "partial") && (t.attempt_count || 0) < maxAttempts) {
    return el("div", { class: "retry-status pending",
      text: `↻ retrying (${t.attempt_count || 0}/${maxAttempts})` });
  }
  return null;
}
```

> NOTE: `maxAttempts` is the server default (6). It is display-only; if you want it exact, expose it via the existing identity/config endpoint later — out of scope here. Keep the literal 6.

- [ ] **Step 2: Render it in the target result row**

In `history.js`, find `resultRow(t)` (the function building a target's detail row — around line 90-112, returns the row with `tgBadge`/`relayBlock`). Append the retry status line when present. Where it currently returns `relays ? el("div", {class:"res-wrap"}, row, relays) : row;`, change to include the status line:

```js
  const rs = retryStatusLine(t);
  const children = [row];
  if (rs) children.push(rs);
  if (relays) children.push(relays);
  return children.length > 1 ? el("div", { class: "res-wrap" }, ...children) : row;
```

- [ ] **Step 3: Style (optional, in app.css)**

```css
.retry-status { font-size: 0.8em; margin-top: 0.2em; }
.retry-status.gaveup { color: var(--danger, #d9534f); }
.retry-status.pending { color: var(--muted, #888); }
```

- [ ] **Step 4: Build + manual verify**

Run: `go build ./...`
Manual check: a failed-but-not-given-up target shows `↻ retrying (n/6)`; a given-up target shows `⚠ gave up after N attempts`; the existing manual Retry button still renders and works.

- [ ] **Step 5: Commit**

```bash
git add internal/web/assets/history.js internal/web/assets/app.css
git commit -m "web: per-target auto-retry status line in history detail"
```

---

## Task 15: Documentation

**Files:**
- Modify: `README.md`

- [ ] **Step 1: Document env vars**

In the README configuration table, add rows (place after `SCHEDULE_GRACE`):

```markdown
| `AUTO_RETRY_ENABLED` | no | `true` | Master switch for the auto-retry worker (Retrier) |
| `AUTO_RETRY_MAX_ATTEMPTS` | no | `6` | Per-target attempt cap before giving up |
| `AUTO_RETRY_BASE_DELAY` | no | `2m` | First backoff interval |
| `AUTO_RETRY_MAX_DELAY` | no | `1h` | Backoff ceiling |
| `RETRIER_TICK` | no | `60s` | Retrier loop cadence |
```

- [ ] **Step 2: Document behavior + endpoint**

Add a short subsection near the scheduled-posting / retry docs:

```markdown
### Automatic failure recovery

Failed and partial deliveries are re-driven automatically by a background
Retrier with exponential backoff (`AUTO_RETRY_BASE_DELAY` × 2 per attempt,
capped at `AUTO_RETRY_MAX_DELAY`), up to `AUTO_RETRY_MAX_ATTEMPTS` per target.
Nostr partials retry at the per-relay level (rebroadcasting the same signed
event, no duplicate note); other platforms retry the whole target. `missed`
scheduled posts are **not** auto-retried (a scheduling miss, not a delivery
failure) — retry them manually.

After the cap, a target/relay is marked **given up** and an operational alert
fires once (via `ALERT_WEBHOOK_URL`). Immediate-post failures also alert. The
History tab gains an **attention** filter (delivery failed/partial) with a live
count from `GET /api/posts/attention/count`; manual retry remains available on
any target, including given-up ones.
```

- [ ] **Step 3: Commit**

```bash
git add README.md
git commit -m "docs: document automatic failure recovery + AUTO_RETRY_* settings"
```

---

## Final verification

- [ ] **Run the whole suite:** `go test ./...` — all green.
- [ ] **Build:** `go build ./...` — clean.
- [ ] **Vet:** `go vet ./...` — clean.
- [ ] Confirm `git log --oneline` shows the task commits in order.
