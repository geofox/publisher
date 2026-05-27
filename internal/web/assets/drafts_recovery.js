"use strict";
import { state, buildSpec } from "./state.js";
import { markDirty } from "./compose.js";

const KEY = "compose_recovery_v1";

// snapshot writes the current Compose state to localStorage as a recovery
// snapshot. Called on every state change via installRecoveryAutosave. Silently
// no-ops on quota errors so a full localStorage never breaks the editor.
export function snapshot() {
  try {
    if (state.activeDraftId) {
      // When a draft is active, the server is the source of truth; clear
      // recovery to avoid offering to restore stale local edits next session.
      localStorage.removeItem(KEY);
      return;
    }
    const spec = buildSpec();
    // Skip empty drafts to avoid recovery noise.
    if (!spec.master_text && (!spec.images || spec.images.length === 0)) {
      localStorage.removeItem(KEY);
      return;
    }
    localStorage.setItem(KEY, JSON.stringify(spec));
  } catch (e) {
    // quota or serialization error — log and continue
    console.warn("drafts recovery snapshot failed:", e);
  }
}

export function loadRecovery() {
  try {
    const raw = localStorage.getItem(KEY);
    if (!raw) return null;
    return JSON.parse(raw);
  } catch (e) {
    console.warn("drafts recovery load failed:", e);
    return null;
  }
}

export function clearRecovery() {
  try { localStorage.removeItem(KEY); } catch { /* ignore */ }
}

// installRecoveryAutosave debounces snapshot() and wires it to all state-mutating
// UI events. Call once on app boot.
export function installRecoveryAutosave() {
  let t = null;
  const fire = () => {
    clearTimeout(t);
    t = setTimeout(snapshot, 250);
  };
  document.addEventListener("input", (e) => {
    if (e.target.closest("#compose")) { fire(); markDirty(); }
  });
  document.addEventListener("change", (e) => {
    if (e.target.closest("#compose")) { fire(); markDirty(); }
  });
}
