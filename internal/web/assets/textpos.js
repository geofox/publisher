"use strict";
// textpos.js holds the position arithmetic of the thread editor. The composer
// keeps positions (breaks, media anchors) as UTF-16 offsets, the unit of JS
// strings and of a textarea's selection; the API speaks Unicode code points.
// The conversions happen once, at the edge (state.js v1PostBody, drafts.js,
// the plan's ranges).

// cpToU16 is the UTF-16 offset of code point position cp in text (clamped).
export function cpToU16(text, cp) {
  let u = 0, n = 0;
  while (n < cp && u < text.length) {
    const c = text.charCodeAt(u);
    u += c >= 0xd800 && c <= 0xdbff && u + 1 < text.length ? 2 : 1;
    n++;
  }
  return u;
}

// u16ToCp is the code point position of UTF-16 offset u in text. An offset
// inside a surrogate pair counts as the pair's start.
export function u16ToCp(text, u) {
  let n = 0;
  for (let i = 0; i < u && i < text.length; i++) {
    const c = text.charCodeAt(i);
    if (c >= 0xd800 && c <= 0xdbff && i + 1 < u) i++;
    else if (c >= 0xd800 && c <= 0xdbff) break; // u splits the pair
    n++;
  }
  return n;
}

// cpIndex maps every code point position of text onto its UTF-16 offset, for
// converting many positions of one text (a plan's ranges) at once.
export function cpIndex(text) {
  const out = [0];
  for (let u = 0; u < text.length;) {
    const c = text.charCodeAt(u);
    u += c >= 0xd800 && c <= 0xdbff && u + 1 < text.length ? 2 : 1;
    out.push(u);
  }
  return out;
}

// diff is the one edit that turns a into b: at, how much of a it removes,
// and the text it inserts.
export function diff(a, b) {
  let pre = 0;
  const max = Math.min(a.length, b.length);
  while (pre < max && a.charCodeAt(pre) === b.charCodeAt(pre)) pre++;
  let suf = 0;
  while (suf < max - pre && a.charCodeAt(a.length - 1 - suf) === b.charCodeAt(b.length - 1 - suf)) suf++;
  return { at: pre, removed: a.length - pre - suf, inserted: b.slice(pre, b.length - suf) };
}

// rebase moves a position across an edit at `at` that removed `removed`
// units and inserted `inserted`: a position at or before the edit stays (so
// text typed at a post's start joins that post), one after it shifts, one
// inside the removed text goes to its start.
export function rebase(pos, at, removed, inserted) {
  if (pos <= at) return pos;
  if (pos >= at + removed) return pos + inserted - removed;
  return at;
}

