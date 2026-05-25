# Auto-Threading — Plan B2: Orchestration (chain dispatch + resume + UI)

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Turn the B1 reply primitives into real threaded posting — split a draft per platform, post the segments as a sequential reply-chain, stop-&-resume on failure, and render chains in history.

**Architecture:** A `ReplyRef` flows through the adapter layer (interfaces gain a `replyTo` param; `TargetResult` gains `CID`). `Dispatcher.Post` posts each platform's chain via a new `runChain` (split → sequential `runPlatform` calls, threading each segment to the previous; media on the head only; single-segment ⇒ today's exact behavior). Partial chains resume from the first non-success segment via `resumeChain`, persisted with a new `store.UpdateTargetSegments`. The `number` toggle plumbs through `/api/post`; history renders chains with a resume action.

**Tech Stack:** Go 1.26; the `internal/thread` splitter (Plan A) and the B1 reply primitives; vanilla-JS SPA.

**Spec:** `docs/superpowers/specs/2026-05-25-auto-threading-composer-design.md` (§3 sequencing, §4 dispatch/resume, §5 failure, §6 API, §7 history).

**Builds on B1 (already merged on this branch):** `store.Segment` + `Target.Segments`; `bluesky.Post.Reply *bluesky.ReplyRef{RootURI,RootCID,ParentURI,ParentCID}`; `mastodon.Post.InReplyToID`; `nostr.PublishInput.ReplyTo *nostr.NostrReply{RootID,ParentID,RelayHint}`; `threads.Post.ReplyToID`; `bluesky.Result.CID`.

---

## File Structure

| File | Responsibility (B2) |
|---|---|
| `internal/dispatch/dispatch.go` | `ReplyRef`, `TargetResult.CID`, adapter interface `replyTo` param, `runPlatform` param, `runChain`/`chainOutcome`/`chainStatus`, `PostSpec.Number`, rewired `Post`, `resumeChain` + `dispatchTargets`/`Retry` resume |
| `internal/dispatch/adapters.go` | 4 adapters accept `replyTo` → client reply field; Bluesky sets `CID` |
| `internal/dispatch/chain_test.go` | fake posters; chain + resume orchestration tests |
| `internal/store/store.go` | `UpdateTargetSegments` |
| `internal/store/models_test.go` | UpdateTargetSegments test |
| `internal/api/api.go` | `postSpecJSON.Number` → `PostSpec.Number`; `targetOut.Segments` |
| `internal/web/assets/compose.js` | send `number` in the post spec |
| `internal/web/assets/history.js` | render segment chains + resume button |
| `internal/web/assets/app.css` | chain styles |
| `internal/nostr/nostr.go` | NIP-10: single `root` e-tag when root==parent |
| `README.md` | note threaded posting + resume |

---

## Task 1: ReplyRef, TargetResult.CID, and reply-aware adapters

**Files:**
- Modify: `internal/dispatch/dispatch.go` (ReplyRef, TargetResult.CID, interfaces, runPlatform)
- Modify: `internal/dispatch/adapters.go` (4 adapter methods)
- Test: `internal/dispatch/chain_test.go` (fakes + a runPlatform forward test)

- [ ] **Step 1: Write the failing test** (`internal/dispatch/chain_test.go`)

```go
package dispatch

import (
	"context"
	"testing"

	gonostr "fiatjaf.com/nostr"
)

// fakeBsky is a scriptable BlueskyPoster: it records each call (text, replyTo,
// image count) and returns sequential ids/cids, failing from failAt onward.
type fakeBsky struct {
	calls  []fakeCall
	failAt int // index (0-based) from which calls fail; -1 = never
}
type fakeCall struct {
	text    string
	replyTo *ReplyRef
	nImgs   int
}

func (f *fakeBsky) PostBsky(_ context.Context, text string, _ Overrides, imgs []Img, replyTo *ReplyRef) (TargetResult, error) {
	i := len(f.calls)
	f.calls = append(f.calls, fakeCall{text: text, replyTo: replyTo, nImgs: len(imgs)})
	if f.failAt >= 0 && i >= f.failAt {
		return TargetResult{Platform: "bluesky", Status: "failed", Error: "boom"}, nil
	}
	return TargetResult{
		Platform: "bluesky", Status: "success",
		RemoteID: "at://post" + itoa(i), RemoteURL: "https://bsky/" + itoa(i), CID: "cid" + itoa(i),
	}, nil
}

func itoa(n int) string { return string(rune('0' + n)) } // single-digit indices only in tests

func TestRunPlatformForwardsReplyAndCID(t *testing.T) {
	f := &fakeBsky{failAt: -1}
	d := &Dispatcher{Bluesky: f}
	ref := &ReplyRef{RootID: "r", RootCID: "rc", ParentID: "p", ParentCID: "pc"}
	r := d.runPlatform(context.Background(), "bluesky", "hi", Overrides{}, nil, nil, ref)
	if r.Status != "success" || r.CID != "cid0" {
		t.Fatalf("result: %+v", r)
	}
	if len(f.calls) != 1 || f.calls[0].replyTo != ref {
		t.Fatalf("replyTo not forwarded: %+v", f.calls)
	}
}

// satisfy unused import if needed
var _ = gonostr.Tag{}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/dispatch/ -run TestRunPlatformForwardsReplyAndCID -v`
Expected: FAIL — `ReplyRef` undefined / `PostBsky` signature mismatch / `runPlatform` arity.

- [ ] **Step 3: Add `ReplyRef`, `CID`, and thread `replyTo` through interfaces + runPlatform** (`dispatch.go`)

