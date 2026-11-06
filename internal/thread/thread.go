// Package thread plans how one text is posted as a thread on one
// destination: where it breaks (by hand, then to the destination's length
// limit), which media go with which post, and the " k/n" counters. It is
// pure (no I/O), so the API's preview, validation and the engine all plan
// the same way.
//
// Positions (breaks, media, item ranges) are Unicode code point offsets
// into the text.
package thread

import (
	"slices"
	"strconv"
	"strings"
	"unicode"

	"github.com/rivo/uniseg"

	"github.com/geofox/publisher/internal/destination"
)

// Layout is what the author decided about a text: where it breaks by hand
// and where each media goes.
type Layout struct {
	// Breaks are hand-made breaks: code point offsets, each starting a new
	// post. The planner ignores ones out of range and sorts the rest.
	Breaks []int
	// MediaAt has one position per media: the media goes with the post that
	// holds it (0, or none given: the first post).
	MediaAt []int
}

// Rules are the destination's: how long a post may be and how it is
// counted, how many images a post takes, and whether posts are numbered.
type Rules struct {
	// Limit is the most a post may count; 0 = unlimited (nothing is split
	// for length).
	Limit    int
	Counting destination.Counting
	// ImgCap is the most images one post takes; 0 = unlimited.
	ImgCap int
	// Number appends a " k/n" counter to every post of a thread of two or
	// more.
	Number bool
	// MaxLinks caps the distinct URLs in one post; 0 = no cap. A post over
	// it is reported in Item.Problems (splitting doesn't fix it).
	MaxLinks int
}

// Item is one post of the plan.
type Item struct {
	// Text is what is posted, counter included.
	Text string
	// From and To are the code points of the text the post carries,
	// counter excluded. A media-only post appended for media that didn't
	// fit has From == To == the text's length.
	From, To int
	// Manual is set when a hand-made break starts this post.
	Manual bool
	// Media are the media (by index) the post carries.
	Media []int
	// Length is Text as the destination counts it.
	Length int
	// Problems are what the destination will refuse about this post.
	Problems []string
}

// Plan plans text under the rules. Every text gets at least one item. The
// planner is deterministic: planning the same input again (a retry, a
// restart) gives the same items.
func Plan(text string, lay Layout, r Rules) (items []Item, warnings []string) {
	rs := []rune(text)
	p := planner{rs: rs, r: r}
	pieces := p.pieces(lay.Breaks)

	nMedia := len(lay.MediaAt)
	ranges, manual, warns := p.splitAll(pieces, r.Limit)
	total := postsFor(ranges, lay.MediaAt, r.ImgCap)
	if r.Number && total >= 2 && r.Limit > 0 {
		// The counter costs room, and its width depends on the total, which
		// depends on the room: iterate to a fixpoint. Splitting is monotone
		// (less room, same or more posts), so the total only grows; it
		// settles once it stops crossing a digit boundary.
		for i := 0; i < 8; i++ {
			eff := r.Limit - counterWidth(total)
			if eff < 1 {
				break // a pathological tiny limit: no room for counters
			}
			ranges, manual, warns = p.splitAll(pieces, eff)
			next := postsFor(ranges, lay.MediaAt, r.ImgCap)
			if next == total {
				break
			}
			total = next
		}
	}
	warnings = warns

	end := len(rs)
	numbered := r.Number && total >= 2
	for i := 0; i < total; i++ {
		it := Item{From: end, To: end, Media: []int{}}
		if i < len(ranges) {
			it.From, it.To, it.Manual = ranges[i][0], ranges[i][1], manual[i]
		}
		body := string(rs[it.From:it.To])
		it.Text = body
		if numbered {
			c := strconv.Itoa(i+1) + "/" + strconv.Itoa(total)
			if body == "" {
				it.Text = c
			} else {
				it.Text = body + " " + c
			}
		}
		it.Length = r.Counting.Count(it.Text)
		if r.MaxLinks > 0 {
			if n := destination.CountURLs(it.Text); n > r.MaxLinks {
				it.Problems = append(it.Problems, "has "+strconv.Itoa(n)+" links; at most "+strconv.Itoa(r.MaxLinks)+" per post")
			}
		}
		items = append(items, it)
	}

	// Media: each goes with the text post holding its position, spilling
	// forward when that post is full.
	if nMedia > 0 {
		anchors := make([]int, nMedia)
		for m, at := range lay.MediaAt {
			anchors[m] = postAt(ranges, at)
		}
		postPart := make([]int, total)
		for j := range postPart {
			postPart[j] = min(j, max(len(ranges)-1, 0))
		}
		for j, idx := range PlaceMedia(anchors, postPart, r.ImgCap) {
			items[j].Media = idx
		}
	}
	return items, warnings
}

