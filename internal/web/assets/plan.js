"use strict";
// plan.js keeps the server's plan of the composer: POST /v1/posts:preview for
// every selected account, refreshed (debounced) whenever the composer
// changes. The server's planner is the single source of truth for how a post
// splits, so thread counts, the thread notice and the editor read it from
// here rather than estimating in the browser.
import { v1 } from "./common.js";
import { state, postedText, v1PostBody } from "./state.js";

// current is the last plan the server answered: {key, byAccount, error}.
// byAccount maps an account id onto its preview target.
let current = { key: "", byAccount: {}, error: null };
let pendingKey = "";
let timer = null;
let seq = 0;
// A request that got no answer (offline, dropped) is asked again, later
// each time; the plan shown meanwhile is the last one answered.
let retryDelay = 0, retryTimer = null;
if (typeof window !== "undefined") window.addEventListener("online", () => { retryDelay = 0; requestPlan(); });

// planBody is the composer as a preview body: v1PostBody without the
// schedule or the draft (neither changes the split, and a past schedule
// would refuse the whole preview). In a reply or a quote each account
// previews the text it will post (the commentary, or the copy), so no source
// needs resolving.
export function planBody() {
  const body = v1PostBody();
  delete body.schedule;
  delete body.draft;
  const it = state.interaction;
  if (it) {
    delete body.reply_to;
    delete body.quote;
    for (const t of body.targets) {
      const txt = postedText(t.account);
      if (txt.trim() && txt !== state.master) {
        t.content = { ...(t.content || {}), text: txt };
        delete t.thread; // a copy is split automatically
      }
    }
  }
  return body;
}

// hasContent reports whether there is anything to plan.
function hasContent(body) {
  return (body.content.text || "").trim() !== "" || (body.content.media || []).length > 0 ||
    body.targets.some(t => t.content && t.content.text);
}

// requestPlan asks for a fresh plan after a short pause in typing. Calls
// while one is pending just move the pause.
export function requestPlan() {
  const body = planBody();
  const key = JSON.stringify(body);
  if (key === current.key || key === pendingKey) return;
  clearTimeout(timer);
  if (!body.targets.length || !hasContent(body)) {
    pendingKey = "";
    set({ key, byAccount: {}, error: null });
    return;
  }
  pendingKey = key;
  timer = setTimeout(() => fetchPlan(body, key), 250);
}

async function fetchPlan(body, key) {
  const mine = ++seq;
  let next;
  try {
    const data = await v1("/v1/posts:preview", { method: "POST", body });
    const byAccount = {};
    for (const t of (data && data.targets) || []) byAccount[t.account] = t;
    next = { key, byAccount, error: null };
    retryDelay = 0;
  } catch (e) {
    if (!e.status) {
      if (mine !== seq) return;
      if (pendingKey === key) pendingKey = "";
      retryDelay = Math.min(retryDelay ? retryDelay * 2 : 1000, 30000);
      clearTimeout(retryTimer);
      retryTimer = setTimeout(requestPlan, retryDelay);
      return;
    }
    next = { key, byAccount: {}, error: e };
  }
  if (mine !== seq) return; // a newer request owns the plan
  if (pendingKey === key) pendingKey = "";
  set(next);
}

function set(next) {
  current = next;
  document.dispatchEvent(new CustomEvent("plan"));
}

// planFor answers account id's preview target when the plan is for the
// composer as it is now, else null (still loading, refused, or stale).
export function planFor(id) {
  if (current.key !== JSON.stringify(planBody())) return null;
  return current.byAccount[id] || null;
}

// lastPlanFor answers account id's preview target from the last plan, even
// if the composer changed since: for counts that may lag a keystroke.
export function lastPlanFor(id) {
  return current.byAccount[id] || null;
}

// planError is the problem the last plan was refused with, when it is for
// the composer as it is now.
export function planError() {
  if (current.key !== JSON.stringify(planBody())) return null;
  return current.error;
}
