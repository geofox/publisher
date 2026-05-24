"use strict";
import { $ } from "./common.js";
import { composeInit } from "./compose.js";
import { historyInit, loadHistory } from "./history.js";
import { toolsInit, loadTools } from "./tools.js";
import { verifyInit } from "./verify.js";

// ---------------------------------------------------------------------------
// Tab routing
// ---------------------------------------------------------------------------

function switchTab(view) {
  document.querySelectorAll(".tab").forEach(b => b.classList.toggle("on", b.dataset.view === view));
  document.querySelectorAll(".view").forEach(s => { s.hidden = s.id !== view; });
  if (view === "history") loadHistory(true);
  if (view === "tools")   loadTools();
}

// ---------------------------------------------------------------------------
// Boot
// ---------------------------------------------------------------------------

function init() {
  composeInit();
  historyInit();
  toolsInit();
  verifyInit();
  document.querySelectorAll(".tab").forEach(b =>
    b.addEventListener("click", () => switchTab(b.dataset.view)));
}

document.addEventListener("DOMContentLoaded", init);
