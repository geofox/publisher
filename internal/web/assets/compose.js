"use strict";
import { el, $, wcount, flash, confirmModal, v1, problemError, TZ, whenText, localInput, pressable, asDialog } from "./common.js";
import { state, byKey, postedText, v1PostBody, ovOf, selected, setTargets, setInflight, getInflight, clearInflight, clearImages, imagesGen, bumpImagesGen } from "./state.js";
import { renderPreview } from "./preview.js";
import { openDetail } from "./history.js";
import { accounts, byId, typeOf, isConnected, defaultTargets, accountsLoaded, networkName, nameOf, accessibleName,
  handleOf, customLabel, isOpaque, shortId, fitHandle } from "./accounts.js";
import { followPost } from "./follow.js";
import { icon, accountTile } from "./brands.js";
import { planFor, lastPlanFor, planError } from "./plan.js";
import { editorInit, renderEditor, setTab } from "./editor.js";
import { fromMarkers, toLF } from "./textpos.js";
import { countFor, limitFor } from "./count.js";

export function markDirty() {
  state.dirty = true;
  state.edits++;
  const el2 = document.getElementById("draft-status");
  if (el2) { el2.className = "draft-status dirty"; el2.textContent = "unsaved changes"; }
  const save = document.getElementById("draft-save");
  if (save) save.disabled = false;
}

// ---------------------------------------------------------------------------
// Counter / character class
// ---------------------------------------------------------------------------

function counterClass(n, limit) {
  if (!limit) return "cnt muted";
  if (n > limit) return "cnt bad";
  if (n > limit * 0.9) return "cnt warn";
  return "cnt ok";
}

// ---------------------------------------------------------------------------
// Live counts. How a post splits is the server's plan (plan.js); the counts
// here keep up with typing, measured the way each platform counts.
// ---------------------------------------------------------------------------

// bskyCardText strips a trailing URL for the Bluesky effective-text estimate,
// mirroring dispatch.PlanBlueskyCard + unfurl.StripTrailing. When a trailing
// URL will become a link card the server posts a shorter text, so the
// client-side counter must count the stripped text — not the raw URL — or it
// diverges from the live preview. Only strips when there are no attached images
// (images take the embed slot, blocking the card) and we're not in interaction
// mode (interactions never carry cards). URL-only posts are left as-is (the URL
// stays in the text even when a card attaches, per PlanBlueskyCard).
const _bskyURLRe = /https?:\/\/[^\s]+/g;
function bskyCardText(text) {
  if (state.images.length > 0 || state.interaction !== null) return text;
  _bskyURLRe.lastIndex = 0;
  let m, last;
  while ((m = _bskyURLRe.exec(text)) !== null) last = m;
  if (!last) return text;
  const url = last[0].replace(/[.,!?);:]+$/, ""); // mirror unfurl.trimURL
  if (text.slice(last.index + url.length).trim() !== "") return text; // not trailing
  const stripped = text.trimEnd().slice(0, text.trimEnd().length - url.length).trimEnd();
  return stripped.trim() === "" ? text : stripped; // URL-only post: keep as-is
}

// effectivePostedText returns the text account id will actually post: on
// Bluesky, a trailing card URL is stripped (server attaches it as a link card).
function effectivePostedText(id) {
  const t = postedText(id);
  return typeOf(id) === "bluesky" ? bskyCardText(t) : t;
}

// threadInfoFor is account id's post count from the server's plan (the last
// one while a newer is on its way).
function threadInfoFor(id) {
  const pl = planFor(id) || lastPlanFor(id);
  const posts = pl && pl.items.length ? pl.items.length : 1;
  return { posts, threaded: posts > 1 };
}

// cwOf is account id's content warning, which Mastodon counts in.
function cwOf(id) {
  const p = typeOf(id), ov = ovOf(id);
  return p === "mastodon" ? ov.spoiler_text || "" : p === "nostr" ? ov.content_warning || "" : "";
}

// targetCount → {text, cls} for an account's live count. State colors mirror the
// design: under 90% → ink2 (no class), >90% → amber, over → indigo (will thread,
// informational), no-limit → teal ∞.
function targetCount(id) {
  const limit = limitFor(id, cwOf(id)), n = countFor(id, effectivePostedText(id));
  if (!limit) return { text: "∞", cls: "inf", n };
  const pct = n / limit;
  return { text: `${n}/${limit}`, cls: n > limit ? "over" : pct > 0.9 ? "near" : "", n };
}

// ---------------------------------------------------------------------------
// Targets — the accounts, grouped by network: selectable rows with avatar,
// handle, live count, switch and thread badge
// ---------------------------------------------------------------------------

function iosSwitch(on) { return el("div", { class: "ios-switch" + (on ? " on" : "") }, el("div", { class: "knob" })); }

function threadBadge(id) {
  const ti = threadInfoFor(id);
  const badge = pressable(el("span", { class: "thread-badge", title: "See and shape the thread",
    onclick: e => { e.stopPropagation(); showThread(id); } }));
  const ic = icon("thread", { size: 12, sw: 1.9 }); if (ic) badge.append(ic);
  badge.append(el("span", { class: "tb-txt", text: `${ti.posts} posts` }));
  return badge;
}

// What the accounts list is doing (main.js reports it): "loading", "error"
// (with the message) or "ready".
let accountsPhase = { phase: "loading", error: "" };
export function setAccountsPhase(phase, error = "") {
  accountsPhase = { phase, error };
  renderChips();
}

const STATUS_TEXT = {
  disabled: "turned off",
  not_configured: "not configured",
  locked: "locked",
  needs_reconnect: "needs reconnecting",
  deleted: "deleted",
};

// unavailableWhy is why account a can't be posted to, or "" when it can.
function unavailableWhy(a) {
  if (isConnected(a)) return "";
  return a.status_reason || STATUS_TEXT[a.status] || a.status || "not available";
}

// barredWhy is why a reply or quote in progress keeps account a out, or "".
// One account of the source's network replies or quotes (the server
// refuses a second); a reply goes only to that network.
function barredWhy(a) {
  const it = state.interaction;
  if (!it || a.id === it.account) return "";
  if (a.type === it.platform) return "One " + networkName(a.type) + " account " + (it.action === "quote" ? "quotes" : "replies") + ": pick it in Interact";
  if (it.action === "reply") return "A reply goes only to " + networkName(it.platform);
  return "";
}

// toggled redraws what a change of selection touches and tells the drafts
// autosave (it listens for change events in the composer).
function toggled(focusId) {
  renderChips(); renderCards(); renderEditor(); renderPreview();
  // A change to something worth keeping (text, attachments, an open draft,
  // a reply): autosave and the unsaved mark follow. Ticking accounts on an
  // empty composer is not unsaved work.
  const c = $("#chips");
  const worth = state.master || state.images.length || state.activeDraftId || state.interaction;
  if (c && worth) c.dispatchEvent(new Event("change", { bubbles: true }));
  // The row was drawn again: keep the keyboard where it was.
  if (focusId) document.querySelector(`#chips [data-account="${CSS.escape(focusId)}"]`)?.focus({ preventScroll: true });
}