Add the type (near `TargetResult`):
```go
// ReplyRef threads one segment onto the previous in a chain. RootID/RootCID
// identify the chain head; ParentID/ParentCID the immediately-preceding segment.
// IDs are platform-native: at:// URIs (+ cids) for Bluesky, status/media ids for
// Mastodon/Threads, event ids for Nostr (cids unused there).
type ReplyRef struct {
	RootID, RootCID, ParentID, ParentCID string
}
```
Add `CID string` to `TargetResult` (after `RemoteURL`):
```go
	CID string // bluesky content-hash of the created record (for threading the next reply)
```
Change the four poster interfaces to take `replyTo *ReplyRef`:
```go
type NostrPoster interface {
	PublishText(ctx context.Context, text string, pow *int, imetas []gonostr.Tag, replyTo *ReplyRef) (TargetResult, error)
	RebroadcastToRelay(ctx context.Context, signedEventJSON, relayURL string) (ok bool, message string)
}
type MastodonPoster interface {
	PostText(ctx context.Context, text string, o Overrides, imgs []Img, replyTo *ReplyRef) (TargetResult, error)
}
type BlueskyPoster interface {
	PostBsky(ctx context.Context, text string, o Overrides, imgs []Img, replyTo *ReplyRef) (TargetResult, error)
}
type ThreadsPoster interface {
	PostThreads(ctx context.Context, text string, o Overrides, imgs []Img, replyTo *ReplyRef) (TargetResult, error)
}
```
Change `runPlatform`'s signature to accept `replyTo *ReplyRef` and pass it to each adapter call:
```go
func (d *Dispatcher) runPlatform(ctx context.Context, plat, text string, ov Overrides, imgs []Img, imetas []gonostr.Tag, replyTo *ReplyRef) TargetResult {
```
and inside, change the four adapter invocations to pass `replyTo`:
```go
			r, err = d.Nostr.PublishText(ctx, text, ov.POW, imetas, replyTo)
			...
			r, err = d.Mastodon.PostText(ctx, text, ov, imgs, replyTo)
			...
			r, err = d.Bluesky.PostBsky(ctx, text, ov, imgs, replyTo)
			...
			r, err = d.Threads.PostThreads(ctx, text, ov, imgs, replyTo)
```
Update the two existing `runPlatform` call sites to pass `nil` for now (Task 2 replaces the Post one):
- in `Post` (the goroutine): `results[i] = d.runPlatform(ctx, plat, text, ov, spec.Images, imetas, nil)`
- in `dispatchTargets`: `r := d.runPlatform(ctx, tg.Platform, tg.FinalText, ov, imgs, imetas, nil)`

- [ ] **Step 4: Update the four adapters** (`adapters.go`)

Each adapter method gains the `replyTo *ReplyRef` param and maps it to its client's B1 reply field. Bluesky also sets `r.CID`.

`NostrAdapter.PublishText` — change the signature and build the reply:
```go
func (a NostrAdapter) PublishText(ctx context.Context, text string, pow *int, imetas []gonostr.Tag, replyTo *ReplyRef) (TargetResult, error) {
	in := pubnostr.PublishInput{Text: text, POW: pow, Imetas: imetas}
	if replyTo != nil {
		in.ReplyTo = &pubnostr.NostrReply{RootID: replyTo.RootID, ParentID: replyTo.ParentID}
	}
	r := TargetResult{Platform: "nostr"}
	// ... (rest of the existing body unchanged: marshal req, Publish, relays, status)
```
(Keep the rest of the method body exactly as-is.)

`MastodonAdapter.PostText`:
```go
func (a MastodonAdapter) PostText(ctx context.Context, text string, o Overrides, imgs []Img, replyTo *ReplyRef) (TargetResult, error) {
	// ... existing mi/p construction ...
	if replyTo != nil {
		p.InReplyToID = replyTo.ParentID
	}
	// ... existing Post call + result mapping ...
```

`BlueskyAdapter.PostBsky`:
```go
func (a BlueskyAdapter) PostBsky(ctx context.Context, text string, o Overrides, imgs []Img, replyTo *ReplyRef) (TargetResult, error) {
	// ... existing bi/r/reqB construction ...
	bp := bluesky.Post{
		Text: text, Langs: o.Langs, Images: bi,
		ReplyGate:     bluesky.ParseReplyGate(o.BlueskyReply),
		DisableQuotes: o.BlueskyDisableQuotes,
	}
	if replyTo != nil {
		bp.Reply = &bluesky.ReplyRef{
			RootURI: replyTo.RootID, RootCID: replyTo.RootCID,
			ParentURI: replyTo.ParentID, ParentCID: replyTo.ParentCID,
		}
	}
	res, err := a.C.Post(ctx, bp)
	r.RemoteID, r.RemoteURL, r.CID = res.RemoteID, res.RemoteURL, res.CID
	// ... existing response marshal + gate-partial error handling + status (unchanged) ...
```
(Set `r.CID = res.CID`. Keep the partial/failed handling and `r.Status = "success"` as-is.)

`ThreadsAdapter.PostThreads`:
```go
func (a ThreadsAdapter) PostThreads(ctx context.Context, text string, o Overrides, imgs []Img, replyTo *ReplyRef) (TargetResult, error) {
	// ... existing ti/r/reqB construction ...
	tp := threads.Post{Text: text, TopicTag: o.TopicTag, Images: ti, ReplyControl: o.ThreadsReplyControl}
	if replyTo != nil {
		tp.ReplyToID = replyTo.ParentID
	}
	res, err := a.C.Post(ctx, tp)
	// ... existing result mapping (unchanged) ...
```

- [ ] **Step 5: Run tests**

Run: `go test ./internal/dispatch/ -v` then `go build ./...`
Expected: PASS (new forward test; existing dispatch tests pass since callers pass nil → identical behavior). Build clean (all adapters + interfaces consistent).

- [ ] **Step 6: Commit**

```bash
git add internal/dispatch/dispatch.go internal/dispatch/adapters.go internal/dispatch/chain_test.go
git commit -m "dispatch: thread ReplyRef through adapters; capture bluesky CID"
```

---

## Task 2: runChain — sequential per-platform chain posting

**Files:**
- Modify: `internal/dispatch/dispatch.go` (PostSpec.Number, chainOutcome, runChain, chainStatus, rewire Post)
- Test: `internal/dispatch/chain_test.go`

- [ ] **Step 1: Write the failing test** (append to `chain_test.go`)

