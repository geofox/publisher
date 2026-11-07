"use strict";
import { el, v1, flash, copyText } from "./common.js";
import { loadDestinations } from "./accounts.js";
import { settingsForm, readSettings } from "./settings-config.js";
import { setsSection, defaultsSection } from "./settings-sets.js";
import { portSection, showPort } from "./settings-export.js";

// Settings → Composing, Media, Publishing, Integrations and System: forms
// over the settings API (settings-config.js), account sets and defaults
// (settings-sets.js), what the server started with, and export and import
// (settings-export.js).

const code = s => el("code", { text: s });

// Each section is built once, on its first showing, then read again each
// time it is shown.
const built = new Map();

const SECTIONS = {
  composing: { scopes: ["owner"], build: () => [
    settingsForm("owner", ["composing.languages"], { title: "Languages",
      intro: "The languages you write in: the composer offers them, and a new post starts in the first." }).node,
    settingsForm("owner", ["translate.deepl_key"], { title: "Translation",
      intro: "With a DeepL key, the composer can translate a post into your other languages." }).node,
    setsSection(),
    defaultsSection(),
  ] },
  media: { scopes: ["instance"], build: () => [
    settingsForm("instance", ["media.store", "media.blossom_url", "media.blossom_key", "media.public", "media.local_quota"], {
      title: "Media",
      intro: "Where photos and videos go when you attach them. This is the server's setting: it applies to every post.",
      shows: {
        "media.blossom_url": get => get("media.store") === "blossom",
        "media.blossom_key": get => get("media.store") === "blossom",
        "media.public": get => get("media.store") === "local",
        "media.local_quota": get => get("media.store") === "local",
      },
      extra: mediaNotes,
      test: { group: "media", label: "Test the media store", what: "The media store" },
    }).node,
  ] },
  publishing: { scopes: ["owner"], build: () => [
    settingsForm("owner", ["publishing.auto_retry", "publishing.max_attempts", "publishing.schedule_grace"], {
      title: "Publishing", intro: "What happens when a post fails, or a scheduled one is late." }).node,
  ] },
  integrations: { scopes: ["owner", "instance"], build: () => [
    settingsForm("owner", ["feed.token", "feed.webhook_url", "feed.webhook_token"], { title: "Public feed",
      intro: feedIntro(),
      test: { group: "feed", label: "Send a test ping", what: "The feed webhook" } }).node,
    settingsForm("owner", ["alerts.webhook_url", "alerts.webhook_user", "alerts.webhook_pass"], { title: "Alerts",
      intro: "Told when a scheduled post fails, or an account needs you.",
      test: { group: "alerts", label: "Send a test alert", what: "The alert webhook" } }).node,
    settingsForm("instance", ["threads.app_id", "threads.app_secret"], { title: "Threads app", intro: threadsIntro() }).node,
  ] },
  system: { scopes: [], build: () => [systemSection(), portSection()] },
};

export const CONFIG_SECTIONS = Object.keys(SECTIONS);

// showSection builds a section into its panel (once) and reads its
// settings again.
export function showSection(name, panel) {
  const s = SECTIONS[name];
  if (!s || !panel) return;
  if (!built.has(name)) {
    panel.replaceChildren(...s.build());
    built.set(name, true);
  }
  for (const scope of s.scopes) readSettings(scope);
  if (name === "system") { readSystem(); showPort(); }
}

