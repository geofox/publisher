# Quote-Carries-Media + Interaction UX Polish — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make quote actions carry the user's attached media natively on every platform (fixing the preview-vs-post fidelity gaps), and polish the Interact→Compose hand-off for clarity and safety.

**Architecture:** Backend (Go) — stop dropping `imgs`/`imetas` at the quote adapters, delete the Mastodon quote+media→reproduction degrade, and thread media through `runAction`. Frontend (vanilla JS/CSS) — four self-contained friendliness bundles in `compose.js`/`interact.js`/`preview.js`/`app.css`.

**Tech Stack:** Go 1.26 (`go test ./...`, `go vet`), vanilla ES-module SPA verified with `node --check`. Branch: `feature/interaction-preview` (continues the in-flight fidelity work; do NOT merge/tag/deploy without explicit user approval).

**Background facts (verified, do not re-discover):**
- Bluesky `Client.Post` already emits `app.bsky.embed.recordWithMedia` when `p.Quote != nil && len(images) > 0` (`internal/bluesky/bluesky.go:191-196`). The adapter `QuoteBsky` just never sets `bp.Images`.
- Nostr `Publisher.Publish` already appends each imeta's URL to the content and adds the imeta tags (`internal/nostr/nostr.go:117-121, 140-143`). The adapter `Quote` just never passes `Imetas`.
- Mastodon `Client.QuotePost` (`internal/mastodon/source.go:125`) sends only `status` + `quoted_status_id`. The gomast media-upload path is `POST /api/v2/media` returning `{"id": "..."}` (see `internal/mastodon/mastodon_test.go:14`).
- Dispatch flow for a native source quote: `Interact` → `runChain(..., spec.Images, buildImetas(spec.MediaRecords), ..., head{quote})` → `runHead` → `runAction(actionQuote, plat, text, ov, imgs, ref)` → adapter. `runAction` currently has **no imetas param**, so Nostr quote media is dropped there.
- `Interact` currently has a degrade branch (`internal/dispatch/dispatch.go:663-664`) sending Mastodon quote+media through `fanoutChain`. This is REMOVED by this plan.
- Frontend: images are attachable in interaction mode and `previewMedia(p)` already returns the user's images for the source platform — so once the adapters carry media, the existing preview becomes accurate with NO frontend preview change for media.

---

## Phase 1 — Backend: quotes carry media

### Task B1: Mastodon `QuotePost` uploads and attaches media

**Files:**
- Modify: `internal/mastodon/source.go:125-135` (`QuotePost`)
- Test: `internal/mastodon/action_test.go`

- [ ] **Step 1: Write the failing test**

Add to `internal/mastodon/action_test.go`:

```go
func TestQuotePostSendsMediaIDs(t *testing.T) {
	var gotMediaIDs []string
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v2/media", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "media1"})
	})
	mux.HandleFunc("/api/v1/statuses", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		gotMediaIDs = r.Form["media_ids[]"]
		if r.FormValue("quoted_status_id") != "99" || r.FormValue("status") != "take" {
			t.Errorf("quote form missing fields: %v", r.Form)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "200", "url": "https://x/@me/200"})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := New(srv.URL, "tok")
	res, err := c.QuotePost(context.Background(), "take", "99", []Image{{Bytes: []byte("img"), Alt: "a cat"}})
	if err != nil {
		t.Fatal(err)
	}
	if res.RemoteID != "200" {
		t.Errorf("quote id wrong: %+v", res)
	}
	if len(gotMediaIDs) != 1 || gotMediaIDs[0] != "media1" {
		t.Errorf("media_ids[] not sent: %v", gotMediaIDs)
	}
}
```