```go
func TestRunChainThreadsSegments(t *testing.T) {
	f := &fakeBsky{failAt: -1}
	d := &Dispatcher{Bluesky: f}
	// 3 short segments forced by --- markers (so split is deterministic and >1).
	text := "aaa\n---\nbbb\n---\nccc"
	out := d.runChain(context.Background(), "bluesky", text, Overrides{}, nil, nil, false)

	if out.Status != "success" {
		t.Fatalf("status=%s segs=%+v", out.Status, out.Segments)
	}
	if len(out.Segments) != 3 {
		t.Fatalf("want 3 segments, got %d", len(out.Segments))
	}
	if out.HeadRemoteID != "at://post0" {
		t.Errorf("head remote id = %q", out.HeadRemoteID)
	}
	// Segment 0: no reply. Segment 1: parent=post0. Segment 2: parent=post1, root=post0.
	if f.calls[0].replyTo != nil {
		t.Errorf("head must not reply: %+v", f.calls[0].replyTo)
	}
	if f.calls[1].replyTo == nil || f.calls[1].replyTo.ParentID != "at://post0" || f.calls[1].replyTo.RootID != "at://post0" {
		t.Errorf("seg1 reply wrong: %+v", f.calls[1].replyTo)
	}
	if f.calls[2].replyTo.ParentID != "at://post1" || f.calls[2].replyTo.RootID != "at://post0" {
		t.Errorf("seg2 reply wrong: %+v", f.calls[2].replyTo)
	}
	if f.calls[2].replyTo.ParentCID != "cid1" || f.calls[2].replyTo.RootCID != "cid0" {
		t.Errorf("seg2 cids wrong: %+v", f.calls[2].replyTo)
	}
}

func TestRunChainStopsOnFailure(t *testing.T) {
	f := &fakeBsky{failAt: 1} // segment 0 ok, segment 1 fails
	d := &Dispatcher{Bluesky: f}
	out := d.runChain(context.Background(), "bluesky", "aaa\n---\nbbb\n---\nccc", Overrides{}, nil, nil, false)
	if out.Status != "partial" {
		t.Fatalf("status=%s", out.Status)
	}
	if len(out.Segments) != 2 { // posted head + the failed one, then stopped (segment 2 never attempted)
		t.Fatalf("want 2 attempted segments, got %d: %+v", len(out.Segments), out.Segments)
	}
	if out.Segments[1].Status != "failed" {
		t.Errorf("seg1 should be failed: %+v", out.Segments[1])
	}
	if len(f.calls) != 2 {
		t.Errorf("should have stopped after the failure: %d calls", len(f.calls))
	}
}

func TestRunChainSingleSegmentNoChain(t *testing.T) {
	f := &fakeBsky{failAt: -1}
	d := &Dispatcher{Bluesky: f}
	out := d.runChain(context.Background(), "bluesky", "short", Overrides{}, nil, nil, false)
	if len(out.Segments) != 0 {
		t.Fatalf("single post must have no Segments: %+v", out.Segments)
	}
	if out.Status != "success" || out.HeadRemoteID != "at://post0" {
		t.Errorf("single outcome wrong: %+v", out)
	}
	if f.calls[0].replyTo != nil {
		t.Errorf("single post must not reply")
	}
}

func TestRunChainMediaOnHeadOnly(t *testing.T) {
	f := &fakeBsky{failAt: -1}
	d := &Dispatcher{Bluesky: f}
	imgs := []Img{{BlossomURL: "https://b/x"}}
	d.runChain(context.Background(), "bluesky", "aaa\n---\nbbb", Overrides{}, imgs, nil, false)
	if f.calls[0].nImgs != 1 {
		t.Errorf("head should carry images: %d", f.calls[0].nImgs)
	}
	if f.calls[1].nImgs != 0 {
		t.Errorf("non-head segments must carry no images: %d", f.calls[1].nImgs)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/dispatch/ -run TestRunChain -v`
Expected: FAIL — `runChain` undefined.

- [ ] **Step 3: Add `PostSpec.Number`, `chainOutcome`, `runChain`, `chainStatus`** (`dispatch.go`)

Add `Number bool` to `PostSpec` (after `MediaRecords`):
```go
	Number bool // append k/n counters to threaded segments
```
Add the import for the splitter (in the import block):
```go
	"github.com/geofox/publisher/internal/thread"
```
Add the outcome type and functions (near `runPlatform`):
```go
// chainOutcome is the result of posting one platform's (possibly single-segment)
// chain. Segments is empty for a single post (preserving non-threaded behavior).
type chainOutcome struct {
	Platform                  string
	Status                    string
	Error                     string
	HeadRemoteID, HeadRemoteURL string
	LatencyMS                 int
	Relays                    []store.RelayState
	SignedEventJSON           string
	RequestJSON, ResponseJSON string
	Segments                  []store.Segment
}

// runChain splits text to the platform's limit and posts the segments as a
// reply-chain (segment k+1 replies to segment k; media on the head only). A
// single segment posts exactly as before, with no Segments recorded.
func (d *Dispatcher) runChain(ctx context.Context, plat, text string, ov Overrides, imgs []Img, imetas []gonostr.Tag, number bool) chainOutcome {
	segTexts, _ := thread.Split(text, thread.LimitFor(plat), thread.Opts{Number: number})
	if len(segTexts) <= 1 {
		r := d.runPlatform(ctx, plat, text, ov, imgs, imetas, nil)
		return chainOutcome{
			Platform: plat, Status: r.Status, Error: r.Error,
			HeadRemoteID: r.RemoteID, HeadRemoteURL: r.RemoteURL, LatencyMS: r.LatencyMS,
			Relays: r.Relays, SignedEventJSON: r.SignedEventJSON,
			RequestJSON: r.RequestJSON, ResponseJSON: r.ResponseJSON,
		}
	}
	out := chainOutcome{Platform: plat}
	var rootID, rootCID, parentID, parentCID string
	for i, st := range segTexts {
		var replyTo *ReplyRef
		if i > 0 {
			replyTo = &ReplyRef{RootID: rootID, RootCID: rootCID, ParentID: parentID, ParentCID: parentCID}
		}
		var segImgs []Img
		var segImetas []gonostr.Tag
		if i == 0 {
			segImgs, segImetas = imgs, imetas // media + imeta on the head only
		}
		r := d.runPlatform(ctx, plat, st, ov, segImgs, segImetas, replyTo)
		out.Segments = append(out.Segments, store.Segment{
			Ordinal: i, Text: st, RemoteID: r.RemoteID, RemoteURL: r.RemoteURL, CID: r.CID,
			Status: r.Status, Error: r.Error,
		})
		if i == 0 {
			rootID, rootCID = r.RemoteID, r.CID
			out.HeadRemoteID, out.HeadRemoteURL = r.RemoteID, r.RemoteURL
			out.Relays, out.SignedEventJSON = r.Relays, r.SignedEventJSON
			out.RequestJSON, out.ResponseJSON, out.LatencyMS = r.RequestJSON, r.ResponseJSON, r.LatencyMS
		}
		parentID, parentCID = r.RemoteID, r.CID
		out.Error = r.Error
		if r.RemoteID == "" { // no id to reply to → cannot continue the chain
			break
		}
	}
	out.Status = chainStatus(out.Segments, len(segTexts))
	return out
}

// chainStatus aggregates a chain's segment statuses. expected is the planned
// segment count; fewer attempted (a stop) or any non-success ⇒ partial.
func chainStatus(segs []store.Segment, expected int) string {
	if len(segs) == 0 || segs[0].RemoteID == "" || segs[0].Status == "failed" {
		return "failed"
	}
	complete := len(segs) == expected
	for _, s := range segs {
		if s.Status != "success" {
			complete = false
		}
	}
	if complete {
		return "success"
	}
	return "partial"
}
```