function targetRow(a) {
  const id = a.id;
  const on = state.targets.has(id);
  const it = state.interaction;
  const locked = it != null && id === it.account;
  const why = unavailableWhy(a);
  const bar = barredWhy(a);
  // An account that can't be posted to can only be unticked (a draft may
  // name one); the account replying or quoting stays ticked.
  const fixed = locked || (bar && !on) || (why && !on);
  const tc = on || !why ? targetCount(id) : null;
  const ti = threadInfoFor(id);

  // The network's name, and the owner's label for the account: in its place
  // when it names the network already ("Mastodon (work)"), else beside it.
  const label = customLabel(a);
  const named = label && label.toLowerCase().includes(networkName(a.type).toLowerCase());
  const top = el("div", { class: "target-top" }, el("span", { class: "t-name", text: named ? label : networkName(a.type) }));
  if (label && !named) top.append(el("span", { class: "t-label", text: label }));
  if (it && !why && !bar) top.append(el("span", { class: "t-tag" + (locked ? " native" : ""), text: locked ? "native" : "copy" }));
  if (on && ti.threaded) top.append(threadBadge(id));
  const main = el("div", { class: "target-main" }, top);
  const handle = handleOf(a);
  // A long one is cut in the user name, keeping the server.
  if (handle) main.append(el("div", { class: "t-handle", text: fitHandle(handle, 36), title: a.handle }));
  let reason = null;
  if (why) {
    reason = el("div", { class: "t-reason", id: "why-" + id, text: why });
    main.append(reason);
  }

  const row = el("div", {
    class: "target-row p-" + a.type + (on ? "" : " off") + (locked ? " locked" : "") + (bar ? " barred" : "") + (why ? " unavailable" : ""),
    "data-account": id, "data-type": a.type,
    title: bar || null,
    onclick: () => {
      if (fixed) return;
      on ? state.targets.delete(id) : state.targets.add(id);
      state.chosen = true;
      if (!state.targets.has(state.focus)) state.focus = state.targets.has(id) ? id : (selected()[0] || "");
      toggled(id);
    },
  },
    accountTile(a),
    main,
    tc ? el("span", { class: "t-count" + (tc.cls ? " " + tc.cls : ""), text: tc.text }) : null,
    iosSwitch(on),
  );
  pressable(row, "switch");
  row.setAttribute("aria-checked", on ? "true" : "false");
  row.setAttribute("aria-label", accessibleName(id));
  if (reason) row.setAttribute("aria-describedby", reason.id);
  else if (bar) row.setAttribute("aria-description", bar);
  if (fixed) row.setAttribute("aria-disabled", "true");
  return row;
}

function renderChips() {
  const c = $("#chips"); if (!c) return;
  const hadFocus = c.contains(document.activeElement);
  c.replaceChildren();
  const list = accounts();
  // What the empty states say, also told to assistive technology (a live
  // region that outlives the redraws).
  let live = $("#chips-status");
  if (!live) { live = el("span", { id: "chips-status", class: "sr-only", role: "status" }); c.after(live); }
  if (!list.length) {
    const phase = accountsPhase.phase === "ready" ? "empty" : accountsPhase.phase;
    c.dataset.state = phase;
    let say;
    if (phase === "loading") c.append(el("p", { class: "targets-empty muted", text: say = "Reading your accounts…" }));
    else if (phase === "error") {
      const again = el("button", { class: "ghost sm", type: "button", text: "Try again",
        onclick: () => {
          if (!reloadAccounts) return;
          setAccountsPhase("loading");
          reloadAccounts().catch(() => {}); // a failure shows the error again
        } });
      c.append(el("div", { class: "targets-empty" },
        el("span", { class: "bad", text: say = "Couldn't read your accounts: " + accountsPhase.error }), again));
      if (hadFocus) again.focus({ preventScroll: true }); // the keyboard stays on Try again
    } else {
      // Settings says how: added there, or from the server's environment.
      const open = el("button", { class: "ghost sm", type: "button", text: "Open Settings",
        onclick: () => document.dispatchEvent(new CustomEvent("open-settings", { detail: "accounts" })) });
      c.append(el("div", { class: "targets-empty" },
        el("span", { class: "muted", text: say = "No accounts yet: add one in Settings." }), open));
    }
    if (hadFocus && !c.contains(document.activeElement)) { c.tabIndex = -1; c.focus({ preventScroll: true }); }
    live.textContent = say;
    renderTargetsMeta();
    renderThreadNotice();
    return;
  }
  c.dataset.state = "ready";
  // One group per network, in the accounts' order.
  const groups = [];
  for (const a of list) {
    let g = groups.find(x => x.type === a.type);
    if (!g) groups.push(g = { type: a.type, accts: [] });
    g.accts.push(a);
  }
  for (const g of groups) {
    const box = el("div", { class: "target-group p-" + g.type, role: "group", "aria-label": networkName(g.type) + " accounts" });
    if (g.accts.length > 1) {
      const head = el("div", { class: "tg-head" },
        el("span", { class: "tg-name", text: networkName(g.type) }),
        el("span", { class: "tg-n muted", text: `${g.accts.length} accounts` }));
      // Select all: the accounts of the network that can be ticked.
      const pickable = g.accts.filter(a => !unavailableWhy(a) && !barredWhy(a) && !(state.interaction && a.id === state.interaction.account));
      if (pickable.length > 1) {
        const allOn = pickable.every(a => state.targets.has(a.id));
        head.append(el("button", { class: "ghost xs tg-all", type: "button",
          text: allOn ? "Clear" : "Select all",
          "aria-label": (allOn ? "Clear all " : "Select all ") + networkName(g.type) + " accounts",
          onclick: () => {
            for (const a of pickable) allOn ? state.targets.delete(a.id) : state.targets.add(a.id);
            state.chosen = true;
            if (!state.targets.has(state.focus)) state.focus = selected()[0] || "";
            toggled(null);
            document.querySelector(`#chips .target-group.p-${CSS.escape(g.type)} .tg-all`)?.focus({ preventScroll: true });
          } }));
      }
      box.append(head);
    }
    for (const a of g.accts) box.append(targetRow(a));
    c.append(box);
  }
  live.textContent = "";
  // The keyboard was on Try again (or the loading line): on to the first row.
  if (hadFocus && !c.contains(document.activeElement)) c.querySelector(".target-row")?.focus({ preventScroll: true });
  renderTargetsMeta();
  renderThreadNotice();
}

// refreshTargets updates each row's count + thread badge in place on every
// keystroke (rebuilding rows would re-create the avatars and flicker).
function refreshTargets() {
  for (const row of document.querySelectorAll("#chips .target-row")) {
    const id = row.dataset.account;
    const on = state.targets.has(id);
    const cnt = row.querySelector(".t-count");
    if (cnt) {
      const tc = targetCount(id);
      cnt.textContent = tc.text; cnt.className = "t-count" + (tc.cls ? " " + tc.cls : "");
    }
    const top = row.querySelector(".target-top");
    const ti = threadInfoFor(id);
    let badge = top.querySelector(".thread-badge");
    if (on && ti.threaded) {
      if (!badge) { badge = threadBadge(id); top.append(badge); }
      else badge.querySelector(".tb-txt").textContent = `${ti.posts} posts`;
    } else if (badge) { badge.remove(); }
  }
  renderTargetsMeta();
  renderThreadNotice();
}

function renderTargetsMeta() {
  const n = state.targets.size;
  const m = $("#targets-meta"); if (m) m.textContent = `${n} selected`;
  const aud = $("#compose-audience");
  if (aud) aud.textContent = n ? `Public · mirror to ${n}` : "Public";
}

// applyIdentity swaps the placeholder author for the owner's profile (the
// first account with a display name, accounts.js identity). Failures leave
// the placeholder untouched.
export function applyIdentity(id) {
  if (!id) return;
  const nameEl = $("#compose-author");
  if (nameEl && id.name) nameEl.textContent = isOpaque(id.name) ? shortId(id.name) : id.name;
  const av = $("#compose-avatar");
  if (av) {
    if (id.avatar) {
      av.style.background = "none";
      av.textContent = "";
      av.append(el("img", { src: id.avatar, alt: "", class: "avatar-img" }));
    } else if (id.monogram) {
      av.textContent = id.monogram;
    }
  }
}

// rerenderComposer redraws every composer view, after something they all
// read changed (the accounts arrived: limits, handles, statuses).
export function rerenderComposer() {
  renderChips(); renderCards(); renderMeta(); renderEditor(); renderPreview();
}

// showThread opens account id's tab in the thread editor.
function showThread(id) {
  setTab(id);
  document.getElementById("editor")?.scrollIntoView({ behavior: "smooth", block: "start" });
}

// renderThreadNotice shows the calm informational banner above POST TO whenever
// ≥1 selected target is a thread; tapping it opens that account's tab.
function renderThreadNotice() {
  const host = $("#threadnotice"); if (!host) return;
  host.innerHTML = "";
  const threaded = selected().filter(id => threadInfoFor(id).threaded);
  if (!threaded.length) return;
  const names = threaded.map(id => `${nameOf(id)} (${threadInfoFor(id).posts})`).join(", ");
  const banner = pressable(el("div", { class: "thread-notice", onclick: () => showThread(threaded[0]) }));
  const ic = icon("thread", { size: 20, sw: 1.8 }); if (ic) banner.append(ic);
  banner.append(el("div", { class: "tn-body" },
    el("div", { class: "tn-title", text: `Auto-threading on ${names}` }),
    el("div", { class: "tn-sub", text: "Each account gets a thread that fits its limit: tap to see and shape it." }),
  ));
  const ch = icon("chevron", { size: 16 }); if (ch) banner.append(ch);
  host.append(banner);
}