Add `"encoding/json"` to the test file's imports (alongside existing `io`, `net/http`, `net/http/httptest`, `strings`).

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/mastodon/ -run TestQuotePostSendsMediaIDs`
Expected: FAIL — `QuotePost` currently takes 3 args (compile error: too many arguments).

- [ ] **Step 3: Implement**

Replace `QuotePost` in `internal/mastodon/source.go`:

```go
// QuotePost creates a native quote post (server 4.5+). text is the commentary;
// quotedID is the LOCAL status id to quote; imgs are attached as media.
func (c *Client) QuotePost(ctx context.Context, text, quotedID string, imgs []Image) (Result, error) {
	form := url.Values{"status": {text}, "quoted_status_id": {quotedID}}
	for _, img := range imgs {
		att, err := c.c.UploadMediaFromMedia(ctx, &gomast.Media{
			File: bytes.NewReader(img.Bytes), Description: img.Alt,
		})
		if err != nil {
			return Result{}, fmt.Errorf("quote media: %w", err)
		}
		form.Add("media_ids[]", string(att.ID))
	}
	var st struct {
		ID  string `json:"id"`
		URL string `json:"url"`
	}
	if err := c.postForm(ctx, "/api/v1/statuses", form, &st); err != nil {
		return Result{}, fmt.Errorf("quote: %w", err)
	}
	return Result{RemoteID: st.ID, RemoteURL: st.URL}, nil
}
```

Add imports to `internal/mastodon/source.go`: `"bytes"` and `gomast "github.com/mattn/go-mastodon"` (keep existing imports).

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/mastodon/`
Expected: PASS (both the new test and the existing `TestQuotePostSendsQuotedStatusID` — note the latter still calls `QuotePost(ctx, "my take", "99")` with 3 args and will now fail to compile).

- [ ] **Step 5: Fix the existing 3-arg caller test**

In `internal/mastodon/action_test.go`, update `TestQuotePostSendsQuotedStatusID` to pass `nil` for images:

```go
	res, err := c.QuotePost(context.Background(), "my take", "99", nil)
```

Run: `go test ./internal/mastodon/` → Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/mastodon/source.go internal/mastodon/action_test.go
git commit -m "mastodon: QuotePost attaches media via media_ids[]"
```

---

### Task B2: Mastodon actor carries media; remove the quote+media degrade

**Files:**
- Modify: `internal/dispatch/dispatch.go` — `MastodonActor` interface (line 126-129), `runAction` mastodon-quote case (line 231-236), `Interact` degrade branch (line 662-667)
- Modify: `internal/dispatch/adapters.go` — `QuoteStatus` (line 218-224)
- Modify fakes: `internal/dispatch/interact_test.go:93`, `internal/dispatch/retry_test.go:30`, `internal/dispatch/dispatch_test.go:92`
- Modify/replace test: `internal/dispatch/interact_test.go` `TestInteractMastodonQuoteWithMediaDegrades` (line ~286)

- [ ] **Step 1: Write the failing test (replace the degrade test)**

In `internal/dispatch/interact_test.go`, the `fakeMastoActor.QuoteStatus` must record the images it receives. First update the fake (its struct + method) so it captures media; the existing struct is near line 88-96. Make it:

```go
type fakeMastoActor struct {
	quoteText string
	quoteImgs []Img
}

func (f *fakeMastoActor) Reblog(context.Context, string) (TargetResult, error) {
	return TargetResult{Platform: "mastodon", Status: "success", RemoteID: "rb"}, nil
}
func (f *fakeMastoActor) QuoteStatus(_ context.Context, text, _ string, imgs []Img) (TargetResult, error) {
	f.quoteText, f.quoteImgs = text, imgs
	return TargetResult{Platform: "mastodon", Status: "success", RemoteID: "mq"}, nil
}
```

(If `fakeMastoActor` currently has different fields, preserve any used by other tests and ADD `quoteImgs`. Check usages before editing.)

Then REPLACE `TestInteractMastodonQuoteWithMediaDegrades` with:

```go
func TestInteractMastodonQuoteCarriesMedia(t *testing.T) {
	masto := &fakeMastoActor{}
	d := &Dispatcher{Mastodon: mastoPosterWith(masto)} // see note below
	post := d.Interact(context.Background(), InteractSpec{
		Action: actionQuote, SourcePlatform: "mastodon",
		Ref:    InteractRef{LocalID: "42"},
		Text:   "look", Images: []Img{{Alt: "x"}},
	})
	if len(masto.quoteImgs) != 1 || masto.quoteImgs[0].Alt != "x" {
		t.Errorf("mastodon native quote should carry the attached media, got %#v", masto.quoteImgs)
	}
	if masto.quoteText != "look" {
		t.Errorf("native quote text should be the commentary only, got %q", masto.quoteText)
	}
	if got := post.Targets[0].FinalText; got != "look" {
		t.Errorf("final text = %q, want commentary-only \"look\"", got)
	}
}
```

NOTE: match however the existing degrade test constructed its `Dispatcher.Mastodon` (the `mastoPosterWith` placeholder above stands for that existing construction — reuse the exact pattern already in the file; the degrade test built a `MastodonPoster` wrapping the actor). Read the old test before deleting it and mirror its dispatcher wiring.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/dispatch/ -run TestInteractMastodonQuoteCarriesMedia`
Expected: FAIL to compile — `QuoteStatus` interface/signature mismatch.

