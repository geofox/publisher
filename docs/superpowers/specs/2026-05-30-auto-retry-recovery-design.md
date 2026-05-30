# Automated failure recovery (Retrier) + observability

**Status:** Approved design — ready for implementation plan
**Date:** 2026-05-30
**Area:** Reliability & ops — failures & retries

## Problem

The publisher tracks failures in fine detail — `posts` roll up to a `status`
(`success` / `partial` / `failed` / `missed`), each `post_target` records an
`attempt_count` and a full `target_attempts` history, and Nostr targets track
per-relay accept/reject in `target_relays`. But recovery is **entirely manual
and pull-based**: `POST /api/posts/{id}/retry` and `/relay-retry` exist, yet
nothing re-drives a failure on its own. The only background actor — the 30s
`Scheduler` — only fires *scheduled* posts and only alerts on *scheduled*
failures. A relay rejection or a platform error on a normal post sits broken
until a human notices and clicks retry.

This feature adds **automated self-healing** of failed deliveries plus **loud
surfacing** of what's still broken, so failures recover without intervention
and the operator learns about anything that can't.

## Goals

- A background worker that re-drives failed/partial targets with exponential
  backoff until they succeed or hit an attempt cap.
- Give up cleanly after the cap instead of hammering a permanently-broken
  target.
- Surface failures that need attention (a filterable view + a badge count).
- Alert on immediate-post failures (not just scheduled) and on retry
  exhaustion — both deduplicated.

## Non-goals (YAGNI)

- Per-post retry toggles.
- Configurable per-platform retry policies.
- A generalized "due work" job queue unifying scheduling + retries.
- Auto-retrying `missed` scheduled posts (a scheduling miss, not a delivery
  failure — stays manual).

## Architecture

