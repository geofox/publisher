"use strict";
// brands.js — platform brand marks + the UI icon set, ported from the design
// handoff (pub-data.jsx). Everything builds real DOM via the SVG namespace so it
// drops straight into the vanilla-JS views. Exports:
//   PLATFORM_META  — tint + handle + glyph scale per platform
//   brandGlyph()   — the white brand mark (Bluesky/Mastodon = inline SVG; Threads/Nostr = PNG)
//   brandTile()    — the rounded, tinted tile with the mark centered
//   icon()         — a stroke UI glyph (compose, history, calendar, thread, …)

import { META } from "./common.js";

const SVGNS = "http://www.w3.org/2000/svg";

// svg(attrs, ...children) builds an <svg>/SVG-namespaced element. Children may be
// raw markup strings (assigned via innerHTML, trusted constants only) or nodes.
function svgEl(tag, attrs = {}, ...kids) {
  const n = document.createElementNS(SVGNS, tag);
  for (const [k, v] of Object.entries(attrs)) {
    if (v === false || v == null) continue;
    n.setAttribute(k, v);
  }
  for (const kid of kids) if (kid != null) n.append(kid);
  return n;
}

// Real platform brand colors + handles + glyph scale inside the tile. Limits come
// from common.js META so there is a single source of truth.
export const PLATFORM_META = {
  bluesky:  { tint: "#1185fe", handle: "geoffrey.one",           glyph: 0.58 },
  mastodon: { tint: "#6364ff", handle: "@geoffrey@geoffrey.one", glyph: 0.60 },
  threads:  { tint: "#1c1c1e", handle: "@geoffrey",              glyph: 0.66 },
  nostr:    { tint: "#8e44ec", handle: "npub1g4…7q",             glyph: 0.74 },
};

export function tintOf(id) { return (PLATFORM_META[id] || {}).tint || "#5a6373"; }

// brandGlyph returns the white brand mark sized `size`. Bluesky + Mastodon are
// official inline SVG paths; Threads + Nostr are extracted white silhouettes (PNG).
export function brandGlyph(id, size = 18, color = "#fff") {
  if (id === "bluesky") {
    const s = svgEl("svg", { width: size, height: size, viewBox: "0 0 568 501", "aria-hidden": "true" });
    s.append(svgEl("path", { fill: color, d: "M123.121 33.664C188.241 82.553 258.281 181.68 284 234.873c25.719-53.193 95.759-152.32 160.879-201.21C491.866-1.611 568-28.906 568 57.947c0 17.346-9.945 145.713-15.778 166.555-20.275 72.453-94.155 90.933-159.875 79.748 114.875 19.55 144.097 84.31 80.986 149.07-119.86 122.992-172.272-30.859-185.702-70.281-2.462-7.227-3.614-10.608-3.631-7.733-.017-2.875-1.169.506-3.631 7.733-13.43 39.422-65.842 193.273-185.702 70.281-63.111-64.76-33.89-129.52 80.986-149.07-65.72 11.185-139.6-7.295-159.875-79.748C9.945 203.66 0 75.293 0 57.947 0-28.906 76.135-1.611 123.121 33.664Z" }));
    return s;
  }
  if (id === "mastodon") {
    const s = svgEl("svg", { width: size, height: size, viewBox: "0 0 24 24", "aria-hidden": "true" });
    s.append(svgEl("path", { fill: color, d: "M23.268 5.313c-.35-2.578-2.617-4.61-5.304-5.004C17.51.242 15.792 0 11.813 0h-.03c-3.98 0-4.835.242-5.288.309C3.882.692 1.496 2.518.917 5.127.64 6.412.61 7.837.661 9.143c.074 1.874.088 3.745.26 5.611.118 1.24.325 2.47.62 3.68.55 2.237 2.777 4.098 4.96 4.857 2.336.792 4.849.923 7.256.38.265-.061.527-.132.786-.213.585-.184 1.27-.39 1.774-.753a.057.057 0 0 0 .023-.043v-1.809a.052.052 0 0 0-.02-.041.053.053 0 0 0-.046-.01 20.282 20.282 0 0 1-4.709.545c-2.73 0-3.463-1.284-3.674-1.818a5.593 5.593 0 0 1-.319-1.433.053.053 0 0 1 .066-.054c1.517.363 3.072.546 4.632.546.376 0 .75 0 1.125-.01 1.57-.044 3.224-.124 4.768-.422.038-.008.077-.015.11-.024 2.435-.464 4.753-1.92 4.989-5.604.008-.145.03-1.52.03-1.67.002-.512.167-3.63-.024-5.545zm-3.748 9.195h-2.561V8.29c0-1.309-.55-1.976-1.67-1.976-1.23 0-1.846.79-1.846 2.35v3.403h-2.546V8.663c0-1.56-.617-2.35-1.848-2.35-1.112 0-1.668.668-1.67 1.977v6.218H4.822V8.102c0-1.31.337-2.35 1.011-3.12.696-.77 1.608-1.165 2.74-1.165 1.311 0 2.302.504 2.962 1.51l.638 1.07.638-1.07c.66-1.006 1.65-1.51 2.96-1.51 1.13 0 2.043.395 2.74 1.166.675.77 1.012 1.81 1.012 3.12z" }));
    return s;
  }
  const src = id === "threads" ? "marks/threads-mark.png" : id === "nostr" ? "marks/nostr-mark.png" : null;
  if (!src) return null;
  const img = document.createElement("img");
  img.src = src; img.alt = ""; img.width = size; img.height = size;
  img.style.cssText = `display:block;width:${size}px;height:${size}px;object-fit:contain`;
  return img;
}