- [ ] **Step 4: Rewire `Post` to use runChain** (`dispatch.go`)

Replace the goroutine result type and body, and the Target-building loop. Specifically:

Change `results := make([]TargetResult, len(platforms))` to:
```go
	outcomes := make([]chainOutcome, len(platforms))
```
Change the goroutine body line from `results[i] = d.runPlatform(...)` to:
```go
			outcomes[i] = d.runChain(ctx, plat, text, ov, spec.Images, imetas, spec.Number)
```
Replace the aggregation loop (`for _, r := range results { ... }`) with one over `outcomes`:
```go
	succ, failed := 0, 0
	for _, o := range outcomes {
		fields, _ := json.Marshal(ov2fields(spec.Overrides[o.Platform]))
		tg := store.Target{
			Platform: o.Platform, FinalText: finalText(spec, o.Platform), FieldsJSON: string(fields),
			Status: o.Status, RemoteID: o.HeadRemoteID, RemoteURL: o.HeadRemoteURL, LatencyMS: o.LatencyMS,
			Relays: o.Relays, SignedEventJSON: o.SignedEventJSON, Segments: o.Segments,
			Attempts: []store.Attempt{{
				AttemptNo: 1, Status: o.Status, Error: o.Error, LatencyMS: o.LatencyMS, RemoteID: o.HeadRemoteID,
				RequestJSON: o.RequestJSON, ResponseJSON: o.ResponseJSON, AttemptedAt: time.Now().UTC(),
			}},
		}
		rec.Targets = append(rec.Targets, tg)
		switch o.Status {
		case "success":
			succ++
		case "failed":
			failed++
		}
	}
	total := len(outcomes)
```
(Keep the `switch { case total == 0 || failed == total ... }` aggregation that follows, now using `total = len(outcomes)`; keep the `SavePost` + `return rec`.)

- [ ] **Step 5: Run tests**

Run: `go test ./internal/dispatch/ -v` then `go test ./... && go build ./...`
Expected: PASS. The single-segment path keeps existing dispatch/api tests green (no Segments, identical Target). New chain tests pass.

- [ ] **Step 6: Commit**

```bash
git add internal/dispatch/dispatch.go internal/dispatch/chain_test.go
git commit -m "dispatch: post per-platform reply chains (runChain) + number plumbing"
```

---

## Task 3: store.UpdateTargetSegments (persist a resumed chain)

**Files:**
- Modify: `internal/store/store.go`
- Test: `internal/store/models_test.go`

- [ ] **Step 1: Write the failing test** (append to `models_test.go`)

```go
func TestUpdateTargetSegments(t *testing.T) {
	db, _ := Open(filepath.Join(t.TempDir(), "upd.db"))
	defer db.Close()
	rec := &Post{
		ID: "u1", CreatedAt: time.Now().UTC().Truncate(time.Second),
		Platforms: []string{"bluesky"}, Source: "web", Status: "partial",
		Targets: []Target{{
			Platform: "bluesky", Status: "partial", RemoteID: "at://0", RemoteURL: "https://b/0",
			Segments: []Segment{
				{Ordinal: 0, Text: "a", RemoteID: "at://0", CID: "c0", Status: "success"},
				{Ordinal: 1, Text: "b", Status: "failed", Error: "x"},
			},
			Attempts: []Attempt{{AttemptNo: 1, Status: "partial", AttemptedAt: time.Now()}},
		}},
	}
	if err := db.SavePost(rec); err != nil {
		t.Fatal(err)
	}
	got, _ := db.GetPost("u1")
	tid := got.Targets[0].ID

	resumed := []Segment{
		{Ordinal: 0, Text: "a", RemoteID: "at://0", CID: "c0", Status: "success"},
		{Ordinal: 1, Text: "b", RemoteID: "at://1", CID: "c1", Status: "success"},
	}
	if err := db.UpdateTargetSegments(tid, resumed, "success", "at://0", "https://b/0", 12, ""); err != nil {
		t.Fatal(err)
	}
	after, _ := db.GetPost("u1")
	tg := after.Targets[0]
	if tg.Status != "success" || len(tg.Segments) != 2 || tg.Segments[1].Status != "success" || tg.Segments[1].RemoteID != "at://1" {
		t.Fatalf("segments not updated: status=%s segs=%+v", tg.Status, tg.Segments)
	}
	if tg.AttemptCount < 2 {
		t.Errorf("attempt_count should bump: %d", tg.AttemptCount)
	}
	if after.Status != "success" {
		t.Errorf("post status should recompute to success: %s", after.Status)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/store/ -run TestUpdateTargetSegments -v`
Expected: FAIL — `UpdateTargetSegments` undefined.

- [ ] **Step 3: Add `UpdateTargetSegments`** (`store.go`, near `AppendTargetAttempt`)

