"use strict";
import { el, v1, flash, confirmModal, asDialog } from "./common.js";
import { accounts, accountSets, reloadAccounts, byId, nameOf, isConnected, loadDestinations, networkName } from "./accounts.js";
import { showProblems } from "./fields.js";

// Settings → Composing: account sets (several accounts picked at once in
// the composer; the default one is what a new post starts with) and each
// account's defaults (content warning, languages, its network's options),
// which a new post starts with on that account.

const enc = encodeURIComponent;

// dialog is a small modal with a title and a form.
function dialog(title) {
  const bk = el("div", { class: "modal-bk" });
  let close = () => bk.remove();
  const status = el("p", { class: "reauth-status", role: "status", "aria-live": "polite" });
  const form = el("form", { class: "reauth set-dlg-form", novalidate: "" });
  const card = el("div", { class: "modal acct-dlg" },
    el("button", { class: "modal-x", type: "button", text: "✕", "aria-label": "Close", onclick: () => close() }),
    el("p", { class: "modal-title", text: title }), form);
  return {
    form, status,
    show(focusEl) { bk.append(card); document.body.append(bk); close = asDialog(bk, card, close, focusEl); },
    close: () => close(),
  };
}

// ── account sets ────────────────────────────────────────────────────────

// setsSection lists the sets, with adding, changing and removing one.
export function setsSection() {
  const node = el("section", { class: "set-sect", "aria-labelledby": "set-sets-title" });
  const draw = () => {
    const sets = accountSets();
    const list = el("ul", { class: "set-list" });
    for (const s of sets) {
      const names = (s.accounts || []).map(id => nameOf(id));
      const off = (s.accounts || []).filter(id => !isConnected(byId(id))).length;
      list.append(el("li", { class: "set-item" },
        el("div", { class: "set-item-main" },
          el("p", { class: "set-item-name" }, s.name, s.is_default ? el("span", { class: "set-badge", text: "new posts" }) : null),
          el("p", { class: "set-item-sub", text: names.join(", ") + (off ? " (" + off + " can't be posted to now)" : "") })),
        el("div", { class: "set-item-actions" },
          s.is_default ? null : el("button", { class: "ghost xs", type: "button", text: "Start new posts with it",
            onclick: e => patchSet(s, { is_default: true }, e.target) }),
          el("button", { class: "ghost xs", type: "button", text: "Change", "aria-label": "Change " + s.name, onclick: () => editSet(s) }),
          el("button", { class: "ghost xs danger-text", type: "button", text: "Remove", "aria-label": "Remove " + s.name,
            onclick: () => confirmModal({ title: "Remove " + s.name + "?", body: "Its accounts stay as they are.",
              confirmText: "Remove", onConfirm: () => removeSet(s) }) }))));
    }
    const live = accounts().filter(a => a.status !== "deleted");
    node.replaceChildren(
      el("h2", { id: "set-sets-title", class: "set-sect-title", text: "Account sets" }),
      el("p", { class: "set-sect-intro", text: "Pick several accounts at once in the composer. The set new posts start with replaces each account's “Picked for new posts”." }),
      sets.length ? list : el("p", { class: "set-status", text: "No sets yet." }),
      el("div", { class: "set-actions" },
        el("button", { class: "ghost sm", type: "button", text: "New set", disabled: live.length ? false : "",
          onclick: () => editSet(null) }),
        accountSets().some(s => s.is_default) ? el("button", { class: "ghost sm", type: "button", text: "Start new posts with each account's flag",
          onclick: e => { const d = accountSets().find(s => s.is_default); if (d) patchSet(d, { is_default: false }, e.target); } }) : null));
  };
  document.addEventListener("accounts", draw);
  draw();
  return node;
}

async function patchSet(s, body, b) {
  if (b) b.disabled = true;
  try {
    await v1("/v1/account-sets/" + enc(s.id), { method: "PATCH", body });
    await reloadAccounts();
    flash("Saved");
  } catch (e) {
    flash(e.message);
  } finally {
    if (b) b.disabled = false;
  }
}

async function removeSet(s) {
  try {
    await v1("/v1/account-sets/" + enc(s.id), { method: "DELETE" });
    await reloadAccounts();
    flash("Removed");
    return true;
  } catch (e) {
    flash(e.message);
    return false;
  }
}