// postAt is the text post holding position at: the last one starting at or
// before it, where a post starts right after the one before it ends (the
// whitespace between two posts, where a hand-made break sits, belongs to
// the later one).
func postAt(ranges [][2]int, at int) int {
	j := 0
	for i, rg := range ranges {
		if rg[0] <= at || (i > 0 && at > ranges[i-1][1]) {
			j = i
		}
	}
	return j
}

// postsFor is how many posts a plan of the text posts in ranges needs for
// media at positions mediaAt, at most imgCap per post: each goes on the
// post holding its position or, that one full, a later one; media-only posts
// are added at the end for what doesn't fit. PlaceMedia fills the same way.
func postsFor(ranges [][2]int, mediaAt []int, imgCap int) int {
	n := max(len(ranges), 1)
	if len(mediaAt) == 0 || imgCap <= 0 {
		return n
	}
	used := make([]int, n)
	put := func(from int) {
		for j := from; ; j++ {
			if j == len(used) {
				used = append(used, 0)
			}
			if used[j] < imgCap {
				used[j]++
				return
			}
		}
	}
	// Anchored media first, then those on the first post, as PlaceMedia.
	for _, at := range mediaAt {
		if a := postAt(ranges, at); a > 0 {
			put(a)
		}
	}
	for _, at := range mediaAt {
		if postAt(ranges, at) == 0 {
			put(0)
		}
	}
	return len(used)
}

type planner struct {
	rs []rune
	r  Rules
}

// count is a range's length as the destination counts it.
func (p *planner) count(a, b int) int { return p.r.Counting.Count(string(p.rs[a:b])) }

// pieces cuts the text at the hand-made breaks.
func (p *planner) pieces(breaks []int) [][2]int {
	cuts := []int{}
	for _, b := range breaks {
		if b > 0 && b < len(p.rs) {
			cuts = append(cuts, b)
		}
	}
	slices.Sort(cuts)
	cuts = slices.Compact(cuts)
	out := make([][2]int, 0, len(cuts)+1)
	start := 0
	for _, c := range cuts {
		out = append(out, [2]int{start, c})
		start = c
	}
	return append(out, [2]int{start, len(p.rs)})
}

// splitAll splits every piece to limit. manual[i] is set on the first post
// of every piece after the first that has text. A text with no text at all
// is one empty post.
func (p *planner) splitAll(pieces [][2]int, limit int) (ranges [][2]int, manual []bool, warns []string) {
	for k, pc := range pieces {
		a, b := p.trim(pc[0], pc[1])
		if a == b {
			continue
		}
		chunks, w := p.split(a, b, limit)
		warns = append(warns, w...)
		for i, c := range chunks {
			ranges = append(ranges, c)
			manual = append(manual, i == 0 && k > 0 && len(ranges) > 1)
		}
	}
	if len(ranges) == 0 {
		a, _ := p.trim(0, len(p.rs))
		ranges, manual = [][2]int{{a, a}}, []bool{false}
	}
	return ranges, manual, warns
}

func (p *planner) trim(a, b int) (int, int) {
	for a < b && unicode.IsSpace(p.rs[a]) {
		a++
	}
	for b > a && unicode.IsSpace(p.rs[b-1]) {
		b--
	}
	return a, b
}