```go
// UpdateTargetSegments overwrites a threaded target's segment chain + status
// (used when resuming a partial thread), records a new attempt, and recomputes
// the post's aggregate status. Mirrors AppendTargetAttempt's bookkeeping.
func (s *Store) UpdateTargetSegments(targetID int64, segments []Segment, status, headRemoteID, headRemoteURL string, latencyMS int, errMsg string) error {
	tx, err := s.sql.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	segJSON, err := json.Marshal(segments)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	var n int
	if err := tx.QueryRow(`SELECT attempt_count FROM post_targets WHERE id=?`, targetID).Scan(&n); err != nil {
		return err
	}
	n++
	if _, err := tx.Exec(
		`INSERT INTO target_attempts(target_id,attempt_no,status,error,latency_ms,remote_id,request_json,response_json,attempted_at)
		 VALUES(?,?,?,?,?,?,?,?,?)`,
		targetID, n, status, errMsg, latencyMS, headRemoteID, "", "", now,
	); err != nil {
		return err
	}
	if _, err := tx.Exec(
		`UPDATE post_targets SET status=?, remote_id=?, remote_url=?, latency_ms=?, attempt_count=?, last_attempt_at=?, segments_json=? WHERE id=?`,
		status, headRemoteID, headRemoteURL, latencyMS, n, now, string(segJSON), targetID,
	); err != nil {
		return err
	}
	var postID string
	if err := tx.QueryRow(`SELECT post_id FROM post_targets WHERE id=?`, targetID).Scan(&postID); err != nil {
		return err
	}
	if err := recomputeStatus(tx, postID); err != nil {
		return err
	}
	return tx.Commit()
}
```
(`recomputeStatus(tx, postID)` already exists in store.go and is used by `AppendTargetAttempt`.)

- [ ] **Step 4: Run tests**

Run: `go test ./internal/store/ -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/store/store.go internal/store/models_test.go
git commit -m "store: UpdateTargetSegments for resuming a partial thread"
```

---

## Task 4: resumeChain — continue a partial thread

**Files:**
- Modify: `internal/dispatch/dispatch.go` (resumeChain, dispatchTargets branch, Retry predicate)
- Test: `internal/dispatch/chain_test.go`

- [ ] **Step 1: Write the failing test** (append to `chain_test.go`)

```go
func TestResumeChainContinuesFromFailure(t *testing.T) {
	// Stored target: head succeeded, segment 1 failed, segment 2 never attempted.
	tg := store.Target{
		ID: 7, Platform: "bluesky", Status: "partial",
		Segments: []store.Segment{
			{Ordinal: 0, Text: "aaa", RemoteID: "at://post0", CID: "cid0", Status: "success"},
			{Ordinal: 1, Text: "bbb", Status: "failed", Error: "boom"},
			{Ordinal: 2, Text: "ccc", Status: "failed"},
		},
	}
	f := &fakeBsky{failAt: -1}
	out := resumeSegments(&Dispatcher{Bluesky: f}, context.Background(), tg, Overrides{}, nil, nil)

	if out.Status != "success" {
		t.Fatalf("status=%s segs=%+v", out.Status, out.Segments)
	}
	if len(f.calls) != 2 { // only segments 1 and 2 reposted; head untouched
		t.Fatalf("expected 2 reposts, got %d", len(f.calls))
	}
	// segment 1 replies to head (post0); segment 2 replies to the just-posted seg1.
	if f.calls[0].text != "bbb" || f.calls[0].replyTo.ParentID != "at://post0" || f.calls[0].replyTo.RootID != "at://post0" {
		t.Errorf("resume seg1 reply wrong: %+v", f.calls[0])
	}
	if f.calls[1].text != "ccc" || f.calls[1].replyTo.ParentID != "at://post0" /*post0+0? no*/ {
		// f.calls[1].replyTo.ParentID should be the new seg1 id ("at://post0" is head;
		// new seg1 returns "at://post0" only if index resets — verify against fake ids)
	}
	if out.Segments[1].RemoteID == "" || out.Segments[1].Status != "success" {
		t.Errorf("seg1 not resumed: %+v", out.Segments[1])
	}
}
```

NOTE: the `fakeBsky` returns ids by *call index* (`at://post0`, `at://post1`, …). In resume, the first repost (segment 1) is call index 0 → returns `at://post0`; segment 2 is call index 1 → `at://post1` with parent = the new seg1 id (`at://post0`). Adjust the assertions to the fake's call-index ids: seg1's reply parent/root = the ORIGINAL head id stored in the target (`at://post0`); seg2's reply parent = the freshly-returned seg1 id (`at://post0` again, since fake call index 0). To avoid confusion, make the test assert: (a) 2 reposts, (b) seg1 reply root/parent == stored head `at://post0`, (c) out.Segments all success, (d) out.Status == success. Replace the seg2 parent assertion with: `f.calls[1].replyTo.RootID == "at://post0"` (root is always the stored head). Keep it simple:

```go
	if f.calls[1].replyTo == nil || f.calls[1].replyTo.RootID != "at://post0" {
		t.Errorf("resume seg2 root wrong: %+v", f.calls[1].replyTo)
	}
```

The test calls a small test-only helper `resumeSegments` that exposes `resumeChain`'s computed outcome without a store (so it's unit-testable). Implement `resumeChain` to take the store, but also factor the pure chain-continuation into a helper the test can call. To keep it simple, expose the core as `func (d *Dispatcher) resumeSegments(ctx, tg store.Target, ov Overrides, imgs []Img, imetas []gonostr.Tag) chainOutcome` (no store writes) and have `resumeChain` call it then persist via `UpdateTargetSegments`. The test calls `d.resumeSegments(...)` directly. Update the test's call to a method: `out := (&Dispatcher{Bluesky: f}).resumeSegments(context.Background(), tg, Overrides{}, nil, nil)`.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/dispatch/ -run TestResumeChain -v`
Expected: FAIL — `resumeSegments` undefined.

- [ ] **Step 3: Implement `resumeSegments` + `resumeChain` + wire into dispatchTargets/Retry** (`dispatch.go`)

