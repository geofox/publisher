# Auto-Threading — Plan A: Splitter + Preview

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build the pure thread-splitter and a server-backed compose preview that shows exactly how a long draft will break into a per-platform reply-chain (with `k/n` numbering) — without changing how posts are sent yet.

**Architecture:** A dependency-free `internal/thread` package owns all splitting logic (manual `---` markers, natural-boundary wrapping, grapheme-aware sizing, and the counter-budget fixpoint). A new read-only `POST /api/thread-preview` endpoint runs it per selected platform. The compose SPA calls that endpoint (debounced) to render the split, plus a "number thread posts" toggle. No posting, store, or dispatch changes — those are Plan B.

**Tech Stack:** Go 1.26, `github.com/rivo/uniseg` (already a dep, used by the Bluesky client), vanilla-JS SPA.

**Spec:** `docs/superpowers/specs/2026-05-25-auto-threading-composer-design.md` (§2, §6 — splitter + preview only)

---

## File Structure

| File | Responsibility |
|---|---|
| `internal/thread/thread.go` | Pure splitter: `Split`, `Opts`, `LimitFor`, and unexported wrap/marker/number helpers |
| `internal/thread/thread_test.go` | Exhaustive splitter tests |
| `internal/api/api.go` | `handleThreadPreview` + route registration |
| `internal/api/thread_preview_test.go` | Endpoint tests |
| `internal/web/assets/index.html` | "number thread posts" toggle in compose |
| `internal/web/assets/preview.js` | Call `/api/thread-preview` (debounced) and render the segment chain |
| `internal/web/assets/app.css` | Segment-preview styles |
| `README.md` | Document `POST /api/thread-preview` |

---

## Task 1: Splitter core (markers, wrapping, limits) — no numbering yet

**Files:**
- Create: `internal/thread/thread.go`
- Test: `internal/thread/thread_test.go`

- [ ] **Step 1: Write the failing test**

```go
package thread

import (
	"strings"
	"testing"

	"github.com/rivo/uniseg"
)

func glen(s string) int { return uniseg.GraphemeClusterCount(s) }

func TestLimitFor(t *testing.T) {
	cases := map[string]int{"bluesky": 300, "mastodon": 500, "threads": 500, "nostr": 0, "weird": 0}
	for p, want := range cases {
		if got := LimitFor(p); got != want {
			t.Errorf("LimitFor(%q)=%d want %d", p, got, want)
		}
	}
}

func TestSplitNoLimitIsSingle(t *testing.T) {
	segs, warns := Split("a fairly long nostr note that has no length cap at all", 0, Opts{})
	if len(segs) != 1 || len(warns) != 0 {
		t.Fatalf("nostr no-limit should be 1 segment, got %v / warns %v", segs, warns)
	}
}

func TestSplitUnderLimitIsSingle(t *testing.T) {
	segs, _ := Split("short", 300, Opts{})
	if len(segs) != 1 || segs[0] != "short" {
		t.Fatalf("got %v", segs)
	}
}

func TestSplitMarkers(t *testing.T) {
	// Manual --- markers force breaks even when each piece fits the limit.
	segs, _ := Split("one\n---\ntwo\n---\nthree", 300, Opts{})
	if len(segs) != 3 || segs[0] != "one" || segs[1] != "two" || segs[2] != "three" {
		t.Fatalf("markers: got %v", segs)
	}
}

func TestSplitMarkersHonoredAtNoLimit(t *testing.T) {
	segs, _ := Split("a\n---\nb", 0, Opts{})
	if len(segs) != 2 {
		t.Fatalf("markers at no-limit: got %v", segs)
	}
}

func TestSplitWrapsAtWordBoundary(t *testing.T) {
	words := strings.Repeat("word ", 100) // 500 chars-ish, no word > limit
	segs, warns := Split(strings.TrimSpace(words), 50, Opts{})
	if len(segs) < 2 {
		t.Fatalf("expected multiple segments, got %d", len(segs))
	}
	for i, s := range segs {
		if glen(s) > 50 {
			t.Errorf("segment %d over limit: %d graphemes", i, glen(s))
		}
		if strings.HasPrefix(s, " ") || strings.HasSuffix(s, " ") {
			t.Errorf("segment %d has edge whitespace: %q", i, s)
		}
	}
}

func TestSplitNeverBreaksMidWord(t *testing.T) {
	segs, _ := Split("alpha bravo charlie delta echo foxtrot", 12, Opts{})
	for _, s := range segs {
		// every segment is whole words separated by single spaces
		for _, w := range strings.Fields(s) {
			if w == "" {
				t.Fatalf("empty word in %q", s)
			}
		}
	}
}

func TestSplitHardSplitsGiantToken(t *testing.T) {
	url := "https://example.com/" + strings.Repeat("x", 100)
	segs, warns := Split(url, 40, Opts{})
	if len(segs) < 2 {
		t.Fatalf("giant token should hard-split, got %d", len(segs))
	}
	if len(warns) == 0 {
		t.Errorf("expected a hard-split warning")
	}
	for _, s := range segs {
		if glen(s) > 40 {
			t.Errorf("hard-split segment over limit: %d", glen(s))
		}
	}
}

func TestSplitGraphemeAware(t *testing.T) {
	// Each emoji family is one grapheme cluster but many bytes/runes.
	fam := "👨‍👩‍👧‍👦"
	segs, _ := Split(strings.Repeat(fam+" ", 10), 3, Opts{})
	for _, s := range segs {
		if glen(s) > 3 {
			t.Errorf("emoji segment over grapheme limit: %d (%q)", glen(s), s)
		}
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/thread/ -v`
Expected: FAIL — package/`Split`/`LimitFor`/`Opts` undefined.

