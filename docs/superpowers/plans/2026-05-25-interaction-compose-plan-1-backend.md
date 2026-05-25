# Interaction Compose Mode — Plan 1: Backend Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make `dispatch.Interact` post **threaded** reply/quote interactions with **fan-out reproduction** (commentary + the original's text + re-hosted media + link) on other platforms, and turn `POST /api/interact` into a multipart endpoint that re-hosts the source's media.

**Architecture:** `runChain` (the threading engine) gains an optional "head action" so the head segment can be a reply or a quote (tail segments thread as replies). `dispatch.Interact` becomes one `runChain` per platform: the source platform does the native action; fan-out platforms post an *assembled reproduction*. Media re-hosting (download source images → Blossom/upload pipeline) happens in the API (which already owns `media.Process`) and is passed to dispatch as a separate `SourceImages` field, so dispatch needs no new media/HTTP dependency.

**Tech Stack:** Go 1.26; existing `internal/dispatch` (`runChain`/`runPlatform`/`runAction`), `internal/media` pipeline, `internal/thread` splitter.

**Spec:** `docs/superpowers/specs/2026-05-25-interaction-compose-mode-design.md` (§Threading model, §Content assembly, §Backend). The frontend (Compose interaction mode + Interact hand-off) is **Plan 2**.

**Builds on:** the shipped quote/reply/repost feature (`dispatch.Interact`, `runAction`, `runChain`, `InteractSpec`/`InteractRef`, `store.Post.Interaction`).

---

## File Structure

| File | Responsibility (Plan 1) |
|---|---|
| `internal/dispatch/dispatch.go` | `headSpec` + `runHead`; `runChain` head-action param; `InteractSpec` additions (`Number`, `SourcePreview`, `SourceImages`, `SourceMediaRecords`); reworked `Interact`; `assembleReproduction`, `capMedia` helpers |
| `internal/dispatch/interact_test.go` | threaded reply/quote, fan-out reproduction, media cap, degrade tests |
| `internal/api/api.go` | `handleInteract` → multipart; re-host source media (guarded fetch + `media.Process`); forward new spec fields |
| `internal/api/interact_post_test.go` | multipart parse + forward test |

---

## Task 1: `runChain` head-action seeding

**Files:** Modify `internal/dispatch/dispatch.go`; Test `internal/dispatch/interact_test.go`.

Today `runChain(ctx, plat, text, ov, imgs, imetas, number)` posts a chain whose head is a plain post. Add an optional head action so the head can be a reply or a quote; the tail always threads as replies under the head (unchanged).

- [ ] **Step 1: Read** `runChain` (the `len(segTexts)<=1` early return AND the per-segment loop where `i==0` gets `replyTo=nil` + media, and `i>0` gets `replyTo={root,parent}`), plus `runPlatform` and `runAction` signatures.

- [ ] **Step 2: Write the failing test** (append to `internal/dispatch/interact_test.go`)

```go
func TestRunChainReplyHead(t *testing.T) {
	f := &fakeBsky{failAt: -1} // existing chain_test.go fake: returns at://post0.. , records calls
	d := &Dispatcher{Bluesky: f}
	head := &headSpec{reply: &ReplyRef{RootID: "at://src", RootCID: "csrc", ParentID: "at://src", ParentCID: "csrc"}}
	out := d.runChain(context.Background(), "bluesky", "aaa\n---\nbbb", Overrides{}, nil, nil, false, head)
	if out.Status != "success" || len(out.Segments) != 2 {
		t.Fatalf("want 2-seg success, got %s %+v", out.Status, out.Segments)
	}
	// head replies to the source; seg2 replies to the head (at://post0).
	if f.calls[0].replyTo == nil || f.calls[0].replyTo.ParentID != "at://src" {
		t.Errorf("head must reply to source: %+v", f.calls[0].replyTo)
	}
	if f.calls[1].replyTo == nil || f.calls[1].replyTo.ParentID != "at://post0" {
		t.Errorf("seg2 must reply to the head: %+v", f.calls[1].replyTo)
	}
}

func TestRunChainPlainHeadUnchanged(t *testing.T) {
	f := &fakeBsky{failAt: -1}
	d := &Dispatcher{Bluesky: f}
	out := d.runChain(context.Background(), "bluesky", "solo", Overrides{}, nil, nil, false, nil)
	if out.Status != "success" || len(out.Segments) != 0 { // single post → no Segments, head not a reply
		t.Fatalf("plain single post changed: %s %+v", out.Status, out.Segments)
	}
	if f.calls[0].replyTo != nil {
		t.Errorf("plain head must not reply: %+v", f.calls[0].replyTo)
	}
}
```