- [ ] **Step 3: Change the interface + adapter + dispatch**

In `internal/dispatch/dispatch.go`, `MastodonActor`:

```go
type MastodonActor interface {
	Reblog(ctx context.Context, statusID string) (TargetResult, error)
	QuoteStatus(ctx context.Context, text, quotedID string, imgs []Img) (TargetResult, error)
}
```

In `internal/dispatch/dispatch.go`, `runAction` mastodon-quote case:

```go
	case action == actionQuote && plat == "mastodon":
		if d.Mastodon != nil {
			r, err = d.Mastodon.QuoteStatus(ctx, text, ref.LocalID, imgs)
		} else {
			err = errors.New("mastodon not configured")
		}
```

In `internal/dispatch/dispatch.go`, `Interact` — DELETE the degrade branch so all quote platforms go through the head path. Replace lines 662-667:

```go
			outcomes = append(outcomes, d.runChain(ctx, spec.SourcePlatform, interactText(spec, spec.SourcePlatform), ov, spec.Images, buildImetas(spec.MediaRecords), spec.Number, head))
```

(i.e. remove the `if spec.Action == actionQuote && spec.SourcePlatform == "mastodon" && len(spec.Images) > 0 { ... } else {` wrapper, keeping only the `runChain` call.)

In `internal/dispatch/adapters.go`, `QuoteStatus`:

```go
func (a MastodonAdapter) QuoteStatus(ctx context.Context, text, quotedID string, imgs []Img) (TargetResult, error) {
	var mi []mastodon.Image
	for _, im := range imgs {
		mi = append(mi, mastodon.Image{Bytes: im.Bytes, Alt: im.Alt})
	}
	res, err := a.C.QuotePost(ctx, text, quotedID, mi)
	if err != nil {
		return TargetResult{Platform: "mastodon"}, err
	}
	return TargetResult{Platform: "mastodon", Status: "success", RemoteID: res.RemoteID, RemoteURL: res.RemoteURL}, nil
}
```

- [ ] **Step 4: Update the other mastodon fakes**

`internal/dispatch/retry_test.go:30` and `internal/dispatch/dispatch_test.go:92` — change each `QuoteStatus(context.Context, string, string)` to `QuoteStatus(context.Context, string, string, []Img)`:

```go
func (m *retryMasto) QuoteStatus(context.Context, string, string, []Img) (TargetResult, error) {
```
```go
func (fakeMasto) QuoteStatus(context.Context, string, string, []Img) (TargetResult, error) {
```

- [ ] **Step 5: Run the suite**

Run: `go test ./internal/dispatch/ && go vet ./internal/dispatch/`
Expected: PASS, clean.

- [ ] **Step 6: Commit**

```bash
git add internal/dispatch/dispatch.go internal/dispatch/adapters.go internal/dispatch/interact_test.go internal/dispatch/retry_test.go internal/dispatch/dispatch_test.go
git commit -m "dispatch: mastodon native quote carries media (drop reproduction degrade)"
```

---

### Task B3: Bluesky quote carries media (recordWithMedia)

**Files:**
- Modify: `internal/dispatch/adapters.go` — `QuoteBsky` (line 196-208)
- Test: `internal/bluesky/bluesky_test.go` (new client-level regression test)

- [ ] **Step 1: Write the failing test**

Add to `internal/bluesky/bluesky_test.go` (model on `TestPostCreatesRecordWithImageAndFacets`):