// split splits [a, b) into posts of at most limit: whole paragraphs where
// they fit, else sentences, else words, else graphemes. Then it evens out
// the end of the thread (rebalance).
func (p *planner) split(a, b, limit int) ([][2]int, []string) {
	if limit <= 0 || p.count(a, b) <= limit {
		return [][2]int{{a, b}}, nil
	}
	var warns []string
	chunks := p.pack(p.paragraphs(a, b), limit, func(a, b int) [][2]int {
		return p.pack(p.sentences(a, b), limit, func(a, b int) [][2]int {
			return p.pack(p.words(a, b), limit, func(a, b int) [][2]int {
				warns = append(warns, "a word or link longer than the "+strconv.Itoa(limit)+"-character limit was split")
				return p.hard(a, b, limit)
			})
		})
	})
	return p.rebalance(chunks, limit), slices.Compact(warns)
}

// pack greedily joins consecutive pieces (keeping the text between them)
// while they fit. A piece that doesn't fit alone goes to fallback.
func (p *planner) pack(pieces [][2]int, limit int, fallback func(a, b int) [][2]int) [][2]int {
	var out [][2]int
	cur := [2]int{-1, -1}
	flush := func() {
		if cur[0] >= 0 {
			out = append(out, cur)
			cur = [2]int{-1, -1}
		}
	}
	for _, pc := range pieces {
		if p.count(pc[0], pc[1]) > limit {
			flush()
			out = append(out, fallback(pc[0], pc[1])...)
			continue
		}
		if cur[0] >= 0 && p.count(cur[0], pc[1]) <= limit {
			cur[1] = pc[1]
			continue
		}
		flush()
		cur = pc
	}
	flush()
	return out
}

// paragraphs are separated by a blank line.
func (p *planner) paragraphs(a, b int) [][2]int {
	var out [][2]int
	start := a
	for i := a; i < b; i++ {
		if p.rs[i] != '\n' {
			continue
		}
		j := i + 1
		for j < b && (p.rs[j] == ' ' || p.rs[j] == '\t' || p.rs[j] == '\r') {
			j++
		}
		if j < b && p.rs[j] == '\n' {
			out = p.appendTrimmed(out, start, i)
			for j < b && unicode.IsSpace(p.rs[j]) {
				j++
			}
			start, i = j, j-1
		}
	}
	return p.appendTrimmed(out, start, b)
}

// sentences end at ., ! or ? (and any closing quotes or brackets) followed
// by whitespace.
func (p *planner) sentences(a, b int) [][2]int {
	var out [][2]int
	start := a
	for i := a; i < b; i++ {
		if end := p.sentenceEnd(i, b); end > 0 {
			out = p.appendTrimmed(out, start, end)
			start = end
		}
	}
	out = p.appendTrimmed(out, start, b)
	if len(out) == 0 {
		return [][2]int{{a, b}}
	}
	return out
}

// sentenceEnd answers the end of a sentence whose terminator is at i (just
// past any closing quotes or brackets), when whitespace follows; else 0.
func (p *planner) sentenceEnd(i, b int) int {
	if r := p.rs[i]; r != '.' && r != '!' && r != '?' && r != '…' {
		return 0
	}
	j := i + 1
	for j < b && strings.ContainsRune(`"')]»”’`, p.rs[j]) {
		j++
	}
	if j < b && unicode.IsSpace(p.rs[j]) {
		return j
	}
	return 0
}

func (p *planner) words(a, b int) [][2]int {
	var out [][2]int
	start := -1
	for i := a; i < b; i++ {
		if unicode.IsSpace(p.rs[i]) {
			if start >= 0 {
				out = append(out, [2]int{start, i})
				start = -1
			}
		} else if start < 0 {
			start = i
		}
	}
	if start >= 0 {
		out = append(out, [2]int{start, b})
	}
	return out
}