- [ ] **Step 3: Run** `go test ./internal/dispatch/ -run 'TestRunChainReplyHead|TestRunChainPlainHeadUnchanged' -v` → FAIL (`headSpec` undefined / `runChain` arity).

- [ ] **Step 4: Implement** (`dispatch.go`)

Add the head type + a `runHead` helper:
```go
// headSpec makes a chain's head segment a reply or a quote instead of a plain
// post. nil → plain post head (normal Post + fan-out reproduction). The tail
// segments always thread as plain replies under the head.
type headSpec struct {
	reply *ReplyRef    // head replies to this
	quote *InteractRef // head quotes this (native quote)
}

// runHead posts the head segment per the headSpec (reply / quote / plain).
func (d *Dispatcher) runHead(ctx context.Context, plat, text string, ov Overrides, imgs []Img, imetas []gonostr.Tag, head *headSpec) TargetResult {
	if head != nil && head.quote != nil {
		return d.runAction(ctx, actionQuote, plat, text, ov, imgs, *head.quote)
	}
	var replyTo *ReplyRef
	if head != nil {
		replyTo = head.reply
	}
	return d.runPlatform(ctx, plat, text, ov, imgs, imetas, replyTo)
}
```

Change `runChain`'s signature to take `head *headSpec` (last param) and route the head segment through `runHead`:
```go
func (d *Dispatcher) runChain(ctx context.Context, plat, text string, ov Overrides, imgs []Img, imetas []gonostr.Tag, number bool, head *headSpec) chainOutcome {
```
- In the `len(segTexts) <= 1` early-return branch, replace `r := d.runPlatform(ctx, plat, text, ov, imgs, imetas, nil)` with `r := d.runHead(ctx, plat, text, ov, imgs, imetas, head)`.
- In the per-segment loop, for the head segment (`i == 0`) replace the `d.runPlatform(...)` call with `d.runHead(ctx, plat, st, ov, segImgs, segImetas, head)`. Leave the tail (`i > 0`) calling `d.runPlatform(...)` with the `replyTo={root,parent}` it already builds.

Update the existing `Post` call site (it calls `runChain(...)` in its goroutine) to pass `nil` for head:
```go
			outcomes[i] = d.runChain(ctx, plat, text, ov, spec.Images, imetas, spec.Number, nil)
```