```go
// resumeSegments re-posts a partial chain's segments from the first non-success
// segment, threading from the last successful one. Returns the updated outcome
// (no store writes). If the head (segment 0) isn't success, the whole chain is
// re-posted from scratch.
func (d *Dispatcher) resumeSegments(ctx context.Context, tg store.Target, ov Overrides, imgs []Img, imetas []gonostr.Tag) chainOutcome {
	segs := append([]store.Segment(nil), tg.Segments...)
	start := 0
	for start < len(segs) && segs[start].Status == "success" && segs[start].RemoteID != "" {
		start++
	}
	out := chainOutcome{Platform: tg.Platform, Segments: segs}
	if start == len(segs) { // already complete
		out.Status = chainStatus(segs, len(segs))
		out.HeadRemoteID, out.HeadRemoteURL = segs[0].RemoteID, segs[0].RemoteURL
		return out
	}
	var rootID, rootCID, parentID, parentCID string
	if start > 0 {
		rootID, rootCID = segs[0].RemoteID, segs[0].CID
		parentID, parentCID = segs[start-1].RemoteID, segs[start-1].CID
	}
	for i := start; i < len(segs); i++ {
		var replyTo *ReplyRef
		if i > 0 {
			replyTo = &ReplyRef{RootID: rootID, RootCID: rootCID, ParentID: parentID, ParentCID: parentCID}
		}
		var segImgs []Img
		var segImetas []gonostr.Tag
		if i == 0 {
			segImgs, segImetas = imgs, imetas
		}
		r := d.runPlatform(ctx, tg.Platform, segs[i].Text, ov, segImgs, segImetas, replyTo)
		segs[i] = store.Segment{Ordinal: i, Text: segs[i].Text, RemoteID: r.RemoteID, RemoteURL: r.RemoteURL, CID: r.CID, Status: r.Status, Error: r.Error}
		if i == 0 {
			rootID, rootCID = r.RemoteID, r.CID
			out.Relays, out.SignedEventJSON = r.Relays, r.SignedEventJSON
		}
		parentID, parentCID = r.RemoteID, r.CID
		out.Error = r.Error
		if r.RemoteID == "" {
			break
		}
	}
	out.Segments = segs
	out.Status = chainStatus(segs, len(segs))
	out.HeadRemoteID, out.HeadRemoteURL = segs[0].RemoteID, segs[0].RemoteURL
	return out
}

// resumeChain resumes a partial threaded target and persists the result.
func (d *Dispatcher) resumeChain(ctx context.Context, tg store.Target, ov Overrides, imgs []Img, imetas []gonostr.Tag) error {
	out := d.resumeSegments(ctx, tg, ov, imgs, imetas)
	return d.Store.UpdateTargetSegments(tg.ID, out.Segments, out.Status, out.HeadRemoteID, out.HeadRemoteURL, out.LatencyMS, out.Error)
}
```

In `dispatchTargets`, branch threaded targets to `resumeChain`:
```go
	for _, tg := range post.Targets {
		if !want(tg) {
			continue
		}
		var ov Overrides
		if tg.FieldsJSON != "" {
			if err := json.Unmarshal([]byte(tg.FieldsJSON), &ov); err != nil {
				slog.Warn("dispatchTargets: bad fields_json, using zero overrides", "target_id", tg.ID, "err", err)
			}
		}
		if len(tg.Segments) > 1 { // threaded target → resume the chain
			if err := d.resumeChain(ctx, tg, ov, imgs, imetas); err != nil {
				return err
			}
			continue
		}
		r := d.runPlatform(ctx, tg.Platform, tg.FinalText, ov, imgs, imetas, nil)
		if err := d.Store.AppendTargetAttempt(tg.ID, r.Status, r.Error, r.RemoteID, r.RemoteURL, r.LatencyMS, r.RequestJSON, r.ResponseJSON, r.Relays, r.SignedEventJSON); err != nil {
			return err
		}
	}
```

In `Retry`, extend the predicate so a *partial threaded* target is also retryable (a single-post "partial" — e.g. nostr with some relays down — is NOT, to avoid duplicating the note; that uses RetryRelay):
```go
	if err := d.dispatchTargets(ctx, post, func(t store.Target) bool {
		retryable := t.Status == "failed" || t.Status == "missed" || (t.Status == "partial" && len(t.Segments) > 1)
		return retryable && (len(platforms) == 0 || want[t.Platform])
	}); err != nil {
		return nil, err
	}
```

- [ ] **Step 4: Run tests**

Run: `go test ./internal/dispatch/ -v` then `go test ./... && go build ./...`
Expected: PASS (resume test + all prior). Existing single-post retry behavior unchanged (threaded branch only triggers when `len(Segments) > 1`).

- [ ] **Step 5: Commit**

```bash
git add internal/dispatch/dispatch.go internal/dispatch/chain_test.go
git commit -m "dispatch: stop-and-resume partial threads"
```

---

## Task 5: API — number in, segments out

**Files:**
- Modify: `internal/api/api.go`
- Test: `internal/api/thread_post_test.go`

- [ ] **Step 1: Write the failing test** (`internal/api/thread_post_test.go`)

```go
package api

import (
	"bytes"
	"context"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/geofox/publisher/internal/dispatch"
	"github.com/geofox/publisher/internal/store"
)

// capturingDispatcher implements the api.Dispatcher interface (Post / Retry /
// RetryRelay / Schedule) and records the Number flag from the spec.
type capturingDispatcher struct{ gotNumber bool }

func (c *capturingDispatcher) Post(_ context.Context, spec dispatch.PostSpec) *store.Post {
	c.gotNumber = spec.Number
	return &store.Post{
		ID: "p1", Status: "success", Platforms: spec.Platforms,
		Targets: []store.Target{{
			Platform: "bluesky", Status: "success",
			Segments: []store.Segment{
				{Ordinal: 0, Text: "a", Status: "success", RemoteURL: "u0"},
				{Ordinal: 1, Text: "b", Status: "success", RemoteURL: "u1"},
			},
		}},
	}
}
func (c *capturingDispatcher) Retry(context.Context, string, []string) (*store.Post, error) {
	return nil, nil
}
func (c *capturingDispatcher) RetryRelay(context.Context, string, string) (*store.Post, error) {
	return nil, nil
}
func (c *capturingDispatcher) Schedule(context.Context, dispatch.PostSpec, time.Time) (*store.Post, error) {
	return nil, nil
}

func TestAPIPostForwardsNumberAndReturnsSegments(t *testing.T) {
	cap := &capturingDispatcher{}
	a := &API{Dispatch: cap}

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	spec, _ := json.Marshal(map[string]any{
		"master_text": "hi", "platforms": []string{"bluesky"}, "number": true,
	})
	_ = mw.WriteField("spec", string(spec))
	mw.Close()

	req := httptest.NewRequest(http.MethodPost, "/api/post", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	rec := httptest.NewRecorder()
	a.Routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	if !cap.gotNumber {
		t.Errorf("PostSpec.Number not forwarded from request")
	}
	var out struct {
		Targets []struct {
			Platform string          `json:"platform"`
			Segments []store.Segment `json:"segments"`
		} `json:"targets"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if len(out.Targets) != 1 || len(out.Targets[0].Segments) != 2 {
		t.Fatalf("segments not in response: %s", rec.Body.String())
	}
}
```

NOTE: this fake must match `api.Dispatcher` exactly. The interface (in `api.go`) is:
`Post(ctx, dispatch.PostSpec) *store.Post`, `Retry(ctx, string, []string) (*store.Post, error)`,
`RetryRelay(ctx, string, string) (*store.Post, error)`, `Schedule(ctx, dispatch.PostSpec, time.Time) (*store.Post, error)`.
If it has drifted, copy its current method set; the existing `internal/api/api_test.go` fake dispatcher is a reference.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/api/ -run TestAPIPostForwardsNumber -v`
Expected: FAIL — `Number` not forwarded / `segments` absent in response.