// hard cuts a word longer than limit between graphemes.
func (p *planner) hard(a, b, limit int) [][2]int {
	// Grapheme boundaries of [a, b).
	bounds := []int{a}
	g := uniseg.NewGraphemes(string(p.rs[a:b]))
	pos := a
	for g.Next() {
		pos += len([]rune(g.Str()))
		bounds = append(bounds, pos)
	}
	// Each post takes the most graphemes that fit: found by binary search
	// (the count grows with the text, but for a mention rule that can
	// shrink it, so the pick is checked and stepped back), which keeps a
	// long unbroken run O(n log n) rather than O(n × limit).
	var out [][2]int
	i := 0
	for i < len(bounds)-1 {
		// Gallop from about a limit's worth (a URL rule can make many
		// graphemes cost little), then search between the last fit and
		// the first miss.
		last := len(bounds) - 1
		lo, step := i+1, max(limit, 1) // the first grapheme always goes
		for lo < last && p.count(bounds[i], bounds[min(lo+step, last)]) <= limit {
			lo = min(lo+step, last)
			step *= 2
		}
		hi := min(lo+step, last)
		for lo < hi {
			mid := (lo + hi + 1) / 2
			if p.count(bounds[i], bounds[mid]) <= limit {
				lo = mid
			} else {
				hi = mid - 1
			}
		}
		for lo > i+1 && p.count(bounds[i], bounds[lo]) > limit {
			lo--
		}
		out = append(out, [2]int{bounds[i], bounds[lo]})
		i = lo
	}
	return out
}

func (p *planner) appendTrimmed(out [][2]int, a, b int) [][2]int {
	if a, b = p.trim(a, b); a < b {
		out = append(out, [2]int{a, b})
	}
	return out
}

// rebalance evens out the end of a thread. A last post that is only links,
// hashtags or mentions takes the last word (or sentence) of the post before
// it, so it doesn't stand alone. A last post under a quarter of the limit
// takes the last sentence of the post before it, when both still fit and
// the one before keeps at least a quarter.
func (p *planner) rebalance(chunks [][2]int, limit int) [][2]int {
	n := len(chunks)
	if n < 2 {
		return chunks
	}
	prev, last := chunks[n-2], chunks[n-1]
	try := func(cut int) bool {
		pa, pb := p.trim(prev[0], cut)
		la, lb := p.trim(cut, last[1])
		if pa >= pb || la >= lb || p.count(la, lb) > limit {
			return false
		}
		chunks[n-2], chunks[n-1] = [2]int{pa, pb}, [2]int{la, lb}
		return true
	}
	if p.onlyTokens(last[0], last[1]) {
		if c := p.lastSentenceStart(prev); c > 0 && try(c) {
			return chunks
		}
		if c := p.lastWordStart(prev); c > 0 && try(c) {
			return chunks
		}
		return chunks
	}
	quarter := limit / 4
	if p.count(last[0], last[1]) >= quarter {
		return chunks
	}
	if c := p.lastSentenceStart(prev); c > 0 {
		if pa, pb := p.trim(prev[0], c); pa < pb && p.count(pa, pb) >= quarter && try(c) {
			return chunks
		}
	}
	// No sentence to move (one long run of words): move the fewest words
	// that make the tail a quarter.
	ws := p.words(prev[0], prev[1])
	for k := len(ws) - 1; k > 0; k-- {
		c := ws[k][0]
		if p.count(prev[0], ws[k-1][1]) < quarter || p.count(c, last[1]) > limit {
			break
		}
		if p.count(c, last[1]) >= quarter {
			try(c)
			break
		}
	}
	return chunks
}

// onlyTokens reports whether [a, b) is nothing but links, hashtags and
// mentions.
func (p *planner) onlyTokens(a, b int) bool {
	ws := p.words(a, b)
	if len(ws) == 0 {
		return false
	}
	for _, w := range ws {
		s := string(p.rs[w[0]:w[1]])
		if !strings.HasPrefix(s, "http://") && !strings.HasPrefix(s, "https://") &&
			!strings.HasPrefix(s, "#") && !strings.HasPrefix(s, "@") {
			return false
		}
	}
	return true
}

