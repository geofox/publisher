"use strict";
import { state, buildSpec } from "./state.js";
import { markDirty } from "./compose.js";

const KEY = "compose_recovery_v1";

// pending: the banner offers a stored snapshot the owner hasn't restored
// or discarded yet. Until then nothing removes it: an empty composer (or a
// draft opened meanwhile) leaves it for a reload to offer again.
let pending = false;
export function setRecoveryPending(on) { pending = !!on; }

// snapshot writes the current Compose state to localStorage as a recovery
// snapshot. Called on every state change via installRecoveryAutosave. Silently
// no-ops on quota errors so a full localStorage never breaks the editor.
export function snapshot() {
  try {
    if (state.activeDraftId && !state.dirty) {
      // An open draft with nothing unsaved: the server has it all.
      if (!pending) localStorage.removeItem(KEY);
      return;
    }
    // Otherwise keep the edits (an open draft's carry its id and etag, so
    // the restore saves into that draft, or finds it changed meanwhile).
    const spec = buildSpec();
    // Skip empty drafts to avoid recovery noise.
    if (!spec.master_text && (!spec.images || spec.images.length === 0)) {
      if (!pending) localStorage.removeItem(KEY);
      return;
    }
    // The tags too: saving the restored draft sends them.
    spec.tags = Array.from(document.querySelectorAll("#draft-tags-input .tag-chip")).map(n => n.dataset.tag);
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
  pending = false; // what was offered is gone now, on purpose
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
  const onComposeEdit = (e) => {
    // #preview is a read-only render of the draft; its platform-switcher only
    // changes which platform is previewed (state.focus), not the draft content,
    // so it must never mark the draft dirty or trigger an autosave snapshot.
    if (e.target.closest("#preview")) return;
    if (e.target.closest("#compose")) { fire(); markDirty(); }
  };
  document.addEventListener("input", onComposeEdit);
  document.addEventListener("change", onComposeEdit);
}
