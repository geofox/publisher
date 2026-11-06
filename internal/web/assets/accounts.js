"use strict";
import { v1, v1All, META, ORDER } from "./common.js";
import { primaryIn } from "./snapshot.js";

// The owner's accounts (GET /v1/accounts), in the app's order: each
// account's `order` (networks in a fixed order, then oldest first), with an
// account the server hasn't loaded yet (order -1) last. The composer,
// History, Interact and Tools pick accounts by id; ORDER is only the
// fallback order for a type the server doesn't rank.
let all = [];
let loading = null;
let loaded = false;

function rank(a) {
  if (Number.isInteger(a.order) && a.order >= 0) return [0, a.order];
  const t = ORDER.indexOf(a.type);
  return [1, t < 0 ? ORDER.length : t];
}

function sortAccounts(list) {
  return [...list].sort((a, b) => {
    const ra = rank(a), rb = rank(b);
    return ra[0] - rb[0] || ra[1] - rb[1] || String(a.created_at || "").localeCompare(String(b.created_at || ""));
  });
}

// loadAccounts reads the accounts once; later calls share the answer. A
// failure is not cached, so the next call asks again. Each successful read
// fires an "accounts" event on document (before the callers continue).
export function loadAccounts() {
  if (!loading) {
    loading = v1All("/v1/accounts?limit=100").then(list => {
      all = sortAccounts(list || []);
      loaded = true;
      document.dispatchEvent(new CustomEvent("accounts"));
      return all;
    }).catch(e => { loading = null; throw e; });
  }
  return loading;
}

// reloadAccounts reads the accounts again even after a successful read
// (one may have been added since).
export function reloadAccounts() {
  loading = null;
  return loadAccounts();
}

// accountsLoaded reports whether a read has succeeded.
export function accountsLoaded() { return loaded; }

// accounts is every account, in order.
export function accounts() { return all; }

// byId is one account, or null (unknown, or not loaded yet).
export function byId(id) { return all.find(a => a.id === id) || null; }

// typeOf is an account's network type, or "" when it isn't known.
export function typeOf(id) { const a = byId(id); return a ? a.type : ""; }

// ofType lists a network's accounts, in order.
export function ofType(type) { return all.filter(a => a.type === type); }

export function isConnected(a) { return !!a && a.status === "connected"; }

// connected lists the accounts that can be used now, in order.
export function connected() { return all.filter(isConnected); }

// primaryOf is the connected account a network stands for when only the
// network is known (an older snapshot, a reply's default actor): the first
// one new posts select, else the first.
export function primaryOf(type) { return primaryIn(all, type); }

// defaultTargets is what a new post selects: the connected accounts that
// new posts select, in order. Empty until the accounts load.
export function defaultTargets() {
  return all.filter(a => isConnected(a) && a.default_selected).map(a => a.id);
}

// inOrder sorts account ids in the app's order; ids it doesn't know go last.
export function inOrder(ids) {
  const pos = new Map(all.map((a, i) => [a.id, i]));
  return [...ids].sort((a, b) => (pos.has(a) ? pos.get(a) : 1e9) - (pos.has(b) ? pos.get(b) : 1e9));
}

// networkName is a type's display name ("Mastodon").
export function networkName(type) { return (META[type] && META[type].label) || type || "account"; }

// isOpaque reports whether s is an identifier (a Nostr npub, a long hex
// key) rather than a handle a person reads; shortId middle-truncates one.
export function isOpaque(s) { return /^(npub1|nprofile1)/i.test(s || "") || /^[0-9a-f]{32,}$/i.test(s || ""); }
export function shortId(s, head = 10, tail = 5) {
  return s && s.length > head + tail + 3 ? s.slice(0, head) + "…" + s.slice(-tail) : s;
}

// handleOf is how an account's handle is shown in full (an npub
// shortened), or "" before the network reported one.
export function handleOf(a) {
  if (!a || !a.handle) return "";
  return isOpaque(a.handle) ? shortId(a.handle) : a.handle;
}

// shortHandleOf drops the server from a handle: "@me@host" and
// "me.bsky.social" become "@me". An npub stays shortened.
export function shortHandleOf(a) {
  const h = (a && a.handle) || "";
  if (!h) return "";
  if (isOpaque(h)) return shortId(h, 8, 4);
  const fedi = /^@?([^@\s]+)@[^@\s]+$/.exec(h);
  if (fedi) return "@" + fedi[1];
  const dot = h.indexOf(".");
  if (!h.startsWith("@") && dot > 0) return "@" + h.slice(0, dot);
  return h.startsWith("@") ? h : "@" + h;
}

