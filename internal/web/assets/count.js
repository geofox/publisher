"use strict";
// count.js measures text the way each destination does: the browser's half
// of destination.Counting (internal/destination/counting.go). Both halves
// run the same cases (internal/destination/testdata/counting.json; the web
// side in e2e/tests/counting.spec.ts), so the counters the composer shows
// while typing agree with the server's planner.
import { META } from "./common.js";
import { accountFor } from "./accounts.js";

// Go's \s and \w are ASCII: [\t\n\f\r ] and [0-9A-Za-z_].
const URL_RE = /https?:\/\/[^\t\n\f\r <>"]+/g;
const MENTION_RE = /(^|[^0-9A-Za-z_@])(@[0-9A-Za-z_.]+)@[0-9A-Za-z_.-]+\.[a-z]{2,}/g;

let _seg;
function graphemes(s) {
  if (_seg === undefined) {
    try { _seg = new Intl.Segmenter(undefined, { granularity: "grapheme" }); } catch { _seg = false; }
  }
  if (!_seg) return [...s];
  const out = [];
  for (const g of _seg.segment(s)) out.push(g.segment);
  return out;
}

const utf8 = new TextEncoder();

// isEmoji mirrors destination.isEmoji: a pictograph, a regional indicator,
// the emoji presentation selector or a keycap.
function isEmoji(cluster) {
  for (const ch of cluster) {
    const r = ch.codePointAt(0);
    if (r === 0xfe0f || r === 0x20e3) return true;
    if ((r >= 0x1f000 && r <= 0x1faff) || (r >= 0x2600 && r <= 0x27bf) ||
        (r >= 0x2300 && r <= 0x23ff) || (r >= 0x2b00 && r <= 0x2bff)) return true;
  }
  return false;
}

function unitLen(c, s) {
  switch (c.unit) {
    case "codepoints": return [...s].length;
    case "utf16": return s.length;
    case "bytes": return utf8.encode(s).length;
    default:
      if (!c.emoji_utf8_bytes) return graphemes(s).length;
      return graphemes(s).reduce((n, g) => n + (isEmoji(g) ? utf8.encode(g).length : 1), 0);
  }
}

// count measures s under counting c (a destination.Counting as JSON).
export function count(c, s) {
  c = c || {};
  s = String(s || "");
  let n = 0;
  if (c.url_length > 0) {
    const urls = s.match(URL_RE) || [];
    n += urls.length * c.url_length;
    s = s.replace(URL_RE, "");
  }
  if (c.mention_local_part_only) s = s.replace(MENTION_RE, "$1$2");
  return n + unitLen(c, s);
}

// countingFor is platform p's counting rule: its account's, else graphemes.
export function countingFor(p) {
  const a = accountFor(p);
  const t = a && a.capabilities && a.capabilities.text;
  return (t && t.counting) || { unit: "graphemes" };
}

// limitFor is the most one post of platform p may count, after a content
// warning the destination counts in (cw: the warning's text).
export function limitFor(p, cw = "") {
  const limit = META[p].limit;
  const c = countingFor(p);
  if (!limit || !c.content_warning_counts || !cw) return limit;
  return Math.max(limit - count(c, cw), 1);
}

// countFor measures text as platform p counts it.
export function countFor(p, text) { return count(countingFor(p), text); }