- [ ] **Step 3: Add `number` to the spec and `segments` to the response** (`api.go`)

In `postSpecJSON`, add:
```go
	Number bool `json:"number"`
```
In `handleAPIPost`, where `dispatch.PostSpec{...}` is built, add `Number: sj.Number`:
```go
	spec := dispatch.PostSpec{
		MasterText: sj.MasterText, Platforms: sj.Platforms, DelaySeconds: sj.DelaySeconds,
		Source: "web", Overrides: sj.Overrides, Images: imgs, MediaRecords: mediaRecs,
		Number: sj.Number,
	}
```
In the immediate-post response (`targetOut` struct + the loop that fills `out.Targets`), add a `Segments` field and populate it:
```go
	type targetOut struct {
		Platform  string             `json:"platform"`
		Status    string             `json:"status"`
		Error     string             `json:"error,omitempty"`
		RemoteURL string             `json:"remote_url,omitempty"`
		LatencyMS int                `json:"latency_ms"`
		Relays    []store.RelayState `json:"relays,omitempty"`
		Segments  []store.Segment    `json:"segments,omitempty"`
	}
```
and in the `for _, tg := range rec.Targets` loop that appends to `out.Targets`, add `Segments: tg.Segments`.

- [ ] **Step 4: Run tests**

Run: `go test ./internal/api/ -v` then `go build ./...`
Expected: PASS (new test + existing api tests; note other api tests that build a fake Dispatcher may need the same interface — they already implement it, so unaffected).

- [ ] **Step 5: Commit**

```bash
git add internal/api/api.go internal/api/thread_post_test.go
git commit -m "api: forward number to dispatch; return segment chains"
```

---

## Task 6: Compose — send the numbering toggle with the post

**Files:**
- Modify: `internal/web/assets/compose.js`

- [ ] **Step 1: Read how compose.js builds the post spec**

Open `internal/web/assets/compose.js` and find where it assembles the `spec` object that's `JSON.stringify`'d into the `spec` multipart field for `POST /api/post` (it includes `master_text`, `platforms`, `overrides`, etc.). Also confirm how it reads the `#threadnum` checkbox (Plan A added it; preview.js reads `document.getElementById("threadnum")?.checked ?? true`).

- [ ] **Step 2: Add `number` to the spec**

In the spec object compose.js builds for `/api/post`, add:
```js
    number: document.getElementById("threadnum")?.checked ?? true,
```
(Place it alongside `master_text`/`platforms`. This makes the actual post use the same numbering choice the preview shows.)

- [ ] **Step 3: Build + manual sanity**