// editSet adds a set (s null) or changes one: its name, its accounts and
// whether new posts start with it.
function editSet(s) {
  const d = dialog(s ? "Change " + s.name : "New account set");
  const name = el("input", { id: "set-name", type: "text", maxlength: "64", value: s ? s.name : "", autocomplete: "off", required: "" });
  const nameErr = el("p", { class: "fld-err" });
  const picked = new Set(s ? s.accounts : []);
  const boxes = [];
  const group = el("fieldset", { class: "set-pick" }, el("legend", { text: "Accounts" }));
  for (const a of accounts().filter(x => x.status !== "deleted")) {
    const id = "set-acct-" + a.id;
    const cb = el("input", { type: "checkbox", id, checked: picked.has(a.id) ? "" : false });
    boxes.push({ a, cb });
    group.append(el("div", { class: "fld-check" }, cb,
      el("label", { for: id, text: nameOf(a.id) + (isConnected(a) ? "" : " (can't be posted to now)") })));
  }
  const accErr = el("p", { class: "fld-err" });
  const isDef = el("input", { type: "checkbox", id: "set-default", checked: s && s.is_default ? "" : false });
  const ok = el("button", { class: "primary sm", type: "submit", text: s ? "Save" : "Add" });
  d.form.append(
    el("div", { class: "fld" }, el("label", { for: "set-name", class: "fld-label", text: "Name" }), name, nameErr),
    group, accErr,
    el("div", { class: "fld-check" }, isDef, el("label", { for: "set-default", text: "Start new posts with this set" })),
    d.status,
    el("div", { class: "confirm-actions" }, el("button", { class: "ghost sm", type: "button", text: "Cancel", onclick: () => d.close() }), ok));
  const fields = new Map([["/name", ctl(name, nameErr)], ["/accounts", ctl(group, accErr)], ["/is_default", ctl(isDef, d.status)]]);
  d.form.addEventListener("submit", async ev => {
    ev.preventDefault();
    if (ok.disabled) return;
    const body = { name: name.value.trim(), accounts: boxes.filter(x => x.cb.checked).map(x => x.a.id), is_default: isDef.checked };
    ok.disabled = true;
    d.status.textContent = "";
    try {
      if (s) await v1("/v1/account-sets/" + enc(s.id), { method: "PATCH", body });
      else await v1("/v1/account-sets", { method: "POST", body });
      await reloadAccounts();
      d.close();
      flash(s ? "Saved" : "Set added");
    } catch (e) {
      d.status.textContent = showProblemsAt(e, fields).join(" ");
    } finally {
      ok.disabled = false;
    }
  });
  d.show(name);
}

// ctl makes an input and its error line look like a field control for
// showProblems.
function ctl(input, err) {
  return {
    setError: m => { err.textContent = m || ""; if (m) input.setAttribute("aria-invalid", "true"); else input.removeAttribute("aria-invalid"); },
    focus: () => (input.querySelector ? (input.querySelector("input") || input) : input).focus(),
  };
}

// showProblemsAt puts a problem's field errors by their pointer's start
// (/accounts/2 goes to /accounts).
function showProblemsAt(e, fields) {
  const p = e.problem && Array.isArray(e.problem.errors) ? { ...e, problem: { ...e.problem, errors: e.problem.errors.map(fe => {
    const head = "/" + String(fe.pointer || "").split("/")[1];
    return fields.has(head) ? { ...fe, pointer: head } : fe;
  }) } } : e;
  return showProblems(p, fields);
}

// ── each account's defaults ─────────────────────────────────────────────

// defaultsSection lists the accounts with what each starts a post with.
export function defaultsSection() {
  const node = el("section", { class: "set-sect", "aria-labelledby": "set-defaults-title" });
  const draw = () => {
    const list = el("ul", { class: "set-list" });
    for (const a of accounts().filter(x => x.status !== "deleted")) {
      list.append(el("li", { class: "set-item" },
        el("div", { class: "set-item-main" },
          el("p", { class: "set-item-name", text: nameOf(a.id) }),
          el("p", { class: "set-item-sub", text: summary(a) })),
        el("div", { class: "set-item-actions" },
          el("button", { class: "ghost xs", type: "button", text: "Change", "aria-label": "Change " + nameOf(a.id) + "'s defaults",
            onclick: () => editDefaults(a) }))));
    }
    node.replaceChildren(
      el("h2", { id: "set-defaults-title", class: "set-sect-title", text: "Each account's defaults" }),
      el("p", { class: "set-sect-intro", text: "What a new post starts with on that account: you can still change it on the post. Posts sent through the API take them too, unless the request sets its own." }),
      list.childNodes.length ? list : el("p", { class: "set-status", text: "No accounts yet." }));
  };
  document.addEventListener("accounts", draw);
  draw();
  return node;
}

function summary(a) {
  const d = a.defaults || {};
  const out = [];
  if (d.content_warning) out.push("content warning “" + d.content_warning + "”");
  if (d.languages && d.languages.length) out.push("languages " + d.languages.join(", "));
  for (const [k, v] of Object.entries(d.options || {})) out.push(k.replace(/_/g, " ") + " " + (v === true ? "on" : v === false ? "off" : v));
  return out.length ? out.join(" · ") : "No defaults: the network's own.";
}

