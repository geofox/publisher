"use strict";
import { v1 } from "./common.js";

// followPost keeps a view of a post current while it sends: its event stream
// (GET /v1/posts/{id}/events) wakes a re-read, and a slow poll backs it up.
// The re-read decides when to stop, not the stream: the stream replays from
// the post's first event, so a post that had settled before a retry replays
// an old post.completed, and the stream of a settled post ends at once.
//
// onPost(post) gets each changed read; until(post) says when the wait is
// over; onGone() is called when the post is not found (and following stops).
// Following also stops after five minutes. Returns a function that stops it.
export function followPost(id, { onPost, until = p => p.status !== "publishing", onGone, onStop } = {}) {
  const path = "/v1/posts/" + encodeURIComponent(id);
  let stopped = false, timer = null, shown = "";
  const deadline = Date.now() + 5 * 60 * 1000;
  const es = new EventSource(path + "/events");
  const poll = setInterval(() => reread(), 2000);
  const stop = () => {
    if (stopped) return;
    stopped = true;
    es.close();
    clearTimeout(timer);
    clearInterval(poll);
    if (onStop) onStop();
  };
  const reread = async () => {
    let p;
    try { p = await v1(path); } catch (e) {
      if (!stopped && e.status === 404) { stop(); if (onGone) onGone(); }
      return;
    }
    if (stopped) return;
    const json = JSON.stringify(p);
    if (json !== shown) { shown = json; if (onPost) onPost(p); }
    if (until(p) || Date.now() > deadline) stop();
  };
  const wake = () => { clearTimeout(timer); timer = setTimeout(reread, 250); };
  for (const t of ["post.updated", "post.completed", "item.sending", "item.published", "item.partial", "item.failed",
    "item.unknown", "item.canceled", "delivery.updated"]) es.addEventListener(t, wake);
  // The server ends the stream once the post settles; closing here stops
  // EventSource from reconnecting.
  es.onerror = () => { if (es.readyState === EventSource.CLOSED || stopped) { es.close(); if (!stopped) wake(); } };
  wake();
  return stop;
}