// ---------------------------------------------------------------------------
// Interaction mode — source banner + constrained chips
// ---------------------------------------------------------------------------

// startInteraction puts Compose into interaction mode for a resolved source post
// (src is a resolved source, see interact.js: src.account is the account that
// replies or quotes) and switches to the Compose tab. action is "reply" | "quote".
export function startInteraction(src, action) {
  if (!src.account) { flash("No " + networkName(src.platform) + " account is connected to " + action + " from"); return; }
  const begin = () => {
    state.interaction = {
      action, account: src.account, platform: src.platform, ref: src.ref,
      sourcePreview: src.preview, sourceURL: src.preview.web_url, sourceAuthor: src.preview.author_handle,
      sourceInput: src.input || src.preview.web_url, // what POST /v1/posts resolves again
      caps: src.caps,
      // restored on exit; preserve the pre-interaction selection across a nested
      // re-entry (starting a new interaction while already in one).
      prevTargets: state.interaction && state.interaction.prevTargets
        ? state.interaction.prevTargets : new Set(state.targets),
    };
    state.master = "";
    state.breaks = [];
    state.tab = "shared";
    clearImages();
    state.ov = byKey(); // drop stale per-account settings
    setTargets([src.account]);
    state.focus = src.account;
    detachDraft(); // the reply is a new post: it must not save over, or use up, the draft that was open
    const tab = document.querySelector('.tab[data-view="compose"]');
    if (tab) tab.click();
    window.scrollTo({ top: 0, behavior: "smooth" });
    flash((action === "quote" ? "Quoting " : "Replying to ") + (src.preview.author_handle || src.platform));
    renderImages(); // entry cleared state.images → refresh the thumbnail strip (symmetric with exit)
    renderInteractionUI();
  };
  if (state.master.trim() || state.images.length) {
    confirmModal({
      title: "Replace your current draft?",
      body: "Starting a " + action + " will clear what you've written in Compose.",
      confirmText: "Replace draft",
      onConfirm: async () => { begin(); return true; },
    });
    return;
  }
  begin();
}

// exitInteraction returns Compose to a normal new post.
export function exitInteraction() {
  const prev = state.interaction && state.interaction.prevTargets;
  state.interaction = null;
  state.master = "";
  state.breaks = [];
  state.tab = "shared";
  clearImages();
  setTargets(prev && prev.size ? [...prev].filter(id => byId(id)) : defaultTargets());
  if (!state.targets.has(state.focus)) state.focus = selected()[0] || "";
  detachDraft(); // what follows is a new post, not the draft the reply came from
  renderImages();
  renderInteractionUI();
}

// detachDraft makes what Compose holds a new, unsaved post: saving or
// posting it no longer touches the draft that was open, and that draft's
// tags go with it. drafts.js listens to redraw its controls.
export function detachDraft() {
  state.activeDraftId = null;
  state.draftETag = "";
  const tags = document.querySelector("#draft-tags-input .tag-chips"); if (tags) tags.replaceChildren();
  // Unsaved edits of that draft must not come back as its recovery.
  import("./drafts_recovery.js").then(m => m.clearRecovery());
  document.dispatchEvent(new Event("draft-detached"));
}

// loadDraft hydrates Compose from a version-3 spec (snapshot.js: master_text,
// breaks, targets, overrides and anchors by account id, a resolved
// interaction, plus a media[] array of already-uploaded references): a draft
// read back (drafts.js specFromDraft) or a recovery snapshot upgraded first.
// { text, lang } is the History translate-to-Compose shape.
export function loadDraft(input) {
  if (!input) return;

  const doLoad = () => {
    // legacy {text, lang}
    if (input.text !== undefined && input.master_text === undefined) {
      state.interaction = null;
      // An old post's text may break its thread with lines of "---".
      const conv = fromMarkers(input.text || "");
      state.master = conv.text;
      state.breaks = conv.breaks;
      state.anchors = {};
      state.tab = "shared";
      // The settings, schedule and tags were made for the post it replaces:
      // a translation starts from the defaults, like a new post.
      state.ov = byKey();
      clearImages();
      const sched = $("#schedat"); if (sched) sched.value = "";
      if (input.lang) {
        for (const a of accounts()) {
          if (a.type === "bluesky") ovOf(a.id).langs = input.lang;
          if (a.type === "mastodon") ovOf(a.id).language = input.lang;
        }
      }
      setTargets(defaultTargets());
      if (!state.targets.has(state.focus)) state.focus = selected()[0] || "";
      detachDraft();
      state.dirty = true;
      const tab = document.querySelector('.tab[data-view="compose"]');
      if (tab) tab.click();
      window.scrollTo({ top: 0, behavior: "smooth" });
      renderImages();
      renderInteractionUI(); // re-renders chips/cards/preview and hides the banner
      updateSched();
      flash("Translated draft loaded" + (input.lang ? " · lang " + input.lang : ""));
      return;
    }

    // full spec shape (version 3; snapshot.js upgrades older ones)
    state.interaction = input.interaction || null;
    state.master = input.master_text || "";
    state.breaks = (input.breaks || []).filter(Number.isInteger);
    state.tab = "shared";
    clearImages();
    // The accounts it names that are still here (the caller said which
    // weren't: `dropped`). The defaults only when it named none: when every
    // one it named is gone, nothing is selected, so Post can't go somewhere
    // the owner didn't pick. A reply or quote keeps its acting account.
    let named = (input.targets || []).filter(id => byId(id));
    const actor = input.interaction && input.interaction.account;
    if (actor) named = [actor, ...named.filter(id => id !== actor)];
    const hadTargets = (input.targets || []).length > 0 || (input.dropped || []).length > 0;
    setTargets(named.length || hadTargets ? named : defaultTargets());
    if (!state.targets.has(state.focus)) state.focus = selected()[0] || "";
    // Each account's network defaults, then its saved settings (checked by
    // snapshot.js cleanOv).
    state.ov = byKey();
    for (const [id, ov] of Object.entries(input.overrides || {})) {
      if (!byId(id) || !ov || typeof ov !== "object") continue;
      Object.assign(ovOf(id), ov);
    }
    // images come from input.media (hydrated draft) — already-uploaded references
    state.images = (input.media || []).map(m => ({
      blossom_url: m.blossom_url, sha256: m.sha256, mime: m.mime,
      dim: m.dim, blurhash: m.blurhash, size_bytes: m.size_bytes, alt: m.alt || "",
      ordinal: m.ordinal, file: null, media_id: m.media_id || "",
      duration_secs: m.duration_secs || 0,
      video: /^video\//.test(m.mime || ""),
      poster_url: m.poster_url || "",
      // derive a displayable URL from blossom_url for the thumbnail strip
      url: m.blossom_url || "",
      // restored video drafts are already transcoded — render as ready
      phase: /^video\//.test(m.mime || "") ? "ready" : undefined,
      id: m.client_id || m.id || newKey(),
    }));
    bumpImagesGen();
    // Media positions, by attachment id.
    state.anchors = {};
    for (const [id, v] of Object.entries(input.anchors || {})) {
      if (Number.isInteger(v) && v > 0) state.anchors[id] = v;
    }
    normalizeLines();
    state.activeDraftId = input.id || null;
    state.draftETag = input.etag || "";
    state.dirty = false;
    const num = $("#threadnum"); if (num && input.number !== undefined) num.checked = !!input.number;
    const sched = $("#schedat"); if (sched) sched.value = input.interaction ? "" : localInput(input.scheduled_at);
    const tab = document.querySelector('.tab[data-view="compose"]');
    if (tab) tab.click();
    window.scrollTo({ top: 0, behavior: "smooth" });
    renderImages();
    renderInteractionUI(); // re-renders chips/cards/preview and hides the banner
    updateSched(); // a loaded schedule shows (and the button says Schedule)
    // A note to read must outlast the usual short flash.
    if (input.notice) flash(input.notice, 8000); else flash("Draft loaded");
    if (input.onLoaded) input.onLoaded();
  };

  if (state.master.trim() || state.images.length || state.interaction) {
    confirmModal({
      title: "Replace your current draft?",
      body: "Loading this draft will replace what's in Compose.",
      confirmText: "Replace",
      onConfirm: async () => { doLoad(); return true; },
    });
    return;
  }
  doLoad();
}