- [ ] **Step 3: Write the implementation**

```go
// Package thread splits a long draft into platform-sized segments for posting
// as a reply-chain. It is pure (no I/O) so all splitting logic — including the
// counter-budget fixpoint — lives in one well-tested place.
package thread

import (
	"strconv"
	"strings"

	"github.com/rivo/uniseg"
)

// Opts controls splitting behaviour.
type Opts struct {
	// Number appends a " k/n" counter to each segment when the resulting chain
	// has >= 2 segments. A single-segment chain is never numbered.
	Number bool
}

// LimitFor returns the per-platform grapheme limit. 0 means "no length limit"
// (Nostr). Mastodon's limit is instance-configurable; 500 is the common default.
func LimitFor(platform string) int {
	switch platform {
	case "bluesky":
		return 300
	case "mastodon", "threads":
		return 500
	default: // nostr and unknown
		return 0
	}
}

// Split returns the ordered segments for one platform plus any warnings (e.g. a
// token longer than the limit had to be hard-split). limit <= 0 means no length
// splitting (manual --- markers are still honoured).
func Split(text string, limit int, opts Opts) (segments []string, warnings []string) {
	segs, warns := splitAt(text, limit)
	if !opts.Number || limit <= 0 || len(segs) < 2 {
		return segs, warns
	}
	return number(text, limit)
}

// splitAt produces segments honouring markers and wrapping over-limit pieces to
// `limit`. No numbering.
func splitAt(text string, limit int) (segs []string, warns []string) {
	for _, u := range splitMarkers(text) {
		if u == "" {
			continue
		}
		if limit <= 0 || graphemeLen(u) <= limit {
			segs = append(segs, u)
			continue
		}
		chunks, w := packParagraphs(u, limit)
		segs = append(segs, chunks...)
		warns = append(warns, w...)
	}
	return segs, warns
}

// splitMarkers breaks text on lines that consist solely of "---", trimming each
// resulting user segment. With no markers it returns the whole (trimmed) text.
func splitMarkers(text string) []string {
	var out, cur []string
	for _, ln := range strings.Split(text, "\n") {
		if strings.TrimSpace(ln) == "---" {
			out = append(out, strings.TrimSpace(strings.Join(cur, "\n")))
			cur = nil
			continue
		}
		cur = append(cur, ln)
	}
	out = append(out, strings.TrimSpace(strings.Join(cur, "\n")))
	return out
}

// packParagraphs greedily packs paragraphs (\n\n) into <= limit chunks, falling
// back to sentence-, then word-, then hard-splitting for oversized pieces.
func packParagraphs(text string, limit int) ([]string, []string) {
	return packPieces(strings.Split(text, "\n\n"), "\n\n", limit, packSentences)
}

func packSentences(para string, limit int) ([]string, []string) {
	return packPieces(splitSentences(para), " ", limit, packWords)
}

func packWords(sent string, limit int) ([]string, []string) {
	return packPieces(strings.Fields(sent), " ", limit, hardSplit)
}

// packPieces greedily joins pieces with sep into chunks <= limit. A piece that
// itself exceeds limit is handed to fallback.
func packPieces(pieces []string, sep string, limit int, fallback func(string, int) ([]string, []string)) (chunks []string, warns []string) {
	cur := ""
	flush := func() {
		if cur != "" {
			chunks = append(chunks, cur)
			cur = ""
		}
	}
	for _, p := range pieces {
		if p == "" {
			continue
		}
		if graphemeLen(p) > limit {
			flush()
			fc, fw := fallback(p, limit)
			chunks = append(chunks, fc...)
			warns = append(warns, fw...)
			continue
		}
		cand := p
		if cur != "" {
			cand = cur + sep + p
		}
		if graphemeLen(cand) <= limit {
			cur = cand
		} else {
			flush()
			cur = p
		}
	}
	flush()
	return chunks, warns
}

// hardSplit chops a single token longer than limit at grapheme boundaries.
func hardSplit(s string, limit int) (chunks []string, warns []string) {
	warns = append(warns, "a word/URL longer than the "+strconv.Itoa(limit)+"-char limit was split")
	g := uniseg.NewGraphemes(s)
	var b strings.Builder
	n := 0
	for g.Next() {
		if n == limit {
			chunks = append(chunks, b.String())
			b.Reset()
			n = 0
		}
		b.WriteString(g.Str())
		n++
	}
	if b.Len() > 0 {
		chunks = append(chunks, b.String())
	}
	return chunks, warns
}

// splitSentences splits on ./!/? followed by whitespace, keeping the terminator.
func splitSentences(s string) []string {
	var out []string
	r := []rune(s)
	start := 0
	for i := 0; i < len(r); i++ {
		if (r[i] == '.' || r[i] == '!' || r[i] == '?') && i+1 < len(r) && (r[i+1] == ' ' || r[i+1] == '\n') {
			out = append(out, strings.TrimSpace(string(r[start:i+1])))
			start = i + 1
		}
	}
	if start < len(r) {
		out = append(out, strings.TrimSpace(string(r[start:])))
	}
	if len(out) == 0 {
		out = []string{strings.TrimSpace(s)}
	}
	return out
}

func graphemeLen(s string) int { return uniseg.GraphemeClusterCount(s) }

// number is implemented in Task 2.
func number(text string, limit int) ([]string, []string) { return splitAt(text, limit) }
```