```go
func TestPostQuoteWithMediaUsesRecordWithMedia(t *testing.T) {
	var record map[string]any
	mux := http.NewServeMux()
	mux.HandleFunc("/xrpc/com.atproto.server.createSession", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"did": "did:plc:abc", "handle": "me.example.com", "accessJwt": "AAA"})
	})
	mux.HandleFunc("/xrpc/com.atproto.repo.uploadBlob", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"blob": map[string]any{"$type": "blob", "ref": map[string]any{"$link": "bafkX"}, "mimeType": "image/jpeg", "size": 1}})
	})
	mux.HandleFunc("/xrpc/com.atproto.repo.createRecord", func(w http.ResponseWriter, r *http.Request) {
		var body struct{ Record map[string]any }
		_ = json.NewDecoder(r.Body).Decode(&body)
		record = body.Record
		_ = json.NewEncoder(w).Encode(map[string]any{"uri": "at://did:plc:abc/app.bsky.feed.post/k", "cid": "bafy"})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	var pbuf bytes.Buffer
	_ = png.Encode(&pbuf, image.NewRGBA(image.Rect(0, 0, 8, 8)))

	c := New(srv.URL, "me.example.com", "app-pw")
	_, err := c.Post(context.Background(), Post{
		Text:   "my take",
		Quote:  &QuoteRef{URI: "at://did/app.bsky.feed.post/x", CID: "cidq"},
		Images: []Image{{Bytes: pbuf.Bytes(), Mime: "image/png", Alt: "a square"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	embed, _ := record["embed"].(map[string]any)
	if embed["$type"] != "app.bsky.embed.recordWithMedia" {
		t.Fatalf("expected recordWithMedia, got %v", embed["$type"])
	}
	media, _ := embed["media"].(map[string]any)
	if media["$type"] != "app.bsky.embed.images" {
		t.Errorf("media embed wrong: %v", media)
	}
	rec, _ := embed["record"].(map[string]any)
	inner, _ := rec["record"].(map[string]any)
	if inner["uri"] != "at://did/app.bsky.feed.post/x" {
		t.Errorf("quoted strongRef wrong: %v", rec)
	}
}
```

Imports needed (already present in `bluesky_test.go` from `TestPostCreatesRecordWithImageAndFacets`): `bytes`, `image`, `image/png`, `encoding/json`, `net/http`, `net/http/httptest`, `context`.

- [ ] **Step 2: Run test to verify it passes already (client side)**

Run: `go test ./internal/bluesky/ -run TestPostQuoteWithMediaUsesRecordWithMedia`
Expected: PASS — the client already builds `recordWithMedia`. This test is a regression guard. (If it unexpectedly fails, the client embed logic regressed — fix `internal/bluesky/bluesky.go:189-200` before continuing.)

- [ ] **Step 3: Make the adapter pass images through**

In `internal/dispatch/adapters.go`, replace `QuoteBsky`:

```go
func (a BlueskyAdapter) QuoteBsky(ctx context.Context, text string, o Overrides, imgs []Img, uri, cid string) (TargetResult, error) {
	var bi []bluesky.Image
	for _, im := range imgs {
		bi = append(bi, bluesky.Image{Bytes: im.Bytes, Mime: im.Mime, Alt: im.Alt})
	}
	bp := bluesky.Post{
		Text: text, Langs: o.Langs, Images: bi, Quote: &bluesky.QuoteRef{URI: uri, CID: cid},
		ReplyGate: bluesky.ParseReplyGate(o.BlueskyReply), DisableQuotes: o.BlueskyDisableQuotes,
	}
	res, err := a.C.Post(ctx, bp)
	if err != nil {
		return TargetResult{Platform: "bluesky"}, err
	}
	return TargetResult{Platform: "bluesky", Status: "success", RemoteID: res.RemoteID, RemoteURL: res.RemoteURL, CID: res.CID}, nil
}
```

- [ ] **Step 4: Run the suites**

Run: `go test ./internal/bluesky/ ./internal/dispatch/ && go vet ./internal/dispatch/`
Expected: PASS, clean. (The dispatch fakes for `QuoteBsky` already take `[]Img`, so no fake changes.)

- [ ] **Step 5: Commit**

```bash
git add internal/dispatch/adapters.go internal/bluesky/bluesky_test.go
git commit -m "bluesky: native quote carries attached media (recordWithMedia)"
```

---

### Task B4: Nostr quote carries media (imeta); thread imetas through runAction

**Files:**
- Modify: `internal/dispatch/dispatch.go` — `NostrActor` interface (line 118-121), `runAction` signature + nostr-quote case + the two `runAction` call sites (`runHead` line 299, `Interact` repost line 648), `runHead` pass-through
- Modify: `internal/dispatch/adapters.go` — `Quote` (line 241-253)
- Modify fakes: `internal/dispatch/interact_test.go:25`, `internal/dispatch/retry_test.go:122` & `:234`, `internal/dispatch/dispatch_test.go:28` & `:47` & `:195`, `internal/dispatch/scheduler_test.go:34`
- Modify test call sites: `internal/dispatch/interact_test.go` `runAction(...)` calls (lines ~62, ~78)