// normalizeLines turns CRLF in the loaded texts into LF, as the cards show
// them, moving every position with it (a draft made through the API may
// have CRLF).
function normalizeLines() {
  const sh = toLF(state.master);
  state.master = sh.text;
  state.breaks = state.breaks.map(sh.move);
  for (const k of Object.keys(state.anchors)) state.anchors[k] = sh.move(state.anchors[k]);
  for (const ov of Object.values(state.ov)) {
    if (ov.text == null) {
      if (ov.breaks) ov.breaks = ov.breaks.map(sh.move);
      continue;
    }
    const own = toLF(ov.text);
    ov.text = own.text;
    if (ov.breaks) ov.breaks = ov.breaks.map(own.move);
    if (ov.mediaAt) for (const k of Object.keys(ov.mediaAt)) ov.mediaAt[k] = own.move(ov.mediaAt[k]);
  }
}


// renderInteractionUI re-renders the compose chrome for the current mode.
function renderInteractionUI() {
  renderSrcBanner();
  const sched = $("#schedrow"); if (sched) sched.hidden = state.interaction != null;
  updateSched(); // the button: Reply, Quote, Schedule or Post
  renderChips(); renderCards(); renderMeta(); renderEditor(); renderPreview();
}

// renderSrcBanner shows the source post being replied-to / quoted (or hides it).
function renderSrcBanner() {
  const host = $("#srcbanner"); if (!host) return;
  host.innerHTML = "";
  const it = state.interaction;
  if (!it) { host.hidden = true; return; }
  host.hidden = false;
  const verb = it.action === "quote" ? "Quoting" : "Replying to";
  host.append(el("div", { class: "srcb-head" },
    el("span", { class: "srcb-verb", text: verb + " " }),
    el("span", { class: "srcb-author", text: it.sourceAuthor || networkName(it.platform) }),
    el("button", { class: "srcb-x", type: "button", text: "← cancel", onclick: exitInteraction }),
  ));
  host.append(el("div", { class: "srcb-text", text: (it.sourcePreview && it.sourcePreview.text) || "" }));
  const sp = it.sourcePreview || {};
  if (sp.media && sp.media.length) {
    const g = el("div", { class: "srcb-media" });
    for (const m of sp.media) g.append(el("img", { src: m.url, alt: m.alt || "" }));
    host.append(g);
  }
  const foot = el("div", { class: "srcb-foot" });
  const actor = nameOf(it.account);
  foot.append(el("span", { class: "srcb-hint muted",
    text: it.action === "quote"
      ? actor + " quotes it; tick accounts on other networks to also post a copy there."
      : actor + " replies; a reply goes only to " + networkName(it.platform) + ": quote the post to share it elsewhere." }));
  if (it.sourceURL) foot.append(el("a", { class: "srcb-link", href: it.sourceURL, target: "_blank", rel: "noopener", text: "open original ↗" }));
  host.append(foot);
  // The action is offered only when allowed (interact.js); a note it came
  // with (a quote that may be shown as a link) is shown here.
  const cap = it.caps && it.caps[it.action];
  if (cap && cap.reason) host.append(el("div", { class: "srcb-note muted", text: cap.reason }));
}

// ---------------------------------------------------------------------------
// Override cards (collapsed summary → expand to edit)
// ---------------------------------------------------------------------------

function renderCards() {
  const c = $("#cards"); c.innerHTML = "";
  for (const id of selected()) c.append(overrideCard(id));
  renderMeta();
}

// refreshCounts updates the per-account counts (and any open mirroring card's
// textarea) in place on every master keystroke, instead of rebuilding all the
// override cards — which is expensive and would collapse expanded cards as you
// type. Edited cards track their own text, so they're left untouched.
function refreshCounts() {
  for (const card of document.querySelectorAll("#cards .ocard")) {
    const id = card.dataset.account;
    if (!state.targets.has(id)) continue;
    const limit = limitFor(id, cwOf(id));
    const n = countFor(id, effectivePostedText(id)); // full posted text (reproduction on fan-out) — matches the preview
    const span = card.querySelector(".cnt");
    if (span) { span.textContent = limit ? `${n}/${limit}` : "∞"; span.className = counterClass(n, limit); }
    const snip = card.querySelector(".ocard-snip");
    if (snip) snip.textContent = wordingNote(id);
  }
}

// wordingNote says where account id's text comes from.
function wordingNote(id) {
  const ov = ovOf(id);
  if (ov.text != null) return `own text: "${ov.text.slice(0, 40)}"`;
  return ov.breaks ? "shared text · own breaks" : "mirrors the shared text";
}

// overrideCard is account id's options (its network's). Its wording and
// breaks are edited in the thread editor (its tab there).
function overrideCard(id) {
  const p = typeOf(id), ov = ovOf(id);
  const edited = ov.text != null || ov.breaks != null;
  const limit = limitFor(id, cwOf(id)), n = countFor(id, effectivePostedText(id));
  const card = el("div", { class: "ocard p-" + p + (edited ? " edited" : ""), "data-account": id });

  // collapsed summary row (always visible, clickable to expand)
  const cnt = el("span", { class: counterClass(n, limit), text: limit ? `${n}/${limit}` : "∞" });
  const body = el("div", { class: "ocard-body", hidden: true });
  const caret = el("span", { class: "ocard-caret", text: "▸" });
  const summary = el("button", { class: "ocard-summary", type: "button",
    onclick: () => {
      const open = body.hidden; body.hidden = !open; caret.textContent = open ? "▾" : "▸";
      if (open) { state.focus = id; renderPreview(); }
    } },
    caret,
    el("span", { class: "ocard-name", text: nameOf(id) }),
    cnt,
    el("span", { class: "ocard-snip muted", text: wordingNote(id) }),
  );
  summary.setAttribute("aria-label", accessibleName(id) + " options");
  const wording = el("button", { class: "ghost xs", type: "button", text: `${nameOf(id)}'s thread and wording →`,
    onclick: () => showThread(id) });
  body.append(fieldsFor(p, ov, () => { refreshCounts(); refreshTargets(); renderMeta(); renderPreview(); }), wording);

  card.append(summary, body);
  return card;
}

// ---------------------------------------------------------------------------
// Plain micro-label helper + per-platform fields
// ---------------------------------------------------------------------------

function lbl(t, input) { return el("label", {}, el("span", { class: "fl", text: t }), input); }

// langField builds the per-platform language widget. When the operator has
// configured 2+ languages (USER_LANGUAGES env, surfaced via /v1/instance →
// state.userLanguages), it's a dropdown of those codes; with 0 or 1 it's a
// static muted line — the value is still set (via defaultOv) just not editable.
// onChange receives the new code; renderPreview re-runs after a pick.
function langField(value, onChange) {
  const langs = state.userLanguages;
  if (langs.length > 1) {
    const sel = el("select", { onchange: e => { onChange(e.target.value); renderPreview(); } });
    for (const code of langs) sel.append(el("option", { value: code, text: code }));
    sel.value = langs.includes(value) ? value : langs[0];
    return sel;
  }
  return el("span", { class: "lang-static muted", text: value });
}

