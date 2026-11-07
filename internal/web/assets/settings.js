"use strict";
import { $ } from "./common.js";
import { accountsShow, accountsHide, setAccountsInstance } from "./settings-accounts.js";
import { accessShow } from "./access.js";
import { tokensShow } from "./tokens.js";
import { showSection, CONFIG_SECTIONS } from "./settings-sections.js";

// The Settings tab: Accounts (settings-accounts.js); Composing, Media,
// Publishing, Integrations and System (settings-sections.js); Access (how
// the owner signs in, sessions, sign-in activity and API tokens:
// access.js, tokens.js). A section nav switches between them; the last
// one shown is remembered in this browser.

const SECTIONS = ["accounts", "composing", "media", "publishing", "integrations", "access", "system"];
const LAST_KEY = "publisher.settingsSection";

let current = "";

function remembered() {
  try { const s = localStorage.getItem(LAST_KEY); return SECTIONS.includes(s) ? s : ""; } catch (_) { return ""; }
}

// settingsInit takes the instance (GET /v1/instance) and wires the nav.
export function settingsInit(instance) {
  setAccountsInstance(instance);
  document.querySelectorAll(".set-nav [data-set]").forEach(b =>
    b.addEventListener("click", () => settingsShow(b.dataset.set)));
  keyBanner(instance);
}

// settingsShow shows a section (the remembered one, else Accounts).
export function settingsShow(section) {
  const s = SECTIONS.includes(section) ? section : (current || remembered() || "accounts");
  current = s;
  try { localStorage.setItem(LAST_KEY, s); } catch (_) { /* storage off */ }
  document.querySelectorAll(".set-nav [data-set]").forEach(b => {
    if (b.dataset.set === s) {
      b.setAttribute("aria-current", "true");
      // The nav scrolls on a narrow screen: the section shown stays in it.
      if (b.parentElement.scrollWidth > b.parentElement.clientWidth) b.scrollIntoView({ block: "nearest", inline: "nearest" });
    } else b.removeAttribute("aria-current");
  });
  for (const id of SECTIONS) { const p = $("#set-" + id); if (p) p.hidden = id !== s; }
  showKeyBanner(s !== "accounts" || proxyMode); // Accounts says it in its own words (not behind the proxy alone)
  if (s === "accounts") accountsShow(); else accountsHide();
  if (s === "access") { accessShow(); tokensShow(); }
  if (CONFIG_SECTIONS.includes(s)) showSection(s, $("#set-" + s));
}

// settingsHidden tells the sections the tab went away.
export function settingsHidden() { accountsHide(); showKeyBanner(true); }

// keyBanner warns, on every tab, that saved credentials can't be opened
// (the master key is missing or another one): those accounts' posts wait
// until it is fixed.
let keyBannerOn = false, proxyMode = false;
function keyBanner(instance) {
  proxyMode = !!instance && instance.auth === "proxy";
  const b = $("#key-banner");
  const s = instance && instance.secrets;
  if (!b || !s || (s.status !== "missing" && s.status !== "mismatch")) return;
  $("#key-banner-text").textContent = s.detail || "The master key needs fixing: accounts added in the app are locked.";
  keyBannerOn = true;
  b.hidden = false;
}

function showKeyBanner(on) {
  const b = $("#key-banner");
  if (b && keyBannerOn) b.hidden = !on;
}