- [ ] **Step 1: Write the failing test**

Add to `internal/dispatch/interact_test.go`. First make `fakeNostrActor` capture imetas (it's at lines 12-27; add a field + update the `Quote` method):

```go
type fakeNostrActor struct {
	gotReply  *ReplyRef
	gotImetas []gonostr.Tag
}

func (f *fakeNostrActor) Quote(_ context.Context, _ string, _ string, _ string, _ string, imetas []gonostr.Tag) (TargetResult, error) {
	f.gotImetas = imetas
	return TargetResult{Platform: "nostr", Status: "success", RemoteID: "nq"}, nil
}
```

(Keep `PublishText`, `RebroadcastToRelay`, `Repost` as they are; only `Quote` gains the param.)

Then add:

```go
func TestInteractNostrQuoteCarriesImetas(t *testing.T) {
	na := &fakeNostrActor{}
	d := &Dispatcher{Nostr: na}
	imeta := gonostr.Tag{"imeta", "url https://blossom/x.jpg", "m image/jpeg"}
	d.Interact(context.Background(), InteractSpec{
		Action: actionQuote, SourcePlatform: "nostr",
		Ref:          InteractRef{EventID: "ev", Author: "auth"},
		Text:         "see this",
		MediaRecords: []store.Media{{BlossomURL: "https://blossom/x.jpg", Mime: "image/jpeg"}},
	})
	if len(na.gotImetas) != 1 {
		t.Fatalf("nostr quote should forward 1 imeta, got %d", len(na.gotImetas))
	}
	_ = imeta
}
```

Ensure the test file imports `gonostr "fiatjaf.com/nostr"` and `"github.com/geofox/publisher/internal/store"` (add if missing).

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/dispatch/ -run TestInteractNostrQuoteCarriesImetas`
Expected: FAIL to compile — `Quote` signature mismatch / `runAction` lacks imetas.

- [ ] **Step 3: Change interface, runAction, runHead, adapter**

`internal/dispatch/dispatch.go`, `NostrActor`:

```go
type NostrActor interface {
	Repost(ctx context.Context, eventID, author string, kind int, relayHint string) (TargetResult, error)
	Quote(ctx context.Context, text, eventID, author, relayHint string, imetas []gonostr.Tag) (TargetResult, error)
}
```

`runAction` — add an `imetas []gonostr.Tag` parameter (place it right after `imgs []Img`) and use it in the nostr-quote case:

```go
func (d *Dispatcher) runAction(ctx context.Context, action, plat, text string, ov Overrides, imgs []Img, imetas []gonostr.Tag, ref InteractRef) TargetResult {
```
```go
	case action == actionQuote && plat == "nostr":
		if d.Nostr != nil {
			r, err = d.Nostr.Quote(ctx, text, ref.EventID, ref.Author, relayHint(ref.RelayHints), imetas)
		} else {
			err = errors.New("nostr not configured")
		}
```

`runHead` — pass its `imetas` into `runAction`:

```go
	if head != nil && head.quote != nil {
		return d.runAction(ctx, actionQuote, plat, text, ov, imgs, imetas, *head.quote)
	}
```

`Interact` repost call site — pass `nil` imetas:

```go
		r := d.runAction(ctx, actionRepost, spec.SourcePlatform, "", spec.Overrides[spec.SourcePlatform], nil, nil, spec.Ref)
```

`internal/dispatch/adapters.go`, `Quote`:

```go
func (a NostrAdapter) Quote(ctx context.Context, text, eventID, author, relayHint string, imetas []gonostr.Tag) (TargetResult, error) {
	content := strings.TrimSpace(text)
	if mention := neventMention(eventID, author, relayHint); mention != "" {
		if content == "" {
			content = mention
		} else {
			content = content + "\n" + mention
		}
	}
	tags := []gonostr.Tag{{"q", eventID, relayHint, author}}
	res, err := a.P.Publish(ctx, pubnostr.PublishInput{Kind: 1, Text: content, Tags: tags, Imetas: imetas})
	return nostrResult(res, err)
}
```

- [ ] **Step 4: Update remaining nostr fakes and runAction test call sites**

Add the `[]gonostr.Tag` param to every other `Quote` fake:
- `internal/dispatch/retry_test.go:122` and `:234`
- `internal/dispatch/dispatch_test.go:28`, `:47`, `:195`
- `internal/dispatch/scheduler_test.go:34`

Each becomes: `func (...) Quote(context.Context, string, string, string, string, []gonostr.Tag) (TargetResult, error)`. Ensure each of those test files imports `gonostr "fiatjaf.com/nostr"` (most already do).

Update direct `runAction` calls in `internal/dispatch/interact_test.go` (the repost and quote-bsky tests near lines 62 and 78) to insert `nil` for the new imetas param, e.g.:

```go
	r := d.runAction(context.Background(), actionRepost, "bluesky", "", Overrides{}, nil, nil, InteractRef{URI: "at://x", CID: "c"})
```

- [ ] **Step 5: Run the whole backend**

Run: `go build ./... && go test ./... && go vet ./...`
Expected: PASS, clean.

- [ ] **Step 6: Commit**

```bash
git add internal/dispatch/ internal/dispatch/adapters.go
git commit -m "dispatch+nostr: native quote carries media via imeta (thread imetas through runAction)"
```

---

## Phase 2 — Frontend: interaction UX polish

> No JS unit-test runner exists in this repo; the established convention is `node --check <file>` per edited asset plus a manual verification checklist. Follow that here.

### Task F1: Draft safety + hand-off (Bundle A)

**Files:**
- Modify: `internal/web/assets/compose.js` — `startInteraction`, `exitInteraction`, `renderSrcBanner`
- Modify: `internal/web/assets/app.css` — `.srcb-x` (back affordance)

- [ ] **Step 1: Snapshot prior selection + guard the draft wipe in `startInteraction`**

Replace `startInteraction` body so it: (a) if the current draft is non-empty, confirms first via `confirmModal` and otherwise proceeds; (b) snapshots the pre-interaction platform set; (c) resets per-platform overrides; (d) flashes + scrolls to top. Use this structure:

```js
export function startInteraction(src, action) {
  const begin = () => {
    state.interaction = {
      action, platform: src.platform, ref: src.ref,
      sourcePreview: src.preview, sourceURL: src.preview.web_url, sourceAuthor: src.preview.author_handle,
      caps: src.caps, force: false,
      prevPlatforms: new Set(state.platforms), // restored on exit
    };
    state.master = "";
    state.images.forEach((i) => URL.revokeObjectURL(i.url));
    state.images = [];
    ORDER.forEach((p) => { state.ov[p] = defaultOv(p); }); // drop stale per-platform overrides
    state.platforms = new Set([src.platform]);
    state.focus = src.platform;
    const tab = document.querySelector('.tab[data-view="compose"]');
    if (tab) tab.click();
    const m = $("#master"); if (m) m.value = "";
    window.scrollTo({ top: 0, behavior: "smooth" });
    flash((action === "quote" ? "Quoting " : "Replying to ") + (src.preview.author_handle || src.platform));
    renderInteractionUI();
  };
  if (state.master.trim() || state.images.length) {
    confirmModal({
      title: "Replace your current draft?",
      body: "Starting a " + action + " will clear what you've written in Compose.",
      confirmText: "Replace draft",
      onConfirm: async () => { begin(); return true; },
    });
    return;
  }
  begin();
}
```

Add `defaultOv` to the `state.js` import in `compose.js` line 3, and `confirmModal` is already imported from `common.js` (line 2); `flash` and `ORDER` are already imported.

- [ ] **Step 2: Restore selection + clear draft in `exitInteraction`**

```js
export function exitInteraction() {
  const prev = state.interaction && state.interaction.prevPlatforms;
  state.interaction = null;
  state.master = "";
  state.images.forEach((i) => URL.revokeObjectURL(i.url));
  state.images = [];
  const m = $("#master"); if (m) m.value = "";
  state.platforms = prev && prev.size ? new Set(prev) : new Set(ORDER);
  if (!state.platforms.has(state.focus)) state.focus = [...state.platforms][0] || "bluesky";
  renderImages();
  renderInteractionUI();
}
```

- [ ] **Step 3: Clearer back affordance in `renderSrcBanner`**

Change the exit button text from `"× exit"` to an explicit cancel, and give it a class for styling:

```js
    el("button", { class: "srcb-x", type: "button", text: "← cancel", onclick: exitInteraction }),
```

In `internal/web/assets/app.css`, update `.srcb-x` (line 93) to read as a button:

```css
.srcb-x{margin-left:auto;font:inherit;font-size:12px;background:none;border:1px solid var(--line-2);border-radius:6px;padding:2px 8px;color:var(--muted);cursor:pointer}
.srcb-x:hover{border-color:var(--accent);color:var(--accent)}
```

- [ ] **Step 4: Verify**

Run: `node --check internal/web/assets/compose.js`
Expected: no output (valid).
Manual: with text in Compose, click Quote in Interact → a confirm appears; confirming clears the draft, scrolls to top, flashes, shows the banner. Cancelling the banner ("← cancel") restores the platforms that were selected before, and the textarea is empty.

- [ ] **Step 5: Commit**

```bash
git add internal/web/assets/compose.js internal/web/assets/app.css
git commit -m "web/compose: guard draft on hand-off; restore selection on exit; clearer cancel"
```

---

### Task F2: Count clarity on the quoted card (Bundle B)

**Files:**
- Modify: `internal/web/assets/preview.js` — `quotedCard`
- Modify: `internal/web/assets/app.css` — add `.pv-quoted-hint`

- [ ] **Step 1: Add the not-counted hint**

In `quotedCard()` (`preview.js:45-57`), after appending author/text/media and before `return card;`, append a hint that depends on the action and platform:

```js
  const verb = it && it.action === "reply" ? "Replied-to" : "Quoted";
  let hint = verb + " post is attached — only your text counts toward the limit.";
  if (it && it.platform === "nostr") hint += " A nostr: mention links it.";
  card.append(el("div", { class: "pv-quoted-hint muted", text: hint }));
```

- [ ] **Step 2: Style the hint**

In `internal/web/assets/app.css`, after the `.pv-quoted-media` rules (line ~375) add:

```css
.pv-quoted-hint{font-size:11px;margin-top:6px;font-style:italic}
```

- [ ] **Step 3: Verify**

Run: `node --check internal/web/assets/preview.js`
Manual: start a quote → the quoted card under the source-platform preview shows the italic hint; for a reply it reads "Replied-to post…"; for a Nostr quote it appends the nostr-mention note.

- [ ] **Step 4: Commit**

```bash
git add internal/web/assets/preview.js internal/web/assets/app.css
git commit -m "web/preview: explain that the quoted/replied post is not counted"
```

---

### Task F3: Repost result modal + consistent override (Bundle C)

**Files:**
- Modify: `internal/web/assets/compose.js` — export `showResultModal`
- Modify: `internal/web/assets/interact.js` — `doRepost` uses `confirmModal` + `showResultModal`

- [ ] **Step 1: Export the result modal**

In `internal/web/assets/compose.js`, change `function showResultModal(data) {` (line 383) to `export function showResultModal(data) {`.

- [ ] **Step 2: Rework `doRepost`**

In `internal/web/assets/interact.js`, import the shared helpers and replace `doRepost`:

Update the import line 2-3 region to add `confirmModal` and `showResultModal`:

```js
import { el, $, api, flash, confirmModal } from "./common.js";
import { startInteraction, showResultModal } from "./compose.js";
```

Replace `doRepost`:

```js
// doRepost posts a one-click repost via /api/interact (multipart spec, no media).
async function doRepost(s, cap) {
  const send = async (force) => {
    const fd = new FormData();
    fd.append("spec", JSON.stringify({
      action: "repost", platform: s.platform, ref: s.ref,
      source_url: s.preview.web_url, source_author: s.preview.author_handle, force: !!force,
    }));
    const r = await fetch("/api/interact", { method: "POST", body: fd, credentials: "same-origin" });
    const data = await r.json();
    if (!r.ok) throw new Error(data.error || ("HTTP " + r.status));
    showResultModal({ post_id: data.id, status: data.status, targets: data.targets });
  };
  if (!cap.allowed) {
    confirmModal({
      title: "Repost blocked",
      body: (cap.reason || "This repost is blocked") +
        (s.platform === "bluesky" ? " (Bluesky may silently drop it)." : "") + " Repost anyway?",
      confirmText: "Repost anyway",
      onConfirm: async () => { try { await send(true); } catch (e) { flash("Error: " + e.message); } return true; },
    });
    return;
  }
  try { await send(false); } catch (e) { flash("Error: " + e.message); }
}
```

- [ ] **Step 3: Verify no import cycle**

`interact.js` already imports from `compose.js` (`startInteraction`), and `compose.js` does not import from `interact.js`, so adding `showResultModal` to that existing edge introduces no cycle.

Run: `node --check internal/web/assets/interact.js && node --check internal/web/assets/compose.js`
Manual: click Repost on an allowed post → the result modal appears (tap-to-open-in-history), same as reply/quote. On a blocked post → branded confirm dialog (not `window.confirm`); confirming reposts and shows the modal.

- [ ] **Step 4: Commit**

```bash
git add internal/web/assets/interact.js internal/web/assets/compose.js
git commit -m "web/interact: repost shows result modal; blocked override uses confirmModal"
```

---

### Task F4: Chip labels + fan-out discoverability + richer banner (Bundle D)

**Files:**
- Modify: `internal/web/assets/compose.js` — `renderChips` (labels), `renderSrcBanner` (helper line + source media + original link)
- Modify: `internal/web/assets/app.css` — banner media/link/helper styles

- [ ] **Step 1: Relabel chips**

In `renderChips` (`compose.js:28-29`), change:

```js
    let label = META[p].label;
    if (it) label += p === it.platform ? " · native" : " · copy";
```

- [ ] **Step 2: Helper line + source media + original link in `renderSrcBanner`**

In `renderSrcBanner`, after the `.srcb-text` line and before the cap/force block, add a fan-out helper and (when present) the source media thumbnails + an open-original link:

```js
  const sp = it.sourcePreview || {};
  if (sp.media && sp.media.length) {
    const g = el("div", { class: "srcb-media" });
    for (const m of sp.media) g.append(el("img", { src: m.url, alt: m.alt || "" }));
    host.append(g);
  }
  const foot = el("div", { class: "srcb-foot" });
  foot.append(el("span", { class: "srcb-hint muted",
    text: it.platform.charAt(0).toUpperCase() + it.platform.slice(1) +
      " gets a native " + it.action + "; toggle other targets to also post a copy there." }));
  if (it.sourceURL) foot.append(el("a", { class: "srcb-link", href: it.sourceURL, target: "_blank", rel: "noopener", text: "open original ↗" }));
  host.append(foot);
```

(Place this block before `const cap = it.caps && it.caps[it.action];`.)

- [ ] **Step 3: Styles**

In `internal/web/assets/app.css`, after `.srcb-force` (line 95) add:

```css
.srcb-media{display:flex;gap:6px;flex-wrap:wrap;margin-top:8px}
.srcb-media img{max-width:90px;max-height:90px;border-radius:5px}
.srcb-foot{display:flex;align-items:baseline;gap:10px;flex-wrap:wrap;margin-top:8px}
.srcb-hint{font-size:12px}
.srcb-link{font-size:12px;margin-left:auto;white-space:nowrap}
```

- [ ] **Step 4: Verify**

Run: `node --check internal/web/assets/compose.js`
Manual: in interaction mode the source chip reads "Bluesky · native" and others read "… · copy"; the banner shows the source's images (if any), a one-line explanation of fan-out, and an "open original ↗" link.

- [ ] **Step 5: Commit**

```bash
git add internal/web/assets/compose.js internal/web/assets/app.css
git commit -m "web/compose: clearer chip labels, fan-out helper, richer source banner"
```

---

## Final verification (after all tasks)

- [ ] `go build ./... && go test ./... && go vet ./...` — all green.
- [ ] `for f in internal/web/assets/*.js; do node --check "$f"; done` — all valid.
- [ ] Dispatch a final holistic code review subagent over the whole branch diff (spec compliance + cross-file seams), per subagent-driven-development.
- [ ] HOLD: surface remarks and ask the user whether to release (v0.6.1 vs v0.7.0) — do NOT merge/tag/deploy without explicit approval.

## Self-review notes (author)

- Spec coverage: quote+media for Bluesky (B3), Mastodon (B1+B2), Nostr (B4); degrade removed (B2). Friendliness bundles A (F1), B (F2), C (F3), D (F4) all mapped.
- Type consistency: `QuoteStatus(...,imgs []Img)` and `Quote(...,imetas []gonostr.Tag)` and `runAction(...,imetas []gonostr.Tag,...)` defined once and updated at every call site + fake (B2, B4 enumerate them).
- The Mastodon degrade test is replaced, not left dangling (B2 Step 1).
- Frontend has no test runner → `node --check` + manual checklist (honest to the repo).