function fieldsFor(p, ov, onText = renderPreview) {
  const f = el("div", { class: "fields" });
  if (p === "bluesky") {
    f.append(lbl("lang (ISO 639-1)", langField(ov.langs, v => { ov.langs = v; })));
    const rsel = el("select", { onchange: e => { ov.reply = e.target.value; renderPreview(); } });
    [["", "anyone"], ["nobody", "nobody"], ["following", "following"], ["follower", "followers"], ["mention", "mentioned"]]
      .forEach(([v, t]) => rsel.append(el("option", { value: v, text: t })));
    rsel.value = ov.reply;
    f.append(lbl("replies", rsel));
    f.append(lbl("disable quotes", el("input", { type: "checkbox", checked: ov.disable_quotes, onchange: e => { ov.disable_quotes = e.target.checked; renderPreview(); } })));
  }
  if (p === "mastodon") {
    f.append(lbl("CW", el("input", { type: "text", value: ov.spoiler_text, oninput: e => { ov.spoiler_text = e.target.value; onText(); } })));
    f.append(lbl("sensitive", el("input", { type: "checkbox", checked: !!ov.sensitive, onchange: e => { ov.sensitive = e.target.checked; renderPreview(); } })));
    const sel = el("select", { onchange: e => { ov.visibility = e.target.value; renderPreview(); } });
    [["", "default"], ["public", "public"], ["unlisted", "unlisted"], ["private", "private"], ["direct", "direct"]]
      .forEach(([v, t]) => sel.append(el("option", { value: v, text: t })));
    sel.value = ov.visibility;
    f.append(lbl("visibility", sel));
    f.append(lbl("lang (ISO 639-1)", langField(ov.language, v => { ov.language = v; })));
  }
  if (p === "threads") {
    f.append(lbl("topic", el("input", { type: "text", value: ov.topic_tag, oninput: e => { ov.topic_tag = e.target.value; renderPreview(); } })));
    const tsel = el("select", { onchange: e => { ov.reply_control = e.target.value; renderPreview(); } });
    [["", "anyone"], ["accounts_you_follow", "following"], ["followers_only", "followers"], ["mentioned_only", "mentioned"], ["parent_post_author_only", "author only"]]
      .forEach(([v, t]) => tsel.append(el("option", { value: v, text: t })));
    tsel.value = ov.reply_control;
    f.append(lbl("replies", tsel));
  }
  if (p === "nostr") {
    f.append(lbl("PoW", el("input", { type: "number", value: ov.pow, min: 0, oninput: e => { ov.pow = parseInt(e.target.value || "0", 10); renderPreview(); } })));
    f.append(lbl("CW", el("input", { type: "text", value: ov.content_warning, oninput: e => { ov.content_warning = e.target.value; renderPreview(); } })));
  }
  return f;
}

// ---------------------------------------------------------------------------
// Images
// ---------------------------------------------------------------------------

// v1Form sends a multipart form to /v1 and answers its JSON, or throws the
// problem it answers with (see common.js problemError).
async function v1Form(path, fd, signal) {
  const r = await fetch(path, { method: "POST", body: fd, credentials: "same-origin", signal });
  if (!r.ok) throw problemError(r.status, await r.json().catch(() => null));
  return r;
}

// compressFile sends a File through POST /v1/media:compress and returns the
// re-encoded JPEG as a new File. Used by the per-attachment presets and the
// HEIC attach-time auto-conversion. Aborts any in-flight request for the same
// attachment (rapid preset switching must not pile up server transcodes).
async function compressFile(img, file, preset) {
  if (img._compressAbort) img._compressAbort.abort();
  const ctl = new AbortController();
  img._compressAbort = ctl;
  try {
    const fd = new FormData();
    fd.append("file", file);
    fd.append("preset", preset);
    const r = await v1Form("/v1/media:compress", fd, ctl.signal);
    const blob = await r.blob();
    const stem = (file.name || "image").replace(/\.[^.]+$/, "");
    return new File([blob], stem + ".jpg", { type: blob.type || "image/jpeg" });
  } finally {
    if (img._compressAbort === ctl) img._compressAbort = null;
  }
}

// uploadImage stores an attachment's current file through POST /v1/media,
// once per file (a preset pick makes a new file, uploaded again), so a post
// names it by id. The upload starts at attach time; Post waits for it. The
// alt text goes with the post, not the upload, so editing it needs no call.
function uploadImage(img) {
  const file = img.file;
  if (!file) return Promise.resolve();
  if (img._uploadFor === file) return img._upload || Promise.resolve();
  if (img._uploadAbort) img._uploadAbort.abort();
  const ctl = new AbortController();
  Object.assign(img, { _uploadFor: file, _uploadAbort: ctl, media_id: "", remote_url: "", upload_error: "" });
  const fd = new FormData();
  fd.append("file", file);
  const run = v1Form("/v1/media", fd, ctl.signal).then(r => r.json()).then(m => {
    if (img._uploadFor !== file) return;
    Object.assign(img, { media_id: m.id, remote_url: m.url || "", sha256: m.sha256 || "", blurhash: m.blurhash || "",
      mime: m.mime || img.mime, dim: m.dim || img.dim, size_bytes: m.size_bytes || img.size_bytes });
    renderPreview(); // the server preview can plan it now
  }).catch(err => {
    if (img._uploadFor !== file || err.name === "AbortError") return;
    img._uploadFor = null; // a retry uploads again
    img.upload_error = err.message || "upload failed";
    renderImages();
  }).finally(() => {
    if (img._uploadAbort === ctl) { img._upload = null; img._uploadAbort = null; }
  });
  img._upload = run;
  return run;
}

function fmtBytes(n) {
  return n >= 1048576 ? (n / 1048576).toFixed(1) + " MB" : Math.max(1, Math.round(n / 1024)) + " KB";
}

const isHeic = (f) => /^image\/hei[cf]$/.test(f.type) || /\.hei[cf]$/i.test(f.name || "");

function fmtDuration(s) { const m = Math.floor(s / 60); return m ? m + "m" + String(s % 60).padStart(2, "0") + "s" : s + "s"; }

// updateVideoProgress refreshes a video tile's progress without rebuilding the
// whole strip; falls back to a full render on phase changes or detached nodes.
function updateVideoProgress(img) {
  if (img._renderedPhase === img.phase && img._fillEl?.isConnected) {
    const pct = Math.round((img.pct || 0) * 100);
    img._labelEl.textContent = img.phase + " " + pct + "%";
    img._fillEl.style.width = pct + "%";
    return;
  }
  renderImages();
}

// One video XOR images per post (v1); a video uploads + transcodes
// immediately via the async job endpoint with three-phase progress:
// uploading (XHR), transcoding and storing (1 s job poll). The quality preset
// is chosen at attach time (#vidpreset) — re-deriving would be a minutes-long
// transcode and the raw original is deleted after normalization.
function attachVideo(file) {
  if (state.images.length) { flash(state.images.some(i => i.video) ? "Only one video per post" : "Remove the images first (one video OR images per post)"); return; }
  const entry = { video: true, _file: file, _preset: $("#vidpreset")?.value || "1080p",
                  url: "", alt: "", phase: "uploading", pct: 0, _xhr: null, _jobTimer: null };
  entry.id = newKey();
  state.images.push(entry);
  renderImages();
  startVideoUpload(entry);
}