- [ ] **Step 5: Run** `go test ./internal/dispatch/ -v` then `go build ./...` → PASS (existing chain/Post tests unaffected — `head=nil` reproduces today's behavior exactly). 

- [ ] **Step 6: Commit**
```bash
git add internal/dispatch/dispatch.go internal/dispatch/interact_test.go
git commit -m "dispatch: runChain optional head action (reply/quote head)"
```

---

## Task 2: reproduction + media-cap helpers + InteractSpec fields

**Files:** Modify `internal/dispatch/dispatch.go`; Test `internal/dispatch/interact_test.go`.

- [ ] **Step 1: Write the failing test** (append to `interact_test.go`)

```go
func TestAssembleReproduction(t *testing.T) {
	sp := SourcePreview{Author: "@bird", Text: "the original"}
	got := assembleReproduction("my take", sp, "https://x/9")
	want := "my take\n\n— @bird:\nthe original\n\nhttps://x/9"
	if got != want {
		t.Fatalf("assembleReproduction:\n got %q\nwant %q", got, want)
	}
	// empty commentary → just the attributed original + url
	if g := assembleReproduction("", sp, "https://x/9"); g != "— @bird:\nthe original\n\nhttps://x/9" {
		t.Errorf("empty commentary wrong: %q", g)
	}
}

func TestCapMedia(t *testing.T) {
	user := []Img{{Alt: "u1"}, {Alt: "u2"}}
	src := []Img{{Alt: "s1"}, {Alt: "s2"}, {Alt: "s3"}}
	out := capMedia(user, src, 4)
	if len(out) != 4 || out[0].Alt != "u1" || out[1].Alt != "u2" || out[2].Alt != "s1" || out[3].Alt != "s2" {
		t.Fatalf("cap should keep user first then fill from source up to max: %+v", out)
	}
}
```

- [ ] **Step 2: Run** `go test ./internal/dispatch/ -run 'TestAssembleReproduction|TestCapMedia' -v` → FAIL (undefined).

- [ ] **Step 3: Implement** (`dispatch.go`)

Add the source-preview type + fields to `InteractSpec` (read the current `InteractSpec` and add):
```go
// SourcePreview is the resolved original's content, passed from the frontend so
// fan-out targets can reproduce it without re-resolving.
type SourcePreview struct {
	Author string // @handle / display
	Text   string
}
```
Add to `InteractSpec` (after the existing fields):
```go
	Number             bool          // k/n counters on threaded segments
	SourcePreview      SourcePreview // for fan-out reproduction
	SourceImages       []Img         // original's media, re-hosted (fan-out only)
	SourceMediaRecords []store.Media // imeta records for the re-hosted source media
```

Add the helpers:
```go
// assembleReproduction builds a fan-out post body: commentary, an attributed copy
// of the original's text, and the source URL — each separated by a blank line.
func assembleReproduction(commentary string, sp SourcePreview, sourceURL string) string {
	var parts []string
	if c := strings.TrimSpace(commentary); c != "" {
		parts = append(parts, c)
	}
	if strings.TrimSpace(sp.Text) != "" {
		parts = append(parts, "— "+sp.Author+":\n"+sp.Text)
	}
	if sourceURL != "" {
		parts = append(parts, sourceURL)
	}
	return strings.Join(parts, "\n\n")
}

// capMedia returns the user's images followed by source images, truncated to max
// (max <= 0 means no cap). User images always take priority.
func capMedia(user, source []Img, max int) []Img {
	out := append([]Img(nil), user...)
	for _, m := range source {
		if max > 0 && len(out) >= max {
			break
		}
		out = append(out, m)
	}
	return out
}

// mediaMax is the per-platform attachment cap (matches the app's 4-image limit).
func mediaMax(plat string) int {
	switch plat {
	case "bluesky", "mastodon", "threads":
		return 4
	default: // nostr: no fixed cap
		return 0
	}
}
```
(Confirm `strings` is imported in dispatch.go — it is.)

- [ ] **Step 4: Run** `go test ./internal/dispatch/ -v` then `go build ./...` → PASS.

- [ ] **Step 5: Commit**
```bash
git add internal/dispatch/dispatch.go internal/dispatch/interact_test.go
git commit -m "dispatch: reproduction + media-cap helpers; InteractSpec source fields"
```

---

## Task 3: rework `dispatch.Interact` to thread + reproduce

**Files:** Modify `internal/dispatch/dispatch.go`; Test `internal/dispatch/interact_test.go`.

Rework `Interact` from "one target per platform" to "one `runChain` per platform": the source platform performs the native action (reply/quote head); fan-out platforms post an assembled reproduction. Repost is unchanged (one-click, single `runAction`). Build each target's `store.Target` from the `chainOutcome` (carry `Segments`), mirroring how `Post` maps `chainOutcome`→`Target`.

- [ ] **Step 1: Read** the current `Interact` (its reply/repost/quote switch + the target-building loop) and how `Post` maps a `chainOutcome` to a `store.Target` (Platform, Status, RemoteID=HeadRemoteID, Segments, Attempts, Relays, SignedEventJSON, LatencyMS).

- [ ] **Step 2: Write the failing tests** (append to `interact_test.go`)

```go
func TestInteractReplyThreads(t *testing.T) {
	f := &fakeBsky{failAt: -1}
	d := &Dispatcher{Bluesky: f}
	post := d.Interact(context.Background(), InteractSpec{
		Action: actionReply, SourcePlatform: "bluesky",
		Ref:  InteractRef{URI: "at://src", CID: "csrc", ReplyRootURI: "at://src", ReplyRootCID: "csrc"},
		Text: "aaa\n---\nbbb", // two segments
	})
	if len(post.Targets) != 1 {
		t.Fatalf("reply → 1 target, got %d", len(post.Targets))
	}
	if len(post.Targets[0].Segments) != 2 {
		t.Fatalf("long reply should thread: %+v", post.Targets[0].Segments)
	}
	if f.calls[0].replyTo.ParentID != "at://src" {
		t.Errorf("head must reply to the source: %+v", f.calls[0].replyTo)
	}
}

func TestInteractQuoteFanoutReproduces(t *testing.T) {
	bsky := &fakeBsky{failAt: -1}
	masto := &fakeMastoActor{} // existing fake: records lastPostText
	d := &Dispatcher{Bluesky: bsky, Mastodon: masto}
	post := d.Interact(context.Background(), InteractSpec{
		Action: actionQuote, SourcePlatform: "bluesky",
		Ref:           InteractRef{URI: "at://src", CID: "csrc"},
		SourceURL:     "https://bsky/9", SourceAuthor: "@bird",
		SourcePreview: SourcePreview{Author: "@bird", Text: "tweet text"},
		Text:          "look", Fanout: []string{"mastodon"},
	})
	if len(post.Targets) != 2 {
		t.Fatalf("quote+fanout → 2 targets, got %d", len(post.Targets))
	}
	// mastodon fan-out reproduces commentary + original text + url (a normal post)
	body := masto.lastPostText.s
	if !strings.Contains(body, "look") || !strings.Contains(body, "tweet text") || !strings.Contains(body, "https://bsky/9") {
		t.Errorf("fan-out should reproduce text + url: %q", body)
	}
}
```

- [ ] **Step 3: Run** `go test ./internal/dispatch/ -run TestInteract -v` → FAIL (no threading / no reproduction).

- [ ] **Step 4: Implement** — replace the `switch spec.Action` block in `Interact` with one that produces `chainOutcome`s, and the target-building loop to map them:

```go
	var outcomes []chainOutcome
	switch spec.Action {
	case actionRepost:
		r := d.runAction(ctx, actionRepost, spec.SourcePlatform, "", ov, nil, spec.Ref)
		outcomes = append(outcomes, chainOutcome{
			Platform: r.Platform, Status: r.Status, Error: r.Error,
			HeadRemoteID: r.RemoteID, HeadRemoteURL: r.RemoteURL, LatencyMS: r.LatencyMS,
			Relays: r.Relays, SignedEventJSON: r.SignedEventJSON, RequestJSON: r.RequestJSON, ResponseJSON: r.ResponseJSON,
		})
	case actionReply, actionQuote:
		// Source platform: native action as the chain head.
		head := &headSpec{}
		if spec.Action == actionReply {
			head.reply = buildReplyRef(spec)
		} else {
			head.quote = &spec.Ref
		}
		// Mastodon native quote can't carry media → degrade to a fan-out reproduction.
		if spec.Action == actionQuote && spec.SourcePlatform == "mastodon" && len(spec.Images) > 0 {
			outcomes = append(outcomes, d.fanoutChain(ctx, "mastodon", spec))
		} else {
			outcomes = append(outcomes, d.runChain(ctx, spec.SourcePlatform, spec.Text, ov, spec.Images, buildImetas(spec.MediaRecords), spec.Number, head))
		}
		// Fan-out platforms: assembled reproduction.
		for _, p := range spec.Fanout {
			if p == spec.SourcePlatform {
				continue
			}
			outcomes = append(outcomes, d.fanoutChain(ctx, p, spec))
		}
	}

	succ, failed := 0, 0
	for _, o := range outcomes {
		fields, _ := json.Marshal(ov2fields(spec.Overrides[o.Platform]))
		rec.Platforms = append(rec.Platforms, o.Platform)
		rec.Targets = append(rec.Targets, store.Target{
			Platform: o.Platform, FinalText: o.FinalText, FieldsJSON: string(fields),
			Status: o.Status, RemoteID: o.HeadRemoteID, RemoteURL: o.HeadRemoteURL, LatencyMS: o.LatencyMS,
			Relays: o.Relays, SignedEventJSON: o.SignedEventJSON, Segments: o.Segments,
			Attempts: []store.Attempt{{AttemptNo: 1, Status: o.Status, Error: o.Error, LatencyMS: o.LatencyMS,
				RemoteID: o.HeadRemoteID, RequestJSON: o.RequestJSON, ResponseJSON: o.ResponseJSON, AttemptedAt: time.Now().UTC()}},
		})
		switch o.Status {
		case "success":
			succ++
		case "failed":
			failed++
		}
	}
	switch total := len(outcomes); {
	case total == 0 || failed == total:
		rec.Status = "failed"
	case succ == total:
		rec.Status = "success"
	default:
		rec.Status = "partial"
	}
```

Add `FinalText` to `chainOutcome` (so the right text is archived) and set it in `runChain` (`out.FinalText = text` at the end, and in the single-segment branch). Add the `fanoutChain` helper:
```go
// fanoutChain posts an assembled reproduction (commentary + original text + url,
// with re-hosted source media capped per platform) as a normal thread.
func (d *Dispatcher) fanoutChain(ctx context.Context, plat string, spec InteractSpec) chainOutcome {
	text := assembleReproduction(spec.Text, spec.SourcePreview, spec.SourceURL)
	imgs := capMedia(spec.Images, spec.SourceImages, mediaMax(plat))
	// imetas for nostr come from the combined media records (user + source), capped.
	recs := capMediaRecords(spec.MediaRecords, spec.SourceMediaRecords, mediaMax(plat))
	ov := spec.Overrides[plat]
	return d.runChain(ctx, plat, text, ov, imgs, buildImetas(recs), spec.Number, nil)
}

// capMediaRecords mirrors capMedia for store.Media (nostr imeta).
func capMediaRecords(user, source []store.Media, max int) []store.Media {
	out := append([]store.Media(nil), user...)
	for _, m := range source {
		if max > 0 && len(out) >= max {
			break
		}
		out = append(out, m)
	}
	return out
}
```
NOTE: `chainOutcome` may not have a `FinalText` field yet — add `FinalText string` to it. In `runChain`, set `out.FinalText = text` for both the single-segment and multi-segment returns so each target archives what it actually sent. (Repost outcome above sets no FinalText → "" which is correct, repost has no text.) Confirm `buildImetas`, `ov2fields`, `buildReplyRef`, `time`, `json` are all in scope (they are, used by `Post`/the old `Interact`). Remove any now-dead code from the old single-target `Interact` (the old `results []TargetResult` loop, `linkQuoteText` if no longer used — keep `linkQuoteText` only if still referenced; otherwise delete it).

- [ ] **Step 5: Run** `go test ./internal/dispatch/ -v` then `go test ./... && go build ./...` → PASS. Confirm the existing interact tests still pass (or update the older `TestInteractQuoteFansOut`/`TestInteractReplySingleTarget` if their assertions assumed the pre-rework single-target shape — they should still hold: 1 target for reply, 2 for quote+1 fanout; the fan-out text now includes the reproduction, so update any assertion that checked for *only* the URL to also allow the reproduced text).

- [ ] **Step 6: Commit**
```bash
git add internal/dispatch/dispatch.go internal/dispatch/interact_test.go
git commit -m "dispatch: Interact threads source action + reproduces fan-out targets"
```

---

## Task 4: `/api/interact` multipart + source-media re-host

**Files:** Modify `internal/api/api.go`; Test `internal/api/interact_post_test.go`.

`handleInteract` becomes multipart (a `spec` JSON field + `image` files), mirroring `handleAPIPost`. It re-hosts the source's media URLs (carried in the spec) via a guarded fetch + `media.Process`, and forwards everything to `dispatch.Interact`.

- [ ] **Step 1: Read** `handleAPIPost` (multipart parse, the `image` files loop calling `a.media.Process` → `dispatch.Img` + `store.Media`, the 4-image cap) and the current `handleInteract`.

- [ ] **Step 2: Write the failing test** (replace the body of the existing `TestAPIInteractForwardsSpec` in `internal/api/interact_post_test.go` with a multipart version; keep `TestAPIInteractRejectsBadAction` but send it as multipart too)

```go
func TestAPIInteractForwardsSpec(t *testing.T) {
	fd := &fakeInteractDispatcher{}
	a := &API{Dispatch: fd}

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	spec, _ := json.Marshal(map[string]any{
		"action": "quote", "platform": "bluesky",
		"ref":            map[string]any{"uri": "at://x", "cid": "cidx"},
		"source_url":     "https://bsky/9", "source_author": "@a",
		"source_preview": map[string]any{"author": "@a", "text": "orig"},
		"text":           "hi", "fanout": []string{"mastodon"}, "number": true,
	})
	_ = mw.WriteField("spec", string(spec))
	mw.Close()

	req := httptest.NewRequest(http.MethodPost, "/api/interact", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	rec := httptest.NewRecorder()
	a.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	if fd.got.Action != "quote" || fd.got.SourcePlatform != "bluesky" || fd.got.Ref.URI != "at://x" {
		t.Fatalf("spec not forwarded: %+v", fd.got)
	}
	if !fd.got.Number || fd.got.SourcePreview.Text != "orig" || len(fd.got.Fanout) != 1 {
		t.Errorf("number/source_preview/fanout not forwarded: %+v", fd.got)
	}
}
```
Add imports `bytes`, `mime/multipart` to the test file. Update `TestAPIInteractRejectsBadAction` to post a multipart body with a `spec` field of `{"action":"bogus","platform":"bluesky"}` (so it exercises the new parse path) and still expect 400.

- [ ] **Step 3: Run** `go test ./internal/api/ -run TestAPIInteract -v` → FAIL (handler still JSON-only).

- [ ] **Step 4: Implement** (`api.go`) — rewrite `handleInteract` to parse multipart like `handleAPIPost`:

```go
func (a *API) handleInteract(w http.ResponseWriter, r *http.Request) {
	if a.Dispatch == nil {
		httpx.WriteError(w, http.StatusServiceUnavailable, "dispatch not configured")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxPostRequestBytes)
	if err := r.ParseMultipartForm(8 << 20); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "parse multipart: "+err.Error())
		return
	}
	var sj struct {
		Action        string                        `json:"action"`
		Platform      string                        `json:"platform"`
		Ref           dispatch.InteractRef          `json:"ref"`
		SourceURL     string                        `json:"source_url"`
		SourceAuthor  string                        `json:"source_author"`
		SourcePreview struct {
			Author string `json:"author"`
			Text   string `json:"text"`
			Media  []struct {
				URL string `json:"url"`
				Alt string `json:"alt"`
			} `json:"media"`
		} `json:"source_preview"`
		Text      string                        `json:"text"`
		Overrides map[string]dispatch.Overrides `json:"overrides"`
		Fanout    []string                      `json:"fanout"`
		Number    bool                          `json:"number"`
		Force     bool                          `json:"force"`
		Images    []struct {
			Alt string `json:"alt"`
		} `json:"images"`
	}
	if err := json.Unmarshal([]byte(r.FormValue("spec")), &sj); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "invalid spec json: "+err.Error())
		return
	}
	switch sj.Action {
	case "reply", "repost", "quote":
	default:
		httpx.WriteError(w, http.StatusBadRequest, "action must be reply, repost, or quote")
		return
	}
	if sj.Platform == "" {
		httpx.WriteError(w, http.StatusBadRequest, "platform is required")
		return
	}

	// User-attached images (mirror handleAPIPost).
	var userImgs []dispatch.Img
	var userRecs []store.Media
	files := r.MultipartForm.File["image"]
	if len(files) > 4 {
		httpx.WriteError(w, http.StatusBadRequest, "max 4 images")
		return
	}
	for i, fh := range files {
		f, err := fh.Open()
		if err != nil {
			httpx.WriteError(w, http.StatusBadRequest, "open image: "+err.Error())
			return
		}
		body, err := io.ReadAll(f)
		f.Close()
		if err != nil {
			httpx.WriteError(w, http.StatusInternalServerError, "read image: "+err.Error())
			return
		}
		res, err := a.media.Process(r.Context(), body, fh.Header.Get("Content-Type"))
		if err != nil {
			httpx.WriteError(w, http.StatusBadGateway, "media: "+err.Error())
			return
		}
		alt := ""
		if i < len(sj.Images) {
			alt = sj.Images[i].Alt
		}
		userImgs = append(userImgs, dispatch.Img{Bytes: res.Bytes, Mime: res.Mime, Alt: alt, BlossomURL: res.URL})
		userRecs = append(userRecs, store.Media{Ordinal: i, BlossomURL: res.URL, SHA256: res.SHA256, Mime: res.Mime, Dim: res.Dim, Blurhash: res.Blurhash, SizeBytes: res.Size, Alt: alt})
	}

	// Re-host the original's media (best-effort; skip any that fail).
	var srcImgs []dispatch.Img
	var srcRecs []store.Media
	for i, m := range sj.SourcePreview.Media {
		body, mime, err := a.fetchSourceMedia(r.Context(), m.URL)
		if err != nil {
			continue
		}
		res, err := a.media.Process(r.Context(), body, mime)
		if err != nil {
			continue
		}
		srcImgs = append(srcImgs, dispatch.Img{Bytes: res.Bytes, Mime: res.Mime, Alt: m.Alt, BlossomURL: res.URL})
		srcRecs = append(srcRecs, store.Media{Ordinal: i, BlossomURL: res.URL, SHA256: res.SHA256, Mime: res.Mime, Dim: res.Dim, Blurhash: res.Blurhash, SizeBytes: res.Size, Alt: m.Alt})
	}

	post := a.Dispatch.Interact(r.Context(), dispatch.InteractSpec{
		Action: sj.Action, SourcePlatform: sj.Platform, Ref: sj.Ref,
		SourceURL: sj.SourceURL, SourceAuthor: sj.SourceAuthor, Text: sj.Text,
		Overrides: sj.Overrides, Fanout: sj.Fanout, Force: sj.Force, Number: sj.Number,
		Images: userImgs, MediaRecords: userRecs,
		SourcePreview:      dispatch.SourcePreview{Author: sj.SourcePreview.Author, Text: sj.SourcePreview.Text},
		SourceImages:       srcImgs,
		SourceMediaRecords: srcRecs,
	})
	httpx.WriteJSON(w, http.StatusOK, post)
}

// fetchSourceMedia downloads a source media URL for re-hosting. It uses an
// SSRF-guarded client (only public https hosts; no private/loopback/link-local
// targets) since the URL comes from a pasted (untrusted) post.
func (a *API) fetchSourceMedia(ctx context.Context, rawURL string) ([]byte, string, error) {
	u, err := url.Parse(rawURL)
	if err != nil || u.Scheme != "https" {
		return nil, "", fmt.Errorf("bad media url")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, "", err
	}
	resp, err := safeMediaClient.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, "", fmt.Errorf("media fetch %s", resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxUploadRequestBytes))
	if err != nil {
		return nil, "", err
	}
	return body, resp.Header.Get("Content-Type"), nil
}
```
For `safeMediaClient`: reuse the project's existing SSRF-guarded HTTP client/dialer if one exists (search `internal/verify` and `internal/httpx` for a dialer that blocks private IPs — the NIP-05 SSRF fix added one). If a reusable guarded client exists, use it; otherwise define a package-level `var safeMediaClient = &http.Client{Timeout: 20 * time.Second, Transport: <guarded transport>}` using a `DialContext` that resolves the host and rejects loopback/private/link-local/ULA addresses. Confirm `url`, `io`, `fmt`, `time` imports exist in api.go (they do).

- [ ] **Step 5: Run** `go test ./internal/api/ -v` then `go test ./... && go vet ./... && go build ./cmd/publisher` → PASS.

- [ ] **Step 6: Commit**
```bash
git add internal/api/api.go internal/api/interact_post_test.go
git commit -m "api: /api/interact multipart + source-media re-host (SSRF-guarded)"
```

---

## Task 5: docs + full verification

**Files:** Modify `README.md`.

- [ ] **Step 1: Update the `/api/interact` doc** in `README.md` to note it's now multipart (`spec` JSON + `image` files), carries `source_preview` (for fan-out reproduction) and `number`, and that fan-out targets reproduce the original's text + re-hosted media + link.

- [ ] **Step 2: Full verification** — run `go test ./...`, `go vet ./...`, `go build ./cmd/publisher`. Expected: all pass. STOP and report on any failure.

- [ ] **Step 3: Commit**
```bash
git add README.md
git commit -m "docs: /api/interact multipart + fan-out reproduction"
```

---

## Self-Review notes (for the implementer)

- **Spec coverage (Plan 1):** §Threading model — `runChain` head action (T1) + Interact per-platform chains (T3); §Content assembly — `assembleReproduction`/`capMedia`/`fanoutChain` (T2-3); §Backend — multipart `/api/interact` + media re-host (T4), `/api/resolve` unchanged; Mastodon-quote+media degrade (T3); media cap (T2-3). **Deferred to Plan 2:** the Compose interaction mode + Interact hand-off UI.
- **Backward compat:** `runChain` gains a trailing `head *headSpec` param; `Post` passes `nil` → identical behavior, and the threading tests stay green. `Interact`'s repost path is unchanged. The shipped quote/reply behavior is extended (now threads + reproduces) — update the two older interact assertions if they hard-coded the single-target / URL-only shape.
- **Dispatch stays dependency-light:** media re-host (HTTP fetch + `media.Process`) lives in the API; dispatch only receives `SourceImages`/`SourceMediaRecords`. No new dispatch import.
- **Security:** source media URLs are untrusted (from a pasted post) → `fetchSourceMedia` must use an SSRF-guarded client (reuse the existing guarded dialer from the NIP-05 fix if present) and an `https`-only + size-limited read.
- **Type names (Plan 2 depends on these):** `dispatch.SourcePreview{Author,Text}`, `InteractSpec.{Number,SourcePreview,SourceImages,SourceMediaRecords}`, the `/api/interact` multipart shape (`spec` field with `source_preview.{author,text,media[]}`, `number`, `images[].alt`, `image` files).
