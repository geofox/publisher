"use strict";
import { el } from "./common.js";

// Forms built from a destination's field specs (GET /v1/destinations: a
// type's config fields, a way of connecting's fields): one control per
// field, with its label, help and the error the server gives it. The
// server checks every value (ValidateFields); the form only shapes them.

let seq = 0;
const uid = p => p + "-" + (++seq);

const LIST = new Set(["string_list", "url_list"]);

// isList reports whether a field holds a list (typed one per line).
export function isList(spec) { return LIST.has(spec.type); }

// shown is a value as the control shows it.
function shown(spec, v) {
  if (v == null) return "";
  if (isList(spec)) return Array.isArray(v) ? v.join("\n") : String(v);
  return String(v);
}

// field builds one field's control. value is its current value (edit) or
// undefined (a new account: the spec's default fills it in). opts: locked
// (set by the environment: shown, not editable), keep (a saved secret:
// left empty, it stays), warn(value) (a hint, not a refusal, shown as the
// owner types).
export function field(spec, value, { locked = false, keep = false, warn = null } = {}) {
  const id = uid("fld-" + spec.key);
  const helpId = id + "-help", errId = id + "-err", warnId = id + "-warn";
  const start = value !== undefined ? value : spec.default;
  let input;
  const common = { id, name: spec.key, "aria-describedby": helpId + " " + warnId + " " + errId };
  switch (spec.type) {
    case "bool":
      input = el("input", { ...common, type: "checkbox", checked: start ? "" : false });
      break;
    case "int":
      input = el("input", { ...common, type: "number", inputmode: "numeric", step: "1",
        min: spec.min != null ? String(spec.min) : false, max: spec.max != null ? String(spec.max) : false,
        value: start != null ? String(start) : "", placeholder: spec.placeholder || false });
      break;
    case "enum":
      input = el("select", common,
        ...(spec.required ? [] : [el("option", { value: "", text: "—" })]),
        ...(spec.enum || []).map(o => el("option", { value: o.value, text: o.label || o.value, selected: o.value === start ? "" : false })));
      break;
    case "text":
    case "string_list":
    case "url_list":
      input = el("textarea", { ...common, rows: isList(spec) ? "3" : "4", spellcheck: "false",
        placeholder: spec.placeholder || (spec.type === "url_list" ? (spec.url && spec.url.secure ? spec.url.secure + "://…" : "") + "\none per line" : isList(spec) ? "one per line" : false) });
      input.value = shown(spec, start);
      break;
    default: // string, url
      input = el("input", { ...common,
        type: spec.secret ? "password" : spec.type === "url" ? "url" : "text",
        inputmode: spec.type === "url" ? "url" : false,
        // A saved credential is never filled in, and the browser must not
        // offer the owner's own passwords for one.
        autocomplete: "off", "data-1p-ignore": spec.secret ? "" : false, "data-lpignore": spec.secret ? "true" : false,
        spellcheck: "false", autocapitalize: "off", autocorrect: "off",
        maxlength: spec.max_length ? String(spec.max_length) : false,
        placeholder: keep ? "saved: leave empty to keep it" : (spec.placeholder || false),
        value: spec.secret ? "" : shown(spec, start) });
  }
  if (locked) input.disabled = true;
  const needed = spec.required && !locked && !keep;
  if (needed) input.setAttribute("aria-required", "true");
  const optional = !spec.required && !locked && spec.type !== "bool";
  const label = el("label", { for: id, class: "fld-label", text: spec.label + (optional ? " (optional)" : "") });
  const help = el("p", { id: helpId, class: "fld-help" });
  if (locked) help.append(el("span", { class: "fld-locked", text: "Set by the server's environment. " }));
  if (spec.help) help.append(spec.help);
  const warnLine = el("p", { id: warnId, class: "fld-warn", role: "status" });
  const err = el("p", { id: errId, class: "fld-err" });
  const row = el("div", { class: "fld" + (spec.type === "bool" ? " fld-bool" : "") + (locked ? " fld-is-locked" : ""), "data-key": spec.key });
  if (spec.type === "bool") row.append(el("div", { class: "fld-check" }, input, label), help, err);
  else if (spec.secret && !locked) {
    const peek = el("button", { class: "ghost xs fld-peek", type: "button", text: "Show", "aria-controls": id,
      "aria-label": "Show " + spec.label });
    peek.addEventListener("click", () => {
      const show = input.type === "password";
      input.type = show ? "text" : "password";
      peek.textContent = show ? "Hide" : "Show";
      peek.setAttribute("aria-label", (show ? "Hide " : "Show ") + spec.label);
    });
    row.append(label, el("div", { class: "fld-secret" }, input, peek), help, warnLine, err);
  } else row.append(label, input, help, warnLine, err);
  if (!help.childNodes.length) help.hidden = true;
  if (warn) {
    const check = () => { const w = input.value ? warn(input.value) : ""; warnLine.textContent = w || ""; };
    input.addEventListener("input", check);
    input.addEventListener("blur", check);
  }
  input.addEventListener("input", () => setError(""));
  const setError = msg => {
    err.textContent = msg || "";
    if (msg) input.setAttribute("aria-invalid", "true"); else input.removeAttribute("aria-invalid");
  };
  // read is the value to send: undefined when left empty (a new account
  // takes the default; an edit leaves it as it is).
  const read = () => {
    if (spec.type === "bool") return input.checked;
    const raw = input.value;
    if (isList(spec)) {
      const items = raw.split("\n").map(s => s.trim()).filter(Boolean);
      return raw.trim() === "" ? undefined : items;
    }
    const t = raw.trim();
    if (t === "") return undefined;
    if (spec.type === "int") return /^-?\d+$/.test(t) ? Number(t) : t; // the server says what's wrong with "1.5"
    return t;
  };
  return { spec, row, input, read, setError, locked, focus: () => input.focus() };
}

// fieldSet lays out fields: the plain ones, then the advanced ones folded
// away ("Advanced"), which open by themselves when one gets an error.
export function fieldSet(controls) {
  const plain = controls.filter(c => !c.spec.advanced);
  const adv = controls.filter(c => c.spec.advanced);
  const out = plain.map(c => c.row);
  if (adv.length) {
    const d = el("details", { class: "fld-adv" }, el("summary", { text: "Advanced" }), ...adv.map(c => c.row));
    for (const c of adv) c.fold = d;
    out.push(d);
  }
  return out;
}

// showProblems puts a problem's field errors on the fields they name
// (each control's pointer: "/config/pds_url", "/credentials/app_password")
// and answers the messages that name none of them. The first field with
// an error takes focus.
export function showProblems(e, byPointer) {
  for (const c of byPointer.values()) c.setError("");
  const rest = [];
  const errs = (e.problem && Array.isArray(e.problem.errors)) ? e.problem.errors : [];
  let first = null;
  for (const fe of errs) {
    const c = fe.pointer ? byPointer.get(fe.pointer) : null;
    if (c) {
      c.setError(fe.detail || fe.code);
      if (c.fold) c.fold.open = true;
      first = first || c;
    } else rest.push(fe.detail || fe.code);
  }
  if (!errs.length) rest.push(e.message);
  if (first) first.focus();
  return rest;
}

// sameValue compares a control's value with the current one (lists by
// their items).
export function sameValue(spec, a, b) {
  if (isList(spec)) return JSON.stringify(a || []) === JSON.stringify(b || []);
  if (a === undefined || a === null || a === "") return b === undefined || b === null || b === "";
  return a === b;
}