// URL is the server's link pattern (internal/api urlInText, destination
// countURL): a break can't fall inside one. Go's \s is [\t\n\f\r ].
export const URL_RE = /https?:\/\/[^\t\n\f\r <>"]+/g;

function urlSpans(text) {
  const out = [];
  URL_RE.lastIndex = 0;
  let m;
  while ((m = URL_RE.exec(text)) !== null) out.push([m.index, m.index + m[0].length]);
  return out;
}

let _gseg;
function graphemeStarts(text) {
  if (_gseg === undefined) {
    try { _gseg = new Intl.Segmenter(undefined, { granularity: "grapheme" }); } catch { _gseg = false; }
  }
  const starts = new Set();
  if (!_gseg) { for (let u = 0; u <= text.length; u++) starts.add(u); return starts; }
  for (const g of _gseg.segment(text)) starts.add(g.index);
  starts.add(text.length);
  return starts;
}

// cleanBreaks makes breaks valid for text, as the server checks them: inside
// the text, between graphemes, outside links, ascending, each once, at most
// max. A break inside a link moves to the link's start; one inside a
// grapheme to the grapheme's start.
export function cleanBreaks(text, breaks, max = 99) {
  if (!breaks || !breaks.length) return [];
  const urls = urlSpans(text);
  const starts = graphemeStarts(text);
  const out = [];
  for (let b of [...breaks].sort((x, y) => x - y)) {
    for (const [a, z] of urls) if (b > a && b < z) { b = a; break; }
    while (b > 0 && !starts.has(b)) b--;
    if (b <= 0 || b >= text.length) continue;
    if (out.length && out[out.length - 1] >= b) continue;
    out.push(b);
  }
  return out.slice(0, max);
}

const isSpace = c => /\s/.test(c);

// splitPoint is where a break asked for at u goes: past the whitespace
// there (it stays with the post before), and out of a link to its start.
export function splitPoint(text, u) {
  while (u < text.length && isSpace(text[u])) u++;
  for (const [a, z] of urlSpans(text)) if (u > a && u < z) return a;
  return u;
}

// wordStarts lists where words start in text[from, to): after whitespace.
export function wordStarts(text, from = 0, to = text.length) {
  const out = [];
  for (let u = from; u < to; u++) {
    if (!isSpace(text[u]) && (u === 0 || isSpace(text[u - 1]))) out.push(u);
  }
  return out;
}

// sentenceStarts lists where sentences start in text[from, to), the way
// the server's planner ends them (internal/thread sentenceEnd): after . ! ?
// or … (and any closing quotes or brackets) followed by whitespace, and at
// the start of each line.
export function sentenceStarts(text, from = 0, to = text.length) {
  const out = [];
  const closers = "\"')]»”’";
  let want = from === 0;
  for (let u = from; u < to; u++) {
    const c = text[u];
    if (want && !isSpace(c)) { out.push(u); want = false; }
    if (c === "\n") { want = true; continue; }
    if (c === "." || c === "!" || c === "?" || c === "…") {
      let j = u + 1;
      while (j < to && closers.includes(text[j])) j++;
      if (j < to && isSpace(text[j])) want = true;
    }
  }
  return out;
}

// MARKER is a line of "---" followed by a line break: the old way of breaking
// a thread by hand, which the composer turns into a break as it is typed. A
// "---" on the text's last line stays text (it may be meant).
const MARKER = /(^|\n)[ \t]*---[ \t]*\n/;

// nextMarker finds the first marker line in text: where it starts and how
// long it is (its line break included); null if there is none.
export function nextMarker(text) {
  const m = MARKER.exec(text);
  if (!m) return null;
  return { at: m.index + m[1].length, removed: m[0].length - m[1].length };
}

// takeMarkers takes the marker lines out of text and answers the text, the
// breaks they made, and an edit list so other positions can follow
// (rebase each over every edit, in order).
export function takeMarkers(text) {
  const edits = [];
  const breaks = [];
  let m;
  while ((m = MARKER.exec(text)) !== null) {
    const at = m.index + m[1].length;
    const removed = m[0].length - m[1].length;
    text = text.slice(0, at) + text.slice(at + removed);
    for (let i = 0; i < breaks.length; i++) breaks[i] = rebase(breaks[i], at, removed, 0);
    breaks.push(at);
    edits.push({ at, removed, inserted: 0 });
  }
  return { text, breaks: [...new Set(breaks)].filter(b => b > 0 && b < text.length).sort((a, b) => a - b), edits };
}

// fromMarkers converts a text that breaks a thread with lines of "---" (an
// old draft, a recovered snapshot, an old post) into a text and breaks, and
// answers where each non-empty part starts, for media placed by part. Unlike
// takeMarkers it converts a last-line marker too: that text came from the
// old composer, where every marker line was a break.
export function fromMarkers(text) {
  text = String(text || "").replace(/\r\n/g, "\n");
  const { text: out, breaks } = takeMarkers(text.replace(/\n[ \t]*---[ \t]*$/, "\n---\n"));
  const body = out.replace(/\n$/, text.endsWith("\n") ? "\n" : "");
  const kept = breaks.filter(b => b < body.length);
  const bounds = [0, ...kept, body.length];
  const parts = [];
  for (let i = 0; i + 1 < bounds.length; i++) {
    if (body.slice(bounds[i], bounds[i + 1]).trim()) parts.push(bounds[i]);
  }
  return { text: body, breaks: kept, parts: parts.length ? parts : [0] };
}