function startVideoUpload(entry) {
  const gen = imagesGen;
  const xhr = new XMLHttpRequest();
  entry._xhr = xhr;
  // XHR, not fetch: only XHR reports upload progress.
  xhr.open("POST", "/v1/media");
  xhr.upload.onprogress = e => {
    if (e.lengthComputable) { entry.pct = e.loaded / e.total; updateVideoProgress(entry); }
  };
  xhr.onerror = () => failVideo(entry, gen, "upload failed");
  xhr.onload = () => {
    if (gen !== imagesGen) return;
    let m = null;
    try { m = JSON.parse(xhr.responseText); } catch { /* not json */ }
    if (xhr.status !== 202 && xhr.status !== 201) {
      failVideo(entry, gen, problemError(xhr.status, m).message);
      return;
    }
    entry._xhr = null;
    entry.phase = "queued"; entry.pct = 0; renderImages();
    const done = media => {
      clearInterval(entry._jobTimer); entry._jobTimer = null;
      Object.assign(entry, { phase: "ready", pct: 1, media_id: media.id, blossom_url: media.url, remote_url: media.url,
        sha256: media.sha256, mime: media.mime, dim: media.dim, size_bytes: media.size_bytes,
        duration_secs: media.duration_secs || 0, url: media.url, poster_url: media.poster_url || "" });
      entry._file = null;
      renderImages();
    };
    if (m.status === "ready") { done(m); return; }
    // GET /v1/media/{id} says how far processing is: its stage (queued,
    // transcoding, uploading to media storage) and progress, until the video
    // is ready or failed.
    entry._jobTimer = setInterval(async () => {
      if (gen !== imagesGen) { clearInterval(entry._jobTimer); return; }
      try {
        const cur = await v1("/v1/media/" + encodeURIComponent(m.id));
        if (gen !== imagesGen || !entry._jobTimer) return;
        if (cur.status === "failed") throw new Error(cur.error || "processing failed");
        if (cur.status === "ready") { done(cur); return; }
        const prevPhase = entry.phase, prevPct = entry.pct;
        const stage = (cur.processing && cur.processing.stage) || "queued";
        entry.phase = stage === "uploading" ? "storing" : stage;
        entry.pct = (cur.processing && cur.processing.progress) || 0;
        if (entry.phase !== prevPhase || entry.pct !== prevPct) updateVideoProgress(entry);
      } catch (err) {
        clearInterval(entry._jobTimer); entry._jobTimer = null;
        failVideo(entry, gen, err.message);
      }
    }, 1000);
  };
  const fd = new FormData();
  fd.append("preset", entry._preset || "1080p");
  fd.append("file", entry._file);
  xhr.send(fd);
}

function failVideo(entry, gen, msg) {
  if (gen !== imagesGen) return;
  entry.phase = "failed"; entry.error = msg; entry.pct = 0;
  entry._xhr = null; entry._jobTimer = null;
  renderImages();
}

export function renderImages() {
  const c = $("#images"); c.innerHTML = "";
  state.images.forEach((img, i) => {
    if (img.video) {
      const tile = el("div", { class: "thumb vthumb" });
      if (img.phase === "ready") {
        const vAttrs = { src: img.url, preload: "metadata", muted: "muted", controls: "controls", playsinline: "playsinline" };
        if (img.poster_url) vAttrs.poster = img.poster_url;
        tile.append(el("video", vAttrs),
          el("input", { type: "text", placeholder: "alt text", value: img.alt,
            oninput: e => { img.alt = e.target.value; renderPreview(); } }),
          el("div", { class: "imgsize", text: fmtBytes(img.size_bytes) + " · " + fmtDuration(img.duration_secs) }));
      } else if (img.phase === "failed") {
        const fr = el("div", { class: "vprog" },
          el("div", { class: "vprog-label vfail", text: "failed: " + (img.error || "unknown") }));
        if (img._file) {
          fr.append(el("button", { class: "rm", type: "button", text: "retry",
            onclick: () => { img.phase = "uploading"; img.pct = 0; img.error = ""; renderImages(); startVideoUpload(img); } }));
        }
        tile.append(fr);
      } else {
        const label = el("div", { class: "vprog-label", text: img.phase + " " + Math.round((img.pct || 0) * 100) + "%" });
        const fill = el("div", { class: "vprog-fill", style: "width:" + Math.round((img.pct || 0) * 100) + "%" });
        img._labelEl = label; img._fillEl = fill; img._renderedPhase = img.phase;
        tile.append(el("div", { class: "vprog" }, label, el("div", { class: "vprog-bar" }, fill)));
      }
      tile.append(el("button", { class: "rm", type: "button", text: "remove",
        onclick: () => {
          if (img._xhr) img._xhr.abort();           // mid-upload: cancel the transfer
          if (img._jobTimer) clearInterval(img._jobTimer); // job continues server-side by design
          state.images.splice(i, 1); renderImages();
        } }));
      c.append(tile);
      return;
    }
    const thumb = el("img", { src: img.url, alt: "",
      onload: e => {
        // Capture intrinsic dims once; the preview sends them as fit-planning
        // metadata. Guard prevents a render loop.
        if (!img.dim && e.target.naturalWidth) {
          img.dim = e.target.naturalWidth + "x" + e.target.naturalHeight;
          renderPreview();
        }
      } });
    const kids = [thumb,
      el("input", { type: "text", placeholder: "alt text", value: img.alt,
        oninput: e => { img.alt = e.target.value; renderPreview(); } })];
    // Size line + compress preset menu — fresh attachments only (restored
    // draft images are Blossom references without local bytes).
    if (img.orig) {
      const sizeTxt = img.file !== img.orig
        ? `${fmtBytes(img.orig.size)} → ${fmtBytes(img.file.size)}`
        : fmtBytes(img.file.size);
      kids.push(el("div", { class: "imgsize", text: sizeTxt }));
      const sel = el("select", { class: "preset", title: "compress",
        onchange: async ev => {
          const v = ev.target.value;
          // A pending compress for this attachment is now superseded no
          // matter what was picked — original included.
          if (img._compressAbort) { img._compressAbort.abort(); img._compressAbort = null; }
          // Until the new file is uploaded, the post must not take the old one.
          if (img._uploadAbort) img._uploadAbort.abort();
          Object.assign(img, { media_id: "", _uploadFor: null, _upload: null });
          const pick = v === "original" ? Promise.resolve(img.orig) : compressFile(img, img.orig, v);
          img._compressing = pick;
          try {
            const f = await pick;
            if (img._compressing === pick) img._compressing = null;
            URL.revokeObjectURL(img.url);
            img.file = f; img.url = URL.createObjectURL(f); img.preset = v;
            img.size_bytes = f.size; img.mime = f.type; img.dim = "";
            uploadImage(img);
            renderImages();
          } catch (err) {
            if (img._compressing === pick) img._compressing = null;
            if (err.name === "AbortError") return; // superseded by a newer pick
            flash("Compress failed: " + err.message);
            ev.target.value = img.preset;
            uploadImage(img); // the file it keeps
          }
        } });
      for (const [v, label] of [["original", "original"], ["large", "large ≤2048px"],
                                ["medium", "medium ≤1600px"], ["small", "small ≤1080px"]]) {
        sel.append(el("option", { value: v, text: label }));
      }
      sel.value = img.preset || "original";
      kids.push(sel);
    } else if (img.size_bytes) {
      kids.push(el("div", { class: "imgsize", text: fmtBytes(img.size_bytes) }));
    }
    if (img.upload_error) {
      kids.push(el("div", { class: "imgsize bad", text: "upload failed: " + img.upload_error }),
        el("button", { class: "rm", type: "button", text: "retry upload",
          onclick: () => { img.upload_error = ""; uploadImage(img); renderImages(); } }));
    } else if (!img.file && !img.media_id) {
      // A draft attachment the server couldn't turn into media to post.
      kids.push(el("div", { class: "imgsize bad", text: "can't be posted: attach it again" }));
    }
    kids.push(el("button", { class: "rm", type: "button", text: "remove",
      onclick: () => {
        if (img._compressAbort) img._compressAbort.abort();
        if (img._uploadAbort) img._uploadAbort.abort();
        if (img.file) URL.revokeObjectURL(img.url);
        state.images.splice(i, 1); renderImages();
      } }));
    c.append(el("div", { class: "thumb" }, ...kids));
  });
  const vp = $("#vidpreset"); if (vp) vp.hidden = state.mediaOff || state.images.some(i => i.video);
  renderEditor(); // the attachments on its cards
  renderMeta(); renderPreview();
}

// ---------------------------------------------------------------------------
// Meta / preflight summary
// ---------------------------------------------------------------------------

function renderMeta() {
  const mm = $("#mastermeta");
  if (mm) mm.textContent = `${state.master.length} chars · ${wcount(state.master)} words`;
  const sum = $("#summary");
  if (!sum) return;
  sum.innerHTML = "";
  const plats = selected();
  if (plats.length === 0) {
    // Before the accounts are read, the rows say so (loading or the error).
    if (accountsLoaded()) sum.append(el("span", { class: "bad", text: "⚠ no accounts selected" }));
    return;
  }
  sum.append(el("span", { text: `→ ${plats.length} target${plats.length > 1 ? "s" : ""} · ${state.images.length} image${state.images.length === 1 ? "" : "s"}` }));
  // A refusal the server would give on Post (a 422), from the last plan.
  const err = planError();
  const refused = err && err.status === 422;
  sum.append(refused
    ? el("span", { class: "warn", text: "⚠ " + err.message })
    : el("span", { class: "ok", text: "ready ✓" }));
}

