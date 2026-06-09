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

// MaxImagesFor returns the per-platform image cap for one post. 0 means "no
// cap" (Nostr appends URLs/imeta, no attachment slots). Bluesky's 10 is the
// app.bsky.embed.gallery client soft limit (1.123+); Mastodon's 4 is the
// vanilla server default; Threads carousels take 10 comfortably (API max 20).
func MaxImagesFor(platform string) int {
	switch platform {
	case "bluesky", "threads":
		return 10
	case "mastodon":
		return 4
	default: // nostr and unknown
		return 0
	}
}

// PlanMedia assigns nImages images to a chain's segments: up to cap per
// segment, filling in order from the head. If images outrun the text
// segments, image-only segments are appended, so len(result) >= nSegments.
// cap <= 0 puts everything on the head (today's behavior for Nostr).
// Pure and deterministic: resume/republish re-derive the identical plan.
func PlanMedia(nImages, nSegments, cap int) []int {
	if nSegments < 1 {
		nSegments = 1
	}
	counts := make([]int, nSegments)
	if nImages <= 0 {
		return counts
	}
	if cap <= 0 {
		counts[0] = nImages
		return counts
	}
	rem := nImages
	for i := 0; i < nSegments && rem > 0; i++ {
		c := cap
		if rem < c {
			c = rem
		}
		counts[i] = c
		rem -= c
	}
	for rem > 0 {
		c := cap
		if rem < c {
			c = rem
		}
		counts = append(counts, c)
		rem -= c
	}
	return counts
}

// Split returns the ordered segments for one platform plus any warnings (e.g. a
// token longer than the limit had to be hard-split). limit <= 0 means no length
// splitting (manual --- markers are still honoured).
func Split(text string, limit int, opts Opts) (segments []string, warnings []string) {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	segs, warns := splitAt(text, limit)
	if !opts.Number || len(segs) < 2 {
		return segs, warns
	}
	if limit <= 0 {
		// No length budget (Nostr): segments are marker-driven and fixed, so the
		// counter has no budget to reserve — just append it.
		return appendCounters(segs), warns
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
	// NOTE: strings.Fields collapses runs of internal whitespace to single
	// spaces. This is only reached when a single sentence exceeds the limit, and
	// is acceptable for social-post drafts (no meaningful multi-space runs).
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

// number computes a stable segment count under the counter-budget constraint
// (the " k/n" suffix consumes graphemes, and its width depends on n, which
// depends on the budget) by iterating to a fixpoint, then appends the counters.
//
// Convergence: splitAt is monotone (smaller limit ⇒ same-or-more segments), so n
// only grows across iterations, and counterWidth is a step function of n's digit
// count. n stabilizes once it stops crossing a digit boundary (9→10, 99→100,
// 999→1000) — at most three transitions, each costing one iteration, so the loop
// converges in ≤4 passes; the cap of 6 is a safety margin.
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
		// Defensive: Split only calls number when the full-budget split already
		// yields >=2 segments, and reducing the budget can only add segments, so
		// this never fires — but guard rather than emit a "1/1" lone post.
		if len(segs) < 2 {
			return splitAt(text, limit)
		}
		if len(segs) == n {
			break
		}
		n = len(segs)
	}
	return appendCounters(segs), warns
}

// appendCounters appends a " k/n" suffix to each segment in a chain.
func appendCounters(segs []string) []string {
	out := make([]string, len(segs))
	for i, s := range segs {
		out[i] = s + " " + strconv.Itoa(i+1) + "/" + strconv.Itoa(len(segs))
	}
	return out
}

// counterWidth is the worst-case grapheme cost of a " k/n" suffix: a space, a
// slash, and two numbers each up to len(n) digits.
func counterWidth(n int) int {
	d := len(strconv.Itoa(n))
	return 2 + 2*d
}
