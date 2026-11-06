"use strict";
// snapshot.js reads the composer's saved form: a recovery snapshot
// (drafts_recovery.js, state.js buildSpec) or a draft read back (drafts.js
// specFromDraft). Version 3 keys targets and their settings by account id.
// Version 2 keyed them by network; version 1 also wrote breaks as lines of
// "---" in the text and placed media by part number. upgrade turns any of
// them into version 3. Pure functions: no DOM, no network.
import { fromMarkers } from "./textpos.js";

export const VERSION = 3;

// Option keys renamed since the first snapshots (old → new).
const RENAMED = [["bluesky_reply", "reply"], ["bluesky_disable_quotes", "disable_quotes"], ["threads_reply_control", "reply_control"]];

const isObj = v => v != null && typeof v === "object" && !Array.isArray(v);
const positions = v => (Array.isArray(v) ? v.filter(x => Number.isInteger(x) && x >= 0) : []);
const uniq = xs => [...new Set(xs)];

// primaryIn is the account of list a network stands for when only the
// network is known: the first connected one that new posts select, else
// the first connected one (what the composer posted from before accounts
// were picked one by one); null when none is connected.
export function primaryIn(list, type) {
  const c = (list || []).filter(a => a.type === type && a.status === "connected");
  return c.find(a => a.default_selected) || c[0] || null;
}

// standIn is the account a network named by an older snapshot comes back
// as: its primary account (primaryIn), else its first account in any state
// but deleted, which comes back selected and unavailable, saying why, as
// the network did before (Post refuses until it is unticked); null when
// the owner has no account there.
export function standIn(list, type) {
  return primaryIn(list, type) || (list || []).find(a => a.type === type && a.status !== "deleted") || null;
}

// cleanOv checks one target's settings: known shapes only, old key names
// renamed, and (version 1) its own text's "---" lines turned into breaks.
export function cleanOv(src, markers = false) {
  const ov = { ...src };
  // Parsed JSON can carry these as own keys; copied onto the composer's
  // settings (Object.assign) "__proto__" would replace their prototype.
  for (const k of ["__proto__", "constructor", "prototype"]) delete ov[k];
  for (const [from, to] of RENAMED) {
    if (!(from in ov)) continue;
    if (!(to in src)) ov[to] = ov[from];
    delete ov[from];
  }
  if (typeof ov.text !== "string") ov.text = null;
  if (markers && ov.text != null) {
    const own = fromMarkers(ov.text);
    ov.text = own.text;
    ov.breaks = own.breaks.length ? own.breaks : null;
  }
  ov.breaks = Array.isArray(ov.breaks) ? positions(ov.breaks) : null;
  if (!isObj(ov.mediaAt) || ov.text == null) ov.mediaAt = null;
  else {
    const m = {};
    for (const [k, x] of Object.entries(ov.mediaAt)) if (Number.isInteger(x) && x >= 0) m[k] = x;
    ov.mediaAt = m;
  }
  // The editor holds languages as "en,fr"; a stored spec may have a list.
  if (Array.isArray(ov.langs)) ov.langs = ov.langs.join(",");
  return ov;
}

// upgrade turns a snapshot of any version into version 3, for the owner's
// accounts list (in order). A network maps to the account that stands in
// for it (standIn); an account id that isn't in list, or a network where
// the owner has no account, is left out and counted in `dropped` (the ids
// or network names). A reply's acting account must be connected
// (primaryIn). Fields it doesn't know are kept as they are.
export function upgrade(spec, list) {
  const s = isObj(spec) ? spec : {};
  const v = s.v === 3 ? 3 : s.v === 2 ? 2 : 1;
  const known = new Map((list || []).map(a => [a.id, a]));
  const dropped = [];

  let master = typeof s.master_text === "string" ? s.master_text : "";
  let breaks, parts = null;
  if (v === 1) {
    const conv = fromMarkers(master);
    master = conv.text;
    breaks = conv.breaks;
    parts = conv.parts;
  } else {
    breaks = positions(s.breaks);
  }

  // Media positions by attachment id (version 1: part numbers).
  const anchors = {};
  if (isObj(s.anchors)) {
    for (const [id, x] of Object.entries(s.anchors)) {
      if (!Number.isInteger(x) || x <= 0) continue;
      anchors[id] = parts ? parts[Math.min(x, parts.length - 1)] : x;
    }
  }

  const targets = [], overrides = {};
  const ovs = isObj(s.overrides) ? s.overrides : {};
  const add = (key, id) => {
    if (targets.includes(id)) return;
    targets.push(id);
    if (isObj(ovs[key])) overrides[id] = cleanOv(ovs[key], v === 1);
  };
  if (v === 3) {
    for (const id of Array.isArray(s.targets) ? s.targets : []) {
      if (typeof id === "string" && known.has(id)) add(id, id);
      else dropped.push(String(id));
    }
  } else {
    for (const type of Array.isArray(s.platforms) ? s.platforms : []) {
      const a = standIn(list, type);
      if (a) add(type, a.id);
      else dropped.push(String(type));
    }
  }

  // A reply or quote: its acting account and the selection it returns to.
  let interaction = null;
  const it = s.interaction;
  if (isObj(it)) {
    interaction = { action: it.action, sourceInput: it.sourceInput, platform: "", account: "", prevTargets: [] };
    if (v === 3) {
      const a = typeof it.account === "string" ? known.get(it.account) : null;
      interaction.account = a ? a.id : "";
      interaction.platform = typeof it.platform === "string" && it.platform ? it.platform : a ? a.type : "";
      interaction.prevTargets = uniq((Array.isArray(it.prevTargets) ? it.prevTargets : []).filter(id => known.has(id)));
    } else {
      interaction.platform = typeof it.platform === "string" ? it.platform : "";
      const a = primaryIn(list, interaction.platform);
      interaction.account = a ? a.id : "";
      const prev = Array.isArray(it.prevPlatforms) ? it.prevPlatforms : [];
      interaction.prevTargets = uniq(prev.map(t => standIn(list, t)).filter(Boolean).map(x => x.id));
    }
  }

  const out = { ...s, v: VERSION, master_text: master, breaks, anchors, targets, overrides, interaction, dropped };
  delete out.platforms;
  return out;
}