A new background actor, `Retrier`, lives in `internal/dispatch` next to
`Scheduler` and is started alongside it from `cmd/publisher/main.go`. It
mirrors the Scheduler's proven shape — `tick → find eligible → atomic claim →
act → alert` — with an injectable `now func() time.Time` for deterministic
tests. Recovery work itself is **not** reimplemented: the Retrier calls the
existing `Dispatcher.Retry` / `Dispatcher.RetryRelay`, which already encode the
correct semantics.

Keeping the Retrier separate from the Scheduler is deliberate: each background
actor stays single-purpose (the Scheduler owns *future* work, the Retrier owns
*recovery*), matching the codebase's package-boundary discipline and keeping
each unit small enough to test in isolation.

### Per-tick loop (default every 60s)

1. `store.PostsNeedingRetry(now)` returns IDs of posts with at least one
   target that is **eligible, due, and not given up**.
2. For each post, atomically **claim** it (same guard pattern as
   `ClaimScheduled`) so overlapping ticks or a slow retry can't double-fire.
3. Route recovery exactly as manual retry does:
   - platform-level `failed` or threaded `partial` (`len(Segments) > 1`) →
     `Dispatcher.Retry`
   - Nostr single-post `partial` with down relays → `Dispatcher.RetryRelay`
     per failed relay (avoids duplicating the already-delivered note)
4. If a target's attempts are now exhausted, mark it given up and alert
   (see Alerting).

### Eligibility

| Target state | Auto-retry? | Rationale |
|---|---|---|
| `failed` | ✅ | Delivery failure worth healing |
| `partial` (threaded) | ✅ | Resume not-yet-sent segments via `Retry` |
| `partial` (Nostr relay-level) | ✅ | Re-drive down relays via `RetryRelay` |
| `missed` | ❌ | Scheduling miss, not delivery failure — stays manual |
| `success` | ❌ | Nothing to do |

### Backoff & give-up

- Next eligible time is **computed**, not stored:
  `next_eligible = last_attempt_at + backoff(attempt_count)`.
- `backoff` is exponential with jitter: `AUTO_RETRY_BASE_DELAY` (default `2m`),
  ×2 per attempt, clamped to `AUTO_RETRY_MAX_DELAY` (default `1h`).
- After `AUTO_RETRY_MAX_ATTEMPTS` (default `6`) the target is marked **given
  up** and stops auto-retrying.
- Manual retry still works on a given-up target: it clears `gave_up_at` and
  resets the cycle.

## Data model

Two new nullable columns, added via forward-only `ALTER TABLE … ADD COLUMN`
migrations in `store.go` (consistent with current schema evolution):

- `post_targets.gave_up_at TIMESTAMP NULL` — set when a platform target
  exhausts auto-retries.
- `target_relays.gave_up_at TIMESTAMP NULL` — same, for Nostr relay-level
  give-up.

`gave_up_at` is the only new persisted state. It serves three purposes at once:
the "stop auto-retrying" flag, the "alert exactly once" guard, and the UI's
"this one is dead" signal. Backoff timing needs no schedule columns because
`attempt_count` + `attempted_at` already exist. Both columns serialize into the
existing target/relay-status structs so the API carries them to the UI with no
new payload shape.

## API surface

- `GET /api/posts?status=attention` — synthetic filter value returning posts
  that are `partial`, `failed`, or have any given-up target. Existing
  `?status=partial|failed|missed|success` values are unchanged.
- `GET /api/posts/attention/count` → `{ "count": N }` — a cheap badge endpoint
  so the UI needn't fetch a full list to show a number.
- Existing `POST /api/posts/{id}/retry` and `/relay-retry` are unchanged;
  manual retry of a given-up target clears its `gave_up_at`.

## Web UI (History tab)

- An **Attention** filter chip beside the existing status filters, showing the
  live count from `/api/posts/attention/count` as a badge. Count `0` → no badge.
- In a post's detail pane, each target/relay gains a one-line status indicator
  driven by data already present:
  - still healing → `↻ retrying (3/6) · next ~4m` (from `attempt_count` +
    computed backoff)
  - given up → `⚠ gave up after 6 attempts` (from `gave_up_at`)
- The existing manual **Retry** button stays; using it on a given-up target
  clears `gave_up_at` and restarts the auto cycle.

## Alerting

Reuse the existing `notify.Webhook.Alert(ctx, summary, body)` already wired
into the Scheduler. No-op when `ALERT_WEBHOOK_URL` is unset, as today. Two new
triggers, both deduplicated:

1. **Retry exhaustion** — when a post's last eligible target gives up, fire
   **one** alert per post summarizing the dead targets (e.g. *"Post abc123:
   gave up on bluesky, 2 relays after 6 attempts"*). The `gave_up_at` flag
   guarantees once-only: a later tick that finds an already-flagged target does
   not re-alert.
2. **Immediate-post failure** — a post created via `/api/post` that returns
   `failed` / `partial` now alerts once (today only scheduled failures alert).
   Auto-retry then attempts to heal it; the exhaustion alert (#1) fires only if
   it ultimately cannot.

## Configuration (env vars, all optional)

| Var | Default | Purpose |
|---|---|---|
| `AUTO_RETRY_ENABLED` | `true` | Master switch for the Retrier |
| `AUTO_RETRY_MAX_ATTEMPTS` | `6` | Per-target attempt cap before give-up |
| `AUTO_RETRY_BASE_DELAY` | `2m` | First backoff interval |
| `AUTO_RETRY_MAX_DELAY` | `1h` | Backoff ceiling |
| `RETRIER_TICK` | `60s` | Worker loop cadence |

Default-on is intentional: it is the requested reliability behavior. Note the
upgrade effect — on first run after deploy, pre-existing `failed` / `partial`
posts in the archive become auto-retry-eligible and will be re-driven.

## Testing

- **Backoff math** — interval sequence, cap, and jitter bounds.
- **Eligibility/selection** — `missed` excluded, `success` excluded,
  due-vs-not-due, given-up excluded; platform-vs-relay routing picks the right
  path. Extends `dispatch/retry_test.go` and `store/retry_test.go`.
- **Give-up + alert-once** — exhaustion sets `gave_up_at`; the exhaustion alert
  fires exactly once across repeated ticks.
- **Loop with a fake clock** — an injected `now func()` drives the full cycle
  deterministically (same technique as the Scheduler tests).
- **Migration** — the `gave_up_at` ALTER is idempotent on an existing DB.

## Affected packages

- `internal/dispatch` — new `Retrier`; reuses `Retry` / `RetryRelay`.
- `internal/store` — `gave_up_at` columns + migration, `PostsNeedingRetry`,
  retry-claim, `status=attention` filter, attention count.
- `internal/api` — `status=attention`, `/api/posts/attention/count`.
- `internal/notify` — immediate-failure + exhaustion alert callers (the
  `Alert` method itself is unchanged).
- `internal/config` — five new env vars.
- `internal/web` — Attention chip + badge + per-target retry status line.
- `cmd/publisher/main.go` — construct and start the `Retrier`.
- `README.md` — document the new env vars, endpoints, and behavior.
