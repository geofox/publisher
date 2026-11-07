"use strict";
// The end of a re-authentication with the identity provider (a small
// window): tell the page that opened it, then close.
if (window.opener && !window.opener.closed) {
  window.opener.postMessage({ type: "publisher-reauth" }, location.origin);
  window.close();
}
