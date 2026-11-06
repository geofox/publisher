"use strict";
import { v1, v1All } from "./common.js";

// The owner's accounts (GET /v1/accounts), in the app's order (each
// account's `order`). Until the composer picks accounts (part A3) it picks
// types (ORDER), and accountFor maps a type to the first connected account
// of it.
const byType = {};
let all = [];
let loading = null;

// loadAccounts reads the accounts once; later calls share the answer. A
// failure is not cached, so the next call asks again.
export function loadAccounts() {
  if (!loading) {
    loading = v1All("/v1/accounts?limit=100").then(list => {
      all = [...list].sort((a, b) => a.order - b.order);
      for (const k of Object.keys(byType)) delete byType[k];
      for (const a of all) if (a.status === "connected" && !byType[a.type]) byType[a.type] = a;
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

// accountFor is the first connected account of a type, or null (none is
// connected, or the accounts haven't loaded).
export function accountFor(type) { return byType[type] || null; }

// accountByID finds any of the owner's accounts, enabled or not.
export function accountByID(id) { return all.find(a => a.id === id) || null; }

// identityFromAccounts builds the composer's author block from the accounts'
// profiles: the first display name and avatar in type order, else a handle.
export function identityFromAccounts(order) {
  const id = { name: "", avatar: "", monogram: "", accounts: {} };
  for (const t of order) {
    const a = byType[t];
    if (!a) continue;
    id.accounts[t] = { handle: a.handle || "", display_name: a.display_name || "", avatar: a.avatar || "" };
    if (!id.name && a.display_name) id.name = a.display_name;
    if (!id.avatar && a.avatar) id.avatar = a.avatar;
  }
  if (!id.name) for (const t of order) { const h = id.accounts[t] && id.accounts[t].handle; if (h) { id.name = h; break; } }
  const first = [...id.name].find(c => c !== "@" && c !== " ");
  id.monogram = first ? first.toUpperCase() : "";
  return id;
}

// loadInstance reads GET /v1/instance: languages, translate targets, features
// and limits.
export function loadInstance() { return v1("/v1/instance"); }