// ---------------------------------------------------------------------------
// Schedule UI — ported verbatim from the pre-module monolith
// ---------------------------------------------------------------------------

function schedNote(v) {
  const d = new Date(v);
  if (isNaN(d.getTime())) return "";
  const when = whenText(d);
  const mins = Math.round((d.getTime() - Date.now()) / 60000);
  let rel;
  if (mins < 1) rel = "now";
  else if (mins < 60) rel = `in ${mins} min`;
  else if (mins < 1440) rel = `in ${Math.round(mins / 60)}h`;
  else rel = `in ${Math.round(mins / 1440)}d`;
  return `⏳ fires ${rel} · ${when}`;
}

function updateSched() {
  const v = $("#schedat").value;
  const it = state.interaction; // a reply or quote is never scheduled
  $("#submit").textContent = it ? (it.action === "quote" ? "Quote" : "Reply") : v ? "Schedule" : "Post";
  $("#schedclear").hidden = !v;
  const note = $("#schednote");
  if (!v) { note.textContent = ""; note.className = "sched-note muted"; return; }
  const d = new Date(v);
  const past = !isNaN(d.getTime()) && d.getTime() <= Date.now();
  note.textContent = past ? "⚠ that time is in the past" : schedNote(v);
  note.className = "sched-note " + (past ? "bad" : "set");
}

// ---------------------------------------------------------------------------
// Submit — ported verbatim from the pre-module monolith
// ---------------------------------------------------------------------------

// resetComposer clears the draft out of the composer after it's been committed
// (posted or scheduled): text, images, interaction, and per-platform overrides
// all return to empty, and every dependent view is refreshed so no stale text,
// thumbnail, count, thread badge, or preview lingers.
export function resetComposer() {
  clearImages();
  state.master = "";
  state.breaks = [];
  state.anchors = {};
  state.tab = "shared";
  state.interaction = null;
  state.ov = byKey();
  const sched = $("#schedat"); if (sched) { sched.value = ""; updateSched(); }
  // A posted draft's tags don't carry over to the next one.
  const tags = document.querySelector("#draft-tags-input .tag-chips"); if (tags) tags.replaceChildren();
  renderImages();
  refreshCounts();
  refreshTargets();
  renderCards();
  renderMeta();
  renderInteractionUI();
  renderPreview();
}

// The last submit that may not have reached the server: its body and its
// Idempotency-Key. Posting the same body again reuses the key, so a post the
// server did save (the answer was lost) isn't posted twice.
const lastSends = {};

// sendPost sends a POST /v1/posts body with an Idempotency-Key and answers
// the post (202: saved; the workers send it). slot keeps the composer's and
// the Interact tab's retries apart.
export async function sendPost(body, slot = "compose") {
  const json = JSON.stringify(body);
  let lastSend = lastSends[slot];
  if (!lastSend || lastSend.json !== json) lastSend = lastSends[slot] = { json, key: newKey() };
  const key = lastSend.key;
  let post;
  try {
    post = await v1("/v1/posts", { method: "POST", body, headers: { "Idempotency-Key": key } });
  } catch (e) {
    // An answer means the server decided (refused, or the key is in use):
    // a changed body gets a new key anyway. Without one, keep the key.
    if (e.status && e.status !== 409 && lastSends[slot] === lastSend) delete lastSends[slot];
    throw e;
  }
  if (lastSends[slot] === lastSend) delete lastSends[slot];
  return post;
}

// unavailable names the selected accounts that can't be posted to now.
function unavailable() {
  return selected().filter(id => !isConnected(byId(id))).map(id => nameOf(id));
}

// mediaReady waits for the attachments' uploads; false (with a note) when
// one failed or can't be posted.
export async function mediaReady() {
  // A preset being applied makes a new file to upload first.
  await Promise.all(state.images.map(i => i._compressing && i._compressing.catch(() => {})));
  await Promise.all(state.images.map(i => (i.file && !i.media_id ? uploadImage(i) : null)));
  const bad = state.images.filter(i => !i.media_id);
  if (bad.length) {
    // A file can be uploaded again; a draft's attachment without one can't.
    flash(bad.some(i => !i.file && !i._file)
      ? "An attachment from the draft can't be posted: remove it and attach it again"
      : bad.length === 1 ? "An image isn't uploaded: retry or remove it" : bad.length + " images aren't uploaded: retry or remove them");
    renderImages();
    return false;
  }
  return true;
}

// doPost posts (or schedules, or replies/quotes: v1PostBody reads the mode)
// what the composer holds.
async function doPost() {
  const btn = $("#submit");
  const it = state.interaction;
  const label = it ? (it.action === "quote" ? "Quote" : "Reply") : ($("#schedat").value ? "Schedule" : "Post");
  btn.disabled = true;
  btn.textContent = it ? label + "ing…" : $("#schedat").value ? "Scheduling…" : "Posting…";
  try {
    if (!(await mediaReady())) return;
    const draftId = state.activeDraftId;
    const body = v1PostBody();
    // The server uses the draft up once the post is saved.
    if (draftId) body.draft = draftId;
    let post;
    try {
      post = await sendPost(body);
    } catch (e) {
      // Deleted meanwhile (another tab posted it): post without it.
      const gone = e.problem && (e.problem.errors || []).some(fe => fe.pointer === "/draft" && fe.code === "unknown_draft");
      if (!gone) throw e;
      delete body.draft;
      post = await sendPost(body);
    }
    if (post.status === "scheduled" && post.scheduled_at) {
      flash("Scheduled for " + whenText(new Date(post.scheduled_at)));
    } else {
      setInflight(post.id);
      openProgressModal(post.id, post);
    }
    state.activeDraftId = null;
    state.draftETag = "";
    state.dirty = false;
    // Committed → empty the composer so the posted text and images don't
    // linger as if still unsent (and leave a reply or quote).
    if (it) exitInteraction(); else resetComposer();
    import("./drafts_recovery.js").then(m => m.clearRecovery());
    import("./drafts.js").then(m => m.loadDraftList && m.loadDraftList());
  } catch (e) {
    flash("Error: " + e.message);
  } finally {
    // Label from the composer as it is now: a commit leaves interaction mode
    // and clears #schedat.
    btn.disabled = false;
    btn.textContent = state.interaction ? (state.interaction.action === "quote" ? "Quote" : "Reply")
      : $("#schedat").value ? "Schedule" : "Post";
  }
}

// submit checks what the server would refuse before posting: a video still
// processing, no account selected, an account that can't be posted to. Text
// over an account's limit is not one: the server threads it (the preview
// shows how).
async function submit() {
  if (state.images.some(i => i.video && i.phase !== "ready")) {
    flash(state.images.some(i => i.video && i.phase === "failed")
      ? "The video failed — retry or remove it first"
      : "Wait for the video to finish processing");
    return;
  }
  let missing = unavailable();
  if ((!state.targets.size && !accountsLoaded()) || missing.length) {
    // The accounts may have failed to load (a blip at start: nothing could
    // be selected), or one came back since: ask again (main.js then selects
    // the defaults if nothing was chosen). The button waits, so a second tap
    // can't post.
    const btn = $("#submit");
    if (btn.disabled || !reloadAccounts) return;
    btn.disabled = true;
    try { await reloadAccounts(); } catch (e) { flash("Couldn't read your accounts: " + e.message); return; }
    finally { btn.disabled = false; }
    missing = unavailable();
  }
  if (!state.targets.size) { flash("Select at least one account."); return; }
  if (missing.length) {
    const many = missing.length > 1;
    flash(missing.join(", ") + (many ? " aren't" : " isn't") + " connected: untick " + (many ? "them" : "it") + " to post to the rest");
    return;
  }
  doPost();
}

// reloadAccounts reads the accounts again and applies them (main.js sets it).
let reloadAccounts = null;
export function setAccountsReloader(fn) { reloadAccounts = fn; }

// ---------------------------------------------------------------------------
// Progress modal — a post's targets as they send
// ---------------------------------------------------------------------------