Run: `go build ./cmd/publisher && go test ./internal/web/`
Expected: clean + pass. (No JS unit harness; correctness is verified manually in Task 9's live test.)

- [ ] **Step 4: Commit**

```bash
git add internal/web/assets/compose.js
git commit -m "web: send numbering toggle with the post"
```

---

## Task 7: History — render segment chains + resume

**Files:**
- Modify: `internal/web/assets/history.js`
- Modify: `internal/web/assets/app.css`

- [ ] **Step 1: Read the history detail renderer**

Open `internal/web/assets/history.js`. Find `renderDetail(post)` and the per-target rendering (`resultRow(t, ...)` and, for nostr, `relayBlock(t, retryFn)`). Note the existing retry mechanism (how a failed target triggers `POST /api/posts/{id}/retry` with `{platforms:[...]}`), and `common.js`'s `el()` builder + `api()` helper. The post JSON now includes `target.segments` (array of `{ordinal,text,remote_id,remote_url,cid,status,error}`) for threaded targets.

- [ ] **Step 2: Render the chain + resume button**

In the per-target rendering, when `t.segments` is a non-empty array, render the chain instead of (or in addition to) the single-line result: a small list where each segment shows its ordinal+status and links to `remote_url` when present; failed segments show the error. When `t.status === "partial"` (a stopped chain), render a **Resume** button that calls the existing retry endpoint for this post filtered to this platform:
```js
// inside the target block, when segments exist:
function segmentChain(post, t) {
  const wrap = el("div", { class: "seg-chain" });
  (t.segments || []).forEach((s) => {
    const row = el("div", { class: "seg-row seg-" + s.status });
    row.append(el("span", { class: "seg-n" }, `${s.ordinal + 1}.`));
    const body = el("span", { class: "seg-text" }, s.text);
    if (s.remote_url) {
      const a = el("a", { href: s.remote_url, target: "_blank", rel: "noopener" }, "↗");
      row.append(body, a);
    } else {
      row.append(body);
    }
    if (s.error) row.append(el("span", { class: "seg-err" }, s.error));
    wrap.append(row);
  });
  if (t.status === "partial") {
    const btn = el("button", { class: "ghost sm" }, "resume");
    btn.addEventListener("click", async () => {
      btn.disabled = true;
      try {
        await api(`/api/posts/${post.id}/retry`, { method: "POST", body: JSON.stringify({ platforms: [t.platform] }) });
        loadHistory(true); // refresh the list/detail
      } catch (e) {
        btn.disabled = false;
      }
    });
    wrap.append(btn);
  }
  return wrap;
}
```
Call `segmentChain(post, t)` from `renderDetail`'s per-target loop when `t.segments?.length` (otherwise keep the existing single-post `resultRow`). Match the file's actual `el`/`api`/`loadHistory` imports and the way other buttons are wired. Use `el(...)` (which builds text via `textContent`, not innerHTML) so segment text/errors are not an XSS vector.

- [ ] **Step 3: Add CSS** (`app.css`)

```css
/* threaded post history */
.seg-chain { display:flex; flex-direction:column; gap:4px; margin-top:6px; }
.seg-row { display:flex; align-items:baseline; gap:6px; font-size:13px; }
.seg-n { color:var(--muted); font-variant-numeric:tabular-nums; }
.seg-text { color:var(--ink); white-space:pre-wrap; }
.seg-row.seg-failed .seg-text { color:var(--bad); }
.seg-err { color:var(--bad); font-size:12px; }
```
(Confirm the actual CSS variable names at the top of app.css; adjust if they differ.)

- [ ] **Step 4: Build + test**

Run: `go test ./internal/web/ && go build ./cmd/publisher`
Expected: pass + clean.

- [ ] **Step 5: Commit**

```bash
git add internal/web/assets/history.js internal/web/assets/app.css
git commit -m "web: render threaded-post chains in history with resume"
```

---

## Task 8: Nostr — collapse root==parent to a single e-tag (NIP-10)

**Files:**
- Modify: `internal/nostr/nostr.go`
- Test: `internal/nostr/reply_test.go`

- [ ] **Step 1: Update the failing test** (`reply_test.go`)

Replace `TestReplyTagsRootEqualsParentForFirstReply` (which currently asserts 2 e-tags) with:
```go
func TestReplyTagsRootEqualsParentEmitsSingleRoot(t *testing.T) {
	// Replying directly to the root: emit a single "root" e-tag, not duplicate
	// root+reply tags with the same id (canonical NIP-10).
	tags := replyTags(&NostrReply{RootID: "x", ParentID: "x", RelayHint: "wss://r"}, "p")
	eCount := 0
	var eTag []string
	for _, tg := range tags {
		if tg[0] == "e" {
			eCount++
			eTag = tg
		}
	}
	if eCount != 1 {
		t.Fatalf("expected a single e-tag when root==parent, got %d: %v", eCount, tags)
	}
	if eTag[1] != "x" || eTag[3] != "root" {
		t.Errorf("e-tag should be the root marker: %v", eTag)
	}
}
```
(Keep `TestReplyTagsRootAndParent` and `TestReplyTagsNil`.)

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/nostr/ -run TestReplyTags -v`
Expected: FAIL — current `replyTags` emits two e-tags for root==parent.

- [ ] **Step 3: Collapse root==parent in `replyTags`** (`nostr.go`)

```go
func replyTags(r *NostrReply, authorPubkeyHex string) []gonostr.Tag {
	if r == nil {
		return nil
	}
	if r.ParentID == "" || r.ParentID == r.RootID {
		// Replying directly to the root (or no distinct parent): single root marker.
		return []gonostr.Tag{
			{"e", r.RootID, r.RelayHint, "root"},
			{"p", authorPubkeyHex},
		}
	}
	return []gonostr.Tag{
		{"e", r.RootID, r.RelayHint, "root"},
		{"e", r.ParentID, r.RelayHint, "reply"},
		{"p", authorPubkeyHex},
	}
}
```

- [ ] **Step 4: Run tests**

Run: `go test ./internal/nostr/ -v`
Expected: PASS (updated + existing).

- [ ] **Step 5: Commit**

```bash
git add internal/nostr/nostr.go internal/nostr/reply_test.go
git commit -m "nostr: single root e-tag when replying directly to root (NIP-10)"
```

---

## Task 9: Docs + full verification

**Files:**
- Modify: `README.md`

- [ ] **Step 1: Document threaded posting**

In `README.md`, near the `POST /publish`/`/api/post` and `/api/thread-preview` docs, add a short note:
```markdown
**Threaded posting:** when a draft exceeds a platform's limit (or contains `---`
break markers), `/api/post` posts it as a native reply-chain per platform —
Bluesky/Mastodon/Threads wrap to their limits, Nostr stays one note unless marked.
A chain that fails mid-way is recorded as `partial`; **resume** from history
re-posts only the not-yet-sent segments. With `number`, segments get per-platform
` k/n` counters.
```

- [ ] **Step 2: Full verification**

Run:
```bash
go test ./...
go vet ./...
go build ./cmd/publisher
```
Expected: ALL packages PASS, vet clean, build succeeds. If any failure, STOP and report.

- [ ] **Step 3: Commit**

```bash
git add README.md
git commit -m "docs: document threaded posting + resume"
```

---

## Self-Review notes (for the implementer)

- **Spec coverage:** §3 sequencing (Task 2 runChain + Task 1 reply refs), §4 store/dispatch model (Tasks 1–4), §5 stop-&-resume (Tasks 3–4), §6 API (Task 5), §7 history (Task 7), numbering plumbing (Tasks 2,5,6), NIP-10 polish (Task 8), docs (Task 9).
- **Compilation coupling:** Task 1 changes the adapter interfaces + `runPlatform` arity, so all four adapters and both call sites must change together (they do, in Task 1). After Task 1 everything passes `nil` → behavior identical; Task 2 introduces actual chaining.
- **Backward compatibility:** a single-segment post (the common case) takes `runChain`'s `len<=1` branch → no `Segments`, identical Target/Attempt to today; existing dispatch/api/store tests stay green.
- **Resume safety:** only targets with `len(Segments) > 1` resume; single-post `partial` (nostr relays) keeps using `RetryRelay`. Resume re-posts from the first non-success segment (or the whole chain if the head failed), never re-posting a succeeded segment (no duplicates).
- **Test strategy:** the `Dispatcher` fields are interfaces, so chain/resume logic is fully unit-tested with `fakeBsky` (no network). Live end-to-end posting is manual (real platform creds), as with prior features.
- **fakeBsky id scheme:** ids are by call-index (`at://post0`, `post1`, …); the resume test must assert against call-index ids, not the original stored ids, for freshly-posted segments (root stays the stored head id).
- **Type names from B1 (do not rename):** `bluesky.ReplyRef{RootURI,RootCID,ParentURI,ParentCID}`, `nostr.NostrReply{RootID,ParentID,RelayHint}`, `mastodon.Post.InReplyToID`, `threads.Post.ReplyToID`, `store.Segment`, `bluesky.Result.CID`.