// brandTile builds the rounded, tinted square with the white mark centered —
// the recurring platform "logo chip" used in targets, previews, sheets.
export function brandTile(id, { size = 30, r = 8 } = {}) {
  const meta = PLATFORM_META[id] || {};
  const tile = document.createElement("span");
  tile.className = "brand-tile";
  tile.style.cssText =
    `width:${size}px;height:${size}px;border-radius:${r}px;background:${meta.tint || "#5a6373"}`;
  const g = brandGlyph(id, Math.round(size * (meta.glyph || 0.58)));
  if (g) tile.append(g);
  return tile;
}

// ── UI stroke icons (ported from pub-data.jsx Icon) ────────────────────────
const ICON_PATHS = {
  compose:  '<path d="M5 19h14"/><path d="M14.5 5.5l4 4L8 20l-4.5.9L4.5 16z"/>',
  interact: '<path d="M7 7h10l-2.5-2.5M17 17H7l2.5 2.5"/>',
  history:  '<circle cx="12" cy="12" r="8"/><path d="M12 8v4l3 2"/>',
  tools:    '<path d="M5 7h9M5 12h14M5 17h6"/><circle cx="17" cy="7" r="2"/><circle cx="9" cy="17" r="2"/>',
  verify:   '<path d="M12 3l7 3v5c0 4.5-3 7.5-7 9-4-1.5-7-4.5-7-9V6z"/><path d="M9 12l2 2 4-4"/>',
  photo:    '<rect x="3.5" y="5" width="17" height="14" rx="2.5"/><circle cx="8.5" cy="10" r="1.6"/><path d="M5 17l4.5-4 3 2.5L16 12l3 3"/>',
  calendar: '<rect x="4" y="5" width="16" height="15" rx="2.5"/><path d="M4 9h16M8 3v3M16 3v3"/>',
  globe:    '<circle cx="12" cy="12" r="8"/><path d="M4 12h16M12 4c2.5 2.2 2.5 13.8 0 16M12 4c-2.5 2.2-2.5 13.8 0 16"/>',
  chevron:  '<path d="M9 6l6 6-6 6"/>',
  check:    '<path d="M5 12.5l4.5 4.5L19 7"/>',
  plus:     '<path d="M12 5v14M5 12h14"/>',
  sparkle:  '<path d="M12 4l1.6 5.4L19 11l-5.4 1.6L12 18l-1.6-5.4L5 11l5.4-1.6z"/>',
  people:   '<circle cx="9" cy="9" r="3"/><path d="M3.5 19c.6-3 3-4.5 5.5-4.5S14 16 14.6 19"/><path d="M16 7.5a3 3 0 010 5.5M16.5 14.5c2 .5 3.6 2 4 4.5"/>',
  thread:   '<rect x="4" y="3.5" width="12" height="7" rx="2"/><rect x="8" y="13.5" width="12" height="7" rx="2"/><path d="M10 10.5v3"/>',
  link:     '<path d="M9 12h6"/><path d="M10 8H8a4 4 0 000 8h2M14 8h2a4 4 0 010 8h-2"/>',
  search:   '<circle cx="11" cy="11" r="6.5"/><path d="M20 20l-4-4"/>',
  trash:    '<path d="M4 7h16M9 7V5h6v2M6 7l1 13h10l1-13"/>',
  close:    '<path d="M6 6l12 12M18 6L6 18"/>',
  refresh:  '<path d="M4 12a8 8 0 0113.7-5.6L20 8M20 4v4h-4"/><path d="M20 12a8 8 0 01-13.7 5.6L4 16M4 20v-4h4"/>',
  send:     '<path d="M5 12l15-7-7 15-2.5-5.5z"/>',
};

// icon returns a stroke SVG glyph. `name` keys ICON_PATHS; unknown → null.
export function icon(name, { size = 24, color = "currentColor", sw = 1.7 } = {}) {
  const paths = ICON_PATHS[name];
  if (!paths) return null;
  const s = svgEl("svg", {
    width: size, height: size, viewBox: "0 0 24 24", fill: "none",
    stroke: color, "stroke-width": sw, "stroke-linecap": "round", "stroke-linejoin": "round",
    "aria-hidden": "true",
  });
  s.innerHTML = paths; // trusted constant markup
  return s;
}

// platformLabel is a tiny convenience over META.
export function platformLabel(id) { return (META[id] && META[id].label) || id; }