// progressClass maps a /v1 status onto the task tree's row classes.
function progressClass(s) {
  if (s === "published") return "success";
  if (s === "ok" || s === "failed" || s === "partial" || s === "skipped") return s;
  if (s === "publishing" || s === "sending") return "running";
  if (s === "unknown" || s === "needs_attention") return "partial";
  if (s === "canceled" || s === "missed") return "skipped";
  return "queued";
}

// targetDetail is a target row's note: how far a thread got, or what went
// wrong and whether it is retried.
function targetDetail(t) {
  const items = t.items || [];
  const failed = items.find(it => it.status === "failed" || it.status === "unknown");
  if (failed) {
    const retry = failed.next_attempt_at && !t.gave_up_at ? " · retrying" : "";
    if (failed.status === "unknown") return "may have been posted: check it in History";
    return (failed.error || "failed") + retry;
  }
  if (items.length > 1) {
    const sent = items.filter(it => it.status === "published" || it.status === "partial").length;
    return `${sent}/${items.length} posts`;
  }
  if (t.status === "canceled") return "canceled";
  return "";
}

// openProgressModal shows a post's targets as they send: the post's events
// (GET /v1/posts/{id}/events) wake a re-read until it stops publishing.
// Closing only stops watching — the post goes on server-side. first is the
// post as the create answered it, shown at once.
export function openProgressModal(postId, first) {
  const id = postId.startsWith("pst_") ? postId : "pst_" + postId;
  const bk = el("div", { class: "modal-bk" });
  const card = el("div", { class: "modal pmodal" });
  const title = el("p", { class: "pm-title" });
  const tree = el("div", { class: "tasktree" });
  const foot = el("div", { class: "pm-foot" });
  card.append(title, tree, foot);
  bk.append(card);
  document.body.append(bk);

  let stop = null;
  // A dialog: focus moves in, Escape closes (the post goes on sending).
  const close = asDialog(bk, card, () => { if (stop) stop(); stop = null; clearInflight(); bk.remove(); });

  const render = (post) => {
    const settled = post.status !== "publishing";
    title.textContent = !settled ? "Posting…"
      : post.status === "published" ? "✓ Posted"
      : post.status === "failed" ? "⚠ Posting failed"
      : post.status === "canceled" ? "⊘ Canceled"
      : post.status === "missed" ? "⏰ Not posted (missed)" : "⚠ Posted with issues";
    tree.innerHTML = "";
    for (const t of (post.targets || [])) {
      tree.append(el("div", { class: "trow lvl1 st-" + progressClass(t.status) },
        el("span", { class: "ic" }),
        el("span", { text: byId(t.account) ? nameOf(t.account) : networkName(t.type) }),
        el("span", { class: "meta", text: targetDetail(t) })));
      const ds = (t.items && t.items[0] && t.items[0].deliveries) || [];
      for (const d of ds) {
        tree.append(el("div", { class: "trow lvl2 st-" + progressClass(d.status) },
          el("span", { class: "ic" }),
          el("span", { text: d.endpoint.replace(/^wss?:\/\//, "") }),
          el("span", { class: "meta", text: d.message || "" })));
      }
    }
    foot.innerHTML = "";
    if (settled) {
      clearInflight();
      if (post.status !== "published") {
        foot.append(el("a", { class: "pm-link", href: "#", text: "Open in history ↗",
          onclick: (e) => {
            e.preventDefault();
            close();
            document.querySelector('.tab[data-view="history"]')?.click();
            openDetail(post.id);
          } }));
      }
      foot.append(el("button", { class: "pm-btn primary", type: "button", text: "Done", onclick: close }));
    } else {
      foot.append(el("button", { class: "pm-btn", type: "button", text: "Close", onclick: close }));
    }
  };

  if (first && first.targets && first.targets.some(t => t.items)) render(first);
  else { title.textContent = "Posting…"; foot.append(el("button", { class: "pm-btn", type: "button", text: "Close", onclick: close })); }
  let last = first;
  stop = followPost(id, {
    onPost: p => { last = p; render(p); },
    // Following gives up after a while: say so rather than spin.
    onStop: () => {
      if (!bk.isConnected || (last && last.status !== "publishing")) return;
      clearInflight();
      title.textContent = "Still sending: History shows how it goes";
    },
    until: p => p.status !== "publishing",
    onGone: close, // a post that's gone (deleted, or from before an upgrade)
  });
}

// ---------------------------------------------------------------------------
// Init
// ---------------------------------------------------------------------------

export function composeInit() {
  // A new plan from the server: thread counts, the notice and the summary
  // read it.
  document.addEventListener("plan", () => { refreshTargets(); renderMeta(); renderPreview(); });
  editorInit({
    changed: () => { refreshCounts(); refreshTargets(); renderMeta(); renderPreview(); },
    tab: t => { if (t !== "shared") state.focus = t; renderPreview(); },
  });
  $("#addimg").addEventListener("click", () => $("#imgfile").click());
  $("#imgfile").addEventListener("change", async e => {
    const file = e.target.files[0]; if (!file) return;
    e.target.value = "";
    const isVid = /^video\//.test(file.type) || /\.(mp4|mov|webm)$/i.test(file.name || "");
    if (isVid) { attachVideo(file); return; }
    if (state.images.some(i => i.video)) { flash("Remove the video first (one video OR images per post)"); return; }
    if (state.images.length >= 10) { flash("Max 10 images"); return; }
    // orig is kept so presets always re-derive from the as-attached file (no
    // generational quality stacking) and "original" is a true client-side revert.
    const entry = { file, orig: file, url: "", alt: "", preset: "original",
                    size_bytes: file.size, mime: file.type, dim: "" };
    const gen = imagesGen;
    if (isHeic(file)) {
      // Chrome can't render HEIC previews — convert server-side immediately so
      // the thumbnail works and nothing downstream ever sees HEIC. The
      // converted JPEG becomes this entry's "original".
      try {
        entry.file = await compressFile(entry, file, "convert");
        entry.orig = entry.file;
        entry.size_bytes = entry.file.size; entry.mime = entry.file.type;
      } catch (err) { flash("HEIC convert failed: " + err.message); return; }
    }
    if (gen !== imagesGen) return; // composer was cleared/replaced mid-convert
    if (state.images.length >= 10) { flash("Max 10 images"); return; }
    entry.url = URL.createObjectURL(entry.file);
    entry.id = newKey();
    state.images.push(entry);
    uploadImage(entry);
    renderImages();
  });
  const tz = $("#schedtz"); if (tz) tz.textContent = TZ + " · empty = now";
  $("#schedat").addEventListener("input", updateSched);
  $("#schedclear").addEventListener("click", () => { $("#schedat").value = ""; updateSched(); });
  $("#submit").addEventListener("click", submit);
  document.getElementById("threadnum")?.addEventListener("change", () => { refreshTargets(); renderPreview(); });
  renderChips(); renderImages(); renderCards(); renderMeta(); renderPreview();
  reattachInflight();
}

// reattachInflight reopens the progress of a post that was sending when the
// page was left, if it still is: asking the list (not the post) keeps a post
// that is gone from answering 404.
async function reattachInflight() {
  const pending = getInflight();
  if (!pending) return;
  const id = pending.startsWith("pst_") ? pending : "pst_" + pending;
  let page;
  try { page = await v1("/v1/posts?status=publishing&limit=100"); } catch { return; }
  if (getInflight() !== pending) return; // a new post took its place meanwhile
  const p = ((page && page.data) || []).find(x => x.id === id);
  if (p) openProgressModal(id);
  else clearInflight();
}

// newKey is a random id: crypto.randomUUID needs a secure context (https or
// localhost); a plain-http LAN install falls back to getRandomValues.
export function newKey() {
  if (crypto.randomUUID) return crypto.randomUUID();
  const b = crypto.getRandomValues(new Uint8Array(16));
  b[6] = (b[6] & 0x0f) | 0x40; b[8] = (b[8] & 0x3f) | 0x80;
  const h = [...b].map(x => x.toString(16).padStart(2, "0")).join("");
  return `${h.slice(0, 8)}-${h.slice(8, 12)}-${h.slice(12, 16)}-${h.slice(16, 20)}-${h.slice(20)}`;
}