// threadsIntro is how to make the Meta app Threads sign-ins go through,
// with the address Meta sends the owner back to (GET /v1/destinations).
function threadsIntro() {
  const guide = el("a", { target: "_blank", rel: "noopener noreferrer", text: "Meta's guide", hidden: "" });
  const where = el("p", { class: "set-sect-intro", text: "Reading where Meta sends you back…" });
  loadDestinations(true).then(list => {
    const m = (list || []).find(x => x.type === "threads");
    const a = m && (m.auth || []).find(x => x.flow === "redirect");
    if (a && /^https:\/\//i.test(a.help_url || "")) { guide.href = a.help_url; guide.hidden = false; }
    const uri = a && a.redirect_uri;
    if (!uri) {
      where.replaceChildren("Set PUBLIC_URL first, at an https address: Meta sends you back to it, at ", code("/oauth/callback/threads"), ".");
      return;
    }
    const shown = el("code", { class: "set-uri", text: uri });
    const copy = el("button", { class: "ghost sm", type: "button", text: "Copy", "aria-label": "Copy the redirect URI" });
    copy.addEventListener("click", () => copyText(uri, shown, "Redirect URI copied"));
    where.replaceChildren("Add this address to the app's valid OAuth redirect URIs, exactly (Meta compares it character by character): ",
      shown, " ", copy);
    if (!/^https:\/\//i.test(uri)) {
      where.append(el("span", { class: "fld-warn", text: " Meta takes only https: set PUBLIC_URL to Publisher's https address." }));
    }
  }, e => { where.textContent = "Where Meta sends you back couldn't be read: " + e.message; });
  return el("div", {},
    el("p", { class: "set-sect-intro" },
      "To add Threads accounts by signing in, Publisher goes through a Meta app you make once: one with the Threads use case. ",
      "Add your Threads account to it as a Threads Tester (App roles, Roles, Add People) and accept the invite in Threads ",
      "(Account settings, Website permissions); your own accounts need no App Review. ", guide),
    where,
    el("p", { class: "set-sect-intro", text: "Then copy its Threads App ID and Threads App secret here. Without them, add Threads accounts with a token." }));
}

function feedIntro() {
  return el("p", { class: "set-sect-intro" },
    "Posts you mark for it can be read at ", code("/v1/public/feed"), " with the token below (a website or a newsletter, say); the webhook tells a receiver when one is published.");
}

// mediaNotes are what the media choice means: a key to allow on the
// Blossom server, the proxy rule and the IP address a public disk gives
// away.
function mediaNotes(get) {
  const store = get("media.store");
  const out = [];
  if (store === "local") {
    out.push(el("div", { class: "set-note" },
      el("p", { text: "Media are kept beside the database (MEDIA_DIR) and served at /media/ without sign-in, like a Blossom server: anyone with a file's address can see it." }),
      el("p", {}, "Threads and Nostr fetch media by address, so they need ", el("strong", { text: "Serve media publicly" }),
        ": PUBLIC_URL/media/ reachable from the internet, and a sign-in in front of Publisher must let ", code("/media/"), " through (see the README).")));
    if (get("media.public") === true) {
      out.push(el("div", { class: "set-note warn" },
        el("p", { text: "Nostr clients load images straight from this server: anyone who sees a post learns its IP address, your home's if it runs there. A Blossom server avoids that." })));
    }
  }
  if (store === "none") out.push(el("p", { class: "set-status", text: "Posts carry no photos or videos until a store is set." }));
  return out;
}

// ── System ──────────────────────────────────────────────────────────────

let sysNode = null;
function systemSection() {
  sysNode = el("section", { class: "set-sect" }, el("p", { class: "set-reading", text: "Reading…" }));
  return sysNode;
}

const AUTH = { oidc: "your identity provider (OIDC)", local: "a passkey or password set in Publisher", proxy: "the reverse proxy alone" };
const READY = { ok: "Ready", degraded: "Ready, with a problem", unavailable: "Not ready" };

async function readSystem() {
  if (!sysNode) return;
  const [sys, ready, inst] = await Promise.allSettled([
    v1("/v1/instance/system"),
    fetch("/readyz", { credentials: "same-origin", headers: { Accept: "application/json" } }).then(r => r.json()),
    v1("/v1/instance"),
  ]);
  const parts = [el("h2", { class: "set-sect-title", text: "System" })];
  const facts = el("dl", { class: "sys-facts" });
  const fact = (k, v) => facts.append(el("dt", { text: k }), el("dd", {}, v));
  const i = inst.status === "fulfilled" ? inst.value : null;
  if (i) {
    fact("Version", i.version || "?");
    fact("Sign-in", AUTH[i.auth] || i.auth || "?");
    const ks = (i.secrets && i.secrets.status) || "none";
    fact("Master key", ks === "ok" ? "Set: credentials saved in the app are encrypted with it"
      : ks === "none" ? "Not set: accounts and secret settings can only come from the environment"
      : (i.secrets.detail || "Needs fixing"));
  }
  if (ready.status === "fulfilled" && ready.value) {
    const r = ready.value;
    const checks = Object.entries(r.checks || {}).map(([name, c]) =>
      el("li", { class: c.status === "ok" ? "" : "bad" }, name.replace(/_/g, " ") + ": " + (c.status || "?") + (c.error ? " (" + c.error + ")" : "")));
    fact("Health", el("span", {}, READY[r.status] || r.status || "?", checks.length ? el("ul", { class: "sys-checks" }, ...checks) : null));
  }
  parts.push(facts);
  if (sys.status === "fulfilled") {
    const vals = sys.value.values || [];
    const set = vals.filter(v => v.set), unset = vals.filter(v => !v.set);
    const table = el("table", { class: "sys-env" },
      el("thead", {}, el("tr", {}, el("th", { scope: "col", text: "Variable" }), el("th", { scope: "col", text: "Value" }))),
      el("tbody", {}, ...set.map(v => el("tr", {}, el("td", {}, code(v.env)),
        el("td", { text: v.secret ? "set (not shown)" : v.value })))));
    parts.push(el("h3", { class: "set-sub", text: "What the server started with" }),
      el("p", { class: "set-sect-intro", text: "Read from its environment at start: change them there, then restart it. Secrets are never shown." }),
      set.length ? table : el("p", { class: "set-status", text: "None of these are set: the defaults apply." }));
    if (unset.length) {
      parts.push(el("details", { class: "set-more" }, el("summary", { text: "Not set (" + unset.length + ")" }),
        el("p", { class: "sys-unset" }, ...unset.flatMap((v, n) => [n ? ", " : "", code(v.env)]))));
    }
  } else {
    const e = sys.reason || {};
    parts.push(el("div", { class: "set-note" + (e.code === "sign_in_required" ? " warn" : " bad") },
      el("p", { text: e.code === "sign_in_required"
        ? "Behind the reverse proxy alone, what the server started with isn't shown here: set up sign-in first."
        : "What the server started with couldn't be read: " + (e.message || "") })));
  }
  sysNode.replaceChildren(...parts);
}