// editDefaults changes an account's defaults: what its capabilities and
// its network's options allow.
async function editDefaults(a) {
  let types = [];
  try { types = await loadDestinations(); } catch (e) { flash("The networks couldn't be read: " + e.message); return; }
  const m = types.find(t => t.type === a.type) || { options: [] };
  const caps = a.capabilities || {};
  const d0 = a.defaults || {};
  const dlg = dialog(nameOf(a.id) + ": defaults");
  const fields = new Map();
  const reads = [];
  let first = null;
  const row = (ptr, label, input, help) => {
    const err = el("p", { class: "fld-err" });
    const id = "def-" + ptr.replace(/\W+/g, "-");
    input.id = id;
    fields.set(ptr, ctl(input, err));
    first = first || input;
    return el("div", { class: "fld" }, el("label", { for: id, class: "fld-label", text: label }), input,
      help ? el("p", { class: "fld-help", text: help }) : null, err);
  };
  const parts = [];
  if (caps.content_warning) {
    const cw = el("input", { type: "text", maxlength: "1000", value: d0.content_warning || "", autocomplete: "off" });
    parts.push(row("/defaults/content_warning", "Content warning (optional)", cw));
    reads.push(out => { const v = cw.value.trim(); if (v) out.content_warning = v; });
  }
  if (caps.languages > 0) {
    const langs = el("input", { type: "text", value: (d0.languages || []).join(", "), autocomplete: "off", spellcheck: "false", placeholder: "en" });
    parts.push(row("/defaults/languages", caps.languages > 1 ? "Languages (optional, up to " + caps.languages + ")" : "Language (optional)", langs,
      "Language tags such as en or pt-BR, separated by commas."));
    reads.push(out => { const v = langs.value.split(",").map(x => x.trim()).filter(Boolean); if (v.length) out.languages = v; });
  }
  const opts = { ...(d0.options || {}) };
  const optRead = [];
  if (Array.isArray(caps.visibility) && caps.visibility.length) {
    const sel = el("select", {}, el("option", { value: "", text: "The account's own" }),
      ...caps.visibility.map(v => el("option", { value: v, text: v, selected: opts.visibility === v ? "" : false })));
    parts.push(row("/defaults/options/visibility", "Visibility", sel));
    optRead.push(o => { if (sel.value) o.visibility = sel.value; });
  }
  for (const spec of m.options || []) {
    const ptr = "/defaults/options/" + spec.key;
    const cur = opts[spec.key];
    let input, read;
    switch (spec.type) {
      case "bool":
        input = el("select", {}, el("option", { value: "", text: "The network's own" }),
          el("option", { value: "true", text: "On", selected: cur === true ? "" : false }),
          el("option", { value: "false", text: "Off", selected: cur === false ? "" : false }));
        read = o => { if (input.value) o[spec.key] = input.value === "true"; };
        break;
      case "enum":
        input = el("select", {}, el("option", { value: "", text: "The network's own" }),
          ...(spec.enum || []).filter(e => e.value !== "").map(e => el("option", { value: e.value, text: e.label || e.value, selected: cur === e.value ? "" : false })));
        read = o => { if (input.value) o[spec.key] = input.value; };
        break;
      case "int":
        input = el("input", { type: "number", step: "1", inputmode: "numeric", value: cur != null ? String(cur) : "",
          min: spec.min != null ? String(spec.min) : false, max: spec.max != null ? String(spec.max) : false });
        read = o => { const t = input.value.trim(); if (t !== "") o[spec.key] = /^-?\d+$/.test(t) ? Number(t) : t; };
        break;
      default:
        input = el("input", { type: "text", value: cur != null ? String(cur) : "", autocomplete: "off", spellcheck: "false" });
        read = o => { const t = input.value.trim(); if (t) o[spec.key] = t; };
    }
    parts.push(row(ptr, spec.label + " (optional)", input, spec.help));
    optRead.push(read);
  }
  if (!parts.length) parts.push(el("p", { class: "set-status", text: networkName(a.type) + " has nothing to set by default." }));
  const ok = el("button", { class: "primary sm", type: "submit", text: "Save" });
  const clear = el("button", { class: "ghost sm", type: "button", text: "Clear all" });
  clear.addEventListener("click", () => send({}));
  dlg.form.append(...parts, dlg.status,
    el("div", { class: "confirm-actions" }, clear, el("button", { class: "ghost sm", type: "button", text: "Cancel", onclick: () => dlg.close() }), ok));
  async function send(body) {
    if (ok.disabled) return;
    ok.disabled = clear.disabled = true;
    dlg.status.textContent = "";
    try {
      await v1("/v1/accounts/" + enc(a.id), { method: "PATCH", body: { defaults: body } });
      await reloadAccounts();
      dlg.close();
      flash("Saved: new posts on " + nameOf(a.id) + " start with them");
    } catch (e) {
      dlg.status.textContent = showProblems(e, fields).join(" ");
    } finally {
      ok.disabled = clear.disabled = false;
    }
  }
  dlg.form.addEventListener("submit", ev => {
    ev.preventDefault();
    const out = {};
    for (const r of reads) r(out);
    const o = {};
    for (const r of optRead) r(o);
    if (Object.keys(o).length) out.options = o;
    send(out);
  });
  dlg.show(first || ok);
}