(Note: `number` is a placeholder returning unnumbered output until Task 2 — keeps the package compiling. Task 2 replaces it.)

- [ ] **Step 4: Run tests**

Run: `go test ./internal/thread/ -v`
Expected: PASS (all Task-1 tests).

- [ ] **Step 5: Commit**

```bash
git add internal/thread/thread.go internal/thread/thread_test.go
git commit -m "thread: add splitter core (markers, wrapping, grapheme limits)"
```

---

## Task 2: Numbering with the counter-budget fixpoint

**Files:**
- Modify: `internal/thread/thread.go` (replace the `number` stub)
- Test: `internal/thread/thread_test.go` (add numbering tests)

- [ ] **Step 1: Write the failing test**

```go
func TestNumberingAppendsCounters(t *testing.T) {
	// 6 short words, limit small enough to force several segments.
	segs, _ := Split("alpha bravo charlie delta echo foxtrot", 12, Opts{Number: true})
	if len(segs) < 2 {
		t.Fatalf("expected a multi-segment chain, got %d", len(segs))
	}
	n := len(segs)
	for i, s := range segs {
		want := " " + itoa(i+1) + "/" + itoa(n)
		if !strings.HasSuffix(s, want) {
			t.Errorf("segment %d %q missing suffix %q", i, s, want)
		}
		if glen(s) > 12 {
			t.Errorf("numbered segment %d over limit: %d (%q)", i, glen(s), s)
		}
	}
}

func TestNumberingSkippedForSingleSegment(t *testing.T) {
	segs, _ := Split("short", 300, Opts{Number: true})
	if len(segs) != 1 || strings.Contains(segs[0], "/") {
		t.Fatalf("single segment must not be numbered: %v", segs)
	}
}

func TestNumberingNeverExceedsLimit(t *testing.T) {
	// Many segments so the counter width (e.g. " 12/12") meaningfully eats budget.
	body := strings.TrimSpace(strings.Repeat("lorem ipsum dolor ", 60))
	segs, _ := Split(body, 40, Opts{Number: true})
	for i, s := range segs {
		if glen(s) > 40 {
			t.Fatalf("numbered segment %d exceeds limit: %d graphemes (%q)", i, glen(s), s)
		}
	}
	last := segs[len(segs)-1]
	if !strings.HasSuffix(last, "/"+itoa(len(segs))) {
		t.Errorf("last counter wrong: %q (n=%d)", last, len(segs))
	}
}

func itoa(n int) string { return strconv.Itoa(n) }
```