// lastSentenceStart is where the last sentence (or paragraph) of rg starts,
// when it has more than one; else 0.
func (p *planner) lastSentenceStart(rg [2]int) int {
	c := 0
	for i := rg[0]; i < rg[1]; i++ {
		if end := p.sentenceEnd(i, rg[1]); end > 0 {
			c = end
		} else if p.rs[i] == '\n' {
			c = i
		}
	}
	if a, _ := p.trim(c, rg[1]); c <= rg[0] || a >= rg[1] {
		return 0
	}
	return c
}

// lastWordStart is where the last word of rg starts, when it has more than
// one; else 0.
func (p *planner) lastWordStart(rg [2]int) int {
	ws := p.words(rg[0], rg[1])
	if len(ws) < 2 {
		return 0
	}
	return ws[len(ws)-1][0]
}

// counterWidth is the most a " k/n" counter costs: a space, a slash and two
// numbers of up to n's digits.
func counterWidth(n int) int {
	d := len(strconv.Itoa(n))
	return 2 + 2*d
}

// PlanMedia assigns nImages images to a chain's segments: up to cap per
// segment, filling in order from the head. If images outrun the text
// segments, image-only segments are appended, so len(result) >= nSegments.
// cap <= 0 puts everything on the head (Nostr's behavior).
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
		c := min(cap, rem)
		counts[i] = c
		rem -= c
	}
	for rem > 0 {
		c := min(cap, rem)
		counts = append(counts, c)
		rem -= c
	}
	return counts
}

// NormalizeNewlines turns CRLF (and lone CR) into LF, moving the positions
// in each list to match.
func NormalizeNewlines(text string, positions ...[]int) string {
	if !strings.ContainsRune(text, '\r') {
		return text
	}
	rs := []rune(text)
	// shift[i] is how many runes before i are removed.
	removed := make([]int, len(rs)+1)
	out := make([]rune, 0, len(rs))
	n := 0
	for i, r := range rs {
		removed[i] = n
		if r == '\r' {
			if i+1 < len(rs) && rs[i+1] == '\n' {
				n++
				continue
			}
			r = '\n'
		}
		out = append(out, r)
	}
	removed[len(rs)] = n
	for _, ps := range positions {
		for k, pos := range ps {
			if pos >= 0 && pos <= len(rs) {
				ps[k] = pos - removed[pos]
			}
		}
	}
	return string(out)
}

// FromMarkers converts the old way of breaking a thread by hand, lines of
// "---", into a text without them and the breaks they made. parts[i] is
// the code point where part i (0-based, empty parts not counted, as the
// old splitter counted them) starts, for media placed by part.
func FromMarkers(text string) (out string, breaks []int, parts []int) {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	lines := strings.Split(text, "\n")
	var b strings.Builder
	n := 0
	partStart, partHasText := 0, false
	for i, ln := range lines {
		if strings.TrimSpace(ln) == "---" {
			if partHasText {
				parts = append(parts, partStart)
			}
			breaks = append(breaks, n)
			partStart, partHasText = n, false
			continue
		}
		if strings.TrimSpace(ln) != "" {
			partHasText = true
		}
		b.WriteString(ln)
		n += len([]rune(ln))
		if i < len(lines)-1 {
			b.WriteString("\n")
			n++
		}
	}
	if partHasText {
		parts = append(parts, partStart)
	}
	// Markers at the very start or end, or next to each other, made no
	// break the old splitter honoured; the API refuses such positions.
	out = b.String()
	total := len([]rune(out))
	kept := breaks[:0]
	for _, at := range breaks {
		if at > 0 && at < total && (len(kept) == 0 || at > kept[len(kept)-1]) {
			kept = append(kept, at)
		}
	}
	breaks = kept
	if len(breaks) == 0 {
		breaks = nil
		if len(parts) == 0 {
			parts = []int{0}
		}
		return out, nil, parts
	}
	if len(parts) == 0 {
		parts = []int{0}
	}
	return out, breaks, parts
}