// customLabel is the account's label when the owner named it something
// other than its network, else "".
export function customLabel(a) {
  const l = ((a && a.label) || "").trim();
  return l && l !== networkName(a.type) ? l : "";
}

// fitHandle shortens a full handle to about max characters by cutting the
// user name, keeping the server that tells two apart:
// "@averyveryverylongname@one.example" → "@averyveryv…@one.example".
export function fitHandle(h, max = 24) {
  if (!h || h.length <= max) return h || "";
  const m = /^(@?[^@\s]+)(@[^@\s]+)$/.exec(h);
  if (m && m[2].length <= max - 4) return m[1].slice(0, max - m[2].length - 1) + "…" + m[2];
  return h.slice(0, max - 1) + "…";
}

// labelsFor gives each account of ids a short label for tabs, chips and
// columns (which show the network's mark beside it): the network's name
// when the owner has only that account there (or the owner's own label for
// it); else its short handle, or its full handle when another account of the
// same network would read the same (@me@one.example, @me@two.example), or
// its label when it has no handle yet.
export function labelsFor(ids) {
  const out = byKey(), short = byKey(), seen = byKey();
  for (const id of ids) {
    const a = byId(id);
    short[id] = a ? shortHandleOf(a) : "";
    if (short[id]) { const k = a.type + " " + short[id]; seen[k] = (seen[k] || 0) + 1; }
  }
  for (const id of ids) {
    const a = byId(id);
    if (!a) { out[id] = "account"; continue; }
    const s = short[id];
    if (ofType(a.type).length < 2) out[id] = customLabel(a) || networkName(a.type);
    else if (s) out[id] = seen[a.type + " " + s] > 1 ? fitHandle(handleOf(a)) : s;
    else out[id] = customLabel(a) || networkName(a.type);
  }
  return out;
}

const byKey = () => Object.create(null);

// who is what tells an account apart from the others of its network: its
// handle, else the label the owner gave it; "" when it has neither.
function who(a) {
  return handleOf(a) || customLabel(a);
}

// withNetwork puts the network in front of what tells the account apart,
// unless that already names it ("Mastodon (work)").
function withNetwork(a, sep) {
  const net = networkName(a.type), w = who(a);
  if (!w) return net;
  return w.toLowerCase().includes(net.toLowerCase()) ? w : net + sep + w;
}

// nameOf names an account in a sentence: "Mastodon @me@host", or the
// network alone when the owner has only that one account on it.
export function nameOf(id) {
  const a = byId(id);
  if (!a) return "an account that is gone";
  return ofType(a.type).length < 2 ? networkName(a.type) : withNetwork(a, " ");
}

// accessibleName is how assistive technology names an account: the
// network, the owner's label and the handle, each once ("Mastodon,
// @me@host"; "Mastodon (long posts), @me@two.host"; "Threads, Work").
export function accessibleName(id) {
  const a = byId(id);
  if (!a) return "account";
  const net = networkName(a.type), label = customLabel(a);
  const head = label && label.toLowerCase().includes(net.toLowerCase()) ? label : net;
  return [head, head === label ? "" : label, handleOf(a)].filter(Boolean).join(", ");
}

// identity is the composer header's author: the first connected account,
// in order, with a display name; else the first handle (an account that
// can't be used may show a profile it no longer has).
export function identity() {
  const id = { name: "", avatar: "", monogram: "" };
  const live = connected();
  const named = live.find(a => a.display_name);
  if (named) { id.name = named.display_name; id.avatar = named.avatar || ""; }
  if (!id.name) { const h = live.find(a => a.handle); if (h) { id.name = handleOf(h); id.avatar = h.avatar || ""; } }
  if (!id.avatar) { const av = live.find(a => a.avatar); if (av) id.avatar = av.avatar; }
  const first = [...id.name].find(c => c !== "@" && c !== " ");
  id.monogram = first ? first.toUpperCase() : "";
  return id;
}

// loadInstance reads GET /v1/instance: languages, translate targets, features
// and limits.
export function loadInstance() { return v1("/v1/instance"); }