(Add `"strconv"` to the test file imports.)

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/thread/ -run TestNumbering -v`
Expected: FAIL — counters absent (the `number` stub returns unnumbered output).

- [ ] **Step 3: Replace the `number` stub**

In `internal/thread/thread.go`, replace the placeholder `number` function with:

```go
// number computes a stable segment count under the counter-budget constraint
// (the " k/n" suffix consumes graphemes, and its width depends on n, which
// depends on the budget) by iterating to a fixpoint, then appends the counters.
func number(text string, limit int) ([]string, []string) {
	segs, warns := splitAt(text, limit)
	n := len(segs)
	for i := 0; i < 6; i++ {
		w := counterWidth(n)
		eff := limit - w
		if eff < 1 {
			return segs, warns // pathological tiny limit: skip numbering
		}
		segs, warns = splitAt(text, eff)
		if len(segs) < 2 {
			return splitAt(text, limit) // reservation collapsed it; return unnumbered
		}
		if len(segs) == n {
			break
		}
		n = len(segs)
	}
	out := make([]string, len(segs))
	for i, s := range segs {
		out[i] = s + " " + strconv.Itoa(i+1) + "/" + strconv.Itoa(len(segs))
	}
	return out, warns
}

// counterWidth is the worst-case grapheme cost of a " k/n" suffix: a space, a
// slash, and two numbers each up to len(n) digits.
func counterWidth(n int) int {
	d := len(strconv.Itoa(n))
	return 2 + 2*d
}
```

- [ ] **Step 4: Run tests**

Run: `go test ./internal/thread/ -v`
Expected: PASS (all splitter tests incl. numbering). The fixpoint guarantees the invariant the tests assert: every numbered segment is `<= limit`.

- [ ] **Step 5: Commit**

```bash
git add internal/thread/thread.go internal/thread/thread_test.go
git commit -m "thread: add k/n numbering with counter-budget fixpoint"
```

---

## Task 3: `POST /api/thread-preview` endpoint

**Files:**
- Modify: `internal/api/api.go` (import, route, handler)
- Test: `internal/api/thread_preview_test.go`

- [ ] **Step 1: Write the failing test**

```go
package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func postPreview(t *testing.T, body string) *httptest.ResponseRecorder {
	t.Helper()
	a := &API{}
	req := httptest.NewRequest(http.MethodPost, "/api/thread-preview", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	a.Routes().ServeHTTP(rec, req)
	return rec
}

func TestThreadPreviewSplitsPerPlatform(t *testing.T) {
	long := strings.TrimSpace(strings.Repeat("word ", 120)) // ~600 chars
	body, _ := json.Marshal(map[string]any{
		"text": long, "platforms": []string{"bluesky", "nostr"}, "number": true,
	})
	rec := postPreview(t, string(body))
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	var out struct {
		Previews []struct {
			Platform string   `json:"platform"`
			Count    int      `json:"count"`
			Segments []string `json:"segments"`
		} `json:"previews"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	byp := map[string]int{}
	for _, p := range out.Previews {
		byp[p.Platform] = p.Count
	}
	if byp["bluesky"] < 2 {
		t.Errorf("bluesky should thread: count=%d", byp["bluesky"])
	}
	if byp["nostr"] != 1 {
		t.Errorf("nostr should be single: count=%d", byp["nostr"])
	}
}

func TestThreadPreviewEmptyTextIs400(t *testing.T) {
	body, _ := json.Marshal(map[string]any{"text": "  ", "platforms": []string{"bluesky"}})
	if rec := postPreview(t, string(body)); rec.Code != http.StatusBadRequest {
		t.Fatalf("empty text should be 400, got %d", rec.Code)
	}
}

func TestThreadPreviewBadJSONIs400(t *testing.T) {
	if rec := postPreview(t, "{not json"); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad json should be 400, got %d", rec.Code)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/api/ -run TestThreadPreview -v`
Expected: FAIL — route returns 404 (handler not registered).

- [ ] **Step 3: Add import, route, handler**

In `internal/api/api.go`, add to the import block:
```go
	"github.com/geofox/publisher/internal/thread"
```

Register the route in `Routes()` (next to the other `POST /api/*` routes, before `mux.Handle("/", ...)`):
```go
	mux.HandleFunc("POST /api/thread-preview", a.handleThreadPreview)
```

Add the handler (place near the other handlers, e.g. after `handleAPIPost`):
```go
// ─── POST /api/thread-preview ──────────────────────────────────────────────

func (a *API) handleThreadPreview(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 256<<10) // 256 KB
	var req struct {
		Text      string   `json:"text"`
		Platforms []string `json:"platforms"`
		Number    bool     `json:"number"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	if strings.TrimSpace(req.Text) == "" {
		httpx.WriteError(w, http.StatusBadRequest, "text is required")
		return
	}
	type preview struct {
		Platform string   `json:"platform"`
		Count    int      `json:"count"`
		Segments []string `json:"segments"`
		Warnings []string `json:"warnings,omitempty"`
	}
	out := struct {
		Previews []preview `json:"previews"`
	}{Previews: []preview{}}
	for _, p := range req.Platforms {
		segs, warns := thread.Split(req.Text, thread.LimitFor(p), thread.Opts{Number: req.Number})
		out.Previews = append(out.Previews, preview{
			Platform: p, Count: len(segs), Segments: segs, Warnings: warns,
		})
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}
```

(`json`, `net/http`, `strings`, and `httpx` are already imported in api.go.)

- [ ] **Step 4: Run tests**

Run: `go test ./internal/api/ -run TestThreadPreview -v` then `go test ./internal/api/`
Expected: PASS (new tests + existing api tests unchanged).

- [ ] **Step 5: Commit**

```bash
git add internal/api/api.go internal/api/thread_preview_test.go
git commit -m "api: add POST /api/thread-preview (per-platform split preview)"
```

---

## Task 4: Compose UI — numbering toggle + threaded preview rendering

**Files:**
- Modify: `internal/web/assets/index.html` (numbering toggle)
- Modify: `internal/web/assets/preview.js` (call endpoint, render chain)
- Modify: `internal/web/assets/app.css` (segment styles)

- [ ] **Step 1: Read the existing SPA conventions FIRST**

Read these to match patterns exactly before editing:
- `internal/web/assets/preview.js` — the `renderPreview()` function, how it's exported and called, how it reads the focused platform and the master text.
- `internal/web/assets/common.js` — `el()` DOM builder, `gcount()`, the `api()` fetch helper, `META` (per-platform labels + grapheme limits), `ORDER`.
- `internal/web/assets/compose.js` — how state (text, focused platform, platforms) is held and how `renderPreview` is triggered on input; whether there's a debounce utility.
- `internal/web/assets/index.html` — the compose section (`#master`, `#preview`, `#cards`, `#chips`) to place the toggle.

- [ ] **Step 2: Add the numbering toggle to `index.html`**

In the compose-left column (near `#chips`/`#cards`, before `#submit`), add:
```html
      <label class="threadnum"><input id="threadnum" type="checkbox" checked> number thread posts (1/n)</label>
```

- [ ] **Step 3: Add a threaded-preview renderer to `preview.js`**

Add this self-contained function and wire it into the existing preview flow. It fetches the split for the focused platform and renders the chain; when the platform returns a single segment it falls back to the existing single-post preview. Match the file's existing import style for `el`/`api`/`$`.

```js
// threadPreview fetches the per-platform split for the focused platform and
// renders the segment chain into `container`. Returns true if it rendered a
// multi-segment thread, false if the draft fits in one post (caller renders the
// normal single-post preview).
export async function threadPreview(container, text, platform, number) {
  if (!text.trim()) return false;
  let data;
  try {
    const resp = await fetch("/api/thread-preview", {
      method: "POST",
      headers: { "content-type": "application/json" },
      body: JSON.stringify({ text, platforms: [platform], number }),
    });
    data = await resp.json();
  } catch {
    return false; // network hiccup → fall back to normal preview
  }
  const pv = (data.previews || []).find((p) => p.platform === platform);
  if (!pv || pv.count < 2) return false;

  container.innerHTML = "";
  const head = document.createElement("div");
  head.className = "pv-thread-head";
  head.textContent = `${pv.count} posts`;
  container.appendChild(head);
  pv.segments.forEach((seg, i) => {
    const card = document.createElement("div");
    card.className = "pv-seg";
    const n = document.createElement("span");
    n.className = "pv-seg-n";
    n.textContent = `${i + 1}/${pv.count}`;
    const body = document.createElement("div");
    body.className = "pv-seg-body";
    body.textContent = seg; // textContent => no HTML injection
    card.appendChild(n);
    card.appendChild(body);
    container.appendChild(card);
  });
  (pv.warnings || []).forEach((wmsg) => {
    const wd = document.createElement("div");
    wd.className = "pv-warn";
    wd.textContent = wmsg;
    container.appendChild(wd);
  });
  return true;
}
```

Then, in the existing `renderPreview()` flow: read the toggle (`document.getElementById("threadnum")?.checked ?? true`), and for the focused platform call `threadPreview(previewEl, masterText, focusedPlatform, number)`; if it returns `false`, render the existing single-post preview as before. Debounce the call ~250ms on input (use the file's existing debounce if present, else a small local `setTimeout` guard) so it doesn't fire a request per keystroke. Re-run preview when the toggle changes (add a `change` listener on `#threadnum`).

IMPORTANT (CSP/XSS): only ever assign untrusted text via `textContent` (as above), never `innerHTML`, consistent with the strict CSP. Do not introduce inline scripts/styles.

- [ ] **Step 4: Add CSS to `app.css`**

Append (use the project's actual CSS variables — confirm names at the top of app.css; these mirror the verify-tab block):
```css
/* thread preview */
.threadnum { display:flex; align-items:center; gap:6px; margin:8px 0; color:var(--muted); font-size:13px; }
.pv-thread-head { color:var(--muted); font-size:12px; margin-bottom:6px; }
.pv-seg { border:1px solid var(--line); border-radius:8px; padding:8px 10px; margin-bottom:8px; }
.pv-seg-n { color:var(--accent); font-size:11px; font-variant-numeric:tabular-nums; }
.pv-seg-body { white-space:pre-wrap; margin-top:4px; }
.pv-warn { color:var(--warn); font-size:12px; margin-top:6px; }
```

- [ ] **Step 5: Build, test, manual check**

Run:
```bash
go test ./internal/web/
go build ./cmd/publisher
```
Expected: PASS + clean build. Then run the server and open compose: type a >300-grapheme draft with Bluesky focused → preview shows a numbered `1/n … n/n` chain; switch focus to Nostr → single post; toggle "number thread posts" off → counters disappear; add `---` lines → forced breaks appear.

- [ ] **Step 6: Commit**

```bash
git add internal/web/assets/index.html internal/web/assets/preview.js internal/web/assets/app.css
git commit -m "web: thread-aware compose preview + numbering toggle"
```

---

## Task 5: Docs + full verification

**Files:**
- Modify: `README.md`

- [ ] **Step 1: Document the endpoint**

Add a `### POST /api/thread-preview` subsection under the HTTP API section:
```markdown
### `POST /api/thread-preview`

Preview how a draft would split into a per-platform reply-chain (read-only; no
posting). Used by the compose preview.

​```json
{ "text": "<master draft>", "platforms": ["bluesky","mastodon","threads","nostr"], "number": true }
​```

Returns one entry per platform with the computed `segments` (each ≤ that
platform's limit — Bluesky 300 graphemes, Mastodon/Threads 500, Nostr
unlimited), the `count`, and any `warnings` (e.g. a URL longer than the limit
had to be hard-split). With `number`, a chain of ≥2 segments gets per-platform
` k/n` counters. Manual `---` lines in the text force breaks.
​```

(Use real triple-backticks for the json fence.)

- [ ] **Step 2: Full verification**

Run:
```bash
go test ./...
go vet ./...
go build ./cmd/publisher
```
Expected: all packages PASS, vet clean, build succeeds. If any failure, STOP and report it — do not commit over a broken build.

- [ ] **Step 3: Commit**

```bash
git add README.md
git commit -m "docs: document /api/thread-preview"
```

---

## Self-Review notes (for the implementer)

- **Spec coverage (Plan A scope = spec §2 + §6):** splitter with markers/natural-boundaries/grapheme-awareness (Task 1), `k/n` numbering + fixpoint (Task 2), `POST /api/thread-preview` (Task 3), compose preview + numbering toggle (Task 4), docs (Task 5). Posting/store/dispatch/history are intentionally **Plan B**.
- **`number` stub:** Task 1 ships a placeholder `number` so the package compiles; Task 2 replaces it. If executing out of order, Task 2 must land before numbering works.
- **Limits:** Mastodon/Threads hardcoded at 500 (instance-configurable in reality) — a documented v1 simplification; Plan B may revisit if needed.
- **Single source of truth:** the splitter lives only in Go; the SPA calls the endpoint rather than re-implementing it, so numbering/fixpoint logic never diverges.
